package hostd

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ihavespoons/sibyl/internal/disks"
)

// Runner runs a command with stdin and returns its standard output.
type Runner func(ctx context.Context, stdin, name string, args ...string) (string, error)

// Agent processes the request queue.
type Agent struct {
	Queue string // request files, writable by sibyl
	Out   string // state.json and results/, written by root, readable by sibyl
	Data  string // records that only root reads and writes
	Run   Runner
	Now   func() time.Time
	Log   *slog.Logger
	// Label is the SELinux context for files in Out, so the System API
	// container can read them. Empty skips labelling.
	Label string
	// Group is the group that owns the files in Out, so the System API can
	// read them. -1 keeps the group of the process.
	Group int
	// Etc is /etc: crypttab, fstab and the pool key files.
	Etc string
	// PoolsDir is the root-owned directory where pools are mounted.
	PoolsDir string
}

// requestRE matches queue file names: <unix-nanoseconds>.<action>.json.
var requestRE = regexp.MustCompile(`^([0-9]{1,20})\.([a-z-]{1,32})\.json$`)

// ResultTTL is how long results stay in Out/results.
const ResultTTL = 24 * time.Hour

const (
	managedFile = "managed-users.json"
	pendingFile = "pending-network.json"
	nmBus       = "org.freedesktop.NetworkManager"
	nmPath      = "/org/freedesktop/NetworkManager"
	// checkpointDeleteNew makes a rollback delete connections that were
	// created after the checkpoint.
	checkpointDeleteNew = 2
)

// Process handles every queued request, oldest first, and then writes the
// state file.
func (a *Agent) Process(ctx context.Context) error {
	if err := a.prepareOut(); err != nil {
		return err
	}
	reqs, err := readQueue(a.Queue)
	if err != nil {
		return err
	}
	for _, r := range reqs {
		res := a.handle(ctx, r)
		if err := a.writeOut(filepath.Join("results", res.ID+".json"), res); err != nil {
			a.Log.Error("write result", "id", res.ID, "err", err)
		}
	}
	a.pruneResults()
	return a.WriteState(ctx)
}

// queued is one request file that was read and removed from the queue.
type queued struct {
	name string
	data []byte
	err  error
}

func (a *Agent) handle(ctx context.Context, q queued) Result {
	res := Result{ID: strings.TrimSuffix(q.name, ".json")}
	err := q.err
	if err == nil {
		m := requestRE.FindStringSubmatch(q.name)
		if m == nil {
			err = errors.New("invalid request name")
		} else {
			res.Action = Action(m[2])
			res.Message, res.Secret, err = a.do(ctx, res.Action, q.data)
		}
	}
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
		a.Log.Warn("request failed", "id", res.ID, "err", err)
	} else {
		a.Log.Info("request done", "id", res.ID, "message", res.Message)
	}
	res.Finished = a.Now().UTC().Format(time.RFC3339)
	return res
}

func (a *Agent) do(ctx context.Context, act Action, data []byte) (msg, secret string, err error) {
	body, err := Decode(act, data)
	if err != nil {
		return "", "", err
	}
	switch act {
	case SetHostname:
		h := body.(Hostname)
		if _, err := a.Run(ctx, "", "hostnamectl", "hostname", h.Hostname); err != nil {
			return "", "", err
		}
		msg = "hostname set to " + h.Hostname
	case SetNetwork:
		msg, err = a.setNetwork(ctx, body.(Network))
	case ConfirmNetwork:
		msg, err = a.confirmNetwork(ctx)
	case SetUser:
		msg, err = a.setUser(ctx, body.(User))
	case DeleteUser:
		msg, err = a.deleteUser(ctx, body.(UserName).Name)
	case CreatePool:
		return a.createPool(ctx, body.(CreatePoolReq))
	case DetachPool:
		msg, err = a.detachPool(ctx, body.(PoolName).Name)
	case Refresh:
		msg = "state refreshed"
	default:
		err = fmt.Errorf("unknown action %q", act)
	}
	return msg, "", err
}

// setNetwork changes one interface inside a NetworkManager checkpoint. If no
// confirm arrives before the rollback time ends, NetworkManager restores the
// old configuration by itself, so a wrong address cannot lock the user out.
func (a *Agent) setNetwork(ctx context.Context, n Network) (string, error) {
	if p, ok := a.pending(); ok {
		return "", fmt.Errorf("the change to %s waits for a confirm until %s: confirm it or wait for the rollback", p.Interface, p.Expires)
	}
	out, err := a.Run(ctx, "", "nmcli", "-t", "-f", "GENERAL.TYPE,GENERAL.CONNECTION", "device", "show", n.Interface)
	if err != nil {
		return "", fmt.Errorf("interface %s: %w", n.Interface, err)
	}
	dev := terse(out)
	if dev["GENERAL.TYPE"] != "ethernet" {
		return "", fmt.Errorf("interface %s is %q: only ethernet interfaces are supported", n.Interface, dev["GENERAL.TYPE"])
	}

	out, err = a.Run(ctx, "", "busctl", "call", nmBus, nmPath, nmBus, "CheckpointCreate", "aouu",
		"0", strconv.Itoa(n.Rollback()), strconv.Itoa(checkpointDeleteNew))
	if err != nil {
		return "", fmt.Errorf("create a NetworkManager checkpoint: %w", err)
	}
	cp, err := busObject(out)
	if err != nil {
		return "", err
	}
	rollback := func(cause error) error {
		if _, err := a.Run(ctx, "", "busctl", "call", nmBus, nmPath, nmBus, "CheckpointRollback", "o", cp); err != nil {
			return fmt.Errorf("%w (and the rollback failed: %v)", cause, err)
		}
		return fmt.Errorf("%w: the old configuration is back", cause)
	}

	conn := dev["GENERAL.CONNECTION"]
	if conn == "" {
		conn = "sibyl-" + n.Interface
		if _, err := a.Run(ctx, "", "nmcli", "connection", "add", "type", "ethernet", "ifname", n.Interface, "con-name", conn); err != nil {
			return "", rollback(err)
		}
	}
	args := []string{"connection", "modify", conn}
	if n.Method == "dhcp" {
		args = append(args, "ipv4.method", "auto", "ipv4.addresses", "", "ipv4.gateway", "", "ipv4.dns", "")
	} else {
		args = append(args, "ipv4.method", "manual", "ipv4.addresses", n.Address,
			"ipv4.gateway", n.Gateway, "ipv4.dns", strings.Join(n.DNS, ","))
	}
	if _, err := a.Run(ctx, "", "nmcli", args...); err != nil {
		return "", rollback(err)
	}
	if _, err := a.Run(ctx, "", "nmcli", "--wait", "30", "connection", "up", conn); err != nil {
		return "", rollback(err)
	}
	p := PendingNetwork{
		Interface:  n.Interface,
		Checkpoint: cp,
		Expires:    a.Now().Add(time.Duration(n.Rollback()) * time.Second).UTC().Format(time.RFC3339),
	}
	if err := a.writeData(pendingFile, p); err != nil {
		return "", rollback(err)
	}
	return fmt.Sprintf("%s changed to %s: confirm before %s, or it rolls back", n.Interface, n.Method, p.Expires), nil
}

func (a *Agent) confirmNetwork(ctx context.Context) (string, error) {
	p, ok := a.pending()
	if !ok {
		return "", errors.New("no network change waits for a confirm")
	}
	_ = os.Remove(filepath.Join(a.Data, pendingFile))
	if _, err := a.Run(ctx, "", "busctl", "call", nmBus, nmPath, nmBus, "CheckpointDestroy", "o", p.Checkpoint); err != nil {
		return "", fmt.Errorf("keep the change: %w", err)
	}
	return "network change on " + p.Interface + " kept", nil
}

// pending returns the network change that waits for a confirm. An expired
// record is removed, because NetworkManager already rolled it back.
func (a *Agent) pending() (PendingNetwork, bool) {
	var p PendingNetwork
	if !a.readData(pendingFile, &p) {
		return p, false
	}
	exp, err := time.Parse(time.RFC3339, p.Expires)
	if err != nil || !a.Now().Before(exp) {
		_ = os.Remove(filepath.Join(a.Data, pendingFile))
		return p, false
	}
	return p, true
}

// setUser creates an operator account, or replaces the SSH keys of an
// account that sibyl-hostd created.
func (a *Agent) setUser(ctx context.Context, u User) (string, error) {
	managed := a.managed()
	pw, exists, err := a.lookup(ctx, u.Name)
	if err != nil {
		return "", err
	}
	created := false
	switch {
	case exists && !slices.Contains(managed, u.Name):
		return "", fmt.Errorf("account %s exists and Sibyl did not create it: change it with usermod as an administrator", u.Name)
	case !exists:
		if _, err := a.Run(ctx, "", "useradd", "--create-home", "--user-group", "--groups", OperatorGroup,
			"--shell", "/bin/bash", "--comment", "Sibyl operator", u.Name); err != nil {
			return "", err
		}
		if err := a.writeData(managedFile, append(managed, u.Name)); err != nil {
			return "", err
		}
		created = true
		if pw, _, err = a.lookup(ctx, u.Name); err != nil {
			return "", err
		}
	default:
		if _, err := a.Run(ctx, "", "usermod", "--append", "--groups", OperatorGroup, u.Name); err != nil {
			return "", err
		}
	}
	home := pw[5]
	if !filepath.IsAbs(home) {
		return "", fmt.Errorf("account %s has no absolute home directory", u.Name)
	}
	// The user writes its own key file, so a symlink in its home cannot
	// redirect a write by root.
	const script = `umask 077 && mkdir -p "$1/.ssh" && cat > "$1/.ssh/.authorized_keys.sibyl" && mv -f "$1/.ssh/.authorized_keys.sibyl" "$1/.ssh/authorized_keys"`
	keys := strings.Join(u.Keys, "\n") + "\n"
	if _, err := a.Run(ctx, keys, "runuser", "-u", u.Name, "--", "/bin/sh", "-c", script, "sh", home); err != nil {
		return "", fmt.Errorf("write the SSH keys: %w", err)
	}
	if _, err := a.Run(ctx, "", "restorecon", "-R", filepath.Join(home, ".ssh")); err != nil {
		return "", err
	}
	if created {
		return fmt.Sprintf("created operator %s with %d SSH keys", u.Name, len(u.Keys)), nil
	}
	return fmt.Sprintf("set %d SSH keys for %s", len(u.Keys), u.Name), nil
}

func (a *Agent) deleteUser(ctx context.Context, name string) (string, error) {
	managed := a.managed()
	if !slices.Contains(managed, name) {
		return "", fmt.Errorf("account %s was not created by Sibyl, so Sibyl does not delete it", name)
	}
	if _, exists, err := a.lookup(ctx, name); err != nil {
		return "", err
	} else if exists {
		if _, err := a.Run(ctx, "", "userdel", "--remove", name); err != nil {
			return "", err
		}
	}
	managed = slices.DeleteFunc(managed, func(s string) bool { return s == name })
	if err := a.writeData(managedFile, managed); err != nil {
		return "", err
	}
	return "deleted operator " + name + " and its home directory", nil
}

// lookup returns the passwd fields of one account.
func (a *Agent) lookup(ctx context.Context, name string) ([]string, bool, error) {
	// getent prints nothing and exits 2 when the account does not exist.
	out, err := a.Run(ctx, "", "getent", "passwd", name)
	if strings.TrimSpace(out) == "" {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	f := strings.Split(strings.TrimSpace(out), ":")
	if len(f) != 7 || f[0] != name {
		return nil, false, fmt.Errorf("getent passwd %s: unexpected output", name)
	}
	return f, true, nil
}

func (a *Agent) managed() []string {
	var m []string
	a.readData(managedFile, &m)
	return m
}

func (a *Agent) readData(name string, v any) bool {
	b, err := os.ReadFile(filepath.Join(a.Data, name))
	return err == nil && json.Unmarshal(b, v) == nil
}

func (a *Agent) writeData(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(a.Data, name), b, 0o600, "", -1)
}

func (a *Agent) writeOut(name string, v any) error {
	b, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(filepath.Join(a.Out, name), append(b, '\n'), 0o640, a.Label, a.Group)
}

func (a *Agent) pruneResults() {
	dir := filepath.Join(a.Out, "results")
	entries, err := os.ReadDir(dir)
	if err != nil {
		return
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > ResultTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
}

// prepareOut creates Out/results and labels both directories, so the System
// API container can search them.
func (a *Agent) prepareOut() error {
	dir := filepath.Join(a.Out, "results")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if a.PoolsDir != "" {
		if err := os.MkdirAll(a.PoolsDir, 0o755); err != nil {
			return err
		}
		if a.Label != "" {
			// The containers list this directory to find the pools.
			if err := setLabel(a.PoolsDir, a.Label); err != nil {
				return fmt.Errorf("label %s: %w", a.PoolsDir, err)
			}
		}
	}
	for _, d := range []string{a.Out, dir} {
		if a.Group >= 0 {
			if err := os.Lchown(d, -1, a.Group); err != nil {
				return err
			}
		}
		if a.Label != "" {
			if err := setLabel(d, a.Label); err != nil {
				return fmt.Errorf("label %s: %w", d, err)
			}
		}
	}
	return nil
}

// WriteState writes Out/state.json.
func (a *Agent) WriteState(ctx context.Context) error {
	if err := a.prepareOut(); err != nil {
		return err
	}
	st := State{Updated: a.Now().UTC().Format(time.RFC3339)}
	var errs []error
	if out, err := a.Run(ctx, "", "hostnamectl", "hostname"); err != nil {
		errs = append(errs, err)
	} else {
		st.Hostname = strings.TrimSpace(out)
	}
	ifs, err := a.interfaces(ctx)
	errs = append(errs, err)
	st.Interfaces = ifs
	users, err := a.accounts(ctx)
	errs = append(errs, err)
	st.Users = users
	if p, ok := a.pending(); ok {
		st.Pending = &p
	}
	st.Pools = a.poolState(ctx)
	if out, err := a.Run(ctx, "", "lsblk", disks.LsblkArgs...); err != nil {
		errs = append(errs, err)
	} else if st.Disks, err = disks.Parse([]byte(out), nil, MinPoolDisk); err != nil {
		errs = append(errs, err)
	}
	for _, e := range errs {
		if e != nil {
			st.Errors = append(st.Errors, e.Error())
		}
	}
	return a.writeOut("state.json", st)
}

func (a *Agent) interfaces(ctx context.Context) ([]Interface, error) {
	out, err := a.Run(ctx, "", "nmcli", "-t", "-f",
		"GENERAL.DEVICE,GENERAL.TYPE,GENERAL.HWADDR,GENERAL.STATE,GENERAL.CONNECTION,IP4.ADDRESS,IP4.GATEWAY,IP4.DNS",
		"device", "show")
	if err != nil {
		return nil, err
	}
	var ifs []Interface
	for _, block := range strings.Split(strings.TrimSpace(out), "\n\n") {
		var it Interface
		for _, line := range strings.Split(block, "\n") {
			k, v, ok := strings.Cut(line, ":")
			if !ok || v == "" {
				continue
			}
			v = unescapeTerse(v)
			switch base, _, _ := strings.Cut(k, "["); base {
			case "GENERAL.DEVICE":
				it.Name = v
			case "GENERAL.TYPE":
				it.Type = v
			case "GENERAL.HWADDR":
				it.MAC = strings.ToLower(v)
			case "GENERAL.STATE":
				it.State = stateName(v)
			case "GENERAL.CONNECTION":
				it.Connection = v
			case "IP4.ADDRESS":
				it.Addresses = append(it.Addresses, v)
			case "IP4.GATEWAY":
				it.Gateway = v
			case "IP4.DNS":
				it.DNS = append(it.DNS, v)
			}
		}
		if it.Type != "ethernet" && it.Type != "wifi" {
			continue
		}
		if it.Connection != "" {
			if m, err := a.Run(ctx, "", "nmcli", "-g", "ipv4.method", "connection", "show", it.Connection); err == nil {
				switch strings.TrimSpace(m) {
				case "auto":
					it.Method = "dhcp"
				case "manual":
					it.Method = "static"
				default:
					it.Method = strings.TrimSpace(m)
				}
			}
		}
		ifs = append(ifs, it)
	}
	return ifs, nil
}

// stateName turns "100 (connected)" into "connected".
func stateName(v string) string {
	if i := strings.Index(v, "("); i >= 0 && strings.HasSuffix(v, ")") {
		return v[i+1 : len(v)-1]
	}
	return v
}

func (a *Agent) accounts(ctx context.Context) ([]Account, error) {
	out, err := a.Run(ctx, "", "getent", "passwd")
	if err != nil {
		return nil, err
	}
	managed := a.managed()
	var users []Account
	sc := bufio.NewScanner(strings.NewReader(out))
	for sc.Scan() {
		f := strings.Split(sc.Text(), ":")
		if len(f) != 7 {
			continue
		}
		uid, err := strconv.Atoi(f[2])
		if err != nil || uid < 1000 || uid >= 60000 || f[0] == "sibyl" {
			continue
		}
		u := Account{Name: f[0], UID: uid, Managed: slices.Contains(managed, f[0]), Keys: countKeys(f[5])}
		if g, err := a.Run(ctx, "", "id", "-nG", f[0]); err == nil {
			u.Groups = strings.Fields(g)
		}
		u.Operator = slices.Contains(u.Groups, OperatorGroup)
		u.Admin = slices.Contains(u.Groups, "wheel")
		users = append(users, u)
	}
	sort.Slice(users, func(i, j int) bool { return users[i].UID < users[j].UID })
	return users, nil
}

// terse parses "KEY:value" lines from nmcli -t.
func terse(out string) map[string]string {
	m := map[string]string{}
	for _, line := range strings.Split(out, "\n") {
		if k, v, ok := strings.Cut(line, ":"); ok {
			m[k] = unescapeTerse(v)
		}
	}
	return m
}

// unescapeTerse undoes nmcli's escaping of ":" and "\" in terse output.
func unescapeTerse(v string) string {
	return strings.NewReplacer(`\:`, ":", `\\`, `\`).Replace(v)
}

// busObject reads the object path from busctl output such as
// `o "/org/freedesktop/NetworkManager/Checkpoint/1"`.
func busObject(out string) (string, error) {
	f := strings.Fields(strings.TrimSpace(out))
	if len(f) != 2 || f[0] != "o" {
		return "", fmt.Errorf("unexpected busctl output %q", strings.TrimSpace(out))
	}
	p := strings.Trim(f[1], `"`)
	if !strings.HasPrefix(p, nmPath+"/Checkpoint/") {
		return "", fmt.Errorf("unexpected checkpoint path %q", p)
	}
	return p, nil
}
