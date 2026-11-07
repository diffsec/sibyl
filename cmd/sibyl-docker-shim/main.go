// Command sibyl-docker-shim serves the Docker Engine API in front of the
// rootless Podman socket, with the BuildKit endpoints that Podman lacks.
// Arcane mounts its socket in place of the Podman socket, so its builds work.
// See internal/dockershim.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"github.com/diffsec/sibyl/internal/dockershim"
)

func main() {
	run := os.Getenv("XDG_RUNTIME_DIR")
	listen := flag.String("listen", filepath.Join(run, "sibyl-docker.sock"), "the socket to serve")
	upstream := flag.String("upstream", filepath.Join(run, "podman", "podman.sock"), "the Podman socket")
	flag.Parse()
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	if err := serve(*listen, *upstream, log); err != nil {
		log.Error("exit", "err", err)
		os.Exit(1)
	}
}

// listener returns the socket that systemd passes (sibyl-docker-shim.socket),
// or else listens on the path.
func listener(path string) (net.Listener, error) {
	if os.Getenv("LISTEN_PID") == strconv.Itoa(os.Getpid()) && os.Getenv("LISTEN_FDS") == "1" {
		f := os.NewFile(3, "systemd socket")
		defer func() { _ = f.Close() }()
		return net.FileListener(f)
	}
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	// Only the owner reaches the socket, as with the Podman socket.
	old := syscall.Umask(0o177)
	defer syscall.Umask(old)
	return net.Listen("unix", path)
}

func serve(listen, upstream string, log *slog.Logger) error {
	shim, err := dockershim.New(upstream, log)
	if err != nil {
		return err
	}
	l, err := listener(listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: shim, ReadHeaderTimeout: 30 * time.Second}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	go func() {
		<-ctx.Done()
		sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(sctx)
	}()
	log.Info("serving", "listen", l.Addr().String(), "upstream", upstream)
	if err := srv.Serve(l); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
