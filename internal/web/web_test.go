package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"testing"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/store"
)

const testPort = 5600

// fakeReader is a stub store. Blobs are keyed by ref.
type fakeReader struct {
	rows     []model.Exchange
	sessions []store.Session
	threads  []store.Thread
	order    []store.Ordered
	blobs    map[string][]byte
}

func (f *fakeReader) Query(store.Filter) ([]model.Exchange, error)   { return f.rows, nil }
func (f *fakeReader) Sessions(store.Filter) ([]store.Session, error) { return f.sessions, nil }
func (f *fakeReader) Threads(string) ([]store.Thread, []store.Ordered, error) {
	return f.threads, f.order, nil
}
func (f *fakeReader) Page(store.Filter) ([]model.Exchange, []store.Ordered, error) {
	return f.rows, f.order, nil
}

func (f *fakeReader) Manifest(id string) (*model.Exchange, error) {
	for i := range f.rows {
		if f.rows[i].ID == id {
			return &f.rows[i], nil
		}
	}
	return nil, os.ErrNotExist
}

func (f *fakeReader) Blob(_, ref string) ([]byte, error) {
	if b, ok := f.blobs[ref]; ok {
		return b, nil
	}
	return nil, os.ErrNotExist
}

func newTestReader() *fakeReader {
	return &fakeReader{
		rows: []model.Exchange{{
			ID: "ex1", State: model.StateComplete, Method: "POST", URL: "/v1/messages",
			StartedAt: time.Now().UTC(), Status: 200,
			ReqRef: "r1", ResRef: "r2",
			ReqHeaders: map[string]string{"content-type": "application/json"},
			ResHeaders: map[string]string{"content-type": "text/event-stream"},
			Req:        model.ReqMeta{Model: "claude-opus-5[1m]", SessionID: "s1"},
		}},
		sessions: []store.Session{{SessionID: "s1", Turns: 1}},
		blobs: map[string][]byte{
			"r1": []byte(`{"model":"claude-opus-5[1m]","system":[` +
				`{"type":"text","text":"x-anthropic-billing-header: cc_version=2.1.220; cc_entrypoint=cli;"},` +
				`{"type":"text","text":"You are Claude Code."},` +
				`{"type":"text","text":"Base text\n\n# Environment\nPrimary working directory: /tmp\n"}` +
				`],"tools":[{"name":"Bash","description":"run"}],"messages":[{"role":"user","content":"hi"}],"max_tokens":100}`),
			"r2": []byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"),
		},
	}
}

func do(t *testing.T, h http.Handler, method, target string, hdr map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	r := httptest.NewRequest(method, target, nil)
	r.Host = "127.0.0.1:5600"
	for k, v := range hdr {
		if k == "Host" {
			r.Host = v
			continue
		}
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

// ── the security boundary ───────────────────────────────────────────

// A rebound DNS name resolves to loopback but cannot forge the Host header,
// so rejecting a non-loopback Host is what actually stops the attack.
func TestGuardRejectsNonLoopbackHost(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	for _, host := range []string{
		"evil.com",
		"evil.com:5600",
		"goxray.internal:5600",
		"192.168.1.10:5600",
		"",
	} {
		w := do(t, h, "GET", "/api/sessions", map[string]string{"Host": host})
		if w.Code != http.StatusForbidden {
			t.Errorf("Host %q: status %d, want 403", host, w.Code)
		}
	}
}

// A loopback host on the wrong port suggests something is fronting us.
func TestGuardRejectsWrongPort(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	w := do(t, h, "GET", "/api/sessions", map[string]string{"Host": "127.0.0.1:9999"})
	if w.Code != http.StatusForbidden {
		t.Errorf("status %d, want 403", w.Code)
	}
}

func TestGuardAllowsLoopbackHosts(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	for _, host := range []string{"127.0.0.1:5600", "localhost:5600", "[::1]:5600"} {
		w := do(t, h, "GET", "/api/sessions", map[string]string{"Host": host})
		if w.Code != http.StatusOK {
			t.Errorf("Host %q: status %d, want 200", host, w.Code)
		}
	}
}

func TestGuardRejectsRemoteOrigin(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	for _, origin := range []string{"https://evil.com", "http://evil.com:5600", "null"} {
		w := do(t, h, "GET", "/api/sessions", map[string]string{"Origin": origin})
		if w.Code != http.StatusForbidden {
			t.Errorf("Origin %q: status %d, want 403", origin, w.Code)
		}
	}
}

// A dev server on another loopback port is local code, not the threat model.
func TestGuardAllowsLoopbackOrigin(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	w := do(t, h, "GET", "/api/sessions", map[string]string{"Origin": "http://localhost:5173"})
	if w.Code != http.StatusOK {
		t.Errorf("status %d, want 200", w.Code)
	}
}

// CORS headers would let a remote page read responses; we must never send them.
func TestNoCORSHeadersEver(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	for _, target := range []string{"/api/sessions", "/api/exchanges", "/api/exchange/ex1", "/", "/nope"} {
		w := do(t, h, "GET", target, nil)
		for k := range w.Header() {
			if len(k) >= 22 && k[:22] == "Access-Control-Allow-A" {
				t.Errorf("%s leaked %s", target, k)
			}
			if k == "Access-Control-Allow-Origin" || k == "Access-Control-Allow-Credentials" {
				t.Errorf("%s leaked %s", target, k)
			}
		}
		if w.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Errorf("%s missing nosniff", target)
		}
	}
}

func TestHTMLGetsCSP(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	w := do(t, h, "GET", "/", nil)
	if got := w.Header().Get("Content-Security-Policy"); got == "" {
		t.Error("index.html served without a CSP")
	}
}

// A recorded body must never be servable as markup the browser will execute.
func TestRawDowngradesUnknownContentType(t *testing.T) {
	rd := newTestReader()
	rd.rows[0].ReqHeaders["content-type"] = "text/html"
	h := Handler(rd, NewHub(), testPort)
	w := do(t, h, "GET", "/api/exchange/ex1/raw?side=req", nil)
	if got := w.Header().Get("Content-Type"); got != "text/plain; charset=utf-8" {
		t.Errorf("content-type = %q, want text/plain", got)
	}
}

// ── API behavior ────────────────────────────────────────────────────

func TestExchangeDetail(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	w := do(t, h, "GET", "/api/exchange/ex1", nil)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
	var d detail
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.CCVersion != "2.1.220" {
		t.Errorf("cc_version = %q", d.CCVersion)
	}
	if d.CtxWindow != 1_000_000 {
		t.Errorf("ctx_window = %d, want 1000000 for a [1m] model", d.CtxWindow)
	}
	if len(d.System) == 0 {
		t.Error("no system blocks")
	}
	if len(d.Tools.Core) != 1 || d.Tools.Core[0].Name != "Bash" {
		t.Errorf("tools = %+v", d.Tools)
	}
	if d.Tokens.Total == 0 {
		t.Error("no token breakdown")
	}
	if len(d.Messages) == 0 {
		t.Error("messages not passed through")
	}
	if d.Response == nil || len(d.Response.Events) != 1 {
		t.Errorf("response events = %+v", d.Response)
	}
	if _, ok := d.Options["max_tokens"]; !ok {
		t.Errorf("options = %+v", d.Options)
	}
}

// The window is recorded per turn so a listing row has a denominator without
// fetching a body, and the detail view must agree with the row it was opened
// from even when the body is gone.
func TestCtxWindowComesFromTheRecordedTurn(t *testing.T) {
	rd := newTestReader()
	rd.rows[0].Req.Model = "claude-opus-5" // bare id: only the record knows the window
	rd.rows[0].Req.CtxWindow = 1_000_000
	h := Handler(rd, NewHub(), testPort)

	var list struct {
		Exchanges []model.Exchange `json:"exchanges"`
	}
	w := do(t, h, "GET", "/api/exchanges", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &list); err != nil {
		t.Fatal(err)
	}
	if len(list.Exchanges) != 1 || list.Exchanges[0].Req.CtxWindow != 1_000_000 {
		t.Errorf("listing lost the window: %+v", list.Exchanges)
	}

	delete(rd.blobs, "r1")
	var d detail
	w = do(t, h, "GET", "/api/exchange/ex1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.CtxWindow != 1_000_000 {
		t.Errorf("ctx_window = %d, want the recorded 1000000 with no request body", d.CtxWindow)
	}
}

// A turn's own reply is not in its request, so without the assembled message
// the newest turn's output would be visible nowhere but the raw bytes.
func TestExchangeDetailAssemblesTheReply(t *testing.T) {
	rd := newTestReader()
	rd.blobs["r2"] = []byte(
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"\"}}\n\n" +
			"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n" +
			"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
			"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n")

	var d detail
	w := do(t, Handler(rd, NewHub(), testPort), "GET", "/api/exchange/ex1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Response == nil || d.Response.Message == nil {
		t.Fatalf("no assembled message: %+v", d.Response)
	}
	m := d.Response.Message
	if m.Role != "assistant" || m.StopReason != "end_turn" || m.Truncated {
		t.Errorf("message header = %+v", m)
	}
	if len(m.Content) != 1 {
		t.Fatalf("want 1 content block, got %d", len(m.Content))
	}
	var b struct {
		Type, Text string
	}
	if err := json.Unmarshal(m.Content[0], &b); err != nil {
		t.Fatal(err)
	}
	if b.Type != "text" || b.Text != "hello" {
		t.Errorf("block = %+v", b)
	}
}

func TestExchangeDetailAssemblesNonStreamingReply(t *testing.T) {
	rd := newTestReader()
	rd.rows[0].ResHeaders = map[string]string{"content-type": "application/json"}
	rd.blobs["r2"] = []byte(`{"role":"assistant","stop_reason":"tool_use","content":[{"type":"tool_use","id":"t1","name":"Bash","input":{"command":"ls"}}]}`)

	var d detail
	w := do(t, Handler(rd, NewHub(), testPort), "GET", "/api/exchange/ex1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if d.Response == nil || d.Response.Message == nil {
		t.Fatalf("no assembled message: %+v", d.Response)
	}
	if len(d.Response.Message.Content) != 1 {
		t.Errorf("content = %s", d.Response.Message.Content)
	}
}

func TestExchangeNotFound(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	if w := do(t, h, "GET", "/api/exchange/nope", nil); w.Code != http.StatusNotFound {
		t.Errorf("status %d, want 404", w.Code)
	}
}

func TestRawRejectsBadSide(t *testing.T) {
	h := Handler(newTestReader(), NewHub(), testPort)
	if w := do(t, h, "GET", "/api/exchange/ex1/raw?side=etc", nil); w.Code != http.StatusBadRequest {
		t.Errorf("status %d, want 400", w.Code)
	}
}

// An in-flight turn is not in the index yet, so the list must overlay it or a
// page load during a live turn would show nothing.
func TestExchangesMergesInflight(t *testing.T) {
	rd := newTestReader()
	hub := NewHub()
	hub.OnExchange(model.Exchange{ID: "live1", State: model.StateInflight, StartedAt: time.Now().UTC()})
	h := Handler(rd, hub, testPort)

	w := do(t, h, "GET", "/api/exchanges", nil)
	var got struct{ Exchanges []model.Exchange }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Exchanges) != 2 {
		t.Fatalf("got %d exchanges, want 2", len(got.Exchanges))
	}
	if got.Exchanges[0].ID != "live1" {
		t.Errorf("newest first: got %q", got.Exchanges[0].ID)
	}

	// Committing the same id must clear it from the inflight overlay, not double it.
	hub.OnExchange(model.Exchange{ID: "live1", State: model.StateComplete, StartedAt: time.Now().UTC()})
	w = do(t, h, "GET", "/api/exchanges", nil)
	got.Exchanges = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Exchanges) != 1 {
		t.Errorf("after commit got %d exchanges, want 1", len(got.Exchanges))
	}
}

// A session's index rows appear when its responses commit, so a session whose
// first turn is still streaming exists nowhere in the rollup. Without the overlay
// the pane stays empty for the whole first turn while the turn list already shows
// it, and no amount of refetching helps.
func TestSessionsMergesInflight(t *testing.T) {
	rd := newTestReader()
	hub := NewHub()
	now := time.Now().UTC()
	hub.OnExchange(model.Exchange{
		ID: "live1", State: model.StateInflight, StartedAt: now,
		Source: "cli", Req: model.ReqMeta{SessionID: "s2", Model: "claude-opus-5", Cwd: "/tmp/new"},
	})
	// A second turn in a session the index already knows must add to it, not
	// create a duplicate row.
	hub.OnExchange(model.Exchange{
		ID: "live2", State: model.StateInflight, StartedAt: now.Add(-time.Second),
		Req: model.ReqMeta{SessionID: "s1"},
	})
	h := Handler(rd, hub, testPort)

	w := do(t, h, "GET", "/api/sessions", nil)
	var got struct{ Sessions []store.Session }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 {
		t.Fatalf("got %d sessions, want 2: %+v", len(got.Sessions), got.Sessions)
	}
	// Most recently active first, and s2's only activity is the streaming turn.
	s2 := got.Sessions[0]
	if s2.SessionID != "s2" {
		t.Fatalf("newest first: got %q", s2.SessionID)
	}
	if s2.Turns != 1 || s2.Cwd != "/tmp/new" || s2.Model != "claude-opus-5" {
		t.Errorf("inflight session = %+v", s2)
	}
	if len(s2.Sources) != 1 || s2.Sources[0] != "cli" {
		t.Errorf("sources = %+v", s2.Sources)
	}
	if s1 := got.Sessions[1]; s1.SessionID != "s1" || s1.Turns != 2 {
		t.Errorf("known session should gain the inflight turn, got %+v", s1)
	}

	// Committing clears the overlay; the index (stubbed at 1 turn) is truth again.
	hub.OnExchange(model.Exchange{ID: "live1", State: model.StateComplete, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s2"}})
	hub.OnExchange(model.Exchange{ID: "live2", State: model.StateComplete, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s1"}})
	w = do(t, h, "GET", "/api/sessions", nil)
	got.Sessions = nil
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 || got.Sessions[0].Turns != 1 {
		t.Errorf("after commit got %+v, want just the indexed s1", got.Sessions)
	}
}

// A thread filter must hide another agent's streaming turn exactly as the index
// query hides its committed ones.
func TestExchangesInflightRespectsThreadFilter(t *testing.T) {
	rd := newTestReader()
	hub := NewHub()
	now := time.Now().UTC()
	hub.OnExchange(model.Exchange{ID: "live-root", State: model.StateInflight, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s1", Thread: "t-root"}})
	hub.OnExchange(model.Exchange{ID: "live-child", State: model.StateInflight, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s1", Thread: "t-child"}})
	h := Handler(rd, hub, testPort)

	w := do(t, h, "GET", "/api/exchanges?session=s1&thread=t-child", nil)
	var got struct{ Exchanges []model.Exchange }
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	for _, e := range got.Exchanges {
		if e.ID == "live-root" {
			t.Errorf("another thread's inflight turn leaked through the filter")
		}
	}
	if !slices.ContainsFunc(got.Exchanges, func(e model.Exchange) bool { return e.ID == "live-child" }) {
		t.Errorf("the filtered thread's inflight turn is missing: %+v", got.Exchanges)
	}
}

// The agent summary marks a thread running while one of its turns streams, and
// shows a thread whose first turn has not committed yet.
func TestThreadsOverlaysInflight(t *testing.T) {
	rd := newTestReader()
	rd.threads = []store.Thread{{Thread: "t-root", Agent: "/root", Turns: 3, FirstTS: 1}}
	hub := NewHub()
	now := time.Now().UTC()
	hub.OnExchange(model.Exchange{ID: "live-child", State: model.StateInflight, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s1", Thread: "t-child", ParentThread: "t-root", Agent: "/root/task"}})
	hub.OnExchange(model.Exchange{ID: "live-other", State: model.StateInflight, StartedAt: now,
		Req: model.ReqMeta{SessionID: "s2", Thread: "t-elsewhere"}})
	h := Handler(rd, hub, testPort)

	var got struct{ Threads []store.Thread }
	w := do(t, h, "GET", "/api/threads?session=s1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Threads) != 2 || got.Threads[0].Thread != "t-root" || got.Threads[0].Running {
		t.Fatalf("threads = %+v", got.Threads)
	}
	c := got.Threads[1]
	if c.Thread != "t-child" || !c.Running || c.Turns != 1 || c.LastState != "inflight" || c.ParentThread != "t-root" {
		t.Errorf("inflight child = %+v", c)
	}
}

// A session listing says where each row stands in the whole session, so a page
// of the newest turns, or one thread's, is not numbered from 1.
func TestExchangesCarrySessionOrdinals(t *testing.T) {
	rd := newTestReader()
	rd.order = []store.Ordered{{ID: "older", TS: 1}, {ID: "ex1", TS: 2}}
	h := Handler(rd, nil, testPort)
	var got struct {
		Seq    map[string]int `json:"seq"`
		SeqMax int            `json:"seq_max"`
	}
	w := do(t, h, "GET", "/api/exchanges?session=s1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Seq["ex1"] != 2 || len(got.Seq) != 1 || got.SeqMax != 2 {
		t.Errorf("seq = %v, max %d; want only the rows sent, and the session's count", got.Seq, got.SeqMax)
	}
}

// A turn that has finished but whose index row is not written yet is neither
// inflight nor indexed. The summary must still count it, and a listing must
// still number it in start order among the others, not after them.
func TestThreadsAndSeqCountTheNotYetIndexed(t *testing.T) {
	rd := newTestReader()
	t0 := time.UnixMilli(1_000_000).UTC()
	rd.threads = []store.Thread{{Thread: "t-root", Agent: "/root", Turns: 1, Cost: 1, FirstTS: 1, LastState: "complete", LastTurnTS: t0.Add(2 * time.Second).UnixMilli()}}
	rd.order = []store.Ordered{{ID: "ex1", TS: t0.Add(2 * time.Second).UnixMilli()}}
	hub := NewHub()
	early := model.Exchange{ID: "early", State: model.StateInflight, StartedAt: t0.Add(time.Second),
		Req: model.ReqMeta{SessionID: "s1", Thread: "t-child", ParentThread: "t-root", Agent: "/root/task"}}
	hub.OnExchange(early)
	early.State, early.EndedAt, early.Res.Cost = model.StateComplete, t0.Add(3*time.Second), 0.5
	hub.OnExchange(early) // committed; the index has not written it
	h := Handler(rd, hub, testPort)

	var th struct{ Threads []store.Thread }
	if err := json.Unmarshal(do(t, h, "GET", "/api/threads?session=s1", nil).Body.Bytes(), &th); err != nil {
		t.Fatal(err)
	}
	if len(th.Threads) != 2 || th.Threads[1].Turns != 1 || th.Threads[1].Cost != 0.5 ||
		th.Threads[1].LastState != "complete" || th.Threads[1].Running {
		t.Fatalf("threads = %+v", th.Threads)
	}

	var ex struct {
		Exchanges []model.Exchange
		Seq       map[string]int `json:"seq"`
	}
	if err := json.Unmarshal(do(t, h, "GET", "/api/exchanges?session=s1", nil).Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.Seq["early"] != 1 || ex.Seq["ex1"] != 2 {
		t.Errorf("seq = %v, want the earlier-started turn first", ex.Seq)
	}
	if !slices.ContainsFunc(ex.Exchanges, func(e model.Exchange) bool { return e.ID == "early" }) {
		t.Errorf("the not-yet-indexed turn is missing from the listing")
	}
	// Filtered to another thread, the not-yet-indexed turn is not listed but
	// still has its number, since seq_max counts it.
	ex.Seq = nil
	if err := json.Unmarshal(do(t, h, "GET", "/api/exchanges?session=s1&thread=t-root", nil).Body.Bytes(), &ex); err != nil {
		t.Fatal(err)
	}
	if ex.Seq["early"] != 1 {
		t.Errorf("filtered seq = %v, want the hidden pending turn numbered", ex.Seq)
	}

	// Once the index has it, it is counted once.
	rd.threads[0].Turns = 1
	rd.threads = append(rd.threads, store.Thread{Thread: "t-child", ParentThread: "t-root", Agent: "/root/task", Turns: 1, Cost: 0.5, FirstTS: 2})
	rd.order = append(rd.order, store.Ordered{ID: "early", TS: t0.Add(time.Second).UnixMilli()})
	th.Threads = nil
	if err := json.Unmarshal(do(t, h, "GET", "/api/threads?session=s1", nil).Body.Bytes(), &th); err != nil {
		t.Fatal(err)
	}
	for _, x := range th.Threads {
		if x.Thread == "t-child" && x.Turns != 1 {
			t.Errorf("indexed and settled counted twice: %+v", x)
		}
	}
}

// A noise turn must not conjure a session the pane is filtered to hide.
func TestSessionsInflightRespectsNoiseFilter(t *testing.T) {
	rd := newTestReader()
	hub := NewHub()
	hub.OnExchange(model.Exchange{
		ID: "live1", State: model.StateInflight, StartedAt: time.Now().UTC(), IsNoise: true,
		Req: model.ReqMeta{SessionID: "s2"},
	})
	h := Handler(rd, hub, testPort)

	var got struct{ Sessions []store.Session }
	w := do(t, h, "GET", "/api/sessions", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 1 {
		t.Errorf("noise leaked into the default view: %+v", got.Sessions)
	}
	got.Sessions = nil
	w = do(t, h, "GET", "/api/sessions?noise=1", nil)
	if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Sessions) != 2 {
		t.Errorf("noise=1 should show it: %+v", got.Sessions)
	}
}

// A proxy nobody is watching never reads the hub, so finished exchanges must be
// let go on the write side too, or the hub keeps every exchange it ever saw.
func TestHubPrunesSettledWithoutReaders(t *testing.T) {
	hub := NewHub()
	hub.settled["old"] = settledExchange{at: time.Now().Add(-2 * settledTTL)}
	hub.pruned = time.Now().Add(-2 * settledPruneEvery)
	hub.OnExchange(model.Exchange{ID: "new", State: model.StateComplete, StartedAt: time.Now()})
	if _, ok := hub.settled["old"]; ok {
		t.Error("an expired settled exchange survived a write")
	}
	if _, ok := hub.settled["new"]; !ok {
		t.Error("the finished exchange was not kept")
	}
}

// ── the hub must never stall the recording path ─────────────────────

func TestHubDoesNotBlockOnSaturatedClient(t *testing.T) {
	hub := NewHub()
	_, cancel := hub.subscribe()
	defer cancel()

	done := make(chan struct{})
	go func() {
		defer close(done)
		// Far more than clientBuffer, with nobody draining.
		for i := range clientBuffer * 10 {
			hub.OnExchange(model.Exchange{
				ID:        "x" + string(rune('a'+i%26)),
				State:     model.StateComplete,
				StartedAt: time.Now().UTC(),
			})
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("OnExchange blocked on a saturated client — this would stall the proxy")
	}
}

func TestHubInflightExpires(t *testing.T) {
	hub := NewHub()
	hub.OnExchange(model.Exchange{
		ID: "stale", State: model.StateInflight,
		StartedAt: time.Now().Add(-inflightTTL - time.Minute).UTC(),
	})
	if got := hub.Inflight(); len(got) != 0 {
		t.Errorf("stale inflight not evicted: %+v", got)
	}
}

func TestEventsUnavailableWithoutHub(t *testing.T) {
	h := Handler(newTestReader(), nil, testPort)
	if w := do(t, h, "GET", "/api/events", nil); w.Code != http.StatusServiceUnavailable {
		t.Errorf("status %d, want 503", w.Code)
	}
}
