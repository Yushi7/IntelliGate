package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"intelligate/gateway/internal/balancer"
	"intelligate/gateway/internal/health"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/router"
)

func main() {
	if err := runGateway(); err != nil {
		log.Fatal(err)
	}
}

func runGateway() error {
	products, productBackends, err := newServiceProxy("PRODUCT_SERVICE_URLS", "http://localhost:8001")
	if err != nil {
		return err
	}
	orders, orderBackends, err := newServiceProxy("ORDER_SERVICE_URLS", "http://localhost:8002")
	if err != nil {
		return err
	}
	users, userBackends, err := newServiceProxy("USER_SERVICE_URLS", "http://localhost:8003")
	if err != nil {
		return err
	}

	backends := append(append(productBackends, orderBackends...), userBackends...)
	checker, err := health.NewChecker(backends, health.CheckInterval, health.CheckTimeout)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := checker.CheckNow(ctx); err != nil {
		return fmt.Errorf("initial backend health checks: %w", err)
	}
	if ctx.Err() != nil {
		return nil
	}

	server := &http.Server{
		Addr:              ":8080",
		Handler:           newHandler(products, orders, users),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("IntelliGate listening on %s", server.Addr)
	return serveWithHealthChecks(ctx, server, checker)
}

func newServiceProxy(envName, defaultURL string) (*balancer.RoundRobin, []health.Backend, error) {
	urls, err := backendURLsFromEnv(envName, defaultURL)
	if err != nil {
		return nil, nil, err
	}

	backends := make([]http.Handler, 0, len(urls))
	proxies := make([]proxy.Backend, 0, len(urls))
	for _, target := range urls {
		backend, err := proxy.NewBackend(target)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", envName, err)
		}
		proxies = append(proxies, backend)
		backends = append(backends, backend.Handler)
	}

	selector, err := balancer.NewRoundRobin(backends...)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", envName, err)
	}
	healthBackends := make([]health.Backend, len(proxies))
	for i, backend := range proxies {
		if err := selector.SetHealthy(i, false); err != nil {
			return nil, nil, fmt.Errorf("%s backend %d: %w", envName, i, err)
		}
		index := i
		healthBackends[i] = health.Backend{
			URL: backend.HealthURL,
			SetHealthy: func(healthy bool) error {
				return selector.SetHealthy(index, healthy)
			},
		}
	}
	return selector, healthBackends, nil
}

func serveWithHealthChecks(ctx context.Context, server *http.Server, checker *health.Checker) error {
	checkerContext, cancelChecker := context.WithCancel(ctx)
	checkerDone := make(chan error, 1)
	go func() {
		checkerDone <- checker.Run(checkerContext)
	}()

	serverDone := make(chan error, 1)
	go func() {
		serverDone <- server.ListenAndServe()
	}()

	shutdown := func() error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		err := server.Shutdown(shutdownContext)
		if err != nil {
			if closeErr := server.Close(); closeErr != nil {
				return errors.Join(err, fmt.Errorf("force close gateway: %w", closeErr))
			}
		}
		return err
	}

	select {
	case serverErr := <-serverDone:
		cancelChecker()
		if err := <-checkerDone; err != nil {
			return fmt.Errorf("health checker: %w", err)
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			return serverErr
		}
		return nil
	case <-ctx.Done():
		shutdownErr := shutdown()
		cancelChecker()
		serverErr := <-serverDone
		checkerErr := <-checkerDone
		if shutdownErr != nil {
			return fmt.Errorf("shut down gateway: %w", shutdownErr)
		}
		if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
			return serverErr
		}
		if checkerErr != nil {
			return fmt.Errorf("health checker: %w", checkerErr)
		}
		return nil
	case checkerErr := <-checkerDone:
		cancelChecker()
		shutdownErr := shutdown()
		serverErr := <-serverDone
		if ctx.Err() != nil && checkerErr == nil {
			if shutdownErr != nil {
				return fmt.Errorf("shut down gateway: %w", shutdownErr)
			}
			if serverErr != nil && !errors.Is(serverErr, http.ErrServerClosed) {
				return serverErr
			}
			return nil
		}
		if checkerErr == nil {
			checkerErr = errors.New("stopped unexpectedly")
		}
		if shutdownErr != nil {
			return fmt.Errorf("health checker: %v; shut down gateway: %w", checkerErr, shutdownErr)
		}
		return fmt.Errorf("health checker: %w", checkerErr)
	}
}

func backendURLsFromEnv(envName, defaultURL string) ([]string, error) {
	value, configured := os.LookupEnv(envName)
	if !configured {
		value = defaultURL
	}

	urls := strings.Split(value, ",")
	for i := range urls {
		urls[i] = strings.TrimSpace(urls[i])
		if urls[i] == "" {
			return nil, fmt.Errorf("%s contains an invalid backend URL %q: entry is empty", envName, urls[i])
		}
		normalized, err := validateBackendURL(urls[i])
		if err != nil {
			return nil, fmt.Errorf("%s contains an invalid backend URL %q: %w", envName, urls[i], err)
		}
		urls[i] = normalized
	}
	return urls, nil
}

func validateBackendURL(value string) (string, error) {
	backend, err := url.Parse(value)
	if err != nil {
		return "", fmt.Errorf("malformed URL: %w", err)
	}
	if !backend.IsAbs() || backend.Opaque != "" {
		return "", fmt.Errorf("URL must be absolute")
	}

	backend.Scheme = strings.ToLower(backend.Scheme)
	if backend.Scheme != "http" && backend.Scheme != "https" {
		return "", fmt.Errorf("unsupported scheme %q", backend.Scheme)
	}

	hostname := backend.Hostname()
	if hostname == "" || !validBackendHostname(hostname) {
		return "", fmt.Errorf("URL must have a valid hostname")
	}
	if strings.HasSuffix(backend.Host, ":") {
		return "", fmt.Errorf("URL has an empty port")
	}
	if port := backend.Port(); port != "" {
		portNumber, err := strconv.Atoi(port)
		if err != nil || portNumber < 1 || portNumber > 65535 {
			return "", fmt.Errorf("URL has an invalid port %q", port)
		}
	}

	return backend.String(), nil
}

func validBackendHostname(hostname string) bool {
	if net.ParseIP(hostname) != nil {
		return true
	}

	hostname = strings.TrimSuffix(hostname, ".")
	if hostname == "" || len(hostname) > 253 {
		return false
	}
	for _, label := range strings.Split(hostname, ".") {
		if len(label) == 0 || len(label) > 63 || !isHostnameCharacter(label[0]) || !isHostnameCharacter(label[len(label)-1]) {
			return false
		}
		for i := 1; i < len(label)-1; i++ {
			character := label[i]
			if !isHostnameCharacter(character) && character != '-' {
				return false
			}
		}
	}
	return true
}

func isHostnameCharacter(character byte) bool {
	return character >= 'a' && character <= 'z' ||
		character >= 'A' && character <= 'Z' ||
		character >= '0' && character <= '9'
}

func newHandler(products, orders, users http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status  string `json:"status"`
			Service string `json:"service"`
		}{Status: "ok", Service: "intelligate-gateway"})
	})
	mux.Handle("/", router.New(
		router.Route{Prefix: "/api/v1/products", Handler: products},
		router.Route{Prefix: "/api/v1/orders", Handler: orders},
		router.Route{Prefix: "/api/v1/users", Handler: users},
	))
	return requestLogger(mux)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("method=%s path=%s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
