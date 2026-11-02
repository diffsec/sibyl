package client

import (
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/models"
)

func TestOrderWithDrafters(t *testing.T) {
	m := func(id string, drafter bool, draft string) Model {
		c := Model{Model: models.Model{ID: id, Drafter: drafter}}
		c.Settings.Speculative.DraftModel = draft
		return c
	}
	list := []Model{m("a-drafter", true, ""), m("big", false, "b-drafter"), m("b-drafter", true, ""), m("small", false, "")}
	var ids []string
	for _, x := range OrderWithDrafters(list) {
		ids = append(ids, x.ID)
	}
	if got := strings.Join(ids, " "); got != "big b-drafter small a-drafter" {
		t.Errorf("order = %s, want each drafter after its model and the unused one last", got)
	}
}
