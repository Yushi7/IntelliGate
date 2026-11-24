package router

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestRouterSelectsMatchingServiceAndRejectsPartialPrefix(t *testing.T) {
	products := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusAccepted)
	})
	orders := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusCreated)
	})
	users := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	handler := New(
		Route{Prefix: "/api/v1/products", Handler: products},
		Route{Prefix: "/api/v1/orders", Handler: orders},
		Route{Prefix: "/api/v1/users", Handler: users},
	)

	tests := []struct {
		path       string
		statusCode int
	}{
		{path: "/api/v1/products", statusCode: http.StatusAccepted},
		{path: "/api/v1/products/42", statusCode: http.StatusAccepted},
		{path: "/api/v1/products-extra", statusCode: http.StatusNotFound},
		{path: "/api/v1/orders/42", statusCode: http.StatusCreated},
		{path: "/api/v1/users/42", statusCode: http.StatusNoContent},
		{path: "/unknown", statusCode: http.StatusNotFound},
	}

	for _, test := range tests {
		t.Run(test.path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, test.path, nil))
			if recorder.Code != test.statusCode {
				t.Fatalf("status = %d, want %d", recorder.Code, test.statusCode)
			}
		})
	}
}
