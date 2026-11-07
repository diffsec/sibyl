package dockershim

import (
	"archive/tar"
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	bkclient "github.com/moby/buildkit/client"
	"github.com/tonistiigi/fsutil"
)

// fakePodman serves a Podman socket that records each /build request.
type fakePodman struct {
	mu     sync.Mutex
	query  url.Values
	files  map[string]string
	output string
}

func (f *fakePodman) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/build"):
		files := map[string]string{}
		tr := tar.NewReader(r.Body)
		for {
			h, err := tr.Next()
			if err != nil {
				break
			}
			b, _ := io.ReadAll(tr)
			files[h.Name] = string(b)
		}
		f.mu.Lock()
		f.query, f.files = r.URL.Query(), files
		out := f.output
		f.mu.Unlock()
		_, _ = io.WriteString(w, out)
	case strings.HasSuffix(r.URL.Path, "/attach"):
		// An attach upgrades to a raw stream, and echoes it back.
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer func() { _ = conn.Close() }()
		_, _ = io.WriteString(conn, "HTTP/1.1 101 UPGRADED\r\nConnection: Upgrade\r\nUpgrade: tcp\r\n\r\n")
		line, _ := rw.ReadString('\n')
		_, _ = io.WriteString(conn, "echo "+line)
	default:
		_, _ = io.WriteString(w, `{"Version":"5.8.9"}`)
	}
}

// startShim starts a fake Podman and a shim in front of it, and returns the
// shim socket.
func startShim(t *testing.T, fake *fakePodman) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "shim") // a short path: a socket path has a length limit
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	podSock, shimSock := filepath.Join(dir, "podman.sock"), filepath.Join(dir, "shim.sock")
	serve := func(sock string, h http.Handler) {
		l, err := net.Listen("unix", sock)
		if err != nil {
			t.Fatal(err)
		}
		srv := &http.Server{Handler: h, ReadHeaderTimeout: 5 * time.Second}
		go func() { _ = srv.Serve(l) }()
		t.Cleanup(func() { _ = srv.Close() })
	}
	serve(podSock, fake)
	shim, err := New(podSock, slog.New(slog.DiscardHandler))
	if err != nil {
		t.Fatal(err)
	}
	serve(shimSock, shim)
	return shimSock
}

// dialUpgrade opens path on the socket with an HTTP upgrade, as the Docker
// client's DialHijack does.
func dialUpgrade(ctx context.Context, sock, path, proto string, meta map[string][]string) (net.Conn, error) {
	var d net.Dialer
	conn, err := d.DialContext(ctx, "unix", sock)
	if err != nil {
		return nil, err
	}
	req, _ := http.NewRequest(http.MethodPost, "http://docker"+path, nil)
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", proto)
	for k, v := range meta {
		req.Header[k] = v
	}
	if err := req.Write(conn); err != nil {
		return nil, err
	}
	br := bufio.NewReader(conn)
	resp, err := http.ReadResponse(br, req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusSwitchingProtocols {
		return nil, fmt.Errorf("upgrade %s: %s", path, resp.Status)
	}
	return &bufferedConn{Conn: conn, r: br}, nil
}

func buildkitClient(t *testing.T, sock string) *bkclient.Client {
	t.Helper()
	c, err := bkclient.New(context.Background(), "",
		bkclient.WithContextDialer(func(ctx context.Context, _ string) (net.Conn, error) {
			return dialUpgrade(ctx, sock, "/grpc", "h2c", nil)
		}),
		bkclient.WithSessionDialer(func(ctx context.Context, proto string, meta map[string][]string) (net.Conn, error) {
			return dialUpgrade(ctx, sock, "/v1.44/session", proto, meta)
		}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c
}

func solveOpt(t *testing.T, dir string) bkclient.SolveOpt {
	t.Helper()
	fsys, err := fsutil.NewFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return bkclient.SolveOpt{
		Frontend: "dockerfile.v0",
		FrontendAttrs: map[string]string{
			"filename": "build/app.Dockerfile", "target": "final", "build-arg:GREETING": "hi",
			"label:team": "ml", "no-cache": "",
		},
		LocalMounts: map[string]fsutil.FS{"context": fsys, "dockerfile": fsys},
		Exports:     []bkclient.ExportEntry{{Type: "moby", Attrs: map[string]string{"name": "localhost/app:1,localhost/app:latest"}}},
	}
}

func writeFiles(t *testing.T, dir string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(dir, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
}

func TestSolveThroughGRPC(t *testing.T) {
	fake := &fakePodman{output: `{"stream":"STEP 1/2: FROM busybox\n"}` + "\n" +
		`{"aux":{"ID":"sha256:abc123"}}` + "\n" + `{"stream":"Successfully built abc123\n"}` + "\n"}
	c := buildkitClient(t, startShim(t, fake))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{
		"build/app.Dockerfile":              "FROM busybox AS final\n",
		"build/app.Dockerfile.dockerignore": "secret.txt",
		".dockerignore":                     "other\n",
		"src/main.go":                       "package main\n",
	})
	if err := os.Symlink("src/main.go", filepath.Join(dir, "link")); err != nil {
		t.Fatal(err)
	}
	ch := make(chan *bkclient.SolveStatus)
	var logs strings.Builder
	done := make(chan struct{})
	go func() {
		defer close(done)
		for st := range ch {
			for _, l := range st.Logs {
				logs.Write(l.Data)
			}
		}
	}()
	resp, err := c.Solve(context.Background(), nil, solveOpt(t, dir), ch)
	<-done
	if err != nil {
		t.Fatal(err)
	}
	if got := resp.ExporterResponse["containerimage.digest"]; got != "sha256:abc123" {
		t.Errorf("digest %q", got)
	}
	if !strings.Contains(logs.String(), "Successfully built abc123") {
		t.Errorf("logs %q", logs.String())
	}
	q, files := fake.query, fake.files
	if q.Get("dockerfile") != shimDockerfile || q.Get("target") != "final" || q.Get("nocache") != "1" ||
		strings.Join(q["t"], " ") != "localhost/app:1 localhost/app:latest" ||
		q.Get("buildargs") != `{"GREETING":"hi"}` || q.Get("labels") != `{"team":"ml"}` {
		t.Errorf("query %v", q)
	}
	if files[shimDockerfile] != "FROM busybox AS final\n" || files["src/main.go"] != "package main\n" {
		t.Errorf("files %v", files)
	}
	if files[".dockerignore"] != "secret.txt\n"+shimDockerfile+"\n" {
		t.Errorf("dockerignore %q", files[".dockerignore"])
	}
	if _, ok := files["link"]; !ok {
		t.Errorf("the symlink is not in the context: %v", files)
	}
}

func TestSolveError(t *testing.T) {
	fake := &fakePodman{output: `{"stream":"STEP 1/2\n"}` + "\n" +
		`{"errorDetail":{"message":"RUN false: exit status 1"},"error":"RUN false: exit status 1"}` + "\n"}
	c := buildkitClient(t, startShim(t, fake))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"build/app.Dockerfile": "FROM busybox\nRUN false\n"})
	_, err := c.Solve(context.Background(), nil, solveOpt(t, dir), nil)
	if err == nil || !strings.Contains(err.Error(), "RUN false: exit status 1") {
		t.Errorf("err %v", err)
	}
}

func TestSolveRefusesOtherExporters(t *testing.T) {
	c := buildkitClient(t, startShim(t, &fakePodman{}))
	dir := t.TempDir()
	writeFiles(t, dir, map[string]string{"build/app.Dockerfile": "FROM busybox\n"})
	opt := solveOpt(t, dir)
	opt.Exports = []bkclient.ExportEntry{{Type: "image", Attrs: map[string]string{"name": "x", "push": "true"}}}
	if _, err := c.Solve(context.Background(), nil, opt, nil); err == nil || !strings.Contains(err.Error(), "does not work on Podman") {
		t.Errorf("err %v", err)
	}
}

func TestProxyPassesThrough(t *testing.T) {
	sock := startShim(t, &fakePodman{})
	hc := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	}}}
	resp, err := hc.Get("http://docker/v1.44/version")
	if err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	var v struct{ Version string }
	if json.Unmarshal(b, &v) != nil || v.Version != "5.8.9" {
		t.Errorf("version %s", b)
	}
	// An attach upgrade goes through as a raw stream.
	conn, err := dialUpgrade(context.Background(), sock, "/v1.44/containers/x/attach", "tcp", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close() }()
	_, _ = io.WriteString(conn, "ping\n")
	line, _ := bufio.NewReader(conn).ReadString('\n')
	if line != "echo ping\n" {
		t.Errorf("attach %q", line)
	}
}

func TestBuildkitPathNeedsUpgrade(t *testing.T) {
	sock := startShim(t, &fakePodman{})
	for _, p := range []string{"/session", "/v1.44/grpc"} {
		if _, err := dialUpgrade(context.Background(), sock, p, "tcp", nil); err == nil || !strings.Contains(err.Error(), "400") {
			t.Errorf("%s: %v", p, err)
		}
	}
	for p, want := range map[string]bool{"/session": true, "/v1.44/grpc": true, "/v1/session": true,
		"/grpc/x": false, "/containers/grpc": false, "/v1.44/sessions": false} {
		if got := buildkitPath.MatchString(p); got != want {
			t.Errorf("%s: %v", p, got)
		}
	}
}

func TestFromHosts(t *testing.T) {
	df := "# syntax=docker/dockerfile:1\nARG BASE=alpine\nFROM --platform=$BUILDPLATFORM golang:1.27 AS build\n" +
		"FROM ghcr.io/org/tool:1 AS tool\nFROM build AS test\nFROM $BASE\nFROM scratch\n" +
		"from \\\n  xtc6g4dmzn.registry.depot.dev/heathen:rocm\nFROM localhost:5000/x\nFROM ghcr.io/org/other\n"
	got := strings.Join(fromHosts(df), " ")
	if got != "docker.io ghcr.io xtc6g4dmzn.registry.depot.dev localhost:5000" {
		t.Errorf("hosts %q", got)
	}
	for ref, want := range map[string]string{"busybox": "docker.io", "library/busybox": "docker.io",
		"localhost/app": "localhost", "quay.io/fedora/fedora:44": "quay.io"} {
		if got := registryHost(ref); got != want {
			t.Errorf("%s: %s", ref, got)
		}
	}
}

func TestDockerfileName(t *testing.T) {
	for in, want := range map[string]string{"": "Dockerfile", "../../etc/passwd": "etc/passwd", "a/./b.Dockerfile": "a/b.Dockerfile"} {
		if got, err := dockerfileName(in); err != nil || got != want {
			t.Errorf("%q: %q %v", in, got, err)
		}
	}
	if _, err := dockerfileName("/"); err == nil {
		t.Error("an empty name passed")
	}
}
