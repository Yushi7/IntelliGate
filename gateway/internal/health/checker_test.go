package health_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"intelligate/gateway/internal/balancer"
	"intelligate/gateway/internal/health"
)

func TestCheckerMarksAll2xxResponsesHealthyAndProbesHealthDirectly(t *testing.T) {
	for _, status := range []int{http.StatusOK, http.StatusCreated, http.StatusNoContent, 299} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodGet {
					t.Errorf("method = %q, want GET", r.Method)
				}
				if r.URL.Path != "/health" {
					t.Errorf("probe path = %q, want /health", r.URL.Path)
				}
				w.WriteHeader(status)
			}))
			defer server.Close()

			healthy := false
			checker := newChecker(t, []health.Backend{{
				URL:        backendURL(t, server.URL+"/configured/base?ignored=yes"),
				SetHealthy: func(value bool) error { healthy = value; return nil },
			}}, time.Hour, time.Second)

			if err := checker.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if !healthy {
				t.Fatalf("backend with HTTP %d health response marked unhealthy", status)
			}
		})
	}
}

func TestCheckerMarksRedirectAndOtherNon2xxResponsesUnhealthy(t *testing.T) {
	for _, status := range []int{http.StatusFound, http.StatusBadRequest, http.StatusInternalServerError} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var redirectTargetCalled atomic.Bool
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/healthy" {
					redirectTargetCalled.Store(true)
					w.WriteHeader(http.StatusOK)
					return
				}
				if r.URL.Path != "/health" {
					t.Errorf("probe path = %q, want /health", r.URL.Path)
				}
				if status == http.StatusFound {
					w.Header().Set("Location", "/healthy")
				}
				w.WriteHeader(status)
			}))
			defer server.Close()

			healthy := true
			checker := newChecker(t, []health.Backend{{
				URL:        backendURL(t, server.URL),
				SetHealthy: func(value bool) error { healthy = value; return nil },
			}}, time.Hour, time.Second)

			if err := checker.CheckNow(context.Background()); err != nil {
				t.Fatal(err)
			}
			if healthy {
				t.Fatalf("backend with HTTP %d health response marked healthy", status)
			}
			if redirectTargetCalled.Load() {
				t.Fatal("health checker followed a redirect")
			}
		})
	}
}

func TestCheckerMarksConnectionFailureUnhealthy(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := backendURL(t, server.URL)
	server.Close()

	healthy := true
	checker := newChecker(t, []health.Backend{{
		URL:        target,
		SetHealthy: func(value bool) error { healthy = value; return nil },
	}}, time.Hour, time.Second)

	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if healthy {
		t.Fatal("connection failure did not mark backend unhealthy")
	}
}

func TestCheckerMarksTimeoutUnhealthy(t *testing.T) {
	started := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer server.Close()

	healthy := true
	checker := newChecker(t, []health.Backend{{
		URL:        backendURL(t, server.URL),
		SetHealthy: func(value bool) error { healthy = value; return nil },
	}}, time.Hour, 50*time.Millisecond)

	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-started:
	default:
		t.Fatal("health probe did not reach backend")
	}
	if healthy {
		t.Fatal("probe timeout did not mark backend unhealthy")
	}
}

func TestCheckerRecoversBackendAfterSuccessfulProbe(t *testing.T) {
	status := atomic.Int32{}
	status.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	selector, err := balancer.NewRoundRobin(
		identifiedBackend("recovered"),
		identifiedBackend("always-healthy"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetHealthy(0, false); err != nil {
		t.Fatal(err)
	}
	checker := newChecker(t, []health.Backend{{
		URL: backendURL(t, server.URL),
		SetHealthy: func(value bool) error {
			return selector.SetHealthy(0, value)
		},
	}}, time.Hour, time.Second)

	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := selectBackend(t, selector); got != "always-healthy" {
		t.Fatalf("selected backend = %q while probe failed, want always-healthy", got)
	}

	status.Store(http.StatusOK)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := selectBackend(t, selector); got != "recovered" {
		t.Fatalf("selected backend = %q after successful probe, want recovered", got)
	}
}

func TestCheckerReportsOnlyBackendHealthTransitions(t *testing.T) {
	status := atomic.Int32{}
	status.Store(http.StatusServiceUnavailable)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(int(status.Load()))
	}))
	defer server.Close()

	var events []string
	checker := newChecker(t, []health.Backend{{
		URL:        backendURL(t, server.URL),
		SetHealthy: func(bool) error { return nil },
		OnTransition: func(healthy bool, generation uint64) {
			events = append(events, fmt.Sprintf("%d:%t", generation, healthy))
		},
	}}, time.Hour, time.Second)
	for range 2 {
		if err := checker.CheckNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	status.Store(http.StatusOK)
	for range 2 {
		if err := checker.CheckNow(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	status.Store(http.StatusServiceUnavailable)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got, want := fmt.Sprint(events), "[1:true 2:false]"; got != want {
		t.Fatalf("health transition events = %s, want %s", got, want)
	}
}

func TestHealthTransitionObserverPanicDoesNotChangeProbeState(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()
	healthy := false
	checker := newChecker(t, []health.Backend{{
		URL:        backendURL(t, server.URL),
		SetHealthy: func(value bool) error { healthy = value; return nil },
		OnTransition: func(bool, uint64) {
			panic("observer failure")
		},
	}}, time.Hour, time.Second)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !healthy {
		t.Fatal("backend was not marked healthy after observer panic")
	}
}

func TestCheckerProbesBackendsConcurrently(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	firstStarted := make(chan struct{}, 1)
	secondStarted := make(chan struct{}, 1)

	first := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		firstStarted <- struct{}{}
		select {
		case <-release:
		case <-r.Context().Done():
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer first.Close()
	second := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondStarted <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer second.Close()

	checker := newChecker(t, []health.Backend{
		{URL: backendURL(t, first.URL), SetHealthy: func(bool) error { return nil }},
		{URL: backendURL(t, second.URL), SetHealthy: func(bool) error { return nil }},
	}, time.Hour, 5*time.Second)
	checkDone := make(chan error, 1)
	go func() { checkDone <- checker.CheckNow(context.Background()) }()

	for _, started := range []<-chan struct{}{firstStarted, secondStarted} {
		select {
		case <-started:
		case <-time.After(2 * time.Second):
			releaseOnce.Do(func() { close(release) })
			<-checkDone
			t.Fatal("backend probes did not start concurrently")
		}
	}
	releaseOnce.Do(func() { close(release) })
	if err := <-checkDone; err != nil {
		t.Fatal(err)
	}
}

func TestCheckerRunStopsOnContextCancellation(t *testing.T) {
	var requests atomic.Int32
	started := make(chan struct{}, 1)
	canceled := make(chan struct{}, 1)
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		started <- struct{}{}
		<-r.Context().Done()
		canceled <- struct{}{}
	}))
	defer server.Close()

	checker := newChecker(t, []health.Backend{{
		URL:        backendURL(t, server.URL),
		SetHealthy: func(bool) error { return nil },
	}}, 10*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- checker.Run(ctx) }()

	select {
	case <-started:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("periodic health check did not start")
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil on cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("health checker did not stop after cancellation")
	}
	select {
	case <-canceled:
	case <-time.After(time.Second):
		t.Fatal("in-flight health probe was not canceled")
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("health probe count = %d, want 1", got)
	}
}

func TestCheckerRunPeriodicallyRestoresRecoveredBackend(t *testing.T) {
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if requests.Add(1) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer server.Close()

	healthStates := make(chan bool, 4)
	checker := newChecker(t, []health.Backend{{
		URL: backendURL(t, server.URL),
		SetHealthy: func(healthy bool) error {
			healthStates <- healthy
			return nil
		},
	}}, 10*time.Millisecond, time.Second)
	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- checker.Run(ctx) }()
	defer cancel()

	for _, want := range []bool{false, true} {
		select {
		case got := <-healthStates:
			if got != want {
				t.Fatalf("health state = %t, want %t", got, want)
			}
		case <-time.After(time.Second):
			t.Fatalf("did not observe periodic health state %t", want)
		}
	}
	cancel()
	select {
	case err := <-runDone:
		if err != nil {
			t.Fatalf("Run() error = %v, want nil on cancellation", err)
		}
	case <-time.After(time.Second):
		t.Fatal("health checker did not stop after cancellation")
	}
}

func newChecker(t *testing.T, backends []health.Backend, interval, timeout time.Duration) *health.Checker {
	t.Helper()
	checker, err := health.NewChecker(backends, interval, timeout)
	if err != nil {
		t.Fatal(err)
	}
	return checker
}

func backendURL(t *testing.T, value string) *url.URL {
	t.Helper()
	parsed, err := url.Parse(value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}

func identifiedBackend(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, name)
	})
}

func selectBackend(t *testing.T, selector *balancer.RoundRobin) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	return recorder.Body.String()
}
