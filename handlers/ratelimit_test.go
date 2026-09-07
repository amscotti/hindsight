package handlers

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// limitedRequest drives one request through the limiter with explicit
// network identity, so tests can prove buckets are keyed per client.
func limitedRequest(t *testing.T, limiter *RateLimiter, next http.HandlerFunc, target, remoteAddr, forwardedFor string, form url.Values) *httptest.ResponseRecorder {
	t.Helper()
	var body io.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	}
	req := httptest.NewRequest(http.MethodPost, target, body)
	if form != nil {
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	req.RemoteAddr = remoteAddr
	if forwardedFor != "" {
		req.Header.Set("X-Forwarded-For", forwardedFor)
	}
	rec := httptest.NewRecorder()
	limiter.Limit(next).ServeHTTP(rec, req)
	return rec
}

func okHandler(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(`<div>fine</div>`))
}

func TestLimiterPassesBurstThenDenies(t *testing.T) {
	limiter := NewRateLimiter(3, time.Hour)

	for i := 0; i < 3; i++ {
		rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.7:1234", "", nil)
		if rec.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want 200", i+1, rec.Code)
		}
	}
	denied := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.7:1234", "", nil)
	assertFlashOnly(t, denied, http.StatusTooManyRequests)
	if denied.Header().Get("Retry-After") == "" {
		t.Error("429 response carries no Retry-After hint")
	}
}

func TestLimiterRefillsOverTime(t *testing.T) {
	limiter := NewRateLimiter(1, 20*time.Millisecond)

	first := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.8:1234", "", nil)
	if first.Code != http.StatusOK {
		t.Fatalf("first status = %d, want 200", first.Code)
	}
	denied := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.8:1234", "", nil)
	if denied.Code != http.StatusTooManyRequests {
		t.Fatalf("immediate retry status = %d, want 429", denied.Code)
	}
	time.Sleep(100 * time.Millisecond)
	refilled := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.8:1234", "", nil)
	if refilled.Code != http.StatusOK {
		t.Fatalf("post-refill status = %d, want 200", refilled.Code)
	}
}

func TestLimiterBucketsArePerClient(t *testing.T) {
	limiter := NewRateLimiter(1, time.Hour)

	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.9:1234", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("client A status = %d, want 200", rec.Code)
	}
	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.9:1234", "", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("client A retry status = %d, want 429", rec.Code)
	}
	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "192.0.2.10:1234", "", nil); rec.Code != http.StatusOK {
		t.Fatalf("client B status = %d, want 200 (independent bucket)", rec.Code)
	}
}

func TestLimiterHonorsForwardedForBehindProxies(t *testing.T) {
	limiter := NewRateLimiter(1, time.Hour)

	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "127.0.0.1:8080", "203.0.113.5", nil); rec.Code != http.StatusOK {
		t.Fatalf("proxied viewer status = %d, want 200", rec.Code)
	}
	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "127.0.0.1:8080", "203.0.113.5", nil); rec.Code != http.StatusTooManyRequests {
		t.Fatalf("proxied viewer retry status = %d, want 429", rec.Code)
	}
	if rec := limitedRequest(t, limiter, okHandler, "/cards/1/vote", "127.0.0.1:8080", "203.0.113.6", nil); rec.Code != http.StatusOK {
		t.Fatalf("other proxied viewer status = %d, want 200 (independent bucket)", rec.Code)
	}
}

func TestSweepEvictsStaleBucketsKeepsFresh(t *testing.T) {
	limiter := NewRateLimiter(1, time.Hour)
	staleBase := time.Now().Add(-time.Hour)

	for i := 0; i < maxTrackedClients-1; i++ {
		limiter.buckets["stale-"+strconv.Itoa(i)] = &tokenBucket{
			lastSeen: staleBase.Add(time.Duration(i) * time.Millisecond),
		}
	}
	limiter.buckets["fresh-client"] = &tokenBucket{tokens: 1, lastSeen: time.Now()}
	if got := len(limiter.buckets); got != maxTrackedClients {
		t.Fatalf("buckets before overflow = %d, want %d", got, maxTrackedClients)
	}
	limiter.buckets["overflow-stale"] = &tokenBucket{lastSeen: staleBase.Add(-time.Minute)}

	limiter.sweep()

	if got := len(limiter.buckets); got >= maxTrackedClients {
		t.Fatalf("buckets after sweep = %d, want < %d", got, maxTrackedClients)
	}
	if _, ok := limiter.buckets["fresh-client"]; !ok {
		t.Error("sweep evicted the fresh bucket; want it kept")
	}
	if _, ok := limiter.buckets["overflow-stale"]; ok {
		t.Error("sweep kept the stalest bucket; want it evicted")
	}
	if _, ok := limiter.buckets["stale-0"]; ok {
		t.Error("sweep kept the oldest bucket; want stale entries evicted first")
	}
}

func TestLimiterPassThroughKeepsHandlerResponse(t *testing.T) {
	limiter := NewRateLimiter(5, time.Hour)

	rec := limitedRequest(t, limiter, okHandler, "/b/abc/cards", "192.0.2.11:4321", "", url.Values{"body": {"hi"}})
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if body := rec.Body.String(); body != `<div>fine</div>` {
		t.Fatalf("body = %q, want the handler fragment untouched", body)
	}
}

func TestNewRateLimiterRejectsNonPositiveBounds(t *testing.T) {
	for _, tc := range []struct {
		name   string
		burst  int
		refill time.Duration
	}{
		{"zero refill", 1, 0},
		{"negative refill", 1, -time.Second},
		{"zero burst", 0, time.Second},
		{"negative burst", -5, time.Second},
	} {
		t.Run(tc.name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("NewRateLimiter did not panic for non-positive bound")
				}
			}()
			NewRateLimiter(tc.burst, tc.refill)
		})
	}
}

func TestLimiterRetryAfterMatchesRefill(t *testing.T) {
	fast := NewRateLimiter(1, time.Second)
	limitedRequest(t, fast, okHandler, "/cards/1/vote", "192.0.2.21:1", "", nil)
	denied := limitedRequest(t, fast, okHandler, "/cards/1/vote", "192.0.2.21:1", "", nil)
	if got := denied.Header().Get("Retry-After"); got != "1" {
		t.Errorf("1s refill Retry-After = %q, want 1", got)
	}

	slow := NewRateLimiter(1, time.Hour)
	limitedRequest(t, slow, okHandler, "/cards/1/vote", "192.0.2.22:1", "", nil)
	deniedSlow := limitedRequest(t, slow, okHandler, "/cards/1/vote", "192.0.2.22:1", "", nil)
	if got := deniedSlow.Header().Get("Retry-After"); got != "3600" {
		t.Errorf("1h refill Retry-After = %q, want 3600", got)
	}
}

func TestClientKeyFallsBackOnMalformedIdentity(t *testing.T) {
	for _, tc := range []struct {
		name      string
		remote    string
		forwarded string
		want      string
	}{
		{"garbage xff uses peer", "192.0.2.30:1234", "evil, ,", "192.0.2.30"},
		{"unparseable ip uses peer", "192.0.2.31:1234", "999.999.999.999", "192.0.2.31"},
		{"blank xff uses peer", "192.0.2.32:1234", "   ", "192.0.2.32"},
		{"bare remote without port", "bare-peer", "", "bare-peer"},
		{"valid xff wins", "10.0.0.1:1234", "203.0.113.9", "203.0.113.9"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req := httptest.NewRequest(http.MethodPost, "/cards/1/vote", nil)
			req.RemoteAddr = tc.remote
			if tc.forwarded != "" {
				req.Header.Set("X-Forwarded-For", tc.forwarded)
			}
			if got := clientKey(req); got != tc.want {
				t.Errorf("clientKey = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestLimiterAllowsConcurrentUse(t *testing.T) {
	limiter := NewRateLimiter(100000, time.Hour)
	const workers = 8
	const perWorker = 200
	done := make(chan int, workers)
	for w := 0; w < workers; w++ {
		go func() {
			allowed := 0
			for i := 0; i < perWorker; i++ {
				if limiter.Allow("shared-client") {
					allowed++
				}
			}
			done <- allowed
		}()
	}
	total := 0
	for w := 0; w < workers; w++ {
		total += <-done
	}
	if total != workers*perWorker {
		t.Fatalf("concurrent allowed = %d, want %d", total, workers*perWorker)
	}

	distinct := NewRateLimiter(1, time.Hour)
	done2 := make(chan bool, workers)
	for w := 0; w < workers; w++ {
		go func(w int) {
			key := "client-" + strconv.Itoa(w)
			first := distinct.Allow(key)
			second := distinct.Allow(key)
			done2 <- (first && !second)
		}(w)
	}
	for w := 0; w < workers; w++ {
		if !<-done2 {
			t.Fatal("concurrent per-key buckets misbehaved")
		}
	}
}

func TestFlashBodyIsExactlyOneEscapedNode(t *testing.T) {
	rec := httptest.NewRecorder()
	writeFlash(rec, http.StatusTooManyRequests, `<script>alert(1)</script>`)
	if ct := rec.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("Content-Type = %q, want text/html; charset=utf-8", ct)
	}
	if cc := rec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", cc)
	}
	body := rec.Body.String()
	if strings.Count(body, `id="flash"`) != 1 {
		t.Errorf("want exactly one flash node, got body %q", body)
	}
	if strings.Contains(body, "<script>") {
		t.Errorf("flash message not escaped (body %q)", body)
	}
	if !strings.Contains(body, "&lt;script&gt;") {
		t.Errorf("escaped message missing (body %q)", body)
	}
}
