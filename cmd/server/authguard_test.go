package main

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

func TestTokenMatches(t *testing.T) {
	tests := []struct {
		given, want string
		ok          bool
	}{
		{"secret", "secret", true},
		{"secreT", "secret", false},
		{"secret ", "secret", false},
		{"", "secret", false},
		{"secre", "secret", false},
		{"secrett", "secret", false},
	}
	for _, tc := range tests {
		if got := tokenMatches(tc.given, tc.want); got != tc.ok {
			t.Errorf("tokenMatches(%q, %q) = %v, want %v", tc.given, tc.want, got, tc.ok)
		}
	}
}

// fakeClock lets the guard's timing be tested without sleeping.
type fakeClock struct{ t time.Time }

func (c *fakeClock) now() time.Time          { return c.t }
func (c *fakeClock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTestGuard() (*authGuard, *fakeClock) {
	clock := &fakeClock{t: time.Unix(1_000_000, 0)}
	g := newAuthGuard(3, time.Minute, 10*time.Minute)
	g.now = clock.now
	return g, clock
}

func TestGuardBlocksAfterMaxFailures(t *testing.T) {
	g, _ := newTestGuard()

	for i := 1; i <= 2; i++ {
		if g.Fail("1.2.3.4") {
			t.Fatalf("locked out after only %d failures", i)
		}
		if _, blocked := g.Blocked("1.2.3.4"); blocked {
			t.Fatalf("blocked after only %d failures", i)
		}
	}
	if !g.Fail("1.2.3.4") {
		t.Fatal("third failure did not trigger a lockout")
	}
	if wait, blocked := g.Blocked("1.2.3.4"); !blocked || wait <= 0 {
		t.Fatalf("Blocked() = %v, %v after the lockout", wait, blocked)
	}
}

func TestGuardIsPerClient(t *testing.T) {
	g, _ := newTestGuard()
	for i := 0; i < 3; i++ {
		g.Fail("1.2.3.4")
	}
	if _, blocked := g.Blocked("5.6.7.8"); blocked {
		t.Fatal("an unrelated client was blocked")
	}
}

func TestGuardLockoutExpires(t *testing.T) {
	g, clock := newTestGuard()
	for i := 0; i < 3; i++ {
		g.Fail("1.2.3.4")
	}
	clock.advance(10*time.Minute + time.Second)
	if _, blocked := g.Blocked("1.2.3.4"); blocked {
		t.Fatal("still blocked after the lockout elapsed")
	}
}

func TestGuardForgetsOldFailures(t *testing.T) {
	g, clock := newTestGuard()
	g.Fail("1.2.3.4")
	g.Fail("1.2.3.4")
	// The window is one minute, so these two no longer count.
	clock.advance(2 * time.Minute)
	if g.Fail("1.2.3.4") {
		t.Fatal("old failures still counted towards a lockout")
	}
}

func TestGuardResetClearsFailures(t *testing.T) {
	g, _ := newTestGuard()
	g.Fail("1.2.3.4")
	g.Fail("1.2.3.4")
	g.Reset("1.2.3.4")
	if g.Fail("1.2.3.4") {
		t.Fatal("failures survived a successful login")
	}
}

func TestGuardMemoryStaysBounded(t *testing.T) {
	g, _ := newTestGuard()
	for i := 0; i < maxTracked*2; i++ {
		g.Fail("10.0." + strconv.Itoa(i))
	}
	if n := len(g.clients); n > maxTracked {
		t.Fatalf("tracking %d clients, want at most %d", n, maxTracked)
	}
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name    string
		headers map[string]string
		remote  string
		want    string
	}{
		{"socket address", nil, "192.168.1.5:5000", "192.168.1.5"},
		{"cloudflare header wins", map[string]string{"CF-Connecting-IP": "9.9.9.9"}, "127.0.0.1:1", "9.9.9.9"},
		{"forwarded-for, first hop", map[string]string{"X-Forwarded-For": "8.8.8.8, 10.0.0.1"}, "127.0.0.1:1", "8.8.8.8"},
		{"cloudflare beats forwarded-for", map[string]string{"CF-Connecting-IP": "9.9.9.9", "X-Forwarded-For": "8.8.8.8"}, "127.0.0.1:1", "9.9.9.9"},
	}
	for _, tc := range tests {
		r := httptest.NewRequest(http.MethodGet, "/", nil)
		r.RemoteAddr = tc.remote
		for k, v := range tc.headers {
			r.Header.Set(k, v)
		}
		if got := clientIP(r); got != tc.want {
			t.Errorf("%s: clientIP() = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// authedServer builds just enough of a server to exercise authorize.
func authedServer(token string) *server {
	s := &server{guard: newAuthGuard(3, time.Minute, 10*time.Minute)}
	s.cfg.apiToken = token
	return s
}

// call runs authorize for one request and returns the status it replied with,
// or 0 when the request was let through.
func call(s *server, ip, query string, header map[string]string) int {
	r := httptest.NewRequest(http.MethodGet, "/status"+query, nil)
	r.Header.Set("CF-Connecting-IP", ip)
	for k, v := range header {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	if s.authorize(w, r, readParams(r)) {
		return 0
	}
	return w.Code
}

func TestAuthorize(t *testing.T) {
	s := authedServer("secret")

	if got := call(s, "1.1.1.1", "?token=secret", nil); got != 0 {
		t.Errorf("token parameter: status %d, want it let through", got)
	}
	if got := call(s, "1.1.1.1", "", map[string]string{"Authorization": "Bearer secret"}); got != 0 {
		t.Errorf("bearer header: status %d, want it let through", got)
	}
	if got := call(s, "1.1.1.1", "?token=nope", nil); got != http.StatusUnauthorized {
		t.Errorf("wrong token: status %d, want 401", got)
	}
	if got := call(s, "1.1.1.1", "", nil); got != http.StatusUnauthorized {
		t.Errorf("no token: status %d, want 401", got)
	}
	if got := call(s, "1.1.1.1", "", map[string]string{"Authorization": "Bearer nope"}); got != http.StatusUnauthorized {
		t.Errorf("wrong bearer: status %d, want 401", got)
	}
}

func TestAuthorizeOpenWhenNoTokenConfigured(t *testing.T) {
	if got := call(authedServer(""), "1.1.1.1", "", nil); got != 0 {
		t.Fatalf("status %d with no API_TOKEN configured, want it let through", got)
	}
}

func TestAuthorizeLocksOutGuessersEvenWithTheRightToken(t *testing.T) {
	s := authedServer("secret")

	// Three wrong guesses (the test guard's limit) from one address...
	for i := 0; i < 3; i++ {
		call(s, "6.6.6.6", "?token=guess", nil)
	}
	// ...and now even the correct token is refused: otherwise a locked-out
	// attacker could keep guessing for free.
	if got := call(s, "6.6.6.6", "?token=secret", nil); got != http.StatusTooManyRequests {
		t.Fatalf("locked-out client with the right token: status %d, want 429", got)
	}
	// Someone else is unaffected.
	if got := call(s, "7.7.7.7", "?token=secret", nil); got != 0 {
		t.Fatalf("unrelated client: status %d, want it let through", got)
	}
}

func TestAuthorizeSetsRetryAfter(t *testing.T) {
	s := authedServer("secret")
	for i := 0; i < 3; i++ {
		call(s, "6.6.6.6", "?token=guess", nil)
	}
	r := httptest.NewRequest(http.MethodGet, "/status", nil)
	r.Header.Set("CF-Connecting-IP", "6.6.6.6")
	w := httptest.NewRecorder()
	s.authorize(w, r, readParams(r))
	if w.Header().Get("Retry-After") == "" {
		t.Fatal("429 reply carries no Retry-After header")
	}
}
