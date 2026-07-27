package wire

import (
	"bytes"
	"encoding/json"
)

// SplitSSEData walks an SSE byte stream and hands each `data:` payload to fn.
// Frames are separated by blank lines and CRLF is tolerated, matching what the
// Anthropic API emits. fn may return false to stop early.
//
// Shared by the metadata parser (which only needs a few event types) and the
// dashboard's event extractor (which needs all of them), so the frame-splitting
// rules live in exactly one place.
func SplitSSEData(raw []byte, fn func(payload []byte) bool) {
	norm := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	for frame := range bytes.SplitSeq(norm, []byte("\n\n")) {
		for line := range bytes.SplitSeq(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) == 0 || payload[0] != '{' {
				continue
			}
			if !fn(payload) {
				return
			}
		}
	}
}

// SSEEvent is one decoded stream event, kept as raw JSON so the dashboard can
// render the verbatim payload without this package having to model every event
// shape the API may add.
type SSEEvent struct {
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
}

// ParseSSEEvents decodes every well-formed data frame in order. Malformed
// frames are skipped rather than failing the whole stream — a recorded response
// may be partial (client_aborted, upstream_error, timeout) and the prefix is
// still worth showing.
func ParseSSEEvents(raw []byte) []SSEEvent {
	var out []SSEEvent
	SplitSSEData(raw, func(payload []byte) bool {
		var probe struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(payload, &probe) != nil {
			return true
		}
		out = append(out, SSEEvent{Type: probe.Type, Data: bytes.Clone(payload)})
		return true
	})
	return out
}
