package claude

import "crypto/rand"

// Block is a content block of a user message.
type Block interface{ isBlock() }

type TextBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type ImageBlock struct {
	Type   string      `json:"type"`
	Source ImageSource `json:"source"`
}

// ImageSource is either base64 data with a media type, or a URL.
type ImageSource struct {
	Type      string `json:"type"`
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`
}

func (TextBlock) isBlock()  {}
func (ImageBlock) isBlock() {}

func Text(text string) TextBlock { return TextBlock{Type: "text", Text: text} }

func Image(source ImageSource) ImageBlock { return ImageBlock{Type: "image", Source: source} }

// userMessage is a user turn on stdin. Content is a []Block, or a string,
// which is how Claude Code recognises a slash command.
type userMessage struct {
	Type    string `json:"type"`
	Message struct {
		Role    string `json:"role"`
		Content any    `json:"content"`
	} `json:"message"`
}

func newUserMessage(content any) userMessage {
	var m userMessage
	m.Type, m.Message.Role, m.Message.Content = "user", "user", content
	return m
}

type controlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype      string `json:"subtype"`
		CancelQueued bool   `json:"cancel_queued"`
	} `json:"request"`
}

func newInterrupt() controlRequest {
	var m controlRequest
	m.Type, m.RequestID = "control_request", rand.Text()
	m.Request.Subtype, m.Request.CancelQueued = "interrupt", true
	return m
}
