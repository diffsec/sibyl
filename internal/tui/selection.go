package tui

import (
	"fmt"
	"strings"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// A drag with the left button selects text, as in a terminal: from the
// cell of the press to the cell of the release, line by line. The release
// copies the text with OSC 52, a terminal escape code that sets the
// clipboard. A press and a release on the same cell is a click.

// selection is a drag on the screen. The cells are screen cells.
type selection struct {
	active bool // the button is down
	moved  bool // the mouse left the cell of the press
	x0, y0 int  // the press
	x1, y1 int  // the last move
}

var selSpan = lipgloss.NewStyle().Reverse(true)

// span returns the selection from its first cell to its last, in reading
// order, with the last cell included.
func (s selection) span() (x0, y0, x1, y1 int) {
	if s.y1 < s.y0 || s.y1 == s.y0 && s.x1 < s.x0 {
		return s.x1, s.y1, s.x0, s.y0
	}
	return s.x0, s.y0, s.x1, s.y1
}

// cols returns the cells of line y that the selection covers, end
// exclusive, or false.
func (s selection) cols(y int) (int, int, bool) {
	if !s.moved {
		return 0, 0, false
	}
	x0, y0, x1, y1 := s.span()
	if y < y0 || y > y1 {
		return 0, 0, false
	}
	from, to := 0, 1<<16
	if y == y0 {
		from = x0
	}
	if y == y1 {
		to = x1 + 1
	}
	return from, to, true
}

// selectedText returns the text of the selection from the plain lines of
// a frame. Each line loses its trailing spaces.
func selectedText(lines []string, s selection) string {
	var out []string
	for y, l := range lines {
		from, to, ok := s.cols(y)
		if !ok {
			continue
		}
		out = append(out, strings.TrimRight(ansi.Cut(l, from, to), " "))
	}
	return strings.Join(out, "\n")
}

// highlight shows the selection in reverse video.
func highlight(lines []string, s selection) []string {
	if !s.moved {
		return lines
	}
	out := append([]string(nil), lines...)
	for y, l := range out {
		from, to, ok := s.cols(y)
		if !ok {
			continue
		}
		w := ansi.StringWidth(l)
		to = min(to, w)
		if from >= to {
			continue
		}
		out[y] = ansi.Cut(l, 0, from) + selSpan.Render(ansi.Strip(ansi.Cut(l, from, to))) + ansi.Cut(l, to, w)
	}
	return out
}

// copySelection sends the text of a drag to the clipboard. The highlight
// stays until the next key or press.
func (a *App) copySelection(s selection) tea.Cmd {
	text := selectedText(a.frame, s)
	if strings.TrimSpace(text) == "" {
		a.sel = selection{}
		return nil
	}
	n := len([]rune(text))
	return tea.Batch(tea.SetClipboard(text),
		note(true, "copied %s to the clipboard", plural(n, "character")))
}

func plural(n int, word string) string {
	if n == 1 {
		return "1 " + word
	}
	return fmt.Sprintf("%d %ss", n, word)
}
