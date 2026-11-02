package tui

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

// The steps of the drafter finder, after "f" on a model.
const (
	flowPick    = "pick"    // choose a drafter from the search
	flowVariant = "variant" // choose the file of a drafter to download
	flowTest    = "test"    // confirm a test of an installed drafter
	flowEnable  = "enable"  // read the test result and turn the drafter on
)

// drafterDraftMax is the draft length that a test and the enable step use.
// The DFlash 2 cards give 7.
const drafterDraftMax = 7

type draftersFound struct {
	id  string
	d   client.Drafters
	err error
}

type drafterRepo struct {
	pick client.Drafter
	repo client.Repo
	err  error
}

type drafterTestState struct {
	id  string
	t   client.DrafterTest
	err error
}

type drafterTestPoll struct{ id string }

// drafterFlow is the state of the drafter finder for one model.
type drafterFlow struct {
	step    string
	target  client.Model
	found   []client.Drafter
	choice  int
	pick    client.Drafter
	repo    client.Repo
	variant string
	confirm bool
	result  client.DrafterTest
}

func (v *modelsView) findDrafters(m client.Model, refresh bool) tea.Cmd {
	if m.Drafter {
		return note(false, "%s is a drafter; select the model that it drafts for", m.ID)
	}
	id := m.ID
	return tea.Batch(note(true, "searching Hugging Face for drafters of %s", id), func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		d, err := v.c.FindDrafters(ctx, id, refresh)
		return draftersFound{id, d, err}
	})
}

// pickForm lists the drafters of the search.
func (v *modelsView) pickForm(m client.Model, d client.Drafters) tea.Cmd {
	v.flow = &drafterFlow{step: flowPick, target: m, found: d.Drafters}
	opts := make([]huh.Option[int], len(d.Drafters))
	for i, dr := range d.Drafters {
		label := fmt.Sprintf("%s · %s · %d downloads", dr.Repo, strings.TrimPrefix(dr.SpecType, "draft-"), dr.Downloads)
		if dr.Match == "base" {
			label += " · for the base model " + dr.Base
		}
		if dr.Installed != "" {
			label += " · installed"
		}
		opts[i] = huh.NewOption(label, i)
	}
	desc := "A drafter guesses tokens and the model checks them. Sibyl tests it before you turn it on."
	if slices.ContainsFunc(d.Drafters, func(dr client.Drafter) bool { return dr.Match == "base" }) {
		desc += "\nA drafter for the base model accepts fewer tokens on a finetune, and can make it slower."
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[int]().Title("Drafters for " + m.ID).Description(desc).Options(opts...).
			Value(&v.flow.choice).Height(min(len(opts), 12) + 4),
	)).WithShowHelp(false)
	return v.form.Init()
}

// variantForm lists the files of the chosen drafter.
func (v *modelsView) variantForm(r client.Repo) tea.Cmd {
	v.flow.step, v.flow.repo = flowVariant, r
	opts := make([]huh.Option[string], len(r.Variants))
	for i, vr := range r.Variants {
		opts[i] = huh.NewOption(fmt.Sprintf("%s · %s", vr.Name, gib(vr.Size)), vr.Name)
		// The DFlash 2 cards measured Q8_0 close to BF16, at half the size.
		if strings.Contains(vr.Name, "Q8_0") || i == 0 {
			v.flow.variant = vr.Name
		}
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewSelect[string]().Title("Download a file of " + r.Repo).
			Description("After the download, press f on " + v.flow.target.ID + " again to test it.").
			Options(opts...).Value(&v.flow.variant),
	)).WithShowHelp(false)
	return v.form.Init()
}

// testForm asks before a test, which unloads the running models.
func (v *modelsView) testForm(dr client.Drafter) tea.Cmd {
	v.flow.step, v.flow.confirm = flowTest, true
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Test " + dr.Installed + " on " + v.flow.target.ID + "?").
			Description("Sibyl runs two prompts without and with the drafter. It unloads the running models and takes a few minutes.").
			Affirmative("Test").Negative("Cancel").Value(&v.flow.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// enableForm shows the test result and asks to turn the drafter on. The
// default answer follows the result.
func (v *modelsView) enableForm(t client.DrafterTest) tea.Cmd {
	v.flow.step, v.flow.result = flowEnable, t
	v.flow.confirm = t.Speedup > 1
	desc := fmt.Sprintf("Without: %.1f tokens/s. With: %.1f tokens/s, %.2f times as fast.\n"+
		"The model kept %d of %d drafted tokens.", t.Without.TokensPerSecond, t.With.TokensPerSecond, t.Speedup,
		t.With.Accepted, t.With.Drafted)
	if t.Speedup <= 1 {
		desc += "\nThe drafter makes this model slower, so leave it off."
	}
	v.form = huh.NewForm(huh.NewGroup(
		huh.NewConfirm().Title("Turn on " + t.Speculative.DraftModel + " for " + v.flow.target.ID + "?").
			Description(desc).Affirmative("Turn on").Negative("Leave off").Value(&v.flow.confirm),
	)).WithShowHelp(false)
	return v.form.Init()
}

// submitFlow acts on the answer of the current finder step.
func (v *modelsView) submitFlow() tea.Cmd {
	fl := v.flow
	id := fl.target.ID
	switch fl.step {
	case flowPick:
		if fl.choice < 0 || fl.choice >= len(fl.found) {
			v.flow = nil
			return nil
		}
		fl.pick = fl.found[fl.choice]
		if fl.pick.Installed != "" {
			return v.testForm(fl.pick)
		}
		pick := fl.pick
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			r, err := v.c.Repo(ctx, pick.Repo)
			return drafterRepo{pick, r, err}
		}
	case flowVariant:
		repo, rev, variant := fl.repo.Repo, fl.repo.Revision, fl.variant
		v.flow = nil
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if _, err := v.c.Download(ctx, repo, rev, variant, ""); err != nil {
				return noteMsg{"download " + repo + ": " + err.Error(), false}
			}
			return noteMsg{"downloading " + variant + "; when it finishes, press f on " + id + " to test it", true}
		}
	case flowTest:
		if !fl.confirm {
			v.flow = nil
			return note(false, "no test")
		}
		spec := models.Speculative{Types: []string{fl.pick.SpecType}, DraftModel: fl.pick.Installed, DraftMax: new(drafterDraftMax)}
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if err := v.c.StartDrafterTest(ctx, id, spec); err != nil {
				return drafterTestState{id: id, err: err}
			}
			return drafterTestPoll{id}
		}
	case flowEnable:
		v.flow = nil
		if !fl.confirm {
			return note(true, "the drafter stays off for %s", id)
		}
		s := fl.target.Settings
		s.Speculative = fl.result.Speculative
		return call(func(ctx context.Context) (client.ApplyResult, error) { return v.c.SetModel(ctx, id, s) },
			func(r client.ApplyResult, err error) tea.Msg { return modelSaved{id, r, err} })
	}
	v.flow = nil
	return nil
}

// updateFlow handles the messages of the drafter finder. ok is false for
// any other message.
func (v *modelsView) updateFlow(msg tea.Msg) (tea.Cmd, bool) {
	switch msg := msg.(type) {
	case draftersFound:
		if msg.err != nil {
			return note(false, "drafters of %s: %v", msg.id, msg.err), true
		}
		i := slices.IndexFunc(v.list, func(m client.Model) bool { return m.ID == msg.id })
		if i < 0 {
			return nil, true
		}
		if len(msg.d.Drafters) == 0 {
			return note(false, "Hugging Face has no DFlash or EAGLE-3 drafter for %s or its base models", msg.id), true
		}
		return v.pickForm(v.list[i], msg.d), true
	case drafterRepo:
		if msg.err != nil {
			v.flow = nil
			return note(false, "%s: %v", msg.pick.Repo, msg.err), true
		}
		if v.flow == nil || len(msg.repo.Variants) == 0 {
			v.flow = nil
			return note(false, "%s has no complete GGUF file", msg.pick.Repo), true
		}
		return v.variantForm(msg.repo), true
	case drafterTestPoll:
		id := msg.id
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			t, err := v.c.DrafterTest(ctx, id)
			return drafterTestState{id, t, err}
		}, true
	case drafterTestState:
		switch {
		case msg.err != nil:
			v.flow = nil
			return note(false, "drafter test of %s: %v", msg.id, msg.err), true
		case msg.t.Status == "running":
			id := msg.id
			return tea.Batch(note(true, "testing the drafter on %s", id),
				tea.Tick(5*time.Second, func(time.Time) tea.Msg { return drafterTestPoll{id} })), true
		case msg.t.Status == "failed" || msg.t.With == nil || msg.t.Without == nil:
			v.flow = nil
			return note(false, "drafter test of %s failed: %s", msg.id, msg.t.Error), true
		case v.flow == nil || v.flow.target.ID != msg.id:
			return note(true, "drafter test of %s: %.2f times as fast", msg.id, msg.t.Speedup), true
		}
		return v.enableForm(msg.t), true
	}
	return nil, false
}
