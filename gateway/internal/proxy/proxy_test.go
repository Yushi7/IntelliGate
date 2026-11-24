package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
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
