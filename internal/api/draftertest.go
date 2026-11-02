package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/diffsec/sibyl/internal/models"
)

// drafterPrompts are the requests that a drafter test sends, once without
// and once with the drafter. A drafter's gain depends on the text, so the
// test uses code and prose.
var drafterPrompts = []string{
	"Write a Python module that implements an LRU cache class with get, put, delete and resize methods, " +
		"with type hints, docstrings and pytest unit tests.",
	"Write a detailed essay about the history of the printing press.",
}

// drafterTestTokens is the length of each answer. Short answers give a
// noisy speed, and long ones keep the model busy for minutes.
const drafterTestTokens = 256

// drafterTestTimeout bounds one request, which can include a model load.
const drafterTestTimeout = 10 * time.Minute

// SpeedSample is the generation speed of one side of a drafter test.
type SpeedSample struct {
	TokensPerSecond float64 `json:"tokens_per_second"`
	Tokens          int     `json:"tokens"`
	// Drafted and Accepted count the drafted tokens and the ones the
	// model kept. They are 0 without a drafter.
	Drafted  int `json:"drafted"`
	Accepted int `json:"accepted"`
}

// DrafterTest is the state of the last drafter test of one model.
type DrafterTest struct {
	Status      string             `json:"status"` // running, done or failed
	Updated     time.Time          `json:"updated"`
	Speculative models.Speculative `json:"speculative"`
	Without     *SpeedSample       `json:"without,omitempty"`
	With        *SpeedSample       `json:"with,omitempty"`
	// Speedup is With over Without: above 1 the drafter helps.
	Speedup float64 `json:"speedup,omitempty"`
	Error   string  `json:"error,omitempty"`
}

type drafterTests struct {
	mu    sync.Mutex
	state map[string]DrafterTest
	busy  bool
}

func (d *drafterTests) set(id string, t DrafterTest) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.state == nil {
		d.state = map[string]DrafterTest{}
	}
	t.Updated = time.Now().UTC()
	d.state[id] = t
}

// startDrafterTest measures a model's speed with its current settings, then
// with the drafter in the body, and puts the model's speculative settings
// back. The owner turns the drafter on with PUT /models/{id} after reading
// the result. Each settings change reloads llama-swap, which unloads the
// running models. One test runs at a time.
func (s *Server) startDrafterTest(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var spec models.Speculative
	if err := readJSON(r, &spec); err != nil {
		writeError(w, http.StatusBadRequest, err)
		return
	}
	if s.LlamaSwapURL == "" {
		writeError(w, http.StatusServiceUnavailable, errString("no llama-swap address"))
		return
	}
	s.mu.Lock()
	installed, err := s.scanModels()
	if err != nil {
		s.mu.Unlock()
		writeError(w, http.StatusInternalServerError, err)
		return
	}
	i := slices.IndexFunc(installed, func(m models.Model) bool { return m.ID == id })
	d := slices.IndexFunc(installed, func(m models.Model) bool { return m.ID == spec.DraftModel })
	hw, herr := s.hardware()
	trial := models.Settings{Speculative: spec}
	switch {
	case i < 0:
		s.mu.Unlock()
		writeError(w, http.StatusNotFound, errString("model is not installed"))
		return
	case installed[i].Drafter:
		err = errors.New("the model is a drafter")
	case d < 0 || spec.DraftModel == id:
		err = errors.New("draft_model: give another installed model")
	case len(spec.Types) == 0:
		err = errors.New("types: give the speculative type, such as draft-dflash")
	case herr != nil:
		err = herr
	default:
		err = trial.Validate(len(hw.GPUs))
	}
	s.mu.Unlock()
	if err != nil {
		writeError(w, http.StatusUnprocessableEntity, err)
		return
	}

	s.drafterTests.mu.Lock()
	if s.drafterTests.busy {
		s.drafterTests.mu.Unlock()
		writeError(w, http.StatusConflict, errString("a drafter test is running"))
		return
	}
	s.drafterTests.busy = true
	s.drafterTests.mu.Unlock()
	s.drafterTests.set(id, DrafterTest{Status: "running", Speculative: spec})
	go func() {
		defer func() {
			s.drafterTests.mu.Lock()
			s.drafterTests.busy = false
			s.drafterTests.mu.Unlock()
		}()
		t := s.runDrafterTest(id, spec)
		s.drafterTests.set(id, t)
	}()
	writeJSON(w, http.StatusAccepted, map[string]string{"status": "running"})
}

func (s *Server) getDrafterTest(w http.ResponseWriter, r *http.Request) {
	s.drafterTests.mu.Lock()
	t, ok := s.drafterTests.state[r.PathValue("id")]
	s.drafterTests.mu.Unlock()
	if !ok {
		writeError(w, http.StatusNotFound, errString("no drafter test ran for this model"))
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// runDrafterTest runs both sides of a test and restores the model's
// speculative settings, whatever happens.
func (s *Server) runDrafterTest(id string, spec models.Speculative) DrafterTest {
	t := DrafterTest{Status: "failed", Speculative: spec}
	fail := func(err error) DrafterTest {
		t.Error = err.Error()
		return t
	}
	without, err := s.measure(id)
	if err != nil {
		return fail(fmt.Errorf("without the drafter: %w", err))
	}
	t.Without = &without

	orig, err := s.swapSpeculative(id, spec)
	if err != nil {
		return fail(fmt.Errorf("apply the drafter: %w", err))
	}
	with, merr := s.measure(id)
	if _, err := s.swapSpeculative(id, orig); err != nil {
		s.Log.Error("restore the speculative settings after a drafter test", "model", id, "err", err)
	}
	if merr != nil {
		return fail(fmt.Errorf("with the drafter: %w", merr))
	}
	t.With = &with
	if with.Drafted == 0 {
		return fail(errors.New("with the drafter: llama-server drafted no tokens"))
	}
	if without.TokensPerSecond > 0 {
		t.Speedup = with.TokensPerSecond / without.TokensPerSecond
	}
	t.Status = "done"
	return t
}

// swapSpeculative sets one model's speculative settings, applies them, and
// returns the old ones. Only that field changes, so an owner's edit to the
// model during a test stays.
func (s *Server) swapSpeculative(id string, spec models.Speculative) (models.Speculative, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	settings, err := models.LoadSettings(s.Paths.settingsFile())
	if err != nil {
		return models.Speculative{}, err
	}
	st := settings[id]
	old := st.Speculative
	st.Speculative = spec
	settings[id] = st
	if err := models.SaveSettings(s.Paths.settingsFile(), settings); err != nil {
		return old, err
	}
	_, err = s.Apply()
	return old, err
}

// measure unloads llama-swap's models, so the next request starts the
// model with the current configuration, loads the model with a short
// request, and sends the test prompts.
func (s *Server) measure(id string) (SpeedSample, error) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(len(drafterPrompts)+1)*drafterTestTimeout)
	defer cancel()
	// llama-swap reloads its configuration when the file changes. A
	// moment's wait lets it see the file that Apply wrote.
	wait := s.reloadWait
	if wait == 0 {
		wait = 3 * time.Second
	}
	time.Sleep(wait)
	if err := s.llamaSwapGet(ctx, "/unload"); err != nil {
		return SpeedSample{}, err
	}
	if _, err := s.complete(ctx, id, "Say hello.", 8); err != nil {
		return SpeedSample{}, fmt.Errorf("load the model: %w", err)
	}
	var out SpeedSample
	var ms float64
	for _, p := range drafterPrompts {
		tm, err := s.complete(ctx, id, p, drafterTestTokens)
		if err != nil {
			return out, err
		}
		out.Tokens += tm.PredictedN
		ms += tm.PredictedMS
		out.Drafted += tm.DraftN
		out.Accepted += tm.DraftAccepted
	}
	if ms > 0 {
		out.TokensPerSecond = float64(out.Tokens) / ms * 1000
	}
	return out, nil
}

// timings are the llama-server fields of a chat completion that a drafter
// test reads.
type timings struct {
	PredictedN    int     `json:"predicted_n"`
	PredictedMS   float64 `json:"predicted_ms"`
	DraftN        int     `json:"draft_n"`
	DraftAccepted int     `json:"draft_n_accepted"`
}

func (s *Server) complete(ctx context.Context, id, prompt string, maxTokens int) (timings, error) {
	body, _ := json.Marshal(map[string]any{
		"model": id, "max_tokens": maxTokens, "temperature": 0,
		"messages": []map[string]string{{"role": "user", "content": prompt}},
	})
	rctx, cancel := context.WithTimeout(ctx, drafterTestTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(rctx, http.MethodPost, s.LlamaSwapURL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return timings{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return timings{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode != http.StatusOK {
		return timings{}, fmt.Errorf("llama-swap: %s: %s", resp.Status, bytes.TrimSpace(raw[:min(len(raw), 300)]))
	}
	var out struct {
		Timings timings `json:"timings"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return timings{}, err
	}
	return out.Timings, nil
}

func (s *Server) llamaSwapGet(ctx context.Context, path string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.LlamaSwapURL+path, nil)
	if err != nil {
		return err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("llama-swap %s: %s", path, resp.Status)
	}
	return nil
}
