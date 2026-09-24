#!/usr/bin/env bash
# Installs the bridge as a launchd agent: checks and builds it, waits until no
# Claude turn is running, then swaps the binary and restarts the agent. Open
# turns would survive a restart (Codex retries, Claude resumes); waiting just
# avoids the interruption.
set -euo pipefail
cd "$(dirname "$0")/.."

label=com.kushalsm.codex-claude-bridge
bin="$HOME/.local/bin/codex-claude-bridge"
plist="$HOME/Library/LaunchAgents/$label.plist"
logs="$HOME/.codex/log"
health=http://127.0.0.1:41420/health

make check
go build -o "$bin.new" ./cmd/codex-claude-bridge

for _ in $(seq 120); do
  busy=$(curl -fsS -m 2 "$health" 2>/dev/null | grep -o '"busy":[0-9]*' | cut -d: -f2 || true)
  [ "${busy:-0}" = 0 ] && break
  echo "waiting: $busy Claude turn(s) running"
  sleep 5
done

mv "$bin.new" "$bin"
mkdir -p "$logs"
cat > "$plist" <<EOF
<?xml version="1.0" encoding="UTF-8"?>
<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">
<plist version="1.0">
<dict>
  <key>Label</key><string>$label</string>
  <key>ProgramArguments</key><array><string>$bin</string></array>
  <key>RunAtLoad</key><true/>
  <key>KeepAlive</key><true/>
  <key>StandardOutPath</key><string>$logs/claude-bridge.log</string>
  <key>StandardErrorPath</key><string>$logs/claude-bridge-error.log</string>
</dict>
</plist>
EOF
launchctl bootout "gui/$(id -u)/$label" 2>/dev/null || true
# bootout returns before launchd drops the job, and bootstrap fails until it has.
for _ in $(seq 50); do
  launchctl print "gui/$(id -u)/$label" >/dev/null 2>&1 || break
  sleep 0.2
done
launchctl bootstrap "gui/$(id -u)" "$plist"

for _ in $(seq 50); do
  if curl -fs -m 1 "$health"; then echo; exit 0; fi
  sleep 0.2
done
echo "the bridge did not come up; see $logs/claude-bridge.log" >&2
exit 1
