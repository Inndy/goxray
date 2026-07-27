package view

import (
	"encoding/json"
	"testing"
)

// b2 mirrors the real shape of Claude Code's large system block: base
// instructions, then H1-delimited sections, some of which come from the user's
// own machine and config.
const b2 = `
You are an interactive agent that helps users with software engineering tasks.

# Harness
Text you output outside of tool use is displayed to the user.

# Memory
You have a persistent file-based memory at /home/u/mem/.

# Environment
Primary working directory: /home/u/repo

# Delivering work
Do ordinary work as asked.
`

func systemArray(t *testing.T, texts ...string) json.RawMessage {
	t.Helper()
	var blocks []systemBlock
	for _, s := range texts {
		blocks = append(blocks, systemBlock{Type: "text", Text: s})
	}
	b, err := json.Marshal(blocks)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func keys(bs []Block) []string {
	out := make([]string, len(bs))
	for i, b := range bs {
		out[i] = b.Key
	}
	return out
}

func TestSystemBlocksSplitsAndAttributes(t *testing.T) {
	sys := systemArray(t,
		"x-anthropic-billing-header: cc_version=2.1.220.9cd; cc_entrypoint=cli;",
		"You are Claude Code, Anthropic's official CLI for Claude.",
		b2,
	)
	got := SystemBlocks(sys)

	want := []string{
		"billing-header", "core-identity", "core-instructions",
		"harness", "memory", "environment", "delivering-work",
	}
	gotKeys := keys(got)
	if len(gotKeys) != len(want) {
		t.Fatalf("keys = %v, want %v", gotKeys, want)
	}
	for i := range want {
		if gotKeys[i] != want[i] {
			t.Errorf("key[%d] = %q, want %q", i, gotKeys[i], want[i])
		}
	}

	owners := map[string]Owner{}
	for _, b := range got {
		owners[b.Key] = b.Owner
	}
	// The user's machine contributed memory and environment; the rest ships with
	// Claude Code. Unknown sections must default to anthropic so a new Anthropic
	// section is never blamed on the user.
	for _, k := range []string{"memory", "environment"} {
		if owners[k] != OwnerUser {
			t.Errorf("owner[%s] = %q, want user", k, owners[k])
		}
	}
	for _, k := range []string{"billing-header", "core-identity", "core-instructions", "harness", "delivering-work"} {
		if owners[k] != OwnerAnthropic {
			t.Errorf("owner[%s] = %q, want anthropic", k, owners[k])
		}
	}

	for _, b := range got {
		if b.Chars == 0 || b.Text == "" {
			t.Errorf("block %q is empty: %+v", b.Key, b)
		}
		if b.Tokens == 0 {
			t.Errorf("block %q has zero tokens", b.Key)
		}
	}
}

// Sections must partition the block exactly — no text silently dropped, which
// would understate context in the UI.
func TestSystemBlocksPartitionIsLossless(t *testing.T) {
	got := splitSections(b2)
	var joined string
	for _, b := range got {
		joined += b.Text
	}
	if joined != b2 {
		t.Errorf("sections do not reconstruct the input\n got %q\nwant %q", joined, b2)
	}
}

func TestSystemBlocksPlainString(t *testing.T) {
	got := SystemBlocks(json.RawMessage(`"just a string prompt"`))
	if len(got) != 1 || got[0].Key != "system" {
		t.Fatalf("got %+v, want a single system block", got)
	}
	if got[0].Text != "just a string prompt" {
		t.Errorf("text = %q", got[0].Text)
	}
}

func TestSystemBlocksEmpty(t *testing.T) {
	if got := SystemBlocks(nil); got != nil {
		t.Errorf("got %+v, want nil", got)
	}
	if got := SystemBlocks(json.RawMessage(`17`)); got != nil {
		t.Errorf("got %+v, want nil for a non-string non-array system", got)
	}
}

// Older Claude Code inlined config into the system prompt without H1 headings.
func TestSystemBlocksLegacyMarkers(t *testing.T) {
	legacy := "Base instructions here.\n\n**Available custom agents** for the Agent tool:\n- foo\n"
	got := splitSections(legacy)
	if len(got) != 2 {
		t.Fatalf("got %d blocks (%v), want 2", len(got), keys(got))
	}
	if got[1].Key != "custom-agents" || got[1].Owner != OwnerUser {
		t.Errorf("got %+v, want user-owned custom-agents", got[1])
	}
}

func TestCCVersion(t *testing.T) {
	sys := systemArray(t, "x-anthropic-billing-header: cc_version=2.1.220.9cd; cc_entrypoint=cli;", "id")
	if got := CCVersion(sys); got != "2.1.220.9cd" {
		t.Errorf("CCVersion = %q, want 2.1.220.9cd", got)
	}
	if got := CCVersion(json.RawMessage(`"plain"`)); got != "" {
		t.Errorf("CCVersion = %q, want empty", got)
	}
}

func TestEstimateTokens(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want int
	}{
		{"", 0},
		{"a", 1},
		{"abcd", 1},
		{"abcde", 2},
		{"日本語テキスト", 2}, // counted by rune, not byte
	} {
		if got := EstimateTokens(tc.in); got != tc.want {
			t.Errorf("EstimateTokens(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

const reqFixture = `{
  "model": "claude-opus-5[1m]",
  "max_tokens": 64000,
  "stream": true,
  "thinking": {"type": "adaptive"},
  "context_management": {"edits": [{"type": "clear_thinking_20251015"}]},
  "system": ["ignored-for-tokens"],
  "tools": [
    {"name": "Bash", "description": "Run a command\nmore detail"},
    {"name": "Read", "description": "Read a file"},
    {"name": "mcp__linear__list_issues", "description": "List issues"}
  ],
  "messages": [
    {"role": "user", "content": "hello there"},
    {"role": "system", "content": "an injected system message"},
    {"role": "assistant", "content": [
      {"type": "thinking", "thinking": "pondering"},
      {"type": "tool_use", "id": "t1", "name": "Bash", "input": {"command": "ls"}},
      {"type": "tool_use", "id": "t2", "name": "Bash", "input": {"command": "pwd"}}
    ]},
    {"role": "user", "content": [
      {"type": "tool_result", "tool_use_id": "t1", "content": "out"}
    ]}
  ]
}`

func TestTokens(t *testing.T) {
	tb := Tokens([]byte(reqFixture))
	if tb.System == 0 || tb.Tools == 0 || tb.Messages == 0 {
		t.Fatalf("breakdown has an empty section: %+v", tb)
	}
	if tb.Total != tb.System+tb.Tools+tb.Messages {
		t.Errorf("Total = %d, want %d", tb.Total, tb.System+tb.Tools+tb.Messages)
	}
	if len(tb.PerMessage) != 4 {
		t.Fatalf("PerMessage len = %d, want 4", len(tb.PerMessage))
	}
	// A role:"system" message inside messages[] is a real context consumer in
	// current Claude Code and must be accounted for, not skipped.
	if tb.PerMessage[1].Role != "system" || tb.PerMessage[1].Tokens == 0 {
		t.Errorf("system message not accounted: %+v", tb.PerMessage[1])
	}
	if got := len(tb.PerMessage[2].Blocks); got != 3 {
		t.Errorf("assistant blocks = %d, want 3", got)
	}
	if tb.PerMessage[2].Blocks[1].Name != "Bash" {
		t.Errorf("block name = %q, want Bash", tb.PerMessage[2].Blocks[1].Name)
	}
}

func TestTools(t *testing.T) {
	ts := Tools([]byte(reqFixture))
	if len(ts.Core) != 2 || len(ts.MCP) != 1 {
		t.Fatalf("core=%d mcp=%d, want 2/1", len(ts.Core), len(ts.MCP))
	}
	// Sorted by call count descending: Bash was invoked twice, Read never.
	if ts.Core[0].Name != "Bash" || ts.Core[0].Calls != 2 {
		t.Errorf("core[0] = %+v, want Bash with 2 calls", ts.Core[0])
	}
	if ts.Core[1].Name != "Read" || ts.Core[1].Calls != 0 {
		t.Errorf("core[1] = %+v, want Read with 0 calls", ts.Core[1])
	}
	if ts.MCP[0].Plugin != "linear" {
		t.Errorf("plugin = %q, want linear", ts.MCP[0].Plugin)
	}
	if ts.Core[0].Description != "Run a command" {
		t.Errorf("description = %q, want first line only", ts.Core[0].Description)
	}
	if ts.TotalTokens == 0 {
		t.Error("TotalTokens = 0")
	}
}

func TestCtxWindow(t *testing.T) {
	if got := CtxWindow("claude-opus-5[1m]", nil); got != 1_000_000 {
		t.Errorf("marked model id: got %d, want 1000000", got)
	}
	if got := CtxWindow("claude-opus-5", nil); got != 200_000 {
		t.Errorf("bare model id: got %d, want 200000", got)
	}
	// Claude Code frequently sends a bare model id and names the exact one only
	// inside the system prompt; missing that reports 200K for a 1M session and
	// makes the context bar read past 100%.
	sys := json.RawMessage(`[{"type":"text","text":"The exact model ID is claude-opus-5[1m]."}]`)
	if got := CtxWindow("claude-opus-5", sys); got != 1_000_000 {
		t.Errorf("marker in system prompt: got %d, want 1000000", got)
	}
}

func TestOptionsExcludesContent(t *testing.T) {
	opts := Options([]byte(reqFixture))
	for _, k := range []string{"system", "tools", "messages"} {
		if _, ok := opts[k]; ok {
			t.Errorf("options should not contain %q", k)
		}
	}
	for _, k := range []string{"model", "max_tokens", "stream", "thinking", "context_management"} {
		if _, ok := opts[k]; !ok {
			t.Errorf("options missing %q", k)
		}
	}
}

func TestParsersTolerateGarbage(t *testing.T) {
	bad := []byte(`{not json`)
	if got := Tokens(bad); got.Total != 0 {
		t.Errorf("Tokens on garbage = %+v", got)
	}
	if got := Tools(bad); len(got.Core) != 0 || len(got.MCP) != 0 {
		t.Errorf("Tools on garbage = %+v", got)
	}
	if got := Options(bad); got != nil {
		t.Errorf("Options on garbage = %+v", got)
	}
}
