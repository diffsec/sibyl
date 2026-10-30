package console

import (
	"fmt"
	"maps"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/hf"
	"github.com/diffsec/sibyl/internal/modelform"
	"github.com/diffsec/sibyl/internal/models"
)

func (c *Console) modelRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET "+Prefix+"models", c.signedIn(c.modelsPage))
	mux.HandleFunc("GET "+Prefix+"models/edit", c.signedIn(c.modelPage))
	mux.HandleFunc("POST "+Prefix+"models/edit", c.signedIn(c.saveModel))
	mux.HandleFunc("POST "+Prefix+"models/delete", c.signedIn(c.deleteModel))
	mux.HandleFunc("POST "+Prefix+"models/bench", c.signedIn(c.benchModel))
	mux.HandleFunc("GET "+Prefix+"downloads", c.signedIn(c.downloadsPage))
	mux.HandleFunc("GET "+Prefix+"downloads/live", c.signedIn(c.downloadsLive))
	mux.HandleFunc("GET "+Prefix+"downloads/repo", c.signedIn(c.repoPage))
	mux.HandleFunc("POST "+Prefix+"downloads", c.signedIn(c.startDownload))
	mux.HandleFunc("POST "+Prefix+"downloads/cancel", c.signedIn(c.cancelDownload))
	mux.HandleFunc("POST "+Prefix+"downloads/retry", c.signedIn(c.retryDownload))
	mux.HandleFunc("POST "+Prefix+"downloads/token", c.signedIn(c.setHFToken))
}

// back sends the browser to a page with the result of a change, so a
// reload does not repeat it.
func (c *Console) back(w http.ResponseWriter, r *http.Request, page, done string, err error) {
	q := url.Values{}
	if err != nil {
		q.Set("error", err.Error())
	} else {
		q.Set("done", done)
		c.Log.Info("console change", "user", userOf(r), "change", done)
	}
	sep := "?"
	if strings.Contains(page, "?") {
		sep = "&"
	}
	http.Redirect(w, r, Prefix+page+sep+q.Encode(), http.StatusSeeOther)
}

// pageData starts the data of a page with the error from a redirect. Only
// a signed-in user reaches the pages that use it.
func pageData(r *http.Request) map[string]any {
	return map[string]any{"FlashError": r.URL.Query().Get("error")}
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// benchText sums up a model's latest benchmark.
func benchText(b *client.Benchmark) string {
	if b == nil {
		return ""
	}
	if b.Status != "done" {
		return "benchmark " + b.Status
	}
	var parts []string
	for _, backend := range slices.Sorted(maps.Keys(b.Results)) {
		r := b.Results[backend]
		if r.Error != "" {
			parts = append(parts, backend+" failed")
			continue
		}
		parts = append(parts, fmt.Sprintf("%s %.1f tok/s generation, %.0f tok/s prompt", backend, r.GenTPS, r.PromptTPS))
	}
	s := "benchmark: " + strings.Join(parts, " · ")
	if b.Recommended != "" {
		s += " · faster: " + b.Recommended
	}
	return s
}

// fitText describes the context and the memory that the System API chose.
func fitText(f *models.Fit) string {
	if f == nil {
		return ""
	}
	s := fmt.Sprintf("context %d", f.Ctx)
	if f.Slots > 1 {
		s += fmt.Sprintf(" in %d slots of %d", f.Slots, f.SlotCtx)
	}
	if f.TypeK != "" {
		s += " · cache " + f.TypeK + "/" + f.TypeV
	}
	if !f.Fits {
		s += " · part in system RAM"
	}
	if f.Note != "" {
		s += " · " + f.Note
	}
	return s
}

func (c *Console) modelsPage(w http.ResponseWriter, r *http.Request) {
	data := pageData(r)
	list, err := c.client.Models(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	type row struct {
		client.Model
		SizeText, Fit, Bench string
	}
	var rows []row
	for _, m := range list {
		rows = append(rows, row{m, gib(m.Size), fitText(m.Fit), benchText(m.Benchmark)})
	}
	data["Models"] = rows
	c.render(w, r, http.StatusOK, "models", data)
}

func (c *Console) modelPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	list, err := c.client.Models(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	i := slices.IndexFunc(list, func(m client.Model) bool { return m.ID == id })
	if i < 0 {
		http.Error(w, "no model "+id, http.StatusNotFound)
		return
	}
	m := list[i]
	hw, err := c.client.Hardware(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	layouts := [][2]string{{"", "Hardware default (" + string(hw.Layout) + ")"}, {"span", "Span all selected GPUs"}}
	for g := range hw.GPUs {
		layouts = append(layouts, [2]string{"gpu:" + strconv.Itoa(g), fmt.Sprintf("Pin to selected GPU %d", g)})
	}
	var drafts []string
	for _, o := range list {
		if o.ID != m.ID {
			drafts = append(drafts, o.ID)
		}
	}
	head := fmt.Sprintf("%s · %d file(s)", gib(m.Size), m.Shards)
	moe := false
	if m.Meta != nil {
		head += fmt.Sprintf(" · %s · native context %d tokens", m.Meta.Architecture, m.Meta.ContextLength)
		moe = m.Meta.ExpertCount > 0
	}
	spec := slices.DeleteFunc(slices.Clone(models.SpecTypes), func(s string) bool { return s == "none" })
	data := pageData(r)
	for k, v := range map[string]any{
		"Model": m, "Head": head, "Fit": fitText(m.Fit), "Form": modelform.FromSettings(m.Settings),
		"Layouts": layouts, "Drafts": drafts, "CacheTypes": models.CacheTypes, "Presets": models.Presets,
		"SpecTypes": spec, "MoE": moe, "Efforts": models.ReasoningEfforts[1:],
	} {
		data[k] = v
	}
	c.render(w, r, http.StatusOK, "model", data)
}

// formFromPost reads the settings form. The field names are the
// modelform.Form field names in lower case.
func formFromPost(r *http.Request) modelform.Form {
	v := func(name string) string { return strings.TrimSpace(r.PostFormValue(name)) }
	return modelform.Form{
		Layout: v("layout"), CtxSize: v("ctxsize"), Parallel: v("parallel"), TensorSplit: v("tensorsplit"),
		Embedding: r.PostFormValue("embedding") == "on", Hidden: r.PostFormValue("hidden") == "on",
		Temperature: v("temperature"), TopK: v("topk"), TopP: v("topp"), MinP: v("minp"),
		RepeatPenalty: v("repeatpenalty"), PresencePenalty: v("presencepenalty"),
		SpecTypes: r.PostForm["spectypes"], DraftModel: v("draftmodel"), DraftMax: v("draftmax"), DraftMin: v("draftmin"),
		NgramModMin: v("ngrammodmin"), NgramModMax: v("ngrammodmax"), KVTypeK: v("kvtypek"), KVTypeV: v("kvtypev"),
		MoELayers: v("moelayers"), MoECacheMiB: v("moecachemib"), Fit: v("fit"), Preset: v("preset"), Args: v("args"),
		ReasoningEffort: v("reasoningeffort"),
	}
}

func (c *Console) saveModel(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	id := r.PostFormValue("id")
	s, err := formFromPost(r).Settings()
	if err != nil {
		c.back(w, r, "models/edit?"+url.Values{"id": {id}}.Encode(), "", err)
		return
	}
	res, err := c.client.SetModel(r.Context(), id, s)
	done := "saved " + id
	for _, is := range res.Issues {
		done += "; " + is.ID + ": " + is.Reason
	}
	c.back(w, r, "models", done, err)
}

func (c *Console) deleteModel(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	_, err := c.client.DeleteModel(r.Context(), id)
	c.back(w, r, "models", "deleted "+id, err)
}

func (c *Console) benchModel(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	err := c.client.StartBenchmark(r.Context(), id)
	c.back(w, r, "models", "benchmark of "+id+" queued; it unloads the running models and takes a few minutes", err)
}

// Downloads.

func (c *Console) downloadsData(r *http.Request) map[string]any {
	data := pageData(r)
	jobs, err := c.client.Downloads(r.Context())
	if err != nil {
		data["Error"] = err.Error()
	}
	type row struct {
		ID, Repo, Variant, Status, Error, Progress string
		Done                                       bool
	}
	var rows []row
	for _, j := range jobs {
		p := ""
		if j.BytesTotal > 0 {
			p = fmt.Sprintf("%s of %s (%.0f%%)", gib(j.BytesDone), gib(j.BytesTotal), 100*float64(j.BytesDone)/float64(j.BytesTotal))
		}
		st := string(j.Status)
		rows = append(rows, row{ID: j.ID, Repo: j.Repo, Variant: j.Variant, Status: st, Error: j.Error, Progress: p,
			Done: st == "done" || st == "failed" || st == "canceled"})
	}
	data["Jobs"] = rows
	return data
}

func (c *Console) downloadsPage(w http.ResponseWriter, r *http.Request) {
	data := c.downloadsData(r)
	if tok, err := c.client.HuggingFace(r.Context()); err == nil {
		data["HF"] = tok
	}
	if q := strings.TrimSpace(r.URL.Query().Get("q")); q != "" {
		data["Query"] = q
		if hf.ValidRepo(q) {
			http.Redirect(w, r, Prefix+"downloads/repo?"+url.Values{"id": {q}}.Encode(), http.StatusSeeOther)
			return
		}
		res, err := c.client.Search(r.Context(), q)
		if err != nil {
			data["FlashError"] = err.Error()
		}
		data["Results"] = res
	}
	c.render(w, r, http.StatusOK, "downloads", data)
}

func (c *Console) downloadsLive(w http.ResponseWriter, r *http.Request) {
	c.renderBlock(w, r, http.StatusOK, "downloads", "jobs", c.downloadsData(r))
}

func (c *Console) repoPage(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("id")
	data := pageData(r)
	repo, err := c.client.Repo(r.Context(), id)
	if err != nil {
		data["FlashError"] = err.Error()
	}
	pools, _ := c.client.Pools(r.Context())
	type row struct {
		Name, Size, Fit string
		Shards          int
	}
	var rows []row
	for _, v := range repo.Variants {
		rows = append(rows, row{v.Name, gib(v.Size), v.Fit, v.Shards})
	}
	data["ID"], data["Repo"], data["Variants"], data["Pools"] = id, repo, rows, pools.Pools
	data["VRAM"], data["RAM"] = gib(repo.VRAM), gib(repo.RAM)
	c.render(w, r, http.StatusOK, "repo", data)
}

func (c *Console) startDownload(w http.ResponseWriter, r *http.Request) {
	repo, variant := r.PostFormValue("repo"), r.PostFormValue("variant")
	_, err := c.client.Download(r.Context(), repo, r.PostFormValue("revision"), variant, r.PostFormValue("pool"))
	c.back(w, r, "downloads", "download of "+repo+" "+variant+" started", err)
}

func (c *Console) cancelDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	c.back(w, r, "downloads", "download cancelled", c.client.CancelDownload(r.Context(), id))
}

func (c *Console) retryDownload(w http.ResponseWriter, r *http.Request) {
	id := r.PostFormValue("id")
	c.back(w, r, "downloads", "download restarted", c.client.RetryDownload(r.Context(), id))
}

func (c *Console) setHFToken(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSpace(r.PostFormValue("token"))
	var err error
	if r.PostFormValue("clear") == "1" {
		_, err = c.client.ClearHuggingFaceToken(r.Context())
		c.back(w, r, "downloads", "Hugging Face token removed", err)
		return
	}
	_, err = c.client.SetHuggingFaceToken(r.Context(), tok)
	c.back(w, r, "downloads", "Hugging Face token saved", err)
}
