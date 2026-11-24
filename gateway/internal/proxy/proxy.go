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
	outcome := &attemptOutcome{}
	ctx := context.WithValue(req.Context(), attemptOutcomeKey{}, outcome)
	h.ServeHTTP(w, req.WithContext(ctx))
	return outcome.err
}

type attemptOutcomeKey struct{}

type attemptOutcome struct {
	err *AttemptError
}

func attemptOutcomeFrom(ctx context.Context) *attemptOutcome {
	outcome, _ := ctx.Value(attemptOutcomeKey{}).(*attemptOutcome)
	return outcome
}

type breakerAwareTransport struct {
	next http.RoundTripper
}

func (t breakerAwareTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	response, err := t.next.RoundTrip(req)
	permit := circuitbreaker.PermitFromContext(req.Context())
	if permit == nil {
		return response, err
	}
	if err != nil {
		if IsClientContextDone(req.Context()) {
			permit.Abandon()
		} else {
			permit.Failure()
		}
		return response, err
	}
	if response == nil || response.Body == nil {
		permit.Success()
		return response, nil
	}

	response.Body = observedResponseBody{
		ReadCloser: response.Body,
		permit:     permit,
		ctx:        req.Context(),
	}
	return response, nil
}

type observedResponseBody struct {
	io.ReadCloser
	permit *circuitbreaker.Permit
	ctx    context.Context
}

func (b observedResponseBody) Read(buffer []byte) (int, error) {
	count, err := b.ReadCloser.Read(buffer)
	if err == nil {
		return count, nil
	}
	if errors.Is(err, io.EOF) {
		b.permit.Success()
	} else if IsClientContextDone(b.ctx) {
		b.permit.Abandon()
	} else {
		b.permit.Failure()
	}
	return count, err
}

func (b observedResponseBody) Close() error {
	err := b.ReadCloser.Close()
	if IsClientContextDone(b.ctx) {
		b.permit.Abandon()
	} else if isGatewayTimeoutContext(b.ctx) || err != nil {
		b.permit.Failure()
	} else {
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
