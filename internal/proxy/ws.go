package proxy

import (
	"errors"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"go.inndy.tw/goxray/internal/model"
)

// OpenStreamFunc starts recording a connection: it makes the connection
// exchange durable as inflight and returns its log. It runs before goxray dials
// upstream, so a failure here means the upgrade is refused and nothing reaches
// the API. A connection is recorded as one log; see docs/codex.md.
type OpenStreamFunc func(conn *model.Exchange) (StreamLog, error)

// StreamLog records one connection. Calls come from both pump goroutines and
// are serialised inside; the order they are made in is the log's order.
type StreamLog interface {
	// Client records a client frame and makes it durable — log segment and, for
	// a response.create, the turn's inflight manifest. Only after it returns
	// nil may the frame be forwarded.
	Client(payload []byte, binary bool) error
	// Server records an upstream frame in memory. It reports whether the frame
	// ends a turn, in which case it must not reach the client before Commit.
	Server(payload []byte, binary bool) (terminal bool)
	// Commit makes everything recorded so far durable and writes the final
	// manifest of a turn that has ended.
	Commit() error
	// ShouldRotate reports that the log has grown past its soft limit and the
	// connection should be closed now, between turns.
	ShouldRotate() bool
	// Upgraded records the upstream's 101 and its headers durably; only then
	// may the client be told.
	Upgraded(status int, headers map[string]string) error
	// CloseFrame records a close frame seen in direction dir.
	CloseFrame(dir string, code int, reason string)
	// Refused records an upgrade the upstream answered with something other
	// than 101: its status, curated headers and body. On error the answer must
	// not be relayed.
	Refused(status int, headers map[string]string, body []byte) error
	// End records how the connection finished, settles any turn still open,
	// seals the log and writes the connection's final manifest. state is the
	// connection's disposition, open that of a turn still without its terminal
	// event: a connection can close cleanly around a response that never
	// completed.
	End(how string, state, open model.ExchangeState) error
}

// How a connection ended, as recorded in the log's end record.
const (
	EndClean         = "clean"
	EndClientGone    = "client_gone"
	EndUpstreamGone  = "upstream_gone"
	EndPersistFailed = "persist_failed"
	EndRefused       = "refused"
	// EndRotated is goxray closing the connection itself, between turns, so the
	// next one starts a fresh log. The close frames it sends are not recorded as
	// observed ones; this record is the account of them.
	EndRotated = "rotated"
	// EndShutdown is goxray closing the connection because it is stopping.
	EndShutdown = "shutdown"
)

type wsTransport struct {
	upgrader websocket.Upgrader
	dialer   websocket.Dialer

	// http.Server.Shutdown does not wait for a hijacked connection, so the
	// transport counts its own handlers: without this a socket could still be
	// recording after the store it records into has closed. A handler is
	// counted from its first line, before it hijacks anything, and none starts
	// once closing is set, so CloseAll cannot miss one.
	mu      sync.Mutex
	closing bool
	active  map[*pumpState]struct{}
	wg      sync.WaitGroup
}

// enter counts a handler in, or reports false once the transport is closing.
func (t *wsTransport) enter() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.closing {
		return false
	}
	t.wg.Add(1)
	return true
}

// CloseAll closes every open connection with 1001 and waits for each handler
// to settle its record, or for the deadline.
func (t *wsTransport) CloseAll(deadline time.Time) {
	t.mu.Lock()
	t.closing = true
	open := make([]*pumpState, 0, len(t.active))
	for p := range t.active {
		open = append(open, p)
	}
	t.mu.Unlock()
	// Concurrently and outside the lock: each close writes to two peers, and a
	// slow one must not spend the others' deadline.
	for _, p := range open {
		go p.goingAway()
	}
	done := make(chan struct{})
	go func() { t.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(time.Until(deadline)):
	}
}

func newWSTransport() *wsTransport {
	return &wsTransport{
		active: map[*pumpState]struct{}{},
		upgrader: websocket.Upgrader{
			// Compression is per-leg: whatever each side negotiates, the log
			// holds decompressed payloads.
			EnableCompression: true,
			// The proxy port has no Origin or Host guard by design; Codex sends
			// no Origin, and refusing one here would protect nothing the HTTP
			// path does not already expose.
			CheckOrigin: func(*http.Request) bool { return true },
		},
		dialer: websocket.Dialer{
			Proxy:             http.ProxyFromEnvironment,
			HandshakeTimeout:  45 * time.Second,
			EnableCompression: true,
		},
	}
}

// wsSkipHeaders are request headers the dialer sets itself or that only
// describe the client's own hop.
var wsSkipHeaders = map[string]bool{
	"host": true, "upgrade": true, "connection": true,
	"sec-websocket-key": true, "sec-websocket-version": true,
	"sec-websocket-extensions": true, "sec-websocket-accept": true,
	"accept-encoding": true, "content-length": true,
}

// Handle proxies one WebSocket connection, recording it fail-closed: every
// client frame is durable before it is forwarded, and every turn's terminal
// event is held until the turn is committed. A failure at either barrier tears
// both sides down without a close frame, so the client sees a broken stream,
// never a clean one, and resends on a new connection.
func (t *wsTransport) Handle(w http.ResponseWriter, r *http.Request, up *url.URL, source string, rec Recorder, open OpenStreamFunc, maxBody int64) error {
	if open == nil {
		// Nothing can record a socket, so none is opened. 426 is the answer
		// Codex takes as "use HTTP", which this proxy can record.
		writeError(w, http.StatusUpgradeRequired, "websocket recording unavailable; use HTTP")
		return nil
	}
	if !t.enter() {
		writeError(w, http.StatusServiceUnavailable, "goxray is shutting down")
		return nil
	}
	defer t.wg.Done()

	reqURI := up.Path
	if up.RawQuery != "" {
		reqURI += "?" + up.RawQuery
	}
	conn := rec.NewExchange(r.Method, reqURI, r.Header)
	conn.Source = source
	conn.Provider = model.ProviderOpenAI
	conn.Transport = model.TransportWS
	conn.Kind = model.KindWSConn
	slog, err := open(conn)
	if err != nil {
		log.Printf("goxray: open stream %s: %v — refusing upgrade", conn.ID, err)
		writeError(w, http.StatusBadGateway, "failed to durably record connection (fail-closed)")
		return nil
	}

	// ── upstream handshake ──────────────────────────────────
	wsURL := *up
	switch wsURL.Scheme {
	case "https":
		wsURL.Scheme = "wss"
	case "http":
		wsURL.Scheme = "ws"
	}
	hdr := http.Header{}
	drop := connectionTokens(r.Header)
	for k, vs := range r.Header {
		lk := strings.ToLower(k)
		if wsSkipHeaders[lk] || hopByHop[lk] || drop[lk] {
			continue
		}
		hdr[k] = vs
	}
	upConn, resp, err := t.dialer.Dial(wsURL.String(), hdr)
	if err != nil {
		if resp == nil {
			_ = slog.End(EndUpstreamGone, model.StateUpstreamError, model.StateUpstreamError)
			writeError(w, http.StatusBadGateway, "upstream websocket: "+err.Error())
			return err
		}
		// Refused with a real HTTP answer — 401, 426, 429. Relay it as-is: Codex
		// acts on these (refresh auth, fall back to HTTP), so they must arrive
		// intact — and what is relayed must be what is recorded, so a body that
		// cannot be read whole is not relayed at all.
		body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBody+1))
		resp.Body.Close()
		if rerr == nil && int64(len(body)) > maxBody {
			rerr = errors.New("refusal body exceeds the size limit")
		}
		if rerr != nil {
			_ = slog.End(EndUpstreamGone, model.StateUpstreamError, model.StateUpstreamError)
			writeError(w, http.StatusBadGateway, "upstream websocket refusal unreadable: "+rerr.Error())
			return rerr
		}
		if err := slog.Refused(resp.StatusCode, curateResHeaders(resp.Header), body); err != nil {
			log.Printf("goxray: record refused upgrade %s: %v", conn.ID, err)
			_ = slog.End(EndPersistFailed, model.StatePersistFailed, model.StatePersistFailed)
			writeError(w, http.StatusBadGateway, "failed to durably record response (fail-closed)")
			return nil
		}
		if err := slog.End(EndRefused, model.StateComplete, model.StateComplete); err != nil {
			log.Printf("goxray: record refused upgrade %s: %v", conn.ID, err)
			writeError(w, http.StatusBadGateway, "failed to durably record response (fail-closed)")
			return nil
		}
		copyRespHeaders(w.Header(), resp.Header)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		return nil
	}
	upConn.SetReadLimit(maxBody)

	// ── client handshake ────────────────────────────────────
	respHdr := http.Header{}
	for k, vs := range resp.Header {
		lk := strings.ToLower(k)
		if wsSkipHeaders[lk] || hopByHop[lk] {
			continue
		}
		respHdr[k] = vs
	}
	if err := slog.Upgraded(resp.StatusCode, curateResHeaders(resp.Header)); err != nil {
		log.Printf("goxray: record upgrade %s: %v — refusing it (fail-closed)", conn.ID, err)
		upConn.Close()
		_ = slog.End(EndPersistFailed, model.StatePersistFailed, model.StatePersistFailed)
		writeError(w, http.StatusBadGateway, "failed to durably record connection (fail-closed)")
		return nil
	}
	cliConn, err := t.upgrader.Upgrade(w, r, respHdr)
	if err != nil {
		upConn.Close()
		_ = slog.End(EndClientGone, model.StateClientAborted, model.StateClientAborted)
		return err // Upgrade has already answered the client
	}
	cliConn.SetReadLimit(maxBody)

	p := &pumpState{cli: cliConn, up: upConn}
	t.mu.Lock()
	closing := t.closing
	if !closing {
		t.active[p] = struct{}{}
	}
	t.mu.Unlock()
	if closing {
		// CloseAll has already taken its snapshot; this one closes itself.
		p.goingAway()
	}
	defer func() {
		t.mu.Lock()
		delete(t.active, p)
		t.mu.Unlock()
	}()
	return p.run(slog)
}

// pumpState is one proxied connection.
type pumpState struct {
	cli, up *websocket.Conn
	once    sync.Once
	how     string
	state   model.ExchangeState // the connection's
	open    model.ExchangeState // a turn's that never got its terminal event
	cause   error
}

// finish tears both legs down, once, and the first caller's account of how
// the connection ended is the one recorded. send, if set, runs inside the same
// once, after the account is claimed: a close frame sent before it would let
// the peer's reply race in and claim the end as its own. A send that fails is a
// failed barrier, and turns the end into an abrupt persist_failed. abrupt skips
// close frames: after a barrier failed, a clean close would tell the client the
// stream ended normally.
func (p *pumpState) finish(h string, st, open model.ExchangeState, err error, abrupt bool, send func() error) {
	p.once.Do(func() {
		p.how, p.state, p.open, p.cause = h, st, open, err
		if send != nil {
			if serr := send(); serr != nil {
				log.Printf("goxray: record close frame: %v — dropping connection (fail-closed)", serr)
				p.how, p.state, p.open, p.cause = EndPersistFailed, model.StatePersistFailed, model.StatePersistFailed, serr
				abrupt = true
			}
		}
		if abrupt {
			_ = p.cli.UnderlyingConn().Close()
			_ = p.up.UnderlyingConn().Close()
		} else {
			_ = p.cli.Close()
			_ = p.up.Close()
		}
	})
}

// goingAway closes both legs with 1001 because goxray is stopping. Codex
// reconnects with a full request, so the next process loses nothing. A turn cut
// off here is settled as an upstream error, as crash recovery would settle it.
func (p *pumpState) goingAway() {
	p.finish(EndShutdown, model.StateComplete, model.StateUpstreamError, nil, false,
		func() error { p.closeBoth("goxray: shutting down", 2*time.Second); return nil })
}

// closeBoth sends 1001 to both peers.
func (p *pumpState) closeBoth(reason string, wait time.Duration) {
	msg := websocket.FormatCloseMessage(websocket.CloseGoingAway, reason)
	deadline := time.Now().Add(wait)
	_ = p.cli.WriteControl(websocket.CloseMessage, msg, deadline)
	_ = p.up.WriteControl(websocket.CloseMessage, msg, deadline)
}

// run pumps both directions until either side ends, then settles the record.
func (p *pumpState) run(slog StreamLog) error {
	cli, up, finish := p.cli, p.up, p.finish
	rotation := func() error { p.closeBoth("goxray: log rotation", 5*time.Second); return nil }
	// A peer's close frame is a frame like any other: durable before it is
	// passed on, so a peer is never told of a clean close the log may not hold.
	forwardClose := func(to *websocket.Conn, ce *websocket.CloseError) func() error {
		return func() error {
			if err := slog.Commit(); err != nil {
				return err
			}
			_ = to.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(ce.Code, ce.Text), time.Now().Add(5*time.Second))
			return nil
		}
	}
	var wg sync.WaitGroup

	// client → upstream
	wg.Go(func() {
		for {
			mt, data, err := cli.ReadMessage()
			if err != nil {
				if ce, ok := closeErr(err); ok {
					slog.CloseFrame("c", ce.Code, ce.Text)
					finish(EndClean, model.StateComplete, model.StateClientAborted, nil, false, forwardClose(up, ce))
				} else {
					finish(EndClientGone, model.StateClientAborted, model.StateClientAborted, err, false, nil)
				}
				return
			}
			if err := slog.Client(data, mt == websocket.BinaryMessage); err != nil {
				var r interface{ Rotating() bool }
				if errors.As(err, &r) && r.Rotating() {
					// A create raced the rotation close. Nothing was recorded or
					// forwarded; closing cleanly sends it to a new socket.
					finish(EndRotated, model.StateComplete, model.StateClientAborted, nil, false, rotation)
					return
				}
				log.Printf("goxray: record client frame: %v — dropping connection (fail-closed)", err)
				finish(EndPersistFailed, model.StatePersistFailed, model.StatePersistFailed, err, true, nil)
				return
			}
			if err := up.WriteMessage(mt, data); err != nil {
				finish(EndUpstreamGone, model.StateUpstreamError, model.StateUpstreamError, err, false, nil)
				return
			}
		}
	})

	// upstream → client
	wg.Go(func() {
		for {
			mt, data, err := up.ReadMessage()
			if err != nil {
				if ce, ok := closeErr(err); ok {
					slog.CloseFrame("s", ce.Code, ce.Text)
					finish(EndClean, model.StateComplete, model.StateUpstreamError, nil, false, forwardClose(cli, ce))
				} else {
					finish(EndUpstreamGone, model.StateUpstreamError, model.StateUpstreamError, err, false, nil)
				}
				return
			}
			held := slog.Server(data, mt == websocket.BinaryMessage)
			if held {
				if err := slog.Commit(); err != nil {
					log.Printf("goxray: commit turn: %v — dropping connection (fail-closed)", err)
					finish(EndPersistFailed, model.StatePersistFailed, model.StatePersistFailed, err, true, nil)
					return
				}
			}
			if err := cli.WriteMessage(mt, data); err != nil {
				finish(EndClientGone, model.StateClientAborted, model.StateClientAborted, err, false, nil)
				return
			}
			if held && slog.ShouldRotate() {
				// Between turns as Codex sees them: the turn that just committed
				// has been released and no interrupt acknowledgement is owed. The
				// log refuses new turns from here on; Codex reconnects and resends
				// in full.
				finish(EndRotated, model.StateComplete, model.StateUpstreamError, nil, false, rotation)
				return
			}
		}
	})

	// Both legs are closed once either goroutine finishes, so the other's read
	// returns promptly; End must not race a Client or Commit still in flight.
	wg.Wait()
	if err := slog.End(p.how, p.state, p.open); err != nil {
		log.Printf("goxray: settle stream: %v", err)
	}
	return p.cause
}

// closeErr reports whether err is the peer's close frame. A connection that
// simply drops is not one: that is the client or upstream going away.
func closeErr(err error) (*websocket.CloseError, bool) {
	var ce *websocket.CloseError
	if errors.As(err, &ce) && ce.Code != websocket.CloseAbnormalClosure {
		return ce, true
	}
	return nil, false
}
