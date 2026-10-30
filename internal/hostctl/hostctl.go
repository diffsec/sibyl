// Package hostctl asks the host agent to control Sibyl units.
//
// The System API runs in a container with no access to the systemd bus. It
// drops an empty request file into <run>/requests. The sibyl-agent.path user
// unit on the host sees it and runs /usr/libexec/sibyl/agent, which accepts
// only the units in Units. The agent writes unit states to <run>/status.json.
package hostctl

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"time"

	"github.com/diffsec/sibyl/internal/hostd"
)

// Units are the services that the API can restart. The host agent holds the
// same list and rejects anything else.
var Units = []string{"llama-swap", "router", "open-webui", "caddy", "arcane", "system-api"}

// Action is a request type.
type Action string

const (
	// Restart restarts one unit.
	Restart Action = "restart"
	// Reload runs daemon-reload, so Quadlet regenerates units from changed files.
	Reload Action = "reload"
	// Bench runs llama-bench with both backend images on one model.
	Bench Action = "bench"
)

// benchTarget matches the model IDs that the host agent accepts.
var benchTarget = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)

// UnitState is one unit's state as the agent last saw it.
type UnitState struct {
	Active string `json:"active"`
	Sub    string `json:"sub"`
	Since  string `json:"since,omitempty"`
}

// Status is the content of status.json.
type Status struct {
	Updated string               `json:"updated"`
	Units   map[string]UnitState `json:"units"`
}

// Client writes requests and reads status under one run directory.
type Client struct {
	Dir string
	now func() time.Time
}

// New returns a Client for dir.
func New(dir string) *Client {
	return &Client{Dir: dir, now: time.Now}
}

// Request queues one action. unit is ignored for Reload.
func (c *Client) Request(a Action, unit string) error {
	switch a {
	case Reload:
		unit = "all"
	case Restart:
		if !slices.Contains(Units, unit) {
			return fmt.Errorf("unit %q is not one of %v", unit, Units)
		}
	case Bench:
		if !benchTarget.MatchString(unit) {
			return fmt.Errorf("model ID %q is not valid for a benchmark", unit)
		}
	default:
		return fmt.Errorf("unknown action %q", a)
	}
	dir := filepath.Join(c.Dir, "requests")
	name := strconv.FormatInt(c.now().UnixNano(), 10) + "." + string(a) + "." + unit
	f, err := os.OpenFile(filepath.Join(dir, name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o640)
	if err != nil {
		return err
	}
	return f.Close()
}

// Status reads status.json. A missing file returns an empty Status.
func (c *Client) Status() (Status, error) {
	data, err := os.ReadFile(filepath.Join(c.Dir, "status.json"))
	if errors.Is(err, os.ErrNotExist) {
		return Status{Units: map[string]UnitState{}}, nil
	}
	if err != nil {
		return Status{}, err
	}
	var s Status
	if err := json.Unmarshal(data, &s); err != nil {
		return Status{}, fmt.Errorf("parse status.json: %w", err)
	}
	return s, nil
}

// HostClient queues privileged requests for sibyl-hostd and reads its
// results and state. See package hostd.
type HostClient struct {
	Queue string // the request queue, <run>/host
	// AuthQueue is the memory queue for password checks, /run/sibyl-auth/req.
	AuthQueue string
	Out       string // sibyl-hostd's state.json and results/, mounted read-only
	now       func() time.Time
	// Poll is how often Wait checks for a result.
	Poll time.Duration
}

// NewHost returns a HostClient.
func NewHost(queue, out string) *HostClient {
	return &HostClient{Queue: queue, Out: out, now: time.Now, Poll: 250 * time.Millisecond}
}

// Submit checks a request with the same rules as sibyl-hostd and queues it.
// It returns the request ID.
func (c *HostClient) Submit(a hostd.Action, body any) (string, error) {
	data, err := json.Marshal(body)
	if err != nil {
		return "", err
	}
	if _, err := hostd.Decode(a, data); err != nil {
		return "", &InvalidError{err}
	}
	id := strconv.FormatInt(c.now().UnixNano(), 10) + "." + string(a)
	// Write next to the queue, then rename, so sibyl-hostd never reads a
	// half-written request, and a failed write never starts it.
	tmp := filepath.Join(filepath.Dir(c.Queue), ".host-"+id)
	if err := os.WriteFile(tmp, data, 0o640); err != nil {
		return "", err
	}
	if err := os.Rename(tmp, filepath.Join(c.Queue, id+".json")); err != nil {
		_ = os.Remove(tmp)
		return "", err
	}
	return id, nil
}

// InvalidError is a request that fails validation.
type InvalidError struct{ Err error }

func (e *InvalidError) Error() string { return e.Err.Error() }
func (e *InvalidError) Unwrap() error { return e.Err }

// ErrPending means sibyl-hostd has not finished the request yet.
var ErrPending = errors.New("the request is still queued")

// Result reads the result of one request.
func (c *HostClient) Result(id string) (hostd.Result, error) {
	if !hostID.MatchString(id) {
		return hostd.Result{}, fmt.Errorf("invalid request ID %q", id)
	}
	var r hostd.Result
	data, err := os.ReadFile(filepath.Join(c.Out, "results", id+".json"))
	if errors.Is(err, os.ErrNotExist) {
		return r, ErrPending
	}
	if err != nil {
		return r, err
	}
	return r, json.Unmarshal(data, &r)
}

var hostID = regexp.MustCompile(`^[0-9]{1,20}\.[a-z-]{1,32}$`)

// Wait polls for a result until ctx ends. It returns ErrPending on timeout.
func (c *HostClient) Wait(ctx context.Context, id string) (hostd.Result, error) {
	t := time.NewTicker(c.Poll)
	defer t.Stop()
	for {
		r, err := c.Result(id)
		if !errors.Is(err, ErrPending) {
			return r, err
		}
		select {
		case <-ctx.Done():
			return r, ErrPending
		case <-t.C:
		}
	}
}

// State reads sibyl-hostd's state file.
func (c *HostClient) State() (hostd.State, error) {
	var st hostd.State
	data, err := os.ReadFile(filepath.Join(c.Out, "state.json"))
	if errors.Is(err, os.ErrNotExist) {
		return st, errors.New("sibyl-hostd has not written its state yet")
	}
	if err != nil {
		return st, err
	}
	return st, json.Unmarshal(data, &st)
}

// CheckPassword asks sibyl-hostd whether user is in the wheel group and
// password is right. The request goes to AuthQueue, which is in memory, so
// the password never reaches a disk. It returns hostd.ErrDenied for a wrong
// user or password, and ErrPending when no answer comes before ctx ends.
func (c *HostClient) CheckPassword(ctx context.Context, user, password string) error {
	if c.AuthQueue == "" {
		return errors.New("password checks are not configured")
	}
	p := hostd.PasswordCheck{User: user, Password: password}
	if err := p.Validate(); err != nil {
		return hostd.ErrDenied
	}
	data, err := json.Marshal(p)
	if err != nil {
		return err
	}
	id := strconv.FormatInt(c.now().UnixNano(), 10) + "." + string(hostd.CheckPassword)
	tmp := filepath.Join(filepath.Dir(c.AuthQueue), ".auth-"+id)
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if err := os.Rename(tmp, filepath.Join(c.AuthQueue, id+".json")); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	t := time.NewTicker(c.Poll)
	defer t.Stop()
	for {
		b, err := os.ReadFile(filepath.Join(c.Out, "auth", id+".json"))
		if err == nil {
			var r hostd.Result
			if err := json.Unmarshal(b, &r); err != nil {
				return err
			}
			if !r.OK {
				return hostd.ErrDenied
			}
			return nil
		}
		if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		select {
		case <-ctx.Done():
			// Take the request back if sibyl-hostd has not read it.
			_ = os.Remove(filepath.Join(c.AuthQueue, id+".json"))
			return ErrPending
		case <-t.C:
		}
	}
}
