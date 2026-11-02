package tui

import (
	"reflect"
	"slices"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
)

// A long settings form shows one group at a time. The section bar above it
// names every group, marks the groups with a change, and jumps to a group
// on pgup, pgdn or a click.

// settingsSection is one group of the model settings form, and the fields
// of modelform.Form that it holds.
type settingsSection struct {
	title  string
	fields []string
}

var settingsSections = []settingsSection{
	{"Model", []string{"Layout", "Fit", "Parallel", "CtxSize", "TensorSplit", "KVTypeK", "KVTypeV", "Embedding", "Hidden"}},
	{"Mixture of Experts", []string{"MoELayers", "MoECacheMiB"}},
	{"Sampling defaults", []string{"Preset", "Temperature", "TopK", "TopP", "MinP", "RepeatPenalty", "PresencePenalty"}},
	{"Reasoning", []string{"Reasoning"}},
	{"Speculative decoding", []string{"SpecTypes", "DraftModel", "DraftMax", "DraftMin", "NgramModMin", "NgramModMax"}},
	{"Advanced", []string{"Args"}},
}

// visibleSections returns the sections that the form shows for m. The
// form hides the experts group for a dense model, and the reasoning group
// for a template with no reasoning control.
func visibleSections(m client.Model) []settingsSection {
	var out []settingsSection
	for _, s := range settingsSections {
		switch s.title {
		case "Mixture of Experts":
			if m.Meta == nil || m.Meta.ExpertCount == 0 {
				continue
			}
		case "Reasoning":
			if m.Meta == nil || m.Meta.Reasoning == nil {
				continue
			}
		}
		out = append(out, s)
	}
	return out
}

// changed tells whether a field of the section differs from its value when
// the form opened.
func (s settingsSection) changed(orig, cur *settingsForm) bool {
	a, b := reflect.ValueOf(orig).Elem(), reflect.ValueOf(cur).Elem()
	for _, f := range s.fields {
		if !reflect.DeepEqual(a.FieldByName(f).Interface(), b.FieldByName(f).Interface()) {
			return true
		}
	}
	return false
}

// currentSection finds the group that the form shows: huh writes the
// group's title above its fields.
func currentSection(view string, secs []settingsSection) int {
	for _, l := range strings.Split(ansi.Strip(view), "\n") {
		l = strings.TrimSpace(l)
		if l == "" {
			continue
		}
		for i, s := range secs {
			if l == s.title {
				return i
			}
		}
		// The title is the first line of a group. A group with no title
		// does not match.
		break
	}
	return -1
}

// sectionBar renders the section names on one line, the current one
// underlined and a changed one with a dot, and the zone of each name.
func sectionBar(secs []settingsSection, cur int, orig, now *settingsForm) (string, []zone) {
	var b strings.Builder
	var zones []zone
	x := 0
	for i, s := range secs {
		if i > 0 {
			b.WriteString(mutedSty.Render(" │ "))
			x += 3
		}
		name := s.title
		if s.changed(orig, now) {
			name += " •"
		}
		if i == cur {
			b.WriteString(tabOn.Render(name))
		} else {
			b.WriteString(tabOff.Render(name))
		}
		w := ansi.StringWidth(name)
		zones = append(zones, zone{x0: x, x1: x + w, tab: i})
		x += w
	}
	return b.String(), zones
}

// gotoSection moves form to the section at index target, one group at a
// time. It never moves past the last section, which would submit the form,
// and it stops when a group with an error does not let the form move.
func gotoSection(form *huh.Form, secs []settingsSection, target int) tea.Cmd {
	if form == nil || target < 0 || target >= len(secs) {
		return nil
	}
	var cmds []tea.Cmd
	for range secs {
		cur := currentSection(form.View(), secs)
		if cur < 0 || cur == target {
			break
		}
		if target > cur {
			cmds = append(cmds, form.NextGroup())
		} else {
			cmds = append(cmds, form.PrevGroup())
		}
		if currentSection(form.View(), secs) == cur {
			break
		}
	}
	return tea.Batch(cmds...)
}

// copySettings returns a deep copy, so later edits do not change it.
func copySettings(f settingsForm) settingsForm {
	f.SpecTypes = slices.Clone(f.SpecTypes)
	return f
}
