package balancer

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/proxy"
)

func TestAdaptiveEWMAUsesOnlyCompletedSuccessfulAttempts(t *testing.T) {
	config := adaptiveTestConfig()
	config.EWMAAlpha = 0.5
	selector, err := NewRoundRobinWithConfig(noRetryPolicy(), config, &measuredHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}), ttfb: []time.Duration{100 * time.Millisecond, 300 * time.Millisecond}})
	if err != nil {
		t.Fatal(err)
	}
	times := []time.Time{
		time.Unix(0, 0), time.Unix(0, int64(100*time.Millisecond)),
		time.Unix(1, 0), time.Unix(1, int64(300*time.Millisecond)),
	}
	var timeIndex atomic.Int32
	selector.now = func() time.Time {
		return times[timeIndex.Add(1)-1]
	}
	for range 2 {
		recorder := httptest.NewRecorder()
		selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("status = %d, want 204", recorder.Code)
		}
	}
	snapshot := selector.performance[0].snapshot()
	if snapshot.ewma != 200*time.Millisecond || snapshot.samples != 2 || snapshot.inFlight != 0 {
		t.Fatalf("backend performance = %+v, want 200ms EWMA, 2 samples, no in-flight requests", snapshot)
	}
}

func TestAdaptiveColdStartExploresLeastSampledBackendWithRoundRobinTies(t *testing.T) {
	selector := newAdaptiveSelector(t, 3)
	setPerformance(selector, 0, 2, 10*time.Millisecond, 0)
	setPerformance(selector, 1, 1, time.Second, 0)
	setPerformance(selector, 2, 2, time.Millisecond, 0)

	if got := selectedIndex(t, selector); got != 1 {
		t.Fatalf("cold-start selected backend %d, want least-sampled backend 1", got)
	}

	selector.performance[1].mu.Lock()
	selector.performance[1].samples = 2
	selector.performance[1].mu.Unlock()
	selector.next = 2
	if got := selectedIndex(t, selector); got != 2 {
		t.Fatalf("equal-sample exploration tie selected backend %d, want Round Robin backend 2", got)
	}
}

func TestAdaptiveSelectionPrefersLowestLatencyAfterMinimumSamples(t *testing.T) {
	selector := newAdaptiveSelector(t, 3)
	setPerformance(selector, 0, 5, 90*time.Millisecond, 0)
	setPerformance(selector, 1, 5, 40*time.Millisecond, 0)
	setPerformance(selector, 2, 5, 70*time.Millisecond, 0)
	selector.next = 0

	if got := selectedIndex(t, selector); got != 1 {
		t.Fatalf("adaptive selected backend %d, want lowest-latency backend 1", got)
	}
}

func TestAdaptiveScoreAccountsForInFlightRequests(t *testing.T) {
	selector := newAdaptiveSelector(t, 2)
	setPerformance(selector, 0, 5, 10*time.Millisecond, 9)
	setPerformance(selector, 1, 5, 50*time.Millisecond, 0)
	selector.next = 0

	if got := selectedIndex(t, selector); got != 1 {
		t.Fatalf("adaptive selected backend %d, want lower-load score backend 1", got)
	}
}

func TestAdaptiveEqualScoresUseDeterministicRoundRobinTieBreaking(t *testing.T) {
	selector := newAdaptiveSelector(t, 3)
	selector.next = 2
	for index := range 3 {
		setPerformance(selector, index, 5, 25*time.Millisecond, 2)
	}
	for _, want := range []int{2, 0, 1, 2} {
		if got := selectedIndex(t, selector); got != want {
			t.Fatalf("selected backend %d, want Round Robin tie-break backend %d", got, want)
		}
	}
}

func TestAdaptiveNeverSelectsUnhealthyOrBreakerRejectedBackends(t *testing.T) {
	selector := newAdaptiveSelector(t, 3)
	for index := range 3 {
		setPerformance(selector, index, 5, time.Millisecond, 0)
	}
	if err := selector.SetHealthy(0, false); err != nil {
		t.Fatal(err)
	}

	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Minute
	openBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := openBreaker.TryAcquire()
	if !allowed {
		t.Fatal("new breaker did not admit request")
	}
	permit.Failure()
	if _, allowed := openBreaker.TryAcquire(); allowed {
		t.Fatal("open breaker admitted request before cooldown")
	}
	if err := selector.SetCircuitBreaker(1, openBreaker); err != nil {
		t.Fatal(err)
	}

	if got := selectedIndex(t, selector); got != 2 {
		t.Fatalf("selected backend %d, want only eligible backend 2", got)
	}

	halfOpenConfig := circuitbreaker.DefaultConfig()
	halfOpenConfig.FailureThreshold = 1
	halfOpenConfig.OpenCooldown = time.Nanosecond
	halfOpenBreaker, err := circuitbreaker.New(halfOpenConfig)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed = halfOpenBreaker.TryAcquire()
	if !allowed {
		t.Fatal("new breaker did not admit half-open setup request")
	}
	permit.Failure()
	deadline := time.Now().Add(time.Second)
	var trial *circuitbreaker.Permit
	for time.Now().Before(deadline) {
		trial, allowed = halfOpenBreaker.TryAcquire()
		if allowed {
			break
		}
	}
	if !allowed {
		t.Fatal("breaker did not admit half-open trial after cooldown")
	}
	if err := selector.SetCircuitBreaker(2, halfOpenBreaker); err != nil {
		t.Fatal(err)
	}
	_ = trial
	if got := selectedIndex(t, selector); got != -1 {
		t.Fatalf("adaptive selected backend %d while its half-open trial slot was full", got)
	}
	trial.Abandon()
}

func TestAdaptiveTracksEachRetryAttemptAndExcludesGatewayErrorsFromSamples(t *testing.T) {
	policy := noRetryPolicy()
	policy.MaxRetries = 1
	policy.Backoffs = []time.Duration{0}
	config := adaptiveTestConfig()
	first := measuredScriptedAttemptHandler{scriptedAttemptHandler: &scriptedAttemptHandler{attempt: func(http.ResponseWriter, *http.Request) *proxy.AttemptError {
		return &proxy.AttemptError{StatusCode: http.StatusBadGateway, Err: errors.New("connection failed")}
	}}}
	second := measuredScriptedAttemptHandler{scriptedAttemptHandler: &scriptedAttemptHandler{attempt: func(w http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
		w.WriteHeader(http.StatusOK)
		return nil
	}}, successful: true}
	selector, err := NewRoundRobinWithConfig(policy, config, &first, &second)
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	var clockCalls atomic.Int32
	base := time.Unix(5, 0)
	selector.now = func() time.Time {
		return base.Add(time.Duration(clockCalls.Add(1)) * time.Millisecond)
	}
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("final response = %d, want 200", recorder.Code)
	}
	if got := selector.performance[0].snapshot(); got.samples != 0 || got.inFlight != 0 {
		t.Fatalf("failed first attempt performance = %+v, want no sample and no in-flight work", got)
	}
	if got := selector.performance[1].snapshot(); got.samples != 1 || got.inFlight != 0 {
		t.Fatalf("successful retry performance = %+v, want one sample and no in-flight work", got)
	}
}

func TestAdaptiveInFlightIsReleasedOnCancellation(t *testing.T) {
	started := make(chan struct{})
	backend := http.HandlerFunc(func(_ http.ResponseWriter, request *http.Request) {
		close(started)
		<-request.Context().Done()
	})
	selector, err := NewRoundRobinWithConfig(noRetryPolicy(), adaptiveTestConfig(), backend)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(ctx))
		close(done)
	}()
	<-started
	if got := selector.performance[0].snapshot().inFlight; got != 1 {
		t.Fatalf("in-flight count during request = %d, want 1", got)
	}
	cancel()
	<-done
	snapshot := selector.performance[0].snapshot()
	if snapshot.inFlight != 0 || snapshot.samples != 0 {
		t.Fatalf("cancelled attempt performance = %+v, want no in-flight requests or samples", snapshot)
	}
}

func TestAdaptiveConcurrentRequestsKeepPerformanceStateSafe(t *testing.T) {
	const requests = 64
	started := make(chan struct{}, requests)
	release := make(chan struct{})
	backend := &measuredHandler{handler: http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		started <- struct{}{}
		<-release
		w.WriteHeader(http.StatusNoContent)
	}), ttfb: []time.Duration{time.Millisecond}}
	selector, err := NewRoundRobinWithConfig(noRetryPolicy(), adaptiveTestConfig(), backend)
	if err != nil {
		t.Fatal(err)
	}
	var workers sync.WaitGroup
	workers.Add(requests)
	for range requests {
		go func() {
			defer workers.Done()
			recorder := httptest.NewRecorder()
			selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusNoContent {
				t.Errorf("response status = %d, want 204", recorder.Code)
			}
		}()
	}
	for range requests {
		<-started
	}
	if got := selector.performance[0].snapshot().inFlight; got != requests {
		t.Fatalf("concurrent in-flight count = %d, want %d", got, requests)
	}
	close(release)
	workers.Wait()
	snapshot := selector.performance[0].snapshot()
	if snapshot.inFlight != 0 || snapshot.samples != requests {
		t.Fatalf("completed concurrent performance = %+v, want zero in-flight and %d samples", snapshot, requests)
	}
}

func TestAdaptiveConfigValidation(t *testing.T) {
	tests := []struct {
		name   string
		config RoutingConfig
	}{
		{name: "unknown strategy", config: RoutingConfig{Strategy: "least_loaded", EWMAAlpha: 0.2, MinSamples: 5}},
		{name: "zero alpha", config: RoutingConfig{Strategy: StrategyAdaptive, EWMAAlpha: 0, MinSamples: 5}},
		{name: "negative alpha", config: RoutingConfig{Strategy: StrategyAdaptive, EWMAAlpha: -0.1, MinSamples: 5}},
		{name: "alpha above one", config: RoutingConfig{Strategy: StrategyAdaptive, EWMAAlpha: 1.1, MinSamples: 5}},
		{name: "NaN alpha", config: RoutingConfig{Strategy: StrategyAdaptive, EWMAAlpha: math.NaN(), MinSamples: 5}},
		{name: "no minimum samples", config: RoutingConfig{Strategy: StrategyAdaptive, EWMAAlpha: 0.2, MinSamples: 0}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := NewRoundRobinWithConfig(noRetryPolicy(), test.config, identifiedBackend("backend")); err == nil {
				t.Fatalf("invalid routing config %+v was accepted", test.config)
			}
		})
	}
}

func TestRoundRobinConstructorRemainsRoundRobinByDefault(t *testing.T) {
	selector, err := NewRoundRobin(identifiedBackend("first"), identifiedBackend("second"))
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range []string{"first", "second", "first", "second"} {
		if got := selectBackend(t, selector); got != want {
			t.Fatalf("request %d selected %q, want round-robin backend %q", index+1, got, want)
		}
	}
	if selector.routing.Strategy != StrategyRoundRobin {
		t.Fatalf("default strategy = %q, want round_robin", selector.routing.Strategy)
	}
}

func newAdaptiveSelector(t *testing.T, backendCount int) *RoundRobin {
	t.Helper()
	backends := make([]http.Handler, backendCount)
	for index := range backends {
		backends[index] = identifiedBackend(fmt.Sprintf("backend-%d", index))
	}
	selector, err := NewRoundRobinWithConfig(noRetryPolicy(), adaptiveTestConfig(), backends...)
	if err != nil {
		t.Fatal(err)
	}
	return selector
}

func adaptiveTestConfig() RoutingConfig {
	config := DefaultRoutingConfig()
	config.Strategy = StrategyAdaptive
	return config
}

func noRetryPolicy() RetryPolicy {
	return RetryPolicy{UpstreamTimeout: time.Second}
}

func setPerformance(selector *RoundRobin, index int, samples uint64, latency time.Duration, inFlight uint64) {
	performance := selector.performance[index]
	performance.mu.Lock()
	performance.samples = samples
	performance.ewma = latency
	performance.inFlight = inFlight
	performance.mu.Unlock()
}

func TestCooldownProgressionAndCap(t *testing.T) {
	want := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 30 * time.Second, 30 * time.Second}
	for i, duration := range want {
		if got := cooldownForFailure(uint32(i + 1)); got != duration {
			t.Fatalf("failure %d cooldown = %s, want %s", i+1, got, duration)
		}
	}
}

func TestAdaptiveCooldownPrefersAvailableThenEarliestRecovery(t *testing.T) {
	selector := newAdaptiveSelector(t, 3)
	selector.now = func() time.Time { return time.Unix(100, 0) }
	for i := range 3 {
		setPerformance(selector, i, 5, time.Second, 0)
	}
	selector.performance[0].mu.Lock()
	selector.performance[0].cooldownUntil = time.Unix(110, 0)
	selector.performance[0].mu.Unlock()
	selector.performance[1].mu.Lock()
	selector.performance[1].cooldownUntil = time.Unix(105, 0)
	selector.performance[1].mu.Unlock()
	if got := selectedIndex(t, selector); got != 2 {
		t.Fatalf("available selected %d, want 2", got)
	}
	selector.performance[2].mu.Lock()
	selector.performance[2].cooldownUntil = time.Unix(120, 0)
	selector.performance[2].mu.Unlock()
	if got := selectedIndex(t, selector); got != 1 {
		t.Fatalf("earliest recovery selected %d, want 1", got)
	}
}

func TestCompletedSuccessResetsCooldownAndFailures(t *testing.T) {
	p := &backendPerformance{alpha: 0.2}
	now := time.Unix(10, 0)
	for range 3 {
		p.finish(0, false, false, true, now)
	}
	if s := p.snapshot(); s.consecutiveFailures != 3 || !s.cooldownUntil.Equal(now.Add(20*time.Second)) {
		t.Fatalf("failure state = %+v", s)
	}
	p.finish(25*time.Millisecond, true, false, false, now)
	if s := p.snapshot(); s.consecutiveFailures != 0 || !s.cooldownUntil.IsZero() || s.samples != 1 || s.ewma != 25*time.Millisecond {
		t.Fatalf("success did not reset failure state: %+v", s)
	}
}

func TestCustomHandlerWithoutTTFBDoesNotCreateSample(t *testing.T) {
	selector, err := NewRoundRobinWithConfig(noRetryPolicy(), adaptiveTestConfig(), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }))
	if err != nil {
		t.Fatal(err)
	}
	selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if s := selector.performance[0].snapshot(); s.samples != 0 || s.ewma != 0 || s.inFlight != 0 {
		t.Fatalf("custom handler created sample or leaked in-flight: %+v", s)
	}
}

func selectedIndex(t *testing.T, selector *RoundRobin) int {
	t.Helper()
	_, index, permit, _, _ := selector.selectBackend(make([]bool, len(selector.backends)))
	if permit != nil {
		permit.Abandon()
	}
	return index
}

func TestAdaptiveInFlightIsReleasedWhenBackendPanics(t *testing.T) {
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		panic("simulated backend panic")
	})

	selector, err := NewRoundRobinWithConfig(
		noRetryPolicy(),
		adaptiveTestConfig(),
		backend,
	)
	if err != nil {
		t.Fatal(err)
	}

	panicked := false
	func() {
		defer func() {
			if recover() != nil {
				panicked = true
			}
		}()
		selector.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/", nil),
		)
	}()

	if !panicked {
		t.Fatal("expected backend panic to propagate")
	}

	snapshot := selector.performance[0].snapshot()
	if snapshot.inFlight != 0 || snapshot.samples != 0 {
		t.Fatalf(
			"performance after backend panic = %+v, want zero in-flight and zero successful samples",
			snapshot,
		)
	}
}

func TestAdaptiveRetryPrefersUnattemptedBackendOverFasterAttemptedBackend(t *testing.T) {
	var firstCalls atomic.Int32
	var secondCalls atomic.Int32

	first := measuredScriptedAttemptHandler{scriptedAttemptHandler: &scriptedAttemptHandler{
		attempt: func(http.ResponseWriter, *http.Request) *proxy.AttemptError {
			firstCalls.Add(1)
			return &proxy.AttemptError{
				StatusCode: http.StatusBadGateway,
				Err:        errors.New("simulated first backend failure"),
			}
		},
	}}

	second := measuredScriptedAttemptHandler{scriptedAttemptHandler: &scriptedAttemptHandler{
		attempt: func(w http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			secondCalls.Add(1)
			w.WriteHeader(http.StatusOK)
			return nil
		},
	}, successful: true}

	policy := noRetryPolicy()
	policy.MaxRetries = 1
	policy.Backoffs = []time.Duration{0}

	selector, err := NewRoundRobinWithConfig(
		policy,
		adaptiveTestConfig(),
		&first,
		&second,
	)
	if err != nil {
		t.Fatal(err)
	}

	// Backend 0 is faster, but it must not be selected again for the retry.
	setPerformance(selector, 0, 5, time.Millisecond, 0)
	setPerformance(selector, 1, 5, 100*time.Millisecond, 0)

	selector.wait = func(context.Context, time.Duration) bool {
		return true
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/", nil),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("final response status = %d, want 200", recorder.Code)
	}
	if got := firstCalls.Load(); got != 1 {
		t.Fatalf("first backend calls = %d, want 1", got)
	}
	if got := secondCalls.Load(); got != 1 {
		t.Fatalf("second backend calls = %d, want 1", got)
	}
	if got := selector.performance[0].snapshot(); got.samples != 5 || got.inFlight != 0 {
		t.Fatalf("first backend performance = %+v, want unchanged samples and zero in-flight", got)
	}
	if got := selector.performance[1].snapshot(); got.samples != 6 || got.inFlight != 0 {
		t.Fatalf("second backend performance = %+v, want one additional successful sample", got)
	}
}

type measuredHandler struct {
	handler http.Handler
	ttfb    []time.Duration
	mu      sync.Mutex
}

func (h *measuredHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.handler.ServeHTTP(w, r)
}
func (h *measuredHandler) ServeMeasuredAttemptResult(w http.ResponseWriter, r *http.Request) proxy.MeasuredAttemptResult {
	h.handler.ServeHTTP(w, r)
	h.mu.Lock()
	defer h.mu.Unlock()
	d := time.Millisecond
	if len(h.ttfb) > 0 {
		d = h.ttfb[0]
		h.ttfb = h.ttfb[1:]
	}
	return proxy.MeasuredAttemptResult{BodyCompleted: true, TTFB: d, TTFBAvailable: true}
}

type measuredScriptedAttemptHandler struct {
	scriptedAttemptHandler *scriptedAttemptHandler
	successful             bool
}

func (h *measuredScriptedAttemptHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.scriptedAttemptHandler.ServeHTTP(w, r)
}

func (h measuredScriptedAttemptHandler) ServeMeasuredAttemptResult(w http.ResponseWriter, r *http.Request) proxy.MeasuredAttemptResult {
	err := h.scriptedAttemptHandler.ServeAttempt(w, r)
	return proxy.MeasuredAttemptResult{Err: err, BodyCompleted: err == nil, TTFB: 10 * time.Millisecond, TTFBAvailable: err == nil, TransportFailure: err != nil}
}
