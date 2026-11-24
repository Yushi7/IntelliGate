from fastapi import FastAPI

app = FastAPI(title="IntelliGate Product Service")


@app.get("/")
def root():
    return {"service": "product", "status": "ok"}


@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/api/v1/products")
def list_products():
    return {
        "items": [
            {"id": "prod-101", "name": "Wireless Keyboard", "price": 49.99},
            {"id": "prod-102", "name": "USB-C Hub", "price": 34.5},
        ],
        "total": 2,
    }
