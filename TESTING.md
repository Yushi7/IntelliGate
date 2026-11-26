# Testing Strategy

**Status:** Phase 1 includes Go router/proxy tests and pytest service endpoint tests. Phase 2A and 2B include Go Round Robin, active health-check, and gateway integration tests. Phase 3A adds timeout configuration and proxy timeout tests. Phase 3B adds circuit-breaker state, selector, proxy-outcome, and configuration tests. Phase 3C adds controlled retry, deadline, and admission tests. Phase 4A adds in-memory token-bucket, cleanup, configuration, and gateway-middleware tests. Phase 4B.1 adds API-key authentication tests; Phase 4B.2 adds role configuration, per-method authorization, middleware-order, request-context isolation, and gateway integration tests. Phase 5A adds Redis-backed shared-quota, atomicity, TTL, failure-mode, startup, and shutdown tests. Phase 5B adds Prometheus registry, HTTP/component instrumentation, metrics-listener lifecycle/isolation, concurrency, and monitoring configuration.

## Principles

- Test behavior at the narrowest practical level, then validate component interactions.
- Keep tests deterministic; use controlled clocks, fixtures, and isolated dependencies where needed.
- Include success, invalid input, timeout, dependency failure, and recovery cases.
- Avoid placing credentials or sensitive client data in test fixtures, logs, or committed configuration.
- Run tests in CI after the corresponding implementation exists.

## Go unit testing

Use the Go standard `testing` package for gateway packages. Phase 2B tests health-aware selection and probing. Phase 3A tests timeout configuration, timely/timed-out upstreams, 504 mapping, concurrent request handling, and existing error/path behavior. Phase 3B tests breaker defaults and config validation, consecutive failure threshold and reset, cooldown, HALF-OPEN admission limits, successful close, failed reopen, stale permits, cancellation, concurrent outcomes, backend isolation, Round Robin exclusion/re-entry, health-probe independence, and gateway-vs-application response classification. Phase 3C tests GET/HEAD-only retries, bounded attempts, backoff order/cancellation, replayable request bodies, alternate and single-backend selection, health/breaker admission, HALF-OPEN trials, application-status non-retry, body-stream non-retry, client-vs-gateway deadlines, retry-budget timeout, and permit completion. Phase 4A tests token consumption/refill, Retry-After rounding, per-IP separation and IPv4/IPv6 parsing, concurrent capacity, cleanup, environment validation, disabled behavior, health exemption, spoofed forwarding headers, and rejection before routing. Phase 5B tests final HTTP status/duration capture, bounded method/route labels, preserved streaming/flush behavior, one HTTP metric per request despite retries, backend and breaker state initialization/transitions, retry and security/rate-limit metrics, store failures distinct from quota 429, observer concurrency, label sensitivity, and metrics listener isolation/lifecycle. Phase 6 tests adaptive scoring, EWMA samples, cold-start exploration, cooldown ranking, deterministic injected-clock behavior, retry ordering, and proxy measurement lifecycle. Existing 401, 403, 404, 429, 502, 503, 504, and path/query tests provide regression coverage. JWT/OAuth and distributed quotas remain unimplemented.

## FastAPI testing with Pytest

Use Pytest with FastAPI's test client for each service. Cover endpoint contracts, validation, expected status codes, domain behavior, and persistence interactions. Use isolated test data and a dedicated test database or suitable test doubles; do not point automated tests at production data.

## Gateway test commands

Run the gateway tests:

```powershell
Set-Location gateway
go test ./...
go test -count=10 ./internal/ratelimit
go test -count=10 ./internal/auth
go test -count=10 ./internal/observability
go test -count=10 ./internal/balancer
go test -count=10 ./internal/circuitbreaker
go test -count=10 ./internal/health
go test -count=10 ./cmd/gateway
go vet ./...
go test -race ./...
Set-Location ..
```

The race-enabled run covers concurrent selection, health-state updates, and proxy requests. On Windows, it requires a compatible 64-bit C compiler for cgo.

For each service, create its own virtual environment, install its requirements, and run pytest. From the repository root:

```powershell
python -m venv services\product-service\.venv
services\product-service\.venv\Scripts\python.exe -m pip install -r services\product-service\requirements.txt
services\product-service\.venv\Scripts\python.exe -m pytest services\product-service

python -m venv services\order-service\.venv
services\order-service\.venv\Scripts\python.exe -m pip install -r services\order-service\requirements.txt
services\order-service\.venv\Scripts\python.exe -m pytest services\order-service

python -m venv services\user-service\.venv
services\user-service\.venv\Scripts\python.exe -m pip install -r services\user-service\requirements.txt
services\user-service\.venv\Scripts\python.exe -m pytest services\user-service
```

## Manual gateway-to-service integration

1. In three separate PowerShell terminals from the repository root, start the services:

   ```powershell
   services\product-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir services\product-service --host 127.0.0.1 --port 8001
   services\order-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir services\order-service --host 127.0.0.1 --port 8002
   services\user-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir services\user-service --host 127.0.0.1 --port 8003
   ```

2. In another terminal, start the gateway:

   ```powershell
   $env:INTELLIGATE_AUTH_ENABLED = "true"
   $env:INTELLIGATE_API_KEYS = "local-placeholder-key-one,local-placeholder-key-two"
   Set-Location gateway
   go run .\cmd\gateway
   ```

3. From a fifth terminal, confirm each route returns service-specific JSON:

   ```powershell
   $headers = @{ "X-API-Key" = "local-placeholder-key-one" }
   Invoke-RestMethod http://localhost:8080/api/v1/products -Headers $headers
   Invoke-RestMethod http://localhost:8080/api/v1/orders -Headers $headers
   Invoke-RestMethod http://localhost:8080/api/v1/users -Headers $headers
   Invoke-RestMethod http://localhost:8080/health
   ```

4. Confirm service root and health endpoints directly, and verify an unmapped gateway path returns 404:

   ```powershell
   Invoke-RestMethod http://localhost:8001/
   Invoke-RestMethod http://localhost:8001/health
   try { Invoke-WebRequest http://localhost:8080/not-a-service } catch { [int]$_.Exception.Response.StatusCode }
   ```

5. Stop one service with `Ctrl+C`. Until its next health probe completes, a request may still select it and receive 502. After the failed probe, requests should use only healthy instances. Restart the service and verify it becomes eligible after a successful probe. If all instances for a service are stopped, the route returns 503 after they are marked unhealthy. The gateway's own `/health` remains 200. Automated proxy tests verify path and query preservation.

### Health-aware Round Robin across multiple local instances

The existing service ports remain the single-instance defaults. To try two Product instances, use any free secondary port (for example `8011`; this is an example, not a required assignment):

1. Start a second Product Service in another terminal:

   ```powershell
   services\product-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir services\product-service --host 127.0.0.1 --port 8011
   ```

2. In the gateway terminal, configure both URLs and start the gateway:

   ```powershell
   $env:PRODUCT_SERVICE_URLS = "http://localhost:8001,http://localhost:8011"
   $env:INTELLIGATE_UPSTREAM_TIMEOUT = "5s"
   $env:INTELLIGATE_API_KEYS = "local-placeholder-key-one,local-placeholder-key-two"
   Set-Location gateway
   go run .\cmd\gateway
   ```

3. Send multiple requests to `http://localhost:8080/api/v1/products` with `X-API-Key: local-placeholder-key-one`. Both Uvicorn access logs should show `/health` probes and application traffic. The automated tests use distinguishable test backends to verify round-robin traffic, health exclusion and recovery, all-unhealthy 503, path/query preservation, and 502 if a backend fails between probes.

4. Stop one Product Service instance and wait for the next probe (up to 5 seconds). Requests should be routed only to the remaining healthy instance. Restart the stopped instance and verify that it re-enters selection after its next successful probe.

5. Stop both Product instances and wait for the next health checks. `GET /api/v1/products` should return 503 while `GET /health` at the gateway should continue returning 200. Restart at least one instance and verify routing resumes after a successful probe.

### Manual upstream-timeout check

Run this standard-library-only delayed backend in one PowerShell terminal. It answers health probes immediately and delays application responses for 10 seconds:

```powershell
@'
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
import time

class Handler(BaseHTTPRequestHandler):
    def do_GET(self):
        if self.path == "/health":
            body = b'{"status":"ok"}'
            self.send_response(200)
            self.send_header("Content-Type", "application/json")
            self.send_header("Content-Length", str(len(body)))
            self.end_headers()
            self.wfile.write(body)
            return
        time.sleep(10)
        body = b'{"delayed":true}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

ThreadingHTTPServer(("127.0.0.1", 8001), Handler).serve_forever()
'@ | python -
```

In another terminal, route Product traffic to that backend and configure a 2-second timeout:

```powershell
Set-Location gateway
$env:INTELLIGATE_API_KEYS = "local-placeholder-key-one,local-placeholder-key-two"
$env:PRODUCT_SERVICE_URLS = "http://127.0.0.1:8001"
$env:INTELLIGATE_UPSTREAM_TIMEOUT = "2s"
go run .\cmd\gateway
```

From a third terminal, the product request should report 504, while gateway liveness remains 200:

```powershell
try { (Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-placeholder-key-one" }).StatusCode } catch { [int]$_.Exception.Response.StatusCode }
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/health).StatusCode
```

The default is `5s` when `INTELLIGATE_UPSTREAM_TIMEOUT` is unset. The timeout cancels a proxy request; if it expires after upstream response headers have been sent, the gateway cannot replace the status and the response may be truncated.

## Integration testing

Redis-backed rate-limit integration tests use a real Redis instance when available. They create a unique IntelliGate test namespace, clean only keys in that namespace, and never flush a Redis database. Set `INTELLIGATE_TEST_REDIS_ADDR` to use a non-default local test server; otherwise tests try `localhost:6379` and skip Redis-only cases if it is unavailable. Prometheus registry/handler behavior and API/metrics listener lifecycle have automated tests; live scraping through Docker Desktop and Grafana UI verification require a local monitoring-stack run. PostgreSQL configuration/data access and deployment readiness remain planned.

## Prometheus and Grafana monitoring (Phase 5B)

The gateway API and metrics listener start and shut down together. Metrics are exposed only as `GET /metrics` on the separate `INTELLIGATE_METRICS_LISTEN_ADDR` listener (default `:9091`), not on the authenticated API listener. Automated gateway tests verify startup/bind errors, graceful shutdown, successful scraping without API credentials, and that API auth/RBAC/rate-limit middleware does not wrap the metrics handler. They also verify that rate-limit 503 store errors increment `store_unavailable`, not the 429 `exhausted` reason.

To manually start the monitoring stack from the repository root in PowerShell, set local Grafana credentials first:

```powershell
$env:GRAFANA_ADMIN_USER = "intelligate-admin"
$env:GRAFANA_ADMIN_PASSWORD = "replace-with-a-strong-local-password"
docker compose -f observability\docker-compose.yml up -d
docker compose -f observability\docker-compose.yml ps
```

In another terminal, with the backend services running if you want application metrics, start the gateway natively:

```powershell
Set-Location gateway
$env:INTELLIGATE_API_KEYS = "local-placeholder-key"
$env:INTELLIGATE_METRICS_LISTEN_ADDR = ":9091"
go run .\cmd\gateway
```

Then verify the scrape endpoint and monitoring UI:

```powershell
Invoke-WebRequest -UseBasicParsing http://localhost:9091/metrics
Invoke-WebRequest -UseBasicParsing http://localhost:9090/targets
```

Prometheus should show the `intelligate-gateway` target as UP. Open `http://localhost:3000`, sign in with the environment-supplied Grafana username/password, and open the provisioned **IntelliGate Overview** dashboard. It includes request rate/total, status distribution, p50/p95/p99 latency, backend health/selection, breaker state/transitions, retry attempts/outcomes, rate-limit rejections, and security denials. Prometheus and Grafana are bound to localhost ports 9090 and 3000; named volumes preserve their data. Clean up the containers without deleting volumes:

```powershell
docker compose -f observability\docker-compose.yml down
```

The Prometheus container scrapes the Windows host through `host.docker.internal:9091` every five seconds. The metrics listener has no API-key authentication; firewall or otherwise restrict port 9091 to trusted monitoring clients. A successful local browser/Grafana demonstration is not implied by automated Go tests.

## Gateway-to-service communication

Verify service path routing, method/body/header forwarding, response propagation, timeouts, unavailable backends, malformed upstream responses, and behavior when no backend is eligible. Confirm that client credentials and sensitive headers are not accidentally forwarded to unrelated services.

## API-key authentication and RBAC testing (implemented in Phases 4B.1/4B.2)

Automated tests cover multiple configured keys, strict JSON role mapping, malformed/duplicate/unknown-role configuration, ambiguity when both key variables are set, and legacy keys retaining `admin`. Method tests cover all six supported methods for `admin`/`write`, GET/HEAD-only access for `read`, and 403 for denied or unsupported methods. Integration tests verify client-supplied role headers/query parameters cannot escalate privileges, forbidden/unauthorized requests do not consume limiter tokens or reach upstreams, key and role headers are not forwarded, public health behavior, and concurrent role isolation. Missing/invalid keys remain 401; authenticated method denial is 403.

For a local role-based check, use only placeholder values (do not put real keys in documentation or source control). Configure exactly one key source and start the gateway:

```powershell
$env:INTELLIGATE_AUTH_ENABLED = "true"
$env:INTELLIGATE_API_KEY_ROLES = '{"local-admin-key":"admin","local-read-key":"read","local-write-key":"write"}'
Remove-Item Env:\INTELLIGATE_API_KEYS -ErrorAction SilentlyContinue
Set-Location gateway
go run .\cmd\gateway
```

In another terminal, check read access, forbidden write access, admin access, and public health:

```powershell
try { (Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products).StatusCode } catch { [int]$_.Exception.Response.StatusCode }
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-read-key" }).StatusCode
try { (Invoke-WebRequest -UseBasicParsing -Method Post http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-read-key" }).StatusCode } catch { [int]$_.Exception.Response.StatusCode }
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-admin-key" }).StatusCode
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/health).StatusCode
```

Expected behavior: unauthenticated API request returns 401; read-role GET and admin GET return the backend response; read-role POST returns 403 before contacting the service; public health returns 200. Legacy `INTELLIGATE_API_KEYS` remains supported and assigns every key `admin`; migrate to `INTELLIGATE_API_KEY_ROLES` for least privilege, and never set both.

## Rate-limiting testing (implemented in Phases 4A and 5A)

Automated memory-store tests cover token consumption/refill, burst capacity, independent clients, IPv4/IPv6 parsing, Retry-After rounding, concurrent capacity, cleanup, public health, and disabled behavior. Redis integration tests cover full initial burst, exhaustion, fixed-point refill, Retry-After, isolated client buckets, quota sharing across separate store instances, concurrent atomic consumption, TTL expiry, request cancellation, operation timeout, Redis close, and fail-open/fail-closed behavior. Startup tests cover defaults, invalid configuration, Redis connectivity failures, successful initialization, and avoiding the memory cleanup loop for Redis mode. Redis-specific tests skip only when no Redis endpoint is available.

Run the repeated limiter and gateway checks from `gateway`:

```powershell
go test -count=10 ./internal/ratelimit
go test -count=10 ./cmd/gateway
```

### Local Redis setup and two-gateway quota demonstration

The memory store is the default and does not require Redis. For Redis mode, start a local Redis 7 container if one is not already running:

```powershell
docker run --name intelligate-redis -p 6379:6379 -d redis:7-alpine
```

Start the Product, Order, and User services as described above. In two separate terminals, start gateway instances with identical Redis, rate-limit, and placeholder authentication configuration; use a different listen address for each:

```powershell
$env:INTELLIGATE_RATE_LIMIT_STORE = "redis"
$env:INTELLIGATE_REDIS_ADDR = "localhost:6379"
$env:INTELLIGATE_RATE_LIMIT_RATE = "0.000001"
$env:INTELLIGATE_RATE_LIMIT_BURST = "20"
$env:INTELLIGATE_API_KEYS = "local-placeholder-key"
$env:INTELLIGATE_LISTEN_ADDR = "127.0.0.1:8080" # use :8081 in the second terminal
Set-Location gateway
go run .\cmd\gateway
```

From another PowerShell terminal, send 12 requests through the first instance and 8 through the second, then verify the next request is rate limited across the shared bucket:

```powershell
$headers = @{ "X-API-Key" = "local-placeholder-key" }
1..12 | ForEach-Object { Invoke-RestMethod http://127.0.0.1:8080/api/v1/products -Headers $headers | Out-Null }
1..8 | ForEach-Object { Invoke-RestMethod http://127.0.0.1:8081/api/v1/products -Headers $headers | Out-Null }
try {
    Invoke-WebRequest -UseBasicParsing http://127.0.0.1:8080/api/v1/products -Headers $headers
} catch {
    [int]$_.Exception.Response.StatusCode
    $_.Exception.Response.Headers["Retry-After"]
}
```

With the example's deliberately low refill rate, the final request should return `429` and a positive `Retry-After`. Redis outages return `503` by default; set `INTELLIGATE_RATE_LIMIT_REDIS_FAILURE_MODE=open` to log a generic warning and allow requests. Never use real API keys or Redis passwords in this example. All gateways must observe the same canonical client IP; forwarded headers are not trusted.

The Redis bucket uses a SHA-256 digest of the canonical IP as its key suffix, server time in the Lua script, and a 10-minute idle TTL. The fixed-point representation has 1,000,000 units per token and Redis mode supports rate/burst values from 0.000001 through 1000 tokens. These integration tests validate behavior, not throughput or performance. The current client configuration is for one Redis address and does not configure TLS, Sentinel, or Cluster.

## Load-balancing testing

For Phase 2A/2B/3B routing, validate sequential selection, wraparound, distribution among healthy breaker-admitted instances, concurrency safety, exclusion after health failure or breaker open, half-open recovery re-entry, all-ineligible 503, and 502/504 for attempted upstream failures. Phase 6 adaptive tests validate EWMA-based load scoring, exploration, cooldowns, deterministic ties, clock behavior, and measured-attempt lifecycle. Probe failures—not application response codes—change health; breaker outcomes come from upstream transport/body events, not application status codes.

## Circuit breaker and retry testing (implemented in Phases 3B and 3C)

The Go tests use an injected clock for deterministic breaker threshold, cooldown, HALF-OPEN, failure/recovery, and stale-permit transitions. Retry backoff is injectable in balancer tests so delay order and cancellation can be verified without waiting through production delays. Integration tests verify independent per-instance breakers, Round Robin exclusion and re-entry, fresh admission for each attempt, that successful health probes do not close breakers, application 4xx/5xx responses do not count as failures, and only pre-header gateway transport errors trigger retries. Client cancellation/deadline abandons a permit; response-body failures after headers do not retry.

Run the repeated reliability checks from the gateway directory:

```powershell
go test -count=10 ./internal/balancer
go test -count=10 ./internal/proxy
go test -count=10 ./internal/circuitbreaker
```

## Rate-limit testing (implemented in Phase 4A)

The token-bucket tests use an injectable clock for deterministic refill and expiration checks. Concurrent requests share a fixed clock to prove acceptance never exceeds bucket capacity; cleanup and access run concurrently under the limiter's map lock. Gateway tests verify startup configuration, disabled-mode compatibility, `GET /health` exemption, Retry-After/JSON responses, and that rejected requests do not reach the selector. Authentication tests verify unauthorized requests are rejected before consuming the bucket. Repeat these tests with:

```powershell
go test -count=10 ./internal/ratelimit
go test -count=10 ./cmd/gateway
go test -count=10 ./internal/auth
```

## Performance testing (Phase 8)

The `load-tests/` directory contains an equal-weight Locust workload for `GET /api/v1/products`, `GET /api/v1/orders`, and `GET /api/v1/users`, a PowerShell runner, and a dependency-free comparison script. The API key is read from `INTELLIGATE_BENCHMARK_API_KEY`; it is not stored in source or result metadata. The runner checks that Locust is installed before creating a run, verifies the active strategy gauge, performs a separate warm-up run, records the measured Locust CSVs, saves operator-declared gateway configuration, and captures backend selection, health, breaker, EWMA, in-flight, successful-sample, and rate-limit rejection metrics before and after measurement. A run is marked complete only after its outputs and metadata pass validation; a failure retains a `failed` metadata record and any partial artifacts.

### Install and prepare

From the repository root, start the three FastAPI services, Prometheus/Grafana, and the gateway as described above. Prometheus must scrape the gateway metrics listener. Use identical backend URL lists and all other gateway settings for both scenarios; only change the routing strategy. Set the same API key in the gateway and Locust terminal sessions. These prompts avoid putting a key literal in shell history:

```powershell
py -3 -m venv load-tests\.venv
.\load-tests\.venv\Scripts\Activate.ps1
python -m pip install -r load-tests\requirements.txt
$env:INTELLIGATE_BENCHMARK_API_KEY = Read-Host "API key configured in the gateway"
```

For a meaningful backend distribution comparison, run one additional instance of each service on ports `8011`, `8012`, and `8013`. From the repository root, start these in separate service terminals:

```powershell
& .\services\product-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir .\services\product-service --host 127.0.0.1 --port 8011
& .\services\order-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir .\services\order-service --host 127.0.0.1 --port 8012
& .\services\user-service\.venv\Scripts\python.exe -m uvicorn main:app --app-dir .\services\user-service --host 127.0.0.1 --port 8013
```

In the gateway PowerShell session, set the same API key and backend lists for both runs. Keep authentication enabled and explicitly set an identical benchmark-only limiter configuration of 1,000 tokens/second with a 1,000-token burst. The limiter stays enabled; these values are within the Redis fixed-point range and are intentionally high relative to this 20-user workload. They do not change the gateway defaults (10/second, burst 20). The remaining values below state the retry, timeout, and breaker settings that the runner records; retries are fixed in code, while timeout and breaker settings are configurable:

```powershell
$secureKey = Read-Host "API key for gateway" -AsSecureString
$env:INTELLIGATE_API_KEYS = [System.Net.NetworkCredential]::new("", $secureKey).Password
$env:PRODUCT_SERVICE_URLS = "http://localhost:8001,http://localhost:8011"
$env:ORDER_SERVICE_URLS = "http://localhost:8002,http://localhost:8012"
$env:USER_SERVICE_URLS = "http://localhost:8003,http://localhost:8013"
$env:INTELLIGATE_RATE_LIMIT_ENABLED = "true"
$env:INTELLIGATE_RATE_LIMIT_RATE = "1000"
$env:INTELLIGATE_RATE_LIMIT_BURST = "1000"
$env:INTELLIGATE_RATE_LIMIT_STORE = "memory"
$env:INTELLIGATE_REDIS_ADDR = "localhost:6379"
$env:INTELLIGATE_REDIS_DB = "0"
$env:INTELLIGATE_RATE_LIMIT_REDIS_FAILURE_MODE = "closed"
$env:INTELLIGATE_UPSTREAM_TIMEOUT = "5s"
$env:INTELLIGATE_CIRCUIT_BREAKER_FAILURE_THRESHOLD = "5"
$env:INTELLIGATE_CIRCUIT_BREAKER_OPEN_COOLDOWN = "10s"
$env:INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_MAX_REQUESTS = "1"
$env:INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_SUCCESS_THRESHOLD = "1"
$env:INTELLIGATE_ROUTING_STRATEGY = "round_robin"
Set-Location gateway
go run .\cmd\gateway
```

Keep the gateway process running. In the Locust PowerShell session (with the virtual environment active), securely set the same key and declare the exact same backend URL lists and relevant gateway configuration. These values are written into metadata but the key itself is not:

```powershell
$secureKey = Read-Host "API key for Locust" -AsSecureString
$env:INTELLIGATE_BENCHMARK_API_KEY = [System.Net.NetworkCredential]::new("", $secureKey).Password
$productUrls = "http://localhost:8001,http://localhost:8011"
$orderUrls = "http://localhost:8002,http://localhost:8012"
$userUrls = "http://localhost:8003,http://localhost:8013"
$repositoryRoot = git rev-parse --show-toplevel
Set-Location $repositoryRoot
$common = @{
    ProductBackendUrls = $productUrls
    OrderBackendUrls = $orderUrls
    UserBackendUrls = $userUrls
    Users = 20
    SpawnRate = 5
    WarmupSeconds = 30
    Duration = "5m"
    RateLimitRate = 1000
    RateLimitBurst = 1000
    RateLimitStore = "memory"
    RateLimitRedisAddress = "localhost:6379"
    RateLimitRedisDB = 0
    RateLimitFailureMode = "closed"
    UpstreamTimeout = "5s"
    BreakerFailureThreshold = 5
    BreakerOpenCooldown = "10s"
    BreakerHalfOpenMaxRequests = 1
    BreakerHalfOpenSuccessThreshold = 1
}
.\load-tests\run_benchmark.ps1 -Strategy round_robin @common
```

Stop the gateway with Ctrl+C. In its PowerShell session, change only the strategy and restart it:

```powershell
$env:INTELLIGATE_ROUTING_STRATEGY = "adaptive"
go run .\cmd\gateway
```

Run Adaptive with the exact same parameters, changing only the strategy:

```powershell
.\load-tests\run_benchmark.ps1 -Strategy adaptive @common
```

`Users`, `SpawnRate`, `WarmupSeconds`, and `Duration` are configurable; both runs must use matching values. The separate 30-second warm-up uses the same user count and spawn rate, but has no measured CSV output. The measured run starts with fresh Locust clients, so the warm-up exercises the gateway/backends and allows adaptive exploration before measurement, but does not preserve client connections. The runner requires the warm-up to allow the complete user ramp plus two seconds. It writes a unique directory under `benchmark-results\phase-8\` containing Locust CSVs, metadata, and Prometheus snapshots. Backend URL lists/counts and rate-limit (including Redis address/database but never its password), timeout, retry, breaker, Locust version, and workload fingerprints are recorded. Gateway settings are operator declarations mirrored from its startup environment because the gateway has no safe configuration-discovery endpoint; the runner checks the declared backend IDs/counts against health metrics, but health metrics alone cannot prove the actual URL lists match. The comparison rejects differences in declared configuration, code/workload fingerprints, workload, addresses, warm-up, or health.

Run Adaptive after restarting the gateway with only `INTELLIGATE_ROUTING_STRATEGY` changed. Compare the latest run directories after reviewing their names:

```powershell
$roundRobinRun = Get-ChildItem .\benchmark-results\phase-8\round_robin-* -Directory | Sort-Object Name | Select-Object -Last 1
$adaptiveRun = Get-ChildItem .\benchmark-results\phase-8\adaptive-* -Directory | Sort-Object Name | Select-Object -Last 1
python .\load-tests\compare_results.py --round-robin $roundRobinRun.FullName --adaptive $adaptiveRun.FullName
```

The comparison writes timestamped JSON and Markdown reports under `benchmark-results\phase-8\`. It validates required Locust CSVs, completed metadata, configuration equality, health, gateway process stability, and non-resetting Prometheus counters before reporting throughput, average, median, P95/P99 latency, failure rate, selection deltas, and rate-limit rejection deltas. Locust counts HTTP 429 responses as failures. The runner records `exhausted` quota rejections separately; runs with 429s are limiter-constrained and should not be presented as an unconstrained routing-performance result. Do not disable the limiter to remove failures. The example's 1000/1000 setting remains enabled and is only for the benchmark gateway process; it does not change production defaults. Locust latency is end-to-end through the gateway, not upstream TTFB. Snapshot deltas bracket the measured run and can include completion of requests already in flight at the measurement boundary; Prometheus scrape cadence also limits timing precision.

See [EXPERIMENTS.md](./EXPERIMENTS.md) for metric definitions and interpretation notes. Generated benchmark files remain ignored by Git.

## Failure and recovery testing

Inject controlled backend failure, latency, and restart/recovery events in a local test environment. Check health detection, traffic exclusion, circuit state, recovery re-entry, error rates, and recovery time. Ensure tests can cleanly restore the environment and do not target external or production systems.

## Current status

Phase 1 service tests and Phase 2A/2B/3A/3B/3C/4A/4B.1/4B.2/5A/6 Go routing, adaptive routing, health-check, timeout, circuit-breaker, retry, memory/Redis rate-limit, API-key authentication, RBAC, and integration tests are implemented. Phase 8 adds repeatable Locust workloads; performance measurements remain pending until benchmark runs are completed and recorded. Tests for JWT/OAuth, distributed authentication, and persistent database integration are not implemented.
