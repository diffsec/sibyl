// Package tui is the Sibyl console interface. It talks only to the System
// API, so it holds no host privileges of its own.
package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"

	"github.com/ihavespoons/sibyl/internal/client"
)

var (
	accent   = lipgloss.Color("#d97757")
	muted    = lipgloss.Color("#8a8a8a")
	good     = lipgloss.Color("#5fb36b")
	bad      = lipgloss.Color("#e05d5d")
	titleSty = lipgloss.NewStyle().Bold(true).Foreground(accent)
	tabOn    = lipgloss.NewStyle().Bold(true).Foreground(accent).Underline(true)
	tabOff   = lipgloss.NewStyle().Foreground(muted)
	mutedSty = lipgloss.NewStyle().Foreground(muted)
	goodSty  = lipgloss.NewStyle().Foreground(good)
	badSty   = lipgloss.NewStyle().Foreground(bad)
	selSty   = lipgloss.NewStyle().Bold(true).Foreground(accent)
)

// view is one screen of the app.
type view interface {
	// activate runs when the screen is shown.
	activate() tea.Cmd
	update(msg tea.Msg) tea.Cmd
	render(width int) string
	// capturing is true while a form or text input owns the keyboard.
	capturing() bool
	help() string
}

// App is the root Bubble Tea model.
type App struct {
	c      *client.Client
	views  []view
	names  []string
	active int
	width  int
	note   string
	noteOK bool
	opts   Options
	step   int // wizard step
}

// New returns the app for one API client.
func New(c *client.Client, opts Options) *App {
	a := &App{c: c, opts: opts}
	a.views = []view{newStatusView(c), newHardwareView(c), newModelsView(c), newDownloadView(c), newHostView(c)}
	a.names = []string{"Status", "Hardware", "Models", "Download", "Host"}
	if opts.Wizard {
		a.active = wizardSteps[0].view
	}
	return a
}

// noteMsg shows a one-line result at the bottom of the screen.
type noteMsg struct {
	text string
	ok   bool
}

func note(ok bool, format string, args ...any) tea.Cmd {
	return func() tea.Msg { return noteMsg{fmt.Sprintf(format, args...), ok} }
}

// call runs an API call with a timeout, off the UI goroutine.
func call[T any](fn func(context.Context) (T, error), wrap func(T, error) tea.Msg) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		return wrap(fn(ctx))
	}
}

func (a *App) Init() tea.Cmd { return a.views[a.active].activate() }

func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
		listHeight = msg.Height - 12
	case noteMsg:
		a.note, a.noteOK = msg.text, msg.ok
		return a, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
		if !a.views[a.active].capturing() {
			switch k := msg.String(); k {
			case "q":
				return a, tea.Quit
			case "]", "[":
				if a.opts.Wizard {
					delta := 1
					if k == "[" {
						delta = -1
					}
					if a.wizardMove(delta) {
						return a, a.views[a.active].activate()
					}
					return a, nil
				}
			case "tab", "shift+tab", "1", "2", "3", "4", "5":
				switch k {
				case "tab":
					a.active = (a.active + 1) % len(a.views)
				case "shift+tab":
					a.active = (a.active + len(a.views) - 1) % len(a.views)
				default:
					a.active = int(k[0] - '1')
				}
				a.note = ""
				return a, a.views[a.active].activate()
			}
		}
	}
	// Data messages go to every view, so a screen that is not shown stays current.
	if _, isKey := msg.(tea.KeyPressMsg); isKey {
		return a, a.views[a.active].update(msg)
	}
	var cmds []tea.Cmd
	for _, v := range a.views {
		cmds = append(cmds, v.update(msg))
	}
	return a, tea.Batch(cmds...)
}

func (a *App) View() tea.View {
	var b strings.Builder
	b.WriteString(titleSty.Render("Sibyl") + "  ")
	for i, n := range a.names {
		label := fmt.Sprintf("%d %s", i+1, n)
		if i == a.active {
			b.WriteString(tabOn.Render(label))
		} else {
			b.WriteString(tabOff.Render(label))
		}
		b.WriteString("  ")
	}
	b.WriteString("\n\n")
	if a.opts.Wizard {
		b.WriteString(a.wizardHeader())
	}
	b.WriteString(a.views[a.active].render(a.width))
	b.WriteString("\n")
	if a.note != "" {
		if a.noteOK {
			b.WriteString(goodSty.Render(a.note))
		} else {
			b.WriteString(badSty.Render(a.note))
		}
		b.WriteString("\n")
	}
	keys := a.views[a.active].help()
	if !a.views[a.active].capturing() {
		keys += " · tab/1-5 switch · q quit"
	}
	b.WriteString(mutedSty.Render(keys))
	v := tea.NewView(b.String())
	v.AltScreen = true
	v.WindowTitle = "Sibyl"
	return v
}

func gib(b int64) string { return fmt.Sprintf("%.1f GiB", float64(b)/(1<<30)) }

// listHeight is how many list rows fit: the screen less the tabs, titles,
// notes and key help around a list.
var listHeight = 15

// window returns the slice bounds of a list that keep cursor visible.
func window(n, cursor int) (start, end int) {
	h := max(listHeight, 3)
	if n <= h {
		return 0, n
	}
	start = min(max(cursor-h/2, 0), n-h)
	return start, start + h
}

// more renders a hint when a list has rows above or below the window.
func more(start, end, n int) string {
	if start == 0 && end == n {
		return ""
	}
	return mutedSty.Render(fmt.Sprintf("  rows %d-%d of %d", start+1, end, n)) + "\n"
}

func cursorLine(selected bool, s string) string {
	if selected {
		return selSty.Render("› " + s)
	}
	return "  " + s
}
