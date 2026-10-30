package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// authAgent is a test agent with a memory queue and two accounts: admin
// in wheel, and ops outside it.
func authAgent(t *testing.T) *testAgent {
	t.Helper()
	ta := newAgent(t)
	ta.AuthQueue = filepath.Join(t.TempDir(), "req")
	if err := os.MkdirAll(ta.AuthQueue, 0o700); err != nil {
		t.Fatal(err)
	}
	ta.host.answers["id -nG -- admin"] = "admin wheel sibyl-operators\n"
	ta.host.answers["id -nG -- ops"] = "ops sibyl-operators\n"
	return ta
}

func (ta *testAgent) check(t *testing.T, id, action, body string) (Result, string) {
	t.Helper()
	name := id + "." + action + ".json"
	if err := os.WriteFile(filepath.Join(ta.AuthQueue, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ta.ProcessAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(ta.AuthQueue); len(left) != 0 {
		t.Fatalf("queue not empty: %v", left)
	}
	b, err := os.ReadFile(filepath.Join(ta.Out, "auth", strings.TrimSuffix(name, ".json")+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var r Result
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	return r, string(b)
}

func TestPasswordCheck(t *testing.T) {
	ta := authAgent(t)
	r, raw := ta.check(t, "1", string(CheckPassword), `{"user":"admin","password":"s3cret pass"}`)
	if !r.OK {
		t.Fatalf("result %+v", r)
	}
	if strings.Contains(raw, "s3cret") {
		t.Error("the result holds the password")
	}
	if got := ta.host.stdin["unix_chkpwd admin nonull"]; got != "s3cret pass\x00" {
		t.Errorf("unix_chkpwd stdin %q", got)
	}
}

func TestPasswordCheckRefusals(t *testing.T) {
	long := strings.Repeat("x", maxPassword+1)
	for _, c := range []struct {
		name, action, body string
		chkpwd             bool // whether unix_chkpwd may run
		wrongPassword      bool
	}{
		{"wrong password", string(CheckPassword), `{"user":"admin","password":"nope"}`, true, true},
		{"not in wheel", string(CheckPassword), `{"user":"ops","password":"s3cret"}`, false, false},
		{"no such user", string(CheckPassword), `{"user":"ghost","password":"s3cret"}`, false, false},
		{"bad user name", string(CheckPassword), `{"user":"Admin;rm","password":"s3cret"}`, false, false},
		{"option as user", string(CheckPassword), `{"user":"-r","password":"s3cret"}`, false, false},
		{"empty password", string(CheckPassword), `{"user":"admin","password":""}`, false, false},
		{"NUL in password", string(CheckPassword), `{"user":"admin","password":"a\u0000b"}`, false, false},
		{"long password", string(CheckPassword), `{"user":"admin","password":"` + long + `"}`, false, false},
		{"unknown field", string(CheckPassword), `{"user":"admin","password":"s3cret","x":1}`, false, false},
		{"another action", string(SetHostname), `{"hostname":"evil"}`, false, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			ta := authAgent(t)
			if c.wrongPassword {
				ta.host.fail["unix_chkpwd"] = errors.New("unix_chkpwd: exit status 7")
			}
			r, _ := ta.check(t, "1", c.action, c.body)
			if r.OK {
				t.Fatalf("result %+v, want a refusal", r)
			}
			if r.Action == CheckPassword && c.action == string(CheckPassword) && r.Error != ErrDenied.Error() {
				t.Errorf("error %q, want %q for every refusal", r.Error, ErrDenied)
			}
			if !c.chkpwd && ta.host.ran("unix_chkpwd") {
				t.Error("unix_chkpwd ran")
			}
			if ta.host.ran("hostnamectl") {
				t.Error("the memory queue ran another action")
			}
		})
	}
}

// TestDiskQueueRefusesPasswordCheck makes sure a password check never runs
// from the disk queue.
func TestDiskQueueRefusesPasswordCheck(t *testing.T) {
	ta := authAgent(t)
	ta.host.answers["id -nG -- admin"] = "admin wheel\n"
	r := ta.submit(t, "1", CheckPassword, `{"user":"admin","password":"s3cret"}`)
	if r.OK || !strings.Contains(r.Error, "unknown action") {
		t.Errorf("result %+v", r)
	}
	if ta.host.ran("unix_chkpwd") || ta.host.ran("id -nG -- ") {
		t.Error("the disk queue checked a password")
	}
}

func TestAuthResultsExpire(t *testing.T) {
	ta := authAgent(t)
	ta.check(t, "1", string(CheckPassword), `{"user":"admin","password":"s3cret"}`)
	ta.now = ta.now.Add(2 * AuthResultTTL)
	old := filepath.Join(ta.Out, "auth", "1."+string(CheckPassword)+".json")
	if err := os.Chtimes(old, ta.now.Add(-2*AuthResultTTL), ta.now.Add(-2*AuthResultTTL)); err != nil {
		t.Fatal(err)
	}
	if err := ta.ProcessAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Errorf("an old result is kept: %v", err)
	}
}

func TestPasswordCheckLimits(t *testing.T) {
	ta := authAgent(t)
	var slept time.Duration
	ta.Sleep = func(d time.Duration) { slept += d }
	ta.host.fail["unix_chkpwd"] = errors.New("unix_chkpwd: exit status 7")
	for i := range FailsPerUser {
		if r, _ := ta.check(t, strconv.Itoa(i+1), string(CheckPassword), `{"user":"admin","password":"guess"}`); r.OK {
			t.Fatal("a wrong password passed")
		}
	}
	if slept != FailsPerUser*FailDelay {
		t.Errorf("slept %s, want %s", slept, FailsPerUser*FailDelay)
	}
	// Now even the right password is refused without a try.
	delete(ta.host.fail, "unix_chkpwd")
	ta.host.calls = nil
	if r, _ := ta.check(t, "100", string(CheckPassword), `{"user":"admin","password":"right"}`); r.OK {
		t.Fatal("a check passed over the limit")
	}
	if ta.host.ran("unix_chkpwd") {
		t.Error("unix_chkpwd ran over the limit")
	}
	// The count is in Data, so a new process keeps it, and it ends with
	// the window.
	ta.now = ta.now.Add(FailWindow)
	if r, _ := ta.check(t, "101", string(CheckPassword), `{"user":"admin","password":"right"}`); !r.OK {
		t.Errorf("after the window: %+v", r)
	}
}

func TestPasswordCheckExpiredAccount(t *testing.T) {
	ta := authAgent(t)
	// Expired on day 20000, 2024-10-04.
	ta.host.answers["getent shadow -- admin"] = "admin:$y$hash:20000:0:99999:7::20000:\n"
	if r, _ := ta.check(t, "1", string(CheckPassword), `{"user":"admin","password":"right"}`); r.OK {
		t.Error("an expired account passed")
	}
	if ta.host.ran("unix_chkpwd") {
		t.Error("unix_chkpwd ran for an expired account")
	}
	ta.host.answers["getent shadow -- admin"] = "admin:$y$hash:20000:0:99999:7::99999:\n"
	if r, _ := ta.check(t, "2", string(CheckPassword), `{"user":"admin","password":"right"}`); !r.OK {
		t.Errorf("an account that expires later: %+v", r)
	}
}

func TestPasswordCheckBadNameWritesNothing(t *testing.T) {
	ta := authAgent(t)
	if err := os.WriteFile(filepath.Join(ta.AuthQueue, "evil-name"), []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := ta.ProcessAuth(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ents, _ := os.ReadDir(filepath.Join(ta.Out, "auth")); len(ents) != 0 {
		t.Errorf("results %v, want none", ents)
	}
	if left, _ := os.ReadDir(ta.AuthQueue); len(left) != 0 {
		t.Errorf("queue %v, want empty", left)
	}
}
