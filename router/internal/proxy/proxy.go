// Package proxy is the router's HTTP server. Codex sends it every model
// request as if it were OpenAI's Codex backend; it forwards each one to the
// upstream serving the model and adds the external models to the model
// catalog Codex fetches.
package proxy

import (
	"bytes"
	"cmp"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/klauspost/compress/zstd"

	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/catalog"
	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/upstream"
)

const maxBodyBytes = 32 << 20

// Config says where the upstreams are and how the picker looks.
type Config struct {
	NativeURL       *url.URL // OpenAI's Codex backend: https://chatgpt.com/backend-api/codex
	OpenRouterURL   *url.URL // https://openrouter.ai/api/v1
	BridgeURL       *url.URL // the Claude Code bridge: http://127.0.0.1:41420/api/v1
	OpenRouterKey   func() (string, error)
	MaxOutputTokens int // the output cap for OpenRouter models
	Picker          catalog.Picker
}

// Server forwards Codex's requests.
type Server struct {
	cfg       Config
	external  []catalog.Model
	routes    map[string]upstream.Route // external model slug to its upstream
	transport http.RoundTripper
	client    *http.Client
	started   time.Time
	inFlight  atomic.Int64 // requests, and responses on websockets
	sessions  sync.Map     // *wsSession: Codex's open websockets
	draining  atomic.Bool
}

func New(cfg Config, external []catalog.Model) (*Server, error) {
	s := &Server{cfg: cfg, external: external, routes: map[string]upstream.Route{},
		transport: http.DefaultTransport, started: time.Now()}
	s.client = &http.Client{Transport: s.transport}
	for _, m := range external {
		switch r := upstream.Route(m.Upstream); r {
		case upstream.OpenRouter, upstream.Bridge:
			s.routes[m.Slug] = r
		default:
			return nil, fmt.Errorf("model %s: unknown upstream %q", m.Slug, m.Upstream)
		}
	}
	return s, nil
}

// InFlight is the number of requests being forwarded, counting each response
// on a websocket.
func (s *Server) InFlight() int64 { return s.inFlight.Load() }

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet && r.URL.Path == "/health" {
		s.health(w)
		return
	}
	if isWebsocket(r) && strings.HasSuffix(r.URL.Path, "/responses") {
		// A websocket stays open between responses; each is counted alone.
		s.serveResponsesWebsocket(w, r)
		return
	}
	s.inFlight.Add(1)
	defer s.inFlight.Add(-1)
	start := time.Now()
	rec := &recorder{ResponseWriter: w}
	var f forwarding
	defer func() {
		// A stream that ends early aborts with a panic, which still gets its
		// line: Codex hanging up once it has the whole reply is normal.
		p := recover()
		slog.Info("request", "method", r.Method, "path", r.URL.Path, "model", f.model, "route", f.route,
			"status", rec.status, "duration", time.Since(start).Round(time.Millisecond), "codex_hung_up", r.Context().Err() != nil)
		if p != nil {
			panic(p)
		}
	}()
	f, err := s.plan(w, r)
	if err != nil {
		writeError(rec, f.status, err.Error())
		return
	}
	if f.compaction {
		s.serveCompaction(rec, r, f)
		return
	}
	s.forward(rec, r, f)
}

// forwarding is where one request goes, and what it carries.
type forwarding struct {
	route   upstream.Route
	model   string
	url     *url.URL
	body    []byte
	apiKey  string // OpenRouter's
	catalog bool   // a model catalog, to add the external models to
	rewrote bool   // the body is the router's JSON, not Codex's possibly compressed bytes
	status  int    // the error status when the request cannot be forwarded
	// compaction: the body asks for the summary of a compaction that the
	// router answers itself, for a model that cannot compact.
	compaction bool
}

// plan decides where a request goes. Responses requests go by model; every
// other request, such as the model catalog, goes to OpenAI.
func (s *Server) plan(w http.ResponseWriter, r *http.Request) (forwarding, error) {
	f := forwarding{route: upstream.Native, status: http.StatusBadRequest}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		return f, fmt.Errorf("read request: %w", err)
	}
	f.body = body
	if f.url, err = s.nativeURL(r.URL); err != nil {
		f.status = http.StatusNotFound
		return f, err
	}
	f.catalog = r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/models")
	if r.Method != http.MethodPost || !strings.HasSuffix(r.URL.Path, "/responses") {
		return f, nil
	}

	plain, err := decodeBody(r.Header, body)
	if err != nil {
		return f, err
	}
	req, err := upstream.Parse(plain)
	if err != nil {
		return f, err
	}
	f.model = req.Model()
	if f.route = s.route(f.model); f.route != upstream.Native {
		return f, s.forExternal(&f, req, plain)
	}
	// Codex's bytes go to OpenAI as they are, compressed or not, unless
	// another model's history needed fixing.
	var fixed []byte
	if fixed, f.rewrote, err = req.ForNative(); f.rewrote {
		f.body = fixed
	}
	return f, err
}

// route is the upstream serving model.
func (s *Server) route(model string) upstream.Route {
	return cmp.Or(s.routes[model], upstream.Native)
}

// forExternal points f at the upstream serving an external model. body is
// the request as plain JSON, which the bridge reads as it is; OpenRouter gets
// its own form of the request.
func (s *Server) forExternal(f *forwarding, req *upstream.Request, body []byte) error {
	switch f.route {
	case upstream.OpenRouter:
		f.url = s.cfg.OpenRouterURL.JoinPath("responses")
		key, err := s.cfg.OpenRouterKey()
		if err != nil {
			f.status = http.StatusBadGateway
			return err
		}
		f.apiKey = key
		if f.compaction = req.IsCompaction(); f.compaction {
			if err := req.ForSummary(); err != nil {
				return err
			}
		}
		f.body, err = req.ForOpenRouter(s.cfg.MaxOutputTokens)
		return err
	case upstream.Bridge:
		f.url = s.cfg.BridgeURL.JoinPath("responses")
		f.body = body
	}
	return nil
}

// externalHeader is all an external upstream gets of a request's headers.
// The ChatGPT auth and account reach OpenAI only.
func (f forwarding) externalHeader(contentType, accept string) http.Header {
	h := http.Header{}
	h.Set("Content-Type", cmp.Or(contentType, "application/json"))
	h.Set("Accept", cmp.Or(accept, "text/event-stream"))
	if f.apiKey != "" {
		h.Set("Authorization", "Bearer "+f.apiKey)
	}
	return h
}

func (s *Server) forward(w http.ResponseWriter, r *http.Request, f forwarding) {
	rp := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			out := pr.Out
			out.URL, out.Host = f.url, f.url.Host
			out.Body, out.ContentLength = io.NopCloser(bytes.NewReader(f.body)), int64(len(f.body))
			out.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(f.body)), nil }
			switch f.route {
			case upstream.Native:
				// OpenAI gets Codex's headers as they are, with the ChatGPT
				// auth and account. No other upstream sees them.
				if f.catalog {
					out.Header.Del("If-None-Match")
					out.Header.Set("Accept-Encoding", "identity")
				}
				if f.rewrote {
					out.Header.Del("Content-Encoding")
				}
			default:
				out.Header = f.externalHeader(pr.In.Header.Get("Content-Type"), pr.In.Header.Get("Accept"))
			}
		},
		Transport:     s.transport,
		FlushInterval: -1, // Responses stream as server-sent events
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if r.Context().Err() != nil {
				return // Codex hung up first; there is no one to answer
			}
			slog.Error("upstream", "route", f.route, "model", f.model, "err", err)
			writeError(w, http.StatusBadGateway, err.Error())
		},
	}
	if f.catalog {
		rp.ModifyResponse = s.addExternal
	}
	rp.ServeHTTP(w, r)
}

// Codex addresses the router as OpenAI's Codex backend, at /backend-api/codex.
// /api/v1 is where a custom Codex model provider pointed before; both reach
// the same routes.
var basePaths = []string{"/backend-api/codex/", "/api/v1/"}

// nativeURL is where OpenAI serves the path Codex asked for.
func (s *Server) nativeURL(u *url.URL) (*url.URL, error) {
	for _, base := range basePaths {
		if rest, ok := strings.CutPrefix(u.Path, base); ok {
			native := s.cfg.NativeURL.JoinPath(rest)
			native.RawQuery = u.RawQuery
			return native, nil
		}
	}
	return nil, fmt.Errorf("no route for %s; Codex's base URL is /backend-api/codex", u.Path)
}

// zstdDecoder reads the zstd bodies Codex sends OpenAI's backend.
var zstdDecoder = mustZstd()

func mustZstd() *zstd.Decoder {
	d, err := zstd.NewReader(nil, zstd.WithDecoderMaxMemory(512<<20))
	if err != nil {
		panic(err)
	}
	return d
}

// decodeBody is a request body as JSON, decompressed when Codex compressed it.
func decodeBody(h http.Header, body []byte) ([]byte, error) {
	switch enc := h.Get("Content-Encoding"); enc {
	case "", "identity":
		return body, nil
	case "zstd":
		plain, err := zstdDecoder.DecodeAll(body, nil)
		if err != nil {
			return nil, fmt.Errorf("decompress request: %w", err)
		}
		return plain, nil
	default:
		return nil, fmt.Errorf("unsupported request Content-Encoding %q", enc)
	}
}

func isWebsocket(r *http.Request) bool {
	return strings.EqualFold(r.Header.Get("Upgrade"), "websocket")
}

// addExternal adds the external models to OpenAI's model catalog. A catalog
// it cannot read fails the request, so Codex keeps the catalog it has.
//
// The merged catalog keeps OpenAI's ETag. Codex compares it with the catalog
// version OpenAI reports alongside each response, and waits on a fresh
// catalog whenever they differ.
func (s *Server) addExternal(resp *http.Response) error {
	if resp.StatusCode != http.StatusOK {
		return nil
	}
	native, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		return err
	}
	merged, err := catalog.Merge(native, s.listed(), s.cfg.Picker)
	if err != nil {
		return err
	}
	resp.Body = io.NopCloser(bytes.NewReader(merged))
	resp.ContentLength = int64(len(merged))
	for _, h := range []string{"Content-Encoding", "Transfer-Encoding"} {
		resp.Header.Del(h)
	}
	resp.Header.Set("Content-Length", strconv.Itoa(len(merged)))
	return nil
}

// listed is the external models the catalog shows: OpenRouter's only while
// there is a key for them.
func (s *Server) listed() []catalog.Model {
	if _, err := s.cfg.OpenRouterKey(); err == nil {
		return s.external
	}
	var out []catalog.Model
	for _, m := range s.external {
		if upstream.Route(m.Upstream) != upstream.OpenRouter {
			out = append(out, m)
		}
	}
	return out
}

func (s *Server) health(w http.ResponseWriter) {
	var slugs []string
	for _, m := range s.listed() {
		slugs = append(slugs, m.Slug)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"ok": true, "models": slugs, "in_flight": s.inFlight.Load(),
		"uptime_seconds": int(time.Since(s.started).Seconds()),
	})
}

func writeError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"error": map[string]string{"message": message}})
}

// recorder notes the status a response starts with.
type recorder struct {
	http.ResponseWriter
	status int
}

func (r *recorder) WriteHeader(status int) {
	if r.status == 0 {
		r.status = status
	}
	r.ResponseWriter.WriteHeader(status)
}

func (r *recorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	return r.ResponseWriter.Write(b)
}

func (r *recorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
