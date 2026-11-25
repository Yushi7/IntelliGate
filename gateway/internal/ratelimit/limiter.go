package ratelimit

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	defaultRate            = 10.0
	defaultBurst           = 20.0
	defaultIdleTimeout     = 10 * time.Minute
	defaultCleanupInterval = time.Minute
)

type FailureMode string

const (
	FailureModeClosed FailureMode = "closed"
	FailureModeOpen   FailureMode = "open"
)

type Config struct {
	Rate            float64
	Burst           float64
	IdleTimeout     time.Duration
	CleanupInterval time.Duration
	FailureMode     FailureMode
}

func DefaultConfig() Config {
	return Config{
		Rate:            defaultRate,
		Burst:           defaultBurst,
		IdleTimeout:     defaultIdleTimeout,
		CleanupInterval: defaultCleanupInterval,
		FailureMode:     FailureModeClosed,
	}
}

type Store interface {
	Consume(ctx context.Context, clientIP string) (allowed bool, retryAfterSeconds int64, err error)
	Close() error
}

type cleanupStore interface {
	runCleanup(ctx context.Context)
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

type MemoryStore struct {
	mu      sync.Mutex
	config  Config
	clients map[string]*bucket
	now     func() time.Time
}

type Limiter struct {
	config      Config
	store       Store
	memoryStore *MemoryStore
	onRejection func(reason string)
}

func (l *Limiter) SetRejectionObserver(observer func(reason string)) {
	l.onRejection = observer
}

func New(config Config) (*Limiter, error) {
	return newWithClock(config, time.Now)
}

func NewMemoryStore(config Config) (*MemoryStore, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return newMemoryStore(normalized, time.Now), nil
}

func NewWithStore(config Config, store Store) (*Limiter, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	if store == nil {
		return nil, fmt.Errorf("rate-limit store must not be nil")
	}
	return newLimiter(normalized, store), nil
}

func newWithClock(config Config, now func() time.Time) (*Limiter, error) {
	normalized, err := normalizeConfig(config)
	if err != nil {
		return nil, err
	}
	return newLimiter(normalized, newMemoryStore(normalized, now)), nil
}

func normalizeConfig(config Config) (Config, error) {
	if config.FailureMode == "" {
		config.FailureMode = FailureModeClosed
	}
	switch config.FailureMode {
	case FailureModeClosed, FailureModeOpen:
	default:
		return Config{}, fmt.Errorf("rate-limit failure mode must be %q or %q", FailureModeClosed, FailureModeOpen)
	}
	if math.IsNaN(config.Rate) || math.IsInf(config.Rate, 0) || config.Rate <= 0 {
		return Config{}, fmt.Errorf("rate-limit rate must be a finite positive number")
	}
	if math.IsNaN(config.Burst) || math.IsInf(config.Burst, 0) || config.Burst <= 0 {
		return Config{}, fmt.Errorf("rate-limit burst must be a finite positive number")
	}
	if config.IdleTimeout <= 0 {
		return Config{}, fmt.Errorf("rate-limit idle timeout must be greater than zero")
	}
	if config.CleanupInterval <= 0 {
		return Config{}, fmt.Errorf("rate-limit cleanup period must be greater than zero")
	}
	return config, nil
}

func newLimiter(config Config, store Store) *Limiter {
	limiter := &Limiter{config: config, store: store}
	if memoryStore, ok := store.(*MemoryStore); ok {
		limiter.memoryStore = memoryStore
	}
	return limiter
}

func newMemoryStore(config Config, now func() time.Time) *MemoryStore {
	return &MemoryStore{
		config:  config,
		clients: make(map[string]*bucket),
		now:     now,
	}
}

// Allow preserves the existing two-result API; use AllowContext when store errors matter.
func (l *Limiter) Allow(clientIP string) (bool, int64) {
	allowed, retryAfter, err := l.AllowContext(context.Background(), clientIP)
	if err != nil {
		log.Printf("warning: rate-limit store failed in Allow; use AllowContext to handle store errors")
		return false, 0
	}
	return allowed, retryAfter
}

func (l *Limiter) AllowContext(ctx context.Context, clientIP string) (bool, int64, error) {
	return l.store.Consume(ctx, clientIP)
}

func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/health" {
			next.ServeHTTP(w, r)
			return
		}

		clientIP, err := ClientIPFromRemoteAddr(r.RemoteAddr)
		if err != nil {
			http.Error(w, "invalid client address", http.StatusBadRequest)
			return
		}

		allowed, retryAfter, err := l.AllowContext(r.Context(), clientIP)
		if err != nil {
			if r.Context().Err() != nil {
				return
			}
			l.observeRejection("store_unavailable")
			if l.config.FailureMode == FailureModeOpen {
				log.Printf("warning: rate-limit store unavailable; allowing request in open mode")
				next.ServeHTTP(w, r)
				return
			}
			log.Printf("warning: rate-limit store unavailable; rejecting request in closed mode")
			writeJSONError(w, http.StatusServiceUnavailable, "rate limiter unavailable")
			return
		}
		if !allowed {
			l.observeRejection("exhausted")
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}

		next.ServeHTTP(w, r)
	})
}

func (l *Limiter) observeRejection(reason string) {
	if l.onRejection == nil {
		return
	}
	defer func() {
		if recover() != nil {
			log.Printf("warning: rate-limit metrics observer panicked")
		}
	}()
	l.onRejection(reason)
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}

func ClientIPFromRemoteAddr(remoteAddr string) (string, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", fmt.Errorf("parse remote address: %w", err)
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", fmt.Errorf("parse remote IP: %w", err)
	}
	return address.Unmap().String(), nil
}

func (s *MemoryStore) Consume(ctx context.Context, clientIP string) (bool, int64, error) {
	if err := ctx.Err(); err != nil {
		return false, 0, err
	}
	now := s.now()

	s.mu.Lock()
	defer s.mu.Unlock()

	current, ok := s.clients[clientIP]
	if !ok {
		current = &bucket{tokens: s.config.Burst, lastRefill: now, lastSeen: now}
		s.clients[clientIP] = current
	}

	elapsed := now.Sub(current.lastRefill).Seconds()
	if elapsed > 0 {
		current.tokens = math.Min(s.config.Burst, current.tokens+elapsed*s.config.Rate)
		current.lastRefill = now
	}
	current.lastSeen = now

	if current.tokens >= 1 {
		current.tokens--
		return true, 0, nil
	}

	return false, max(1, int64(math.Ceil((1-current.tokens)/s.config.Rate))), nil
}

func (s *MemoryStore) Close() error {
	return nil
}

func (s *MemoryStore) CleanupExpired() int {
	return s.cleanupExpiredAt(s.now())
}

func (s *MemoryStore) cleanupExpiredAt(now time.Time) int {
	s.mu.Lock()
	defer s.mu.Unlock()

	removed := 0
	for clientIP, current := range s.clients {
		if now.Sub(current.lastSeen) >= s.config.IdleTimeout {
			delete(s.clients, clientIP)
			removed++
		}
	}
	return removed
}

func (s *MemoryStore) runCleanup(ctx context.Context) {
	ticker := time.NewTicker(s.config.CleanupInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case now := <-ticker.C:
			s.cleanupExpiredAt(now)
		}
	}
}

func (l *Limiter) CleanupExpired() int {
	if l.memoryStore == nil {
		return 0
	}
	return l.memoryStore.CleanupExpired()
}

func (l *Limiter) cleanupExpiredAt(now time.Time) int {
	if l.memoryStore == nil {
		return 0
	}
	return l.memoryStore.cleanupExpiredAt(now)
}

func (l *Limiter) RunCleanup(ctx context.Context) {
	if cleaner, ok := l.store.(cleanupStore); ok {
		cleaner.runCleanup(ctx)
	}
}

func (l *Limiter) NeedsCleanup() bool {
	_, ok := l.store.(cleanupStore)
	return ok
}

func (l *Limiter) Close() error {
	return l.store.Close()
}
