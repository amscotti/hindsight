package handlers

import (
	"math"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"
)

// mutationBurst and mutationRefill bound how fast one client may hit the
// mutation endpoints the limiter guards (card and wall adds, votes,
// comments). Thirty requests burst with one token a second back is
// generous for a human facilitator or voter clicking through a retro
// and tight enough to blunt a runaway loop or a flood script.
const (
	mutationBurst  = 30
	mutationRefill = time.Second
)

// maxTrackedClients caps the bucket table; past it the stalest idle
// buckets are swept so a scanner rotating source addresses cannot
// grow memory without bound.
const maxTrackedClients = 4096

// RateLimiter is a per-client token bucket guarding the mutation
// endpoints. Buckets refill lazily on access and are safe for
// concurrent use. The zero value is not usable; build one with
// NewRateLimiter.
type RateLimiter struct {
	mu      sync.Mutex
	buckets map[string]*tokenBucket
	burst   float64
	rate    float64
}

// tokenBucket holds one client's balance plus its last access, so idle
// buckets can be swept once the table grows past its cap.
type tokenBucket struct {
	tokens   float64
	lastSeen time.Time
}

// NewRateLimiter builds a limiter allowing burst requests at once and
// then one token per refill interval per client, sustained. It panics on
// non-positive burst or refill so misconfiguration fails fast in tests
// and at startup instead of silently disabling the limit (zero refill
// would divide by zero, negative values would invert the rate).
func NewRateLimiter(burst int, refill time.Duration) *RateLimiter {
	if burst <= 0 {
		panic("handlers: NewRateLimiter burst must be > 0")
	}
	if refill <= 0 {
		panic("handlers: NewRateLimiter refill must be > 0")
	}
	return &RateLimiter{
		buckets: map[string]*tokenBucket{},
		burst:   float64(burst),
		rate:    float64(time.Second) / float64(refill),
	}
}

// NewMutationLimiter builds the limiter with the production bounds for
// the vote/add/comment endpoints.
func NewMutationLimiter() *RateLimiter {
	return NewRateLimiter(mutationBurst, mutationRefill)
}

// Allow spends one token for key, refilling the bucket for the time
// since its last access. Unknown keys start full.
func (l *RateLimiter) Allow(key string) bool {
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()

	bucket, ok := l.buckets[key]
	if !ok {
		if len(l.buckets) >= maxTrackedClients {
			l.sweep()
		}
		bucket = &tokenBucket{tokens: l.burst, lastSeen: now}
		l.buckets[key] = bucket
	} else {
		bucket.tokens += now.Sub(bucket.lastSeen).Seconds() * l.rate
		if bucket.tokens > l.burst {
			bucket.tokens = l.burst
		}
		bucket.lastSeen = now
	}
	if bucket.tokens < 1 {
		return false
	}
	bucket.tokens--
	return true
}

// sweep drops the stalest idle buckets until the table fits its cap in a
// single pass. Callers hold the lock.
func (l *RateLimiter) sweep() {
	overflow := len(l.buckets) - maxTrackedClients + 1
	if overflow < 1 {
		overflow = 1
	}
	type entry struct {
		key  string
		seen time.Time
	}
	entries := make([]entry, 0, len(l.buckets))
	for key, bucket := range l.buckets {
		entries = append(entries, entry{key, bucket.lastSeen})
	}
	slices.SortFunc(entries, func(a, b entry) int {
		return a.seen.Compare(b.seen)
	})
	for i := 0; i < overflow && i < len(entries); i++ {
		delete(l.buckets, entries[i].key)
	}
}

// Limit wraps a mutation handler: requests with a token pass through
// untouched, and exhausted clients get 429 plus the flash fragment the
// vote-button state machine already renders, with a Retry-After hint.
// The signature stays (http.HandlerFunc) -> http.HandlerFunc because
// main.go passes the result straight to ServeMux.HandleFunc, which takes
// a func; an http.Handler return would break every wiring site. Method
// routing happens in the mux before this wrapper runs, so wrong-method
// requests never reach the limiter and are never charged a token.
func (l *RateLimiter) Limit(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := clientKey(r)
		if !l.Allow(key) {
			w.Header().Set("Retry-After", strconv.Itoa(l.retryAfterSecs(key)))
			writeFlash(w, http.StatusTooManyRequests, "Slow down a little, then try again.")
			return
		}
		next(w, r)
	}
}

// retryAfterSecs reports how many whole seconds until the bucket for key
// holds one token again, rounded up with a floor of 1.
func (l *RateLimiter) retryAfterSecs(key string) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	bucket, ok := l.buckets[key]
	if !ok {
		return 1
	}
	if bucket.tokens >= 1 {
		return 1
	}
	secs := int(math.Ceil((1 - bucket.tokens) / l.rate))
	if secs < 1 {
		return 1
	}
	return secs
}

// clientKey identifies one caller for the bucket table. Behind a proxy
// or tunnel the TCP peer is the proxy itself, so the leftmost
// forwarded address wins when present and well-formed; otherwise the
// connection peer is the key. A spoofed header only isolates the
// sender into their own bucket, and every bucket is still bounded.
// Trust trade-off: anyone can send any X-Forwarded-For value, so a
// client can land in another IP's bucket (dodging or inheriting its
// limit). That is accepted: buckets only dampen abuse, they gate no
// auth or identity decision. Preferring the header is still required
// so per-IP buckets work at all behind a proxy/tunnel (e.g. ngrok),
// where every connection's RemoteAddr is the proxy itself.
func clientKey(r *http.Request) string {
	if forwarded := r.Header.Get("X-Forwarded-For"); forwarded != "" {
		if first, _, _ := strings.Cut(forwarded, ","); strings.TrimSpace(first) != "" {
			if ip := net.ParseIP(strings.TrimSpace(first)); ip != nil {
				return ip.String()
			}
		}
	}
	if host, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		return host
	}
	return r.RemoteAddr
}
