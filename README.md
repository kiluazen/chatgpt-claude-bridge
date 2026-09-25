# chatgpt-claude-bridge

Use Claude and OpenRouter models from the model picker of Codex, in the ChatGPT desktop app, next to GPT. Claude runs on your own Claude Code subscription and OpenRouter models on your own OpenRouter key.

It is two small local services, written in Go:

```
Codex (ChatGPT app) ──► router 127.0.0.1:41419 ──► chatgpt.com       GPT models, with your ChatGPT sign-in
                                                ├──► openrouter.ai     router/models/openrouter/*.json
                                                └──► bridge 127.0.0.1:41420 ──► Claude Code (claude -p)
```

- **router**: the one model provider Codex talks to. It sends each request to the upstream that serves the model and gives Codex the model list, OpenAI's plus yours.
- **bridge**: runs Claude Code headless under your own login, one process per Codex thread. Codex's tools reach Claude over a local MCP endpoint, and Codex still runs every tool, including commands, patches, approvals and sub-agents. Claude's thinking, text and tool calls stream back as native Codex items.

## Status

Early. The author uses it every day, but it depends on Codex's internal provider interface, so a ChatGPT app update can break it.

Tested on macOS (Apple silicon) with ChatGPT app 26.917 (Codex 0.155.0-alpha.16.4), Claude Code 2.1.280 and Go 1.25.

**What works.** End-to-end tests or live runs cover all of this:

- Opus 5.5 in the picker, with thinking, parallel tool calls and Codex's tools: shell, apply_patch, images, and MCP tools and plugins.
- Stopping and steering a turn. A bridge restart continues the interrupted turn.
- Context compaction. Claude Code compacts on its own; Codex never summarizes a Claude thread.
- Claude Code slash commands such as `/compact` and `/context`.
- Sub-agents across models. Any model can spawn, message and wait on sub-agents running any other model, nested too.
- DeepSeek v4.1 Flash and Kimi K3 through OpenRouter.

**Known limits.**

- **macOS only.** It runs under launchd and uses the ChatGPT app's own Codex binary.
- **One Claude model.** The bridge serves `claude-opus-5-5` only.
- **Some GPT features are lost.** Codex turns on a few features only when its provider is OpenAI itself, so GPT threads that go through the router lose server-side compaction and the Fast-mode routing hint.
- **Switching models hands over a summary.** When a thread switches from GPT to Claude, or you open a Claude side chat on a GPT thread, Codex has GPT write a handoff summary first. Claude starts from that summary plus your messages, not the raw thread.
- **GPT's sub-agent tasks are sealed.** OpenAI encrypts the task text a GPT agent writes for a sub-agent, and only OpenAI models can read it. A Claude, DeepSeek or Kimi sub-agent of GPT sees its task only through the forked conversation. `fork_turns` defaults to all.
- **Claude uses Codex's setup, not yours.** Claude runs with Codex's instructions as its system prompt, and without your Claude Code settings: no CLAUDE.md, hooks, plugins or memory.
- **Claude's web tools run on its side.** Claude's own web search and fetch run inside Claude Code; Codex only shows them.
- **Claude turns use your Claude plan.** Plan limits show up in Codex as rate-limit errors.

## Set up

This part is written so your coding agent can follow it.

You need:
- macOS
- the ChatGPT desktop app, signed in
- Claude Code, installed and logged in, so that `claude -p "say hi"` works in a terminal
- Go 1.25 or later
- optionally, an OpenRouter API key

**1. Install both services.**

```bash
git clone https://github.com/kiluazen/chatgpt-claude-bridge
cd chatgpt-claude-bridge
make install
```

This runs the checks, builds both binaries into `~/.local/bin`, and starts them as launchd agents, which also start at login. Check that both answer:

```bash
curl -s http://127.0.0.1:41419/health
curl -s http://127.0.0.1:41420/health
```

**2. Add OpenRouter (optional).** Put your key in `~/.codex/model-router.env`, and give that file mode 600:

```
OPENROUTER_API_KEY=sk-or-...
```

The router reads the file on every request, so a new key takes effect without a restart. DeepSeek and Kimi appear in the picker once a key is there.

**3. Point Codex at the router.** Back up `~/.codex/config.toml`, then add the lines below.

- `model_provider` is a top-level key, so it must come before the first `[table]` in the file.
- Remove any `model_catalog_json` line, because the router serves the model list itself.

```toml
model_provider = "router"

[model_providers.router]
name = "Codex model router"
base_url = "http://127.0.0.1:41419/api/v1"
wire_api = "responses"
requires_openai_auth = true
```

`requires_openai_auth = true` makes Codex send your ChatGPT sign-in to the router. The router forwards it only to chatgpt.com.

**4. Quit and reopen the ChatGPT app.** Opus 5.5 appears in Codex's model picker, along with DeepSeek and Kimi if you added a key.

**5. Verify from a terminal.** `codex exec` waits for stdin to close, so keep the `< /dev/null`.

```bash
/Applications/ChatGPT.app/Contents/Resources/codex exec --skip-git-repo-check -m claude-opus-5-5 'Run `echo ok` and reply with its output.' < /dev/null
```

## Configure the router

Everything is optional. Put what you want to change in `~/.codex/model-router.json`:

```json
{
  "picker": ["gpt-6-sol", "claude-opus-5-5", "deepseek/deepseek-v4.1-flash", "moonshotai/kimi-k3"],
  "hide": ["gpt-5.5"],
  "env_file": "~/.codex/model-router.env",
  "max_output_tokens": 16384
}
```

- `picker`: the picker, top to bottom. Every model not listed stays available but hidden. Without a `picker`, OpenAI's models keep their own order and the added models follow them.
- `hide`: models to leave out entirely.
- `env_file`: the file that holds `OPENROUTER_API_KEY`.
- `max_output_tokens`: a cap on OpenRouter requests. Codex sends no cap, and OpenRouter checks your balance against the model's full output limit, so without a cap a small balance gets 402 errors.

Unknown settings are an error. After changing the file, restart the router with `make install-router`, then reopen the ChatGPT app.

## Add a model

Add one Codex catalog entry per model under `router/models/<upstream>/`. The directory name picks the upstream:

- `openrouter/`: the entry's `slug` is the model's id on openrouter.ai, such as `moonshotai/kimi-k3`. The easiest start is a copy of `kimi-k3.json`. Change `slug`, `display_name`, `description` and the context window fields.
- `bridge/`: Claude. For now this holds only `claude-opus-5-5`, the one model the bridge serves.

Then run `make install-router` and reopen the ChatGPT app.

## What it puts on your machine

- `~/.local/bin/codex-model-router` and `~/.local/bin/chatgpt-claude-bridge`
- `~/Library/LaunchAgents/com.github.kiluazen.codex-model-router.plist` and `com.github.kiluazen.chatgpt-claude-bridge.plist`
- Logs: `~/.codex/log/codex-model-router.log` (one line per request, with model, route, status and duration) and `~/.codex/log/chatgpt-claude-bridge.log`
- `~/.codex/claude-bridge-state/`: each Claude session's id, state and system prompt
- Claude Code's own transcripts of those sessions, under `~/.claude/projects/`

Both services listen on 127.0.0.1 only. The bridge's MCP endpoint path includes a random secret for each session. Your ChatGPT sign-in goes only to chatgpt.com and your OpenRouter key only to openrouter.ai. The bridge gets neither, and it never reads your Claude credentials: it runs the official `claude` CLI, which uses its own login.

## Troubleshooting

- **See what happened:** `tail -f ~/.codex/log/codex-model-router.log ~/.codex/log/chatgpt-claude-bridge.log`
- **"Claude Code could not start":** check that `claude -p hi` works. The bridge runs the `claude` path found at install time, so run `make install-bridge` again after moving or reinstalling Claude Code.
- **Models missing from the picker:** reopen the ChatGPT app and check both health URLs. DeepSeek and Kimi need a key in `~/.codex/model-router.env`.

## Uninstall

```bash
for s in codex-model-router chatgpt-claude-bridge; do
  launchctl bootout "gui/$(id -u)/com.github.kiluazen.$s"
  rm -f ~/Library/LaunchAgents/com.github.kiluazen.$s.plist ~/.local/bin/$s
done
```

Then remove the router provider from `~/.codex/config.toml` and reopen the ChatGPT app.

## Develop

```
router/             the model router
  cmd/              entry point
  internal/         catalog, config, proxy, upstream
  models/           one catalog entry per external model
bridge/             the Claude bridge
  cmd/              entry point
  internal/         bridge (server, sessions), claude, responses, translate, patch, config
  e2e/              end-to-end tests against the real Claude Code and Codex
scripts/install.sh  builds one service and runs it under launchd
```

```bash
make check    # gofmt, vet, and tests with the race detector
make e2e      # the bridge against the real Claude Code and Codex, on port 41421; uses your Claude plan
make install  # check, then build and restart both services once they are idle
```

The two services share no code. The router knows the bridge only as an address, and the bridge knows nothing about routing.

To run a second copy while developing: the router takes `-addr` and `-config` flags. The bridge reads `CHATGPT_CLAUDE_BRIDGE_PORT`, `_CLAUDE`, `_CODEX`, `_STATE` and `_WATCHDOG` from its environment; `install.sh` sets `_CLAUDE` to the `claude` on your PATH.

## Terms

The bridge drives the official Claude Code CLI under your own login, on your own machine. Anthropic's terms for your plan apply, as do OpenAI's and OpenRouter's for theirs.

MIT license.
