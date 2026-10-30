package hostd

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// CheckPassword checks the password of an account for the web console
// sign-in. Only the memory queue accepts it, so a password never reaches a
// disk. The disk queue refuses it, because Decode does not know it.
const CheckPassword Action = "password-check"

// AuthGroup is the group whose members can sign in to the web console.
// They can already use sudo, so the console gives them no new power.
const AuthGroup = "wheel"

// AuthResultTTL is how long a password check result stays in Out/auth.
const AuthResultTTL = time.Minute

// maxPassword is the longest password that a check accepts.
const maxPassword = 512

// PasswordCheck is the body of a password check.
type PasswordCheck struct {
	User     string `json:"user"`
	Password string `json:"password"`
}

// ErrDenied is the only failure that a password check reports, so a check
// does not tell whether the account exists.
var ErrDenied = errors.New("wrong user or password")

// Validate checks the form of a password check.
func (p PasswordCheck) Validate() error {
	if !userRE.MatchString(p.User) {
		return ErrDenied
	}
	if p.Password == "" || len(p.Password) > maxPassword || strings.ContainsRune(p.Password, 0) {
		return ErrDenied
	}
	return nil
}

// DecodePasswordCheck reads a password check, with unknown fields refused.
func DecodePasswordCheck(data []byte) (PasswordCheck, error) {
	var p PasswordCheck
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&p); err != nil {
		return p, ErrDenied
	}
	return p, p.Validate()
}

// ProcessAuth handles the password checks in AuthQueue, oldest first. It
// writes each result, which holds no secret, to Out/auth.
func (a *Agent) ProcessAuth(ctx context.Context) error {
	dir := filepath.Join(a.Out, "auth")
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	if a.Group >= 0 {
		if err := os.Lchown(dir, -1, a.Group); err != nil {
			return err
		}
	}
	if a.Label != "" {
		if err := setLabel(dir, a.Label); err != nil {
			return fmt.Errorf("label %s: %w", dir, err)
		}
	}
	reqs, err := readQueue(a.AuthQueue)
	if err != nil {
		return err
	}
	for _, q := range reqs {
		res := a.handleAuth(ctx, q)
		if err := a.writeOut(filepath.Join("auth", res.ID+".json"), res); err != nil {
			a.Log.Error("write result", "id", res.ID, "err", err)
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	for _, e := range entries {
		if info, err := e.Info(); err == nil && a.Now().Sub(info.ModTime()) > AuthResultTTL {
			_ = os.Remove(filepath.Join(dir, e.Name()))
		}
	}
	return nil
}

func (a *Agent) handleAuth(ctx context.Context, q queued) Result {
	res := Result{ID: strings.TrimSuffix(q.name, ".json"), Action: CheckPassword}
	err := q.err
	var user string
	if err == nil {
		m := requestRE.FindStringSubmatch(q.name)
		switch {
		case m == nil || Action(m[2]) != CheckPassword:
			err = errors.New("the memory queue takes only password checks")
		default:
			var p PasswordCheck
			if p, err = DecodePasswordCheck(q.data); err == nil {
				user = p.User
				err = a.checkPassword(ctx, p)
			}
		}
	}
	res.OK = err == nil
	if err != nil {
		res.Error = err.Error()
		a.Log.Warn("password check failed", "id", res.ID, "user", user, "err", err)
	} else {
		a.Log.Info("password check passed", "id", res.ID, "user", user)
	}
	res.Finished = a.Now().UTC().Format(time.RFC3339)
	return res
}

// checkPassword passes when the account is in AuthGroup and the password
// is right. Every failure is ErrDenied.
func (a *Agent) checkPassword(ctx context.Context, p PasswordCheck) error {
	out, err := a.Run(ctx, "", "id", "-nG", "--", p.User)
	if err != nil || !slices.Contains(strings.Fields(out), AuthGroup) {
		return ErrDenied
	}
	// unix_chkpwd is PAM's own helper. It reads the password from standard
	// input up to a NUL, and reads /etc/shadow, so it handles every hash
	// that the host uses.
	if _, err := a.Run(ctx, p.Password+"\x00", "unix_chkpwd", p.User, "nonull"); err != nil {
		return ErrDenied
	}
	return nil
}
