package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/diffsec/sibyl/internal/gguf"
)

// TestRunningLoadUnload checks that GET /running reports llama-swap's
// models, that a load goes through llama-swap's upstream route in the
// background, and that an unload names the model.
func TestRunningLoadUnload(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var calls []string
	running := []map[string]string{{"model": "small", "state": "ready"}}
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.Method+" "+r.URL.Path)
		switch r.URL.Path {
		case "/running":
			_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
		case "/upstream/big/health":
			running = append(running, map[string]string{"model": "big", "state": "ready"})
		case "/upstream/broken/health":
			w.WriteHeader(http.StatusBadGateway)
		case "/api/models/unload/small":
			running = running[1:]
		}
	}))
	t.Cleanup(swap.Close)
	f.server.LlamaSwapURL = swap.URL
	header := func(arch string) string {
		data, err := gguf.Encode(map[string]any{"general.architecture": arch, arch + ".block_count": 2, arch + ".context_length": 4096})
		if err != nil {
			t.Fatal(err)
		}
		return string(data)
	}
	for _, id := range []string{"big", "small", "broken"} {
		write(t, filepath.Join(f.paths.Models, id+".gguf"), header("llama"))
	}
	write(t, filepath.Join(f.paths.Models, "drafter.gguf"), header("dflash"))

	running1 := func() Running {
		code, body := f.doRaw(t, "GET", "/running")
		if code != http.StatusOK {
			t.Fatalf("running = %d %s", code, body)
		}
		var r Running
		_ = json.Unmarshal(body, &r)
		return r
	}
	if r := running1(); len(r.Models) != 1 || r.Models[0].ID != "small" || r.Models[0].State != "ready" {
		t.Fatalf("running = %+v", r)
	}
	for _, id := range []string{"big", "broken"} {
		if code, b := f.do(t, "POST", "/models/"+id+"/load", "", true); code != http.StatusAccepted {
			t.Fatalf("load %s = %d %v", id, code, b)
		}
	}
	var r Running
	for range 200 {
		if r = running1(); len(r.Loading) == 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(r.Models) != 2 || r.Models[0].ID != "big" || r.Errors["broken"] == "" || r.Errors["big"] != "" {
		t.Errorf("after the loads: %+v", r)
	}
	if code, b := f.do(t, "POST", "/models/small/unload", "", true); code != http.StatusOK {
		t.Fatalf("unload = %d %v", code, b)
	}
	if r := running1(); len(r.Models) != 1 || r.Models[0].ID != "big" {
		t.Errorf("after the unload: %+v", r)
	}
	for path, want := range map[string]int{
		"/models/drafter/load": http.StatusUnprocessableEntity, "/models/gone/load": http.StatusNotFound,
		"/models/drafter/unload": http.StatusUnprocessableEntity,
	} {
		if code, _ := f.do(t, "POST", path, "", true); code != want {
			t.Errorf("%s = %d, want %d", path, code, want)
		}
	}
}

// An Edge0 model keeps its expert file in system RAM. A load is refused
// when the host does not have that RAM free, unless the model already runs.
func TestLoadEdge0NeedsRAM(t *testing.T) {
	f := newFixture(t)
	var mu sync.Mutex
	var running []map[string]string
	loads := 0
	swap := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		switch r.URL.Path {
		case "/running":
			_ = json.NewEncoder(w).Encode(map[string]any{"running": running})
		case "/upstream/edge0/health":
			loads++
		}
	}))
	t.Cleanup(swap.Close)
	f.server.LlamaSwapURL = swap.URL
	f.server.Paths.MemInfo = filepath.Join(t.TempDir(), "meminfo")
	folder := filepath.Join(f.paths.Models, "edge0")
	write(t, filepath.Join(folder, "model.json"), `{"family":"Edge0","context":262144}`)
	write(t, filepath.Join(folder, "experts.bin"), strings.Repeat("x", 1<<20)) // 1 MiB, so 6 GiB + 1 MiB of RAM
	meminfo := func(availGiB int) {
		write(t, f.server.Paths.MemInfo, fmt.Sprintf("MemTotal: %d kB\nMemAvailable: %d kB\n", 64<<20, availGiB<<20))
	}
	wait := func() {
		for range 200 {
			code, body := f.doRaw(t, "GET", "/running")
			var r Running
			_ = json.Unmarshal(body, &r)
			if code == http.StatusOK && len(r.Loading) == 0 {
				return
			}
			time.Sleep(10 * time.Millisecond)
		}
		t.Fatal("the load did not finish")
	}

	meminfo(4)
	code, b := f.do(t, "POST", "/models/edge0/load", "", true)
	if code != http.StatusUnprocessableEntity || !strings.Contains(fmt.Sprint(b["error"]), "4.0 GiB is available") {
		t.Fatalf("load with 4 GiB free = %d %v, want 422 that names the RAM", code, b)
	}
	// A GGUF model has no RAM need.
	if code, b := f.do(t, "POST", "/models/chat-14b/load", "", true); code != http.StatusAccepted {
		t.Errorf("GGUF load with 4 GiB free = %d %v", code, b)
	}
	wait()
	// The running model holds its RAM already.
	mu.Lock()
	running = []map[string]string{{"model": "edge0", "state": "ready"}}
	mu.Unlock()
	if code, b := f.do(t, "POST", "/models/edge0/load", "", true); code != http.StatusAccepted {
		t.Errorf("load of the running model = %d %v", code, b)
	}
	wait()
	mu.Lock()
	running = nil
	mu.Unlock()
	meminfo(8)
	if code, b := f.do(t, "POST", "/models/edge0/load", "", true); code != http.StatusAccepted {
		t.Errorf("load with 8 GiB free = %d %v", code, b)
	}
	wait()
	mu.Lock()
	defer mu.Unlock()
	if loads != 2 {
		t.Errorf("llama-swap got %d loads, want 2", loads)
	}
}
