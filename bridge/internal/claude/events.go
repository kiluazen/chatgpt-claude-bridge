// Package claude drives Claude Code in headless stream-json mode: it starts
// the CLI, decodes its event stream, and writes user messages and control
// requests to it.
package claude

import "encoding/json"

// Event is one line of Claude Code's stream-json output. Which fields are set
// depends on Type and Subtype.
type Event struct {
	Type    string `json:"type"`
	Subtype string `json:"subtype"`

	// system/init: the slash commands and skills this Claude Code offers.
	SlashCommands []string `json:"slash_commands"`
	// system/compact_boundary: Claude Code compacted its context.
	CompactMetadata CompactMetadata `json:"compact_metadata"`
	// rate_limit_event
	RateLimit *RateLimit `json:"rate_limit_info"`
	// stream_event: the raw Anthropic API stream event.
	Stream StreamEvent `json:"event"`
	// assistant: a finished content block of the current API message. Claude
	// Code emits one assistant event per block.
	Message json.RawMessage `json:"message"`

	// result: the end of a turn.
	IsError        bool     `json:"is_error"`
	Result         string   `json:"result"`
	Errors         []string `json:"errors"`
	TerminalReason string   `json:"terminal_reason"`
}

// StreamEvent is an Anthropic Messages API streaming event.
type StreamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage Usage `json:"usage"`
	} `json:"message"`
	ContentBlock ContentBlock `json:"content_block"`
	Delta        Delta        `json:"delta"`
	Usage        Usage        `json:"usage"`
}

type Delta struct {
	Type     string `json:"type"`
	Text     string `json:"text"`
	Thinking string `json:"thinking"`
}

type Usage struct {
	InputTokens              int `json:"input_tokens"`
	CacheCreationInputTokens int `json:"cache_creation_input_tokens"`
	CacheReadInputTokens     int `json:"cache_read_input_tokens"`
	OutputTokens             int `json:"output_tokens"`
	OutputTokensDetails      struct {
		ThinkingTokens int `json:"thinking_tokens"`
	} `json:"output_tokens_details"`
}

type ContentBlock struct {
	Type  string          `json:"type"`
	ID    string          `json:"id"`
	Name  string          `json:"name"`
	Input json.RawMessage `json:"input"`
}

type CompactMetadata struct {
	Trigger    string `json:"trigger"`
	PreTokens  int    `json:"pre_tokens"`
	PostTokens int    `json:"post_tokens"`
}

type RateLimit struct {
	Status         string `json:"status"`
	RateLimitType  string `json:"rateLimitType"`
	ResetsAt       int64  `json:"resetsAt"`
	IsUsingOverage bool   `json:"isUsingOverage"`
}

// ToolUses returns the tool_use blocks of an assistant event.
func (e Event) ToolUses() ([]ContentBlock, error) {
	var m struct {
		Content []ContentBlock `json:"content"`
	}
	if err := json.Unmarshal(e.Message, &m); err != nil {
		return nil, err
	}
	var uses []ContentBlock
	for _, b := range m.Content {
		if b.Type == "tool_use" {
			uses = append(uses, b)
		}
	}
	return uses, nil
}
