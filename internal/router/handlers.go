package router

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
)

func (r *Router) models(writer http.ResponseWriter) {
	r.mu.RLock()
	ids := make([]string, 0, len(r.snapshot.documents))
	for id := range r.snapshot.documents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	documents := make([]json.RawMessage, 0, len(ids))
	for _, id := range ids {
		documents = append(documents, r.snapshot.documents[id])
	}
	r.mu.RUnlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(modelDocument{Object: "list", Data: documents})
}

func (r *Router) model(writer http.ResponseWriter, id string) {
	r.mu.RLock()
	document, ok := r.snapshot.documents[id]
	r.mu.RUnlock()
	if !ok {
		writeError(writer, http.StatusNotFound, fmt.Sprintf("model %q does not exist", id), "invalid_request_error", "model_not_found")
		return
	}
	writer.Header().Set("Content-Type", "application/json")
	_, _ = writer.Write(append(append(json.RawMessage(nil), document...), '\n'))
}

// ready reports 503 until discovery finds at least one routable model.
func (r *Router) ready(writer http.ResponseWriter) {
	r.mu.RLock()
	models := len(r.snapshot.models)
	r.mu.RUnlock()
	status, code := "ready", http.StatusOK
	if models == 0 {
		status, code = "not_ready", http.StatusServiceUnavailable
	}
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.WriteHeader(code)
	_ = json.NewEncoder(writer).Encode(map[string]any{"status": status, "models": models})
}

func (r *Router) health(writer http.ResponseWriter) {
	r.mu.RLock()
	payload := map[string]any{
		"status": "ok", "models": len(r.snapshot.models), "conflicts": r.snapshot.conflicts,
		"endpoint_errors": r.snapshot.errors, "unavailable_models": r.snapshot.unavailable, "updated_at": r.snapshot.updatedAt,
	}
	r.mu.RUnlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

var proxyRoutes = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
	"/v1/embeddings":       true,
}

// ServeHTTP routes a request to a local handler or to the endpoint that serves the model.
func (r *Router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r.cors(writer, request) {
		return
	}
	if !r.authorized(request) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="llm-router"`)
		writeError(writer, http.StatusUnauthorized, "missing or invalid API key", "invalid_request_error", "invalid_api_key")
		return
	}
	switch {
	case request.URL.Path == "/metrics":
		if request.Method != http.MethodGet && request.Method != http.MethodHead {
			writer.Header().Set("Allow", "GET, HEAD")
			writeError(writer, http.StatusMethodNotAllowed, "method not allowed", "invalid_request_error", "method_not_allowed")
			return
		}
		r.metrics.handler.ServeHTTP(writer, request)
	case request.Method == http.MethodGet && request.URL.Path == "/health":
		r.health(writer)
	case request.Method == http.MethodGet && request.URL.Path == "/ready":
		r.ready(writer)
	case request.Method == http.MethodGet && request.URL.Path == "/v1/models":
		r.models(writer)
	case request.Method == http.MethodGet && strings.HasPrefix(request.URL.Path, "/v1/models/") && len(request.URL.Path) > len("/v1/models/"):
		r.model(writer, strings.TrimPrefix(request.URL.Path, "/v1/models/"))
	case request.Method == http.MethodPost && proxyRoutes[request.URL.Path]:
		r.proxy(writer, request)
	default:
		writeError(writer, http.StatusNotFound, "route not found", "invalid_request_error", "not_found")
	}
}
