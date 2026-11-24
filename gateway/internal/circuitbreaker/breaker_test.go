package circuitbreaker

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDefaultConfig(t *testing.T) {
	config := DefaultConfig()
	if config.FailureThreshold != 5 ||
		config.OpenCooldown != 10*time.Second ||
		config.HalfOpenMaxRequests != 1 ||
		config.HalfOpenSuccessThreshold != 1 {
		t.Fatalf("DefaultConfig() = %+v, want the Phase 3B defaults", config)
	}
}

func TestNewRejectsInvalidConfig(t *testing.T) {
	defaults := DefaultConfig()
	tests := []struct {
		name   string
		config Config
	}{
		{name: "failure threshold", config: Config{OpenCooldown: defaults.OpenCooldown, HalfOpenMaxRequests: defaults.HalfOpenMaxRequests, HalfOpenSuccessThreshold: defaults.HalfOpenSuccessThreshold}},
		{name: "cooldown", config: Config{FailureThreshold: defaults.FailureThreshold, HalfOpenMaxRequests: defaults.HalfOpenMaxRequests, HalfOpenSuccessThreshold: defaults.HalfOpenSuccessThreshold}},
		{name: "half-open max requests", config: Config{FailureThreshold: defaults.FailureThreshold, OpenCooldown: defaults.OpenCooldown, HalfOpenSuccessThreshold: defaults.HalfOpenSuccessThreshold}},
		{name: "half-open success threshold", config: Config{FailureThreshold: defaults.FailureThreshold, OpenCooldown: defaults.OpenCooldown, HalfOpenMaxRequests: defaults.HalfOpenMaxRequests}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := New(test.config); err == nil {
				t.Fatal("New() error = nil, want invalid config error")
			}
		})
	}
}

func TestFailureThresholdOpensBreaker(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 3
	breaker, _ := newTestBreaker(t, config)

	for range config.FailureThreshold - 1 {
		permit, allowed := breaker.TryAcquire()
		if !allowed {
			t.Fatal("closed breaker rejected a request")
		}
		permit.Failure()
	}
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("state before threshold = %s, want closed", got)
	}

	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request at threshold")
	}
	permit.Failure()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state after threshold = %s, want open", got)
	}
	if _, allowed := breaker.TryAcquire(); allowed {
		t.Fatal("open breaker admitted request before cooldown")
	}
}

func TestSuccessResetsConsecutiveFailureCount(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 3
	breaker, _ := newTestBreaker(t, config)

	failRequest(t, breaker)
	failRequest(t, breaker)
	completeRequest(t, breaker, func(permit *Permit) { permit.Success() })
	failRequest(t, breaker)
	failRequest(t, breaker)
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("state after reset failures = %s, want closed", got)
	}
	failRequest(t, breaker)
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state at next failure threshold = %s, want open", got)
	}
}

func TestCooldownAdmitsHalfOpenTrial(t *testing.T) {
	config := DefaultConfig()
	breaker, now := newTestBreaker(t, config)
	openBreaker(t, breaker, config.FailureThreshold)

	*now = now.Add(config.OpenCooldown - time.Nanosecond)
	if _, allowed := breaker.TryAcquire(); allowed {
		t.Fatal("breaker admitted trial before cooldown elapsed")
	}
	*now = now.Add(time.Nanosecond)
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected trial after cooldown")
	}
	if got := breaker.State(); got != StateHalfOpen {
		t.Fatalf("state after cooldown = %s, want half-open", got)
	}
	permit.Success()
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("state after successful trial = %s, want closed", got)
	}
}

func TestHalfOpenConcurrentTrialLimit(t *testing.T) {
	config := DefaultConfig()
	breaker, now := newTestBreaker(t, config)
	openBreaker(t, breaker, config.FailureThreshold)
	*now = now.Add(config.OpenCooldown)

	const attempts = 64
	var allowed atomic.Int32
	permits := make(chan *Permit, attempts)
	var workers sync.WaitGroup
	workers.Add(attempts)
	for range attempts {
		go func() {
			defer workers.Done()
			permit, ok := breaker.TryAcquire()
			if ok {
				allowed.Add(1)
				permits <- permit
			}
		}()
	}
	workers.Wait()

	if got := allowed.Load(); got != 1 {
		t.Fatalf("half-open admitted %d concurrent trials, want 1", got)
	}
	(<-permits).Abandon()
	if got := breaker.State(); got != StateHalfOpen {
		t.Fatalf("state after abandoned trial = %s, want half-open", got)
	}
	next, ok := breaker.TryAcquire()
	if !ok {
		t.Fatal("abandoned trial did not release its half-open slot")
	}
	next.Success()
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("state after replacement trial success = %s, want closed", got)
	}
}

func TestFailedHalfOpenTrialReopensAndRestartsCooldown(t *testing.T) {
	config := DefaultConfig()
	breaker, now := newTestBreaker(t, config)
	openBreaker(t, breaker, config.FailureThreshold)
	*now = now.Add(config.OpenCooldown)

	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected half-open trial")
	}
	permit.Failure()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state after failed trial = %s, want open", got)
	}

	*now = now.Add(config.OpenCooldown - time.Nanosecond)
	if _, allowed := breaker.TryAcquire(); allowed {
		t.Fatal("breaker admitted retry before restarted cooldown elapsed")
	}
	*now = now.Add(time.Nanosecond)
	if _, allowed := breaker.TryAcquire(); !allowed {
		t.Fatal("breaker rejected trial after restarted cooldown")
	}
}

func TestHalfOpenRequiresConfiguredSuccessfulTrials(t *testing.T) {
	config := DefaultConfig()
	config.HalfOpenSuccessThreshold = 2
	breaker, now := newTestBreaker(t, config)
	openBreaker(t, breaker, config.FailureThreshold)
	*now = now.Add(config.OpenCooldown)

	first, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected first half-open trial")
	}
	first.Success()
	if got := breaker.State(); got != StateHalfOpen {
		t.Fatalf("state after first success = %s, want half-open", got)
	}

	second, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected second half-open trial")
	}
	second.Success()
	if got := breaker.State(); got != StateClosed {
		t.Fatalf("state after required successes = %s, want closed", got)
	}
}

func TestLatePermitResultsDoNotOverrideNewerState(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 1
	config.HalfOpenMaxRequests = 2
	breaker, now := newTestBreaker(t, config)

	first, _ := breaker.TryAcquire()
	stale, _ := breaker.TryAcquire()
	first.Failure()
	stale.Success()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("late closed-state result changed state to %s, want open", got)
	}

	*now = now.Add(config.OpenCooldown)
	trialOne, _ := breaker.TryAcquire()
	trialTwo, _ := breaker.TryAcquire()
	trialOne.Failure()
	trialTwo.Success()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("late half-open result changed state to %s, want open", got)
	}
}

func TestPermitCanBeRetrievedFromContext(t *testing.T) {
	breaker, _ := newTestBreaker(t, DefaultConfig())
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}

	ctx := WithPermit(context.Background(), permit)
	if got := PermitFromContext(ctx); got != permit {
		t.Fatalf("PermitFromContext() = %p, want %p", got, permit)
	}
}

func TestPermitCompletionIsIdempotent(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 2
	breaker, _ := newTestBreaker(t, config)

	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	permit.Failure()
	permit.Success()
	failRequest(t, breaker)
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state after idempotent completion = %s, want open", got)
	}
}

func TestConcurrentFailuresOpenBreakerAtThreshold(t *testing.T) {
	config := DefaultConfig()
	config.FailureThreshold = 5
	breaker, _ := newTestBreaker(t, config)
	const requests = 64
	permits := make([]*Permit, requests)
	for i := range permits {
		permit, allowed := breaker.TryAcquire()
		if !allowed {
			t.Fatalf("closed breaker rejected permit %d", i)
		}
		permits[i] = permit
	}

	var workers sync.WaitGroup
	workers.Add(requests)
	for _, permit := range permits {
		go func() {
			defer workers.Done()
			permit.Failure()
		}()
	}
	workers.Wait()
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state after concurrent failures = %s, want open", got)
	}
}

func failRequest(t *testing.T, breaker *Breaker) {
	t.Helper()
	completeRequest(t, breaker, func(permit *Permit) { permit.Failure() })
}

func completeRequest(t *testing.T, breaker *Breaker, complete func(*Permit)) {
	t.Helper()
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected request")
	}
	complete(permit)
}

func openBreaker(t *testing.T, breaker *Breaker, failures int) {
	t.Helper()
	for range failures {
		failRequest(t, breaker)
	}
	if got := breaker.State(); got != StateOpen {
		t.Fatalf("state after failures = %s, want open", got)
	}
}

func newTestBreaker(t *testing.T, config Config) (*Breaker, *time.Time) {
	t.Helper()
	now := time.Unix(0, 0)
	return newWithClock(config, func() time.Time { return now }), &now
}
