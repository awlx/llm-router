package router

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
)

type requestIdentity struct {
	Model string `json:"model"`
}

var errBodyTooLarge = errors.New("request body is too large")

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

func (r *Router) proxy(writer http.ResponseWriter, request *http.Request) {
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
	if r.config.ClientAPIKey != "" {
		// Do not send the router API key to the server. An endpoint api_key is set again by the proxy.
		request.Header.Del("Authorization")
	}
	upstream.proxy.ServeHTTP(writer, request)
}
