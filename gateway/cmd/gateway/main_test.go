package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
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
