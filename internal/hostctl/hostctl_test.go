package hostctl

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func newClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	if err := os.Mkdir(filepath.Join(dir, "requests"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	c.now = func() time.Time { return time.Unix(0, 42) }
	return c
}

func TestRequest(t *testing.T) {
	c := newClient(t)
	if err := c.Request(Restart, "llama-swap"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "requests", "42.restart.llama-swap")); err != nil {
		t.Errorf("request file missing: %v", err)
	}
	c.now = func() time.Time { return time.Unix(0, 43) }
	if err := c.Request(Reload, "ignored"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(c.Dir, "requests", "43.reload.all")); err != nil {
		t.Errorf("reload file missing: %v", err)
	}
}

func TestRequestRejects(t *testing.T) {
	c := newClient(t)
	for _, tc := range []struct {
		a    Action
		unit string
	}{
		{Restart, "sshd"},
		{Restart, "../../etc/passwd"},
		{"stop", "caddy"},
	} {
		if err := c.Request(tc.a, tc.unit); err == nil {
			t.Errorf("%s %s: want an error", tc.a, tc.unit)
		}
	}
	entries, _ := os.ReadDir(filepath.Join(c.Dir, "requests"))
	if len(entries) != 0 {
		t.Errorf("rejected requests left %d files", len(entries))
	}
}

func TestStatus(t *testing.T) {
	c := newClient(t)
	s, err := c.Status()
	if err != nil || len(s.Units) != 0 {
		t.Fatalf("missing file: status %+v, err %v", s, err)
	}
	body := `{"updated":"2026-10-26T00:00:00Z","units":{"caddy":{"active":"active","sub":"running"}}}`
	if err := os.WriteFile(filepath.Join(c.Dir, "status.json"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	s, err = c.Status()
	if err != nil {
		t.Fatal(err)
	}
	if s.Units["caddy"].Sub != "running" {
		t.Errorf("status = %+v", s)
	}
}
