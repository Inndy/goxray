package wire

import (
	"encoding/json"
	"strings"
)

// AssembledMessage is the assistant message a recorded response represents.
//
// It exists because a turn's own reply is not in its request: messages[] ends
// with the user or tool_result that prompted the model. Without reassembling
// the response, the newest turn's output is visible nowhere but the raw bytes.
//
// Content blocks stay as raw JSON so unmodelled block types survive intact.
type AssembledMessage struct {
	Role       string            `json:"role"`
	Model      string            `json:"model,omitempty"`
	ID         string            `json:"id,omitempty"`
	StopReason string            `json:"stop_reason,omitempty"`
	Content    []json.RawMessage `json:"content"`
	// Truncated marks a stream that never reached message_stop — an aborted or
	// errored recording. The blocks below it are a prefix, not the whole reply.
	Truncated bool `json:"truncated,omitempty"`
}

// AssembleJSON reads a non-streaming Messages response. Error envelopes and
// anything else without content yield nil.
func AssembleJSON(body []byte) *AssembledMessage {
	var m struct {
		Role       string            `json:"role"`
		Model      string            `json:"model"`
		ID         string            `json:"id"`
		StopReason string            `json:"stop_reason"`
		Content    []json.RawMessage `json:"content"`
	}
	if json.Unmarshal(body, &m) != nil || len(m.Content) == 0 {
		return nil
	}
	if m.Role == "" {
		m.Role = "assistant"
	}
	return &AssembledMessage{
		Role: m.Role, Model: m.Model, ID: m.ID,
		StopReason: m.StopReason, Content: m.Content,
	}
}

// AssembleSSE replays a stream's events back into the message they built.
//
// Every block is emitted whether or not its content_block_stop arrived, so a
// recording cut short still shows how far the model got.
func AssembleSSE(events []SSEEvent) *AssembledMessage {
	msg := &AssembledMessage{Role: "assistant", Truncated: true}
	blocks := map[int]*blockAcc{}
	var order []int

	at := func(i int) *blockAcc {
		b, ok := blocks[i]
		if !ok {
			b = &blockAcc{}
			blocks[i] = b
			order = append(order, i)
		}
		return b
	}

	for _, ev := range events {
		switch ev.Type {
		case "message_start":
			var d struct {
				Message struct {
					Role  string `json:"role"`
					Model string `json:"model"`
					ID    string `json:"id"`
				} `json:"message"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				if d.Message.Role != "" {
					msg.Role = d.Message.Role
				}
				msg.Model, msg.ID = d.Message.Model, d.Message.ID
			}

		case "content_block_start":
			var d struct {
				Index int            `json:"index"`
				Block map[string]any `json:"content_block"`
			}
			if json.Unmarshal(ev.Data, &d) == nil {
				at(d.Index).raw = d.Block
			}

		case "content_block_delta":
			var d struct {
				Index int `json:"index"`
				Delta struct {
					Type        string `json:"type"`
					Text        string `json:"text"`
					Thinking    string `json:"thinking"`
					PartialJSON string `json:"partial_json"`
					Signature   string `json:"signature"`
				} `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) != nil {
				continue
			}
			b := at(d.Index)
			switch d.Delta.Type {
			case "text_delta":
				b.text.WriteString(d.Delta.Text)
			case "thinking_delta":
				b.thinking.WriteString(d.Delta.Thinking)
			case "input_json_delta":
				b.input.WriteString(d.Delta.PartialJSON)
			case "signature_delta":
				b.signature.WriteString(d.Delta.Signature)
			}

		case "message_delta":
			var d struct {
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if json.Unmarshal(ev.Data, &d) == nil && d.Delta.StopReason != "" {
				msg.StopReason = d.Delta.StopReason
			}

		case "message_stop":
			msg.Truncated = false
		}
	}

	for _, i := range order {
		if raw := blocks[i].finish(); raw != nil {
			msg.Content = append(msg.Content, raw)
		}
	}
	if len(msg.Content) == 0 && msg.StopReason == "" {
		return nil
	}
	return msg
}

type blockAcc struct {
	raw       map[string]any
	text      strings.Builder
	thinking  strings.Builder
	signature strings.Builder
	input     strings.Builder
}

func (b *blockAcc) finish() json.RawMessage {
	m := b.raw
	if m == nil {
		m = map[string]any{}
	}
	if s := b.text.String(); s != "" {
		m["text"] = asString(m["text"]) + s
	}
	if s := b.thinking.String(); s != "" {
		m["thinking"] = asString(m["thinking"]) + s
	}
	if s := b.signature.String(); s != "" {
		m["signature"] = s
	}
	if s := b.input.String(); s != "" {
		var v any
		if json.Unmarshal([]byte(s), &v) == nil {
			m["input"] = v
		} else {
			// A tool call whose argument JSON was cut mid-stream. Keeping the
			// fragment beats dropping the only record of what was being called.
			delete(m, "input")
			m["partial_input"] = s
		}
	}
	if len(m) == 0 {
		return nil
	}
	out, err := json.Marshal(m)
	if err != nil {
		return nil
	}
	return out
}

func asString(v any) string {
	s, _ := v.(string)
	return s
}
