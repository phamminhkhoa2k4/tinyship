package api

// Bearer-token authentication for the local control API and a per-source
// throttle for repeated bad-token attempts. The API is loopback-only, so
// the realistic brute-force actor is a non-admin local process; the
// window bounds how often it can guess while still letting a legit local
// admin through. Thresholds are provisional until the benchmark tunes a
// cost that is safe for the target VPS.

import (
	"context"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenVerifier authenticates a presented bearer token and returns the
// name of the token that owns it. *store.Store satisfies this interface.
type tokenVerifier interface {
	VerifyToken(ctx context.Context, token string) (string, error)
}

// actorKey carries the authenticated token name on the request context
// so handlers can record audit rows against the real caller.
type actorKey struct{}

// actorName returns the authenticated token name from the request, falling
// back to a platform actor for paths reached without authentication.
func actorName(r *http.Request) string {
	if v, ok := r.Context().Value(actorKey{}).(string); ok && v != "" {
		return v
	}
	return "api"
}

// authLimiter throttles bad-token attempts per source address with a
// fixed-window counter and a temporary lockout once the threshold is hit.
type authLimiter struct {
	window   time.Duration
	maxFails int
	lockTime time.Duration

	mu     sync.Mutex
	fails  map[string][]time.Time
	locked map[string]time.Time
}

func newAuthLimiter() *authLimiter {
	return &authLimiter{
		window:   60 * time.Second,
		maxFails: 10,
		lockTime: 60 * time.Second,
		fails:    make(map[string][]time.Time),
		locked:   make(map[string]time.Time),
	}
}

// allow reports whether src may attempt authentication now (not locked).
func (l *authLimiter) allow(src string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if until, ok := l.locked[src]; ok {
		if time.Now().Before(until) {
			return false
		}
		delete(l.locked, src)
	}
	return true
}

// fail records one failed attempt from src; if it crosses the threshold it
// returns the lockout duration (and stops counting further attempts until
// the lock expires).
func (l *authLimiter) fail(src string) time.Duration {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	cutoff := now.Add(-l.window)
	kept := l.fails[src][:0]
	for _, t := range l.fails[src] {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	kept = append(kept, now)
	l.fails[src] = kept
	if len(kept) >= l.maxFails {
		l.locked[src] = now.Add(l.lockTime)
		l.fails[src] = nil
		return l.lockTime
	}
	return 0
}

// remoteHost splits the source IP out of a TCP remote address. Unix
// socket sources have no port and pass through unchanged.
func remoteHost(addr string) string {
	if h, _, err := net.SplitHostPort(addr); err == nil {
		return h
	}
	return addr
}

// bearerToken extracts the token from an Authorization: Bearer header.
func bearerToken(r *http.Request) (string, bool) {
	const prefix = "Bearer "
	h := r.Header.Get("Authorization")
	if !strings.HasPrefix(h, prefix) {
		return "", false
	}
	tok := strings.TrimSpace(h[len(prefix):])
	if tok == "" {
		return "", false
	}
	return tok, true
}

// authMiddleware wraps the route mux. The process-health endpoint stays
// open; every other route requires a valid admin token. Failed attempts
// count against the caller's source and, on overflow, return 429.
func (s *server) authMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			next.ServeHTTP(w, r)
			return
		}
		if s.auth == nil {
			next.ServeHTTP(w, r)
			return
		}
		src := remoteHost(r.RemoteAddr)
		if !s.limiter.allow(src) {
			writeError(w, http.StatusTooManyRequests, "E_RATE_LIMITED",
				"too many failed admin token attempts; try again shortly")
			return
		}
		token, ok := bearerToken(r)
		if !ok {
			writeError(w, http.StatusUnauthorized, "E_UNAUTHORIZED",
				"missing admin token; run 'tinyship token rotate' to (re)issue one "+
					"and send it as Authorization: Bearer <token>")
			return
		}
		name, err := s.auth.VerifyToken(r.Context(), token)
		if err != nil {
			s.limiter.fail(src)
			writeError(w, http.StatusUnauthorized, "E_UNAUTHORIZED",
				"invalid or revoked admin token; run 'tinyship token rotate' to re-issue one")
			return
		}
		ctx := context.WithValue(r.Context(), actorKey{}, name)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
