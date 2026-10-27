package view

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"strings"

	"go.inndy.tw/goxray/internal/wire"
)

// The dashboard's views were built around the Anthropic Messages shape. A
// Responses request is translated into that shape here, once, so every view —
// system blocks, steps, tool chips, token split — works on both providers.
// The translation is for reading only: the raw view still serves the bytes
// that crossed the wire.

type respItem struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Content   json.RawMessage `json:"content"`
	CallID    string          `json:"call_id"`
	Namespace string          `json:"namespace"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     json.RawMessage `json:"input"`
	Output    json.RawMessage `json:"output"`
	Summary   []struct {
		Text string `json:"text"`
	} `json:"summary"`
	// agent_message: a message one Codex agent sends another.
	Author    string `json:"author"`
	Recipient string `json:"recipient"`
}

// Encrypted is what the views show in place of a value the API encrypted. Codex
// marks some tool parameters encrypted — the task a sub-agent is given, a
// message to another agent — and the API hands them to the client already
// sealed, so neither Codex nor goxray ever holds the plaintext; only the model
// on the receiving end does.
const Encrypted = "[encrypted by the API — not disclosed to the client]"

// Encrypted parameters are the ones the request's own tool schema marks
// "encrypted": true (Codex: .with_encrypted() in multi_agents_spec.rs). Read
// from the schema, keyed by namespace and name exactly as a call addresses its
// tool, because nothing else identifies them: Codex's agent tools live in the
// "collaboration" namespace, an MCP server or a client-defined tool may use the
// same names, and a tool may take a Fernet token as an ordinary argument —
// calling any of those "encrypted by the API" would be false.
type EncryptedParams map[string][]string

func encKey(namespace, name string) string { return namespace + "/" + name }

// FindEncryptedParams reads a request's tool schemas — top-level tools and
// additional_tools input items, through namespaces — for encrypted parameters.
// Nil when there are none, or no request.
func FindEncryptedParams(req []byte) EncryptedParams {
	var r struct {
		Tools []json.RawMessage `json:"tools"`
		Input json.RawMessage   `json:"input"`
	}
	if json.Unmarshal(req, &r) != nil {
		return nil
	}
	var input []json.RawMessage
	_ = json.Unmarshal(r.Input, &input)
	var out EncryptedParams
	var walk func(list []json.RawMessage, ns string)
	walk = func(list []json.RawMessage, ns string) {
		for _, raw := range list {
			var t struct {
				Type       string            `json:"type"`
				Name       string            `json:"name"`
				Tools      []json.RawMessage `json:"tools"`
				Parameters struct {
					Properties map[string]struct {
						Encrypted bool `json:"encrypted"`
					} `json:"properties"`
				} `json:"parameters"`
			}
			if json.Unmarshal(raw, &t) != nil {
				continue
			}
			switch t.Type {
			case "namespace":
				walk(t.Tools, t.Name)
				continue
			case "additional_tools":
				walk(t.Tools, ns)
				continue
			}
			for k, p := range t.Parameters.Properties {
				if p.Encrypted {
					if out == nil {
						out = EncryptedParams{}
					}
					out[encKey(ns, t.Name)] = append(out[encKey(ns, t.Name)], k)
				}
			}
		}
	}
	walk(r.Tools, "")
	for _, it := range input {
		if wire.EventType(it) == "additional_tools" {
			walk([]json.RawMessage{it}, "")
		}
	}
	return out
}

// isFernet reports whether s is a Fernet token, the form those values arrive in:
// URL-safe base64 of version byte 0x80, an 8-byte timestamp, a 16-byte IV, AES
// blocks, and a 32-byte HMAC. Checked even for a parameter marked encrypted, so
// a client that sends one in the clear is shown in the clear.
func isFernet(s string) bool {
	if len(s) < 80 || !strings.HasPrefix(s, "gAAAAA") {
		return false
	}
	b, err := base64.URLEncoding.DecodeString(s)
	if err != nil {
		return false
	}
	const overhead = 1 + 8 + 16 + 32
	return b[0] == 0x80 && len(b) > overhead && (len(b)-overhead)%16 == 0
}

// redactEncrypted replaces a call's encrypted arguments, which would otherwise
// read as a corrupt base64 blob.
func (enc EncryptedParams) redact(namespace, name string, args map[string]any) map[string]any {
	for _, k := range enc[encKey(namespace, name)] {
		if s, ok := args[k].(string); ok && isFernet(s) {
			args[k] = Encrypted
		}
	}
	return args
}

type respTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Format      json.RawMessage `json:"format"`
}

// optionKeys are the request settings carried over for the Options view.
var optionKeys = []string{
	"model", "reasoning", "text", "tool_choice", "parallel_tool_calls", "service_tier",
	"prompt_cache_key", "store", "stream", "include", "previous_response_id", "client_metadata", "generate",
}

// FromResponses rewrites a Responses API request as an Anthropic Messages
// request. Leading developer and system messages — Codex's instructions —
// become the system prompt; the rest of the input becomes messages, with tool
// calls and their outputs as tool_use and tool_result blocks.
func FromResponses(req []byte) []byte {
	var r map[string]json.RawMessage
	if json.Unmarshal(req, &r) != nil {
		return nil
	}
	var items []json.RawMessage
	_ = json.Unmarshal(r["input"], &items)

	out := map[string]any{}
	for _, k := range optionKeys {
		if v, ok := r[k]; ok {
			out[k] = v
		}
	}

	// Tool definitions that ride in the input (Responses Lite) are tools, not
	// conversation; they are listed with the others and left out of messages.
	var conv []json.RawMessage
	for _, it := range items {
		if wire.EventType(it) != "additional_tools" {
			conv = append(conv, it)
		}
	}
	var system []map[string]string
	i := 0
	for ; i < len(conv); i++ {
		var it respItem
		if json.Unmarshal(conv[i], &it) != nil || it.Type != "message" || (it.Role != "developer" && it.Role != "system") {
			break
		}
		for _, t := range contentTexts(it.Content) {
			system = append(system, map[string]string{"type": "text", "text": t})
		}
	}
	out["system"] = system
	out["messages"] = itemsToMessages(conv[i:], FindEncryptedParams(req))

	var tools []json.RawMessage
	_ = json.Unmarshal(r["tools"], &tools)
	out["tools"] = convertTools(wire.Tools(tools, items))
	b, _ := marshal(out)
	return b
}

type msg struct {
	Role    string            `json:"role"`
	Content []json.RawMessage `json:"content"`
}

// itemsToMessages turns Responses items into alternating messages.
// Consecutive items on the same side are merged, so reasoning and the parallel
// calls it led to read as one assistant turn, and their outputs as one reply.
func itemsToMessages(items []json.RawMessage, enc EncryptedParams) []msg {
	var out []msg
	push := func(role string, block json.RawMessage) {
		if n := len(out); n > 0 && out[n-1].Role == role {
			out[n-1].Content = append(out[n-1].Content, block)
			return
		}
		out = append(out, msg{Role: role, Content: []json.RawMessage{block}})
	}
	for _, raw := range items {
		role, block := convertItem(raw, enc)
		if block != nil {
			push(role, block)
		}
	}
	return out
}

// convertItem maps one item to the role it is spoken in and a content block.
func convertItem(raw json.RawMessage, enc EncryptedParams) (string, json.RawMessage) {
	var it respItem
	if json.Unmarshal(raw, &it) != nil {
		return "", nil
	}
	switch it.Type {
	case "message", "":
		role := it.Role
		if role == "developer" {
			role = "system"
		}
		return role, textBlock(strings.Join(contentTexts(it.Content), "\n"))
	case "reasoning":
		var parts []string
		for _, s := range it.Summary {
			parts = append(parts, s.Text)
		}
		b, _ := marshal(map[string]string{"type": "thinking", "thinking": strings.Join(parts, "\n\n")})
		return "assistant", b
	case "function_call":
		var input any = map[string]string{"arguments": it.Arguments}
		var parsed map[string]any
		if json.Unmarshal([]byte(it.Arguments), &parsed) == nil {
			input = enc.redact(it.Namespace, it.Name, parsed)
		}
		return "assistant", toolUse(it.CallID, wire.ToolName(it.Namespace, it.Name), input)
	case "custom_tool_call":
		var s string
		if json.Unmarshal(it.Input, &s) != nil {
			s = string(it.Input)
		}
		return "assistant", toolUse(it.CallID, wire.ToolName(it.Namespace, it.Name), map[string]string{"input": s})
	case "function_call_output", "custom_tool_call_output", "local_shell_call_output", "tool_search_output":
		b, _ := marshal(map[string]any{"type": "tool_result", "tool_use_id": it.CallID, "content": outputText(it.Output)})
		return "user", b
	case "compaction":
		return "system", textBlock("[compaction: the earlier conversation, summarised by the server and encrypted]")
	case "agent_message":
		// Delivered to the model by Codex, not typed by anyone, so it reads as
		// context: a task from the parent, or a sub-agent's answer coming back.
		head := "[agent message] " + it.Author + " → " + it.Recipient
		return "system", textBlock(strings.Join(append([]string{head}, contentTexts(it.Content)...), "\n"))
	default:
		// web_search_call, local_shell_call, image_generation_call, …: shown as a
		// call so it lands in the timeline, with the item itself as its input.
		name := it.Name
		if name == "" {
			name = it.Type
		}
		var input any
		_ = json.Unmarshal(raw, &input)
		return "assistant", toolUse(firstNonEmpty(it.CallID, name), name, input)
	}
}

// AssembleResponses rebuilds a turn's reply from its output items, in the shape
// AssembleSSE gives an Anthropic reply. truncated marks a turn that never saw
// a terminal event. enc comes from the turn's request (FindEncryptedParams): the
// reply's calls are to the tools that request defined.
func AssembleResponses(output []json.RawMessage, enc EncryptedParams, model, id, stop string, truncated bool) *wire.AssembledMessage {
	m := &wire.AssembledMessage{Role: "assistant", Model: model, ID: id, StopReason: stop, Truncated: truncated}
	for _, raw := range output {
		if _, block := convertItem(raw, enc); block != nil {
			m.Content = append(m.Content, block)
		}
	}
	if len(m.Content) == 0 && !truncated {
		return nil
	}
	return m
}

// ResponsesOutput collects the output items of a recorded Responses stream:
// each response.output_item.done, or failing that the terminal event's copy.
// It also reports the terminal event's type, "" if the stream never got one.
func ResponsesOutput(events [][]byte) (items []json.RawMessage, terminal string) {
	var final []json.RawMessage
	for _, e := range events {
		typ := wire.EventType(e)
		switch {
		case typ == "response.output_item.done":
			var ev struct {
				Item json.RawMessage `json:"item"`
			}
			if json.Unmarshal(e, &ev) == nil && len(ev.Item) > 0 {
				items = append(items, ev.Item)
			}
		case wire.IsTerminalEvent(typ):
			terminal = typ
			var ev struct {
				Response *struct {
					Output []json.RawMessage `json:"output"`
				} `json:"response"`
			}
			if json.Unmarshal(e, &ev) == nil && ev.Response != nil {
				final = ev.Response.Output
			}
		}
	}
	if len(items) == 0 {
		items = final
	}
	return items, terminal
}

func convertTools(tools []json.RawMessage) []map[string]any {
	out := []map[string]any{}
	for _, raw := range tools {
		var t respTool
		if json.Unmarshal(raw, &t) != nil {
			continue
		}
		name := t.Name
		if name == "" {
			name = t.Type
		}
		def := map[string]any{"name": name, "description": t.Description}
		switch {
		case len(t.Parameters) > 0:
			def["input_schema"] = t.Parameters
		case len(t.Format) > 0:
			def["input_schema"] = t.Format
		}
		out = append(out, def)
	}
	return out
}

// contentTexts pulls the text out of a message's content, which is a string or
// a list of parts. Non-text parts are named rather than dropped.
func contentTexts(c json.RawMessage) []string {
	var s string
	if json.Unmarshal(c, &s) == nil {
		return []string{s}
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if json.Unmarshal(c, &parts) != nil {
		return nil
	}
	var out []string
	for _, p := range parts {
		switch {
		case p.Type == "encrypted_content":
			out = append(out, Encrypted)
		case p.Text != "":
			out = append(out, p.Text)
		case p.Type != "":
			out = append(out, "["+p.Type+"]")
		}
	}
	return out
}

func outputText(o json.RawMessage) any {
	var s string
	if json.Unmarshal(o, &s) == nil {
		return s
	}
	if t := contentTexts(o); len(t) > 0 {
		return strings.Join(t, "\n")
	}
	return string(o)
}

func textBlock(text string) json.RawMessage {
	b, _ := marshal(map[string]string{"type": "text", "text": text})
	return b
}

func toolUse(id, name string, input any) json.RawMessage {
	b, _ := marshal(map[string]any{"type": "tool_use", "id": id, "name": name, "input": input})
	return b
}

// marshal is json.Marshal without HTML escaping; the conversation is full of
// angle brackets the reader should see as written.
func marshal(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}

func firstNonEmpty(ss ...string) string {
	for _, s := range ss {
		if s != "" {
			return s
		}
	}
	return ""
}
