package wire

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strings"

	"go.inndy.tw/goxray/internal/model"
)

// Anthropic parses the Anthropic Messages API wire format (Claude Code).
// Logic ported from ccxray: server/store.js (session/cwd/subagent),
// server/wire-parsers/anthropic.js (usage), server/helpers.js (title/SSE).
type Anthropic struct{}

// ── request-side types ──────────────────────────────────────────────

type anthropicReq struct {
	Model     string            `json:"model"`
	Messages  []json.RawMessage `json:"messages"`
	System    json.RawMessage   `json:"system"` // string OR array of blocks
	Tools     []json.RawMessage `json:"tools"`
	MaxTokens int               `json:"max_tokens"`
	Metadata  struct {
		SessionID string `json:"session_id"`
		UserID    string `json:"user_id"`
	} `json:"metadata"`
}

var (
	reSessionJSON = regexp.MustCompile(`"session_id"\s*:\s*"([a-f0-9-]+)"`)
	reSessionLeg  = regexp.MustCompile(`session_([a-f0-9-]+)`)
	reCwd         = regexp.MustCompile(`Primary working directory:\s*(.+)`)
	reTitle       = regexp.MustCompile(`"title"\s*:\s*"((?:[^"\\]|\\.)*)"`)
)

// IsNoise: token pre-counts (endpoint) and quota probes (body shape). Recorded
// but flagged. count_tokens/quota bodies contain real content sent to Anthropic,
// so unlike ccxray we never drop them — we only mark them.
func (Anthropic) IsNoise(method, path string, _ http.Header, body []byte) bool {
	switch strings.SplitN(path, "?", 2)[0] {
	case "/v1/messages/count_tokens", // token pre-count (contains real content)
		"/api/hello": // Claude Code connectivity preflight (bodyless)
		return true
	}
	return isQuotaCheck(body)
}

func isQuotaCheck(body []byte) bool {
	var r struct {
		MaxTokens int             `json:"max_tokens"`
		System    json.RawMessage `json:"system"`
		Messages  []struct {
			Content json.RawMessage `json:"content"`
		} `json:"messages"`
	}
	if json.Unmarshal(body, &r) != nil {
		return false
	}
	if r.MaxTokens != 1 || len(r.System) != 0 || len(r.Messages) != 1 {
		return false
	}
	var s string
	return json.Unmarshal(r.Messages[0].Content, &s) == nil && s == "quota"
}

func (Anthropic) ParseRequest(_ http.Header, body []byte) model.ReqMeta {
	var req anthropicReq
	if json.Unmarshal(body, &req) != nil {
		return model.ReqMeta{}
	}
	cwd := extractCwd(req.System)
	sid := extractSessionID(req.Metadata.SessionID, req.Metadata.UserID)
	return model.ReqMeta{
		Model:      req.Model,
		SessionID:  sid,
		Cwd:        cwd,
		MsgCount:   len(req.Messages),
		ToolCount:  len(req.Tools),
		IsSubagent: cwd == "" && sid == "", // MVP heuristic: no cwd and no explicit session
	}
}

// extractSessionID: explicit metadata.session_id, else parse out of user_id
// (JSON {"session_id":"..."} form or legacy session_xxx form).
func extractSessionID(explicit, userID string) string {
	if explicit != "" {
		return explicit
	}
	if m := reSessionJSON.FindStringSubmatch(userID); m != nil {
		return m[1]
	}
	if m := reSessionLeg.FindStringSubmatch(userID); m != nil {
		return m[1]
	}
	return ""
}

// extractCwd reads "Primary working directory: X" from the system prompt, which
// may be a plain string or an array of {type,text} blocks.
func extractCwd(system json.RawMessage) string {
	if len(system) == 0 {
		return ""
	}
	var txt string
	var s string
	if json.Unmarshal(system, &s) == nil {
		txt = s
	} else {
		var blocks []struct {
			Text string `json:"text"`
		}
		if json.Unmarshal(system, &blocks) == nil {
			var b strings.Builder
			for _, bl := range blocks {
				b.WriteString(bl.Text)
				b.WriteByte('\n')
			}
			txt = b.String()
		}
	}
	if m := reCwd.FindStringSubmatch(txt); m != nil {
		return strings.TrimSpace(m[1])
	}
	return ""
}

// ── response-side types ─────────────────────────────────────────────

type rawUsage struct {
	InputTokens              int `json:"input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
}

type sseData struct {
	Type    string `json:"type"`
	Message *struct {
		Usage      *rawUsage `json:"usage"`
		StopReason string    `json:"stop_reason"`
	} `json:"message"`
	Usage *rawUsage `json:"usage"`
	Delta *struct {
		Type       string `json:"type"`
		Text       string `json:"text"`
		StopReason string `json:"stop_reason"`
	} `json:"delta"`
	ContentBlock *struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content_block"`
}

func (Anthropic) ParseResponse(_ http.Header, raw []byte, contentType string) (model.ResMeta, bool) {
	if strings.Contains(contentType, "text/event-stream") {
		return parseSSE(raw)
	}
	return parseJSONMessage(raw)
}

// parseSSE aggregates usage/stop_reason/title across streamed events.
func parseSSE(raw []byte) (model.ResMeta, bool) {
	var meta model.ResMeta
	var any bool
	var text strings.Builder

	// Split into frames on blank lines; tolerate CRLF.
	norm := bytes.ReplaceAll(raw, []byte("\r\n"), []byte("\n"))
	for _, frame := range bytes.Split(norm, []byte("\n\n")) {
		for _, line := range bytes.Split(frame, []byte("\n")) {
			if !bytes.HasPrefix(line, []byte("data:")) {
				continue
			}
			payload := bytes.TrimSpace(line[len("data:"):])
			if len(payload) == 0 || payload[0] != '{' {
				continue
			}
			var d sseData
			if json.Unmarshal(payload, &d) != nil {
				continue
			}
			any = true
			switch d.Type {
			case "message_start":
				if d.Message != nil && d.Message.Usage != nil {
					applyUsage(&meta.Usage, d.Message.Usage)
				}
			case "message_delta":
				if d.Usage != nil && d.Usage.OutputTokens > 0 {
					meta.Usage.OutputTokens = d.Usage.OutputTokens
				}
				if d.Delta != nil && d.Delta.StopReason != "" {
					meta.StopReason = d.Delta.StopReason
				}
			case "content_block_delta":
				if d.Delta != nil && d.Delta.Type == "text_delta" {
					text.WriteString(d.Delta.Text)
				}
			case "content_block_start":
				if d.ContentBlock != nil && d.ContentBlock.Type == "text" {
					text.WriteString(d.ContentBlock.Text)
				}
			}
		}
	}
	meta.Title = extractTitle(text.String())
	return meta, any
}

// parseJSONMessage handles non-streaming responses (stream:false, errors).
func parseJSONMessage(raw []byte) (model.ResMeta, bool) {
	var msg struct {
		Type       string    `json:"type"`
		StopReason string    `json:"stop_reason"`
		Usage      *rawUsage `json:"usage"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if json.Unmarshal(raw, &msg) != nil {
		return model.ResMeta{}, false
	}
	var meta model.ResMeta
	meta.StopReason = msg.StopReason
	if msg.Usage != nil {
		applyUsage(&meta.Usage, msg.Usage)
	}
	var text strings.Builder
	for _, c := range msg.Content {
		if c.Type == "text" {
			text.WriteString(c.Text)
		}
	}
	meta.Title = extractTitle(text.String())
	return meta, msg.Type != ""
}

func applyUsage(dst *model.Usage, u *rawUsage) {
	if u.InputTokens > 0 {
		dst.InputTokens = u.InputTokens
	}
	if u.OutputTokens > 0 {
		dst.OutputTokens = u.OutputTokens
	}
	if u.CacheReadInputTokens > 0 {
		dst.CacheReadTokens = u.CacheReadInputTokens
	}
	if u.CacheCreationInputTokens > 0 {
		dst.CacheCreateTokens = u.CacheCreationInputTokens
	}
}

// extractTitle pulls a title-gen turn's title from its text ({"title":"..."}).
func extractTitle(text string) string {
	if text == "" {
		return ""
	}
	if m := reTitle.FindStringSubmatch(text); m != nil {
		var s string
		if json.Unmarshal([]byte(`"`+m[1]+`"`), &s) == nil {
			return s
		}
		return m[1]
	}
	return ""
}
