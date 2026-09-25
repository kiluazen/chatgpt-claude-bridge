package proxy

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
)

// Codex asks a model to compact a thread with a request that ends in a
// compaction_trigger, and reads back one compaction item: the summary that
// stands in for the thread before it. OpenAI's models write it themselves,
// and the bridge answers with Claude Code's own compaction. OpenRouter's
// models cannot, so the router asks them for the summary in a plain request
// and answers the compaction with it.

// upstreamError is an HTTP error from an upstream, passed on as it came.
type upstreamError struct {
	status int
	header http.Header
	body   []byte
}

func (e *upstreamError) Error() string {
	return fmt.Sprintf("upstream status %d: %s", e.status, bytes.TrimSpace(e.body))
}

// summarize sends f, a request for a summary, and returns the events that
// answer the compaction it stands for.
func (s *Server) summarize(ctx context.Context, f forwarding) ([][]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, f.url.String(), bytes.NewReader(f.body))
	if err != nil {
		return nil, err
	}
	req.Header = f.externalHeader("", "")
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		return nil, &upstreamError{resp.StatusCode, resp.Header, body}
	}

	var summary strings.Builder
	var usage usageCounts
	var failure string
	completed := false
	readErr := readEvents(resp.Body, func(data []byte) bool {
		var ev struct {
			Type string `json:"type"`
			Item struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"item"`
			Response struct {
				Usage *usageCounts `json:"usage"`
				Error *struct {
					Message string `json:"message"`
				} `json:"error"`
			} `json:"response"`
			Error *struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal(data, &ev) != nil {
			return true
		}
		switch ev.Type {
		case "response.output_item.done":
			if ev.Item.Type == "message" {
				for _, c := range ev.Item.Content {
					if c.Type == "output_text" {
						summary.WriteString(c.Text)
					}
				}
			}
		case "response.completed":
			completed = true
			if ev.Response.Usage != nil {
				usage = *ev.Response.Usage
			}
			return false
		case "response.failed", "response.incomplete", "error":
			failure = ev.Type
			if e := ev.Response.Error; e != nil {
				failure += ": " + e.Message
			} else if ev.Error != nil {
				failure += ": " + ev.Error.Message
			}
			return false
		}
		return true
	})
	switch {
	case failure != "":
		return nil, fmt.Errorf("%s wrote no summary: %s", f.model, failure)
	case readErr != nil:
		return nil, readErr
	case !completed || strings.TrimSpace(summary.String()) == "":
		return nil, fmt.Errorf("%s wrote no summary", f.model)
	}
	return compactionEvents(f.model, strings.TrimSpace(summary.String()), usage), nil
}

// usageCounts is the part of a response's usage Codex requires.
type usageCounts struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// compactionEvents are a whole response whose one output is a compaction
// item holding summary. OpenAI's compaction items are encrypted; this one is
// plain text, which the router turns into a summary message for any model
// that reads the thread later.
func compactionEvents(model, summary string, u usageCounts) [][]byte {
	id := "resp_" + rand.Text()
	item := map[string]string{"id": "cmp_" + rand.Text(), "type": "compaction", "encrypted_content": summary}
	response := func(status string, output []any, usage any) map[string]any {
		return map[string]any{"id": id, "object": "response", "model": model, "status": status, "output": output, "usage": usage}
	}
	events := []map[string]any{
		{"type": "response.created", "response": response("in_progress", []any{}, nil)},
		{"type": "response.output_item.added", "output_index": 0, "item": item},
		{"type": "response.output_item.done", "output_index": 0, "item": item},
		{"type": "response.completed", "response": response("completed", []any{item}, map[string]int{
			"input_tokens": u.InputTokens, "output_tokens": u.OutputTokens, "total_tokens": u.InputTokens + u.OutputTokens})},
	}
	out := make([][]byte, len(events))
	for i, ev := range events {
		out[i], _ = json.Marshal(ev)
	}
	return out
}

// serveCompaction answers an HTTP compaction request for an OpenRouter model.
func (s *Server) serveCompaction(w http.ResponseWriter, r *http.Request, f forwarding) {
	events, err := s.summarize(r.Context(), f)
	if err != nil {
		if r.Context().Err() != nil {
			return // Codex hung up first
		}
		if ue, ok := err.(*upstreamError); ok {
			w.Header().Set("Content-Type", ue.header.Get("Content-Type"))
			w.WriteHeader(ue.status)
			w.Write(ue.body)
			return
		}
		writeError(w, http.StatusBadGateway, err.Error())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusOK)
	for _, ev := range events {
		fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventType(ev), ev)
	}
}
