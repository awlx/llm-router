package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestTimeToFirstByteMetric(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path == "/v1/models" {
			_ = json.NewEncoder(writer).Encode(map[string]any{"data": []map[string]any{{"id": "stream-model"}}})
			return
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		writer.WriteHeader(http.StatusOK)
		writer.(http.Flusher).Flush()
		time.Sleep(200 * time.Millisecond)
		_, _ = writer.Write([]byte("data: first\n\n"))
		writer.(http.Flusher).Flush()
		time.Sleep(400 * time.Millisecond)
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer backend.Close()
	router := testRouter(t, EndpointConfig{Name: "node-a", URL: backend.URL})
	server := httptest.NewServer(router)
	defer server.Close()
	response, err := http.Post(server.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"stream-model","stream":true}`))
	if err != nil {
		t.Fatal(err)
	}
	_, _ = response.Body.Read(make([]byte, 64))
	response.Body.Close()
	unknown := httptest.NewRecorder()
	router.ServeHTTP(unknown, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"missing"}`)))

	metrics := scrapeRouterMetrics(t, router)
	if !strings.Contains(metrics, `llm_router_time_to_first_byte_seconds_count{endpoint="node-a",model="stream-model"} 1`) {
		t.Fatalf("missing time to first byte count:\n%s", metrics)
	}
	if strings.Contains(metrics, `llm_router_time_to_first_byte_seconds_count{endpoint="",`) {
		t.Fatal("rejected requests must not record time to first byte")
	}
	match := regexp.MustCompile(`llm_router_time_to_first_byte_seconds_sum\{endpoint="node-a",model="stream-model"\} (\S+)`).FindStringSubmatch(metrics)
	if match == nil {
		t.Fatal("missing time to first byte sum")
	}
	seconds, _ := strconv.ParseFloat(match[1], 64)
	if seconds < 0.2 || seconds >= 0.6 {
		t.Fatalf("time to first byte=%.3fs, want first chunk time and not stream end", seconds)
	}
}
