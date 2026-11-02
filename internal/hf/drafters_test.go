package hf

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// fakeHub serves model cards with base_model fields, and a search that
// filters on base_model and the search term.
func fakeHub(t *testing.T, bases map[string]any, models []map[string]any) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/models" {
			q := r.URL.Query()
			var base string
			for _, f := range q["filter"] {
				if b, ok := strings.CutPrefix(f, "base_model:"); ok {
					base = b
				}
			}
			out := []map[string]any{}
			for _, m := range models {
				if m["base"] == base && strings.Contains(strings.ToLower(m["id"].(string)), q.Get("search")) {
					out = append(out, map[string]any{"id": m["id"], "downloads": m["downloads"]})
				}
			}
			_ = json.NewEncoder(w).Encode(out)
			return
		}
		repo := strings.TrimPrefix(r.URL.Path, "/api/models/")
		b, ok := bases[repo]
		if !ok {
			http.NotFound(w, r)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"cardData": map[string]any{"base_model": b}})
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

func TestLineage(t *testing.T) {
	c := fakeHub(t, map[string]any{
		"q/Fine-GGUF": "q/Fine",
		"q/Fine":      []string{"Qwen/Base"},
		"Qwen/Base":   nil,
		"o/Plain":     nil,
	}, nil)
	got := c.Lineage(context.Background(), "q/Fine-GGUF")
	want := map[string]int{"q/Fine-GGUF": 0, "q/Fine": 1, "Qwen/Base": 2}
	if len(got) != len(want) {
		t.Fatalf("lineage = %v, want %v", got, want)
	}
	for r, d := range want {
		if got[r] != d {
			t.Errorf("depth of %s = %d, want %d", r, got[r], d)
		}
	}
	// No base_model: the name without -GGUF is still tried.
	if got := c.Lineage(context.Background(), "o/Plain-GGUF"); got["o/Plain"] != 1 {
		t.Errorf("lineage of a card with no base_model = %v", got)
	}
}

func TestLineageIsBounded(t *testing.T) {
	bases := map[string]any{}
	for i := range 20 {
		bases["o/m"+string(rune('a'+i))] = "o/m" + string(rune('a'+i+1))
	}
	if got := fakeHub(t, bases, nil).Lineage(context.Background(), "o/ma"); len(got) != maxLineage {
		t.Errorf("lineage visited %d repositories, want %d", len(got), maxLineage)
	}
}

func TestSearchDrafters(t *testing.T) {
	c := fakeHub(t, nil, []map[string]any{
		{"id": "z/Base-DFlash2-GGUF", "base": "Qwen/Base", "downloads": 10},
		{"id": "e/Base-EAGLE3-GGUF", "base": "Qwen/Base", "downloads": 5},
		{"id": "z/Other-DFlash2-GGUF", "base": "Qwen/Other", "downloads": 99},
	})
	got, err := c.SearchDrafters(context.Background(), "Qwen/Base", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Repo != "z/Base-DFlash2-GGUF" || got[0].SpecType != "draft-dflash" ||
		got[1].SpecType != "draft-eagle3" || got[0].Base != "Qwen/Base" {
		t.Errorf("drafters = %+v", got)
	}
	if _, err := c.SearchDrafters(context.Background(), "../x", 10); err == nil {
		t.Error("an invalid base was accepted")
	}
}
