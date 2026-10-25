package router

import (
	"net/http"
	"strings"
)

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
func (r *Router) cors(writer http.ResponseWriter, request *http.Request) bool {
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
