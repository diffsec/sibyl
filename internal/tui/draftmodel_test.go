package tui

import (
	"strings"
	"testing"

	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

func draftFixture() *modelsView {
	v := newModelsView(nil)
	v.list = []client.Model{
		{Model: models.Model{ID: "flint"}},
		{Model: models.Model{ID: "flint-dflash", Drafter: true}, Meta: &models.Meta{Architecture: "dflash"}},
		{Model: models.Model{ID: "small"}},
		{Model: models.Model{ID: "embed"}, Settings: models.Settings{Embedding: true}},
	}
	return v
}

func TestDraftModelOptions(t *testing.T) {
	list := draftFixture().list
	keys := func(types []string, current string) string {
		var out []string
		for _, o := range draftChoices(list, "flint", types, current) {
			out = append(out, o.Key)
		}
		return strings.Join(out, ", ")
	}
	if got := keys(nil, ""); got != "None, flint-dflash (draft-dflash)" {
		t.Errorf("no draft-simple: %s", got)
	}
	if got := keys([]string{"draft-simple"}, ""); got != "None, flint-dflash (draft-dflash), small (draft-simple)" {
		t.Errorf("draft-simple: %s", got)
	}
	if got := keys(nil, "small"); !strings.Contains(got, "small (draft-simple)") {
		t.Errorf("the current plain model is gone: %s", got)
	}
}

func TestDraftSaveKeepsForm(t *testing.T) {
	v := draftFixture()
	v.editForm(v.list[0])
	if strings.Contains(ansi.Strip(v.render(120)), "•") {
		t.Errorf("a new form marks a change:\n%s", strings.SplitN(ansi.Strip(v.render(120)), "\n", 2)[0])
	}
	v.fields.DraftModel = "flint-dflash"
	var msg string
	for _, m := range drain(v.update(ctrlS)) {
		if n, ok := m.(noteMsg); ok {
			msg = n.text
		}
	}
	if v.form == nil || !strings.Contains(msg, "draft-dflash") {
		t.Errorf("ctrl+s with no type: form open %v, note %q", v.form != nil, msg)
	}
	if v.fields.DraftModel != "flint-dflash" {
		t.Error("the edit was lost")
	}
}
