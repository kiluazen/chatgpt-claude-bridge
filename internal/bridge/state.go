package bridge

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"os"
	"slices"
	"sync"
	"time"
)

// savedState is a session's state on disk, so a restarted bridge resumes each
// thread where it was.
type savedState struct {
	Model      string    `json:"model"`
	UpdatedAt  time.Time `json:"updated_at"`
	TurnOpen   bool      `json:"turnOpen"`
	SeenIDs    []string  `json:"seenIds"`
	SeenHashes []string  `json:"seenHashes"`
	Awaiting   []string  `json:"awaiting"`
}

// loadState reads a session's state; exists is false for a new session.
func loadState(path string) (st savedState, exists bool, err error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return savedState{}, false, nil
	}
	if err != nil {
		return savedState{}, false, err
	}
	if err := json.Unmarshal(data, &st); err != nil {
		return savedState{}, false, fmt.Errorf("%s: %w", path, err)
	}
	return st, true, nil
}

func (st savedState) save(path string) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// commands is the set of slash commands Claude Code reports at startup. It is
// saved, so the first message of a new thread can already be a command.
type commands struct {
	path  string
	mu    sync.RWMutex
	known map[string]bool
}

func loadCommands(path string) (*commands, error) {
	c := &commands{path: path, known: map[string]bool{}}
	data, err := os.ReadFile(path)
	if errors.Is(err, fs.ErrNotExist) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	var names []string
	if err := json.Unmarshal(data, &names); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	for _, n := range names {
		c.known[n] = true
	}
	return c, nil
}

func (c *commands) has(name string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.known[name]
}

func (c *commands) count() int {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return len(c.known)
}

// learn records the commands from a Claude Code init event.
func (c *commands) learn(names []string) error {
	if len(names) == 0 {
		return nil
	}
	next := map[string]bool{}
	for _, n := range names {
		next[n] = true
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if maps.Equal(next, c.known) {
		return nil
	}
	c.known = next
	data, err := json.Marshal(slices.Sorted(maps.Keys(next)))
	if err != nil {
		return err
	}
	return os.WriteFile(c.path, data, 0o600)
}
