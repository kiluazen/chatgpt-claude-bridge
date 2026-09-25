// Package responses models the part of OpenAI's Responses API that Codex
// speaks to a model provider: the request it sends and the server-sent events
// it reads back.
package responses

import (
	"crypto/rand"
	"encoding/json"
)

// Request is one Codex call to POST /responses. Codex resends the whole
// thread in Input on every call.
type Request struct {
	Model          string            `json:"model"`
	Instructions   string            `json:"instructions"`
	Input          []InputItem       `json:"input"`
	Tools          []Tool            `json:"tools"`
	Reasoning      ReasoningConfig   `json:"reasoning"`
	ClientMetadata map[string]string `json:"client_metadata"`
}

type ReasoningConfig struct {
	Effort string `json:"effort"`
}

// ThreadID is the Codex thread the request belongs to.
func (r *Request) ThreadID() string { return r.ClientMetadata["thread_id"] }

// Kind is Codex's request kind, such as "turn" or "compact".
func (r *Request) Kind() string {
	var meta struct {
		RequestKind string `json:"request_kind"`
	}
	if json.Unmarshal([]byte(r.ClientMetadata["x-codex-turn-metadata"]), &meta) != nil {
		return ""
	}
	return meta.RequestKind
}

// Input item types the bridge reads. Reasoning and web searches in the
// thread are Codex's copies of what a model produced; tool calls matter only
// in history Claude has not seen, such as a forked parent's turns.
const (
	TypeMessage        = "message"
	TypeAgentMessage   = "agent_message" // a message from another agent in the thread tree
	TypeFunctionCall   = "function_call"
	TypeCustomCall     = "custom_tool_call"
	TypeFunctionOutput = "function_call_output"
	TypeCustomOutput   = "custom_tool_call_output"
	// TypeCompaction is the summary that stands in for a compacted thread's
	// earlier part: OpenAI's is encrypted, the bridge's and OpenRouter's are
	// plain text. Codex asks for one by ending a request with a
	// TypeCompactionTrigger.
	TypeCompaction        = "compaction"
	TypeContextCompaction = "context_compaction"
	TypeCompactionTrigger = "compaction_trigger"
)

// InputItem is one entry of the thread Codex sends.
type InputItem struct {
	Type      string          `json:"type"`
	ID        string          `json:"id"`
	Role      string          `json:"role"`
	Content   Content         `json:"content"`
	CallID    string          `json:"call_id"`
	Output    json.RawMessage `json:"output"`
	Name      string          `json:"name"`
	Namespace string          `json:"namespace"`
	Arguments string          `json:"arguments"` // function calls
	Input     string          `json:"input"`     // custom tool calls
	// EncryptedContent is a compaction's summary.
	EncryptedContent string `json:"encrypted_content"`
}

// Author is the message role, "user" when unset.
func (it InputItem) Author() string {
	if it.Role == "" {
		return "user"
	}
	return it.Role
}

// IsToolOutput reports whether the item is the result of a tool call.
func (it InputItem) IsToolOutput() bool {
	return it.Type == TypeFunctionOutput || it.Type == TypeCustomOutput
}

// Content is a message's content: a list of parts, or a plain string, which
// the API also allows. Raw keeps the exact bytes for content hashing.
type Content struct {
	Parts []Part
	Raw   json.RawMessage
}

func (c *Content) UnmarshalJSON(data []byte) error {
	c.Raw = append(json.RawMessage(nil), data...)
	if len(data) > 0 && data[0] == '"' {
		var text string
		if err := json.Unmarshal(data, &text); err != nil {
			return err
		}
		c.Parts = []Part{{Type: PartInputText, Text: text}}
		return nil
	}
	return json.Unmarshal(data, &c.Parts)
}

// Content part types.
const (
	PartInputText  = "input_text"
	PartOutputText = "output_text"
	PartInputImage = "input_image"
	PartInputFile  = "input_file"
	// PartEncrypted carries an agent message's payload. OpenAI models write
	// it encrypted; other models' payloads arrive in it as plain text.
	PartEncrypted = "encrypted_content"
)

// Part is one piece of a message: text, an image, a file or an agent
// message's payload.
type Part struct {
	Type             string `json:"type"`
	Text             string `json:"text"`
	ImageURL         string `json:"image_url"`
	Filename         string `json:"filename"`
	FileURL          string `json:"file_url"`
	EncryptedContent string `json:"encrypted_content"`
}

// IsText reports whether the part carries text.
func (p Part) IsText() bool { return p.Type == PartInputText || p.Type == PartOutputText }

// Tool is one entry of Codex's tool list. Namespaces group member tools.
type Tool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Parameters  json.RawMessage `json:"parameters"`
	Tools       []Tool          `json:"tools"`
}

// NewID returns a random id for responses and output items.
func NewID() string { return rand.Text() }
