package hostctl

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// checkTuneArgs runs the host agent's allowlist on one argument list.
func checkTuneArgs(t *testing.T, tool string, args ...string) error {
	t.Helper()
	f := filepath.Join(t.TempDir(), "args")
	body := strings.Join(args, "\n")
	if len(args) > 0 {
		body += "\n"
	}
	if err := os.WriteFile(f, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return exec.Command("bash", "../../os/rootfs/usr/libexec/sibyl/agent", "--check-tune-args", tool, f).Run()
}

func TestAgentTuneAllowlist(t *testing.T) {
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skip("no bash")
	}
	good := []struct {
		tool string
		args []string
	}{
		{"bench", []string{"-p", "512", "-n", "128", "-d", "8192", "-r", "2"}},
		{"bench", []string{"-b", "2048,4096", "-ub", "256,512,1024,2048"}},
		{"bench", []string{"-ctk", "f16,q8_0", "-ctv", "f16,q8_0", "-sm", "layer,row", "-ts", "3/2,1/1"}},
		{"bench", []string{"-ncmoe", "0,8,16", "-ot", `blk\.(1[0-9])\.ffn_.*_exps\.=CPU;blk\.2.\.ffn_up_exps\.=CPU`}},
		{"batched", []string{"-c", "32768", "-npp", "512", "-ntg", "128", "-npl", "1,2,4,8", "-ctk", "q8_0"}},
		{"server", []string{"-c", "65536", "-b", "4096", "-ub", "1024", "-np", "4", "-sm", "row", "-ts", "3/2"}},
		{"server", nil},
	}
	for _, c := range good {
		if err := checkTuneArgs(t, c.tool, c.args...); err != nil {
			t.Errorf("%s %v refused: %v", c.tool, c.args, err)
		}
	}
	bad := []struct {
		tool string
		args []string
	}{
		{"bench", []string{"-m", "/etc/shadow"}}, // the agent sets the model
		{"bench", []string{"--model", "x.gguf"}},
		{"bench", []string{"-p"}}, // a flag with no value
		{"bench", []string{"-p", "512", "-n"}},
		{"bench", []string{"-p", "-n"}}, // a flag as a value
		{"bench", []string{"-p", "512;rm -rf /"}},
		{"bench", []string{"-p", "1234567"}}, // too large
		{"bench", []string{"-ctk", "f32"}},
		{"bench", []string{"-sm", "tensor"}},
		{"bench", []string{"-ot", "-rpc=CPU"}},    // a pattern that starts with a dash
		{"bench", []string{"-ot", "blk.*=ROCm0"}}, // only system RAM
		{"bench", []string{"-rpc", "10.0.0.1:50052"}},
		{"bench", []string{"-o", "md"}},
		{"bench", []string{"--hf-repo", "evil/model"}},
		{"batched", []string{"-npl", "1,2", "-c", "1,2"}}, // one value for -c
		{"batched", []string{"--host", "0.0.0.0"}},
		{"server", []string{"--host", "0.0.0.0"}},
		{"server", []string{"--port", "80"}},
		{"server", []string{"-np", "4,8"}},
		{"server", []string{"--api-key", "x"}},
		{"server", []string{"-c", "4096\n--host"}},
		{"other", []string{"-p", "512"}},
	}
	for _, c := range bad {
		if err := checkTuneArgs(t, c.tool, c.args...); err == nil {
			t.Errorf("%s %q passed", c.tool, c.args)
		}
	}
}

func TestTuneRequest(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "requests"), 0o750); err != nil {
		t.Fatal(err)
	}
	c := New(dir)
	if err := c.Request(Tune, "qwen3.8-27b-q8_0"); err != nil {
		t.Fatal(err)
	}
	if err := c.Request(Tune, "../x"); err == nil {
		t.Error("a bad model ID passed")
	}
	names, _ := os.ReadDir(filepath.Join(dir, "requests"))
	if len(names) != 1 || !strings.HasSuffix(names[0].Name(), ".tune.qwen3.8-27b-q8_0") {
		t.Errorf("requests %v", names)
	}
}

// fakeHost lays out the agent's directories, with a podman and a systemctl
// that log their arguments.
type fakeHost struct {
	run, models, share, bin, log string
}

func newFakeHost(t *testing.T) *fakeHost {
	t.Helper()
	root := t.TempDir()
	h := &fakeHost{run: filepath.Join(root, "run"), models: filepath.Join(root, "models"), share: filepath.Join(root, "share"),
		bin: filepath.Join(root, "bin"), log: filepath.Join(root, "podman.log")}
	for _, d := range []string{filepath.Join(h.run, "requests"), filepath.Join(h.run, "tune"), h.models, h.share, h.bin} {
		if err := os.MkdirAll(d, 0o750); err != nil {
			t.Fatal(err)
		}
	}
	files := map[string]string{
		filepath.Join(h.models, "m.gguf"):     "GGUF",
		filepath.Join(h.share, "image-tag"):   "sha-test",
		filepath.Join(h.share, "gpu-variant"): "amd",
		filepath.Join(h.bin, "systemctl"):     "#!/bin/sh\necho active\n",
		// podman logs each call. llama-bench prints one JSONL row, and a
		// started llama-server answers its health check.
		filepath.Join(h.bin, "podman"): "#!/bin/sh\necho \"$*\" >> \"" + h.log + "\"\n" +
			"case \"$*\" in\n" +
			"  *llama-bench*) echo '{\"n_prompt\":512,\"avg_ts\":1000.5}' ;;\n" +
			"  *llama-batched-bench*) echo '{\"pl\":4,\"speed\":80.5}' ;;\n" +
			"  run\\ -d*) echo ctr ;;\n" +
			"  inspect*) echo true ;;\n" +
			"esac\n",
	}
	for p, body := range files {
		if err := os.WriteFile(p, []byte(body), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	return h
}

// tune writes a tune request and runs the agent once.
func (h *fakeHost) tune(t *testing.T, tool string, args ...string) (status, out, calls string) {
	t.Helper()
	files := map[string]string{"path": "m.gguf", "gpus": "0", "pool": "", "backend": "hip", "tool": tool,
		"args": strings.Join(args, "\n") + "\n"}
	for ext, body := range files {
		if err := os.WriteFile(filepath.Join(h.run, "tune", "m."+ext), []byte(body), 0o640); err != nil {
			t.Fatal(err)
		}
	}
	_ = os.Remove(h.log)
	if err := os.WriteFile(filepath.Join(h.run, "requests", "1.tune.m"), nil, 0o640); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("bash", "../../os/rootfs/usr/libexec/sibyl/agent")
	cmd.Env = append(os.Environ(), "PATH="+h.bin+":"+os.Getenv("PATH"), "SIBYL_AGENT_RUN="+h.run,
		"SIBYL_AGENT_MODELS="+h.models, "SIBYL_AGENT_POOLS="+filepath.Join(h.run, "nopools"), "SIBYL_AGENT_SHARE="+h.share)
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("agent: %v\n%s", err, b)
	}
	read := func(p string) string { b, _ := os.ReadFile(p); return string(b) }
	return read(filepath.Join(h.run, "tune", "m.status")), read(filepath.Join(h.run, "tune", "m.out")), read(h.log)
}

func TestAgentTuneRuns(t *testing.T) {
	if _, err := exec.LookPath("realpath"); err != nil || exec.Command("realpath", "-e", "/").Run() != nil {
		t.Skip("no GNU realpath")
	}
	h := newFakeHost(t)
	status, out, calls := h.tune(t, "bench", "-p", "512", "-n", "128", "-d", "8192", "-ub", "512,1024")
	if !strings.HasPrefix(status, "done ") || !strings.Contains(out, `"avg_ts":1000.5`) {
		t.Errorf("bench: status %q out %q", status, out)
	}
	for _, want := range []string{"--network none", "--cap-drop all", "--security-opt no-new-privileges",
		"ghcr.io/diffsec/sibyl-llama-swap:sha-test-hip", "-m /models/m.gguf -ngl 999 -fa on -o jsonl -p 512 -n 128 -d 8192 -ub 512,1024",
		"HIP_VISIBLE_DEVICES=0", "--device /dev/kfd"} {
		if !strings.Contains(calls, want) {
			t.Errorf("bench: no %q in\n%s", want, calls)
		}
	}

	status, out, calls = h.tune(t, "batched", "-c", "32768", "-npl", "1,2,4")
	if !strings.HasPrefix(status, "done ") || !strings.Contains(out, `"speed":80.5`) ||
		!strings.Contains(calls, "--output-format jsonl -c 32768 -npl 1,2,4") {
		t.Errorf("batched: status %q out %q calls %s", status, out, calls)
	}

	status, out, calls = h.tune(t, "server", "-c", "65536", "-ub", "1024")
	if !strings.HasPrefix(status, "done ") || !strings.Contains(out, `"loaded":true`) ||
		!strings.Contains(calls, "--host 127.0.0.1 --port 8080 -c 65536 -ub 1024") || !strings.Contains(calls, "rm -f sibyl-tune-m") {
		t.Errorf("server: status %q out %q calls %s", status, out, calls)
	}

	status, _, calls = h.tune(t, "bench", "-m", "/etc/shadow")
	if !strings.HasPrefix(status, "failed: the arguments are not allowed") || strings.Contains(calls, "run ") {
		t.Errorf("bad args: status %q calls %s", status, calls)
	}
}
