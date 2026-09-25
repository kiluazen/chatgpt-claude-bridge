package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/catalog"
	"github.com/kiluazen/chatgpt-claude-bridge/router/models"
)

// seen is what a fake upstream received.
type seen struct {
	path   string
	header http.Header
	body   map[string]any
}

// fakeUpstream records each request and answers with respond.
func fakeUpstream(t *testing.T, got chan<- seen, respond http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(data, &body)
		got <- seen{r.URL.RequestURI(), r.Header.Clone(), body}
		respond(w, r)
	}))
	t.Cleanup(srv.Close)
	return srv
}

func ok(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, `{}`) }

type harness struct {
	router                     *httptest.Server
	native, openrouter, bridge chan seen
}

// picker is a curated picker, as a config file sets one.
var picker = catalog.Picker{
	Order: []string{"gpt-6-sol", "claude-opus-5-5", "deepseek/deepseek-v4.1-flash", "moonshotai/kimi-k3", "gpt-6-astra"},
	Hide:  []string{"gpt-5.5"},
}

func withKey() (string, error) { return "sk-or-test", nil }

func newHarness(t *testing.T, key func() (string, error), native, openrouter, bridge http.HandlerFunc) harness {
	t.Helper()
	h := harness{native: make(chan seen, 4), openrouter: make(chan seen, 4), bridge: make(chan seen, 4)}
	external, err := catalog.Load(models.FS)
	if err != nil {
		t.Fatal(err)
	}
	at := func(srv *httptest.Server, path string) *url.URL {
		u, _ := url.Parse(srv.URL + path)
		return u
	}
	srv, err := New(Config{
		NativeURL:       at(fakeUpstream(t, h.native, native), "/backend-api/codex"),
		OpenRouterURL:   at(fakeUpstream(t, h.openrouter, openrouter), "/api/v1"),
		BridgeURL:       at(fakeUpstream(t, h.bridge, bridge), "/api/v1"),
		OpenRouterKey:   key,
		MaxOutputTokens: 16384,
		Picker:          picker,
	}, external)
	if err != nil {
		t.Fatal(err)
	}
	h.router = httptest.NewServer(srv)
	t.Cleanup(h.router.Close)
	return h
}

// codexHeaders are the ones only OpenAI may see.
var codexHeaders = map[string]string{
	"Authorization": "Bearer chatgpt-token", "Chatgpt-Account-Id": "acct", "Session_id": "thread",
	"Content-Type": "application/json", "Accept": "text/event-stream",
}

func (h harness) post(t *testing.T, path, body string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, h.router.URL+path, strings.NewReader(body))
	for k, v := range codexHeaders {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func receive(t *testing.T, ch <-chan seen) seen {
	t.Helper()
	select {
	case s := <-ch:
		return s
	case <-time.After(5 * time.Second):
		t.Fatal("upstream got no request")
		return seen{}
	}
}

func TestRoutesByModel(t *testing.T) {
	h := newHarness(t, withKey, ok, ok, ok)

	h.post(t, "/api/v1/responses", `{"model":"gpt-6-sol","input":[{"type":"agent_message","content":[{"type":"encrypted_content","encrypted_content":"From Claude"}]}]}`)
	native := receive(t, h.native)
	if native.path != "/backend-api/codex/responses" || native.header.Get("Authorization") != "Bearer chatgpt-token" ||
		native.header.Get("Chatgpt-Account-Id") != "acct" || !strings.Contains(mustJSON(native.body), `{"text":"From Claude","type":"input_text"}`) {
		t.Errorf("native got %+v", native)
	}

	for _, model := range []string{"deepseek/deepseek-v4.1-flash", "moonshotai/kimi-k3"} {
		h.post(t, "/api/v1/responses", `{"model":"`+model+`","max_output_tokens":50000,"input":[]}`)
		or := receive(t, h.openrouter)
		if or.path != "/api/v1/responses" || or.header.Get("Authorization") != "Bearer sk-or-test" ||
			or.header.Get("Chatgpt-Account-Id") != "" || or.header.Get("Session_id") != "" || or.body["max_output_tokens"] != 16384.0 {
			t.Errorf("%s: openrouter got %+v", model, or)
		}
	}

	h.post(t, "/api/v1/responses", `{"model":"claude-opus-5-5","input":[]}`)
	bridge := receive(t, h.bridge)
	wantHeaders := http.Header{"Content-Type": {"application/json"}, "Accept": {"text/event-stream"}}
	for k := range bridge.header {
		if wantHeaders.Get(k) == "" && k != "Content-Length" && k != "Accept-Encoding" {
			t.Errorf("bridge got header %s", k)
		}
	}
	if bridge.path != "/api/v1/responses" || bridge.body["model"] != "claude-opus-5-5" {
		t.Errorf("bridge got %+v", bridge)
	}
}

// catalogOf fetches the model catalog Codex would get from the router.
func catalogOf(t *testing.T, key func() (string, error)) (string, harness) {
	t.Helper()
	h := newHarness(t, key, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		io.WriteString(w, `{"models":[{"slug":"gpt-6-sol"},{"slug":"gpt-6-astra"},{"slug":"gpt-5.5"}]}`)
	}, ok, ok)
	req, _ := http.NewRequest(http.MethodGet, h.router.URL+"/api/v1/models?client_version=0.155.0", nil)
	req.Header.Set("If-None-Match", `"v0"`)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.Header.Get("ETag") != "" {
		t.Errorf("catalog kept OpenAI's ETag")
	}
	var cat struct {
		Models []struct {
			Slug       string `json:"slug"`
			Visibility string `json:"visibility"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&cat); err != nil {
		t.Fatal(err)
	}
	var shown []string
	for _, m := range cat.Models {
		if m.Visibility != "hide" {
			shown = append(shown, m.Slug)
		}
	}
	return strings.Join(shown, " "), h
}

func TestCatalogGetsExternalModels(t *testing.T) {
	shown, h := catalogOf(t, withKey)
	native := receive(t, h.native)
	if native.path != "/backend-api/codex/models?client_version=0.155.0" || native.header.Get("If-None-Match") != "" ||
		native.header.Get("Accept-Encoding") != "identity" {
		t.Errorf("native got %+v", native)
	}
	if want := "gpt-6-sol claude-opus-5-5 deepseek/deepseek-v4.1-flash moonshotai/kimi-k3 gpt-6-astra"; shown != want {
		t.Fatalf("picker %q", shown)
	}
}

func TestOpenRouterModelsNeedAKey(t *testing.T) {
	noKey := func() (string, error) { return "", errors.New("OPENROUTER_API_KEY is missing") }
	if shown, _ := catalogOf(t, noKey); shown != "gpt-6-sol claude-opus-5-5 gpt-6-astra" {
		t.Fatalf("picker without a key %q", shown)
	}
	h := newHarness(t, noKey, ok, ok, ok)
	if resp := h.post(t, "/api/v1/responses", `{"model":"moonshotai/kimi-k3","input":[]}`); resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d", resp.StatusCode)
	}
}

func TestStreamsAndHangsUp(t *testing.T) {
	hungUp := make(chan struct{})
	h := newHarness(t, withKey, ok, ok, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.created\"}\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // a Claude turn that runs until Codex hangs up
		close(hungUp)
	})
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, h.router.URL+"/api/v1/responses",
		strings.NewReader(`{"model":"claude-opus-5-5","input":[]}`))
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	line, err := bufio.NewReader(resp.Body).ReadString('\n')
	if err != nil || !strings.Contains(line, "response.created") {
		t.Fatalf("first event %q, %v", line, err)
	}
	cancel() // Stop in Codex
	select {
	case <-hungUp:
	case <-time.After(5 * time.Second):
		t.Fatal("the bridge never saw Codex hang up")
	}
	// Deploys wait on in_flight, so an aborted stream must leave it.
	for range 50 {
		var health struct {
			InFlight int `json:"in_flight"`
		}
		resp, err := http.Get(h.router.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		json.NewDecoder(resp.Body).Decode(&health)
		resp.Body.Close()
		if health.InFlight == 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("in_flight never went back to 0")
}

func TestRejectsWhatItCannotRoute(t *testing.T) {
	h := newHarness(t, withKey, ok, ok, ok)
	for path, want := range map[string]int{"/v1/responses": http.StatusNotFound, "/api/v1/responses": http.StatusBadRequest} {
		if resp := h.post(t, path, `{"model":`); resp.StatusCode != want {
			t.Errorf("%s: status %d, want %d", path, resp.StatusCode, want)
		}
	}
}

func mustJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return string(b)
}
