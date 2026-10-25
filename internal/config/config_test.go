package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadConfigRejectsMetadataIDOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	config := `{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"model_metadata":{"m":{"id":"other"}}}`
	if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected id override error")
	}
}

func TestLoadConfigRejectsUnknownFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	if err := os.WriteFile(path, []byte(`{"listen":"127.0.0.1:1","endpoints":[],"unknown":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(path); err == nil {
		t.Fatal("expected unknown field error")
	}
}

func TestLoadConfigCORSOrigins(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	write := func(origins string) {
		t.Helper()
		config := `{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"cors_allowed_origins":` + origins + `}`
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`["https://app.example.com"]`)
	config, err := Load(path)
	if err != nil || len(config.CORSAllowedOrigins) != 1 {
		t.Fatalf("config=%v err=%v", config.CORSAllowedOrigins, err)
	}
	t.Setenv("LLM_ROUTER_CORS_ORIGINS", " https://a.example.com , http://localhost:3000 ")
	config, err = Load(path)
	if err != nil || strings.Join(config.CORSAllowedOrigins, "|") != "https://a.example.com|http://localhost:3000" {
		t.Fatalf("config=%v err=%v", config.CORSAllowedOrigins, err)
	}
	t.Setenv("LLM_ROUTER_CORS_ORIGINS", "")
	if config, err = Load(path); err != nil || len(config.CORSAllowedOrigins) != 0 {
		t.Fatalf("empty variable did not disable CORS: %v %v", config.CORSAllowedOrigins, err)
	}
	for _, origin := range []string{"example.com", "https://example.com/", "ftp://example.com", "https://example.com/path"} {
		t.Setenv("LLM_ROUTER_CORS_ORIGINS", origin)
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted origin %q", origin)
		}
	}
}

func TestLoadConfigAPIKeyEnv(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	write := func(config string) {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("TEST_BACKEND_KEY", "from-env")
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2","api_key_env":"TEST_BACKEND_KEY"}]}`)
	config, err := Load(path)
	if err != nil || config.Endpoints[0].APIKey != "from-env" {
		t.Fatalf("config=%+v err=%v", config, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2","api_key_env":"TEST_MISSING_KEY"}]}`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected empty environment variable error")
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2","api_key":"x","api_key_env":"TEST_BACKEND_KEY"}]}`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected api_key and api_key_env conflict error")
	}
}

func TestLoadConfigClientAuth(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	write := func(config string) {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"client_api_key":"from-file"}`)
	t.Setenv("LLM_ROUTER_API_KEY", "from-env")
	config, err := Load(path)
	if err != nil || config.ClientAPIKey != "from-env" {
		t.Fatalf("key=%q err=%v", config.ClientAPIKey, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"auth_exempt_paths":["/v1/models"]}`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid exempt path error")
	}
}

func TestLoadConfigTrustedProxies(t *testing.T) {
	path := filepath.Join(t.TempDir(), "router.json")
	write := func(config string) {
		if err := os.WriteFile(path, []byte(config), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}]}`)
	config, err := Load(path)
	if err != nil || len(config.TrustedProxies) != 2 {
		t.Fatalf("default trusted proxies=%v err=%v", config.TrustedProxies, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"trusted_proxies":[]}`)
	if config, err := Load(path); err != nil || len(config.TrustedProxies) != 0 {
		t.Fatalf("trusted proxies=%v err=%v", config.TrustedProxies, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"trusted_proxies":["10.0.0.0/33"]}`)
	if _, err := Load(path); err == nil {
		t.Fatal("expected invalid CIDR error")
	}
}

func TestNormalizeEndpointURL(t *testing.T) {
	for input, expected := range map[string]string{
		"http://192.0.2.1:8000":         "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/":        "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/v1":      "http://192.0.2.1:8000",
		"http://192.0.2.1:8000/v1/":     "http://192.0.2.1:8000",
		"https://example.com/proxy/v1/": "https://example.com/proxy",
		"https://example.com/v10":       "https://example.com/v10",
	} {
		if actual := normalizeEndpointURL(input); actual != expected {
			t.Errorf("normalizeEndpointURL(%q)=%q, want %q", input, actual, expected)
		}
	}
}
