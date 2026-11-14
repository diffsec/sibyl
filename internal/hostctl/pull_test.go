package hostctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// fakePullPodman is a podman that has the refs in $FAKE_HAVE, records each
// pull in $FAKE_LOG, and fails to pull the refs in $FAKE_FAIL.
const fakePullPodman = `#!/bin/bash
case "$1 $2" in
"image exists") grep -qxF "$3" "$FAKE_HAVE" ;;
"pull --quiet")
    ref="${@: -1}"
    if grep -qxF "$ref" "$FAKE_FAIL"; then exit 125; fi
    echo "pull $ref" >>"$FAKE_LOG" ;;
*) exit 1 ;;
esac
`

// fakeSystemctl records a reboot in $FAKE_LOG.
const fakeSystemctl = `#!/bin/bash
echo "systemctl $*" >>"$FAKE_LOG"
`

const stagedSum = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

type pullHost struct {
	staged  bool     // a deployment is staged
	newTag  string   // the staged deployment's image tag
	oldTag  string   // the running image tag
	backend string   // the Image= line of the running backend drop-in
	have    []string // images that the sibyl user has
	fail    []string // images that fail to pull
}

func runPull(t *testing.T, h pullHost, args ...string) (log []string, out string, err error) {
	t.Helper()
	dir := t.TempDir()
	write := func(path, body string) string {
		p := filepath.Join(dir, path)
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
		return p
	}
	units := "usr/etc/containers/systemd/users/1001/"
	root := "ostree/default/deploy/" + stagedSum + ".0/"
	write(root+"usr/share/sibyl/image-tag", h.newTag+"\n")
	write(root+units+"caddy.container", "[Container]\nImage=ghcr.io/diffsec/sibyl-caddy:"+h.newTag+"\n")
	write(root+units+"system-api.container", "[Container]\nImage=ghcr.io/diffsec/sibyl-system-api:"+h.newTag+"\n")
	write(root+units+"open-webui.container", "[Container]\nImage=ghcr.io/open-webui/open-webui@sha256:"+strings.Repeat("a", 64)+"\n")
	write(root+units+"llama-swap.container", "[Container]\nContainerName=llama-swap\n")
	write(root+units+"llama-swap.container.d/backend.conf", "[Container]\nImage=ghcr.io/diffsec/sibyl-llama-swap:"+h.newTag+"-hip\n")
	write("current/usr/share/sibyl/image-tag", h.oldTag+"\n")
	write("current/etc/containers/systemd/users/1001/llama-swap.container.d/backend.conf", "[Container]\n"+h.backend+"\n")
	status := `{"status":{"booted":{},"staged":null}}`
	if h.staged {
		status = `{"status":{"staged":{"ostree":{"checksum":"` + stagedSum + `","deploySerial":0,"stateroot":"default"}}}}`
	}
	write("bin/podman", fakePullPodman)
	write("bin/systemctl", fakeSystemctl)
	logPath := filepath.Join(dir, "log")
	cmd := exec.Command("bash", append([]string{"../../os/rootfs/usr/libexec/sibyl/image-pull"}, args...)...)
	cmd.Env = append(os.Environ(), "PATH="+filepath.Join(dir, "bin")+":"+os.Getenv("PATH"), "SIBYL_PULL_AS_SELF=1",
		"SIBYL_PULL_DEPLOY="+filepath.Join(dir, "ostree"), "SIBYL_PULL_CURRENT="+filepath.Join(dir, "current"),
		"SIBYL_PULL_STATUS="+write("status.json", status),
		"FAKE_HAVE="+write("have", strings.Join(h.have, "\n")+"\n"),
		"FAKE_FAIL="+write("fail", strings.Join(h.fail, "\n")+"\n"), "FAKE_LOG="+logPath)
	b, err := cmd.CombinedOutput()
	raw, _ := os.ReadFile(logPath)
	for l := range strings.SplitSeq(strings.TrimSpace(string(raw)), "\n") {
		if l != "" {
			log = append(log, l)
		}
	}
	return log, string(b), err
}

func TestImagePull(t *testing.T) {
	webui := "ghcr.io/open-webui/open-webui@sha256:" + strings.Repeat("a", 64)
	log, out, err := runPull(t, pullHost{
		staged: true, newTag: "sha-new", oldTag: "sha-old",
		// This host runs the Vulkan backend, which is not the image's default.
		backend: "Image=ghcr.io/diffsec/sibyl-llama-swap:sha-old-vulkan",
		have:    []string{webui},
	})
	if err != nil {
		t.Fatalf("image-pull: %v\n%s", err, out)
	}
	want := []string{
		"pull ghcr.io/diffsec/sibyl-caddy:sha-new",
		"pull ghcr.io/diffsec/sibyl-system-api:sha-new",
		"pull ghcr.io/diffsec/sibyl-llama-swap:sha-new-vulkan",
	}
	if !slices.Equal(log, want) {
		t.Errorf("log %q, want %q\n%s", log, want, out)
	}
	if !strings.Contains(out, "the 4 image(s) of sha-new are ready") {
		t.Errorf("no ready line:\n%s", out)
	}
}

func TestImagePullFallsBackToTheStagedBackend(t *testing.T) {
	// The running drop-in names no tag of the running image, so the staged
	// deployment's own backend image applies.
	log, out, err := runPull(t, pullHost{staged: true, newTag: "sha-new", oldTag: "sha-old",
		backend: "Image=localhost/llama-swap:test"})
	if err != nil {
		t.Fatalf("image-pull: %v\n%s", err, out)
	}
	if !slices.Contains(log, "pull ghcr.io/diffsec/sibyl-llama-swap:sha-new-hip") {
		t.Errorf("log %q has no staged backend image\n%s", log, out)
	}
}

func TestImagePullNothingStaged(t *testing.T) {
	log, out, err := runPull(t, pullHost{newTag: "sha-new", oldTag: "sha-old"}, "--reboot")
	if err != nil || len(log) > 0 || !strings.Contains(out, "no update is staged") {
		t.Errorf("err %v, log %q with nothing staged\n%s", err, log, out)
	}
}

func TestImagePullFailure(t *testing.T) {
	bad := "ghcr.io/diffsec/sibyl-system-api:sha-new"
	log, out, err := runPull(t, pullHost{staged: true, newTag: "sha-new", oldTag: "sha-old",
		backend: "Image=ghcr.io/diffsec/sibyl-llama-swap:sha-old-hip", fail: []string{bad}}, "--reboot")
	if err == nil {
		t.Errorf("a failed pull exits 0\n%s", out)
	}
	if !strings.Contains(out, "could not pull "+bad) || !strings.Contains(out, "1 of 4 image(s) are missing") {
		t.Errorf("the failure is not reported:\n%s", out)
	}
	// The other images still pull, and --reboot still reboots.
	if !slices.Contains(log, "pull ghcr.io/diffsec/sibyl-llama-swap:sha-new-hip") || log[len(log)-1] != "systemctl reboot" {
		t.Errorf("log %q", log)
	}
}

func TestImagePullBadTag(t *testing.T) {
	log, out, err := runPull(t, pullHost{staged: true, newTag: "*", oldTag: "sha-old"})
	if err == nil || len(log) > 0 || !strings.Contains(out, "no valid image tag") {
		t.Errorf("err %v, log %q with a bad tag\n%s", err, log, out)
	}
}
