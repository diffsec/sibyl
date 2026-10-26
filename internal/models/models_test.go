package models

import (
	"os"
	"path/filepath"
	"testing"
)

func touch(t *testing.T, path string, size int) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestScan(t *testing.T) {
	dir := t.TempDir()
	touch(t, filepath.Join(dir, "Qwen3-14B-Q8_0.gguf"), 10)
	touch(t, filepath.Join(dir, "llama", "Llama-3.3-70B-Q4_K_M-00001-of-00002.gguf"), 7)
	touch(t, filepath.Join(dir, "llama", "Llama-3.3-70B-Q4_K_M-00002-of-00002.gguf"), 5)
	touch(t, filepath.Join(dir, "orphan-00002-of-00002.gguf"), 1)
	touch(t, filepath.Join(dir, "mmproj-F16.gguf"), 3)
	touch(t, filepath.Join(dir, ".partial", "x.gguf"), 3)
	touch(t, filepath.Join(dir, "readme.txt"), 3)

	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "llama--llama-3.3-70b-q4_k_m", Path: filepath.Join("llama", "Llama-3.3-70B-Q4_K_M-00001-of-00002.gguf"), Size: 12, Shards: 2},
		{ID: "qwen3-14b-q8_0", Path: "Qwen3-14B-Q8_0.gguf", Size: 10, Shards: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, got[i], want[i])
		}
	}
}

func TestSettingsValidate(t *testing.T) {
	cases := []struct {
		name    string
		s       Settings
		wantErr bool
	}{
		{"empty", Settings{}, false},
		{"span", Settings{Layout: "span"}, false},
		{"pin in range", Settings{Layout: "gpu:1"}, false},
		{"pin out of range", Settings{Layout: "gpu:2"}, true},
		{"bad layout", Settings{Layout: "both"}, true},
		{"safe args", Settings{Args: []string{"--temp=0.6", "--cache-type-k=q8_0"}}, false},
		{"shell chars", Settings{Args: []string{"--temp;rm"}}, true},
		{"reserved flag", Settings{Args: []string{"--port=1"}}, true},
		{"reserved short flag", Settings{Args: []string{"-m"}}, true},
		{"negative ctx", Settings{CtxSize: -1}, true},
	}
	for _, c := range cases {
		err := c.s.Validate(2)
		if (err != nil) != c.wantErr {
			t.Errorf("%s: err = %v, wantErr %v", c.name, err, c.wantErr)
		}
	}
}

func TestSettingsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.yaml")
	in := map[string]Settings{"a": {Layout: "gpu:0", CtxSize: 8192, Args: []string{"--temp=0.7"}}}
	if err := SaveSettings(path, in); err != nil {
		t.Fatal(err)
	}
	out, err := LoadSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	if out["a"].Layout != "gpu:0" || out["a"].CtxSize != 8192 || out["a"].Args[0] != "--temp=0.7" {
		t.Errorf("round trip = %+v", out)
	}
}
