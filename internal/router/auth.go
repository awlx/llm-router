package router

import (
	"crypto/subtle"
	"net/http"
	"strings"
)

// authorized checks the client API key. Exempt paths do not need a key.
func (r *Router) authorized(request *http.Request) bool {
	if r.config.ClientAPIKey == "" {
		return true
	}
	for _, path := range r.config.AuthExemptPaths {
		if request.URL.Path == path {
			return true
		}
	}
	token, ok := strings.CutPrefix(request.Header.Get("Authorization"), "Bearer ")
	return ok && subtle.ConstantTimeCompare([]byte(token), []byte(r.config.ClientAPIKey)) == 1
}
