from fastapi import FastAPI

app = FastAPI(title="IntelliGate Order Service")


@app.get("/")
def root():
    return {"service": "order", "status": "ok"}


@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/api/v1/orders")
def list_orders():
    return {
        "items": [
            {
                "id": "order-201",
                "user_id": "user-301",
                "status": "processing",
                "total": 84.49,
            }
        ],
        "total": 1,
    }
