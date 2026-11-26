package balancer

import (
	"context"
	"errors"
	"log"
	"math"
	"net/http"
	"sort"
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

type RoutingStrategy string

const (
	StrategyRoundRobin RoutingStrategy = "round_robin"
	StrategyAdaptive   RoutingStrategy = "adaptive"
)

type RoutingConfig struct {
	Strategy   RoutingStrategy
	EWMAAlpha  float64
	MinSamples uint64
}

func DefaultRoutingConfig() RoutingConfig {
	return RoutingConfig{
		Strategy:   StrategyRoundRobin,
		EWMAAlpha:  0.2,
		MinSamples: 5,
	}
}

func DefaultRetryPolicy(upstreamTimeout time.Duration) RetryPolicy {
	return RetryPolicy{
		MaxRetries:      2,
		Backoffs:        []time.Duration{100 * time.Millisecond, 200 * time.Millisecond},
		UpstreamTimeout: upstreamTimeout,
	}
}

type RoundRobin struct {
	backends    []http.Handler
	healthy     []bool
	breakers    []*circuitbreaker.Breaker
	performance []*backendPerformance
	mu          sync.Mutex
	next        int
	retries     RetryPolicy
	wait        func(context.Context, time.Duration) bool
	observer    Observer
	routing     RoutingConfig
	now         func() time.Time
}

type Observer interface {
	BackendSelected(index int, attempt string)
	BackendSelectionFailed(reason string)
	RetryAttempt(method string)
	RetryOutcome(outcome string)
}

type performanceObserver interface {
	BackendPerformance(index int, ewma time.Duration, inFlight uint64, sampleAdded bool, generation uint64)
}

type backendPerformance struct {
	mu                  sync.Mutex
	alpha               float64
	ewma                time.Duration
	inFlight            uint64
	samples             uint64
	generation          uint64
	consecutiveFailures uint32
	cooldownUntil       time.Time
}

type performanceSnapshot struct {
	ewma                time.Duration
	inFlight            uint64
	samples             uint64
	generation          uint64
	consecutiveFailures uint32
	cooldownUntil       time.Time
}

func NewRoundRobin(backends ...http.Handler) (*RoundRobin, error) {
	return NewRoundRobinWithRetryPolicy(DefaultRetryPolicy(proxy.DefaultUpstreamTimeout), backends...)
}

func NewRoundRobinWithRetryPolicy(policy RetryPolicy, backends ...http.Handler) (*RoundRobin, error) {
	return NewRoundRobinWithConfig(policy, DefaultRoutingConfig(), backends...)
}

func NewRoundRobinWithConfig(policy RetryPolicy, routing RoutingConfig, backends ...http.Handler) (*RoundRobin, error) {
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
	if err := validateRoutingConfig(routing); err != nil {
		return nil, err
	}
	healthy := make([]bool, len(backends))
	performance := make([]*backendPerformance, len(backends))
	for i := range healthy {
		healthy[i] = true
		performance[i] = &backendPerformance{alpha: routing.EWMAAlpha}
	}
	return &RoundRobin{
		backends:    append([]http.Handler(nil), backends...),
		healthy:     healthy,
		breakers:    make([]*circuitbreaker.Breaker, len(backends)),
		performance: performance,
		retries:     copyRetryPolicy(policy),
		wait:        waitForBackoff,
		routing:     routing,
		now:         time.Now,
	}, nil
}

func validateRoutingConfig(config RoutingConfig) error {
	if config.Strategy != StrategyRoundRobin && config.Strategy != StrategyAdaptive {
		return errors.New("routing strategy must be round_robin or adaptive")
	}
	if math.IsNaN(config.EWMAAlpha) || math.IsInf(config.EWMAAlpha, 0) || config.EWMAAlpha <= 0 || config.EWMAAlpha > 1 {
		return errors.New("adaptive EWMA alpha must be greater than 0 and at most 1")
	}
	if config.MinSamples == 0 {
		return errors.New("adaptive minimum samples must be positive")
	}
	return nil
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

func (r *RoundRobin) SetObserver(observer Observer) {
	r.observer = observer
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
			reason := "unhealthy"
			if anyHealthy && breakerRejected {
				reason = "breaker"
			}
			r.observeSelectionFailure(reason)
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
		attemptKind := "initial"
		if attempt > 0 {
			attemptKind = "retry"
			r.observeRetryAttempt(req.Method)
		}
		r.observeBackendSelected(index, attemptKind)

		if permit != nil {
			attemptRequest = attemptRequest.WithContext(circuitbreaker.WithPermit(attemptRequest.Context(), permit))
		}

		attemptError := r.serveMeasuredAttempt(index, backend, w, attemptRequest, permit)
		if proxy.IsClientContextDone(retryContext) {
			if attempt > 0 {
				r.observeRetryOutcome("cancelled")
			}
			return
		}
		if attemptError == nil {
			if attempt > 0 {
				r.observeRetryOutcome("response")
			}
			return
		}
		lastError = attemptError
		if attempt > 0 {
			outcome := "transport_error"
			if attempt == r.retries.MaxRetries {
				outcome = "exhausted"
			}
			r.observeRetryOutcome(outcome)
		}
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

func (r *RoundRobin) observeBackendSelected(index int, attempt string) {
	r.observe(func(observer Observer) { observer.BackendSelected(index, attempt) })
}

func (r *RoundRobin) observeSelectionFailure(reason string) {
	r.observe(func(observer Observer) { observer.BackendSelectionFailed(reason) })
}

func (r *RoundRobin) observeRetryAttempt(method string) {
	r.observe(func(observer Observer) { observer.RetryAttempt(method) })
}

func (r *RoundRobin) observeRetryOutcome(outcome string) {
	r.observe(func(observer Observer) { observer.RetryOutcome(outcome) })
}

func (r *RoundRobin) observe(event func(Observer)) {
	if r.observer == nil {
		return
	}
	defer func() {
		if recover() != nil {
			log.Printf("warning: load-balancer metrics observer panicked")
		}
	}()
	event(r.observer)
}

func (r *RoundRobin) selectBackend(attempted []bool) (http.Handler, int, *circuitbreaker.Permit, bool, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.routing.Strategy == StrategyAdaptive {
		return r.selectAdaptiveBackend(attempted)
	}
	return r.selectRoundRobinBackend(attempted)
}

func (r *RoundRobin) selectRoundRobinBackend(attempted []bool) (http.Handler, int, *circuitbreaker.Permit, bool, bool) {
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

func (r *RoundRobin) selectAdaptiveBackend(attempted []bool) (http.Handler, int, *circuitbreaker.Permit, bool, bool) {
	anyHealthy := false
	breakerRejected := false
	for pass := 0; pass < 2; pass++ {
		candidates := make([]adaptiveCandidate, 0, len(r.backends))
		exploration := false
		for offset := 0; offset < len(r.backends); offset++ {
			index := (r.next + offset) % len(r.backends)
			if pass == 0 && attempted[index] || pass == 1 && !attempted[index] || !r.healthy[index] {
				continue
			}
			anyHealthy = true
			snapshot := r.performance[index].snapshot()
			if snapshot.samples < r.routing.MinSamples {
				exploration = true
			}
			candidates = append(candidates, adaptiveCandidate{
				index:    index,
				snapshot: snapshot,
				distance: offset,
			})
		}
		now := r.now()
		sort.Slice(candidates, func(i, j int) bool {
			left, right := candidates[i], candidates[j]
			leftCooling := left.snapshot.cooldownUntil.After(now)
			rightCooling := right.snapshot.cooldownUntil.After(now)
			if leftCooling != rightCooling {
				return !leftCooling
			}
			if leftCooling && !left.snapshot.cooldownUntil.Equal(right.snapshot.cooldownUntil) {
				return left.snapshot.cooldownUntil.Before(right.snapshot.cooldownUntil)
			}
			if exploration && left.snapshot.samples != right.snapshot.samples {
				return left.snapshot.samples < right.snapshot.samples
			}
			if !exploration {
				leftScore := adaptiveScore(left.snapshot)
				rightScore := adaptiveScore(right.snapshot)
				if leftScore != rightScore {
					return leftScore < rightScore
				}
			}
			return left.distance < right.distance
		})
		for _, candidate := range candidates {
			var permit *circuitbreaker.Permit
			if breaker := r.breakers[candidate.index]; breaker != nil {
				var allowed bool
				permit, allowed = breaker.TryAcquire()
				if !allowed {
					breakerRejected = true
					continue
				}
			}
			r.next = (candidate.index + 1) % len(r.backends)
			return r.backends[candidate.index], candidate.index, permit, anyHealthy, breakerRejected
		}
	}
	return nil, -1, nil, anyHealthy, breakerRejected
}

type adaptiveCandidate struct {
	index    int
	snapshot performanceSnapshot
	distance int
}

func adaptiveScore(snapshot performanceSnapshot) float64 {
	return float64(snapshot.ewma) * (1 + float64(snapshot.inFlight))
}

func (r *RoundRobin) serveMeasuredAttempt(index int, backend http.Handler, w http.ResponseWriter, req *http.Request, permit *circuitbreaker.Permit) (attemptError *proxy.AttemptError) {
	performance := r.performance[index]
	snapshot, counted := performance.begin()
	r.observePerformance(index, snapshot, false)
	completed := false
	upstreamSuccessful := false
	transportFailed := false
	var ttfb time.Duration
	ttfbAvailable := false
	defer func() {
		sampleAdded := completed && upstreamSuccessful && ttfbAvailable && attemptError == nil && !proxy.IsClientContextDone(req.Context())
		snapshot := performance.finish(ttfb, sampleAdded, counted, transportFailed, r.now())
		r.observePerformance(index, snapshot, sampleAdded)
	}()
	attemptError, upstreamSuccessful, ttfb, ttfbAvailable, transportFailed = serveAttemptResultMeasured(backend, w, req, permit)
	if attemptError != nil && !proxy.IsClientContextDone(req.Context()) {
		transportFailed = true
	}
	completed = true
	return attemptError
}

func (r *RoundRobin) observePerformance(index int, snapshot performanceSnapshot, sampleAdded bool) {
	observer, ok := r.observer.(performanceObserver)
	if !ok {
		return
	}
	defer func() {
		if recover() != nil {
			log.Printf("warning: backend performance metrics observer panicked")
		}
	}()
	observer.BackendPerformance(index, snapshot.ewma, snapshot.inFlight, sampleAdded, snapshot.generation)
}

func (p *backendPerformance) begin() (performanceSnapshot, bool) {
	p.mu.Lock()
	counted := p.inFlight < ^uint64(0)
	if counted {
		p.inFlight++
	}
	p.advanceGeneration()
	snapshot := p.snapshotLocked()
	p.mu.Unlock()
	return snapshot, counted
}

func cooldownForFailure(consecutive uint32) time.Duration {
	if consecutive == 0 {
		return 0
	}
	cooldown := 5 * time.Second
	for step := uint32(1); step < consecutive && cooldown < 30*time.Second; step++ {
		if cooldown > 15*time.Second {
			return 30 * time.Second
		}
		cooldown *= 2
	}
	if cooldown > 30*time.Second {
		return 30 * time.Second
	}
	return cooldown
}

func (p *backendPerformance) finish(duration time.Duration, sampleAdded, counted, failed bool, now time.Time) performanceSnapshot {
	p.mu.Lock()
	if counted && p.inFlight > 0 {
		p.inFlight--
	}
	if sampleAdded {
		p.consecutiveFailures = 0
		p.cooldownUntil = time.Time{}
		if p.samples < ^uint64(0) {
			if p.samples == 0 {
				p.ewma = duration
			} else {
				p.ewma += time.Duration(float64(duration-p.ewma) * p.alpha)
			}
			p.samples++
		}
	} else if failed {
		if p.consecutiveFailures < 32 {
			p.consecutiveFailures++
		}
		p.cooldownUntil = now.Add(cooldownForFailure(p.consecutiveFailures))
	}
	p.advanceGeneration()
	snapshot := p.snapshotLocked()
	p.mu.Unlock()
	return snapshot
}

func (p *backendPerformance) snapshot() performanceSnapshot {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.snapshotLocked()
}

func (p *backendPerformance) snapshotLocked() performanceSnapshot {
	return performanceSnapshot{
		ewma:                p.ewma,
		inFlight:            p.inFlight,
		samples:             p.samples,
		generation:          p.generation,
		consecutiveFailures: p.consecutiveFailures,
		cooldownUntil:       p.cooldownUntil,
	}
}

func (p *backendPerformance) advanceGeneration() {
	if p.generation < ^uint64(0) {
		p.generation++
	}
}

type attemptHandler interface {
	ServeAttempt(http.ResponseWriter, *http.Request) *proxy.AttemptError
}

type attemptResultHandler interface {
	ServeAttemptResult(http.ResponseWriter, *http.Request) (*proxy.AttemptError, bool)
}

type measuredAttemptHandler interface {
	ServeMeasuredAttemptResult(http.ResponseWriter, *http.Request) proxy.MeasuredAttemptResult
}

func serveAttemptResultMeasured(backend http.Handler, w http.ResponseWriter, req *http.Request, permit *circuitbreaker.Permit) (*proxy.AttemptError, bool, time.Duration, bool, bool) {
	if permit != nil {
		defer permit.Abandon()
	}
	if measured, ok := backend.(measuredAttemptHandler); ok {
		result := measured.ServeMeasuredAttemptResult(w, req)
		return result.Err, result.BodyCompleted, result.TTFB, result.TTFBAvailable, result.TransportFailure
	}
	// Custom handlers have no upstream-header observation point; never treat handler duration as TTFB.
	if measured, ok := backend.(attemptResultHandler); ok {
		err, complete := measured.ServeAttemptResult(w, req)
		return err, complete, 0, false, err != nil
	}
	if retryable, ok := backend.(attemptHandler); ok {
		err := retryable.ServeAttempt(w, req)
		return err, err == nil, 0, false, err != nil
	}
	backend.ServeHTTP(w, req)
	return nil, true, 0, false, false
}

func serveAttempt(backend http.Handler, w http.ResponseWriter, req *http.Request, permit *circuitbreaker.Permit) *proxy.AttemptError {
	attemptError, _ := serveAttemptResult(backend, w, req, permit)
	return attemptError
}

func serveAttemptResult(backend http.Handler, w http.ResponseWriter, req *http.Request, permit *circuitbreaker.Permit) (*proxy.AttemptError, bool) {
	if permit != nil {
		defer permit.Abandon()
	}
	if measured, ok := backend.(attemptResultHandler); ok {
		return measured.ServeAttemptResult(w, req)
	}
	if retryable, ok := backend.(attemptHandler); ok {
		attemptError := retryable.ServeAttempt(w, req)
		return attemptError, attemptError == nil
	}
	backend.ServeHTTP(w, req)
	return nil, true
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
