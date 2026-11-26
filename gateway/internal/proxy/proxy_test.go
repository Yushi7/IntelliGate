package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"intelligate/gateway/internal/circuitbreaker"
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

	proxyBackend, err := NewBackendWithTimeout(backend.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}

	slowResponse := httptest.NewRecorder()
	slowDone := make(chan struct{})
	go func() {
		proxyBackend.Handler.ServeHTTP(slowResponse, httptest.NewRequest(http.MethodGet, "/slow", nil))
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
		proxyBackend.Handler.ServeHTTP(fastResponse, httptest.NewRequest(http.MethodGet, "/fast", nil))
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
	case <-time.After(timeout + time.Second):
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

func TestApplicationStatusesDoNotCountAsCircuitBreakerFailures(t *testing.T) {
	statuses := []int{
		http.StatusBadRequest,
		http.StatusNotFound,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusGatewayTimeout,
	}
	nextStatus := 0
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(statuses[nextStatus])
		_, _ = fmt.Fprint(w, "application response")
		nextStatus++
	}))
	defer backend.Close()

	proxyBackend, err := NewBackend(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	breaker, err := circuitbreaker.New(circuitbreaker.DefaultConfig())
	if err != nil {
		t.Fatal(err)
	}

	for _, status := range statuses {
		permit, allowed := breaker.TryAcquire()
		if !allowed {
			t.Fatalf("breaker rejected application status %d", status)
		}
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request = request.WithContext(circuitbreaker.WithPermit(request.Context(), permit))
		recorder := httptest.NewRecorder()
		attemptHandler, ok := proxyBackend.Handler.(interface {
			ServeAttemptResult(http.ResponseWriter, *http.Request) (*AttemptError, bool)
		})
		if !ok {
			t.Fatal("proxy handler does not expose attempt result")
		}
		_, successful := attemptHandler.ServeAttemptResult(recorder, request)
		if !successful {
			t.Fatal("completed application response was marked as an unsuccessful upstream attempt")
		}
		if recorder.Code != status {
			t.Errorf("application status = %d, want %d", recorder.Code, status)
		}
		if got := breaker.State(); got != circuitbreaker.StateClosed {
			t.Fatalf("application status %d changed breaker state to %s, want closed", status, got)
		}
	}
}

func TestGatewayTimeoutCountsAsCircuitBreakerFailure(t *testing.T) {
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer backend.Close()

	const timeout = 100 * time.Millisecond
	handler, err := NewWithTimeout(backend.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	clientCtx, cancelClient := context.WithDeadline(context.Background(), time.Now().Add(time.Hour))
	defer cancelClient()
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(clientCtx)
	request = request.WithContext(circuitbreaker.WithPermit(request.Context(), permit))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	select {
	case <-started:
	default:
		t.Fatal("timed-out request did not reach upstream")
	}
	if recorder.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", recorder.Code, http.StatusGatewayTimeout)
	}
	if got := breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after gateway timeout = %s, want open", got)
	}
}

func TestResponseBodyTransportErrorCountsAsCircuitBreakerFailure(t *testing.T) {
	started := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.(http.Flusher).Flush()
		started <- struct{}{}
		<-r.Context().Done()
	}))
	defer backend.Close()

	const timeout = 100 * time.Millisecond
	handler, err := NewWithTimeout(backend.URL, timeout)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request = request.WithContext(circuitbreaker.WithPermit(request.Context(), permit))
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	select {
	case <-started:
	default:
		t.Fatal("response-body request did not reach upstream")
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("status after response headers = %d, want original upstream status %d", recorder.Code, http.StatusOK)
	}
	if got := breaker.State(); got != circuitbreaker.StateOpen {
		t.Fatalf("breaker state after response-body timeout = %s, want open", got)
	}
}

func TestCanceledRequestAbandonsCircuitBreakerPermit(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler, err := New(backend.URL)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
		circuitbreaker.WithPermit(ctx, permit),
	)
	handler.ServeHTTP(httptest.NewRecorder(), request)
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after canceled request = %s, want closed", got)
	}
}

func TestClientCancellationBeforeUpstreamCompletionAbandonsPermit(t *testing.T) {
	breaker, err := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:         1,
		OpenCooldown:             time.Hour,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}

	started := make(chan struct{})
	transport := breakerAwareTransport{next: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
		close(started)
		<-req.Context().Done()
		return nil, req.Context().Err()
	})}
	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstreamCtx, cancelUpstream := withUpstreamTimeout(clientCtx, time.Hour)
	defer cancelUpstream()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
		circuitbreaker.WithPermit(upstreamCtx, permit),
	)

	result := make(chan error, 1)
	go func() {
		_, err := transport.RoundTrip(request)
		result <- err
	}()
	<-started
	cancelClient()
	if err := <-result; !errors.Is(err, context.Canceled) {
		t.Fatalf("RoundTrip error = %v, want context.Canceled", err)
	}
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after client cancellation = %s, want closed", got)
	}
}

func TestClientDeadlineBeforeGatewayTimeoutDoesNotFailBreaker(t *testing.T) {
	upstreamRequests := make(chan struct{}, 1)
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		upstreamRequests <- struct{}{}
		w.WriteHeader(http.StatusOK)
	}))
	defer backend.Close()

	handler, err := NewWithTimeout(backend.URL, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	clientCtx, cancelClient := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancelClient()
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
		circuitbreaker.WithPermit(clientCtx, permit),
	)
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)

	if recorder.Body.Len() != 0 {
		t.Fatalf("client-deadline response body = %q, want no gateway error response", recorder.Body.String())
	}
	select {
	case <-upstreamRequests:
		t.Fatal("upstream received a request after the client deadline")
	default:
	}
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after client deadline = %s, want closed", got)
	}
}

func TestClientCancellationDuringResponseBodyStreamingAbandonsPermit(t *testing.T) {
	breaker, err := circuitbreaker.New(circuitbreaker.Config{
		FailureThreshold:         1,
		OpenCooldown:             time.Hour,
		HalfOpenMaxRequests:      1,
		HalfOpenSuccessThreshold: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	permit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstreamCtx, cancelUpstream := withUpstreamTimeout(clientCtx, time.Hour)
	defer cancelUpstream()
	body := &contextWaitingBody{ctx: upstreamCtx, started: make(chan struct{})}
	transport := breakerAwareTransport{next: roundTripperFunc(func(*http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: body}, nil
	})}
	request := httptest.NewRequest(http.MethodGet, "/", nil).WithContext(
		circuitbreaker.WithPermit(upstreamCtx, permit),
	)
	response, err := transport.RoundTrip(request)
	if err != nil {
		t.Fatal(err)
	}

	readResult := make(chan error, 1)
	go func() {
		_, err := response.Body.Read(make([]byte, 1))
		readResult <- err
	}()
	<-body.started
	cancelClient()
	if err := <-readResult; !errors.Is(err, context.Canceled) {
		t.Fatalf("response body read error = %v, want context.Canceled", err)
	}
	_ = response.Body.Close()
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after canceled body stream = %s, want closed", got)
	}
}

func TestClientCancellationReleasesHalfOpenPermitOnDeferredBodyClose(t *testing.T) {
	config := circuitbreaker.DefaultConfig()
	config.FailureThreshold = 1
	config.OpenCooldown = time.Nanosecond
	breaker, err := circuitbreaker.New(config)
	if err != nil {
		t.Fatal(err)
	}
	openingPermit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("closed breaker rejected request")
	}
	openingPermit.Failure()
	timer := time.NewTimer(config.OpenCooldown)
	<-timer.C
	timer.Stop()

	trialPermit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("breaker rejected half-open trial")
	}
	if _, allowed := breaker.TryAcquire(); allowed {
		t.Fatal("breaker admitted a second concurrent half-open trial")
	}

	clientCtx, cancelClient := context.WithCancel(context.Background())
	defer cancelClient()
	upstreamCtx, cancelUpstream := withUpstreamTimeout(clientCtx, time.Hour)
	defer cancelUpstream()
	body := observedResponseBody{
		ReadCloser: io.NopCloser(strings.NewReader("")),
		permit:     trialPermit,
		ctx:        upstreamCtx,
	}
	cancelClient()
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}

	replacementPermit, allowed := breaker.TryAcquire()
	if !allowed {
		t.Fatal("client cancellation did not release half-open trial capacity")
	}
	replacementPermit.Success()
	if got := breaker.State(); got != circuitbreaker.StateClosed {
		t.Fatalf("breaker state after replacement trial = %s, want closed", got)
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

func TestTransportFailureRecordedWithoutBreakerPermit(t *testing.T) {
	outcome := &attemptOutcome{}
	request := httptest.NewRequest(http.MethodGet, "http://backend.test", nil)
	request = request.WithContext(context.WithValue(request.Context(), attemptOutcomeKey{}, outcome))
	transport := breakerAwareTransport{next: roundTripperFunc(func(*http.Request) (*http.Response, error) { return nil, errors.New("dial failed") })}
	if _, err := transport.RoundTrip(request); err == nil {
		t.Fatal("RoundTrip error = nil, want transport failure")
	}
	if !outcome.failed.Load() {
		t.Fatal("transport failure was not recorded without breaker permit")
	}
}

func TestMeasuredAttemptReportsTTFBAndCompletedBody(t *testing.T) {
	b, err := NewBackend("http://backend.test")
	if err != nil {
		t.Fatal(err)
	}
	handler := b.Handler.(*backendHandler)
	handler.reverseProxy.Transport = breakerAwareTransport{next: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
		// Model a measurable upstream wait without sleeping or relying on local network scheduling.
		started := time.Now()
		for time.Since(started) < time.Millisecond {
		}
		return &http.Response{
			StatusCode: http.StatusServiceUnavailable,
			Status:     "503 Service Unavailable",
			Header:     make(http.Header),
			Body:       io.NopCloser(strings.NewReader("body")),
			Request:    request,
		}, nil
	})}
	measured := b.Handler.(interface {
		ServeMeasuredAttemptResult(http.ResponseWriter, *http.Request) MeasuredAttemptResult
	})
	recorder := httptest.NewRecorder()
	result := measured.ServeMeasuredAttemptResult(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if result.Err != nil || !result.BodyCompleted || !result.TTFBAvailable || result.TTFB <= 0 || result.TransportFailure {
		t.Fatalf("measured result = %+v, want completed 503 with available TTFB and no transport failure", result)
	}
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("forwarded response status = %d, want 503", recorder.Code)
	}
}

func TestCloseBeforeEOFDoesNotCreateSuccessOrBackendPenalty(t *testing.T) {
	outcome := &attemptOutcome{}
	body := observedResponseBody{ReadCloser: io.NopCloser(strings.NewReader("payload")), ctx: context.Background(), outcome: outcome}
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if outcome.successful.Load() || outcome.failed.Load() {
		t.Fatalf("close-before-EOF outcome = success %v failure %v, want neither", outcome.successful.Load(), outcome.failed.Load())
	}
}

func TestCloseAfterEOFDoesNotInvalidateCompletion(t *testing.T) {
	outcome := &attemptOutcome{}
	body := observedResponseBody{ReadCloser: io.NopCloser(strings.NewReader("")), ctx: context.Background(), outcome: outcome}
	_, _ = body.Read(make([]byte, 1))
	if err := body.Close(); err != nil {
		t.Fatal(err)
	}
	if !outcome.successful.Load() || outcome.failed.Load() {
		t.Fatalf("EOF outcome = success %v failure %v, want successful completion", outcome.successful.Load(), outcome.failed.Load())
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}

type contextWaitingBody struct {
	ctx     context.Context
	started chan struct{}
}

func (b *contextWaitingBody) Read([]byte) (int, error) {
	close(b.started)
	<-b.ctx.Done()
	return 0, b.ctx.Err()
}

func (b *contextWaitingBody) Close() error {
	return nil
}
