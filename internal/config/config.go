// Package config loads and validates the llm-router JSON configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"strings"
)

// EndpointConfig describes one OpenAI-compatible model server.
type EndpointConfig struct {
	Name string `json:"name"`
	URL  string `json:"url"`
	// APIKey is sent as a bearer token for discovery and proxied requests to this endpoint.
	APIKey string `json:"api_key,omitempty"`
	// APIKeyEnv names an environment variable that contains the API key.
	APIKeyEnv string `json:"api_key_env,omitempty"`
}

// Config is the router configuration.
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
	DiscoveryGraceSeconds int `json:"discovery_grace_seconds"`
	// ClientAPIKey enables bearer-token authentication for clients. LLM_ROUTER_API_KEY overrides it.
	ClientAPIKey string `json:"client_api_key,omitempty"`
	// AuthExemptPaths lists paths that do not need the client API key: /metrics, /health or /ready.
	AuthExemptPaths []string `json:"auth_exempt_paths,omitempty"`
	// TrustedProxies lists proxy addresses or CIDR ranges. Only these proxies can set the client IP with X-Forwarded-For.
	TrustedProxies []string         `json:"trusted_proxies"`
	Endpoints      []EndpointConfig `json:"endpoints"`
	// Top-level fields merged into a model's /v1/models entry, e.g. architecture.input_modalities.
	ModelMetadata map[string]map[string]json.RawMessage `json:"model_metadata"`
	// Browser origins allowed by CORS, or "*". Empty disables CORS. LLM_ROUTER_CORS_ORIGINS overrides it.
	CORSAllowedOrigins []string `json:"cors_allowed_origins"`
}

// Default returns the configuration defaults.
func Default() Config {
	return Config{
		Listen:                       "127.0.0.1:1234",
		DiscoveryIntervalSeconds:     15,
		DiscoveryTimeoutSeconds:      4,
		MaxRequestBytes:              32 << 20,
		ResponseHeaderTimeoutSeconds: 600,
		ShutdownTimeoutSeconds:       30,
		DiscoveryGraceSeconds:        60,
		TrustedProxies:               []string{"127.0.0.0/8", "::1/128"},
	}
}

// Load reads, validates and completes the configuration file at path.
func Load(path string) (Config, error) {
	config := Default()
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
	if key := os.Getenv("LLM_ROUTER_API_KEY"); key != "" {
		config.ClientAPIKey = key
	}
	if _, err := ParseTrustedProxies(config.TrustedProxies); err != nil {
		return Config{}, err
	}
	for _, path := range config.AuthExemptPaths {
		if path != "/metrics" && path != "/health" && path != "/ready" {
			return Config{}, fmt.Errorf("auth_exempt_paths: %q is not /metrics, /health or /ready", path)
		}
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

// ParseTrustedProxies parses addresses and CIDR ranges from trusted_proxies.
func ParseTrustedProxies(values []string) ([]*net.IPNet, error) {
	result := make([]*net.IPNet, 0, len(values))
	for _, value := range values {
		if !strings.Contains(value, "/") {
			ip := net.ParseIP(value)
			if ip == nil {
				return nil, fmt.Errorf("trusted_proxies: invalid address %q", value)
			}
			bits := 128
			if ip.To4() != nil {
				ip, bits = ip.To4(), 32
			}
			result = append(result, &net.IPNet{IP: ip, Mask: net.CIDRMask(bits, bits)})
			continue
		}
		_, network, err := net.ParseCIDR(value)
		if err != nil {
			return nil, fmt.Errorf("trusted_proxies: invalid CIDR %q", value)
		}
		result = append(result, network)
	}
	return result, nil
}
