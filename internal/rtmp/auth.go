package rtmp

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	// adminCookieName is the session cookie handed out after a successful login.
	adminCookieName = "fastrtmp_admin"
	// adminSessionTTL is how long a dashboard login stays valid without use.
	adminSessionTTL = 12 * time.Hour
	// adminMaxFails is how many bad passwords an address may send before it is
	// temporarily locked out.
	adminMaxFails = 5
	adminLockout  = 30 * time.Second
)

// adminAuth holds the dashboard password (as a hash, never as plain text) and
// the live login sessions. An empty password disables the gate entirely, which
// keeps the pre-existing "open dashboard" behaviour for trusted LANs.
type adminAuth struct {
	mu       sync.Mutex
	sum      []byte               // sha256(password); nil => no password set
	sessions map[string]time.Time // token -> expiry
	fails    map[string]*failCount
}

type failCount struct {
	n    int
	last time.Time
	// until is non-zero only while the address is locked out.
	until time.Time
}

func newAdminAuth(password string) *adminAuth {
	a := &adminAuth{
		sessions: make(map[string]time.Time),
		fails:    make(map[string]*failCount),
	}
	a.setPassword(password)
	return a
}

// setPassword installs a new password and drops every existing session: a
// password change must not leave old cookies working.
func (a *adminAuth) setPassword(pw string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	// Only an entirely blank value disables the gate; leading or trailing
	// spaces are part of the password.
	if strings.TrimSpace(pw) == "" {
		a.sum = nil
	} else {
		sum := sha256.Sum256([]byte(pw))
		a.sum = sum[:]
	}
	a.sessions = make(map[string]time.Time)
	a.fails = make(map[string]*failCount)
}

// ValidateAdminPassword rejects values that cannot be typed back into the
// login form or that are pointlessly long. Unlike a stream key it never travels
// in a URL, so spaces and punctuation are allowed.
func ValidateAdminPassword(pw string) error {
	if len(pw) > 200 {
		return fmt.Errorf("password is too long (max 200 bytes)")
	}
	for _, r := range pw {
		if r < 0x20 || r == 0x7f {
			return fmt.Errorf("password must not contain control characters")
		}
	}
	return nil
}

// enabled reports whether a dashboard password is configured.
func (a *adminAuth) enabled() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.sum != nil
}

// check compares a supplied password against the stored hash in constant time.
func (a *adminAuth) check(pw string) bool {
	a.mu.Lock()
	sum := a.sum
	a.mu.Unlock()
	if sum == nil {
		return true
	}
	got := sha256.Sum256([]byte(pw))
	return subtle.ConstantTimeCompare(sum, got[:]) == 1
}

// issue creates a session token.
func (a *adminAuth) issue() string {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		// crypto/rand does not fail in practice; fall back to a time-seeded
		// value rather than panicking in a request handler.
		for i := range raw {
			raw[i] = byte(time.Now().UnixNano() >> (i % 8 * 8))
		}
	}
	tok := hex.EncodeToString(raw[:])
	a.mu.Lock()
	a.sessions[tok] = time.Now().Add(adminSessionTTL)
	a.mu.Unlock()
	return tok
}

// valid reports whether a token is still good, sliding its expiry forward so an
// open dashboard does not log itself out in the middle of a session.
func (a *adminAuth) valid(token string) bool {
	if token == "" {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	exp, ok := a.sessions[token]
	if !ok {
		return false
	}
	now := time.Now()
	if now.After(exp) {
		delete(a.sessions, token)
		return false
	}
	// Only rewrite when the token is more than halfway through its life, to
	// keep the once-a-second dashboard polling cheap.
	if exp.Sub(now) < adminSessionTTL/2 {
		a.sessions[token] = now.Add(adminSessionTTL)
	}
	return true
}

func (a *adminAuth) revoke(token string) {
	a.mu.Lock()
	delete(a.sessions, token)
	a.mu.Unlock()
}

// locked reports how long an address must still wait before it may try again.
func (a *adminAuth) locked(addr string) (time.Duration, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[addr]
	if f == nil || f.until.IsZero() {
		return 0, false
	}
	if wait := time.Until(f.until); wait > 0 {
		return wait, true
	}
	delete(a.fails, addr) // lockout expired, start with a clean slate
	return 0, false
}

func (a *adminAuth) noteFailure(addr string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	f := a.fails[addr]
	if f == nil {
		f = &failCount{}
		a.fails[addr] = f
	}
	f.n++
	f.last = time.Now()
	if f.n >= adminMaxFails {
		f.until = time.Now().Add(adminLockout)
		f.n = 0
	}
}

func (a *adminAuth) noteSuccess(addr string) {
	a.mu.Lock()
	delete(a.fails, addr)
	a.mu.Unlock()
}

// sweep drops expired sessions and stale failure counters.
func (a *adminAuth) sweep() {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for tok, exp := range a.sessions {
		if now.After(exp) {
			delete(a.sessions, tok)
		}
	}
	for addr, f := range a.fails {
		expiredLock := !f.until.IsZero() && now.After(f.until)
		stale := f.n == 0 && now.Sub(f.last) > time.Minute
		if expiredLock || stale {
			delete(a.fails, addr)
		}
	}
}

// authenticated reports whether a request carries a valid admin session.
// Without a configured password every request is considered authenticated.
func (a *adminAuth) authenticated(r *http.Request) bool {
	if !a.enabled() {
		return true
	}
	c, err := r.Cookie(adminCookieName)
	if err != nil {
		return false
	}
	return a.valid(c.Value)
}

// ---------- stream secrets (publish / play keys) ----------

// keyStore holds the two shared secrets. Unlike the rest of Config they are
// mutable: the dashboard can rotate them while the server runs, so guards that
// read them must go through the mutex-protected accessors.
type keyStore struct {
	mu      sync.RWMutex
	pubKey  string
	playKey string
}

func (k *keyStore) publish() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.pubKey
}

func (k *keyStore) play() string {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.playKey
}

func (k *keyStore) setPublish(v string) {
	k.mu.Lock()
	k.pubKey = v
	k.mu.Unlock()
}

func (k *keyStore) setPlay(v string) {
	k.mu.Lock()
	k.playKey = v
	k.mu.Unlock()
}

// snapshot returns both secrets under a single lock acquisition.
func (k *keyStore) snapshot() (publish, play string) {
	k.mu.RLock()
	defer k.mu.RUnlock()
	return k.pubKey, k.playKey
}

// ValidateStreamKey rejects secrets that would be awkward or unsafe inside the
// ?key=... query parameter that OBS, ffmpeg and browsers have to carry. An
// empty string is valid and means "no auth".
func ValidateStreamKey(k string) error {
	if len(k) > 128 {
		return fmt.Errorf("key is too long (max 128 bytes)")
	}
	for _, r := range k {
		if r <= 0x20 || r == 0x7f {
			return fmt.Errorf("key must not contain spaces or control characters")
		}
		if strings.ContainsRune("?#&=%/+", r) {
			return fmt.Errorf("key must not contain %q", string(r))
		}
	}
	return nil
}

// clientIP extracts the peer address without trusting proxy headers: the
// dashboard is normally exposed directly, and honouring X-Forwarded-For would
// let anyone bypass the login lockout.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
