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

	"intelligate/gateway/internal/auth"
	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/health"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/ratelimit"
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

func TestUpstreamTimeoutFromEnvUsesDefaultWhenUnset(t *testing.T) {
	previous, wasSet := os.LookupEnv(upstreamTimeoutEnv)
	if err := os.Unsetenv(upstreamTimeoutEnv); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if wasSet {
			_ = os.Setenv(upstreamTimeoutEnv, previous)
		} else {
			_ = os.Unsetenv(upstreamTimeoutEnv)
		}
	})

	timeout, err := upstreamTimeoutFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if timeout != proxy.DefaultUpstreamTimeout {
		t.Fatalf("timeout = %s, want default %s", timeout, proxy.DefaultUpstreamTimeout)
	}
}

func TestUpstreamTimeoutFromEnvAcceptsCustomDuration(t *testing.T) {
	t.Setenv(upstreamTimeoutEnv, "1500ms")

	timeout, err := upstreamTimeoutFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := 1500 * time.Millisecond; timeout != want {
		t.Fatalf("timeout = %s, want %s", timeout, want)
	}
}

func TestUpstreamTimeoutFromEnvRejectsInvalidOrNonPositiveDuration(t *testing.T) {
	for _, value := range []string{"not-a-duration", "", "0s", "-1s"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(upstreamTimeoutEnv, value)

			_, err := upstreamTimeoutFromEnv()
			if err == nil {
				t.Fatal("upstreamTimeoutFromEnv() error = nil, want invalid timeout error")
			}
			if !strings.Contains(err.Error(), upstreamTimeoutEnv) {
				t.Errorf("error %q does not identify %s", err, upstreamTimeoutEnv)
			}
		})
	}
}

func TestCircuitBreakerConfigFromEnvUsesDefaultsWhenUnset(t *testing.T) {
	unsetEnvironment(t,
		breakerFailureThresholdEnv,
		breakerOpenCooldownEnv,
		breakerHalfOpenMaxRequestsEnv,
		breakerHalfOpenSuccessThresholdEnv,
	)

	config, err := circuitBreakerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if want := circuitbreaker.DefaultConfig(); config != want {
		t.Fatalf("config = %+v, want defaults %+v", config, want)
	}
}

func TestCircuitBreakerConfigFromEnvAcceptsOverrides(t *testing.T) {
	t.Setenv(breakerFailureThresholdEnv, "7")
	t.Setenv(breakerOpenCooldownEnv, "12s")
	t.Setenv(breakerHalfOpenMaxRequestsEnv, "2")
	t.Setenv(breakerHalfOpenSuccessThresholdEnv, "3")

	config, err := circuitBreakerConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	want := circuitbreaker.Config{
		FailureThreshold:         7,
		OpenCooldown:             12 * time.Second,
		HalfOpenMaxRequests:      2,
		HalfOpenSuccessThreshold: 3,
	}
	if config != want {
		t.Fatalf("config = %+v, want %+v", config, want)
	}
}

func TestCircuitBreakerConfigFromEnvRejectsInvalidValues(t *testing.T) {
	unsetEnvironment(t,
		breakerFailureThresholdEnv,
		breakerOpenCooldownEnv,
		breakerHalfOpenMaxRequestsEnv,
		breakerHalfOpenSuccessThresholdEnv,
	)
	tests := []struct {
		envName string
		value   string
	}{
		{envName: breakerFailureThresholdEnv, value: "invalid"},
		{envName: breakerFailureThresholdEnv, value: "0"},
		{envName: breakerFailureThresholdEnv, value: "-1"},
		{envName: breakerOpenCooldownEnv, value: "invalid"},
		{envName: breakerOpenCooldownEnv, value: "0s"},
		{envName: breakerOpenCooldownEnv, value: "-1s"},
		{envName: breakerHalfOpenMaxRequestsEnv, value: "0"},
		{envName: breakerHalfOpenSuccessThresholdEnv, value: "-1"},
	}

	for _, test := range tests {
		t.Run(test.envName+"="+test.value, func(t *testing.T) {
			t.Setenv(test.envName, test.value)
			if _, err := circuitBreakerConfigFromEnv(); err == nil {
				t.Fatal("circuitBreakerConfigFromEnv() error = nil, want invalid config error")
			} else if !strings.Contains(err.Error(), test.envName) {
				t.Errorf("error %q does not identify %s", err, test.envName)
			}
		})
	}
}

func TestAuthenticationSettingsFromEnvRequiresKeysByDefault(t *testing.T) {
	unsetEnvironment(t, authEnabledEnv, apiKeysEnv, apiKeyRolesEnv)

	_, err := authenticationSettingsFromEnv()
	if err == nil {
		t.Fatal("authenticationSettingsFromEnv() error = nil, want missing-key error")
	}
	if !strings.Contains(err.Error(), apiKeysEnv) {
		t.Errorf("error %q does not identify %s", err, apiKeysEnv)
	}
}

func TestAuthenticationSettingsFromEnvAcceptsAndTrimsMultipleKeys(t *testing.T) {
	unsetEnvironment(t, apiKeyRolesEnv)
	t.Setenv(authEnabledEnv, "true")
	t.Setenv(apiKeysEnv, "  first-key , second-key  ")

	settings, err := authenticationSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.Authenticator == nil {
		t.Fatalf("authentication settings = %+v, want enabled authenticator", settings)
	}
	handler := settings.Authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, key := range []string{"first-key", "second-key"} {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/products", nil)
		request.Header.Set(auth.APIKeyHeader, key)
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Errorf("legacy key %q status = %d, want 204 admin access", key, recorder.Code)
		}
	}
}

func TestAuthenticationSettingsFromEnvAcceptsRoleMapping(t *testing.T) {
	unsetEnvironment(t, apiKeysEnv)
	t.Setenv(authEnabledEnv, "true")
	t.Setenv(apiKeyRolesEnv, `{"admin-key":"admin","read-key":"read","write-key":"write"}`)

	settings, err := authenticationSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.Authenticator == nil {
		t.Fatalf("authentication settings = %+v, want role-aware authenticator", settings)
	}
}

func TestAuthenticationSettingsFromEnvCanBeExplicitlyDisabled(t *testing.T) {
	unsetEnvironment(t, authEnabledEnv, apiKeysEnv, apiKeyRolesEnv)
	t.Setenv(authEnabledEnv, "false")

	settings, err := authenticationSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled || settings.Authenticator != nil {
		t.Fatalf("authentication settings = %+v, want disabled without authenticator", settings)
	}
}

func TestAuthenticationSettingsFromEnvRejectsMalformedConfiguration(t *testing.T) {
	unsetEnvironment(t, apiKeyRolesEnv)
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "whitespace", value: " \t "},
		{name: "empty entry", value: "first-key,,second-key"},
		{name: "trailing delimiter", value: "first-key,"},
		{name: "internal whitespace", value: "bad key"},
		{name: "control character", value: "bad\nkey"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv(authEnabledEnv, "true")
			t.Setenv(apiKeysEnv, test.value)
			_, err := authenticationSettingsFromEnv()
			if err == nil {
				t.Fatal("authenticationSettingsFromEnv() error = nil, want malformed-key error")
			}
			if !strings.Contains(err.Error(), apiKeysEnv) {
				t.Errorf("error %q does not identify %s", err, apiKeysEnv)
			}
			if test.value != "" && strings.Contains(err.Error(), test.value) {
				t.Errorf("error exposed configured key data: %q", err)
			}
		})
	}
}

func TestAuthenticationSettingsFromEnvRejectsMalformedRoleConfiguration(t *testing.T) {
	unsetEnvironment(t, apiKeysEnv)
	tests := []struct {
		name  string
		value string
	}{
		{name: "empty", value: ""},
		{name: "invalid JSON", value: `{"read-key":`},
		{name: "not an object", value: `["read-key"]`},
		{name: "empty object", value: `{}`},
		{name: "unknown role", value: `{"read-key":"owner"}`},
		{name: "duplicate key", value: `{"key":"admin","key":"read"}`},
		{name: "normalized duplicate", value: `{" key ":"read","key":"write"}`},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetEnvironment(t, apiKeysEnv)
			t.Setenv(authEnabledEnv, "true")
			t.Setenv(apiKeyRolesEnv, test.value)
			_, err := authenticationSettingsFromEnv()
			if err == nil {
				t.Fatal("authenticationSettingsFromEnv() error = nil, want invalid role configuration error")
			}
			if !strings.Contains(err.Error(), apiKeyRolesEnv) {
				t.Errorf("error %q does not identify %s", err, apiKeyRolesEnv)
			}
			if strings.Contains(err.Error(), "read-key") || strings.Contains(err.Error(), "owner") {
				t.Errorf("error exposed role mapping data: %q", err)
			}
		})
	}
}

func TestAuthenticationSettingsFromEnvRejectsBothKeyConfigurations(t *testing.T) {
	unsetEnvironment(t, authEnabledEnv, apiKeysEnv, apiKeyRolesEnv)
	t.Setenv(authEnabledEnv, "true")
	t.Setenv(apiKeysEnv, "legacy-key")
	t.Setenv(apiKeyRolesEnv, `{"admin-key":"admin"}`)

	_, err := authenticationSettingsFromEnv()
	if err == nil {
		t.Fatal("authenticationSettingsFromEnv() error = nil, want ambiguous configuration error")
	}
	if !strings.Contains(err.Error(), apiKeysEnv) || !strings.Contains(err.Error(), apiKeyRolesEnv) {
		t.Fatalf("error %q must identify both conflicting environment variables", err)
	}
	if strings.Contains(err.Error(), "legacy-key") || strings.Contains(err.Error(), "admin-key") {
		t.Errorf("error exposed configured key data: %q", err)
	}
}

func TestAuthenticationSettingsFromEnvRejectsInvalidEnabledFlag(t *testing.T) {
	unsetEnvironment(t, apiKeysEnv, apiKeyRolesEnv)
	t.Setenv(authEnabledEnv, "sometimes")
	t.Setenv(apiKeysEnv, "placeholder-key")

	if _, err := authenticationSettingsFromEnv(); err == nil || !strings.Contains(err.Error(), authEnabledEnv) {
		t.Fatalf("authenticationSettingsFromEnv() error = %v, want invalid %s error", err, authEnabledEnv)
	}
}

func TestRateLimitSettingsFromEnvUsesDefaultsWhenUnset(t *testing.T) {
	unsetEnvironment(
		t,
		rateLimitEnabledEnv,
		rateLimitRateEnv,
		rateLimitBurstEnv,
		rateLimitStoreEnv,
		redisAddrEnv,
		redisPasswordEnv,
		redisDBEnv,
		rateLimitRedisFailureModeEnv,
	)

	settings, err := rateLimitSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled {
		t.Fatal("rate limiting is disabled by default")
	}
	if want := ratelimit.DefaultConfig(); settings.Config != want {
		t.Fatalf("rate-limit config = %+v, want defaults %+v", settings.Config, want)
	}
	if settings.StoreName != "memory" {
		t.Fatalf("rate-limit store = %q, want memory", settings.StoreName)
	}
	if settings.Redis.Addr != "localhost:6379" || settings.Redis.DB != 0 {
		t.Fatalf("Redis settings = %+v, want localhost:6379 and DB 0", settings.Redis)
	}
}

func TestListenAddressFromEnvDefaultsAndSupportsLocalInstances(t *testing.T) {
	unsetEnvironment(t, listenAddrEnv)
	if address, err := listenAddressFromEnv(); err != nil || address != ":8080" {
		t.Fatalf("default listen address = %q, err=%v, want :8080", address, err)
	}
	t.Setenv(listenAddrEnv, "127.0.0.1:8081")
	if address, err := listenAddressFromEnv(); err != nil || address != "127.0.0.1:8081" {
		t.Fatalf("configured listen address = %q, err=%v, want 127.0.0.1:8081", address, err)
	}
}

func TestListenAddressFromEnvRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"8081", "localhost:0", "localhost:65536", "localhost:+8081", " localhost:8081"} {
		t.Run(value, func(t *testing.T) {
			t.Setenv(listenAddrEnv, value)
			if _, err := listenAddressFromEnv(); err == nil {
				t.Fatalf("listenAddressFromEnv(%q) error = nil, want validation error", value)
			}
		})
	}
}

func TestRateLimitSettingsFromEnvAcceptsOverrides(t *testing.T) {
	unsetEnvironment(t, rateLimitStoreEnv, redisAddrEnv, redisPasswordEnv, redisDBEnv, rateLimitRedisFailureModeEnv)
	t.Setenv(rateLimitEnabledEnv, "true")
	t.Setenv(rateLimitRateEnv, "12.5")
	t.Setenv(rateLimitBurstEnv, "30")

	settings, err := rateLimitSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if !settings.Enabled || settings.Config.Rate != 12.5 || settings.Config.Burst != 30 {
		t.Fatalf("rate-limit settings = %+v, want enabled with rate 12.5 and burst 30", settings)
	}
}

func TestRateLimitSettingsFromEnvAcceptsRedisConfiguration(t *testing.T) {
	unsetEnvironment(t, rateLimitEnabledEnv, rateLimitStoreEnv, redisAddrEnv, redisPasswordEnv, redisDBEnv, rateLimitRedisFailureModeEnv)
	t.Setenv(rateLimitStoreEnv, "redis")
	t.Setenv(redisAddrEnv, "127.0.0.1:6380")
	t.Setenv(redisPasswordEnv, "local-placeholder")
	t.Setenv(redisDBEnv, "2")
	t.Setenv(rateLimitRedisFailureModeEnv, "open")

	settings, err := rateLimitSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if settings.StoreName != "redis" || settings.Redis.Addr != "127.0.0.1:6380" ||
		settings.Redis.Password != "local-placeholder" || settings.Redis.DB != 2 ||
		settings.Config.FailureMode != ratelimit.FailureModeOpen {
		t.Fatalf("Redis rate-limit settings = %+v", settings)
	}
}

func TestRateLimitSettingsFromEnvRejectsInvalidEnabledConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		value   string
	}{
		{name: "invalid enabled flag", envName: rateLimitEnabledEnv, value: "sometimes"},
		{name: "invalid rate", envName: rateLimitRateEnv, value: "fast"},
		{name: "empty rate", envName: rateLimitRateEnv, value: ""},
		{name: "zero rate", envName: rateLimitRateEnv, value: "0"},
		{name: "negative rate", envName: rateLimitRateEnv, value: "-1"},
		{name: "NaN rate", envName: rateLimitRateEnv, value: "NaN"},
		{name: "infinite rate", envName: rateLimitRateEnv, value: "+Inf"},
		{name: "invalid burst", envName: rateLimitBurstEnv, value: "many"},
		{name: "zero burst", envName: rateLimitBurstEnv, value: "0"},
		{name: "negative burst", envName: rateLimitBurstEnv, value: "-2"},
		{name: "infinite burst", envName: rateLimitBurstEnv, value: "Inf"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetEnvironment(
				t,
				rateLimitEnabledEnv,
				rateLimitRateEnv,
				rateLimitBurstEnv,
				rateLimitStoreEnv,
				redisAddrEnv,
				redisPasswordEnv,
				redisDBEnv,
				rateLimitRedisFailureModeEnv,
			)
			t.Setenv(rateLimitEnabledEnv, "true")
			t.Setenv(test.envName, test.value)
			_, err := rateLimitSettingsFromEnv()
			if err == nil {
				t.Fatal("rateLimitSettingsFromEnv() error = nil, want configuration error")
			}
			if !strings.Contains(err.Error(), test.envName) {
				t.Errorf("error %q does not identify %s", err, test.envName)
			}
		})
	}
}

func TestRateLimitSettingsFromEnvRejectsInvalidRedisConfiguration(t *testing.T) {
	tests := []struct {
		name    string
		envName string
		value   string
	}{
		{name: "unknown store", envName: rateLimitStoreEnv, value: "local"},
		{name: "malformed address", envName: redisAddrEnv, value: "redis://localhost:6379"},
		{name: "non-numeric port", envName: redisAddrEnv, value: "localhost:+6379"},
		{name: "invalid host", envName: redisAddrEnv, value: "bad host:6379"},
		{name: "empty address", envName: redisAddrEnv, value: ""},
		{name: "invalid database", envName: redisDBEnv, value: "not-a-number"},
		{name: "negative database", envName: redisDBEnv, value: "-1"},
		{name: "invalid failure mode", envName: rateLimitRedisFailureModeEnv, value: "fallback"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			unsetEnvironment(t, rateLimitEnabledEnv, rateLimitStoreEnv, redisAddrEnv, redisPasswordEnv, redisDBEnv, rateLimitRedisFailureModeEnv)
			t.Setenv(test.envName, test.value)
			if test.envName == redisAddrEnv || test.envName == redisDBEnv {
				t.Setenv(rateLimitStoreEnv, "redis")
			}
			_, err := rateLimitSettingsFromEnv()
			if err == nil || !strings.Contains(err.Error(), test.envName) {
				t.Fatalf("rateLimitSettingsFromEnv() error = %v, want an error identifying %s", err, test.envName)
			}
		})
	}
}

func TestDisabledRateLimitIgnoresRateValuesAndPreservesHandlerBehavior(t *testing.T) {
	unsetEnvironment(t, rateLimitStoreEnv, redisAddrEnv, redisPasswordEnv, redisDBEnv, rateLimitRedisFailureModeEnv)
	t.Setenv(rateLimitEnabledEnv, "false")
	t.Setenv(rateLimitRateEnv, "invalid")
	t.Setenv(rateLimitBurstEnv, "0")
	settings, err := rateLimitSettingsFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if settings.Enabled {
		t.Fatal("rate-limit settings are enabled despite explicit false")
	}

	var downstreamCalls atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		downstreamCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	var limiter *ratelimit.Limiter
	if settings.Enabled {
		limiter, err = ratelimit.New(settings.Config)
		if err != nil {
			t.Fatal(err)
		}
	}
	handler := newHandlerWithRateLimiter(backend, backend, backend, limiter)
	for i := 0; i < 2; i++ {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
		request.RemoteAddr = "192.0.2.10:1111"
		handler.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusNoContent {
			t.Fatalf("disabled request %d status = %d, want 204", i+1, recorder.Code)
		}
	}
	if downstreamCalls.Load() != 2 {
		t.Fatalf("downstream calls with limiter disabled = %d, want 2", downstreamCalls.Load())
	}
}

func TestNewRateLimiterReportsRedisConnectivityFailure(t *testing.T) {
	options := ratelimit.DefaultRedisOptions()
	options.Addr = "127.0.0.1:1"
	options.OperationTimeout = 100 * time.Millisecond
	options.StartupTimeout = 200 * time.Millisecond
	settings := rateLimitSettings{
		Enabled:   true,
		Config:    ratelimit.DefaultConfig(),
		StoreName: "redis",
		Redis:     options,
	}

	if _, err := newRateLimiter(settings); err == nil || !strings.Contains(err.Error(), "startup connectivity check failed") {
		t.Fatalf("newRateLimiter() error = %v, want Redis startup connectivity failure", err)
	}
}

func TestNewRateLimiterInitializesRedisWithoutMemoryCleanup(t *testing.T) {
	options := ratelimit.DefaultRedisOptions()
	if address := os.Getenv("INTELLIGATE_TEST_REDIS_ADDR"); address != "" {
		options.Addr = address
	}
	options.Namespace = fmt.Sprintf("gateway-test-%x", time.Now().UnixNano())
	settings := rateLimitSettings{
		Enabled:   true,
		Config:    ratelimit.DefaultConfig(),
		StoreName: "redis",
		Redis:     options,
	}
	limiter, err := newRateLimiter(settings)
	if err != nil {
		if strings.Contains(err.Error(), "startup connectivity check failed") {
			t.Skipf("Redis gateway initialization test skipped because Redis is unavailable: %v", err)
		}
		t.Fatal(err)
	}
	defer func() {
		if err := limiter.Close(); err != nil {
			t.Errorf("close Redis limiter: %v", err)
		}
	}()
	if limiter.NeedsCleanup() {
		t.Fatal("Redis-backed limiter must not start the in-memory cleanup loop")
	}
}

func TestRateLimiterRejectsBeforeRoutingAndExemptsGatewayHealth(t *testing.T) {
	config := ratelimit.DefaultConfig()
	config.Rate = 0.000001
	config.Burst = 1
	limiter, err := ratelimit.New(config)
	if err != nil {
		t.Fatal(err)
	}
	var selectorCalls atomic.Int32
	selector := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		selectorCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	})
	handler := newHandlerWithRateLimiter(selector, http.NotFoundHandler(), http.NotFoundHandler(), limiter)
	perform := func(path, forwarded string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "198.51.100.40:1234"
		request.Header.Set("X-Forwarded-For", forwarded)
		request.Header.Set("X-Real-IP", forwarded)
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	if response := perform("/api/v1/products", "192.0.2.1"); response.Code != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", response.Code)
	}
	if response := perform("/health", "192.0.2.2"); response.Code != http.StatusOK {
		t.Fatalf("health status = %d, want 200 despite exhausted bucket", response.Code)
	}
	rejected := perform("/api/v1/products", "192.0.2.2")
	if rejected.Code != http.StatusTooManyRequests {
		t.Fatalf("second API request status = %d, want 429; forwarded IP must not select a new bucket", rejected.Code)
	}
	if selectorCalls.Load() != 1 {
		t.Fatalf("selector calls = %d, want only the accepted request", selectorCalls.Load())
	}
}

func TestUnauthenticatedRequestDoesNotConsumeRateLimitTokenOrReachBackend(t *testing.T) {
	authenticator, err := auth.New([]string{"local-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	config := ratelimit.DefaultConfig()
	config.Rate = 1
	config.Burst = 1
	limiter, err := ratelimit.New(config)
	if err != nil {
		t.Fatal(err)
	}
	var backendCalls atomic.Int32
	backend := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		backendCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	})
	handler := newHandlerWithAuthentication(backend, backend, backend, authenticator, limiter)

	perform := func(path, key string) *httptest.ResponseRecorder {
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.RemoteAddr = "192.0.2.50:4321"
		if key != "" {
			request.Header.Set(auth.APIKeyHeader, key)
		}
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	unauthorized := perform("/api/v1/future", "")
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized future route status = %d, want 401", unauthorized.Code)
	}
	if backendCalls.Load() != 0 {
		t.Fatal("unauthorized request reached a backend")
	}

	accepted := perform("/api/v1/products", "local-test-key")
	if accepted.Code != http.StatusNoContent {
		t.Fatalf("first authorized request status = %d, want 204; authentication must precede rate limiting", accepted.Code)
	}
	if backendCalls.Load() != 1 {
		t.Fatalf("backend calls after authorized request = %d, want 1", backendCalls.Load())
	}

	limited := perform("/api/v1/products", "local-test-key")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("second authorized request status = %d, want 429", limited.Code)
	}
	if backendCalls.Load() != 1 {
		t.Fatalf("backend calls after rate-limited request = %d, want 1", backendCalls.Load())
	}

	healthResponse := perform("/health", "")
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("unauthenticated health status = %d, want 200", healthResponse.Code)
	}
}

func TestReadRoleCannotReachUpstreamAndForbiddenRequestDoesNotConsumeToken(t *testing.T) {
	type observedRequest struct {
		method          string
		requestURI      string
		apiKey          string
		role            string
		userRole        string
		intelligateRole string
	}
	upstreamRequests := make(chan observedRequest, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		upstreamRequests <- observedRequest{
			method:          r.Method,
			requestURI:      r.URL.RequestURI(),
			apiKey:          r.Header.Get(auth.APIKeyHeader),
			role:            r.Header.Get("X-Role"),
			userRole:        r.Header.Get("X-User-Role"),
			intelligateRole: r.Header.Get("X-Intelligate-Role"),
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Setenv("PRODUCT_SERVICE_URLS", upstream.URL)

	selector, backends, err := newServiceProxy(
		"PRODUCT_SERVICE_URLS",
		"http://localhost:8001",
		time.Second,
		circuitbreaker.DefaultConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := health.NewChecker(backends, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	authenticator, err := auth.NewWithRoles(map[string]auth.Role{"read-role-key": auth.RoleRead})
	if err != nil {
		t.Fatal(err)
	}
	rateLimitConfig := ratelimit.DefaultConfig()
	rateLimitConfig.Rate = 1
	rateLimitConfig.Burst = 1
	limiter, err := ratelimit.New(rateLimitConfig)
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandlerWithAuthentication(
		selector,
		http.NotFoundHandler(),
		http.NotFoundHandler(),
		authenticator,
		limiter,
	)
	perform := func(method string, target string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(method, target, nil)
		request.RemoteAddr = "192.0.2.61:4321"
		request.Header.Set(auth.APIKeyHeader, "read-role-key")
		request.Header.Set("X-Role", "admin")
		request.Header.Set("X-User-Role", "admin")
		request.Header.Set("X-Intelligate-Role", "admin")
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, request)
		return recorder
	}

	forbidden := perform(http.MethodPost, "/api/v1/products?role=admin")
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-role POST status = %d, want 403", forbidden.Code)
	}
	select {
	case got := <-upstreamRequests:
		t.Fatalf("forbidden request reached upstream: %+v", got)
	default:
	}

	allowed := perform(http.MethodGet, "/api/v1/products?role=admin")
	if allowed.Code != http.StatusNoContent {
		t.Fatalf("read-role GET after forbidden POST status = %d, want 204; forbidden request must not consume a token", allowed.Code)
	}
	select {
	case got := <-upstreamRequests:
		if got.method != http.MethodGet || got.requestURI != "/api/v1/products?role=admin" {
			t.Errorf("upstream request = %+v, want GET with preserved query", got)
		}
		if got.apiKey != "" || got.role != "" || got.userRole != "" || got.intelligateRole != "" {
			t.Errorf("upstream received authentication metadata: %+v", got)
		}
	default:
		t.Fatal("authorized GET did not reach upstream")
	}

	limited := perform(http.MethodGet, "/api/v1/products")
	if limited.Code != http.StatusTooManyRequests {
		t.Fatalf("second authorized GET status = %d, want 429", limited.Code)
	}
	healthResponse := httptest.NewRecorder()
	handler.ServeHTTP(healthResponse, httptest.NewRequest(http.MethodGet, "/health", nil))
	if healthResponse.Code != http.StatusOK {
		t.Fatalf("public health status = %d, want 200", healthResponse.Code)
	}
}

func TestGatewayStripsAPIKeyBeforeProxyingToUpstream(t *testing.T) {
	receivedAPIKey := make(chan string, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/health" {
			w.WriteHeader(http.StatusOK)
			return
		}
		receivedAPIKey <- r.Header.Get(auth.APIKeyHeader)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer upstream.Close()
	t.Setenv("PRODUCT_SERVICE_URLS", upstream.URL)

	selector, backends, err := newServiceProxy(
		"PRODUCT_SERVICE_URLS",
		"http://localhost:8001",
		time.Second,
		circuitbreaker.DefaultConfig(),
	)
	if err != nil {
		t.Fatal(err)
	}
	checker, err := health.NewChecker(backends, time.Hour, time.Second)
	if err != nil {
		t.Fatal(err)
	}
	if err := checker.CheckNow(context.Background()); err != nil {
		t.Fatal(err)
	}

	authenticator, err := auth.New([]string{"valid-upstream-test-key"})
	if err != nil {
		t.Fatal(err)
	}
	handler := newHandlerWithAuthentication(
		selector,
		http.NotFoundHandler(),
		http.NotFoundHandler(),
		authenticator,
		nil,
	)
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products", nil)
	request.Header.Set(auth.APIKeyHeader, "valid-upstream-test-key")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusNoContent {
		t.Fatalf("gateway status = %d, want 204", recorder.Code)
	}
	select {
	case got := <-receivedAPIKey:
		if got != "" {
			t.Errorf("upstream received X-API-Key %q, want no API key header", got)
		}
	case <-time.After(time.Second):
		t.Fatal("upstream did not receive the proxied request")
	}
}

func TestGatewayHealthAwareRoutingRecoveryAndLiveness(t *testing.T) {
	firstServer, first := newProductTestBackend(t, "first")
	defer firstServer.Close()
	secondServer, second := newProductTestBackend(t, "second")
	defer secondServer.Close()
	t.Setenv("PRODUCT_SERVICE_URLS", firstServer.URL+","+secondServer.URL)

	productSelector, productBackends, err := newServiceProxy(
		"PRODUCT_SERVICE_URLS",
		"http://localhost:8001",
		time.Second,
		circuitbreaker.DefaultConfig(),
	)
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

	selector, targets, err := newServiceProxy(
		"PRODUCT_SERVICE_URLS",
		"http://localhost:8001",
		time.Second,
		circuitbreaker.DefaultConfig(),
	)
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

func unsetEnvironment(t *testing.T, names ...string) {
	t.Helper()
	previous := make(map[string]string, len(names))
	wasSet := make(map[string]bool, len(names))
	for _, name := range names {
		previous[name], wasSet[name] = os.LookupEnv(name)
		if err := os.Unsetenv(name); err != nil {
			t.Fatalf("unset %s: %v", name, err)
		}
	}
	t.Cleanup(func() {
		for _, name := range names {
			if wasSet[name] {
				_ = os.Setenv(name, previous[name])
			} else {
				_ = os.Unsetenv(name)
			}
		}
	})
}
