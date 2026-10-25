package router

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

func (r *Router) withMetadata(id string, raw json.RawMessage) json.RawMessage {
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

// Discover queries all endpoints once and replaces the routing table.
func (r *Router) Discover() {
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

// RunDiscovery calls Discover at the configured interval. It does not return.
func (r *Router) RunDiscovery() {
	ticker := time.NewTicker(time.Duration(r.config.DiscoveryIntervalSeconds) * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		r.Discover()
	}
}
