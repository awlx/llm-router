package router

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awlx/llm-router/internal/config"
)

func authRouter(t *testing.T, exempt ...string) (*Router, *string) {
	t.Helper()
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_, _ = writer.Write([]byte(`{"data":[{"id":"m"}]}`))
			return
		}
		seen = request.Header.Get("Authorization")
		_, _ = writer.Write([]byte(`{}`))
	}))
	t.Cleanup(backend.Close)
	cfg := config.Default()
	cfg.ClientAPIKey = "router-secret"
	cfg.AuthExemptPaths = exempt
	cfg.Endpoints = []config.EndpointConfig{{Name: "node-a", URL: backend.URL}}
	result, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	result.Discover()
	return result, &seen
}

func serveWithKey(router *Router, method, path, key string) *httptest.ResponseRecorder {
	var body *strings.Reader
	if method == http.MethodPost {
		body = strings.NewReader(`{"model":"m"}`)
	} else {
		body = strings.NewReader("")
	}
	request := httptest.NewRequest(method, path, body)
	if key != "" {
		request.Header.Set("Authorization", "Bearer "+key)
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	return response
}

func TestClientAuthProtectsAllRoutes(t *testing.T) {
	router, _ := authRouter(t)
	for _, route := range [][2]string{{"GET", "/v1/models"}, {"GET", "/health"}, {"GET", "/ready"}, {"GET", "/metrics"}, {"POST", "/v1/chat/completions"}} {
		for _, key := range []string{"", "wrong"} {
			response := serveWithKey(router, route[0], route[1], key)
			if response.Code != http.StatusUnauthorized || decodeAPIError(t, response).Code != "invalid_api_key" || response.Header().Get("WWW-Authenticate") == "" {
				t.Fatalf("%s %s key=%q status=%d", route[0], route[1], key, response.Code)
			}
		}
		if response := serveWithKey(router, route[0], route[1], "router-secret"); response.Code != http.StatusOK {
			t.Fatalf("%s %s with key: status=%d", route[0], route[1], response.Code)
		}
	}
}

func TestClientAuthExemptPaths(t *testing.T) {
	router, _ := authRouter(t, "/metrics", "/health")
	for _, path := range []string{"/metrics", "/health"} {
		if response := serveWithKey(router, http.MethodGet, path, ""); response.Code != http.StatusOK {
			t.Fatalf("%s status=%d", path, response.Code)
		}
	}
	if response := serveWithKey(router, http.MethodGet, "/v1/models", ""); response.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestClientAuthKeyNotForwarded(t *testing.T) {
	router, seen := authRouter(t)
	if response := serveWithKey(router, http.MethodPost, "/v1/chat/completions", "router-secret"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	if *seen != "" {
		t.Fatalf("router key forwarded: %q", *seen)
	}
}

func TestClientAuthWithCORS(t *testing.T) {
	cfg := config.Default()
	cfg.ClientAPIKey = "router-secret"
	cfg.CORSAllowedOrigins = []string{"https://app.example.com"}
	cfg.Endpoints = []config.EndpointConfig{{Name: "node-a", URL: "http://192.0.2.1:8000"}}
	router, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	preflight := httptest.NewRequest(http.MethodOptions, "/v1/chat/completions", nil)
	preflight.Header.Set("Origin", "https://app.example.com")
	preflight.Header.Set("Access-Control-Request-Method", "POST")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, preflight)
	if response.Code != http.StatusNoContent {
		t.Fatalf("preflight status=%d", response.Code)
	}
	request := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	request.Header.Set("Origin", "https://app.example.com")
	response = httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusUnauthorized || response.Header().Get("Access-Control-Allow-Origin") != "https://app.example.com" {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
}
