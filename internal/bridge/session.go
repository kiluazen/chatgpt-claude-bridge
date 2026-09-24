package bridge

import (
	"cmp"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/kiluazen/chatgpt-claude-bridge/internal/claude"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/config"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/patch"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/responses"
	"github.com/kiluazen/chatgpt-claude-bridge/internal/translate"
)

// maxSeen caps how many message ids and hashes a session keeps on disk.
const maxSeen = 20_000

// resumeNudge restarts a turn that was cut off when Claude Code or the bridge
// stopped mid-turn.
const resumeNudge = "[bridge] Your previous turn was cut off before it finished because the Claude process restarted. Continue where you left off."

var errNotRunning = errors.New("claude code is not running")

// failureFor maps a bridge error to a Codex failure: a dead Claude process is
// retryable, since the retry starts a fresh one.
func failureFor(err error) responses.Failure {
	if errors.Is(err, errNotRunning) {
		return responses.Failure{Code: responses.CodeServerError, Message: "Claude Code is not running; retrying starts it again."}
	}
	return responses.Failure{Code: responses.CodeInvalidPrompt, Message: "Claude bridge: " + err.Error()}
}

// Session is one Codex thread's Claude Code process and turn state. Claude
// Code owns the conversation and its context; the session relays Codex's
// tools to it and streams every step back to Codex. Fields below mu are
// guarded by it.
type Session struct {
	cfg        config.Config
	commands   *commands
	onExit     func(*Session)
	threadID   string
	id         string // the Claude Code session id, derived from the thread id
	secret     string // part of the MCP URL, so only this Claude can call it
	statePath  string
	promptPath string

	mu     sync.Mutex
	proc   *claude.Process
	effort string

	seen        *translate.Seen
	awaiting    map[string]bool // Codex calls Claude asked for and has not been answered
	interrupted bool            // a turn was open when Claude Code or the bridge last stopped
	running     bool            // a Claude turn is in progress
	strict      bool            // dedupe strictly: the thread was just compacted
	registry    *translate.Registry
	toolsHash   [32]byte

	stream     *responses.Stream // the open Codex request
	compaction *compaction       // a Codex compaction Claude Code is carrying out
	message    *apiMessage       // the Claude API message being streamed
	decisions  map[string]translate.Decision
	virtual    map[string]string // files as edits earlier in the same reply leave them
	pending    map[string]*pendingCall
	queued     map[string]json.RawMessage
	held       []claude.Block // Codex context held back while a slash command runs
	notes      []string       // compaction notes for the next Codex request

	lastCtx         translate.ContextSize
	outputTokens    int
	reasoningTokens int
	rateLimit       *claude.RateLimit
	lastUsed        time.Time
	lastEvent       time.Time
	stopping        bool // the bridge is shutting down
}

type apiMessage struct {
	blocks map[int]string // content block index to block type
	calls  []call
}

type call struct {
	id       string
	decision translate.Decision
}

type compaction struct {
	stream *responses.Stream
	meta   claude.CompactMetadata
}

// pendingCall is an MCP tools/call waiting for Codex to run the tool.
type pendingCall struct {
	result chan callResult
}

type callResult struct {
	output json.RawMessage
	err    string
}

func newSession(cfg config.Config, cmds *commands, req *responses.Request, onExit func(*Session)) (*Session, error) {
	id := sessionID(req.ThreadID())
	s := &Session{
		cfg: cfg, commands: cmds, onExit: onExit, threadID: req.ThreadID(), id: id, secret: rand.Text(),
		statePath:  filepath.Join(cfg.StateDir, id+".json"),
		promptPath: filepath.Join(cfg.StateDir, id+".system.txt"),
		effort:     effortOf(req),
		awaiting:   map[string]bool{},
		decisions:  map[string]translate.Decision{},
		virtual:    map[string]string{},
		pending:    map[string]*pendingCall{},
		queued:     map[string]json.RawMessage{},
		lastUsed:   time.Now(),
		lastEvent:  time.Now(),
	}
	st, exists, err := loadState(s.statePath)
	if err != nil {
		return nil, err
	}
	s.seen = translate.NewSeen(st.SeenIDs, st.SeenHashes)
	s.interrupted = st.TurnOpen
	for _, callID := range st.Awaiting {
		s.awaiting[callID] = true
	}
	if !exists {
		if err := os.WriteFile(s.promptPath, []byte(req.Instructions), 0o600); err != nil {
			return nil, fmt.Errorf("write system prompt: %w", err)
		}
	}
	s.setTools(req.Tools)
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.start(); err != nil {
		return nil, err
	}
	return s, nil
}

// sessionID derives a stable UUID-shaped Claude Code session id from a Codex
// thread id, so a restarted bridge resumes the same Claude session.
func sessionID(threadID string) string {
	sum := sha256.Sum256([]byte(threadID))
	h := []byte(hex.EncodeToString(sum[:16]))
	h[12], h[16] = '4', '8'
	return fmt.Sprintf("%s-%s-%s-%s-%s", h[0:8], h[8:12], h[12:16], h[16:20], h[20:32])
}

// effortOf maps Codex's reasoning effort onto Claude Code's --effort scale.
// Codex offers the same five levels for this model; requests without one,
// such as thread titles, run at medium.
func effortOf(req *responses.Request) string {
	switch e := req.Reasoning.Effort; e {
	case "low", "medium", "high", "xhigh", "max":
		return e
	}
	return "medium"
}

// start launches Claude Code for this session, resuming it once it has state
// on disk.
func (s *Session) start() error {
	_, err := os.Stat(s.statePath)
	resume := err == nil
	p, err := claude.Start(claude.Options{
		Bin: s.cfg.ClaudeBin, Model: s.cfg.Model, Effort: s.effort, SessionID: s.id, Resume: resume,
		SystemPromptFile: s.promptPath, MCPURL: fmt.Sprintf("http://%s/mcp/%s/%s", s.cfg.Addr, s.id, s.secret),
		Autocompact: s.cfg.Autocompact, Dir: s.cfg.StateDir,
	})
	if err != nil {
		return err
	}
	slog.Info("claude session started", "session", s.id, "effort", s.effort, "resume", resume)
	s.proc = p
	go s.consume(p)
	return nil
}

// consume handles one process's events until it exits.
func (s *Session) consume(p *claude.Process) {
	for ev := range p.Events() {
		s.mu.Lock()
		if s.proc == p {
			s.handle(ev)
		}
		s.mu.Unlock()
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.proc != p {
		return // replaced or closed on purpose
	}
	s.proc = nil
	if s.stopping {
		return
	}
	slog.Error("claude code exited", "session", s.id, "err", p.Err(), "stderr", p.Stderr())
	s.fail(translate.Failure(p.Stderr(), nil, true))
	s.cancelPending("Claude Code exited.")
	s.onExit(s)
}

func (s *Session) setTools(tools []responses.Tool) {
	data, err := json.Marshal(tools)
	if err != nil {
		panic(err) // decoded JSON always encodes
	}
	if sum := sha256.Sum256(data); s.registry == nil || sum != s.toolsHash {
		s.registry, s.toolsHash = translate.NewRegistry(tools), sum
	}
}

// serve takes one Codex request: it opens the reply stream and passes what
// is new in the request to Claude.
func (s *Session) serve(st *responses.Stream, req *responses.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastUsed = time.Now()
	if s.stream != nil {
		// Codex dropped the previous stream and retried; the newest request wins.
		slog.Info("replacing an open Codex request", "session", s.id)
		s.stream.Abandon()
	}
	s.stream = st
	s.outputTokens, s.reasoningTokens = 0, 0
	s.setTools(req.Tools)
	for _, note := range s.notes {
		st.Note(note)
	}
	s.notes = nil

	if translate.IsCompaction(req) {
		s.compact(req)
		return
	}
	if err := s.switchEffort(effortOf(req)); err != nil {
		s.fail(failureFor(err))
		return
	}
	if s.stream != st {
		return // Codex left while Claude Code restarted
	}

	messages, outputs := translate.NewInput(req.Input, s.seen, s.strict)
	s.strict = false
	var orphans []responses.InputItem
	resumed := false // a tool result reached Claude
	for _, out := range outputs {
		switch s.deliver(out) {
		case delivered, queued:
			resumed = true
		case orphaned:
			orphans = append(orphans, out)
		}
	}
	if len(messages) > 0 && len(s.pending) > 0 && !resumed {
		s.cancelPending("The user sent a new message instead of a tool result.")
	}

	blocks, command := translate.SplitCommand(messages, s.commands.has)
	if len(orphans) > 0 {
		blocks = append([]claude.Block{claude.Text(translate.OrphanText(orphans))}, blocks...)
	}
	var err error
	switch {
	case command != "":
		s.held = append(s.held, blocks...)
		err = s.sendCommand(command)
	case len(blocks) > 0:
		err = s.sendUser(append(s.held, blocks...))
		s.held, s.interrupted = nil, false
	case resumed || s.running:
		// Claude continues with the tool results, or is still on this turn.
	case s.interrupted:
		s.interrupted = false
		err = s.sendUser([]claude.Block{claude.Text(resumeNudge)})
	default:
		s.fail(responses.Failure{Code: responses.CodeInvalidPrompt,
			Message: "Claude bridge: this request had nothing new for Claude (no new message or tool result)."})
	}
	if err != nil {
		s.fail(failureFor(err))
	}
}

// compact answers a Codex compaction with Claude Code's own /compact. Claude
// keeps its context; Codex's summary of it is never sent.
func (s *Session) compact(req *responses.Request) {
	translate.NewInput(req.Input, s.seen, false) // what Codex replays counts as seen
	if s.running {
		s.stream.Delta("Claude Code manages this thread's context itself, so Codex's summary is not used for Claude.")
		s.complete(s.lastCtx)
		s.strict = true
		return
	}
	s.compaction = &compaction{stream: s.stream}
	if err := s.sendCommand("/compact"); err != nil {
		s.compaction = nil
		s.fail(failureFor(err))
	}
}

// switchEffort restarts Claude Code with another effort level between turns.
// It releases mu while the old process exits.
func (s *Session) switchEffort(effort string) error {
	if effort == s.effort || s.running || len(s.pending) > 0 {
		return nil
	}
	old := s.proc
	if old == nil {
		return errNotRunning
	}
	s.proc = nil
	s.mu.Unlock()
	old.Terminate(3 * time.Second)
	s.mu.Lock()
	s.effort = effort
	return s.start()
}

// disconnected handles Codex closing a stream early: the user pressed Stop.
func (s *Session) disconnected(st *responses.Stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != st {
		return
	}
	st.Abandon()
	s.stream = nil
	if s.running {
		s.interrupt("stopped in Codex")
	}
}

// sendCommand sends a slash command, which Claude Code recognises as a
// plain-string user message.
func (s *Session) sendCommand(command string) error {
	slog.Info("claude command", "session", s.id, "command", strings.Fields(command)[0])
	return s.sendUser(command)
}

func (s *Session) sendUser(content any) error {
	if s.proc == nil {
		return errNotRunning
	}
	if err := s.proc.SendUser(content); err != nil {
		return fmt.Errorf("%w: %w", errNotRunning, err)
	}
	s.running = true
	s.lastEvent = time.Now()
	s.save()
	return nil
}

func (s *Session) handle(ev claude.Event) {
	s.lastEvent = time.Now()
	switch ev.Type {
	case "system":
		switch ev.Subtype {
		case "init":
			if err := s.commands.learn(ev.SlashCommands); err != nil {
				slog.Error("save slash commands", "err", err)
			}
		case "compact_boundary":
			s.compacted(ev.CompactMetadata)
		}
	case "rate_limit_event":
		s.rateLimit = ev.RateLimit
		if l := ev.RateLimit; l != nil {
			slog.Info("claude limit", "session", s.id, "status", l.Status, "window", l.RateLimitType, "overage", l.IsUsingOverage)
		}
	case "stream_event":
		s.streamEvent(ev.Stream)
	case "assistant":
		uses, err := ev.ToolUses()
		if err != nil {
			slog.Error("undecodable assistant event", "session", s.id, "err", err)
			return
		}
		for _, b := range uses {
			s.recordCall(b)
		}
	case "result":
		s.finishTurn(ev)
	}
}

func (s *Session) compacted(m claude.CompactMetadata) {
	if s.compaction != nil {
		s.compaction.meta = m
		return
	}
	slog.Info("claude compacted", "session", s.id, "pre", m.PreTokens, "post", m.PostTokens, "trigger", m.Trigger)
	note := translate.CompactionNote(m)
	if s.stream != nil {
		s.stream.Note(note)
	} else {
		s.notes = append(s.notes, note)
	}
}

func (s *Session) streamEvent(e claude.StreamEvent) {
	switch e.Type {
	case "message_start":
		s.message = &apiMessage{blocks: map[int]string{}}
		clear(s.virtual)
		s.lastCtx = translate.ContextOf(e.Message.Usage)
	case "content_block_start":
		if s.message != nil {
			s.message.blocks[e.Index] = e.ContentBlock.Type
		}
		if e.ContentBlock.Type == "thinking" && s.stream != nil {
			s.stream.StartReasoning()
		}
	case "content_block_delta":
		if s.stream == nil {
			return
		}
		switch e.Delta.Type {
		case "text_delta":
			s.stream.Delta(e.Delta.Text)
		case "thinking_delta":
			s.stream.ReasoningDelta(e.Delta.Thinking)
		}
	case "content_block_stop":
		if s.message != nil && s.message.blocks[e.Index] == "thinking" && s.stream != nil {
			s.stream.FinishReasoning()
		}
	case "message_delta":
		s.outputTokens += e.Usage.OutputTokens
		s.reasoningTokens += e.Usage.OutputTokensDetails.ThinkingTokens
	case "message_stop":
		s.flushCalls()
	}
}

// recordCall notes one tool call from an assistant event.
func (s *Session) recordCall(b claude.ContentBlock) {
	d := s.decide(b.ID, b.Name, b.Input)
	switch d.Kind {
	case translate.KindDisplay:
		delete(s.decisions, b.ID)
		if s.stream != nil {
			s.stream.Add(responses.NewWebSearchCall(d.Action))
		}
	case translate.KindInternal:
		delete(s.decisions, b.ID)
	case translate.KindCodex:
		if s.message == nil {
			slog.Error("tool call outside an API message", "session", s.id, "tool", b.Name)
			return
		}
		s.message.calls = append(s.message.calls, call{b.ID, d})
	}
}

// flushCalls hands the finished message's tool calls to Codex. Claude Code
// emits one assistant event per content block and starts MCP calls as soon as
// each block is complete, so the calls go out together at message_stop: a
// reply with two tool calls sends both.
func (s *Session) flushCalls() {
	msg := s.message
	s.message = nil
	if msg == nil || len(msg.calls) == 0 {
		return
	}
	if s.stream == nil {
		slog.Error("claude tool calls with no open Codex request; dropped", "session", s.id, "calls", len(msg.calls))
		return
	}
	for _, c := range msg.calls {
		s.awaiting[c.id] = true
		s.stream.Add(translate.CallItem(c.id, c.decision))
	}
	s.complete(s.lastCtx)
	s.save()
}

// decide classifies a tool call once; the stream and the MCP call share the
// decision.
func (s *Session) decide(id, name string, input json.RawMessage) translate.Decision {
	if d, ok := s.decisions[id]; ok && id != "" {
		return d
	}
	d := translate.Classify(name, input, s.registry, s.readFile)
	if d.Kind == translate.KindCodex && d.Edit != nil {
		d = s.checkPatch(d)
		s.virtual[d.Edit.Path] = d.Edit.After
	}
	if id != "" {
		s.decisions[id] = d
	}
	return d
}

// checkPatch runs an Edit or Write patch with Codex's patch engine on a
// scratch copy first. If it would not produce exactly the edited file, Codex
// gets a full rewrite instead.
func (s *Session) checkPatch(d translate.Decision) translate.Decision {
	if !d.Edit.Exists {
		return d
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := patch.Check(ctx, s.cfg.CodexBin, d.Input, d.Edit.Path, d.Edit.Before, d.Edit.After); err != nil {
		slog.Error("patch check failed; sending a full rewrite", "session", s.id, "path", d.Edit.Path, "err", err)
		d.Input = patch.Rewrite(d.Edit.Path, d.Edit.After)
	}
	return d
}

func (s *Session) readFile(path string) (string, bool, error) {
	if text, ok := s.virtual[path]; ok {
		return text, true, nil
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return string(data), true, nil
}

func (s *Session) finishTurn(ev claude.Event) {
	if s.stopping {
		return // the turn stays open on disk, so the next bridge resumes it
	}
	s.running = false
	s.save()
	if c := s.compaction; c != nil {
		s.compaction = nil
		if s.stream == c.stream {
			s.stream.Delta(translate.CompactReply(c.meta))
			s.complete(translate.ContextSize{Tokens: c.meta.PostTokens})
		}
		s.strict = true
		return
	}
	if ev.Subtype == "success" && !ev.IsError {
		// Local commands such as /context answer only in the result.
		if s.stream != nil && !s.stream.HasOutput() {
			s.stream.Delta(ev.Result)
		}
		s.complete(s.lastCtx)
		if s.rateLimit != nil && s.rateLimit.Status != "rejected" {
			s.rateLimit = nil
		}
		return
	}
	if strings.HasPrefix(ev.TerminalReason, "aborted") && s.stream == nil {
		return // a stopped turn; its Codex request is already closed
	}
	s.fail(translate.Failure(cmp.Or(ev.Result, strings.Join(ev.Errors, "; "), ev.Subtype), s.rateLimit, false))
}

func (s *Session) complete(c translate.ContextSize) {
	if s.stream == nil {
		return
	}
	s.stream.Complete(translate.Usage(c, s.outputTokens, s.reasoningTokens))
	s.stream = nil
	s.outputTokens, s.reasoningTokens = 0, 0
}

func (s *Session) fail(f responses.Failure) {
	if s.stream == nil {
		return
	}
	slog.Error("turn failed", "session", s.id, "code", f.Code, "message", f.Message)
	s.stream.Fail(f)
	s.stream = nil
	s.outputTokens, s.reasoningTokens = 0, 0
}

func (s *Session) save() {
	st := savedState{Model: s.cfg.Model, UpdatedAt: time.Now().UTC(), TurnOpen: s.running,
		SeenIDs: s.seen.IDs(maxSeen), SeenHashes: s.seen.Hashes(maxSeen), Awaiting: slices.Sorted(maps.Keys(s.awaiting))}
	if err := st.save(s.statePath); err != nil {
		slog.Error("save session state", "session", s.id, "err", err)
	}
}

func (s *Session) interrupt(reason string) {
	slog.Info("interrupting claude", "session", s.id, "reason", reason)
	s.cancelPending(reason)
	clear(s.awaiting)
	if s.proc == nil {
		return
	}
	if err := s.proc.Interrupt(); err != nil {
		slog.Error("interrupt claude", "session", s.id, "err", err)
	}
}

// cancelPending answers every waiting MCP call with an error.
func (s *Session) cancelPending(reason string) {
	for id, c := range s.pending {
		c.result <- callResult{err: reason}
		delete(s.pending, id)
		delete(s.awaiting, id)
		delete(s.decisions, id)
	}
}

type delivery int

const (
	history   delivery = iota // a result Claude already has
	delivered                 // handed to a waiting MCP call
	queued                    // kept until Claude's MCP call arrives
	orphaned                  // Claude stopped waiting: the bridge restarted mid-call
)

// deliver routes one tool result. Codex resends every earlier result on each
// request; only results for calls Claude is still awaiting count.
func (s *Session) deliver(out responses.InputItem) delivery {
	if !s.awaiting[out.CallID] {
		return history
	}
	delete(s.awaiting, out.CallID)
	if c, ok := s.pending[out.CallID]; ok {
		delete(s.pending, out.CallID)
		c.result <- callResult{output: out.Output}
		return delivered
	}
	if s.running {
		s.queued[out.CallID] = out.Output
		return queued
	}
	return orphaned
}

// callTool answers one MCP tools/call from Claude. A Codex tool call waits,
// without a timeout, until Codex returns the result, the user stops the turn
// or Claude hangs up (ctx).
func (s *Session) callTool(ctx context.Context, name string, args json.RawMessage, id string) (translate.MCPResult, error) {
	s.mu.Lock()
	d := s.decide(id, translate.MCPPrefix+name, args)
	switch d.Kind {
	case translate.KindSearch:
		delete(s.decisions, id)
		hits := s.registry.Search(d.Query)
		s.mu.Unlock()
		return translate.JSONResult(hits), nil
	case translate.KindReject:
		delete(s.decisions, id)
		s.mu.Unlock()
		slog.Info("rejected tool call", "session", s.id, "tool", name, "reason", d.Message)
		return translate.ErrorResult(d.Message), nil
	case translate.KindCodex:
	default:
		s.mu.Unlock()
		return translate.MCPResult{}, fmt.Errorf("%s is not a Codex tool", name)
	}
	if id == "" {
		s.mu.Unlock()
		return translate.MCPResult{}, errors.New("tools/call without claudecode/toolUseId")
	}
	if out, ok := s.queued[id]; ok {
		delete(s.queued, id)
		delete(s.decisions, id)
		s.mu.Unlock()
		return toolResult(d, out), nil
	}
	c := &pendingCall{result: make(chan callResult, 1)}
	s.pending[id] = c
	s.mu.Unlock()

	select {
	case r := <-c.result:
		s.mu.Lock()
		delete(s.decisions, id)
		s.mu.Unlock()
		if r.err != "" {
			return translate.ErrorResult(r.err), nil
		}
		return toolResult(d, r.output), nil
	case <-ctx.Done():
		s.mu.Lock()
		if s.pending[id] == c {
			delete(s.pending, id)
		}
		s.mu.Unlock()
		return translate.MCPResult{}, ctx.Err()
	}
}

func toolResult(d translate.Decision, output json.RawMessage) translate.MCPResult {
	var text string
	if d.ReadFrom > 0 && json.Unmarshal(output, &text) == nil {
		return translate.TextResult(translate.NumberRead(text, d.ReadFrom))
	}
	return translate.ToolResult(output)
}

// tools is what Claude sees in tools/list.
func (s *Session) tools() []translate.MCPTool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.registry.Listed
}

// sweep stops a turn Claude has gone silent on, and reports whether the
// session has been idle long enough to close.
func (s *Session) sweep(now time.Time) (idle bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stream != nil && s.running && len(s.pending) == 0 && now.Sub(s.lastEvent) > s.cfg.Watchdog {
		s.interrupt("no output from Claude")
		s.fail(responses.Failure{Code: responses.CodeInvalidPrompt,
			Message: fmt.Sprintf("Claude produced no output for %d minutes, so the bridge stopped the turn.", int(s.cfg.Watchdog.Minutes()))})
	}
	return now.Sub(s.lastUsed) > s.cfg.IdleTimeout && s.stream == nil && !s.running && len(s.pending) == 0
}

func (s *Session) busy() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.running || s.stream != nil || len(s.pending) > 0 || len(s.awaiting) > 0
}

// close ends an idle session's Claude process.
func (s *Session) close() {
	s.mu.Lock()
	s.interrupt("idle")
	p := s.proc
	s.proc = nil
	s.mu.Unlock()
	if p != nil {
		p.Terminate(3 * time.Second)
	}
}

// shutdown ends the session for a bridge restart. The open Codex request
// fails as retryable, and the turn stays open on disk, so after the restart
// Codex retries and Claude continues where it left off. It returns the
// process to wait for.
func (s *Session) shutdown() *claude.Process {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stopping = true
	s.fail(responses.Failure{Code: responses.CodeServerError,
		Message: "The Claude bridge restarted. Codex retries, and Claude continues where it left off."})
	p := s.proc
	if p == nil {
		return nil
	}
	if err := p.Interrupt(); err != nil {
		slog.Error("interrupt claude", "session", s.id, "err", err)
	}
	return p
}
