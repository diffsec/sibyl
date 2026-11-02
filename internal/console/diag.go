package console

import (
	"errors"
	"net/http"
	"strings"

	"github.com/diffsec/sibyl/internal/diag"
	"github.com/diffsec/sibyl/internal/hostd"
)

func (c *Console) diagRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"diagnostics", c.signedIn(c.diagPage))
	mux.HandleFunc("POST "+Prefix+"diagnostics/bundle", c.signedIn(c.hostChangeTo("diagnostics", c.makeBundle)))
	mux.HandleFunc("POST "+Prefix+"diagnostics/submit", c.signedIn(c.hostChangeTo("diagnostics", c.submitBundle)))
	mux.HandleFunc("POST "+Prefix+"diagnostics/consent", c.signedIn(c.hostChangeTo("diagnostics", c.setConsent)))
	mux.HandleFunc("POST "+Prefix+"diagnostics/safe-mode", c.signedIn(c.hostChangeTo("diagnostics", c.setSafeMode)))
}

func (c *Console) diagPage(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	st, err := c.client.Diagnostics(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	perf, err := c.client.DiagnosticsPerf(r.Context())
	if err != nil {
		data["PerfError"] = err.Error()
	}
	data["Diag"], data["Perf"], data["MaxNote"] = st, perf, hostd.MaxNote
	data["Modes"] = []string{diag.ConsentOff, diag.ConsentManual, diag.ConsentAutomatic}
	c.render(w, r, http.StatusOK, "diagnostics", data)
}

func (c *Console) makeBundle(r *http.Request) (string, hostd.Result, error) {
	b := hostd.DiagBundleReq{Incident: r.PostFormValue("incident"), Note: strings.TrimSpace(r.PostFormValue("note"))}
	if err := b.Validate(); err != nil {
		return "", hostd.Result{}, err
	}
	res, err := c.client.MakeBundle(r.Context(), b)
	return "bundle", res, err
}

func (c *Console) submitBundle(r *http.Request) (string, hostd.Result, error) {
	id := r.PostFormValue("id")
	if !diag.ValidID(id) {
		return "", hostd.Result{}, errors.New("no bundle to send")
	}
	res, err := c.client.SubmitBundle(r.Context(), id)
	return "send", res, err
}

func (c *Console) setConsent(r *http.Request) (string, hostd.Result, error) {
	res, err := c.client.SetConsent(r.Context(), r.PostFormValue("mode"))
	return "upload consent", res, err
}

func (c *Console) setSafeMode(r *http.Request) (string, hostd.Result, error) {
	res, err := c.client.SetSafeMode(r.Context(), r.PostFormValue("on") == "true")
	return "safe mode", res, err
}
