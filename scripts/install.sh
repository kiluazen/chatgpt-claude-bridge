#!/usr/bin/env bash
# Installs one service as a macOS launchd agent: builds it to ~/.local/bin,
# waits until it has nothing in flight, swaps the binary and restarts it.
#
#   scripts/install.sh bridge   the Claude bridge on 127.0.0.1:41420
#   scripts/install.sh router   the model router on 127.0.0.1:41419
set -euo pipefail
cd "$(dirname "$0")/.."

codex=/Applications/ChatGPT.app/Contents/Resources/codex
env="" after=:
case "${1:-}" in
bridge)
  name=chatgpt-claude-bridge port=41420 busy=busy
  claude=$(command -v claude) || { echo "Claude Code (claude) is not on PATH; install it and log in first" >&2; exit 1; }
  [ -x "$codex" ] || { echo "the ChatGPT desktop app is not at /Applications/ChatGPT.app" >&2; exit 1; }
  # launchd starts agents with a bare PATH, so the agent gets claude's full path.
  env="<key>EnvironmentVariables</key><dict><key>CHATGPT_CLAUDE_BRIDGE_CLAUDE</key><string>$claude</string></dict>"
  ;;
router)
  name=codex-model-router port=41419 busy=in_flight
  # Codex keeps its model catalog for as long as OpenAI's catalog is
  # unchanged, so it would not see a new picker. Without the cache, the
  # reopened app fetches the catalog again.
  after="rm -f ${CODEX_HOME:-$HOME/.codex}/models_cache.json"
  ;;
*)
  echo "usage: $0 bridge|router" >&2
  exit 2
  ;;
esac

label=com.github.kiluazen.$name
bin="$HOME/.local/bin/$name"
plist="$HOME/Library/LaunchAgents/$label.plist"
logs="$HOME/.codex/log"
health="http://127.0.0.1:$port/health"

mkdir -p "$(dirname "$bin")" "$logs" "$(dirname "$plist")"
go build -o "$bin.new" "./$1/cmd/$name"

for _ in $(seq 120); do
  n=$(curl -fsS -m 2 "$health" 2>/dev/null | grep -o "\"$busy\":[0-9]*" | cut -d: -f2 || true)
  [ "${n:-0}" = 0 ] && break
  echo "$name: waiting for $n request(s) in flight"
  sleep 5
done

mv "$bin.new" "$bin"
cat > "$plist" <<PLIST
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$bin</string></array>
  $env
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>ExitTimeOut</key><integer>330</integer>
  <key>StandardOutPath</key><string>$logs/$name.log</string>
  <key>StandardErrorPath</key><string>$logs/$name-error.log</string>
</dict>
</plist>
PLIST
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
# bootout returns before launchd drops the job, and bootstrap fails until it
# has; a stopping service may still be finishing a stream.
for _ in $(seq 1650); do
  launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break
  sleep 0.2
done
launchctl bootstrap "gui/$(id -u)" "$plist"

for _ in $(seq 50); do
  if curl -fs -m 1 "$health"; then echo; $after; exit 0; fi
  sleep 0.2
done
echo "$name did not come up; see $logs/$name.log" >&2
exit 1
