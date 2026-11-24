# Testing Strategy

**Status:** Phase 1 includes Go router/proxy tests and pytest health/sample-endpoint tests for each FastAPI service. The broader tests below are planned for later features.

## Principles

- Test behavior at the narrowest practical level, then validate component interactions.
- Keep tests deterministic; use controlled clocks, fixtures, and isolated dependencies where needed.
- Include success, invalid input, timeout, dependency failure, and recovery cases.
- Avoid placing credentials or sensitive client data in test fixtures, logs, or committed configuration.
- Run tests in CI after the corresponding implementation exists.

## Go unit testing

Use the Go standard `testing` package for gateway packages. Planned targets include route matching, authentication decisions, token-bucket behavior, Round Robin eligibility, adaptive scoring and tie-breaking, EWMA updates, health transitions, circuit-breaker transitions, proxy error handling, and metrics instrumentation. Test concurrent behavior where shared state is introduced.

## FastAPI testing with Pytest

Use Pytest with FastAPI's test client for each service. Cover endpoint contracts, validation, expected status codes, domain behavior, and persistence interactions. Use isolated test data and a dedicated test database or suitable test doubles; do not point automated tests at production data.

## Phase 1 test commands

Run the gateway tests:

```powershell
Set-Location gateway
go test ./...
Set-Location ..
```

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

5. Stop one service with `Ctrl+C`, request its gateway route, and verify the response status is 502. Restart the service afterward. The Go proxy test separately verifies preservation of the original path and query string.

## Integration testing

Once components exist, verify gateway-to-Redis rate-limit behavior, gateway-to-PostgreSQL configuration/data access, Prometheus scraping, and service startup/readiness using isolated dependencies. Docker-based integration environments may be introduced with the implementation; none exists in Phase 0.

## Gateway-to-service communication

Verify service path routing, method/body/header forwarding, response propagation, timeouts, unavailable backends, malformed upstream responses, and behavior when no backend is eligible. Confirm that client credentials and sensitive headers are not accidentally forwarded to unrelated services.

## Authentication testing

Test valid, missing, invalid, expired/revoked (if supported), and malformed API keys. Verify that protected endpoints reject unauthenticated requests before upstream invocation and that explicitly public operational endpoints follow their documented policy. Test that secrets are not exposed in responses, metrics, or logs.

## Rate-limiting testing

Test token consumption, refill timing, bucket capacity, isolation between clients, and consistent decisions across gateway instances when Redis is available. Verify the documented limit response and headers. Test Redis timeout/unavailability behavior once a fail-open or fail-closed policy is selected.

## Load-balancing testing

For Round Robin, validate distribution across eligible instances and exclusion of unhealthy instances. For Adaptive Least-Loaded, validate scoring with controlled load/latency inputs, deterministic ties, and changes as signals update. Compare both policies under the same backend pool and workload.

## Circuit breaker testing

Verify Closed-to-Open behavior at the configured threshold, rejection/fallback behavior while Open, Half-Open transition after cooldown, successful closure, failed probe reopening, and isolation between intended breaker scopes. Test concurrent requests around transitions.

## Performance testing

Use Locust for repeatable load generation. Record workload shape, duration, warm-up, concurrency, service configuration, and routing policy. Measure throughput, average/p50/p95/p99 latency, error rate, backend distribution, rate-limit behavior, and recovery time as described in [EXPERIMENTS.md](./EXPERIMENTS.md). Performance results are pending until real experiments are run.

## Failure and recovery testing

Inject controlled backend failure, latency, and restart/recovery events in a local test environment. Check health detection, traffic exclusion, circuit state, recovery re-entry, error rates, and recovery time. Ensure tests can cleanly restore the environment and do not target external or production systems.

## Current status

Phase 1 unit and service endpoint tests are implemented. The manual integration procedure above exercises live local routing; record its result in [DEVELOPMENT_LOG.md](./DEVELOPMENT_LOG.md) only after running it. Tests for authentication, rate limiting, databases, dynamic routing, monitoring, and resilience are not implemented.
