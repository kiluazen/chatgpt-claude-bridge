package responses

// OutputItem is an item the bridge streams to Codex.
type OutputItem interface {
	// inProgress is the item as output_item.added announces it.
	inProgress() OutputItem
}

type Message struct {
	ID      string       `json:"id"`
	Type    string       `json:"type"`
	Status  string       `json:"status"`
	Role    string       `json:"role"`
	Content []OutputText `json:"content"`
}

type OutputText struct {
	Type        string     `json:"type"`
	Text        string     `json:"text"`
	Annotations []struct{} `json:"annotations"`
}

// ReasoningItem is shown by Codex as the model's thinking.
type ReasoningItem struct {
	ID      string        `json:"id"`
	Type    string        `json:"type"`
	Summary []SummaryText `json:"summary"`
}

type SummaryText struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// FunctionCall asks Codex to run a JSON function tool. Tools inside a
// namespace go back with the member name and the namespace, as native models
// send them.
type FunctionCall struct {
	ID        string `json:"id"`
	Type      string `json:"type"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Namespace string `json:"namespace,omitempty"`
	Arguments string `json:"arguments"`
}

// CustomToolCall asks Codex to run a freeform tool such as apply_patch.
type CustomToolCall struct {
	ID     string `json:"id"`
	Type   string `json:"type"`
	Status string `json:"status"`
	CallID string `json:"call_id"`
	Name   string `json:"name"`
	Input  string `json:"input"`
}

// WebSearchCall shows a web search Claude ran itself; Codex only draws it.
type WebSearchCall struct {
	ID     string    `json:"id"`
	Type   string    `json:"type"`
	Status string    `json:"status"`
	Action WebAction `json:"action"`
}

type WebAction struct {
	Type  string `json:"type"`
	Query string `json:"query,omitempty"`
	URL   string `json:"url,omitempty"`
}

// Compaction answers a compaction Codex asks for: the summary that stands in
// for the thread before it. OpenAI encrypts its summaries; this one is plain
// text.
type Compaction struct {
	ID               string `json:"id"`
	Type             string `json:"type"`
	EncryptedContent string `json:"encrypted_content"`
}

func NewCompaction(summary string) Compaction {
	return Compaction{ID: "cmp_" + NewID(), Type: TypeCompaction, EncryptedContent: summary}
}

func NewFunctionCall(callID, name, namespace, arguments string) FunctionCall {
	return FunctionCall{ID: "fc_" + NewID(), Type: "function_call", Status: "completed", CallID: callID,
		Name: name, Namespace: namespace, Arguments: arguments}
}

func NewCustomToolCall(callID, name, input string) CustomToolCall {
	return CustomToolCall{ID: "ctc_" + NewID(), Type: "custom_tool_call", Status: "completed", CallID: callID,
		Name: name, Input: input}
}

func NewWebSearchCall(action WebAction) WebSearchCall {
	return WebSearchCall{ID: "ws_" + NewID(), Type: "web_search_call", Status: "completed", Action: action}
}

func outputText(text string) OutputText {
	return OutputText{Type: "output_text", Text: text, Annotations: []struct{}{}}
}

func (m Message) inProgress() OutputItem {
	m.Status, m.Content = "in_progress", []OutputText{}
	return m
}

func (r ReasoningItem) inProgress() OutputItem {
	r.Summary = []SummaryText{}
	return r
}

func (c FunctionCall) inProgress() OutputItem {
	c.Status = "in_progress"
	return c
}

func (c CustomToolCall) inProgress() OutputItem {
	c.Status = "in_progress"
	return c
}

func (c Compaction) inProgress() OutputItem { return c }

func (c WebSearchCall) inProgress() OutputItem {
	c.Status = "in_progress"
	return c
}
