package translate

import (
	"cmp"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"unicode"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
)

// MCPPrefix is how Claude Code names the tools the bridge serves.
const MCPPrefix = "mcp__codex__"

// directTools are the Codex tools Claude sees as its own. The rest are reached
// through search_codex_tools and call_codex_tool, which keeps Claude's tool
// list small.
var directTools = map[string]bool{
	"exec_command": true, "write_stdin": true, "apply_patch": true,
	"view_image": true, "update_plan": true, "request_user_input": true,
}

var (
	searchTool = MCPTool{
		Name:        "search_codex_tools",
		Description: "Search the Codex tools and plugins available in this thread. Returns exact tool names, descriptions and input schemas. Search before using a tool that is not directly listed.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}`),
	}
	callTool = MCPTool{
		Name:        "call_codex_tool",
		Description: "Call a Codex tool or plugin found with search_codex_tools. Codex runs it and applies its own approvals. Use the exact name from the search result and pass arguments matching its input schema.",
		InputSchema: json.RawMessage(`{"type":"object","properties":{"name":{"type":"string"},"arguments":{"type":"object"}},"required":["name"]}`),
	}
)

// Tool is a Codex tool as Claude addresses it.
type Tool struct {
	Key         string // the name Claude uses
	CodexName   string // the name Codex expects back
	Namespace   string // Codex's namespace, empty for top-level tools
	Custom      bool   // a freeform tool (apply_patch) rather than a JSON function
	Description string
	InputSchema json.RawMessage
}

// MCPTool is a tool as listed to Claude over MCP.
type MCPTool struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
}

// Registry is the tool view built from one Codex request's tool list.
type Registry struct {
	byKey map[string]*Tool
	// Listed are the tools Claude sees directly.
	Listed    []MCPTool
	fileTools bool
}

func NewRegistry(specs []responses.Tool) *Registry {
	r := &Registry{byKey: map[string]*Tool{}}
	add := func(spec responses.Tool, namespace string) {
		key := spec.Name
		if namespace != "" {
			sep := "__"
			if strings.HasPrefix(spec.Name, "_") {
				sep = ""
			}
			key = namespace + sep + spec.Name
		}
		t := &Tool{Key: key, CodexName: spec.Name, Namespace: namespace, Custom: spec.Type == "custom",
			Description: cmp.Or(spec.Description, key), InputSchema: spec.Parameters}
		switch {
		case t.Custom:
			t.InputSchema = mustJSON(map[string]any{"type": "object", "required": []string{"input"},
				"properties": map[string]any{"input": map[string]string{"type": "string", "description": t.Description}}})
		case len(t.InputSchema) == 0:
			t.InputSchema = json.RawMessage(`{"type":"object","properties":{}}`)
		}
		r.byKey[key] = t
		if namespace == "" && directTools[spec.Name] {
			r.Listed = append(r.Listed, MCPTool{Name: key, Description: t.Description, InputSchema: t.InputSchema})
		}
	}
	for _, spec := range specs {
		switch spec.Type {
		case "function", "custom":
			add(spec, "")
		case "namespace":
			for _, member := range spec.Tools {
				add(member, spec.Name)
			}
		}
	}
	if r.byKey["exec_command"] != nil && r.byKey["apply_patch"] != nil {
		r.fileTools = true
		r.Listed = append(slices.Clone(fileTools), r.Listed...)
	}
	r.Listed = append(r.Listed, searchTool, callTool)
	return r
}

// SearchHit is one search_codex_tools result.
type SearchHit struct {
	Name        string          `json:"name"`
	Description string          `json:"description"`
	InputSchema json.RawMessage `json:"inputSchema"`
	Type        string          `json:"type"`
}

// Search ranks tools by how many query words their name (5 points each) and
// description (1 point each) contain, and returns the best eight.
func (r *Registry) Search(query string) []SearchHit {
	words := strings.FieldsFunc(strings.ToLower(query), func(c rune) bool { return !unicode.IsLetter(c) && !unicode.IsDigit(c) })
	type scored struct {
		hit   SearchHit
		score int
	}
	var all []scored
	for _, t := range r.byKey {
		label, desc := strings.ToLower(t.Key), strings.ToLower(t.Description)
		score := 0
		for _, w := range words {
			if strings.Contains(label, w) {
				score += 5
			}
			if strings.Contains(desc, w) {
				score++
			}
		}
		if score > 0 {
			kind := "function"
			if t.Custom {
				kind = "custom"
			}
			all = append(all, scored{SearchHit{t.Key, t.Description, t.InputSchema, kind}, score})
		}
	}
	slices.SortFunc(all, func(a, b scored) int { return cmp.Or(b.score-a.score, strings.Compare(a.hit.Name, b.hit.Name)) })
	hits := make([]SearchHit, 0, 8)
	for _, s := range all[:min(8, len(all))] {
		hits = append(hits, s.hit)
	}
	return hits
}

// Kind says who handles a Claude tool call.
type Kind string

const (
	KindCodex    Kind = "codex"    // Codex runs it; it goes to Codex as a Responses item
	KindSearch   Kind = "search"   // the bridge answers it from the tool list
	KindReject   Kind = "reject"   // the bridge answers it with an error Claude can correct
	KindDisplay  Kind = "display"  // Claude ran it itself (web search or fetch); Codex only shows it
	KindInternal Kind = "internal" // another Claude-internal tool
)

// Decision is what the bridge does with one Claude tool call.
type Decision struct {
	Kind     Kind
	Tool     *Tool               // KindCodex
	Args     string              // KindCodex function tools: JSON arguments
	Input    string              // KindCodex custom tools: freeform input
	ReadFrom int                 // a Read: the first line, for numbering the output
	Edit     *FileEdit           // an Edit or Write: the change the patch makes
	Query    string              // KindSearch
	Message  string              // KindReject
	Action   responses.WebAction // KindDisplay
}

// FileEdit is the file change behind an Edit or Write.
type FileEdit struct {
	Path   string
	Before string
	Exists bool
	After  string
}

// FileReader returns a file's current text and whether it exists.
type FileReader func(path string) (text string, exists bool, err error)

func reject(message string) Decision { return Decision{Kind: KindReject, Message: message} }

// Classify decides what happens to one Claude tool call.
func Classify(name string, input json.RawMessage, r *Registry, read FileReader) Decision {
	switch name {
	case "WebSearch":
		var a struct {
			Query string `json:"query"`
		}
		if json.Unmarshal(input, &a) != nil {
			return Decision{Kind: KindInternal}
		}
		return Decision{Kind: KindDisplay, Action: responses.WebAction{Type: "search", Query: a.Query}}
	case "WebFetch":
		var a struct {
			URL string `json:"url"`
		}
		if json.Unmarshal(input, &a) != nil {
			return Decision{Kind: KindInternal}
		}
		return Decision{Kind: KindDisplay, Action: responses.WebAction{Type: "open_page", URL: a.URL}}
	}
	tool, ok := strings.CutPrefix(name, MCPPrefix)
	if !ok {
		return Decision{Kind: KindInternal}
	}
	switch tool {
	case "search_codex_tools":
		var a struct {
			Query string `json:"query"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return reject("search_codex_tools: " + err.Error())
		}
		return Decision{Kind: KindSearch, Query: a.Query}
	case "Read", "Edit", "Write":
		if r.fileTools {
			return fileCall(tool, input, r, read)
		}
	case "call_codex_tool":
		var a struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
		}
		if err := json.Unmarshal(input, &a); err != nil {
			return reject("call_codex_tool: " + err.Error())
		}
		return codexCall(r, a.Name, a.Arguments)
	}
	return codexCall(r, tool, input)
}

func codexCall(r *Registry, key string, args json.RawMessage) Decision {
	t := r.byKey[key]
	if t == nil {
		return reject(fmt.Sprintf("Unknown Codex tool %q. Call search_codex_tools and pass the exact name it returns.", key))
	}
	if len(args) == 0 || string(args) == "null" {
		args = json.RawMessage(`{}`)
	}
	if !t.Custom {
		return Decision{Kind: KindCodex, Tool: t, Args: string(args)}
	}
	var a struct {
		Input string `json:"input"`
	}
	if err := json.Unmarshal(args, &a); err != nil {
		return reject(key + ": " + err.Error())
	}
	return Decision{Kind: KindCodex, Tool: t, Input: a.Input}
}

// CallItem is the Responses item that hands a KindCodex decision to Codex.
func CallItem(callID string, d Decision) responses.OutputItem {
	if d.Tool.Custom {
		return responses.NewCustomToolCall(callID, d.Tool.CodexName, d.Input)
	}
	return responses.NewFunctionCall(callID, d.Tool.CodexName, d.Tool.Namespace, d.Args)
}

// mustJSON encodes values built in this package, which always encode.
func mustJSON(v any) json.RawMessage {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}
