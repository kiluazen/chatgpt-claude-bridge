// Package bridge runs Claude Code behind Codex's model-provider interface.
// Each Codex thread gets one warm Claude Code process. Codex's tools reach
// Claude through a loopback MCP endpoint and still run in Codex, and every
// step Claude takes streams back to Codex as a native Responses item.
package bridge

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"github.com/kiluazen/codex-claude-bridge/internal/config"
	"github.com/kiluazen/codex-claude-bridge/internal/responses"
)

const maxBodyBytes = 32 << 20

// Server is the bridge's HTTP handler: POST /api/v1/responses for Codex,
// POST /mcp/{session}/{secret} for Claude, and GET /health.
type Server struct {
	cfg      config.Config
	sessions *sessions
	commands *commands
	mux      *http.ServeMux
}

func New(cfg config.Config) (*Server, error) {
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		return nil, fmt.Errorf("state dir: %w", err)
	}
	cmds, err := loadCommands(filepath.Join(cfg.StateDir, "claude-slash-commands.json"))
	if err != nil {
		return nil, fmt.Errorf("slash commands: %w", err)
	}
	s := &Server{cfg: cfg, commands: cmds, mux: http.NewServeMux(),
		sessions: &sessions{cfg: cfg, commands: cmds, byThread: map[string]*Session{}}}
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("POST /api/v1/responses", s.responses)
	s.mux.HandleFunc("POST /mcp/{session}/{secret}", s.mcp)
	return s, nil
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) { s.mux.ServeHTTP(w, r) }

// Run sweeps sessions until ctx ends: it stops turns Claude has gone silent
// on and closes threads that sat idle.
func (s *Server) Run(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.sessions.sweep(now)
		}
	}
}

// Shutdown ends every session so that, after a restart, Codex retries and
// Claude continues where it left off.
func (s *Server) Shutdown() { s.sessions.shutdown() }

func (s *Server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "model": s.cfg.Model, "sessions": len(s.sessions.list()), "busy": s.sessions.busy(),
		"claude_cli": s.cfg.ClaudeBin, "commands": s.commands.count(),
	})
}

// responses serves one Codex request until its reply stream ends or Codex
// hangs up.
func (s *Server) responses(w http.ResponseWriter, r *http.Request) {
	var req responses.Request
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid request: "+err.Error()))
		return
	}
	switch {
	case req.Model != s.cfg.Model:
		writeJSON(w, http.StatusBadRequest, errorBody("unsupported model "+req.Model))
		return
	case req.ThreadID() == "":
		writeJSON(w, http.StatusBadRequest, errorBody("client_metadata.thread_id is required"))
		return
	}
	sess, err := s.sessions.get(&req)
	st := responses.NewStream(w, req.Model)
	if err != nil {
		slog.Error("start claude code", "thread", req.ThreadID(), "err", err)
		st.Fail(responses.Failure{Code: responses.CodeInvalidPrompt, Message: "Claude Code could not start: " + err.Error()})
		return
	}
	sess.serve(st, &req)
	select {
	case <-st.Finished():
	case <-r.Context().Done():
		sess.disconnected(st)
	}
	if err := st.Err(); err != nil {
		slog.Info("codex stream ended early", "session", sess.id, "err", err)
	}
}

type rpcRequest struct {
	ID     json.RawMessage `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

// mcp serves Claude's MCP requests: the tool list and tool calls.
func (s *Server) mcp(w http.ResponseWriter, r *http.Request) {
	sess := s.sessions.byMCP(r.PathValue("session"), r.PathValue("secret"))
	if sess == nil {
		writeJSON(w, http.StatusNotFound, errorBody("unknown MCP session"))
		return
	}
	var req rpcRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorBody("invalid JSON-RPC request: "+err.Error()))
		return
	}
	if len(req.ID) == 0 {
		w.WriteHeader(http.StatusAccepted) // a notification
		return
	}
	switch req.Method {
	case "initialize":
		var p struct {
			ProtocolVersion string `json:"protocolVersion"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPCError(w, req.ID, -32602, err.Error())
			return
		}
		writeRPC(w, req.ID, map[string]any{
			"protocolVersion": cmp.Or(p.ProtocolVersion, "2025-06-18"),
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]string{"name": "codex", "version": "2"},
		})
	case "tools/list":
		writeRPC(w, req.ID, map[string]any{"tools": sess.tools()})
	case "tools/call":
		var p struct {
			Name      string          `json:"name"`
			Arguments json.RawMessage `json:"arguments"`
			Meta      struct {
				ToolUseID string `json:"claudecode/toolUseId"`
			} `json:"_meta"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			writeRPCError(w, req.ID, -32602, err.Error())
			return
		}
		result, err := sess.callTool(r.Context(), p.Name, p.Arguments, p.Meta.ToolUseID)
		switch {
		case r.Context().Err() != nil:
			// Claude hung up.
		case err != nil:
			writeRPCError(w, req.ID, -32603, err.Error())
		default:
			writeRPC(w, req.ID, result)
		}
	default:
		// Claude Code probes optional methods such as server/discover; an
		// empty result is the compatible answer.
		writeRPC(w, req.ID, struct{}{})
	}
}

func writeRPC(w http.ResponseWriter, id json.RawMessage, result any) {
	w.Header().Set("Mcp-Session-Id", "codex-claude-bridge")
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "result": result})
}

func writeRPCError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	writeJSON(w, http.StatusOK, map[string]any{"jsonrpc": "2.0", "id": id, "error": map[string]any{"code": code, "message": message}})
}

func errorBody(message string) map[string]any {
	return map[string]any{"error": map[string]string{"message": message}}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(v); err != nil {
		slog.Error("write response", "err", err)
	}
}
