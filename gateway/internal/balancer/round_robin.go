package balancer

import (
	"errors"
	"net/http"
	"sync"

	"intelligate/gateway/internal/circuitbreaker"
)

type RoundRobin struct {
	backends []http.Handler
	healthy  []bool
	breakers []*circuitbreaker.Breaker
	mu       sync.Mutex
	next     int
}

func NewRoundRobin(backends ...http.Handler) (*RoundRobin, error) {
	if len(backends) == 0 {
		return nil, errors.New("round robin requires at least one backend")
	}
	for _, backend := range backends {
		if backend == nil {
			return nil, errors.New("round robin backend cannot be nil")
		}
	}
	healthy := make([]bool, len(backends))
	for i := range healthy {
		healthy[i] = true
	}
	return &RoundRobin{
		backends: append([]http.Handler(nil), backends...),
		healthy:  healthy,
		breakers: make([]*circuitbreaker.Breaker, len(backends)),
	}, nil
}

func (r *RoundRobin) SetHealthy(index int, healthy bool) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if index < 0 || index >= len(r.healthy) {
		return errors.New("backend health index out of range")
	}
	r.healthy[index] = healthy
	return nil
}

func (r *RoundRobin) SetCircuitBreaker(index int, breaker *circuitbreaker.Breaker) error {
	r.mu.Lock()
	defer r.mu.Unlock()

	if index < 0 || index >= len(r.breakers) {
		return errors.New("backend circuit-breaker index out of range")
	}
	r.breakers[index] = breaker
	return nil
}

func (r *RoundRobin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	var backend http.Handler
	var permit *circuitbreaker.Permit
	anyHealthy := false
	breakerRejected := false
	for offset := 0; offset < len(r.backends); offset++ {
		index := (r.next + offset) % len(r.backends)
		if !r.healthy[index] {
			continue
		}
		anyHealthy = true
		if r.breakers[index] != nil {
			var allowed bool
			permit, allowed = r.breakers[index].TryAcquire()
			if !allowed {
				breakerRejected = true
				continue
			}
		}
		backend = r.backends[index]
		r.next = (index + 1) % len(r.backends)
		break
	}
	r.mu.Unlock()

	if backend == nil {
		if anyHealthy && breakerRejected {
			http.Error(w, "no backends available: circuit breaker open or half-open trial limit reached", http.StatusServiceUnavailable)
			return
		}
		http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
		return
	}
	if permit != nil {
		defer permit.Abandon()
		req = req.WithContext(circuitbreaker.WithPermit(req.Context(), permit))
	}
	backend.ServeHTTP(w, req)
}
