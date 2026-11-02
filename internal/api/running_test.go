package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
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
