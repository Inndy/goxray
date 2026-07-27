package proxy

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"

	"go.inndy.tw/goxray/internal/model"
)

// idleTimeout bounds how long an in-flight response may stall with no bytes
// before goxray gives up, records the partial, and aborts the client.
const idleTimeout = 5 * time.Minute

// httpTransport implements the HTTP request/response recording path. A future
// wsTransport (Codex) implements the same conceptual contract for WebSocket
// frames, emitting Exchanges through the same Recorder.
type httpTransport struct{}

// Handle runs one exchange fail-closed:
//  1. buffer request, PersistRequest (fsync) BEFORE forwarding — else 502;
//  2. forward with identity encoding;
//  3. stream the response, holding back terminal SSE frames until the response
//     is durably committed — else abort the connection.
func (t *httpTransport) Handle(w http.ResponseWriter, r *http.Request, up *url.URL, source string, rec Recorder, client *http.Client, maxBody int64) error {
	// ── request ──────────────────────────────────────────────
	body, err := io.ReadAll(io.LimitReader(r.Body, maxBody+1))
	if err != nil {
		writeError(w, http.StatusBadGateway, "read request body: "+err.Error())
		return err
	}
	if int64(len(body)) > maxBody {
		writeError(w, http.StatusRequestEntityTooLarge, "request body exceeds limit")
		return nil
	}

	// ex.URL records the (slug-stripped) API path actually sent upstream; the
	// slug is captured separately as Source.
	reqURI := up.Path
	if up.RawQuery != "" {
		reqURI += "?" + up.RawQuery
	}
	ex := rec.NewExchange(r.Method, reqURI, r.Header)
	ex.Source = source
	if err := rec.PersistRequest(ex, body); err != nil {
		// Fail-closed: we could not durably record the request, so it must not
		// reach the API. This is the only 502 that precedes forwarding.
		log.Printf("goxray: persist request %s: %v — refusing to forward", ex.ID, err)
		writeError(w, http.StatusBadGateway, "failed to durably record request (fail-closed)")
		return nil
	}

	// ── forward ──────────────────────────────────────────────
	upCtx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var timedOut atomic.Bool
	timer := time.AfterFunc(idleTimeout, func() { timedOut.Store(true); cancel() })
	defer timer.Stop()

	upReq, err := http.NewRequestWithContext(upCtx, r.Method, up.String(), bytes.NewReader(body))
	if err != nil {
		return t.finalizeNoResponse(w, ex, rec, model.StateUpstreamError, err)
	}
	forwardReqHeaders(upReq, r.Header)
	upReq.Host = up.Host

	resp, err := client.Do(upReq)
	if err != nil {
		return t.finalizeNoResponse(w, ex, rec, upstreamState(&timedOut), err)
	}
	defer resp.Body.Close()

	ex.Status = resp.StatusCode
	ex.ResHeaders = curateResHeaders(resp.Header)

	if isSSE(resp.Header.Get("content-type")) {
		return t.streamSSE(w, resp, ex, rec, timer, &timedOut)
	}
	return t.bufferNonSSE(w, resp, ex, rec, &timedOut)
}

// finalizeNoResponse records an exchange that never produced a client-visible
// response (forward failed). No bytes have gone to the client, so a clean 502
// is honest.
func (t *httpTransport) finalizeNoResponse(w http.ResponseWriter, ex *model.Exchange, rec Recorder, state model.ExchangeState, cause error) error {
	ex.State = state
	if err := rec.PersistResponse(ex, nil); err != nil {
		log.Printf("goxray: finalize %s (%s): %v", ex.ID, state, err)
	}
	writeError(w, http.StatusBadGateway, "upstream error: "+cause.Error())
	return cause
}

// streamSSE forwards SSE frames live while teeing every raw byte, holds back the
// terminal message_delta/message_stop frames, commits the response durably, then
// releases the held frames. A commit failure aborts the connection (no clean
// terminating chunk) so the client sees an unexpected EOF.
func (t *httpTransport) streamSSE(w http.ResponseWriter, resp *http.Response, ex *model.Exchange, rec Recorder, timer *time.Timer, timedOut *atomic.Bool) error {
	flusher, _ := w.(http.Flusher)
	copyRespHeaders(w.Header(), resp.Header)
	w.WriteHeader(ex.Status)
	if flusher != nil {
		flusher.Flush()
	}

	var capture bytes.Buffer
	var pending []byte
	var held [][]byte
	buf := make([]byte, 32*1024)

	flushHeld := func() error {
		for _, h := range held {
			if err := writeFrame(w, flusher, h); err != nil {
				return err
			}
		}
		held = held[:0]
		return nil
	}

	for {
		n, rerr := resp.Body.Read(buf)
		if n > 0 {
			timer.Reset(idleTimeout)
			chunk := buf[:n]
			capture.Write(chunk)
			pending = append(pending, chunk...)
			for _, frame := range takeFrames(&pending) {
				if isTerminalFrame(frame) {
					held = append(held, frame)
					continue
				}
				// A non-terminal frame after held candidates means those weren't
				// terminal-in-position: flush them, then this frame.
				if err := flushHeld(); err != nil {
					return t.clientAborted(ex, rec, &capture, err)
				}
				if err := writeFrame(w, flusher, frame); err != nil {
					return t.clientAborted(ex, rec, &capture, err)
				}
			}
		}
		if rerr != nil {
			if rerr == io.EOF {
				break
			}
			return t.upstreamFailedMidStream(ex, rec, &capture, upstreamState(timedOut), rerr)
		}
	}

	// Trailing partial frame (no delimiter at EOF).
	if len(pending) > 0 {
		if isTerminalFrame(pending) {
			held = append(held, pending)
		} else if err := writeFrame(w, flusher, pending); err != nil {
			return t.clientAborted(ex, rec, &capture, err)
		}
	}

	// ── commit point ─────────────────────────────────────────
	ex.State = model.StateComplete
	if err := rec.PersistResponse(ex, capture.Bytes()); err != nil {
		log.Printf("goxray: persist response %s: %v — aborting stream (fail-closed)", ex.ID, err)
		panic(http.ErrAbortHandler) // TCP abort, no clean chunk → client sees unexpected EOF
	}

	// Response is durable; release the held terminal frames.
	if err := flushHeld(); err != nil {
		// Client vanished at the very end; the record is already committed.
		log.Printf("goxray: release terminal frames %s: %v (record committed)", ex.ID, err)
	}
	return nil
}

// bufferNonSSE handles non-streaming responses: buffer fully, commit, THEN write
// to the client — so a header-only/204 response can still return an honest 502
// on persist failure. The read is deliberately uncapped: maxBody bounds what
// clients may send, but capping the upstream's reply would silently truncate a
// verbatim record and hand the client a cut body it cannot detect (the proxy
// strips content-length). The SSE path is likewise unbounded.
func (t *httpTransport) bufferNonSSE(w http.ResponseWriter, resp *http.Response, ex *model.Exchange, rec Recorder, timedOut *atomic.Bool) error {
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		ex.State = upstreamState(timedOut)
		if perr := rec.PersistResponse(ex, body); perr != nil {
			log.Printf("goxray: finalize %s: %v", ex.ID, perr)
		}
		writeError(w, http.StatusBadGateway, "upstream read error: "+err.Error())
		return err
	}

	ex.State = model.StateComplete
	if err := rec.PersistResponse(ex, body); err != nil {
		log.Printf("goxray: persist response %s: %v", ex.ID, err)
		writeError(w, http.StatusBadGateway, "failed to durably record response (fail-closed)")
		return nil
	}

	copyRespHeaders(w.Header(), resp.Header)
	w.WriteHeader(ex.Status)
	_, _ = w.Write(body)
	return nil
}

func (t *httpTransport) clientAborted(ex *model.Exchange, rec Recorder, capture *bytes.Buffer, cause error) error {
	ex.State = model.StateClientAborted
	if err := rec.PersistResponse(ex, capture.Bytes()); err != nil {
		log.Printf("goxray: finalize client_aborted %s: %v", ex.ID, err)
	}
	return cause
}

func (t *httpTransport) upstreamFailedMidStream(ex *model.Exchange, rec Recorder, capture *bytes.Buffer, state model.ExchangeState, cause error) error {
	ex.State = state
	if err := rec.PersistResponse(ex, capture.Bytes()); err != nil {
		log.Printf("goxray: finalize %s (%s): %v", ex.ID, state, err)
	}
	// We already sent 200 + partial body; abort so the client sees a broken stream.
	panic(http.ErrAbortHandler)
}

func upstreamState(timedOut *atomic.Bool) model.ExchangeState {
	if timedOut.Load() {
		return model.StateTimeout
	}
	return model.StateUpstreamError
}

// ── SSE frame handling ───────────────────────────────────────

// takeFrames pulls complete SSE frames (up to and including their blank-line
// delimiter) out of pending, leaving any trailing partial. Frames are copied so
// later appends to pending never corrupt them. Operates on bytes only — never
// string(chunk), which would split a UTF-8 rune across a chunk boundary.
func takeFrames(pending *[]byte) [][]byte {
	buf := *pending
	var frames [][]byte
	for {
		pos, dlen := findDelim(buf)
		if pos < 0 {
			break
		}
		end := pos + dlen
		frame := make([]byte, end)
		copy(frame, buf[:end])
		frames = append(frames, frame)
		buf = buf[end:]
	}
	*pending = append([]byte(nil), buf...) // compact so backing array is released
	return frames
}

// findDelim returns the offset and length of the earliest SSE frame delimiter
// (LF LF or CRLF CRLF), or (-1, 0) if none is present.
func findDelim(b []byte) (int, int) {
	i1 := bytes.Index(b, []byte("\n\n"))
	i2 := bytes.Index(b, []byte("\r\n\r\n"))
	switch {
	case i1 < 0 && i2 < 0:
		return -1, 0
	case i2 < 0:
		return i1, 2
	case i1 < 0:
		return i2, 4
	case i2 < i1:
		return i2, 4
	default:
		return i1, 2
	}
}

var (
	tokMsgStop  = []byte(`"type":"message_stop"`)
	tokMsgDelta = []byte(`"type":"message_delta"`)
	tokEvtStop  = []byte("event: message_stop")
	tokEvtDelta = []byte("event: message_delta")
)

func isTerminalFrame(frame []byte) bool {
	return bytes.Contains(frame, tokMsgStop) || bytes.Contains(frame, tokMsgDelta) ||
		bytes.Contains(frame, tokEvtStop) || bytes.Contains(frame, tokEvtDelta)
}

func writeFrame(w http.ResponseWriter, f http.Flusher, frame []byte) error {
	if _, err := w.Write(frame); err != nil {
		return err
	}
	if f != nil {
		f.Flush()
	}
	return nil
}

// ── header handling ──────────────────────────────────────────

var hopByHop = map[string]bool{
	"connection": true, "keep-alive": true, "proxy-authenticate": true,
	"proxy-authorization": true, "te": true, "trailers": true,
	"transfer-encoding": true, "upgrade": true,
}

// forwardReqHeaders copies client request headers to the upstream request,
// dropping hop-by-hop headers (and tokens named in Connection), rewriting the
// encoding to identity so captured bytes are plaintext.
func forwardReqHeaders(up *http.Request, src http.Header) {
	drop := connectionTokens(src)
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || drop[lk] || lk == "host" || lk == "accept-encoding" {
			continue
		}
		for _, v := range vs {
			up.Header.Add(k, v)
		}
	}
	up.Header.Set("accept-encoding", "identity")
}

func copyRespHeaders(dst, src http.Header) {
	drop := connectionTokens(src)
	for k, vs := range src {
		lk := strings.ToLower(k)
		if hopByHop[lk] || drop[lk] || lk == "content-length" {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

func connectionTokens(h http.Header) map[string]bool {
	out := map[string]bool{}
	for _, v := range h.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if t := strings.ToLower(strings.TrimSpace(tok)); t != "" {
				out[t] = true
			}
		}
	}
	return out
}

// curateResHeaders keeps the audit-relevant response headers: request-id (log
// correlation), rate-limit headers, retry-after, content-type.
func curateResHeaders(h http.Header) map[string]string {
	out := map[string]string{}
	for k, vs := range h {
		lk := strings.ToLower(k)
		if lk == "request-id" || lk == "retry-after" || lk == "content-type" ||
			strings.HasPrefix(lk, "anthropic-ratelimit-") || strings.HasPrefix(lk, "anthropic-") {
			if len(vs) > 0 {
				out[lk] = vs[0]
			}
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

func isSSE(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

// writeError emits an Anthropic-error-shaped JSON body so Claude Code renders
// the reason.
func writeError(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("content-type", "application/json")
	w.WriteHeader(status)
	fmt.Fprintf(w, `{"type":"error","error":{"type":"api_error","message":%q}}`, "goxray: "+msg)
}
