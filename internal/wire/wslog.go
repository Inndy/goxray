package wire

import (
	"bufio"
	"bytes"
	"encoding/json"
	"strconv"
	"unicode/utf8"
)

// A WebSocket connection is recorded as one append-only log of its frames, in
// order. Decoded, the log is JSON Lines, one Record per line. See
// docs/codex.md for why the connection rather than the turn is the unit.

// Directions a record can have.
const (
	DirClient = "c" // client → upstream
	DirServer = "s" // upstream → client
)

// Record ops. A data frame has none.
const (
	OpClose = "close" // a close frame, in either direction
	// OpRefused is an upgrade upstream answered with something other than
	// 101: status in Code, body as the payload.
	OpRefused = "refused"
	// OpTurn and OpEnd are goxray's own annotations, never wire data. A turn
	// record precedes each response.create and names the turn exchange in ID,
	// which lets recovery and fsck find a connection's turns from its log
	// alone. An end record says how goxray saw the connection finish.
	OpTurn = "turn"
	OpEnd  = "end"
)

// Record is one line of a connection log.
//
// A text frame's payload is kept byte-exact: in P when it is a JSON value that
// can sit on one line unchanged, otherwise in S as a JSON string. A binary
// frame is in B (base64), and so, in X, is a text frame that is not valid
// UTF-8: a JSON string cannot hold those bytes — Marshal would replace them
// with U+FFFD — and a well-formed frame is not the only kind worth keeping.
// Exactly one of the four is set on a data record.
type Record struct {
	N      int64           `json:"n"`
	T      int64           `json:"t"` // unix ms
	D      string          `json:"d"`
	Op     string          `json:"op,omitempty"`
	P      json.RawMessage `json:"p,omitempty"`
	S      *string         `json:"s,omitempty"`
	B      []byte          `json:"b,omitempty"`
	X      []byte          `json:"x,omitempty"`
	Code   int             `json:"code,omitempty"`
	Reason string          `json:"reason,omitempty"`
	ID     string          `json:"id,omitempty"`
}

// Payload returns the frame's bytes exactly as they crossed the wire.
func (r *Record) Payload() []byte {
	switch {
	case r.P != nil:
		return r.P
	case r.S != nil:
		return []byte(*r.S)
	case r.X != nil:
		return r.X
	default:
		return r.B
	}
}

// IsData reports whether the record is a frame payload rather than a close or
// end marker.
func (r *Record) IsData() bool { return r.Op == "" }

// AppendRecord appends one encoded line to dst.
//
// The line is assembled by hand rather than with json.Marshal because Marshal
// compacts a RawMessage, which would rewrite the payload's own whitespace — the
// one thing a verbatim log must not do.
func AppendRecord(dst []byte, r Record, payload []byte, binary bool) []byte {
	dst = append(dst, `{"n":`...)
	dst = strconv.AppendInt(dst, r.N, 10)
	dst = append(dst, `,"t":`...)
	dst = strconv.AppendInt(dst, r.T, 10)
	dst = append(dst, `,"d":`...)
	dst = strconv.AppendQuote(dst, r.D)
	if r.Op != "" {
		dst = append(dst, `,"op":`...)
		dst = appendJSONString(dst, r.Op)
		if r.Code != 0 {
			dst = append(dst, `,"code":`...)
			dst = strconv.AppendInt(dst, int64(r.Code), 10)
		}
		if r.Reason != "" {
			dst = append(dst, `,"reason":`...)
			dst = appendJSONString(dst, r.Reason)
		}
		if r.ID != "" {
			dst = append(dst, `,"id":`...)
			dst = appendJSONString(dst, r.ID)
		}
		if payload == nil {
			return append(dst, "}\n"...)
		}
	}
	switch {
	case binary:
		b, _ := json.Marshal(payload) // []byte marshals as base64
		dst = append(dst, `,"b":`...)
		dst = append(dst, b...)
	case !utf8.Valid(payload):
		b, _ := json.Marshal(payload)
		dst = append(dst, `,"x":`...)
		dst = append(dst, b...)
	case inlineable(payload):
		dst = append(dst, `,"p":`...)
		dst = append(dst, payload...)
	default:
		dst = append(dst, `,"s":`...)
		dst = appendJSONString(dst, string(payload))
	}
	return append(dst, "}\n"...)
}

// inlineable reports whether payload survives a round trip through a
// json.RawMessage field on a single line, byte for byte. Unmarshal hands a
// RawMessage the value's exact bytes, but not the whitespace around it, and a
// newline would end the line.
func inlineable(p []byte) bool {
	if len(p) == 0 || bytes.ContainsAny(p, "\r\n") {
		return false
	}
	if isSpace(p[0]) || isSpace(p[len(p)-1]) {
		return false
	}
	return json.Valid(p)
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }

func appendJSONString(dst []byte, s string) []byte {
	b, _ := json.Marshal(s)
	return append(dst, b...)
}

// EachRecord decodes a log front to back. A line that does not decode — which
// can only be a torn final line — ends the walk rather than failing it: the
// prefix is still a faithful record.
func EachRecord(log []byte, fn func(Record) bool) {
	sc := bufio.NewScanner(bytes.NewReader(log))
	sc.Buffer(nil, len(log)+1)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var r Record
		if json.Unmarshal(line, &r) != nil {
			return
		}
		if !fn(r) {
			return
		}
	}
}
