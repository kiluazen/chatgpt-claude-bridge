package translate

import (
	"cmp"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// ContextSize is the context of one Claude API call.
type ContextSize struct {
	Tokens int
	Cached int
}

// ContextOf reads a call's context size from its usage: every input token,
// cached or not.
func ContextOf(u claude.Usage) ContextSize {
	return ContextSize{Tokens: u.InputTokens + u.CacheCreationInputTokens + u.CacheReadInputTokens, Cached: u.CacheReadInputTokens}
}

// Usage is the usage reported to Codex. Codex reads how full the context is
// from the last response, so input is the newest call's context rather than
// a total for the turn.
func Usage(c ContextSize, outputTokens, reasoningTokens int) responses.Usage {
	return responses.Usage{
		InputTokens:         c.Tokens,
		InputTokensDetails:  responses.InputDetails{CachedTokens: c.Cached},
		OutputTokens:        outputTokens,
		OutputTokensDetails: responses.OutputDetails{ReasoningTokens: reasoningTokens},
		TotalTokens:         c.Tokens + outputTokens,
	}
}

var (
	promptTooLong = regexp.MustCompile(`prompt is too long|context (window|length)|too many tokens`)
	overloaded    = regexp.MustCompile(`overloaded|\b529\b`)
	rateLimited   = regexp.MustCompile(`rate.?limit|\b429\b`)
)

// Failure maps a failed Claude turn to the Codex error that shows it
// truthfully. exited means Claude Code itself stopped, which a retry recovers
// from by resuming the session.
func Failure(resultText string, limit *claude.RateLimit, exited bool) responses.Failure {
	lower := strings.ToLower(resultText)
	switch {
	case limit != nil && limit.Status == "rejected":
		when := "a later time"
		if limit.ResetsAt > 0 {
			when = time.Unix(limit.ResetsAt, 0).Format("15:04")
		}
		return responses.Failure{Code: responses.CodeInvalidPrompt,
			Message: fmt.Sprintf("Claude usage limit reached (%s window). It resets at %s.", cmp.Or(limit.RateLimitType, "plan"), when)}
	case promptTooLong.MatchString(lower):
		return responses.Failure{Code: responses.CodeContextLength, Message: resultText}
	case overloaded.MatchString(lower):
		return responses.Failure{Code: responses.CodeRateLimited, Message: "Claude is overloaded right now. Please try again in 20s."}
	case rateLimited.MatchString(lower):
		return responses.Failure{Code: responses.CodeRateLimited, Message: "Claude is rate limited. Please try again in 30s."}
	case exited:
		return responses.Failure{Code: responses.CodeServerError,
			Message: strings.TrimSpace("Claude Code stopped unexpectedly; retrying resumes the session. " + resultText)}
	}
	return responses.Failure{Code: responses.CodeInvalidPrompt, Message: "Claude Code error: " + cmp.Or(resultText, "unknown error")}
}

// CompactionNote is the status line shown when Claude Code compacts on its own.
func CompactionNote(m claude.CompactMetadata) string {
	size := ""
	switch {
	case m.PreTokens > 0 && m.PostTokens > 0:
		size = fmt.Sprintf(" from %s to %s tokens", thousands(m.PreTokens), thousands(m.PostTokens))
	case m.PreTokens > 0:
		size = fmt.Sprintf(" (it was %s tokens)", thousands(m.PreTokens))
	}
	return "**Context compacted**\n\nClaude Code compacted this thread's context" + size + "."
}

// CompactReply answers a Codex compaction that Claude Code carried out.
func CompactReply(m claude.CompactMetadata) string {
	size := ""
	if m.PreTokens > 0 {
		size = fmt.Sprintf(" (it was %s tokens)", thousands(m.PreTokens))
	}
	return "Claude Code compacted its own context" + size + ". Codex's summary is not used for Claude."
}

func thousands(n int) string {
	s := strconv.Itoa(n)
	for i := len(s) - 3; i > 0; i -= 3 {
		s = s[:i] + "," + s[i:]
	}
	return s
}
