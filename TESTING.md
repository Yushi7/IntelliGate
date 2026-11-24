# Testing Strategy

**Status:** Phase 1 includes Go router/proxy tests and pytest service endpoint tests. Phase 2A and 2B include Go Round Robin, active health-check, and gateway integration tests. Phase 3A adds timeout configuration and proxy timeout tests. Phase 3B adds circuit-breaker state, selector, proxy-outcome, and configuration tests. Phase 3C adds controlled retry, deadline, and admission tests. Phase 4A adds token-bucket, cleanup, configuration, and gateway-middleware tests. Phase 4B.1 adds API-key parsing, constant-time validation, configuration, middleware-order, health-exemption, and unauthorized-request tests.

## Principles

- Test behavior at the narrowest practical level, then validate component interactions.
- Keep tests deterministic; use controlled clocks, fixtures, and isolated dependencies where needed.
- Include success, invalid input, timeout, dependency failure, and recovery cases.
- Avoid placing credentials or sensitive client data in test fixtures, logs, or committed configuration.
- Run tests in CI after the corresponding implementation exists.

## Go unit testing

Use the Go standard `testing` package for gateway packages. Phase 2B tests health-aware selection and probing. Phase 3A tests timeout configuration, timely/timed-out upstreams, 504 mapping, concurrent request handling, and existing error/path behavior. Phase 3B tests breaker defaults and config validation, consecutive failure threshold and reset, cooldown, HALF-OPEN admission limits, successful close, failed reopen, stale permits, cancellation, concurrent outcomes, backend isolation, Round Robin exclusion/re-entry, health-probe independence, and gateway-vs-application response classification. Phase 3C tests GET/HEAD-only retries, bounded attempts, backoff order/cancellation, replayable request bodies, alternate and single-backend selection, health/breaker admission, HALF-OPEN trials, application-status non-retry, body-stream non-retry, client-vs-gateway deadlines, retry-budget timeout, and permit completion. Phase 4A tests token consumption/refill, Retry-After rounding, per-IP separation and IPv4/IPv6 parsing, concurrent capacity, cleanup, environment validation, disabled behavior, health exemption, spoofed forwarding headers, and rejection before routing. Existing 404, 429, 502, 503, 504, and path/query tests provide regression coverage. JWT/role-based authorization, distributed shared quotas, adaptive scoring, EWMA, and metrics remain planned.

## FastAPI testing with Pytest

Use Pytest with FastAPI's test client for each service. Cover endpoint contracts, validation, expected status codes, domain behavior, and persistence interactions. Use isolated test data and a dedicated test database or suitable test doubles; do not point automated tests at production data.

## Gateway test commands

Run the gateway tests:

```powershell
Set-Location gateway
go test ./...
go test -count=10 ./internal/auth
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

Once components exist, verify gateway-to-Redis rate-limit behavior, gateway-to-PostgreSQL configuration/data access, Prometheus scraping, and service startup/readiness using isolated dependencies. Docker-based integration environments may be introduced with the implementation; none exists in Phase 0.

## Gateway-to-service communication

Verify service path routing, method/body/header forwarding, response propagation, timeouts, unavailable backends, malformed upstream responses, and behavior when no backend is eligible. Confirm that client credentials and sensitive headers are not accidentally forwarded to unrelated services.

## API-key authentication testing (implemented in Phase 4B.1)

Automated tests cover multiple configured keys, whitespace trimming and malformed configuration, missing/invalid/repeated keys returning the same 401 JSON, required keys at startup, concurrent validation, no API key forwarding to services, and authentication before rate limiting and routing. They also verify that `GET /health` is public and unauthorized requests neither consume a token nor reach a backend. JWT, role, expiration, and revocation tests do not apply to this shared-key phase.

For a local manual check, use only placeholder values (do not put real keys in documentation or source control). Configure and start the gateway:

```powershell
$env:INTELLIGATE_AUTH_ENABLED = "true"
$env:INTELLIGATE_API_KEYS = "local-placeholder-key-one,local-placeholder-key-two"
Set-Location gateway
go run .\cmd\gateway
```

In another terminal, check the unauthenticated and authenticated behavior:

```powershell
try { (Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products).StatusCode } catch { [int]$_.Exception.Response.StatusCode }
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products -Headers @{ "X-API-Key" = "local-placeholder-key-one" }).StatusCode
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/health).StatusCode
```

The expected statuses are 401, the backend response status, and 200 respectively. An API key is stripped before proxying and should not appear in backend logs or responses.

## Rate-limiting testing

Test token consumption, refill timing, bucket capacity, isolation between clients, and consistent decisions across gateway instances when Redis is available. Verify the documented limit response and headers. Test Redis timeout/unavailability behavior once a fail-open or fail-closed policy is selected.

## Load-balancing testing

For Phase 2A/2B/3B routing, validate sequential selection, wraparound, distribution among healthy breaker-admitted instances, concurrency safety, exclusion after health failure or breaker open, half-open recovery re-entry, all-ineligible 503, and 502/504 for attempted upstream failures. Probe failures—not application response codes—change health; breaker outcomes come from upstream transport/body events, not application status codes. For future Adaptive Least-Loaded routing, validate scoring with controlled load/latency inputs, deterministic ties, and changes as signals update.

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

## Performance testing

Use Locust for repeatable load generation. Record workload shape, duration, warm-up, concurrency, service configuration, and routing policy. Measure throughput, average/p50/p95/p99 latency, error rate, backend distribution, rate-limit behavior, and recovery time as described in [EXPERIMENTS.md](./EXPERIMENTS.md). Performance results are pending until real experiments are run.

## Failure and recovery testing

Inject controlled backend failure, latency, and restart/recovery events in a local test environment. Check health detection, traffic exclusion, circuit state, recovery re-entry, error rates, and recovery time. Ensure tests can cleanly restore the environment and do not target external or production systems.

## Current status

Phase 1 service tests and Phase 2A/2B/3A/3B/3C/4A/4B.1 Go routing, health-check, timeout, circuit-breaker, retry, rate-limit, authentication, and simulated integration tests are implemented. Record manual verification results in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md) only after running them. Tests for JWT/roles, distributed quotas, databases, adaptive routing, and monitoring are not implemented.
