package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"charm.land/bubbles/v2/progress"
	"charm.land/bubbles/v2/textinput"
	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/ihavespoons/sibyl/internal/client"
	"github.com/ihavespoons/sibyl/internal/download"
	"github.com/ihavespoons/sibyl/internal/hf"
	"github.com/ihavespoons/sibyl/internal/hostd"
)

type searchResult struct {
	repos []hf.Summary
	err   error
}

type repoResult struct {
	repo client.Repo
	err  error
}

type jobsResult struct {
	jobs []download.Job
	err  error
}

type jobStarted struct {
	job download.Job
	err error
}

type downloadTick struct{}

// downloadStage is where the user is in the picker.
type downloadStage int

const (
	stageJobs     downloadStage = iota // job list
	stageSearch                        // typing a query
	stageResults                       // choosing a repository
	stageVariants                      // choosing a variant
	stageConfirm                       // confirming a download
)

type downloadView struct {
	c       *client.Client
	stage   downloadStage
	input   textinput.Model
	repos   []hf.Summary
	repo    client.Repo
	jobs    []download.Job
	cursor  int
	bar     progress.Model
	form    *huh.Form
	confirm bool
	pool    string
	pools   []hostd.Pool
	loading string
	ticking bool
}

type poolsResult struct {
	pools client.Pools
	err   error
}

func newDownloadView(c *client.Client) *downloadView {
	in := textinput.New()
	in.Placeholder = "search Hugging Face, e.g. Qwen3 GGUF"
	in.CharLimit = 200
	return &downloadView{c: c, input: in, bar: progress.New(progress.WithWidth(30))}
}

func (v *downloadView) fetchJobs() tea.Cmd {
	return call(v.c.Downloads, func(j []download.Job, err error) tea.Msg { return jobsResult{j, err} })
}

func (v *downloadView) fetchPools() tea.Cmd {
	return call(v.c.Pools, func(p client.Pools, err error) tea.Msg { return poolsResult{p, err} })
}

func (v *downloadView) activate() tea.Cmd {
	if v.ticking {
		return tea.Batch(v.fetchJobs(), v.fetchPools())
	}
	v.ticking = true
	return tea.Batch(v.fetchJobs(), v.fetchPools(), tea.Tick(2*time.Second, func(time.Time) tea.Msg { return downloadTick{} }))
}

func (v *downloadView) capturing() bool { return v.stage == stageSearch || v.form != nil }

func (v *downloadView) help() string {
	switch v.stage {
	case stageSearch:
		return "enter search · esc back"
	case stageResults, stageVariants:
		return "↑/↓ select · enter choose · esc back"
	case stageConfirm:
		return "←/→ choose · enter confirm · esc back"
	default:
		return "/ search · ↑/↓ select · c cancel · R retry"
	}
}

func (v *downloadView) count() int {
	switch v.stage {
	case stageResults:
		return len(v.repos)
	case stageVariants:
		return len(v.repo.Variants)
	default:
		return len(v.jobs)
	}
}

func (v *downloadView) update(msg tea.Msg) tea.Cmd {
	switch msg := msg.(type) {
	case downloadTick:
		return tea.Batch(v.fetchJobs(), tea.Tick(2*time.Second, func(time.Time) tea.Msg { return downloadTick{} }))
	case jobsResult:
		if msg.err == nil {
			v.jobs = msg.jobs
		}
		return nil
	case poolsResult:
		if msg.err == nil {
			v.pools = msg.pools.Pools
		}
		return nil
	case searchResult:
		v.loading = ""
		if msg.err != nil {
			v.stage = stageSearch
			return note(false, "search: %v", msg.err)
		}
		v.repos, v.cursor, v.stage = msg.repos, 0, stageResults
		return nil
	case repoResult:
		v.loading = ""
		if msg.err != nil {
			v.stage = stageResults
			if len(v.repos) == 0 {
				v.stage = stageSearch // opened by name: let the user fix the name
			}
			return note(false, "repository: %v", msg.err)
		}
		v.repo, v.cursor, v.stage = msg.repo, 0, stageVariants
		return nil
	case jobStarted:
		if msg.err != nil {
			return note(false, "download: %v", msg.err)
		}
		v.stage, v.cursor = stageJobs, 0
		return tea.Batch(note(true, "downloading %s (job %s)", msg.job.Variant, msg.job.ID), v.fetchJobs())
	case tea.KeyPressMsg:
		return v.key(msg)
	case tea.PasteMsg:
		// A paste arrives as its own message, not as key presses.
		if v.stage == stageSearch && v.form == nil {
			var cmd tea.Cmd
			v.input, cmd = v.input.Update(msg)
			return cmd
		}
	}
	if v.form != nil {
		return v.formStep(msg)
	}
	return nil
}

// formStep passes any message to the confirm form. huh completes a form
// through its own follow-up messages, so the form needs more than key presses.
func (v *downloadView) formStep(msg tea.Msg) tea.Cmd {
	m, cmd := v.form.Update(msg)
	v.form = m.(*huh.Form)
	switch v.form.State {
	case huh.StateCompleted:
		v.form = nil
		if !v.confirm {
			v.stage = stageVariants
			return cmd
		}
		variant := v.repo.Variants[v.cursor]
		repo, rev, pool := v.repo.Repo, v.repo.Revision, v.pool
		return tea.Batch(cmd, call(func(ctx context.Context) (download.Job, error) {
			return v.c.Download(ctx, repo, rev, variant.Name, pool)
		}, func(j download.Job, err error) tea.Msg { return jobStarted{j, err} }))
	case huh.StateAborted:
		v.form, v.stage = nil, stageVariants
	}
	return cmd
}

func (v *downloadView) key(msg tea.KeyPressMsg) tea.Cmd {
	k := msg.String()
	if v.form != nil {
		if k == "esc" {
			v.form, v.stage = nil, stageVariants
			return nil
		}
		return v.formStep(msg)
	}
	if v.stage == stageSearch {
		switch k {
		case "esc":
			v.stage = stageJobs
			v.input.Blur()
			return nil
		case "enter":
			q := strings.TrimSpace(v.input.Value())
			if q == "" {
				return nil
			}
			v.input.Blur()
			// An exact owner/name opens that repository, without a search.
			if hf.ValidRepo(q) {
				v.repos = nil
				v.loading = "reading " + q + "…"
				return call(func(ctx context.Context) (client.Repo, error) { return v.c.Repo(ctx, q) },
					func(r client.Repo, err error) tea.Msg { return repoResult{r, err} })
			}
			v.loading = "searching…"
			return call(func(ctx context.Context) ([]hf.Summary, error) { return v.c.Search(ctx, q) },
				func(r []hf.Summary, err error) tea.Msg { return searchResult{r, err} })
		}
		var cmd tea.Cmd
		v.input, cmd = v.input.Update(msg)
		return cmd
	}
	switch k {
	case "/":
		v.stage = stageSearch
		return v.input.Focus()
	case "esc":
		switch v.stage {
		case stageVariants:
			v.stage, v.cursor = stageResults, 0
		case stageResults:
			v.stage, v.cursor = stageJobs, 0
		}
	case "up", "k":
		if v.cursor > 0 {
			v.cursor--
		}
	case "down", "j":
		if v.cursor < v.count()-1 {
			v.cursor++
		}
	case "enter":
		switch v.stage {
		case stageResults:
			if v.cursor < len(v.repos) {
				id := v.repos[v.cursor].ID
				v.loading = "reading " + id + "…"
				return call(func(ctx context.Context) (client.Repo, error) { return v.c.Repo(ctx, id) },
					func(r client.Repo, err error) tea.Msg { return repoResult{r, err} })
			}
		case stageVariants:
			if v.cursor < len(v.repo.Variants) {
				return v.confirmForm(v.repo.Variants[v.cursor])
			}
		}
	case "c", "R":
		if v.stage != stageJobs || v.cursor >= len(v.jobs) {
			return nil
		}
		id := v.jobs[v.cursor].ID
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			var err error
			action := "canceled"
			if k == "c" {
				err = v.c.CancelDownload(ctx, id)
			} else {
				action = "queued again"
				err = v.c.RetryDownload(ctx, id)
			}
			if err != nil {
				return noteMsg{err.Error(), false}
			}
			return noteMsg{"job " + id + " " + action, true}
		}
	}
	return nil
}

func (v *downloadView) confirmForm(variant client.Variant) tea.Cmd {
	v.stage = stageConfirm
	v.confirm = true
	desc := fmt.Sprintf("%s in %d file(s). Fit: %s. Usable VRAM %s, RAM %s.\nCommit %s.",
		gib(variant.Size), variant.Shards, fitText(variant.Fit), gib(v.repo.VRAM), gib(v.repo.RAM), v.repo.Revision[:12])
	fields := []huh.Field{}
	v.pool = ""
	where := []huh.Option[string]{huh.NewOption("Models folder", "")}
	for _, p := range v.pools {
		if p.Mounted {
			where = append(where, huh.NewOption(fmt.Sprintf("%s  %s free", p.Name, gib(p.Free)), p.Name))
		}
	}
	if len(where) > 1 {
		fields = append(fields, huh.NewSelect[string]().Title("Where").Options(where...).Value(&v.pool))
	}
	fields = append(fields, huh.NewConfirm().Title("Download "+variant.Name+"?").Description(desc).
		Affirmative("Download").Negative("Back").Value(&v.confirm))
	v.form = huh.NewForm(huh.NewGroup(fields...)).WithShowHelp(false)
	return v.form.Init()
}

func fitText(f string) string {
	switch f {
	case "gpu":
		return "fits in VRAM"
	case "split":
		return "needs system RAM too (some models still run fully on GPU; see the model card)"
	default:
		return "too large for this host"
	}
}

func fitStyled(f string) string {
	switch f {
	case "gpu":
		return goodSty.Render("gpu  ")
	case "split":
		return selSty.Render("split")
	default:
		return badSty.Render("no   ")
	}
}

func (v *downloadView) render(int) string {
	var b strings.Builder
	switch v.stage {
	case stageSearch:
		b.WriteString(titleSty.Render("Search Hugging Face") + "\n\n  " + v.input.View() + "\n")
	case stageResults:
		b.WriteString(titleSty.Render("GGUF repositories") + "\n\n")
		start, end := window(len(v.repos), v.cursor)
		for i := start; i < end; i++ {
			r := v.repos[i]
			b.WriteString(cursorLine(i == v.cursor, fmt.Sprintf("%-60s %9d downloads", r.ID, r.Downloads)) + "\n")
		}
		b.WriteString(more(start, end, len(v.repos)))
	case stageVariants, stageConfirm:
		if v.form != nil {
			return v.form.View()
		}
		fmt.Fprintf(&b, "%s\n%s\n\n", titleSty.Render(v.repo.Repo), mutedSty.Render("commit "+v.repo.Revision))
		if len(v.repo.Variants) == 0 {
			b.WriteString(mutedSty.Render("  No complete GGUF variants with checksums in this repository.") + "\n")
		}
		start, end := window(len(v.repo.Variants), v.cursor)
		for i := start; i < end; i++ {
			va := v.repo.Variants[i]
			b.WriteString(cursorLine(i == v.cursor, fmt.Sprintf("%-55s %10s  %s", va.Name, gib(va.Size), fitStyled(va.Fit))) + "\n")
		}
		b.WriteString(more(start, end, len(v.repo.Variants)))
	default:
		b.WriteString(titleSty.Render("Downloads") + "  " + mutedSty.Render("press / to search Hugging Face") + "\n\n")
		if len(v.jobs) == 0 {
			b.WriteString(mutedSty.Render("  No downloads yet.") + "\n")
		}
		start, end := window(len(v.jobs), v.cursor)
		for i := start; i < end; i++ {
			j := v.jobs[i]
			pct := 0.0
			if j.BytesTotal > 0 {
				pct = float64(j.BytesDone) / float64(j.BytesTotal)
			}
			line := fmt.Sprintf("%-45s %-11s %s %s / %s", j.Variant, j.Status, v.bar.ViewAs(pct), gib(j.BytesDone), gib(j.BytesTotal))
			b.WriteString(cursorLine(i == v.cursor, line) + "\n")
			if j.Error != "" {
				b.WriteString("    " + badSty.Render(j.Error) + "\n")
			}
		}
		b.WriteString(more(start, end, len(v.jobs)))
	}
	if v.loading != "" {
		b.WriteString("\n" + mutedSty.Render(v.loading) + "\n")
	}
	return b.String()
}
