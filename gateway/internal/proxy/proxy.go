package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

type Backend struct {
	Handler   http.Handler
	HealthURL *url.URL
}

func New(target string) (http.Handler, error) {
	backend, err := NewBackend(target)
	if err != nil {
		return nil, err
	}
	return backend.Handler, nil
}

func NewBackend(target string) (Backend, error) {
	backend, err := url.Parse(target)
	if err != nil {
		return Backend{}, fmt.Errorf("parse backend URL %q: %w", target, err)
	}
	if backend.Scheme == "" || backend.Host == "" {
		return Backend{}, fmt.Errorf("backend URL %q must include a scheme and host", target)
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(backend)
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "backend service unavailable", http.StatusBadGateway)
	}

	healthURL := *backend
	healthURL.Path = "/health"
	healthURL.RawPath = ""
	healthURL.RawQuery = ""
	healthURL.ForceQuery = false
	healthURL.Fragment = ""
	healthURL.RawFragment = ""

	return Backend{Handler: reverseProxy, HealthURL: &healthURL}, nil
}
