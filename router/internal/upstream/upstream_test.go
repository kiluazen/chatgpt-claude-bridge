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
	out, err := parse(t, `{"model":"gpt-6-sol","input":[`+user+`,
		{"type":"agent_message","id":"amsg_1","author":"/root","recipient":"/root/gpt_child","content":[
			{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},
			{"type":"encrypted_content","encrypted_content":"Reply with PONG"}]},
		{"type":"agent_message","id":"amsg_2","content":[{"type":"encrypted_content","encrypted_content":"`+fernet+`"}]}]}`).ForNative()
	if err != nil {
		t.Fatal(err)
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

func TestParseRejectsInvalidJSON(t *testing.T) {
	if _, err := Parse([]byte(`{"model":`)); err == nil || !strings.Contains(err.Error(), "invalid Responses API JSON") {
		t.Fatalf("err %v", err)
	}
}
