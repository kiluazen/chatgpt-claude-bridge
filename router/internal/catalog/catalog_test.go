package catalog

import (
	"encoding/json"
	"reflect"
	"testing"
	"testing/fstest"

	"github.com/kiluazen/chatgpt-claude-bridge/router/models"
)

func model(slug, displayName string, priority int) Model {
	return Model{Slug: slug, entry: entry{"slug": encode(slug), "display_name": encode(displayName), "priority": encode(priority)}}
}

var (
	opus     = model("claude-opus-5-5", "Opus 5.5", 100)
	deepseek = model("deepseek/deepseek-v4.1-flash", "DeepSeek V4.1 Flash", 101)
	kimi     = model("moonshotai/kimi-k3", "Kimi K3", 102)
)

const native = `{"models":[
	{"slug":"gpt-6-astra","priority":1,"visibility":"list"},
	{"slug":"gpt-6-sol","priority":2,"visibility":"list"},
	{"slug":"gpt-6-luna","priority":3,"visibility":"list"},
	{"slug":"gpt-5.5","priority":12,"visibility":"list"},
	{"slug":"codex-auto-review","priority":43,"visibility":"hide"}]}`

type listing []struct {
	Slug        string `json:"slug"`
	DisplayName string `json:"display_name"`
	Priority    int    `json:"priority"`
	Visibility  string `json:"visibility"`
}

func merge(t *testing.T, native string, external []Model, p Picker) listing {
	t.Helper()
	out, err := Merge([]byte(native), external, p)
	if err != nil {
		t.Fatal(err)
	}
	var cat struct{ Models listing }
	if err := json.Unmarshal(out, &cat); err != nil {
		t.Fatal(err)
	}
	return cat.Models
}

func (l listing) slugs() []string {
	var out []string
	for _, e := range l {
		out = append(out, e.Slug)
	}
	return out
}

func TestMergeKeepsEntriesAsTheyAre(t *testing.T) {
	got := merge(t, native, []Model{opus, deepseek}, Picker{Hide: []string{"gpt-5.5"}})
	want := []string{"gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "codex-auto-review", opus.Slug, deepseek.Slug}
	if !reflect.DeepEqual(got.slugs(), want) {
		t.Fatalf("slugs %v", got.slugs())
	}
	if got[1].Priority != 2 || got[1].Visibility != "list" || got[3].Visibility != "hide" || got[4].Priority != 100 {
		t.Fatalf("entries changed: %+v", got)
	}
}

func TestMergeAppliesAPickerOrder(t *testing.T) {
	got := merge(t, native, []Model{opus, deepseek, kimi},
		Picker{Order: []string{"gpt-6-sol", opus.Slug, deepseek.Slug, kimi.Slug, "gpt-6-astra"}, Hide: []string{"gpt-6-luna", "gpt-5.5"}})
	want := []string{"gpt-6-sol", opus.Slug, deepseek.Slug, kimi.Slug, "gpt-6-astra", "codex-auto-review"}
	if !reflect.DeepEqual(got.slugs(), want) {
		t.Fatalf("picker %v", got.slugs())
	}
	for i, e := range got[:5] {
		if e.Priority != i || e.Visibility == "hide" {
			t.Errorf("%s: priority %d, visibility %q", e.Slug, e.Priority, e.Visibility)
		}
	}
	if got[5].Visibility != "hide" {
		t.Errorf("a model outside the picker is shown: %+v", got[5])
	}
}

func TestMergeIsIdempotent(t *testing.T) {
	stale := `{"models":[{"slug":"gpt-6-sol"},{"slug":"deepseek/deepseek-v4.1-flash","display_name":"stale"}]}`
	p := Picker{Order: []string{"gpt-6-sol", deepseek.Slug}}
	once, err := Merge([]byte(stale), []Model{deepseek}, p)
	if err != nil {
		t.Fatal(err)
	}
	twice, err := Merge(once, []Model{deepseek}, p)
	if err != nil {
		t.Fatal(err)
	}
	if string(once) != string(twice) {
		t.Fatalf("second merge changed the catalog:\n%s\n%s", once, twice)
	}
	if got := merge(t, string(twice), nil, Picker{}); got[1].DisplayName != "DeepSeek V4.1 Flash" {
		t.Fatalf("stale entry kept: %+v", got)
	}
}

func TestMergeKeepsOtherCatalogFields(t *testing.T) {
	out, err := Merge([]byte(`{"etag":"abc","models":[{"slug":"gpt-6-sol"}]}`), nil, Picker{})
	if err != nil {
		t.Fatal(err)
	}
	var cat map[string]json.RawMessage
	json.Unmarshal(out, &cat)
	if string(cat["etag"]) != `"abc"` {
		t.Fatalf("catalog %s", out)
	}
	if _, err := Merge([]byte(`{"data":[]}`), nil, Picker{}); err == nil {
		t.Fatal("a catalog without models merged")
	}
}

func TestLoad(t *testing.T) {
	ms, err := Load(fstest.MapFS{
		"openrouter/a.json": {Data: []byte(`{"slug":"vendor/a"}`)},
		"bridge/b.json":     {Data: []byte(`{"slug":"claude-b"}`)},
		"README":            {Data: []byte(`not a model`)},
	})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range ms {
		got[m.Slug] = m.Upstream
	}
	if !reflect.DeepEqual(got, map[string]string{"vendor/a": "openrouter", "claude-b": "bridge"}) {
		t.Fatalf("loaded %v", got)
	}
	if _, err := Load(fstest.MapFS{"bridge/x.json": {Data: []byte(`{"display_name":"no slug"}`)}}); err == nil {
		t.Fatal("an entry without a slug loaded")
	}
}

func TestShippedModels(t *testing.T) {
	ms, err := Load(models.FS)
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, m := range ms {
		got[m.Slug] = m.Upstream
	}
	want := map[string]string{"claude-opus-5-5": "bridge", "deepseek/deepseek-v4.1-flash": "openrouter", "moonshotai/kimi-k3": "openrouter"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("shipped models %v", got)
	}
}
