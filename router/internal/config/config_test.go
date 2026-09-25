package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func write(t *testing.T, name, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestDefaultsWithoutAFile(t *testing.T) {
	c, err := Load(filepath.Join(t.TempDir(), "missing.json"))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	if c.EnvFile != filepath.Join(home, ".codex/model-router.env") || c.MaxOutputTokens != 16384 || c.Picker != nil || c.Hide != nil {
		t.Fatalf("defaults %+v", c)
	}
}

func TestFileOverrides(t *testing.T) {
	c, err := Load(write(t, "c.json", `{"env_file":"~/keys/.env","picker":["gpt-6-sol","claude-opus-5-5"],"hide":["gpt-5.5"],"max_output_tokens":32000}`))
	if err != nil {
		t.Fatal(err)
	}
	home, _ := os.UserHomeDir()
	want := Config{EnvFile: filepath.Join(home, "keys/.env"), Picker: []string{"gpt-6-sol", "claude-opus-5-5"}, Hide: []string{"gpt-5.5"}, MaxOutputTokens: 32000}
	if !reflect.DeepEqual(c, want) {
		t.Fatalf("got %+v", c)
	}
}

func TestRejectsTyposAndBadValues(t *testing.T) {
	for _, body := range []string{`{"pickr":["gpt-6-sol"]}`, `{"max_output_tokens":0}`, `{`} {
		if _, err := Load(write(t, "c.json", body)); err == nil {
			t.Errorf("%s loaded", body)
		}
	}
}

func TestOpenRouterKey(t *testing.T) {
	c := Config{EnvFile: write(t, ".env", "OTHER=1\nOPENROUTER_API_KEY= sk-or-test \n")}
	if key, err := c.OpenRouterKey(); err != nil || key != "sk-or-test" {
		t.Fatalf("key %q, %v", key, err)
	}
	for _, c := range []Config{{EnvFile: write(t, ".env", "OPENROUTER_API_KEY=\n")}, {EnvFile: filepath.Join(t.TempDir(), "none")}} {
		if _, err := c.OpenRouterKey(); err == nil || !strings.Contains(err.Error(), c.EnvFile) {
			t.Errorf("%s: err %v", c.EnvFile, err)
		}
	}
}
