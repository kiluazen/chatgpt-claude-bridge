package responses

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// Usage is the token usage of a response. Codex reads how full the context
// window is from the last response's input tokens.
type Usage struct {
	InputTokens         int           `json:"input_tokens"`
	InputTokensDetails  InputDetails  `json:"input_tokens_details"`
	OutputTokens        int           `json:"output_tokens"`
	OutputTokensDetails OutputDetails `json:"output_tokens_details"`
	TotalTokens         int           `json:"total_tokens"`
}

type InputDetails struct {
	CachedTokens int `json:"cached_tokens"`
}

type OutputDetails struct {
	ReasoningTokens int `json:"reasoning_tokens"`
}

// Failure is the error of a response.failed event. Codex picks its behaviour
// from Code.
type Failure struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

const (
	// CodeInvalidPrompt: Codex shows Message verbatim and does not retry.
	CodeInvalidPrompt = "invalid_prompt"
	// CodeRateLimited: Codex waits for "try again in Ns" in Message, then retries.
	CodeRateLimited = "rate_limit_exceeded"
	// CodeContextLength: Codex shows its own out-of-context message and stops.
	CodeContextLength = "context_length_exceeded"
	// CodeServerError: Codex retries the request, up to five times.
	CodeServerError = "server_error"
)

// Stream writes one Codex /responses reply as server-sent events:
// response.created, then each output item (added, deltas, done), then
// response.completed or response.failed. It keeps the first write error and
// stops writing after it. Callers serialise access; Finished may be waited on
// from any goroutine.
type Stream struct {
	w         io.Writer
	flush     func() error
	id        string
	model     string
	seq       int
	nextIndex int
	output    []OutputItem
	text      *openItem
	reasoning *openItem
	finished  chan struct{}
	closed    bool
	err       error
}

type openItem struct {
	id    string
	index int
	value strings.Builder
}

type event map[string]any

// NewStream starts the event stream for one request.
func NewStream(w http.ResponseWriter, model string) *Stream {
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	s := &Stream{
		w:        w,
		flush:    http.NewResponseController(w).Flush,
		id:       "resp_" + NewID(),
		model:    model,
		output:   []OutputItem{},
		finished: make(chan struct{}),
	}
	s.emit("response.created", event{"response": s.response("in_progress", nil, nil)})
	return s
}

// Finished is closed once the stream has completed, failed or been abandoned.
func (s *Stream) Finished() <-chan struct{} { return s.finished }

// Err is the write error that ended the stream early, if any.
func (s *Stream) Err() error { return s.err }

// HasOutput reports whether anything was streamed yet.
func (s *Stream) HasOutput() bool { return len(s.output) > 0 || s.text != nil || s.reasoning != nil }

// StartReasoning opens a reasoning item.
func (s *Stream) StartReasoning() {
	if s.reasoning != nil {
		return
	}
	s.finishText()
	r := s.open(&s.reasoning, "rs_")
	s.emit("response.output_item.added", event{"output_index": r.index, "item": ReasoningItem{ID: r.id, Type: "reasoning"}.inProgress()})
	s.emit("response.reasoning_summary_part.added", event{"item_id": r.id, "output_index": r.index, "summary_index": 0,
		"part": SummaryText{Type: "summary_text"}})
}

// ReasoningDelta appends thinking text.
func (s *Stream) ReasoningDelta(text string) {
	if text == "" {
		return
	}
	s.StartReasoning()
	r := s.reasoning
	r.value.WriteString(text)
	s.emit("response.reasoning_summary_text.delta", event{"item_id": r.id, "output_index": r.index, "summary_index": 0, "delta": text})
}

// FinishReasoning closes the open reasoning item.
func (s *Stream) FinishReasoning() {
	r := s.reasoning
	if r == nil {
		return
	}
	s.reasoning = nil
	text := r.value.String()
	part := SummaryText{Type: "summary_text", Text: text}
	item := ReasoningItem{ID: r.id, Type: "reasoning", Summary: []SummaryText{}}
	if text != "" {
		item.Summary = append(item.Summary, part)
	}
	s.emit("response.reasoning_summary_text.done", event{"item_id": r.id, "output_index": r.index, "summary_index": 0, "text": text})
	s.emit("response.reasoning_summary_part.done", event{"item_id": r.id, "output_index": r.index, "summary_index": 0, "part": part})
	s.emit("response.output_item.done", event{"output_index": r.index, "item": item})
	s.output = append(s.output, item)
}

// Note shows a status line in the thinking area, for something Claude Code
// did on its own.
func (s *Stream) Note(text string) {
	s.FinishReasoning()
	s.ReasoningDelta(text)
	s.FinishReasoning()
}

// Delta appends assistant text.
func (s *Stream) Delta(text string) {
	if text == "" {
		return
	}
	if s.text == nil {
		s.FinishReasoning()
		t := s.open(&s.text, "msg_")
		s.emit("response.output_item.added", event{"output_index": t.index,
			"item": Message{ID: t.id, Type: "message", Role: "assistant"}.inProgress()})
		s.emit("response.content_part.added", event{"item_id": t.id, "output_index": t.index, "content_index": 0, "part": outputText("")})
	}
	s.text.value.WriteString(text)
	s.emit("response.output_text.delta", event{"item_id": s.text.id, "output_index": s.text.index, "content_index": 0, "delta": text})
}

func (s *Stream) finishText() {
	t := s.text
	if t == nil {
		return
	}
	s.text = nil
	part := outputText(t.value.String())
	item := Message{ID: t.id, Type: "message", Status: "completed", Role: "assistant", Content: []OutputText{part}}
	s.emit("response.output_text.done", event{"item_id": t.id, "output_index": t.index, "content_index": 0, "text": part.Text})
	s.emit("response.content_part.done", event{"item_id": t.id, "output_index": t.index, "content_index": 0, "part": part})
	s.emit("response.output_item.done", event{"output_index": t.index, "item": item})
	s.output = append(s.output, item)
}

// Add emits a finished item, such as a tool call, in the order Claude made it.
func (s *Stream) Add(item OutputItem) {
	s.finishText()
	s.FinishReasoning()
	index := s.nextIndex
	s.nextIndex++
	s.emit("response.output_item.added", event{"output_index": index, "item": item.inProgress()})
	s.emit("response.output_item.done", event{"output_index": index, "item": item})
	s.output = append(s.output, item)
}

// Complete ends the response successfully.
func (s *Stream) Complete(u Usage) {
	if s.closed {
		return
	}
	s.finishText()
	s.FinishReasoning()
	s.emit("response.completed", event{"response": s.response("completed", &u, nil)})
	s.close()
}

// Fail ends the response with an error Codex acts on.
func (s *Stream) Fail(f Failure) {
	if s.closed {
		return
	}
	s.finishText()
	s.FinishReasoning()
	s.emit("response.failed", event{"response": s.response("failed", nil, &f)})
	s.close()
}

// Abandon ends the stream without writing, because Codex is gone.
func (s *Stream) Abandon() { s.close() }

func (s *Stream) open(slot **openItem, prefix string) *openItem {
	*slot = &openItem{id: prefix + NewID(), index: s.nextIndex}
	s.nextIndex++
	return *slot
}

func (s *Stream) response(status string, usage *Usage, failure *Failure) event {
	return event{"id": s.id, "object": "response", "created_at": time.Now().Unix(), "status": status, "model": s.model,
		"output": s.output, "usage": usage, "error": failure, "parallel_tool_calls": true}
}

func (s *Stream) emit(eventType string, fields event) {
	if s.closed {
		return
	}
	s.seq++
	fields["type"] = eventType
	fields["sequence_number"] = s.seq
	data, err := json.Marshal(fields)
	if err == nil {
		_, err = fmt.Fprintf(s.w, "event: %s\ndata: %s\n\n", eventType, data)
	}
	if err == nil {
		err = s.flush()
	}
	if err != nil {
		s.err = err
		s.close()
	}
}

func (s *Stream) close() {
	if !s.closed {
		s.closed = true
		close(s.finished)
	}
}
