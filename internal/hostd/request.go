// Package hostd holds the privileged host actions of Sibyl: the hostname, the
// network, and operator accounts with their SSH keys.
//
// The System API runs as the unprivileged sibyl user. It writes one JSON
// request per file into a queue directory. sibyl-hostd runs as root, reads
// each request, checks it against the rules in this file, runs it, and writes
// a result that the API can read. The System API uses the same checks to
// return clear errors, but only sibyl-hostd's checks protect the host.
//
// The actions never give the System API a path to root. sibyl-hostd creates
// accounts only in the sibyl-operators group, never in wheel, and it changes
// or deletes only the accounts that it created.
package hostd

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"regexp"
	"slices"
	"strings"
)

// Action is a request type.
type Action string

const (
	// SetHostname sets the static hostname.
	SetHostname Action = "hostname"
	// SetNetwork sets DHCP or a static IPv4 address on one interface. It
	// rolls back by itself unless ConfirmNetwork follows in time.
	SetNetwork Action = "network"
	// ConfirmNetwork keeps the last network change.
	ConfirmNetwork Action = "network-confirm"
	// SetUser creates an operator account, or replaces its SSH keys.
	SetUser Action = "user"
	// DeleteUser deletes an operator account that sibyl-hostd created.
	DeleteUser Action = "user-delete"
	// Refresh only rewrites the state file.
	Refresh Action = "state"
)

// Actions are every action that sibyl-hostd accepts.
var Actions = []Action{SetHostname, SetNetwork, ConfirmNetwork, SetUser, DeleteUser, Refresh}

// MaxRequestSize is the largest request file that sibyl-hostd reads.
const MaxRequestSize = 64 << 10

// OperatorGroup is the only group that sibyl-hostd adds accounts to.
const OperatorGroup = "sibyl-operators"

// Hostname is the body of a hostname request.
type Hostname struct {
	Hostname string `json:"hostname"`
}

// Network is the body of a network request.
type Network struct {
	Interface string   `json:"interface"`
	Method    string   `json:"method"`            // dhcp or static
	Address   string   `json:"address,omitempty"` // 192.168.1.26/24
	Gateway   string   `json:"gateway,omitempty"`
	DNS       []string `json:"dns,omitempty"`
	// RollbackSeconds is how long the change waits for a confirm before
	// NetworkManager restores the old configuration. 0 means 120.
	RollbackSeconds int `json:"rollback_seconds,omitempty"`
}

// User is the body of a user request.
type User struct {
	Name string   `json:"name"`
	Keys []string `json:"keys"`
}

// UserName is the body of a user-delete request.
type UserName struct {
	Name string `json:"name"`
}

var (
	hostnameRE = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	userRE     = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
	ifaceRE    = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,14}$`)
	// reservedUsers are names that sibyl-hostd never creates, changes or deletes.
	reservedUsers = []string{"root", "sibyl", "admin", "nobody", "nfsnobody", "core"}
)

// Validate checks a hostname request.
func (h Hostname) Validate() error {
	if !hostnameRE.MatchString(h.Hostname) {
		return fmt.Errorf("hostname %q: use 1 to 63 lowercase letters, digits and hyphens, not starting or ending with a hyphen", h.Hostname)
	}
	return nil
}

// Rollback returns the rollback time in seconds.
func (n Network) Rollback() int {
	if n.RollbackSeconds == 0 {
		return 120
	}
	return n.RollbackSeconds
}

// Validate checks a network request.
func (n Network) Validate() error {
	if !ifaceRE.MatchString(n.Interface) {
		return fmt.Errorf("interface %q is not a valid interface name", n.Interface)
	}
	if r := n.Rollback(); r < 30 || r > 600 {
		return fmt.Errorf("rollback_seconds %d: use 30 to 600", r)
	}
	switch n.Method {
	case "dhcp":
		if n.Address != "" || n.Gateway != "" || len(n.DNS) > 0 {
			return errors.New("dhcp takes no address, gateway or dns")
		}
		return nil
	case "static":
	default:
		return fmt.Errorf("method %q: use dhcp or static", n.Method)
	}
	p, err := netip.ParsePrefix(n.Address)
	if err != nil || !p.Addr().Is4() || p.Bits() < 8 || p.Bits() > 32 {
		return fmt.Errorf("address %q: use an IPv4 address with a prefix length, such as 192.168.1.26/24", n.Address)
	}
	if !usable(p.Addr()) {
		return fmt.Errorf("address %q is not a usable host address", n.Address)
	}
	if n.Gateway != "" {
		g, err := netip.ParseAddr(n.Gateway)
		if err != nil || !g.Is4() || !usable(g) {
			return fmt.Errorf("gateway %q is not a usable IPv4 address", n.Gateway)
		}
		if !p.Masked().Contains(g) {
			return fmt.Errorf("gateway %s is outside %s", g, p.Masked())
		}
		if g == p.Addr() {
			return errors.New("the gateway cannot be the host's own address")
		}
	}
	if len(n.DNS) > 3 {
		return errors.New("dns: use at most 3 servers")
	}
	for _, d := range n.DNS {
		a, err := netip.ParseAddr(d)
		if err != nil || !a.Is4() || !usable(a) {
			return fmt.Errorf("dns server %q is not a usable IPv4 address", d)
		}
	}
	return nil
}

func usable(a netip.Addr) bool {
	return !a.IsUnspecified() && !a.IsLoopback() && !a.IsMulticast() && a != netip.AddrFrom4([4]byte{255, 255, 255, 255})
}

// ValidateUserName checks that name is a valid account name that
// sibyl-hostd is allowed to manage.
func ValidateUserName(name string) error {
	if !userRE.MatchString(name) {
		return fmt.Errorf("user name %q: use up to 32 lowercase letters, digits, _ and -, starting with a letter or _", name)
	}
	if slices.Contains(reservedUsers, name) {
		return fmt.Errorf("user name %q is reserved", name)
	}
	return nil
}

// Validate checks a user request and returns the keys in canonical form.
func (u User) Validate() ([]string, error) {
	if err := ValidateUserName(u.Name); err != nil {
		return nil, err
	}
	if len(u.Keys) == 0 {
		return nil, errors.New("keys: add at least one SSH public key, because SSH accepts keys only")
	}
	if len(u.Keys) > 32 {
		return nil, errors.New("keys: use at most 32 keys")
	}
	out := make([]string, 0, len(u.Keys))
	for i, k := range u.Keys {
		c, err := ParseKey(k)
		if err != nil {
			return nil, fmt.Errorf("keys[%d]: %w", i, err)
		}
		if !slices.Contains(out, c) {
			out = append(out, c)
		}
	}
	return out, nil
}

var keyTypes = []string{
	"ssh-ed25519",
	"sk-ssh-ed25519@openssh.com",
	"ecdsa-sha2-nistp256",
	"ecdsa-sha2-nistp384",
	"ecdsa-sha2-nistp521",
	"sk-ecdsa-sha2-nistp256@openssh.com",
	"ssh-rsa",
}

// ParseKey checks one line in authorized_keys format and returns it as
// "<type> <base64> [comment]". It rejects key options such as command= or
// from=, because the line would then grant more than a login.
func ParseKey(line string) (string, error) {
	line = strings.Trim(line, " \t\r\n")
	for _, r := range line {
		if (r < 0x20 && r != '\t') || r == 0x7f {
			return "", errors.New("the key has control characters or more than one line")
		}
	}
	f := strings.Fields(line)
	if len(f) < 2 {
		return "", errors.New("not an SSH public key: expected \"<type> <base64> [comment]\"")
	}
	if !slices.Contains(keyTypes, f[0]) {
		return "", fmt.Errorf("key type %q is not allowed: use one of %s", truncate(f[0], 40), strings.Join(keyTypes, ", "))
	}
	blob, err := base64.StdEncoding.DecodeString(f[1])
	if err != nil {
		return "", errors.New("the key data is not valid base64")
	}
	// The blob starts with its own type as an SSH string: a 4-byte length,
	// then the name.
	if len(blob) < 4 {
		return "", errors.New("the key data is too short")
	}
	n := binary.BigEndian.Uint32(blob)
	if uint64(n) > uint64(len(blob)-4) || !bytes.Equal(blob[4:4+n], []byte(f[0])) {
		return "", fmt.Errorf("the key data is not a %s key", f[0])
	}
	if f[0] == "ssh-rsa" && len(blob) < 256+4+len(f[0]) {
		return "", errors.New("RSA keys must have at least 2048 bits")
	}
	out := f[0] + " " + f[1]
	if c := strings.Join(f[2:], " "); c != "" {
		out += " " + truncate(c, 200)
	}
	return out, nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

// Decode reads a request body for action a and checks it. It returns the
// decoded body.
func Decode(a Action, data []byte) (any, error) {
	dec := func(v any) error {
		d := json.NewDecoder(bytes.NewReader(data))
		d.DisallowUnknownFields()
		if err := d.Decode(v); err != nil {
			return fmt.Errorf("invalid %s request: %w", a, err)
		}
		return nil
	}
	switch a {
	case SetHostname:
		var h Hostname
		if err := dec(&h); err != nil {
			return nil, err
		}
		return h, h.Validate()
	case SetNetwork:
		var n Network
		if err := dec(&n); err != nil {
			return nil, err
		}
		return n, n.Validate()
	case SetUser:
		var u User
		if err := dec(&u); err != nil {
			return nil, err
		}
		keys, err := u.Validate()
		u.Keys = keys
		return u, err
	case DeleteUser:
		var u UserName
		if err := dec(&u); err != nil {
			return nil, err
		}
		return u, ValidateUserName(u.Name)
	case ConfirmNetwork, Refresh:
		return nil, nil
	}
	return nil, fmt.Errorf("unknown action %q", a)
}

// Result is the outcome of one request.
type Result struct {
	ID       string `json:"id"`
	Action   Action `json:"action"`
	OK       bool   `json:"ok"`
	Error    string `json:"error,omitempty"`
	Message  string `json:"message,omitempty"`
	Finished string `json:"finished"`
}

// State is what sibyl-hostd last saw on the host.
type State struct {
	Updated    string          `json:"updated"`
	Hostname   string          `json:"hostname"`
	Interfaces []Interface     `json:"interfaces"`
	Users      []Account       `json:"users"`
	Pending    *PendingNetwork `json:"pending_network,omitempty"`
	Errors     []string        `json:"errors,omitempty"`
}

// Interface is one physical network interface.
type Interface struct {
	Name       string   `json:"name"`
	Type       string   `json:"type"`
	MAC        string   `json:"mac"`
	State      string   `json:"state"`
	Connection string   `json:"connection,omitempty"`
	Method     string   `json:"method,omitempty"` // dhcp, static, or empty
	Addresses  []string `json:"addresses,omitempty"`
	Gateway    string   `json:"gateway,omitempty"`
	DNS        []string `json:"dns,omitempty"`
}

// Account is one login account.
type Account struct {
	Name     string   `json:"name"`
	UID      int      `json:"uid"`
	Groups   []string `json:"groups"`
	Managed  bool     `json:"managed"`
	Operator bool     `json:"operator"`
	Admin    bool     `json:"admin"`
	Keys     int      `json:"keys"`
}

// PendingNetwork is a network change that rolls back unless it is confirmed.
type PendingNetwork struct {
	Interface  string `json:"interface"`
	Checkpoint string `json:"checkpoint"`
	Expires    string `json:"expires"`
}
