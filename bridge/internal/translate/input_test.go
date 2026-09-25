package translate

import (
	"encoding/json"
	"fmt"
	"reflect"
	"testing"

	"github.com/kiluazen/chatgpt-claude-bridge/bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/bridge/internal/responses"
)

func items(t *testing.T, js string) []responses.InputItem {
	t.Helper()
	var v []responses.InputItem
	if err := json.Unmarshal([]byte(js), &v); err != nil {
		t.Fatal(err)
	}
	return v
}

func ids(list []responses.InputItem) []string {
	out := []string{}
	for _, it := range list {
		out = append(out, it.ID)
	}
	return out
}

func TestItemBlocksRelaysImages(t *testing.T) {
	got := ItemBlocks(items(t, `[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"Describe this"},
		{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]`)[0])
	want := []claude.Block{
		claude.Text("[user] Describe this"),
		claude.Image(claude.ImageSource{Type: "base64", MediaType: "image/png", Data: "aGVsbG8="}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}

	got = ItemBlocks(items(t, `[{"type":"message","role":"user","content":[
		{"type":"input_image","image_url":"https://example.com/picture.png"},
		{"type":"input_image","image_url":"file:///tmp/x.png"}]}]`)[0])
	want = []claude.Block{
		claude.Text("[user] (image)"),
		claude.Image(claude.ImageSource{Type: "url", URL: "https://example.com/picture.png"}),
		claude.Text("[Image could not be relayed: unsupported image reference]"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestItemBlocksFiles(t *testing.T) {
	got := ItemBlocks(items(t, `[{"type":"message","role":"user","content":[
		{"type":"input_file","file_url":"https://example.com/a.pdf"},
		{"type":"input_file","filename":"notes.pdf"}]}]`)[0])
	want := []claude.Block{
		claude.Text("[user] [Attached file: https://example.com/a.pdf. Read it with a Codex tool if needed.]"),
		claude.Text("[Attached file notes.pdf was not relayed. Ask for a local path or use a Codex file tool.]"),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestNewInputSkipsReplaysAfterCompaction(t *testing.T) {
	seen := NewSeen(nil, nil)
	first, _ := NewInput(items(t, `[
		{"id":"dev1","type":"message","role":"developer","content":[{"type":"input_text","text":"<skills>"}]},
		{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"Hii"}]}]`), seen, false)
	if len(first) != 2 {
		t.Fatalf("first request: %v", ids(first))
	}
	// After a compaction Codex replays the same content under new ids, plus its summary.
	replay, _ := NewInput(items(t, `[
		{"id":"dev1b","type":"message","role":"developer","content":[{"type":"input_text","text":"<skills>"}]},
		{"id":"u1b","type":"message","role":"user","content":[{"type":"input_text","text":"Hii"}]},
		{"id":"s1","type":"message","role":"user","content":[{"type":"input_text","text":"Another language model started to solve this problem and produced a summary..."}]},
		{"id":"u2","type":"message","role":"user","content":[{"type":"input_text","text":"what next?"}]}]`), seen, false)
	if got := ids(replay); !reflect.DeepEqual(got, []string{"u2"}) {
		t.Fatalf("replay passed %v", got)
	}
}

func TestNewInputRepeatedText(t *testing.T) {
	seen := NewSeen(nil, nil)
	NewInput(items(t, `[{"id":"a","type":"message","role":"user","content":[{"type":"input_text","text":"yes"}]}]`), seen, false)
	got, _ := NewInput(items(t, `[
		{"id":"a","type":"message","role":"user","content":[{"type":"input_text","text":"yes"}]},
		{"id":"r","type":"message","role":"assistant","content":[]},
		{"id":"b","type":"message","role":"user","content":[{"type":"input_text","text":"yes"}]}]`), seen, false)
	if !reflect.DeepEqual(ids(got), []string{"b"}) {
		t.Fatalf("second yes: %v", ids(got))
	}
	strict, _ := NewInput(items(t, `[{"id":"c","type":"message","role":"user","content":[{"type":"input_text","text":"yes"}]}]`), seen, true)
	if len(strict) != 0 {
		t.Fatalf("strict passed %v", ids(strict))
	}
}

func TestNewInputOutputs(t *testing.T) {
	seen := NewSeen([]string{"u0"}, nil)
	messages, outputs := NewInput(items(t, `[
		{"type":"reasoning","id":"rs","summary":[]},
		{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"custom_tool_call_output","call_id":"c2","output":"patched"}]`), seen, false)
	if len(messages) != 0 || len(outputs) != 2 || outputs[0].CallID != "c1" || outputs[1].CallID != "c2" {
		t.Fatalf("messages %v outputs %v", ids(messages), outputs)
	}
}

func TestNewInputAgentMessages(t *testing.T) {
	seen := NewSeen([]string{"u0"}, nil)
	task := `{"id":"%s","type":"agent_message","author":"/root","recipient":"/root/worker","content":[
		{"type":"input_text","text":"Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\n"},
		{"type":"encrypted_content","encrypted_content":"Run the tests"}]}`
	first, _ := NewInput(items(t, "["+fmt.Sprintf(task, "amsg_1")+"]"), seen, false)
	// The same task sent again under a new id is a new task.
	again, _ := NewInput(items(t, "["+fmt.Sprintf(task, "amsg_1")+","+fmt.Sprintf(task, "amsg_2")+"]"), seen, false)
	if !reflect.DeepEqual(ids(first), []string{"amsg_1"}) || !reflect.DeepEqual(ids(again), []string{"amsg_2"}) {
		t.Fatalf("first %v, again %v", ids(first), ids(again))
	}
	want := []claude.Block{claude.Text("[agent message] Message Type: NEW_TASK\nTask name: /root/worker\nSender: /root\nPayload:\nRun the tests")}
	if got := ItemBlocks(first[0]); !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}
}

func TestAgentMessageCiphertext(t *testing.T) {
	// A Fernet token, as OpenAI models' agent messages arrive.
	token := "gAAAAABqtNzW8E9RElMT83aBrlaAXBg8KryjzwzkyDkVz0pDy07k_SVN3fBYPqoD32hkIwrB9t69Clor_Yj1zmitfIGOA1QtV1ZlIPNqtuxIXT7adzt26N99kd4PsDlGLphhKWQ5NeCHuYD1RTZ33NkjUwSclHk9VvW-OmUFrdVGKOXOhIA-02YA8Cxvbe7NKepKMimwCVKr"
	it := items(t, `[{"type":"agent_message","content":[{"type":"input_text","text":"Payload:\n"},{"type":"encrypted_content","encrypted_content":"`+token+`"}]}]`)[0]
	if got := AgentMessageText(it); got != "Payload:\n"+unreadablePayload {
		t.Fatalf("got %q", got)
	}
	for _, text := range []string{"gAAAAA is how a Fernet token starts", "Reply with PONG"} {
		if isCiphertext(text) {
			t.Errorf("%q read as ciphertext", text)
		}
	}
}

func TestSplitAgentMessages(t *testing.T) {
	mail, rest := SplitAgentMessages(items(t, `[
		{"id":"u","type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]},
		{"id":"a","type":"agent_message","content":[{"type":"input_text","text":"Message Type: FINAL_ANSWER\nPayload:\ndone"}]}]`))
	if !reflect.DeepEqual(mail, []string{"[agent message] Message Type: FINAL_ANSWER\nPayload:\ndone"}) || !reflect.DeepEqual(ids(rest), []string{"u"}) {
		t.Fatalf("mail %q rest %v", mail, ids(rest))
	}
}

func TestNewInputForkedHistory(t *testing.T) {
	// A sub-agent forked from its parent: Claude has seen none of the thread.
	seen := NewSeen(nil, nil)
	thread := items(t, `[
		{"id":"u1","type":"message","role":"user","content":[{"type":"input_text","text":"find the bug"}]},
		{"type":"reasoning","id":"rs","summary":[]},
		{"id":"m1","type":"message","role":"assistant","content":[{"type":"output_text","text":"Looking."}]},
		{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{\"cmd\":\"grep -n bug a.go\"}"},
		{"type":"function_call_output","call_id":"c1","output":"a.go:3: bug"},
		{"type":"function_call","call_id":"c2","namespace":"collaboration","name":"spawn_agent","arguments":"{}"},
		{"id":"d1","type":"message","role":"developer","content":[{"type":"input_text","text":"You are an agent in a team"}]},
		{"id":"amsg_1","type":"agent_message","content":[{"type":"input_text","text":"Message Type: NEW_TASK\nPayload:\n"},{"type":"encrypted_content","encrypted_content":"Fix it"}]}]`)
	messages, outputs := NewInput(thread, seen, false)
	var got []string
	for _, it := range messages {
		for _, b := range ItemBlocks(it) {
			got = append(got, b.(claude.TextBlock).Text)
		}
	}
	want := []string{
		"[user] find the bug",
		"[assistant] Looking.",
		`[tool call c1] exec_command {"cmd":"grep -n bug a.go"}`,
		"[tool result c1] a.go:3: bug",
		"[tool call c2] collaboration__spawn_agent {}",
		"[developer] You are an agent in a team",
		"[agent message] Message Type: NEW_TASK\nPayload:\nFix it",
	}
	if !reflect.DeepEqual(got, want) || len(outputs) != 1 {
		t.Fatalf("got %q\noutputs %d", got, len(outputs))
	}
	// The next request passes only what is new.
	next, _ := NewInput(append(thread, items(t, `[{"id":"u2","type":"message","role":"user","content":[{"type":"input_text","text":"thanks"}]}]`)...), seen, false)
	if !reflect.DeepEqual(ids(next), []string{"u2"}) {
		t.Fatalf("next request passed %v", ids(next))
	}
}

func TestSplitCommand(t *testing.T) {
	known := func(name string) bool { return name == "design" || name == "compact" || name == "clear" }
	msgs := items(t, `[
		{"id":"d","type":"message","role":"developer","content":[{"type":"input_text","text":"ctx"}]},
		{"id":"u","type":"message","role":"user","content":[{"type":"input_text","text":"/design make a landing page"}]}]`)
	blocks, command := SplitCommand(msgs, known)
	if command != "/design make a landing page" || !reflect.DeepEqual(blocks, []claude.Block{claude.Text("[developer] ctx")}) {
		t.Fatalf("got %q %#v", command, blocks)
	}
	// Unknown commands, and commands that would split Claude from the thread, stay text.
	for _, text := range []string{"/nonexistent hi", "/clear"} {
		blocks, command := SplitCommand(items(t, `[{"type":"message","role":"user","content":[{"type":"input_text","text":"`+text+`"}]}]`), known)
		if command != "" || !reflect.DeepEqual(blocks, []claude.Block{claude.Text("[user] " + text)}) {
			t.Fatalf("%s: got %q %#v", text, command, blocks)
		}
	}
}

func TestIsCompaction(t *testing.T) {
	cases := []struct {
		name string
		req  responses.Request
		want bool
	}{
		{"request kind", responses.Request{ClientMetadata: map[string]string{"x-codex-turn-metadata": `{"request_kind":"compact"}`}}, true},
		{"prompt", responses.Request{Input: items(t, `[{"type":"message","role":"user","content":[{"type":"input_text","text":"You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff"}]}]`)}, true},
		{"turn", responses.Request{ClientMetadata: map[string]string{"x-codex-turn-metadata": `{"request_kind":"turn"}`},
			Input: items(t, `[{"type":"message","role":"user","content":[{"type":"input_text","text":"hi"}]}]`)}, false},
	}
	for _, c := range cases {
		if got := IsCompaction(&c.req); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}
