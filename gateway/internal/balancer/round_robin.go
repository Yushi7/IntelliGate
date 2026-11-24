package balancer

import (
	"errors"
	"net/http"
	"sync"
)

type RoundRobin struct {
	backends []http.Handler
	healthy  []bool
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

func (r *RoundRobin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	var backend http.Handler
	for offset := 0; offset < len(r.backends); offset++ {
		index := (r.next + offset) % len(r.backends)
		if r.healthy[index] {
			backend = r.backends[index]
			r.next = (index + 1) % len(r.backends)
			break
		}
	}
	r.mu.Unlock()

	if backend == nil {
		http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
		return
	}
	backend.ServeHTTP(w, req)
}
