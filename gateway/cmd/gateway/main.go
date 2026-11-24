package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"intelligate/gateway/internal/balancer"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/router"
)

func main() {
	products, err := newServiceProxy("PRODUCT_SERVICE_URLS", "http://localhost:8001")
	if err != nil {
		log.Fatal(err)
	}
	orders, err := newServiceProxy("ORDER_SERVICE_URLS", "http://localhost:8002")
	if err != nil {
		log.Fatal(err)
	}
	users, err := newServiceProxy("USER_SERVICE_URLS", "http://localhost:8003")
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              ":8080",
		Handler:           newHandler(products, orders, users),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("IntelliGate listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func newServiceProxy(envName, defaultURL string) (http.Handler, error) {
	urls, err := backendURLsFromEnv(envName, defaultURL)
	if err != nil {
		return nil, err
	}

	backends := make([]http.Handler, 0, len(urls))
	for _, target := range urls {
		backend, err := proxy.New(target)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", envName, err)
		}
		backends = append(backends, backend)
	}

	selector, err := balancer.NewRoundRobin(backends...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", envName, err)
	}
	return selector, nil
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
