package api

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/ihavespoons/sibyl/internal/hostctl"
	"github.com/ihavespoons/sibyl/internal/hostd"
)

const testKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ben@laptop"

// hostFixture runs a real hostd.Agent against the API's queue, with a
// runner that records commands instead of running them.
type hostFixture struct {
	*fixture
	mu    sync.Mutex
	calls []string
}

func newHostFixture(t *testing.T, agent bool) *hostFixture {
	t.Helper()
	f := &hostFixture{fixture: newFixture(t)}
	queue := filepath.Join(f.run, "host")
	out := filepath.Join(f.run, "host-out")
	mkdir(t, queue)
	mkdir(t, out)
	f.server.HostAgent = hostctl.NewHost(queue, out)
	f.server.HostAgent.Poll = 10 * time.Millisecond
	f.server.HostWait = 2 * time.Second
	if !agent {
		f.server.HostWait = 100 * time.Millisecond
		return f
	}
	a := &hostd.Agent{
		Queue: queue, Out: out, Data: t.TempDir(), Now: time.Now, Group: -1,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Run: func(_ context.Context, _, name string, args ...string) (string, error) {
			f.mu.Lock()
			defer f.mu.Unlock()
			f.calls = append(f.calls, strings.Join(append([]string{name}, args...), " "))
			switch c := f.calls[len(f.calls)-1]; {
			case c == "hostnamectl hostname":
				return "sibyl\n", nil
			case c == "getent passwd ops" && f.ranLocked("useradd"):
				return "ops:x:1002:1002::/home/ops:/bin/bash\n", nil
			}
			return "", nil
		},
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ctx.Err() == nil {
			if entries, _ := os.ReadDir(queue); len(entries) > 0 {
				_ = a.Process(ctx)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { cancel(); <-done })
	return f
}

func (f *hostFixture) ran(cmd string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, c := range f.calls {
		if c == cmd {
			return true
		}
	}
	return false
}

func (f *hostFixture) ranLocked(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

func TestHostHostname(t *testing.T) {
	f := newHostFixture(t, true)
	code, body := f.do(t, "PUT", "/host/hostname", `{"hostname":"sibyl-02"}`, true)
	if code != http.StatusOK || body["ok"] != true || !f.ran("hostnamectl hostname sibyl-02") {
		t.Fatalf("PUT /host/hostname = %d %v", code, body)
	}
	code, body = f.do(t, "GET", "/host", "", true)
	if code != http.StatusOK || body["hostname"] != "sibyl" {
		t.Fatalf("GET /host = %d %v", code, body)
	}
}

func TestHostValidationNeverQueues(t *testing.T) {
	f := newHostFixture(t, false)
	for _, c := range []struct{ method, path, body string }{
		{"PUT", "/host/hostname", `{"hostname":"a b"}`},
		{"PUT", "/host/network/enp8s0", `{"method":"static","address":"300.1.1.1/24"}`},
		{"PUT", "/host/users/root", `{"keys":["` + testKey + `"]}`},
		{"PUT", "/host/users/ops", `{"keys":["command=\"sh\" ` + testKey + `"]}`},
		{"DELETE", "/host/users/admin", ``},
	} {
		if code, body := f.do(t, c.method, c.path, c.body, true); code != http.StatusUnprocessableEntity {
			t.Errorf("%s %s = %d %v, want 422", c.method, c.path, code, body)
		}
	}
	if entries, _ := os.ReadDir(filepath.Join(f.run, "host")); len(entries) != 0 {
		t.Fatalf("queued %v", entries)
	}
}

func TestHostQueuedWithoutAgent(t *testing.T) {
	f := newHostFixture(t, false)
	code, body := f.do(t, "PUT", "/host/network/enp8s0", `{"method":"dhcp"}`, true)
	if code != http.StatusAccepted || body["status"] != "queued" {
		t.Fatalf("PUT /host/network = %d %v", code, body)
	}
	id, _ := body["id"].(string)
	if code, _ := f.do(t, "GET", "/host/requests/"+id, "", true); code != http.StatusAccepted {
		t.Fatalf("GET /host/requests/%s = %d", id, code)
	}
	if code, _ := f.do(t, "GET", "/host/requests/..%2Fetc", "", true); code != http.StatusBadRequest {
		t.Fatalf("GET with a bad ID = %d", code)
	}
}

func TestHostUserFromGitHub(t *testing.T) {
	f := newHostFixture(t, true)
	gh := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/octo.keys" {
			http.NotFound(w, r)
			return
		}
		_, _ = io.WriteString(w, "ssh-dss AAAAB3NzaC1kc3MAAACBAP\n"+strings.Fields(testKey)[0]+" "+strings.Fields(testKey)[1]+"\n")
	}))
	t.Cleanup(gh.Close)
	f.server.GitHubBase = gh.URL

	code, body := f.do(t, "PUT", "/host/users/ops", `{"keys":[],"github":"octo"}`, true)
	if code != http.StatusOK || body["ok"] != true {
		t.Fatalf("PUT /host/users/ops = %d %v", code, body)
	}
	if code, _ := f.do(t, "PUT", "/host/users/ops", `{"keys":[],"github":"nobody"}`, true); code != http.StatusUnprocessableEntity {
		t.Fatalf("unknown GitHub user = %d", code)
	}
}
