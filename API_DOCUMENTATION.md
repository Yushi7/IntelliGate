# API Documentation

**Status:** Phase 1 implements the service `GET /`, `GET /health`, sample collection GET endpoints, gateway proxy routing, and gateway `GET /health`. Phase 2A adds round-robin distribution; Phase 2B restricts selection to backends whose direct `/health` probe returned 2xx; Phase 3A adds configurable upstream request timeouts; Phase 3B adds per-backend circuit breakers; Phase 3C adds bounded retries for eligible GET/HEAD transport failures; Phase 4A adds in-memory per-client-IP token-bucket rate limiting; Phase 4B.1 adds API-key authentication; Phase 4B.2 adds role-based access control.

## Conventions

- Gateway service routes are shown relative to the gateway base URL.
- Gateway route ownership is: `/api/v1/products/*` → Product Service; `/api/v1/orders/*` → Order Service; `/api/v1/users/*` → User Service.
- The gateway listens on port `8080`; single-instance defaults are Product `8001`, Order `8002`, and User `8003`.
- Optional multiple instances are configured with comma-separated full URLs in `PRODUCT_SERVICE_URLS`, `ORDER_SERVICE_URLS`, and `USER_SERVICE_URLS`. No additional port assignment is mandated.
- The gateway probes each backend directly with `GET /health` every 5 seconds and a 2-second timeout. Redirects are not followed. Each service request goes to the next healthy configured instance.
- API-key authentication is enabled by default. Configure either `INTELLIGATE_API_KEY_ROLES` as a JSON key-to-role object, or legacy comma-separated `INTELLIGATE_API_KEYS`; setting both is rejected. Legacy keys receive the `admin` role. `INTELLIGATE_AUTH_ENABLED=false` explicitly disables authentication.
- Every `/api/v1` and `/api/v1/*` request requires one `X-API-Key` header. Missing, invalid, repeated, or invalid credentials return `401 Unauthorized` with `{"error":"unauthorized"}`. Authenticated requests without method permission return `403 Forbidden` with `{"error":"forbidden"}`.
- Roles allow these methods: `admin` allows GET, HEAD, POST, PUT, PATCH, DELETE; `read` allows GET and HEAD; `write` allows GET, HEAD, POST, PUT, PATCH, DELETE. Unsupported methods, including OPTIONS, TRACE, and CONNECT, are denied.
- Gateway `GET /health` is public and bypasses authentication and rate limiting. Middleware order is request logging (method/path only), authentication, authorization, rate limiting, routing, backend selection, retries, then proxying.
- Roles are loaded from authenticated credentials into private request context; client role headers/query parameters are not trusted. API keys and role-assertion headers are stripped before backend proxying.
- This is shared-key authorization only. It does not provide JWT, OAuth, distributed authentication, per-key quotas, or a key rotation service. Configuration changes require gateway restart, and direct service ports bypass gateway auth. Use high-entropy keys from protected secret management and TLS outside local development; never commit real keys.
- Rate limiting is enabled by default. `INTELLIGATE_RATE_LIMIT_ENABLED` accepts `true` or `false` (unset means `true`); `INTELLIGATE_RATE_LIMIT_RATE` defaults to 10 tokens per second and `INTELLIGATE_RATE_LIMIT_BURST` defaults to 20 tokens. When enabled, rate and burst must be finite positive numbers or startup fails.
- The token bucket is in memory, keyed by the IP parsed from `RemoteAddr`; `X-Forwarded-For` and `X-Real-IP` are not trusted. Idle buckets expire after 10 minutes and are cleaned every minute. `GET /health` is exempt. Limits are per gateway process, not global across instances.
- A request with insufficient tokens receives `429 Too Many Requests`, `Retry-After` rounded up to a positive number of seconds, and JSON `{"error":"rate limit exceeded"}`. No global-quota headers are returned.
- If all instances for a service are unhealthy, the gateway returns `503 Service Unavailable`. If a selected backend becomes unreachable between probes, the gateway may retry a GET/HEAD request after a gateway-observed pre-header transport failure, subject to normal health and breaker admission.
- `INTELLIGATE_UPSTREAM_TIMEOUT` configures the maximum upstream request duration using a positive Go duration; it defaults to `5s`. Invalid, zero, or negative values prevent startup.
- If the upstream deadline expires before response headers arrive, the gateway returns `504 Gateway Timeout` with the plain-text message `upstream request timed out`. Ordinary connection/proxy failures remain `502 Bad Gateway`.
- The timeout is derived from the incoming request context and cancels upstream work. If it expires after response headers have been sent, the status cannot be changed and the response may be truncated; responses are not buffered.
- GET and HEAD requests allow up to 2 retries after the initial attempt. Backoff is 100ms then 200ms. An overall deadline is derived from `(3 × INTELLIGATE_UPSTREAM_TIMEOUT) + 300ms` (15.3 seconds with the default timeout). Client cancellation/deadline stops retries immediately.
- Only gateway-observed transport errors before response headers are committed can be retried. Completed upstream responses—including application 4xx/5xx and application 502/503/504—are forwarded without retry. Response-body streaming failures are not retried. GET/HEAD requests with a body are retried only when `GetBody` is available to replay it.
- Each retry is a new selector attempt with fresh health and circuit-breaker admission. A different eligible backend is preferred; a previously attempted backend is reused only if it remains eligible. No eligible backend before the first attempt returns 503; after a retryable failure, if no next backend can be admitted, the last gateway-generated 502/504 is returned.
- Each backend has an independent circuit breaker. Defaults are 5 consecutive qualifying upstream transport failures, a `10s` cooldown, 1 concurrent half-open trial, and 1 successful trial to close. `INTELLIGATE_CIRCUIT_BREAKER_FAILURE_THRESHOLD`, `INTELLIGATE_CIRCUIT_BREAKER_OPEN_COOLDOWN`, `INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_MAX_REQUESTS`, and `INTELLIGATE_CIRCUIT_BREAKER_HALF_OPEN_SUCCESS_THRESHOLD` override these values; invalid or nonpositive values prevent startup.
- Round Robin skips OPEN breakers and backends with no available half-open trial slot. A complete upstream response, including an application 4xx/5xx, is a transport success; application response codes are not breaker failures. A successful health probe does not close an OPEN breaker.
- If no backend is eligible because breakers are OPEN or half-open trial slots are full, the gateway returns `503 Service Unavailable` with a breaker-unavailable message. A gateway-generated upstream 502/504 counts toward that instance's breaker; a backend-originated 502/504 response does not.
- The gateway preserves the full incoming path and query when proxying. It does not strip the `/api/v1/<service>` prefix.
- Implemented service routes return sample data. Requests through the gateway require an API key with a role permitting the HTTP method; direct access to service ports bypasses gateway authentication. Do not treat these sample services as production APIs.
- The user-order listing operation is owned by the Order Service, not the User Service. Although it may be expressed conceptually as `GET /api/v1/users/{user_id}/orders`, it is exposed at `GET /api/v1/orders/users/{user_id}` to follow the Order Service route namespace.
- JSON examples are illustrative contract proposals, not observed responses.
- Use `X-API-Key: <client-api-key>` for gateway API requests. Never commit a real key.
- Only gateway `GET /health` is currently public. Authentication policy for future readiness, metrics, and administrative endpoints must be explicitly configured before deployment.
- Error response schema, pagination, and exact field validation remain to be finalized.

## Gateway operational endpoints

### `GET /health` (implemented)

- **Purpose:** Gateway liveness.
- **Response:** `200 OK`, `{"status":"ok","service":"intelligate-gateway"}`.
- **Routing:** Handled by the gateway and never forwarded to a service. This remains a liveness check and returns 200 independently of backend health.
- **Authentication:** Public; no API key is required.

## Gateway API key authentication and authorization (implemented in Phases 4B.1 and 4B.2)

- **Protected paths:** `/api/v1` and all `/api/v1/*` paths, including currently unmapped/future routes.
- **Header:** `X-API-Key: <configured-key>`.
- **Configuration:** `INTELLIGATE_API_KEY_ROLES` accepts a JSON object such as `{"local-admin-key":"admin","local-read-key":"read","local-write-key":"write"}`. Keys are trimmed; malformed JSON, empty/duplicate/ambiguous keys, or unknown roles prevent startup. Alternatively, legacy `INTELLIGATE_API_KEYS=key-one,key-two` remains supported, assigning each key `admin`. Both configuration variables together are rejected. Authentication defaults to enabled; missing key configuration prevents startup. `INTELLIGATE_AUTH_ENABLED=false` explicitly disables authentication.
- **Role permissions:** `admin`: GET, HEAD, POST, PUT, PATCH, DELETE. `read`: GET, HEAD only. `write`: GET, HEAD, POST, PUT, PATCH, DELETE. Unsupported methods such as OPTIONS, TRACE, and CONNECT are denied with 403.
- **Failure responses:** Missing, invalid, repeated, or comma-combined `X-API-Key` values return `401 Unauthorized`, `Content-Type: application/json`, and `{"error":"unauthorized"}`. A valid key without method permission returns `403 Forbidden` and `{"error":"forbidden"}`.
- **Middleware order:** Request logger → authentication → authorization → rate limiter → route mux/router → health-aware selection/retries/proxy. Unauthorized and forbidden requests do not consume rate-limit tokens or reach a backend.
- **Public exception:** `GET /health` is public and also bypasses rate limiting.
- **Credential handling:** Only SHA-256 digests are retained by the authenticator and compared using constant-time comparison. `X-API-Key`, `X-Role`, `X-User-Role`, and `X-Intelligate-Role` are stripped before forwarding. Roles come from a private request-context value; client-supplied role headers or query parameters cannot grant permissions.
- **Legacy migration:** Existing `INTELLIGATE_API_KEYS` deployments continue to work with unrestricted `admin` permissions. Migrate by replacing it with `INTELLIGATE_API_KEY_ROLES` and assigning least-privilege roles; do not set both variables.
- **Limitations:** No JWT/OAuth, distributed authentication, key rotation/revocation service, or per-key quotas. Configuration changes require restart; direct service ports do not enforce gateway roles. Use high-entropy keys, protected secret management, and TLS in deployed environments.

The following additional operational endpoints remain planned.

### `GET /healthz` (planned)

- **Purpose:** Planned process liveness check.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"status":"ok"}` when the gateway process is alive.
- **Authentication:** Planned public probe; final exposure policy TBD.

### `GET /readyz` (planned)

- **Purpose:** Planned readiness check, including required gateway dependencies.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"status":"ready"}` when ready; non-2xx when not ready.
- **Authentication:** Planned probe endpoint; final exposure policy TBD.

### `GET /metrics` (planned)

- **Purpose:** Planned Prometheus scrape endpoint.
- **Request:** No body.
- **Expected response:** Prometheus exposition format.
- **Authentication:** Scrape access policy TBD; should be restricted to the monitoring network in deployed environments.

## Product service endpoints

Each service also implements `GET /` (service name and status) and `GET /health` (returns `{"status":"ok"}`) directly on its local port.

### `GET /api/v1/products` (implemented)

- **Purpose:** Return the sample product catalog.
- **Request:** No body. Query parameters are accepted by the gateway and forwarded; pagination/filtering is not implemented.
- **Response:** `200 OK`, `{"items":[{"id":"prod-101","name":"Wireless Keyboard","price":49.99},{"id":"prod-102","name":"USB-C Hub","price":34.5}],"total":2}`.
- **Authentication:** API key required when called through the gateway; direct service access is not authenticated.

### `GET /api/v1/products/{product_id}` (planned)

- **Purpose:** Retrieve one product.
- **Request:** Path parameter `product_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"prod-123","name":"Example","price":12.5}`; not-found response if absent.
- **Authorization:** API key with a role permitting GET is required through the gateway.

### `POST /api/v1/products` (planned)

- **Purpose:** Create a product.
- **Request:** Planned JSON, `{"name":"Example","price":12.5}`.
- **Expected response:** `201 Created`, with the created product and assigned identifier.
- **Authorization:** API key with `admin` or `write` role is required through the gateway for POST.

## Order service endpoints

### `GET /api/v1/orders` (implemented)

- **Purpose:** Return a small sample list of orders.
- **Request:** No body.
- **Expected response:** `200 OK`, with an `items` array and `total`.
- **Authentication:** API key required when called through the gateway; direct service access is not authenticated.

### `POST /api/v1/orders` (planned)

- **Purpose:** Create an order.
- **Request:** Planned JSON, `{"user_id":"user-123","items":[{"product_id":"prod-123","quantity":2}]}`.
- **Expected response:** `201 Created`, for example `{"id":"order-123","status":"pending"}`.
- **Authorization:** API key with `admin` or `write` role is required through the gateway for POST; user identity binding remains TBD.

### `GET /api/v1/orders/{order_id}` (planned)

- **Purpose:** Retrieve an order.
- **Request:** Path parameter `order_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"order-123","status":"pending","items":[]}`; not-found response if absent.
- **Authorization:** API key with a role permitting GET is required through the gateway; ownership policy remains TBD.

### `GET /api/v1/orders/users/{user_id}` (planned)

- **Purpose:** List orders associated with a user. This is an Order Service operation in the Order Service route namespace.
- **Request:** Path parameter `user_id`; pagination TBD.
- **Expected response:** `200 OK`, for example `{"items":[{"id":"order-123","status":"pending"}]}`.
- **Authorization:** API key with a role permitting GET is required through the gateway; ownership policy remains TBD.

## User service endpoints

### `GET /api/v1/users` (implemented)

- **Purpose:** Return a small sample list of users.
- **Request:** No body.
- **Expected response:** `200 OK`, with an `items` array and `total`.
- **Authentication:** API key required when called through the gateway; direct service access is not authenticated.

### `POST /api/v1/users` (planned)

- **Purpose:** Create a demonstration user.
- **Request:** Planned JSON, `{"name":"Example User","email":"user@example.test"}`. Use non-real test data.
- **Expected response:** `201 Created`, for example `{"id":"user-123","name":"Example User","email":"user@example.test"}`.
- **Authorization:** API key with `admin` or `write` role is required through the gateway for POST; credential/password handling remains unspecified.

### `GET /api/v1/users/{user_id}` (planned)

- **Purpose:** Retrieve a user profile.
- **Request:** Path parameter `user_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"user-123","name":"Example User","email":"user@example.test"}`; not-found response if absent.
- **Authorization:** API key with a role permitting GET is required through the gateway; profile authorization remains TBD.

## Administrative endpoints

Administrative paths are provisional and must not be exposed publicly without an explicit authorization and network-access design.

### `GET /admin/v1/instances`

- **Purpose:** Planned inspection of configured backend instances and health state.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"items":[{"service":"products","instance":"product-1","healthy":true}]}`.
- **Authentication:** Planned administrator-only authentication; mechanism TBD. A client API key alone is not assumed sufficient.

### `PUT /admin/v1/routing-policy`

- **Purpose:** Planned selection of a routing policy.
- **Request:** Planned JSON, `{"policy":"round_robin"}` or `{"policy":"adaptive_least_loaded"}`.
- **Expected response:** `200 OK`, for example `{"policy":"round_robin"}` after successful validation.
- **Authentication:** Planned administrator-only authentication; mechanism TBD.

### `GET /admin/v1/rate-limits`

- **Purpose:** Planned inspection of configured rate-limit policies, without returning secrets.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"items":[{"client_id":"client-123","capacity":100,"refill_per_second":10}]}`. Actual fields and exposure are TBD.
- **Authentication:** Planned administrator-only authentication; mechanism TBD.

## Gateway error behavior

The gateway returns `401 Unauthorized` with `{"error":"unauthorized"}` for missing or invalid API keys, `403 Forbidden` with `{"error":"forbidden"}` for a method not permitted by the key's role or for unsupported methods, `404 Not Found` for an authenticated and authorized unmapped path, `429 Too Many Requests` when the per-client bucket is empty, `503 Service Unavailable` when no healthy breaker-admitted instance is available before the first attempt, `502 Bad Gateway` for exhausted ordinary proxy failures, and `504 Gateway Timeout` for upstream or retry-budget expiration before response headers arrive. After a retryable failure, if no backend can be admitted, the last gateway-observed 502/504 is returned. Otherwise, the gateway forwards the backend's response. Application responses do not alter health or count as breaker failures and are never retried.
