# IntelliGate

**An Adaptive and Intelligent API Gateway for Secure and Dynamic Microservices Traffic Management**

IntelliGate is an API gateway and demonstration environment for routing traffic to an e-commerce application's Product, Order, and User services. The project is being built to explore secure client access, traffic management, service resilience, and observability in a modular microservices system.

> **Phase 4B.1 status:** The Go gateway and three FastAPI demonstration services are implemented. The gateway uses configurable per-service Round Robin pools, active health checks, upstream request timeouts, per-backend circuit breakers, bounded retries for eligible GET/HEAD transport failures, API-key authentication, and an in-memory per-client-IP token bucket. Distributed rate-limit storage, JWT/role-based authorization, adaptive routing, persistence, monitoring, dashboard, and deployment features remain planned.

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
| Gateway and service tests | Implemented in Phase 1 |
| Shared/distributed rate limiting backed by Redis | Planned |
| Round Robin baseline routing | Implemented in Phase 2A |
| Adaptive least-loaded routing | Planned |
| EWMA backend latency tracking | Planned |
| Backend health monitoring and recovery | Implemented in Phase 2B |
| Circuit breaker | Implemented in Phase 3B |
| Controlled upstream retries | Implemented in Phase 3C |
| In-memory rate limiting | Implemented in Phase 4A |
| Prometheus metrics and Grafana dashboards | Planned |
| Traffic anomaly detection using Python and scikit-learn | Planned |
| Product, Order, and User FastAPI services | Implemented in Phase 1 |
| React + TypeScript dashboard | Planned |
| Docker Compose demonstration environment | Planned |
| Locust performance experiments | Planned |

## Architecture

Clients send service requests to the Go gateway on port 8080. The gateway authenticates every `/api/v1/*` request with `X-API-Key` before applying rate limits or routing. `INTELLIGATE_API_KEYS` supplies comma-separated keys; authentication is enabled by default, and startup fails if enabled without valid keys. Only `GET /health` is public. The key is removed before proxying and is not logged. Then an in-memory token bucket applies per-client-IP limits: 10 tokens/second with a capacity of 20 by default. `INTELLIGATE_RATE_LIMIT_ENABLED=false` disables rate limiting; `INTELLIGATE_RATE_LIMIT_RATE` and `INTELLIGATE_RATE_LIMIT_BURST` override positive values. Client identity comes from the parsed `RemoteAddr`; forwarded-IP headers are not trusted. `GET /health` is also exempt from rate limiting. Inactive buckets expire after 10 minutes and are cleaned every minute. A rejected request receives 429 with `Retry-After`; the quota is process-local, not shared across gateway instances.

The gateway round-robins among configured instances that pass direct `/health` checks and have an available circuit-breaker permit. Breakers are per instance: after 5 qualifying transport failures, an instance is excluded for a 10-second cooldown before a half-open trial is allowed. A failed trial reopens it; the configured number of complete upstream responses closes it. Configure thresholds with the `INTELLIGATE_CIRCUIT_BREAKER_*` environment variables. A 5-second upstream timeout is the default (`INTELLIGATE_UPSTREAM_TIMEOUT` overrides it); GET and HEAD requests retry eligible pre-header transport failures up to twice, with 100ms and 200ms backoffs. The overall retry budget is `(3 × upstream timeout) + 300ms` (15.3 seconds by default). GET/HEAD bodies require `GetBody` for replay. Completed application responses and response-body streaming failures are not retried. Timeout before headers returns 504, exhausted ordinary proxy failures return 502, and no initially eligible backend returns 503. Gateway `GET /health` remains independent of backend state. Distributed Redis rate limiting, PostgreSQL, Prometheus, and Grafana integrations remain planned. See [ARCHITECTURE.md](./ARCHITECTURE.md) for details.

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
│       ├── auth/       # API key authentication
│       ├── proxy/      # implemented
│       ├── router/     # implemented
│       ├── middleware/ # planned
│       ├── health/     # active backend probes
│       ├── circuitbreaker/ # per-backend circuit state
│       └── metrics/    # planned
├── services/
│   ├── product-service/{main.py,requirements.txt,test_main.py}
│   ├── order-service/{main.py,requirements.txt,test_main.py}
│   └── user-service/{main.py,requirements.txt,test_main.py}
├── dashboard/
├── monitoring/
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

Phase 0 created the directory structure; Phase 1 adds gateway and service source under those directories. `docker-compose.yml` remains deferred until a verified containerized environment is designed.

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
| Later | Add JWT/role-based authorization, distributed rate limiting, adaptive routing, and metrics | Pending |
| Later | Add dashboard, anomaly detection, load experiments, and deployment automation | Pending |

Phase 1, Phase 2A, Phase 2B, Phase 3A, Phase 3B, Phase 3C, Phase 4A, and Phase 4B.1 implementations are recorded below. Later-phase work remains pending.

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

## Run tests

```powershell
Set-Location gateway
go test ./...
Set-Location ..
```

For each service, install its requirements in that service's virtual environment, then run `python -m pytest services\<service-folder>` from the repository root. See [TESTING.md](./TESTING.md) for the complete service commands and manual integration checks.

## Prerequisites

For Phase 1, install Go and Python. Node.js, Docker, PostgreSQL, Redis, and Locust are not required to run the current gateway and services.

## Future execution

There is no Docker deployment configuration. Run the gateway and services directly as described below.

## Current implementation status

- **Implemented:** Phase 1 Go reverse proxy and three FastAPI services; Phase 2A per-service Round Robin; Phase 2B health checks and health-aware selection; Phase 3A configurable upstream request timeout; Phase 3B per-backend circuit breakers; Phase 3C bounded controlled retries; Phase 4A in-memory per-client-IP token bucket; Phase 4B.1 API key authentication.
- **Not implemented:** JWT/role-based authorization, distributed rate-limit storage, adaptive routing, database/cache integration, monitoring, dashboard, load tests, Docker configuration, and CI/CD workflows.
- **Verification:** Current test and manual integration procedures are recorded in [TESTING.md](./TESTING.md); actual run results are recorded in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md).
