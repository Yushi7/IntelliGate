package auth

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
)

const APIKeyHeader = "X-API-Key"

type Authenticator struct {
	keyDigests [][sha256.Size]byte
}

func ParseKeys(value string) ([]string, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("API key list must not be empty")
	}

	entries := strings.Split(value, ",")
	keys := make([]string, len(entries))
	for i, entry := range entries {
		key := strings.TrimSpace(entry)
		if !validKey(key) {
			return nil, errors.New("API key list contains an empty or malformed key")
		}
		keys[i] = key
	}
	return keys, nil
}

func New(keys []string) (*Authenticator, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one API key is required")
	}

	digests := make([][sha256.Size]byte, len(keys))
	for i, key := range keys {
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, errors.New("API key list contains an empty or malformed key")
		}
		digests[i] = sha256.Sum256([]byte(key))
	}
	return &Authenticator{keyDigests: digests}, nil
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e {
			return false
		}
	}
	return true
}

func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !requiresAPIKey(req.URL.Path) {
			next.ServeHTTP(w, req)
			return
		}
		if !a.validRequestKey(req) {
			writeUnauthorized(w)
			return
		}

		authenticatedRequest := req.Clone(req.Context())
		authenticatedRequest.Header = req.Header.Clone()
		authenticatedRequest.Header.Del(APIKeyHeader)
		next.ServeHTTP(w, authenticatedRequest)
	})
}

func requiresAPIKey(path string) bool {
	return path == "/api/v1" || strings.HasPrefix(path, "/api/v1/")
}

func (a *Authenticator) validRequestKey(req *http.Request) bool {
	values := req.Header.Values(APIKeyHeader)
	if len(values) != 1 || values[0] == "" {
		return false
	}

	candidate := sha256.Sum256([]byte(values[0]))
	match := 0
	for _, digest := range a.keyDigests {
		match |= subtle.ConstantTimeCompare(candidate[:], digest[:])
	}
	return match == 1
}

func writeUnauthorized(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusUnauthorized)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: "unauthorized"})
}
