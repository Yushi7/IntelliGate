package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"reflect"
	"strings"
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
