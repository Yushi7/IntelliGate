package auth

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
)

const APIKeyHeader = "X-API-Key"

type Role string

const (
	RoleAdmin Role = "admin"
	RoleRead  Role = "read"
	RoleWrite Role = "write"
)

type credential struct {
	digest   [sha256.Size]byte
	roleCode int
}

type Authenticator struct {
	credentials []credential
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

func ParseRoleMapping(value string) (map[string]Role, error) {
	if strings.TrimSpace(value) == "" {
		return nil, errors.New("API key role mapping must be a non-empty JSON object")
	}

	decoder := json.NewDecoder(strings.NewReader(value))
	token, err := decoder.Token()
	if err != nil {
		return nil, errors.New("API key role mapping must be a valid JSON object")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '{' {
		return nil, errors.New("API key role mapping must be a JSON object")
	}

	roles := make(map[string]Role)
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, errors.New("API key role mapping must be a valid JSON object")
		}
		key, ok := token.(string)
		if !ok {
			return nil, errors.New("API key role mapping contains an invalid key")
		}
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, errors.New("API key role mapping contains an empty or malformed key")
		}
		if _, exists := roles[key]; exists {
			return nil, errors.New("API key role mapping contains duplicate keys")
		}

		var roleName string
		if err := decoder.Decode(&roleName); err != nil {
			return nil, errors.New("API key role mapping contains an invalid role")
		}
		role := Role(roleName)
		if !validRole(role) {
			return nil, errors.New("API key role mapping contains an unknown role")
		}
		roles[key] = role
	}

	token, err = decoder.Token()
	if err != nil {
		return nil, errors.New("API key role mapping must be a valid JSON object")
	}
	if delimiter, ok := token.(json.Delim); !ok || delimiter != '}' {
		return nil, errors.New("API key role mapping must be a valid JSON object")
	}
	if len(roles) == 0 {
		return nil, errors.New("API key role mapping must contain at least one key")
	}

	var trailing json.RawMessage
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("API key role mapping contains trailing data")
	}
	return roles, nil
}

func New(keys []string) (*Authenticator, error) {
	if len(keys) == 0 {
		return nil, errors.New("at least one API key is required")
	}

	credentials := make([]credential, len(keys))
	for i, key := range keys {
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, errors.New("API key list contains an empty or malformed key")
		}
		credentials[i] = credential{digest: sha256.Sum256([]byte(key)), roleCode: roleCode(RoleAdmin)}
	}
	return &Authenticator{credentials: credentials}, nil
}

func NewWithRoles(roles map[string]Role) (*Authenticator, error) {
	if len(roles) == 0 {
		return nil, errors.New("at least one API key role mapping is required")
	}

	credentials := make([]credential, 0, len(roles))
	seen := make(map[string]struct{}, len(roles))
	for key, role := range roles {
		key = strings.TrimSpace(key)
		if !validKey(key) {
			return nil, errors.New("API key role mapping contains an empty or malformed key")
		}
		if _, exists := seen[key]; exists {
			return nil, errors.New("API key role mapping contains duplicate keys")
		}
		if !validRole(role) {
			return nil, errors.New("API key role mapping contains an unknown role")
		}
		seen[key] = struct{}{}
		credentials = append(credentials, credential{
			digest:   sha256.Sum256([]byte(key)),
			roleCode: roleCode(role),
		})
	}
	return &Authenticator{credentials: credentials}, nil
}

func validKey(key string) bool {
	if key == "" {
		return false
	}
	for i := 0; i < len(key); i++ {
		if key[i] < 0x21 || key[i] > 0x7e || key[i] == ',' {
			return false
		}
	}
	return true
}

func validRole(role Role) bool {
	switch role {
	case RoleAdmin, RoleRead, RoleWrite:
		return true
	default:
		return false
	}
}

func roleCode(role Role) int {
	switch role {
	case RoleAdmin:
		return 1
	case RoleRead:
		return 2
	case RoleWrite:
		return 3
	default:
		return 0
	}
}

func roleFromCode(code int) Role {
	switch code {
	case 1:
		return RoleAdmin
	case 2:
		return RoleRead
	case 3:
		return RoleWrite
	default:
		return ""
	}
}

func (a *Authenticator) Middleware(next http.Handler) http.Handler {
	return a.AuthenticationMiddleware(a.AuthorizationMiddleware(next))
}

func (a *Authenticator) AuthenticationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !requiresAPIKey(req.URL.Path) {
			next.ServeHTTP(w, req)
			return
		}
		role, ok := a.roleForRequest(req)
		if !ok {
			writeUnauthorized(w)
			return
		}

		ctx := context.WithValue(req.Context(), roleContextKey{}, role)
		authenticatedRequest := req.Clone(ctx)
		authenticatedRequest.Header = req.Header.Clone()
		authenticatedRequest.Header.Del(APIKeyHeader)
		authenticatedRequest.Header.Del("X-Role")
		authenticatedRequest.Header.Del("X-User-Role")
		authenticatedRequest.Header.Del("X-Intelligate-Role")
		next.ServeHTTP(w, authenticatedRequest)
	})
}

func (a *Authenticator) AuthorizationMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		if !requiresAPIKey(req.URL.Path) {
			next.ServeHTTP(w, req)
			return
		}
		role, ok := roleFromContext(req.Context())
		if !ok {
			writeUnauthorized(w)
			return
		}
		if !roleAllowsMethod(role, req.Method) {
			writeForbidden(w)
			return
		}
		next.ServeHTTP(w, req)
	})
}

func requiresAPIKey(path string) bool {
	return path == "/api/v1" || strings.HasPrefix(path, "/api/v1/")
}

type roleContextKey struct{}

func roleFromContext(ctx context.Context) (Role, bool) {
	role, ok := ctx.Value(roleContextKey{}).(Role)
	return role, ok && validRole(role)
}

func roleAllowsMethod(role Role, method string) bool {
	switch method {
	case http.MethodGet, http.MethodHead:
		return true
	case http.MethodPost, http.MethodPut, http.MethodPatch, http.MethodDelete:
		return role == RoleAdmin || role == RoleWrite
	default:
		return false
	}
}

func (a *Authenticator) roleForRequest(req *http.Request) (Role, bool) {
	values := req.Header.Values(APIKeyHeader)
	if len(values) != 1 || values[0] == "" {
		return "", false
	}

	candidate := sha256.Sum256([]byte(values[0]))
	match := 0
	selectedRoleCode := 0
	for _, credential := range a.credentials {
		equal := subtle.ConstantTimeCompare(candidate[:], credential.digest[:])
		match |= equal
		selectedRoleCode = subtle.ConstantTimeSelect(equal, credential.roleCode, selectedRoleCode)
	}
	return roleFromCode(selectedRoleCode), match == 1
}

func writeUnauthorized(w http.ResponseWriter) {
	writeJSONError(w, http.StatusUnauthorized, "unauthorized")
}

func writeForbidden(w http.ResponseWriter) {
	writeJSONError(w, http.StatusForbidden, "forbidden")
}

func writeJSONError(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(struct {
		Error string `json:"error"`
	}{Error: message})
}
