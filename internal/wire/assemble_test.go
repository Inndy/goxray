package wire

import (
	"encoding/json"
	"strings"
	"testing"
)

func sse(frames ...string) []SSEEvent {
	return ParseSSEEvents([]byte(strings.Join(frames, "")))
}

func frame(payload string) string { return "data: " + payload + "\n\n" }

func blockField(t *testing.T, raw json.RawMessage, key string) any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatalf("block is not an object: %v", err)
	}
	return m[key]
}

func TestAssembleSSEJoinsTextDeltas(t *testing.T) {
	ev := sse(
		frame(`{"type":"message_start","message":{"role":"assistant","model":"claude-opus-5","id":"msg_1"}}`),
		frame(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"comm"}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"it this"}}`),
		frame(`{"type":"content_block_stop","index":0}`),
		frame(`{"type":"message_delta","delta":{"stop_reason":"end_turn"}}`),
		frame(`{"type":"message_stop"}`),
	)
	m := AssembleSSE(ev)
	if m == nil {
		t.Fatal("nil message")
	}
	if m.Model != "claude-opus-5" || m.ID != "msg_1" || m.Role != "assistant" {
		t.Errorf("header wrong: %+v", m)
	}
	if m.StopReason != "end_turn" {
		t.Errorf("stop_reason = %q", m.StopReason)
	}
	if m.Truncated {
		t.Error("a stream ending in message_stop is not truncated")
	}
	if len(m.Content) != 1 {
		t.Fatalf("want 1 block, got %d", len(m.Content))
	}
	if got := blockField(t, m.Content[0], "text"); got != "commit this" {
		t.Errorf("text = %q", got)
	}
}

func TestAssembleSSEBuildsToolInputFromPartialJSON(t *testing.T) {
	ev := sse(
		frame(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"comm"}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"and\":\"ls\"}"}}`),
		frame(`{"type":"content_block_stop","index":0}`),
		frame(`{"type":"message_stop"}`),
	)
	m := AssembleSSE(ev)
	if m == nil || len(m.Content) != 1 {
		t.Fatalf("want 1 block, got %+v", m)
	}
	in, ok := blockField(t, m.Content[0], "input").(map[string]any)
	if !ok {
		t.Fatalf("input is not an object: %v", blockField(t, m.Content[0], "input"))
	}
	if in["command"] != "ls" {
		t.Errorf("input = %v", in)
	}
	if blockField(t, m.Content[0], "name") != "Bash" {
		t.Error("tool name lost")
	}
}

// A recording cut mid-stream must still show how far the model got, including a
// tool call whose arguments never finished arriving.
func TestAssembleSSEKeepsTruncatedStream(t *testing.T) {
	ev := sse(
		frame(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"partial"}}`),
		frame(`{"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"toolu_2","name":"Read","input":{}}}`),
		frame(`{"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"file_pa"}}`),
	)
	m := AssembleSSE(ev)
	if m == nil {
		t.Fatal("nil message")
	}
	if !m.Truncated {
		t.Error("a stream with no message_stop must be marked truncated")
	}
	if len(m.Content) != 2 {
		t.Fatalf("want 2 blocks (neither got content_block_stop), got %d", len(m.Content))
	}
	if got := blockField(t, m.Content[0], "text"); got != "partial" {
		t.Errorf("text = %q", got)
	}
	if got := blockField(t, m.Content[1], "partial_input"); got != `{"file_pa` {
		t.Errorf("partial_input = %v", got)
	}
	if blockField(t, m.Content[1], "input") != nil {
		t.Error("unparseable input must not be presented as a real input object")
	}
}

func TestAssembleSSEKeepsThinkingAndUnmodelledFields(t *testing.T) {
	ev := sse(
		frame(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":"","some_future_field":7}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`),
		frame(`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sig"}}`),
		frame(`{"type":"message_stop"}`),
	)
	m := AssembleSSE(ev)
	if m == nil || len(m.Content) != 1 {
		t.Fatalf("want 1 block, got %+v", m)
	}
	if got := blockField(t, m.Content[0], "thinking"); got != "hmm" {
		t.Errorf("thinking = %q", got)
	}
	if got := blockField(t, m.Content[0], "signature"); got != "sig" {
		t.Errorf("signature = %q", got)
	}
	if got := blockField(t, m.Content[0], "some_future_field"); got != float64(7) {
		t.Errorf("unmodelled field dropped: %v", got)
	}
}

func TestAssembleSSEEmptyStreamIsNil(t *testing.T) {
	if m := AssembleSSE(nil); m != nil {
		t.Errorf("want nil, got %+v", m)
	}
	if m := AssembleSSE(sse(frame(`{"type":"ping"}`))); m != nil {
		t.Errorf("want nil for a ping-only stream, got %+v", m)
	}
}

func TestAssembleJSON(t *testing.T) {
	body := []byte(`{"id":"msg_2","type":"message","role":"assistant","model":"claude-opus-5",
		"stop_reason":"tool_use","content":[{"type":"tool_use","id":"toolu_3","name":"explain_command",
		"input":{"risk":"LOW"},"caller":{"type":"direct"}}]}`)
	m := AssembleJSON(body)
	if m == nil {
		t.Fatal("nil message")
	}
	if m.StopReason != "tool_use" || m.ID != "msg_2" {
		t.Errorf("header wrong: %+v", m)
	}
	if len(m.Content) != 1 {
		t.Fatalf("want 1 block, got %d", len(m.Content))
	}
	// Non-streaming bodies pass through verbatim, so fields this package does
	// not model must survive.
	if blockField(t, m.Content[0], "caller") == nil {
		t.Error("passthrough lost an unmodelled field")
	}
}

func TestAssembleJSONIgnoresErrorEnvelopes(t *testing.T) {
	for _, body := range []string{
		`{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`,
		`{"content":[]}`,
		`not json`,
	} {
		if m := AssembleJSON([]byte(body)); m != nil {
			t.Errorf("%s: want nil, got %+v", body, m)
		}
	}
}
