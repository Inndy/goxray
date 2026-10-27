// Package web serves the read-only goxray dashboard: a JSON API over recorded
// exchanges plus the embedded single-page app that renders it.
//
// It reaches the store through the narrow Reader interface below, so nothing in
// this package can write to the blob store, the manifests, or the index. The
// dashboard runs on its own port and its own http.Server; the proxy's
// fail-closed recording path shares only read access with it.
package web

import (
	"bytes"
	"cmp"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"go.inndy.tw/goxray/internal/model"
	"go.inndy.tw/goxray/internal/pricing"
	"go.inndy.tw/goxray/internal/store"
	"go.inndy.tw/goxray/internal/view"
	"go.inndy.tw/goxray/internal/wire"
)

// Reader is the read-only slice of the store the dashboard needs. Both
// *store.Reader and the in-process adapter over *store.Store satisfy it.
type Reader interface {
	Query(f store.Filter) ([]model.Exchange, error)
	Sessions(f store.Filter) ([]store.Session, error)
	Threads(session string) ([]store.Thread, []store.Ordered, error)
	Page(f store.Filter) ([]model.Exchange, []store.Ordered, error)
	Manifest(id string) (*model.Exchange, error)
	// Blob takes the exchange's session id alongside the ref: archived bodies
	// live in a per-session file, and the handler has already read the manifest.
	Blob(session, ref string) ([]byte, error)
}

// storeReader adapts the live *store.Store to the read-only Reader, so the
// handlers cannot reach Recorder or Index writes even by accident.
type storeReader struct{ st *store.Store }

// FromStore exposes a running store to the dashboard read-only.
func FromStore(st *store.Store) Reader { return storeReader{st} }

func (s storeReader) Query(f store.Filter) ([]model.Exchange, error) { return s.st.Index.Query(f) }
func (s storeReader) Sessions(f store.Filter) ([]store.Session, error) {
	return s.st.Index.Sessions(f)
}
func (s storeReader) Threads(session string) ([]store.Thread, []store.Ordered, error) {
	return s.st.Index.Threads(session)
}
func (s storeReader) Page(f store.Filter) ([]model.Exchange, []store.Ordered, error) {
	return s.st.Index.Page(f)
}
func (s storeReader) Manifest(id string) (*model.Exchange, error) { return s.st.Manifests.Read(id) }
func (s storeReader) Blob(session, ref string) ([]byte, error) {
	return s.st.Blobs.Get(session, ref)
}

const (
	defaultLimit = 200
	maxLimit     = 2000
)

type server struct {
	rd  Reader
	hub *Hub
}

// Handler builds the dashboard handler. hub may be nil, in which case live
// events and inflight exchanges are simply unavailable (the case for a
// read-only reader detached from a running proxy).
func Handler(rd Reader, hub *Hub, uiPort int) http.Handler {
	s := &server{rd: rd, hub: hub}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/sessions", s.handleSessions)
	mux.HandleFunc("GET /api/exchanges", s.handleExchanges)
	mux.HandleFunc("GET /api/threads", s.handleThreads)
	mux.HandleFunc("GET /api/exchange/{id}", s.handleExchange)
	mux.HandleFunc("GET /api/exchange/{id}/raw", s.handleRaw)
	mux.HandleFunc("GET /api/events", s.handleEvents)
	mux.Handle("/", spaHandler())
	return recoverer(guard(uiPort, mux))
}

// ── helpers ─────────────────────────────────────────────────────────

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(v); err != nil {
		// Response already begun; nothing useful left to say to the client.
		return
	}
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	w.Header().Set("content-type", "application/json; charset=utf-8")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func filterFrom(r *http.Request) store.Filter {
	q := r.URL.Query()
	f := store.Filter{
		Session:      q.Get("session"),
		Thread:       q.Get("thread"),
		Source:       q.Get("source"),
		Limit:        defaultLimit,
		IncludeNoise: q.Get("noise") == "1",
	}
	if n, err := strconv.Atoi(q.Get("limit")); err == nil && n > 0 {
		f.Limit = min(n, maxLimit)
	}
	if n, err := strconv.Atoi(q.Get("offset")); err == nil && n > 0 {
		f.Offset = n
	}
	if n, err := strconv.ParseInt(q.Get("since"), 10, 64); err == nil && n > 0 {
		f.Since = n
	}
	return f
}

// ── handlers ────────────────────────────────────────────────────────

func (s *server) handleSessions(w http.ResponseWriter, r *http.Request) {
	f := filterFrom(r)
	f.Limit = 0 // sessions are already an aggregate; don't truncate them
	sessions, err := s.rd.Sessions(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"sessions": s.mergeInflightSessions(sessions, f)})
}

// mergeInflightSessions overlays sessions that exist only as streaming turns, on
// the same grounds as mergeInflight: the rollup is a GROUP BY over the index, and
// an index row appears when a response commits. Without this a brand-new session
// is missing from the pane for the whole duration of its first turn — while that
// very turn is already in the turn list, which does overlay inflight.
//
// Usage and cost stay at whatever the index reported, which for a first turn is
// zero: nothing has told us yet. Undercounting a turn in progress is the honest
// answer, and the row corrects itself the moment the response commits.
func (s *server) mergeInflightSessions(rows []store.Session, f store.Filter) []store.Session {
	if s.hub == nil {
		return rows
	}
	live := s.hub.Inflight()
	if len(live) == 0 {
		return rows
	}
	at := make(map[string]int, len(rows))
	for i := range rows {
		at[rows[i].SessionID] = i
	}
	for _, e := range live {
		if !f.Matches(&e) {
			continue
		}
		ts := e.StartedAt.UnixMilli()
		i, ok := at[e.Req.SessionID]
		if !ok {
			// Sources is never nil: a Go nil slice marshals to JSON null and blanks
			// the pane, same as in querySessions.
			rows = append(rows, store.Session{SessionID: e.Req.SessionID, FirstTS: ts, Sources: []string{}})
			i = len(rows) - 1
			at[e.Req.SessionID] = i
		}
		r := &rows[i]
		r.Turns++
		if e.Req.IsSubagent {
			r.Subagents++
		}
		if ts > r.LastTS {
			r.LastTS = ts
		}
		if ts < r.FirstTS {
			r.FirstTS = ts
		}
		if r.Model == "" {
			r.Model = e.Req.Model
		}
		if r.Cwd == "" {
			r.Cwd = e.Req.Cwd
		}
		if e.Source != "" && !slices.Contains(r.Sources, e.Source) {
			r.Sources = append(r.Sources, e.Source)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].LastTS > rows[j].LastTS })
	return rows
}

// handleThreads serves the agent summary of one session. It is an aggregate
// over the whole session, not over the page of turns the list holds: a summary
// built from a capped page undercounts a long session, and one built from a
// thread-filtered page cannot see the other agents at all.
func (s *server) handleThreads(w http.ResponseWriter, r *http.Request) {
	sid := r.URL.Query().Get("session")
	rows := []store.Thread{}
	if sid != "" {
		threads, order, err := s.rd.Threads(sid)
		if err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if threads != nil {
			rows = threads
		}
		rows = s.live().overlayThreads(rows, sid, indexedIDs(order))
	}
	writeJSON(w, map[string]any{"threads": rows})
}

func indexedIDs(order []store.Ordered) map[string]bool {
	out := make(map[string]bool, len(order))
	for _, o := range order {
		out[o.ID] = true
	}
	return out
}

// liveView is one reading of the hub, taken once per request so every overlay
// in a response agrees on what is inflight and what has settled.
type liveView struct{ inflight, settled []model.Exchange }

func (s *server) live() liveView {
	if s.hub == nil {
		return liveView{}
	}
	i, st := s.hub.Snapshot()
	return liveView{i, st}
}

// pending returns a session's exchanges the index does not hold yet: those
// still streaming, and those that finished but whose row the index worker has
// not written. The second kind is why this is not just the inflight set — the
// dashboard hears of a commit first, and a rollup taken in between would miss
// the turn with nothing prompting it to look again.
func (v liveView) pending(sid string, indexed map[string]bool) []model.Exchange {
	var out []model.Exchange
	for _, set := range [][]model.Exchange{v.inflight, v.settled} {
		for _, e := range set {
			if e.Req.SessionID == sid && !indexed[e.ID] {
				out = append(out, e)
			}
		}
	}
	return out
}

// overlayThreads adds what the index has not caught up with to the rollup: a
// thread with a turn streaming is running, and a turn it has not stored yet is
// counted, on the same grounds as mergeInflightSessions.
func (v liveView) overlayThreads(rows []store.Thread, sid string, indexed map[string]bool) []store.Thread {
	at := make(map[string]int, len(rows))
	for i := range rows {
		at[rows[i].Thread] = i
	}
	get := func(e model.Exchange) *store.Thread {
		i, ok := at[e.Req.Thread]
		if !ok {
			ts := e.StartedAt.UnixMilli()
			rows = append(rows, store.Thread{Thread: e.Req.Thread, FirstTS: ts, LastTS: ts})
			i = len(rows) - 1
			at[e.Req.Thread] = i
		}
		t := &rows[i]
		t.ParentThread = cmp.Or(t.ParentThread, e.Req.ParentThread)
		t.Agent = cmp.Or(t.Agent, e.Req.Agent)
		return t
	}
	for _, e := range v.inflight {
		if e.Req.SessionID == sid && e.Req.Thread != "" {
			get(e).Running = true
		}
	}
	for _, e := range v.pending(sid, indexed) {
		if e.Req.Thread == "" {
			continue
		}
		t := get(e)
		ts := e.StartedAt.UnixMilli()
		t.FirstTS = min(t.FirstTS, ts)
		t.LastTS = max(t.LastTS, ts, e.EndedAt.UnixMilli())
		if e.IsNoise {
			continue
		}
		t.Turns++
		t.Cost += e.Res.Cost
		if ts >= t.LastTurnTS {
			t.LastTurnTS, t.LastState = ts, string(e.State)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].FirstTS < rows[j].FirstTS })
	return rows
}

func (s *server) handleExchanges(w http.ResponseWriter, r *http.Request) {
	f := filterFrom(r)
	// Page reads the rows and, inside a session, the session's indexed turns
	// from one snapshot, so a row indexed mid-request is in both or neither.
	rows, order, err := s.rd.Page(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	lv := s.live()
	rows = mergeInflight(rows, f, lv.inflight)
	out := map[string]any{"exchanges": rows}
	if f.Session != "" {
		indexed := indexedIDs(order)
		seq, n := lv.sessionSeq(f, order, indexed)
		// A finished turn the index has not stored yet is listed too, or a
		// reload in that moment would drop it from the list.
		have := make(map[string]bool, len(rows))
		for _, e := range rows {
			have[e.ID] = true
		}
		for _, e := range lv.pending(f.Session, indexed) {
			if !have[e.ID] && f.Matches(&e) {
				rows = append(rows, e)
				have[e.ID] = true
			}
		}
		sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
		// The rows sent, plus every turn the index has not caught up with, listed
		// or not: seq_max counts those, so a client filtered to one thread must
		// know their numbers or it would number one again when it streams in.
		sent := make(map[string]int, len(rows))
		for _, e := range rows {
			if k, ok := seq[e.ID]; ok {
				sent[e.ID] = k
			}
		}
		for _, e := range lv.pending(f.Session, indexed) {
			if k, ok := seq[e.ID]; ok {
				sent[e.ID] = k
			}
		}
		out["exchanges"], out["seq"], out["seq_max"] = rows, sent, n
	}
	writeJSON(w, out)
}

// sessionSeq numbers a session's turns in the order they started: the indexed
// ones and the ones the index has not caught up with, ranked together, so a
// turn still streaming takes its place among the others instead of being put
// after every indexed one. Only the noise filter applies; the thread filter
// and the page do not, which is what keeps a number the same however the list
// is narrowed.
func (v liveView) sessionSeq(f store.Filter, order []store.Ordered, indexed map[string]bool) (map[string]int, int) {
	all := append([]store.Ordered(nil), order...)
	for _, e := range v.pending(f.Session, indexed) {
		if f.IncludeNoise || !e.IsNoise {
			all = append(all, store.Ordered{ID: e.ID, TS: e.StartedAt.UnixMilli()})
		}
	}
	sort.SliceStable(all, func(i, j int) bool {
		if all[i].TS != all[j].TS {
			return all[i].TS < all[j].TS
		}
		return all[i].ID < all[j].ID
	})
	seq := make(map[string]int, len(all))
	for i, o := range all {
		seq[o.ID] = i + 1
	}
	return seq, len(all)
}

// mergeInflight overlays exchanges that are still streaming. They exist as
// manifests but not yet in the index — the index upsert happens when the
// response commits — so without this a turn would only be visible to clients
// that were already listening on /api/events when it started.
func mergeInflight(rows []model.Exchange, f store.Filter, live []model.Exchange) []model.Exchange {
	if len(live) == 0 {
		return rows
	}
	seen := make(map[string]struct{}, len(rows))
	for _, e := range rows {
		seen[e.ID] = struct{}{}
	}
	for _, e := range live {
		if _, dup := seen[e.ID]; dup {
			continue
		}
		if !f.Matches(&e) {
			continue
		}
		rows = append(rows, e)
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].StartedAt.After(rows[j].StartedAt) })
	return rows
}

// detail is the per-exchange payload. Derived views sit beside the raw message
// array rather than replacing it: the frontend shapes messages into steps, and
// the verbatim bytes stay one request away at /api/exchange/{id}/raw.
type detail struct {
	Exchange  *model.Exchange            `json:"exchange"`
	CCVersion string                     `json:"cc_version,omitempty"`
	CtxWindow int                        `json:"ctx_window"`
	System    []view.Block               `json:"system"`
	Tools     view.ToolSummary           `json:"tools"`
	Tokens    view.TokenBreakdown        `json:"tokens"`
	Options   map[string]json.RawMessage `json:"options,omitempty"`
	Messages  json.RawMessage            `json:"messages,omitempty"`
	Response  *responseView              `json:"response,omitempty"`
	Warnings  []string                   `json:"warnings,omitempty"`
}

type responseView struct {
	ContentType string          `json:"content_type,omitempty"`
	Bytes       int             `json:"bytes"`
	Events      []wire.SSEEvent `json:"events,omitempty"`
	JSON        json.RawMessage `json:"json,omitempty"`
	Text        string          `json:"text,omitempty"`
	// Message is the reply reassembled into the same shape as a request
	// message, so the timeline can show what the model actually did this turn.
	// Without it a turn's own output appears nowhere until the next request
	// quotes it back — and the newest turn never gets one.
	Message *wire.AssembledMessage `json:"message,omitempty"`
}

func (s *server) handleExchange(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ex, err := s.rd.Manifest(id)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "no such exchange")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}

	// The recorded window is what a listing row shows, so prefer it here too and
	// let the request body correct it below: a detail view that disagreed with the
	// row it was opened from would read as one of the two being wrong.
	d := detail{Exchange: ex, CtxWindow: ex.Req.CtxWindow}
	if ex.Provider == model.ProviderOpenAI {
		s.openaiDetail(&d)
		writeJSON(w, d)
		return
	}
	if d.CtxWindow == 0 {
		d.CtxWindow = wire.CtxWindow(ex.Req.Model, nil)
	}

	if ex.ReqRef == "" {
		d.Warnings = append(d.Warnings, "request body was not recorded")
	} else if body, err := s.rd.Blob(ex.Req.SessionID, ex.ReqRef); err != nil {
		d.Warnings = append(d.Warnings, "request body unreadable: "+err.Error())
	} else {
		var req struct {
			System   json.RawMessage `json:"system"`
			Messages json.RawMessage `json:"messages"`
		}
		if json.Unmarshal(body, &req) != nil {
			d.Warnings = append(d.Warnings, "request body is not JSON; use the raw view")
		} else {
			d.System = view.SystemBlocks(req.System)
			d.CCVersion = view.CCVersion(req.System)
			d.CtxWindow = wire.CtxWindow(ex.Req.Model, req.System)
			d.Messages = req.Messages
		}
		d.Tools = view.Tools(body)
		d.Tokens = view.Tokens(body)
		d.Options = view.Options(body)
	}

	// $0.00 on a turn that clearly used tokens is confusing unless we say why.
	if ex.Res.Cost == 0 && ex.Req.Model != "" && !pricing.Known(ex.Req.Model) {
		d.Warnings = append(d.Warnings, "no price on file for "+ex.Req.Model+"; cost shown as zero")
	}

	switch {
	case ex.State == model.StateInflight:
		d.Warnings = append(d.Warnings, "response still streaming")
	case ex.ResRef == "":
		d.Warnings = append(d.Warnings, "response body was not recorded")
	default:
		body, err := s.rd.Blob(ex.Req.SessionID, ex.ResRef)
		if err != nil {
			d.Warnings = append(d.Warnings, "response body unreadable: "+err.Error())
			break
		}
		d.Response = buildResponseView(ex, body)
	}
	writeJSON(w, d)
}

// openaiDetail fills a detail for a Responses API turn. The request is
// translated into the Messages shape the views are built on (view.FromResponses);
// for a WebSocket turn it is first rebuilt from the connection's log, because
// the frame itself may carry only what was new since the previous response.
//
// No context window is assumed: a Responses request does not say which one the
// client is working against, and a guessed denominator is worse than none.
func (s *server) openaiDetail(d *detail) {
	ex := d.Exchange
	var req []byte
	var events [][]byte
	var resBytes int
	resKnown := false

	switch {
	case ex.WS != nil:
		log, err := s.rd.Blob(ex.Req.SessionID, ex.WS.Log)
		if err != nil {
			d.Warnings = append(d.Warnings, "connection log unreadable: "+err.Error())
			break
		}
		// A record number is only a pointer; the turn record says whose turn
		// starts there, and a manifest pointing at another's must not show it.
		t := wire.ReplayTurn(log, ex.WS.First, wsLast(ex))
		if t == nil || t.ID != ex.ID {
			d.Warnings = append(d.Warnings, "this turn's records are not in its connection log; run goxray fsck")
			break
		}
		if t.Incomplete {
			d.Warnings = append(d.Warnings, "this request continued a response the connection log does not hold; earlier context is missing")
		}
		req = t.Request
		if req == nil {
			req = t.Create
		}
		events, resKnown = t.Events, true
		for _, e := range events {
			resBytes += len(e) + 1
		}
	case ex.ReqRef == "":
		d.Warnings = append(d.Warnings, "request body was not recorded")
	default:
		body, err := s.rd.Blob(ex.Req.SessionID, ex.ReqRef)
		if err != nil {
			d.Warnings = append(d.Warnings, "request body unreadable: "+err.Error())
			break
		}
		req = wire.DecodeContent(ex.ReqHeaders["content-encoding"], body)
	}

	if req != nil {
		if conv := view.FromResponses(req); conv == nil {
			d.Warnings = append(d.Warnings, "request body is not JSON; use the raw view")
		} else {
			var r struct {
				System   json.RawMessage `json:"system"`
				Messages json.RawMessage `json:"messages"`
			}
			_ = json.Unmarshal(conv, &r)
			d.System = view.SystemBlocks(r.System)
			d.Messages = r.Messages
			d.Tools = view.Tools(conv)
			d.Tokens = view.Tokens(conv)
			d.Options = view.Options(conv)
		}
	}

	if ex.Res.Cost == 0 && ex.Req.Model != "" && !pricing.Known(ex.Req.Model) && !pricing.Known(ex.Res.Model) {
		d.Warnings = append(d.Warnings, "no price on file for "+ex.Req.Model+"; cost shown as zero")
	}

	if !resKnown && ex.WS == nil && ex.ResRef != "" {
		body, err := s.rd.Blob(ex.Req.SessionID, ex.ResRef)
		if err != nil {
			d.Warnings = append(d.Warnings, "response body unreadable: "+err.Error())
		} else {
			resBytes, resKnown = len(body), true
			ct := ex.ResHeaders["content-type"]
			switch {
			case strings.Contains(ct, "text/event-stream"):
				for _, e := range wire.ParseSSEEvents(body) {
					events = append(events, e.Data)
				}
			case json.Valid(body):
				// A whole response object: its output is the reply.
				events = [][]byte{[]byte(`{"type":"response.completed","response":` + string(body) + `}`)}
			}
		}
	}
	if ex.State == model.StateInflight {
		d.Warnings = append(d.Warnings, "response still streaming")
	}
	if !resKnown {
		if ex.State != model.StateInflight {
			d.Warnings = append(d.Warnings, "response body was not recorded")
		}
		return
	}
	rv := &responseView{ContentType: "application/jsonl", Bytes: resBytes}
	for _, e := range events {
		rv.Events = append(rv.Events, wire.SSEEvent{Type: wire.EventType(e), Data: e})
	}
	items, terminal := view.ResponsesOutput(events)
	rv.Message = view.AssembleResponses(items, view.FindEncryptedParams(req), ex.Res.Model, ex.Res.ResponseID, ex.Res.StopReason, terminal == "")
	d.Response = rv
}

func buildResponseView(ex *model.Exchange, body []byte) *responseView {
	ct := ex.ResHeaders["content-type"]
	rv := &responseView{ContentType: ct, Bytes: len(body)}
	switch {
	case strings.Contains(ct, "text/event-stream"):
		rv.Events = wire.ParseSSEEvents(body)
		rv.Message = wire.AssembleSSE(rv.Events)
	case json.Valid(body):
		rv.JSON = body
		rv.Message = wire.AssembleJSON(body)
	default:
		rv.Text = string(body)
	}
	return rv
}

// rawContentTypes are the content types served back verbatim. Anything else is
// downgraded to text/plain so a recorded body can never be interpreted as
// markup by the browser.
var rawContentTypes = map[string]bool{
	"application/json":  true,
	"text/event-stream": true,
	"text/plain":        true,
}

func (s *server) handleRaw(w http.ResponseWriter, r *http.Request) {
	ex, err := s.rd.Manifest(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such exchange")
		return
	}
	side := r.URL.Query().Get("side")
	if side == "" {
		side = "req"
	}
	if ex.WS != nil {
		s.rawWS(w, ex, side)
		return
	}
	var ref, ct string
	switch side {
	case "req":
		ref, ct = ex.ReqRef, ex.ReqHeaders["content-type"]
	case "res":
		ref, ct = ex.ResRef, ex.ResHeaders["content-type"]
	default:
		writeErr(w, http.StatusBadRequest, "side must be req or res")
		return
	}
	if ref == "" {
		writeErr(w, http.StatusNotFound, "no recorded "+side+" body")
		return
	}
	body, err := s.rd.Blob(ex.Req.SessionID, ref)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if side == "req" {
		// Stored as received; a browser cannot show a zstd body, so this view
		// shows it decoded and says so. `goxray raw` gives the stored bytes.
		if enc := ex.ReqHeaders["content-encoding"]; enc != "" {
			if dec := wire.DecodeContent(enc, body); !bytes.Equal(dec, body) {
				body = dec
				w.Header().Set("x-goxray-decoded", enc)
			}
		}
	}
	base, _, _ := strings.Cut(ct, ";")
	if !rawContentTypes[strings.TrimSpace(base)] {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("content-type", ct)
	w.Header().Set("content-length", strconv.Itoa(len(body)))
	w.Header().Set("content-disposition", fmt.Sprintf("inline; filename=%q", ex.ID+"_"+side+".json"))
	_, _ = w.Write(body)
}

// wsLast is where a turn's replay stops: its recorded range once settled. An
// inflight turn's range is only what it had when its request was written, and
// its events are what a reader is watching for, so it replays to the log's end.
func wsLast(ex *model.Exchange) int64 {
	if ex.State == model.StateInflight {
		return 0
	}
	return ex.WS.Last
}

// rawWS serves a WebSocket turn's bytes out of its connection log: the
// response.create as sent (req), the events that answered it one per line
// (res), or the connection's whole log (log).
func (s *server) rawWS(w http.ResponseWriter, ex *model.Exchange, side string) {
	log, err := s.rd.Blob(ex.Req.SessionID, ex.WS.Log)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var body []byte
	switch side {
	case "log":
		body = log
	case "req", "res":
		t := wire.ReplayTurn(log, ex.WS.First, wsLast(ex))
		if t == nil || t.ID != ex.ID {
			writeErr(w, http.StatusNotFound, "turn not found in its connection log")
			return
		}
		if side == "req" {
			body = t.Create
		} else {
			body = t.EventsJSONL()
		}
	default:
		writeErr(w, http.StatusBadRequest, "side must be req, res or log")
		return
	}
	w.Header().Set("content-type", "text/plain; charset=utf-8")
	w.Header().Set("content-length", strconv.Itoa(len(body)))
	w.Header().Set("content-disposition", fmt.Sprintf("inline; filename=%q", ex.ID+"_"+side+".jsonl"))
	_, _ = w.Write(body)
}

// handleEvents streams exchanges as they commit. Each exchange is sent twice in
// the normal case — once inflight, once final — and the client upserts by id.
func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if s.hub == nil {
		writeErr(w, http.StatusServiceUnavailable, "live events unavailable")
		return
	}
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeErr(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}
	w.Header().Set("content-type", "text/event-stream")
	w.Header().Set("cache-control", "no-cache")
	w.Header().Set("connection", "keep-alive")
	w.Header().Set("x-accel-buffering", "no")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	events, cancel := s.hub.subscribe()
	defer cancel()

	// Heartbeat so an idle stream is not mistaken for a dead one.
	ticker := time.NewTicker(25 * time.Second)
	defer ticker.Stop()

	enc := json.NewEncoder(w)
	for {
		select {
		case <-r.Context().Done():
			return
		case <-ticker.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case ex := <-events:
			if _, err := fmt.Fprint(w, "data: "); err != nil {
				return
			}
			if err := enc.Encode(ex); err != nil { // Encode appends the newline
				return
			}
			if _, err := fmt.Fprint(w, "\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

// ── static ──────────────────────────────────────────────────────────

const csp = "default-src 'self'; script-src 'self'; style-src 'self' 'unsafe-inline'; " +
	"img-src 'self' data:; connect-src 'self'; font-src 'self' data:; " +
	"frame-ancestors 'none'; base-uri 'none'; object-src 'none'"

// spaHandler serves the embedded bundle, falling back to index.html so the
// single-page app's own routes deep-link correctly.
func spaHandler() http.Handler {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			writeErr(w, http.StatusInternalServerError, "ui bundle unavailable")
		})
	}
	files := http.FileServer(http.FS(sub))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := strings.TrimPrefix(r.URL.Path, "/")
		if p == "" {
			p = "index.html"
		}
		if _, err := fs.Stat(sub, p); err != nil {
			serveIndex(w, sub)
			return
		}
		if strings.HasSuffix(p, ".html") {
			w.Header().Set("content-security-policy", csp)
		}
		files.ServeHTTP(w, r)
	})
}

func serveIndex(w http.ResponseWriter, sub fs.FS) {
	body, err := fs.ReadFile(sub, "index.html")
	if err != nil {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	w.Header().Set("content-security-policy", csp)
	w.Header().Set("content-type", "text/html; charset=utf-8")
	_, _ = w.Write(body)
}
