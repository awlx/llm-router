package router

import (
	"io"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/felixge/httpsnoop"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

type routerMetrics struct {
	handler           http.Handler
	requests          *prometheus.CounterVec
	duration          *prometheus.HistogramVec
	headers           *prometheus.HistogramVec
	firstByte         *prometheus.HistogramVec
	responseBytes     *prometheus.CounterVec
	requestBytes      *prometheus.CounterVec
	requestSize       *prometheus.HistogramVec
	responseSize      *prometheus.HistogramVec
	inflight          *prometheus.GaugeVec
	discoveries       *prometheus.CounterVec
	discoveryDuration *prometheus.HistogramVec
}

func newRouterMetrics(owner *Router) *routerMetrics {
	labels := []string{"endpoint", "model", "route"}
	sizeBuckets := []float64{128, 512, 1024, 4096, 16384, 65536, 262144, 1048576, 4194304, 16777216, 33554432}
	result := &routerMetrics{
		requests:          prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llm_router_requests_total", Help: "Finished inference requests, including rejected and aborted requests."}, []string{"endpoint", "model", "route", "code", "outcome"}),
		duration:          prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_request_duration_seconds", Help: "Inference request duration including the entire response stream, errors and cancellations.", Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300, 600, 1800, 3600}}, labels),
		headers:           prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_response_headers_duration_seconds", Help: "Time to first final response headers; not time to first generated token.", Buckets: []float64{.005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120}}, labels),
		firstByte:         prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_time_to_first_byte_seconds", Help: "Time from request start to the first response body byte written to the client, for routed requests with status below 400. For streams, this is close to time to first token.", Buckets: []float64{.025, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 120, 300}}, []string{"endpoint", "model"}),
		responseBytes:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llm_router_response_bytes_total", Help: "Response body bytes successfully written, excluding HTTP headers."}, labels),
		requestBytes:      prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llm_router_request_bytes_total", Help: "Request body bytes read, including JSON framing; oversized requests are counted only to the read limit."}, labels),
		requestSize:       prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_request_size_bytes", Help: "Request body size read, including JSON framing and all chat messages; not token count.", Buckets: sizeBuckets}, labels),
		responseSize:      prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_response_size_bytes", Help: "Response body size successfully written, including JSON/SSE framing; partial responses count only delivered bytes.", Buckets: sizeBuckets}, labels),
		inflight:          prometheus.NewGaugeVec(prometheus.GaugeOpts{Name: "llm_router_requests_in_flight", Help: "Inference requests currently proxied, including open response streams."}, []string{"endpoint", "model"}),
		discoveries:       prometheus.NewCounterVec(prometheus.CounterOpts{Name: "llm_router_discovery_requests_total", Help: "Endpoint discovery attempts by success or error."}, []string{"endpoint", "result"}),
		discoveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "llm_router_discovery_duration_seconds", Help: "Endpoint model discovery duration.", Buckets: prometheus.DefBuckets}, []string{"endpoint"}),
	}
	registry := prometheus.NewRegistry()
	registry.MustRegister(result.requests, result.duration, result.headers, result.firstByte, result.responseBytes, result.requestBytes, result.requestSize, result.responseSize, result.inflight, result.discoveries, result.discoveryDuration, &discoveryCollector{owner: owner})
	result.handler = promhttp.HandlerFor(registry, promhttp.HandlerOpts{})
	return result
}

type requestObservation struct {
	mu                              sync.Mutex
	metrics                         *routerMetrics
	trustedProxies                  []*net.IPNet
	started                         time.Time
	endpoint, model, route, outcome string
	// requestedModel is the client's model field, for logs only; metrics use model.
	requestedModel string
	code           int
	firstByteSeen  bool
	bytes          int64
	inputBytes     int64
}

type observedRequestBody struct {
	io.ReadCloser
	observation *requestObservation
}

func (body observedRequestBody) Read(data []byte) (int, error) {
	count, err := body.ReadCloser.Read(data)
	body.observation.inputBytes += int64(count)
	return count, err
}

func (r *Router) observe(writer http.ResponseWriter, request *http.Request) (http.ResponseWriter, *requestObservation) {
	observation := &requestObservation{metrics: r.metrics, trustedProxies: r.trustedProxies, started: time.Now(), route: request.URL.Path, outcome: "invalid_request"}
	if request.Body != nil {
		request.Body = observedRequestBody{ReadCloser: request.Body, observation: observation}
	}
	markHeaders := func(code int) {
		observation.mu.Lock()
		defer observation.mu.Unlock()
		if observation.code == 0 && (code >= 200 || code == 101) {
			observation.code = code
			r.metrics.headers.WithLabelValues(observation.endpoint, observation.model, observation.route).Observe(time.Since(observation.started).Seconds())
		}
	}
	wrapped := httpsnoop.Wrap(writer, httpsnoop.Hooks{
		WriteHeader: func(next httpsnoop.WriteHeaderFunc) httpsnoop.WriteHeaderFunc {
			return func(code int) { markHeaders(code); next(code) }
		},
		Write: func(next httpsnoop.WriteFunc) httpsnoop.WriteFunc {
			return func(data []byte) (int, error) {
				markHeaders(http.StatusOK)
				written, err := next(data)
				observation.addBytes(int64(written))
				return written, err
			}
		},
		ReadFrom: func(next httpsnoop.ReadFromFunc) httpsnoop.ReadFromFunc {
			return func(reader io.Reader) (int64, error) {
				markHeaders(http.StatusOK)
				written, err := next(reader)
				observation.addBytes(written)
				return written, err
			}
		},
		Flush: func(next httpsnoop.FlushFunc) httpsnoop.FlushFunc {
			return func() { markHeaders(http.StatusOK); next() }
		},
	})
	return wrapped, observation
}

// addBytes counts written body bytes and records the time to the first byte of a routed request.
func (o *requestObservation) addBytes(written int64) {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.bytes += written
	if o.firstByteSeen || written == 0 {
		return
	}
	o.firstByteSeen = true
	if o.endpoint != "" && o.code < 400 {
		o.metrics.firstByte.WithLabelValues(o.endpoint, o.model).Observe(time.Since(o.started).Seconds())
	}
}

func (o *requestObservation) finish(request *http.Request) {
	panicValue := recover()
	o.mu.Lock()
	defer o.mu.Unlock()
	if request.Context().Err() != nil {
		o.outcome = "canceled"
	} else if panicValue != nil {
		o.outcome = "aborted"
	} else if o.outcome == "completed" && o.code >= 400 {
		o.outcome = "upstream_error"
	}
	if o.code == 0 && panicValue == nil && request.Context().Err() == nil {
		o.code = http.StatusOK
	}
	o.metrics.requests.WithLabelValues(o.endpoint, o.model, o.route, strconv.Itoa(o.code), o.outcome).Inc()
	duration := time.Since(o.started)
	o.metrics.duration.WithLabelValues(o.endpoint, o.model, o.route).Observe(duration.Seconds())
	o.metrics.responseBytes.WithLabelValues(o.endpoint, o.model, o.route).Add(float64(o.bytes))
	o.metrics.requestBytes.WithLabelValues(o.endpoint, o.model, o.route).Add(float64(o.inputBytes))
	o.metrics.requestSize.WithLabelValues(o.endpoint, o.model, o.route).Observe(float64(o.inputBytes))
	o.metrics.responseSize.WithLabelValues(o.endpoint, o.model, o.route).Observe(float64(o.bytes))
	logRequest(request, o, duration)
	if panicValue != nil {
		panic(panicValue)
	}
}

var (
	endpointUp       = prometheus.NewDesc("llm_router_endpoint_up", "Whether the last model discovery succeeded; not inference health.", []string{"endpoint"}, nil)
	discoveryUpdated = prometheus.NewDesc("llm_router_discovery_last_update_timestamp_seconds", "Unix timestamp of the last completed discovery cycle.", nil, nil)
	discoveredModels = prometheus.NewDesc("llm_router_models", "Number of uniquely routable model IDs.", nil, nil)
	modelConflicts   = prometheus.NewDesc("llm_router_model_conflicts", "Number of model IDs rejected because multiple endpoints advertise them.", nil, nil)
	modelRoute       = prometheus.NewDesc("llm_router_model_info", "Current discovered model routing map; value 1 means uniquely routable.", []string{"endpoint", "model"}, nil)
)

type discoveryCollector struct{ owner *Router }

func (c *discoveryCollector) Describe(ch chan<- *prometheus.Desc) {
	for _, desc := range []*prometheus.Desc{endpointUp, discoveryUpdated, discoveredModels, modelConflicts, modelRoute} {
		ch <- desc
	}
}

func (c *discoveryCollector) Collect(ch chan<- prometheus.Metric) {
	c.owner.mu.RLock()
	snapshot := c.owner.snapshot
	c.owner.mu.RUnlock()
	updated := 0.0
	if !snapshot.updatedAt.IsZero() {
		updated = float64(snapshot.updatedAt.UnixMilli()) / 1000
	}
	ch <- prometheus.MustNewConstMetric(discoveryUpdated, prometheus.GaugeValue, updated)
	ch <- prometheus.MustNewConstMetric(discoveredModels, prometheus.GaugeValue, float64(len(snapshot.models)))
	ch <- prometheus.MustNewConstMetric(modelConflicts, prometheus.GaugeValue, float64(len(snapshot.conflicts)))
	for _, upstream := range c.owner.endpoints {
		up := 0.0
		if _, failed := snapshot.errors[upstream.config.Name]; !failed && updated > 0 {
			up = 1
		}
		ch <- prometheus.MustNewConstMetric(endpointUp, prometheus.GaugeValue, up, upstream.config.Name)
	}
	for model, upstream := range snapshot.models {
		ch <- prometheus.MustNewConstMetric(modelRoute, prometheus.GaugeValue, 1, upstream.config.Name, model)
	}
}
