"""Fixed mixed GET workload for the IntelliGate gateway."""

import os

from locust import HttpUser, between, task


class IntelliGateUser(HttpUser):
    """A user sends the same weighted Product, Order, and User requests."""

    wait_time = between(0.1, 0.3)

    def on_start(self):
        api_key = os.environ.get("INTELLIGATE_BENCHMARK_API_KEY", "").strip()
        if not api_key:
            raise RuntimeError(
                "Set INTELLIGATE_BENCHMARK_API_KEY before starting Locust."
            )
        self.client.headers.update({"X-API-Key": api_key})

    @task(1)
    def get_products(self):
        self.client.get("/api/v1/products", name="GET /api/v1/products")

    @task(1)
    def get_orders(self):
        self.client.get("/api/v1/orders", name="GET /api/v1/orders")

    @task(1)
    def get_users(self):
        self.client.get("/api/v1/users", name="GET /api/v1/users")
