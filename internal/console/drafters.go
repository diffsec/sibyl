package console

import (
	"fmt"
	"net/http"
	"net/url"
	"slices"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

// drafterDraftMax is the draft length that a test and the enable step use.
// The DFlash 2 cards give 7.
const drafterDraftMax = 7

func (c *Console) drafterRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"models/drafters", c.signedIn(c.draftersPage))
	mux.HandleFunc("GET "+Prefix+"models/drafters/test", c.signedIn(c.drafterTestLive))
	mux.HandleFunc("POST "+Prefix+"models/drafters/test", c.signedIn(c.startDrafterTest))
	mux.HandleFunc("POST "+Prefix+"models/drafters/enable", c.signedIn(c.enableDrafter))
}

// drafterFor returns the model that uses m as its draft model, if any.
func drafterFor(m client.Model, list []client.Model) string {
	for _, o := range list {
		if o.Settings.Speculative.DraftModel == m.ID {
			return o.ID
		}
	}
	return ""
}

// testData adds the model's last drafter test to data.
func (c *Console) testData(r *http.Request, id string, data map[string]any) {
	data["ID"] = id
	t, err := c.client.DrafterTest(r.Context(), id)
	if err != nil {
		return // no test ran
	}
	data["Test"] = t
	if t.Status == "done" && t.With != nil && t.Without != nil {
		data["Result"] = fmt.Sprintf("Without: %.1f tokens/s. With: %.1f tokens/s, %.2f times as fast. The model kept %d of %d drafted tokens.",
			t.Without.TokensPerSecond, t.With.TokensPerSecond, t.Speedup, t.With.Accepted, t.With.Drafted)
		data["Faster"] = t.Speedup > 1
	}
}

func (c *Console) draftersPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	data := pageData(r)
	d, err := c.client.FindDrafters(r.Context(), id, r.URL.Query().Get("refresh") == "1")
	if err != nil {
		data["Error"] = err.Error()
	}
	data["Drafters"] = d.Drafters
	data["Source"] = d.Source
	data["Base"] = slices.ContainsFunc(d.Drafters, func(dr client.Drafter) bool { return dr.Match == "base" })
	c.testData(r, id, data)
	c.render(w, r, http.StatusOK, "drafters", data)
}

func (c *Console) drafterTestLive(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	c.testData(r, r.URL.Query().Get("id"), data)
	c.renderBlock(w, r, http.StatusOK, "drafters", "test", data)
}

func (c *Console) startDrafterTest(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	spec := models.Speculative{
		Types: []string{r.PostFormValue("spectype")}, DraftModel: r.PostFormValue("drafter"), DraftMax: new(drafterDraftMax),
	}
	err := c.client.StartDrafterTest(r.Context(), id, spec)
	c.back(w, r, "models/drafters?"+url.Values{"id": {id}}.Encode(),
		"testing "+spec.DraftModel+" on "+id+"; it unloads the running models and takes a few minutes", err)
}

// enableDrafter turns on the drafter of the model's last test.
func (c *Console) enableDrafter(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	page := "models/drafters?" + url.Values{"id": {id}}.Encode()
	t, err := c.client.DrafterTest(r.Context(), id)
	if err == nil && t.Status != "done" {
		err = fmt.Errorf("the last drafter test of %s did not finish", id)
	}
	if err != nil {
		c.back(w, r, page, "", err)
		return
	}
	list, err := c.client.Models(r.Context())
	if err != nil {
		c.back(w, r, page, "", err)
		return
	}
	i := slices.IndexFunc(list, func(m client.Model) bool { return m.ID == id })
	if i < 0 {
		c.back(w, r, page, "", fmt.Errorf("no model %s", id))
		return
	}
	s := list[i].Settings
	s.Speculative = t.Speculative
	_, err = c.client.SetModel(r.Context(), id, s)
	c.back(w, r, "models", "turned on "+t.Speculative.DraftModel+" for "+id, err)
}
