# API Documentation

**Status:** Phase 1 implements the service `GET /`, `GET /health`, and sample collection GET endpoints listed below, plus gateway proxy routing. The remaining operations and policies are planned. The gateway has no separate health, readiness, or metrics endpoint in Phase 1.

## Conventions

- Gateway service routes are shown relative to the gateway base URL.
- Gateway route ownership is: `/api/v1/products/*` → Product Service; `/api/v1/orders/*` → Order Service; `/api/v1/users/*` → User Service.
- The gateway listens on port `8080`; local services listen on Product `8001`, Order `8002`, and User `8003`.
- The gateway preserves the full incoming path and query when proxying. It does not strip the `/api/v1/<service>` prefix.
- Implemented service routes return sample data and do not require authentication. Do not treat them as production APIs.
- The user-order listing operation is owned by the Order Service, not the User Service. Although it may be expressed conceptually as `GET /api/v1/users/{user_id}/orders`, it is exposed at `GET /api/v1/orders/users/{user_id}` to follow the Order Service route namespace.
- JSON examples are illustrative contract proposals, not observed responses.
- Protected routes are planned to use `X-API-Key: <client-api-key>`. Never commit a real key.
- Authentication policy for health, readiness, metrics, and administrative endpoints must be explicitly configured before deployment.
- Error response schema, pagination, and exact field validation remain to be finalized.

## Gateway operational endpoints

No gateway-specific health, readiness, or metrics endpoints are implemented in Phase 1. The operational endpoints below remain planned.

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
- **Authentication:** None in Phase 1; API-key enforcement remains planned.

### `GET /api/v1/products/{product_id}` (planned)

- **Purpose:** Retrieve one product.
- **Request:** Path parameter `product_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"prod-123","name":"Example","price":12.5}`; not-found response if absent.
- **Authentication:** Planned API key required.

### `POST /api/v1/products` (planned)

- **Purpose:** Create a product.
- **Request:** Planned JSON, `{"name":"Example","price":12.5}`.
- **Expected response:** `201 Created`, with the created product and assigned identifier.
- **Authentication:** Planned API key required; authorization roles TBD.

## Order service endpoints

### `GET /api/v1/orders` (implemented)

- **Purpose:** Return a small sample list of orders.
- **Request:** No body.
- **Expected response:** `200 OK`, with an `items` array and `total`.
- **Authentication:** None in Phase 1.

### `POST /api/v1/orders` (planned)

- **Purpose:** Create an order.
- **Request:** Planned JSON, `{"user_id":"user-123","items":[{"product_id":"prod-123","quantity":2}]}`.
- **Expected response:** `201 Created`, for example `{"id":"order-123","status":"pending"}`.
- **Authentication:** Planned API key required; user identity binding/authorization TBD.

### `GET /api/v1/orders/{order_id}` (planned)

- **Purpose:** Retrieve an order.
- **Request:** Path parameter `order_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"order-123","status":"pending","items":[]}`; not-found response if absent.
- **Authentication:** Planned API key required; ownership/authorization policy TBD.

### `GET /api/v1/orders/users/{user_id}` (planned)

- **Purpose:** List orders associated with a user. This is an Order Service operation in the Order Service route namespace.
- **Request:** Path parameter `user_id`; pagination TBD.
- **Expected response:** `200 OK`, for example `{"items":[{"id":"order-123","status":"pending"}]}`.
- **Authentication:** Planned API key required; ownership/authorization policy TBD.

## User service endpoints

### `GET /api/v1/users` (implemented)

- **Purpose:** Return a small sample list of users.
- **Request:** No body.
- **Expected response:** `200 OK`, with an `items` array and `total`.
- **Authentication:** None in Phase 1.

### `POST /api/v1/users` (planned)

- **Purpose:** Create a demonstration user.
- **Request:** Planned JSON, `{"name":"Example User","email":"user@example.test"}`. Use non-real test data.
- **Expected response:** `201 Created`, for example `{"id":"user-123","name":"Example User","email":"user@example.test"}`.
- **Authentication:** Planned API key required; credential/password handling is not specified by this proposal.

### `GET /api/v1/users/{user_id}` (planned)

- **Purpose:** Retrieve a user profile.
- **Request:** Path parameter `user_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"user-123","name":"Example User","email":"user@example.test"}`; not-found response if absent.
- **Authentication:** Planned API key required; authorization policy TBD.

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

## Planned error cases

In Phase 1, the gateway returns `404 Not Found` for an unmapped path and `502 Bad Gateway` when a mapped backend is unavailable. Otherwise, it forwards the backend's response. Authentication failure, rate limiting, invalid-request, and other planned error schemas are not implemented.
