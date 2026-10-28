package tui

import "fmt"

// Options change how the app starts.
type Options struct {
	// Wizard walks through the setup steps in order. It runs with sibyl setup.
	Wizard bool
	// Fingerprint is the SHA-256 fingerprint of the host's TLS certificate,
	// which the last wizard step shows.
	Fingerprint string
}

// wizardStep is one step of the setup wizard: a screen and what to do on it.
type wizardStep struct {
	title string
	view  int // index into App.views
	hint  string
}

// The order follows the setup wizard in docs/design.md. Each step uses the
// normal screen, so every step can run again later without the wizard.
var wizardSteps = []wizardStep{
	{"GPUs, backend and layout", 1, "press e to choose the GPUs, the backend and the default layout"},
	{"Hostname and network", 4, "press h for the hostname and n for the network"},
	{"Operator accounts and SSH keys", 4, "press a to add an operator who can run sibyl over SSH"},
	{"Open WebUI administrator", 0, "press o to create the first Open WebUI admin"},
	{"First model", 3, "press / to search Hugging Face; fit shows what suits your GPUs"},
	{"Done", 0, ""}, // the Status screen, with the addresses
}

func (a *App) wizardHeader() string {
	s := wizardSteps[a.step]
	head := fmt.Sprintf("Setup %d/%d: %s", a.step+1, len(wizardSteps), s.title)
	if a.step == len(wizardSteps)-1 {
		return titleSty.Render(head) + "\n" +
			"  The Status screen (1) lists the addresses of Open WebUI, Arcane and the APIs as links.\n" +
			"  API clients need the key in /var/lib/sibyl-operator/api.key.\n" +
			"  TLS certificate SHA-256: " + a.opts.Fingerprint + "\n" +
			mutedSty.Render("  [ back · q quit · run sibyl setup again at any time") + "\n\n"
	}
	return titleSty.Render(head) + "  " + s.hint + "\n" + mutedSty.Render("  ] next step · [ back") + "\n\n"
}

// wizardMove changes the step and shows its screen.
func (a *App) wizardMove(delta int) bool {
	n := a.step + delta
	if n < 0 || n >= len(wizardSteps) {
		return false
	}
	a.step = n
	a.active = wizardSteps[n].view
	a.note = ""
	return true
}
