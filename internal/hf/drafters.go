package hf

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
)

// Drafter is one GGUF repository with a draft model for speculative
// decoding. A drafter is trained for one target model and names it in its
// card's base_model.
type Drafter struct {
	Repo string `json:"repo"`
	// SpecType is the llama.cpp --spec-type that runs it: draft-dflash or
	// draft-eagle3.
	SpecType string `json:"spec_type"`
	// Base is the target model that the drafter's card names, and Depth
	// is how many base_model steps it is above the installed repository.
	Base      string `json:"base"`
	Depth     int    `json:"depth"`
	Downloads int    `json:"downloads"`
	Likes     int    `json:"likes"`
}

// drafterKinds are the search terms for the drafter families that llama.cpp
// b11889 runs, and their --spec-type.
var drafterKinds = []struct{ term, specType string }{
	{"dflash", "draft-dflash"},
	{"eagle3", "draft-eagle3"},
}

// BaseModels returns the repositories that repo's card names as its
// base_model. The field is a string or a list.
func (c *Client) BaseModels(ctx context.Context, repo string) ([]string, error) {
	if !ValidRepo(repo) {
		return nil, fmt.Errorf("invalid repository %q", repo)
	}
	var info struct {
		CardData struct {
			BaseModel json.RawMessage `json:"base_model"`
		} `json:"cardData"`
	}
	if err := c.get(ctx, c.Base+"/api/models/"+repo, &info); err != nil {
		return nil, err
	}
	raw := info.CardData.BaseModel
	var one string
	if json.Unmarshal(raw, &one) == nil {
		raw, _ = json.Marshal([]string{one})
	}
	var list []string
	_ = json.Unmarshal(raw, &list)
	var out []string
	for _, b := range list {
		if b != repo && ValidRepo(b) {
			out = append(out, b)
		}
	}
	return out, nil
}

// maxLineage bounds the repositories that Lineage visits, and so the Hub
// requests of one drafter search.
const maxLineage = 6

// Lineage returns repo and its base models, nearest first, with the
// number of base_model steps to each. A GGUF repository often has the
// original's name with "-GGUF" added and no base_model, so that name is
// tried at depth 1. A failed lookup ends that branch.
func (c *Client) Lineage(ctx context.Context, repo string) map[string]int {
	depth := map[string]int{repo: 0}
	queue := []string{repo}
	if s, ok := trimGGUF(repo); ok {
		depth[s] = 1
		queue = append(queue, s)
	}
	for len(queue) > 0 && len(depth) < maxLineage {
		r := queue[0]
		queue = queue[1:]
		bases, err := c.BaseModels(ctx, r)
		if err != nil {
			continue
		}
		for _, b := range bases {
			if _, seen := depth[b]; !seen && len(depth) < maxLineage {
				depth[b] = depth[r] + 1
				queue = append(queue, b)
			}
		}
	}
	return depth
}

func trimGGUF(repo string) (string, bool) {
	for _, suf := range []string{"-GGUF", "-gguf"} {
		if s, ok := strings.CutSuffix(repo, suf); ok && ValidRepo(s) {
			return s, true
		}
	}
	return "", false
}

// SearchDrafters finds GGUF drafter repositories whose card names base as a
// base model, most downloaded first.
func (c *Client) SearchDrafters(ctx context.Context, base string, limit int) ([]Drafter, error) {
	if !ValidRepo(base) {
		return nil, fmt.Errorf("invalid repository %q", base)
	}
	var out []Drafter
	for _, k := range drafterKinds {
		q := url.Values{
			"search": {k.term}, "filter": {"gguf", "base_model:" + base},
			"sort": {"downloads"}, "limit": {fmt.Sprint(limit)},
		}
		var found []Summary
		if err := c.get(ctx, c.Base+"/api/models?"+q.Encode(), &found); err != nil {
			return out, err
		}
		for _, s := range found {
			out = append(out, Drafter{Repo: s.ID, SpecType: k.specType, Base: base, Downloads: s.Downloads, Likes: s.Likes})
		}
	}
	return out, nil
}
