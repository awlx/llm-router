package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func mustTrusted(t *testing.T, values ...string) []*net.IPNet {
	t.Helper()
	trusted, err := parseTrustedProxies(values)
	if err != nil {
		t.Fatal(err)
	}
	return trusted
}

func TestRequestAddressesTrustedProxies(t *testing.T) {
	trusted := mustTrusted(t, "127.0.0.0/8", "198.51.100.0/24", "2001:db8::1")
	for _, item := range []struct {
		name, remote, forwarded, client string
	}{
		{"untrusted peer ignores header", "203.0.113.9:1000", "192.0.2.44", "203.0.113.9"},
		{"trusted peer uses header", "127.0.0.1:1000", "192.0.2.44", "192.0.2.44"},
		{"spoofed first entry ignored", "127.0.0.1:1000", "192.0.2.1, 192.0.2.44", "192.0.2.44"},
		{"trusted chain skipped", "127.0.0.1:1000", "192.0.2.44, 198.51.100.7", "192.0.2.44"},
		{"invalid entry stops", "127.0.0.1:1000", "not-an-ip", "127.0.0.1"},
		{"invalid entry before trusted hop", "127.0.0.1:1000", "not-an-ip, 198.51.100.7", "198.51.100.7"},
		{"single IPv6 address", "[2001:db8::1]:1000", "192.0.2.44", "192.0.2.44"},
		{"no header", "127.0.0.1:1000", "", "127.0.0.1"},
	} {
		request := httptest.NewRequest(http.MethodGet, "/", nil)
		request.RemoteAddr = item.remote
		if item.forwarded != "" {
			request.Header.Set("X-Forwarded-For", item.forwarded)
		}
		if client, _, _ := requestAddresses(request, trusted); client != item.client {
			t.Errorf("%s: client=%q, want %q", item.name, client, item.client)
		}
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.RemoteAddr = "127.0.0.1:1000"
	request.Header.Set("X-Forwarded-For", "192.0.2.44")
	if client, _, _ := requestAddresses(request, nil); client != "127.0.0.1" {
		t.Errorf("empty trusted list: client=%q", client)
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
	config, err := loadConfig(path)
	if err != nil || len(config.TrustedProxies) != 2 {
		t.Fatalf("default trusted proxies=%v err=%v", config.TrustedProxies, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"trusted_proxies":[]}`)
	if config, err := loadConfig(path); err != nil || len(config.TrustedProxies) != 0 {
		t.Fatalf("trusted proxies=%v err=%v", config.TrustedProxies, err)
	}
	write(`{"listen":"127.0.0.1:1","endpoints":[{"name":"a","url":"http://127.0.0.1:2"}],"trusted_proxies":["10.0.0.0/33"]}`)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected invalid CIDR error")
	}
}
