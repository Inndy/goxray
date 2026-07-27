package wire

import "testing"

const cannedSSE = `event: message_start
data: {"type":"message_start","message":{"id":"msg_1","usage":{"input_tokens":10,"cache_read_input_tokens":5,"cache_creation_input_tokens":2}}}

event: content_block_start
data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}

event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello"}}

event: message_delta
data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":7}}

event: message_stop
data: {"type":"message_stop"}

`

func TestParseRequest(t *testing.T) {
	body := []byte(`{
		"model":"claude-opus-4-8",
		"metadata":{"session_id":"abc-123"},
		"system":[{"type":"text","text":"You are Claude.\nPrimary working directory: /home/dev/proj\n"}],
		"tools":[{"name":"Bash"},{"name":"Read"}],
		"messages":[{"role":"user","content":"hi"},{"role":"assistant","content":"yo"}]
	}`)
	m := Anthropic{}.ParseRequest(nil, body)
	if m.Model != "claude-opus-4-8" {
		t.Errorf("model = %q", m.Model)
	}
	if m.SessionID != "abc-123" {
		t.Errorf("session = %q", m.SessionID)
	}
	if m.Cwd != "/home/dev/proj" {
		t.Errorf("cwd = %q", m.Cwd)
	}
	if m.MsgCount != 2 || m.ToolCount != 2 {
		t.Errorf("counts msg=%d tool=%d", m.MsgCount, m.ToolCount)
	}
	if m.IsSubagent {
		t.Errorf("should not be subagent (has cwd+session)")
	}
}

func TestParseRequestSubagentHeuristic(t *testing.T) {
	// No cwd, no session_id → subagent.
	m := Anthropic{}.ParseRequest(nil, []byte(`{"model":"x","messages":[{"role":"user","content":"go"}]}`))
	if !m.IsSubagent {
		t.Errorf("expected subagent")
	}
}

func TestSessionFromUserID(t *testing.T) {
	m := Anthropic{}.ParseRequest(nil, []byte(`{"metadata":{"user_id":"{\"session_id\":\"deadbeef-1\"}"}}`))
	if m.SessionID != "deadbeef-1" {
		t.Errorf("session = %q", m.SessionID)
	}
}

func TestParseResponseSSE(t *testing.T) {
	meta, ok := Anthropic{}.ParseResponse(nil, []byte(cannedSSE), "text/event-stream")
	if !ok {
		t.Fatal("expected ok")
	}
	if meta.Usage.InputTokens != 10 || meta.Usage.OutputTokens != 7 {
		t.Errorf("usage = %+v", meta.Usage)
	}
	if meta.Usage.CacheReadTokens != 5 || meta.Usage.CacheCreateTokens != 2 {
		t.Errorf("cache usage = %+v", meta.Usage)
	}
	if meta.StopReason != "end_turn" {
		t.Errorf("stop = %q", meta.StopReason)
	}
}

func TestParseResponseCRLF(t *testing.T) {
	crlf := ""
	for _, line := range []string{
		"event: message_delta",
		`data: {"type":"message_delta","delta":{"stop_reason":"max_tokens"},"usage":{"output_tokens":42}}`,
	} {
		crlf += line + "\r\n"
	}
	crlf += "\r\n"
	meta, ok := Anthropic{}.ParseResponse(nil, []byte(crlf), "text/event-stream")
	if !ok || meta.StopReason != "max_tokens" || meta.Usage.OutputTokens != 42 {
		t.Errorf("crlf parse: ok=%v meta=%+v", ok, meta)
	}
}

func TestParseResponseNonSSE(t *testing.T) {
	body := []byte(`{"type":"message","stop_reason":"end_turn","usage":{"input_tokens":3,"output_tokens":4},"content":[{"type":"text","text":"hi"}]}`)
	meta, ok := Anthropic{}.ParseResponse(nil, body, "application/json")
	if !ok || meta.StopReason != "end_turn" || meta.Usage.OutputTokens != 4 {
		t.Errorf("nonSSE: ok=%v meta=%+v", ok, meta)
	}
}

func TestParseResponseSchemaDrift(t *testing.T) {
	// Unknown top-level shape → not ok, no panic, zero meta.
	meta, ok := Anthropic{}.ParseResponse(nil, []byte(`not json at all`), "application/json")
	if ok {
		t.Errorf("expected not ok on garbage")
	}
	if meta.Usage.OutputTokens != 0 {
		t.Errorf("expected zero meta")
	}
}

func TestIsNoise(t *testing.T) {
	a := Anthropic{}
	if !a.IsNoise("POST", "/v1/messages/count_tokens", nil, []byte(`{}`)) {
		t.Error("count_tokens should be noise")
	}
	if !a.IsNoise("HEAD", "/api/hello", nil, nil) {
		t.Error("/api/hello connectivity preflight should be noise")
	}
	quota := []byte(`{"max_tokens":1,"messages":[{"role":"user","content":"quota"}]}`)
	if !a.IsNoise("POST", "/v1/messages", nil, quota) {
		t.Error("quota probe should be noise")
	}
	if a.IsNoise("POST", "/v1/messages", nil, []byte(`{"model":"x","messages":[]}`)) {
		t.Error("normal request should not be noise")
	}
}
