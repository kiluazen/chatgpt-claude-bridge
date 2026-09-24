package translate

import (
	"encoding/base64"
	"strings"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// unreadablePayload stands in for an agent message payload an OpenAI model
// wrote encrypted.
const unreadablePayload = "[payload encrypted by OpenAI for OpenAI models; it cannot be read here]"

// AgentMessageText is an agent message as Codex frames it for native models:
// a header with the message type, the recipient and the sender, then the
// payload.
func AgentMessageText(it responses.InputItem) string {
	var b strings.Builder
	for _, p := range it.Content.Parts {
		switch {
		case p.IsText():
			b.WriteString(p.Text)
		case p.Type == responses.PartEncrypted && isCiphertext(p.EncryptedContent):
			b.WriteString(unreadablePayload)
		case p.Type == responses.PartEncrypted:
			b.WriteString(p.EncryptedContent)
		}
	}
	return b.String()
}

// SplitAgentMessages separates agent messages, as text, from the other new
// messages.
func SplitAgentMessages(messages []responses.InputItem) (mail []string, rest []responses.InputItem) {
	for _, it := range messages {
		if it.Type == responses.TypeAgentMessage {
			mail = append(mail, agentMessageTag+AgentMessageText(it))
		} else {
			rest = append(rest, it)
		}
	}
	return mail, rest
}

const agentMessageTag = "[agent message] "

// isCiphertext reports whether s is a Fernet token, the form OpenAI's
// encrypted content takes: URL-safe base64 of a version byte, a timestamp,
// an IV, whole AES blocks and an HMAC.
func isCiphertext(s string) bool {
	if !strings.HasPrefix(s, "gAAAAA") {
		return false
	}
	b, err := base64.URLEncoding.DecodeString(s)
	return err == nil && len(b) >= 73 && b[0] == 0x80 && (len(b)-57)%16 == 0
}
