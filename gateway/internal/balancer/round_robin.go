package balancer

import (
	"errors"
	"net/http"
	"sync"
)

type RoundRobin struct {
	backends []http.Handler
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
	return &RoundRobin{backends: append([]http.Handler(nil), backends...)}, nil
}

func (r *RoundRobin) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	backend := r.backends[r.next]
	r.next = (r.next + 1) % len(r.backends)
	r.mu.Unlock()

	backend.ServeHTTP(w, req)
}
