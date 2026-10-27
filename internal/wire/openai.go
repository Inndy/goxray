package wire

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"go.inndy.tw/goxray/internal/model"
)

// OpenAI parses the Responses API as Codex speaks it, over either transport.
//
// A request body is a full Responses request: an HTTP POST body, or a
// WebSocket turn's response.create with its input already replaced by the full
// conversation (see Replayer). A response is an SSE stream, a JSON response
// object, or a WebSocket turn's events as JSON Lines.
type OpenAI struct{}

type openaiReq struct {
	Model          string            `json:"model"`
	Input          json.RawMessage   `json:"input"`
	Tools          []json.RawMessage `json:"tools"`
	Generate       *bool             `json:"generate"`
	PrevResponseID string            `json:"previous_response_id"`
	ClientMetadata map[string]string `json:"client_metadata"`
	PromptCacheKey string            `json:"prompt_cache_key"`
}

// turnMetadata is the part of Codex's x-codex-turn-metadata this package uses.
// It travels as a JSON string inside client_metadata.
type turnMetadata struct {
	SessionID      string `json:"session_id"`
	ThreadID       string `json:"thread_id"`
	ParentThreadID string `json:"parent_thread_id"`
	AgentName      string `json:"agent_name"`
	RequestKind    string `json:"request_kind"`
	SubagentKind   string `json:"subagent_kind"`
	ThreadSource   string `json:"thread_source"`
}

func (r *openaiReq) turnMeta() turnMetadata {
	var tm turnMetadata
	if s := r.ClientMetadata["x-codex-turn-metadata"]; s != "" {
		_ = json.Unmarshal([]byte(s), &tm)
	}
	return tm
}

// noiseSources are Codex threads that exist only to decorate another one — a
// title, a recap — and are started as their own throwaway root sessions with no
// link back. Left visible they would each appear as a one-turn session.
var noiseSources = map[string]bool{"thread_title": true, "system": true}

func (OpenAI) IsNoise(method, path string, _ http.Header, body []byte) bool {
	if method == http.MethodGet && strings.HasSuffix(path, "/models") {
		return true
	}
	var req openaiReq
	if json.Unmarshal(body, &req) != nil {
		return false
	}
	// A prewarm asks for nothing to be generated; it only primes the
	// connection. It stays recorded because the next turn continues from it.
	if req.Generate != nil && !*req.Generate {
		return true
	}
	return noiseSources[req.turnMeta().ThreadSource]
}

var reCwdTag = regexp.MustCompile(`<cwd>([^<]*)</cwd>`)

func (OpenAI) ParseRequest(h http.Header, body []byte) model.ReqMeta {
	var req openaiReq
	if json.Unmarshal(body, &req) != nil {
		return model.ReqMeta{}
	}
	var input []json.RawMessage
	_ = json.Unmarshal(req.Input, &input)
	tm := req.turnMeta()
	md := req.ClientMetadata

	sid := firstNonEmpty(md["session_id"], tm.SessionID, h.Get("session-id"), h.Get("session_id"))
	subagent := firstNonEmpty(md["x-openai-subagent"], h.Get("x-openai-subagent"), tm.SubagentKind)
	return model.ReqMeta{
		Model:          req.Model,
		SessionID:      sid,
		Cwd:            lastCwd(input),
		MsgCount:       len(input),
		ToolCount:      len(Tools(req.Tools, input)),
		IsSubagent:     subagent != "",
		Thread:         firstNonEmpty(md["thread_id"], tm.ThreadID, h.Get("thread-id")),
		ParentThread:   firstNonEmpty(md["x-codex-parent-thread-id"], tm.ParentThreadID, h.Get("x-codex-parent-thread-id")),
		Agent:          agentName(tm.AgentName, subagent),
		Kind:           tm.RequestKind,
		PrevResponseID: req.PrevResponseID,
	}
}

// agentName is the agent path Codex reports, except where that path cannot tell
// the thread apart: a reviewer, a compaction or a guardian thread has no path of
// its own and reports the root's, so its sub-agent label says more.
func agentName(path, subagent string) string {
	if subagent != "" && (path == "" || path == "/root") {
		return subagent
	}
	return path
}

// lastCwd finds the working directory in Codex's <environment_context>. Later
// turns resend it only when it changes, so the last one in the full input is
// the current one.
func lastCwd(input []json.RawMessage) string {
	for i := len(input) - 1; i >= 0; i-- {
		// Matched without the brackets: a re-encoded body may carry them as <.
		if !bytes.Contains(input[i], []byte("environment_context")) {
			continue
		}
		var msg struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(input[i], &msg) != nil {
			continue
		}
		for j := len(msg.Content) - 1; j >= 0; j-- {
			if m := reCwdTag.FindAllStringSubmatch(msg.Content[j].Text, -1); m != nil {
				return strings.TrimSpace(m[len(m)-1][1])
			}
		}
	}
	return ""
}

// ── response side ───────────────────────────────────────────────────

type openaiUsage struct {
	InputTokens        int `json:"input_tokens"`
	OutputTokens       int `json:"output_tokens"`
	InputTokensDetails *struct {
		CachedTokens            int `json:"cached_tokens"`
		CacheWriteTokens        int `json:"cache_write_tokens"`
		CacheWriteTokensDetails *struct {
			TTL30m int `json:"ttl_30m_tokens"`
			TTL12h int `json:"ttl_12h_tokens"`
		} `json:"cache_write_tokens_details"`
	} `json:"input_tokens_details"`
	OutputTokensDetails *struct {
		ReasoningTokens int `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

type openaiResponse struct {
	ID                string            `json:"id"`
	Object            string            `json:"object"`
	Model             string            `json:"model"`
	Status            string            `json:"status"`
	ServiceTier       string            `json:"service_tier"`
	Usage             *openaiUsage      `json:"usage"`
	Output            []json.RawMessage `json:"output"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
}

func (OpenAI) ParseResponse(_ http.Header, raw []byte, contentType string) (model.ResMeta, bool) {
	var meta model.ResMeta
	var any bool
	apply := func(payload []byte) {
		var ev struct {
			Type     string          `json:"type"`
			Response *openaiResponse `json:"response"`
		}
		if json.Unmarshal(payload, &ev) != nil {
			return
		}
		any = true
		if ev.Response != nil {
			applyResponse(&meta, ev.Response)
		}
		if ev.Type == "error" {
			meta.StopReason = "error"
		}
	}

	switch {
	case strings.Contains(contentType, "text/event-stream"):
		SplitSSEData(raw, func(p []byte) bool { apply(p); return true })
	default:
		var resp openaiResponse
		if json.Unmarshal(raw, &resp) == nil && resp.Object == "response" {
			applyResponse(&meta, &resp)
			return meta, true
		}
		// A WebSocket turn's events, one per line.
		for line := range bytes.SplitSeq(raw, []byte("\n")) {
			if line = bytes.TrimSpace(line); len(line) > 0 {
				apply(line)
			}
		}
	}
	return meta, any
}

// applyResponse folds one response snapshot in. A stream carries several —
// created, in_progress, then the terminal one with usage — and each field is
// assigned, never summed, so repeats are idempotent.
func applyResponse(meta *model.ResMeta, r *openaiResponse) {
	if r.ID != "" {
		meta.ResponseID = r.ID
	}
	if r.Model != "" {
		meta.Model = r.Model
	}
	if r.ServiceTier != "" {
		meta.ServiceTier = r.ServiceTier
	}
	if r.Status != "" && r.Status != "in_progress" && r.Status != "queued" {
		meta.StopReason = r.Status
		if r.IncompleteDetails != nil && r.IncompleteDetails.Reason != "" {
			meta.StopReason += ":" + r.IncompleteDetails.Reason
		} else if r.Status == "completed" && endsInToolCall(r.Output) {
			meta.StopReason = "tool_use"
		}
	}
	if r.Usage != nil {
		meta.Usage = normalizeUsage(r.Usage)
	}
	if t := titleFromOutput(r.Output); t != "" {
		meta.Title = t
	}
}

// normalizeUsage converts Responses accounting into goxray's, which is
// Anthropic's: there, input_tokens excludes cache reads and writes; here it
// includes both. Converting at capture is what lets pricing and every context
// percentage treat both providers alike.
func normalizeUsage(u *openaiUsage) model.Usage {
	var cached, written, w30m, w12h, reasoning int
	if d := u.InputTokensDetails; d != nil {
		cached, written = d.CachedTokens, d.CacheWriteTokens
		// A 12-hour write costs twice a 30-minute one. The split rides in the
		// fields Anthropic's 5m/1h split uses, and the rate table prices
		// OpenAI's lifetimes in those slots; the two sum to written.
		if w := d.CacheWriteTokensDetails; w != nil {
			w30m, w12h = w.TTL30m, w.TTL12h
		}
	}
	if d := u.OutputTokensDetails; d != nil {
		reasoning = d.ReasoningTokens
	}
	return model.Usage{
		InputTokens:       max(u.InputTokens-cached-written, 0),
		OutputTokens:      u.OutputTokens,
		CacheReadTokens:   cached,
		CacheCreateTokens: written,
		CacheCreate5m:     w30m,
		CacheCreate1h:     w12h,
		ThinkingTokens:    reasoning,
	}
}

func endsInToolCall(output []json.RawMessage) bool {
	if len(output) == 0 {
		return false
	}
	typ := EventType(output[len(output)-1])
	return strings.HasSuffix(typ, "_call") && typ != "web_search_call"
}

// titleFromOutput picks a title out of a title-generation turn's text, the
// same {"title": "..."} convention Claude Code's title turns use.
func titleFromOutput(output []json.RawMessage) string {
	for _, item := range output {
		if !bytes.Contains(item, []byte(`title`)) {
			continue
		}
		var msg struct {
			Type    string `json:"type"`
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		if json.Unmarshal(item, &msg) != nil || msg.Type != "message" {
			continue
		}
		for _, c := range msg.Content {
			if t := extractTitle(c.Text); t != "" {
				return t
			}
		}
	}
	return ""
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}

// ToolName is the flat name of a tool inside a namespace: an MCP server's
// tools are spelled "mcp__<server>__<tool>", any other namespace's keep their
// own name. A call names its namespace and tool separately, and must come out
// spelled as its definition in Tools does.
func ToolName(namespace, name string) string {
	if strings.HasPrefix(namespace, "mcp__") && name != "" {
		return namespace + "__" + name
	}
	return name
}

// Tools returns a request's tool definitions as a flat list. Codex sends them
// two ways: in the top-level "tools" field, and — under Responses Lite — as an
// additional_tools item inside the input. Either way they may be grouped into
// namespaces, each with its own "tools". An MCP server's namespace is
// "mcp__<server>", so its tools are named "mcp__<server>__<tool>", which is the
// spelling the dashboard already files under MCP.
func Tools(tools, input []json.RawMessage) []json.RawMessage {
	var out []json.RawMessage
	var walk func(list []json.RawMessage, prefix string)
	walk = func(list []json.RawMessage, prefix string) {
		for _, raw := range list {
			var t struct {
				Type  string            `json:"type"`
				Name  string            `json:"name"`
				Tools []json.RawMessage `json:"tools"`
			}
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			if t.Type == "namespace" || (t.Type == "additional_tools" && len(t.Tools) > 0) {
				walk(t.Tools, t.Name)
				continue
			}
			if name := ToolName(prefix, t.Name); name != t.Name {
				var obj map[string]json.RawMessage
				if json.Unmarshal(raw, &obj) == nil {
					obj["name"], _ = json.Marshal(name)
					if b, err := json.Marshal(obj); err == nil {
						raw = b
					}
				}
			}
			out = append(out, raw)
		}
	}
	walk(tools, "")
	for _, it := range input {
		if bytes.Contains(it, []byte(`"additional_tools"`)) && EventType(it) == "additional_tools" {
			walk([]json.RawMessage{it}, "")
		}
	}
	return out
}
