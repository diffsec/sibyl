package api

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/diffsec/sibyl/internal/gguf"
	"github.com/diffsec/sibyl/internal/hf"
)

// TestDrafterFinder checks that the finder starts from the download record,
// follows base_model up the lineage, marks a drafter for the model itself
// as "model" and one for its base as "base", marks a drafter that is
// already installed, and caches the search.
func TestDrafterFinder(t *testing.T) {
	f := newFixture(t)
	var searches atomic.Int32
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/models/u/Fine-GGUF":
			_, _ = io.WriteString(w, `{"cardData":{"base_model":"u/Fine"}}`)
		case "/api/models/u/Fine":
			_, _ = io.WriteString(w, `{"cardData":{"base_model":["Qwen/Base"]}}`)
		case "/api/models/Qwen/Base":
			_, _ = io.WriteString(w, `{"cardData":{}}`)
		case "/api/models":
			searches.Add(1)
			q := r.URL.Query()
			base := ""
			for _, v := range q["filter"] {
				if b, ok := strings.CutPrefix(v, "base_model:"); ok {
					base = b
				}
			}
			switch {
			case base == "u/Fine" && q.Get("search") == "dflash":
				_, _ = io.WriteString(w, `[{"id":"u/Fine-DFlash-GGUF","downloads":3}]`)
			case base == "Qwen/Base" && q.Get("search") == "dflash":
				_, _ = io.WriteString(w, `[{"id":"z/Base-DFlash2-GGUF","downloads":400}]`)
			default:
				_, _ = io.WriteString(w, `[]`)
			}
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(hub.Close)
	f.server.HF = hf.New(hub.URL, "")

	header := func(arch string) string {
		data, err := gguf.Encode(map[string]any{"general.architecture": arch, arch + ".block_count": 2, arch + ".context_length": 4096})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	write(t, filepath.Join(f.paths.Models, "Fine-Q8_0", "Fine-Q8_0.gguf"), header("llama"))
	write(t, filepath.Join(f.paths.Models, "Fine-Q8_0", ".sibyl-source.json"), `{"repo":"u/Fine-GGUF"}`)
	write(t, filepath.Join(f.paths.Models, "Base-DFlash2-Q8_0", "Base-DFlash2-Q8_0.gguf"), header("dflash"))
	write(t, filepath.Join(f.paths.Models, "Base-DFlash2-Q8_0", ".sibyl-source.json"), `{"repo":"z/Base-DFlash2-GGUF"}`)

	code, body := f.doRaw(t, "GET", "/models/fine-q8_0/drafters")
	if code != http.StatusOK {
		t.Fatalf("drafters = %d %s", code, body)
	}
	var v draftersView
	if err := json.Unmarshal(body, &v); err != nil {
		t.Fatal(err)
	}
	if v.Source != "u/Fine-GGUF" || v.Lineage["Qwen/Base"] != 2 || len(v.Drafters) != 2 {
		t.Fatalf("view = %+v", v)
	}
	first, second := v.Drafters[0], v.Drafters[1]
	if first.Repo != "u/Fine-DFlash-GGUF" || first.Match != "model" || first.SpecType != "draft-dflash" {
		t.Errorf("first = %+v: the model's own drafter must come first", first)
	}
	if second.Repo != "z/Base-DFlash2-GGUF" || second.Match != "base" || second.Installed != "base-dflash2-q8_0" {
		t.Errorf("second = %+v", second)
	}

	n := searches.Load()
	if code, _ := f.doRaw(t, "GET", "/models/fine-q8_0/drafters"); code != http.StatusOK || searches.Load() != n {
		t.Errorf("a second request searched again: %d searches, want %d", searches.Load(), n)
	}
	if code, _ := f.doRaw(t, "GET", "/models/base-dflash2-q8_0/drafters"); code != http.StatusUnprocessableEntity {
		t.Errorf("drafters of a drafter = %d, want 422", code)
	}
	if code, _ := f.doRaw(t, "GET", "/models/gone/drafters"); code != http.StatusNotFound {
		t.Errorf("drafters of a missing model = %d, want 404", code)
	}
}
