// Command codex-model-router is the model provider Codex talks to. It sends
// OpenAI models to OpenAI's Codex backend with the user's ChatGPT sign-in,
// the models in router/models/openrouter to OpenRouter, and those in
// router/models/bridge to the local Claude Code bridge. It also serves Codex
// the model catalog, OpenAI's plus those models.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/catalog"
	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/config"
	"github.com/kiluazen/chatgpt-claude-bridge/router/internal/proxy"
	"github.com/kiluazen/chatgpt-claude-bridge/router/models"
)

// drainTimeout bounds how long a stopping router waits for open streams.
const drainTimeout = 5 * time.Minute

func main() {
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stdout, nil)))
	configPath := flag.String("config", "~/.codex/model-router.json", "settings file; optional")
	addr := flag.String("addr", "127.0.0.1:41419", "where Codex's model provider points")
	flag.Parse()
	if err := serve(*configPath, *addr); err != nil {
		slog.Error(err.Error())
		os.Exit(1)
	}
}

func serve(configPath, addr string) error {
	cfg, err := config.Load(configPath)
	if err != nil {
		return err
	}
	external, err := catalog.Load(models.FS)
	if err != nil {
		return err
	}
	srv, err := proxy.New(proxy.Config{
		NativeURL:       mustURL("https://chatgpt.com/backend-api/codex"),
		OpenRouterURL:   mustURL("https://openrouter.ai/api/v1"),
		BridgeURL:       mustURL("http://127.0.0.1:41420/api/v1"),
		OpenRouterKey:   cfg.OpenRouterKey,
		MaxOutputTokens: cfg.MaxOutputTokens,
		Picker:          catalog.Picker{Order: cfg.Picker, Hide: cfg.Hide},
	}, external)
	if err != nil {
		return err
	}
	httpSrv := &http.Server{Addr: addr, Handler: srv, ReadHeaderTimeout: 10 * time.Second}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	slog.Info("router listening", "addr", addr, "models", len(external), "env_file", cfg.EnvFile)
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	// The listener closes at once, so a new router can start while open
	// streams finish here.
	slog.Info("draining", "in_flight", srv.InFlight())
	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()
	if err := httpSrv.Shutdown(drainCtx); err != nil {
		return fmt.Errorf("drain: %w", err)
	}
	return nil
}

func mustURL(s string) *url.URL {
	u, err := url.Parse(s)
	if err != nil {
		panic(err)
	}
	return u
}
