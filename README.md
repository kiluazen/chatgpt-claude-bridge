# codex-claude-bridge

Puts Claude Opus in the Codex desktop app's model picker, backed by your local Claude Code login.

Codex sends its Responses API requests for `claude-opus-5-5` to the bridge on `127.0.0.1:41420`. The bridge keeps one Claude Code process per Codex thread, serves Codex's tools to Claude over a loopback MCP endpoint, and streams Claude's thinking, text and tool calls back as native Responses items. Codex still runs every tool, so commands, patches, approvals and sub-agents look the same as with any Codex model.

## Layout

```
cmd/codex-claude-bridge   entry point
internal/bridge           HTTP server, per-thread sessions, MCP endpoint
internal/claude           the Claude Code process and its stream-json events
internal/responses        Codex's side: the Responses request and event stream
internal/translate        pure rules mapping one side to the other
internal/patch            apply_patch documents for Claude's Edit and Write
e2e                       end-to-end tests against the real Claude Code and Codex
```

## Develop

```
make check    # gofmt, vet, unit tests
make e2e      # real Claude Code and Codex on port 41421; uses your Claude plan
make deploy   # check, build to ~/.local/bin, restart the launchd agent when idle
```

## Configuration

Environment variables, with defaults: `CODEX_CLAUDE_BRIDGE_PORT` (41420), `CODEX_CLAUDE_BRIDGE_CLAUDE` (`~/.local/bin/claude`), `CODEX_CLAUDE_BRIDGE_CODEX` (the Codex app's binary), `CODEX_CLAUDE_BRIDGE_STATE` (`~/.codex/claude-bridge-state`), `CODEX_CLAUDE_BRIDGE_WATCHDOG` (`5m`). Logs go to `~/.codex/log/claude-bridge.log`.

On the Codex side, the model provider must route `claude-opus-5-5` here, and its catalog entry needs `use_responses_lite: false`, `supports_search_tool: false` and `auto_compact_token_limit: 950000`.
