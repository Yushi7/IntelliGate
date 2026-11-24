package ratelimit

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTokenBucketAcceptsBurstThenRejectsWithRoundedRetryAfter(t *testing.T) {
	now := time.Unix(1_000, 0)
	config := DefaultConfig()
	config.Rate = 0.4
	config.Burst = 2
	limiter := newTestLimiter(t, config, func() time.Time { return now })

	for i := 0; i < 2; i++ {
		if allowed, _ := limiter.Allow("192.0.2.1"); !allowed {
			t.Fatalf("burst request %d was rejected", i+1)
		}
	}
	if allowed, retryAfter := limiter.Allow("192.0.2.1"); allowed {
		t.Fatal("request beyond burst was accepted")
	} else if retryAfter != 3 {
		t.Fatalf("Retry-After = %d, want 3 seconds (ceil of 2.5)", retryAfter)
	}
}

func TestTokensRefillAccordingToElapsedTime(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Unix(2_000, 0)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	config := DefaultConfig()
	config.Rate = 2
	config.Burst = 2
	limiter := newTestLimiter(t, config, clock)

	limiter.Allow("192.0.2.2")
	limiter.Allow("192.0.2.2")
	nowMu.Lock()
	now = now.Add(500 * time.Millisecond)
	nowMu.Unlock()

	if allowed, _ := limiter.Allow("192.0.2.2"); !allowed {
		t.Fatal("one token was not refilled after 500ms at 2 tokens/second")
	}
	if allowed, retryAfter := limiter.Allow("192.0.2.2"); allowed || retryAfter != 1 {
		t.Fatalf("request after consuming refill = allowed:%t Retry-After:%d, want false and 1", allowed, retryAfter)
	}
}

func TestClientIPsHaveIndependentBuckets(t *testing.T) {
	config := DefaultConfig()
	config.Burst = 1
	limiter := newTestLimiter(t, config, func() time.Time { return time.Unix(3_000, 0) })

	if allowed, _ := limiter.Allow("192.0.2.3"); !allowed {
		t.Fatal("first client request was rejected")
	}
	if allowed, _ := limiter.Allow("192.0.2.3"); allowed {
		t.Fatal("same client received more than its burst")
	}
	if allowed, _ := limiter.Allow("192.0.2.4"); !allowed {
		t.Fatal("different client did not receive an independent bucket")
	}
}

func TestClientIPParsingSupportsIPv4AndIPv6(t *testing.T) {
	tests := []struct {
		remoteAddr string
		want       string
	}{
		{remoteAddr: "192.0.2.8:1234", want: "192.0.2.8"},
		{remoteAddr: "[2001:db8::8]:4321", want: "2001:db8::8"},
		{remoteAddr: "[::ffff:192.0.2.9]:4321", want: "192.0.2.9"},
	}
	for _, test := range tests {
		t.Run(test.remoteAddr, func(t *testing.T) {
			got, err := ClientIPFromRemoteAddr(test.remoteAddr)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("client IP = %q, want %q", got, test.want)
			}
		})
	}
	for _, malformed := range []string{"", "192.0.2.1", "spoofed.example:1234", "[invalid]:1234"} {
		if _, err := ClientIPFromRemoteAddr(malformed); err == nil {
			t.Errorf("ClientIPFromRemoteAddr(%q) error = nil, want error", malformed)
		}
	}
}

func TestConcurrentRequestsDoNotExceedBucketCapacity(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 1
	config.Burst = 20
	limiter := newTestLimiter(t, config, func() time.Time { return time.Unix(4_000, 0) })

	const requests = 200
	var allowed atomic.Int32
	var workers sync.WaitGroup
	workers.Add(requests)
	for i := 0; i < requests; i++ {
		go func() {
			defer workers.Done()
			if accepted, _ := limiter.Allow("198.51.100.1"); accepted {
				allowed.Add(1)
			}
		}()
	}
	workers.Wait()
	if got := allowed.Load(); got != int32(config.Burst) {
		t.Fatalf("accepted concurrent requests = %d, want capacity %g", got, config.Burst)
	}
}

func TestCleanupExpiresIdleBucketsButKeepsRecentlyActiveBuckets(t *testing.T) {
	var nowMu sync.Mutex
	now := time.Unix(5_000, 0)
	clock := func() time.Time {
		nowMu.Lock()
		defer nowMu.Unlock()
		return now
	}
	config := DefaultConfig()
	config.IdleTimeout = time.Minute
	limiter := newTestLimiter(t, config, clock)
	limiter.Allow("active")
	limiter.Allow("idle")

	nowMu.Lock()
	now = now.Add(50 * time.Second)
	nowMu.Unlock()
	limiter.Allow("active")

	nowMu.Lock()
	now = now.Add(50 * time.Second)
	current := now
	nowMu.Unlock()
	if removed := limiter.cleanupExpiredAt(current); removed != 1 {
		t.Fatalf("first cleanup removed %d buckets, want only the idle bucket", removed)
	}
	if _, exists := limiter.memoryStore.clients["active"]; !exists {
		t.Fatal("cleanup removed a recently active bucket")
	}

	nowMu.Lock()
	now = now.Add(11 * time.Second)
	current = now
	nowMu.Unlock()
	if removed := limiter.cleanupExpiredAt(current); removed != 1 {
		t.Fatalf("second cleanup removed %d buckets, want the now-idle bucket", removed)
	}
	if len(limiter.memoryStore.clients) != 0 {
		t.Fatalf("client buckets remaining after expiry = %d, want 0", len(limiter.memoryStore.clients))
	}
}

func TestCleanupConcurrentWithActiveBucketAccess(t *testing.T) {
	config := DefaultConfig()
	config.IdleTimeout = time.Hour
	limiter := newTestLimiter(t, config, func() time.Time { return time.Unix(6_000, 0) })
	limiter.Allow("198.51.100.2")

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 1_000; i++ {
			limiter.Allow("198.51.100.2")
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 1_000; i++ {
			limiter.CleanupExpired()
		}
	}()
	workers.Wait()
	if len(limiter.memoryStore.clients) != 1 {
		t.Fatalf("active client bucket count = %d, want 1", len(limiter.memoryStore.clients))
	}
}

func TestCleanupLoopStopsOnContextCancellation(t *testing.T) {
	config := DefaultConfig()
	config.CleanupInterval = time.Hour
	limiter := newTestLimiter(t, config, time.Now)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		limiter.RunCleanup(ctx)
		close(done)
	}()
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup loop did not stop after context cancellation")
	}
}

func TestMiddlewareRejectsBeforeDownstreamAndLeavesHealthAvailable(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 0.5
	config.Burst = 1
	limiter := newTestLimiter(t, config, func() time.Time { return time.Unix(7_000, 0) })
	var downstreamCalls atomic.Int32
	handler := limiter.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	makeRequest := func(method, path string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(method, path, nil)
		request.RemoteAddr = "203.0.113.8:1010"
		handler.ServeHTTP(recorder, request)
		return recorder
	}
	if got := makeRequest(http.MethodGet, "/api/v1/products").Code; got != http.StatusNoContent {
		t.Fatalf("first request status = %d, want 204", got)
	}
	health := makeRequest(http.MethodGet, "/health")
	if health.Code != http.StatusNoContent {
		t.Fatalf("health request status = %d, want 204", health.Code)
	}

	rejected := makeRequest(http.MethodGet, "/api/v1/products")
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("rejected request status = %d, want 429", rejected.Code)
	}
	if got := rejected.Header().Get("Retry-After"); got != "2" {
		t.Fatalf("Retry-After = %q, want 2", got)
	}
	if got := rejected.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("Content-Type = %q, want application/json", got)
	}
	if got, want := rejected.Body.String(), "{\"error\":\"rate limit exceeded\"}\n"; got != want {
		t.Fatalf("429 body = %q, want %q", got, want)
	}
	if downstreamCalls.Load() != 2 {
		t.Fatalf("downstream calls = %d, want 2 (accepted request and exempt health)", downstreamCalls.Load())
	}
	if rejected.Header().Get("X-RateLimit-Limit") != "" || rejected.Header().Get("X-RateLimit-Remaining") != "" {
		t.Fatal("middleware emitted undefined global quota headers")
	}
}

func TestMiddlewareRejectsMalformedRemoteAddr(t *testing.T) {
	limiter := newTestLimiter(t, DefaultConfig(), time.Now)
	called := false
	handler := limiter.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		called = true
	}))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "not-an-ip"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400 for malformed remote address", recorder.Code)
	}
	if called {
		t.Fatal("malformed remote address reached downstream")
	}
}

func TestLimiterConfigurationValidation(t *testing.T) {
	base := DefaultConfig()
	tests := []struct {
		name   string
		config Config
	}{
		{name: "zero rate", config: Config{Rate: 0, Burst: base.Burst, IdleTimeout: base.IdleTimeout, CleanupInterval: base.CleanupInterval}},
		{name: "negative rate", config: Config{Rate: -1, Burst: base.Burst, IdleTimeout: base.IdleTimeout, CleanupInterval: base.CleanupInterval}},
		{name: "NaN rate", config: Config{Rate: math.NaN(), Burst: base.Burst, IdleTimeout: base.IdleTimeout, CleanupInterval: base.CleanupInterval}},
		{name: "zero burst", config: Config{Rate: base.Rate, Burst: 0, IdleTimeout: base.IdleTimeout, CleanupInterval: base.CleanupInterval}},
		{name: "infinite burst", config: Config{Rate: base.Rate, Burst: math.Inf(1), IdleTimeout: base.IdleTimeout, CleanupInterval: base.CleanupInterval}},
		{name: "zero idle timeout", config: Config{Rate: base.Rate, Burst: base.Burst, CleanupInterval: base.CleanupInterval}},
		{name: "zero cleanup interval", config: Config{Rate: base.Rate, Burst: base.Burst, IdleTimeout: base.IdleTimeout}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.config); err == nil {
				t.Fatal("New() error = nil, want configuration error")
			}
		})
	}
}

func newTestLimiter(t *testing.T, config Config, clock func() time.Time) *Limiter {
	t.Helper()
	limiter, err := newWithClock(config, clock)
	if err != nil {
		t.Fatal(err)
	}
	return limiter
}
