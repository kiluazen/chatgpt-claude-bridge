package translate

import (
	"encoding/json"
	"strings"
	"testing"
)

func files(contents map[string]string) FileReader {
	return func(path string) (string, bool, error) {
		text, ok := contents[path]
		return text, ok, nil
	}
}

func classify(t *testing.T, tool, input string, fs FileReader) Decision {
	t.Helper()
	return Classify(MCPPrefix+tool, json.RawMessage(input), registry(t), fs)
}

func TestReadRunsSedOrViewImage(t *testing.T) {
	d := classify(t, "Read", `{"file_path":"/tmp/it's.txt","offset":10,"limit":5}`, files(nil))
	var args struct {
		Cmd string `json:"cmd"`
	}
	if err := json.Unmarshal([]byte(d.Args), &args); err != nil {
		t.Fatal(err)
	}
	if d.Tool.CodexName != "exec_command" || args.Cmd != `sed -n '10,14p' '/tmp/it'\''s.txt'` || d.ReadFrom != 10 {
		t.Fatalf("got %+v %q", d, args.Cmd)
	}
	if d := classify(t, "Read", `{"file_path":"/tmp/x.PNG"}`, files(nil)); d.Tool.CodexName != "view_image" {
		t.Errorf("image read went to %s", d.Tool.CodexName)
	}
	if d := classify(t, "Read", `{"file_path":"relative.txt"}`, files(nil)); d.Kind != KindReject {
		t.Errorf("relative path: %+v", d)
	}
}

func TestNumberRead(t *testing.T) {
	out := "Chunk ID: a\nWall time: 0\nProcess exited with code 0\nOriginal token count: 3\nOutput:\nalpha\nbeta\n"
	if got := NumberRead(out, 10); got != "    10\talpha\n    11\tbeta" {
		t.Fatalf("got %q", got)
	}
	failed := "Chunk ID: a\nProcess exited with code 1\nOutput:\nsed: no such file\n"
	if got := NumberRead(failed, 1); got != failed {
		t.Fatalf("failed read changed: %q", got)
	}
}

func TestEditBecomesMinimalPatch(t *testing.T) {
	fs := files(map[string]string{"/w/f.txt": "a\nb\nc\nd\ne\nf\ng\nh\n", "/w/g.txt": "x\nx\n"})
	d := classify(t, "Edit", `{"file_path":"/w/f.txt","old_string":"d\n","new_string":"D\n"}`, fs)
	want := "*** Begin Patch\n*** Update File: /w/f.txt\n@@\n a\n b\n c\n-d\n+D\n e\n f\n g\n*** End Patch"
	if d.Tool.CodexName != "apply_patch" || d.Input != want {
		t.Fatalf("got %q", d.Input)
	}
	if d := classify(t, "Edit", `{"file_path":"/w/g.txt","old_string":"x","new_string":"y","replace_all":true}`, fs); d.Edit.After != "y\ny\n" {
		t.Errorf("replace_all: %q", d.Edit.After)
	}
	rejects := map[string]string{
		`{"file_path":"/w/f.txt","old_string":"zz","new_string":"y"}`: "not found",
		`{"file_path":"/w/g.txt","old_string":"x","new_string":"y"}`:  "Found 2 matches",
		`{"file_path":"/w/none","old_string":"a","new_string":"b"}`:   "does not exist",
		`{"file_path":"/w/f.txt","old_string":"a","new_string":"a"}`:  "the same",
		`{"file_path":"/w/f.txt","old_string":"","new_string":"new"}`: "empty",
	}
	for input, want := range rejects {
		if d := classify(t, "Edit", input, fs); d.Kind != KindReject || !strings.Contains(d.Message, want) {
			t.Errorf("%s: %+v", input, d)
		}
	}
}

func TestWriteAddsOrUpdates(t *testing.T) {
	fs := files(map[string]string{"/w/old.txt": "one\ntwo\nthree\n", "/w/same.txt": "same\n"})
	if d := classify(t, "Write", `{"file_path":"/w/new.txt","content":"hi\nthere\n"}`, fs); d.Input != "*** Begin Patch\n*** Add File: /w/new.txt\n+hi\n+there\n*** End Patch" {
		t.Errorf("new file: %q", d.Input)
	}
	if d := classify(t, "Write", `{"file_path":"/w/old.txt","content":"one\nTWO\nthree\n"}`, fs); d.Input != "*** Begin Patch\n*** Update File: /w/old.txt\n@@\n one\n-two\n+TWO\n three\n*** End Patch" {
		t.Errorf("existing file: %q", d.Input)
	}
	if d := classify(t, "Write", `{"file_path":"/w/same.txt","content":"same\n"}`, fs); d.Kind != KindReject {
		t.Errorf("unchanged file: %+v", d)
	}
}
