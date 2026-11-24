package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"intelligate/gateway/internal/health"
)

func TestHealthReturnsGatewayStatusWithoutProxying(t *testing.T) {
	proxyCalled := false
	backend := http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		proxyCalled = true
	})

	recorder := httptest.NewRecorder()
	newHandler(backend, backend, backend).ServeHTTP(
		recorder,
		httptest.NewRequest(http.MethodGet, "/health", nil),
	)

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusOK)
	}

	var response struct {
		Status  string `json:"status"`
		Service string `json:"service"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if response.Status != "ok" || response.Service != "intelligate-gateway" {
		t.Fatalf("response = %+v, want status ok and service intelligate-gateway", response)
	}
	if proxyCalled {
		t.Fatal("health request invoked a backend proxy")
	}
}

func TestBackendURLsFromEnvAcceptsHTTPAndHTTPS(t *testing.T) {
	t.Setenv("PRODUCT_SERVICE_URLS", "http://localhost:8001, https://products.example.test:8443")

	urls, err := backendURLsFromEnv("PRODUCT_SERVICE_URLS", "http://localhost:8001")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://localhost:8001", "https://products.example.test:8443"}
	if !reflect.DeepEqual(urls, want) {
		t.Fatalf("backend URLs = %v, want %v", urls, want)
	}
}

func TestBackendURLsFromEnvUsesDefaultWhenUnset(t *testing.T) {
	const envName = "INTELLIGATE_TEST_DEFAULT_BACKENDS"
	previous, wasSet := os.LookupEnv(envName)
	if err := os.Unsetenv(envName); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(envName, previous)
		} else {
			_ = os.Unsetenv(envName)
		}
	})

	urls, err := backendURLsFromEnv(envName, "http://localhost:8001")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"http://localhost:8001"}; !reflect.DeepEqual(urls, want) {
		t.Fatalf("backend URLs = %v, want default %v", urls, want)
	}
}

func TestBackendURLsFromEnvRejectsInvalidEntries(t *testing.T) {
	const envName = "PRODUCT_SERVICE_URLS"
	tests := []struct {
		name         string
		value        string
		invalidEntry string
	}{
		{name: "malformed URL", value: "http://%zz", invalidEntry: "http://%zz"},
		{name: "unsupported scheme", value: "ftp://localhost:8001", invalidEntry: "ftp://localhost:8001"},
		{name: "relative URL", value: "localhost:8001", invalidEntry: "localhost:8001"},
		{name: "empty hostname", value: "http:///products", invalidEntry: "http:///products"},
		{name: "invalid hostname", value: "http://-invalid.example", invalidEntry: "http://-invalid.example"},
		{name: "empty port", value: "http://localhost:", invalidEntry: "http://localhost:"},
		{name: "invalid port", value: "http://localhost:70000", invalidEntry: "http://localhost:70000"},
		{name: "empty list entry", value: "http://localhost:8001,,http://localhost:8011", invalidEntry: ""},
		{name: "empty configuration", value: "", invalidEntry: ""},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(envName, test.value)

			_, err := backendURLsFromEnv(envName, "http://localhost:8001")
			if err == nil {
				t.Fatal("backendURLsFromEnv() error = nil, want invalid URL error")
			}
			if !strings.Contains(err.Error(), envName) {
				t.Errorf("error %q does not identify environment variable %q", err, envName)
			}
			if !strings.Contains(err.Error(), `"`+test.invalidEntry+`"`) {
				t.Errorf("error %q does not identify invalid entry %q", err, test.invalidEntry)
			}
		})
	}
}

func TestGatewayHealthAwareRoutingRecoveryAndLiveness(t *testing.T) {
	firstServer, first := newProductTestBackend(t, "first")
	defer firstServer.Close()
	secondServer, second := newProductTestBackend(t, "second")
	defer secondServer.Close()
	t.Setenv("PRODUCT_SERVICE_URLS", firstServer.URL+","+secondServer.URL)

	productSelector, productBackends, err := newServiceProxy("PRODUCT_SERVICE_URLS", "http://localhost:8001")
	if err != nil {
		t.Fatal(err)
	}
	checker, err := health.NewChecker(productBackends, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	handler := newHandler(productSelector, http.NotFoundHandler(), http.NotFoundHandler())
	for i, want := range []string{"first", "second", "first", "second", "first", "second"} {
		recorder := performProductRequest(handler)
		if recorder.Code != http.StatusOK || recorder.Body.String() != want {
			t.Fatalf("request %d = status %d, body %q; want 200 and %q", i+1, recorder.Code, recorder.Body.String(), want)
		}
	}
	if got := first.requests.Load(); got != 3 {
		t.Fatalf("first instance received %d application requests, want 3", got)
	}
	if got := second.requests.Load(); got != 3 {
		t.Fatalf("second instance received %d application requests, want 3", got)
	}

	first.healthStatus.Store(http.StatusInternalServerError)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 3; i++ {
		recorder := performProductRequest(handler)
		if recorder.Code != http.StatusOK || recorder.Body.String() != "second" {
			t.Fatalf("request with first instance unhealthy = status %d, body %q; want second instance", recorder.Code, recorder.Body.String())
		}
	}

	first.healthStatus.Store(http.StatusOK)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	if recorder := performProductRequest(handler); recorder.Code != http.StatusOK || recorder.Body.String() != "first" {
		t.Fatalf("recovered instance response = status %d, body %q; want first instance", recorder.Code, recorder.Body.String())
	}

	first.healthStatus.Store(http.StatusServiceUnavailable)
	second.healthStatus.Store(http.StatusServiceUnavailable)
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}
	unavailable := performProductRequest(handler)
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("all-unhealthy status = %d, want %d", unavailable.Code, http.StatusServiceUnavailable)
	}

	healthRecorder := httptest.NewRecorder()
	handler.ServeHTTP(healthRecorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if healthRecorder.Code != http.StatusOK {
		t.Fatalf("gateway health status = %d, want %d while services are unhealthy", healthRecorder.Code, http.StatusOK)
	}

	unknown := httptest.NewRecorder()
	handler.ServeHTTP(unknown, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if unknown.Code != http.StatusNotFound {
		t.Fatalf("unknown route status = %d, want %d", unknown.Code, http.StatusNotFound)
	}
}

func TestGatewayPreserves502ForBackendUnreachableAfterHealthCheck(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		_, _ = fmt.Fprint(w, "product")
	}))
	t.Setenv("PRODUCT_SERVICE_URLS", backend.URL)

	selector, targets, err := newServiceProxy("PRODUCT_SERVICE_URLS", "http://localhost:8001")
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	checker, err := health.NewChecker(targets, time.Hour, time.Second)
	if err != nil {
		backend.Close()
		t.Fatal(err)
	}
	if err := checker.CheckNow(context.Background()); err != nil {
		backend.Close()
		t.Fatal(err)
	}
	backend.Close()

	handler := newHandler(selector, http.NotFoundHandler(), http.NotFoundHandler())
	recorder := performProductRequest(handler)
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("unreachable backend status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}

type productTestBackend struct {
	healthStatus atomic.Int32
	requests     atomic.Int32
}

func newProductTestBackend(t *testing.T, name string) (*httptest.Server, *productTestBackend) {
	t.Helper()
	state := &productTestBackend{}
	state.healthStatus.Store(http.StatusOK)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			if r.Method != http.MethodGet {
				t.Errorf("health probe method = %q, want GET", r.Method)
			}
			w.WriteHeader(int(state.healthStatus.Load()))
			return
		}
		if got, want := r.URL.RequestURI(), "/api/v1/products?source=test"; got != want {
			t.Errorf("application request URI = %q, want %q", got, want)
		}
		state.requests.Add(1)
		_, _ = fmt.Fprint(w, name)
	}))
	return server, state
}

func performProductRequest(handler http.Handler) *httptest.ResponseRecorder {
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(
		http.MethodGet,
		"/api/v1/products?source=test",
		nil,
	))
	return recorder
}
