package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const edKey = "ssh-ed25519 AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl ben@laptop"

func TestParseKey(t *testing.T) {
	got, err := ParseKey("  " + edKey + "  ")
	if err != nil || got != edKey {
		t.Fatalf("ParseKey = %q, %v", got, err)
	}
	for name, line := range map[string]string{
		"options":     `command="/bin/sh" ` + edKey,
		"type":        "ssh-dss AAAAB3NzaC1kc3MAAACBAP",
		"base64":      "ssh-ed25519 !!!!",
		"mismatch":    "ssh-rsa AAAAC3NzaC1lZDI1NTE5AAAAIOMqqnkVzrm0SdG6UOoqKLsabgH5C9okWi0dh2l9GKJl",
		"one field":   "ssh-ed25519",
		"control":     edKey + "\x1b[2J",
		"short blob":  "ssh-ed25519 AAAA",
		"newline key": edKey + "\nssh-ed25519 AAAA",
	} {
		if _, err := ParseKey(line); err == nil {
			t.Errorf("%s: ParseKey(%q) accepted", name, line)
		}
	}
}

func TestNetworkValidate(t *testing.T) {
	ok := []Network{
		{Interface: "enp8s0", Method: "dhcp"},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26/24", Gateway: "192.168.1.1", DNS: []string{"1.1.1.1"}},
		{Interface: "enp8s0", Method: "static", Address: "10.0.0.5/8", RollbackSeconds: 600},
	}
	for _, n := range ok {
		if err := n.Validate(); err != nil {
			t.Errorf("%+v: %v", n, err)
		}
	}
	bad := []Network{
		{Interface: "enp8s0; reboot", Method: "dhcp"},
		{Interface: "enp8s0", Method: "dhcp", Address: "192.168.1.26/24"},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26"},
		{Interface: "enp8s0", Method: "static", Address: "fe80::1/64"},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26/24", Gateway: "10.0.0.1"},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26/24", Gateway: "192.168.1.26"},
		{Interface: "enp8s0", Method: "static", Address: "127.0.0.2/8"},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26/24", DNS: []string{"x"}},
		{Interface: "enp8s0", Method: "static", Address: "192.168.1.26/24", RollbackSeconds: 5},
		{Interface: "enp8s0", Method: "pppoe"},
	}
	for _, n := range bad {
		if err := n.Validate(); err == nil {
			t.Errorf("%+v: accepted", n)
		}
	}
}

func TestDecodeRejects(t *testing.T) {
	for _, c := range []struct {
		a    Action
		body string
	}{
		{SetHostname, `{"hostname":"Bad_Name"}`},
		{SetHostname, `{"hostname":"ok","extra":1}`},
		{SetUser, `{"name":"root","keys":["` + edKey + `"]}`},
		{SetUser, `{"name":"admin","keys":["` + edKey + `"]}`},
		{SetUser, `{"name":"ops","keys":[]}`},
		{DeleteUser, `{"name":"../etc"}`},
		{Action("shell"), `{}`},
	} {
		if _, err := Decode(c.a, []byte(c.body)); err == nil {
			t.Errorf("%s %s: accepted", c.a, c.body)
		}
	}
}

// fakeHost records commands and answers them from a table.
type fakeHost struct {
	calls   []string
	stdin   map[string]string
	users   map[string]string // name -> passwd line
	answers map[string]string // command prefix -> output
	fail    map[string]error
}

func newFakeHost() *fakeHost {
	return &fakeHost{
		stdin: map[string]string{},
		users: map[string]string{
			"admin": "admin:x:1000:1000::/home/admin:/bin/bash",
			"sibyl": "sibyl:x:1001:1001:Sibyl services:/var/lib/sibyl/home:/usr/sbin/nologin",
		},
		answers: map[string]string{
			"hostnamectl hostname": "sibyl\n",
			"nmcli -t -f GENERAL.TYPE,GENERAL.CONNECTION device show enp8s0": "GENERAL.TYPE:ethernet\nGENERAL.CONNECTION:enp8s0\n",
			"nmcli -t -f GENERAL.DEVICE": "GENERAL.DEVICE:enp8s0\nGENERAL.TYPE:ethernet\nGENERAL.HWADDR:30:56:0F:AA:DB:23\n" +
				"GENERAL.STATE:100 (connected)\nGENERAL.CONNECTION:enp8s0\nIP4.ADDRESS[1]:192.168.1.26/24\n" +
				"IP4.GATEWAY:192.168.1.1\nIP4.DNS[1]:1.1.1.1\n\nGENERAL.DEVICE:lo\nGENERAL.TYPE:loopback\n",
			"nmcli -g ipv4.method connection show enp8s0": "auto\n",
			"busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager org.freedesktop.NetworkManager CheckpointCreate": `o "/org/freedesktop/NetworkManager/Checkpoint/7"` + "\n",
			"id -nG admin": "admin wheel sibyl-operators\n",
			"id -nG ops":   "ops sibyl-operators\n",
		},
		fail: map[string]error{},
	}
}

func (f *fakeHost) run(_ context.Context, stdin, name string, args ...string) (string, error) {
	cmd := strings.Join(append([]string{name}, args...), " ")
	f.calls = append(f.calls, cmd)
	if stdin != "" {
		f.stdin[cmd] = stdin
	}
	for prefix, err := range f.fail {
		if strings.HasPrefix(cmd, prefix) {
			return "", err
		}
	}
	switch {
	case cmd == "getent passwd":
		var lines []string
		for _, l := range f.users {
			lines = append(lines, l)
		}
		return strings.Join(lines, "\n") + "\n", nil
	case strings.HasPrefix(cmd, "getent passwd "):
		if l, ok := f.users[args[1]]; ok {
			return l + "\n", nil
		}
		return "", errors.New("getent: exit status 2")
	case strings.HasPrefix(cmd, "useradd "):
		n := args[len(args)-1]
		f.users[n] = n + ":x:1002:1002:Sibyl operator:/home/" + n + ":/bin/bash"
	case strings.HasPrefix(cmd, "userdel "):
		delete(f.users, args[len(args)-1])
	}
	for prefix, out := range f.answers {
		if strings.HasPrefix(cmd, prefix) {
			return out, nil
		}
	}
	return "", nil
}

func (f *fakeHost) ran(prefix string) bool {
	for _, c := range f.calls {
		if strings.HasPrefix(c, prefix) {
			return true
		}
	}
	return false
}

type testAgent struct {
	*Agent
	host *fakeHost
	now  time.Time
}

func newAgent(t *testing.T) *testAgent {
	t.Helper()
	dir := t.TempDir()
	ta := &testAgent{host: newFakeHost(), now: time.Date(2026, 10, 27, 3, 0, 0, 0, time.UTC)}
	ta.Agent = &Agent{
		Queue: filepath.Join(dir, "queue"),
		Out:   filepath.Join(dir, "out"),
		Data:  dir,
		Run:   ta.host.run,
		Now:   func() time.Time { return ta.now },
		Log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Group: -1,
	}
	if err := os.MkdirAll(ta.Queue, 0o750); err != nil {
		t.Fatal(err)
	}
	return ta
}

func (ta *testAgent) submit(t *testing.T, id string, a Action, body string) Result {
	t.Helper()
	if err := os.WriteFile(filepath.Join(ta.Queue, id+"."+string(a)+".json"), []byte(body), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ta.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	var r Result
	b, err := os.ReadFile(filepath.Join(ta.Out, "results", id+"."+string(a)+".json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	if left, _ := os.ReadDir(ta.Queue); len(left) != 0 {
		t.Fatalf("queue not empty: %v", left)
	}
	return r
}

func (ta *testAgent) state(t *testing.T) State {
	t.Helper()
	var st State
	b, err := os.ReadFile(filepath.Join(ta.Out, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(b, &st); err != nil {
		t.Fatal(err)
	}
	return st
}

func TestHostname(t *testing.T) {
	ta := newAgent(t)
	r := ta.submit(t, "1", SetHostname, `{"hostname":"sibyl-02"}`)
	if !r.OK || !ta.host.ran("hostnamectl hostname sibyl-02") {
		t.Fatalf("result %+v, calls %v", r, ta.host.calls)
	}
	st := ta.state(t)
	if st.Hostname != "sibyl" || len(st.Interfaces) != 1 || st.Interfaces[0].Method != "dhcp" ||
		st.Interfaces[0].MAC != "30:56:0f:aa:db:23" || st.Interfaces[0].State != "connected" {
		t.Fatalf("state %+v", st)
	}
}

func TestInvalidRequestRunsNothing(t *testing.T) {
	ta := newAgent(t)
	r := ta.submit(t, "1", SetHostname, `{"hostname":"x; reboot"}`)
	if r.OK || ta.host.ran("hostnamectl hostname x") {
		t.Fatalf("result %+v, calls %v", r, ta.host.calls)
	}
}

func TestNetworkConfirm(t *testing.T) {
	ta := newAgent(t)
	r := ta.submit(t, "1", SetNetwork, `{"interface":"enp8s0","method":"static","address":"192.168.1.30/24","gateway":"192.168.1.1","dns":["1.1.1.1","9.9.9.9"]}`)
	if !r.OK {
		t.Fatalf("network: %+v", r)
	}
	if !ta.host.ran("nmcli connection modify enp8s0 ipv4.method manual ipv4.addresses 192.168.1.30/24 ipv4.gateway 192.168.1.1 ipv4.dns 1.1.1.1,9.9.9.9") ||
		!ta.host.ran("nmcli --wait 30 connection up enp8s0") ||
		!ta.host.ran("busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager org.freedesktop.NetworkManager CheckpointCreate aouu 0 120 2") {
		t.Fatalf("calls %v", ta.host.calls)
	}
	if p := ta.state(t).Pending; p == nil || p.Checkpoint != "/org/freedesktop/NetworkManager/Checkpoint/7" {
		t.Fatalf("pending %+v", p)
	}

	// A second change waits for the first.
	if r := ta.submit(t, "2", SetNetwork, `{"interface":"enp8s0","method":"dhcp"}`); r.OK {
		t.Fatal("second change accepted while one is pending")
	}

	r = ta.submit(t, "3", ConfirmNetwork, `{}`)
	if !r.OK || !ta.host.ran("busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager org.freedesktop.NetworkManager CheckpointDestroy o /org/freedesktop/NetworkManager/Checkpoint/7") {
		t.Fatalf("confirm %+v, calls %v", r, ta.host.calls)
	}
	if ta.state(t).Pending != nil {
		t.Fatal("pending after confirm")
	}
	if r := ta.submit(t, "4", ConfirmNetwork, `{}`); r.OK {
		t.Fatal("second confirm accepted")
	}
}

func TestNetworkExpires(t *testing.T) {
	ta := newAgent(t)
	if r := ta.submit(t, "1", SetNetwork, `{"interface":"enp8s0","method":"dhcp","rollback_seconds":30}`); !r.OK {
		t.Fatalf("network: %+v", r)
	}
	ta.now = ta.now.Add(31 * time.Second)
	if r := ta.submit(t, "2", ConfirmNetwork, `{}`); r.OK {
		t.Fatal("confirm accepted after the rollback time")
	}
}

func TestNetworkRollsBackOnFailure(t *testing.T) {
	ta := newAgent(t)
	ta.host.fail["nmcli --wait 30 connection up"] = errors.New("activation failed")
	r := ta.submit(t, "1", SetNetwork, `{"interface":"enp8s0","method":"dhcp"}`)
	if r.OK || !strings.Contains(r.Error, "old configuration is back") ||
		!ta.host.ran("busctl call org.freedesktop.NetworkManager /org/freedesktop/NetworkManager org.freedesktop.NetworkManager CheckpointRollback o /org/freedesktop/NetworkManager/Checkpoint/7") {
		t.Fatalf("result %+v, calls %v", r, ta.host.calls)
	}
	if ta.state(t).Pending != nil {
		t.Fatal("pending after a rollback")
	}
}

func TestUsers(t *testing.T) {
	ta := newAgent(t)
	body := `{"name":"ops","keys":["` + edKey + `","` + edKey + `"]}`
	r := ta.submit(t, "1", SetUser, body)
	if !r.OK || !ta.host.ran("useradd --create-home --user-group --groups sibyl-operators --shell /bin/bash --comment Sibyl operator ops") {
		t.Fatalf("result %+v, calls %v", r, ta.host.calls)
	}
	var keys string
	for cmd, in := range ta.host.stdin {
		if strings.HasPrefix(cmd, "runuser -u ops -- /bin/sh -c ") && strings.HasSuffix(cmd, " sh /home/ops") {
			keys = in
		}
	}
	if keys != edKey+"\n" {
		t.Fatalf("keys written %q", keys)
	}
	for _, c := range ta.host.calls {
		if strings.Contains(c, "wheel") {
			t.Fatalf("a command named wheel: %s", c)
		}
	}
	st := ta.state(t)
	if len(st.Users) != 2 || st.Users[1].Name != "ops" || !st.Users[1].Managed || !st.Users[1].Operator || st.Users[0].Managed || !st.Users[0].Admin {
		t.Fatalf("users %+v", st.Users)
	}

	// Updating keys does not create the account again.
	ta.host.calls = nil
	if r := ta.submit(t, "2", SetUser, body); !r.OK || ta.host.ran("useradd") {
		t.Fatalf("update %+v, calls %v", r, ta.host.calls)
	}

	if r := ta.submit(t, "3", DeleteUser, `{"name":"ops"}`); !r.OK || !ta.host.ran("userdel --remove ops") {
		t.Fatalf("delete %+v, calls %v", r, ta.host.calls)
	}
	if len(ta.state(t).Users) != 1 {
		t.Fatal("ops still listed")
	}
}

func TestUnmanagedUsersAreRefused(t *testing.T) {
	ta := newAgent(t)
	ta.host.users["alice"] = "alice:x:1005:1005::/home/alice:/bin/bash"
	if r := ta.submit(t, "1", SetUser, `{"name":"alice","keys":["`+edKey+`"]}`); r.OK || ta.host.ran("runuser") {
		t.Fatalf("set %+v, calls %v", r, ta.host.calls)
	}
	if r := ta.submit(t, "2", DeleteUser, `{"name":"alice"}`); r.OK || ta.host.ran("userdel") {
		t.Fatalf("delete %+v, calls %v", r, ta.host.calls)
	}
}

func TestBadQueueNames(t *testing.T) {
	ta := newAgent(t)
	if err := os.WriteFile(filepath.Join(ta.Queue, "x.hostname.json"), []byte(`{"hostname":"a"}`), 0o640); err != nil {
		t.Fatal(err)
	}
	if err := ta.Process(context.Background()); err != nil {
		t.Fatal(err)
	}
	if ta.host.ran("hostnamectl hostname a") {
		t.Fatal("ran a request with an invalid name")
	}
	if left, _ := os.ReadDir(ta.Queue); len(left) != 0 {
		t.Fatalf("queue not empty: %v", left)
	}
}
