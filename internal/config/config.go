// Package config holds the bridge's settings. Defaults match a standard Codex
// and Claude Code install; environment variables override them.
package config

import (
	"fmt"
	"os"
	"path/filepath"
	"time"
)

type Config struct {
	// Addr is where Codex's requests and Claude's MCP calls arrive.
	Addr string
	// Model is the Codex model slug served, and the Claude model run for it.
	Model     string
	ClaudeBin string
	// CodexBin doubles as Codex's patch engine (--codex-run-as-apply-patch).
	CodexBin string
	// StateDir holds each thread's session state and system prompt.
	StateDir string
	// Watchdog stops a turn when Claude is silent this long with no Codex tool running.
	Watchdog time.Duration
	// IdleTimeout closes a thread's Claude process after this long unused.
	IdleTimeout time.Duration
	// Autocompact is Claude's compaction window. It sits below Codex's 950k
	// threshold for the model (auto_compact_token_limit in the catalog), so
	// Claude always compacts first and Codex never compacts Claude threads.
	Autocompact string
}

// FromEnv returns the configuration, with overrides from
// CODEX_CLAUDE_BRIDGE_{PORT,CLAUDE,CODEX,STATE,WATCHDOG}.
func FromEnv() (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{
		Addr:        "127.0.0.1:" + env("CODEX_CLAUDE_BRIDGE_PORT", "41420"),
		Model:       "claude-opus-5-5",
		ClaudeBin:   env("CODEX_CLAUDE_BRIDGE_CLAUDE", filepath.Join(home, ".local/bin/claude")),
		CodexBin:    env("CODEX_CLAUDE_BRIDGE_CODEX", "/Applications/ChatGPT.app/Contents/Resources/codex"),
		StateDir:    env("CODEX_CLAUDE_BRIDGE_STATE", filepath.Join(home, ".codex/claude-bridge-state")),
		Watchdog:    5 * time.Minute,
		IdleTimeout: 20 * time.Minute,
		Autocompact: "800k",
	}
	if v := os.Getenv("CODEX_CLAUDE_BRIDGE_WATCHDOG"); v != "" {
		if c.Watchdog, err = time.ParseDuration(v); err != nil {
			return Config{}, fmt.Errorf("CODEX_CLAUDE_BRIDGE_WATCHDOG: %w", err)
		}
	}
	return c, nil
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
