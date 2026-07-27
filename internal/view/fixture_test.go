package view

import (
	"encoding/json"
	"os"
	"testing"
)

// TestAgainstRealCapture runs the derivations over a real recorded request body,
// so a Claude Code prompt reshuffle can be caught without guessing.
//
// Opt-in, because a capture contains a real conversation and must never live in
// the repo:
//
//	./goxray raw <id> req > /tmp/req.json
//	GOXRAY_VIEW_FIXTURE=/tmp/req.json go test ./internal/view/ -run RealCapture -v
func TestAgainstRealCapture(t *testing.T) {
	path := os.Getenv("GOXRAY_VIEW_FIXTURE")
	if path == "" {
		t.Skip("set GOXRAY_VIEW_FIXTURE to a recorded request body to run")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	var req requestBody
	if err := json.Unmarshal(body, &req); err != nil {
		t.Fatalf("fixture is not a JSON request body: %v", err)
	}

	blocks := SystemBlocks(req.System)
	if len(blocks) == 0 {
		t.Error("no system blocks derived — the prompt shape likely changed")
	}
	t.Logf("cc_version=%q  model=%q  ctx_window=%d", CCVersion(req.System), req.Model, CtxWindow(req.Model, req.System))
	for _, b := range blocks {
		t.Logf("  %-28s %-9s %6d chars %6d tok", b.Key, b.Owner, b.Chars, b.Tokens)
	}

	tb := Tokens(body)
	t.Logf("tokens: system=%d tools=%d messages=%d total=%d over %d messages",
		tb.System, tb.Tools, tb.Messages, tb.Total, len(tb.PerMessage))

	roles := map[string]int{}
	for _, m := range tb.PerMessage {
		roles[m.Role]++
	}
	t.Logf("message roles: %v", roles)

	ts := Tools(body)
	t.Logf("tools: core=%d mcp=%d tokens=%d distinct_called=%d",
		len(ts.Core), len(ts.MCP), ts.TotalTokens, len(ts.Calls))

	opts := Options(body)
	optKeys := make([]string, 0, len(opts))
	for k := range opts {
		optKeys = append(optKeys, k)
	}
	t.Logf("option keys: %v", optKeys)

	// Every message must be accounted for; a silently skipped role would
	// understate context in the UI.
	if len(tb.PerMessage) != len(req.Messages) {
		t.Errorf("accounted %d messages, request has %d", len(tb.PerMessage), len(req.Messages))
	}
}
