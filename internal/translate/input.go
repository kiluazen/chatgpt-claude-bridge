// Package translate holds the pure rules that map Codex's Responses items to
// Claude Code messages and tool calls, and Claude's results back to Codex.
package translate

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
	"slices"
	"strings"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// CompactionPrompt starts the message Codex sends when it compacts a thread.
const CompactionPrompt = "You are performing a CONTEXT CHECKPOINT COMPACTION"

// compactionSummary starts the summary Codex puts in a compacted thread.
const compactionSummary = "Another language model started to solve this problem"

// blockedCommands are Claude Code commands that would pull Claude's session
// apart from the Codex thread.
var blockedCommands = map[string]bool{
	"bg": true, "clear": true, "exit": true, "login": true, "logout": true, "quit": true,
	"remote-control": true, "resume": true, "rewind": true, "teleport": true,
}

var commandPattern = regexp.MustCompile(`^/([A-Za-z0-9][\w:.-]*)(?:\s|$)`)

// Seen records which Codex messages Claude already has: message ids, and
// content hashes that catch replays arriving under new ids.
type Seen struct {
	ids, hashes orderedSet
}

func NewSeen(ids, hashes []string) *Seen {
	s := &Seen{}
	for _, id := range ids {
		s.ids.add(id)
	}
	for _, h := range hashes {
		s.hashes.add(h)
	}
	return s
}

// IDs returns up to limit of the newest message ids.
func (s *Seen) IDs(limit int) []string { return s.ids.newest(limit) }

// Hashes returns up to limit of the newest content hashes.
func (s *Seen) Hashes(limit int) []string { return s.hashes.newest(limit) }

type orderedSet struct {
	order []string
	has   map[string]bool
}

func (o *orderedSet) add(v string) {
	if o.has == nil {
		o.has = map[string]bool{}
	}
	if !o.has[v] {
		o.has[v] = true
		o.order = append(o.order, v)
	}
}

func (o *orderedSet) contains(v string) bool { return o.has[v] }

func (o *orderedSet) newest(n int) []string { return slices.Clone(o.order[max(0, len(o.order)-n):]) }

// NewInput splits one Codex request into what is new for Claude. Codex
// resends the whole thread on every request, and Claude Code already holds
// it, so only unseen user and developer messages pass. Every tool result is
// returned; the session knows which ones Claude is waiting for.
//
// After a compaction Codex replays old messages under new ids, which content
// hashes catch. The newest user message still passes when it repeats an
// earlier text ("yes" twice), unless strict is set right after a compaction.
func NewInput(items []responses.InputItem, seen *Seen, strict bool) (messages, outputs []responses.InputItem) {
	lastUser := -1
	for i, it := range items {
		if it.Type == responses.TypeMessage && it.Author() == "user" {
			lastUser = i
		}
	}
	for i, it := range items {
		if it.IsToolOutput() {
			outputs = append(outputs, it)
			continue
		}
		if it.Type != responses.TypeMessage {
			continue
		}
		hash := contentHash(it)
		idSeen := it.ID != "" && seen.ids.contains(it.ID)
		hashSeen := seen.hashes.contains(hash)
		if it.ID != "" {
			seen.ids.add(it.ID)
		}
		seen.hashes.add(hash)
		if idSeen || it.Author() == "assistant" {
			continue
		}
		if hashSeen && (strict || i != lastUser || it.ID == "") {
			continue
		}
		if text := ItemText(it); strings.HasPrefix(text, compactionSummary) || strings.HasPrefix(text, CompactionPrompt) {
			continue
		}
		messages = append(messages, it)
	}
	return messages, outputs
}

func contentHash(it responses.InputItem) string {
	sum := sha256.Sum256(append([]byte(it.Author()+"\n"), it.Content.Raw...))
	return hex.EncodeToString(sum[:16])
}

// ItemText is the text of a message.
func ItemText(it responses.InputItem) string {
	var b strings.Builder
	for _, p := range it.Content.Parts {
		if p.IsText() {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// MessageBlocks turns one Codex message into Claude content blocks. The role
// tag sits inside the first text block, so text that starts with "/" never
// reads as a Claude Code command.
func MessageBlocks(it responses.InputItem) []claude.Block {
	var blocks []claude.Block
	text := func(s string) {
		if len(blocks) == 0 {
			s = "[" + it.Author() + "] " + s
		}
		blocks = append(blocks, claude.Text(s))
	}
	for _, p := range it.Content.Parts {
		switch {
		case p.IsText():
			if p.Text != "" {
				text(p.Text)
			}
		case p.Type == responses.PartInputImage:
			if len(blocks) == 0 {
				text("(image)")
			}
			if src, ok := imageFromURL(p.ImageURL); ok {
				blocks = append(blocks, claude.Image(src))
			} else {
				blocks = append(blocks, claude.Text("[Image could not be relayed: unsupported image reference]"))
			}
		case p.Type == responses.PartInputFile:
			if p.FileURL != "" {
				text("[Attached file: " + p.FileURL + ". Read it with a Codex tool if needed.]")
			} else {
				text("[Attached file " + p.Filename + " was not relayed. Ask for a local path or use a Codex file tool.]")
			}
		}
	}
	return blocks
}

// SplitCommand separates a trailing Claude Code slash command from the rest.
// The newest user message is a command only when it is plain text that starts
// with a command Claude Code reported (known) and that is not blocked.
func SplitCommand(messages []responses.InputItem, known func(string) bool) (blocks []claude.Block, command string) {
	if n := len(messages); n > 0 && messages[n-1].Author() == "user" && textOnly(messages[n-1]) {
		text := strings.TrimSpace(ItemText(messages[n-1]))
		if m := commandPattern.FindStringSubmatch(text); m != nil && known(m[1]) && !blockedCommands[m[1]] {
			return blocksOf(messages[:n-1]), text
		}
	}
	return blocksOf(messages), ""
}

func textOnly(it responses.InputItem) bool {
	for _, p := range it.Content.Parts {
		if p.Type != responses.PartInputText {
			return false
		}
	}
	return len(it.Content.Parts) > 0
}

func blocksOf(messages []responses.InputItem) []claude.Block {
	var blocks []claude.Block
	for _, it := range messages {
		blocks = append(blocks, MessageBlocks(it)...)
	}
	return blocks
}

// IsCompaction reports whether Codex is asking for a context compaction,
// either by request kind or by its compaction prompt.
func IsCompaction(r *responses.Request) bool {
	if strings.Contains(strings.ToLower(r.Kind()), "compact") {
		return true
	}
	for i := len(r.Input) - 1; i >= 0; i-- {
		if it := r.Input[i]; it.Type == responses.TypeMessage && it.Author() == "user" {
			return strings.HasPrefix(strings.TrimSpace(ItemText(it)), CompactionPrompt)
		}
	}
	return false
}
