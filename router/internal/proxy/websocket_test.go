package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// wsSeen is a message OpenAI's fake websocket received.
type wsSeen struct {
	header http.Header // the handshake it came on
	raw    string
}

// fakeOpenAI serves websockets as OpenAI's Codex backend does. It answers
// each message with respond's frames; a nil respond closes the socket with a
// policy violation.
func fakeOpenAI(got chan<- wsSeen, respond func(msg string) []string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isWebsocket(r) {
			io.WriteString(w, `{}`)
			return
		}
		w.Header().Set("X-Codex-Turn-State", "sticky-1")
		w.Header().Set("Openai-Model", "gpt-6-sol")
		c, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer c.CloseNow()
		c.SetReadLimit(maxBodyBytes)
		for {
			_, data, err := c.Read(context.Background())
			if err != nil {
				return
			}
			got <- wsSeen{r.Header.Clone(), string(data)}
			if respond == nil {
				c.Close(websocket.StatusPolicyViolation, "connection limit")
				return
			}
			for _, frame := range respond(string(data)) {
				c.Write(context.Background(), websocket.MessageText, []byte(frame))
			}
		}
	}
}

// completes answers a message with a whole response.
func completes(msg string) []string {
	return []string{
		`{"type":"codex.rate_limits","rate_limits":{"primary":{"used_percent":12}}}`,
		`{"type":"response.created","response":{"id":"resp_oa1"}}`,
		`{"type":"response.completed","response":{"id":"resp_oa1"}}`,
	}
}

// sse answers a request with server-sent events.
func sse(events ...string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, ev := range events {
			fmt.Fprintf(w, "event: message\ndata: %s\n\n", ev)
		}
	}
}

var codexHandshake = http.Header{
	"Authorization":      {"Bearer chatgpt-token"},
	"Chatgpt-Account-Id": {"acct"},
	"Openai-Beta":        {"responses_websockets=2026-02-06"},
	"X-Codex-Turn-State": {"turn-0"},
}

func dial(t *testing.T, h harness) (*websocket.Conn, *http.Response) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, resp, err := websocket.Dial(ctx, h.router.URL+"/backend-api/codex/responses",
		&websocket.DialOptions{HTTPHeader: codexHandshake})
	if err != nil {
		t.Fatal(err)
	}
	c.SetReadLimit(maxBodyBytes)
	t.Cleanup(func() { c.CloseNow() })
	return c, resp
}

func send(t *testing.T, c *websocket.Conn, msg string) {
	t.Helper()
	if err := c.Write(context.Background(), websocket.MessageText, []byte(msg)); err != nil {
		t.Fatal(err)
	}
}

// frames reads until a frame ends a response, and returns what it read.
func frames(t *testing.T, c *websocket.Conn) []string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	var out []string
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("after %q: %v", out, err)
		}
		out = append(out, string(data))
		if isTerminal(eventType(data)) {
			return out
		}
	}
}

func receiveWS(t *testing.T, ch <-chan wsSeen) wsSeen {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("OpenAI got no message")
		return wsSeen{}
	}
}

func TestWebsocketRelaysOpenAIModels(t *testing.T) {
	got := make(chan wsSeen, 8)
	h := newHarness(t, withKey, fakeOpenAI(got, completes), ok, ok)
	c, resp := dial(t, h)
	if resp.Header.Get("X-Codex-Turn-State") != "sticky-1" || resp.Header.Get("Openai-Model") != "gpt-6-sol" {
		t.Errorf("Codex did not get OpenAI's handshake headers: %v", resp.Header)
	}

	msg := `{"type":"response.create","model":"gpt-6-sol","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"<b>hi</b>"}]}],"client_metadata":{"x-codex-turn-state":"sticky-1"}}`
	send(t, c, msg)
	seen := receiveWS(t, got)
	if seen.raw != msg {
		t.Errorf("OpenAI got %s", seen.raw)
	}
	for k, v := range codexHandshake {
		if seen.header.Get(k) != v[0] {
			t.Errorf("OpenAI's handshake had %s %q", k, seen.header.Get(k))
		}
	}
	if want := completes(""); strings.Join(frames(t, c), "\n") != strings.Join(want, "\n") {
		t.Errorf("Codex did not get OpenAI's frames as they were")
	}

	// A sub-agent message Claude wrote is fixed for OpenAI.
	send(t, c, `{"type":"response.create","model":"gpt-6-sol","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"From Claude"}]}]}`)
	if seen := receiveWS(t, got); !strings.Contains(seen.raw, `{"text":"From Claude","type":"input_text"}`) {
		t.Errorf("OpenAI got %s", seen.raw)
	}
	frames(t, c)
}

func TestWebsocketHandshakeRefusal(t *testing.T) {
	long := `{"error":{"code":"usage_limit_reached","message":"` + strings.Repeat("x", 3000) + `"}}`
	h := newHarness(t, withKey, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Codex-Primary-Used-Percent", "100")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, long)
	}, ok, ok)
	req, _ := http.NewRequest(http.MethodGet, h.router.URL+"/backend-api/codex/responses", nil)
	for k, v := range map[string]string{"Connection": "Upgrade", "Upgrade": "websocket",
		"Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="} {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || string(body) != long || resp.Header.Get("X-Codex-Primary-Used-Percent") != "100" {
		t.Fatalf("status %d, %d bytes, headers %v", resp.StatusCode, len(body), resp.Header)
	}
}

func TestWebsocketServesClaude(t *testing.T) {
	got := make(chan wsSeen, 8)
	h := newHarness(t, withKey, fakeOpenAI(got, completes), ok, sse(
		`{"type":"response.created","response":{"id":"resp_b1"}}`,
		`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A1"}]}}`,
		`{"type":"response.completed","response":{"id":"resp_b1"}}`,
	))
	c, _ := dial(t, h)
	user := func(text string) string {
		return `{"type":"message","role":"user","content":[{"type":"input_text","text":"` + text + `"}]}`
	}

	// Codex readies the socket; the router answers without the bridge.
	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","generate":false,"input":[`+user("U0")+`]}`)
	warm := frames(t, c)
	var completed struct {
		Response struct{ ID string }
	}
	json.Unmarshal([]byte(warm[len(warm)-1]), &completed)
	if len(warm) != 2 || eventType([]byte(warm[1])) != "response.completed" || completed.Response.ID == "" {
		t.Fatalf("prewarm answered %q", warm)
	}

	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","previous_response_id":"`+completed.Response.ID+`","input":[`+user("U1")+`]}`)
	bridge := receive(t, h.bridge)
	if in := mustJSON(bridge.body["input"]); !strings.Contains(in, "U0") || !strings.Contains(in, "U1") {
		t.Errorf("bridge input %s", in)
	}
	for _, k := range []string{"type", "previous_response_id", "generate"} {
		if _, ok := bridge.body[k]; ok {
			t.Errorf("bridge got %s", k)
		}
	}
	if bridge.header.Get("Authorization") != "" || bridge.header.Get("Chatgpt-Account-Id") != "" {
		t.Errorf("bridge got ChatGPT's headers: %v", bridge.header)
	}
	if f := frames(t, c); len(f) != 3 || !strings.Contains(f[1], "A1") {
		t.Errorf("Codex got %q", f)
	}

	// The next message continues from the bridge's response.
	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","previous_response_id":"resp_b1","input":[`+user("U2")+`]}`)
	bridge = receive(t, h.bridge)
	var in []map[string]any
	json.Unmarshal([]byte(mustJSON(bridge.body["input"])), &in)
	if len(in) != 4 || !strings.Contains(mustJSON(in[2]), "A1") || !strings.Contains(mustJSON(in[3]), "U2") {
		t.Errorf("bridge input %s", mustJSON(in))
	}
	frames(t, c)

	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","previous_response_id":"resp_elsewhere","input":[]}`)
	if f := frames(t, c); len(f) != 1 || !strings.Contains(f[0], `"previous_response_not_found"`) || !strings.Contains(f[0], `"status":400`) {
		t.Errorf("unknown continuation got %q", f)
	}

	// OpenAI's socket closed at the first Claude message, and is dialed
	// again for an OpenAI model, without the old turn's sticky routing.
	send(t, c, `{"type":"response.create","model":"gpt-6-sol","input":[]}`)
	seen := receiveWS(t, got)
	if seen.header.Get("X-Codex-Turn-State") != "" || seen.header.Get("Authorization") != "Bearer chatgpt-token" {
		t.Errorf("second handshake %v", seen.header)
	}
	frames(t, c)
}

func TestWebsocketServesOpenRouter(t *testing.T) {
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), completes), sse(
		`{"type":"response.completed","response":{"id":"gen_1"}}`, `[DONE]`), ok)
	c, _ := dial(t, h)
	send(t, c, `{"type":"response.create","model":"moonshotai/kimi-k3","max_output_tokens":50000,"input":[]}`)
	or := receive(t, h.openrouter)
	if or.header.Get("Authorization") != "Bearer sk-or-test" || or.body["max_output_tokens"] != 16384.0 {
		t.Errorf("openrouter got %+v", or)
	}
	if f := frames(t, c); len(f) != 1 {
		t.Errorf("Codex got %q", f)
	}
}

func TestWebsocketErrors(t *testing.T) {
	bridge := make(chan http.HandlerFunc, 2)
	bridge <- func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Retry-After", "7")
		w.WriteHeader(http.StatusTooManyRequests)
		io.WriteString(w, `{"error":{"message":"slow down","code":"rate_limit"}}`)
	}
	bridge <- sse(`{"type":"response.created","response":{"id":"resp_b1"}}`) // then ends early
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), completes), ok,
		func(w http.ResponseWriter, r *http.Request) { (<-bridge)(w, r) })
	c, _ := dial(t, h)

	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","input":[]}`)
	f := frames(t, c)
	var ev struct {
		Status  int
		Error   map[string]string
		Headers map[string]string
	}
	json.Unmarshal([]byte(f[0]), &ev)
	if ev.Status != 429 || ev.Error["code"] != "rate_limit" || ev.Headers["retry-after"] != "7" {
		t.Errorf("bridge error reached Codex as %s", f[0])
	}

	send(t, c, `{"type":"response.create","model":"claude-opus-5-5","input":[]}`)
	if f := frames(t, c); len(f) != 2 || !strings.Contains(f[1], `"status":502`) {
		t.Errorf("stream that ended early reached Codex as %q", f)
	}
}

func TestWebsocketPassesOpenAIsClose(t *testing.T) {
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), nil), ok, ok)
	c, _ := dial(t, h)
	send(t, c, `{"type":"response.create","model":"gpt-6-sol","input":[]}`)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	_, _, err := c.Read(ctx)
	var ce websocket.CloseError
	if !errors.As(err, &ce) || ce.Code != websocket.StatusPolicyViolation || ce.Reason != "connection limit" {
		t.Fatalf("Codex got %v", err)
	}
}

func TestWebsocketDrain(t *testing.T) {
	release := make(chan struct{})
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), completes), ok, func(w http.ResponseWriter, r *http.Request) {
		<-release // a Claude turn still running when the router stops
		sse(`{"type":"response.completed","response":{"id":"resp_b1"}}`)(w, r)
	})
	idle, _ := dial(t, h)
	busy, _ := dial(t, h)
	send(t, busy, `{"type":"response.create","model":"claude-opus-5-5","input":[]}`)
	receive(t, h.bridge)
	if n := h.srv.InFlight(); n != 1 {
		t.Fatalf("in_flight %d", n)
	}

	h.srv.Drain()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if _, _, err := idle.Read(ctx); websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Errorf("idle websocket: %v", err)
	}
	close(release)
	if f := frames(t, busy); eventType([]byte(f[len(f)-1])) != "response.completed" {
		t.Errorf("busy websocket got %q", f)
	}
	if _, _, err := busy.Read(ctx); websocket.CloseStatus(err) != websocket.StatusServiceRestart {
		t.Errorf("busy websocket after its response: %v", err)
	}
	if err := h.srv.Wait(ctx); err != nil || h.srv.InFlight() != 0 {
		t.Fatalf("wait %v, in_flight %d", err, h.srv.InFlight())
	}
}

// summary answers a request for a summary as OpenRouter streams one.
var summary = sse(
	`{"type":"response.created","response":{"id":"gen_1"}}`,
	`{"type":"response.output_item.done","item":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"We fixed the parser."}]}}`,
	`{"type":"response.completed","response":{"id":"gen_1","usage":{"input_tokens":900,"output_tokens":40,"total_tokens":940}}}`,
	`[DONE]`)

const compactionRequest = `{"type":"response.create","model":"deepseek/deepseek-v4.1-flash","tools":[{"type":"function","name":"exec"}],
	"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"fix the parser"}]},{"type":"compaction_trigger"}]}`

// compactionItem is the compaction item among a response's frames.
func compactionItem(t *testing.T, events []string) map[string]string {
	t.Helper()
	for _, ev := range events {
		var done struct {
			Type string
			Item map[string]string
		}
		json.Unmarshal([]byte(ev), &done)
		if done.Type == "response.output_item.done" && done.Item["type"] == "compaction" {
			return done.Item
		}
	}
	t.Fatalf("no compaction item in %q", events)
	return nil
}

func TestWebsocketCompactsOpenRouterModels(t *testing.T) {
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), completes), summary, ok)
	c, _ := dial(t, h)
	send(t, c, compactionRequest)
	or := receive(t, h.openrouter)
	if _, ok := or.body["tools"]; ok || !strings.Contains(mustJSON(or.body["input"]), "handoff summary") {
		t.Errorf("openrouter got %s", mustJSON(or.body))
	}
	f := frames(t, c)
	if item := compactionItem(t, f); item["encrypted_content"] != "We fixed the parser." {
		t.Errorf("compaction item %v", item)
	}
	if !strings.Contains(f[len(f)-1], `"input_tokens":900`) {
		t.Errorf("completed %s", f[len(f)-1])
	}
}

func TestHTTPCompactsOpenRouterModels(t *testing.T) {
	h := newHarness(t, withKey, ok, summary, ok)
	resp := h.post(t, "/backend-api/codex/responses", strings.Replace(compactionRequest, `"type":"response.create",`, "", 1))
	var events []string
	readEvents(resp.Body, func(data []byte) bool {
		events = append(events, string(data))
		return true
	})
	if item := compactionItem(t, events); item["encrypted_content"] != "We fixed the parser." || resp.StatusCode != http.StatusOK {
		t.Errorf("status %d, compaction item %v", resp.StatusCode, item)
	}
}

func TestWebsocketPassesCompactionToTheBridge(t *testing.T) {
	h := newHarness(t, withKey, fakeOpenAI(make(chan wsSeen, 8), completes), ok, sse(
		`{"type":"response.output_item.done","item":{"type":"compaction","encrypted_content":"Claude's summary"}}`,
		`{"type":"response.completed","response":{"id":"resp_b1"}}`))
	c, _ := dial(t, h)
	send(t, c, strings.Replace(compactionRequest, "deepseek/deepseek-v4.1-flash", "claude-opus-5-5", 1))
	if bridge := receive(t, h.bridge); !strings.Contains(mustJSON(bridge.body["input"]), "compaction_trigger") {
		t.Errorf("bridge got %s", mustJSON(bridge.body["input"]))
	}
	if item := compactionItem(t, frames(t, c)); item["encrypted_content"] != "Claude's summary" {
		t.Errorf("compaction item %v", item)
	}
}
