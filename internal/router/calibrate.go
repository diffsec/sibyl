package router

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// CalibrationConfig sets up continuous calibration: the router keeps a
// sample of real requests, gets the answer of the other side (the local
// model or a provider) when the host is idle, and asks a judge which answer
// is better. docs/design/router.md describes the loop.
type CalibrationConfig struct {
	// SampleRate is the share of a calibrating client's requests that the
	// router keeps. The default is 0.02.
	SampleRate *float64 `yaml:"sample_rate,omitempty" json:"sample_rate,omitempty"`
	// DailyUSD limits the provider spend of the comparisons and the judge
	// per UTC day. The default is 1. Zero stops all provider calls.
	DailyUSD *float64 `yaml:"daily_usd,omitempty" json:"daily_usd,omitempty"`
	// Judge is the route of the judge model. Empty means the provider
	// model with the highest output price.
	Judge string `yaml:"judge,omitempty" json:"judge,omitempty"`
	// Compare is the provider route that answers a request that the local
	// model served. Empty means the first provider route of the profile.
	Compare string `yaml:"compare,omitempty" json:"compare,omitempty"`
	// KeepDays is how long a sample is kept. The default is 14.
	KeepDays int `yaml:"keep_days,omitempty" json:"keep_days,omitempty"`
	// MaxSamples is the most samples kept. The oldest go first. The
	// default is 2000.
	MaxSamples int `yaml:"max_samples,omitempty" json:"max_samples,omitempty"`
	// NoSeed stops the first measurement of a new local model with the
	// labelled request set.
	NoSeed bool `yaml:"no_seed,omitempty" json:"no_seed,omitempty"`
	// AutoApply lets the System API apply a rule proposal by itself, when
	// the profile already names the proposal's route.
	AutoApply bool `yaml:"auto_apply,omitempty" json:"auto_apply,omitempty"`
}

// The calibration defaults.
const (
	DefaultSampleRate  = 0.02
	DefaultCalibUSD    = 1.0
	DefaultKeepDays    = 14
	DefaultMaxSamples  = 2000
	maxSampleAttempts  = 3
	maxJudgePartChars  = 24000
	judgeMaxTokens     = 4096
	calibrationSpender = "@calibration"
)

// CalibrationSpender is the name under which usage.json keeps the provider
// spend of calibration. It is not a valid client name, so no client shares
// it.
const CalibrationSpender = calibrationSpender

func (c CalibrationConfig) validate(providers ProvidersFile) error {
	if r := c.SampleRate; r != nil && (*r < 0 || *r > 1) {
		return errors.New("calibration: sample_rate must be 0 to 1")
	}
	if d := c.DailyUSD; d != nil && *d < 0 {
		return errors.New("calibration: daily_usd must not be negative")
	}
	if c.KeepDays < 0 || c.KeepDays > 365 {
		return errors.New("calibration: keep_days must be 0 to 365")
	}
	if c.MaxSamples < 0 || c.MaxSamples > 100000 {
		return errors.New("calibration: max_samples must be 0 to 100000")
	}
	for field, v := range map[string]string{"judge": c.Judge, "compare": c.Compare} {
		if v == "" {
			continue
		}
		// "local" means the profile's local model, so any name parses.
		rt, err := ParseRoute(v, "local", providers)
		if err != nil {
			return fmt.Errorf("calibration %s: %w", field, err)
		}
		if field == "compare" && rt.Local() {
			return errors.New("calibration compare: give a provider route")
		}
	}
	return nil
}

func (c CalibrationConfig) rate() float64 {
	if c.SampleRate == nil {
		return DefaultSampleRate
	}
	return *c.SampleRate
}

// Daily returns the daily provider budget of calibration.
func (c CalibrationConfig) Daily() float64 {
	if c.DailyUSD == nil {
		return DefaultCalibUSD
	}
	return *c.DailyUSD
}

func (c CalibrationConfig) keep() time.Duration {
	d := c.KeepDays
	if d == 0 {
		d = DefaultKeepDays
	}
	return time.Duration(d) * 24 * time.Hour
}

func (c CalibrationConfig) maxSamples() int {
	if c.MaxSamples == 0 {
		return DefaultMaxSamples
	}
	return c.MaxSamples
}

// Side is one answer to a sampled request.
type Side struct {
	Route     string  `json:"route"`
	Answer    string  `json:"answer"`
	Finish    string  `json:"finish,omitempty"`
	LatencyMS int64   `json:"latency_ms"`
	Usage     Usage   `json:"usage"`
	CostUSD   float64 `json:"cost_usd,omitempty"`
}

// The winners of a verdict.
const (
	WinLocal    = "local"
	WinProvider = "provider"
	WinTie      = "tie"
)

// Verdict is the judge's comparison of the two answers.
type Verdict struct {
	Time  time.Time `json:"time"`
	Judge string    `json:"judge"`
	// Winner is local, provider or tie.
	Winner string `json:"winner"`
	// LocalOK is true when the local answer won or tied.
	LocalOK bool `json:"local_ok"`
	// JudgeIsSide is true when the judge also wrote one of the answers.
	JudgeIsSide bool `json:"judge_is_side,omitempty"`
	// LocalFirst is true when the judge saw the local answer as answer A.
	LocalFirst bool    `json:"local_first"`
	CostUSD    float64 `json:"cost_usd,omitempty"`
}

// Sample is one kept request with its answers. The router writes it to
// <calibration dir>/<id>.json.
type Sample struct {
	ID         string    `json:"id"`
	Time       time.Time `json:"time"`
	Client     string    `json:"client"`
	Profile    string    `json:"profile"`
	Task       string    `json:"task,omitempty"`
	Difficulty string    `json:"difficulty,omitempty"`
	// LocalModel is the profile's local model when the request came.
	LocalModel string `json:"local_model"`
	// Served is the side that answered the client: local or provider.
	Served string `json:"served"`
	// Request is the chat request as the client sent it.
	Request  json.RawMessage `json:"request"`
	Local    *Side           `json:"local,omitempty"`
	Provider *Side           `json:"provider,omitempty"`
	Verdict  *Verdict        `json:"verdict,omitempty"`
	// Attempts counts the failed comparisons. After three the sample is
	// left alone.
	Attempts  int    `json:"attempts,omitempty"`
	LastError string `json:"last_error,omitempty"`
}

// CalibrationStatus is status.json in the calibration directory: what the
// worker did last.
type CalibrationStatus struct {
	Time time.Time `json:"time"`
	// State is idle, busy, waiting or error.
	State  string `json:"state"`
	Reason string `json:"reason,omitempty"`
}

// StatusName is the worker's status file in the calibration directory.
const StatusName = "status.json"

// chance returns true with probability p.
func chance(p float64) bool {
	if p <= 0 {
		return false
	}
	var b [8]byte
	_, _ = rand.Read(b[:])
	return float64(binary.BigEndian.Uint64(b[:])>>11)/(1<<53) < p
}

func sampleID(t time.Time) string {
	var b [4]byte
	_, _ = rand.Read(b[:])
	return fmt.Sprintf("%019d-%x", t.UnixNano(), b)
}

// samples sets whether the router keeps this request. Only a hybrid
// client's chat request that a profile routed is kept. A request that is
// sensitive, or that has images or tools, is never kept.
func (s *Server) samples(c Client, d Decision) bool {
	if s.CalibrationDir == "" || !c.Calibrate || c.Policy != Hybrid || d.Profile == "" || d.Features == nil {
		return false
	}
	f := d.Features
	if f.Sensitive || f.HasImages || f.HasTools {
		return false
	}
	return chance(s.config().Profiles.Calibration.rate())
}

// keepSample writes a sample of a request that the router answered.
func (s *Server) keepSample(c Client, d Decision, rt Route, body []byte, tap *usageTap, start time.Time) {
	cfg := s.config()
	smp := Sample{
		ID:         sampleID(start),
		Time:       start.UTC(),
		Client:     c.Name,
		Profile:    d.Profile,
		LocalModel: d.LocalModel,
		Request:    json.RawMessage(body),
	}
	if cl := d.Classification; cl != nil && cl.Error == "" {
		smp.Task, smp.Difficulty = cl.Task, cl.Difficulty
	} else if req, err := parseChat(body); err == nil {
		// The rules did not need the labels, so the request has none yet.
		cl := s.classifier().classify(cfg.Profiles.Classifier, req.lastUser())
		if cl.Error == "" {
			smp.Task, smp.Difficulty = cl.Task, cl.Difficulty
		}
	}
	side := &Side{Route: rt.String(), Answer: tap.text.String(), Finish: tap.finish,
		LatencyMS: time.Since(start).Milliseconds(), Usage: tap.result()}
	if rt.Local() {
		smp.Served, smp.Local = WinLocal, side
	} else {
		smp.Served, smp.Provider = WinProvider, side
		if p, ok := cfg.price(rt); ok {
			side.CostUSD = p.Cost(side.Usage)
		}
	}
	if err := s.writeSample(smp); err != nil {
		s.logger().Warn("keep a calibration sample", "err", err)
	}
	s.prune(cfg.Profiles.Calibration, time.Now())
}

func (s *Server) writeSample(smp Sample) error {
	b, err := json.Marshal(smp)
	if err != nil {
		return err
	}
	return writeFileAtomic(filepath.Join(s.CalibrationDir, smp.ID+".json"), b)
}

func writeFileAtomic(path string, b []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp.*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmp.Name())
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmp.Name())
		return err
	}
	return os.Rename(tmp.Name(), path)
}

// sampleFiles returns the sample file names, oldest first. The names start
// with the time in nanoseconds, so they sort by time.
func sampleFiles(dir string) []string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return nil
	}
	var names []string
	for _, e := range ents {
		n := e.Name()
		if e.Type().IsRegular() && strings.HasSuffix(n, ".json") && n != StatusName && n != SeededName && !strings.HasPrefix(n, ".") {
			names = append(names, n)
		}
	}
	slices.Sort(names)
	return names
}

// sampleTime reads the time from a sample file name.
func sampleTime(name string) (time.Time, bool) {
	ns, _, ok := strings.Cut(name, "-")
	if !ok {
		return time.Time{}, false
	}
	n, err := strconv.ParseInt(ns, 10, 64)
	if err != nil {
		return time.Time{}, false
	}
	return time.Unix(0, n), true
}

// prune deletes the samples older than the keep time and the oldest
// samples over the limit.
func (s *Server) prune(cc CalibrationConfig, now time.Time) {
	names := sampleFiles(s.CalibrationDir)
	over := len(names) - cc.maxSamples()
	for i, n := range names {
		t, ok := sampleTime(n)
		if i < over || (ok && now.Sub(t) > cc.keep()) {
			_ = os.Remove(filepath.Join(s.CalibrationDir, n))
		}
	}
}

func (s *Server) setStatus(state, reason string) {
	if s.CalibrationDir == "" {
		return
	}
	b, err := json.Marshal(CalibrationStatus{Time: time.Now().UTC(), State: state, Reason: reason})
	if err == nil {
		_ = writeFileAtomic(filepath.Join(s.CalibrationDir, StatusName), b)
	}
}

// Calibrate compares the kept samples, one at a time, while no local
// request is in flight. It checks every interval until ctx ends.
func (s *Server) Calibrate(ctx context.Context, interval time.Duration) {
	if s.CalibrationDir == "" {
		return
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		s.seed(s.config())
		s.classifier().syncOutcomes(LoadSamples(s.CalibrationDir))
		for ctx.Err() == nil && s.calibrateOne(ctx) {
		}
	}
}

// errWait means the worker must wait and try the sample again later.
type errWait struct{ reason string }

func (e errWait) Error() string { return e.reason }

// calibrateOne works on the oldest sample that has no verdict. It returns
// true when it finished a step and another can follow at once.
func (s *Server) calibrateOne(ctx context.Context) bool {
	cfg := s.config()
	cc := cfg.Profiles.Calibration
	s.prune(cc, time.Now())
	smp, ok := s.nextSample()
	if !ok {
		s.setStatus("idle", "")
		return false
	}
	if s.localInflight.Load() > 0 {
		s.setStatus("waiting", "a local request is in flight")
		return false
	}
	err := s.compare(ctx, cfg, &smp)
	var wait errWait
	switch {
	case errors.As(err, &wait):
		s.setStatus("waiting", wait.reason)
		return false
	case err != nil:
		smp.Attempts++
		smp.LastError = err.Error()
		if len(smp.LastError) > 300 {
			smp.LastError = smp.LastError[:300]
		}
		s.setStatus("error", fmt.Sprintf("sample %s: %v", smp.ID, err))
		s.saveIfPresent(smp)
		return false
	}
	smp.LastError = ""
	s.saveIfPresent(smp)
	s.setStatus("busy", "")
	return true
}

// saveIfPresent writes a sample back unless the owner deleted it.
func (s *Server) saveIfPresent(smp Sample) {
	if _, err := os.Stat(filepath.Join(s.CalibrationDir, smp.ID+".json")); err != nil {
		return
	}
	if err := s.writeSample(smp); err != nil {
		s.logger().Warn("save a calibration sample", "err", err)
	}
}

func (s *Server) nextSample() (Sample, bool) {
	for _, n := range sampleFiles(s.CalibrationDir) {
		b, err := os.ReadFile(filepath.Join(s.CalibrationDir, n))
		if err != nil {
			continue
		}
		var smp Sample
		if json.Unmarshal(b, &smp) != nil || smp.Verdict != nil || smp.Attempts >= maxSampleAttempts {
			continue
		}
		return smp, true
	}
	return Sample{}, false
}

// compareRoute is the provider route that answers a request that the local
// model served.
func compareRoute(cfg *Config, profile string) (Route, bool) {
	cc := cfg.Profiles.Calibration
	if cc.Compare != "" {
		rt, err := ParseRoute(cc.Compare, "", cfg.Providers)
		return rt, err == nil
	}
	p := cfg.Profiles.Profiles[profile]
	routes := make([]string, 0, len(p.Rules)+len(p.Fallback))
	for _, r := range p.Rules {
		routes = append(routes, r.Route)
	}
	routes = append(routes, p.Fallback...)
	for _, r := range routes {
		if rt, err := ParseRoute(r, p.LocalModel, cfg.Providers); err == nil && !rt.Local() {
			return rt, true
		}
	}
	return judgeRoute(cfg, p.LocalModel)
}

// judgeRoute is the judge: the configured route, or the listed provider
// model with the highest output price.
func judgeRoute(cfg *Config, localModel string) (Route, bool) {
	if j := cfg.Profiles.Calibration.Judge; j != "" {
		rt, err := ParseRoute(j, localModel, cfg.Providers)
		return rt, err == nil
	}
	var best Route
	bestPrice := -1.0
	for _, name := range cfg.ProviderNames() {
		for _, m := range cfg.Providers.Providers[name].Models {
			rt := Route{Provider: name, Model: m}
			out := 0.0
			if p, ok := cfg.price(rt); ok {
				out = p.Output
			}
			if out > bestPrice {
				best, bestPrice = rt, out
			}
		}
	}
	return best, bestPrice >= 0
}

// spendOK checks the calibration budget before a provider call.
func (s *Server) spendOK(cfg *Config, rt Route) error {
	if rt.Local() {
		return nil
	}
	daily := cfg.Profiles.Calibration.Daily()
	if daily <= 0 {
		return errWait{"calibration daily_usd is 0, so no provider call is made"}
	}
	if _, ok := cfg.price(rt); !ok {
		return errWait{fmt.Sprintf("%s has no price, so the calibration budget cannot count it; add a price in providers.yaml", rt)}
	}
	if day, _ := s.Spend.Spent(calibrationSpender, time.Now()); day >= daily {
		return errWait{fmt.Sprintf("the calibration budget of %.2f USD for today is spent", daily)}
	}
	return nil
}

// compare fills the missing labels and answer, and then the verdict of a
// sample.
func (s *Server) compare(ctx context.Context, cfg *Config, smp *Sample) error {
	if smp.Task == "" {
		// The classifier was not ready when the request came.
		if req, err := parseChat(smp.Request); err == nil {
			if cl := s.classifier().classify(cfg.Profiles.Classifier, req.lastUser()); cl.Error == "" {
				smp.Task, smp.Difficulty = cl.Task, cl.Difficulty
			}
		}
	}
	if smp.Local == nil {
		rt := Route{Model: smp.LocalModel}
		// The worker never makes llama-swap load a model, because a swap
		// delays the next client request.
		if !s.localReady(rt.Model) {
			return errWait{fmt.Sprintf("%s is not loaded", rt.Model)}
		}
		side, err := s.answer(ctx, cfg, rt, smp.Request)
		if err != nil {
			return err
		}
		smp.Local = side
		return nil
	}
	if smp.Provider == nil {
		rt, ok := compareRoute(cfg, smp.Profile)
		if !ok {
			return errWait{"no provider route to compare with"}
		}
		if err := s.spendOK(cfg, rt); err != nil {
			return err
		}
		side, err := s.answer(ctx, cfg, rt, smp.Request)
		if err != nil {
			return err
		}
		smp.Provider = side
		return nil
	}
	judge, ok := judgeRoute(cfg, smp.LocalModel)
	if !ok {
		return errWait{"no judge model"}
	}
	if judge.Local() && !s.localReady(judge.Model) {
		return errWait{fmt.Sprintf("the judge %s is not loaded", judge.Model)}
	}
	if err := s.spendOK(cfg, judge); err != nil {
		return err
	}
	v, err := s.judge(ctx, cfg, judge, smp)
	if err != nil {
		return err
	}
	smp.Verdict = v
	return nil
}

// complete sends one chat request without streaming and reads the answer.
func (s *Server) complete(ctx context.Context, cfg *Config, rt Route, body []byte) (*Side, error) {
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		return nil, err
	}
	req["model"] = rt.Model
	req["stream"] = false
	delete(req, "stream_options")
	b, err := json.Marshal(req)
	if err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := s.do(ctx, cfg, rt, "/chat/completions", b, http.Header{})
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	out, err := io.ReadAll(io.LimitReader(resp.Body, maxTapBody))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s returned %d: %s", rt, resp.StatusCode, upstreamMessage(out))
	}
	tap := &usageTap{keep: true}
	_, _ = tap.Write(out)
	side := &Side{Route: rt.String(), LatencyMS: time.Since(start).Milliseconds(), Usage: tap.result()}
	side.Answer, side.Finish = tap.text.String(), tap.finish
	if !rt.Local() {
		if p, ok := cfg.price(rt); ok {
			side.CostUSD = p.Cost(side.Usage)
			if err := s.Spend.Add(calibrationSpender, side.CostUSD, start); err != nil {
				s.logger().Error("record the calibration spend", "err", err)
			}
		}
	}
	return side, nil
}

// answer gets the other side's answer to a sampled request.
func (s *Server) answer(ctx context.Context, cfg *Config, rt Route, request []byte) (*Side, error) {
	side, err := s.complete(ctx, cfg, rt, request)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(side.Answer) == "" {
		return nil, fmt.Errorf("%s gave no answer (finish %q)", rt, side.Finish)
	}
	return side, nil
}

const judgeSystem = `You compare two answers to the same request. Judge correctness first, then completeness, then clarity. Do not prefer an answer for its length or its position. Answer with one line of JSON and nothing after it: {"winner": "A"}, {"winner": "B"} or {"winner": "tie"}.`

var winnerRE = regexp.MustCompile(`"winner"\s*:\s*"(A|B|tie)"`)

func clipJudge(s string) string {
	if len(s) <= maxJudgePartChars {
		return s
	}
	return s[:maxJudgePartChars] + "\n[cut]"
}

// conversation renders the messages of a chat request as text.
func conversation(request []byte) string {
	req, err := parseChat(request)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for _, m := range req.Messages {
		fmt.Fprintf(&b, "%s: %s\n\n", m.Role, messageText(m.Content))
	}
	return strings.TrimSpace(b.String())
}

// judge asks the judge which answer is better. The order is random, and
// the judge does not see which model wrote which answer.
func (s *Server) judge(ctx context.Context, cfg *Config, rt Route, smp *Sample) (*Verdict, error) {
	localFirst := chance(0.5)
	a, b := smp.Local.Answer, smp.Provider.Answer
	if !localFirst {
		a, b = b, a
	}
	prompt := fmt.Sprintf("Request:\n<<<\n%s\n>>>\n\nAnswer A:\n<<<\n%s\n>>>\n\nAnswer B:\n<<<\n%s\n>>>",
		clipJudge(conversation(smp.Request)), clipJudge(a), clipJudge(b))
	body, err := json.Marshal(map[string]any{
		"model": rt.Model,
		"messages": []map[string]string{
			{"role": "system", "content": judgeSystem},
			{"role": "user", "content": prompt},
		},
		"max_tokens":  judgeMaxTokens,
		"temperature": 0,
	})
	if err != nil {
		return nil, err
	}
	side, err := s.complete(ctx, cfg, rt, body)
	if err != nil {
		return nil, err
	}
	m := winnerRE.FindAllStringSubmatch(side.Answer, -1)
	if m == nil {
		return nil, fmt.Errorf("the judge %s gave no verdict (finish %q)", rt, side.Finish)
	}
	v := &Verdict{Time: time.Now().UTC(), Judge: rt.String(), LocalFirst: localFirst, CostUSD: side.CostUSD,
		JudgeIsSide: rt.String() == smp.Local.Route || rt.String() == smp.Provider.Route}
	switch got := m[len(m)-1][1]; {
	case got == "tie":
		v.Winner = WinTie
	case (got == "A") == localFirst:
		v.Winner = WinLocal
	default:
		v.Winner = WinProvider
	}
	v.LocalOK = v.Winner != WinProvider
	return v, nil
}

// chatChunk holds the answer fields of a chat response or stream chunk.
type chatChunk struct {
	Choices []struct {
		Delta struct {
			Content string `json:"content"`
		} `json:"delta"`
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
}

// addText adds the answer text of one chat response or chunk to the tap.
func (u *usageTap) addText(data []byte) {
	var c chatChunk
	if json.Unmarshal(bytes.TrimSpace(data), &c) != nil || len(c.Choices) == 0 {
		return
	}
	ch := c.Choices[0]
	if u.text.Len() < maxTapBody {
		u.text.WriteString(ch.Delta.Content)
		u.text.WriteString(ch.Message.Content)
	}
	if ch.FinishReason != "" {
		u.finish = ch.FinishReason
	}
}
