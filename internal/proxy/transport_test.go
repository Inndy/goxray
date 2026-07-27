package proxy

import (
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"go.inndy.tw/goxray/internal/config"
	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/store"
)

const cannedSSE = `event: message_start
data: {"type":"message_start","message":{"usage":{"input_tokens":10}}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

// mockUpstream emits a canned SSE stream and counts hits + records accept-encoding.
func mockUpstream(t *testing.T, hits *atomic.Int64, sse bool, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if got := r.Header.Get("Accept-Encoding"); got != "identity" {
			t.Errorf("upstream accept-encoding = %q, want identity", got)
		}
		w.Header().Set("request-id", "req_test")
		if sse {
			w.Header().Set("content-type", "text/event-stream")
		} else {
			w.Header().Set("content-type", "application/json")
		}
		w.WriteHeader(200)
		io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
}

func testCfg(t *testing.T, upstream string) *config.Config {
	t.Helper()
	u, err := url.Parse(upstream)
	if err != nil {
		t.Fatal(err)
	}
	return &config.Config{Port: 0, Upstream: u, MaxBodyBytes: 50 << 20}
}

// fakeRec implements Recorder and can be told to fail either persist call.
type fakeRec struct {
	failReq, failRes bool
	lastState        model.ExchangeState
}

func (f *fakeRec) NewExchange(m, u string, h http.Header) *model.Exchange {
	return &model.Exchange{ID: "fake-id", Method: m, URL: u, State: model.StateInflight}
}
func (f *fakeRec) PersistRequest(ex *model.Exchange, b []byte) error {
	if f.failReq {
		return errors.New("disk full")
	}
	return nil
}
func (f *fakeRec) PersistResponse(ex *model.Exchange, b []byte) error {
	f.lastState = ex.State
	if f.failRes {
		return errors.New("disk full")
	}
	return nil
}

func TestSplitSourceSlug(t *testing.T) {
	cases := []struct{ in, src, api string }{
		{"/v1/messages", "", "/v1/messages"},
		{"/v1/messages/count_tokens", "", "/v1/messages/count_tokens"},
		{"/proj-a/v1/messages", "proj-a", "/v1/messages"},
		{"/api/hello", "", "/api/hello"},
		{"/team1/api/hello", "team1", "/api/hello"},
		{"/", "", "/"},
		{"/onlyslug", "onlyslug", "/"},
	}
	for _, c := range cases {
		src, api := splitSourceSlug(c.in)
		if src != c.src || api != c.api {
			t.Errorf("splitSourceSlug(%q) = (%q,%q), want (%q,%q)", c.in, src, api, c.src, c.api)
		}
	}
}

func TestSourceSlugStrippedAndRecorded(t *testing.T) {
	var hits atomic.Int64
	var gotPath atomic.Value
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		gotPath.Store(r.URL.Path)
		w.Header().Set("content-type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, cannedSSE)
	}))
	defer up.Close()

	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "index.db"), model.ProviderAnthropic, 2)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(testCfg(t, up.URL), st.Recorder)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/proj-x/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()

	// Upstream must see the slug STRIPPED.
	if p, _ := gotPath.Load().(string); p != "/v1/messages" {
		t.Errorf("upstream saw path %q, want /v1/messages (slug not stripped)", p)
	}
	st.Close()

	rd, _ := store.OpenReader(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"), filepath.Join(d, "index.db"))
	defer rd.Close()
	rows, _ := rd.List(store.Filter{Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	if rows[0].Source != "proj-x" {
		t.Errorf("source = %q, want proj-x", rows[0].Source)
	}
	if rows[0].URL != "/v1/messages" {
		t.Errorf("recorded url = %q, want stripped /v1/messages", rows[0].URL)
	}
	// Source filter works.
	if got, _ := rd.List(store.Filter{Source: "proj-x", Limit: 10}); len(got) != 1 {
		t.Errorf("source=proj-x filter returned %d", len(got))
	}
	if none, _ := rd.List(store.Filter{Source: "nope", Limit: 10}); len(none) != 0 {
		t.Errorf("source=nope should return 0, got %d", len(none))
	}
}

func TestStreamingRoundTrip(t *testing.T) {
	var hits atomic.Int64
	up := mockUpstream(t, &hits, true, cannedSSE)
	defer up.Close()

	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "index.db"), model.ProviderAnthropic, 2)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(testCfg(t, up.URL), st.Recorder)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json",
		strings.NewReader(`{"model":"claude-opus-4-8","metadata":{"session_id":"s1"},"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != cannedSSE {
		t.Errorf("client did not receive full stream:\n got %q", got)
	}
	if hits.Load() != 1 {
		t.Errorf("upstream hits = %d, want 1", hits.Load())
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	// Verify durable record.
	rd, _ := store.OpenReader(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"), filepath.Join(d, "index.db"))
	defer rd.Close()
	rows, err := rd.List(store.Filter{Limit: 10})
	if err != nil || len(rows) != 1 {
		t.Fatalf("expected 1 recorded row, got %d (err %v)", len(rows), err)
	}
	if rows[0].State != model.StateComplete {
		t.Errorf("state = %q", rows[0].State)
	}
	raw, err := rd.Blob(rows[0].ResRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != cannedSSE {
		t.Errorf("stored raw response not byte-exact")
	}
}

func TestFailClosedRequestReturns502NoUpstream(t *testing.T) {
	var hits atomic.Int64
	up := mockUpstream(t, &hits, true, cannedSSE)
	defer up.Close()

	srv := New(testCfg(t, up.URL), &fakeRec{failReq: true})
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("status = %d, want 502", resp.StatusCode)
	}
	if hits.Load() != 0 {
		t.Errorf("upstream was hit %d times; fail-closed must not forward", hits.Load())
	}
	body, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(body), `"type":"error"`) {
		t.Errorf("502 body not Anthropic-error-shaped: %s", body)
	}
}

func TestFailClosedResponseAbortsStream(t *testing.T) {
	var hits atomic.Int64
	up := mockUpstream(t, &hits, true, cannedSSE)
	defer up.Close()

	rec := &fakeRec{failRes: true}
	srv := New(testCfg(t, up.URL), rec)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	body, readErr := io.ReadAll(resp.Body)
	resp.Body.Close()

	// The terminal frames were held back and never released (commit failed), and
	// the connection was aborted — so the client must NOT have seen message_stop,
	// and/or the read errors with an unexpected EOF.
	if strings.Contains(string(body), "message_stop") {
		t.Errorf("client received terminal frame despite failed commit: %q", body)
	}
	if readErr == nil {
		t.Logf("note: read returned no error; body truncated at %d bytes", len(body))
	}
	if rec.lastState != model.StateComplete {
		t.Errorf("recorder saw state %q at commit attempt", rec.lastState)
	}
}

// A non-SSE response larger than the request-body cap must still be recorded
// and relayed byte-exact: maxBody bounds clients, never the upstream's reply.
func TestNonSSELargeResponseNotTruncated(t *testing.T) {
	var hits atomic.Int64
	jsonBody := `{"type":"message","big":"` + strings.Repeat("x", 4096) + `"}`
	up := mockUpstream(t, &hits, false, jsonBody)
	defer up.Close()

	d := t.TempDir()
	st, _ := store.Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "index.db"), model.ProviderAnthropic, 2)
	cfg := testCfg(t, up.URL)
	cfg.MaxBodyBytes = 1024
	srv := New(cfg, st.Recorder)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != jsonBody {
		t.Errorf("client body truncated: got %d bytes, want %d", len(got), len(jsonBody))
	}
	st.Close()

	rd, _ := store.OpenReader(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"), filepath.Join(d, "index.db"))
	defer rd.Close()
	rows, _ := rd.List(store.Filter{Limit: 10})
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d", len(rows))
	}
	raw, err := rd.Blob(rows[0].ResRef)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != jsonBody {
		t.Errorf("stored response truncated: got %d bytes, want %d", len(raw), len(jsonBody))
	}
}

func TestNonSSECommitsBeforeClientWrite(t *testing.T) {
	var hits atomic.Int64
	jsonBody := `{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4}}`
	up := mockUpstream(t, &hits, false, jsonBody)
	defer up.Close()

	d := t.TempDir()
	st, _ := store.Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "index.db"), model.ProviderAnthropic, 2)
	srv := New(testCfg(t, up.URL), st.Recorder)
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	resp, err := http.Post(ts.URL+"/v1/messages", "application/json", strings.NewReader(`{"model":"x"}`))
	if err != nil {
		t.Fatal(err)
	}
	got, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if string(got) != jsonBody {
		t.Errorf("non-SSE body mismatch: %q", got)
	}
	st.Close()

	rd, _ := store.OpenReader(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"), filepath.Join(d, "index.db"))
	defer rd.Close()
	rows, _ := rd.List(store.Filter{Limit: 10})
	if len(rows) != 1 || rows[0].State != model.StateComplete {
		t.Errorf("non-SSE not recorded complete: %+v", rows)
	}
}
