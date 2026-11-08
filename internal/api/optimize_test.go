package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/models"
	"github.com/diffsec/sibyl/internal/optimize"
)

// fakeAgent answers tune requests until the test ends: Vulkan generates
// 10 tok/s faster than HIP, and llama-server always loads. hang makes it
// never answer.
func fakeAgent(t *testing.T, f *fixture, hang bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		reqs, tune := filepath.Join(f.run, "requests"), filepath.Join(f.run, "tune")
		for ctx.Err() == nil {
			time.Sleep(5 * time.Millisecond)
			entries, _ := os.ReadDir(reqs)
			for _, e := range entries {
				_, id, ok := strings.Cut(e.Name()[strings.Index(e.Name(), ".")+1:], "tune.")
				if !ok || hang {
					continue
				}
				_ = os.Remove(filepath.Join(reqs, e.Name()))
				read := func(ext string) string {
					b, _ := os.ReadFile(filepath.Join(tune, id+ext))
					return strings.TrimSpace(string(b))
				}
				var out strings.Builder
				switch read(".tool") {
				case "bench":
					v := values(strings.Split(read(".args"), "\n"))
					gen := 40.0
					if read(".backend") == "vulkan" {
						gen = 50
					}
					for _, ub := range v["-ub"] {
						for _, ctk := range v["-ctk"] {
							for _, ctv := range v["-ctv"] {
								for _, b := range v["-b"] {
									key := fmt.Sprintf(`"n_batch":%s,"n_ubatch":%s,"type_k":"%s","type_v":"%s","n_cpu_moe":%s,"split_mode":"%s"`,
										b, ub, ctk, ctv, v["-ncmoe"][0], v["-sm"][0])
									fmt.Fprintf(&out, "{%s,\"n_prompt\":512,\"n_gen\":0,\"avg_ts\":1000,\"stddev_ts\":1}\n", key)
									fmt.Fprintf(&out, "{%s,\"n_prompt\":0,\"n_gen\":128,\"avg_ts\":%v,\"stddev_ts\":0.1}\n", key, gen)
								}
							}
						}
					}
				case "server":
					out.WriteString(`{"loaded":true,"seconds":1}`)
				}
				_ = os.WriteFile(filepath.Join(tune, id+".out"), []byte(out.String()), 0o640)
				_ = os.WriteFile(filepath.Join(tune, id+".status"), []byte("done now"), 0o640)
			}
		}
	}()
}

func values(args []string) map[string][]string {
	m := map[string][]string{}
	for i := 0; i+1 < len(args); i += 2 {
		m[args[i]] = strings.Split(args[i+1], ",")
	}
	return m
}

func waitOptimize(t *testing.T, f *fixture, id, status string) map[string]any {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		if code, b := f.do(t, "GET", "/models/"+id+"/optimize", "", true); code == 200 && b["status"] == status {
			return b
		}
		time.Sleep(10 * time.Millisecond)
	}
	_, b := f.do(t, "GET", "/models/"+id+"/optimize", "", true)
	t.Fatalf("no %s state: %v", status, b)
	return nil
}

func TestOptimizeRunAndApply(t *testing.T) {
	f := newFixture(t)
	f.server.tunePoll = 5 * time.Millisecond
	f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[1,0],"layout":"span"}`, true)
	f.do(t, "PUT", "/models/big", `{"ctx_size":32768,"args":["--mlock"]}`, true)
	fakeAgent(t, f, false)
	if code, b := f.do(t, "POST", "/models/big/optimize", `{"profile":"single"}`, true); code != http.StatusAccepted || b["status"] != "running" {
		t.Fatalf("start = %d %v", code, b)
	}
	b := waitOptimize(t, f, "big", "done")
	raw, _ := json.Marshal(b["recommendation"])
	var rec struct {
		Backend  string
		Verified bool
		Settings models.Settings
		Changes  []map[string]string
	}
	if err := json.Unmarshal(raw, &rec); err != nil {
		t.Fatal(err)
	}
	if rec.Backend != "vulkan" || !rec.Verified || rec.Settings.CtxSize != 32768 || rec.Settings.Args[0] != "--mlock" {
		t.Fatalf("recommendation %s", raw)
	}
	// The agent got the model and both selected GPUs.
	for ext, want := range map[string]string{".path": "big-00001-of-00002.gguf", ".gpus": "0,1"} {
		if got, _ := os.ReadFile(filepath.Join(f.run, "tune", "big"+ext)); string(got) != want {
			t.Errorf("%s = %q", ext, got)
		}
	}
	if code, b := f.do(t, "POST", "/models/big/optimize/apply", `{"backend":true}`, true); code != 200 {
		t.Fatalf("apply = %d %v", code, b)
	}
	hw, err := f.server.hardware()
	if err != nil || hw.Backend != "vulkan" {
		t.Errorf("hardware %+v %v", hw, err)
	}
	set, _ := models.LoadSettings(f.paths.settingsFile())
	if set["big"].CtxSize != 32768 {
		t.Errorf("settings %+v", set["big"])
	}
	if _, b := f.do(t, "GET", "/models/big/optimize", "", true); b["applied"] == nil {
		t.Errorf("applied is not set: %v", b)
	}
}

func TestOptimizeRefusals(t *testing.T) {
	f := newFixture(t)
	f.server.tunePoll = 5 * time.Millisecond
	f.do(t, "PUT", "/hardware", `{"backend":"hip","gpus":[1,0],"layout":"span"}`, true)
	for body, want := range map[string]int{`{"profile":"fast"}`: 422, `{"profile":"single","x":1}`: 400} {
		if code, _ := f.do(t, "POST", "/models/big/optimize", body, true); code != want {
			t.Errorf("%s = %d, want %d", body, code, want)
		}
	}
	if code, _ := f.do(t, "POST", "/models/missing/optimize", `{}`, true); code != http.StatusNotFound {
		t.Errorf("missing = %d", code)
	}
	if code, _ := f.do(t, "GET", "/models/big/optimize", "", true); code != http.StatusNotFound {
		t.Errorf("no run = %d", code)
	}
	if code, _ := f.do(t, "POST", "/models/big/optimize/apply", `{}`, true); code != http.StatusNotFound {
		t.Errorf("apply with no run = %d", code)
	}
	// A run that hangs: a second start conflicts, and cancel ends it.
	fakeAgent(t, f, true)
	if code, _ := f.do(t, "POST", "/models/big/optimize", `{}`, true); code != http.StatusAccepted {
		t.Fatalf("start = %d", code)
	}
	if code, _ := f.do(t, "POST", "/models/chat-14b/optimize", `{}`, true); code != http.StatusConflict {
		t.Errorf("second run = %d", code)
	}
	if code, _ := f.do(t, "DELETE", "/models/chat-14b/optimize", "", true); code != http.StatusNotFound {
		t.Errorf("cancel of another model = %d", code)
	}
	if code, _ := f.do(t, "DELETE", "/models/big/optimize", "", true); code != http.StatusAccepted {
		t.Errorf("cancel = %d", code)
	}
	waitOptimize(t, f, "big", "cancelled")
	// An unverified recommendation cannot be applied.
	st, _ := f.server.readOptimize("big")
	st.Status = "done"
	st.Recommendation = &optimizeRecommendationUnverified
	if err := f.server.writeOptimize("big", st); err != nil {
		t.Fatal(err)
	}
	if code, _ := f.do(t, "POST", "/models/big/optimize/apply", `{}`, true); code != http.StatusConflict {
		t.Errorf("unverified apply = %d", code)
	}
}

var optimizeRecommendationUnverified = optimize.Recommendation{}
