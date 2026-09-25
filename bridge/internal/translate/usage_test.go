package translate

import (
	"regexp"
	"testing"

	"github.com/kiluazen/chatgpt-claude-bridge/bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/bridge/internal/responses"
)

func TestUsageReportsNewestCallContext(t *testing.T) {
	c := ContextOf(claude.Usage{InputTokens: 2, CacheCreationInputTokens: 1000, CacheReadInputTokens: 65840})
	want := responses.Usage{InputTokens: 66842, InputTokensDetails: responses.InputDetails{CachedTokens: 65840},
		OutputTokens: 50, OutputTokensDetails: responses.OutputDetails{ReasoningTokens: 10}, TotalTokens: 66892}
	if got := Usage(c, 50, 10); got != want {
		t.Fatalf("got %+v", got)
	}
}

func TestFailureCodes(t *testing.T) {
	limit := Failure("", &claude.RateLimit{Status: "rejected", RateLimitType: "five_hour", ResetsAt: 1790164200}, false)
	if limit.Code != responses.CodeInvalidPrompt || !regexp.MustCompile(`usage limit reached \(five_hour window\)\. It resets at \d\d:\d\d`).MatchString(limit.Message) {
		t.Errorf("usage limit: %+v", limit)
	}
	cases := map[string]string{
		"API Error: 529 overloaded": responses.CodeRateLimited,
		"rate limited":              responses.CodeRateLimited,
		"Prompt is too long":        responses.CodeContextLength,
		"boom":                      responses.CodeInvalidPrompt,
	}
	for text, code := range cases {
		if got := Failure(text, nil, false); got.Code != code {
			t.Errorf("%q: got %s", text, got.Code)
		}
	}
	if got := Failure("rate limited", nil, false); !regexp.MustCompile(`try again in \d+s`).MatchString(got.Message) {
		t.Errorf("rate limit message lacks a retry delay: %q", got.Message)
	}
	if got := Failure("", nil, true); got.Code != responses.CodeServerError {
		t.Errorf("exited: %+v", got)
	}
}

func TestCompactionNote(t *testing.T) {
	got := CompactionNote(claude.CompactMetadata{PreTokens: 1016426, PostTokens: 288378})
	if got != "**Context compacted**\n\nClaude Code compacted this thread's context from 1,016,426 to 288,378 tokens." {
		t.Fatalf("got %q", got)
	}
}
