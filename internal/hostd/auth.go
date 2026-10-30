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
	"strconv"
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

// The limits on failed checks. They live in Data, which only root can
// write, so a compromised System API cannot reset them by writing to the
// queue directly.
const (
	// FailWindow is the time over which failures count.
	FailWindow = 10 * time.Minute
	// FailsPerUser is the number of failures for one account in FailWindow
	// after which its checks are refused without a try.
	FailsPerUser = 10
	// FailDelay is the wait after each failure. sibyl-hostd -auth handles
	// the checks one at a time, so it limits the rate of guesses.
	FailDelay    = time.Second
	failuresFile = "auth-failures.json"
)

// authFailure is one failed check.
type authFailure struct {
	User string    `json:"user"`
	Time time.Time `json:"time"`
}

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
	var fails []authFailure
	a.readData(failuresFile, &fails)
	for _, q := range reqs {
		// A result is written only under a name that sibyl-hostd accepts,
		// so the queue cannot pick the names of root's files.
		if !requestRE.MatchString(q.name) {
			a.Log.Warn("password check refused: invalid request name")
			continue
		}
		res, failedUser := a.handleAuth(ctx, q, &fails)
		if err := a.writeOut(filepath.Join("auth", res.ID+".json"), res); err != nil {
			a.Log.Error("write result", "id", res.ID, "err", err)
		}
		if failedUser != "" {
			fails = append(fails, authFailure{User: failedUser, Time: a.Now().UTC()})
			if err := a.writeData(failuresFile, fails); err != nil {
				a.Log.Error("record a failed password check", "err", err)
			}
			if a.Sleep != nil {
				a.Sleep(FailDelay)
			}
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

// limited reports whether the failures in the window refuse a check for
// user. It drops the failures that are older than the window.
func (a *Agent) limited(fails *[]authFailure, user string) bool {
	now := a.Now()
	kept := (*fails)[:0]
	mine := 0
	for _, f := range *fails {
		if now.Sub(f.Time) < FailWindow {
			kept = append(kept, f)
			if f.User == user {
				mine++
			}
		}
	}
	*fails = kept
	return mine >= FailsPerUser
}

// handleAuth runs one check. It returns the result, and the user to count
// a failure against, or "" for no failure.
func (a *Agent) handleAuth(ctx context.Context, q queued, fails *[]authFailure) (Result, string) {
	res := Result{ID: strings.TrimSuffix(q.name, ".json"), Action: CheckPassword}
	err := q.err
	var user, failed string
	if err == nil {
		m := requestRE.FindStringSubmatch(q.name)
		switch {
		case Action(m[2]) != CheckPassword:
			err = errors.New("the memory queue takes only password checks")
		default:
			var p PasswordCheck
			if p, err = DecodePasswordCheck(q.data); err == nil {
				user = p.User
				if a.limited(fails, user) {
					a.Log.Warn("password check refused: too many failures", "user", user)
					err = ErrDenied
				} else if err = a.checkPassword(ctx, p); err != nil {
					failed = user
				}
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
	return res, failed
}

// checkPassword passes when the account is in AuthGroup and the password
// is right. Every failure is ErrDenied.
func (a *Agent) checkPassword(ctx context.Context, p PasswordCheck) error {
	out, err := a.Run(ctx, "", "id", "-nG", "--", p.User)
	if err != nil || !slices.Contains(strings.Fields(out), AuthGroup) {
		return ErrDenied
	}
	// Run as root, unix_chkpwd does not check the account's expiry date,
	// so this check does: field 8 of the shadow entry, in days since 1970.
	shadow, err := a.Run(ctx, "", "getent", "shadow", "--", p.User)
	if err != nil {
		a.Log.Error("read the shadow entry", "user", p.User, "err", err)
		return ErrDenied
	}
	if f := strings.Split(strings.TrimSpace(shadow), ":"); len(f) >= 8 && f[7] != "" {
		days, err := strconv.ParseInt(f[7], 10, 64)
		if err != nil || a.Now().Unix() >= days*86400 {
			return ErrDenied
		}
	}
	// unix_chkpwd is PAM's own helper. It reads the password from standard
	// input up to a NUL, and reads /etc/shadow, so it handles every hash
	// that the host uses. A wrong password exits 7. Any other failure is
	// logged, so a broken check does not look like wrong passwords.
	if _, err := a.Run(ctx, p.Password+"\x00", "unix_chkpwd", p.User, "nonull"); err != nil {
		if !strings.HasSuffix(err.Error(), "exit status 7") {
			a.Log.Error("unix_chkpwd failed", "user", p.User, "err", err)
		}
		return ErrDenied
	}
	return nil
}
