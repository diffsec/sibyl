package tui

import (
	"github.com/diffsec/sibyl/internal/modelform"
	"github.com/diffsec/sibyl/internal/models"
)

// settingsForm is the shared model settings form.
type settingsForm = modelform.Form

func formFromSettings(s models.Settings) settingsForm { return modelform.FromSettings(s) }

func fmtFloat(v *float64) string { return modelform.FmtFloat(v) }

func parseMoELayers(v string) (int, error) { return modelform.ParseMoELayers(v) }

func numberCheck(whole bool) func(string) error { return modelform.NumberCheck(whole) }
