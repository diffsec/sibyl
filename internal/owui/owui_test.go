package owui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

// fakeAuth models Open WebUI accounts, the admin config and API keys.
type fakeAuth struct {
	fakeOWUI
	users      map[string]string // email -> password
	enableKeys bool
	apiKey     string
	created    int
	cfgPosted  map[string]any
}

func (f *fakeAuth) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	tokenOK := r.Header.Get("Authorization") == "Bearer session"
	switch r.URL.Path {
	case "/api/v1/auths/signup":
		if len(f.users) > 0 {
			w.WriteHeader(http.StatusForbidden)
			_, _ = w.Write([]byte(`{"detail":"Access prohibited"}`))
			return
		}
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		f.users[b["email"]] = b["password"]
		_, _ = w.Write([]byte(`{"token":"session","role":"admin"}`))
	case "/api/v1/auths/signin":
		var b map[string]string
		_ = json.NewDecoder(r.Body).Decode(&b)
		if f.users[b["email"]] != b["password"] || b["password"] == "" {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"detail":"The email or password provided is incorrect."}`))
			return
		}
		_, _ = w.Write([]byte(`{"token":"session","role":"admin"}`))
	case "/api/v1/auths/admin/config":
		if !tokenOK {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		if r.Method == http.MethodPost {
			f.cfgPosted = map[string]any{}
			_ = json.NewDecoder(r.Body).Decode(&f.cfgPosted)
			f.enableKeys, _ = f.cfgPosted["ENABLE_API_KEYS"].(bool)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"ENABLE_API_KEYS": f.enableKeys, "WEBUI_URL": "http://x", "ENABLE_SIGNUP": false})
	case "/api/v1/auths/api_key":
		if !tokenOK || !f.enableKeys {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		if r.Method == http.MethodPost {
			f.created++
			f.apiKey = fmt.Sprintf("sk-%d", f.created)
		} else if f.apiKey == "" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]string{"api_key": f.apiKey})
	default:
		f.fakeOWUI.ServeHTTP(w, r)
	}
}

func TestConnectFlow(t *testing.T) {
	f := &fakeAuth{users: map[string]string{}}
	srv := httptest.NewServer(f)
	defer srv.Close()
	ctx := context.Background()
	c := New(srv.URL, "")

	if _, err := c.SignIn(ctx, "a@b.c", "pw"); err == nil {
		t.Fatal("sign in before any account exists must fail")
	}
	token, err := c.SignUp(ctx, "Admin", "a@b.c", "pw")
	if err != nil || token != "session" {
		t.Fatalf("sign up: %q %v", token, err)
	}
	if _, err := c.SignUp(ctx, "Other", "x@y.z", "pw"); !errors.Is(err, ErrAdminExists) {
		t.Errorf("second sign up: %v, want ErrAdminExists", err)
	}
	changed, err := c.EnableAPIKeys(ctx, token)
	if err != nil || !changed || f.cfgPosted["WEBUI_URL"] != "http://x" {
		t.Fatalf("enable keys: changed %v, err %v, posted %v", changed, err, f.cfgPosted)
	}
	if changed, _ := c.EnableAPIKeys(ctx, token); changed {
		t.Error("enabling keys twice posted the config again")
	}
	key, err := c.APIKey(ctx, token)
	if err != nil || key != "sk-1" {
		t.Fatalf("first key: %q %v", key, err)
	}
	// A second call reuses the key; creating one would replace it.
	if key, _ := c.APIKey(ctx, token); key != "sk-1" || f.created != 1 {
		t.Errorf("second key: %q, created %d", key, f.created)
	}
	if _, err := c.SignIn(ctx, "a@b.c", "pw"); err != nil {
		t.Errorf("sign in: %v", err)
	}
}
