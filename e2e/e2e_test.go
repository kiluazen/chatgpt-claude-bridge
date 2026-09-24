//go:build e2e

// Package e2e runs the bridge against the real Claude Code and Codex. Each
// test drives a Codex turn (codex exec) or raw Responses requests through a
// bridge on a test port, so it uses your Claude plan. Run: make e2e.
package e2e

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	addr     = "127.0.0.1:41421"
	model    = "claude-opus-5-5"
	codexBin = "/Applications/ChatGPT.app/Contents/Resources/codex"
)

var bridge struct {
	bin, state string
	cmd        *exec.Cmd
}

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "chatgpt-claude-bridge-e2e-")
	if err != nil {
		panic(err)
	}
	bridge.bin, bridge.state = filepath.Join(dir, "bridge"), filepath.Join(dir, "state")
	if out, err := exec.Command("go", "build", "-o", bridge.bin, "../cmd/chatgpt-claude-bridge").CombinedOutput(); err != nil {
		panic(fmt.Sprintf("build: %v\n%s", err, out))
	}
	if err := startBridge(); err != nil {
		panic(err)
	}
	code := m.Run()
	stopBridge()
	os.RemoveAll(dir)
	os.Exit(code)
}

func startBridge() error {
	cmd := exec.Command(bridge.bin)
	cmd.Env = append(os.Environ(), "CHATGPT_CLAUDE_BRIDGE_PORT=41421", "CHATGPT_CLAUDE_BRIDGE_STATE="+bridge.state)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	bridge.cmd = cmd
	for range 50 {
		if resp, err := http.Get("http://" + addr + "/health"); err == nil {
			resp.Body.Close()
			return nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("bridge did not come up on %s", addr)
}

func stopBridge() {
	bridge.cmd.Process.Signal(syscall.SIGTERM)
	bridge.cmd.Wait()
}

// item is a completed Codex item from codex exec --json.
type item struct {
	Type    string `json:"type"`
	Text    string `json:"text"`
	Command string `json:"command"`
}

type turn struct {
	items  []item
	failed bool
}

func (t turn) of(kind string) []item {
	var out []item
	for _, it := range t.items {
		if it.Type == kind {
			out = append(out, it)
		}
	}
	return out
}

func (t turn) message() string {
	var b strings.Builder
	for _, it := range t.of("agent_message") {
		b.WriteString(it.Text)
	}
	return b.String()
}

// codexTurn runs one Codex turn on Opus through the test bridge.
func codexTurn(t *testing.T, dir, prompt string) turn {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	cmd := exec.CommandContext(ctx, codexBin, "exec", "--json", "--skip-git-repo-check", "-m", model,
		"-c", `model_provider="e2e"`,
		"-c", `model_providers.e2e={name="e2e",base_url="http://`+addr+`/api/v1",wire_api="responses",requires_openai_auth=true,stream_max_retries=2}`,
		prompt)
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		t.Fatalf("codex exec: %v", err)
	}
	var tr turn
	sc := bufio.NewScanner(bytes.NewReader(out))
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		var e struct {
			Type string `json:"type"`
			Item item   `json:"item"`
		}
		if json.Unmarshal(sc.Bytes(), &e) != nil {
			continue
		}
		switch e.Type {
		case "item.completed":
			tr.items = append(tr.items, e.Item)
		case "turn.failed":
			tr.failed = true
		}
	}
	if tr.failed {
		t.Fatalf("turn failed:\n%s", out)
	}
	return tr
}

func TestThinkingShows(t *testing.T) {
	tr := codexTurn(t, t.TempDir(), "Think carefully and double-check: how many ways can you tile a 2x10 board with 1x2 dominoes? Answer in two sentences.")
	thought := false
	for _, r := range tr.of("reasoning") {
		thought = thought || strings.TrimSpace(r.Text) != ""
	}
	if !thought || !strings.Contains(tr.message(), "89") {
		t.Fatalf("reasoning %v, message %q", tr.of("reasoning"), tr.message())
	}
}

func TestParallelToolCalls(t *testing.T) {
	dir := t.TempDir()
	writePNG(t, filepath.Join(dir, "flag.png"))
	tr := codexTurn(t, dir, "In ONE reply, make two tool calls in parallel: run `echo PARALLEL-$RANDOM` with exec_command, and look at "+
		filepath.Join(dir, "flag.png")+" with view_image. Then tell me the echo output and the two colors (which side each is on).")
	msg := strings.ToLower(tr.message())
	if len(tr.of("command_execution")) == 0 || !strings.Contains(msg, "red") || !strings.Contains(msg, "blue") {
		t.Fatalf("commands %v, message %q", tr.of("command_execution"), tr.message())
	}
}

func TestUnknownToolRecovers(t *testing.T) {
	tr := codexTurn(t, t.TempDir(), `Test for me: first call call_codex_tool with name "definitely_not_a_real_tool" and no arguments. Then tell me in one line exactly what error you got back.`)
	if !strings.Contains(tr.message(), "Unknown Codex tool") {
		t.Fatalf("message %q", tr.message())
	}
}

func TestReadAndEdit(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "math.js")
	src := "const greeting = \"hello\";\nfunction sub(a, b) {\n  return a - b;\n}\nmodule.exports = { sub };\n"
	if err := os.WriteFile(file, []byte(src), 0o644); err != nil {
		t.Fatal(err)
	}
	tr := codexTurn(t, dir, "Use your Read tool on "+file+`. Then, in ONE reply, make two separate Edit calls on that file: change "hello" to "hi there", and change the sub function body to return b - a.`)
	got, _ := os.ReadFile(file)
	if !strings.Contains(string(got), `"hi there"`) || !strings.Contains(string(got), "return b - a") || len(tr.of("file_change")) < 2 {
		t.Fatalf("file %q, changes %v", got, tr.of("file_change"))
	}
}

func TestUnknownSlashCommandStaysText(t *testing.T) {
	tr := codexTurn(t, t.TempDir(), "/nonexistent hello, reply with one word: received")
	if !strings.Contains(strings.ToLower(tr.message()), "received") {
		t.Fatalf("message %q", tr.message())
	}
}

// reply is the outcome of one raw Responses request.
type reply struct {
	text, status, code string
}

type message struct {
	ID      string `json:"id"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []part `json:"content"`
}

type part struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

func msg(id, role, text string) message {
	kind := "input_text"
	if role == "assistant" {
		kind = "output_text"
	}
	return message{ID: id, Type: "message", Role: role, Content: []part{{kind, text}}}
}

// respond sends one Responses request straight to the bridge. stopAfter > 0
// hangs up after that many text deltas, like pressing Stop; onDelta runs on
// each delta.
func respond(t *testing.T, thread, kind string, input []message, stopAfter int, onDelta func(n int)) reply {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model, "instructions": "You are a coding agent.", "tools": []any{}, "reasoning": map[string]string{"effort": "low"},
		"stream": true, "input": input,
		"client_metadata": map[string]string{"thread_id": thread, "x-codex-turn-metadata": fmt.Sprintf(`{"request_kind":%q}`, kind)},
	})
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, "http://"+addr+"/api/v1/responses", bytes.NewReader(body))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var r reply
	deltas := 0
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(nil, 16<<20)
	for sc.Scan() {
		data, ok := strings.CutPrefix(sc.Text(), "data: ")
		if !ok {
			continue
		}
		var e struct {
			Type     string `json:"type"`
			Delta    string `json:"delta"`
			Response struct {
				Status string `json:"status"`
				Error  struct {
					Code string `json:"code"`
				} `json:"error"`
			} `json:"response"`
		}
		if err := json.Unmarshal([]byte(data), &e); err != nil {
			t.Fatal(err)
		}
		switch e.Type {
		case "response.output_text.delta":
			r.text += e.Delta
			deltas++
			if onDelta != nil {
				onDelta(deltas)
			}
			if stopAfter > 0 && deltas >= stopAfter {
				return r
			}
		case "response.completed", "response.failed":
			r.status, r.code = e.Response.Status, e.Response.Error.Code
			return r
		}
	}
	return r
}

func TestCompactionUsesClaudesOwnCompact(t *testing.T) {
	thread := fmt.Sprintf("e2e-compact-%d", time.Now().UnixNano())
	dev := message{ID: "d1", Type: "message", Role: "developer", Content: []part{{"input_text", "<permissions>full access</permissions>"}}}
	first := msg("u1", "user", "Remember the codeword KESTREL-44. Reply only: noted")
	if r := respond(t, thread, "turn", []message{dev, first}, 0, nil); r.status != "completed" {
		t.Fatalf("turn 1: %+v", r)
	}
	compact := respond(t, thread, "compact", []message{dev, first, msg("a1", "assistant", "noted"),
		msg("c1", "user", "You are performing a CONTEXT CHECKPOINT COMPACTION. Create a handoff summary for another LLM.")}, 0, nil)
	if !strings.HasPrefix(compact.text, "Claude Code compacted its own context") {
		t.Fatalf("compaction reply: %+v", compact)
	}
	// Codex replays the thread under new ids after compacting; only the new question may reach Claude.
	replayDev := dev
	replayDev.ID = "d1b"
	r := respond(t, thread, "turn", []message{replayDev, msg("u1b", "user", first.Content[0].Text),
		msg("s1", "user", "Another language model started to solve this problem and produced a summary of its thinking process..."),
		msg("u2", "user", "What was the codeword? Reply with just the codeword.")}, 0, nil)
	if r.status != "completed" || !strings.Contains(r.text, "KESTREL-44") {
		t.Fatalf("after compaction: %+v", r)
	}
}

func TestStopThenContinue(t *testing.T) {
	thread := fmt.Sprintf("e2e-stop-%d", time.Now().UnixNano())
	story := msg("u1", "user", "Write a 700-word story about a lighthouse.")
	if r := respond(t, thread, "turn", []message{story}, 20, nil); r.text == "" {
		t.Fatal("no text before stopping")
	}
	time.Sleep(2 * time.Second)
	r := respond(t, thread, "turn", []message{story, msg("u2", "user", "Forget the story. Reply with exactly: READY")}, 0, nil)
	if r.status != "completed" || !strings.Contains(r.text, "READY") {
		t.Fatalf("after stop: %+v", r)
	}
}

// Keep last: it restarts the bridge.
func TestRestartResumesTheTurn(t *testing.T) {
	thread := fmt.Sprintf("e2e-restart-%d", time.Now().UnixNano())
	input := []message{msg("u1", "user", "Write a 500-word story about a lighthouse keeper, then end with the word FIN.")}
	r := respond(t, thread, "turn", input, 0, func(n int) {
		if n == 30 {
			bridge.cmd.Process.Signal(syscall.SIGTERM)
		}
	})
	if r.status != "failed" || r.code != "server_error" {
		t.Fatalf("restart should fail the request as retryable: %+v", r)
	}
	bridge.cmd.Wait()
	if err := startBridge(); err != nil {
		t.Fatal(err)
	}
	retry := respond(t, thread, "turn", input, 0, nil) // Codex retries the same request
	if retry.status != "completed" || !strings.Contains(retry.text, "FIN") {
		t.Fatalf("retry: %+v", retry)
	}
}

// writePNG writes a 64x32 image: red on the left half, blue on the right.
func writePNG(t *testing.T, path string) {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, 64, 32))
	for x := range 64 {
		for y := range 32 {
			c := color.RGBA{255, 32, 32, 255}
			if x >= 32 {
				c = color.RGBA{32, 64, 255, 255}
			}
			img.Set(x, y, c)
		}
	}
	f, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := png.Encode(f, img); err != nil {
		t.Fatal(err)
	}
}
