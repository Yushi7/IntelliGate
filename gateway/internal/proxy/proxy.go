package proxy

import (
	"fmt"
	"net/http"
	"net/http/httputil"
	"net/url"
)

func New(target string) (http.Handler, error) {
	backend, err := url.Parse(target)
	if err != nil {
		return nil, fmt.Errorf("parse backend URL %q: %w", target, err)
	}
	if backend.Scheme == "" || backend.Host == "" {
		return nil, fmt.Errorf("backend URL %q must include a scheme and host", target)
	}

	reverseProxy := httputil.NewSingleHostReverseProxy(backend)
	reverseProxy.ErrorHandler = func(w http.ResponseWriter, _ *http.Request, err error) {
		http.Error(w, "backend service unavailable", http.StatusBadGateway)
	}
	return reverseProxy, nil
}
