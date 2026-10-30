package router

import (
	"sort"
	"sync"
)

// MinOutcomes is the number of judged samples of a local model that the
// label needs_provider waits for. With fewer, the label is unknown.
const MinOutcomes = 20

// outcome is one judged sample: its request, embedded, and whether the
// local answer won or tied.
type outcome struct {
	id  string
	vec []float32
	ok  bool
}

// outcomeIndex holds the outcomes per local model. Its vectors come from
// the classifier's embedding model and prefix, named by sig.
type outcomeIndex struct {
	mu      sync.RWMutex
	sig     string
	byLocal map[string][]outcome
}

// maxOutcomeEmbeds limits the requests that one sync embeds, so that a
// large store fills over several worker ticks.
const maxOutcomeEmbeds = 256

// syncOutcomes makes the outcome index match the judged samples. It
// embeds the requests that the index does not hold yet.
func (c *classifier) syncOutcomes(samples []Sample) {
	c.mu.RLock()
	model, prefix, ready := c.model, c.prefix, c.ready
	c.mu.RUnlock()
	if !ready {
		return
	}
	sig := model + "\x00" + prefix
	o := &c.outcomes
	o.mu.RLock()
	have := map[string]bool{}
	if o.sig == sig {
		for _, outs := range o.byLocal {
			for _, x := range outs {
				have[x.id] = true
			}
		}
	}
	o.mu.RUnlock()

	judged := map[string]Sample{}
	var texts []string
	var add []Sample
	for _, smp := range samples {
		if smp.Verdict == nil {
			continue
		}
		judged[smp.ID] = smp
		if have[smp.ID] || len(add) >= maxOutcomeEmbeds {
			continue
		}
		req, err := parseChat(smp.Request)
		if err != nil || req.lastUser() == "" {
			continue
		}
		texts = append(texts, prefix+clip(req.lastUser()))
		add = append(add, smp)
	}
	var vecs [][]float32
	if len(texts) > 0 {
		var err error
		if vecs, err = c.embedAll(model, texts); err != nil {
			c.log("calibration outcomes not embedded", "err", err)
			add = nil
		}
	}

	o.mu.Lock()
	defer o.mu.Unlock()
	if o.sig != sig {
		o.sig, o.byLocal = sig, map[string][]outcome{}
	}
	// A deleted sample stops voting.
	for lm, outs := range o.byLocal {
		kept := outs[:0]
		for _, x := range outs {
			if _, ok := judged[x.id]; ok {
				kept = append(kept, x)
			}
		}
		o.byLocal[lm] = kept
	}
	for i, smp := range add {
		o.byLocal[smp.LocalModel] = append(o.byLocal[smp.LocalModel], outcome{id: smp.ID, vec: vecs[i], ok: smp.Verdict.LocalOK})
	}
}

// vote sets the label needs_provider of a classification: true when most
// of the k nearest outcomes of the local model went to the provider. The
// label stays unknown below MinOutcomes.
func (c *classifier) vote(localModel string, k int, cl *Classification) {
	if cl == nil || cl.vec == nil {
		return
	}
	o := &c.outcomes
	o.mu.RLock()
	outs := o.byLocal[localModel]
	o.mu.RUnlock()
	cl.Outcomes = len(outs)
	if len(outs) < MinOutcomes {
		return
	}
	type scored struct {
		ok  bool
		sim float64
	}
	all := make([]scored, len(outs))
	for i, x := range outs {
		all[i] = scored{x.ok, dot(cl.vec, x.vec)}
	}
	sort.SliceStable(all, func(a, b int) bool { return all[a].sim > all[b].sim })
	all = all[:min(k, len(all))]
	bad := 0
	for _, s := range all {
		if !s.ok {
			bad++
		}
	}
	needs := bad*2 > len(all)
	cl.NeedsProvider = &needs
}
