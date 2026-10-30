package hostd

import (
	"context"
	"strings"
	"testing"
)

const bootcJSON = `{"status":{"booted":{"image":{"image":{"image":"ghcr.io/diffsec/sibyl-os:latest-amd"},"version":"sha-old-amd",
"imageDigest":"sha256:old"},"cachedUpdate":{"version":"sha-new-amd","imageDigest":"sha256:new"}},"staged":null}}`

func TestUpdateActions(t *testing.T) {
	ta := newAgent(t)
	for _, c := range []struct {
		action Action
		body   string
		ran    string
	}{
		{UpdateCheck, `{}`, "systemctl start --no-block -- " + CheckUnit},
		{UpdateApply, `{"reboot":false}`, "systemctl start --no-block -- " + DownloadUnit},
		{UpdateApply, `{"reboot":true}`, "systemctl start --no-block -- " + DownloadBootUnit},
		{Reboot, `{}`, "systemd-run --on-active=5s --unit=sibyl-reboot -- /usr/bin/systemctl reboot"},
	} {
		ta.host.calls = nil
		if r := ta.submit(t, "1", c.action, c.body); !r.OK {
			t.Errorf("%s %s: %+v", c.action, c.body, r)
		}
		if !ta.host.ran(c.ran) {
			t.Errorf("%s %s did not run %q: %v", c.action, c.body, c.ran, ta.host.calls)
		}
	}
	if _, err := Decode(UpdateApply, []byte(`{"reboot":true,"image":"evil"}`)); err == nil {
		t.Error("an update request with another image passed")
	}
}

func TestUpdateState(t *testing.T) {
	ta := newAgent(t)
	ta.host.answers["bootc status --format=json"] = bootcJSON
	ta.host.answers["systemctl show -p ActiveState -p Result -- "+DownloadUnit] = "ActiveState=activating\nResult=success\n"
	ta.host.answers["systemctl show -p ActiveState -p Result -- "+CheckUnit] = "ActiveState=inactive\nResult=exit-code\n"
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	u := ta.state(t).Update
	if u == nil || u.Booted != "sha-old-amd" || u.Available != "sha-new-amd" || u.Staged != "" ||
		u.Image != "ghcr.io/diffsec/sibyl-os:latest-amd" {
		t.Fatalf("update %+v", u)
	}
	if u.Download.Active != "activating" || u.Check.Result != "exit-code" {
		t.Errorf("units %+v %+v", u.Download, u.Check)
	}
	// A staged update is not also available.
	ta.host.answers["bootc status --format=json"] = strings.Replace(bootcJSON, `"staged":null`,
		`"staged":{"image":{"version":"sha-new-amd","imageDigest":"sha256:new"}}`, 1)
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	if u := ta.state(t).Update; u.Staged != "sha-new-amd" || u.Available != "" {
		t.Errorf("staged update %+v", u)
	}
}

func TestUpdateStateWhileBootcIsBusy(t *testing.T) {
	ta := newAgent(t)
	ta.host.answers["bootc status --format=json"] = bootcJSON
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	// A running upgrade holds bootc's lock, and bootc status times out.
	ta.host.fail["bootc status"] = context.DeadlineExceeded
	ta.host.answers["systemctl show -p ActiveState -p Result -- "+DownloadBootUnit] = "ActiveState=activating\nResult=success\n"
	if err := ta.WriteState(context.Background()); err != nil {
		t.Fatal(err)
	}
	st := ta.state(t)
	if u := st.Update; u == nil || u.Booted != "sha-old-amd" || u.Available != "sha-new-amd" || u.DownloadReboot.Active != "activating" {
		t.Errorf("update %+v", st.Update)
	}
	for _, e := range st.Errors {
		if strings.Contains(e, "bootc") || strings.Contains(e, "deadline") {
			t.Errorf("a busy bootc is an error: %v", st.Errors)
		}
	}
}
