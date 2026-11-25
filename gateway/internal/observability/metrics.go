package observability

import (
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type Metrics struct {
	registry *prometheus.Registry

	httpRequests           *prometheus.CounterVec
	httpRequestDuration    *prometheus.HistogramVec
	rateLimitRejections    *prometheus.CounterVec
	securityDenials        *prometheus.CounterVec
	backendHealth          *prometheus.GaugeVec
	backendHealthChanges   *prometheus.CounterVec
	backendSelections      *prometheus.CounterVec
	selectionFailures      *prometheus.CounterVec
	breakerStates          *prometheus.GaugeVec
	breakerTransitions     *prometheus.CounterVec
	retryAttempts          *prometheus.CounterVec
	retryOutcomes          *prometheus.CounterVec
	backendHealthMu        sync.Mutex
	backendHealthVersion   map[string]uint64
	breakerStateMu         sync.Mutex
	breakerStateGeneration map[string]uint64
}

func New(upstreamTimeout time.Duration) *Metrics {
	registry := prometheus.NewRegistry()
	metrics := &Metrics{
		registry: registry,
		httpRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_http_requests_total",
			Help: "Total incoming HTTP requests handled by the gateway.",
		}, []string{"method", "route", "status"}),
		httpRequestDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "intelligate_http_request_duration_seconds",
			Help:    "Duration of incoming HTTP requests handled by the gateway.",
			Buckets: durationBuckets(upstreamTimeout),
		}, []string{"method", "route"}),
		rateLimitRejections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_rate_limit_rejections_total",
			Help: "Requests rejected by rate limiting or rate-limit store failures.",
		}, []string{"reason"}),
		securityDenials: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_security_denials_total",
			Help: "Requests denied by gateway authentication or authorization.",
		}, []string{"stage", "status"}),
		backendHealth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "intelligate_backend_health",
			Help: "Whether a configured backend is healthy (1) or unhealthy (0).",
		}, []string{"service", "backend_id"}),
		backendHealthChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_backend_health_transitions_total",
			Help: "Backend health state transitions.",
		}, []string{"service", "backend_id", "state"}),
		backendSelections: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_backend_selections_total",
			Help: "Backend selections made by the load balancer.",
		}, []string{"service", "backend_id", "attempt"}),
		selectionFailures: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_backend_selection_failures_total",
			Help: "Load-balancer selection attempts that found no eligible backend.",
		}, []string{"service", "reason"}),
		breakerStates: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "intelligate_circuit_breaker_state",
			Help: "One-hot circuit-breaker state for each configured backend.",
		}, []string{"service", "backend_id", "state"}),
		breakerTransitions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_circuit_breaker_transitions_total",
			Help: "Circuit-breaker state transitions.",
		}, []string{"service", "backend_id", "from", "to"}),
		retryAttempts: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_upstream_retry_attempts_total",
			Help: "Upstream retry attempts, excluding initial attempts.",
		}, []string{"service", "method"}),
		retryOutcomes: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "intelligate_upstream_retry_outcomes_total",
			Help: "Outcomes for dispatched upstream retry attempts.",
		}, []string{"service", "outcome"}),
		backendHealthVersion:   make(map[string]uint64),
		breakerStateGeneration: make(map[string]uint64),
	}

	registry.MustRegister(
		prometheus.NewGoCollector(),
		prometheus.NewProcessCollector(prometheus.ProcessCollectorOpts{}),
		metrics.httpRequests,
		metrics.httpRequestDuration,
		metrics.rateLimitRejections,
		metrics.securityDenials,
		metrics.backendHealth,
		metrics.backendHealthChanges,
		metrics.backendSelections,
		metrics.selectionFailures,
		metrics.breakerStates,
		metrics.breakerTransitions,
		metrics.retryAttempts,
		metrics.retryOutcomes,
	)
	return metrics
}

func durationBuckets(upstreamTimeout time.Duration) []float64 {
	buckets := []float64{0.001, 0.005, 0.01, 0.025, 0.05, 0.1, 0.25, 0.5, 1, 2.5, 5, 10, 20}
	retryBudgetSeconds := upstreamTimeout.Seconds()*3 + 0.3
	for last := buckets[len(buckets)-1]; last < retryBudgetSeconds; last *= 2 {
		buckets = append(buckets, last*2)
	}
	return buckets
}

func (m *Metrics) Registry() *prometheus.Registry {
	return m.registry
}

func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{})
}

func (m *Metrics) ObserveHTTP(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		method := normalizeMethod(r.Method)
		route := NormalizeRoute(r.URL.Path)
		captured := httpsnoop.CaptureMetrics(next, w, r)
		status := captured.Code
		if status == 0 {
			status = http.StatusOK
		}
		m.httpRequests.WithLabelValues(method, route, fmt.Sprint(status)).Inc()
		m.httpRequestDuration.WithLabelValues(method, route).Observe(captured.Duration.Seconds())
	})
}

func NormalizeRoute(path string) string {
	switch {
	case path == "/health":
		return "health"
	case path == "/api/v1/products" || strings.HasPrefix(path, "/api/v1/products/"):
		return "products"
	case path == "/api/v1/orders" || strings.HasPrefix(path, "/api/v1/orders/"):
		return "orders"
	case path == "/api/v1/users" || strings.HasPrefix(path, "/api/v1/users/"):
		return "users"
	default:
		return "unmatched"
	}
}

func normalizeMethod(method string) string {
	switch method {
	case http.MethodGet, http.MethodHead, http.MethodPost, http.MethodPut,
		http.MethodPatch, http.MethodDelete, http.MethodOptions,
		http.MethodConnect, http.MethodTrace:
		return method
	default:
		return "OTHER"
	}
}

func (m *Metrics) RecordRateLimitRejection(reason string) {
	if reason != "exhausted" && reason != "store_unavailable" {
		return
	}
	m.rateLimitRejections.WithLabelValues(reason).Inc()
}

func (m *Metrics) RecordSecurityDenial(stage string, status int) {
	if stage != "authentication" && stage != "authorization" {
		return
	}
	if status != http.StatusUnauthorized && status != http.StatusForbidden {
		return
	}
	m.securityDenials.WithLabelValues(stage, fmt.Sprint(status)).Inc()
}

func (m *Metrics) InitializeBackend(service, backendID string) {
	if !validService(service) || !validBackendID(backendID) {
		return
	}
	m.backendHealthMu.Lock()
	m.backendHealthVersion[backendKey(service, backendID)] = 0
	m.backendHealth.WithLabelValues(service, backendID).Set(0)
	m.backendHealthMu.Unlock()

	m.breakerStateMu.Lock()
	m.breakerStateGeneration[backendKey(service, backendID)] = 0
	for _, state := range []string{"closed", "open", "half_open"} {
		value := 0.0
		if state == "closed" {
			value = 1
		}
		m.breakerStates.WithLabelValues(service, backendID, state).Set(value)
	}
	m.breakerStateMu.Unlock()
}

func (m *Metrics) SetBackendHealth(service, backendID string, healthy bool, generation uint64) {
	if !validService(service) || !validBackendID(backendID) {
		return
	}
	key := backendKey(service, backendID)
	value, state := 0.0, "unhealthy"
	if healthy {
		value, state = 1, "healthy"
	}
	m.backendHealthChanges.WithLabelValues(service, backendID, state).Inc()
	m.backendHealthMu.Lock()
	if generation > m.backendHealthVersion[key] {
		m.backendHealthVersion[key] = generation
		m.backendHealth.WithLabelValues(service, backendID).Set(value)
	}
	m.backendHealthMu.Unlock()
}

func (m *Metrics) RecordBackendSelection(service, backendID, attempt string) {
	if !validService(service) || !validBackendID(backendID) || (attempt != "initial" && attempt != "retry") {
		return
	}
	m.backendSelections.WithLabelValues(service, backendID, attempt).Inc()
}

func (m *Metrics) RecordBackendSelectionFailure(service, reason string) {
	if !validService(service) || (reason != "unhealthy" && reason != "breaker") {
		return
	}
	m.selectionFailures.WithLabelValues(service, reason).Inc()
}

func (m *Metrics) RecordCircuitBreakerTransition(service, backendID, from, to string, generation uint64) {
	if !validService(service) || !validBackendID(backendID) || !validBreakerState(from) || !validBreakerState(to) || from == to {
		return
	}
	m.breakerTransitions.WithLabelValues(service, backendID, from, to).Inc()
	key := backendKey(service, backendID)
	m.breakerStateMu.Lock()
	if generation > m.breakerStateGeneration[key] {
		m.breakerStateGeneration[key] = generation
		for _, state := range []string{"closed", "open", "half_open"} {
			value := 0.0
			if state == to {
				value = 1
			}
			m.breakerStates.WithLabelValues(service, backendID, state).Set(value)
		}
	}
	m.breakerStateMu.Unlock()
}

func (m *Metrics) RecordRetryAttempt(service, method string) {
	if !validService(service) {
		return
	}
	m.retryAttempts.WithLabelValues(service, normalizeMethod(method)).Inc()
}

func (m *Metrics) RecordRetryOutcome(service, outcome string) {
	if !validService(service) ||
		(outcome != "response" && outcome != "transport_error" && outcome != "cancelled" && outcome != "exhausted") {
		return
	}
	m.retryOutcomes.WithLabelValues(service, outcome).Inc()
}

func validService(service string) bool {
	return service == "products" || service == "orders" || service == "users"
}

func validBreakerState(state string) bool {
	return state == "closed" || state == "open" || state == "half_open"
}

func validBackendID(backendID string) bool {
	if backendID == "" || len(backendID) > 64 {
		return false
	}
	for _, character := range backendID {
		if !(character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			character == '_' || character == '-') {
			return false
		}
	}
	return true
}

func backendKey(service, backendID string) string {
	return service + "\x00" + backendID
}
