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
- Design and test authentication, rate limiting, balancing, health checks, circuit breaking, and recovery.
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
