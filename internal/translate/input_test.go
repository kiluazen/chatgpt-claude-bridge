package translate

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
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

func TestMessageBlocksRelaysImages(t *testing.T) {
	got := MessageBlocks(items(t, `[{"type":"message","role":"user","content":[
		{"type":"input_text","text":"Describe this"},
		{"type":"input_image","image_url":"data:image/png;base64,aGVsbG8="}]}]`)[0])
	want := []claude.Block{
		claude.Text("[user] Describe this"),
		claude.Image(claude.ImageSource{Type: "base64", MediaType: "image/png", Data: "aGVsbG8="}),
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %#v", got)
	}

	got = MessageBlocks(items(t, `[{"type":"message","role":"user","content":[
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

func TestMessageBlocksFiles(t *testing.T) {
	got := MessageBlocks(items(t, `[{"type":"message","role":"user","content":[
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
	messages, outputs := NewInput(items(t, `[
		{"type":"reasoning","id":"rs","summary":[]},
		{"type":"function_call","call_id":"c1","name":"exec_command","arguments":"{}"},
		{"type":"function_call_output","call_id":"c1","output":"ok"},
		{"type":"custom_tool_call_output","call_id":"c2","output":"patched"}]`), NewSeen(nil, nil), false)
	if len(messages) != 0 || len(outputs) != 2 || outputs[0].CallID != "c1" || outputs[1].CallID != "c2" {
		t.Fatalf("messages %v outputs %v", ids(messages), outputs)
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
