package bundle

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testdata/good was signed with cosign sign-blob and the project key, as CI
// does. testdata/sibyl-cosign.pub is the host's public key.
func copyDir(t *testing.T, src string) string {
	t.Helper()
	dst := t.TempDir()
	entries, err := os.ReadDir(src)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(src, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dst, e.Name()), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return dst
}

func TestVerify(t *testing.T) {
	key, err := LoadPublicKey("testdata/sibyl-cosign.pub")
	if err != nil {
		t.Fatal(err)
	}
	m, err := Verify("testdata/good", key, nil)
	if err != nil {
		t.Fatal(err)
	}
	if m.Tag != "sha-test" || m.OSImage().Ref != "ghcr.io/ihavespoons/sibyl-os:sha-test-amd" {
		t.Fatalf("manifest %+v", m)
	}

	// A changed manifest fails the signature.
	dir := copyDir(t, "testdata/good")
	b, _ := os.ReadFile(filepath.Join(dir, "manifest.json"))
	b = []byte(strings.Replace(string(b), "sha-test-amd", "sha-evil-amd", 1))
	if err := os.WriteFile(filepath.Join(dir, "manifest.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, key, nil); err == nil || !strings.Contains(err.Error(), "does not match the project key") {
		t.Fatalf("changed manifest: %v", err)
	}

	// A changed archive fails its SHA-256.
	dir = copyDir(t, "testdata/good")
	if err := os.WriteFile(filepath.Join(dir, "tui.oci.tar"), []byte("evil\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := Verify(dir, key, nil); err == nil || !strings.Contains(err.Error(), "tui.oci.tar does not match") {
		t.Fatalf("changed archive: %v", err)
	}
}

func TestValidate(t *testing.T) {
	good := Image{Ref: "ghcr.io/ihavespoons/sibyl-os:sha-1-amd", File: "os.oci.tar", SHA256: strings.Repeat("a", 64), Store: OS}
	for name, m := range map[string]Manifest{
		"no os":      {Version: 1, Images: []Image{{Ref: good.Ref, File: "x.oci.tar", SHA256: good.SHA256, Store: Sibyl}}},
		"path":       {Version: 1, Images: []Image{{Ref: good.Ref, File: "../etc/passwd.oci.tar", SHA256: good.SHA256, Store: OS}}},
		"bad ref":    {Version: 1, Images: []Image{{Ref: "evil;rm", File: good.File, SHA256: good.SHA256, Store: OS}}},
		"store":      {Version: 1, Images: []Image{{Ref: good.Ref, File: good.File, SHA256: good.SHA256, Store: "root"}}},
		"version":    {Version: 2, Images: []Image{good}},
		"two os":     {Version: 1, Images: []Image{good, {Ref: good.Ref, File: "b.oci.tar", SHA256: good.SHA256, Store: OS}}},
		"duplicates": {Version: 1, Images: []Image{good, good}},
	} {
		if err := m.Validate(); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestRepoName(t *testing.T) {
	for in, want := range map[string]string{
		"ghcr.io/ihavespoons/sibyl-tui:sha-1":                            "ghcr.io/ihavespoons/sibyl-tui",
		"ghcr.io/getarcaneapp/manager@sha256:" + strings.Repeat("a", 64): "ghcr.io/getarcaneapp/manager",
		"localhost:5000/x/y:1":                                           "localhost:5000/x/y",
	} {
		if got := RepoName(in); got != want {
			t.Errorf("RepoName(%q) = %q, want %q", in, got, want)
		}
	}
}
