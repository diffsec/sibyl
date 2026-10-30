package hostctl

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/hostd"
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

// fakeHostd answers each password check in the memory queue as sibyl-hostd
// would, with ok for admin and right.
func fakeHostd(t *testing.T, c *HostClient, stop <-chan struct{}) {
	t.Helper()
	go func() {
		for {
			select {
			case <-stop:
				return
			case <-time.After(5 * time.Millisecond):
			}
			ents, _ := os.ReadDir(c.AuthQueue)
			for _, e := range ents {
				p := filepath.Join(c.AuthQueue, e.Name())
				b, err := os.ReadFile(p)
				_ = os.Remove(p)
				if err != nil {
					continue
				}
				pc, err := hostd.DecodePasswordCheck(b)
				ok := err == nil && pc.User == "admin" && pc.Password == "right"
				res, _ := json.Marshal(hostd.Result{ID: strings.TrimSuffix(e.Name(), ".json"), OK: ok})
				_ = os.WriteFile(filepath.Join(c.Out, "auth", e.Name()), res, 0o600)
			}
		}
	}()
}

func TestCheckPassword(t *testing.T) {
	dir := t.TempDir()
	c := NewHost(filepath.Join(dir, "host"), filepath.Join(dir, "out"))
	c.AuthQueue, c.Poll = filepath.Join(dir, "auth", "req"), 5*time.Millisecond
	for _, d := range []string{c.AuthQueue, filepath.Join(c.Out, "auth")} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	stop := make(chan struct{})
	fakeHostd(t, c, stop)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := c.CheckPassword(ctx, "admin", "right"); err != nil {
		t.Errorf("right password: %v", err)
	}
	if err := c.CheckPassword(ctx, "admin", "wrong"); !errors.Is(err, hostd.ErrDenied) {
		t.Errorf("wrong password: %v", err)
	}
	// A request that fails the form check never reaches the queue.
	if err := c.CheckPassword(ctx, "-r", "x"); !errors.Is(err, hostd.ErrDenied) {
		t.Errorf("bad user: %v", err)
	}
	// With no answer, the request is taken back from the queue.
	close(stop)
	time.Sleep(20 * time.Millisecond) // let the fake finish its last pass
	short, cancel2 := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel2()
	if err := c.CheckPassword(short, "admin", "right"); !errors.Is(err, ErrPending) {
		t.Errorf("no answer: %v", err)
	}
	if left, _ := os.ReadDir(c.AuthQueue); len(left) != 0 {
		t.Errorf("a request is left in the queue: %v", left)
	}
}
