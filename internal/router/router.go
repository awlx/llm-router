// Package router discovers models on the configured endpoints and proxies requests to the endpoint that serves the model.
package router

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/awlx/llm-router/internal/config"
)

type modelDocument struct {
	Object string            `json:"object"`
	Data   []json.RawMessage `json:"data"`
}

type modelIdentity struct {
	ID string `json:"id"`
}

type endpoint struct {
	config config.EndpointConfig
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

// Router routes requests by model to the discovered endpoints.
type Router struct {
	metrics        *routerMetrics
	trustedProxies []*net.IPNet
	config         config.Config
	client         *http.Client
	endpoints      []*endpoint
	mu             sync.RWMutex
	snapshot       registrySnapshot
	// corsOrigins is nil when CORS is disabled.
	corsOrigins map[string]bool
	// discoveryMu serializes discovery cycles and protects lastSeen.
	discoveryMu sync.Mutex
	lastSeen    map[string]seenModels
}

func newEndpoint(config config.EndpointConfig, responseHeaderTimeout time.Duration) (*endpoint, error) {
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

// New creates a Router. Call Discover before it serves requests.
func New(cfg config.Config) (*Router, error) {
	result := &Router{
		config:   cfg,
		client:   &http.Client{Timeout: time.Duration(cfg.DiscoveryTimeoutSeconds) * time.Second},
		snapshot: registrySnapshot{},
		lastSeen: map[string]seenModels{},
	}
	trusted, err := config.ParseTrustedProxies(cfg.TrustedProxies)
	if err != nil {
		return nil, err
	}
	result.trustedProxies = trusted
	for _, item := range cfg.Endpoints {
		upstream, err := newEndpoint(item, time.Duration(cfg.ResponseHeaderTimeoutSeconds)*time.Second)
		if err != nil {
			return nil, err
		}
		if len(cfg.CORSAllowedOrigins) > 0 {
			upstream.proxy.ModifyResponse = removeCORSHeaders
		}
		result.endpoints = append(result.endpoints, upstream)
	}
	if len(cfg.CORSAllowedOrigins) > 0 {
		result.corsOrigins = map[string]bool{}
		for _, origin := range cfg.CORSAllowedOrigins {
			result.corsOrigins[origin] = true
		}
	}
	result.metrics = newRouterMetrics(result)
	return result, nil
}
