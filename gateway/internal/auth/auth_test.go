package auth

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

func TestParseKeysTrimsAndValidatesEntries(t *testing.T) {
	keys, err := ParseKeys("  first-key \t, second-key ")
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"first-key", "second-key"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("keys = %#v, want %#v", keys, want)
	}

	for _, value := range []string{"", " \t ", ",", "first-key,", ",second-key", "first,,second", "white space", "line\nbreak", "ключ"} {
		t.Run(value, func(t *testing.T) {
			if _, err := ParseKeys(value); err == nil {
				t.Fatal("ParseKeys() error = nil, want malformed-key error")
			}
		})
	}
}

func TestNewRejectsEmptyOrMalformedKeys(t *testing.T) {
	for _, keys := range [][]string{nil, {}, {""}, {"  "}, {"invalid key"}} {
		if _, err := New(keys); err == nil {
			t.Errorf("New(%#v) error = nil, want invalid-key error", keys)
		}
	}
}

func TestMiddlewareAcceptsEachConfiguredKey(t *testing.T) {
	authenticator, err := New([]string{"first-key", "second-key"})
	if err != nil {
		t.Fatal(err)
	}
	var downstreamCalls atomic.Int32
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if got := req.Header.Get(APIKeyHeader); got != "" {
			t.Errorf("API key was forwarded downstream: %q", got)
		}
		downstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))

	for _, key := range []string{"first-key", "second-key"} {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		request.Header.Set(APIKeyHeader, key)
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Errorf("key %q status = %d, want 204", key, recorder.Code)
		}
	}
	if got := downstreamCalls.Load(); got != 2 {
		t.Fatalf("downstream calls = %d, want 2", got)
	}
}

func TestMiddlewareRejectsMissingInvalidAndRepeatedKeysConsistently(t *testing.T) {
	authenticator, err := New([]string{"valid-key"})
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthorized request reached downstream handler")
	}))

	for _, test := range []struct {
		name string
		keys []string
	}{
		{name: "missing"},
		{name: "invalid", keys: []string{"wrong-key"}},
		{name: "multiple header values", keys: []string{"valid-key", "wrong-key"}},
		{name: "comma-combined key", keys: []string{"valid-key,another-key"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			request := httptest.NewRequest(http.MethodGet, "/api/v1/future", nil)
			for _, key := range test.keys {
				request.Header.Add(APIKeyHeader, key)
			}
			handler.ServeHTTP(recorder, request)

			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("status = %d, want 401", recorder.Code)
			}
			if got, want := recorder.Header().Get("Content-Type"), "application/json"; got != want {
				t.Errorf("Content-Type = %q, want %q", got, want)
			}
			var body struct {
				Error string `json:"error"`
			}
			if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
				t.Fatalf("response is not valid JSON: %v", err)
			}
			if body.Error != "unauthorized" {
				t.Errorf("error = %q, want unauthorized", body.Error)
			}
			if strings.Contains(recorder.Body.String(), "wrong-key") ||
				strings.Contains(recorder.Body.String(), "valid-key") ||
				strings.Contains(recorder.Body.String(), "another-key") {
				t.Errorf("response exposed an API key: %q", recorder.Body.String())
			}
		})
	}
}

func TestMiddlewareLeavesHealthAndNonAPIPathsPublic(t *testing.T) {
	authenticator, err := New([]string{"configured-key"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))

	for _, path := range []string{"/health", "/api/v10/products", "/unknown"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Errorf("path %q status = %d, want 200", path, recorder.Code)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("downstream calls = %d, want 3", got)
	}
}

func TestMiddlewareValidatesConcurrentRequestsSafely(t *testing.T) {
	authenticator, err := New([]string{"first-key", "second-key"})
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	const workers = 16
	const requestsPerWorker = 25
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer wait.Done()
			key := "first-key"
			if worker%2 == 1 {
				key = "second-key"
			}
			for i := 0; i < requestsPerWorker; i++ {
				recorder := httptest.NewRecorder()
				request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
				request.Header.Set(APIKeyHeader, key)
				handler.ServeHTTP(recorder, request)
				if recorder.Code != http.StatusNoContent {
					t.Errorf("concurrent status = %d, want 204", recorder.Code)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
}
