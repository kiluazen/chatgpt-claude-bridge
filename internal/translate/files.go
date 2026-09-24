package translate

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/patch"
)

// fileTools are Claude Code's own file tools, which Claude uses best. Each
// call becomes the Codex tool doing the same job, so Codex still runs it and
// draws it natively: Read runs sed through exec_command (or view_image for
// images), Edit and Write become apply_patch.
var fileTools = []MCPTool{
	{
		Name:        "Read",
		Description: "Read a file from the local filesystem. file_path must be absolute. Returns lines numbered from 1, like cat -n. For long files pass offset (first line, 1-based) and limit (line count, default 2000). Image files (png, jpg, gif, webp) are shown to you.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"offset":{"type":"integer"},"limit":{"type":"integer"}},"required":["file_path"]}`),
	},
	{
		Name:        "Edit",
		Description: "Replace exact text in a file. old_string must match the file exactly, including indentation, and must be unique in the file unless replace_all is true. Read the file before editing it.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"old_string":{"type":"string"},"new_string":{"type":"string"},"replace_all":{"type":"boolean"}},"required":["file_path","old_string","new_string"]}`),
	},
	{
		Name:        "Write",
		Description: "Write a whole file, creating it or replacing everything in it. Prefer Edit for changes to an existing file.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"file_path":{"type":"string"},"content":{"type":"string"}},"required":["file_path","content"]}`),
	},
}

var imageFile = regexp.MustCompile(`(?i)\.(png|jpe?g|gif|webp)$`)

const readLimit = 2000

func fileCall(tool string, input json.RawMessage, r *Registry, read FileReader) Decision {
	var a struct {
		FilePath   string `json:"file_path"`
		Offset     int    `json:"offset"`
		Limit      int    `json:"limit"`
		OldString  string `json:"old_string"`
		NewString  string `json:"new_string"`
		ReplaceAll bool   `json:"replace_all"`
		Content    string `json:"content"`
	}
	if err := json.Unmarshal(input, &a); err != nil {
		return reject(tool + ": " + err.Error())
	}
	if !filepath.IsAbs(a.FilePath) {
		return reject("file_path must be an absolute path.")
	}
	if tool == "Read" {
		return readCall(r, a.FilePath, a.Offset, a.Limit)
	}
	before, exists, err := read(a.FilePath)
	if err != nil {
		return reject(fmt.Sprintf("%s: %v", tool, err))
	}
	after := a.Content
	if tool == "Edit" {
		switch n := strings.Count(before, a.OldString); {
		case !exists:
			return reject("File does not exist: " + a.FilePath)
		case a.OldString == "":
			return reject("old_string is empty. Use Write to create a file.")
		case a.OldString == a.NewString:
			return reject("old_string and new_string are the same; nothing to change.")
		case n == 0:
			return reject("String to replace not found in file. Read the file and copy the text exactly, including whitespace.")
		case n > 1 && !a.ReplaceAll:
			return reject(fmt.Sprintf("Found %d matches of the string to replace, but replace_all is false. Add surrounding lines to make it unique, or set replace_all to true.", n))
		case a.ReplaceAll:
			after = strings.ReplaceAll(before, a.OldString, a.NewString)
		default:
			after = strings.Replace(before, a.OldString, a.NewString, 1)
		}
	}
	doc := patch.Add(a.FilePath, after)
	if exists {
		var changed bool
		if doc, changed = patch.Update(a.FilePath, before, after); !changed {
			return reject("The file already has exactly this content; nothing to change.")
		}
	}
	return Decision{Kind: KindCodex, Tool: r.byKey["apply_patch"], Input: doc,
		Edit: &FileEdit{Path: a.FilePath, Before: before, Exists: exists, After: after}}
}

func readCall(r *Registry, path string, offset, limit int) Decision {
	if imageFile.MatchString(path) && r.byKey["view_image"] != nil {
		return Decision{Kind: KindCodex, Tool: r.byKey["view_image"], Args: string(mustJSON(map[string]string{"path": path}))}
	}
	start := max(1, offset)
	if limit < 1 {
		limit = readLimit
	}
	cmd := fmt.Sprintf("sed -n '%d,%dp' %s", start, start+limit-1, shellQuote(path))
	return Decision{Kind: KindCodex, Tool: r.byKey["exec_command"], ReadFrom: start,
		Args: string(mustJSON(map[string]any{"cmd": cmd, "max_output_tokens": 25000}))}
}

func shellQuote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// NumberRead numbers the sed output of a Read the way Claude Code numbers
// Read results (cat -n). Codex puts its own header, ending in "Output:",
// before command output.
func NumberRead(output string, start int) string {
	header, body, ok := strings.Cut(output, "\nOutput:\n")
	if !ok || !strings.Contains(header, "Process exited with code 0") {
		return output
	}
	var lines []string
	for _, line := range strings.Split(header, "\n") {
		if strings.Contains(strings.ToLower(line), "truncat") {
			lines = append(lines, line)
		}
	}
	rows := patch.Lines(body)
	if len(rows) == 0 {
		return "(empty file or no lines in this range)"
	}
	for i, line := range rows {
		lines = append(lines, fmt.Sprintf("%6d\t%s", start+i, line))
	}
	return strings.Join(lines, "\n")
}
