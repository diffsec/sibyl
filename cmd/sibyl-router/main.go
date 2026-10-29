// Command sibyl-router serves the Sibyl model router: the OpenAI and
// Anthropic compatible endpoints in front of llama-swap and the providers.
//
// With -healthcheck it asks a running router for /health and exits, for the
// container health check.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/diffsec/sibyl/internal/router"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	health := flag.Bool("healthcheck", false, "check a running router and exit")
	flag.Parse()
	addr := env("SIBYL_LISTEN", ":8080")
	if *health {
		if err := router.Healthcheck(addr); err != nil {
			os.Exit(1)
		}
		return
	}
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log, addr); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func readKey(path string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	k := strings.TrimSpace(string(b))
	if k == "" {
		return "", errors.New(path + " is empty")
	}
	return k, nil
}

func run(log *slog.Logger, addr string) error {
	admin, err := readKey(env("SIBYL_API_KEY_FILE", "/run/secrets/api-key"))
	if err != nil {
		return err
	}
	owui, err := readKey(env("SIBYL_OPEN_WEBUI_KEY_FILE", "/run/secrets/open-webui-key"))
	if err != nil {
		log.Warn("no Open WebUI key; the open-webui client is off", "err", err)
	}
	s := &router.Server{
		ConfigDir:    env("SIBYL_ROUTER_CONFIG", "/config"),
		LocalURL:     env("SIBYL_LLAMA_SWAP_URL", "http://llama-swap:8080"),
		AdminKey:     admin,
		OpenWebUIKey: owui,
		HTTP: &http.Client{Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			MaxIdleConnsPerHost:   16,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ExpectContinueTimeout: time.Second,
		}},
		Log:    &router.Log{Path: filepath.Join(env("SIBYL_ROUTER_LOG_DIR", "/log"), "decisions.jsonl")},
		Logger: log,
	}
	// A configuration error at startup is not fatal: with no configuration
	// the router serves local models to the built-in clients.
	if err := s.Reload(); err != nil {
		log.Error("configuration not loaded", "err", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	go s.Watch(ctx, 2*time.Second)

	srv := &http.Server{
		Addr:              addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	errc := make(chan error, 1)
	go func() {
		log.Info("listening", "addr", srv.Addr)
		errc <- srv.ListenAndServe()
	}()
	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return srv.Shutdown(shutdown)
}
