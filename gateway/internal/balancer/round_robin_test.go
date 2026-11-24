package balancer

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"

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

func TestUnavailableBackendReturnsBadGatewayAndSelectionContinues(t *testing.T) {
	unavailable := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	unavailableURL := unavailable.URL
	unavailable.Close()

	available := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
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

	failedRequest := httptest.NewRecorder()
	handler.ServeHTTP(failedRequest, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if failedRequest.Code != http.StatusBadGateway {
		t.Fatalf("unavailable backend status = %d, want %d", failedRequest.Code, http.StatusBadGateway)
	}

	nextRequest := httptest.NewRecorder()
	handler.ServeHTTP(nextRequest, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if nextRequest.Code != http.StatusOK || nextRequest.Body.String() != "available" {
		t.Fatalf("next request = status %d, body %q; want 200 and available backend", nextRequest.Code, nextRequest.Body.String())
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
