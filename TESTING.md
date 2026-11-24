# Testing Strategy

**Status:** Phase 1 includes Go router/proxy tests and pytest service endpoint tests. Phase 2A and 2B include Go Round Robin, active health-check, and gateway integration tests. Phase 3A adds timeout configuration and upstream proxy timeout tests. Tests for later features remain planned.

## Principles

- Test behavior at the narrowest practical level, then validate component interactions.
- Keep tests deterministic; use controlled clocks, fixtures, and isolated dependencies where needed.
- Include success, invalid input, timeout, dependency failure, and recovery cases.
- Avoid placing credentials or sensitive client data in test fixtures, logs, or committed configuration.
- Run tests in CI after the corresponding implementation exists.

## Go unit testing

Use the Go standard `testing` package for gateway packages. Phase 2B tests healthy-only selection, distribution and wraparound among healthy instances, all-unhealthy 503, recovery, concurrent selection/health updates, concurrent probes, probe statuses/timeouts/connection failures, periodic recovery, cancellation, path/query preservation, and existing 404/502 behavior. Phase 3A tests default/custom timeout parsing, invalid/zero/negative values, timely upstream responses, timeout-to-504 mapping, and that a slow request does not block a concurrent request. Existing 404, 502, 503, and path/query tests provide regression coverage. Authentication, token-bucket behavior, adaptive scoring, EWMA, circuit breakers, and metrics remain planned.

## FastAPI testing with Pytest

Use Pytest with FastAPI's test client for each service. Cover endpoint contracts, validation, expected status codes, domain behavior, and persistence interactions. Use isolated test data and a dedicated test database or suitable test doubles; do not point automated tests at production data.

## Gateway test commands

Run the gateway tests:

```powershell
Set-Location gateway
go test ./...
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
   Set-Location gateway
   go run .\cmd\gateway
   ```

3. From a fifth terminal, confirm each route returns service-specific JSON:

   ```powershell
   Invoke-RestMethod http://localhost:8080/api/v1/products
   Invoke-RestMethod http://localhost:8080/api/v1/orders
   Invoke-RestMethod http://localhost:8080/api/v1/users
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
   Set-Location gateway
   go run .\cmd\gateway
   ```

3. Send multiple requests to `http://localhost:8080/api/v1/products`. Both Uvicorn access logs should show `/health` probes and application traffic. The automated tests use distinguishable test backends to verify round-robin traffic, health exclusion and recovery, all-unhealthy 503, path/query preservation, and 502 if a backend fails between probes.

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
$env:PRODUCT_SERVICE_URLS = "http://127.0.0.1:8001"
$env:INTELLIGATE_UPSTREAM_TIMEOUT = "2s"
go run .\cmd\gateway
```

From a third terminal, the product request should report 504, while gateway liveness remains 200:

```powershell
try { (Invoke-WebRequest -UseBasicParsing http://localhost:8080/api/v1/products).StatusCode } catch { [int]$_.Exception.Response.StatusCode }
(Invoke-WebRequest -UseBasicParsing http://localhost:8080/health).StatusCode
```

The default is `5s` when `INTELLIGATE_UPSTREAM_TIMEOUT` is unset. The timeout cancels a proxy request; if it expires after upstream response headers have been sent, the gateway cannot replace the status and the response may be truncated.

## Integration testing

Once components exist, verify gateway-to-Redis rate-limit behavior, gateway-to-PostgreSQL configuration/data access, Prometheus scraping, and service startup/readiness using isolated dependencies. Docker-based integration environments may be introduced with the implementation; none exists in Phase 0.

## Gateway-to-service communication

Verify service path routing, method/body/header forwarding, response propagation, timeouts, unavailable backends, malformed upstream responses, and behavior when no backend is eligible. Confirm that client credentials and sensitive headers are not accidentally forwarded to unrelated services.

## Authentication testing

Test valid, missing, invalid, expired/revoked (if supported), and malformed API keys. Verify that protected endpoints reject unauthenticated requests before upstream invocation and that explicitly public operational endpoints follow their documented policy. Test that secrets are not exposed in responses, metrics, or logs.

## Rate-limiting testing

Test token consumption, refill timing, bucket capacity, isolation between clients, and consistent decisions across gateway instances when Redis is available. Verify the documented limit response and headers. Test Redis timeout/unavailability behavior once a fail-open or fail-closed policy is selected.

## Load-balancing testing

For Phase 2A/2B Round Robin, validate sequential selection, wraparound, even distribution among healthy configured instances, concurrency safety, exclusion of failed probes, recovery re-entry, all-unhealthy 503, and 502 for a backend that fails between probes. Probe failures—not application response codes—change health. For future Adaptive Least-Loaded routing, validate scoring with controlled load/latency inputs, deterministic ties, and changes as signals update.

## Circuit breaker testing

Verify Closed-to-Open behavior at the configured threshold, rejection/fallback behavior while Open, Half-Open transition after cooldown, successful closure, failed probe reopening, and isolation between intended breaker scopes. Test concurrent requests around transitions.

## Performance testing

Use Locust for repeatable load generation. Record workload shape, duration, warm-up, concurrency, service configuration, and routing policy. Measure throughput, average/p50/p95/p99 latency, error rate, backend distribution, rate-limit behavior, and recovery time as described in [EXPERIMENTS.md](./EXPERIMENTS.md). Performance results are pending until real experiments are run.

## Failure and recovery testing

Inject controlled backend failure, latency, and restart/recovery events in a local test environment. Check health detection, traffic exclusion, circuit state, recovery re-entry, error rates, and recovery time. Ensure tests can cleanly restore the environment and do not target external or production systems.

## Current status

Phase 1 service tests and Phase 2A/2B Go routing, health-check, and simulated integration tests are implemented. The manual integration procedure above exercises live local routing; record its result in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md) only after running it. Tests for authentication, rate limiting, databases, adaptive routing, monitoring, and circuit breakers are not implemented.
