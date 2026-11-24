# Proposed API Documentation

**Status:** All endpoints in this document are **planned**. None is implemented or available in Phase 0. Paths and response shapes are provisional API contracts and must be confirmed during implementation.

## Conventions

- Gateway service routes are shown relative to the gateway base URL.
- Proposed gateway route ownership is: `/api/v1/products/*` → Product Service; `/api/v1/orders/*` → Order Service; `/api/v1/users/*` → User Service.
- The user-order listing operation is owned by the Order Service, not the User Service. Although it may be expressed conceptually as `GET /api/v1/users/{user_id}/orders`, it is exposed at `GET /api/v1/orders/users/{user_id}` to follow the Order Service route namespace.
- JSON examples are illustrative contract proposals, not observed responses.
- Protected routes are planned to use `X-API-Key: <client-api-key>`. Never commit a real key.
- Authentication policy for health, readiness, metrics, and administrative endpoints must be explicitly configured before deployment.
- Error response schema, pagination, and exact field validation remain to be finalized.

## Gateway operational endpoints

### `GET /healthz`

- **Purpose:** Planned process liveness check.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"status":"ok"}` when the gateway process is alive.
- **Authentication:** Planned public probe; final exposure policy TBD.

### `GET /readyz`

- **Purpose:** Planned readiness check, including required gateway dependencies.
- **Request:** No body.
- **Expected response:** `200 OK`, for example `{"status":"ready"}` when ready; non-2xx when not ready.
- **Authentication:** Planned probe endpoint; final exposure policy TBD.

### `GET /metrics`

- **Purpose:** Planned Prometheus scrape endpoint.
- **Request:** No body.
- **Expected response:** Prometheus exposition format.
- **Authentication:** Scrape access policy TBD; should be restricted to the monitoring network in deployed environments.

## Product service endpoints

### `GET /api/v1/products`

- **Purpose:** List products; pagination/filter parameters are TBD.
- **Request:** No body. Planned query example: `?limit=20&offset=0`.
- **Expected response:** `200 OK`, for example `{"items":[{"id":"prod-123","name":"Example","price":12.5}],"total":1}`.
- **Authentication:** Planned API key required.

### `GET /api/v1/products/{product_id}`

- **Purpose:** Retrieve one product.
- **Request:** Path parameter `product_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"prod-123","name":"Example","price":12.5}`; not-found response if absent.
- **Authentication:** Planned API key required.

### `POST /api/v1/products`

- **Purpose:** Create a product.
- **Request:** Planned JSON, `{"name":"Example","price":12.5}`.
- **Expected response:** `201 Created`, with the created product and assigned identifier.
- **Authentication:** Planned API key required; authorization roles TBD.

## Order service endpoints

### `POST /api/v1/orders`

- **Purpose:** Create an order.
- **Request:** Planned JSON, `{"user_id":"user-123","items":[{"product_id":"prod-123","quantity":2}]}`.
- **Expected response:** `201 Created`, for example `{"id":"order-123","status":"pending"}`.
- **Authentication:** Planned API key required; user identity binding/authorization TBD.

### `GET /api/v1/orders/{order_id}`

- **Purpose:** Retrieve an order.
- **Request:** Path parameter `order_id`; no body.
- **Expected response:** `200 OK`, for example `{"id":"order-123","status":"pending","items":[]}`; not-found response if absent.
- **Authentication:** Planned API key required; ownership/authorization policy TBD.

### `GET /api/v1/orders/users/{user_id}`

- **Purpose:** List orders associated with a user. This is an Order Service operation in the Order Service route namespace.
- **Request:** Path parameter `user_id`; pagination TBD.
- **Expected response:** `200 OK`, for example `{"items":[{"id":"order-123","status":"pending"}]}`.
- **Authentication:** Planned API key required; ownership/authorization policy TBD.

## User service endpoints

### `POST /api/v1/users`

- **Purpose:** Create a demonstration user.
- **Request:** Planned JSON, `{"name":"Example User","email":"user@example.test"}`. Use non-real test data.
- **Expected response:** `201 Created`, for example `{"id":"user-123","name":"Example User","email":"user@example.test"}`.
- **Authentication:** Planned API key required; credential/password handling is not specified by this proposal.

### `GET /api/v1/users/{user_id}`

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

Protected routes are expected to distinguish authentication failure, rate limiting, not found, invalid request, unavailable backend, and internal gateway failure. Status codes and response schema will be finalized with implementation and documented consistently across services. No specific error response is yet implemented.
