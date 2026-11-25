# IntelliGate

**An Adaptive and Intelligent API Gateway for Secure and Dynamic Microservices Traffic Management**

IntelliGate is an API gateway and demonstration environment for routing traffic to an e-commerce application's Product, Order, and User services. The project is being built to explore secure client access, traffic management, service resilience, and observability in a modular microservices system.

> **Phase 5B status:** The Go gateway and three FastAPI demonstration services are implemented. The gateway uses configurable per-service Round Robin pools, active health checks, upstream request timeouts, per-backend circuit breakers, bounded retries for eligible GET/HEAD transport failures, API-key authentication with role-based access control, and per-client-IP token-bucket rate limiting backed by either memory or Redis. Prometheus metrics and a provisioned Grafana overview dashboard are implemented. JWT/OAuth, distributed authentication, adaptive routing, persistence, application containerization, and anomaly detection remain planned.

## Problem

Microservice applications need a consistent entry point to authenticate clients, distribute requests, limit traffic, and respond to backend failures. Implementing these concerns independently in every service can lead to duplicated policies and limited visibility. IntelliGate proposes a central gateway to make these concerns explicit and measurable.

## Objectives

- Establish a modular Go gateway foundation for future traffic management.
- Simulate an e-commerce backend with Python and FastAPI services.
- Compare Round Robin routing with an adaptive least-loaded strategy.
- Plan security, resilience, observability, and reproducible performance evaluation.
- Keep design and implementation status transparent throughout development.

## Key features and status

| Capability | Status |
| --- | --- |
| Go gateway with static service routing and reverse proxy | Implemented in Phase 1 |
| Per-service Round Robin across configured backend instances | Implemented in Phase 2A |
| Active `/health` probes and health-aware Round Robin routing | Implemented in Phase 2B |
| Configurable upstream request timeout | Implemented in Phase 3A; 5-second default |
| Per-backend circuit breakers | Implemented in Phase 3B |
| Bounded retries for eligible GET/HEAD transport failures | Implemented in Phase 3C |
| In-memory per-client-IP token bucket rate limiting | Implemented in Phase 4A |
| API key authentication for `/api/v1/*` | Implemented in Phase 4B.1 |
| API-key role-based access control | Implemented in Phase 4B.2 |
| Gateway and service tests | Implemented in Phase 1 |
| Shared/distributed rate limiting backed by Redis | Implemented in Phase 5A |
| Round Robin baseline routing | Implemented in Phase 2A |
| Adaptive least-loaded routing | Planned |
| EWMA backend latency tracking | Planned |
| Backend health monitoring and recovery | Implemented in Phase 2B |
| Circuit breaker | Implemented in Phase 3B |
| Controlled upstream retries | Implemented in Phase 3C |
| In-memory rate limiting | Implemented in Phase 4A |
| Prometheus metrics and Grafana overview dashboard | Implemented in Phase 5B |
| Traffic anomaly detection using Python and scikit-learn | Planned |
| Product, Order, and User FastAPI services | Implemented in Phase 1 |
| React + TypeScript dashboard | Planned |
| Docker Compose demonstration environment | Planned |
| Locust performance experiments | Planned |

## Architecture

Clients send service requests to the Go gateway on port 8080 by default; `INTELLIGATE_LISTEN_ADDR` can select another host/port for local multi-instance runs. The gateway authenticates every `/api/v1/*` request with `X-API-Key`, authorizes its configured role, then applies rate limits and routing. Use either `INTELLIGATE_API_KEY_ROLES` as a JSON key-to-role object, or legacy comma-separated `INTELLIGATE_API_KEYS`; these variables cannot be set together. Legacy keys receive the `admin` role. Authentication is enabled by default, and startup fails if enabled without valid configuration. `admin` and `write` may use GET, HEAD, POST, PUT, PATCH, and DELETE; `read` may use only GET and HEAD. Denied methods return 403; unauthenticated requests return 401. Only `GET /health` is public. Credentials and role metadata are removed before proxying and roles are never accepted from client headers or query parameters.

Rate limiting is enabled by default. The `memory` store applies 10 tokens/second with a burst of 20 per client IP and is process-local. Set `INTELLIGATE_RATE_LIMIT_STORE=redis` to share buckets through Redis; configure its address, optional password, and database with `INTELLIGATE_REDIS_ADDR`, `INTELLIGATE_REDIS_PASSWORD`, and `INTELLIGATE_REDIS_DB`. Redis mode performs a bounded startup check and never silently falls back to memory. Failure mode is `closed` by default (HTTP 503 on Redis errors) or may be set to `open` (log a generic warning and allow the request). `GET /health` bypasses rate limiting in either mode. Client identity comes from the canonical IP parsed from `RemoteAddr`; forwarded-IP headers are not trusted. Idle state expires after 10 minutes; only memory mode runs a cleanup loop. A denied request receives HTTP 429 with a positive `Retry-After`.

The gateway round-robins among configured instances that pass direct `/health` checks and have an available circuit-breaker permit. Breakers are per instance: after 5 qualifying transport failures, an instance is excluded for a 10-second cooldown before a half-open trial is allowed. A failed trial reopens it; the configured number of complete upstream responses closes it. Configure thresholds with the `INTELLIGATE_CIRCUIT_BREAKER_*` environment variables. A 5-second upstream timeout is the default (`INTELLIGATE_UPSTREAM_TIMEOUT` overrides it); GET and HEAD requests retry eligible pre-header transport failures up to twice, with 100ms and 200ms backoffs. The overall retry budget is `(3 × upstream timeout) + 300ms` (15.3 seconds by default). GET/HEAD bodies require `GetBody` for replay. Completed application responses and response-body streaming failures are not retried. Timeout before headers returns 504, exhausted ordinary proxy failures return 502, and no initially eligible backend returns 503. Gateway `GET /health` remains independent of backend state. The API listens on `:8080`; a separate metrics listener defaults to `:9091` and exposes Prometheus at `/metrics`, outside API authentication and rate limiting. Restrict its network access because it is unauthenticated. See [ARCHITECTURE.md](./ARCHITECTURE.md) for details.

## Technology stack

| Component | Technology |
| --- | --- |
| API Gateway | Go |
| Backend services | Python + FastAPI |
| Frontend | React + TypeScript |
| Styling | Tailwind CSS |
| Database | PostgreSQL |
| Caching and rate limiting | Redis |
| Monitoring | Prometheus |
| Visualization | Grafana |
| Load testing | Locust |
| Containerization | Docker + Docker Compose |
| Testing | Go Testing + Pytest |
| CI/CD | GitHub Actions |
| Deployment | AWS EC2 (optional) |
| Anomaly detection | Python + scikit-learn |

The stack is fixed for this project; any proposed change requires technical justification and explicit approval.

## Project structure

```text
IntelliGate/
├── gateway/
│   ├── go.mod
│   ├── cmd/gateway/main.go
│   └── internal/
│       ├── balancer/   # Round Robin
│       ├── auth/       # API keys and role-based access control
│       ├── proxy/      # implemented
│       ├── router/     # implemented
│       ├── middleware/ # planned
│       ├── health/     # active backend probes
│       ├── circuitbreaker/ # per-backend circuit state
│       └── observability/ # Prometheus metrics and observers
├── services/
│   ├── product-service/{main.py,requirements.txt,test_main.py}
│   ├── order-service/{main.py,requirements.txt,test_main.py}
│   └── user-service/{main.py,requirements.txt,test_main.py}
├── dashboard/
├── observability/
│   ├── docker-compose.yml
│   ├── prometheus/
│   └── grafana/
├── load-tests/
├── docs/
├── scripts/
├── .gitignore
├── README.md
├── PROJECT_SCOPE.md
├── ARCHITECTURE.md
├── DEVELOPMENT_LOG.md
├── TESTING.md
├── EXPERIMENTS.md
└── API_DOCUMENTATION.md
```

The `observability/` Compose stack runs only Prometheus and Grafana; the gateway and services remain native processes. No application-service containerization is included.

## Development roadmap

| Phase | Scope | Status |
| --- | --- | --- |
| 0 | Initialize structure, scope, architecture, API plan, and verification strategy | Complete |
| 1 | Implement the foundational Go gateway and service communication | Implemented; automated tests included |
| 2A | Add Round Robin selection across configured backend instances | Implemented; tests included |
| 2B | Add active backend health checks and health-aware Round Robin | Implemented; tests included |
| 3A | Add configurable upstream request timeouts | Implemented; tests included |
| 3B | Add per-backend circuit breakers | Implemented; tests included |
| 3C | Add bounded retries for eligible upstream transport failures | Implemented; tests included |
| 4A | Add in-memory token-bucket rate limiting per client IP | Implemented; tests included |
| 4B.1 | Add API key authentication for gateway API routes | Implemented; tests included |
| 4B.2 | Add role-based access control for API keys | Implemented; tests included |
| 5A | Add optional Redis-backed shared token-bucket rate limiting | Implemented; tests included |
| 5B | Add Prometheus metrics and provisioned Grafana monitoring | Implemented; tests included |
| Later | Add JWT/OAuth, distributed authentication, and adaptive routing | Pending |
| Later | Add dashboard, anomaly detection, load experiments, and deployment automation | Pending |

Phase 1, Phase 2A, Phase 2B, Phase 3A, Phase 3B, Phase 3C, Phase 4A, Phase 4B.1, Phase 4B.2, Phase 5A, and Phase 5B implementations are recorded below. Later-phase work remains pending.

## Run locally

Open separate PowerShell terminals from the repository root.

```powershell
python -m venv services\product-service\.venv
services\product-service\.venv\Scripts\Activate.ps1
python -m pip install -r services\product-service\requirements.txt
python -m uvicorn main:app --app-dir services\product-service --host 127.0.0.1 --port 8001
```

Repeat in separate terminals for Order (`services\order-service`, port `8002`) and User (`services\user-service`, port `8003`), using each service's own `.venv` and requirements file. These ports are the single-backend defaults. To route a service across multiple instances, configure a comma-separated URL list before starting the gateway. For example, after starting a second Product Service on port `8011`:

```powershell
$env:PRODUCT_SERVICE_URLS = "http://localhost:8001,http://localhost:8011"
$env:INTELLIGATE_API_KEYS = "local-placeholder-key-one,local-placeholder-key-two"
Set-Location gateway
go run .\cmd\gateway
```

The supported variables are `PRODUCT_SERVICE_URLS`, `ORDER_SERVICE_URLS`, and `USER_SERVICE_URLS`; each defaults to its existing localhost port when unset. The example secondary port is illustrative and not reserved by the project.

The gateway listens on `http://localhost:8080`. The placeholder keys above are for local testing only; use protected secret management for real keys and never commit them. API routes require the `X-API-Key` header, for example:

```powershell
Invoke-RestMethod http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-placeholder-key-one" }
Invoke-RestMethod http://localhost:8080/health
```

Authentication is enabled by default. To explicitly disable it for isolated development only, set `INTELLIGATE_AUTH_ENABLED=false`; do not use that mode for exposed deployments.

For role-based local testing, use `INTELLIGATE_API_KEY_ROLES` instead of `INTELLIGATE_API_KEYS`:

```powershell
$env:INTELLIGATE_API_KEY_ROLES = '{"local-admin-key":"admin","local-read-key":"read","local-write-key":"write"}'
Remove-Item Env:\INTELLIGATE_API_KEYS -ErrorAction SilentlyContinue
```

The legacy `INTELLIGATE_API_KEYS` configuration remains supported and assigns every listed key the `admin` role. Do not set both variables. Both JSON-mapped and legacy credentials are placeholders here; use protected secret management for real keys.

## Run tests

```powershell
Set-Location gateway
go test ./...
Set-Location ..
```

For each service, install its requirements in that service's virtual environment, then run `python -m pytest services\<service-folder>` from the repository root. See [TESTING.md](./TESTING.md) for the complete service commands and manual integration checks.

### Optional Redis-backed shared rate limiting

The default `memory` store does not need Redis. For the distributed store, start Redis locally if the `intelligate-redis` container is not already running:

```powershell
docker run --name intelligate-redis -p 6379:6379 -d redis:7-alpine
```

Set `INTELLIGATE_RATE_LIMIT_STORE=redis` and optionally `INTELLIGATE_REDIS_ADDR`, `INTELLIGATE_REDIS_PASSWORD`, and `INTELLIGATE_REDIS_DB` before starting the gateway. Redis mode performs a bounded startup connectivity check and does not fall back to memory. `INTELLIGATE_RATE_LIMIT_REDIS_FAILURE_MODE` defaults to `closed` (HTTP 503 when Redis is unavailable); `open` logs a generic warning and allows the request. See [TESTING.md](./TESTING.md) for a two-instance shared-quota demonstration.

## Prerequisites

For the gateway and services in default memory mode, install Go and Python. Docker is needed for the optional Prometheus/Grafana monitoring stack; Redis is optional, but must be reachable when `INTELLIGATE_RATE_LIMIT_STORE=redis`. Node.js, PostgreSQL, and Locust are not required for the implemented runtime.

## Prometheus and Grafana monitoring

The gateway's metrics listener defaults to `:9091` and is separate from the API listener. Start the monitoring stack from the repository root after setting local Grafana credentials:

```powershell
$env:GRAFANA_ADMIN_USER = "intelligate-admin"
$env:GRAFANA_ADMIN_PASSWORD = "replace-with-a-strong-local-password"
docker compose -f observability\docker-compose.yml up -d
```

In another PowerShell terminal, run the gateway natively from the `gateway` directory (and configure the API key as usual):

```powershell
Set-Location gateway
$env:INTELLIGATE_API_KEYS = "local-placeholder-key"
$env:INTELLIGATE_METRICS_LISTEN_ADDR = ":9091"
go run .\cmd\gateway
```

Prometheus scrapes `host.docker.internal:9091` every five seconds; open Prometheus at `http://localhost:9090` and Grafana at `http://localhost:3000`. Sign in with the credentials set in the shell that starts Compose; the IntelliGate Overview dashboard is provisioned automatically. Metrics are not API-authenticated, so restrict access to port 9091 using host firewall/network controls. Stop the monitoring containers with `docker compose -f observability\docker-compose.yml down`; named data volumes are retained.

The custom metric families cover `intelligate_http_requests_total`, `intelligate_http_request_duration_seconds`, `intelligate_rate_limit_rejections_total`, `intelligate_security_denials_total`, `intelligate_backend_health`, `intelligate_backend_health_transitions_total`, `intelligate_backend_selections_total`, `intelligate_backend_selection_failures_total`, `intelligate_circuit_breaker_state`, `intelligate_circuit_breaker_transitions_total`, `intelligate_upstream_retry_attempts_total`, and `intelligate_upstream_retry_outcomes_total`. Labels use normalized methods/routes, status/reason/stage/state/outcome, service, and stable backend IDs only; raw URLs, IPs, API keys, and arbitrary errors are never labels. See [API_DOCUMENTATION.md](./API_DOCUMENTATION.md) for types and label sets.

## Current implementation status

- **Implemented:** Phase 1 Go reverse proxy and three FastAPI services; Phase 2A per-service Round Robin; Phase 2B health checks and health-aware selection; Phase 3A configurable upstream request timeout; Phase 3B per-backend circuit breakers; Phase 3C bounded controlled retries; Phase 4A in-memory per-client-IP token bucket; Phase 4B.1 API-key authentication; Phase 4B.2 role-based access control; Phase 5A optional Redis-backed shared token buckets; Phase 5B Prometheus instrumentation and Grafana monitoring stack.
- **Not implemented:** JWT/OAuth, distributed authentication, adaptive routing, database integration, application containerization, load tests, and CI/CD workflows.
- **Verification:** Current test and manual integration procedures are recorded in [TESTING.md](./TESTING.md); actual run results are recorded in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md).
