package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// Creating an identity is the one thing anyone can do without already having
// one, so it is the one thing worth limiting: without this, a loop fills the
// database with users. These numbers are meant to be invisible to a household
// and tedious for a script.
const (
	signupBurst  = 10          // new identities in a row from one address
	signupRefill = time.Minute // and one more every minute after that
)

// limiter is a token bucket per client address.
type limiter struct {
	mu      sync.Mutex
	burst   float64
	refill  time.Duration
	buckets map[string]*bucket
	now     func() time.Time // replaced in tests
}

type bucket struct {
	tokens float64
	seen   time.Time
}

func newLimiter(burst int, refill time.Duration) *limiter {
	return &limiter{
		burst:   float64(burst),
		refill:  refill,
		buckets: map[string]*bucket{},
		now:     time.Now,
	}
}

// allow reports whether this address may do the thing now, and takes a token
// if so.
func (l *limiter) allow(key string) bool {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, seen: now}
		l.buckets[key] = b
		l.sweep(now)
	}
	// Hand back one token per refill period since we last looked.
	b.tokens += now.Sub(b.seen).Seconds() / l.refill.Seconds()
	if b.tokens > l.burst {
		b.tokens = l.burst
	}
	b.seen = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// sweep forgets addresses whose buckets have long since refilled, so a busy
// server does not accumulate one entry per address that ever visited. Called
// with the lock held, and only when adding an address.
func (l *limiter) sweep(now time.Time) {
	if len(l.buckets) < 1000 {
		return
	}
	stale := time.Duration(l.burst) * l.refill
	for key, b := range l.buckets {
		if now.Sub(b.seen) > stale {
			delete(l.buckets, key)
		}
	}
}

// clientIP is who to count against. A proxy's own address is used when there
// is one in front: X-Forwarded-For is not trusted, because anyone can send it
// and a limit you can opt out of is not a limit. Behind a shared proxy the
// whole site counts as one caller, which is the safe way round.
func clientIP(r *http.Request) string {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// limited wraps a handler so that one address cannot call it endlessly.
// denied says what to do about the ones that are turned away.
func (s *Server) limited(h http.HandlerFunc, denied http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.signups.allow(clientIP(r)) {
			w.Header().Set("Retry-After", "60")
			denied(w, r)
			return
		}
		h(w, r)
	}
}
