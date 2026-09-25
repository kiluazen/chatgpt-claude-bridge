package bridge

import (
	"os"
	"path/filepath"
	"testing"
)

// Session ids and state files must match what earlier bridges wrote, so a
// thread keeps its Claude session across bridge upgrades.
func TestSessionIDIsStable(t *testing.T) {
	if got := sessionID("critique-probe-model-switch-0923"); got != "fe83623a-1524-4282-8c60-dde1cf826cc7" {
		t.Fatalf("got %s", got)
	}
}

func TestLoadStateReadsEarlierFormat(t *testing.T) {
	path := filepath.Join(t.TempDir(), "s.json")
	old := `{"model":"claude-opus-5-5","updated_at":"2026-09-23T17:53:43.302Z","turnOpen":true,"seenIds":["m1"],"seenHashes":["h1"],"awaiting":["toolu_1"]}`
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	st, exists, err := loadState(path)
	if err != nil || !exists || !st.TurnOpen || st.SeenIDs[0] != "m1" || st.Awaiting[0] != "toolu_1" {
		t.Fatalf("got %+v %v %v", st, exists, err)
	}
	if _, exists, err := loadState(filepath.Join(t.TempDir(), "missing.json")); exists || err != nil {
		t.Fatalf("missing file: %v %v", exists, err)
	}
}

func TestCommandsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "commands.json")
	c, err := loadCommands(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.learn([]string{"design", "compact"}); err != nil {
		t.Fatal(err)
	}
	again, err := loadCommands(path)
	if err != nil || !again.has("design") || again.has("nope") || again.count() != 2 {
		t.Fatalf("reloaded %+v %v", again, err)
	}
}
