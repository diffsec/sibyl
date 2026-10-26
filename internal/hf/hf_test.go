package hf

import "testing"

func TestVariants(t *testing.T) {
	files := []File{
		{Path: "Model-Q4_K_M.gguf", Size: 10},
		{Path: "big/Model-Q8_0-00001-of-00002.gguf", Size: 5},
		{Path: "big/Model-Q8_0-00002-of-00002.gguf", Size: 6},
		{Path: "Model-IQ2-00001-of-00003.gguf", Size: 1}, // incomplete split set
		{Path: "mmproj-F16.gguf", Size: 3},
	}
	got := Variants(files)
	if len(got) != 2 {
		t.Fatalf("variants = %+v", got)
	}
	if got[0].Name != "Model-Q4_K_M" || got[0].Shards != 1 || got[0].Size != 10 {
		t.Errorf("single = %+v", got[0])
	}
	if got[1].Name != "big/Model-Q8_0" || got[1].Shards != 2 || got[1].Size != 11 {
		t.Errorf("split = %+v", got[1])
	}
}

func TestSafePathAndValidators(t *testing.T) {
	for p, want := range map[string]bool{
		"a.gguf": true, "dir/a.gguf": true, "../a.gguf": false, "/a.gguf": false,
		"a/../b.gguf": false, ".hidden/a.gguf": false, "a\\b.gguf": false, "": false,
	} {
		if got := SafePath(p); got != want {
			t.Errorf("SafePath(%q) = %v, want %v", p, got, want)
		}
	}
	for r, want := range map[string]bool{"owner/name": true, "o/n.v-1_2": true, "owner": false, "o/../x": false, "o/n/x": false} {
		if got := ValidRepo(r); got != want {
			t.Errorf("ValidRepo(%q) = %v, want %v", r, got, want)
		}
	}
	if ValidRevision("main") || !ValidRevision("4c577be098c274a1f579e072178db01dd3a8e449") {
		t.Error("ValidRevision")
	}
}

func TestFileURLEscapes(t *testing.T) {
	c := New("https://hf.example", "")
	got := c.FileURL("o/r", "abc", "dir/a b.gguf")
	if got != "https://hf.example/o/r/resolve/abc/dir/a%20b.gguf" {
		t.Errorf("FileURL = %s", got)
	}
}
