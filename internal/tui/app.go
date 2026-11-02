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
	"github.com/charmbracelet/x/ansi"

	"github.com/diffsec/sibyl/internal/client"
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
	btnSty   = lipgloss.NewStyle().Foreground(accent)
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
	height int
	note   string
	noteOK bool
	opts   Options
	step   int // wizard step

	// The layout of the last frame, for the mouse: the clickable zones,
	// the screen row of the view's first line, and its scroll position.
	zones    []zone
	bodyTop  int
	bodyRows int
	scroll   map[int]*scroller

	// sel is the text selection of a mouse drag, and frame the plain
	// lines of the last frame, for the text of a selection.
	sel   selection
	frame []string
}

// New returns the app for one API client.
func New(c *client.Client, opts Options) *App {
	a := &App{c: c, opts: opts, scroll: map[int]*scroller{}}
	a.views = []view{newStatusView(c), newHardwareView(c), newModelsView(c), newDownloadView(c), newHostView(c), newRouterView(c)}
	a.names = []string{"Status", "Hardware", "Models", "Download", "Host", "Router"}
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
		a.width, a.height = msg.Width, msg.Height
		listHeight = msg.Height - 12
	case tea.MouseClickMsg:
		// A press starts a selection. The release tells a click from a
		// drag.
		if msg.Button == tea.MouseLeft {
			a.sel = selection{active: true, x0: msg.X, y0: msg.Y, x1: msg.X, y1: msg.Y}
		}
		return a, nil
	case tea.MouseMotionMsg:
		if a.sel.active {
			a.sel.x1, a.sel.y1 = msg.X, msg.Y
			a.sel.moved = a.sel.moved || msg.X != a.sel.x0 || msg.Y != a.sel.y0
		}
		return a, nil
	case tea.MouseReleaseMsg:
		if !a.sel.active {
			return a, nil
		}
		s := a.sel
		a.sel.active = false
		if !s.moved {
			a.sel = selection{}
			return a, a.click(tea.Mouse{X: s.x0, Y: s.y0, Button: tea.MouseLeft})
		}
		return a, a.copySelection(s)
	case tea.MouseWheelMsg:
		switch msg.Button {
		case tea.MouseWheelUp:
			a.scroller().scroll(-3)
		case tea.MouseWheelDown:
			a.scroller().scroll(3)
		}
		return a, nil
	case noteMsg:
		a.note, a.noteOK = msg.text, msg.ok
		return a, nil
	case tea.KeyPressMsg:
		if msg.String() == "ctrl+c" {
			return a, tea.Quit
		}
		a.sel = selection{} // a key clears the highlight of the last copy
		if !a.views[a.active].capturing() {
			switch k := msg.String(); k {
			case "q":
				return a, tea.Quit
			case "pgup", "pgdown":
				n := max(a.bodyHeight()-1, 1)
				if k == "pgup" {
					n = -n
				}
				a.scroller().scroll(n)
				return a, nil
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
			case "tab", "shift+tab", "1", "2", "3", "4", "5", "6":
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
	a.zones = a.zones[:0]
	var head strings.Builder
	head.WriteString(titleSty.Render("Sibyl") + "  ")
	x := ansi.StringWidth("Sibyl  ")
	for i, n := range a.names {
		label := fmt.Sprintf("%d %s", i+1, n)
		if i == a.active {
			head.WriteString(tabOn.Render(label))
		} else {
			head.WriteString(tabOff.Render(label))
		}
		w := ansi.StringWidth(label)
		a.zones = append(a.zones, zone{x0: x, x1: x + w, y: 0, tab: i})
		head.WriteString("  ")
		x += w + 2
	}
	head.WriteString("\n\n")
	if a.opts.Wizard {
		head.WriteString(a.wizardHeader())
	}
	header := strings.Split(strings.TrimSuffix(head.String(), "\n"), "\n")

	keys := a.views[a.active].help()
	if !a.views[a.active].capturing() {
		keys += " · tab/1-6 switch · q quit"
	}
	width := a.width
	if width <= 0 {
		width = 80
	}
	var footer []string
	if a.note != "" {
		sty := badSty
		if a.noteOK {
			sty = goodSty
		}
		footer = append(footer, sty.Render(ansi.Truncate(a.note, width, "…")))
	}
	bodyH := a.height - len(header) - len(footer) - 1
	buttons, zones := buttonBar(parseHelp(keys), width, 0)
	bodyH -= len(buttons)
	body := fit(strings.Split(strings.TrimRight(a.views[a.active].render(width), "\n"), "\n"), width)
	if a.height > 0 {
		body = a.scroller().window(body, bodyH)
		for len(body) < bodyH {
			body = append(body, "")
		}
	}
	a.bodyTop, a.bodyRows = len(header), max(bodyH, 1)
	top := len(header) + len(body) + 1 + len(footer)
	for _, z := range zones {
		z.y += top
		a.zones = append(a.zones, z)
	}
	lines := append(append(append(header, body...), ""), footer...)
	lines = append(lines, buttons...)
	a.frame = a.frame[:0]
	for _, l := range lines {
		a.frame = append(a.frame, ansi.Strip(l))
	}
	lines = highlight(lines, a.sel)
	v := tea.NewView(strings.Join(lines, "\n"))
	v.AltScreen = true
	// Cell motion reports a move only while a button is down, so a drag
	// reaches the app and a still mouse costs nothing.
	v.MouseMode = tea.MouseModeCellMotion
	v.WindowTitle = "Sibyl"
	return v
}

// bodyHeight is the number of rows that the view had in the last frame.
func (a *App) bodyHeight() int { return max(a.bodyRows, 1) }

// scroller returns the active view's scroll position.
func (a *App) scroller() *scroller {
	s, ok := a.scroll[a.active]
	if !ok {
		s = &scroller{focus: -1}
		a.scroll[a.active] = s
	}
	return s
}

// click handles a left click on a tab, a button, or a list row.
func (a *App) click(m tea.Mouse) tea.Cmd {
	if m.Button != tea.MouseLeft {
		return nil
	}
	for _, z := range a.zones {
		if m.Y != z.y || m.X < z.x0 || m.X >= z.x1 {
			continue
		}
		if z.tab >= 0 {
			if a.views[a.active].capturing() {
				return note(false, "finish or discard the form first: esc discards")
			}
			a.active, a.note = z.tab, ""
			return a.views[a.active].activate()
		}
		if k, ok := keyMsg(z.key); ok {
			_, cmd := a.Update(k)
			return cmd
		}
		return nil
	}
	if m.Y < a.bodyTop {
		return nil
	}
	if a.views[a.active].capturing() {
		if fc, ok := a.views[a.active].(formClicker); ok {
			return fc.clickForm(m.Y-a.bodyTop+a.scroller().offset, m.X)
		}
		return nil
	}
	if pc, ok := a.views[a.active].(pointClicker); ok {
		return pc.clickAt(m.Y-a.bodyTop+a.scroller().offset, m.X)
	}
	if rc, ok := a.views[a.active].(rowClicker); ok {
		return rc.clickLine(m.Y - a.bodyTop + a.scroller().offset)
	}
	return nil
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
