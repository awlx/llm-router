package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func flakyModelServer(t *testing.T, ids ...string) (*httptest.Server, *atomic.Bool) {
	t.Helper()
	down := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if down.Load() {
			http.Error(writer, "down", http.StatusBadGateway)
			return
		}
		if request.URL.Path == "/v1/models" {
			data := []map[string]any{}
			for _, id := range ids {
				data = append(data, map[string]any{"id": id, "object": "model"})
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": data})
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"ok":true}`))
	}))
	t.Cleanup(server.Close)
	return server, down
}

func chat(router *router, model string) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"`+model+`"}`)))
	return response
}

func TestDiscoveryGraceReturnsUnavailableThenRecovers(t *testing.T) {
	server, down := flakyModelServer(t, "flaky")
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	if response := chat(router, "flaky"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	down.Store(true)
	router.discover()
	response := chat(router, "flaky")
	if response.Code != http.StatusServiceUnavailable || decodeAPIError(t, response).Code != "model_unavailable" || response.Header().Get("Retry-After") == "" {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	models := httptest.NewRecorder()
	router.ServeHTTP(models, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if !strings.Contains(models.Body.String(), `"flaky"`) {
		t.Fatalf("model must stay listed during the grace period: %s", models.Body.String())
	}
	down.Store(false)
	router.discover()
	if response := chat(router, "flaky"); response.Code != http.StatusOK {
		t.Fatalf("status after recovery=%d", response.Code)
	}
}

func TestDiscoveryGraceExpires(t *testing.T) {
	server, down := flakyModelServer(t, "flaky")
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	down.Store(true)
	router.lastSeen["node-a"] = seenModels{documents: router.lastSeen["node-a"].documents, at: time.Now().Add(-time.Hour)}
	router.discover()
	if response := chat(router, "flaky"); response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestDiscoveryGraceDisabled(t *testing.T) {
	server, down := flakyModelServer(t, "flaky")
	config := defaultConfig()
	config.DiscoveryGraceSeconds = 0
	config.Endpoints = []EndpointConfig{{Name: "node-a", URL: server.URL}}
	router, err := newRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	router.discover()
	down.Store(true)
	router.discover()
	if response := chat(router, "flaky"); response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestDiscoveryGraceKeepsDuplicateConflict(t *testing.T) {
	serverA, _ := flakyModelServer(t, "dup", "only-a")
	serverB, downB := flakyModelServer(t, "dup")
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: serverA.URL}, EndpointConfig{Name: "node-b", URL: serverB.URL})
	downB.Store(true)
	router.discover()
	if response := chat(router, "dup"); response.Code != http.StatusConflict {
		t.Fatalf("duplicate must stay a conflict during the grace period: status=%d", response.Code)
	}
	if response := chat(router, "only-a"); response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
}
