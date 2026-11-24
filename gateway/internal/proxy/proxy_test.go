package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestProxyPreservesPathQueryAndResponse(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got, want := r.URL.RequestURI(), "/api/v1/products/demo?source=test"; got != want {
			t.Errorf("backend request URI = %q, want %q", got, want)
		}
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte(`{"proxied":true}`))
	}))
	defer backend.Close()

	handler, err := New(backend.URL)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products/demo?source=test", nil))

	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
	if got, want := recorder.Body.String(), `{"proxied":true}`; got != want {
		t.Fatalf("response body = %q, want %q", got, want)
	}
}

func TestProxyAcceptsUpstreamResponseWithinTimeout(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	}))
	defer backend.Close()

	handler, err := NewWithTimeout(backend.URL, time.Second)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if recorder.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusAccepted)
	}
}

func TestProxyTimesOutSlowUpstreamWithoutBlockingConcurrentRequest(t *testing.T) {
	const timeout = 2 * time.Second
	slowStarted := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			close(slowStarted)
			<-r.Context().Done()
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler, err := NewWithTimeout(backend.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}

	slowResponse := httptest.NewRecorder()
	slowDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(slowResponse, httptest.NewRequest(http.MethodGet, "/slow", nil))
		close(slowDone)
	}()
	select {
	case <-slowStarted:
	case <-time.After(time.Second):
		t.Fatal("slow upstream request did not start")
	}

	fastResponse := httptest.NewRecorder()
	fastDone := make(chan struct{})
	go func() {
		handler.ServeHTTP(fastResponse, httptest.NewRequest(http.MethodGet, "/fast", nil))
		close(fastDone)
	}()
	select {
	case <-fastDone:
	case <-slowDone:
		t.Fatal("slow request timed out before the concurrent fast request completed")
	case <-time.After(timeout):
		t.Fatal("concurrent fast request was blocked by the slow request")
	}
	if fastResponse.Code != http.StatusOK {
		t.Fatalf("fast response status = %d, want %d", fastResponse.Code, http.StatusOK)
	}

	select {
	case <-slowDone:
	case <-time.After(timeout):
		t.Fatal("slow request did not finish after its timeout")
	}
	if slowResponse.Code != http.StatusGatewayTimeout {
		t.Fatalf("slow response status = %d, want %d", slowResponse.Code, http.StatusGatewayTimeout)
	}
	if got, want := slowResponse.Body.String(), "upstream request timed out\n"; got != want {
		t.Fatalf("slow response body = %q, want %q", got, want)
	}
}

func TestNewBackendWithTimeoutRejectsNonPositiveTimeout(t *testing.T) {
	if _, err := NewBackendWithTimeout("http://localhost:8001", 0); err == nil {
		t.Fatal("NewBackendWithTimeout() error = nil, want non-positive timeout error")
	}
}

func TestProxyReturnsBadGatewayWhenBackendIsUnavailable(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {}))
	target := backend.URL
	backend.Close()

	handler, err := New(target)
	if err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))

	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
}
