package oauth

import (
	"sync"
	"time"
)

// stateExpiry is how long an OAuth state nonce stays valid.
const stateExpiry = 10 * time.Minute

// stateSet is an in-memory set of OAuth state nonces with expiry, used as
// CSRF protection for the login flow. Safe for concurrent use.
type stateSet struct {
	mu     sync.Mutex
	states map[string]time.Time // state → expiry
}

func newStateSet() *stateSet {
	return &stateSet{states: make(map[string]time.Time)}
}

// add generates a fresh nonce, records it, and returns it.
func (s *stateSet) add() (string, error) {
	state, err := randomState()
	if err != nil {
		return "", err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	s.states[state] = time.Now().Add(stateExpiry)
	return state, nil
}

// consume reports whether state is present and unexpired, removing it.
func (s *stateSet) consume(state string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	expiry, ok := s.states[state]
	if ok {
		delete(s.states, state)
	}
	return ok && time.Now().Before(expiry)
}

// prune removes expired states. Must be called with s.mu held.
func (s *stateSet) prune() {
	now := time.Now()
	for st, exp := range s.states {
		if now.After(exp) {
			delete(s.states, st)
		}
	}
}
