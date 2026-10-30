package download

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/hf"
)

const rev = "4c577be098c274a1f579e072178db01dd3a8e449"

type fakeHub struct {
	files   map[string][]byte
	shaFor  map[string]string // override the advertised digest
	cutOnce sync.Map          // path -> true: first GET stops halfway
	gets    atomic.Int32
}

func digest(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func (h *fakeHub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	switch {
	case r.URL.Path == "/api/models/o/r":
		_ = json.NewEncoder(w).Encode(map[string]string{"sha": rev})
	case r.URL.Path == "/api/models/o/r/tree/"+rev:
		var tree []map[string]any
		for p, b := range h.files {
			sha := digest(b)
			if o, ok := h.shaFor[p]; ok {
				sha = o
			}
			tree = append(tree, map[string]any{"type": "file", "path": p, "size": len(b), "lfs": map[string]any{"oid": sha}})
		}
		_ = json.NewEncoder(w).Encode(tree)
	case strings.HasPrefix(r.URL.Path, "/o/r/resolve/"+rev+"/"):
		p := strings.TrimPrefix(r.URL.Path, "/o/r/resolve/"+rev+"/")
		b, ok := h.files[p]
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.gets.Add(1)
		if _, cut := h.cutOnce.LoadAndDelete(p); cut {
			// Promise the full body, send half, then drop the connection.
			w.Header().Set("Content-Length", fmt.Sprint(len(b)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(b[:len(b)/2])
			if hj, ok := w.(http.Hijacker); ok {
				conn, _, _ := hj.Hijack()
				_ = conn.Close()
			}
			return
		}
		http.ServeContent(w, r, p, time.Time{}, bytes.NewReader(b))
	default:
		http.NotFound(w, r)
	}
}

func newManager(t *testing.T, hub *fakeHub) (*Manager, chan struct{}) {
	t.Helper()
	srv := httptest.NewServer(hub)
	t.Cleanup(srv.Close)
	dir := t.TempDir()
	done := make(chan struct{}, 4)
	m := &Manager{
		ModelsDir:    filepath.Join(dir, "models"),
		PoolsDir:     filepath.Join(dir, "pools"),
		StateFile:    filepath.Join(dir, "downloads.json"),
		HF:           hf.New(srv.URL, ""),
		Log:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		OnDone:       func() { done <- struct{}{} },
		StallTimeout: 5 * time.Second,
	}
	if err := os.MkdirAll(m.ModelsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := m.Start(ctx); err != nil {
		t.Fatal(err)
	}
	return m, done
}

func waitFor(t *testing.T, m *Manager, id string, want Status) Job {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if j, _ := m.Get(id); j.Status == want {
			return j
		} else if j.Status == Failed && want != Failed {
			t.Fatalf("job failed: %s", j.Error)
		}
		time.Sleep(20 * time.Millisecond)
	}
	j, _ := m.Get(id)
	t.Fatalf("job %s is %s, want %s", id, j.Status, want)
	return j
}

func TestDownloadSplitWithResume(t *testing.T) {
	a := bytes.Repeat([]byte("A"), 300_000)
	b := bytes.Repeat([]byte("B"), 200_000)
	hub := &fakeHub{files: map[string][]byte{
		"Big-Q4-00001-of-00002.gguf": a,
		"Big-Q4-00002-of-00002.gguf": b,
		"Small.gguf":                 []byte("small"),
	}}
	hub.cutOnce.Store("Big-Q4-00001-of-00002.gguf", true)
	m, done := newManager(t, hub)

	j, err := m.Add(context.Background(), "o/r", "", "Big-Q4", "")
	if err != nil {
		t.Fatal(err)
	}
	if j.Revision != rev || j.BytesTotal != int64(len(a)+len(b)) || j.Dir != "Big-Q4" {
		t.Fatalf("job = %+v", j)
	}
	j = waitFor(t, m, j.ID, Done)
	<-done

	for name, want := range map[string][]byte{"Big-Q4-00001-of-00002.gguf": a, "Big-Q4-00002-of-00002.gguf": b} {
		got, err := os.ReadFile(filepath.Join(m.ModelsDir, "Big-Q4", name))
		if err != nil || !bytes.Equal(got, want) {
			t.Errorf("%s: err %v, %d bytes", name, err, len(got))
		}
	}
	if _, err := os.Stat(filepath.Join(m.ModelsDir, "Big-Q4", ".sibyl-source.json")); err != nil {
		t.Errorf("provenance file missing: %v", err)
	}
	if _, err := os.Stat(m.partialDir(&j)); !os.IsNotExist(err) {
		t.Errorf("partial directory left behind: %v", err)
	}
	// The cut transfer must resume, not restart: 3 GETs for 2 files.
	if n := hub.gets.Load(); n != 3 {
		t.Errorf("GET count = %d, want 3", n)
	}

	if _, err := m.Add(context.Background(), "o/r", rev, "Big-Q4", ""); err == nil || !strings.Contains(err.Error(), "already installed") {
		t.Errorf("second add: err = %v, want already installed", err)
	}
}

func TestDownloadChecksumMismatch(t *testing.T) {
	hub := &fakeHub{
		files:  map[string][]byte{"Bad.gguf": []byte("payload")},
		shaFor: map[string]string{"Bad.gguf": strings.Repeat("0", 64)},
	}
	m, _ := newManager(t, hub)
	j, err := m.Add(context.Background(), "o/r", rev, "Bad", "")
	if err != nil {
		t.Fatal(err)
	}
	j = waitFor(t, m, j.ID, Failed)
	if !strings.Contains(j.Error, "SHA-256 mismatch") {
		t.Errorf("error = %q", j.Error)
	}
	if _, err := os.Stat(filepath.Join(m.ModelsDir, "Bad")); !os.IsNotExist(err) {
		t.Error("a model that failed its checksum was installed")
	}
	// The worker saves the state file after the status changes. Wait for
	// that save, or TempDir cleanup races its temporary file.
	deadline := time.Now().Add(10 * time.Second)
	for b, _ := os.ReadFile(m.StateFile); !strings.Contains(string(b), string(Failed)); b, _ = os.ReadFile(m.StateFile) {
		if time.Now().After(deadline) {
			t.Fatal("the failed job never reached the state file")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func TestDownloadIntoPool(t *testing.T) {
	m, done := newManager(t, &fakeHub{files: map[string][]byte{"A.gguf": []byte("a")}})
	if _, err := m.Add(context.Background(), "o/r", rev, "A", "pool-1"); err == nil {
		t.Fatal("queued a download into a pool that does not exist")
	}
	if err := os.MkdirAll(filepath.Join(m.PoolsDir, "pool-1"), 0o755); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Add(context.Background(), "o/r", rev, "A", "../models"); err == nil {
		t.Fatal("queued a download outside the pools")
	}
	j, err := m.Add(context.Background(), "o/r", rev, "A", "pool-1")
	if err != nil {
		t.Fatal(err)
	}
	waitFor(t, m, j.ID, Done)
	<-done
	if b, err := os.ReadFile(filepath.Join(m.PoolsDir, "pool-1", "A", "A.gguf")); err != nil || string(b) != "a" {
		t.Fatalf("pool file: %q %v", b, err)
	}
}

func TestAddRejects(t *testing.T) {
	m, _ := newManager(t, &fakeHub{files: map[string][]byte{"A.gguf": []byte("a")}})
	ctx := context.Background()
	for _, c := range []struct{ repo, rev, variant string }{
		{"not-a-repo", "", "A"},
		{"o/r", "main", "A"},
		{"o/r", rev, "Missing"},
	} {
		if _, err := m.Add(ctx, c.repo, c.rev, c.variant, ""); err == nil {
			t.Errorf("Add(%q, %q, %q): want an error", c.repo, c.rev, c.variant)
		}
	}
}

func TestDirName(t *testing.T) {
	for in, want := range map[string]string{"sub/Model-Q4_K_M": "Model-Q4_K_M", "Odd name!": "Odd-name", "..": ""} {
		if got := DirName(in); got != want {
			t.Errorf("DirName(%q) = %q, want %q", in, got, want)
		}
	}
}
