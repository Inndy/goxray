// Package view derives dashboard-facing structure from a recorded request body.
// It is read-only and never touches the store, so nothing here can affect the
// fail-closed recording path.
//
// Everything it produces is derived, not authoritative: token counts are
// estimates and the system-prompt section split is a heuristic over prose that
// Anthropic can reshape at any time. The verbatim blob always remains the source
// of truth, reachable beside every derived view.
package view

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"unicode/utf8"
)

// Owner attributes a system-prompt section to whoever put it there. "anthropic"
// is the prompt Claude Code ships; "user" is what your own machine and config
// contributed. It answers "which part of this context is my fault".
type Owner string

const (
	OwnerAnthropic Owner = "anthropic"
	OwnerUser      Owner = "user"
)

// Block is one addressable slice of the system prompt.
type Block struct {
	Key    string `json:"key"`
	Label  string `json:"label"`
	Owner  Owner  `json:"owner"`
	Chars  int    `json:"chars"`
	Tokens int    `json:"tokens"`
	Text   string `json:"text"`
}

// userOwnedKeys lists sections that come from the user's machine or config
// rather than Anthropic's shipped prompt. Anything unrecognized defaults to
// anthropic, so a newly added Anthropic section is never mislabeled as yours.
var userOwnedKeys = map[string]bool{
	// Current Claude Code (H1 sections observed in 2.1.x).
	"memory":                    true,
	"environment":               true,
	"scratchpad-directory":      true,
	"session-specific-guidance": true,
	"auto-memory":               true,
	// Older layouts that inlined config into the system prompt.
	"custom-skills": true,
	"custom-agents": true,
	"plugin-skills": true,
	"mcp-servers":   true,
	"settings-json": true,
	"env-and-git":   true,
}

// h1Re finds markdown H1 headings, which is how current Claude Code delimits
// system-prompt sections.
var h1Re = regexp.MustCompile(`(?m)^# (.+)$`)

// legacyMarkers catch sections from older Claude Code builds that were not H1
// headings. Kept so previously recorded captures still split correctly. Each
// pattern is a distinctive bold/heading lead-in, never body prose — matching on
// body text would cut a section in half.
var legacyMarkers = []struct {
	Key, Label string
	Re         *regexp.Regexp
}{
	{"custom-agents", "Custom agents", regexp.MustCompile(`\*\*Available custom agents`)},
	{"mcp-servers", "MCP servers", regexp.MustCompile(`\*\*Configured MCP servers`)},
	{"plugin-skills", "Plugin skills", regexp.MustCompile(`\*\*Available plugin skills`)},
	{"settings-json", "settings.json", regexp.MustCompile(`\*\*User's settings\.json`)},
	{"env-and-git", "Environment & git", regexp.MustCompile(`<env>`)},
}

var ccVersionRe = regexp.MustCompile(`cc_version=(\S+?)[; ]`)

// systemBlock is the wire shape of one system entry.
type systemBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// SystemBlocks splits the request's system prompt into owner-tagged sections.
//
// A plain-string system prompt yields a single block. An array yields the
// billing header and core identity as their own blocks, and any large block is
// further split into its named sections.
func SystemBlocks(system json.RawMessage) []Block {
	if len(system) == 0 {
		return nil
	}
	var s string
	if json.Unmarshal(system, &s) == nil {
		return []Block{newBlock("system", "System prompt", OwnerAnthropic, s)}
	}
	var arr []systemBlock
	if json.Unmarshal(system, &arr) != nil {
		return nil
	}

	var out []Block
	haveIdentity := false
	for _, b := range arr {
		switch {
		case strings.Contains(b.Text, "billing-header") || strings.Contains(b.Text, "cc_version="):
			out = append(out, newBlock("billing-header", "Billing header", OwnerAnthropic, b.Text))
		case !haveIdentity && utf8.RuneCountInString(b.Text) < 500:
			haveIdentity = true
			out = append(out, newBlock("core-identity", "Core identity", OwnerAnthropic, b.Text))
		default:
			out = append(out, splitSections(b.Text)...)
		}
	}
	return out
}

// CCVersion reads the Claude Code version out of the billing header, or "".
func CCVersion(system json.RawMessage) string {
	var arr []systemBlock
	if json.Unmarshal(system, &arr) != nil {
		return ""
	}
	for _, b := range arr {
		if m := ccVersionRe.FindStringSubmatch(b.Text); m != nil {
			return m[1]
		}
	}
	return ""
}

type cut struct {
	at    int
	key   string
	label string
}

// splitSections cuts one large system block into named sections at heading and
// legacy-marker boundaries. Text before the first cut is the shipped base
// instructions.
func splitSections(text string) []Block {
	var cuts []cut
	for _, m := range h1Re.FindAllStringSubmatchIndex(text, -1) {
		label := strings.TrimSpace(text[m[2]:m[3]])
		cuts = append(cuts, cut{at: m[0], key: slug(label), label: label})
	}
	for _, lm := range legacyMarkers {
		if loc := lm.Re.FindStringIndex(text); loc != nil {
			cuts = append(cuts, cut{at: loc[0], key: lm.Key, label: lm.Label})
		}
	}
	sort.SliceStable(cuts, func(i, j int) bool { return cuts[i].at < cuts[j].at })

	// Drop cuts that coincide with an earlier one (a legacy marker landing on a
	// heading we already cut at) so a section is never split into an empty pair.
	deduped := cuts[:0]
	for i, c := range cuts {
		if i > 0 && c.at-deduped[len(deduped)-1].at < 4 {
			continue
		}
		deduped = append(deduped, c)
	}
	cuts = deduped

	var out []Block
	head := text
	if len(cuts) > 0 {
		head = text[:cuts[0].at]
	}
	if strings.TrimSpace(head) != "" {
		out = append(out, newBlock("core-instructions", "Core instructions", OwnerAnthropic, head))
	}
	for i, c := range cuts {
		end := len(text)
		if i+1 < len(cuts) {
			end = cuts[i+1].at
		}
		owner := OwnerAnthropic
		if userOwnedKeys[c.key] {
			owner = OwnerUser
		}
		out = append(out, newBlock(c.key, c.label, owner, text[c.at:end]))
	}
	return out
}

func newBlock(key, label string, owner Owner, text string) Block {
	return Block{
		Key:    key,
		Label:  label,
		Owner:  owner,
		Chars:  utf8.RuneCountInString(text),
		Tokens: EstimateTokens(text),
		Text:   text,
	}
}

var nonSlug = regexp.MustCompile(`[^a-z0-9]+`)

func slug(s string) string {
	return strings.Trim(nonSlug.ReplaceAllString(strings.ToLower(s), "-"), "-")
}

// EstimateTokens approximates tokens as characters/4.
//
// This is deliberately cheap — no tokenizer, no cgo, no model-specific vocab.
// It runs roughly 20–40% under the API's real input_tokens, so the UI labels it
// an estimate and prefers authoritative usage from the response wherever both
// are available.
func EstimateTokens(s string) int {
	n := utf8.RuneCountInString(s)
	if n == 0 {
		return 0
	}
	return (n + 3) / 4
}

// ── token breakdown ─────────────────────────────────────────────────

type BlockTokens struct {
	Type   string `json:"type"`
	Name   string `json:"name,omitempty"`
	Tokens int    `json:"tokens"`
}

type MsgTokens struct {
	Role   string        `json:"role"`
	Tokens int           `json:"tokens"`
	Blocks []BlockTokens `json:"blocks,omitempty"`
}

type TokenBreakdown struct {
	System     int         `json:"system"`
	Tools      int         `json:"tools"`
	Messages   int         `json:"messages"`
	Total      int         `json:"total"`
	PerMessage []MsgTokens `json:"per_message"`
}

// contentBlock is the wire shape of a message content block. Text/Thinking are
// pulled out for labeling; the whole block is still measured as JSON.
type contentBlock struct {
	Type string `json:"type"`
	Name string `json:"name,omitempty"`
	Raw  json.RawMessage
}

func (c *contentBlock) UnmarshalJSON(b []byte) error {
	var probe struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	if err := json.Unmarshal(b, &probe); err != nil {
		return err
	}
	c.Type, c.Name, c.Raw = probe.Type, probe.Name, b
	return nil
}

type message struct {
	Role    string          `json:"role"`
	Content json.RawMessage `json:"content"`
}

type requestBody struct {
	Model    string            `json:"model"`
	System   json.RawMessage   `json:"system"`
	Tools    []json.RawMessage `json:"tools"`
	Messages []message         `json:"messages"`
}

// Tokens estimates the request's context split by section. Measurement is on
// each part's JSON encoding, which is what actually goes over the wire.
func Tokens(reqBody []byte) TokenBreakdown {
	var req requestBody
	if json.Unmarshal(reqBody, &req) != nil {
		return TokenBreakdown{}
	}
	var tb TokenBreakdown
	tb.System = EstimateTokens(string(req.System))
	for _, t := range req.Tools {
		tb.Tools += EstimateTokens(string(t))
	}
	for _, m := range req.Messages {
		mt := MsgTokens{Role: m.Role}
		var str string
		if json.Unmarshal(m.Content, &str) == nil {
			mt.Tokens = EstimateTokens(str)
		} else {
			var blocks []contentBlock
			if json.Unmarshal(m.Content, &blocks) == nil {
				for _, b := range blocks {
					n := EstimateTokens(string(b.Raw))
					mt.Blocks = append(mt.Blocks, BlockTokens{Type: b.Type, Name: b.Name, Tokens: n})
					mt.Tokens += n
				}
			}
		}
		tb.Messages += mt.Tokens
		tb.PerMessage = append(tb.PerMessage, mt)
	}
	tb.Total = tb.System + tb.Tools + tb.Messages
	return tb
}

// ── tools ───────────────────────────────────────────────────────────

type ToolDef struct {
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
	Tokens      int    `json:"tokens"`
	Calls       int    `json:"calls"`
	Plugin      string `json:"plugin,omitempty"`
}

type ToolSummary struct {
	Core        []ToolDef      `json:"core"`
	MCP         []ToolDef      `json:"mcp"`
	Calls       map[string]int `json:"calls"`
	TotalTokens int            `json:"total_tokens"`
}

// Tools lists the request's tool definitions, split core vs MCP and annotated
// with how many times each was actually invoked in this request's history.
// Sorted by call count descending so the tools in play surface above the long
// tail of definitions that only cost context.
func Tools(reqBody []byte) ToolSummary {
	var req requestBody
	if json.Unmarshal(reqBody, &req) != nil {
		return ToolSummary{Calls: map[string]int{}}
	}
	calls := toolCalls(req.Messages)
	out := ToolSummary{Calls: calls}
	for _, raw := range req.Tools {
		var td struct {
			Name        string `json:"name"`
			Description string `json:"description"`
		}
		if json.Unmarshal(raw, &td) != nil {
			continue
		}
		d := ToolDef{
			Name:        td.Name,
			Description: firstLine(td.Description),
			Tokens:      EstimateTokens(string(raw)),
			Calls:       calls[td.Name],
		}
		out.TotalTokens += d.Tokens
		if after, ok := strings.CutPrefix(td.Name, "mcp__"); ok {
			d.Plugin, _, _ = strings.Cut(after, "__")
			out.MCP = append(out.MCP, d)
		} else {
			out.Core = append(out.Core, d)
		}
	}
	sortTools(out.Core)
	sortTools(out.MCP)
	return out
}

func sortTools(ts []ToolDef) {
	sort.SliceStable(ts, func(i, j int) bool {
		if ts[i].Calls != ts[j].Calls {
			return ts[i].Calls > ts[j].Calls
		}
		return ts[i].Name < ts[j].Name
	})
}

func toolCalls(msgs []message) map[string]int {
	calls := map[string]int{}
	for _, m := range msgs {
		var blocks []contentBlock
		if json.Unmarshal(m.Content, &blocks) != nil {
			continue
		}
		for _, b := range blocks {
			if b.Type == "tool_use" && b.Name != "" {
				calls[b.Name]++
			}
		}
	}
	return calls
}

func firstLine(s string) string {
	line, _, _ := strings.Cut(strings.TrimSpace(s), "\n")
	if utf8.RuneCountInString(line) > 160 {
		r := []rune(line)
		return string(r[:160]) + "…"
	}
	return line
}

// ── misc ────────────────────────────────────────────────────────────

// reservedKeys are the request fields the dashboard renders through dedicated
// views; everything else is surfaced verbatim as request options.
var reservedKeys = map[string]bool{"system": true, "tools": true, "messages": true}

// Options returns the request's non-content top-level fields (max_tokens,
// stream, thinking, output_config, context_management, metadata, …) verbatim.
// Passing them through rather than modeling them means a new API field shows up
// in the UI without a code change — context_management, for instance, is what
// explains a turn whose thinking blocks were stripped.
func Options(reqBody []byte) map[string]json.RawMessage {
	var all map[string]json.RawMessage
	if json.Unmarshal(reqBody, &all) != nil {
		return nil
	}
	for k := range all {
		if reservedKeys[k] {
			delete(all, k)
		}
	}
	return all
}
