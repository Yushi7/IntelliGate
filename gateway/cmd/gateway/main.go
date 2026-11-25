package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"math"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"intelligate/gateway/internal/auth"
	"intelligate/gateway/internal/balancer"
	"intelligate/gateway/internal/circuitbreaker"
	"intelligate/gateway/internal/health"
	"intelligate/gateway/internal/observability"
	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/ratelimit"
	"intelligate/gateway/internal/router"
)

const (
	authEnabledEnv                     = "INTELLIGATE_AUTH_ENABLED"
	apiKeysEnv                         = "INTELLIGATE_API_KEYS"
	apiKeyRolesEnv                     = "INTELLIGATE_API_KEY_ROLES"
	upstreamTimeoutEnv                 = "INTELLIGATE_UPSTREAM_TIMEOUT"
	listenAddrEnv                      = "INTELLIGATE_LISTEN_ADDR"
	metricsListenAddrEnv               = "INTELLIGATE_METRICS_LISTEN_ADDR"
	rateLimitEnabledEnv                = "INTELLIGATE_RATE_LIMIT_ENABLED"
	rateLimitRateEnv                   = "INTELLIGATE_RATE_LIMIT_RATE"
	rateLimitBurstEnv                  = "INTELLIGATE_RATE_LIMIT_BURST"
	rateLimitStoreEnv                  = "INTELLIGATE_RATE_LIMIT_STORE"
	redisAddrEnv                       = "INTELLIGATE_REDIS_ADDR"
	redisPasswordEnv                   = "INTELLIGATE_REDIS_PASSWORD"
	redisDBEnv                         = "INTELLIGATE_REDIS_DB"
	rateLimitRedisFailureModeEnv       = "INTELLIGATE_RATE_LIMIT_REDIS_FAILURE_MODE"
	breakerFailureThresholdEnv         = "INTELLIGATE_CIRCUIT_BREAKER_FAILURE_THRESHOLD"
	breakerOpenCooldownEnv             = "INTELLIGATE_CIRCUIT_BREAKER_OPEN_COOLDOWN"
	breakerHalfOpenMaxRequestsEnv      = "INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_MAX_REQUESTS"
	breakerHalfOpenSuccessThresholdEnv = "INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_SUCCESS_THRESHOLD"
)

func main() {
	if err := runGateway(); err != nil {
		log.Fatal(err)
	}
}

func runGateway() error {
	authSettings, err := authenticationSettingsFromEnv()
	if err != nil {
		return err
	}
	upstreamTimeout, err := upstreamTimeoutFromEnv()
	if err != nil {
		return err
	}
	listenAddr, err := listenAddressFromEnv()
	if err != nil {
		return err
	}
	metricsListenAddr, err := metricsListenAddressFromEnv()
	if err != nil {
		return err
	}
	metrics := observability.New(upstreamTimeout)
	if authSettings.Authenticator != nil {
		authSettings.Authenticator.SetDenialObserver(metrics.RecordSecurityDenial)
	}
	rateSettings, err := rateLimitSettingsFromEnv()
	if err != nil {
		return err
	}
	var rateLimiter *ratelimit.Limiter
	rateLimiter, err = newRateLimiter(rateSettings)
	if err != nil {
		return err
	}
	if rateLimiter != nil {
		rateLimiter.SetRejectionObserver(metrics.RecordRateLimitRejection)
		defer func() {
			if err := rateLimiter.Close(); err != nil {
				log.Printf("warning: failed to close rate-limit store: %v", err)
			}
		}()
	}
	breakerConfig, err := circuitBreakerConfigFromEnv()
	if err != nil {
		return err
	}

	products, productBackends, err := newServiceProxyWithMetrics("PRODUCT_SERVICE_URLS", "products", "http://localhost:8001", upstreamTimeout, breakerConfig, metrics)
	if err != nil {
		return err
	}
	orders, orderBackends, err := newServiceProxyWithMetrics("ORDER_SERVICE_URLS", "orders", "http://localhost:8002", upstreamTimeout, breakerConfig, metrics)
	if err != nil {
		return err
	}
	users, userBackends, err := newServiceProxyWithMetrics("USER_SERVICE_URLS", "users", "http://localhost:8003", upstreamTimeout, breakerConfig, metrics)
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
		Addr:              listenAddr,
		Handler:           metrics.ObserveHTTP(newHandlerWithAuthentication(products, orders, users, authSettings.Authenticator, rateLimiter)),
		ReadHeaderTimeout: 5 * time.Second,
	}
	metricsServer := newMetricsHTTPServer(metricsListenAddr, metrics)

	log.Printf("IntelliGate listening on %s (metrics on %s)", server.Addr, metricsServer.Addr)
	if rateLimiter != nil && rateLimiter.NeedsCleanup() {
		cleanupContext, cancelCleanup := context.WithCancel(ctx)
		cleanupDone := make(chan struct{})
		go func() {
			defer close(cleanupDone)
			rateLimiter.RunCleanup(cleanupContext)
		}()
		defer func() {
			cancelCleanup()
			<-cleanupDone
		}()
	}
	return serveWithServers(ctx, []*http.Server{server, metricsServer}, checker)
}

func listenAddressFromEnv() (string, error) {
	address := ":8080"
	if value, configured := os.LookupEnv(listenAddrEnv); configured {
		address = value
	}
	if err := validateListenAddress(listenAddrEnv, address); err != nil {
		return "", err
	}
	return address, nil
}

func metricsListenAddressFromEnv() (string, error) {
	address := ":9091"
	if value, configured := os.LookupEnv(metricsListenAddrEnv); configured {
		address = value
	}
	if err := validateListenAddress(metricsListenAddrEnv, address); err != nil {
		return "", err
	}
	return address, nil
}

func validateListenAddress(envName, address string) error {
	if strings.TrimSpace(address) != address {
		return fmt.Errorf("%s must be a host:port address", envName)
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil || strings.ContainsAny(host, " \t\r\n") {
		return fmt.Errorf("%s must be a valid host:port address", envName)
	}
	for _, character := range port {
		if character < '0' || character > '9' {
			return fmt.Errorf("%s must include a numeric port", envName)
		}
	}
	portNumber, err := strconv.Atoi(port)
	if err != nil || portNumber < 1 || portNumber > 65535 {
		return fmt.Errorf("%s must include a port between 1 and 65535", envName)
	}
	return nil
}

func newMetricsHTTPServer(address string, metrics *observability.Metrics) *http.Server {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", metrics.Handler())
	return &http.Server{
		Addr:              address,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
		IdleTimeout:       60 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
}

type authenticationSettings struct {
	Enabled       bool
	Authenticator *auth.Authenticator
}

func authenticationSettingsFromEnv() (authenticationSettings, error) {
	settings := authenticationSettings{Enabled: true}
	if value, configured := os.LookupEnv(authEnabledEnv); configured {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return authenticationSettings{}, fmt.Errorf("%s must be a boolean: %w", authEnabledEnv, err)
		}
		settings.Enabled = enabled
	}

	keyList, keysConfigured := os.LookupEnv(apiKeysEnv)
	roleMapping, rolesConfigured := os.LookupEnv(apiKeyRolesEnv)
	if keysConfigured && rolesConfigured {
		return authenticationSettings{}, fmt.Errorf("%s and %s cannot both be set", apiKeysEnv, apiKeyRolesEnv)
	}
	if !keysConfigured && !rolesConfigured {
		if settings.Enabled {
			return authenticationSettings{}, fmt.Errorf("either %s or %s must be set when authentication is enabled", apiKeysEnv, apiKeyRolesEnv)
		}
		return settings, nil
	}

	var keys []string
	var roles map[string]auth.Role
	var err error
	if rolesConfigured {
		roles, err = auth.ParseRoleMapping(roleMapping)
		if err != nil {
			return authenticationSettings{}, fmt.Errorf("%s: %w", apiKeyRolesEnv, err)
		}
	} else {
		keys, err = auth.ParseKeys(keyList)
		if err != nil {
			return authenticationSettings{}, fmt.Errorf("%s: %w", apiKeysEnv, err)
		}
	}
	if !settings.Enabled {
		return settings, nil
	}

	if rolesConfigured {
		settings.Authenticator, err = auth.NewWithRoles(roles)
	} else {
		settings.Authenticator, err = auth.New(keys)
	}
	if err != nil {
		configuredEnv := apiKeysEnv
		if rolesConfigured {
			configuredEnv = apiKeyRolesEnv
		}
		return authenticationSettings{}, fmt.Errorf("%s: %w", configuredEnv, err)
	}
	return settings, nil
}

func newServiceProxy(envName, defaultURL string, upstreamTimeout time.Duration, breakerConfig circuitbreaker.Config) (*balancer.RoundRobin, []health.Backend, error) {
	service := map[string]string{
		"PRODUCT_SERVICE_URLS": "products",
		"ORDER_SERVICE_URLS":   "orders",
		"USER_SERVICE_URLS":    "users",
	}[envName]
	return newServiceProxyWithMetrics(envName, service, defaultURL, upstreamTimeout, breakerConfig, nil)
}

func newServiceProxyWithMetrics(
	envName, service, defaultURL string,
	upstreamTimeout time.Duration,
	breakerConfig circuitbreaker.Config,
	metrics *observability.Metrics,
) (*balancer.RoundRobin, []health.Backend, error) {
	urls, err := backendURLsFromEnv(envName, defaultURL)
	if err != nil {
		return nil, nil, err
	}

	backends := make([]http.Handler, 0, len(urls))
	proxies := make([]proxy.Backend, 0, len(urls))
	breakers := make([]*circuitbreaker.Breaker, 0, len(urls))
	backendIDs := make([]string, len(urls))
	for index, target := range urls {
		backend, err := proxy.NewBackendWithTimeout(target, upstreamTimeout)
		if err != nil {
			return nil, nil, fmt.Errorf("%s: %w", envName, err)
		}
		breaker, err := circuitbreaker.New(breakerConfig)
		if err != nil {
			return nil, nil, fmt.Errorf("%s backend %d circuit breaker: %w", envName, len(proxies), err)
		}
		backendID := fmt.Sprintf("%s-%d", service, index+1)
		backendIDs[index] = backendID
		if metrics != nil {
			metrics.InitializeBackend(service, backendID)
			breaker.SetTransitionObserver(func(from, to circuitbreaker.State, generation uint64) {
				metrics.RecordCircuitBreakerTransition(service, backendID, metricBreakerState(from), metricBreakerState(to), generation)
			})
		}
		proxies = append(proxies, backend)
		backends = append(backends, backend.Handler)
		breakers = append(breakers, breaker)
	}

	selector, err := balancer.NewRoundRobinWithRetryPolicy(
		balancer.DefaultRetryPolicy(upstreamTimeout),
		backends...,
	)
	if err != nil {
		return nil, nil, fmt.Errorf("%s: %w", envName, err)
	}
	if metrics != nil {
		selector.SetObserver(&serviceBalancerObserver{
			metrics:    metrics,
			service:    service,
			backendIDs: backendIDs,
		})
	}
	for i, breaker := range breakers {
		if err := selector.SetCircuitBreaker(i, breaker); err != nil {
			return nil, nil, fmt.Errorf("%s backend %d circuit breaker: %w", envName, i, err)
		}
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
		if metrics != nil {
			backendID := backendIDs[index]
			healthBackends[i].OnTransition = func(healthy bool, generation uint64) {
				metrics.SetBackendHealth(service, backendID, healthy, generation)
			}
		}
	}
	return selector, healthBackends, nil
}

type serviceBalancerObserver struct {
	metrics    *observability.Metrics
	service    string
	backendIDs []string
}

func (o *serviceBalancerObserver) BackendSelected(index int, attempt string) {
	if index >= 0 && index < len(o.backendIDs) {
		o.metrics.RecordBackendSelection(o.service, o.backendIDs[index], attempt)
	}
}

func (o *serviceBalancerObserver) BackendSelectionFailed(reason string) {
	o.metrics.RecordBackendSelectionFailure(o.service, reason)
}

func (o *serviceBalancerObserver) RetryAttempt(method string) {
	o.metrics.RecordRetryAttempt(o.service, method)
}

func (o *serviceBalancerObserver) RetryOutcome(outcome string) {
	o.metrics.RecordRetryOutcome(o.service, outcome)
}

func metricBreakerState(state circuitbreaker.State) string {
	if state == circuitbreaker.StateHalfOpen {
		return "half_open"
	}
	return state.String()
}

func upstreamTimeoutFromEnv() (time.Duration, error) {
	value, configured := os.LookupEnv(upstreamTimeoutEnv)
	if !configured {
		return proxy.DefaultUpstreamTimeout, nil
	}

	timeout, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", upstreamTimeoutEnv, err)
	}
	if timeout <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", upstreamTimeoutEnv)
	}
	return timeout, nil
}

func circuitBreakerConfigFromEnv() (circuitbreaker.Config, error) {
	config := circuitbreaker.DefaultConfig()
	var err error

	config.FailureThreshold, err = positiveIntFromEnv(breakerFailureThresholdEnv, config.FailureThreshold)
	if err != nil {
		return circuitbreaker.Config{}, err
	}
	config.OpenCooldown, err = positiveDurationFromEnv(breakerOpenCooldownEnv, config.OpenCooldown)
	if err != nil {
		return circuitbreaker.Config{}, err
	}
	config.HalfOpenMaxRequests, err = positiveIntFromEnv(breakerHalfOpenMaxRequestsEnv, config.HalfOpenMaxRequests)
	if err != nil {
		return circuitbreaker.Config{}, err
	}
	config.HalfOpenSuccessThreshold, err = positiveIntFromEnv(breakerHalfOpenSuccessThresholdEnv, config.HalfOpenSuccessThreshold)
	if err != nil {
		return circuitbreaker.Config{}, err
	}
	return config, nil
}

type rateLimitSettings struct {
	Enabled   bool
	Config    ratelimit.Config
	StoreName string
	Redis     ratelimit.RedisOptions
}

func rateLimitSettingsFromEnv() (rateLimitSettings, error) {
	settings := rateLimitSettings{
		Enabled:   true,
		Config:    ratelimit.DefaultConfig(),
		StoreName: "memory",
		Redis:     ratelimit.DefaultRedisOptions(),
	}
	if value, configured := os.LookupEnv(rateLimitEnabledEnv); configured {
		enabled, err := strconv.ParseBool(value)
		if err != nil {
			return rateLimitSettings{}, fmt.Errorf("%s must be a boolean: %w", rateLimitEnabledEnv, err)
		}
		settings.Enabled = enabled
	}

	if value, configured := os.LookupEnv(rateLimitStoreEnv); configured {
		settings.StoreName = strings.ToLower(strings.TrimSpace(value))
	}
	if settings.StoreName != "memory" && settings.StoreName != "redis" {
		return rateLimitSettings{}, fmt.Errorf("%s must be either memory or redis", rateLimitStoreEnv)
	}
	if value, configured := os.LookupEnv(redisAddrEnv); configured {
		settings.Redis.Addr = value
	}
	if value, configured := os.LookupEnv(redisPasswordEnv); configured {
		settings.Redis.Password = value
	}
	if value, configured := os.LookupEnv(redisDBEnv); configured {
		db, err := strconv.Atoi(value)
		if err != nil || db < 0 {
			return rateLimitSettings{}, fmt.Errorf("%s must be a non-negative integer", redisDBEnv)
		}
		settings.Redis.DB = db
	}
	if err := ratelimit.ValidateRedisAddress(settings.Redis.Addr); err != nil {
		return rateLimitSettings{}, fmt.Errorf("%s: %w", redisAddrEnv, err)
	}
	if value, configured := os.LookupEnv(rateLimitRedisFailureModeEnv); configured {
		switch strings.ToLower(strings.TrimSpace(value)) {
		case string(ratelimit.FailureModeClosed):
			settings.Config.FailureMode = ratelimit.FailureModeClosed
		case string(ratelimit.FailureModeOpen):
			settings.Config.FailureMode = ratelimit.FailureModeOpen
		default:
			return rateLimitSettings{}, fmt.Errorf("%s must be either closed or open", rateLimitRedisFailureModeEnv)
		}
	}
	if !settings.Enabled {
		return settings, nil
	}

	rate, err := positiveFloatFromEnv(rateLimitRateEnv, settings.Config.Rate)
	if err != nil {
		return rateLimitSettings{}, err
	}
	burst, err := positiveFloatFromEnv(rateLimitBurstEnv, settings.Config.Burst)
	if err != nil {
		return rateLimitSettings{}, err
	}
	settings.Config.Rate = rate
	settings.Config.Burst = burst
	return settings, nil
}

func newRateLimiter(settings rateLimitSettings) (*ratelimit.Limiter, error) {
	if !settings.Enabled {
		return nil, nil
	}
	if settings.StoreName == "memory" {
		return ratelimit.New(settings.Config)
	}
	if settings.StoreName != "redis" {
		return nil, fmt.Errorf("unsupported rate-limit store %q", settings.StoreName)
	}

	store, err := ratelimit.NewRedisStore(settings.Config, settings.Redis)
	if err != nil {
		return nil, fmt.Errorf("initialize Redis rate limiter: %w", err)
	}
	limiter, err := ratelimit.NewWithStore(settings.Config, store)
	if err != nil {
		if closeErr := store.Close(); closeErr != nil {
			return nil, errors.Join(err, fmt.Errorf("close Redis rate-limit store after initialization failure: %w", closeErr))
		}
		return nil, err
	}
	return limiter, nil
}

func positiveFloatFromEnv(envName string, defaultValue float64) (float64, error) {
	value, configured := os.LookupEnv(envName)
	if !configured {
		return defaultValue, nil
	}
	number, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(number) || math.IsInf(number, 0) || number <= 0 {
		if err != nil {
			return 0, fmt.Errorf("%s must be a finite positive number: %w", envName, err)
		}
		return 0, fmt.Errorf("%s must be a finite positive number", envName)
	}
	return number, nil
}

func positiveIntFromEnv(envName string, defaultValue int) (int, error) {
	value, configured := os.LookupEnv(envName)
	if !configured {
		return defaultValue, nil
	}
	number, err := strconv.Atoi(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a positive integer: %w", envName, err)
	}
	if number <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", envName)
	}
	return number, nil
}

func positiveDurationFromEnv(envName string, defaultValue time.Duration) (time.Duration, error) {
	value, configured := os.LookupEnv(envName)
	if !configured {
		return defaultValue, nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil {
		return 0, fmt.Errorf("%s must be a valid duration: %w", envName, err)
	}
	if duration <= 0 {
		return 0, fmt.Errorf("%s must be greater than zero", envName)
	}
	return duration, nil
}

func serveWithHealthChecks(ctx context.Context, server *http.Server, checker *health.Checker) error {
	return serveWithServers(ctx, []*http.Server{server}, checker)
}

func serveWithServers(ctx context.Context, servers []*http.Server, checker *health.Checker) error {
	if len(servers) == 0 {
		return errors.New("at least one HTTP server is required")
	}
	listeners := make([]net.Listener, len(servers))
	for index, server := range servers {
		listener, err := net.Listen("tcp", server.Addr)
		if err != nil {
			for _, opened := range listeners {
				if opened != nil {
					_ = opened.Close()
				}
			}
			return fmt.Errorf("listen on %s: %w", server.Addr, err)
		}
		listeners[index] = listener
	}

	checkerContext, cancelChecker := context.WithCancel(ctx)
	checkerDone := make(chan error, 1)
	go func() {
		checkerDone <- checker.Run(checkerContext)
	}()

	type serverResult struct {
		index int
		err   error
	}
	serverDone := make(chan serverResult, len(servers))
	for index, server := range servers {
		index, server, listener := index, server, listeners[index]
		go func() {
			serverDone <- serverResult{index: index, err: server.Serve(listener)}
		}()
	}

	shutdown := func() error {
		shutdownContext, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var failures []error
		for _, server := range servers {
			if err := server.Shutdown(shutdownContext); err != nil {
				failures = append(failures, fmt.Errorf("shut down server %s: %w", server.Addr, err))
				if closeErr := server.Close(); closeErr != nil {
					failures = append(failures, fmt.Errorf("force close server %s: %w", server.Addr, closeErr))
				}
			}
		}
		return errors.Join(failures...)
	}

	select {
	case result := <-serverDone:
		shutdownErr := shutdown()
		cancelChecker()
		checkerErr := <-checkerDone
		var serverErrors []error
		if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
			serverErrors = append(serverErrors, fmt.Errorf("HTTP server %s: %w", servers[result.index].Addr, result.err))
		}
		for range len(servers) - 1 {
			other := <-serverDone
			if other.err != nil && !errors.Is(other.err, http.ErrServerClosed) {
				serverErrors = append(serverErrors, fmt.Errorf("HTTP server %s: %w", servers[other.index].Addr, other.err))
			}
		}
		if checkerErr != nil && ctx.Err() == nil {
			serverErrors = append(serverErrors, fmt.Errorf("health checker: %w", checkerErr))
		}
		return errors.Join(append(serverErrors, shutdownErr)...)
	case <-ctx.Done():
		shutdownErr := shutdown()
		cancelChecker()
		var serverErrors []error
		for range servers {
			result := <-serverDone
			if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
				serverErrors = append(serverErrors, fmt.Errorf("HTTP server %s: %w", servers[result.index].Addr, result.err))
			}
		}
		checkerErr := <-checkerDone
		if shutdownErr != nil {
			serverErrors = append(serverErrors, shutdownErr)
		}
		if checkerErr != nil {
			serverErrors = append(serverErrors, fmt.Errorf("health checker: %w", checkerErr))
		}
		return errors.Join(serverErrors...)
	case checkerErr := <-checkerDone:
		if ctx.Err() != nil {
			shutdownErr := shutdown()
			cancelChecker()
			for range servers {
				<-serverDone
			}
			if shutdownErr != nil {
				return fmt.Errorf("shut down gateway: %w", shutdownErr)
			}
			return nil
		}
		cancelChecker()
		shutdownErr := shutdown()
		var serverErrors []error
		for range servers {
			result := <-serverDone
			if result.err != nil && !errors.Is(result.err, http.ErrServerClosed) {
				serverErrors = append(serverErrors, fmt.Errorf("HTTP server %s: %w", servers[result.index].Addr, result.err))
			}
		}
		if checkerErr == nil {
			checkerErr = errors.New("stopped unexpectedly")
		}
		if shutdownErr != nil {
			serverErrors = append(serverErrors, shutdownErr)
		}
		serverErrors = append(serverErrors, fmt.Errorf("health checker: %w", checkerErr))
		return errors.Join(serverErrors...)
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
	return newHandlerWithRateLimiter(products, orders, users, nil)
}

func newHandlerWithRateLimiter(products, orders, users http.Handler, limiter *ratelimit.Limiter) http.Handler {
	return newHandlerWithAuthentication(products, orders, users, nil, limiter)
}

func newHandlerWithAuthentication(
	products, orders, users http.Handler,
	authenticator *auth.Authenticator,
	limiter *ratelimit.Limiter,
) http.Handler {
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
	var handler http.Handler = mux
	if limiter != nil {
		handler = limiter.Middleware(handler)
	}
	if authenticator != nil {
		handler = authenticator.AuthorizationMiddleware(handler)
		handler = authenticator.AuthenticationMiddleware(handler)
	}
	return requestLogger(handler)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("method=%s path=%s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
