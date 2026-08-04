// Package web serves the read-only goxray dashboard: a JSON API over recorded
// exchanges plus the embedded single-page app that renders it.
//
// It reaches the store through the narrow Reader interface below, so nothing in
// this package can write to the blob store, the manifests, or the index. The
// dashboard runs on its own port and its own http.Server; the proxy's
// fail-closed recording path shares only read access with it.
package web

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/http"
	"os"
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
	writeJSON(w, map[string]any{"sessions": sessions})
}

func (s *server) handleExchanges(w http.ResponseWriter, r *http.Request) {
	f := filterFrom(r)
	rows, err := s.rd.Query(f)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, map[string]any{"exchanges": s.mergeInflight(rows, f)})
}

// mergeInflight overlays exchanges that are still streaming. They exist as
// manifests but not yet in the index — the index upsert happens when the
// response commits — so without this a turn would only be visible to clients
// that were already listening on /api/events when it started.
func (s *server) mergeInflight(rows []model.Exchange, f store.Filter) []model.Exchange {
	if s.hub == nil {
		return rows
	}
	live := s.hub.Inflight()
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
		if f.Session != "" && e.Req.SessionID != f.Session {
			continue
		}
		if f.Source != "" && e.Source != f.Source {
			continue
		}
		if !f.IncludeNoise && e.IsNoise {
			continue
		}
		if f.Since > 0 && e.StartedAt.UnixMilli() <= f.Since {
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
	base, _, _ := strings.Cut(ct, ";")
	if !rawContentTypes[strings.TrimSpace(base)] {
		ct = "text/plain; charset=utf-8"
	}
	w.Header().Set("content-type", ct)
	w.Header().Set("content-length", strconv.Itoa(len(body)))
	w.Header().Set("content-disposition", fmt.Sprintf("inline; filename=%q", ex.ID+"_"+side+".json"))
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
