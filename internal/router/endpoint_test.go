package router

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/awlx/llm-router/internal/config"
)

func protectedModelServer(t *testing.T, id, key string) *httptest.Server {
	t.Helper()
	inner := modelServer(t, id)
	t.Cleanup(inner.Close)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("Authorization") != "Bearer "+key {
			http.Error(writer, "unauthorized", http.StatusUnauthorized)
			return
		}
		inner.Config.Handler.ServeHTTP(writer, request)
	}))
	t.Cleanup(server.Close)
	return server
}

func TestEndpointAPIKeyUsedForDiscoveryAndProxy(t *testing.T) {
	server := protectedModelServer(t, "org/protected", "backend-secret")
	router := testRouter(t, config.EndpointConfig{Name: "keyed", URL: server.URL, APIKey: "backend-secret"})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/protected"}`))
	request.Header.Set("Authorization", "Bearer client-token")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestEndpointWithoutAPIKeyKeepsClientAuthorization(t *testing.T) {
	server := protectedModelServer(t, "org/protected", "client-token")
	router := testRouter(t, config.EndpointConfig{Name: "open", URL: server.URL})
	if len(router.snapshot.models) != 0 {
		t.Fatal("discovery without a key must fail")
	}
	server2 := modelServer(t, "org/open")
	defer server2.Close()
	var seen string
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		seen = request.Header.Get("Authorization")
		server2.Config.Handler.ServeHTTP(writer, request)
	}))
	defer backend.Close()
	router = testRouter(t, config.EndpointConfig{Name: "open", URL: backend.URL})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/open"}`))
	request.Header.Set("Authorization", "Bearer client-token")
	router.ServeHTTP(httptest.NewRecorder(), request)
	if seen != "Bearer client-token" {
		t.Fatalf("authorization=%q", seen)
	}
}

func TestResponseHeaderTimeoutReturnsBadGateway(t *testing.T) {
	release := make(chan struct{})
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"id": "slow"}}})
			return
		}
		<-release
	}))
	defer backend.Close()
	defer close(release)
	cfg := config.Default()
	cfg.Endpoints = []config.EndpointConfig{{Name: "slow", URL: backend.URL}}
	cfg.ResponseHeaderTimeoutSeconds = 1
	router, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	router.Discover()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"slow"}`)))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", response.Code)
	}
}
