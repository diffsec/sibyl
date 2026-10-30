package tui

import "testing"

func TestWindow(t *testing.T) {
	listHeight = 5
	for _, c := range []struct{ n, cursor, start, end int }{
		{3, 0, 0, 3},
		{20, 0, 0, 5},
		{20, 10, 8, 13},
		{20, 19, 15, 20},
	} {
		if s, e := window(c.n, c.cursor); s != c.start || e != c.end {
			t.Errorf("window(%d, %d) = %d,%d, want %d,%d", c.n, c.cursor, s, e, c.start, c.end)
		}
	}
}
