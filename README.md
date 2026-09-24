# chatgpt-claude-bridge

Puts Claude Opus in the model picker of Codex, in the ChatGPT desktop app, backed by your local Claude Code login.

Codex sends its Responses API requests for `claude-opus-5-5` to the bridge on `127.0.0.1:41420`. The bridge keeps one Claude Code process per Codex thread, serves Codex's tools to Claude over a loopback MCP endpoint, and streams Claude's thinking, text and tool calls back as native Responses items. Codex still runs every tool, so commands, patches, approvals and sub-agents look the same as with any Codex model.

It drives the official Claude Code CLI under your own login, on your own machine; Anthropic's terms for your plan apply.

Status: early. It runs as a macOS launchd agent, and the Codex side (a model provider and a catalog entry, below) is set up by hand for now.

## Layout

```
cmd/chatgpt-claude-bridge  entry point
internal/bridge            HTTP server, per-thread sessions, MCP endpoint
internal/claude            the Claude Code process and its stream-json events
internal/responses         Codex's side: the Responses request and event stream
internal/translate         pure rules mapping one side to the other
internal/patch             apply_patch documents for Claude's Edit and Write
e2e                        end-to-end tests against the real Claude Code and Codex
```

## Develop

Requires Go 1.25, the ChatGPT desktop app, and Claude Code logged in.

```
make check    # gofmt, vet, unit tests
make e2e      # real Claude Code and Codex on port 41421; uses your Claude plan
make deploy   # check, build to ~/.local/bin, restart the launchd agent when idle
```

## Configuration

Environment variables, with defaults: `CHATGPT_CLAUDE_BRIDGE_PORT` (41420), `CHATGPT_CLAUDE_BRIDGE_CLAUDE` (`~/.local/bin/claude`), `CHATGPT_CLAUDE_BRIDGE_CODEX` (the Codex binary inside the ChatGPT app), `CHATGPT_CLAUDE_BRIDGE_STATE` (`~/.codex/claude-bridge-state`), `CHATGPT_CLAUDE_BRIDGE_WATCHDOG` (`5m`). Logs go to `~/.codex/log/claude-bridge.log`.

On the Codex side, a model provider must route `claude-opus-5-5` to the bridge, and the model's catalog entry needs `use_responses_lite: false`, `supports_search_tool: false` and `auto_compact_token_limit: 950000`.
