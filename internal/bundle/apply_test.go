package bundle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestApply(t *testing.T) {
	dir := copyDir(t, "testdata/good")
	if err := os.WriteFile(filepath.Join(dir, "arcane.oci.tar"), []byte("a"), 0o644); err != nil {
		t.Fatal(err)
	}
	m := Manifest{Version: 1, Tag: "sha-test", Variant: "amd", Images: []Image{
		{Ref: "ghcr.io/diffsec/sibyl-os:sha-test-amd", File: "os.oci.tar", Store: OS},
		{Ref: "ghcr.io/getarcaneapp/manager@sha256:" + strings.Repeat("a", 64), File: "arcane.oci.tar", Store: Sibyl},
		{Ref: "ghcr.io/diffsec/sibyl-tui:sha-test", File: "tui.oci.tar", Store: TUI},
	}}
	stage := filepath.Join(t.TempDir(), "stage")
	var calls []string
	a := &Applier{Stage: stage, SibylUID: 1001, SibylGID: os.Getgid(),
		Run: func(_ context.Context, c Cmd) error {
			calls = append(calls, strings.Join(append([]string{c.Name}, c.Args...), " "))
			if c.Name == "runuser" {
				// The staged copy must exist while skopeo reads it.
				if _, err := os.Stat(filepath.Join(stage, "arcane.oci.tar")); err != nil {
					t.Errorf("archive not staged: %v", err)
				}
			}
			return nil
		}}
	if err := a.Apply(context.Background(), dir, m); err != nil {
		t.Fatal(err)
	}
	want := []string{
		"runuser -u sibyl -- env XDG_RUNTIME_DIR=/run/user/1001 HOME=/var/lib/sibyl/home skopeo copy --quiet --preserve-digests oci-archive:" +
			stage + "/arcane.oci.tar containers-storage:ghcr.io/getarcaneapp/manager",
		"bootc switch --enforce-container-sigpolicy --transport oci-archive " + filepath.Join(dir, "os.oci.tar"),
	}
	if strings.Join(calls, "\n") != strings.Join(want, "\n") {
		t.Fatalf("calls:\n%s", strings.Join(calls, "\n"))
	}
	if _, err := os.Stat(filepath.Join(stage, "arcane.oci.tar")); !os.IsNotExist(err) {
		t.Error("staged archive not removed")
	}
	if b, _ := os.ReadFile(filepath.Join(stage, "sibyl-tui.ref")); string(b) != "ghcr.io/diffsec/sibyl-tui:sha-test\n" {
		t.Errorf("tui ref %q", b)
	}
	if _, err := os.Stat(filepath.Join(stage, "sibyl-tui.oci.tar")); err != nil {
		t.Error("tui archive not kept")
	}
}
