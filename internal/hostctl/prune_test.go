package hostctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakePrunePodman is a podman that lists $FAKE_IMAGES, records each rmi and
// prune in $FAKE_LOG, and refuses to remove the refs in $FAKE_INUSE.
const fakePrunePodman = `#!/bin/bash
case "$1" in
images) cat "$FAKE_IMAGES" ;;
rmi)
    if grep -qxF "$2" "$FAKE_INUSE"; then exit 2; fi
    echo "rmi $2" >>"$FAKE_LOG" ;;
image) echo "prune" >>"$FAKE_LOG" ;;
*) exit 1 ;;
esac
`

func runPrune(t *testing.T, tags, images, inUse []string, args ...string) (removed []string, pruned bool, out string) {
	t.Helper()
	dir := t.TempDir()
	deploy := filepath.Join(dir, "ostree")
	for i, tag := range tags {
		d := filepath.Join(deploy, "default", "deploy", strings.Repeat("a", i+1)+".0", "usr", "share", "sibyl")
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "image-tag"), []byte(tag+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	bin := filepath.Join(dir, "bin")
	if err := os.MkdirAll(bin, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bin, "podman"), []byte(fakePrunePodman), 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, lines []string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(strings.Join(lines, "\n")+"\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	log := filepath.Join(dir, "log")
	cmd := exec.Command("bash", append([]string{"../../os/rootfs/usr/libexec/sibyl/image-prune"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"), "SIBYL_PRUNE_AS_SELF=1",
		"SIBYL_PRUNE_DEPLOY="+deploy, "FAKE_IMAGES="+write("images", images),
		"FAKE_INUSE="+write("inuse", inUse), "FAKE_LOG="+log)
	b, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("image-prune: %v\n%s", err, b)
	}
	raw, _ := os.ReadFile(log)
	for l := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		switch {
		case strings.HasPrefix(l, "rmi "):
			removed = append(removed, strings.TrimPrefix(l, "rmi "))
		case l == "prune":
			pruned = true
		}
	}
	return removed, pruned, string(b)
}

func TestImagePrune(t *testing.T) {
	images := []string{
		"ghcr.io/diffsec/sibyl-caddy:sha-new",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-new-hip",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-new-vulkan",
		"ghcr.io/diffsec/sibyl-caddy:sha-old",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-old-hip",
		"ghcr.io/diffsec/sibyl-caddy:sha-dead",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-dead-hip",
		"ghcr.io/diffsec/sibyl-router:sha-dead",
		"ghcr.io/diffsec/sibyl-caddy:sha-newer",
		"ghcr.io/diffsec/sibyl-caddy:<none>",
		"docker.io/library/busybox:1.37",
		"ghcr.io/other/sibyl-caddy:sha-dead",
		"localhost/sibyl-test:sha-dead",
	}
	removed, pruned, out := runPrune(t, []string{"sha-new", "sha-old"}, images, []string{"ghcr.io/diffsec/sibyl-router:sha-dead"})
	want := []string{
		"ghcr.io/diffsec/sibyl-caddy:sha-dead",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-dead-hip",
		"ghcr.io/diffsec/sibyl-caddy:sha-newer",
	}
	if !slices.Equal(removed, want) {
		t.Errorf("removed %q, want %q\n%s", removed, want, out)
	}
	if !pruned {
		t.Errorf("no image prune after the removals\n%s", out)
	}
	if !strings.Contains(out, "kept ghcr.io/diffsec/sibyl-router:sha-dead: a container uses it") {
		t.Errorf("the image in use is not reported:\n%s", out)
	}
}

func TestImagePruneNoDeployments(t *testing.T) {
	removed, pruned, out := runPrune(t, nil, []string{"ghcr.io/diffsec/sibyl-caddy:sha-dead"}, nil)
	if len(removed) > 0 || pruned {
		t.Errorf("removed %q, pruned %v with no deployment tag\n%s", removed, pruned, out)
	}
}

func TestImagePruneBadTag(t *testing.T) {
	// A tag file with a glob must not keep every image or match by pattern.
	removed, _, out := runPrune(t, []string{"*"}, []string{"ghcr.io/diffsec/sibyl-caddy:sha-dead"}, nil)
	if len(removed) > 0 || !strings.Contains(out, "removing nothing") {
		t.Errorf("removed %q with only a bad tag\n%s", removed, out)
	}
}

func TestImagePruneDryRun(t *testing.T) {
	removed, pruned, out := runPrune(t, []string{"sha-new"},
		[]string{"ghcr.io/diffsec/sibyl-caddy:sha-new", "ghcr.io/diffsec/sibyl-caddy:sha-dead"}, nil, "--dry-run")
	if len(removed) > 0 || pruned {
		t.Errorf("the dry run removed %q, pruned %v", removed, pruned)
	}
	if !strings.Contains(out, "would remove ghcr.io/diffsec/sibyl-caddy:sha-dead") || strings.Contains(out, "sibyl-caddy:sha-new\n") {
		t.Errorf("the dry run does not list the right image:\n%s", out)
	}
}
