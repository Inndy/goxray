package wire

import (
	"bytes"
	"encoding/json"
)

// IsTerminalEvent reports whether a Responses stream event ends its response.
func IsTerminalEvent(typ string) bool {
	switch typ {
	case "response.completed", "response.failed", "response.incomplete", "error":
		return true
	}
	return false
}

// IsHeldEvent reports whether an upstream event must not reach the client
// before the turn is durably committed. That is every event a client may take
// as "this turn is over": the terminal events, and the interrupt
// acknowledgements, which Codex waits for before it releases an interrupted
// turn — and which can arrive after the terminal event.
func IsHeldEvent(typ string) bool {
	return IsTerminalEvent(typ) || typ == "response.interrupt.accepted" || typ == "response.interrupt.failed"
}

// EventType returns the "type" of a JSON event payload, or "".
func EventType(payload []byte) string {
	var probe struct {
		Type string `json:"type"`
	}
	if json.Unmarshal(payload, &probe) != nil {
		return ""
	}
	return probe.Type
}

// Turn is one response.create on a connection and everything up to the next
// one, as replayed from the connection's log.
//
// A turn runs to the next response.create (or the end of the log) rather than
// to its terminal event, because events can follow the terminal one: Codex may
// receive response.interrupt.accepted after an interrupted response has
// already ended.
type Turn struct {
	// ID is the turn exchange's id, from the turn record goxray wrote just
	// before the create. Empty in a log that has none.
	ID          string
	First, Last int64 // record numbers: the create, and the last record of the turn
	// TerminalN is the record number of the latest terminal event, 0 if none.
	TerminalN int64
	StartedMS int64
	EndedMS   int64

	// Create is the response.create frame exactly as sent.
	Create []byte
	// Events are the upstream data frames that answered it, exactly as sent.
	Events [][]byte
	// ClientFrames are any further client frames inside the turn
	// (response.interrupt), exactly as sent.
	ClientFrames [][]byte

	// Request is Create with "input" replaced by the full conversation the
	// model saw: the frame only carries what is new since PrevResponseID. Nil
	// when Create is not a JSON object.
	Request json.RawMessage
	// Incomplete is set when the frame continued a response this log does not
	// hold, so Request is only the part that was sent.
	Incomplete     bool
	PrevResponseID string
	ResponseID     string
	// Terminal is the type of the latest terminal event; empty if none came.
	Terminal string
	// AwaitingAck is set while a response.interrupt the client sent has not
	// been acknowledged. Codex keeps reading for the acknowledgement after the
	// terminal event, so the turn is not over for it until this clears.
	AwaitingAck bool

	full   []json.RawMessage // full input
	output []json.RawMessage // output items, in order
	final  []json.RawMessage // the terminal event's response.output, the fallback
}

// Step is what one record did.
type Step struct {
	// Turn is the turn the record belongs to, if any.
	Turn *Turn
	// Started is set when the record was a response.create that began Turn.
	Started bool
	// Held is set when the record is an event that must not reach the client
	// before Turn is committed (IsHeldEvent).
	Held bool
	// Ended is a turn this record closed: the previous one, on a new create, or
	// the open one, on the end record.
	Ended *Turn
}

// Replayer folds a connection log into turns, front to back. The recorder runs
// one live, frame by frame; the dashboard runs one over a stored log. Both get
// the same answer because it is the same code.
type Replayer struct {
	// contexts maps a response id to the conversation a continuation of it
	// extends: that response's full input followed by its output.
	contexts map[string][]json.RawMessage
	cur      *Turn
	// nextID is from a turn record, and names the create only if that is the
	// very next record, nextN+1: the recorder writes the two together.
	nextID string
	nextN  int64
}

// Quiescent reports whether the connection is between turns as the client sees
// it: no turn open, or the open one has its terminal event and no interrupt
// acknowledgement outstanding.
func (r *Replayer) Quiescent() bool {
	return r.cur == nil || (r.cur.Terminal != "" && !r.cur.AwaitingAck)
}

func NewReplayer() *Replayer {
	return &Replayer{contexts: map[string][]json.RawMessage{}}
}

// Current returns the open turn, or nil.
func (r *Replayer) Current() *Turn { return r.cur }

// Feed applies one record.
func (r *Replayer) Feed(rec Record) Step {
	if !rec.IsData() {
		switch rec.Op {
		case OpTurn:
			r.nextID, r.nextN = rec.ID, rec.N
			return Step{}
		case OpEnd:
			if r.cur != nil {
				r.cur.Last = rec.N
				return Step{Ended: r.finish(rec.T)}
			}
			return Step{}
		}
		if r.cur != nil {
			r.cur.Last = rec.N
		}
		return Step{Turn: r.cur}
	}
	payload := rec.Payload()
	typ := EventType(payload)

	if rec.D == DirClient {
		if typ == "response.create" {
			var ended *Turn
			if r.cur != nil {
				ended = r.finish(rec.T)
			}
			r.start(rec, payload)
			return Step{Turn: r.cur, Started: true, Ended: ended}
		}
		if r.cur != nil {
			r.cur.Last = rec.N
			r.cur.ClientFrames = append(r.cur.ClientFrames, payload)
			if typ == "response.interrupt" {
				r.cur.AwaitingAck = true
			}
		}
		return Step{Turn: r.cur}
	}

	t := r.cur
	if t == nil {
		return Step{} // an event with no request to answer; the log still has it
	}
	t.Last = rec.N
	t.Events = append(t.Events, payload)
	switch typ {
	case "response.created", "response.in_progress":
		if id := responseID(payload); id != "" {
			t.ResponseID = id
		}
	case "response.output_item.done":
		var ev struct {
			Item json.RawMessage `json:"item"`
		}
		if json.Unmarshal(payload, &ev) == nil && len(ev.Item) > 0 {
			t.output = append(t.output, ev.Item)
		}
	case "response.interrupt.accepted", "response.interrupt.failed":
		t.AwaitingAck = false
	}
	if IsTerminalEvent(typ) {
		var ev struct {
			Response *struct {
				ID     string            `json:"id"`
				Output []json.RawMessage `json:"output"`
			} `json:"response"`
		}
		if json.Unmarshal(payload, &ev) == nil && ev.Response != nil {
			if ev.Response.ID != "" {
				t.ResponseID = ev.Response.ID
			}
			if ev.Response.Output != nil {
				t.final = ev.Response.Output
			}
		}
		t.Terminal, t.TerminalN = typ, rec.N
		r.remember(t)
	}
	return Step{Turn: t, Held: IsHeldEvent(typ)}
}

func (r *Replayer) start(rec Record, payload []byte) {
	t := &Turn{First: rec.N, Last: rec.N, StartedMS: rec.T, Create: payload}
	if r.nextID != "" && rec.N == r.nextN+1 {
		t.ID = r.nextID
	}
	r.nextID = ""
	var frame struct {
		Input              json.RawMessage `json:"input"`
		PreviousResponseID string          `json:"previous_response_id"`
	}
	if json.Unmarshal(payload, &frame) == nil {
		t.PrevResponseID = frame.PreviousResponseID
		var input []json.RawMessage
		if len(frame.Input) > 0 && frame.Input[0] == '[' {
			_ = json.Unmarshal(frame.Input, &input)
		} else if len(frame.Input) > 0 && !bytes.Equal(frame.Input, []byte("null")) {
			// A bare string is one user message in the Responses API.
			input = []json.RawMessage{userMessage(frame.Input)}
		}
		if t.PrevResponseID != "" {
			base, ok := r.contexts[t.PrevResponseID]
			if !ok {
				t.Incomplete = true
			}
			full := make([]json.RawMessage, 0, len(base)+len(input))
			t.full = append(append(full, base...), input...)
		} else {
			t.full = input
		}
		t.Request = withInput(payload, t.full)
	}
	r.cur = t
}

func (r *Replayer) finish(endMS int64) *Turn {
	t := r.cur
	r.cur = nil
	t.EndedMS = endMS
	r.remember(t)
	return t
}

// remember makes a response available as a continuation base, whatever its
// terminal state: Codex continues an interrupted response.incomplete exactly as
// it does a completed one.
func (r *Replayer) remember(t *Turn) {
	if t.ResponseID == "" {
		return
	}
	out := t.Output()
	ctx := make([]json.RawMessage, 0, len(t.full)+len(out))
	r.contexts[t.ResponseID] = append(append(ctx, t.full...), out...)
}

// Output returns the response's output items: those streamed as
// response.output_item.done, or failing that the terminal event's copy.
func (t *Turn) Output() []json.RawMessage {
	if len(t.output) > 0 {
		return t.output
	}
	return t.final
}

// FullInput returns the conversation the model saw for this turn.
func (t *Turn) FullInput() []json.RawMessage { return t.full }

// EventsJSONL returns the turn's upstream events one per line — the shape
// ParseResponse takes for a WebSocket turn.
func (t *Turn) EventsJSONL() []byte {
	var b bytes.Buffer
	for _, e := range t.Events {
		b.Write(e)
		b.WriteByte('\n')
	}
	return b.Bytes()
}

// ReplayTurn replays log up to the end of the turn whose response.create is
// record first, and returns it; nil if the log has no such turn. last, if
// positive, stops the replay after that record: a manifest's range is the
// authority on what belongs to its turn, and nothing past it is shown as part
// of it.
func ReplayTurn(log []byte, first, last int64) *Turn {
	r := NewReplayer()
	var found *Turn
	EachRecord(log, func(rec Record) bool {
		if last > 0 && rec.N > last {
			return false
		}
		if st := r.Feed(rec); st.Ended != nil && st.Ended.First == first {
			found = st.Ended
			return false
		}
		return true
	})
	if found == nil {
		// Still open when the log ends: an inflight turn, or a crash.
		if cur := r.Current(); cur != nil && cur.First == first {
			found = cur
		}
	}
	return found
}

func responseID(payload []byte) string {
	var ev struct {
		Response *struct {
			ID string `json:"id"`
		} `json:"response"`
	}
	if json.Unmarshal(payload, &ev) == nil && ev.Response != nil {
		return ev.Response.ID
	}
	return ""
}

func userMessage(text json.RawMessage) json.RawMessage {
	b, _ := marshalNoEscape(map[string]any{"type": "message", "role": "user", "content": text})
	return b
}

// withInput returns the frame with "input" replaced. Every other field is
// carried over as sent: in the Responses API a continuation inherits only the
// conversation, never tools or settings, and Codex resends those every time.
func withInput(frame []byte, input []json.RawMessage) json.RawMessage {
	var obj map[string]json.RawMessage
	if json.Unmarshal(frame, &obj) != nil {
		return nil
	}
	if input == nil {
		input = []json.RawMessage{}
	}
	b, err := marshalNoEscape(input)
	if err != nil {
		return nil
	}
	obj["input"] = b
	out, err := marshalNoEscape(obj)
	if err != nil {
		return nil
	}
	return out
}

// marshalNoEscape is json.Marshal without HTML escaping. Marshal would turn
// every '<' in the conversation into <, which is still valid JSON but no
// longer the text the model saw — and Codex's context is full of tags.
func marshalNoEscape(v any) ([]byte, error) {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(b.Bytes(), "\n"), nil
}
