package hostd

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"
)

// The online update actions. bootc downloads from the registry, and the
// sibyl-hostd unit has no network, so each action starts a unit that runs
// bootc, and returns at once. Each unit refreshes the state when it ends.
const (
	// UpdateCheck asks the registry whether a newer image exists.
	UpdateCheck Action = "update-check"
	// UpdateApply downloads and stages the newer image. With Reboot it
	// also reboots into it.
	UpdateApply Action = "update-apply"
	// Reboot restarts the host after 5 seconds, so the result is written
	// first.
	Reboot Action = "reboot"
)

// The units that run bootc.
const (
	CheckUnit        = "sibyl-upgrade-check.service"
	DownloadUnit     = "sibyl-upgrade.service"
	DownloadBootUnit = "sibyl-upgrade-reboot.service"
)

// UpdateRequest is the body of an update-apply request.
type UpdateRequest struct {
	Reboot bool `json:"reboot"`
}

// UnitRun is the state of one update unit.
type UnitRun struct {
	Active string `json:"active"`
	Result string `json:"result"`
}

// UpdateState is what bootc reports about the host image.
type UpdateState struct {
	Image        string `json:"image,omitempty"`
	Booted       string `json:"booted,omitempty"`
	BootedDigest string `json:"booted_digest,omitempty"`
	// Staged is the version that the next boot starts.
	Staged string `json:"staged,omitempty"`
	// Available is a newer version that the last check found, if any.
	Available string `json:"available,omitempty"`
	// Check, Download and DownloadReboot are the states of the units.
	Check          UnitRun `json:"check"`
	Download       UnitRun `json:"download"`
	DownloadReboot UnitRun `json:"download_reboot"`
}

// bootcStatus is the part of bootc status --format=json that Sibyl reads.
type bootcStatus struct {
	Status struct {
		Booted *bootcEntry `json:"booted"`
		Staged *bootcEntry `json:"staged"`
	} `json:"status"`
}

type bootcEntry struct {
	Image *struct {
		Image struct {
			Image string `json:"image"`
		} `json:"image"`
		Version string `json:"version"`
		Digest  string `json:"imageDigest"`
	} `json:"image"`
	CachedUpdate *struct {
		Version string `json:"version"`
		Digest  string `json:"imageDigest"`
	} `json:"cachedUpdate"`
}

func (a *Agent) unitRun(ctx context.Context, unit string) UnitRun {
	out, err := a.Run(ctx, "", "systemctl", "show", "-p", "ActiveState", "-p", "Result", "--", unit)
	var r UnitRun
	if err != nil {
		return r
	}
	for _, line := range strings.Split(out, "\n") {
		k, v, _ := strings.Cut(strings.TrimSpace(line), "=")
		switch k {
		case "ActiveState":
			r.Active = v
		case "Result":
			r.Result = v
		}
	}
	return r
}

// bootcStatusWait limits bootc status. It waits for the lock that a
// running bootc upgrade holds, and the result of a request waits for the
// state, so a long wait would hold back the answer to the update request.
const bootcStatusWait = 5 * time.Second

// updateState reads bootc's status and the update units. When bootc is
// busy, it keeps the versions of the last state.
func (a *Agent) updateState(ctx context.Context) (*UpdateState, error) {
	u := &UpdateState{
		Check:          a.unitRun(ctx, CheckUnit),
		Download:       a.unitRun(ctx, DownloadUnit),
		DownloadReboot: a.unitRun(ctx, DownloadBootUnit),
	}
	sctx, cancel := context.WithTimeout(ctx, bootcStatusWait)
	defer cancel()
	out, err := a.Run(sctx, "", "bootc", "status", "--format=json")
	if err != nil {
		var last State
		if b, rerr := os.ReadFile(filepath.Join(a.Out, "state.json")); rerr == nil && json.Unmarshal(b, &last) == nil && last.Update != nil {
			l := last.Update
			u.Image, u.Booted, u.BootedDigest, u.Staged, u.Available = l.Image, l.Booted, l.BootedDigest, l.Staged, l.Available
			return u, nil
		}
		return u, err
	}
	var bs bootcStatus
	if err := json.Unmarshal([]byte(out), &bs); err != nil {
		return u, err
	}
	staged := ""
	if s := bs.Status.Staged; s != nil && s.Image != nil {
		u.Staged, staged = s.Image.Version, s.Image.Digest
	}
	if b := bs.Status.Booted; b != nil && b.Image != nil {
		u.Image, u.Booted, u.BootedDigest = b.Image.Image.Image, b.Image.Version, b.Image.Digest
		if c := b.CachedUpdate; c != nil && c.Digest != b.Image.Digest && c.Digest != staged {
			u.Available = c.Version
			if u.Available == "" {
				u.Available = c.Digest
			}
		}
	}
	return u, nil
}

// update runs an update action.
func (a *Agent) update(ctx context.Context, act Action, body any) (string, error) {
	switch act {
	case UpdateCheck:
		_, err := a.Run(ctx, "", "systemctl", "start", "--no-block", "--", CheckUnit)
		return "update check started", err
	case UpdateApply:
		unit, msg := DownloadUnit, "update download started; reboot when it is staged"
		if body.(UpdateRequest).Reboot {
			unit, msg = DownloadBootUnit, "update download started; the host reboots when it is staged"
		}
		_, err := a.Run(ctx, "", "systemctl", "start", "--no-block", "--", unit)
		return msg, err
	case Reboot:
		_, err := a.Run(ctx, "", "systemd-run", "--on-active=5s", "--unit=sibyl-reboot", "--", "/usr/bin/systemctl", "reboot")
		return "the host reboots in 5 seconds", err
	}
	return "", nil
}
