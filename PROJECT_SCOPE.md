# IntelliGate Project Scope

**Project title:** An Adaptive and Intelligent API Gateway for Secure and Dynamic Microservices Traffic Management
**Document status:** Phase 0 proposal. Requirements describe intended behavior; they are not evidence of implemented functionality.

## 1. Introduction

IntelliGate proposes a custom API gateway as the central entry point to an e-commerce demonstration system composed of Product, Order, and User microservices. The project will investigate secure access, traffic distribution, resilience, and operational visibility using the fixed technology stack documented here and in [README.md](./README.md).

## 2. Problem statement

In a microservices application, clients need a stable entry point while services may vary in availability and capacity. Without a common gateway, authentication, rate policies, routing, and monitoring can be duplicated across services or handled inconsistently. A static routing policy may also fail to account for changing backend load. IntelliGate will provide a controlled environment to implement and evaluate these concerns; no such runtime behavior is implemented in Phase 0.

## 3. Objectives

- Build a Go-based gateway using standard HTTP reverse-proxy capabilities.
- Provide API-key-based client authentication and per-client token bucket rate limiting.
- Use Round Robin as the baseline and compare it with Adaptive Least-Loaded routing.
- Track backend latency with EWMA and monitor health.
- Apply circuit breaking and support unhealthy-instance exclusion and recovery.
- Expose metrics to Prometheus and visualize them with Grafana.
- Provide Product, Order, and User demonstration services using Python and FastAPI.
- Evaluate behavior with controlled Locust workloads and documented metrics.
- Explore traffic anomaly detection using Python and scikit-learn.

## 4. Motivation

The project provides a practical way to study how gateway design affects security, fairness, latency, throughput, and availability. Comparing a simple baseline with an adaptive strategy under repeatable workloads can make design trade-offs observable and suitable for an academic project report.

## 5. Functional requirements

The future system is expected to:

1. Accept HTTP requests through a single gateway entry point.
2. Authenticate protected client requests using API keys.
3. Enforce per-client token bucket rate limits with Redis-backed shared state.
4. Route service requests to configured backend instances.
5. Support Round Robin and Adaptive Least-Loaded policies.
6. Track backend response latency using EWMA.
7. Check backend health and avoid routing to unhealthy instances.
8. Use a circuit breaker to stop calls to persistently failing backends and permit recovery checks.
9. Expose health, readiness, and Prometheus metrics endpoints.
10. Persist application records and gateway configuration using PostgreSQL where appropriate.
11. Provide the Product, Order, and User service API surfaces.
12. Support controlled load and failure experiments; report results only after execution.
13. Provide a React/TypeScript dashboard and anomaly-detection component in later phases, subject to the design details in [ARCHITECTURE.md](./ARCHITECTURE.md).

## 6. Non-functional requirements

- **Modularity:** Keep gateway concerns separated into focused Go packages and services.
- **Security:** Do not commit credentials; protect client and administrative routes; minimize sensitive data in logs and metrics.
- **Reliability:** Bound timeouts and failures, monitor backend health, and support recovery.
- **Performance:** Measure throughput and latency under controlled and repeatable load.
- **Observability:** Make gateway behavior inspectable through metrics and dashboards.
- **Maintainability:** Use the language-appropriate standard testing tools and document interfaces.
- **Reproducibility:** Record experiment configuration and avoid presenting unmeasured results.
- **Configurability:** Keep backend instances, routing, and operational configuration manageable; exact persistence boundaries are to be finalized during implementation.

## 7. Core features

Planned core features are API key authentication, distributed per-client rate limiting, reverse-proxy request routing, Round Robin balancing, health checks, and basic gateway/service observability.

## 8. Advanced features

Planned advanced features are Adaptive Least-Loaded routing, EWMA latency signals, circuit breaking and instance recovery, Prometheus/Grafana monitoring, anomaly detection, and controlled comparative performance evaluation.

## 9. Technology stack

| Concern                            | Selected technology     |
| ---------------------------------- | ----------------------- |
| Gateway                            | Go                      |
| Backend services                   | Python + FastAPI        |
| Frontend                           | React + TypeScript      |
| Styling                            | Tailwind CSS            |
| Persistent database                | PostgreSQL              |
| Distributed rate-limit/cache state | Redis                   |
| Metrics and visualization          | Prometheus + Grafana    |
| Load generation                    | Locust                  |
| Containers                         | Docker + Docker Compose |
| Tests                              | Go Testing + Pytest     |
| CI/CD                              | GitHub Actions          |
| Optional deployment                | AWS EC2                 |
| Anomaly detection                  | Python + scikit-learn   |

No technology change is in scope without technical justification and explicit approval.

## 10. Project scope

The project covers the gateway, the three demonstration services, request-routing policies, gateway security and resilience, monitoring, a dashboard, and performance evaluation. The gateway is the client-facing entry point. The services are demonstration backends, not production e-commerce systems.

Phase 0 is limited to creating the planned directory structure and documentation. It does not create application code, dependency manifests, runtime configuration, database schemas, deployment files, or experiment results. A Docker Compose file is intentionally deferred until it can describe a real, verified environment.

## 11. Project limitations

- This is an academic demonstration, not a production-ready commercial gateway.
- No production identity provider, secrets manager, multi-region design, or high-availability control plane is specified.
- The initial service domain and expected workloads are limited to an e-commerce demonstration.
- Adaptive routing quality depends on the selected load signal, measurement interval, and workload; these must be experimentally justified.
- Anomaly detection is exploratory and will require suitable data and validation before operational use.
- Deployment to AWS EC2 is optional and is not part of Phase 0.

## 12. Expected outcomes

- A modular gateway and service demonstration.
- An implementation of baseline and adaptive routing policies.
- Tests and observability sufficient to inspect correctness and failure behavior.
- Repeatable measurements comparing the policies under documented workloads.
- A transparent project report that distinguishes measured results from planned work.
