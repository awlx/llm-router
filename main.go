package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
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
	ResponseHeaderTimeoutSeconds int `json:"response_header_timeout_seconds"`
	// ShutdownTimeoutSeconds limits the wait for open requests and streams on SIGTERM or SIGINT.
	ShutdownTimeoutSeconds int `json:"shutdown_timeout_seconds"`
	// DiscoveryGraceSeconds keeps the models of a failed endpoint for this time and returns 503 for them.
	DiscoveryGraceSeconds int              `json:"discovery_grace_seconds"`
	Endpoints             []EndpointConfig `json:"endpoints"`
	// Top-level fields merged into a model's /v1/models entry, e.g. architecture.input_modalities.
	ModelMetadata map[string]map[string]json.RawMessage `json:"model_metadata"`
	// Browser origins allowed by CORS, or "*". Empty disables CORS. LLM_ROUTER_CORS_ORIGINS overrides it.
	CORSAllowedOrigins []string `json:"cors_allowed_origins"`
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
	// unavailable maps model IDs of endpoints in the discovery grace period to the endpoint name.
	unavailable map[string]string
	updatedAt   time.Time
}

type seenModels struct {
	documents map[string]json.RawMessage
	at        time.Time
}

type router struct {
	metrics   *routerMetrics
	config    Config
	client    *http.Client
	endpoints []*endpoint
	mu        sync.RWMutex
	snapshot  registrySnapshot
	// corsOrigins is nil when CORS is disabled.
	corsOrigins map[string]bool
	// discoveryMu serializes discovery cycles and protects lastSeen.
	discoveryMu sync.Mutex
	lastSeen    map[string]seenModels
}

func defaultConfig() Config {
	return Config{
		Listen:                       "127.0.0.1:1234",
		DiscoveryIntervalSeconds:     15,
		DiscoveryTimeoutSeconds:      4,
		MaxRequestBytes:              32 << 20,
		ResponseHeaderTimeoutSeconds: 600,
		ShutdownTimeoutSeconds:       30,
		DiscoveryGraceSeconds:        60,
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
	if config.DiscoveryIntervalSeconds < 1 || config.DiscoveryTimeoutSeconds < 1 || config.MaxRequestBytes < 1024 || config.ResponseHeaderTimeoutSeconds < 1 || config.ShutdownTimeoutSeconds < 1 || config.DiscoveryGraceSeconds < 0 {
		return Config{}, errors.New("invalid discovery interval, timeout, grace, response header timeout, shutdown timeout, or request size")
	}
	names := map[string]bool{}
	for index, item := range config.Endpoints {
		item.URL = normalizeEndpointURL(item.URL)
		config.Endpoints[index].URL = item.URL
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
	if value, ok := os.LookupEnv("LLM_ROUTER_CORS_ORIGINS"); ok {
		config.CORSAllowedOrigins = nil
		for _, origin := range strings.Split(value, ",") {
			if origin = strings.TrimSpace(origin); origin != "" {
				config.CORSAllowedOrigins = append(config.CORSAllowedOrigins, origin)
			}
		}
	}
	for _, origin := range config.CORSAllowedOrigins {
		parsed, err := url.Parse(origin)
		if origin != "*" && (err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.Path != "" || parsed.RawQuery != "" || parsed.User != nil) {
			return Config{}, fmt.Errorf("invalid CORS origin %q", origin)
		}
	}
	return config, nil
}

// normalizeEndpointURL removes trailing slashes and a trailing /v1, because the router adds /v1 paths itself.
func normalizeEndpointURL(raw string) string {
	trimmed := strings.TrimRight(raw, "/")
	if strings.HasSuffix(trimmed, "/v1") {
		trimmed = strings.TrimRight(strings.TrimSuffix(trimmed, "/v1"), "/")
	}
	return trimmed
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
		lastSeen: map[string]seenModels{},
	}
	for _, item := range config.Endpoints {
		upstream, err := newEndpoint(item, time.Duration(config.ResponseHeaderTimeoutSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		if len(config.CORSAllowedOrigins) > 0 {
			upstream.proxy.ModifyResponse = removeCORSHeaders
		}
		result.endpoints = append(result.endpoints, upstream)
	}
	if len(config.CORSAllowedOrigins) > 0 {
		result.corsOrigins = map[string]bool{}
		for _, origin := range config.CORSAllowedOrigins {
			result.corsOrigins[origin] = true
		}
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

	r.discoveryMu.Lock()
	defer r.discoveryMu.Unlock()
	now := time.Now()
	grace := time.Duration(r.config.DiscoveryGraceSeconds) * time.Second
	discovered := map[string][]*endpoint{}
	documents := map[string]json.RawMessage{}
	errorsByEndpoint := map[string]string{}
	stale := map[*endpoint]bool{}
	for range r.endpoints {
		item := <-results
		name := item.endpoint.config.Name
		if item.err != nil {
			errorsByEndpoint[name] = item.err.Error()
			previous, ok := r.lastSeen[name]
			if !ok || now.Sub(previous.at) > grace {
				delete(r.lastSeen, name)
				continue
			}
			// Keep the last known models during the grace period, so that duplicate detection stays stable.
			item.documents = previous.documents
			stale[item.endpoint] = true
		} else {
			r.lastSeen[name] = seenModels{documents: item.documents, at: now}
		}
		for id, document := range item.documents {
			discovered[id] = append(discovered[id], item.endpoint)
			documents[id] = document
		}
	}

	models := map[string]*endpoint{}
	conflicts := map[string][]string{}
	unavailable := map[string]string{}
	for id, upstreams := range discovered {
		if len(upstreams) == 1 && stale[upstreams[0]] {
			unavailable[id] = upstreams[0].config.Name
			continue
		}
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
	r.snapshot = registrySnapshot{models: models, conflicts: conflicts, documents: documents, errors: errorsByEndpoint, unavailable: unavailable, updatedAt: time.Now()}
	r.mu.Unlock()
	log.Printf("discovery: %d models, %d conflicts, %d unavailable endpoints, %d models in grace period", len(models), len(conflicts), len(errorsByEndpoint), len(unavailable))
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

// ready reports 503 until discovery finds at least one routable model.
func (r *router) ready(writer http.ResponseWriter) {
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

func (r *router) health(writer http.ResponseWriter) {
	r.mu.RLock()
	payload := map[string]any{
		"status": "ok", "models": len(r.snapshot.models), "conflicts": r.snapshot.conflicts,
		"endpoint_errors": r.snapshot.errors, "unavailable_models": r.snapshot.unavailable, "updated_at": r.snapshot.updatedAt,
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

// logRequest writes one summary line when a proxied request finishes. The caller holds o.mu.
func logRequest(request *http.Request, o *requestObservation, duration time.Duration) {
	clientIP, directIP, forwardedFor := requestAddresses(request)
	log.Printf("request client_ip=%q x_forwarded_for=%q direct_ip=%q method=%q path=%q model=%q endpoint=%q status=%d outcome=%q duration_ms=%d request_bytes=%d response_bytes=%d",
		clientIP, forwardedFor, directIP, request.Method, request.URL.Path, o.requestedModel, o.endpoint, o.code, o.outcome, duration.Milliseconds(), o.inputBytes, o.bytes)
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
	observation.requestedModel = model
	r.mu.RLock()
	conflict := append([]string(nil), r.snapshot.conflicts[model]...)
	upstream := r.snapshot.models[model]
	unavailable := r.snapshot.unavailable[model]
	r.mu.RUnlock()
	if len(conflict) > 0 {
		observation.model, observation.outcome = model, "conflict"
		writeError(writer, http.StatusConflict, fmt.Sprintf("model %q is served by more than one endpoint", model), "invalid_request_error", "model_conflict")
		return
	}
	if unavailable != "" {
		observation.endpoint, observation.model, observation.outcome = unavailable, model, "endpoint_unavailable"
		writer.Header().Set("Retry-After", strconv.Itoa(r.config.DiscoveryIntervalSeconds))
		writeError(writer, http.StatusServiceUnavailable, fmt.Sprintf("model %q is temporarily unavailable", model), "server_error", "model_unavailable")
		return
	}
	if upstream == nil {
		observation.outcome = "unknown_model"
		writeError(writer, http.StatusNotFound, fmt.Sprintf("model %q does not exist", model), "invalid_request_error", "model_not_found")
		return
	}
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

// removeCORSHeaders drops upstream CORS headers so that only the router's headers reach the client.
func removeCORSHeaders(response *http.Response) error {
	for name := range response.Header {
		if strings.HasPrefix(name, "Access-Control-") {
			response.Header.Del(name)
		}
	}
	return nil
}

// cors adds CORS headers for allowed origins and reports whether the request was a handled preflight.
func (r *router) cors(writer http.ResponseWriter, request *http.Request) bool {
	if r.corsOrigins == nil {
		return false
	}
	header := writer.Header()
	header.Add("Vary", "Origin")
	origin := request.Header.Get("Origin")
	if origin == "" || (!r.corsOrigins["*"] && !r.corsOrigins[origin]) {
		return false
	}
	if r.corsOrigins["*"] {
		header.Set("Access-Control-Allow-Origin", "*")
	} else {
		header.Set("Access-Control-Allow-Origin", origin)
	}
	if request.Method != http.MethodOptions || request.Header.Get("Access-Control-Request-Method") == "" {
		return false
	}
	header.Add("Vary", "Access-Control-Request-Method")
	header.Add("Vary", "Access-Control-Request-Headers")
	header.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	allowHeaders := request.Header.Get("Access-Control-Request-Headers")
	if allowHeaders == "" {
		allowHeaders = "Authorization, Content-Type"
	}
	header.Set("Access-Control-Allow-Headers", allowHeaders)
	header.Set("Access-Control-Max-Age", "600")
	writer.WriteHeader(http.StatusNoContent)
	return true
}

func (r *router) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if r.cors(writer, request) {
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

func isLoopbackListen(address string) bool {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func inContainer() bool {
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	cgroup, err := os.ReadFile("/proc/1/cgroup")
	if err != nil {
		return false
	}
	for _, marker := range []string{"docker", "kubepods", "containerd", "libpod", "lxc"} {
		if bytes.Contains(cgroup, []byte(marker)) {
			return true
		}
	}
	return false
}

// serve runs the server until ctx ends, then stops new connections and waits up to timeout for open requests.
func serve(ctx context.Context, server *http.Server, listener net.Listener, timeout time.Duration) error {
	errorsChannel := make(chan error, 1)
	go func() { errorsChannel <- server.Serve(listener) }()
	select {
	case err := <-errorsChannel:
		return err
	case <-ctx.Done():
	}
	log.Printf("shutdown: waiting up to %s for open requests", timeout)
	shutdownContext, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := server.Shutdown(shutdownContext); err != nil {
		log.Printf("shutdown: %v; closing open connections", err)
		_ = server.Close()
		return err
	}
	log.Print("shutdown: complete")
	return nil
}

func main() {
	configPath := flag.String("config", "", "path to the JSON config file (default: $LLM_ROUTER_CONFIG)")
	flag.Parse()
	path := *configPath
	if path == "" {
		path = os.Getenv("LLM_ROUTER_CONFIG")
	}
	if path == "" {
		log.Fatal("set -config or LLM_ROUTER_CONFIG")
	}
	config, err := loadConfig(path)
	if err != nil {
		log.Fatal(err)
	}
	router, err := newRouter(config)
	if err != nil {
		log.Fatal(err)
	}
	if isLoopbackListen(config.Listen) && inContainer() {
		log.Printf("warning: listen address %s is loopback inside a container; clients outside the container cannot connect. Use 0.0.0.0:<port> to publish the port", config.Listen)
	}
	router.discover()
	go router.runDiscovery()
	server := &http.Server{
		Handler:           router,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    1 << 20,
	}
	listener, err := net.Listen("tcp", config.Listen)
	if err != nil {
		log.Fatal(err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	log.Printf("llm-router listening on %s with %d endpoints", config.Listen, len(config.Endpoints))
	if err := serve(ctx, server, listener, time.Duration(config.ShutdownTimeoutSeconds)*time.Second); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Fatal(err)
	}
}
