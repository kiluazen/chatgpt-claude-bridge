// Package upstream holds what differs between the upstreams the router
// sends Codex's Responses requests to: OpenAI's Codex backend, OpenRouter
// and the local Claude Code bridge. Each gets the request body in the form
// it accepts.
package upstream

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// Route names an upstream.
type Route string

const (
	Native     Route = "native"     // OpenAI's Codex backend, with the user's ChatGPT auth
	OpenRouter Route = "openrouter" // OpenRouter, with the router's API key
	Bridge     Route = "bridge"     // the Claude Code bridge, which reads Codex's requests as they are
)

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

// ForNative is the body for OpenAI's backend. Codex puts an agent message's
// payload in an encrypted_content part. An OpenAI model's payload is
// ciphertext there, sealed by OpenAI for OpenAI models; Claude's, DeepSeek's
// and Kimi's is plain text, which OpenAI's servers fail to decrypt, so it
// becomes a text part.
func (r *Request) ForNative() ([]byte, error) {
	err := r.eachItem(func(it object) (object, error) {
		parts, err := contentParts(it)
		if err != nil {
			return nil, err
		}
		for i, p := range parts {
			if text := p.str("encrypted_content"); p.str("type") == "encrypted_content" && !isCiphertext(text) {
				parts[i] = object{"type": encode("input_text"), "text": encode(text)}
			}
		}
		it["content"] = encode(parts)
		return it, nil
	}, "agent_message")
	if err != nil {
		return nil, err
	}
	return encode(r.body), nil
}

// ForOpenRouter is the body for OpenRouter: output is capped at
// maxOutputTokens, and agent messages, an item type only Codex and OpenAI
// know, become user messages that read the way Codex frames them for GPT.
func (r *Request) ForOpenRouter(maxOutputTokens int) ([]byte, error) {
	limit := maxOutputTokens
	var requested float64
	if json.Unmarshal(r.body["max_output_tokens"], &requested) == nil && requested > 0 {
		limit = min(int(requested), maxOutputTokens)
	}
	r.body["max_output_tokens"] = encode(limit)
	err := r.eachItem(func(it object) (object, error) {
		parts, err := contentParts(it)
		if err != nil {
			return nil, err
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
		return object{
			"type":    encode("message"),
			"role":    encode("user"),
			"content": encode([]object{{"type": encode("input_text"), "text": encode(text.String())}}),
		}, nil
	}, "agent_message")
	if err != nil {
		return nil, err
	}
	return encode(r.body), nil
}

// eachItem replaces the input items of the given types with what fn returns
// for them; every other item passes as it came.
func (r *Request) eachItem(fn func(object) (object, error), types ...string) error {
	var items []json.RawMessage
	if err := json.Unmarshal(r.body["input"], &items); err != nil {
		return fmt.Errorf("input: %w", err)
	}
	for i, raw := range items {
		it, err := decode(raw)
		if err != nil {
			return fmt.Errorf("input item: %w", err)
		}
		if !slices.Contains(types, it.str("type")) {
			continue
		}
		out, err := fn(it)
		if err != nil {
			return err
		}
		items[i] = encode(out)
	}
	r.body["input"] = encode(items)
	return nil
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
