# IntelliGate

**An Adaptive and Intelligent API Gateway for Secure and Dynamic Microservices Traffic Management**

IntelliGate is an API gateway and demonstration environment for routing traffic to an e-commerce application's Product, Order, and User services. The project is being built to explore secure client access, traffic management, service resilience, and observability in a modular microservices system.

> **Phase 3B status:** The Go gateway and three FastAPI demonstration services are implemented. The gateway uses configurable per-service Round Robin pools, active health checks, upstream request timeouts, and per-backend circuit breakers. Authentication, rate limiting, adaptive routing, persistence, monitoring, dashboard, and deployment features remain planned.

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
| Gateway and service tests | Implemented in Phase 1 |
| API key authentication | Planned |
| Per-client token bucket rate limiting backed by Redis | Planned |
| Round Robin baseline routing | Implemented in Phase 2A |
| Adaptive least-loaded routing | Planned |
| EWMA backend latency tracking | Planned |
| Backend health monitoring and recovery | Implemented in Phase 2B |
| Circuit breaker | Implemented in Phase 3B |
| Prometheus metrics and Grafana dashboards | Planned |
| Traffic anomaly detection using Python and scikit-learn | Planned |
| Product, Order, and User FastAPI services | Implemented in Phase 1 |
| React + TypeScript dashboard | Planned |
| Docker Compose demonstration environment | Planned |
| Locust performance experiments | Planned |

## Architecture

Clients send service requests to the Go gateway on port 8080. It round-robins among configured instances that pass direct `/health` checks and have an available circuit-breaker permit. Breakers are per instance: after 5 qualifying transport failures, an instance is excluded for a 10-second cooldown before a half-open trial is allowed. A failed trial reopens it; the configured number of complete upstream responses closes it. Configure thresholds with the `INTELLIGATE_CIRCUIT_BREAKER_*` environment variables. A 5-second upstream timeout is the default (`INTELLIGATE_UPSTREAM_TIMEOUT` overrides it); timeout before response headers returns 504, ordinary proxy failures return 502, and no eligible backend returns 503. Requests are not retried. Gateway `GET /health` remains independent of backend state. Redis, PostgreSQL, Prometheus, and Grafana integrations remain planned. See [ARCHITECTURE.md](./ARCHITECTURE.md) for details.

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
| Later | Add authentication, rate limiting, adaptive routing, and metrics | Pending |
| Later | Add dashboard, anomaly detection, load experiments, and deployment automation | Pending |

Phase 1, Phase 2A, Phase 2B, Phase 3A, and Phase 3B implementations were approved and are recorded below. Later-phase work remains pending.

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
Set-Location gateway
go run .\cmd\gateway
```

The supported variables are `PRODUCT_SERVICE_URLS`, `ORDER_SERVICE_URLS`, and `USER_SERVICE_URLS`; each defaults to its existing localhost port when unset. The example secondary port is illustrative and not reserved by the project.

The gateway listens on `http://localhost:8080`. Example: `Invoke-RestMethod http://localhost:8080/api/v1/products`.

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

- **Implemented:** Phase 1 Go reverse proxy and three FastAPI services; Phase 2A per-service Round Robin; Phase 2B health checks and health-aware selection; Phase 3A configurable upstream request timeout.
- **Not implemented:** Authentication, rate limiting, adaptive routing, circuit breaking, database/cache integration, monitoring, dashboard, load tests, Docker configuration, and CI/CD workflows.
- **Verification:** Current test and manual integration procedures are recorded in [TESTING.md](./TESTING.md); actual run results are recorded in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md).
