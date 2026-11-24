package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/proxy"
)

func TestRetryPolicyValidationAndDefaults(t *testing.T) {
	policy := DefaultRetryPolicy(5 * time.Second)
	if policy.MaxRetries != 2 || len(policy.Backoffs) != 2 ||
		policy.Backoffs[0] != 100*time.Millisecond ||
		policy.Backoffs[1] != 200*time.Millisecond {
		t.Fatalf("DefaultRetryPolicy() = %+v, want 2 retries with 100ms and 200ms backoffs", policy)
	}

	tests := []RetryPolicy{
		{MaxRetries: -1, UpstreamTimeout: time.Second},
		{MaxRetries: 1, Backoffs: []time.Duration{time.Millisecond, time.Millisecond}, UpstreamTimeout: time.Second},
		{MaxRetries: 1, Backoffs: []time.Duration{-time.Millisecond}, UpstreamTimeout: time.Second},
		{MaxRetries: 0, UpstreamTimeout: 0},
		{MaxRetries: 1, Backoffs: []time.Duration{time.Millisecond}, UpstreamTimeout: time.Duration(1<<63 - 1)},
	}
	for i, invalid := range tests {
		if _, err := NewRoundRobinWithRetryPolicy(invalid, identifiedBackend("backend")); err == nil {
			t.Errorf("invalid retry policy %d was accepted: %+v", i, invalid)
		}
	}
}

func TestOnlyGETAndHEADAreRetried(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)

	tests := []struct {
		method       string
		wantAttempts int32
	}{
		{method: http.MethodGet, wantAttempts: 3},
		{method: http.MethodHead, wantAttempts: 3},
		{method: http.MethodPost, wantAttempts: 1},
		{method: http.MethodPut, wantAttempts: 1},
		{method: http.MethodPatch, wantAttempts: 1},
		{method: http.MethodDelete, wantAttempts: 1},
		{method: http.MethodOptions, wantAttempts: 1},
	}
	for _, test := range tests {
		t.Run(test.method, func(t *testing.T) {
			var attempts atomic.Int32
			selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
				attempt: func(_ http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
					attempts.Add(1)
					return testAttemptFailure(http.StatusBadGateway)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			selector.wait = func(context.Context, time.Duration) bool { return true }
			recorder := httptest.NewRecorder()
			selector.ServeHTTP(recorder, httptest.NewRequest(test.method, "/", nil))
			if got := attempts.Load(); got != test.wantAttempts {
				t.Fatalf("attempt count = %d, want %d", got, test.wantAttempts)
			}
		})
	}
}

func TestRetryBackoffOrderAndCancellation(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	var attempts atomic.Int32
	selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			attempts.Add(1)
			return testAttemptFailure(http.StatusBadGateway)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var waits []time.Duration
	selector.wait = func(_ context.Context, delay time.Duration) bool {
		waits = append(waits, delay)
		return true
	}
	selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	if got, want := fmt.Sprint(waits), fmt.Sprint([]time.Duration{100 * time.Millisecond, 200 * time.Millisecond}); got != want {
		t.Fatalf("backoff sequence = %s, want %s", got, want)
	}
	if got := attempts.Load(); got != 3 {
		t.Fatalf("attempt count = %d, want 3", got)
	}

	attempts.Store(0)
	selector, err = NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			attempts.Add(1)
			return testAttemptFailure(http.StatusBadGateway)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	waitStarted := make(chan struct{})
	selector.wait = func(ctx context.Context, _ time.Duration) bool {
		close(waitStarted)
		<-ctx.Done()
		return false
	}
	clientCtx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		selector.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil).WithContext(clientCtx))
		close(done)
	}()
	<-waitStarted
	cancel()
	<-done
	if got := attempts.Load(); got != 1 {
		t.Fatalf("attempt count after client cancellation during backoff = %d, want 1", got)
	}
}

func TestSingleBackendRetriesAreBoundedAndEachPermitCompletesOnce(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}
	selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, req *http.Request) *proxy.AttemptError {
			permit := circuitbreaker.PermitFromContext(req.Context())
			if permit == nil {
				t.Fatal("attempt has no circuit-breaker permit")
			}
			permit.Failure()
			return &proxy.AttemptError{StatusCode: http.StatusBadGateway, Err: errors.New("transport failed")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 4
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, breaker); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("exhausted status = %d, want %d", recorder.Code, http.StatusBadGateway)
	}
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after three failed attempts = %s, want closed", got)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected fourth request before failure threshold")
	}
	permit.Failure()
	if got := breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after fourth failure = %s, want open", got)
	}
}

func TestRetriesPreferDifferentEligibleBackendAndPreserveRequest(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}
	var firstAttempts, secondAttempts atomic.Int32
	first := &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, req *http.Request) *proxy.AttemptError {
			firstAttempts.Add(1)
			if req.URL.RequestURI() != "/api/v1/products/42?source=retry" ||
				req.Header.Get("X-Request-ID") != "retry-test" {
				t.Errorf("retry request path/query/header = %q/%q, want preserved values", req.URL.RequestURI(), req.Header.Get("X-Request-ID"))
			}
			return testAttemptFailure(http.StatusBadGateway)
		},
	}
	second := &scriptedAttemptHandler{
		attempt: func(w http.ResponseWriter, req *http.Request) *proxy.AttemptError {
			secondAttempts.Add(1)
			if req.URL.RequestURI() != "/api/v1/products/42?source=retry" ||
				req.Header.Get("X-Request-ID") != "retry-test" {
				t.Errorf("retry request path/query/header = %q/%q, want preserved values", req.URL.RequestURI(), req.Header.Get("X-Request-ID"))
			}
			_, _ = fmt.Fprint(w, "second")
			return nil
		},
	}
	selector, err := NewRoundRobinWithRetryPolicy(policy, first, second)
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	request := httptest.NewRequest(http.MethodGet, "/api/v1/products/42?source=retry", nil)
	request.Header.Set("X-Request-ID", "retry-test")
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK || recorder.Body.String() != "second" {
		t.Fatalf("response = %d %q, want 200 from second backend", recorder.Code, recorder.Body.String())
	}
	if firstAttempts.Load() != 1 || secondAttempts.Load() != 1 {
		t.Fatalf("attempt counts = first:%d second:%d, want one each", firstAttempts.Load(), secondAttempts.Load())
	}
}

func TestRetryReusesSingleBackendAndReturnsLastError(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}
	var attempts atomic.Int32
	statuses := []int{http.StatusBadGateway, http.StatusGatewayTimeout, http.StatusGatewayTimeout}
	selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			index := int(attempts.Add(1)) - 1
			return testAttemptFailure(statuses[index])
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))

	if got := attempts.Load(); got != 3 {
		t.Fatalf("single backend attempt count = %d, want 3", got)
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("exhausted status = %d, want last attempt status %d", recorder.Code, http.StatusGatewayTimeout)
	}
}

func TestRetryAfterFailureWithNoEligibleBackendReturnsLastError(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}
	var attempts atomic.Int32
	selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, req *http.Request) *proxy.AttemptError {
			attempts.Add(1)
			circuitbreaker.PermitFromContext(req.Context()).Failure()
			return &proxy.AttemptError{StatusCode: http.StatusBadGateway, Err: errors.New("connection failed")}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, breaker); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusBadGateway {
		t.Fatalf("response status = %d, want last upstream failure 502, not local 503", recorder.Code)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempt count = %d, want 1 after breaker opens", attempts.Load())
	}
}

func TestUnhealthyAndOpenBackendsAreNeverSelected(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	var calls [3]atomic.Int32
	handlers := make([]http.Handler, len(calls))
	for i := range handlers {
		index := i
		handlers[i] = &scriptedAttemptHandler{attempt: func(w http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			calls[index].Add(1)
			_, _ = fmt.Fprint(w, fmt.Sprintf("backend-%d", index))
			return nil
		}}
	}
	selector, err := NewRoundRobinWithRetryPolicy(policy, handlers...)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetHealthy(0, false); err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Hour
	openBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := openBreaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected setup permit")
	}
	permit.Failure()
	if err := selector.SetCircuitBreaker(1, openBreaker); err != nil {
		t.Fatal(err)
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "backend-2" {
		t.Fatalf("response = %d %q, want backend-2", recorder.Code, recorder.Body.String())
	}
	if calls[0].Load() != 0 || calls[1].Load() != 0 || calls[2].Load() != 1 {
		t.Fatalf("backend calls = [%d %d %d], want [0 0 1]", calls[0].Load(), calls[1].Load(), calls[2].Load())
	}
}

func TestHalfOpenFailureCompletesBeforeRetrySelectsAnotherBackend(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}
	first := &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, req *http.Request) *proxy.AttemptError {
			circuitbreaker.PermitFromContext(req.Context()).Failure()
			return &proxy.AttemptError{StatusCode: http.StatusBadGateway, Err: errors.New("trial failed")}
		},
	}
	var secondCalls atomic.Int32
	second := &scriptedAttemptHandler{
		attempt: func(w http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			secondCalls.Add(1)
			_, _ = fmt.Fprint(w, "second")
			return nil
		},
	}
	selector, err := NewRoundRobinWithRetryPolicy(policy, first, second)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Nanosecond
	firstBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	openingPermit, allowed := firstBreaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected setup permit")
	}
	openingPermit.Failure()
	timer := time.NewTimer(config.OpenCooldown)
	<-timer.C
	timer.Stop()
	if got := firstBreaker.State(); got != circuitbreaker.StateHalfOpen {
		t.Fatalf("breaker state before trial = %s, want half-open", got)
	}
	if err := selector.SetCircuitBreaker(0, firstBreaker); err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool {
		if got := firstBreaker.State(); got != circuitbreaker.StateOpen {
			t.Errorf("first breaker state during backoff = %s, want reopened", got)
		}
		return true
	}

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK || recorder.Body.String() != "second" {
		t.Fatalf("response = %d %q, want successful second backend", recorder.Code, recorder.Body.String())
	}
	if got := firstBreaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("failed half-open trial state = %s, want open", got)
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("second backend calls = %d, want 1", secondCalls.Load())
	}
}

func TestRetryBudgetExpiresDuringBackoffAsGatewayTimeout(t *testing.T) {
	policy := RetryPolicy{
		MaxRetries:      2,
		Backoffs:        []time.Duration{time.Millisecond, 2 * time.Millisecond},
		UpstreamTimeout: 10 * time.Millisecond,
	}
	var attempts atomic.Int32
	selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
		attempt: func(_ http.ResponseWriter, _ *http.Request) *proxy.AttemptError {
			attempts.Add(1)
			return testAttemptFailure(http.StatusBadGateway)
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(ctx context.Context, _ time.Duration) bool {
		<-ctx.Done()
		return false
	}
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("retry-budget response status = %d, want %d", recorder.Code, http.StatusGatewayTimeout)
	}
	if attempts.Load() != 1 {
		t.Fatalf("attempt count after retry-budget expiration = %d, want 1", attempts.Load())
	}
}

func TestClientDeadlineDoesNotRetryOrBecomeGatewayTimeout(t *testing.T) {
	var upstreamCalls atomic.Int32
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()
	handler, err := proxy.New(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(handler)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, breaker); err != nil {
		t.Fatal(err)
	}
	clientCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(clientCtx))
	if recorder.Body.Len() != 0 {
		t.Fatalf("client-deadline response body = %q, want no gateway response", recorder.Body.String())
	}
	if upstreamCalls.Load() != 0 {
		t.Fatalf("upstream calls = %d after client deadline, want 0", upstreamCalls.Load())
	}
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after client deadline = %s, want closed", got)
	}
}

func TestClientDeadlineDuringAttemptDoesNotRetryOrFailBreaker(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	var secondCalls atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		firstStarted <- struct{}{}
		<-req.Context().Done()
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondServer.Close()
	first, err := proxy.New(firstServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.New(secondServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(first, second)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	firstBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, firstBreaker); err != nil {
		t.Fatal(err)
	}
	clientCtx, cancel := context.WithTimeout(context.Background(), 250*time.Millisecond)
	defer cancel()
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(clientCtx))
		close(done)
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		t.Fatal("first backend did not receive request")
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not stop after client deadline")
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("second backend calls = %d, want 0 after client deadline", secondCalls.Load())
	}
	if got := firstBreaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after client deadline = %s, want closed", got)
	}
}

func TestGETBodyRetriesOnlyWhenReplayable(t *testing.T) {
	policy := DefaultRetryPolicy(time.Second)
	policy.Backoffs = []time.Duration{0, 0}

	for _, replayable := range []bool{false, true} {
		t.Run(fmt.Sprintf("replayable-%t", replayable), func(t *testing.T) {
			var attempts atomic.Int32
			selector, err := NewRoundRobinWithRetryPolicy(policy, &scriptedAttemptHandler{
				attempt: func(_ http.ResponseWriter, req *http.Request) *proxy.AttemptError {
					attempts.Add(1)
					if req.Body != nil && req.Body != http.NoBody {
						body, readErr := io.ReadAll(req.Body)
						if readErr != nil {
							t.Errorf("read request body: %v", readErr)
						}
						if string(body) != "payload" {
							t.Errorf("request body = %q, want payload", body)
						}
					}
					return testAttemptFailure(http.StatusBadGateway)
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			selector.wait = func(context.Context, time.Duration) bool { return true }
			request := httptest.NewRequest(http.MethodGet, "/", strings.NewReader("payload"))
			if replayable {
				request.GetBody = func() (io.ReadCloser, error) {
					return io.NopCloser(strings.NewReader("payload")), nil
				}
			} else {
				request.GetBody = nil
			}
			selector.ServeHTTP(httptest.NewRecorder(), request)
			want := int32(1)
			if replayable {
				want = 3
			}
			if got := attempts.Load(); got != want {
				t.Fatalf("attempt count = %d, want %d", got, want)
			}
		})
	}
}

func TestRequestForAttemptRejectsNilReplayBody(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", strings.NewReader("payload"))
	request.GetBody = func() (io.ReadCloser, error) { return nil, nil }
	if _, err := requestForAttempt(request, context.Background(), 1); err == nil {
		t.Fatal("requestForAttempt() error = nil, want nil replay body error")
	}
}

func TestApplicationHTTPFailuresAreNotRetried(t *testing.T) {
	for _, status := range []int{
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout,
	} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var firstCalls, secondCalls atomic.Int32
			firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				firstCalls.Add(1)
				w.WriteHeader(status)
				_, _ = fmt.Fprint(w, "application response")
			}))
			defer firstServer.Close()
			secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				secondCalls.Add(1)
				w.WriteHeader(http.StatusOK)
			}))
			defer secondServer.Close()
			first, err := proxy.New(firstServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			second, err := proxy.New(secondServer.URL)
			if err != nil {
				t.Fatal(err)
			}
			selector, err := NewRoundRobin(first, second)
			if err != nil {
				t.Fatal(err)
			}
			selector.wait = func(context.Context, time.Duration) bool { return true }

			recorder := httptest.NewRecorder()
			selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
			if recorder.Code != status || recorder.Body.String() != "application response" {
				t.Fatalf("response = %d %q, want application status %d", recorder.Code, recorder.Body.String(), status)
			}
			if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
				t.Fatalf("backend calls = first:%d second:%d, want 1 and 0", firstCalls.Load(), secondCalls.Load())
			}
		})
	}
}

func TestBodyStreamingFailureAfterHeadersIsNotRetried(t *testing.T) {
	var firstCalls, secondCalls atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		firstCalls.Add(1)
		w.Header().Set("Content-Length", "100")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("partial"))
		w.(http.Flusher).Flush()
		<-req.Context().Done()
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondServer.Close()
	const upstreamTimeout = 100 * time.Millisecond
	first, err := proxy.NewWithTimeout(firstServer.URL, upstreamTimeout)
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.NewWithTimeout(secondServer.URL, upstreamTimeout)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobinWithRetryPolicy(
		DefaultRetryPolicy(upstreamTimeout),
		first,
		second,
	)
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }

	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream response status = %d, want original upstream 200", recorder.Code)
	}
	if firstCalls.Load() != 1 || secondCalls.Load() != 0 {
		t.Fatalf("backend calls = first:%d second:%d, want no retry after headers", firstCalls.Load(), secondCalls.Load())
	}
}

func TestGatewayUpstreamTimeoutRetriesDifferentBackend(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	var secondCalls atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		firstStarted <- struct{}{}
		<-req.Context().Done()
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		_, _ = fmt.Fprint(w, "recovered")
	}))
	defer secondServer.Close()

	const upstreamTimeout = 100 * time.Millisecond
	first, err := proxy.NewWithTimeout(firstServer.URL, upstreamTimeout)
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.NewWithTimeout(secondServer.URL, upstreamTimeout)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobinWithRetryPolicy(DefaultRetryPolicy(upstreamTimeout), first, second)
	if err != nil {
		t.Fatal(err)
	}
	selector.wait = func(context.Context, time.Duration) bool { return true }
	recorder := httptest.NewRecorder()
	selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	select {
	case <-firstStarted:
	default:
		t.Fatal("first backend was not attempted")
	}
	if recorder.Code != http.StatusOK || recorder.Body.String() != "recovered" {
		t.Fatalf("response = %d %q, want successful retry response", recorder.Code, recorder.Body.String())
	}
	if secondCalls.Load() != 1 {
		t.Fatalf("second backend calls = %d, want 1", secondCalls.Load())
	}
}

func TestClientCancellationDuringAttemptDoesNotRetryOrFailBreaker(t *testing.T) {
	firstStarted := make(chan struct{}, 1)
	var secondCalls atomic.Int32
	firstServer := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, req *http.Request) {
		firstStarted <- struct{}{}
		<-req.Context().Done()
	}))
	defer firstServer.Close()
	secondServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		secondCalls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer secondServer.Close()
	first, err := proxy.New(firstServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	second, err := proxy.New(secondServer.URL)
	if err != nil {
		t.Fatal(err)
	}
	selector, err := NewRoundRobin(first, second)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	firstBreaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := selector.SetCircuitBreaker(0, firstBreaker); err != nil {
		t.Fatal(err)
	}
	clientCtx, cancel := context.WithCancel(context.Background())
	recorder := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		selector.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil).WithContext(clientCtx))
		close(done)
	}()
	select {
	case <-firstStarted:
	case <-time.After(time.Second):
		cancel()
		t.Fatal("first backend did not receive request")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("request did not stop after client cancellation")
	}
	if secondCalls.Load() != 0 {
		t.Fatalf("second backend calls = %d, want 0 after client cancellation", secondCalls.Load())
	}
	if got := firstBreaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after client cancellation = %s, want closed", got)
	}
}

type scriptedAttemptHandler struct {
	attempt func(http.ResponseWriter, *http.Request) *proxy.AttemptError
}

func (h *scriptedAttemptHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	if err := h.ServeAttempt(w, req); err != nil {
		err.WriteHTTPError(w)
	}
}

func (h *scriptedAttemptHandler) ServeAttempt(w http.ResponseWriter, req *http.Request) *proxy.AttemptError {
	return h.attempt(w, req)
}

func testAttemptFailure(status int) *proxy.AttemptError {
	return &proxy.AttemptError{StatusCode: status, Err: errors.New("upstream transport failed")}
}
