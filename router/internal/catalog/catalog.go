// Package catalog builds the model catalog Codex shows in its picker: OpenAI's
// native catalog plus the models the router serves through other upstreams.
package catalog

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"path"
	"slices"
	"strconv"
)

// Model is a model served outside OpenAI.
type Model struct {
	Slug     string
	Upstream string // the directory its entry sits in, such as "openrouter"
	entry    entry
}

// entry is one model in a Codex catalog.
type entry map[string]json.RawMessage

func (e entry) slug() string {
	var s string
	json.Unmarshal(e["slug"], &s)
	return s
}

// Load reads one Codex catalog entry per model from fsys, laid out as
// <upstream>/<name>.json.
func Load(fsys fs.FS) ([]Model, error) {
	files, err := fs.Glob(fsys, "*/*.json")
	if err != nil {
		return nil, err
	}
	var out []Model
	for _, p := range files {
		data, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		var e entry
		if err := json.Unmarshal(data, &e); err != nil {
			return nil, fmt.Errorf("%s: %w", p, err)
		}
		if e.slug() == "" {
			return nil, fmt.Errorf("%s: no slug", p)
		}
		out = append(out, Model{Slug: e.slug(), Upstream: path.Dir(p), entry: e})
	}
	return out, nil
}

// Picker shapes the model picker.
type Picker struct {
	Order []string // top to bottom; when set, every other model is hidden
	Hide  []string // left out of the catalog entirely
}

// Merge adds the external models to a native catalog ({"models": [...]}) and
// applies the picker. An external model replaces a native entry with its
// slug, so merging twice changes nothing. Without an order, every model keeps
// the priority and visibility its entry gives it.
func Merge(native []byte, external []Model, p Picker) ([]byte, error) {
	var cat map[string]json.RawMessage
	var entries []entry
	if err := json.Unmarshal(native, &cat); err != nil {
		return nil, fmt.Errorf("model catalog: %w", err)
	}
	if err := json.Unmarshal(cat["models"], &entries); err != nil || entries == nil {
		return nil, errors.New("model catalog has no models list")
	}

	ours := map[string]bool{}
	for _, m := range external {
		ours[m.Slug] = true
	}
	var slugs []string // first-seen order
	bySlug := map[string]entry{}
	add := func(e entry) {
		s := e.slug()
		if slices.Contains(p.Hide, s) {
			return
		}
		if _, seen := bySlug[s]; !seen {
			slugs = append(slugs, s)
		}
		bySlug[s] = maps.Clone(e)
	}
	for _, e := range entries {
		if !ours[e.slug()] {
			add(e)
		}
	}
	for _, m := range external {
		add(m.entry)
	}

	merged := make([]entry, 0, len(slugs))
	for i, s := range p.Order {
		if e, ok := bySlug[s]; ok {
			e["priority"] = json.RawMessage(strconv.Itoa(i))
			merged = append(merged, e)
			delete(bySlug, s)
		}
	}
	for _, s := range slugs {
		if e, ok := bySlug[s]; ok {
			if len(p.Order) > 0 {
				e["visibility"] = json.RawMessage(`"hide"`)
			}
			merged = append(merged, e)
		}
	}
	cat["models"] = encode(merged)
	return encode(cat), nil
}

// encode marshals values that always encode, keeping < > & as they are.
func encode(v any) json.RawMessage {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		panic(err)
	}
	return bytes.TrimSuffix(b.Bytes(), []byte("\n"))
}
