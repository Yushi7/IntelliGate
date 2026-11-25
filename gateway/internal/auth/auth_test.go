package auth

import (
	"encoding/json"
	"fmt"
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

func TestDenialObserverDistinguishesAuthenticationAndAuthorization(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{"read-only-key": RoleRead})
	if err != nil {
		t.Fatal(err)
	}
	var denials []string
	authenticator.SetDenialObserver(func(stage string, status int) {
		denials = append(denials, fmt.Sprintf("%s:%d", stage, status))
	})
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("missing key status = %d, want 401", unauthorized.Code)
	}
	forbidden := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/api/v1/products", nil)
	request.Header.Set(APIKeyHeader, "read-only-key")
	handler.ServeHTTP(forbidden, request)
	if forbidden.Code != http.StatusForbidden {
		t.Fatalf("read-role POST status = %d, want 403", forbidden.Code)
	}
	if got, want := fmt.Sprint(denials), "[authentication:401 authorization:403]"; got != want {
		t.Fatalf("denial observer events = %s, want %s", got, want)
	}
}

func TestDenialObserverPanicDoesNotChangeAuthenticationOutcome(t *testing.T) {
	authenticator, err := New([]string{"valid-key"})
	if err != nil {
		t.Fatal(err)
	}
	authenticator.SetDenialObserver(func(string, int) { panic("observer failure") })
	recorder := httptest.NewRecorder()
	authenticator.Middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Fatal("unauthorized request reached downstream")
	})).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/products", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("status with panicking observer = %d, want 401", recorder.Code)
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

func TestParseRoleMappingAcceptsTrimmedKeysAndKnownRoles(t *testing.T) {
	roles, err := ParseRoleMapping(`{" admin-key ":"admin","read-key":"read","write-key":"write"}`)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]Role{
		"admin-key": RoleAdmin,
		"read-key":  RoleRead,
		"write-key": RoleWrite,
	}
	if !reflect.DeepEqual(roles, want) {
		t.Fatalf("roles = %#v, want %#v", roles, want)
	}
}

func TestParseRoleMappingRejectsMalformedDuplicateAndUnknownEntries(t *testing.T) {
	for _, value := range []string{
		"",
		" ",
		"[]",
		"{}",
		`{"key":"unknown"}`,
		`{"key":1}`,
		`{"key":"read",}`,
		`{"key":"read"} trailing`,
		`{"same-key":"admin","same-key":"read"}`,
		`{" same-key ":"read","same-key":"write"}`,
		`{"bad,key":"admin"}`,
		`{"":"read"}`,
	} {
		t.Run(value, func(t *testing.T) {
			_, err := ParseRoleMapping(value)
			if err == nil {
				t.Fatal("ParseRoleMapping() error = nil, want invalid mapping error")
			}
			if strings.Contains(err.Error(), "same-key") || strings.Contains(err.Error(), "bad,key") {
				t.Errorf("error exposed configured API key: %q", err)
			}
		})
	}
}

func TestNewWithRolesRejectsEmptyDuplicateAndUnknownRoles(t *testing.T) {
	for _, roles := range []map[string]Role{
		nil,
		{},
		{"key": "owner"},
		{"": RoleAdmin},
		{"bad,key": RoleAdmin},
		{"same-key": RoleAdmin, " same-key ": RoleRead},
	} {
		if _, err := NewWithRoles(roles); err == nil {
			t.Errorf("NewWithRoles(%#v) error = nil, want invalid mapping error", roles)
		}
	}
}

func TestAdminRoleAllowsSupportedMethodsAndRejectsUnsupportedMethods(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{"admin-key": RoleAdmin})
	if err != nil {
		t.Fatal(err)
	}
	assertRoleMethods(t, authenticator, "admin-key",
		[]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"},
		[]string{"OPTIONS", "TRACE", "CONNECT"},
	)
}

func TestReadRoleAllowsOnlyGETAndHEAD(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{"read-key": RoleRead})
	if err != nil {
		t.Fatal(err)
	}
	assertRoleMethods(t, authenticator, "read-key",
		[]string{"GET", "HEAD"},
		[]string{"POST", "PUT", "PATCH", "DELETE", "OPTIONS"},
	)
}

func TestWriteRoleAllowsSupportedMethods(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{"write-key": RoleWrite})
	if err != nil {
		t.Fatal(err)
	}
	assertRoleMethods(t, authenticator, "write-key",
		[]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"},
		[]string{"OPTIONS", "TRACE", "CONNECT"},
	)
}

func TestLegacyAPIKeysRetainAdminAccess(t *testing.T) {
	authenticator, err := New([]string{"legacy-key"})
	if err != nil {
		t.Fatal(err)
	}
	assertRoleMethods(t, authenticator, "legacy-key",
		[]string{"GET", "HEAD", "POST", "PUT", "PATCH", "DELETE"},
		[]string{"OPTIONS", "TRACE", "CONNECT"},
	)
}

func TestClientRoleHeadersAndQueryCannotEscalateReadRole(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{"read-key": RoleRead})
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	request := httptest.NewRequest(http.MethodPost, "/api/v1/products?role=admin", nil)
	request.Header.Set(APIKeyHeader, "read-key")
	request.Header.Set("X-Role", "admin")
	request.Header.Set("X-User-Role", "admin")
	request.Header.Set("X-Intelligate-Role", "admin")
	recorder := httptest.NewRecorder()

	handler.ServeHTTP(recorder, request)

	assertForbidden(t, recorder)
}

func TestConcurrentRequestsDoNotLeakRolesBetweenClients(t *testing.T) {
	authenticator, err := NewWithRoles(map[string]Role{
		"admin-key": RoleAdmin,
		"read-key":  RoleRead,
	})
	if err != nil {
		t.Fatal(err)
	}
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		role, ok := roleFromContext(req.Context())
		if !ok || role != RoleAdmin {
			t.Errorf("downstream role = %q, present %t; want admin", role, ok)
		}
		if req.Header.Get("X-Role") != "" {
			t.Errorf("client role header was forwarded: %q", req.Header.Get("X-Role"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))

	const workers = 24
	const requestsPerWorker = 20
	var wait sync.WaitGroup
	wait.Add(workers)
	for worker := 0; worker < workers; worker++ {
		go func(worker int) {
			defer wait.Done()
			key := "admin-key"
			wantStatus := http.StatusNoContent
			if worker%2 == 1 {
				key = "read-key"
				wantStatus = http.StatusForbidden
			}
			for i := 0; i < requestsPerWorker; i++ {
				request := httptest.NewRequest(http.MethodPost, "/api/v1/products?role=admin", nil)
				request.Header.Set(APIKeyHeader, key)
				request.Header.Set("X-Role", "admin")
				recorder := httptest.NewRecorder()
				handler.ServeHTTP(recorder, request)
				if recorder.Code != wantStatus {
					t.Errorf("key %q response status = %d, want %d", key, recorder.Code, wantStatus)
					return
				}
			}
		}(worker)
	}
	wait.Wait()
}

func assertRoleMethods(t *testing.T, authenticator *Authenticator, key string, allowed, denied []string) {
	t.Helper()
	handler := authenticator.Middleware(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	for _, method := range allowed {
		t.Run(method+"_allowed", func(t *testing.T) {
			request := httptest.NewRequest(method, "/api/v1/products", nil)
			request.Header.Set(APIKeyHeader, key)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			if recorder.Code != http.StatusNoContent {
				t.Fatalf("status = %d, want 204", recorder.Code)
			}
		})
	}
	for _, method := range denied {
		t.Run(method+"_denied", func(t *testing.T) {
			request := httptest.NewRequest(method, "/api/v1/products", nil)
			request.Header.Set(APIKeyHeader, key)
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, request)
			assertForbidden(t, recorder)
		})
	}
}

func assertForbidden(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", recorder.Code)
	}
	var body struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("response is not valid JSON: %v", err)
	}
	if body.Error != "forbidden" {
		t.Fatalf("error = %q, want forbidden", body.Error)
	}
}
