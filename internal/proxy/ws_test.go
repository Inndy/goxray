package proxy

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/store"
)

// fakeCodexUpstream answers each response.create with a short Responses
// stream. It counts the creates it actually received.
func fakeCodexUpstream(t *testing.T, creates *atomic.Int64) *httptest.Server {
	t.Helper()
	up := websocket.Upgrader{EnableCompression: true}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Upgrade") == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		c, err := up.Upgrade(w, r, http.Header{"X-Codex-Turn-State": {"ts-1"}})
		if err != nil {
			return
		}
		defer c.Close()
		n := 0
		for {
			_, data, err := c.ReadMessage()
			if err != nil {
				return
			}
			if !strings.Contains(string(data), `"response.create"`) {
				continue
			}
			creates.Add(1)
			n++
			id := "r" + string(rune('0'+n))
			for _, ev := range []string{
				`{"type":"response.created","response":{"id":"` + id + `"}}`,
				`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]}}`,
				`{"type":"response.completed","response":{"id":"` + id + `","model":"gpt-6.1-sol","status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`,
			} {
				if err := c.WriteMessage(websocket.TextMessage, []byte(ev)); err != nil {
					return
				}
			}
		}
	}))
}

func wsTestServer(t *testing.T, upstream string, open OpenStreamFunc) *httptest.Server {
	t.Helper()
	ts, _ := wsTestProxy(t, upstream, open)
	return ts
}

func wsTestProxy(t *testing.T, upstream string, open OpenStreamFunc) (*httptest.Server, *Server) {
	t.Helper()
	cfg := testCfg(t, "http://127.0.0.1:1")
	u, _ := url.Parse(upstream)
	cfg.ChatGPTUpstream = u
	srv := New(cfg, &fakeRec{})
	srv.SetStreams(open)
	return httptest.NewServer(srv.Handler()), srv
}

func dialCodex(t *testing.T, base string) (*websocket.Conn, *http.Response, error) {
	t.Helper()
	u := "ws" + strings.TrimPrefix(base, "http") + "/backend-api/codex/responses"
	h := http.Header{"Authorization": {"Bearer secret"}, "Session-Id": {"sess-ws"}}
	d := websocket.Dialer{EnableCompression: true}
	return d.Dial(u, h)
}

func create(prev string) []byte {
	p := ""
	if prev != "" {
		p = `"previous_response_id":"` + prev + `",`
	}
	return []byte(`{"type":"response.create","model":"gpt-6.1-sol",` + p + `"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}]}`)
}

// readTurn reads events until the turn's completion.
func readTurn(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	var got []string
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		_, data, err := c.ReadMessage()
		if err != nil {
			t.Fatalf("read: %v (after %v)", err, got)
		}
		got = append(got, string(data))
		if strings.Contains(string(data), `"response.completed"`) {
			return got
		}
	}
}

func TestWebSocketRecordsAndRelays(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()

	d := t.TempDir()
	st, err := store.Open(filepath.Join(d, "blobs"), filepath.Join(d, "manifests"),
		filepath.Join(d, "archives"), filepath.Join(d, "index.db"), model.ProviderAnthropic, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	srv := New(testCfg(t, "http://127.0.0.1:1"), st.Recorder)
	srv.cfg.ChatGPTUpstream, _ = url.Parse(up.URL)
	srv.SetStreams(func(c *model.Exchange) (StreamLog, error) { return st.Recorder.OpenStream(c) })
	ts := httptest.NewServer(srv.Handler())
	defer ts.Close()

	c, resp, err := dialCodex(t, ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	if resp.Header.Get("X-Codex-Turn-State") != "ts-1" {
		t.Errorf("upstream handshake headers were not relayed: %v", resp.Header)
	}
	for _, prev := range []string{"", "r1"} {
		if err := c.WriteMessage(websocket.TextMessage, create(prev)); err != nil {
			t.Fatal(err)
		}
		if ev := readTurn(t, c); len(ev) != 3 {
			t.Fatalf("got %d events, want 3", len(ev))
		}
	}
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	c.Close()

	// The connection settles asynchronously once both legs close.
	deadline := time.Now().Add(5 * time.Second)
	var turns, conns int
	for time.Now().Before(deadline) {
		turns, conns = 0, 0
		_ = st.Manifests.EachID(func(id string) error {
			ex, err := st.Manifests.Read(id)
			if err != nil || ex.State == model.StateInflight {
				return nil
			}
			if ex.Kind == model.KindWSConn {
				conns++
			} else if ex.WS != nil && ex.State == model.StateComplete && ex.Res.ResponseID != "" {
				turns++
			}
			return nil
		})
		if turns == 2 && conns == 1 {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if turns != 2 || conns != 1 {
		t.Fatalf("settled %d turns and %d connections, want 2 and 1", turns, conns)
	}
	if creates.Load() != 2 {
		t.Fatalf("upstream saw %d creates", creates.Load())
	}
}

// failingLog wraps nothing: every client frame fails to persist.
type failingLog struct {
	mu    sync.Mutex
	ended string
}

func (f *failingLog) Client([]byte, bool) error                    { return errors.New("disk full") }
func (f *failingLog) Server([]byte, bool) bool                     { return false }
func (f *failingLog) Commit() error                                { return nil }
func (f *failingLog) ShouldRotate() bool                           { return false }
func (f *failingLog) Upgraded(int, map[string]string) error        { return nil }
func (f *failingLog) CloseFrame(string, int, string)               {}
func (f *failingLog) Refused(int, map[string]string, []byte) error { return nil }
func (f *failingLog) End(how string, _, _ model.ExchangeState) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.ended = how
	return nil
}

func TestWebSocketFailClosedOnClientFrame(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	fl := &failingLog{}
	ts := wsTestServer(t, up.URL, func(*model.Exchange) (StreamLog, error) { return fl, nil })
	defer ts.Close()

	c, _, err := dialCodex(t, ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.WriteMessage(websocket.TextMessage, create(""))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	if err == nil {
		t.Fatal("the client got a frame after its request could not be recorded")
	}
	if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
		t.Fatalf("a persist failure must not look like a clean close: %v", err)
	}
	time.Sleep(100 * time.Millisecond)
	if creates.Load() != 0 {
		t.Fatal("an unrecorded request reached upstream")
	}
	fl.mu.Lock()
	defer fl.mu.Unlock()
	if fl.ended != EndPersistFailed {
		t.Fatalf("ended %q, want %q", fl.ended, EndPersistFailed)
	}
}

// gatedLog holds Commit until released, so the test can watch what the client
// receives in the meantime.
type gatedLog struct {
	failingLog
	gate     chan struct{}
	commits  atomic.Int64
	terminal atomic.Bool
}

func (g *gatedLog) Client([]byte, bool) error { return nil }
func (g *gatedLog) Server(p []byte, _ bool) bool {
	held := strings.Contains(string(p), `"response.completed"`)
	if held {
		g.terminal.Store(true)
	}
	return held
}
func (g *gatedLog) Commit() error { <-g.gate; g.commits.Add(1); return nil }

func TestWebSocketHoldsTerminalUntilCommit(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	g := &gatedLog{gate: make(chan struct{})}
	ts := wsTestServer(t, up.URL, func(*model.Exchange) (StreamLog, error) { return g, nil })
	defer ts.Close()

	c, _, err := dialCodex(t, ts.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	_ = c.WriteMessage(websocket.TextMessage, create(""))
	_ = c.SetReadDeadline(time.Now().Add(5 * time.Second))
	for i := 0; i < 2; i++ { // created, output_item.done
		if _, _, err := c.ReadMessage(); err != nil {
			t.Fatal(err)
		}
	}
	_ = c.SetReadDeadline(time.Now().Add(300 * time.Millisecond))
	if _, data, err := c.ReadMessage(); err == nil {
		t.Fatalf("terminal event released before commit: %s", data)
	}
	close(g.gate)
	// The read above timed out, which poisons a gorilla conn; dial-level check
	// is enough: commit ran once and only after the gate opened.
	deadline := time.Now().Add(2 * time.Second)
	for g.commits.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if g.commits.Load() != 1 {
		t.Fatalf("commits = %d", g.commits.Load())
	}
}

func TestWebSocketRefusedUpgradeIsRelayed(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	fl := &failingLog{}
	ts := wsTestServer(t, up.URL, func(*model.Exchange) (StreamLog, error) { return fl, nil })
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/backend-api/codex/responses"
	_, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer wrong"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("want the upstream's 401 relayed, got %v %v", resp, err)
	}
	if fl.ended != EndRefused {
		t.Fatalf("ended %q, want %q", fl.ended, EndRefused)
	}
}

// quietLog records everything without complaint.
type quietLog struct{ failingLog }

func (q *quietLog) Client([]byte, bool) error { return nil }

func TestWebSocketCloseStreamsSettlesAndRefusesNewOnes(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	ql := &quietLog{}
	ts, srv := wsTestProxy(t, up.URL, func(*model.Exchange) (StreamLog, error) { return ql, nil })
	defer ts.Close()

	c, _, err := dialCodex(t, ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer c.Close()
	srv.CloseStreams(time.Now().Add(3 * time.Second))
	// CloseStreams returns only once the connection is settled.
	ql.mu.Lock()
	ended := ql.ended
	ql.mu.Unlock()
	if ended != EndShutdown {
		t.Fatalf("ended %q when CloseStreams returned, want %q", ended, EndShutdown)
	}
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, err = c.ReadMessage()
	var ce *websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.CloseGoingAway {
		t.Fatalf("client saw %v, want 1001", err)
	}
	if _, resp, err := dialCodex(t, ts.URL); err == nil || resp == nil || resp.StatusCode != http.StatusServiceUnavailable {
		t.Fatalf("an upgrade after shutdown began: %v %v", resp, err)
	}
}

// refusingLog cannot record a refusal.
type refusingLog struct{ failingLog }

func (r *refusingLog) Refused(int, map[string]string, []byte) error { return errors.New("log full") }

func TestWebSocketUnrecordedRefusalIsNotRelayed(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	rl := &refusingLog{}
	ts := wsTestServer(t, up.URL, func(*model.Exchange) (StreamLog, error) { return rl, nil })
	defer ts.Close()

	u := "ws" + strings.TrimPrefix(ts.URL, "http") + "/backend-api/codex/responses"
	_, resp, err := websocket.DefaultDialer.Dial(u, http.Header{"Authorization": {"Bearer wrong"}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("want 502 for a refusal that was not recorded, got %v %v", resp, err)
	}
	if rl.ended != EndPersistFailed {
		t.Fatalf("ended %q, want %q", rl.ended, EndPersistFailed)
	}
}

// commitFailLog records client frames but cannot make anything durable after.
type commitFailLog struct{ quietLog }

func (c *commitFailLog) Commit() error { return errors.New("disk full") }

// A close frame is passed on only once it is durable; if it cannot be made so,
// the connection ends as a failed barrier, not a clean close.
func TestWebSocketCloseFrameIsDurableBeforeItIsForwarded(t *testing.T) {
	var creates atomic.Int64
	up := fakeCodexUpstream(t, &creates)
	defer up.Close()
	cl := &commitFailLog{}
	ts := wsTestServer(t, up.URL, func(*model.Exchange) (StreamLog, error) { return cl, nil })
	defer ts.Close()

	c, _, err := dialCodex(t, ts.URL)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	_ = c.WriteMessage(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""))
	_ = c.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, _, _ = c.ReadMessage()
	c.Close()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		cl.mu.Lock()
		ended := cl.ended
		cl.mu.Unlock()
		if ended != "" {
			if ended != EndPersistFailed {
				t.Fatalf("ended %q, want %q", ended, EndPersistFailed)
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("connection never ended")
}

func TestWebSocketWithoutRecordingAsksForHTTP(t *testing.T) {
	ts := wsTestServer(t, "http://127.0.0.1:1", nil)
	defer ts.Close()
	_, resp, err := dialCodex(t, ts.URL)
	if err == nil || resp == nil || resp.StatusCode != http.StatusUpgradeRequired {
		t.Fatalf("want 426 so Codex falls back to HTTP, got %v %v", resp, err)
	}
}
