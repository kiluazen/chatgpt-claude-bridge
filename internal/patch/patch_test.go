package patch

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const codexBin = "/Applications/ChatGPT.app/Contents/Resources/codex"

func numbered(n int, changed map[int]bool) string {
	var b strings.Builder
	for i := range n {
		if changed[i] {
			b.WriteString("changed line\n")
		} else {
			b.WriteString("line " + string(rune('a'+i%26)) + "\n")
		}
	}
	return b.String()
}

var cases = []struct{ name, before, after string }{
	{"one line", "a\nb\nc\n", "a\nB\nc\n"},
	{"two far edits", numbered(40, nil), numbered(40, map[int]bool{3: true, 30: true})},
	{"append", "x\n", "x\ny\nz\n"},
	{"delete", "keep\ndrop me\nkeep too\n", "keep\nkeep too\n"},
	{"no final newline", "no newline at end", "changed line"},
}

func TestUpdateAppliesWithCodex(t *testing.T) {
	if _, err := os.Stat(codexBin); err != nil {
		t.Skip("Codex is not installed")
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			doc, changed := Update("f", c.before, c.after)
			if !changed {
				t.Fatal("no change detected")
			}
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "f"), []byte(c.before), 0o600); err != nil {
				t.Fatal(err)
			}
			cmd := exec.Command(codexBin, "--codex-run-as-apply-patch", doc)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("%v: %s\n%s", err, out, doc)
			}
			got, _ := os.ReadFile(filepath.Join(dir, "f"))
			if strings.TrimSuffix(string(got), "\n") != strings.TrimSuffix(c.after, "\n") {
				t.Fatalf("got %q", got)
			}
			if err := Check(context.Background(), codexBin, doc, "f", c.before, c.after); err != nil {
				t.Fatalf("Check: %v", err)
			}
		})
	}
}

func TestCheckRejectsAWrongPatch(t *testing.T) {
	if _, err := os.Stat(codexBin); err != nil {
		t.Skip("Codex is not installed")
	}
	doc, _ := Update("/w/f", "a\nb\n", "a\nB\n")
	if err := Check(context.Background(), codexBin, doc, "/w/f", "a\nb\n", "a\nC\n"); err == nil {
		t.Fatal("a patch that produces the wrong file passed")
	}
}

func TestUpdateListsRemovalsBeforeAdditions(t *testing.T) {
	doc, _ := Update("/w/f", "one\ntwo\nthree\n", "one\nTWO\nthree\n")
	if want := "*** Begin Patch\n*** Update File: /w/f\n@@\n one\n-two\n+TWO\n three\n*** End Patch"; doc != want {
		t.Fatalf("got %q", doc)
	}
	if _, changed := Update("/w/f", "same\n", "same\n"); changed {
		t.Fatal("identical files reported as changed")
	}
}

func TestAddAndRewrite(t *testing.T) {
	if got := Add("/w/n", "hi\nthere\n"); got != "*** Begin Patch\n*** Add File: /w/n\n+hi\n+there\n*** End Patch" {
		t.Errorf("Add: %q", got)
	}
	if got := Rewrite("/w/n", "x\n"); got != "*** Begin Patch\n*** Delete File: /w/n\n*** Add File: /w/n\n+x\n*** End Patch" {
		t.Errorf("Rewrite: %q", got)
	}
}
