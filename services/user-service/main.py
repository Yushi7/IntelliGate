from fastapi import FastAPI

app = FastAPI(title="IntelliGate User Service")


@app.get("/")
def root():
    return {"service": "user", "status": "ok"}


@app.get("/health")
def health():
    return {"status": "ok"}


@app.get("/api/v1/users")
def list_users():
    return {
        "items": [
            {
                "id": "user-301",
                "name": "Alex Morgan",
                "email": "alex.morgan@example.test",
            }
        ],
        "total": 1,
    }
