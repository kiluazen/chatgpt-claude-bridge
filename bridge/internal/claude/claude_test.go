package claude

import (
	"encoding/json"
	"slices"
	"testing"
)

func TestArgs(t *testing.T) {
	o := Options{Model: "claude-opus-5-5", Effort: "high", SessionID: "s1", SystemPromptFile: "/tmp/p.txt", MCPURL: "http://x/mcp", Autocompact: "800k"}
	args, err := o.args()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range [][]string{{"--thinking-display", "summarized"}, {"--effort", "high"}, {"--session-id", "s1"}, {"--system-prompt-file", "/tmp/p.txt"}, {"--setting-sources", ""}} {
		if i := slices.Index(args, want[0]); i < 0 || args[i+1] != want[1] {
			t.Errorf("missing %v in %v", want, args)
		}
	}
	o.Resume = true
	resumed, _ := o.args()
	if i := slices.Index(resumed, "--resume"); i < 0 || resumed[i+1] != "s1" || slices.Contains(resumed, "--system-prompt-file") {
		t.Errorf("resume args: %v", resumed)
	}
}

// Event lines as Claude Code 2.1 writes them.
func TestDecodeEvents(t *testing.T) {
	var ev Event
	must := func(line string) Event {
		t.Helper()
		ev = Event{}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		return ev
	}

	ev = must(`{"type":"assistant","message":{"id":"m1","content":[{"type":"tool_use","id":"toolu_1","name":"mcp__codex__exec_command","input":{"cmd":"ls"}}]}}`)
	uses, err := ev.ToolUses()
	if err != nil || len(uses) != 1 || uses[0].ID != "toolu_1" || string(uses[0].Input) != `{"cmd":"ls"}` {
		t.Errorf("tool uses: %+v %v", uses, err)
	}

	ev = must(`{"type":"stream_event","event":{"type":"message_start","message":{"usage":{"input_tokens":2,"cache_creation_input_tokens":1810,"cache_read_input_tokens":5}}}}`)
	if u := ev.Stream.Message.Usage; u.CacheCreationInputTokens != 1810 || u.CacheReadInputTokens != 5 {
		t.Errorf("message_start usage: %+v", u)
	}

	ev = must(`{"type":"stream_event","event":{"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":323,"output_tokens_details":{"thinking_tokens":36}}}}`)
	if ev.Stream.Usage.OutputTokens != 323 || ev.Stream.Usage.OutputTokensDetails.ThinkingTokens != 36 {
		t.Errorf("message_delta usage: %+v", ev.Stream.Usage)
	}

	ev = must(`{"type":"system","subtype":"compact_boundary","compact_metadata":{"trigger":"manual","pre_tokens":2581,"post_tokens":713}}`)
	if ev.CompactMetadata.PreTokens != 2581 || ev.CompactMetadata.PostTokens != 713 {
		t.Errorf("compact metadata: %+v", ev.CompactMetadata)
	}

	ev = must(`{"type":"rate_limit_event","rate_limit_info":{"status":"allowed","resetsAt":1790164200,"rateLimitType":"five_hour","isUsingOverage":false}}`)
	if ev.RateLimit.Status != "allowed" || ev.RateLimit.ResetsAt != 1790164200 {
		t.Errorf("rate limit: %+v", ev.RateLimit)
	}

	ev = must(`{"type":"result","subtype":"error_during_execution","is_error":true,"terminal_reason":"aborted_streaming","errors":["[ede_diagnostic] result_type=user"]}`)
	if ev.TerminalReason != "aborted_streaming" || ev.Errors[0] != "[ede_diagnostic] result_type=user" {
		t.Errorf("result: %+v", ev)
	}

	// A user event's message may carry string content; it must not break decoding.
	must(`{"type":"user","message":{"role":"user","content":"<command-name>/compact</command-name>"}}`)
}

func TestUserText(t *testing.T) {
	for line, want := range map[string]string{
		`{"type":"user","message":{"role":"user","content":"This session is being continued. Summary: x"}}`:                            "This session is being continued. Summary: x",
		`{"type":"user","message":{"role":"user","content":[{"type":"text","text":"a"},{"type":"image"},{"type":"text","text":"b"}]}}`: "ab",
		`{"type":"user"}`: "",
	} {
		var ev Event
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			t.Fatal(err)
		}
		if got := ev.UserText(); got != want {
			t.Errorf("%s: %q", line, got)
		}
	}
}
