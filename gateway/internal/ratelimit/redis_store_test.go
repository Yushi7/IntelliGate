package ratelimit

import (
	"context"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

var redisTestNamespaceCounter atomic.Uint64

func TestRedisStoreSharesBurstAcrossInstancesAndSeparatesClients(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 0.000001
	config.Burst = 20
	storeA, options := newRedisTestStore(t, config)
	storeB, err := NewRedisStore(config, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := storeB.Close(); err != nil {
			t.Errorf("close second Redis store: %v", err)
		}
	})

	for i := 0; i < 12; i++ {
		if allowed, _, err := storeA.Consume(context.Background(), "192.0.2.10"); err != nil || !allowed {
			t.Fatalf("store A request %d: allowed=%t err=%v, want allowed", i+1, allowed, err)
		}
	}
	for i := 0; i < 8; i++ {
		if allowed, _, err := storeB.Consume(context.Background(), "192.0.2.10"); err != nil || !allowed {
			t.Fatalf("store B request %d: allowed=%t err=%v, want allowed", i+1, allowed, err)
		}
	}
	if allowed, retryAfter, err := storeB.Consume(context.Background(), "192.0.2.10"); err != nil || allowed || retryAfter <= 0 {
		t.Fatalf("request beyond shared burst: allowed=%t Retry-After=%d err=%v, want denied with positive retry", allowed, retryAfter, err)
	}
	if allowed, _, err := storeB.Consume(context.Background(), "192.0.2.11"); err != nil || !allowed {
		t.Fatalf("independent client request: allowed=%t err=%v, want allowed", allowed, err)
	}
}

func TestRedisStoreCanonicalizesClientIPBeforeHashing(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 0.000001
	config.Burst = 1
	store, _ := newRedisTestStore(t, config)
	if allowed, _, err := store.Consume(context.Background(), "192.0.2.12"); err != nil || !allowed {
		t.Fatalf("IPv4 request: allowed=%t err=%v, want allowed", allowed, err)
	}
	if allowed, _, err := store.Consume(context.Background(), "::ffff:192.0.2.12"); err != nil || allowed {
		t.Fatalf("IPv4-mapped IPv6 request: allowed=%t err=%v, want same exhausted bucket", allowed, err)
	}
}

func TestRedisStoreRefillsAndRoundsRetryAfter(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 20
	config.Burst = 1
	store, _ := newRedisTestStore(t, config)

	if allowed, _, err := store.Consume(context.Background(), "192.0.2.20"); err != nil || !allowed {
		t.Fatalf("initial request: allowed=%t err=%v, want allowed", allowed, err)
	}
	if allowed, retryAfter, err := store.Consume(context.Background(), "192.0.2.20"); err != nil || allowed || retryAfter != 1 {
		t.Fatalf("exhausted request: allowed=%t Retry-After=%d err=%v, want denied with Retry-After 1", allowed, retryAfter, err)
	}
	time.Sleep(150 * time.Millisecond)
	if allowed, _, err := store.Consume(context.Background(), "192.0.2.20"); err != nil || !allowed {
		t.Fatalf("request after refill: allowed=%t err=%v, want allowed", allowed, err)
	}

	config.Rate = 0.4
	slowStore, _ := newRedisTestStore(t, config)
	if allowed, _, err := slowStore.Consume(context.Background(), "192.0.2.21"); err != nil || !allowed {
		t.Fatalf("slow-bucket initial request: allowed=%t err=%v, want allowed", allowed, err)
	}
	if allowed, retryAfter, err := slowStore.Consume(context.Background(), "192.0.2.21"); err != nil || allowed || retryAfter != 3 {
		t.Fatalf("slow-bucket exhausted request: allowed=%t Retry-After=%d err=%v, want denied with Retry-After 3", allowed, retryAfter, err)
	}
}

func TestRedisStoreConcurrentRequestsDoNotExceedBurst(t *testing.T) {
	config := DefaultConfig()
	config.Rate = 0.000001
	config.Burst = 20
	store, _ := newRedisTestStore(t, config)

	var accepted atomic.Int32
	var workers sync.WaitGroup
	for i := 0; i < 100; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			allowed, _, err := store.Consume(context.Background(), "198.51.100.40")
			if err != nil {
				t.Errorf("Redis token-bucket request: %v", err)
				return
			}
			if allowed {
				accepted.Add(1)
			}
		}()
	}
	workers.Wait()
	if got := accepted.Load(); got != 20 {
		t.Fatalf("accepted concurrent requests = %d, want exactly burst capacity 20", got)
	}
}

func TestRedisStoreAppliesAndExpiresIdleTTL(t *testing.T) {
	config := DefaultConfig()
	config.IdleTimeout = 150 * time.Millisecond
	store, _ := newRedisTestStore(t, config)
	ip := "203.0.113.50"
	if allowed, _, err := store.Consume(context.Background(), ip); err != nil || !allowed {
		t.Fatalf("initial request: allowed=%t err=%v, want allowed", allowed, err)
	}

	key := store.key(ip)
	ttl, err := store.client.PTTL(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if ttl <= 0 || ttl > config.IdleTimeout {
		t.Fatalf("key TTL = %s, want a positive value no greater than %s", ttl, config.IdleTimeout)
	}

	time.Sleep(config.IdleTimeout + 100*time.Millisecond)
	exists, err := store.client.Exists(context.Background(), key).Result()
	if err != nil {
		t.Fatal(err)
	}
	if exists != 0 {
		t.Fatalf("expired bucket key still exists (Exists=%d)", exists)
	}
}

func TestRedisStoreUsesHashedNamespacedKeys(t *testing.T) {
	config := DefaultConfig()
	store, _ := newRedisTestStore(t, config)
	key := store.key("2001:db8::1")
	if key == "2001:db8::1" || len(key) <= len(defaultRedisNamespace) {
		t.Fatalf("Redis key %q does not include a namespace and hashed client identifier", key)
	}
	if key[:len(defaultRedisNamespace)] != defaultRedisNamespace {
		t.Fatalf("Redis key %q does not use the IntelliGate namespace", key)
	}
	digest := key[len(key)-64:]
	if _, err := hex.DecodeString(digest); err != nil {
		t.Fatalf("Redis key suffix %q is not a SHA-256 digest", digest)
	}
}

func TestRedisStoreHonorsCancellationAndOperationTimeout(t *testing.T) {
	config := DefaultConfig()
	store, _ := newRedisTestStore(t, config)
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := store.Consume(cancelled, "192.0.2.30"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled Redis operation error = %v, want context.Canceled", err)
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serverDone := make(chan struct{})
	defer close(serverDone)
	defer listener.Close()
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			return
		}
		defer conn.Close()
		<-serverDone
	}()

	options := DefaultRedisOptions()
	options.Addr = listener.Addr().String()
	options.Namespace = "timeout-" + fmt.Sprintf("%x", time.Now().UnixNano())
	options.OperationTimeout = 100 * time.Millisecond
	client := redis.NewClient(&redis.Options{
		Addr:         options.Addr,
		DialTimeout:  options.OperationTimeout,
		ReadTimeout:  options.OperationTimeout,
		WriteTimeout: options.OperationTimeout,
		MaxRetries:   -1,
	})
	timeoutStore, err := newRedisStore(client, config, options)
	if err != nil {
		t.Fatal(err)
	}
	defer timeoutStore.Close()

	started := time.Now()
	if _, _, err := timeoutStore.Consume(context.Background(), "192.0.2.31"); err == nil {
		t.Fatal("Redis operation against unresponsive server succeeded")
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("Redis operation took %s, want bounded completion within one second", elapsed)
	}
}

func TestRedisStoreCloseClosesClient(t *testing.T) {
	store, _ := newRedisTestStore(t, DefaultConfig())
	client := store.client
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Ping(context.Background()).Err(); !errors.Is(err, redis.ErrClosed) {
		t.Fatalf("Ping after Close() error = %v, want redis.ErrClosed", err)
	}
}

func TestUnavailableRedisStoreUsesConfiguredFailureMode(t *testing.T) {
	store, _ := newRedisTestStore(t, DefaultConfig())
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	var downstreamCalls atomic.Int32
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	perform := func(mode FailureMode) *httptest.ResponseRecorder {
		config := DefaultConfig()
		config.FailureMode = mode
		limiter, err := NewWithStore(config, store)
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		request.RemoteAddr = "198.51.100.55:1234"
		recorder := httptest.NewRecorder()
		limiter.Middleware(downstream).ServeHTTP(recorder, request)
		return recorder
	}

	if response := perform(FailureModeClosed); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed-mode status with unavailable Redis = %d, want 503", response.Code)
	}
	if response := perform(FailureModeOpen); response.Code != http.StatusNoContent {
		t.Fatalf("open-mode status with unavailable Redis = %d, want 204", response.Code)
	}
	if downstreamCalls.Load() != 1 {
		t.Fatalf("downstream calls with unavailable Redis = %d, want 1 only in open mode", downstreamCalls.Load())
	}
}

func TestRedisFailureModesAndRecovery(t *testing.T) {
	config := DefaultConfig()
	config.FailureMode = FailureModeClosed
	closedLimiter, err := NewWithStore(config, &scriptedStore{err: errors.New("private backend detail")})
	if err != nil {
		t.Fatal(err)
	}
	var downstreamCalls atomic.Int32
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.40:1234"
	recorder := httptest.NewRecorder()
	closedLimiter.Middleware(downstream).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable || recorder.Code == http.StatusTooManyRequests {
		t.Fatalf("closed-mode status = %d, want 503 and not 429", recorder.Code)
	}
	if recorder.Body.String() != "{\"error\":\"rate limiter unavailable\"}\n" {
		t.Fatalf("closed-mode body = %q, want generic rate-limit-store error", recorder.Body.String())
	}
	if downstreamCalls.Load() != 0 {
		t.Fatalf("downstream calls in closed mode = %d, want 0", downstreamCalls.Load())
	}
	if strings.Contains(recorder.Body.String(), "private backend detail") {
		t.Fatal("closed-mode response exposed store error details")
	}

	config.FailureMode = FailureModeOpen
	openLimiter, err := NewWithStore(config, &scriptedStore{err: errors.New("private backend detail")})
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.41:1234"
	openLimiter.Middleware(downstream).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent || downstreamCalls.Load() != 1 {
		t.Fatalf("open-mode status=%d downstream calls=%d, want 204 and one call", recorder.Code, downstreamCalls.Load())
	}

	config.FailureMode = FailureModeClosed
	config.Burst = 1
	memoryStore, err := NewMemoryStore(config)
	if err != nil {
		t.Fatal(err)
	}
	recovering := &recoveringStore{delegate: memoryStore, unavailable: true}
	recoveredLimiter, err := NewWithStore(config, recovering)
	if err != nil {
		t.Fatal(err)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.42:1234"
	recoveredLimiter.Middleware(downstream).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable status before recovery = %d, want 503", recorder.Code)
	}
	recovering.setUnavailable(false)
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.42:1234"
	recoveredLimiter.Middleware(downstream).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusNoContent {
		t.Fatalf("status after store recovery = %d, want 204", recorder.Code)
	}
	recorder = httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.42:1234"
	recoveredLimiter.Middleware(downstream).ServeHTTP(recorder, request)
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status after recovery consumed burst = %d, want 429", recorder.Code)
	}
}

func TestRedisMiddlewareRestoresQuotaEnforcementAfterRecovery(t *testing.T) {
	config := DefaultConfig()
	config.Burst = 1
	store, _ := newRedisTestStore(t, config)
	recovering := &recoveringStore{delegate: store, unavailable: true}
	limiter, err := NewWithStore(config, recovering)
	if err != nil {
		t.Fatal(err)
	}
	downstream := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	perform := func() *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		request.RemoteAddr = "203.0.113.61:1234"
		recorder := httptest.NewRecorder()
		limiter.Middleware(downstream).ServeHTTP(recorder, request)
		return recorder
	}

	if response := perform(); response.Code != http.StatusServiceUnavailable {
		t.Fatalf("unavailable Redis status = %d, want 503", response.Code)
	}
	recovering.setUnavailable(false)
	if response := perform(); response.Code != http.StatusNoContent {
		t.Fatalf("first recovered Redis request status = %d, want 204", response.Code)
	}
	if response := perform(); response.Code != http.StatusTooManyRequests {
		t.Fatalf("second recovered Redis request status = %d, want 429 after burst is consumed", response.Code)
	}
}

func TestRedisStoreRejectsUnsupportedFixedPointValues(t *testing.T) {
	if _, err := redisFixedUnits(0.0000001); err == nil {
		t.Fatal("sub-microtoken rate was accepted by Redis fixed-point store")
	}
	if _, err := redisFixedUnits(1001); err == nil {
		t.Fatal("burst above fixed-point range was accepted")
	}
}

func TestValidateRedisAddress(t *testing.T) {
	for _, address := range []string{"localhost:6379", "127.0.0.1:6379", "[::1]:6379"} {
		if err := ValidateRedisAddress(address); err != nil {
			t.Errorf("ValidateRedisAddress(%q) error = %v, want nil", address, err)
		}
	}
	for _, address := range []string{"localhost", "localhost:0", "localhost:65536", "localhost:+6379", "bad host:6379"} {
		if err := ValidateRedisAddress(address); err == nil {
			t.Errorf("ValidateRedisAddress(%q) error = nil, want validation error", address)
		}
	}
}

type scriptedStore struct {
	err error
}

func (s *scriptedStore) Consume(context.Context, string) (bool, int64, error) {
	return false, 0, s.err
}

func (s *scriptedStore) Close() error {
	return nil
}

type recoveringStore struct {
	mu          sync.Mutex
	delegate    Store
	unavailable bool
}

func (s *recoveringStore) Consume(ctx context.Context, clientIP string) (bool, int64, error) {
	s.mu.Lock()
	unavailable := s.unavailable
	s.mu.Unlock()
	if unavailable {
		return false, 0, errors.New("temporarily unavailable")
	}
	return s.delegate.Consume(ctx, clientIP)
}

func (s *recoveringStore) setUnavailable(unavailable bool) {
	s.mu.Lock()
	s.unavailable = unavailable
	s.mu.Unlock()
}

func (s *recoveringStore) Close() error {
	return s.delegate.Close()
}

func newRedisTestStore(t *testing.T, config Config) (*RedisStore, RedisOptions) {
	t.Helper()
	options := DefaultRedisOptions()
	if address := os.Getenv("INTELLIGATE_TEST_REDIS_ADDR"); address != "" {
		options.Addr = address
	}
	options.Namespace = fmt.Sprintf("test-%x-%x", time.Now().UnixNano(), redisTestNamespaceCounter.Add(1))
	options.OperationTimeout = 5 * time.Second
	options.StartupTimeout = 500 * time.Millisecond
	store, err := NewRedisStore(config, options)
	if err != nil {
		t.Skipf("Redis integration test skipped because Redis is unavailable: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := store.client.Ping(ctx).Err(); errors.Is(err, redis.ErrClosed) {
			return
		}
		var cursor uint64
		for {
			keys, next, err := store.client.Scan(ctx, cursor, store.namespace+":*", 100).Result()
			if err != nil {
				t.Errorf("scan isolated Redis test keys: %v", err)
				break
			}
			if len(keys) > 0 {
				if err := store.client.Del(ctx, keys...).Err(); err != nil {
					t.Errorf("delete isolated Redis test keys: %v", err)
					break
				}
			}
			cursor = next
			if cursor == 0 {
				break
			}
		}
		if err := store.Close(); err != nil {
			t.Errorf("close Redis test store: %v", err)
		}
	})
	return store, options
}
