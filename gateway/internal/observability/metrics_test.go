package observability_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"intelligate/gateway/internal/balancer"
	"intelligate/gateway/internal/observability"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/ratelimit"
)

func TestMetricsRegisterExpectedFamilies(t *testing.T) {
	metrics := observability.New(5 * time.Second)
	metrics.ObserveHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	metrics.RecordRateLimitRejection("exhausted")
	metrics.RecordSecurityDenial("authentication", http.StatusUnauthorized)
	metrics.InitializeBackend("products", "products-1")
	metrics.SetBackendHealth("products", "products-1", true, 1)
	metrics.RecordBackendSelection("products", "products-1", "initial")
	metrics.RecordBackendSelectionFailure("products", "unhealthy")
	metrics.RecordCircuitBreakerTransition("products", "products-1", "closed", "open", 1)
	metrics.RecordRetryAttempt("products", http.MethodGet)
	metrics.RecordRetryOutcome("products", "response")

	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := make(map[string]bool, len(families))
	for _, family := range families {
		found[family.GetName()] = true
	}
	for _, name := range []string{
		"intelligate_http_requests_total",
		"intelligate_http_request_duration_seconds",
		"intelligate_rate_limit_rejections_total",
		"intelligate_security_denials_total",
		"intelligate_backend_health",
		"intelligate_backend_health_transitions_total",
		"intelligate_backend_selections_total",
		"intelligate_backend_selection_failures_total",
		"intelligate_circuit_breaker_state",
		"intelligate_circuit_breaker_transitions_total",
		"intelligate_upstream_retry_attempts_total",
		"intelligate_upstream_retry_outcomes_total",
		"go_goroutines",
	} {
		if !found[name] {
			t.Errorf("registered metrics do not include %q", name)
		}
	}
	if metrics.Registry() == prometheus.DefaultRegisterer {
		t.Fatal("metrics registry unexpectedly uses Prometheus's global default registry")
	}
}

func TestHTTPMetricsNormalizeMethodRouteAndCaptureFinalStatus(t *testing.T) {
	metrics := observability.New(5 * time.Second)
	handler := metrics.ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
	}))
	request := httptest.NewRequest("BREW_SECRET", "/api/v1/products/secret-id?api_key=secret-value", nil)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("response status = %d, want 502", recorder.Code)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_http_requests_total",
		map[string]string{"method": "OTHER", "route": "products", "status": "502"}); got != 1 {
		t.Fatalf("request counter = %v, want 1", got)
	}
	if got := histogramCount(t, metrics.Registry(), "intelligate_http_request_duration_seconds",
		map[string]string{"method": "OTHER", "route": "products"}); got != 1 {
		t.Fatalf("request duration sample count = %d, want 1", got)
	}
	if got := observability.NormalizeRoute("/api/v1/orders/any/path?private=yes"); got != "orders" {
		t.Fatalf("normalized route = %q, want orders", got)
	}
	for path, want := range map[string]string{
		"/health":              "health",
		"/api/v1/products":     "products",
		"/api/v1/orders/x":     "orders",
		"/api/v1/users/secret": "users",
		"/unmatched/private":   "unmatched",
	} {
		if got := observability.NormalizeRoute(path); got != want {
			t.Errorf("NormalizeRoute(%q) = %q, want %q", path, got, want)
		}
	}
}

func TestHTTPMetricsPreserveStreamingFlush(t *testing.T) {
	metrics := observability.New(time.Second)
	flushed := false
	recorder := httptest.NewRecorder()
	handler := metrics.ObserveHTTP(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
		if err := http.NewResponseController(w).Flush(); err != nil {
			t.Errorf("flush response: %v", err)
		}
		flushed = recorder.Flushed
		_, _ = w.Write([]byte("streamed"))
	}))
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if !flushed || !recorder.Flushed || recorder.Body.String() != "streamed" {
		t.Fatalf("flush=%t recorder flushed=%t body=%q; want flush preserved and body streamed", flushed, recorder.Flushed, recorder.Body.String())
	}
}

func TestHTTPMetricsAreRecordedOnceAroundActualUpstreamRetries(t *testing.T) {
	var upstreamCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if upstreamCalls.Add(1) == 1 {
			connection, _, err := w.(http.Hijacker).Hijack()
			if err != nil {
				t.Errorf("hijack first upstream request: %v", err)
				return
			}
			_ = connection.Close()
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("retried"))
	}))
	defer upstream.Close()
	backend, err := proxy.New(upstream.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := balancer.NewRoundRobin(backend)
	if err != nil {
		t.Fatal(err)
	}
	metrics := observability.New(time.Second)
	handler := metrics.ObserveHTTP(selector)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products/42", nil))

	if recorder.Code != http.StatusOK || recorder.Body.String() != "retried" {
		t.Fatalf("gateway response = %d %q, want successful retry response", recorder.Code, recorder.Body.String())
	}
	if upstreamCalls.Load() != 2 {
		t.Fatalf("upstream attempts = %d, want initial request plus one retry", upstreamCalls.Load())
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_http_requests_total",
		map[string]string{"method": http.MethodGet, "route": "products", "status": "200"}); got != 1 {
		t.Fatalf("incoming request counter = %v, want one despite retries", got)
	}
	if got := histogramCount(t, metrics.Registry(), "intelligate_http_request_duration_seconds",
		map[string]string{"method": http.MethodGet, "route": "products"}); got != 1 {
		t.Fatalf("incoming duration observations = %d, want one despite retries", got)
	}
}

func TestComponentMetricsTransitionsAreOneHotAndBounded(t *testing.T) {
	metrics := observability.New(time.Second)
	metrics.InitializeBackend("products", "products-1")
	metrics.SetBackendHealth("products", "products-1", false, 2)
	metrics.SetBackendHealth("products", "products-1", true, 1)
	metrics.RecordBackendSelection("products", "products-1", "initial")
	metrics.RecordBackendSelection("products", "products-1", "retry")
	metrics.RecordBackendSelectionFailure("products", "breaker")
	metrics.RecordCircuitBreakerTransition("products", "products-1", "open", "half_open", 2)
	metrics.RecordCircuitBreakerTransition("products", "products-1", "closed", "open", 1)
	metrics.RecordRateLimitRejection("exhausted")
	metrics.RecordRateLimitRejection("store_unavailable")
	metrics.RecordSecurityDenial("authentication", http.StatusUnauthorized)
	metrics.RecordSecurityDenial("authorization", http.StatusForbidden)
	metrics.RecordRetryAttempt("products", http.MethodGet)
	for _, outcome := range []string{"response", "transport_error", "cancelled", "exhausted"} {
		metrics.RecordRetryOutcome("products", outcome)
	}
	metrics.RecordRetryOutcome("products", "arbitrary-error-text")
	metrics.RecordBackendSelection("products", "https://secret-host/key", "initial")
	metrics.RecordSecurityDenial("api-key-secret", http.StatusUnauthorized)

	for state, want := range map[string]float64{"closed": 0, "open": 0, "half_open": 1} {
		if got := gaugeValue(t, metrics.Registry(), "intelligate_circuit_breaker_state",
			map[string]string{"service": "products", "backend_id": "products-1", "state": state}); got != want {
			t.Errorf("breaker state %s gauge = %v, want %v", state, got, want)
		}
	}
	if got := gaugeValue(t, metrics.Registry(), "intelligate_backend_health",
		map[string]string{"service": "products", "backend_id": "products-1"}); got != 0 {
		t.Fatalf("backend health gauge = %v, want 0 after unhealthy transition", got)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_backend_health_transitions_total",
		map[string]string{"service": "products", "backend_id": "products-1", "state": "healthy"}); got != 1 {
		t.Fatalf("healthy transition count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_backend_health_transitions_total",
		map[string]string{"service": "products", "backend_id": "products-1", "state": "unhealthy"}); got != 1 {
		t.Fatalf("unhealthy transition count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_circuit_breaker_transitions_total",
		map[string]string{"service": "products", "backend_id": "products-1", "from": "closed", "to": "open"}); got != 1 {
		t.Fatalf("closed-to-open transition count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_rate_limit_rejections_total",
		map[string]string{"reason": "store_unavailable"}); got != 1 {
		t.Fatalf("store-unavailable rejection count = %v, want 1", got)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_upstream_retry_outcomes_total",
		map[string]string{"service": "products", "outcome": "exhausted"}); got != 1 {
		t.Fatalf("exhausted retry outcome count = %v, want 1", got)
	}

	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	serialized := fmt.Sprint(families)
	for _, forbidden := range []string{"secret-host", "api-key-secret", "secret-id", "secret-value", "private=yes"} {
		if strings.Contains(serialized, forbidden) {
			t.Errorf("metrics contain sensitive/high-cardinality value %q", forbidden)
		}
	}
}

func TestConcurrentMetricUpdatesAreSafe(t *testing.T) {
	metrics := observability.New(time.Second)
	const workers = 24
	const updates = 100
	var wait sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for i := 0; i < updates; i++ {
				metrics.RecordSecurityDenial("authentication", http.StatusUnauthorized)
				metrics.RecordRateLimitRejection("exhausted")
				metrics.RecordBackendSelection("products", "products-1", "initial")
				metrics.RecordRetryAttempt("products", http.MethodGet)
			}
		}()
	}
	wait.Wait()
	want := float64(workers * updates)
	for _, test := range []struct {
		name   string
		labels map[string]string
	}{
		{name: "intelligate_security_denials_total", labels: map[string]string{"stage": "authentication", "status": "401"}},
		{name: "intelligate_rate_limit_rejections_total", labels: map[string]string{"reason": "exhausted"}},
		{name: "intelligate_backend_selections_total", labels: map[string]string{"service": "products", "backend_id": "products-1", "attempt": "initial"}},
		{name: "intelligate_upstream_retry_attempts_total", labels: map[string]string{"service": "products", "method": http.MethodGet}},
	} {
		if got := counterValue(t, metrics.Registry(), test.name, test.labels); got != want {
			t.Errorf("%s counter = %v, want %v", test.name, got, want)
		}
	}
}

func TestRateLimitStoreErrorMetricIsDistinctFrom429(t *testing.T) {
	metrics := observability.New(time.Second)
	limiter, err := ratelimit.NewWithStore(ratelimit.DefaultConfig(), failingRateLimitStore{})
	if err != nil {
		t.Fatal(err)
	}
	limiter.SetRejectionObserver(metrics.RecordRateLimitRejection)
	handler := metrics.ObserveHTTP(limiter.Middleware(http.NotFoundHandler()))
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.RemoteAddr = "192.0.2.12:1000"
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("Redis store error status = %d, want 503", recorder.Code)
	}
	if got := counterValue(t, metrics.Registry(), "intelligate_rate_limit_rejections_total",
		map[string]string{"reason": "store_unavailable"}); got != 1 {
		t.Fatalf("store-unavailable metric = %v, want 1", got)
	}
	if got := counterValueOrZero(t, metrics.Registry(), "intelligate_rate_limit_rejections_total",
		map[string]string{"reason": "exhausted"}); got != 0 {
		t.Fatalf("quota-exhaustion metric = %v, want 0 for Redis error", got)
	}
}

func TestDurationBucketsCoverConfiguredRetryBudget(t *testing.T) {
	metrics := observability.New(45 * time.Second)
	metrics.ObserveHTTP(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {})).ServeHTTP(
		httptest.NewRecorder(),
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)
	families, err := metrics.Registry().Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != "intelligate_http_request_duration_seconds" {
			continue
		}
		buckets := family.GetMetric()[0].GetHistogram().GetBucket()
		if len(buckets) == 0 {
			t.Fatal("duration histogram has no buckets")
		}
		if got := buckets[len(buckets)-1].GetUpperBound(); got < 135.3 {
			t.Fatalf("duration histogram final bucket = %v, want >= retry budget 135.3s", got)
		}
		return
	}
	t.Fatal("HTTP duration histogram was not registered")
}

type failingRateLimitStore struct{}

func (failingRateLimitStore) Consume(context.Context, string) (bool, int64, error) {
	return false, 0, errors.New("sensitive Redis error")
}

func (failingRateLimitStore) Close() error {
	return nil
}

func counterValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	metric := findMetric(t, registry, name, labels)
	return metric.GetCounter().GetValue()
}

func counterValueOrZero(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metricHasLabels(metric, labels) {
				return metric.GetCounter().GetValue()
			}
		}
	}
	return 0
}

func gaugeValue(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) float64 {
	t.Helper()
	metric := findMetric(t, registry, name, labels)
	return metric.GetGauge().GetValue()
}

func histogramCount(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) uint64 {
	t.Helper()
	return findMetric(t, registry, name, labels).GetHistogram().GetSampleCount()
}

func findMetric(t *testing.T, registry *prometheus.Registry, name string, labels map[string]string) *dto.Metric {
	t.Helper()
	families, err := registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	for _, family := range families {
		if family.GetName() != name {
			continue
		}
		for _, metric := range family.GetMetric() {
			if metricHasLabels(metric, labels) {
				return metric
			}
		}
	}
	t.Fatalf("metric %q with labels %v was not found", name, labels)
	return nil
}

func metricHasLabels(metric *dto.Metric, expected map[string]string) bool {
	if len(metric.GetLabel()) != len(expected) {
		return false
	}
	for _, label := range metric.GetLabel() {
		if expected[label.GetName()] != label.GetValue() {
			return false
		}
	}
	return true
}
