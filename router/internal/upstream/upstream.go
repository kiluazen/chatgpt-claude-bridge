// Package upstream holds what differs between the upstreams the router
// sends Codex's Responses requests to: OpenAI's Codex backend, OpenRouter
// and the local Claude Code bridge. Each gets the request body in the form
// it accepts.
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
)

// Route names an upstream.
type Route string

const (
	Native     Route = "native"     // OpenAI's Codex backend, with the user's ChatGPT auth
	OpenRouter Route = "openrouter" // OpenRouter, with the router's API key
	Bridge     Route = "bridge"     // the Claude Code bridge, which reads Codex's requests as they are
)

// SummaryPrefix opens a compaction summary another model wrote, as Codex
// words its own local summaries.
const SummaryPrefix = "Another language model started to solve this problem and produced a summary of its thinking process. You also have access to the state of the tools that were used by that language model. Use this to build on the work that has already been done and avoid duplicating work. Here is the summary produced by the other language model, use the information in this summary to assist with your own analysis:\n"

// sealedSummary stands in for a compaction summary OpenAI encrypted.
const sealedSummary = "[The earlier part of this conversation was summarized by OpenAI and encrypted for OpenAI models; it cannot be read here.]"

// Request is the body of a Codex Responses request.
type Request struct {
	body object
}

func Parse(body []byte) (*Request, error) {
	o, err := decode(body)
	if err != nil {
		return nil, fmt.Errorf("invalid Responses API JSON: %w", err)
	}
	return &Request{o}, nil
}

// Model is the model the request asks for.
func (r *Request) Model() string { return r.body.str("model") }

// Type is a websocket message's type, such as response.create. An HTTP
// request has none.
func (r *Request) Type() string { return r.body.str("type") }

// ForNative is the body for OpenAI's backend, and whether it differs from
// what Codex sent. Codex's own requests pass unchanged. Only history other
// models left in a thread is fixed:
//   - An agent message's payload arrives in an encrypted_content part.
//     Claude's, DeepSeek's and Kimi's is plain text there, which OpenAI's
//     servers fail to decrypt, so it becomes a text part.
//   - Their reasoning items carry no OpenAI ciphertext for OpenAI to resolve,
//     so they are dropped.
//   - Their compaction summaries are plain text, so they become a summary
//     message.
func (r *Request) ForNative() ([]byte, bool, error) {
	changed, err := r.transform(func(it object) ([]object, bool, error) {
		switch it.str("type") {
		case "agent_message":
			parts, err := contentParts(it)
			if err != nil {
				return nil, false, err
			}
			retyped := false
			for i, p := range parts {
				if text := p.str("encrypted_content"); p.str("type") == "encrypted_content" && !isCiphertext(text) {
					parts[i] = object{"type": encode("input_text"), "text": encode(text)}
					retyped = true
				}
			}
			if !retyped {
				return nil, false, nil
			}
			it["content"] = encode(parts)
			return []object{it}, true, nil
		case "reasoning":
			if isCiphertext(it.str("encrypted_content")) {
				return nil, false, nil
			}
			return []object{}, true, nil
		case "compaction", "context_compaction":
			if text := it.str("encrypted_content"); !isCiphertext(text) {
				return []object{userMessage(SummaryPrefix + text)}, true, nil
			}
		}
		return nil, false, nil
	})
	if err != nil || !changed {
		return nil, false, err
	}
	return encode(r.body), true, nil
}

// ForOpenRouter is the body for OpenRouter. Output is capped at
// maxOutputTokens. Items only Codex and OpenAI know become what OpenRouter
// reads: agent messages become user messages that read the way Codex frames
// them for GPT, and compaction summaries become a summary message. OpenAI's
// reasoning items, which only OpenAI can use, are dropped, and so are the
// fields Codex sends only to OpenAI.
func (r *Request) ForOpenRouter(maxOutputTokens int) ([]byte, error) {
	limit := maxOutputTokens
	var requested float64
	if json.Unmarshal(r.body["max_output_tokens"], &requested) == nil && requested > 0 {
		limit = min(int(requested), maxOutputTokens)
	}
	r.body["max_output_tokens"] = encode(limit)
	delete(r.body, "stream_options")
	_, err := r.transform(func(it object) ([]object, bool, error) {
		stripped := false
		for _, k := range openAIItemFields {
			if _, ok := it[k]; ok {
				delete(it, k)
				stripped = true
			}
		}
		switch it.str("type") {
		case "agent_message":
			parts, err := contentParts(it)
			if err != nil {
				return nil, false, err
			}
			var text strings.Builder
			for _, p := range parts {
				switch p.str("type") {
				case "input_text", "output_text":
					text.WriteString(p.str("text"))
				case "encrypted_content":
					if payload := p.str("encrypted_content"); isCiphertext(payload) {
						text.WriteString("[payload encrypted by OpenAI for OpenAI models; it cannot be read here]")
					} else {
						text.WriteString(payload)
					}
				}
			}
			return []object{userMessage(text.String())}, true, nil
		case "reasoning":
			if isCiphertext(it.str("encrypted_content")) {
				return []object{}, true, nil
			}
		case "compaction", "context_compaction":
			text := it.str("encrypted_content")
			if isCiphertext(text) {
				return []object{userMessage(sealedSummary)}, true, nil
			}
			return []object{userMessage(SummaryPrefix + text)}, true, nil
		}
		if stripped {
			return []object{it}, true, nil
		}
		return nil, false, nil
	})
	if err != nil {
		return nil, err
	}
	return encode(r.body), nil
}

// openAIItemFields are item fields Codex keeps for OpenAI alone.
var openAIItemFields = []string{"internal_chat_message_metadata_passthrough", "encrypted_function_args"}

// IsCompaction reports whether the request asks for a compaction. Codex ends
// such a request with a compaction_trigger and expects one compaction item
// back: the summary that stands in for the thread before it.
func (r *Request) IsCompaction() bool {
	items, err := r.Input()
	if err != nil || len(items) == 0 {
		return false
	}
	last, err := decode(items[len(items)-1])
	return err == nil && last.str("type") == "compaction_trigger"
}

// SummaryRequest asks for the summary a compaction returns, from a model that
// cannot compact: the compaction_trigger becomes a request to write it, and
// the tools go.
const SummaryRequest = "Your context window is being compacted. Write a handoff summary of this conversation for the model that will continue it, which will see only your summary and the most recent messages. Include the user's goals and requests, key decisions and constraints, the work done so far (files, commands and their results), the current state, and the remaining next steps. Keep exact names, paths, identifiers and values. Write only the summary."

// ForSummary turns a compaction request into a plain request for the summary
// it asks for.
func (r *Request) ForSummary() error {
	items, err := r.Input()
	if err != nil {
		return err
	}
	if n := len(items); n > 0 {
		if last, err := decode(items[n-1]); err == nil && last.str("type") == "compaction_trigger" {
			items = items[:n-1]
		}
	}
	r.body["input"] = encode(append(items, encode(userMessage(SummaryRequest))))
	for _, k := range []string{"tools", "tool_choice", "parallel_tool_calls"} {
		delete(r.body, k)
	}
	return nil
}

// transform replaces each input item fn changes with the items it returns;
// every other item passes as it came. It reports whether anything changed.
func (r *Request) transform(fn func(object) ([]object, bool, error)) (bool, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(r.body["input"], &items); err != nil {
		return false, fmt.Errorf("input: %w", err)
	}
	out := make([]json.RawMessage, 0, len(items))
	changed := false
	for _, raw := range items {
		it, err := decode(raw)
		if err != nil {
			return false, fmt.Errorf("input item: %w", err)
		}
		replacement, ok, err := fn(it)
		if err != nil {
			return false, err
		}
		if !ok {
			out = append(out, raw)
			continue
		}
		changed = true
		for _, item := range replacement {
			out = append(out, encode(item))
		}
	}
	if changed {
		r.body["input"] = encode(out)
	}
	return changed, nil
}

func userMessage(text string) object {
	return object{
		"type":    encode("message"),
		"role":    encode("user"),
		"content": encode([]object{{"type": encode("input_text"), "text": encode(text)}}),
	}
}

func contentParts(it object) ([]object, error) {
	var parts []object
	if err := json.Unmarshal(it["content"], &parts); err != nil {
		return nil, fmt.Errorf("%s content: %w", it.str("type"), err)
	}
	return parts, nil
}

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

// Websocket is what a response.create message says beyond an HTTP request:
// the earlier response it continues, and whether to generate at all.
type Websocket struct {
	PreviousResponseID string
	Generate           bool
}

// Websocket reads a response.create message's websocket-only fields.
func (r *Request) Websocket() Websocket {
	ws := Websocket{PreviousResponseID: r.body.str("previous_response_id"), Generate: true}
	var generate bool
	if json.Unmarshal(r.body["generate"], &generate) == nil {
		ws.Generate = generate
	}
	return ws
}

// Input is the request's input items.
func (r *Request) Input() ([]json.RawMessage, error) {
	var items []json.RawMessage
	if err := json.Unmarshal(r.body["input"], &items); err != nil {
		return nil, fmt.Errorf("input: %w", err)
	}
	return items, nil
}

// Standalone turns a response.create message into the HTTP request it
// stands for: its input follows the items before it, and the websocket-only
// fields go.
func (r *Request) Standalone(before []json.RawMessage) error {
	items, err := r.Input()
	if err != nil {
		return err
	}
	r.body["input"] = encode(append(before[:len(before):len(before)], items...))
	for _, k := range []string{"type", "previous_response_id", "generate"} {
		delete(r.body, k)
	}
	r.body["stream"] = encode(true)
	return nil
}

// Body is the request as JSON.
func (r *Request) Body() []byte { return encode(r.body) }
