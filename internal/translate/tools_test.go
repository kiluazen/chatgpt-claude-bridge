package translate

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

func registry(t *testing.T) *Registry {
	t.Helper()
	var specs []responses.Tool
	if err := json.Unmarshal([]byte(`[
		{"type":"function","name":"exec_command","description":"Run a command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}}}},
		{"type":"custom","name":"apply_patch","description":"Patch files","format":{"type":"grammar"}},
		{"type":"function","name":"view_image","parameters":{"type":"object"}},
		{"type":"namespace","name":"mcp__codex_app","tools":[{"type":"function","name":"open_in_codex","description":"Open a file in a Codex panel","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"mcp__codex_apps__gmail","tools":[{"type":"function","name":"_read_email","description":"Read an email","parameters":{"type":"object"}}]},
		{"type":"namespace","name":"collaboration","tools":[
			{"type":"function","name":"spawn_agent","description":"Spawn an agent","parameters":{"type":"object","properties":{"message":{"type":"string","encrypted":true},"task_name":{"type":"string"}},"required":["task_name","message"]}},
			{"type":"function","name":"wait_agent","description":"Wait for agents","parameters":{"type":"object","properties":{}}}]}
	]`), &specs); err != nil {
		t.Fatal(err)
	}
	return NewRegistry(specs)
}

func TestRegistryListing(t *testing.T) {
	r := registry(t)
	var names []string
	for _, tool := range r.Listed {
		names = append(names, tool.Name)
	}
	want := "Read Edit Write exec_command apply_patch view_image collaboration__spawn_agent collaboration__wait_agent search_codex_tools call_codex_tool"
	if got := strings.Join(names, " "); got != want {
		t.Fatalf("listed %q", got)
	}
	if got := r.Search("panel")[0].Name; got != "mcp__codex_app__open_in_codex" {
		t.Errorf("search panel: %s", got)
	}
	if got := r.Search("gmail")[0].Name; got != "mcp__codex_apps__gmail_read_email" {
		t.Errorf("search gmail: %s", got)
	}
}

func TestAgentToolsDropEncryptionMarks(t *testing.T) {
	for _, tool := range registry(t).Listed {
		if tool.Name != "collaboration__spawn_agent" {
			continue
		}
		var schema struct {
			Properties map[string]map[string]any `json:"properties"`
			Required   []string                  `json:"required"`
		}
		if err := json.Unmarshal(tool.InputSchema, &schema); err != nil {
			t.Fatal(err)
		}
		if _, ok := schema.Properties["message"]["encrypted"]; ok || schema.Properties["message"]["type"] != "string" || len(schema.Required) != 2 {
			t.Fatalf("schema %s", tool.InputSchema)
		}
		return
	}
	t.Fatal("spawn_agent not listed")
}

func TestNamespacedToolGoesBackWithNamespace(t *testing.T) {
	d := Classify(MCPPrefix+"call_codex_tool", json.RawMessage(`{"name":"mcp__codex_app__open_in_codex","arguments":{"path":"/tmp/a.html"}}`), registry(t), nil)
	if d.Kind != KindCodex {
		t.Fatalf("kind %s: %s", d.Kind, d.Message)
	}
	call := CallItem("toolu_1", d).(responses.FunctionCall)
	if call.Name != "open_in_codex" || call.Namespace != "mcp__codex_app" || call.CallID != "toolu_1" || call.Arguments != `{"path":"/tmp/a.html"}` {
		t.Fatalf("got %+v", call)
	}
}

func TestApplyPatchGoesBackAsCustomToolCall(t *testing.T) {
	d := Classify(MCPPrefix+"apply_patch", json.RawMessage(`{"input":"*** Begin Patch\n*** End Patch"}`), registry(t), nil)
	call := CallItem("toolu_2", d).(responses.CustomToolCall)
	if call.Name != "apply_patch" || call.Input != "*** Begin Patch\n*** End Patch" || call.Type != "custom_tool_call" {
		t.Fatalf("got %+v", call)
	}
}

func TestLocalAndDisplayDecisions(t *testing.T) {
	r := registry(t)
	if d := Classify(MCPPrefix+"call_codex_tool", json.RawMessage(`{"name":"nope"}`), r, nil); d.Kind != KindReject || !strings.Contains(d.Message, "search_codex_tools") {
		t.Errorf("unknown tool: %+v", d)
	}
	if d := Classify(MCPPrefix+"search_codex_tools", json.RawMessage(`{"query":"x"}`), r, nil); d.Kind != KindSearch || d.Query != "x" {
		t.Errorf("search: %+v", d)
	}
	if d := Classify("WebSearch", json.RawMessage(`{"query":"codex"}`), r, nil); d.Kind != KindDisplay || d.Action != (responses.WebAction{Type: "search", Query: "codex"}) {
		t.Errorf("web search: %+v", d)
	}
	if d := Classify("WebFetch", json.RawMessage(`{"url":"https://a.b"}`), r, nil); d.Kind != KindDisplay || d.Action != (responses.WebAction{Type: "open_page", URL: "https://a.b"}) {
		t.Errorf("web fetch: %+v", d)
	}
	if d := Classify("TodoWrite", json.RawMessage(`{}`), r, nil); d.Kind != KindInternal {
		t.Errorf("internal: %+v", d)
	}
}
