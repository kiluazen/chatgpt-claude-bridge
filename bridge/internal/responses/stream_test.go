package responses

import (
	"bufio"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

type sseEvent struct {
	Type        string          `json:"type"`
	OutputIndex int             `json:"output_index"`
	Item        json.RawMessage `json:"item"`
	Delta       string          `json:"delta"`
	Response    struct {
		Status string          `json:"status"`
		Output []any           `json:"output"`
		Usage  *Usage          `json:"usage"`
		Error  json.RawMessage `json:"error"`
	} `json:"response"`
}

func events(t *testing.T, body string) []sseEvent {
	t.Helper()
	var out []sseEvent
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data: "); ok {
			var e sseEvent
			if err := json.Unmarshal([]byte(data), &e); err != nil {
				t.Fatal(err)
			}
			out = append(out, e)
		}
	}
	return out
}

func TestStreamEventOrder(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewStream(rec, "claude-opus-5-5")
	s.ReasoningDelta("Weighing it")
	s.Delta("Hello ")
	s.Delta("world")
	s.Add(NewFunctionCall("toolu_1", "exec_command", "", `{"cmd":"ls"}`))
	s.Complete(Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})

	var types []string
	for _, e := range events(t, rec.Body.String()) {
		types = append(types, e.Type)
	}
	want := []string{
		"response.created",
		"response.output_item.added", "response.reasoning_summary_part.added", "response.reasoning_summary_text.delta",
		"response.reasoning_summary_text.done", "response.reasoning_summary_part.done", "response.output_item.done",
		"response.output_item.added", "response.content_part.added", "response.output_text.delta", "response.output_text.delta",
		"response.output_text.done", "response.content_part.done", "response.output_item.done",
		"response.output_item.added", "response.output_item.done",
		"response.completed",
	}
	if strings.Join(types, " ") != strings.Join(want, " ") {
		t.Fatalf("events:\n%s", strings.Join(types, "\n"))
	}
	all := events(t, rec.Body.String())
	done := all[len(all)-1]
	if done.Response.Status != "completed" || len(done.Response.Output) != 3 || done.Response.Usage.InputTokens != 10 {
		t.Fatalf("completed response: %+v", done.Response)
	}
	if call := all[len(all)-2]; call.OutputIndex != 2 || !strings.Contains(string(call.Item), `"status":"completed"`) {
		t.Fatalf("tool call item: %s at %d", call.Item, call.OutputIndex)
	}
	select {
	case <-s.Finished():
	default:
		t.Fatal("stream not finished after Complete")
	}
}

func TestStreamFailAndAbandon(t *testing.T) {
	rec := httptest.NewRecorder()
	s := NewStream(rec, "m")
	s.Fail(Failure{Code: CodeServerError, Message: "restarting"})
	s.Delta("ignored after the end")
	all := events(t, rec.Body.String())
	if last := all[len(all)-1]; last.Type != "response.failed" || !strings.Contains(string(last.Response.Error), `"code":"server_error"`) {
		t.Fatalf("last event %+v", last)
	}

	rec = httptest.NewRecorder()
	s = NewStream(rec, "m")
	s.Abandon()
	s.Delta("ignored")
	if n := len(events(t, rec.Body.String())); n != 1 {
		t.Fatalf("abandoned stream wrote %d events", n)
	}
}
