# IntelliGate

**An Adaptive and Intelligent API Gateway for Secure and Dynamic Microservices Traffic Management**

IntelliGate is a proposed API gateway and demonstration environment for routing traffic to an e-commerce application's Product, Order, and User services. The project is intended to explore secure client access, traffic management, service resilience, and observability in a modular microservices system.

> **Phase 0 status:** Project structure and planning documentation are being established. No gateway, service, dashboard, monitoring configuration, or runnable deployment has been implemented yet. All runtime features below are **planned** unless explicitly marked otherwise.

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
| Phase 0 project structure and documentation | Implemented in this initialization phase |
| API key authentication | Planned |
| Per-client token bucket rate limiting backed by Redis | Planned |
| Round Robin baseline routing | Planned |
| Adaptive least-loaded routing | Planned |
| EWMA backend latency tracking | Planned |
| Backend health monitoring and recovery | Planned |
| Circuit breaker | Planned |
| Prometheus metrics and Grafana dashboards | Planned |
| Traffic anomaly detection using Python and scikit-learn | Planned |
| Product, Order, and User FastAPI services | Planned |
| React + TypeScript dashboard | Planned |
| Docker Compose demonstration environment | Planned |
| Locust performance experiments | Planned |

## Proposed architecture

Clients send requests to a Go gateway, which is intended to authenticate, apply rate limits, select a healthy backend, and proxy requests. The Product, Order, and User services will be implemented in Python with FastAPI. Redis is designated for distributed rate-limit state; PostgreSQL for persistent records and configuration; Prometheus and Grafana for metrics and visualization. Locust will generate controlled workloads. See [ARCHITECTURE.md](./ARCHITECTURE.md) for proposed flows and component responsibilities.

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
│   ├── cmd/
│   └── internal/
│       ├── proxy/
│       ├── router/
│       ├── middleware/
│       ├── health/
│       ├── circuitbreaker/
│       └── metrics/
├── services/
│   ├── product-service/
│   ├── order-service/
│   └── user-service/
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

The directories are intentionally empty at Phase 0; no placeholder source or configuration files have been added. `docker-compose.yml` will be introduced when a verified runnable local environment is designed, rather than as an empty placeholder.

## Development roadmap

| Phase | Scope | Status |
| --- | --- | --- |
| 0 | Initialize structure, scope, architecture, API plan, and verification strategy | Complete; awaiting review |
| 1 | Implement the foundational Go gateway and service communication | Pending developer review and explicit approval |
| Later | Add authentication, rate limiting, routing algorithms, health/resilience, metrics, and services | Pending |
| Later | Add dashboard, anomaly detection, load experiments, and deployment automation | Pending |

No Phase 1 work should begin until Phase 0 has been reviewed and explicitly approved.

## Prerequisites

For future development and execution, install compatible versions of Go, Python, Node.js, Docker with Docker Compose, PostgreSQL, Redis, and Locust. Git and a GitHub account are needed for collaboration and CI/CD. Exact supported versions, environment variables, and commands have not yet been established and will be documented alongside the relevant implementation.

## Future execution

**There is currently no executable application or deployment setup.** Once later phases add the implementation and verified configuration, this section will provide exact setup, environment configuration, startup, and test commands. Do not expect `docker compose up` or service-specific run commands to work in Phase 0.

## Current implementation status

- **Implemented:** Phase 0 directory structure and planning documents.
- **Not implemented:** All application logic and runtime configuration, including the Go gateway, FastAPI services, React dashboard, database/cache integration, monitoring stack, load tests, and CI/CD workflows.
- **Verification:** Phase 0 contents and status are recorded in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md). Planned tests and experiment result tables are intentionally pending.
