package upstream

import (
	"encoding/json"
	"strings"
	"testing"
)

// fernet is an agent message payload as an OpenAI model's arrives.
const fernet = "gAAAAABqtNzW8E9RElMT83aBrlaAXBg8KryjzwzkyDkVz0pDy07k_SVN3fBYPqoD32hkIwrB9t69Clor_Yj1zmitfIGOA1QtV1ZlIPNqtuxIXT7adzt26N99kd4PsDlGLphhKWQ5NeCHuYD1RTZ33NkjUwSclHk9VvW-OmUFrdVGKOXOhIA-02YA8Cxvbe7NKepKMimwCVKr"

func parse(t *testing.T, body string) *Request {
	t.Helper()
	r, err := Parse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	return r
}

type body struct {
	MaxOutputTokens int               `json:"max_output_tokens"`
	Tools           []json.RawMessage `json:"tools"`
	Input           []json.RawMessage `json:"input"`
}

func read(t *testing.T, data []byte) body {
	t.Helper()
	var b body
	if err := json.Unmarshal(data, &b); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return b
}

func TestForNativeRetypesPlainAgentPayloads(t *testing.T) {
	user := `{"type":"message","role":"user","content":[{"type":"input_text","text":"<b>hi</b> & bye"}]}`
	out, changed, err := parse(t, `{"model":"gpt-6-sol","input":[`+user+`,
		{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/gpt_child","content":[
			{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
			{"type":"encrypted_content","encrypted_content":"Reply with PONG"}]},
		{"type":"agent_message","id":"amsg_2","content":[{"type":"encrypted_content","encrypted_content":"`+fernet+`"}]}]}`).ForNative()
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	b := read(t, out)
	if string(b.Input[0]) != user {
		t.Errorf("untouched item changed: %s", b.Input[0])
	}
	var task struct {
		ID      string `json:"id"`
		Author  string `json:"author"`
		Content []map[string]string
	}
	json.Unmarshal(b.Input[1], &task)
	if task.ID != "amsg_1" || task.Author != "/root" || task.Content[1]["type"] != "input_text" || task.Content[1]["text"] != "Reply with PONG" {
		t.Errorf("plain payload: %s", b.Input[1])
	}
	var sealed struct{ Content []map[string]string }
	json.Unmarshal(b.Input[2], &sealed)
	if p := sealed.Content[0]; p["type"] != "encrypted_content" || p["encrypted_content"] != fernet {
		t.Errorf("ciphertext changed: %s", b.Input[2])
	}
}

func TestForNativeLeavesCodexRequestsAlone(t *testing.T) {
	out, changed, err := parse(t, `{"model":"gpt-6-sol","input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"type":"reasoning","summary":[],"encrypted_content":"`+fernet+`"},
		{"type":"compaction","encrypted_content":"`+fernet+`"},
		{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"`+fernet+`"}]}]}`).ForNative()
	if out != nil || changed || err != nil {
		t.Fatalf("out %s, changed %v, err %v", out, changed, err)
	}
}

func TestForNativeFixesOtherModelsHistory(t *testing.T) {
	out, changed, err := parse(t, `{"model":"gpt-6-sol","input":[
		{"type":"compaction","encrypted_content":"Opus summarized: the tests pass."},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"next"}]},
		{"type":"reasoning","summary":[{"type":"summary_text","text":"thinking"}],"encrypted_content":"claude signature"},
		{"type":"reasoning","summary":[],"encrypted_content":"`+fernet+`"}]}`).ForNative()
	if err != nil || !changed {
		t.Fatalf("changed %v, err %v", changed, err)
	}
	b := read(t, out)
	if len(b.Input) != 3 {
		t.Fatalf("input: %s", out)
	}
	var summary struct {
		Type, Role string
		Content    []map[string]string
	}
	json.Unmarshal(b.Input[0], &summary)
	if summary.Type != "message" || summary.Role != "user" || summary.Content[0]["text"] != SummaryPrefix+"Opus summarized: the tests pass." {
		t.Errorf("summary: %s", b.Input[0])
	}
	if !strings.Contains(string(b.Input[2]), fernet) {
		t.Errorf("OpenAI's reasoning was dropped: %s", out)
	}
}

func TestForOpenRouter(t *testing.T) {
	cases := map[string]int{`"max_output_tokens":50000,`: 16384, `"max_output_tokens":100,`: 100, ``: 16384}
	for field, want := range cases {
		out, err := parse(t, `{"model":"moonshotai/kimi-k3",`+field+`"input":[]}`).ForOpenRouter(16384)
		if err != nil {
			t.Fatal(err)
		}
		if got := read(t, out).MaxOutputTokens; got != want {
			t.Errorf("%q: max_output_tokens %d, want %d", field, got, want)
		}
	}

	out, err := parse(t, `{"model":"moonshotai/kimi-k3","input":[
		{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/kimi","content":[
			{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
			{"type":"encrypted_content","encrypted_content":"Summarize a.go"}]},
		{"type":"agent_message","id":"amsg_2","content":[{"type":"input_text","text":"Payload:\n"},{"type":"encrypted_content","encrypted_content":"`+fernet+`"}]}]}`).ForOpenRouter(16384)
	if err != nil {
		t.Fatal(err)
	}
	var got struct {
		Input []struct {
			Type, Role, ID string
			Content        []map[string]string
		}
	}
	if err := json.Unmarshal(out, &got); err != nil {
		t.Fatal(err)
	}
	msgs := got.Input
	if msgs[0].Type != "message" || msgs[0].Role != "user" || msgs[0].ID != "" ||
		msgs[0].Content[0]["text"] != "Message Type: NEW_TASK\nPayload:\nSummarize a.go" {
		t.Errorf("task: %+v", msgs[0])
	}
	if !strings.Contains(msgs[1].Content[0]["text"], "cannot be read here") {
		t.Errorf("ciphertext: %+v", msgs[1])
	}
}

func TestForOpenRouterReadsCompaction(t *testing.T) {
	out, err := parse(t, `{"model":"moonshotai/kimi-k3","input":[
		{"type":"compaction","encrypted_content":"`+fernet+`"},
		{"type":"compaction","encrypted_content":"Opus summarized."},
		{"type":"reasoning","summary":[],"encrypted_content":"`+fernet+`"}]}`).ForOpenRouter(16384)
	if err != nil {
		t.Fatal(err)
	}
	b := read(t, out)
	if len(b.Input) != 2 || !strings.Contains(string(b.Input[0]), "encrypted for OpenAI models") ||
		!strings.Contains(string(b.Input[1]), "Opus summarized.") {
		t.Errorf("input: %s", out)
	}
}

func TestStandalone(t *testing.T) {
	r := parse(t, `{"type":"response.create","model":"claude-opus-5-5","previous_response_id":"resp_1","generate":false,
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"two"}]}]}`)
	if r.Type() != "response.create" {
		t.Errorf("type %q", r.Type())
	}
	if ws := r.Websocket(); ws.PreviousResponseID != "resp_1" || ws.Generate {
		t.Errorf("websocket fields: %+v", ws)
	}
	before := []json.RawMessage{json.RawMessage(`{"type":"message","role":"user","content":[{"type":"input_text","text":"one"}]}`)}
	if err := r.Standalone(before); err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	json.Unmarshal(r.Body(), &got)
	for _, k := range []string{"type", "previous_response_id", "generate"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s kept: %s", k, r.Body())
		}
	}
	if string(got["stream"]) != "true" {
		t.Errorf("stream: %s", got["stream"])
	}
	input, _ := r.Input()
	if len(input) != 2 || !strings.Contains(string(input[0]), "one") || !strings.Contains(string(input[1]), "two") {
		t.Errorf("input: %s", got["input"])
	}
	if len(before) != 1 || cap(before) != 1 {
		t.Errorf("before changed: %d items", len(before))
	}
	if ws := parse(t, `{"type":"response.create","input":[]}`).Websocket(); !ws.Generate || ws.PreviousResponseID != "" {
		t.Errorf("defaults: %+v", ws)
	}
}

func TestParseRejectsInvalidJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"model":`)); err == nil || !strings.Contains(err.Error(), "invalid Responses API JSON") {
		t.Fatalf("err %v", err)
	}
}

func TestSummaryForCompaction(t *testing.T) {
	r := parse(t, `{"model":"moonshotai/kimi-k3","tools":[{"type":"function","name":"exec"}],"tool_choice":"auto","parallel_tool_calls":true,
		"stream_options":{"reasoning_summary_delivery":"sequential_cutoff"},
		"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix it"}],"internal_chat_message_metadata_passthrough":{"x":1}},
		{"type":"function_call","call_id":"c1","name":"exec","arguments":"{}","encrypted_function_args":"gAAAA"},
		{"type":"compaction_trigger"}]}`)
	if !r.IsCompaction() || parse(t, `{"input":[{"type":"message","role":"user","content":[]}]}`).IsCompaction() {
		t.Fatal("IsCompaction")
	}
	if err := r.ForSummary(); err != nil {
		t.Fatal(err)
	}
	out, err := r.ForOpenRouter(16384)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	json.Unmarshal(out, &got)
	for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls", "stream_options"} {
		if _, ok := got[k]; ok {
			t.Errorf("%s kept", k)
		}
	}
	in := string(got["input"])
	if strings.Contains(in, "compaction_trigger") || strings.Contains(in, "internal_chat_message_metadata_passthrough") ||
		strings.Contains(in, "encrypted_function_args") || !strings.Contains(in, "handoff summary") || !strings.Contains(in, "fix it") {
		t.Errorf("input %s", in)
	}
}
