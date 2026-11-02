package hostd

import (
	"errors"
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/diag"
)

const diagRun = "systemd-run --quiet --wait --pipe --collect --service-type=exec --property=RuntimeMaxSec=150 -- /usr/sbin/sibyl-diag "

func TestDiagActions(t *testing.T) {
	id := diag.NewID()
	for _, c := range []struct {
		action Action
		body   string
		ran    string
		msg    string
	}{
		{DiagBundle, `{}`, diagRun + "bundle -json", "made bundle " + id + " with 2 redacted files"},
		{DiagBundle, `{"latest":true}`, diagRun + "bundle -json -latest", id},
		{DiagBundle, `{"incident":"20261031T103035Z-generation-stalled","note":"-yes\nslow after the update"}`,
			diagRun + "bundle -json -incident=20261031T103035Z-generation-stalled -note=-yes\nslow after the update", id},
		{DiagSubmit, `{"diagnostic_id":"` + strings.ToLower(id) + `"}`, diagRun + "submit -yes " + id, "sent " + id},
		{DiagConsent, `{"mode":"manual"}`, diagRun + "consent manual", "upload consent is now manual"},
		{SetSafeMode, `{"on":true}`, diagRun + "safe-mode on", "safe mode is on"},
		{SetSafeMode, `{"on":false}`, diagRun + "safe-mode off", "safe mode is off"},
	} {
		ta := newAgent(t)
		ta.host.answers[diagRun+"bundle"] = `{"diagnostic_id":"` + id + `","path":"/x","files":[{"path":"a"},{"path":"b"}]}`
		r := ta.submit(t, "1", c.action, c.body)
		if !r.OK || !strings.Contains(r.Message, c.msg) {
			t.Errorf("%s %s: %+v", c.action, c.body, r)
		}
		if ta.host.calls[0] != c.ran {
			t.Errorf("%s %s ran %q, want %q", c.action, c.body, ta.host.calls[0], c.ran)
		}
	}
}

func TestDiagErrorsReachTheResult(t *testing.T) {
	ta := newAgent(t)
	ta.host.fail[diagRun+"submit"] = errors.New("systemd-run: sibyl-diag: upload consent is off")
	r := ta.submit(t, "1", DiagSubmit, `{"diagnostic_id":"`+diag.NewID()+`"}`)
	if r.OK || !strings.Contains(r.Error, "upload consent is off") {
		t.Fatalf("result %+v", r)
	}
	ta.host.answers[diagRun+"bundle"] = "Diagnostic ID: SBL-XXXX-XXXX\n"
	if r := ta.submit(t, "2", DiagBundle, `{}`); r.OK || !strings.Contains(r.Error, "unexpected output") {
		t.Fatalf("result %+v", r)
	}
}

func TestDiagDecodeRejects(t *testing.T) {
	for _, c := range []struct {
		a    Action
		body string
	}{
		{DiagBundle, `{"incident":"../../etc/shadow"}`},
		{DiagBundle, `{"incident":"20261031T103035Z-x","latest":true}`},
		{DiagBundle, `{"incident":"-latest"}`},
		{DiagBundle, `{"note":"a\u0000b"}`},
		{DiagBundle, `{"note":"a\u001b[2Jb"}`},
		{DiagBundle, `{"note":"` + strings.Repeat("x", MaxNote+1) + `"}`},
		{DiagBundle, `{"path":"/etc"}`},
		{DiagSubmit, `{"diagnostic_id":"SBL-ABCD"}`},
		{DiagSubmit, `{"diagnostic_id":"/var/lib/sibyl-diag/consent.json"}`},
		{DiagSubmit, `{"diagnostic_id":"` + diag.NewID() + `","endpoint":"https://evil.example"}`},
		{DiagConsent, `{"mode":"always"}`},
		{DiagConsent, `{"mode":"manual","endpoint":"https://evil.example"}`},
		{SetSafeMode, `{"on":"yes"}`},
	} {
		if _, err := Decode(c.a, []byte(c.body)); err == nil {
			t.Errorf("%s %s: accepted", c.a, truncate(c.body, 60))
		}
	}
}

func TestInvalidDiagRequestRunsNothing(t *testing.T) {
	ta := newAgent(t)
	r := ta.submit(t, "1", DiagConsent, `{"mode":"manual","endpoint":"https://evil.example"}`)
	if r.OK || ta.host.ran("systemd-run") {
		t.Fatalf("result %+v, calls %v", r, ta.host.calls)
	}
}
