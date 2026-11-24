package balancer

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

	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/health"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/router"
)

func TestRoundRobinSelectsSequentiallyAndWrapsAround(t *testing.T) {
	selector, err := NewRoundRobin(
		identifiedBackend("first"),
		identifiedBackend("second"),
		identifiedBackend("third"),
	)
	if err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "second", "third", "first"}
	for i, expected := range want {
		recorder := httptest.NewRecorder()
		selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if got := recorder.Body.String(); got != expected {
			t.Errorf("request %d selected %q, want %q", i+1, got, expected)
		}
	}
}

func TestProductAndOrderSelectorsMaintainIndependentCursors(t *testing.T) {
	productSelector, err := NewRoundRobin(
		identifiedBackend("product-first"),
		identifiedBackend("product-second"),
	)
	if err != nil {
		t.Fatal(err)
	}
	orderSelector, err := NewRoundRobin(
		identifiedBackend("order-first"),
		identifiedBackend("order-second"),
	)
	if err != nil {
		t.Fatal(err)
	}

	if got := selectBackend(t, productSelector); got != "product-first" {
		t.Fatalf("first Product backend = %q, want product-first", got)
	}
	if got := selectBackend(t, orderSelector); got != "order-first" {
		t.Fatalf("first Order backend = %q, want order-first", got)
	}
	if got := selectBackend(t, productSelector); got != "product-second" {
		t.Fatalf("second Product backend = %q, want product-second", got)
	}
	if got := selectBackend(t, orderSelector); got != "order-second" {
		t.Fatalf("second Order backend = %q, want order-second", got)
	}
}

func TestRoundRobinDistributesRequestsAcrossBackends(t *testing.T) {
	const requests = 12
	counts := []atomic.Int64{{}, {}, {}}
	backends := make([]http.Handler, len(counts))
	for i := range backends {
		index := i
		backends[i] = http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			counts[index].Add(1)
			w.WriteHeader(http.StatusNoContent)
		})
	}

	selector, err := NewRoundRobin(backends...)
	if err != nil {
		t.Fatal(err)
	}
	for range requests {
		selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	}

	for i := range counts {
		if got := counts[i].Load(); got != requests/int64(len(counts)) {
			t.Errorf("backend %d received %d requests, want %d", i, got, requests/int64(len(counts)))
		}
	}
}

func TestRoundRobinSelectsOnlyHealthyBackends(t *testing.T) {
	selector, err := NewRoundRobin(
		identifiedBackend("first"),
		identifiedBackend("second"),
		identifiedBackend("third"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetHealthy(1, false); err != nil {
		t.Fatal(err)
	}

	want := []string{"first", "third", "first", "third"}
	for i, expected := range want {
		if got := selectBackend(t, selector); got != expected {
			t.Errorf("request %d selected %q, want %q", i+1, got, expected)
		}
	}
}

func TestRoundRobinReturnsUnavailableWhenAllBackendsAreUnhealthy(t *testing.T) {
	selector, err := NewRoundRobin(identifiedBackend("first"), identifiedBackend("second"))
	if err != nil {
		t.Fatal(err)
	}
	for i := range 2 {
		if err := selector.SetHealthy(i, false); err != nil {
			t.Fatal(err)
		}
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusServiceUnavailable)
	}
}

func TestRecoveredBackendReentersRoundRobinSelection(t *testing.T) {
	selector, err := NewRoundRobin(identifiedBackend("first"), identifiedBackend("second"))
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetHealthy(1, false); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		if got := selectBackend(t, selector); got != "first" {
			t.Fatalf("selected %q while second backend was unhealthy, want first", got)
		}
	}
	if err := selector.SetHealthy(1, true); err != nil {
		t.Fatal(err)
	}
	if got := selectBackend(t, selector); got != "second" {
		t.Fatalf("recovered backend selected %q, want second", got)
	}
}

func TestRoundRobinSelectionIsSafeDuringHealthUpdates(t *testing.T) {
	selector, err := NewRoundRobin(
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
		http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusNoContent) }),
	)
	if err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	workers.Add(2)
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			recorder := httptest.NewRecorder()
			selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != http.StatusNoContent {
				t.Errorf("selection returned status %d, want %d", recorder.Code, http.StatusNoContent)
				return
			}
		}
	}()
	go func() {
		defer workers.Done()
		for i := 0; i < 1000; i++ {
			if err := selector.SetHealthy(1, i%2 == 0); err != nil {
				t.Errorf("SetHealthy(1): %v", err)
				return
			}
			if err := selector.SetHealthy(2, i%2 != 0); err != nil {
				t.Errorf("SetHealthy(2): %v", err)
				return
			}
		}
	}()
	workers.Wait()
}

func TestRoundRobinIsSafeForConcurrentRequests(t *testing.T) {
	const (
		backendCount = 4
		requestCount = 400
	)
	counts := make([]atomic.Int64, backendCount)
	backends := make([]http.Handler, backendCount)
	for i := range backends {
		index := i
		backends[i] = http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			counts[index].Add(1)
		})
	}

	selector, err := NewRoundRobin(backends...)
	if err != nil {
		t.Fatal(err)
	}

	var workers sync.WaitGroup
	workers.Add(requestCount)
	for range requestCount {
		go func() {
			defer workers.Done()
			selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	workers.Wait()

	for i := range counts {
		if got := counts[i].Load(); got != requestCount/backendCount {
			t.Errorf("backend %d received %d requests, want %d", i, got, requestCount/backendCount)
		}
	}
}

func TestRoundRobinRequiresBackends(t *testing.T) {
	if _, err := NewRoundRobin(); err == nil {
		t.Fatal("NewRoundRobin() error = nil, want error for empty backend list")
	}
	if _, err := NewRoundRobin(nil); err == nil {
		t.Fatal("NewRoundRobin(nil) error = nil, want error for nil backend")
	}
}

func TestRoundRobinRoutesToServiceInstancesAndPreservesPathQuery(t *testing.T) {
	newBackend := func(name string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if got, want := r.URL.RequestURI(), "/api/v1/products/demo?source=test"; got != want {
				t.Errorf("backend request URI = %q, want %q", got, want)
			}
			_, _ = fmt.Fprint(w, name)
		}))
	}
	first := newBackend("first")
	defer first.Close()
	second := newBackend("second")
	defer second.Close()

	firstProxy, err := proxy.New(first.URL)
	if err != nil {
		t.Fatal(err)
	}
	secondProxy, err := proxy.New(second.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(firstProxy, secondProxy)
	if err != nil {
		t.Fatal(err)
	}
	handler := router.New(router.Route{Prefix: "/api/v1/products", Handler: selector})

	for i, want := range []string{"first", "second"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(
			http.MethodGet,
			"/api/v1/products/demo?source=test",
			nil,
		))
		if recorder.Code != http.StatusOK {
			t.Fatalf("request %d status = %d, want %d", i+1, recorder.Code, http.StatusOK)
		}
		if got := recorder.Body.String(); got != want {
			t.Errorf("request %d reached %q, want %q", i+1, got, want)
		}
	}
}

func TestUnavailableBackendRetriesOnAvailableInstance(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unavailableURL := unavailable.URL
	unavailable.Close()

	var availableRequests atomic.Int32
	available := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		availableRequests.Add(1)
		_, _ = fmt.Fprint(w, "available")
	}))
	defer available.Close()

	unavailableProxy, err := proxy.New(unavailableURL)
	if err != nil {
		t.Fatal(err)
	}
	availableProxy, err := proxy.New(available.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(unavailableProxy, availableProxy)
	if err != nil {
		t.Fatal(err)
	}
	handler := router.New(router.Route{Prefix: "/api/v1/products", Handler: selector})

	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
		if recorder.Code != http.StatusOK || recorder.Body.String() != "available" {
			t.Fatalf("request %d = status %d, body %q; want 200 and available backend", i+1, recorder.Code, recorder.Body.String())
		}
	}
	if got := availableRequests.Load(); got != 2 {
		t.Fatalf("available backend received %d requests, want 2", got)
	}
}

func TestRoundRobinSkipsOpenBackendAndKeepsBreakersIndependent(t *testing.T) {
	firstServer := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	firstURL := firstServer.URL
	firstServer.Close()
	var secondRequests atomic.Int32
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondRequests.Add(1)
		_, _ = fmt.Fprint(w, "second")
	}))
	defer secondServer.Close()

	firstProxy, err := proxy.New(firstURL)
	if err != nil {
		t.Fatal(err)
	}
	secondProxy, err := proxy.New(secondServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(firstProxy, secondProxy)
	if err != nil {
		t.Fatal(err)
	}

	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Hour
	firstBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	secondBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, firstBreaker); err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(1, secondBreaker); err != nil {
		t.Fatal(err)
	}

	firstRequest := httptest.NewRecorder()
	selector.ServeHTTP(firstRequest, httptest.NewRequest(http.MethodGet, "/products", nil))
	if firstRequest.Code != http.StatusOK || firstRequest.Body.String() != "second" {
		t.Fatalf("first request = status %d, body %q; want 200 from second backend after retry", firstRequest.Code, firstRequest.Body.String())
	}
	if got := firstBreaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("first backend breaker state = %s, want open", got)
	}
	if got := secondBreaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("second backend breaker state = %s, want closed", got)
	}

	for range 2 {
		recorder := httptest.NewRecorder()
		selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/products", nil))
		if recorder.Code != http.StatusOK || recorder.Body.String() != "second" {
			t.Fatalf("request to healthy instance = status %d, body %q; want 200 and second", recorder.Code, recorder.Body.String())
		}
	}
	if got := secondRequests.Load(); got != 3 {
		t.Fatalf("healthy backend received %d requests, want 3", got)
	}
}

func TestOpenCircuitReturns503AndSuccessfulHealthProbeDoesNotCloseIt(t *testing.T) {
	var applicationRequests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		applicationRequests.Add(1)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Errorf("hijack application connection: %v", err)
			return
		}
		_ = conn.Close()
	}))
	defer server.Close()

	target, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	backendProxy, err := proxy.New(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(backendProxy)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Hour
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, breaker); err != nil {
		t.Fatal(err)
	}
	checker, err := health.NewChecker([]health.Backend{{
		URL: target,
		SetHealthy: func(healthy bool) error {
			return selector.SetHealthy(0, healthy)
		},
	}}, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	failed := httptest.NewRecorder()
	selector.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/products", nil))
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("failed upstream status = %d, want %d", failed.Code, http.StatusBadGateway)
	}
	if got := breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after transport failure = %s, want open", got)
	}

	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after successful health probe = %s, want open", got)
	}

	requestsBeforeRejection := applicationRequests.Load()
	rejected := httptest.NewRecorder()
	selector.ServeHTTP(rejected, httptest.NewRequest(http.MethodGet, "/products", nil))
	if rejected.Code != http.StatusServiceUnavailable {
		t.Fatalf("open-circuit status = %d, want %d", rejected.Code, http.StatusServiceUnavailable)
	}
	if got := applicationRequests.Load(); got != requestsBeforeRejection {
		t.Fatalf("open-circuit rejection contacted backend: request count changed from %d to %d", requestsBeforeRejection, got)
	}
}

func TestRecoveredBackendReentersAfterSuccessfulHalfOpenTrial(t *testing.T) {
	var firstRequests atomic.Int32
	first := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		permit := circuitbreaker.PermitFromContext(r.Context())
		if permit == nil {
			t.Error("first backend request has no circuit-breaker permit")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		if firstRequests.Add(1) == 1 {
			permit.Failure()
			http.Error(w, "upstream unavailable", http.StatusBadGateway)
			return
		}
		permit.Success()
		_, _ = fmt.Fprint(w, "recovered")
	})
	second := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		permit := circuitbreaker.PermitFromContext(r.Context())
		if permit == nil {
			t.Error("second backend request has no circuit-breaker permit")
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
		permit.Success()
		_, _ = fmt.Fprint(w, "second")
	})
	selector, err := NewRoundRobin(first, second)
	if err != nil {
		t.Fatal(err)
	}

	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Millisecond
	firstBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	secondBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, firstBreaker); err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(1, secondBreaker); err != nil {
		t.Fatal(err)
	}

	failed := httptest.NewRecorder()
	selector.ServeHTTP(failed, httptest.NewRequest(http.MethodGet, "/", nil))
	if failed.Code != http.StatusBadGateway {
		t.Fatalf("initial response status = %d, want %d", failed.Code, http.StatusBadGateway)
	}
	other := httptest.NewRecorder()
	selector.ServeHTTP(other, httptest.NewRequest(http.MethodGet, "/", nil))
	if other.Code != http.StatusOK || other.Body.String() != "second" {
		t.Fatalf("alternate backend response = status %d, body %q; want 200 from second", other.Code, other.Body.String())
	}
	time.Sleep(2 * config.OpenCooldown)
	recovered := httptest.NewRecorder()
	selector.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, "/", nil))
	if recovered.Code != http.StatusOK || recovered.Body.String() != "recovered" {
		t.Fatalf("half-open trial response = status %d, body %q; want recovered backend", recovered.Code, recovered.Body.String())
	}
	if got := firstBreaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("recovered breaker state = %s, want closed", got)
	}
	if got := firstRequests.Load(); got != 2 {
		t.Fatalf("recovered backend received %d requests, want 2", got)
	}
}

func identifiedBackend(name string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = fmt.Fprint(w, name)
	})
}

func selectBackend(t *testing.T, selector *RoundRobin) string {
	t.Helper()
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	return recorder.Body.String()
}
