package balancer

import (
	"context"
	"errors"
	"math"
	"net/http"
	"sync"
	"time"

	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/proxy"
)

type RetryPolicy struct {
	MaxRetries      int
	Backoffs        []time.Duration
	UpstreamTimeout time.Duration
}

func DefaultRetryPolicy(upstreamTimeout time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxRetries:      2,
		Backoffs:        []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
		UpstreamTimeout: upstreamTimeout,
	}
}

type RoundRobin struct {
	backends []http.Handler
	healthy  []bool
	breakers []*circuitbreaker.Breaker
	mu       sync.Mutex
	next     int
	retries  RetryPolicy
	wait     func(context.Context, time.Duration) bool
}

func NewRoundRobin(backends ...http.Handler) (*RoundRobin, error) {
	return NewRoundRobinWithRetryPolicy(DefaultRetryPolicy(proxy.DefaultUpstreamTimeout), backends...)
}

func NewRoundRobinWithRetryPolicy(policy RetryPolicy, backends ...http.Handler) (*RoundRobin, error) {
	if len(backends) == 0 {
		return nil, errors.New("round robin requires at least one backend")
	}
	for _, backend := range backends {
		if backend == nil {
			return nil, errors.New("round robin backend cannot be nil")
		}
	}
	if err := validateRetryPolicy(policy); err != nil {
		return nil, err
	}
	healthy := make([]bool, len(backends))
	for i := range healthy {
		healthy[i] = true
	}
	return &RoundRobin{
		backends: append([]http.Handler(nil), backends...),
		healthy:  healthy,
		breakers: make([]*circuitbreaker.Breaker, len(backends)),
		retries:  copyRetryPolicy(policy),
		wait:     waitForBackoff,
	}, nil
}

func validateRetryPolicy(policy RetryPolicy) error {
	if policy.MaxRetries < 0 {
		return errors.New("retry maximum must not be negative")
	}
	if policy.UpstreamTimeout <= 0 {
		return errors.New("retry upstream timeout must be positive")
	}
	if len(policy.Backoffs) != policy.MaxRetries {
		return errors.New("retry backoff count must match maximum retries")
	}
	attempts := int64(policy.MaxRetries) + 1
	if policy.UpstreamTimeout > time.Duration(math.MaxInt64/attempts) {
		return errors.New("retry budget exceeds maximum duration")
	}
	budget := policy.UpstreamTimeout * time.Duration(attempts)
	for _, backoff := range policy.Backoffs {
		if backoff < 0 {
			return errors.New("retry backoff must not be negative")
		}
		if backoff > time.Duration(math.MaxInt64)-budget {
			return errors.New("retry budget exceeds maximum duration")
		}
		budget += backoff
	}
	return nil
}

func copyRetryPolicy(policy RetryPolicy) RetryPolicy {
	return RetryPolicy{
		MaxRetries:      policy.MaxRetries,
		Backoffs:        append([]time.Duration(nil), policy.Backoffs...),
		UpstreamTimeout: policy.UpstreamTimeout,
	}
}

func (p RetryPolicy) totalBudget() time.Duration {
	budget := p.UpstreamTimeout * time.Duration(p.MaxRetries+1)
	for _, backoff := range p.Backoffs {
		budget += backoff
	}
	return budget
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
	retryContext, cancelRetryBudget := proxy.WithRetryBudget(req.Context(), r.retries.totalBudget())
	defer cancelRetryBudget()

	attempted := make([]bool, len(r.backends))
	var lastError *proxy.AttemptError
	for attempt := 0; attempt <= r.retries.MaxRetries; attempt++ {
		if proxy.IsClientContextDone(retryContext) {
			return
		}
		if proxy.IsRetryBudgetExceeded(retryContext) {
			writeRetryBudgetTimeout(w)
			return
		}

		attemptRequest, err := requestForAttempt(req, retryContext, attempt)
		if err != nil {
			if lastError != nil {
				lastError.WriteHTTPError(w)
			}
			return
		}

		backend, index, permit, anyHealthy, breakerRejected := r.selectBackend(attempted)
		if backend == nil {
			if lastError != nil {
				lastError.WriteHTTPError(w)
			} else if anyHealthy && breakerRejected {
				http.Error(w, "no backends available: circuit breaker open or half-open trial limit reached", http.StatusServiceUnavailable)
			} else {
				http.Error(w, "no healthy backends available", http.StatusServiceUnavailable)
			}
			return
		}
		attempted[index] = true

		if permit != nil {
			attemptRequest = attemptRequest.WithContext(circuitbreaker.WithPermit(attemptRequest.Context(), permit))
		}

		attemptError := serveAttempt(backend, w, attemptRequest, permit)
		if proxy.IsClientContextDone(retryContext) {
			return
		}
		if attemptError == nil {
			return
		}
		lastError = attemptError
		if proxy.IsRetryBudgetExceeded(retryContext) {
			writeRetryBudgetTimeout(w)
			return
		}
		if attempt == r.retries.MaxRetries || !retryableMethod(req.Method) || !requestBodyReplayable(req) {
			lastError.WriteHTTPError(w)
			return
		}
		if !r.wait(retryContext, r.retries.Backoffs[attempt]) {
			if proxy.IsClientContextDone(retryContext) {
				return
			}
			if proxy.IsRetryBudgetExceeded(retryContext) {
				writeRetryBudgetTimeout(w)
				return
			}
			lastError.WriteHTTPError(w)
			return
		}
	}
}

func (r *RoundRobin) selectBackend(attempted []bool) (http.Handler, int, *circuitbreaker.Permit, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	anyHealthy := false
	breakerRejected := false
	for pass := 0; pass < 2; pass++ {
		for offset := 0; offset < len(r.backends); offset++ {
			index := (r.next + offset) % len(r.backends)
			if pass == 0 && attempted[index] || pass == 1 && !attempted[index] {
				continue
			}
			if !r.healthy[index] {
				continue
			}
			anyHealthy = true
			var permit *circuitbreaker.Permit
			if r.breakers[index] != nil {
				var allowed bool
				permit, allowed = r.breakers[index].TryAcquire()
				if !allowed {
					breakerRejected = true
					continue
				}
			}
			r.next = (index + 1) % len(r.backends)
			return r.backends[index], index, permit, anyHealthy, breakerRejected
		}
	}
	return nil, -1, nil, anyHealthy, breakerRejected
}

type attemptHandler interface {
	ServeAttempt(http.ResponseWriter, *http.Request) *proxy.AttemptError
}

func serveAttempt(backend http.Handler, w http.ResponseWriter, req *http.Request, permit *circuitbreaker.Permit) *proxy.AttemptError {
	if permit != nil {
		defer permit.Abandon()
	}
	if retryable, ok := backend.(attemptHandler); ok {
		return retryable.ServeAttempt(w, req)
	}
	backend.ServeHTTP(w, req)
	return nil
}

func requestForAttempt(req *http.Request, ctx context.Context, attempt int) (*http.Request, error) {
	cloned := req.Clone(ctx)
	if attempt == 0 || req.Body == nil || req.Body == http.NoBody {
		return cloned, nil
	}
	if req.GetBody == nil {
		return nil, errors.New("request body cannot be replayed")
	}
	body, err := req.GetBody()
	if err != nil {
		return nil, err
	}
	if body == nil {
		return nil, errors.New("request GetBody returned a nil body")
	}
	cloned.Body = body
	return cloned, nil
}

func requestBodyReplayable(req *http.Request) bool {
	return req.Body == nil || req.Body == http.NoBody || req.GetBody != nil
}

func retryableMethod(method string) bool {
	return method == http.MethodGet || method == http.MethodHead
}

func waitForBackoff(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		select {
		case <-ctx.Done():
			return false
		default:
			return true
		}
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

func writeRetryBudgetTimeout(w http.ResponseWriter) {
	http.Error(w, "upstream request timed out", http.StatusGatewayTimeout)
}
