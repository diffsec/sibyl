package tui

import (
	"regexp"
	"strings"
	"unicode/utf8"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
)

// The frame has three parts: the tab bar at the top, the view in the
// middle, and the note and the action buttons at the bottom. The view
// scrolls in the rows that are left, so the buttons stay on the screen on
// a small console.

// segment is one entry of a view's key line, "n new client". A hint such
// as "↑/↓ select" names more than one key and is not a button.
type segment struct {
	key, label string
	hint       bool
}

// parseHelp splits a key line into its entries.
func parseHelp(h string) []segment {
	var out []segment
	for _, part := range strings.Split(h, " · ") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		key, label, _ := strings.Cut(part, " ")
		out = append(out, segment{key: key, label: label, hint: strings.Contains(key, "/") && key != "/"})
	}
	return out
}

// zone is a clickable part of the screen.
type zone struct {
	x0, x1, y int // x1 is exclusive
	tab       int // the tab to show, or -1
	key       string
}

// keyMsg turns a key name from a key line into the key press it stands for.
func keyMsg(k string) (tea.KeyPressMsg, bool) {
	switch k {
	case "enter":
		return tea.KeyPressMsg{Code: tea.KeyEnter}, true
	case "esc":
		return tea.KeyPressMsg{Code: tea.KeyEscape}, true
	case "tab":
		return tea.KeyPressMsg{Code: tea.KeyTab}, true
	case "shift+tab":
		return tea.KeyPressMsg{Code: tea.KeyTab, Mod: tea.ModShift}, true
	}
	if mod, rest, ok := strings.Cut(k, "+"); ok && mod == "ctrl" && utf8.RuneCountInString(rest) == 1 {
		r, _ := utf8.DecodeRuneInString(rest)
		return tea.KeyPressMsg{Code: r, Mod: tea.ModCtrl}, true
	}
	if utf8.RuneCountInString(k) == 1 {
		r, _ := utf8.DecodeRuneInString(k)
		return tea.KeyPressMsg{Code: r, Text: k}, true
	}
	return tea.KeyPressMsg{}, false
}

// buttonBar lays out the key line as buttons, wrapped to width, and returns
// the lines and the zones of the buttons. top is the screen row of the
// first line.
func buttonBar(segs []segment, width, top int) ([]string, []zone) {
	var lines []string
	var zones []zone
	var cur strings.Builder
	x := 0
	flush := func() {
		lines = append(lines, cur.String())
		cur.Reset()
		x = 0
	}
	for _, s := range segs {
		text := s.key + " " + s.label
		if !s.hint {
			text = "[" + s.key + "] " + s.label
		}
		w := ansi.StringWidth(text)
		if x > 0 && x+2+w > width {
			flush()
		}
		if x > 0 {
			cur.WriteString("  ")
			x += 2
		}
		if s.hint {
			cur.WriteString(mutedSty.Render(text))
		} else {
			cur.WriteString(btnSty.Render(text))
			zones = append(zones, zone{x0: x, x1: x + w, y: top + len(lines), tab: -1, key: s.key})
		}
		x += w
	}
	if x > 0 {
		flush()
	}
	return lines, zones
}

// focusLine finds the line to keep in sight: the list cursor, or the
// focused field of a form.
func focusLine(lines []string) int {
	for i, l := range lines {
		p := ansi.Strip(l)
		if strings.HasPrefix(p, "› ") || strings.HasPrefix(p, "┃") {
			return i
		}
	}
	return -1
}

// scroller keeps one view's scroll position.
type scroller struct {
	offset int
	focus  int // the focus line at the last render
}

// window returns the lines to show in height rows. The view scrolls to
// keep a moved focus line in sight. Without a focus line, or when the
// focus has not moved, the wheel position stays.
func (s *scroller) window(lines []string, height int) []string {
	if height <= 0 {
		return nil
	}
	maxOff := max(len(lines)-height, 0)
	// When the lines do not fit, the last row holds the "more" hint, so
	// the focus line must sit above it.
	room := height
	if maxOff > 0 {
		room = max(height-1, 1)
	}
	if f := focusLine(lines); f >= 0 && f != s.focus {
		s.focus = f
		switch {
		case f < s.offset:
			s.offset = max(f-1, 0)
		case f >= s.offset+room:
			s.offset = f - room + 1
		}
	}
	s.offset = min(max(s.offset, 0), maxOff)
	out := lines[s.offset:min(s.offset+height, len(lines))]
	if s.offset > 0 || s.offset < maxOff {
		out = append([]string(nil), out...)
		more := ""
		switch {
		case s.offset > 0 && s.offset < maxOff:
			more = "↑↓ more · wheel or PgUp/PgDn scrolls"
		case s.offset > 0:
			more = "↑ more above · wheel or PgUp scrolls"
		default:
			more = "↓ more below · wheel or PgDn scrolls"
		}
		out[len(out)-1] = mutedSty.Render(more)
	}
	return out
}

// scroll moves the view by n lines.
func (s *scroller) scroll(n int) { s.offset = max(s.offset+n, 0) }

// fit cuts each line to width, so a long line does not wrap and push the
// buttons off the screen.
func fit(lines []string, width int) []string {
	if width <= 0 {
		return lines
	}
	for i, l := range lines {
		if ansi.StringWidth(l) > width {
			lines[i] = ansi.Truncate(l, width, "…")
		}
	}
	return lines
}

// pointClicker is a view that takes clicks on more than its list rows. It
// gets the line of its last render and the column.
type pointClicker interface {
	clickAt(line, x int) tea.Cmd
}

// rowClicker is a view whose list rows take clicks. clickLine gets the
// line of the view's last render, and selects the row on it; a click on the
// selected row opens it, as enter does.
type rowClicker interface {
	clickLine(line int) tea.Cmd
}

// rowMap records the line of each list row while a view renders, for
// clickLine.
type rowMap struct{ lines map[int]int }

func (r *rowMap) reset() { r.lines = map[int]int{} }

// mark records that row starts on the next line that b gets.
func (r *rowMap) mark(b *strings.Builder, row int) {
	if r.lines == nil {
		r.reset()
	}
	r.lines[strings.Count(b.String(), "\n")] = row
}

// click selects the row on line, or opens it with enter when it is already
// selected.
func (r *rowMap) click(line int, cursor *int, update func(tea.Msg) tea.Cmd) tea.Cmd {
	row, ok := r.lines[line]
	if !ok {
		return nil
	}
	if row == *cursor {
		return update(tea.KeyPressMsg{Code: tea.KeyEnter})
	}
	*cursor = row
	return nil
}

// twoPaneWidth is the screen width from which the details sit next to the
// list.
const twoPaneWidth = 100

// twoPane puts the details next to the list on a wide screen, and under it
// on a narrow one.
func twoPane(list, details string, width int) string {
	if width < twoPaneWidth {
		return list + "\n" + details
	}
	lw := min(max(width*2/5, 32), 56)
	// A list row stays on one line, so a click maps to its row.
	rows := strings.Split(strings.TrimRight(list, "\n"), "\n")
	for i, r := range rows {
		rows[i] = ansi.Truncate(r, lw, "…")
	}
	left := lipgloss.NewStyle().Width(lw).Render(strings.Join(rows, "\n"))
	right := lipgloss.NewStyle().Border(lipgloss.NormalBorder(), false, false, false, true).
		BorderForeground(muted).PaddingLeft(1).Width(width - lw - 2).Render(strings.TrimRight(details, "\n"))
	return lipgloss.JoinHorizontal(lipgloss.Top, left, " ", right) + "\n"
}

// columns puts two blocks of lines side by side, row by row. Each line of
// left keeps its line number, so a click maps to its row. Lines are cut to
// their column, not wrapped.
func columns(left, right string, width, lw int) string {
	l := strings.Split(strings.TrimRight(left, "\n"), "\n")
	r := strings.Split(strings.TrimRight(right, "\n"), "\n")
	rw := max(width-lw-3, 1)
	sep := mutedSty.Render(" │ ")
	var b strings.Builder
	for i := range max(len(l), len(r)) {
		var a, c string
		if i < len(l) {
			a = ansi.Truncate(l[i], lw, "…")
		}
		if i < len(r) {
			c = ansi.Truncate(r[i], rw, "…")
		}
		b.WriteString(a + strings.Repeat(" ", max(lw-ansi.StringWidth(a), 0)) + sep + c + "\n")
	}
	return b.String()
}

// redrawMsg asks a huh form to redraw. huh passes it to its fields, which
// ignore it.
type redrawMsg struct{}

// formClicker is a view with an open form that takes clicks. It gets the
// line of its last render, which is the form's own view.
type formClicker interface {
	clickForm(line, x int) tea.Cmd
}

// focusedBlock returns the first and last line of the focused field, which
// huh draws with a "┃" bar, or -1.
func focusedBlock(view string) (int, int) {
	first, last := -1, -1
	for i, l := range strings.Split(ansi.Strip(view), "\n") {
		if strings.HasPrefix(l, "┃") {
			if first < 0 {
				first = i
			}
			last = i
		} else if first >= 0 {
			break
		}
	}
	return first, last
}

// formClick moves the focus of form to the field on line, one field at a
// time, in the form's current group. On the field that has focus, a click
// on an option of a list moves the cursor to it, a click on a yes or no
// button picks it, and a click on an arrow of an inline list moves it.
func formClick(form *huh.Form, line, x int) tea.Cmd {
	if form == nil {
		return nil
	}
	var cmds []tea.Cmd
	for range 64 {
		first, last := focusedBlock(form.View())
		if first < 0 {
			break
		}
		if line >= first && line <= last {
			cmds = append(cmds, pickAt(form, line, x))
			break
		}
		before := form.GetFocusedField()
		msg := huh.NextField()
		if line < first {
			msg = huh.PrevField()
		}
		_, cmd := form.Update(msg)
		if form.GetFocusedField() == before {
			// The first or last field: a further step would leave the
			// group, and on the last group submit the form. Keep the
			// focus where it was, and drop the step.
			cmds = append(cmds, before.Focus())
			// huh blurred the field before it found no next one. Any
			// message makes the group redraw it with its focus.
			_, cmd := form.Update(redrawMsg{})
			cmds = append(cmds, cmd)
			break
		}
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}

// pickAt acts on a click at line and column x of the focused field.
func pickAt(form *huh.Form, line, x int) tea.Cmd {
	lines := strings.Split(ansi.Strip(form.View()), "\n")
	if line < 0 || line >= len(lines) {
		return nil
	}
	if c, ok := form.GetFocusedField().(*huh.Confirm); ok {
		return pickConfirm(form, c, lines, line, x)
	}
	if cmd, ok := pickArrow(form, lines[line], x); ok {
		return cmd
	}
	return pickOption(form, line)
}

// buttonRE matches a button label of a confirm: words with one space
// between them.
var buttonRE = regexp.MustCompile(`[^\s┃]+(?: [^\s┃]+)*`)

// pickConfirm sets a confirm to the button under the click. huh draws the
// two buttons on the last line of the field, yes first. The toggle key
// changes the value without moving to the next field, so a click never
// submits the form.
func pickConfirm(form *huh.Form, c *huh.Confirm, lines []string, line, x int) tea.Cmd {
	first, last := focusedBlock(strings.Join(lines, "\n"))
	for last > first && strings.TrimSpace(strings.TrimPrefix(lines[last], "┃")) == "" {
		last--
	}
	if line != last {
		return nil
	}
	l := lines[line]
	idx := buttonRE.FindAllStringIndex(l, 2)
	if len(idx) < 2 {
		return nil
	}
	// The columns of the end of the first label and the start of the
	// second, as screen cells.
	end1, start2 := ansi.StringWidth(l[:idx[0][1]]), ansi.StringWidth(l[:idx[1][0]])
	want := x < (end1+start2)/2
	if cur, _ := c.GetValue().(bool); cur == want {
		return nil
	}
	_, cmd := form.Update(tea.KeyPressMsg{Code: tea.KeyLeft})
	return cmd
}

// pickArrow moves an inline list, which huh draws as "← value →", when the
// click is on an arrow.
func pickArrow(form *huh.Form, l string, x int) (tea.Cmd, bool) {
	left, right := strings.Index(l, "← "), strings.LastIndex(l, " →")
	if left < 0 || right < 0 || right < left {
		return nil, false
	}
	lx, rx := ansi.StringWidth(l[:left]), ansi.StringWidth(l[:right])+1
	var key tea.KeyPressMsg
	switch {
	case x >= lx-1 && x <= lx+1:
		key = tea.KeyPressMsg{Code: tea.KeyLeft}
	case x >= rx-1 && x <= rx+1:
		key = tea.KeyPressMsg{Code: tea.KeyRight}
	default:
		return nil, true
	}
	_, cmd := form.Update(key)
	return cmd, true
}

// pickOption moves a list's cursor to the option on line. huh marks the
// cursor's option with "> ".
func pickOption(form *huh.Form, line int) tea.Cmd {
	view := form.View()
	lines := strings.Split(ansi.Strip(view), "\n")
	first, last := focusedBlock(view)
	if first < 0 || line < first || line > last {
		return nil
	}
	// Only inside the focused field: a text input also draws "> ".
	cursor := -1
	for i := first; i <= last; i++ {
		if strings.HasPrefix(strings.TrimLeft(strings.TrimPrefix(lines[i], "┃"), " "), "> ") {
			cursor = i
			break
		}
	}
	// A text input's prompt is on the line after its title, and it has no
	// options to move to.
	if cursor < 0 || line == cursor || line == first || cursor == first+1 && last == first+1 {
		return nil
	}
	key := tea.KeyPressMsg{Code: tea.KeyDown}
	n := line - cursor
	if n < 0 {
		key, n = tea.KeyPressMsg{Code: tea.KeyUp}, -n
	}
	var cmds []tea.Cmd
	for range n {
		_, cmd := form.Update(key)
		cmds = append(cmds, cmd)
	}
	return tea.Batch(cmds...)
}
