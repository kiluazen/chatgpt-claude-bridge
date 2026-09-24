package translate

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// MCPResult is a tools/call result sent back to Claude.
type MCPResult struct {
	Content           []MCPContent    `json:"content"`
	StructuredContent json.RawMessage `json:"structuredContent,omitempty"`
	IsError           bool            `json:"isError,omitempty"`
}

// MCPContent is a text or image item of an MCP result.
type MCPContent interface{ isMCPContent() }

type MCPText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type MCPImage struct {
	Type     string `json:"type"`
	Data     string `json:"data"`
	MimeType string `json:"mimeType"`
}

func (MCPText) isMCPContent()  {}
func (MCPImage) isMCPContent() {}

func mcpText(s string) MCPText { return MCPText{Type: "text", Text: s} }

func TextResult(s string) MCPResult { return MCPResult{Content: []MCPContent{mcpText(s)}} }

func ErrorResult(s string) MCPResult {
	r := TextResult(s)
	r.IsError = true
	return r
}

// JSONResult is a text result holding v as JSON.
func JSONResult(v any) MCPResult { return TextResult(string(mustJSON(v))) }

// ToolResult converts a Codex tool output for Claude. Codex sends a string,
// a list of content items (such as view_image's image), or an MCP tool result
// encoded as JSON inside the string.
func ToolResult(output json.RawMessage) MCPResult {
	value := output
	var text string
	if json.Unmarshal(output, &text) == nil {
		if !json.Valid([]byte(text)) {
			return TextResult(text)
		}
		value = json.RawMessage(text)
	}
	var list []json.RawMessage
	if json.Unmarshal(value, &list) == nil {
		return MCPResult{Content: contentItems(list)}
	}
	var result struct {
		Content           []json.RawMessage `json:"content"`
		StructuredContent json.RawMessage   `json:"structuredContent"`
		IsError           bool              `json:"isError"`
	}
	if json.Unmarshal(value, &result) == nil && result.Content != nil {
		return MCPResult{Content: contentItems(result.Content), StructuredContent: result.StructuredContent, IsError: result.IsError}
	}
	return MCPResult{Content: contentItems([]json.RawMessage{value})}
}

func contentItems(items []json.RawMessage) []MCPContent {
	out := make([]MCPContent, 0, len(items))
	for _, raw := range items {
		var s string
		if json.Unmarshal(raw, &s) == nil {
			out = append(out, mcpText(s))
			continue
		}
		if src, ok := imageRef(raw); ok {
			if src.Type == "base64" {
				out = append(out, MCPImage{Type: "image", Data: src.Data, MimeType: src.MediaType})
			} else {
				out = append(out, mcpText("[Tool image could not be relayed: only inline images reach Claude]"))
			}
			continue
		}
		var part struct {
			Type string `json:"type"`
			Text string `json:"text"`
		}
		if json.Unmarshal(raw, &part) == nil && (part.Type == "text" || part.Type == responses.PartInputText || part.Type == responses.PartOutputText) {
			out = append(out, mcpText(part.Text))
			continue
		}
		out = append(out, mcpText(string(raw)))
	}
	return out
}

var (
	dataURL         = regexp.MustCompile(`(?i)^data:([^;,]+);base64,([A-Za-z0-9+/=]+)$`)
	base64Data      = regexp.MustCompile(`^[A-Za-z0-9+/=]+$`)
	supportedImages = map[string]bool{"image/png": true, "image/jpeg": true, "image/gif": true, "image/webp": true}
)

// imageRef reads an image from a Responses input_image (image_url holding a
// data or https URL) or from an MCP image ({data, mimeType}).
func imageRef(raw json.RawMessage) (claude.ImageSource, bool) {
	var v struct {
		ImageURL string `json:"image_url"`
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	}
	if json.Unmarshal(raw, &v) != nil {
		return claude.ImageSource{}, false
	}
	if v.ImageURL != "" {
		return imageFromURL(v.ImageURL)
	}
	if v.Data != "" && base64Data.MatchString(v.Data) {
		if mime := imageMime(v.MimeType, v.Data); mime != "" {
			return claude.ImageSource{Type: "base64", MediaType: mime, Data: v.Data}, true
		}
	}
	return claude.ImageSource{}, false
}

func imageFromURL(url string) (claude.ImageSource, bool) {
	if strings.HasPrefix(strings.ToLower(url), "https://") {
		return claude.ImageSource{Type: "url", URL: url}, true
	}
	m := dataURL.FindStringSubmatch(url)
	if m == nil {
		return claude.ImageSource{}, false
	}
	if mime := imageMime(m[1], m[2]); mime != "" {
		return claude.ImageSource{Type: "base64", MediaType: mime, Data: m[2]}, true
	}
	return claude.ImageSource{}, false
}

// imageMime is the declared image type when Claude supports it, otherwise
// the type read from the data's magic bytes.
func imageMime(declared, data string) string {
	if declared = strings.ToLower(declared); supportedImages[declared] {
		return declared
	}
	head, err := base64.StdEncoding.DecodeString(data[:min(len(data), 64)/4*4])
	if err != nil {
		return ""
	}
	switch {
	case strings.HasPrefix(string(head), "\x89PNG\r\n\x1a\n"):
		return "image/png"
	case strings.HasPrefix(string(head), "\xff\xd8\xff"):
		return "image/jpeg"
	case strings.HasPrefix(string(head), "GIF"):
		return "image/gif"
	case len(head) >= 12 && string(head[:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	}
	return ""
}

const orphanChars = 20000

var inlineData = regexp.MustCompile(`data:[\w/+.-]+;base64,[A-Za-z0-9+/=]+`)

// OrphanText presents tool results that Claude asked for but stopped waiting
// on, because the bridge restarted mid-call, as text so the turn can carry
// on. Image data is dropped and long outputs are cut: base64 costs about one
// token per character.
func OrphanText(outputs []responses.InputItem) string {
	parts := make([]string, 0, len(outputs))
	for _, it := range outputs {
		var text string
		if json.Unmarshal(it.Output, &text) != nil {
			text = string(it.Output)
		}
		text = dropBase64Runs(inlineData.ReplaceAllString(text, "[binary data omitted]"))
		if len(text) > orphanChars {
			cut := orphanChars
			for cut > 0 && !utf8.RuneStart(text[cut]) {
				cut--
			}
			text = fmt.Sprintf("%s\n[truncated %d characters]", text[:cut], len(text)-cut)
		}
		parts = append(parts, fmt.Sprintf("[bridge] Result of your earlier tool call %s:\n%s", it.CallID, text))
	}
	return strings.Join(parts, "\n\n")
}

// dropBase64Runs replaces runs of 2000 or more base64 characters.
func dropBase64Runs(s string) string {
	var b strings.Builder
	start := -1
	flush := func(end int) {
		if start < 0 {
			return
		}
		if end-start >= 2000 {
			b.WriteString("[binary data omitted]")
		} else {
			b.WriteString(s[start:end])
		}
		start = -1
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c >= 'A' && c <= 'Z' || c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '+' || c == '/' || c == '=' {
			if start < 0 {
				start = i
			}
			continue
		}
		flush(i)
		b.WriteByte(c)
	}
	flush(len(s))
	return b.String()
}
