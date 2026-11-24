package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"time"
)

const DefaultUpstreamTimeout = 5 * time.Second

type Backend struct {
	Handler   http.Handler
	HealthURL *url.URL
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
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		if isTimeoutError(err) {
			http.Error(w, "upstream request timed out", http.StatusGatewayTimeout)
			return
		}
		http.Error(w, "backend service unavailable", http.StatusBadGateway)
	}

	healthURL := *backend
	healthURL.Path = "/health"
	healthURL.RawPath = ""
	healthURL.RawQuery = ""
	healthURL.ForceQuery = false
	healthURL.Fragment = ""
	healthURL.RawFragment = ""

	handler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), timeout)
		defer cancel()
		reverseProxy.ServeHTTP(w, r.WithContext(ctx))
	})

	return Backend{Handler: handler, HealthURL: &healthURL}, nil
}

func isTimeoutError(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var networkError net.Error
	return errors.As(err, &networkError) && networkError.Timeout()
}
