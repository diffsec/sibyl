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
	touch(t, filepath.Join(dir, "Coder-7B", "Coder-7B-00001-of-00002.gguf"), 2)
	touch(t, filepath.Join(dir, "Coder-7B", "Coder-7B-00002-of-00002.gguf"), 2)

	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "coder-7b", Path: filepath.Join("Coder-7B", "Coder-7B-00001-of-00002.gguf"), Size: 4, Shards: 2},
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

func TestScanAll(t *testing.T) {
	main, pools := t.TempDir(), t.TempDir()
	touch(t, filepath.Join(main, "a.gguf"), 1)
	touch(t, filepath.Join(pools, "pool-1", "b", "B.gguf"), 2)
	touch(t, filepath.Join(pools, "pool-1", "a.gguf"), 3)
	touch(t, filepath.Join(pools, "pool-2", "c.gguf"), 4)
	touch(t, filepath.Join(pools, "other", "d.gguf"), 5)

	got, err := ScanAll(main, pools)
	if err != nil {
		t.Fatal(err)
	}
	want := []Model{
		{ID: "a", Path: "a.gguf", Size: 1, Shards: 1},
		{ID: "b", Path: filepath.Join("b", "B.gguf"), Pool: "pool-1", Size: 2, Shards: 1},
		{ID: "c", Path: "c.gguf", Pool: "pool-2", Size: 4, Shards: 1},
	}
	if len(got) != len(want) {
		t.Fatalf("got %+v", got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("model %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if r := got[1].Root(main, pools); r != filepath.Join(pools, "pool-1") {
		t.Errorf("root %s", r)
	}
	if got, err := ScanAll(main, filepath.Join(pools, "missing")); err != nil || len(got) != 1 {
		t.Errorf("missing pools dir: %v %v", got, err)
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
		{"safe args", Settings{Args: []string{"--threads=8", "--mlock"}}, false},
		{"batch in args", Settings{Args: []string{"--batch-size=2048"}}, true},
		{"batch", Settings{Batch: 4096, UBatch: 1024, SplitMode: "row"}, false},
		{"batch too small", Settings{Batch: 16}, true},
		{"ubatch over batch", Settings{Batch: 512, UBatch: 1024}, true},
		{"bad split mode", Settings{SplitMode: "tensor"}, true},
		{"shell chars", Settings{Args: []string{"--temp;rm"}}, true},
		{"reserved flag", Settings{Args: []string{"--port=1"}}, true},
		{"reserved short flag", Settings{Args: []string{"-m"}}, true},
		{"negative ctx", Settings{CtxSize: -1}, true},
		{"reasoning level", Settings{Reasoning: &Reasoning{Var: "reasoning_effort", Value: "xhigh"}}, false},
		{"reasoning switch", Settings{Reasoning: &Reasoning{Var: "enable_thinking", Value: "off"}}, false},
		{"switch with a level", Settings{Reasoning: &Reasoning{Var: "enable_thinking", Value: "high"}}, true},
		{"quote in a level", Settings{Reasoning: &Reasoning{Var: "reasoning_effort", Value: "max'"}}, true},
		{"unknown variable", Settings{Reasoning: &Reasoning{Var: "effort", Value: "high"}}, true},
		{"template kwargs by hand", Settings{Args: []string{"--chat-template-kwargs"}}, true},
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

func TestLoadSettingsMovesBatchArgs(t *testing.T) {
	p := filepath.Join(t.TempDir(), "models.yaml")
	body := "a:\n  args: [--batch-size=4096, -ub, \"1024\", --mlock]\nb:\n  batch: 2048\n  args: [-b, \"512\"]\n"
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := LoadSettings(p)
	if err != nil {
		t.Fatal(err)
	}
	if a := got["a"]; a.Batch != 4096 || a.UBatch != 1024 || len(a.Args) != 1 || a.Args[0] != "--mlock" {
		t.Errorf("a %+v", a)
	}
	if b := got["b"]; b.Batch != 2048 || len(b.Args) != 0 {
		t.Errorf("b keeps its typed batch: %+v", b)
	}
}

func TestScanEdge0Folder(t *testing.T) {
	dir := t.TempDir()
	folder := filepath.Join(dir, "Edge0-145B-q3k")
	touch(t, filepath.Join(folder, "experts.bin"), 40)
	touch(t, filepath.Join(folder, "model-skeleton.gguf"), 6)
	touch(t, filepath.Join(folder, "ple-fp8.gguf"), 51)
	touch(t, filepath.Join(folder, "mtp", "experts.bin"), 2)
	if err := os.WriteFile(filepath.Join(folder, "model.json"), []byte(`{"family":"Edge0","context":262144}`), 0o644); err != nil {
		t.Fatal(err)
	}
	// A folder with a model.json of another family stays a GGUF folder.
	touch(t, filepath.Join(dir, "other", "Other.gguf"), 3)
	if err := os.WriteFile(filepath.Join(dir, "other", "model.json"), []byte(`{"family":"X"}`), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := Scan(dir)
	if err != nil {
		t.Fatal(err)
	}
	manifest := int64(len(`{"family":"Edge0","context":262144}`))
	want := []Model{
		{ID: "edge0-145b-q3k", Path: "Edge0-145B-q3k", Engine: EngineEdge0, Context: 262144, RAM: 40 + Edge0RAMOverhead, Size: 99 + manifest, Shards: 1},
		{ID: "other", Path: filepath.Join("other", "Other.gguf"), Size: 3, Shards: 1},
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

func TestEdge0Context(t *testing.T) {
	m := Model{Engine: EngineEdge0, Context: 262144}
	for _, c := range []struct{ setting, want int }{
		{0, Edge0DefaultContext}, {65536, 65536}, {262144, 262144}, {500000, 262144},
	} {
		if got := m.Edge0Context(c.setting); got != c.want {
			t.Errorf("Edge0Context(%d) = %d, want %d", c.setting, got, c.want)
		}
	}
	if got := (Model{Context: 32768}).Edge0Context(0); got != 32768 {
		t.Errorf("a folder with a small context: got %d, want 32768", got)
	}
}
