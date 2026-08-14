package oauth

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"
)

// sessionTTL is the sliding lifetime of a Query Session. Each authenticated
// request refreshes the expiry.
const sessionTTL = 30 * time.Minute

// Session is a single Query Session: the requester's GitHub token plus any
// cached per-session data. The cache fields are guarded by mu since a session
// can serve concurrent requests.
type Session struct {
	Token     string
	ExpiresAt time.Time

	mu        sync.Mutex
	teams     []Team
	teamRepos map[string][]string
}

// CachedTeams returns the cached teams, or nil if not yet fetched.
func (s *Session) CachedTeams() []Team {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.teams
}

// SetTeams caches the user's teams.
func (s *Session) SetTeams(t []Team) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.teams = t
}

// CachedTeamRepos returns the cached repos for a team, or nil if not yet fetched.
func (s *Session) CachedTeamRepos(slug string) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.teamRepos[slug]
}

// SetTeamRepos caches the repos for a team.
func (s *Session) SetTeamRepos(slug string, repos []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.teamRepos == nil {
		s.teamRepos = make(map[string][]string)
	}
	s.teamRepos[slug] = repos
}

// MemorySessions keeps Query Sessions in process memory, keyed by an
// unguessable session ID. Sessions are lost on restart — the requester simply
// logs in again. It implements SessionStore and is safe for concurrent use.
type MemorySessions struct {
	mu       sync.Mutex
	sessions map[string]*Session
}

// NewSessionStore returns an empty in-memory SessionStore.
func NewSessionStore() *MemorySessions {
	return &MemorySessions{sessions: make(map[string]*Session)}
}

// New creates a Session for token and returns its session ID.
func (s *MemorySessions) New(token string) (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	id := hex.EncodeToString(b)

	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = &Session{Token: token, ExpiresAt: time.Now().Add(sessionTTL)}
	return id, nil
}

// Lookup returns the Session for id, refreshing its expiry (sliding TTL).
// Returns nil if the session does not exist or has expired.
func (s *MemorySessions) Lookup(id string) *Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune()
	sess, ok := s.sessions[id]
	if !ok {
		return nil
	}
	sess.ExpiresAt = time.Now().Add(sessionTTL)
	return sess
}

// Delete removes the session for id, if present.
func (s *MemorySessions) Delete(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
}

// prune removes expired sessions. Must be called with s.mu held.
func (s *MemorySessions) prune() {
	now := time.Now()
	for id, sess := range s.sessions {
		if now.After(sess.ExpiresAt) {
			delete(s.sessions, id)
		}
	}
}
