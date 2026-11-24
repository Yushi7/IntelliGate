package main

import (
	"encoding/json"
	"log"
	"net/http"
	"time"

	"intelligate/gateway/internal/proxy"
	"intelligate/gateway/internal/router"
)

func main() {
	products, err := proxy.New("http://localhost:8001")
	if err != nil {
		log.Fatal(err)
	}
	orders, err := proxy.New("http://localhost:8002")
	if err != nil {
		log.Fatal(err)
	}
	users, err := proxy.New("http://localhost:8003")
	if err != nil {
		log.Fatal(err)
	}

	server := &http.Server{
		Addr:              ":8080",
		Handler:           newHandler(products, orders, users),
		ReadHeaderTimeout: 5 * time.Second,
	}

	log.Printf("IntelliGate listening on %s", server.Addr)
	log.Fatal(server.ListenAndServe())
}

func newHandler(products, orders, users http.Handler) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(struct {
			Status  string `json:"status"`
			Service string `json:"service"`
		}{Status: "ok", Service: "intelligate-gateway"})
	})
	mux.Handle("/", router.New(
		router.Route{Prefix: "/api/v1/products", Handler: products},
		router.Route{Prefix: "/api/v1/orders", Handler: orders},
		router.Route{Prefix: "/api/v1/users", Handler: users},
	))
	return requestLogger(mux)
}

func requestLogger(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("method=%s path=%s", r.Method, r.URL.Path)
		next.ServeHTTP(w, r)
	})
}
