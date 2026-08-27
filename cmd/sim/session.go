// Session store with disk persistence and transparent re-bridge.
//
// This is session plumbing only (README 纪律1): the JWT is WeKnora's
// judgment of who this is; this file never decides what they may do.
//
// Lifecycle: a portal session outlives the 24h bridge token. Sessions
// are persisted to disk so a sim restart keeps users logged in, and a
// token at/near expiry is silently re-bridged (bridge is idempotent —
// WeKnora re-resolves spaces and re-judges everything on every token).
package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"log"
	"os"
	"sync"
	"time"
)

const (
	sessionAbsTTL    = 30 * 24 * time.Hour // absolute portal-session lifetime
	tokenRefreshSkew = 5 * time.Minute     // re-bridge this close to exp
)

type portalSession struct {
	mu           sync.RWMutex // serializes token refresh for this session
	Token        string
	RefreshToken string
	UUMUserID    string
	DisplayName  string
	TokenExp     time.Time // expiry of the current JWT (bridge issues 24h)
	Expires      time.Time // absolute session expiry (cookie mirrors this)
}

// token returns the current JWT. Refresh is handled by resolve.
func (s *portalSession) token() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.Token
}

func (s *portalSession) staleToken() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return time.Until(s.TokenExp) < tokenRefreshSkew
}

// bridgeFn mints a fresh JWT for a uum id. Wired to server.bridge.
type bridgeFn func(ctx context.Context, uum string) (token, refresh string, err error)

type sessionStore struct {
	mu       sync.Mutex
	sessions map[string]*portalSession
	path     string // "" → memory only
}

func newSessionStore(path string) *sessionStore {
	st := &sessionStore{sessions: map[string]*portalSession{}, path: path}
	if path != "" {
		st.loadFromDisk()
	}
	return st
}

// sessionRecord is the on-disk shape. The file holds live JWTs, so it
// must sit outside the repo (gitignored) with 0600 perms.
type sessionRecord struct {
	ID        string `json:"id"`
	UUM       string `json:"uum"`
	Token     string `json:"token"`
	Refresh   string `json:"refresh,omitempty"`
	Name      string `json:"name"`
	TokenExp  int64  `json:"token_exp"`
	Expires   int64  `json:"expires"`
}

func (s *sessionStore) loadFromDisk() {
	raw, err := os.ReadFile(s.path)
	if err != nil {
		if !os.IsNotExist(err) {
			log.Printf("session store: read %s failed: %v", s.path, err)
		}
		return
	}
	var recs []sessionRecord
	if err := json.Unmarshal(raw, &recs); err != nil {
		log.Printf("session store: parse %s failed, starting empty: %v", s.path, err)
		return
	}
	now := time.Now()
	for _, r := range recs {
		if r.ID == "" || r.UUM == "" || r.Token == "" {
			continue
		}
		if now.After(time.Unix(r.Expires, 0)) {
			continue // absolute lifetime over; user re-logs in
		}
		// token_exp in the past is fine: first use will re-bridge
		s.sessions[r.ID] = &portalSession{
			Token: r.Token, RefreshToken: r.Refresh, UUMUserID: r.UUM,
			DisplayName: r.Name,
			TokenExp:    time.Unix(r.TokenExp, 0),
			Expires:     time.Unix(r.Expires, 0),
		}
	}
	log.Printf("session store: restored %d session(s) from %s", len(s.sessions), s.path)
}

func (s *sessionStore) saveLocked() {
	if s.path == "" {
		return
	}
	recs := make([]sessionRecord, 0, len(s.sessions))
	now := time.Now()
	for id, sess := range s.sessions {
		if now.After(sess.Expires) {
			continue
		}
		recs = append(recs, sessionRecord{
			ID: id, UUM: sess.UUMUserID, Token: sess.token(),
			Refresh: sess.RefreshToken, Name: sess.DisplayName,
			TokenExp: sess.TokenExp.Unix(), Expires: sess.Expires.Unix(),
		})
	}
	raw, err := json.Marshal(recs)
	if err != nil {
		log.Printf("session store: marshal failed: %v", err)
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		log.Printf("session store: write %s failed: %v", tmp, err)
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		log.Printf("session store: rename to %s failed: %v", s.path, err)
		return
	}
}

func (s *sessionStore) put(id string, sess *portalSession) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions[id] = sess
	for k, v := range s.sessions { // opportunistic sweep
		if time.Now().After(v.Expires) {
			delete(s.sessions, k)
		}
	}
	s.saveLocked()
}

func (s *sessionStore) drop(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.sessions, id)
	s.saveLocked()
}

// resolve returns a session with a usable JWT, transparently re-bridging
// when the stored token is at/near expiry. Returns nil when the cookie
// matches no live session. A failed re-bridge falls back to the stale
// token (WeKnora will 401 it) rather than killing the session.
func (s *sessionStore) resolve(ctx context.Context, id string, bridge bridgeFn) *portalSession {
	s.mu.Lock()
	sess, ok := s.sessions[id]
	if ok && time.Now().After(sess.Expires) {
		delete(s.sessions, id)
		s.saveLocked()
		ok = false
	}
	s.mu.Unlock()
	if !ok {
		return nil
	}
	if !sess.staleToken() {
		return sess
	}
	sess.mu.Lock()
	if time.Until(sess.TokenExp) >= tokenRefreshSkew { // refreshed while waiting
		sess.mu.Unlock()
		return sess
	}
	token, refresh, err := bridge(ctx, sess.UUMUserID)
	if err != nil {
		log.Printf("session store: re-bridge for %s failed (%v); keeping stale token", sess.UUMUserID, err)
		sess.mu.Unlock()
		return sess
	}
	sess.Token, sess.RefreshToken = token, refresh
	sess.TokenExp = tokenExpiry(token, time.Now().Add(24*time.Hour))
	log.Printf("session store: re-bridged %s, token exp %s", sess.UUMUserID, sess.TokenExp.Format(time.RFC3339))
	// Release sess.mu before persisting: saveLocked reads token() which
	// RLocks the same mutex — RWMutex is not reentrant.
	sess.mu.Unlock()
	s.mu.Lock()
	s.saveLocked()
	s.mu.Unlock()
	return sess
}

// tokenExpiry parses the exp claim out of a JWT without verifying it —
// WeKnora is the verifier; we only schedule our own re-bridge.
func tokenExpiry(tok string, fallback time.Time) time.Time {
	parts := splitJWT(tok)
	if len(parts) != 3 {
		return fallback
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return fallback
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if json.Unmarshal(payload, &claims) != nil || claims.Exp <= 0 {
		return fallback
	}
	return time.Unix(claims.Exp, 0)
}

func splitJWT(tok string) []string {
	var out []string
	start := 0
	for i := 0; i < len(tok); i++ {
		if tok[i] == '.' {
			out = append(out, tok[start:i])
			start = i + 1
		}
	}
	return append(out, tok[start:])
}
