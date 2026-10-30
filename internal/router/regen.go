package router

import (
	"crypto/sha256"
	"sync"
	"time"
)

// RegenWindow is how soon a repeat of the same conversation counts as a
// regeneration: a sign that the client did not like the first answer.
const RegenWindow = time.Minute

// regenTracker remembers, for one minute, a hash of each conversation and
// the route that answered it. It keeps no text.
type regenTracker struct {
	mu   sync.Mutex
	seen map[[32]byte]regenSeen
}

type regenSeen struct {
	at    time.Time
	route string
}

func regenKey(client string, conversation []byte) [32]byte {
	h := sha256.New()
	h.Write([]byte(client))
	h.Write([]byte{0})
	h.Write(conversation)
	var k [32]byte
	copy(k[:], h.Sum(nil))
	return k
}

// earlier returns the route that answered the same conversation of the
// client within RegenWindow before now, or "".
func (r *regenTracker) earlier(key [32]byte, now time.Time) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if s, ok := r.seen[key]; ok && now.Sub(s.at) <= RegenWindow {
		return s.route
	}
	return ""
}

// note records the route that answered a conversation, and forgets the
// conversations older than RegenWindow.
func (r *regenTracker) note(key [32]byte, route string, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.seen == nil {
		r.seen = map[[32]byte]regenSeen{}
	}
	for k, s := range r.seen {
		if now.Sub(s.at) > RegenWindow {
			delete(r.seen, k)
		}
	}
	r.seen[key] = regenSeen{at: now, route: route}
}
