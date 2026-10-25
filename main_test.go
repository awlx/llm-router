package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func modelServer(t *testing.T, id string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/v1/models":
			_ = json.NewEncoder(writer).Encode(map[string]any{"object": "list", "data": []map[string]any{{"id": id, "object": "model"}}})
		case "/v1/chat/completions":
			body, _ := io.ReadAll(request.Body)
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write(body)
		default:
			http.NotFound(writer, request)
		}
	}))
}

func testRouter(t *testing.T, endpoints ...EndpointConfig) *router {
	t.Helper()
	config := defaultConfig()
	config.Endpoints = endpoints
	result, err := newRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	result.discover()
	return result
}

func TestDiscoveryRoutesExactModelWithoutRewrite(t *testing.T) {
	server := modelServer(t, "org/model-a")
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	body := `{"model":"org/model-a","messages":[{"role":"user","content":"hello"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || response.Body.String() != body {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestProxyLogsForwardedClientAndModel(t *testing.T) {
	server := modelServer(t, "org/model-a")
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"org/model-a"}`))
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("X-Forwarded-For", "192.0.2.44")

	var output bytes.Buffer
	previousWriter := log.Writer()
	log.SetOutput(&output)
	t.Cleanup(func() { log.SetOutput(previousWriter) })

	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d", response.Code)
	}
	for _, expected := range []string{
		`client_ip="192.0.2.44"`,
		`x_forwarded_for="192.0.2.44"`,
		`direct_ip="127.0.0.1"`,
		`model="org/model-a"`,
		`result="routed"`,
		`upstream="node-a"`,
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("log %q does not contain %q", output.String(), expected)
		}
	}
}

func TestRequestAddressesRejectsInvalidForwardedClient(t *testing.T) {
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	request.RemoteAddr = "127.0.0.1:43210"
	request.Header.Set("X-Forwarded-For", "not-an-ip")
	clientIP, directIP, forwardedFor := requestAddresses(request)
	if clientIP != "127.0.0.1" || directIP != "127.0.0.1" || forwardedFor != "not-an-ip" {
		t.Fatalf("addresses=%q %q %q", clientIP, directIP, forwardedFor)
	}
}

func TestModelsAggregatesEndpoints(t *testing.T) {
	first := modelServer(t, "org/model-a")
	defer first.Close()
	second := modelServer(t, "model-b")
	defer second.Close()
	router := testRouter(t,
		EndpointConfig{Name: "node-a", URL: first.URL},
		EndpointConfig{Name: "node-b", URL: second.URL},
	)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var document modelDocument
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Data) != 2 {
		t.Fatalf("models=%s", response.Body.String())
	}
}

func TestDuplicateModelIsConflict(t *testing.T) {
	first := modelServer(t, "same-model")
	defer first.Close()
	second := modelServer(t, "same-model")
	defer second.Close()
	router := testRouter(t,
		EndpointConfig{Name: "node-a", URL: first.URL},
		EndpointConfig{Name: "node-b", URL: second.URL},
	)
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"same-model"}`))
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusConflict {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestModelMetadataMergedIntoModels(t *testing.T) {
	server := modelServer(t, "vision-model")
	defer server.Close()
	config := defaultConfig()
	config.Endpoints = []EndpointConfig{{Name: "node-a", URL: server.URL}}
	config.ModelMetadata = map[string]map[string]json.RawMessage{
		"vision-model":  {"architecture": json.RawMessage(`{"input_modalities":["text","image"]}`)},
		"offline-model": {"architecture": json.RawMessage(`{"input_modalities":["text"]}`)},
	}
	router, err := newRouter(config)
	if err != nil {
		t.Fatal(err)
	}
	router.discover()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	var document struct {
		Data []struct {
			ID           string `json:"id"`
			Object       string `json:"object"`
			Architecture struct {
				InputModalities []string `json:"input_modalities"`
			} `json:"architecture"`
		} `json:"data"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil {
		t.Fatal(err)
	}
	if len(document.Data) != 1 || document.Data[0].ID != "vision-model" || document.Data[0].Object != "model" ||
		strings.Join(document.Data[0].Architecture.InputModalities, ",") != "text,image" {
		t.Fatalf("models=%s", response.Body.String())
	}
}

func TestLoadConfigRejectsMetadataIDOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	config := `{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"model_metadata":{"m":{"id":"other"}}}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected id override error")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:1","endpoints":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func scrapeRouterMetrics(t *testing.T, router *router) string {
	t.Helper()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Header().Get("Content-Type"), "text/plain") {
		t.Fatalf("invalid exposition: %d %s", response.Code, response.Body)
	}
	return response.Body.String()
}

func TestMetricsRequestSizesAndBoundedLabels(t *testing.T) {
	upstream := modelServer(t, "actual-model")
	defer upstream.Close()
	router := testRouter(t, EndpointConfig{Name: "node-one", URL: upstream.URL})
	body := `{"model":"actual-model","messages":[{"role":"user","content":"private-content-sentinel"}]}`
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body)))
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"arbitrary-user-model"}`)))
	metrics := scrapeRouterMetrics(t, router)
	labels := `{endpoint="node-one",model="actual-model",route="/v1/chat/completions"}`
	for _, expected := range []string{
		`llm_router_requests_total{code="200",endpoint="node-one",model="actual-model",outcome="completed",route="/v1/chat/completions"} 1`,
		`llm_router_requests_total{code="404",endpoint="",model="",outcome="unknown_model",route="/v1/chat/completions"} 1`,
		fmt.Sprintf("llm_router_request_size_bytes_sum%s %d", labels, len(body)),
		fmt.Sprintf("llm_router_response_size_bytes_sum%s %d", labels, len(body)),
		"llm_router_response_headers_duration_seconds_count" + labels + " 1",
		"llm_router_request_duration_seconds_count" + labels + " 1",
	} {
		if !strings.Contains(metrics, expected) {
			t.Errorf("missing metric %s", expected)
		}
	}
	if strings.Contains(metrics, "private-content-sentinel") || strings.Contains(metrics, "arbitrary-user-model") {
		t.Fatal("request content or unknown model leaked into telemetry")
	}
	for _, method := range []string{http.MethodHead, http.MethodPost} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(method, "/metrics", nil))
		if method == http.MethodPost && response.Code != http.StatusMethodNotAllowed {
			t.Fatal("metrics accepted POST")
		}
		if method == http.MethodHead && response.Code != http.StatusOK {
			t.Fatal("metrics rejected HEAD")
		}
	}
}

func TestMetricsPreserveStreamingAndTrailers(t *testing.T) {
	release := make(chan struct{})
	defer func() {
		select {
		case <-release:
		default:
			close(release)
		}
	}()
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_, _ = io.WriteString(writer, `{"data":[{"id":"stream-model"}]}`)
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.Header().Set("Trailer", "X-Stream-End")
		_, _ = io.WriteString(writer, "data: one\n\n")
		writer.(http.Flusher).Flush()
		select {
		case <-release:
		case <-request.Context().Done():
			return
		}
		_, _ = io.WriteString(writer, "data: two\n\n")
		writer.Header().Set("X-Stream-End", "complete")
	}))
	defer upstream.Close()
	router := testRouter(t, EndpointConfig{Name: "node-stream", URL: upstream.URL})
	done := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer close(done)
		router.ServeHTTP(writer, request)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 5 * time.Second
	response, err := client.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"stream-model"}`))
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	first, err := reader.ReadString('\n')
	if err != nil || first != "data: one\n" {
		t.Fatalf("stream was buffered: %q %v", first, err)
	}
	if !strings.Contains(scrapeRouterMetrics(t, router), `llm_router_requests_in_flight{endpoint="node-stream",model="stream-model"} 1`) {
		t.Fatal("open stream not counted")
	}
	close(release)
	rest, err := io.ReadAll(reader)
	if err != nil || first+string(rest) != "data: one\n\ndata: two\n\n" {
		t.Fatalf("stream changed: %q %v", rest, err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("router did not finish")
	}
	if response.Trailer.Get("X-Stream-End") != "complete" {
		t.Fatal("response trailer lost")
	}
	metrics := scrapeRouterMetrics(t, router)
	if !strings.Contains(metrics, `llm_router_requests_in_flight{endpoint="node-stream",model="stream-model"} 0`) {
		t.Fatal("inflight leaked")
	}
	if !strings.Contains(metrics, `llm_router_response_bytes_total{endpoint="node-stream",model="stream-model",route="/v1/chat/completions"} 22`) {
		t.Fatal("stream bytes counted incorrectly")
	}
}

func TestMetricsDiscoveryFailureRemovesRoutes(t *testing.T) {
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if unavailable.Load() {
			http.Error(writer, "unavailable", 503)
			return
		}
		_, _ = io.WriteString(writer, `{"data":[{"id":"served-model"}]}`)
	}))
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-one", URL: server.URL})
	unavailable.Store(true)
	router.discover()
	metrics := scrapeRouterMetrics(t, router)
	if !strings.Contains(metrics, `llm_router_endpoint_up{endpoint="node-one"} 0`) || strings.Contains(metrics, `llm_router_model_info{`) {
		t.Fatal("failed discovery retained a live route")
	}
}

func TestMetricsTrackCanceledRequests(t *testing.T) {
	server := modelServer(t, "served-model")
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-one", URL: server.URL})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"served-model"}`)).WithContext(ctx)
	router.ServeHTTP(httptest.NewRecorder(), request)
	if !strings.Contains(scrapeRouterMetrics(t, router), `outcome="canceled"`) {
		t.Fatal("cancellation not recorded")
	}
}

func decodeAPIError(t *testing.T, response *httptest.ResponseRecorder) apiError {
	t.Helper()
	if response.Header().Get("Content-Type") != "application/json" {
		t.Fatalf("content type=%q", response.Header().Get("Content-Type"))
	}
	var document struct {
		Error apiError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil || document.Error.Message == "" {
		t.Fatalf("body=%s err=%v", response.Body.String(), err)
	}
	return document.Error
}

func TestErrorsUseOpenAIFormat(t *testing.T) {
	first := modelServer(t, "same-model")
	defer first.Close()
	second := modelServer(t, "same-model")
	defer second.Close()
	router := testRouter(t,
		EndpointConfig{Name: "node-a", URL: first.URL},
		EndpointConfig{Name: "node-b", URL: second.URL},
	)
	router.config.MaxRequestBytes = 1024
	cases := []struct {
		name, method, path, body string
		status                   int
		code                     string
	}{
		{"invalid json", http.MethodPost, "/v1/chat/completions", `{`, http.StatusBadRequest, "invalid_request"},
		{"missing model", http.MethodPost, "/v1/chat/completions", `{}`, http.StatusBadRequest, "invalid_request"},
		{"too large", http.MethodPost, "/v1/chat/completions", `{"model":"` + strings.Repeat("x", 2048) + `"}`, http.StatusRequestEntityTooLarge, "request_too_large"},
		{"unknown model", http.MethodPost, "/v1/chat/completions", `{"model":"missing"}`, http.StatusNotFound, "model_not_found"},
		{"conflict", http.MethodPost, "/v1/chat/completions", `{"model":"same-model"}`, http.StatusConflict, "model_conflict"},
		{"unknown route", http.MethodGet, "/v1/unknown", ``, http.StatusNotFound, "not_found"},
		{"metrics method", http.MethodPost, "/metrics", ``, http.StatusMethodNotAllowed, "method_not_allowed"},
	}
	for _, item := range cases {
		t.Run(item.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			router.ServeHTTP(response, httptest.NewRequest(item.method, item.path, strings.NewReader(item.body)))
			if response.Code != item.status {
				t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
			}
			if apiErr := decodeAPIError(t, response); apiErr.Code != item.code || apiErr.Type == "" {
				t.Fatalf("error=%+v", apiErr)
			}
		})
	}
}

func TestUpstreamUnavailableUsesOpenAIFormat(t *testing.T) {
	server := modelServer(t, "gone-model")
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	server.Close()
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"gone-model"}`)))
	if response.Code != http.StatusBadGateway {
		t.Fatalf("status=%d", response.Code)
	}
	if apiErr := decodeAPIError(t, response); apiErr.Type != "upstream_error" {
		t.Fatalf("error=%+v", apiErr)
	}
}

func TestChunkedRequestForwardedWithContentLength(t *testing.T) {
	type seen struct {
		length   int64
		encoding []string
	}
	received := make(chan seen, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_, _ = io.WriteString(writer, `{"data":[{"id":"chunk-model"}]}`)
			return
		}
		_, _ = io.Copy(io.Discard, request.Body)
		received <- seen{request.ContentLength, request.TransferEncoding}
	}))
	defer upstream.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: upstream.URL})
	server := httptest.NewServer(router)
	defer server.Close()
	body := `{"model":"chunk-model"}`
	request, err := http.NewRequest(http.MethodPost, server.URL+"/v1/chat/completions", io.NopCloser(strings.NewReader(body)))
	if err != nil {
		t.Fatal(err)
	}
	request.ContentLength = -1
	response, err := server.Client().Do(request)
	if err != nil {
		t.Fatal(err)
	}
	response.Body.Close()
	got := <-received
	if got.length != int64(len(body)) || len(got.encoding) != 0 {
		t.Fatalf("content length=%d transfer encoding=%v", got.length, got.encoding)
	}
}

func TestEmbeddingsRouteByModel(t *testing.T) {
	var path atomic.Value
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_, _ = io.WriteString(writer, `{"data":[{"id":"embed-model"}]}`)
			return
		}
		path.Store(request.URL.Path)
		_, _ = io.Copy(writer, request.Body)
	}))
	defer upstream.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: upstream.URL})
	body := `{"model":"embed-model","input":"hello"}`
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(body)))
	if response.Code != http.StatusOK || response.Body.String() != body || path.Load() != "/v1/embeddings" {
		t.Fatalf("status=%d body=%s path=%v", response.Code, response.Body.String(), path.Load())
	}
	response = httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/embeddings", strings.NewReader(`{"model":"other"}`)))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status=%d", response.Code)
	}
}

func TestGetSingleModel(t *testing.T) {
	server := modelServer(t, "org/model-a")
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: server.URL})
	for _, path := range []string{"/v1/models/org/model-a", "/v1/models/org%2Fmodel-a"} {
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		var document map[string]any
		if err := json.Unmarshal(response.Body.Bytes(), &document); err != nil || response.Code != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
		if document["id"] != "org/model-a" || document["object"] != "model" {
			t.Fatalf("document=%v", document)
		}
	}
	response := httptest.NewRecorder()
	router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/v1/models/missing", nil))
	if response.Code != http.StatusNotFound || decodeAPIError(t, response).Code != "model_not_found" {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestReadyRequiresRoutableModel(t *testing.T) {
	var unavailable atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if unavailable.Load() {
			http.Error(writer, "unavailable", http.StatusServiceUnavailable)
			return
		}
		_, _ = io.WriteString(writer, `{"data":[{"id":"served-model"}]}`)
	}))
	defer server.Close()
	router := testRouter(t, EndpointConfig{Name: "node-one", URL: server.URL})
	check := func(path string, status int, body string) {
		t.Helper()
		response := httptest.NewRecorder()
		router.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != status || !strings.Contains(response.Body.String(), body) {
			t.Fatalf("%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	check("/ready", http.StatusOK, `"status":"ready"`)
	unavailable.Store(true)
	router.discover()
	check("/ready", http.StatusServiceUnavailable, `"status":"not_ready"`)
	check("/health", http.StatusOK, `"status":"ok"`)
}
