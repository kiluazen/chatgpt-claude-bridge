// Package config reads the router's settings from an optional JSON file,
// ~/.codex/model-router.json by default. Every setting has a default, so the
// file only needs what differs.
package config

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	// EnvFile holds OPENROUTER_API_KEY=... . It is read on each request, so a
	// new key applies without a restart, and OpenRouter models are listed only
	// while it has one.
	EnvFile string `json:"env_file"`
	// Picker, when set, is the model picker from top to bottom. Every other
	// model stays available but hidden.
	Picker []string `json:"picker"`
	// Hide lists models to leave out of the catalog entirely.
	Hide []string `json:"hide"`
	// MaxOutputTokens caps OpenRouter requests. Codex sends no cap, and
	// OpenRouter checks a request's cost against the model's full output limit.
	MaxOutputTokens int `json:"max_output_tokens"`
}

// Load returns the defaults, overridden by the file at path when it exists.
// Unknown settings are an error, so a typo does not pass silently.
func Load(path string) (Config, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return Config{}, err
	}
	c := Config{EnvFile: "~/.codex/model-router.env", MaxOutputTokens: 16384}
	data, err := os.ReadFile(expand(home, path))
	switch {
	case errors.Is(err, fs.ErrNotExist):
	case err != nil:
		return Config{}, err
	default:
		dec := json.NewDecoder(bytes.NewReader(data))
		dec.DisallowUnknownFields()
		if err := dec.Decode(&c); err != nil {
			return Config{}, fmt.Errorf("%s: %w", path, err)
		}
	}
	if c.MaxOutputTokens <= 0 {
		return Config{}, fmt.Errorf("%s: max_output_tokens must be positive", path)
	}
	c.EnvFile = expand(home, c.EnvFile)
	return c, nil
}

// OpenRouterKey reads OPENROUTER_API_KEY from the env file.
func (c Config) OpenRouterKey() (string, error) {
	f, err := os.Open(c.EnvFile)
	if err != nil {
		return "", fmt.Errorf("OpenRouter key: %w", err)
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if v, ok := strings.CutPrefix(sc.Text(), "OPENROUTER_API_KEY="); ok && strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v), nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("OPENROUTER_API_KEY is missing or empty in " + c.EnvFile)
}

// expand resolves a leading ~/ to home.
func expand(home, path string) string {
	if rest, ok := strings.CutPrefix(path, "~/"); ok {
		return filepath.Join(home, rest)
	}
	return path
}
