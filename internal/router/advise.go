package router

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// OptimizerSpender is the name under which usage.json keeps the provider
// spend of the optimizer's advisor. It is not a valid client name.
const OptimizerSpender = "@optimizer"

// AdviseRequest is the body of POST /sibyl/advise: one question to one
// provider model, for the inference optimizer.
type AdviseRequest struct {
	Provider string `json:"provider"`
	Model    string `json:"model"`
	System   string `json:"system"`
	Prompt   string `json:"prompt"`
}

// AdviseResponse is the answer of /sibyl/advise.
type AdviseResponse struct {
	Route   string  `json:"route"`
	Answer  string  `json:"answer"`
	CostUSD float64 `json:"cost_usd,omitempty"`
}

// Limits of an advise request.
const (
	maxAdvisePrompt = 64 << 10
	adviseTimeout   = 3 * time.Minute
	adviseMaxTokens = 4096
)

// advise asks a provider model a question for the optimizer. Only the
// System API key can call it. The provider keys stay in the router.
func (s *Server) advise(w http.ResponseWriter, r *http.Request) {
	if !equal(clientKey(r), s.AdminKey) {
		writeOpenAIError(w, http.StatusUnauthorized, "missing or invalid API key")
		return
	}
	body, err := readBody(w, r)
	if err != nil {
		writeOpenAIError(w, errCode(err), err.Error())
		return
	}
	var ar AdviseRequest
	if err := json.Unmarshal(body, &ar); err != nil {
		writeOpenAIError(w, http.StatusBadRequest, "the body is not valid JSON")
		return
	}
	cfg := s.config()
	if _, ok := cfg.Providers.Providers[ar.Provider]; !ok || ar.Model == "" || strings.ContainsAny(ar.Model, " \t\r\n") {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("no provider %q with a model %q", ar.Provider, ar.Model))
		return
	}
	if strings.TrimSpace(ar.Prompt) == "" || len(ar.Prompt)+len(ar.System) > maxAdvisePrompt {
		writeOpenAIError(w, http.StatusBadRequest, fmt.Sprintf("the prompt must be 1 to %d bytes", maxAdvisePrompt))
		return
	}
	rt := Route{Provider: ar.Provider, Model: ar.Model}
	chat, err := json.Marshal(map[string]any{
		"model": ar.Model,
		"messages": []map[string]string{
			{"role": "system", "content": ar.System},
			{"role": "user", "content": ar.Prompt},
		},
		"max_tokens": adviseMaxTokens,
	})
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), adviseTimeout)
	defer cancel()
	side, err := s.completeAs(ctx, cfg, rt, chat, OptimizerSpender)
	if err != nil {
		writeOpenAIError(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AdviseResponse{Route: rt.String(), Answer: side.Answer, CostUSD: side.CostUSD})
}
