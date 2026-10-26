// Package hostctl asks the host agent to control Sibyl units.
//
// The System API runs in a container with no access to the systemd bus. It
// drops an empty request file into <run>/requests. The sibyl-agent.path user
// unit on the host sees it and runs /usr/libexec/sibyl/agent, which accepts
// only the units in Units. The agent writes unit states to <run>/status.json.
package hostctl

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"time"
)

// Units are the services that the API can restart. The host agent holds the
// same list and rejects anything else.
var Units = []string{"llama-swap", "open-webui", "caddy", "arcane", "system-api"}

// Action is a request type.
type Action string

const (
	// Restart restarts one unit.
	Restart Action = "restart"
	// Reload runs daemon-reload, so Quadlet regenerates units from changed files.
	Reload Action = "reload"
)

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
