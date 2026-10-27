package wire

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"
)

// logOf builds a connection log from (direction, payload) pairs.
func logOf(t *testing.T, frames ...[2]string) []byte {
	t.Helper()
	var b []byte
	for i, f := range frames {
		b = AppendRecord(b, Record{N: int64(i + 1), T: int64(1000 + i), D: f[0]}, []byte(f[1]), false)
	}
	return b
}

func TestRecordPayloadRoundTripsByteExact(t *testing.T) {
	payloads := []string{
		`{"type":"response.create","input":[]}`,
		`{"a": 1,  "b" : [ 1 , 2 ]}`, // internal whitespace must survive
		" {\"padded\":true} ",        // surrounding whitespace forces the string form
		"{\"multi\":\n1}",            // a newline would end the line
		`not json at all`,
		`"just a string"`,
		``,
		"{\"bad\":\"\xff\xfe\"}", // not UTF-8: a JSON string would turn it into U+FFFD
		"\xc3",                   // a sequence cut short
	}
	var log []byte
	for i, p := range payloads {
		log = AppendRecord(log, Record{N: int64(i + 1), T: 1, D: DirClient}, []byte(p), false)
	}
	log = AppendRecord(log, Record{N: 99, T: 1, D: DirServer}, []byte{0, 1, 2, 0xff}, true)

	var got [][]byte
	EachRecord(log, func(r Record) bool { got = append(got, r.Payload()); return true })
	if len(got) != len(payloads)+1 {
		t.Fatalf("decoded %d records, want %d", len(got), len(payloads)+1)
	}
	for i, p := range payloads {
		if !bytes.Equal(got[i], []byte(p)) {
			t.Errorf("payload %d: got %q, want %q", i, got[i], p)
		}
	}
	if !bytes.Equal(got[len(payloads)], []byte{0, 1, 2, 0xff}) {
		t.Errorf("binary payload: got %v", got[len(payloads)])
	}
	if n := bytes.Count(log, []byte("\n")); n != len(payloads)+1 {
		t.Errorf("log has %d lines, want one per record", n)
	}
	if !utf8.Valid(log) {
		t.Error("log is not valid UTF-8: a non-UTF-8 frame went into it raw")
	}
}

func TestEachRecordStopsAtTornLine(t *testing.T) {
	log := logOf(t, [2]string{"c", `{"type":"x"}`}, [2]string{"s", `{"type":"y"}`})
	torn := log[:len(log)-5]
	n := 0
	EachRecord(torn, func(Record) bool { n++; return true })
	if n != 1 {
		t.Fatalf("got %d records from a torn log, want the 1 intact one", n)
	}
}

func item(text string) string {
	return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
}

func out(text string) string {
	return `{"type":"message","role":"assistant","content":[{"type":"output_text","text":"` + text + `"}]}`
}

func created(id string) string {
	return `{"type":"response.created","response":{"id":"` + id + `","status":"in_progress"}}`
}

func done(it string) string { return `{"type":"response.output_item.done","item":` + it + `}` }

func completed(id string) string {
	return `{"type":"response.completed","response":{"id":"` + id + `","status":"completed","usage":{"input_tokens":10,"output_tokens":2}}}`
}

func inputTexts(t *testing.T, turn *Turn) []string {
	t.Helper()
	var texts []string
	for _, it := range turn.FullInput() {
		var m struct {
			Content []struct {
				Text string `json:"text"`
			} `json:"content"`
		}
		_ = json.Unmarshal(it, &m)
		if len(m.Content) > 0 {
			texts = append(texts, m.Content[0].Text)
		}
	}
	return texts
}

func TestReplayRebuildsContinuationFromPrewarm(t *testing.T) {
	log := logOf(t,
		// prewarm: the base the first real turn continues
		[2]string{"c", `{"type":"response.create","generate":false,"input":[` + item("sys") + `]}`},
		[2]string{"s", created("r0")},
		[2]string{"s", completed("r0")},
		// turn 1 continues r0 with one new item
		[2]string{"c", `{"type":"response.create","previous_response_id":"r0","input":[` + item("hi") + `]}`},
		[2]string{"s", created("r1")},
		[2]string{"s", done(out("hello"))},
		[2]string{"s", completed("r1")},
		// turn 2 continues r1
		[2]string{"c", `{"type":"response.create","previous_response_id":"r1","input":[` + item("again") + `]}`},
		[2]string{"s", created("r2")},
		[2]string{"s", done(out("ok"))},
		[2]string{"s", completed("r2")},
	)
	turn := ReplayTurn(log, 8, 0)
	if turn == nil {
		t.Fatal("turn not found")
	}
	got := strings.Join(inputTexts(t, turn), ",")
	if got != "sys,hi,hello,again" {
		t.Fatalf("full input = %s, want sys,hi,hello,again", got)
	}
	if turn.Incomplete || turn.Terminal != "response.completed" || turn.ResponseID != "r2" {
		t.Fatalf("turn = %+v", turn)
	}
	if turn.First != 8 || turn.Last != 11 || len(turn.Events) != 3 {
		t.Fatalf("range %d..%d with %d events", turn.First, turn.Last, len(turn.Events))
	}
	var req struct {
		Input []json.RawMessage `json:"input"`
	}
	if json.Unmarshal(turn.Request, &req) != nil || len(req.Input) != 4 {
		t.Fatalf("Request does not carry the full input: %s", turn.Request)
	}
}

func TestReplayFullRequestBreaksChain(t *testing.T) {
	log := logOf(t,
		[2]string{"c", `{"type":"response.create","input":[` + item("a") + `]}`},
		[2]string{"s", completed("r1")},
		// after compaction Codex resends everything, with no previous id
		[2]string{"c", `{"type":"response.create","input":[` + item("summary") + `]}`},
		[2]string{"s", completed("r2")},
	)
	if got := strings.Join(inputTexts(t, ReplayTurn(log, 3, 0)), ","); got != "summary" {
		t.Fatalf("full input = %s, want summary", got)
	}
}

func TestReplayUnknownPreviousIsIncomplete(t *testing.T) {
	log := logOf(t, [2]string{"c", `{"type":"response.create","previous_response_id":"elsewhere","input":[` + item("x") + `]}`})
	turn := ReplayTurn(log, 1, 0)
	if turn == nil || !turn.Incomplete {
		t.Fatalf("a continuation of an unknown response must be marked incomplete: %+v", turn)
	}
	if turn.Terminal != "" {
		t.Fatalf("an unanswered turn has no terminal event")
	}
}

func TestReplayNewCreateEndsUnfinishedTurn(t *testing.T) {
	log := logOf(t,
		[2]string{"c", `{"type":"response.create","input":[]}`},
		[2]string{"s", created("r1")},
		[2]string{"c", `{"type":"response.create","input":[]}`},
	)
	r := NewReplayer()
	var ended []*Turn
	EachRecord(log, func(rec Record) bool {
		if st := r.Feed(rec); st.Ended != nil {
			ended = append(ended, st.Ended)
		}
		return true
	})
	if len(ended) != 1 || ended[0].First != 1 || ended[0].Last != 2 || ended[0].Terminal != "" {
		t.Fatalf("ended = %+v", ended)
	}
	if r.Current() == nil || r.Current().First != 3 {
		t.Fatal("the second create should be the current turn")
	}
}

// Codex continues an interrupted response by id, and the interrupt's
// acknowledgement can arrive after the terminal event. Both must stay with the
// turn they belong to.
func TestReplayInterruptedResponseIsAContinuationBase(t *testing.T) {
	incomplete := `{"type":"response.incomplete","response":{"id":"r1","status":"incomplete","incomplete_details":{"reason":"interrupted"}}}`
	log := logOf(t,
		[2]string{"c", `{"type":"response.create","input":[` + item("go") + `]}`},
		[2]string{"s", created("r1")},
		[2]string{"s", done(out("partial"))},
		[2]string{"c", `{"type":"response.interrupt","response_id":"r1","mode":"discard_partial_items"}`},
		[2]string{"s", incomplete},
		[2]string{"s", `{"type":"response.interrupt.accepted","response_id":"r1"}`},
		[2]string{"c", `{"type":"response.create","previous_response_id":"r1","input":[` + item("steer") + `]}`},
		[2]string{"s", completed("r2")},
	)
	first := ReplayTurn(log, 1, 0)
	if first.Last != 6 || first.TerminalN != 5 || first.Terminal != "response.incomplete" || len(first.Events) != 4 || len(first.ClientFrames) != 1 {
		t.Fatalf("interrupted turn: last %d terminal %s@%d events %d client %d",
			first.Last, first.Terminal, first.TerminalN, len(first.Events), len(first.ClientFrames))
	}
	second := ReplayTurn(log, 7, 0)
	if got := strings.Join(inputTexts(t, second), ","); got != "go,partial,steer" || second.Incomplete {
		t.Fatalf("continuation of an interrupted response: %s (incomplete=%v)", got, second.Incomplete)
	}
}

func TestReplayHeldEvents(t *testing.T) {
	log := logOf(t,
		[2]string{"c", `{"type":"response.create","input":[]}`},
		[2]string{"s", created("r1")},
		[2]string{"s", completed("r1")},
		[2]string{"s", `{"type":"response.interrupt.accepted"}`},
	)
	r := NewReplayer()
	var held []int64
	EachRecord(log, func(rec Record) bool {
		if r.Feed(rec).Held {
			held = append(held, rec.N)
		}
		return true
	})
	if len(held) != 2 || held[0] != 3 || held[1] != 4 {
		t.Fatalf("held = %v, want the terminal event and the acknowledgement", held)
	}
}

func TestReplayTurnRecordNamesTheTurn(t *testing.T) {
	var log []byte
	log = AppendRecord(log, Record{N: 1, T: 1, D: DirClient, Op: OpTurn, ID: "ex-1"}, nil, false)
	log = AppendRecord(log, Record{N: 2, T: 1, D: DirClient}, []byte(`{"type":"response.create","input":[]}`), false)
	log = AppendRecord(log, Record{N: 3, T: 1, D: DirServer, Op: OpEnd, Reason: "clean"}, nil, false)
	turn := ReplayTurn(log, 2, 0)
	if turn == nil || turn.ID != "ex-1" || turn.Last != 3 {
		t.Fatalf("turn = %+v", turn)
	}
}

// The recorder writes a turn record and its create as one unit, so a create
// that does not directly follow one is not named by it.
func TestReplayTurnRecordMustBeAdjacent(t *testing.T) {
	var log []byte
	log = AppendRecord(log, Record{N: 1, T: 1, D: DirClient, Op: OpTurn, ID: "ex-1"}, nil, false)
	log = AppendRecord(log, Record{N: 2, T: 1, D: DirServer}, []byte(`{"type":"stray"}`), false)
	log = AppendRecord(log, Record{N: 3, T: 1, D: DirClient}, []byte(`{"type":"response.create","input":[]}`), false)
	if turn := ReplayTurn(log, 3, 0); turn == nil || turn.ID != "" {
		t.Fatalf("turn = %+v, want an unnamed one", turn)
	}
}

func TestOpenAICacheWriteLifetimes(t *testing.T) {
	ev := `{"type":"response.completed","response":{"id":"r","model":"gpt-6.1-terra","status":"completed",` +
		`"usage":{"input_tokens":12000,"input_tokens_details":{"cached_tokens":4000,"cache_write_tokens":7000,` +
		`"cache_write_tokens_details":{"ttl_30m_tokens":3000,"ttl_12h_tokens":4000}},"output_tokens":0}}}`
	meta, ok := OpenAI{}.ParseResponse(nil, []byte(ev+"\n"), "")
	u := meta.Usage
	if !ok || u.InputTokens != 1000 || u.CacheCreateTokens != 7000 || u.CacheCreate5m != 3000 || u.CacheCreate1h != 4000 {
		t.Fatalf("usage = %+v", u)
	}
}

func TestOpenAIUsageNormalised(t *testing.T) {
	ev := `{"type":"response.completed","response":{"id":"r","model":"gpt-6.1-sol","status":"completed","service_tier":"default",` +
		`"usage":{"input_tokens":100,"input_tokens_details":{"cached_tokens":40,"cache_write_tokens":50},` +
		`"output_tokens":10,"output_tokens_details":{"reasoning_tokens":5}},"output":[{"type":"function_call","name":"x"}]}}`
	meta, ok := OpenAI{}.ParseResponse(nil, []byte(ev+"\n"), "")
	if !ok {
		t.Fatal("not parsed")
	}
	u := meta.Usage
	if u.InputTokens != 10 || u.CacheReadTokens != 40 || u.CacheCreateTokens != 50 || u.OutputTokens != 10 || u.ThinkingTokens != 5 {
		t.Fatalf("usage = %+v", u)
	}
	if meta.Model != "gpt-6.1-sol" || meta.ResponseID != "r" || meta.StopReason != "tool_use" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestOpenAIRequestIdentity(t *testing.T) {
	tm, _ := json.Marshal(map[string]string{
		"thread_id": "t-child", "parent_thread_id": "t-root", "agent_name": "/root/task",
		"request_kind": "turn", "thread_source": "subagent",
	})
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol",
		"input": []json.RawMessage{
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context>\n  <cwd>C:\\old</cwd>\n</environment_context>"}]}`),
			json.RawMessage(item("hi")),
			json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"<environment_context><cwd>C:\\new</cwd></environment_context>"}]}`),
		},
		"tools": []any{map[string]any{"type": "function"}},
		"client_metadata": map[string]string{
			"session_id": "s-root", "x-openai-subagent": "collab_spawn", "x-codex-turn-metadata": string(tm),
		},
	})
	m := OpenAI{}.ParseRequest(nil, body)
	if m.SessionID != "s-root" || m.Thread != "t-child" || m.ParentThread != "t-root" || m.Agent != "/root/task" ||
		!m.IsSubagent || m.Kind != "turn" || m.MsgCount != 3 || m.ToolCount != 1 || m.Cwd != `C:\new` {
		t.Fatalf("meta = %+v", m)
	}
}

// A reviewer has no agent path of its own and reports the root's; labelling it
// "/root" would make it indistinguishable from the agent it is reviewing.
func TestOpenAIReviewerIsNotLabelledRoot(t *testing.T) {
	tm, _ := json.Marshal(map[string]string{"thread_id": "t-review", "agent_name": "/root"})
	body, _ := json.Marshal(map[string]any{
		"model": "gpt-6.1-sol",
		"client_metadata": map[string]string{
			"session_id": "s-root", "x-openai-subagent": "review", "x-codex-turn-metadata": string(tm),
		},
	})
	if m := (OpenAI{}).ParseRequest(nil, body); m.Agent != "review" {
		t.Errorf("agent = %q, want review", m.Agent)
	}
	tm, _ = json.Marshal(map[string]string{"thread_id": "t-root", "agent_name": "/root"})
	body, _ = json.Marshal(map[string]any{
		"model":           "gpt-6.1-sol",
		"client_metadata": map[string]string{"session_id": "s-root", "x-codex-turn-metadata": string(tm)},
	})
	if m := (OpenAI{}).ParseRequest(nil, body); m.Agent != "/root" {
		t.Errorf("the root agent = %q, want /root", m.Agent)
	}
}

func TestOpenAINoise(t *testing.T) {
	p := OpenAI{}
	if !p.IsNoise("GET", "/backend-api/codex/models", nil, nil) {
		t.Error("models listing is noise")
	}
	if !p.IsNoise("WS", "", nil, []byte(`{"type":"response.create","generate":false}`)) {
		t.Error("prewarm is noise")
	}
	if p.IsNoise("WS", "", nil, []byte(`{"type":"response.create","input":[]}`)) {
		t.Error("an ordinary turn is not noise")
	}
	title := `{"client_metadata":{"x-codex-turn-metadata":"{\"thread_source\":\"thread_title\"}"}}`
	if !p.IsNoise("POST", "/backend-api/codex/responses", nil, []byte(title)) {
		t.Error("a title thread is noise")
	}
}

// A settled turn's range bounds its replay: records past it are not shown as
// part of the turn.
func TestReplayTurnStopsAtTheRange(t *testing.T) {
	var log []byte
	log = AppendRecord(log, Record{N: 1, T: 1, D: DirClient}, []byte(`{"type":"response.create","input":[]}`), false)
	log = AppendRecord(log, Record{N: 2, T: 1, D: DirServer}, []byte(`{"type":"response.created","response":{"id":"r"}}`), false)
	log = AppendRecord(log, Record{N: 3, T: 1, D: DirServer}, []byte(`{"type":"response.completed","response":{"id":"r"}}`), false)
	if turn := ReplayTurn(log, 1, 2); turn == nil || turn.Last != 2 || len(turn.Events) != 1 {
		t.Fatalf("turn = %+v", turn)
	}
}
