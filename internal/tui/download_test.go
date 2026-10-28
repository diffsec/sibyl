package tui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
)

// A paste arrives as tea.PasteMsg, which the app passes to update, not key.
func TestDownloadSearchTakesPaste(t *testing.T) {
	v := newDownloadView(nil)
	v.key(tea.KeyPressMsg{Code: '/', Text: "/"})
	if v.stage != stageSearch {
		t.Fatalf("stage %v after /", v.stage)
	}
	v.update(tea.PasteMsg{Content: "jdominick05/Qwen3.8-Flash-Next-REAP-288-44GB-262k-GGUF"})
	if got := v.input.Value(); got != "jdominick05/Qwen3.8-Flash-Next-REAP-288-44GB-262k-GGUF" {
		t.Fatalf("search box %q after a paste", got)
	}
}
