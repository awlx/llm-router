package main

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type EndpointConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// APIKey is sent as a bearer token for discovery and proxied requests to this endpoint.
	APIKey string `json:"api_key,omitempty"`
	// APIKeyEnv names an environment variable that contains the API key.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

type Config struct {
	Listen                   string `json:"listen"`
	DiscoveryIntervalSeconds int    `json:"discovery_interval_seconds"`
	DiscoveryTimeoutSeconds  int    `json:"discovery_timeout_seconds"`
	MaxRequestBytes          int64  `json:"max_request_bytes"`
	// ResponseHeaderTimeoutSeconds limits the wait for upstream response headers; streams are not limited.
	ResponseHeaderTimeoutSeconds int              `json:"response_header_timeout_seconds"`
	Endpoints                    []EndpointConfig `json:"endpoints"`
	// Top-level fields merged into a model's /v1/models entry, e.g. architecture.input_modalities.
	ModelMetadata map[string]map[string]json.RawMessage `json:"model_metadata"`
}

type modelDocument struct {
	Object string            `json:"object"`
	Data   []json.RawMessage `json:"data"`
}

type modelIdentity struct {
	ID string `json:"id"`
}

type requestIdentity struct {
	Model string `json:"model"`
}

type endpoint struct {
	config EndpointConfig
	proxy  *httputil.ReverseProxy
}

type registrySnapshot struct {
	models    map[string]*endpoint
	conflicts map[string][]string
	documents map[string]json.RawMessage
	errors    map[string]string
	updatedAt time.Time
}

type router struct {
	metrics   *routerMetrics
	config    Config
	client    *http.Client
	endpoints []*endpoint
	mu        sync.RWMutex
	snapshot  registrySnapshot
}

func defaultConfig() Config {
	return Config{
		Listen:                       "127.0.0.1:1234",
		DiscoveryIntervalSeconds:     15,
		DiscoveryTimeoutSeconds:      4,
		MaxRequestBytes:              32 << 20,
		ResponseHeaderTimeoutSeconds: 600,
	}
}

func loadConfig(path string) (Config, error) {
	config := defaultConfig()
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode config: %w", err)
	}
	if config.Listen == "" || len(config.Endpoints) == 0 {
		return Config{}, errors.New("listen and at least one endpoint are required")
	}
	if config.DiscoveryIntervalSeconds < 1 || config.DiscoveryTimeoutSeconds < 1 || config.MaxRequestBytes < 1024 || config.ResponseHeaderTimeoutSeconds < 1 {
		return Config{}, errors.New("invalid discovery interval, timeout, response header timeout, or request size")
	}
	names := map[string]bool{}
	for index, item := range config.Endpoints {
		parsed, parseErr := url.ParseRequestURI(item.URL)
		if item.Name == "" || names[item.Name] || parseErr != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
			return Config{}, fmt.Errorf("invalid or duplicate endpoint at index %d", index)
		}
		names[item.Name] = true
		if item.APIKey != "" && item.APIKeyEnv != "" {
			return Config{}, fmt.Errorf("endpoint %q: set api_key or api_key_env, not both", item.Name)
		}
		if item.APIKeyEnv != "" {
			key := os.Getenv(item.APIKeyEnv)
			if key == "" {
				return Config{}, fmt.Errorf("endpoint %q: environment variable %s is empty", item.Name, item.APIKeyEnv)
			}
			config.Endpoints[index].APIKey = key
		}
	}
	for model, fields := range config.ModelMetadata {
		if _, ok := fields["id"]; ok {
			return Config{}, fmt.Errorf("model_metadata for %q must not override id", model)
		}
	}
	return config, nil
}

func (r *router) withMetadata(id string, raw json.RawMessage) json.RawMessage {
	fields := r.config.ModelMetadata[id]
	if len(fields) == 0 {
		return raw
	}
	var document map[string]json.RawMessage
	if json.Unmarshal(raw, &document) != nil {
		return raw
	}
	for key, value := range fields {
		document[key] = value
	}
	merged, err := json.Marshal(document)
	if err != nil {
		return raw
	}
	return merged
}

func newEndpoint(config EndpointConfig, responseHeaderTimeout time.Duration) (*endpoint, error) {
	target, err := url.Parse(strings.TrimRight(config.URL, "/"))
	if err != nil {
		return nil, err
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	originalDirector := proxy.Director
	proxy.Director = func(request *http.Request) {
		originalDirector(request)
		request.Host = target.Host
		if config.APIKey != "" {
			request.Header.Set("Authorization", "Bearer "+config.APIKey)
		}
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseHeaderTimeout
	proxy.Transport = transport
	proxy.FlushInterval = -1
	proxy.ErrorHandler = func(writer http.ResponseWriter, _ *http.Request, proxyErr error) {
		log.Printf("proxy %s: %v", config.Name, proxyErr)
		writeError(writer, http.StatusBadGateway, "upstream unavailable", "upstream_error", "upstream_unavailable")
	}
	return &endpoint{config: config, proxy: proxy}, nil
}

func newRouter(config Config) (*router, error) {
	result := &router{
		config:   config,
		client:   &http.Client{Timeout: time.Duration(config.DiscoveryTimeoutSeconds) * time.Second},
		snapshot: registrySnapshot{},
	}
	for _, item := range config.Endpoints {
		upstream, err := newEndpoint(item, time.Duration(config.ResponseHeaderTimeoutSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		result.endpoints = append(result.endpoints, upstream)
	}
	result.metrics = newRouterMetrics(result)
	return result, nil
}

func (r *router) discover() {
	type result struct {
		endpoint  *endpoint
		documents map[string]json.RawMessage
		err       error
	}
	results := make(chan result, len(r.endpoints))
	for _, upstream := range r.endpoints {
		go func(item *endpoint) {
			started := time.Now()
			outcome := "error"
			defer func() {
				r.metrics.discoveryDuration.WithLabelValues(item.config.Name).Observe(time.Since(started).Seconds())
				r.metrics.discoveries.WithLabelValues(item.config.Name, outcome).Inc()
			}()
			modelsURL := strings.TrimRight(item.config.URL, "/") + "/v1/models"
			discoveryRequest, err := http.NewRequest(http.MethodGet, modelsURL, nil)
			if err != nil {
				results <- result{endpoint: item, err: err}
				return
			}
			if item.config.APIKey != "" {
				discoveryRequest.Header.Set("Authorization", "Bearer "+item.config.APIKey)
			}
			response, err := r.client.Do(discoveryRequest)
			if err != nil {
				results <- result{endpoint: item, err: err}
				return
			}
			defer response.Body.Close()
			if response.StatusCode != http.StatusOK {
				results <- result{endpoint: item, err: fmt.Errorf("GET /v1/models: %s", response.Status)}
				return
			}
			var document modelDocument
			if err := json.NewDecoder(io.LimitReader(response.Body, 4<<20)).Decode(&document); err != nil {
				results <- result{endpoint: item, err: err}
				return
			}
			documents := map[string]json.RawMessage{}
			for _, raw := range document.Data {
				var identity modelIdentity
				if json.Unmarshal(raw, &identity) == nil && identity.ID != "" {
					documents[identity.ID] = r.withMetadata(identity.ID, append(json.RawMessage(nil), raw...))
				}
			}
			outcome = "success"
			results <- result{endpoint: item, documents: documents}
		}(upstream)
	}

	discovered := map[string][]*endpoint{}
	documents := map[string]json.RawMessage{}
	errorsByEndpoint := map[string]string{}
	for range r.endpoints {
		item := <-results
		if item.err != nil {
			errorsByEndpoint[item.endpoint.config.Name] = item.err.Error()
			continue
		}
		for id, document := range item.documents {
			discovered[id] = append(discovered[id], item.endpoint)
			documents[id] = document
		}
	}

	models := map[string]*endpoint{}
	conflicts := map[string][]string{}
	for id, upstreams := range discovered {
		if len(upstreams) == 1 {
			models[id] = upstreams[0]
			continue
		}
		delete(documents, id)
		for _, upstream := range upstreams {
			conflicts[id] = append(conflicts[id], upstream.config.Name)
		}
		sort.Strings(conflicts[id])
	}

	r.mu.Lock()
	r.snapshot = registrySnapshot{models: models, conflicts: conflicts, documents: documents, errors: errorsByEndpoint, updatedAt: time.Now()}
	r.mu.Unlock()
	log.Printf("discovery: %d models, %d conflicts, %d unavailable endpoints", len(models), len(conflicts), len(errorsByEndpoint))
}

func (r *router) runDiscovery() {
	ticker := time.NewTicker(time.Duration(r.config.DiscoveryIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		r.discover()
	}
}

func (r *router) models(writer http.ResponseWriter) {
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

func (r *router) model(writer http.ResponseWriter, id string) {
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

func (r *router) health(writer http.ResponseWriter) {
	r.mu.RLock()
	payload := map[string]any{
		"status": "ok", "models": len(r.snapshot.models), "conflicts": r.snapshot.conflicts,
		"endpoint_errors": r.snapshot.errors, "updated_at": r.snapshot.updatedAt,
	}
	r.mu.RUnlock()
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(payload)
}

var errBodyTooLarge = errors.New("request body is too large")

type apiError struct {
	Message string  `json:"message"`
	Type    string  `json:"type"`
	Param   *string `json:"param"`
	Code    string  `json:"code"`
}

// writeError writes an error in the OpenAI API error format.
func writeError(writer http.ResponseWriter, status int, message, errorType, code string) {
	header := writer.Header()
	header.Del("Content-Length")
	header.Set("Content-Type", "application/json")
	header.Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(map[string]apiError{"error": {Message: message, Type: errorType, Code: code}})
}

func readModel(request *http.Request, maxBytes int64) (string, error) {
	if request.Body == nil {
		return "", errors.New("request body is required")
	}
	body, err := io.ReadAll(io.LimitReader(request.Body, maxBytes+1))
	request.Body.Close()
	if err != nil {
		return "", err
	}
	if int64(len(body)) > maxBytes {
		return "", errBodyTooLarge
	}
	request.Body = io.NopCloser(bytes.NewReader(body))
	request.ContentLength = int64(len(body))
	request.TransferEncoding = nil
	request.Header.Del("Transfer-Encoding")
	var identity requestIdentity
	if err := json.Unmarshal(body, &identity); err != nil || identity.Model == "" {
		return "", errors.New("valid JSON model field is required")
	}
	return identity.Model, nil
}

func requestAddresses(request *http.Request) (clientIP, directIP, forwardedFor string) {
	directIP = request.RemoteAddr
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		directIP = host
	}
	forwardedFor = strings.Join(request.Header.Values("X-Forwarded-For"), ", ")
	clientIP = directIP
	if first, _, _ := strings.Cut(forwardedFor, ","); net.ParseIP(strings.TrimSpace(first)) != nil {
		clientIP = strings.TrimSpace(first)
	}
	return clientIP, directIP, forwardedFor
}

func logModelRequest(request *http.Request, model, result, upstream string) {
	clientIP, directIP, forwardedFor := requestAddresses(request)
	log.Printf("request client_ip=%q x_forwarded_for=%q direct_ip=%q model=%q path=%q result=%q upstream=%q",
		clientIP, forwardedFor, directIP, model, request.URL.Path, result, upstream)
}

func (r *router) proxy(writer http.ResponseWriter, request *http.Request) {
	writer, observation := r.observe(writer, request)
	defer observation.finish(request)
	model, err := readModel(request, r.config.MaxRequestBytes)
	if errors.Is(err, errBodyTooLarge) {
		writeError(writer, http.StatusRequestEntityTooLarge, err.Error(), "invalid_request_error", "request_too_large")
		return
	}
	if err != nil {
		writeError(writer, http.StatusBadRequest, err.Error(), "invalid_request_error", "invalid_request")
		return
	}
	r.mu.RLock()
	conflict := append([]string(nil), r.snapshot.conflicts[model]...)
	upstream := r.snapshot.models[model]
	r.mu.RUnlock()
	if len(conflict) > 0 {
		observation.model, observation.outcome = model, "conflict"
		logModelRequest(request, model, "conflict", "")
		writeError(writer, http.StatusConflict, fmt.Sprintf("model %q is served by more than one endpoint", model), "invalid_request_error", "model_conflict")
		return
	}
	if upstream == nil {
		observation.outcome = "unknown_model"
		logModelRequest(request, model, "unknown_model", "")
		writeError(writer, http.StatusNotFound, fmt.Sprintf("model %q does not exist", model), "invalid_request_error", "model_not_found")
		return
	}
	logModelRequest(request, model, "routed", upstream.config.Name)
	observation.endpoint, observation.model, observation.outcome = upstream.config.Name, model, "completed"
	inflight := r.metrics.inflight.WithLabelValues(upstream.config.Name, model)
	inflight.Inc()
	defer inflight.Dec()
	upstream.proxy.ServeHTTP(writer, request)
}

var proxyRoutes = map[string]bool{
	"/v1/chat/completions": true,
	"/v1/completions":      true,
	"/v1/responses":        true,
	"/v1/embeddings":       true,
}

func (r *router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
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

func main() {
	path := os.Getenv("LLM_ROUTER_CONFIG")
	if path == "" {
		log.Fatal("LLM_ROUTER_CONFIG is required")
	}
	config, err := loadConfig(path)
	if err != nil {
		log.Fatal(err)
	}
	router, err := newRouter(config)
	if err != nil {
		log.Fatal(err)
	}
	router.discover()
	go router.runDiscovery()
	server := &http.Server{
		Addr:              config.Listen,
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	log.Printf("llm-router listening on %s with %d endpoints", config.Listen, len(config.Endpoints))
	log.Fatal(server.ListenAndServe())
}
