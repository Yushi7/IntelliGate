package proxy

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"intelligate/gateway/internal/circuitbreaker"
)

const DefaultUpstreamTimeout = 5 * time.Second

var errGatewayUpstreamTimeout = errors.New("gateway upstream timeout")
var errGatewayRetryBudgetTimeout = errors.New("gateway retry budget exceeded")

type Backend struct {
	Handler   http.Handler
	HealthURL *url.URL
}

type AttemptError struct {
	StatusCode int
	Err        error
}

type MeasuredAttemptResult struct {
	Err              *AttemptError
	BodyCompleted    bool
	TTFB             time.Duration
	TTFBAvailable    bool
	TransportFailure bool
}

func (e *AttemptError) Error() string {
	return e.Err.Error()
}

func (e *AttemptError) WriteHTTPError(w http.ResponseWriter) {
	if e.StatusCode == http.StatusGatewayTimeout {
		http.Error(w, "upstream request timed out", e.StatusCode)
		return
	}
	http.Error(w, "backend service unavailable", e.StatusCode)
}

func New(target string) (http.Handler, error) {
	return NewWithTimeout(target, DefaultUpstreamTimeout)
}

func NewWithTimeout(target string, timeout time.Duration) (http.Handler, error) {
	backend, err := NewBackendWithTimeout(target, timeout)
	if err != nil {
		return nil, err
	}
	return backend.Handler, nil
}

func NewBackend(target string) (Backend, error) {
	return NewBackendWithTimeout(target, DefaultUpstreamTimeout)
}

func NewBackendWithTimeout(target string, timeout time.Duration) (Backend, error) {
	if timeout <= 0 {
		return Backend{}, errors.New("upstream timeout must be positive")
	}

	backend, err := url.Parse(target)
	if err != nil {
		return Backend{}, fmt.Errorf("parse backend URL %q: %w", target, err)
	}
	if backend.Scheme == "" || backend.Host == "" {
		return Backend{}, fmt.Errorf("backend URL %q must include a scheme and host", target)
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(backend)
	reverseProxy.Transport = breakerAwareTransport{next: http.DefaultTransport}
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, req *http.Request, err error) {
		if IsClientContextDone(req.Context()) {
			return
		}
		attemptErr := newAttemptError(req.Context(), err)
		if outcome := attemptOutcomeFrom(req.Context()); outcome != nil {
			outcome.err = attemptErr
		} else {
			attemptErr.WriteHTTPError(w)
		}
	}

	healthURL := *backend
	healthURL.Path = "/health"
	healthURL.RawPath = ""
	healthURL.RawQuery = ""
	healthURL.ForceQuery = false
	healthURL.Fragment = ""
	healthURL.RawFragment = ""

	handler := &backendHandler{reverseProxy: reverseProxy, timeout: timeout}

	return Backend{Handler: handler, HealthURL: &healthURL}, nil
}

type backendHandler struct {
	reverseProxy *httputil.ReverseProxy
	timeout      time.Duration
}

func (h *backendHandler) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := withUpstreamTimeout(req.Context(), h.timeout)
	defer cancel()
	h.reverseProxy.ServeHTTP(w, req.WithContext(ctx))
}

func (h *backendHandler) ServeAttempt(w http.ResponseWriter, req *http.Request) *AttemptError {
	attemptError, _ := h.ServeAttemptResult(w, req)
	return attemptError
}

func (h *backendHandler) ServeAttemptResult(w http.ResponseWriter, req *http.Request) (*AttemptError, bool) {
	result := h.ServeMeasuredAttemptResult(w, req)
	return result.Err, result.BodyCompleted
}

func (h *backendHandler) ServeMeasuredAttemptResult(w http.ResponseWriter, req *http.Request) MeasuredAttemptResult {
	outcome := &attemptOutcome{}
	ctx := context.WithValue(req.Context(), attemptOutcomeKey{}, outcome)
	h.ServeHTTP(w, req.WithContext(ctx))
	outcome.mu.Lock()
	ttfb := outcome.ttfb
	ttfbAvailable := outcome.ttfbAvailable
	outcome.mu.Unlock()
	return MeasuredAttemptResult{Err: outcome.err, BodyCompleted: outcome.successful.Load(), TTFB: ttfb, TTFBAvailable: ttfbAvailable, TransportFailure: outcome.failed.Load()}
}

type attemptOutcomeKey struct{}

type attemptOutcome struct {
	err           *AttemptError
	successful    atomic.Bool
	failed        atomic.Bool
	mu            sync.Mutex
	ttfb          time.Duration
	ttfbAvailable bool
}

func attemptOutcomeFrom(ctx context.Context) *attemptOutcome {
	outcome, _ := ctx.Value(attemptOutcomeKey{}).(*attemptOutcome)
	return outcome
}

type breakerAwareTransport struct {
	next http.RoundTripper
}

func (t breakerAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	response, err := t.next.RoundTrip(req)
	outcome := attemptOutcomeFrom(req.Context())
	if outcome != nil && err == nil && response != nil {
		outcome.mu.Lock()
		outcome.ttfb = time.Since(started)
		outcome.ttfbAvailable = outcome.ttfb > 0
		outcome.mu.Unlock()
	}
	permit := circuitbreaker.PermitFromContext(req.Context())
	if err != nil && outcome != nil && !IsClientContextDone(req.Context()) {
		outcome.failed.Store(true)
	}
	if permit == nil {
		if outcome != nil {
			if err == nil && (response == nil || response.Body == nil) {
				outcome.successful.Store(true)
			} else if err == nil && response != nil {
				response.Body = observedResponseBody{ReadCloser: response.Body, ctx: req.Context(), outcome: outcome}
			}
		}
		return response, err
	}
	if err != nil {
		if IsClientContextDone(req.Context()) {
			permit.Abandon()
		} else {
			permit.Failure()
			if outcome != nil {
				outcome.failed.Store(true)
			}
		}
		return response, err
	}
	if response == nil || response.Body == nil {
		permit.Success()
		if outcome != nil {
			outcome.successful.Store(true)
		}
		return response, nil
	}

	response.Body = observedResponseBody{
		ReadCloser: response.Body,
		permit:     permit,
		ctx:        req.Context(),
		outcome:    outcome,
	}
	return response, nil
}

type observedResponseBody struct {
	io.ReadCloser
	permit  *circuitbreaker.Permit
	ctx     context.Context
	outcome *attemptOutcome
}

func (b observedResponseBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if err == nil {
		return count, nil
	}
	if errors.Is(err, io.EOF) {
		if b.outcome != nil {
			b.outcome.successful.Store(true)
		}
		if b.permit != nil {
			b.permit.Success()
		}
	} else if IsClientContextDone(b.ctx) {
		if b.permit != nil {
			b.permit.Abandon()
		}
	} else {
		if b.outcome != nil {
			b.outcome.failed.Store(true)
		}
		if b.permit != nil {
			b.permit.Failure()
		}
	}
	if err != nil && !errors.Is(err, io.EOF) && b.outcome != nil {
		b.outcome.successful.Store(false)
		if !IsClientContextDone(b.ctx) {
			b.outcome.failed.Store(true)
		}
	}
	return count, err
}

func (b observedResponseBody) Close() error {
	completed := b.outcome != nil && b.outcome.successful.Load()
	err := b.ReadCloser.Close()
	if !completed && b.outcome != nil {
		b.outcome.successful.Store(false)
		if !IsClientContextDone(b.ctx) && (isGatewayTimeoutContext(b.ctx) || err != nil) {
			b.outcome.failed.Store(true)
		}
	}
	if b.permit == nil {
		return err
	}
	if IsClientContextDone(b.ctx) {
		b.permit.Abandon()
	} else if !completed && (isGatewayTimeoutContext(b.ctx) || err != nil) {
		b.permit.Failure()
	} else if !completed {
		b.permit.Abandon()
	}
	return err
}

func withUpstreamTimeout(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(parent, timeout, errGatewayUpstreamTimeout)
}

func WithRetryBudget(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	return context.WithTimeoutCause(parent, timeout, errGatewayRetryBudgetTimeout)
}

func IsClientContextDone(ctx context.Context) bool {
	return ctx.Err() != nil && !isGatewayTimeoutContext(ctx)
}

func IsRetryBudgetExceeded(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errGatewayRetryBudgetTimeout)
}

func isGatewayTimeoutContext(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), errGatewayUpstreamTimeout) ||
		errors.Is(context.Cause(ctx), errGatewayRetryBudgetTimeout)
}

func newAttemptError(ctx context.Context, err error) *AttemptError {
	status := http.StatusBadGateway
	if isGatewayTimeoutContext(ctx) || isTimeoutError(err) {
		status = http.StatusGatewayTimeout
	}
	return &AttemptError{StatusCode: status, Err: err}
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
