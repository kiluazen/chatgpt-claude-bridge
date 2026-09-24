// Package patch writes documents in apply_patch format, which Codex's patch
// engine applies and draws as native file changes.
package patch

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
)

// Lines splits text into lines, without the empty entry a final newline leaves.
func Lines(text string) []string {
	lines := strings.Split(text, "\n")
	if lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	return lines
}

// Op is one line of a diff. Kind is ' ' for kept, '-' for removed and '+'
// for added lines.
type Op struct {
	Kind byte
	Line string
}

// maxDiffCells bounds the LCS table; larger changed regions are diffed as one
// replaced block.
const maxDiffCells = 4_000_000

// Diff compares two line lists.
func Diff(a, b []string) []Op {
	head := 0
	for head < len(a) && head < len(b) && a[head] == b[head] {
		head++
	}
	tail := 0
	for tail < len(a)-head && tail < len(b)-head && a[len(a)-1-tail] == b[len(b)-1-tail] {
		tail++
	}
	midA, midB := a[head:len(a)-tail], b[head:len(b)-tail]
	ops := make([]Op, 0, len(a)+len(b))
	for _, line := range a[:head] {
		ops = append(ops, Op{' ', line})
	}
	if len(midA)*len(midB) <= maxDiffCells {
		ops = append(ops, lcs(midA, midB)...)
	} else {
		for _, line := range midA {
			ops = append(ops, Op{'-', line})
		}
		for _, line := range midB {
			ops = append(ops, Op{'+', line})
		}
	}
	for _, line := range a[len(a)-tail:] {
		ops = append(ops, Op{' ', line})
	}
	return ops
}

func lcs(a, b []string) []Op {
	n, m, w := len(a), len(b), len(b)+1
	table := make([]uint32, (n+1)*w)
	for i := n - 1; i >= 0; i-- {
		for j := m - 1; j >= 0; j-- {
			if a[i] == b[j] {
				table[i*w+j] = table[(i+1)*w+j+1] + 1
			} else {
				table[i*w+j] = max(table[(i+1)*w+j], table[i*w+j+1])
			}
		}
	}
	ops := make([]Op, 0, n+m)
	for i, j := 0, 0; i < n || j < m; {
		switch {
		case i < n && j < m && a[i] == b[j]:
			ops = append(ops, Op{' ', a[i]})
			i, j = i+1, j+1
		case i < n && (j >= m || table[(i+1)*w+j] >= table[i*w+j+1]):
			ops = append(ops, Op{'-', a[i]})
			i++
		default:
			ops = append(ops, Op{'+', b[j]})
			j++
		}
	}
	return ops
}

// Update returns an Update File document that turns before into after, with
// three lines of context around each change. It reports false when the two
// are the same.
func Update(path, before, after string) (string, bool) {
	ops := Diff(Lines(before), Lines(after))
	var hunks [][2]int // first and last changed op of each hunk
	for i, op := range ops {
		if op.Kind == ' ' {
			continue
		}
		if n := len(hunks); n > 0 && i-hunks[n-1][1] <= 6 {
			hunks[n-1][1] = i
		} else {
			hunks = append(hunks, [2]int{i, i})
		}
	}
	if len(hunks) == 0 {
		return "", false
	}
	var b strings.Builder
	b.WriteString("*** Begin Patch\n*** Update File: " + path + "\n")
	for _, h := range hunks {
		b.WriteString("@@\n")
		for _, op := range ops[max(0, h[0]-3):min(len(ops), h[1]+4)] {
			b.WriteByte(op.Kind)
			b.WriteString(op.Line + "\n")
		}
	}
	b.WriteString("*** End Patch")
	return b.String(), true
}

// Add returns an Add File document that creates path with content.
func Add(path, content string) string {
	return "*** Begin Patch\n*** Add File: " + path + "\n" + added(content) + "*** End Patch"
}

// Rewrite returns a document that replaces path's whole content.
func Rewrite(path, content string) string {
	return "*** Begin Patch\n*** Delete File: " + path + "\n*** Add File: " + path + "\n" + added(content) + "*** End Patch"
}

func added(content string) string {
	var b strings.Builder
	for _, line := range Lines(content) {
		b.WriteString("+" + line + "\n")
	}
	return b.String()
}

// Check applies an Update File document with Codex's patch engine to a
// scratch copy of the file and returns an error unless the result is
// exactly after.
func Check(ctx context.Context, codexBin, doc, path, before, after string) error {
	dir, err := os.MkdirTemp("", "codex-claude-patch-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(dir)
	scratch := filepath.Join(dir, "f")
	if err := os.WriteFile(scratch, []byte(before), 0o600); err != nil {
		return err
	}
	local := strings.Replace(doc, "*** Update File: "+path+"\n", "*** Update File: f\n", 1)
	cmd := exec.CommandContext(ctx, codexBin, "--codex-run-as-apply-patch", local)
	cmd.Dir = dir
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("apply_patch: %w: %s", err, bytes.TrimSpace(out))
	}
	got, err := os.ReadFile(scratch)
	if err != nil {
		return err
	}
	if strings.TrimSuffix(string(got), "\n") != strings.TrimSuffix(after, "\n") {
		return errors.New("the patch does not reproduce the edit")
	}
	return nil
}
