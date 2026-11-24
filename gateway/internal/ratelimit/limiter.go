package ratelimit

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"net"
	"net/http"
	"net/netip"
	"strconv"
	"sync"
	"time"
)

const (
	DefaultRate            = 10.0
	DefaultBurst           = 20.0
	DefaultIdleTimeout     = 10 * time.Minute
	DefaultCleanupInterval = time.Minute
)

type Config struct {
	Rate            float64
	Burst           float64
	IdleTimeout     time.Duration
	CleanupInterval time.Duration
}

func DefaultConfig() Config {
	return Config{
		Rate:            DefaultRate,
		Burst:           DefaultBurst,
		IdleTimeout:     DefaultIdleTimeout,
		CleanupInterval: DefaultCleanupInterval,
	}
}

type Limiter struct {
	mu      sync.Mutex
	config  Config
	clients map[string]*bucket
	now     func() time.Time
}

type bucket struct {
	tokens     float64
	lastRefill time.Time
	lastSeen   time.Time
}

func New(config Config) (*Limiter, error) {
	return newWithClock(config, time.Now)
}

func newWithClock(config Config, now func() time.Time) (*Limiter, error) {
	if math.IsNaN(config.Rate) || math.IsInf(config.Rate, 0) || config.Rate <= 0 {
		return nil, errors.New("rate-limit rate must be a finite positive number")
	}
	if math.IsNaN(config.Burst) || math.IsInf(config.Burst, 0) || config.Burst <= 0 {
		return nil, errors.New("rate-limit burst must be a finite positive number")
	}
	if config.IdleTimeout <= 0 {
		return nil, errors.New("rate-limit idle timeout must be positive")
	}
	if config.CleanupInterval <= 0 {
		return nil, errors.New("rate-limit cleanup interval must be positive")
	}
	if now == nil {
		return nil, errors.New("rate-limit clock cannot be nil")
	}
	return &Limiter{
		config:  config,
		clients: make(map[string]*bucket),
		now:     now,
	}, nil
}

func ClientIPFromRemoteAddr(remoteAddr string) (string, error) {
	host, _, err := net.SplitHostPort(remoteAddr)
	if err != nil {
		return "", errors.New("remote address must include a host and port")
	}
	address, err := netip.ParseAddr(host)
	if err != nil {
		return "", errors.New("remote address host is not a valid IP address")
	}
	return address.Unmap().String(), nil
}

func (l *Limiter) Allow(clientIP string) (bool, int64) {
	now := l.now()
	l.mu.Lock()
	defer l.mu.Unlock()

	client, ok := l.clients[clientIP]
	if !ok {
		client = &bucket{
			tokens:     l.config.Burst,
			lastRefill: now,
			lastSeen:   now,
		}
		l.clients[clientIP] = client
	}

	if now.After(client.lastRefill) {
		elapsed := now.Sub(client.lastRefill).Seconds()
		client.tokens = math.Min(l.config.Burst, client.tokens+elapsed*l.config.Rate)
		client.lastRefill = now
	}
	if now.After(client.lastSeen) {
		client.lastSeen = now
	}

	if client.tokens >= 1 {
		client.tokens--
		return true, 0
	}
	return false, retryAfterSeconds(1-client.tokens, l.config.Rate)
}

func retryAfterSeconds(tokensNeeded, rate float64) int64 {
	seconds := math.Ceil(tokensNeeded / rate)
	if seconds < 1 {
		return 1
	}
	if seconds >= float64(math.MaxInt64) {
		return math.MaxInt64
	}
	return int64(seconds)
}

func (l *Limiter) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if req.Method == http.MethodGet && req.URL.Path == "/health" {
			next.ServeHTTP(w, req)
			return
		}

		clientIP, err := ClientIPFromRemoteAddr(req.RemoteAddr)
		if err != nil {
			writeJSONError(w, http.StatusBadRequest, "invalid client address")
			return
		}
		allowed, retryAfter := l.Allow(clientIP)
		if !allowed {
			w.Header().Set("Retry-After", strconv.FormatInt(retryAfter, 10))
			writeJSONError(w, http.StatusTooManyRequests, "rate limit exceeded")
			return
		}
		next.ServeHTTP(w, req)
	})
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}

func (l *Limiter) CleanupExpired() int {
	return l.cleanupExpiredAt(l.now())
}

func (l *Limiter) cleanupExpiredAt(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()

	removed := 0
	for clientIP, client := range l.clients {
		if !now.Before(client.lastSeen) && now.Sub(client.lastSeen) >= l.config.IdleTimeout {
			delete(l.clients, clientIP)
			removed++
		}
	}
	return removed
}

func (l *Limiter) RunCleanup(ctx context.Context) {
	ticker := time.NewTicker(l.config.CleanupInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			l.CleanupExpired()
		}
	}
}
