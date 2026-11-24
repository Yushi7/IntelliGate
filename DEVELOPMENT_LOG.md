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
