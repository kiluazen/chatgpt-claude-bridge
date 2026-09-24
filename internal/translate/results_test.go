package translate

import (
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/kiluazen/codex-claude-bridge/internal/responses"
)

func TestToolResult(t *testing.T) {
	pngHeader := base64.StdEncoding.EncodeToString([]byte("\x89PNG\r\n\x1a\n"))
	img := func(data string) MCPContent { return MCPImage{Type: "image", Data: data, MimeType: "image/png"} }
	cases := []struct {
		name   string
		output string
		want   MCPResult
	}{
		{"plain text", `"plain result"`, TextResult("plain result")},
		{"mcp result in a string", `"{\"content\":[{\"type\":\"text\",\"text\":\"Here is the chart\"},{\"type\":\"image\",\"mimeType\":\"image/png\",\"data\":\"aGVsbG8=\"}],\"structuredContent\":{\"title\":\"Chart\"},\"isError\":true}"`,
			MCPResult{Content: []MCPContent{mcpText("Here is the chart"), img("aGVsbG8=")}, StructuredContent: json.RawMessage(`{"title":"Chart"}`), IsError: true}},
		{"data url", `"{\"image_url\":\"data:image/png;base64,aGVsbG8=\"}"`, MCPResult{Content: []MCPContent{img("aGVsbG8=")}}},
		{"sniffed type", `"{\"image_url\":\"data:application/octet-stream;base64,` + pngHeader + `\"}"`, MCPResult{Content: []MCPContent{img(pngHeader)}}},
		{"view_image items", `[{"type":"input_image","image_url":"data:application/octet-stream;base64,` + pngHeader + `","detail":"high"}]`, MCPResult{Content: []MCPContent{img(pngHeader)}}},
	}
	for _, c := range cases {
		if got := ToolResult(json.RawMessage(c.output)); !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: got %#v", c.name, got)
		}
	}
}

func TestOrphanTextDropsImagesAndCaps(t *testing.T) {
	image, _ := json.Marshal(`[{"type":"input_image","image_url":"data:image/png;base64,` + strings.Repeat("A", 50000) + `"}]`)
	long, _ := json.Marshal(strings.Repeat("line of output\n", 2000))
	text := OrphanText([]responses.InputItem{{CallID: "toolu_a", Output: image}, {CallID: "toolu_b", Output: long}})
	if !strings.Contains(text, "Result of your earlier tool call toolu_a:") || !strings.Contains(text, "[binary data omitted]") {
		t.Errorf("image output not replaced: %.200s", text)
	}
	if !strings.Contains(text, "[truncated 10000 characters]") || strings.Contains(text, "AAAAAAAAAA") || len(text) > 21000 {
		t.Errorf("long output not capped: %d chars", len(text))
	}
}
