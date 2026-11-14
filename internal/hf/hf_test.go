package hf

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/diffsec/sibyl/internal/models"
)

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

const testRev = "4c577be098c274a1f579e072178db01dd3a8e449"

func treeServer(t *testing.T, tree string) *Client {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !strings.HasPrefix(r.URL.Path, "/api/models/o/Edge0-145B-q3k/tree/"+testRev) {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(tree))
	}))
	t.Cleanup(srv.Close)
	return New(srv.URL, "")
}

// An Edge0 repository is offered as one folder. Its GGUF files are parts
// of the folder, so they are not offered alone.
func TestRepoVariantsEdge0(t *testing.T) {
	sha, blob := strings.Repeat("a", 64), strings.Repeat("b", 40)
	c := treeServer(t, `[
		{"type":"directory","path":"mtp","size":0,"oid":"`+blob+`"},
		{"type":"file","path":".gitattributes","size":10,"oid":"`+blob+`"},
		{"type":"file","path":"assets/hero.png","size":99,"oid":"`+blob+`","lfs":{"oid":"`+sha+`"}},
		{"type":"file","path":"model.json","size":20,"oid":"`+blob+`"},
		{"type":"file","path":"experts.bin","size":4000,"oid":"`+blob+`","lfs":{"oid":"`+sha+`"}},
		{"type":"file","path":"model-skeleton.gguf","size":600,"oid":"`+blob+`","lfs":{"oid":"`+sha+`"}},
		{"type":"file","path":"mtp/experts.bin","size":70,"oid":"`+blob+`","lfs":{"oid":"`+sha+`"}},
		{"type":"file","path":"start.sh","size":5,"oid":"not-a-digest"}
	]`)
	vs, err := c.RepoVariants(context.Background(), "o/Edge0-145B-q3k", testRev)
	if err != nil || len(vs) != 1 {
		t.Fatalf("variants = %+v, err = %v", vs, err)
	}
	v := vs[0]
	if v.Name != "Edge0-145B-q3k" || v.Engine != models.EngineEdge0 || v.Size != 4690 || v.Shards != 4 ||
		v.RAM != 4000+models.Edge0RAMOverhead {
		t.Errorf("variant = %+v", v)
	}
	for _, f := range v.Files {
		lfs := f.Path != "model.json"
		if f.Dest != f.Path || (f.SHA256 != "") != lfs || (f.GitOID != "") == lfs {
			t.Errorf("file = %+v", f)
		}
	}
}

// A repository with no strata pack at its root keeps its GGUF variants.
func TestRepoVariantsGGUF(t *testing.T) {
	sha := strings.Repeat("a", 64)
	c := treeServer(t, `[
		{"type":"file","path":"model.json","size":20,"oid":"`+strings.Repeat("b", 40)+`"},
		{"type":"file","path":"Model-Q4_K_M.gguf","size":600,"lfs":{"oid":"`+sha+`"}}
	]`)
	vs, err := c.RepoVariants(context.Background(), "o/Edge0-145B-q3k", testRev)
	if err != nil || len(vs) != 1 || vs[0].Name != "Model-Q4_K_M" || vs[0].Engine != "" {
		t.Fatalf("variants = %+v, err = %v", vs, err)
	}
}
