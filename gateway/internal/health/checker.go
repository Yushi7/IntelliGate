package health

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

const (
	CheckInterval = 5 * time.Second
	CheckTimeout  = 2 * time.Second
)

type Backend struct {
	URL        *url.URL
	SetHealthy func(bool) error
}

type Checker struct {
	backends []Backend
	client   *http.Client
	interval time.Duration
}

func NewChecker(backends []Backend, interval, timeout time.Duration) (*Checker, error) {
	if len(backends) == 0 {
		return nil, errors.New("health checker requires at least one backend")
	}
	if interval <= 0 {
		return nil, errors.New("health-check interval must be positive")
	}
	if timeout <= 0 {
		return nil, errors.New("health-check timeout must be positive")
	}

	copiedBackends := make([]Backend, len(backends))
	for i, backend := range backends {
		if backend.URL == nil || !backend.URL.IsAbs() || backend.URL.Hostname() == "" {
			return nil, fmt.Errorf("backend %d must have an absolute URL with a hostname", i)
		}
		scheme := strings.ToLower(backend.URL.Scheme)
		if scheme != "http" && scheme != "https" {
			return nil, fmt.Errorf("backend %d has unsupported health-check URL scheme %q", i, backend.URL.Scheme)
		}
		if backend.SetHealthy == nil {
			return nil, fmt.Errorf("backend %d has no health-state updater", i)
		}

		healthURL := *backend.URL
		healthURL.Scheme = scheme
		healthURL.Path = "/health"
		healthURL.RawPath = ""
		healthURL.RawQuery = ""
		healthURL.ForceQuery = false
		healthURL.Fragment = ""
		healthURL.RawFragment = ""
		copiedBackends[i] = Backend{URL: &healthURL, SetHealthy: backend.SetHealthy}
	}

	return &Checker{
		backends: copiedBackends,
		client: &http.Client{
			Timeout: timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		interval: interval,
	}, nil
}

func (c *Checker) CheckNow(ctx context.Context) error {
	var workers sync.WaitGroup
	errs := make(chan error, len(c.backends))
	for _, backend := range c.backends {
		backend := backend
		workers.Add(1)
		go func() {
			defer workers.Done()
			healthy := c.probe(ctx, backend.URL)
			if err := backend.SetHealthy(healthy); err != nil {
				errs <- fmt.Errorf("update health for backend %q: %w", backend.URL, err)
			}
		}()
	}
	workers.Wait()
	close(errs)

	var failures []error
	for err := range errs {
		failures = append(failures, err)
	}
	return errors.Join(failures...)
}

func (c *Checker) Run(ctx context.Context) error {
	ticker := time.NewTicker(c.interval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			if err := c.CheckNow(ctx); err != nil {
				return err
			}
		}
	}
}

func (c *Checker) probe(ctx context.Context, target *url.URL) bool {
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return false
	}

	response, err := c.client.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	return response.StatusCode >= http.StatusOK && response.StatusCode < http.StatusMultipleChoices
}
