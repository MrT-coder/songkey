package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// tokenMatches compares a presented token with the real one in constant time.
//
// A plain == stops at the first differing byte, so response time leaks how much
// of a guess was right. Hashing both sides first also hides the token's length,
// which ConstantTimeCompare alone would reveal.
func tokenMatches(given, want string) bool {
	a := sha256.Sum256([]byte(given))
	b := sha256.Sum256([]byte(want))
	return subtle.ConstantTimeCompare(a[:], b[:]) == 1
}

// authGuard temporarily blocks a client that keeps presenting a wrong token.
//
// The token is the only thing standing between the internet and this service
// once it sits behind a public hostname, and nothing else limits how fast it can
// be guessed. The limit is per client address and lives in memory: restarting
// the service clears it, which is acceptable for a guard against guessing.
type authGuard struct {
	mu      sync.Mutex
	clients map[string]*guardEntry

	maxFails int           // failures allowed inside window
	window   time.Duration // how long failures are remembered
	lockout  time.Duration // how long a client stays blocked
	now      func() time.Time
}

type guardEntry struct {
	fails        int
	firstFail    time.Time
	blockedUntil time.Time
}

// maxTracked bounds memory when many different addresses fail once each, as a
// scan from a botnet would.
const maxTracked = 10000

func newAuthGuard(maxFails int, window, lockout time.Duration) *authGuard {
	return &authGuard{
		clients:  make(map[string]*guardEntry),
		maxFails: maxFails,
		window:   window,
		lockout:  lockout,
		now:      time.Now,
	}
}

// Blocked reports whether the client is locked out and for how much longer.
func (g *authGuard) Blocked(client string) (time.Duration, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()

	e, ok := g.clients[client]
	if !ok {
		return 0, false
	}
	if wait := e.blockedUntil.Sub(g.now()); wait > 0 {
		return wait, true
	}
	return 0, false
}

// Fail records a wrong token. It reports true when this failure triggered a
// lockout.
func (g *authGuard) Fail(client string) bool {
	g.mu.Lock()
	defer g.mu.Unlock()

	now := g.now()
	if len(g.clients) >= maxTracked {
		g.sweep(now)
		if len(g.clients) >= maxTracked {
			// Still full of live entries: forget everything rather than grow
			// without bound. A flood this size is already being limited upstream.
			g.clients = make(map[string]*guardEntry)
		}
	}

	e, ok := g.clients[client]
	if !ok || now.Sub(e.firstFail) > g.window {
		e = &guardEntry{firstFail: now}
		g.clients[client] = e
	}
	e.fails++
	if e.fails >= g.maxFails {
		e.blockedUntil = now.Add(g.lockout)
		return true
	}
	return false
}

// Reset forgets a client's failures after it presents the right token.
func (g *authGuard) Reset(client string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	delete(g.clients, client)
}

// sweep drops entries that can no longer matter. The caller holds the lock.
func (g *authGuard) sweep(now time.Time) {
	for k, e := range g.clients {
		if now.After(e.blockedUntil) && now.Sub(e.firstFail) > g.window {
			delete(g.clients, k)
		}
	}
}

// clientIP names the caller for rate limiting.
//
// Behind a Cloudflare tunnel every connection arrives from the local tunnel
// process, so the socket address says nothing about the real caller;
// Cloudflare puts that in CF-Connecting-IP and overwrites any value a client
// sends. Reaching the port directly lets a client set that header itself, which
// could only be used to dodge the limit from inside the network — exactly the
// place the service already trusts.
func clientIP(r *http.Request) string {
	if ip := strings.TrimSpace(r.Header.Get("CF-Connecting-IP")); ip != "" {
		return ip
	}
	if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
		if first, _, _ := strings.Cut(xff, ","); strings.TrimSpace(first) != "" {
			return strings.TrimSpace(first)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
