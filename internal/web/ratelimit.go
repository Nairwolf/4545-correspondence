package web

import (
	"net"
	"net/http"
	"sync"
	"time"
)

// rateLimiter is a per-IP token bucket for the sign-in endpoints (spec
// §11: "rate limiting on registration and auth endpoints"). In-memory
// is exactly right: production is one process, and the state is worth
// nothing across a restart.
type rateLimiter struct {
	rate  float64 // tokens added per second
	burst float64 // bucket capacity, and the initial balance
	now   func() time.Time

	mu      sync.Mutex
	buckets map[string]*bucket
	lastGC  time.Time
}

type bucket struct {
	tokens float64
	last   time.Time
}

// newRateLimiter allows burst requests at once, refilling at perMinute.
func newRateLimiter(perMinute, burst int) *rateLimiter {
	return &rateLimiter{
		rate:    float64(perMinute) / 60,
		burst:   float64(burst),
		now:     time.Now,
		buckets: map[string]*bucket{},
	}
}

// allow reports whether one more request from key may proceed.
func (l *rateLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	l.gc(now)

	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: l.burst, last: now}
		l.buckets[key] = b
	}
	b.tokens = min(l.burst, b.tokens+now.Sub(b.last).Seconds()*l.rate)
	b.last = now

	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// gc drops buckets that have been idle long enough to be full again —
// they are indistinguishable from absent ones. Runs at most once a
// minute so a burst of new addresses doesn't pay for a full scan each.
func (l *rateLimiter) gc(now time.Time) {
	if now.Sub(l.lastGC) < time.Minute {
		return
	}
	l.lastGC = now
	idle := time.Duration(l.burst/l.rate) * time.Second
	for key, b := range l.buckets {
		if now.Sub(b.last) > idle {
			delete(l.buckets, key)
		}
	}
}

// middleware answers 429 once the client's bucket is empty. It keys on
// the remote IP as chi's RealIP middleware has already resolved it.
func (l *rateLimiter) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ip, _, err := net.SplitHostPort(r.RemoteAddr)
		if err != nil {
			ip = r.RemoteAddr
		}
		if !l.allow(ip) {
			w.Header().Set("Retry-After", "60")
			http.Error(w, "Too many sign-in attempts from your address. Please wait a minute and try again.", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}
