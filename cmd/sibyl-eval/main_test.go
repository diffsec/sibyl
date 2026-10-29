package main

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunWithoutSend replays a set against a fake box: the classifier
// labels every request as code/easy, and the profile cloud sends
// everything to a priced provider route.
func TestRunWithoutSend(t *testing.T) {
	var paths []string
	box := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		paths = append(paths, r.URL.Path)
		if r.Header.Get("Authorization") != "Bearer sys" {
			http.Error(w, "no", http.StatusUnauthorized)
			return
		}
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		switch r.URL.Path {
		case "/system/router/classify":
			_, _ = io.WriteString(w, `{"index":{"ready":true},"classification":{"task":"code","difficulty":"easy","ms":12}}`)
		case "/system/router/explain":
			model := body["request"].(map[string]any)["model"]
			if model == "auto:cloud" {
				_, _ = io.WriteString(w, `{"route":"p/big","price":{"input":2,"output":10},"features":{"prompt_tokens":1000}}`)
				return
			}
			_, _ = io.WriteString(w, `{"route":"local/small","features":{"prompt_tokens":1000}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer box.Close()

	dir := t.TempDir()
	set := filepath.Join(dir, "set.jsonl")
	_ = os.WriteFile(set, []byte(`{"prompt":"sort a slice","task":"code","difficulty":"easy","output_tokens":100}
{"prompt":"prove it","task":"math","difficulty":"hard"}
`), 0o600)
	key := filepath.Join(dir, "api.key")
	_ = os.WriteFile(key, []byte("sys\n"), 0o600)
	ca := filepath.Join(dir, "ca.crt")
	_ = os.WriteFile(ca, certPEM(t, box), 0o600)

	out := captureStdout(t, func() {
		if err := run(box.URL, key, "", ca, set, "auto,cloud", "default", false, "", ""); err != nil {
			t.Fatal(err)
		}
	})
	for _, want := range []string{
		"Task right 1 (50%). Difficulty right 1 (50%).",
		"auto     2         2 (100%)  0",
		// (1000*2 + 100*10)/1e6 + (1000*2 + 500*10)/1e6 = 0.003 + 0.007
		"cloud    2         0 (0%)    2         0.0100",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("output lacks %q:\n%s", want, out)
		}
	}
	for _, p := range paths {
		if strings.HasPrefix(p, "/v1/") {
			t.Errorf("a run without -send called %s", p)
		}
	}
	if err := run(box.URL, key, "", ca, set, "auto", "default", false, "judge-model", ""); err == nil {
		t.Error("-judge without -send was accepted")
	}
}

func certPEM(t *testing.T, s *httptest.Server) []byte {
	t.Helper()
	c := s.Certificate()
	return []byte("-----BEGIN CERTIFICATE-----\n" + b64(c.Raw) + "\n-----END CERTIFICATE-----\n")
}

func captureStdout(t *testing.T, f func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	old := os.Stdout
	os.Stdout = w
	done := make(chan string)
	go func() { b, _ := io.ReadAll(r); done <- string(b) }()
	f()
	os.Stdout = old
	_ = w.Close()
	return <-done
}

func b64(b []byte) string { return base64.StdEncoding.EncodeToString(b) }
