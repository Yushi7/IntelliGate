# Development Log

## 2026-11-24 — Phase 0 project initialization

**Status:** Initial structure and documentation created for review. Runtime implementation has not started.

### Project objectives

Establish IntelliGate as a modular API gateway demonstration for secure and dynamic microservices traffic management. The planned system will use a Go gateway, Python/FastAPI Product, Order, and User services, compare Round Robin with Adaptive Least-Loaded routing, and explore resilience, monitoring, and traffic analysis.

### Development environment

- Repository workspace: Windows.
- Repository branch observed during initialization: `main`.
- **Installed and verified local tools at initialization:** Windows PowerShell 7.6.10; Go 1.27.4; Python 3.14.5; Git 2.54.0; Docker Engine 29.4.3 (running at initialization); Docker Compose 5.1.3; Node.js 22.16.0; npm 10.9.2; VS Code 1.147.0.
- **Project dependency/configuration status:** These are local tool installations, not evidence that project dependencies have been installed. No project dependency installation or application startup was performed for Phase 0. FastAPI, React, TypeScript, Tailwind CSS, Prometheus, Grafana, Locust, and scikit-learn have not been installed or configured as project dependencies. PostgreSQL and Redis are not yet configured for this project. No Docker Compose project configuration exists.
- **Review-time environment recheck (2026-11-24):** The Docker CLI reported version 29.4.3, but Docker Desktop reported that it was unable to start; the Engine was therefore not available during this recheck. This is separate from the running status verified at initialization.

### Technology stack

Go; Python + FastAPI; React + TypeScript; Tailwind CSS; PostgreSQL; Redis; Prometheus; Grafana; Locust; Docker + Docker Compose; Go Testing; Pytest; GitHub Actions; optional AWS EC2; Python + scikit-learn.

### Phase 0 activities

- Created the planned top-level and nested source/operations directory structure.
- Prepared the README, project scope, proposed architecture, testing strategy, experiment methodology, and planned API documentation.
- Added a repository `.gitignore` for common generated files, caches, local secrets, and test outputs.
- No application code, runtime configuration, dependencies, Docker Compose file, or experimental results were created.

### Initial architectural decisions

- Go is the central gateway language and will use standard HTTP reverse-proxy capabilities.
- Backend demonstration services will use Python and FastAPI.
- Redis is assigned distributed rate-limit state; PostgreSQL is assigned persistent records and selected configuration.
- Prometheus and Grafana are assigned metrics collection and visualization.
- Round Robin is the baseline; Adaptive Least-Loaded is the comparison strategy.
- Locust will generate controlled load.
- Features and APIs remain proposed until implemented and verified.

### Pending implementation tasks

- Phase 1 gateway, service, and foundational routing implementation is complete.
- Phase 2A Round Robin and Phase 2B active health checks and recovery are implemented and tested.
- Design and test authentication, rate limiting, adaptive balancing, and circuit breaking.
- Add database/cache integrations, metrics, dashboards, CI, and container orchestration when justified by working components.
- Add Locust scenarios and conduct experiments; populate results only from actual runs.
- Evaluate and document anomaly-detection design using suitable data.

## 2026-11-24 — Phase 1 gateway and service foundation

**Status:** Implemented and verified locally.

### Implemented

- Added a Go standard-library HTTP gateway on port `8080`, with static Product (`8001`), Order (`8002`), and User (`8003`) routing.
- Added path-boundary-aware route matching, unchanged path/query proxying, request logging, `404` for unmapped paths, and `502` for unavailable backends.
- Added separate FastAPI applications, sample GET endpoints, root and health endpoints, requirements, and pytest tests for each service.
- Updated the README, architecture, API, and testing documentation to separate Phase 1 behavior from planned features. No database, Redis, Docker, authentication, load balancing, or advanced monitoring was added.

### Verification

- `go test ./...` from `gateway`: passed for router and proxy packages.
- Pytest in each service's own virtual environment: Product `2 passed`, Order `2 passed`, User `2 passed`. Each run emitted a Starlette deprecation warning about its current `httpx` test-client integration; no tests failed.
- Manual local integration: all three gateway routes returned the expected service-specific sample data; Product root and health returned successfully; an unmapped gateway path returned `404`; stopping Product Service caused its gateway route to return `502`.
- The Go proxy test verified path and query preservation. No performance or broader resilience results are claimed.

## 2026-11-24 — Phase 2A Round Robin load balancing

**Status:** Implemented; standard and repeated concurrent Go tests passed.

### Implemented

- Added a reusable per-service Round Robin handler with a short `sync.Mutex`-protected selection step. Each configured pool advances independently and proxies outside the lock.
- Added comma-separated `PRODUCT_SERVICE_URLS`, `ORDER_SERVICE_URLS`, and `USER_SERVICE_URLS` configuration. Unset variables retain the existing single-backend defaults (`localhost:8001`, `:8002`, and `:8003`); empty URL entries and invalid backend URLs fail startup.
- Preserved existing service paths, query strings, 404 behavior, 502 behavior, and gateway health handling. Unavailable instances are not health-filtered or retried; selection continues on the next request.
- Updated README, architecture, API, testing, and development-log status. No adaptive routing, EWMA, circuit breaker, authentication, rate limiting, database, Redis, Docker, or dashboard feature was added.

### Verification

- `gofmt` completed for changed Go files.
- `go test ./...` from `gateway`: passed for gateway, balancer, proxy, and router packages.
- `go test -count=25 ./internal/balancer`: passed, including repeated concurrent selector tests.
- `go test -race ./...` was attempted but could not compile because the available MinGW `cc1.exe` lacks 64-bit support (`64-bit mode not compiled in`). The race detector result is therefore unverified in this environment.
- Balancer integration tests use `httptest` instances to verify sequential routing to two instances of one service, path/query preservation, 502 for an unavailable selected instance, and successful selection on the following request. No performance benchmark was run.

### Manual live Round Robin demonstration

This live demonstration is separate from the automated Go tests and is not a performance benchmark.

- Gateway: `localhost:8080`.
- Product Service instances: `localhost:8001` and `localhost:8011`, configured with `PRODUCT_SERVICE_URLS=http://localhost:8001,http://localhost:8011`.
- Sent six sequential `GET /api/v1/products` requests through the gateway. The gateway logged all six.
- Each Product Service instance received 3 requests, all with HTTP 200 responses. The observed 3:3 distribution matched Round Robin across two backends.
- Both instances returned identical sample product data.
- No throughput, latency, concurrency, or other performance measurements were collected. No health-aware routing, automatic failover, or adaptive routing was demonstrated.

## Phase 2B health-aware backend management

**Status:** Implemented; automated Go tests passed. No commit was created.

### Implemented

- Added per-backend health state to the existing Round Robin selectors. The gateway initializes configured instances as unhealthy, selects only healthy instances, returns `503 Service Unavailable` when a service has no healthy instances, and restores backends after a successful probe.
- Added direct concurrent GET probes to each backend's `/health` endpoint. The initial probe cycle completes before the gateway accepts application traffic; periodic checks run every 5 seconds with a 2-second timeout. Only 2xx responses count as healthy, redirects are not followed, and probe outcomes alone determine health.
- Preserved existing environment variables and default backend URLs, service routes, path/query forwarding, 404 behavior, and 502 behavior for an instance that becomes unreachable between checks. Gateway `GET /health` remains a liveness endpoint independent of backend health.
- Added selector, checker, and gateway integration tests using controlled test backends. No retries, health-based application-response inference, circuit breakers, adaptive routing, or performance instrumentation was added.
- Updated the README, architecture, API, and testing documentation. No live multi-process health-check demonstration or performance benchmark was run for Phase 2B.

### Verification

- `gofmt` completed for the changed Go files.
- `go test ./...` from `gateway`: passed for gateway, balancer, health, proxy, and router packages.
- `go test -count=25 ./internal/balancer`: passed.
- `go test -count=25 ./internal/health`: passed.
- `go test -race ./...` could not complete because the available Windows MinGW compiler does not support the required 64-bit mode (`cc1.exe: sorry, unimplemented: 64-bit mode not compiled in`). Race detection is unverified.
- No throughput, latency, concurrency-performance, or other benchmark results are claimed.

## Phase 3A configurable upstream request timeouts

**Status:** Implemented and verified with the Go test suite. No commit was created.

### Implemented

- Added `INTELLIGATE_UPSTREAM_TIMEOUT`, parsed once during gateway startup as a positive Go duration. It defaults to `5s`; malformed, zero, and negative values fail startup with an error naming the setting.
- Configured each reverse proxy with an upstream deadline derived from the incoming request context. Health-check timing remains independent. No retries, circuit breakers, selector changes, or health-state changes were added.
- Upstream timeout errors before response headers return `504 Gateway Timeout`; ordinary connection/proxy failures remain `502 Bad Gateway`, and all-unhealthy pools remain `503 Service Unavailable`.
- The request deadline cancels upstream work without serializing other requests. If a timeout occurs after response headers have been sent, the HTTP status cannot be changed and the response may be truncated; responses remain streamed rather than buffered.
- Updated README, architecture, API, and testing documentation with configuration, behavior, limitations, and a manual verification procedure.

### Verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/balancer`, `internal/health`, `internal/proxy`, and `internal/router`.
- `git diff --check`: passed.
- Tests cover default/custom/invalid timeout configuration; positive-timeout validation; timely upstream responses; 504 on timeout; an independent concurrent request completing while another request times out; and existing 502, 503, and path/query behavior.
- The documented manual timeout procedure was not run. No performance benchmark or race-detector result is claimed.

## Phase 3B per-backend circuit breakers

**Status:** Implemented and verified with automated Go tests and live manual checks on 24 November 2026. No commit was created.

### Implemented

- Added a reusable circuit-breaker state machine with independent instances per configured backend. Defaults are 5 consecutive qualifying failures, a 10-second OPEN cooldown, one concurrent HALF-OPEN trial, and one successful trial required to close.
- Added validated startup overrides: `INTELLIGATE_CIRCUIT_BREAKER_FAILURE_THRESHOLD`, `INTELLIGATE_CIRCUIT_BREAKER_OPEN_COOLDOWN`, `INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_MAX_REQUESTS`, and `INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_SUCCESS_THRESHOLD`.
- Integrated breaker admission with health-aware Round Robin. Unhealthy backends, OPEN breakers, and full HALF-OPEN trial slots are excluded; if no backend is eligible the gateway returns 503. Health flags and breaker state remain independent, and successful health probes do not close breakers.
- Added per-request permits and proxy transport/body observation. Upstream transport errors and timed-out responses count as failures; client cancellations abandon permits. Complete upstream responses, including application 4xx/5xx and application-originated 502/504, are transport successes. The gateway's own 502/504 errors count as upstream failures.
- Updated README, architecture, API, and testing documentation. No automatic retries, external dependencies, or performance claims were added.

### Live integration verification

- Temporary Python backend on port `8091`.
- Demo settings: `1s` upstream timeout, failure threshold `2`, `60s` OPEN cooldown.
- Backend health remained HTTP 200 during product endpoint failures.
- Two product requests returned 504.
- The next request returned 503 (circuit OPEN).
- Backend was restored to healthy mode.
- After cooldown, HALF-OPEN trial returned 200.
- Subsequent product request returned 200.
- Gateway `/health` remained 200.

### Verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/balancer`, `internal/circuitbreaker`, `internal/health`, `internal/proxy`, and `internal/router`.
- `go test -count=10 ./internal/circuitbreaker`: passed.
- `go test -count=5 ./internal/balancer`: passed.
- Automated tests cover configuration validation, state transitions, cooldown and trial limits, cancellation, stale permits, concurrency, backend isolation, health-check independence, application-status classification, and 502/503/504 behavior.
- `git diff --check`: passed. Separate manual live circuit-breaker verification is recorded below. No performance benchmark or race-detector result is claimed.

### Live integration verification

- Used a temporary controllable Python backend on port 8091.
- Demo configuration: 1-second upstream timeout, failure threshold of 2, and OPEN cooldown of 60 seconds.
- The backend health endpoint continued returning HTTP 200 while product requests were deliberately delayed.
- Two consecutive product requests returned HTTP 504.
- The next product request returned HTTP 503 because the circuit was OPEN.
- Restored the backend to healthy mode.
- After the cooldown, the HALF-OPEN trial returned HTTP 200.
- The subsequent product request also returned HTTP 200, confirming recovery.
- Gateway `GET /health` continued returning HTTP 200.

## Phase 3C controlled upstream retries

**Status:** Implemented and verified with automated Go tests. No commit was created.

### Implemented

- Added a typed retry policy with two retries by default, 100ms/200ms backoffs, and an overall budget derived from the configured upstream timeout and backoff durations.
- Restricted automatic retries to GET and HEAD requests with replayable bodies, and only to gateway-observed transport errors before response headers are committed. Completed upstream HTTP responses and body-stream failures are not retried.
- Integrated retries with health-aware Round Robin and per-backend circuit-breaker admission. Each gateway-selected attempt gets a fresh permit; client cancellation/deadline abandons it. The selector prefers a different eligible backend and may reuse an attempted backend only after normal health and breaker checks.
- Kept successful proxy responses streamed without buffering. Exhausted retries return the last gateway-observed 502/504; no eligible backend before the initial attempt remains 503.

### Verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/balancer`, `internal/circuitbreaker`, `internal/health`, `internal/proxy`, and `internal/router`.
- `go test -count=10 ./internal/balancer`: passed.
- `go test -count=10 ./internal/proxy`: passed.
- `go test -count=10 ./internal/circuitbreaker`: passed.
- `git diff --check`: passed; Git emitted only Windows line-ending normalization notices. No race-detector, live manual retry, or performance benchmark result is claimed.

## Phase 4A in-memory token bucket rate limiting

**Status:** Implemented and verified with automated Go tests. No commit was created.

### Implemented

- Added a concurrency-safe in-memory token bucket keyed by the canonical client IP parsed from `RemoteAddr`. Forwarded-IP headers are ignored.
- Enabled rate limiting by default with a 10-token/second refill rate and capacity 20. `GET /health` is exempt; rejected requests return HTTP 429 with JSON and a positive rounded-up `Retry-After`.
- Added startup configuration through `INTELLIGATE_RATE_LIMIT_ENABLED`, `INTELLIGATE_RATE_LIMIT_RATE`, and `INTELLIGATE_RATE_LIMIT_BURST`. Enabled rate and burst values must be finite and positive; explicitly disabled mode bypasses the limiter.
- Expire client buckets after 10 minutes of inactivity and clean them every minute. Bucket accounting and cleanup share a short mutex; downstream handlers run after the lock is released.
- Kept limits process-local; no Redis or global-quota headers were added.

### Verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/balancer`, `internal/circuitbreaker`, `internal/health`, `internal/proxy`, `internal/ratelimit`, and `internal/router`.
- `go test -count=10 ./internal/ratelimit`: passed.
- `go test -count=10 ./cmd/gateway`: passed.
- `git diff --check`: passed; Git reported only Windows line-ending normalization notices.
- Manual verification used 1 token/second and burst capacity 5: the first five rapid requests returned HTTP 200, and subsequent requests returned HTTP 429 with `Retry-After: 1` and `{"error":"rate limit exceeded"}`.
- Confirmed `GET /health` remained accessible with HTTP 200 and normal requests succeeded again after token refill.
- This limiter is process-local, not distributed. No race-detector run or performance benchmark is claimed.

## Phase 4B.1 API key authentication

**Status:** Implemented and verified with automated Go tests. No commit was created.

### Implemented

- Added reusable API-key parsing and authentication under `gateway/internal/auth/`. Keys are configured using comma-separated `INTELLIGATE_API_KEYS`; surrounding whitespace is trimmed and empty or malformed entries are rejected without echoing key values.
- Authentication is enabled by default through `INTELLIGATE_AUTH_ENABLED`; when enabled, missing or empty key configuration prevents startup. Explicit `INTELLIGATE_AUTH_ENABLED=false` disables authentication.
- Protected `/api/v1` and all `/api/v1/*` paths with the `X-API-Key` header, including future/unmapped routes. Missing, invalid, repeated, and comma-combined key values return the same `401` JSON response: `{"error":"unauthorized"}`.
- Stored SHA-256 key digests and compared fixed-size digests with `crypto/subtle`. Validated keys are stripped before upstream proxying. Middleware order is logging → authentication → rate limiting → routing/proxy. Unauthorized requests do not consume limiter tokens or reach backends. Gateway `GET /health` remains public.
- Updated README, architecture, API, and testing docs with configuration, middleware order, local placeholder-key instructions, and shared-key security limitations. Role authorization was not part of Phase 4B.1 and is implemented in Phase 4B.2 below.

### Automated verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/auth`, `internal/balancer`, `internal/circuitbreaker`, `internal/health`, `internal/proxy`, `internal/ratelimit`, and `internal/router`.
- `go test -count=10 ./internal/auth`: passed.
- `go test -count=10 ./cmd/gateway`: passed.
- `git diff --check`: passed; Git reported only Windows line-ending normalization notices.
- Unauthorized requests were verified not to consume rate-limit tokens.
- An upstream `httptest` server verified that the API key is not forwarded.

### Live manual verification — 24 November 2026

- Missing API key returned HTTP 401.
- Invalid API key returned HTTP 401.
- The first configured API key returned HTTP 200 with Product Service data.
- The second configured API key returned HTTP 200 with Product Service data.
- Public gateway `GET /health` returned HTTP 200 without authentication.

### Limitations

- Shared API keys only in this phase; role-based authorization is added in Phase 4B.2 below.
- Configuration changes require a gateway restart.
- Direct microservice ports are not protected by gateway authentication.
- TLS and protected secret management are required for exposed deployments.
- There is no distributed authentication or key-rotation service.

## Phase 4B.2 role-based access control

**Status:** Implemented and verified with automated Go tests. No commit was created.

### Implemented

- Added the `admin`, `read`, and `write` roles. `admin` and `write` allow GET, HEAD, POST, PUT, PATCH, and DELETE; `read` allows GET and HEAD only. Unsupported methods are rejected with HTTP 403.
- Added `INTELLIGATE_API_KEY_ROLES` as a JSON key-to-role mapping. Malformed JSON, empty or duplicate/ambiguous keys, and unknown roles fail startup without exposing credentials. Setting this variable together with `INTELLIGATE_API_KEYS` is rejected as ambiguous.
- Preserved legacy `INTELLIGATE_API_KEYS`; those keys continue to receive `admin` access. Migrate to the JSON role mapping for least-privilege keys.
- Authentication runs before authorization; authenticated role data is attached to a private request-context key. Roles supplied in headers or query parameters are ignored. API keys and known role-assertion headers are stripped before proxying.
- Middleware order is request logging → authentication → authorization → rate limiting → routing/backend selection/retries/proxy. Missing or invalid keys return 401; authenticated requests without permission return 403. Unauthorized and forbidden requests do not consume limiter tokens or reach upstreams. Public `GET /health` remains accessible.
- Updated README, architecture, API, and testing documentation with the role matrix, configuration, migration behavior, middleware order, and limitations.

### Automated verification

- `gofmt` completed for all changed Go files.
- `go test ./...` from `gateway`: passed for `cmd/gateway`, `internal/auth`, `internal/balancer`, `internal/circuitbreaker`, `internal/health`, `internal/proxy`, `internal/ratelimit`, and `internal/router`.
- `go test -count=10 ./internal/auth`: passed.
- `go test -count=10 ./cmd/gateway`: passed.
- Tests cover method permissions for each role, legacy admin compatibility, malformed/unknown/duplicate role configuration, conflicting configuration variables, spoofed role headers/query parameters, unauthorized/forbidden limiter ordering, upstream metadata stripping, and concurrent role isolation.
- No live manual RBAC test, race-detector run, or performance benchmark is claimed.

### Limitations

- Shared API keys only; no JWT, OAuth, or database-backed identity.
- Configuration changes require restarting the gateway; there is no distributed authentication or key-rotation service.
- Direct microservice ports bypass gateway authentication and authorization.
- TLS and protected secret management are required for exposed deployments.
