package owui

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// fakeOWUI accepts only the key "admin" and stores the embedding config.
type fakeOWUI struct {
	engine, model, url string
	updates            int
}

func (f *fakeOWUI) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Header.Get("Authorization") != "Bearer admin" {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	switch r.URL.Path {
	case "/api/v1/retrieval/embedding":
		_ = json.NewEncoder(w).Encode(map[string]any{
			"RAG_EMBEDDING_ENGINE": f.engine, "RAG_EMBEDDING_MODEL": f.model,
			"openai_config": map[string]string{"url": f.url, "key": ""},
		})
	case "/api/v1/retrieval/embedding/update":
		var b struct {
			Engine string `json:"RAG_EMBEDDING_ENGINE"`
			Model  string `json:"RAG_EMBEDDING_MODEL"`
			Cfg    struct {
				URL string `json:"url"`
			} `json:"openai_config"`
		}
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.engine, f.model, f.url = b.Engine, b.Model, b.Cfg.URL
		f.updates++
		_, _ = w.Write([]byte(`{"status":true}`))
	default:
		http.NotFound(w, r)
	}
}

func TestSync(t *testing.T) {
	f := &fakeOWUI{model: "sentence-transformers/all-MiniLM-L6-v2"}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()

	if _, err := New(srv.URL, "user").Embedding(ctx); err == nil {
		t.Fatal("want an error for a non-admin key")
	}
	c := New(srv.URL, "admin")
	changed, err := c.Sync(ctx, "embed", "http://llama-swap:8080/v1")
	if err != nil || !changed {
		t.Fatalf("first sync: changed %v, err %v", changed, err)
	}
	if f.engine != "openai" || f.model != "embed" || f.url != "http://llama-swap:8080/v1" {
		t.Errorf("open webui config = %+v", f)
	}
	changed, err = c.Sync(ctx, "embed", "http://llama-swap:8080/v1")
	if err != nil || changed || f.updates != 1 {
		t.Errorf("second sync must not update: changed %v, updates %d, err %v", changed, f.updates, err)
	}
}
