package tui

import (
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/client"
	"github.com/diffsec/sibyl/internal/models"
)

func TestDrafterMark(t *testing.T) {
	two, none := 2, 0
	m := client.Model{Model: models.Model{ID: "big"}, Drafters: &two}
	if !strings.Contains(drafterMark(m), "2 drafter(s)") {
		t.Errorf("mark = %q", drafterMark(m))
	}
	m.Settings.Speculative.DraftModel = "d"
	if drafterMark(m) != "" {
		t.Error("a model with a draft model is marked")
	}
	if drafterMark(client.Model{Drafters: &none}) != "" || drafterMark(client.Model{}) != "" {
		t.Error("a model with no drafters, or no search yet, is marked")
	}
}
