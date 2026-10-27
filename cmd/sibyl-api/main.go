// Command sibyl-api serves the Sibyl System API.
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/ihavespoons/sibyl/internal/api"
	"github.com/ihavespoons/sibyl/internal/download"
	"github.com/ihavespoons/sibyl/internal/hf"
	"github.com/ihavespoons/sibyl/internal/hostctl"
	"github.com/ihavespoons/sibyl/internal/ollama"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	if err := run(log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	token, err := os.ReadFile(env("SIBYL_API_KEY_FILE", "/run/secrets/api-key"))
	if err != nil {
		return err
	}
	s := &api.Server{
		Paths: api.Paths{
			Config:    env("SIBYL_CONFIG_DIR", "/config"),
			Models:    env("SIBYL_MODELS_DIR", "/models"),
			Pools:     env("SIBYL_POOLS_DIR", "/pools"),
			DropIn:    env("SIBYL_DROPIN_DIR", "/quadlet"),
			SysfsRoot: env("SIBYL_SYSFS", "/sys"),
			MemInfo:   env("SIBYL_MEMINFO", "/proc/meminfo"),
		},
		Host: hostctl.New(env("SIBYL_RUN_DIR", "/run/sibyl")),
		HostAgent: hostctl.NewHost(env("SIBYL_RUN_DIR", "/run/sibyl")+"/host",
			env("SIBYL_HOST_OUT_DIR", "/run/sibyl-host")),
		Token:    strings.TrimSpace(string(token)),
		Registry: env("SIBYL_REGISTRY", "ghcr.io/ihavespoons"),
		ImageTag: env("SIBYL_IMAGE_TAG", "latest"),
		Version:  env("SIBYL_VERSION", "dev"),
		Log:      log,
	}
	if s.Token == "" {
		return errors.New("the API key file is empty")
	}

	// An optional Hugging Face token, for gated repositories.
	hfToken := ""
	if b, err := os.ReadFile(env("SIBYL_HF_TOKEN_FILE", "/run/secrets/hf-token")); err == nil {
		hfToken = strings.TrimSpace(string(b))
	}
	s.HF = hf.New(env("SIBYL_HF_BASE", hf.DefaultBase), hfToken)
	s.Downloads = &download.Manager{
		ModelsDir: s.Paths.Models,
		PoolsDir:  s.Paths.Pools,
		StateFile: s.Paths.Config + "/downloads.json",
		HF:        s.HF,
		Log:       log,
		OnDone:    s.OnDownloadDone,
	}
	s.OpenWebUI = &api.OpenWebUI{
		Base:     env("SIBYL_OPEN_WEBUI_URL", "http://open-webui:8080"),
		EmbedURL: env("SIBYL_LLAMA_SWAP_URL", "http://llama-swap:8080") + "/v1",
	}
	s.Ollama = &ollama.Shim{
		Upstream: env("SIBYL_LLAMA_SWAP_URL", "http://llama-swap:8080"),
		Models:   s.OllamaModels,
		HTTP:     &http.Client{},
	}

	// Bring the generated files in line with the disk at startup, for example
	// after models were copied in by hand.
	if res, err := s.Apply(); err != nil {
		log.Error("startup apply failed", "err", err)
	} else {
		log.Info("startup apply", "models", res.Models, "backend_changed", res.BackendChanged)
	}

	go s.SyncOpenWebUI()

	srv := &http.Server{
		Addr:              env("SIBYL_LISTEN", ":3001"),
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	if err := s.Downloads.Start(ctx); err != nil {
		return err
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
