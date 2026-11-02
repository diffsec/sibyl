package hostd

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/diffsec/sibyl/internal/diag"
)

// The diagnostics actions run sibyl-diag as root. sibyl-hostd only builds
// the command line from a checked request. sibyl-diag checks the consent and
// the endpoint again before an upload.
const (
	// DiagBundle makes a redacted bundle from an incident, or from a fresh
	// capture when the request names none.
	DiagBundle Action = "diag-bundle"
	// DiagSubmit uploads one bundle. It needs upload consent.
	DiagSubmit Action = "diag-submit"
	// DiagConsent sets the upload consent mode. The endpoint cannot change
	// here: only an administrator sets it, with sudo sibyl-diag consent.
	DiagConsent Action = "diag-consent"
	// SetSafeMode sets or clears safe mode.
	SetSafeMode Action = "safe-mode"
)

// DiagBin is the sibyl-diag binary on the host.
const DiagBin = "/usr/sbin/sibyl-diag"

// MaxNote is the longest bundle note in bytes.
const MaxNote = 2000

// incidentRE matches incident directory names, such as
// 20261031T103035Z-generation-stalled.
var incidentRE = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z-[a-z0-9-]{1,48}$`)

// DiagBundleReq is the body of a diag-bundle request. Incident and Latest
// cannot both be set. With neither, sibyl-diag captures a new incident first.
type DiagBundleReq struct {
	Incident string `json:"incident,omitempty"`
	Latest   bool   `json:"latest,omitempty"`
	Note     string `json:"note,omitempty"`
}

// Validate checks a diag-bundle request.
func (b DiagBundleReq) Validate() error {
	if b.Incident != "" && b.Latest {
		return errors.New("use incident or latest, not both")
	}
	if b.Incident != "" && !incidentRE.MatchString(b.Incident) {
		return fmt.Errorf("incident %q is not an incident name, such as 20261031T103035Z-generation-stalled", truncate(b.Incident, 80))
	}
	if len(b.Note) > MaxNote {
		return fmt.Errorf("note: use at most %d bytes", MaxNote)
	}
	if !utf8.ValidString(b.Note) {
		return errors.New("note: use UTF-8 text")
	}
	for _, r := range b.Note {
		if (r < 0x20 && r != '\n' && r != '\t') || r == 0x7f {
			return errors.New("note: control characters are not allowed")
		}
	}
	return nil
}

// DiagSubmitReq is the body of a diag-submit request.
type DiagSubmitReq struct {
	ID string `json:"diagnostic_id"`
}

// Validate checks a diag-submit request.
func (s DiagSubmitReq) Validate() error {
	if !diag.ValidID(s.ID) {
		return fmt.Errorf("%q is not a diagnostic ID, such as SBL-WVBN-18HN", truncate(s.ID, 40))
	}
	return nil
}

// DiagConsentReq is the body of a diag-consent request.
type DiagConsentReq struct {
	Mode string `json:"mode"`
}

// Validate checks a diag-consent request.
func (c DiagConsentReq) Validate() error {
	switch c.Mode {
	case diag.ConsentOff, diag.ConsentManual, diag.ConsentAutomatic:
		return nil
	}
	return fmt.Errorf("mode %q: use %s, %s or %s", truncate(c.Mode, 40), diag.ConsentOff, diag.ConsentManual, diag.ConsentAutomatic)
}

// SafeModeReq is the body of a safe-mode request.
type SafeModeReq struct {
	On bool `json:"on"`
}

// diagArgs returns the sibyl-diag arguments for a checked request. Every
// value goes in a -flag=value argument, so a value cannot become a flag.
func diagArgs(act Action, body any) []string {
	switch act {
	case DiagBundle:
		b := body.(DiagBundleReq)
		args := []string{"bundle", "-json"}
		switch {
		case b.Latest:
			args = append(args, "-latest")
		case b.Incident != "":
			args = append(args, "-incident="+b.Incident)
		}
		if b.Note != "" {
			args = append(args, "-note="+b.Note)
		}
		return args
	case DiagSubmit:
		return []string{"submit", "-yes", strings.ToUpper(body.(DiagSubmitReq).ID)}
	case DiagConsent:
		return []string{"consent", body.(DiagConsentReq).Mode}
	case SetSafeMode:
		if body.(SafeModeReq).On {
			return []string{"safe-mode", "on"}
		}
		return []string{"safe-mode", "off"}
	}
	return nil
}

// runDiag runs sibyl-diag in a transient system unit. The sibyl-hostd unit
// allows only AF_UNIX and AF_NETLINK sockets, but a capture reads llama-swap
// over TCP and an upload needs the network. systemd-run asks systemd over
// D-Bus, so the sandbox of sibyl-hostd stays as it is. RuntimeMaxSec stops
// the unit before sibyl-hostd's own TimeoutStartSec.
func (a *Agent) runDiag(ctx context.Context, act Action, body any) (string, error) {
	args := append([]string{"--quiet", "--wait", "--pipe", "--collect", "--service-type=exec",
		"--property=RuntimeMaxSec=150", "--", DiagBin}, diagArgs(act, body)...)
	out, err := a.Run(ctx, "", "systemd-run", args...)
	if err != nil {
		return "", err
	}
	switch act {
	case DiagBundle:
		var b diag.Bundle
		if err := json.Unmarshal([]byte(out), &b); err != nil || !diag.ValidID(b.ID) {
			return "", fmt.Errorf("sibyl-diag bundle: unexpected output %q", truncate(strings.TrimSpace(out), 200))
		}
		return fmt.Sprintf("made bundle %s with %d redacted files", b.ID, len(b.Files)), nil
	case DiagSubmit:
		return "sent " + strings.ToUpper(body.(DiagSubmitReq).ID), nil
	case DiagConsent:
		return "upload consent is now " + body.(DiagConsentReq).Mode, nil
	case SetSafeMode:
		if body.(SafeModeReq).On {
			return "safe mode is on: llama-swap does not start at the next boot, and the watchdog does not act", nil
		}
		return "safe mode is off", nil
	}
	return "", fmt.Errorf("unknown action %q", act)
}
