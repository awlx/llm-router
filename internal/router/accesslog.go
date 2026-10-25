package router

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

func isTrusted(ip net.IP, trusted []*net.IPNet) bool {
	for _, network := range trusted {
		if network.Contains(ip) {
			return true
		}
	}
	return false
}

// requestAddresses uses X-Forwarded-For only when the direct peer is a trusted proxy.
// It reads the header from right to left and stops at the first address that is not a trusted proxy.
func requestAddresses(request *http.Request, trusted []*net.IPNet) (clientIP, directIP, forwardedFor string) {
	directIP = request.RemoteAddr
	if host, _, err := net.SplitHostPort(request.RemoteAddr); err == nil {
		directIP = host
	}
	forwardedFor = strings.Join(request.Header.Values("X-Forwarded-For"), ", ")
	clientIP = directIP
	direct := net.ParseIP(directIP)
	if direct == nil || !isTrusted(direct, trusted) || forwardedFor == "" {
		return clientIP, directIP, forwardedFor
	}
	hops := strings.Split(forwardedFor, ",")
	for index := len(hops) - 1; index >= 0; index-- {
		ip := net.ParseIP(strings.TrimSpace(hops[index]))
		if ip == nil {
			break
		}
		clientIP = ip.String()
		if !isTrusted(ip, trusted) {
			break
		}
	}
	return clientIP, directIP, forwardedFor
}

// logRequest writes one summary line when a proxied request finishes. The caller holds o.mu.
func logRequest(request *http.Request, o *requestObservation, duration time.Duration) {
	clientIP, directIP, forwardedFor := requestAddresses(request, o.trustedProxies)
	log.Printf("request client_ip=%q x_forwarded_for=%q direct_ip=%q method=%q path=%q model=%q endpoint=%q status=%d outcome=%q duration_ms=%d request_bytes=%d response_bytes=%d",
		clientIP, forwardedFor, directIP, request.Method, request.URL.Path, o.requestedModel, o.endpoint, o.code, o.outcome, duration.Milliseconds(), o.inputBytes, o.bytes)
}
