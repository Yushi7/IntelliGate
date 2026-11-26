# Performance Evaluation Methodology

**Status:** Phase 8 Locust workload and comparison tooling are implemented. Benchmark results remain pending until measured runs are recorded. The limited manual Round Robin demonstration below is not a performance benchmark.

## Research question

Under equivalent backend pools and controlled workloads, how do Round Robin and Adaptive Least-Loaded routing compare in request distribution, throughput, latency, errors, and recovery behavior?

## Tool and setup

Use Locust to generate controlled HTTP traffic through the gateway. Run the same service instances, request mix, resource limits, test duration, warm-up, and concurrency profile for each routing policy. Record environment details, software revisions, configuration, workload parameters, and failure injection conditions so runs can be interpreted and repeated.

Avoid changing multiple independent variables between comparison runs. Repeat runs where practical and report measurement spread rather than relying on a single run. Do not claim causality from uncontrolled measurements.

## Metrics

- **Throughput:** Successfully completed requests per second.
- **Average response time:** Arithmetic mean end-to-end response latency.
- **p50 latency:** Median end-to-end response latency.
- **p95 latency:** 95th percentile end-to-end response latency.
- **p99 latency:** 99th percentile end-to-end response latency.
- **Error rate:** Failed or unexpected responses divided by total requests, with failure categories recorded.
- **Backend request distribution:** Requests per backend instance, including excluded/unhealthy instances where relevant.
- **Rate-limiting effectiveness:** Allowed and rejected requests compared with the configured per-client token bucket policy.
- **Backend recovery time:** Elapsed time from a controlled failure/recovery event to the documented healthy/routable condition.

## Planned scenarios

1. **Baseline routing:** Round Robin under steady, evenly provisioned backends.
2. **Adaptive routing:** Adaptive Least-Loaded under the same workload and backend setup.
3. **Uneven load:** Introduce controlled differences in backend latency or capacity, then compare distribution and latency.
4. **Rate-limit verification:** Drive known per-client request rates around configured policy limits.
5. **Failure and recovery:** Make a backend unavailable and restore it; record detection, exclusion, and recovery timing.

The current automated workload issues equal-weight GET requests to Product, Order, and User routes, with a 100–300 ms per-user wait. Users, spawn rate, a separate warm-up duration, and measured duration are passed to `load-tests/run_benchmark.ps1`. The runner verifies `intelligate_routing_strategy_info`, runs warm-up in a separate Locust process with no measured CSV output, then saves measured Locust output and metadata under `benchmark-results/phase-8/`. Warm-up exercises the gateway and lets adaptive mode gather samples, but the measured process starts fresh Locust clients. Before/after Prometheus snapshots include backend selections, health, breaker state, EWMA latency, in-flight requests, successful samples, rate-limit rejections, and gateway process start time. See `TESTING.md` for exact PowerShell commands. The comparison script rejects incomplete results and mismatches in workload, addresses, backend URL declarations, and recorded rate-limit, retry, timeout, and breaker configuration.

The baseline and Adaptive runs must use the same gateway/API key, backend URL lists and instance counts, health state, retry and breaker settings, rate-limit policy, host resources, Locust workload, users, spawn rate, warm-up, and measured duration. Only `INTELLIGATE_ROUTING_STRATEGY` should differ. Backend URLs and configuration are operator-declared mirrors of gateway startup settings: health series can confirm IDs/counts and current health, but cannot prove that the gateway actually uses the declared URL strings. Compare on a controlled local setup and verify the gateway startup environment yourself. Locust reports end-to-end client latency, while gateway EWMA reports upstream TTFB; these measure different boundaries. Prometheus counter snapshots bracket the measured interval, with scrape cadence and requests already in flight limiting exact boundary precision. The runner preserves default rate limiting and defaults (10 tokens/second, burst 20) are unchanged. The documented routing benchmark explicitly sets the limiter to 1000/1000 while keeping it enabled; this is supported by both memory mode and Redis fixed-point bounds. HTTP 429 responses remain failures. If 429s occur, treat the run as limiter-constrained and do not present it as an unconstrained routing comparison.

## Phase 2A live Round Robin demonstration

This was a manual routing demonstration, not an automated test or a performance benchmark.

- Gateway: `localhost:8080`
- Product Service instance 1: `localhost:8001`
- Product Service instance 2: `localhost:8011`
- Configuration: `PRODUCT_SERVICE_URLS=http://localhost:8001,http://localhost:8011`
- Request sequence: six sequential `GET /api/v1/products` requests sent through the gateway.
- Instance 1 received 3 requests; all returned HTTP 200.
- Instance 2 received 3 requests; all returned HTTP 200.
- The gateway logged all six incoming requests.
- Observed distribution was 3:3, matching Round Robin with two configured backends.
- Both instances returned identical sample product data.

No throughput, latency, concurrency, or other performance measurements were taken. This demonstration does not establish health-aware routing, automatic failover, or adaptive routing.

## Result tables

Performance benchmark values below remain **pending**. The manual demonstration above is not a benchmark result. Populate these tables only from completed, documented benchmark runs.

| Policy / scenario | Throughput (req/s) | Average (ms) | p50 (ms) | p95 (ms) | p99 (ms) | Error rate |
| --- | ---: | ---: | ---: | ---: | ---: | ---: |
| Round Robin / steady | Pending | Pending | Pending | Pending | Pending | Pending |
| Adaptive / steady | Pending | Pending | Pending | Pending | Pending | Pending |
| Round Robin / uneven load | Pending | Pending | Pending | Pending | Pending | Pending |
| Adaptive / uneven load | Pending | Pending | Pending | Pending | Pending | Pending |

| Scenario | Backend request distribution | Rate-limit effectiveness | Backend recovery time |
| --- | --- | --- | --- |
| Round Robin | Pending | Pending | Pending |
| Adaptive Least-Loaded | Pending | Pending | Pending |

## Reporting

Include date, environment, code revision, Locust workload definition, routing configuration, backend count/capacity, warm-up and measurement duration, raw or summarized output, and known limitations with each result. Do not replace pending fields with estimates or illustrative values.
