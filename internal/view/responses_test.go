package view

import (
	"encoding/json"
	"strings"
	"testing"
)

// A call into a namespace names the namespace and the tool separately; it must
// come out under the same flat name as the definition it calls, or the timeline
// shows a tool the catalog does not have.
func TestFromResponsesNamesNamespacedCallsLikeTheirTools(t *testing.T) {
	req := `{"model":"gpt-6.1-sol","tools":[{"type":"namespace","name":"mcp__srv","tools":[` +
		`{"type":"function","name":"lookup","parameters":{}}]}],"input":[` +
		`{"type":"message","role":"user","content":"hi"},` +
		`{"type":"function_call","call_id":"c1","namespace":"mcp__srv","name":"lookup","arguments":"{}"}]}`
	var out struct {
		Tools    []struct{ Name string } `json:"tools"`
		Messages []struct {
			Content []struct {
				Type string `json:"type"`
				Name string `json:"name"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(FromResponses([]byte(req)), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Tools) != 1 || out.Tools[0].Name != "mcp__srv__lookup" {
		t.Fatalf("tools = %+v", out.Tools)
	}
	var call string
	for _, m := range out.Messages {
		for _, c := range m.Content {
			if c.Type == "tool_use" {
				call = c.Name
			}
		}
	}
	if call != "mcp__srv__lookup" {
		t.Fatalf("call named %q, want the tool's flat name", call)
	}
}

const testToken = "gAAAAABq4AS0ac9rPOnIf62D_Ne0AsZR-yoNcN8Jbt2gJXKKSuVyO6wyOZw7w6lF07Cy2UUKCVjoPuevQKg6GaalUyJxCPs7DM_hjchP0_XGNvTeqYAO5cea3rHeiQCVlgKyx7vZyZYnL3t-RMTeP2mRyVmrgPueqpStIc2vm3Cgzmj-dKszQHZVvP9qoXZno4MXj68UCoLimSi0NH8m6fPbaiT6kq0T-f1WnRoayYFlqL6O5Eu7Eo7pmgvYsejtp7mgUzD9lh7FgI_S_6hnQZIgTRwTf9jS8A=="

// collaborationTools is the shape Codex 0.164 sends its agent tools in: an
// additional_tools input item holding a "collaboration" namespace whose
// spawn_agent marks message encrypted.
const collaborationTools = `{"type":"additional_tools","tools":[{"type":"namespace","name":"collaboration","tools":[` +
	`{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{` +
	`"message":{"type":"string","encrypted":true},"task_name":{"type":"string"}}}}]}]}`

func call(id, namespace, name string, args map[string]string) string {
	a, _ := json.Marshal(args)
	c := map[string]string{"type": "function_call", "call_id": id, "name": name, "arguments": string(a)}
	if namespace != "" {
		c["namespace"] = namespace
	}
	b, _ := json.Marshal(c)
	return string(b)
}

// The task a sub-agent is given arrives encrypted, both as the parent's call
// argument and in the message the child receives. Neither may be shown as the
// raw token, and what is in the clear around it — the task name, the message
// header — must survive.
func TestFromResponsesShowsEncryptedAgentTasksAsEncrypted(t *testing.T) {
	req := `{"model":"gpt-6-sol","input":[` + collaborationTools + `,` +
		`{"type":"message","role":"user","content":"hi"},` +
		call("c1", "collaboration", "spawn_agent", map[string]string{"message": testToken, "task_name": "count_go"}) + `,` +
		`{"type":"agent_message","author":"/root","recipient":"/root/count_go","content":[` +
		`{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},` +
		`{"type":"encrypted_content","encrypted_content":"` + testToken + `"}]}]}`
	out := string(FromResponses([]byte(req)))
	if strings.Contains(out, testToken) {
		t.Fatalf("the raw token reached the view: %s", out)
	}
	for _, want := range []string{`"task_name":"count_go"`, "[agent message] /root → /root/count_go", "NEW_TASK", Encrypted} {
		if !strings.Contains(out, want) {
			t.Errorf("view lacks %q: %s", want, out)
		}
	}
}

// A reply is redacted by the schema of the request it answers.
func TestAssembleResponsesRedactsBySchema(t *testing.T) {
	req := []byte(`{"input":[` + collaborationTools + `]}`)
	item := json.RawMessage(call("c1", "collaboration", "spawn_agent", map[string]string{"message": testToken}))
	m := AssembleResponses([]json.RawMessage{item}, FindEncryptedParams(req), "gpt-6-sol", "r1", "completed", false)
	b, _ := json.Marshal(m)
	if strings.Contains(string(b), testToken) || !strings.Contains(string(b), Encrypted) {
		t.Fatalf("reply = %s", b)
	}
}

// Only a parameter the schema marks encrypted is called encrypted. A tool of the
// same name elsewhere — an MCP server's, a top-level client-defined one — or a
// tool taking a token as an ordinary argument must be shown as sent.
func TestFromResponsesLeavesOtherToolsTokensAlone(t *testing.T) {
	plain := `{"type":"function","name":"spawn_agent","parameters":{"type":"object","properties":{"message":{"type":"string"}}}}`
	req := `{"model":"gpt-6-sol","tools":[` + plain + `,{"type":"namespace","name":"mcp__srv","tools":[` + plain + `]}],"input":[` +
		collaborationTools + `,{"type":"message","role":"user","content":"hi"},` +
		call("c1", "", "decrypt", map[string]string{"token": testToken}) + `,` +
		call("c2", "mcp__srv", "spawn_agent", map[string]string{"message": testToken}) + `,` +
		call("c3", "", "spawn_agent", map[string]string{"message": testToken}) + `]}`
	out := string(FromResponses([]byte(req)))
	if strings.Count(out, testToken) != 3 || strings.Contains(out, Encrypted) {
		t.Fatalf("a token outside the schema's encrypted parameter was redacted: %s", out)
	}
}

// A long string that merely looks like base64 is an ordinary argument.
func TestIsFernetNeedsTheTokenShape(t *testing.T) {
	if isFernet(strings.Repeat("gAAAAA", 20)) {
		t.Error("a base64 run whose length is not a Fernet layout was taken for a token")
	}
	if isFernet("echo gAAAAA") {
		t.Error("a short string was taken for a token")
	}
}
