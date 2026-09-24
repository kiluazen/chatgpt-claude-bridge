package claude

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Options configure one headless Claude Code process.
type Options struct {
	Bin              string
	Model            string
	Effort           string
	SessionID        string
	Resume           bool   // continue SessionID instead of starting it
	SystemPromptFile string // for a new session; a resumed one keeps its recorded prompt
	MCPURL           string // loopback MCP endpoint that serves Codex's tools
	Autocompact      string
	Dir              string
}

func (o Options) args() ([]string, error) {
	mcp, err := json.Marshal(map[string]any{
		"mcpServers": map[string]any{"codex": map[string]string{"type": "http", "url": o.MCPURL}},
	})
	if err != nil {
		return nil, err
	}
	args := []string{
		"-p", "--model", o.Model, "--effort", o.Effort,
		// Headless Claude Code omits thinking text unless the display is set explicitly.
		"--thinking-display", "summarized",
		"--autocompact", o.Autocompact,
		// Claude's own tools stay off except web search and fetch; Codex runs everything else.
		"--tools", "WebSearch,WebFetch",
		"--allowedTools", "mcp__codex,WebSearch,WebFetch",
		"--strict-mcp-config", "--mcp-config", string(mcp),
		// No CLAUDE.md, memory, hooks or plugins from this machine's Claude setup.
		"--setting-sources", "",
		"--no-chrome",
		"--input-format", "stream-json", "--output-format", "stream-json", "--verbose", "--include-partial-messages",
	}
	if o.Resume {
		return append(args, "--resume", o.SessionID), nil
	}
	return append(args, "--session-id", o.SessionID, "--system-prompt-file", o.SystemPromptFile), nil
}

// Process is a running Claude Code.
type Process struct {
	cmd    *exec.Cmd
	mu     sync.Mutex // serialises writes to stdin
	stdin  io.WriteCloser
	events chan Event
	done   chan struct{}
	stderr *tail
	err    error // the exit error, set before done closes
}

// Start launches Claude Code. Events is closed, and Done after it, once the
// process has exited.
func Start(o Options) (*Process, error) {
	args, err := o.args()
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(o.Bin, args...)
	cmd.Dir = o.Dir
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("claude stdout: %w", err)
	}
	p := &Process{cmd: cmd, stdin: stdin, events: make(chan Event, 256), done: make(chan struct{}), stderr: &tail{max: 3000}}
	cmd.Stderr = p.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", o.Bin, err)
	}
	go p.read(stdout)
	return p, nil
}

func (p *Process) read(stdout io.Reader) {
	r := bufio.NewReader(stdout)
	for {
		line, err := r.ReadBytes('\n')
		if len(bytes.TrimSpace(line)) > 0 {
			var ev Event
			if uerr := json.Unmarshal(line, &ev); uerr != nil {
				slog.Error("undecodable claude event", "err", uerr, "line", string(line[:min(len(line), 300)]))
			} else {
				p.events <- ev
			}
		}
		if err != nil {
			break
		}
	}
	p.err = p.cmd.Wait()
	close(p.events)
	close(p.done)
}

// Events delivers Claude Code's events in order.
func (p *Process) Events() <-chan Event { return p.events }

// Done is closed after the process has exited.
func (p *Process) Done() <-chan struct{} { return p.done }

// Err is the process's exit error; valid once Done is closed.
func (p *Process) Err() error { return p.err }

// Stderr is the tail of the process's standard error.
func (p *Process) Stderr() string { return p.stderr.String() }

// SendUser writes a user turn: a []Block, or a string for a slash command.
func (p *Process) SendUser(content any) error { return p.send(newUserMessage(content)) }

// Interrupt stops the current turn; Claude Code records it as interrupted.
func (p *Process) Interrupt() error { return p.send(newInterrupt()) }

func (p *Process) send(v any) error {
	data, err := json.Marshal(v)
	if err != nil {
		return err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if _, err := p.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write to claude: %w", err)
	}
	return nil
}

// Terminate sends SIGTERM, kills the process if it is still running after
// grace, and returns once it has exited.
func (p *Process) Terminate(grace time.Duration) {
	if err := p.cmd.Process.Signal(syscall.SIGTERM); err != nil && !errors.Is(err, os.ErrProcessDone) {
		slog.Error("signal claude", "err", err)
	}
	select {
	case <-p.done:
	case <-time.After(grace):
		if err := p.cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
			slog.Error("kill claude", "err", err)
		}
		<-p.done
	}
}

// tail keeps the last max bytes written to it.
type tail struct {
	mu  sync.Mutex
	max int
	buf []byte
}

func (t *tail) Write(b []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, b...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(b), nil
}

func (t *tail) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
