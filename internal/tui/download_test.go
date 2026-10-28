package tui

import (
	"strings"
	"testing"

	tea "charm.land/bubbletea/v2"

	"github.com/diffsec/sibyl/internal/client"
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

func TestDownloadTokenForm(t *testing.T) {
	v := newDownloadView(nil)
	v.update(hfResult{hf: client.HuggingFace{Configured: true, User: "ihavespoons"}})
	if !strings.Contains(ansi.ReplaceAllString(v.render(120), ""), "signed in as ihavespoons") {
		t.Fatalf("no account shown:\n%s", v.render(120))
	}
	v.key(tea.KeyPressMsg{Code: 't', Text: "t"})
	if v.tokenForm == nil || !v.capturing() {
		t.Fatal("t did not open the token form")
	}
	if !strings.Contains(ansi.ReplaceAllString(v.render(120), ""), "Sign out") {
		t.Fatalf("no sign-out choice:\n%s", v.render(120))
	}
}
