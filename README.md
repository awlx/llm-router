# LLM Router

A transparent OpenAI-compatible router that discovers exact model IDs from configured upstream `/v1/models` endpoints.

One endpoint for multiple model servers, without renaming models or changing
request bodies. Works with servers exposing the supported OpenAI-compatible
routes, including vLLM, TensorFold and LM Studio.

## Quick start

Install Go 1.23 or newer, then:

```sh
git clone https://github.com/awlx/llm-router.git
cd llm-router
go build -trimpath -o llm-router .
cp config.example.json router.json
# Edit router.json to point at your own model servers.
LLM_ROUTER_CONFIG=./router.json ./llm-router
```

Endpoint URLs are server roots, such as `http://127.0.0.1:8000`, without a `/v1`
suffix. The example's `192.0.2.0/24` addresses are documentation placeholders.
The default listener is loopback-only. In another terminal:

```sh
curl -q -fsS http://127.0.0.1:1234/v1/models
curl -q -fsS http://127.0.0.1:1234/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"your-exact-model-id","messages":[{"role":"user","content":"Hello!"}]}'
```

Use the exact ID returned by model discovery. Clients can use
`http://127.0.0.1:1234/v1` as their OpenAI-compatible base URL.

### Local Docker demo

No GPU or model download is needed. The demo starts two synthetic upstreams
advertising `model-a` and `model-b`, with the router published on loopback only:

```sh
docker compose -f docker-compose.test.yml up --build -d
# Allow a discovery cycle after both mock servers have started.
curl -q -fsS http://127.0.0.1:18235/v1/models
curl -q -fsS http://127.0.0.1:18235/v1/chat/completions \
  -H 'Content-Type: application/json' \
  -d '{"model":"model-b","messages":[{"role":"user","content":"Hello!"}]}'
docker compose -f docker-compose.test.yml down
```

## Behavior

- Periodically discovers models from every configured endpoint.
- Exposes the union through `GET /v1/models`.
- Routes `chat/completions`, `completions`, and `responses` by the request's exact `model` value.
- Does not alias or rewrite model names or request bodies.
- Streams upstream responses without an overall request timeout.
- Rejects duplicate model IDs with HTTP 409 rather than choosing an arbitrary endpoint.
- Reports discovery state through `GET /health`.
- Logs each valid model request with the effective client IP, `X-Forwarded-For`, direct peer, model ID, path, routing result, and selected upstream. Request bodies and authorization headers are never logged.

## Configuration

Copy `config.example.json`, replace the documentation-only endpoint addresses, and set:

```sh
LLM_ROUTER_CONFIG=/etc/llm-router/router.json llm-router
```

Optional `model_metadata` maps an exact model ID to top-level fields merged into that model's `/v1/models` entry (`id` cannot be overridden). Use it for capabilities upstreams do not report, e.g. OpenRouter-style `"architecture": {"input_modalities": ["text", "image"]}` for vision models.

Bind to loopback and place authentication/TLS in a private reverse proxy. The proxy should overwrite `X-Forwarded-For` with its connection source address so clients cannot spoof log attribution. Production topology belongs in an untracked runtime configuration, not source control.

The router does not authenticate clients; authorization headers are forwarded
to the selected upstream. Protect all routes, including model discovery, health
and metrics. Health errors can include upstream connection details, and request
logs contain client addresses and model IDs.

This is exact-model routing, not load balancing or failover among replicas:
duplicate IDs are rejected. Other OpenAI routes, such as embeddings and audio,
are not implemented. For Linux service installation, the example
[llm-router.service](llm-router.service) expects a dedicated `llm-router` user and
group, the binary at `/usr/local/bin/llm-router`, and configuration at
`/etc/llm-router/router.json`; create these before enabling the unit.

## Prometheus and Grafana

`GET /metrics` (also HEAD) serves Prometheus telemetry on the existing router
listener. It does not trigger discovery, inference, or inspect request bodies.
Keep the endpoint private, with the same access controls as the router.

- `llm_router_requests_total`: finished inference requests by exact served `model`,
	configured `endpoint`, fixed API `route`, HTTP `code`, and bounded `outcome`.
- `llm_router_requests_in_flight`: active proxied requests, including open streams.
- `llm_router_request_duration_seconds`: histogram for full request/stream duration.
- `llm_router_response_headers_duration_seconds`: histogram for time to final HTTP
	response headers. This is **not token TTFT**, since SSE headers may arrive early.
- `llm_router_request_size_bytes` / `llm_router_response_size_bytes`: histograms for
	whole request and answer body sizes. Input includes all chat messages, JSON and
	inline media; output includes JSON/SSE framing. These are bytes, not tokens or
	individual text-message lengths. No prompt/answer content is retained by metrics.
- `llm_router_request_bytes_total` / `llm_router_response_bytes_total`: traffic
	counters recorded when requests finish; unfinished streams contribute at completion.
- `llm_router_endpoint_up`, `llm_router_models`, `llm_router_model_conflicts`,
	`llm_router_model_info` and `llm_router_discovery_last_update_timestamp_seconds`:
	latest discovery state. Endpoint up measures discovery success, not inference health;
	use timestamp age to detect stale discovery.
- `llm_router_discovery_requests_total` / `llm_router_discovery_duration_seconds`:
	endpoint discovery outcomes and timing.

Rejected unknown/invalid model requests use empty model/endpoint labels, never
arbitrary user input. Conflicts use only discovered IDs. Metrics contain no client
IPs, authorization headers, raw URLs, prompts, or answers. Only the three supported
POST inference routes are counted; health, models and metrics requests are excluded.

Outcomes distinguish `completed`, `upstream_error` (HTTP 4xx/5xx), `invalid_request`,
`unknown_model`, `conflict`, `canceled`, and `aborted`. A response that breaks after
HTTP 200 can still be canceled/aborted; code `0` means no final headers were written.
Latency/size histograms include errors and partial responses. Oversized input is
counted only up to the read limit, and partial output counts bytes successfully
written, not proof of client receipt. Streaming is not buffered by instrumentation.
Counters/histograms reset on router restart and appear after their first observation.

Example Prometheus configuration for Prometheus on the same host:

```yaml
scrape_configs:
  - job_name: llm-router
    scrape_interval: 15s
    static_configs:
      - targets: ['127.0.0.1:1234']
```

For a containerized/remote Prometheus, use an existing authenticated proxy or a
reachable private address instead; container localhost is not the host. Do not
expose the router publicly just to enable scraping.

Import [docs/grafana-dashboard.json](docs/grafana-dashboard.json) through Grafana's
dashboard import and select the Prometheus datasource. Select a job, router instance,
endpoint, served model and API route. The panels include request/header latency
heatmaps, payload-size heatmaps and bucket bars, traffic, errors/cancellations and discovery.
Heatmap rows are payload-byte buckets; color is the request/response rate in each
non-cumulative bucket, aggregated across the selected models and endpoints.
Latency heatmaps use seconds instead of bytes. Whole-period size bars subtract
adjacent cumulative buckets and retain the overflow bucket. Their estimated
counts use `increase()` over the selected time range and can be fractional.
The collapsed latency-percentile section includes matching observation-count
charts. p50/p95/p99 are hidden below 2/20/100 observations in the same rate window;
these minimum sample floors are not statistical confidence guarantees.
Grafana itself does not scrape the endpoint.

### Combined Fleet Overview

Import [docs/grafana-overview.json](docs/grafana-overview.json) for a single
combined view of the router and vLLM fleet, with detailed sections collapsed.
Fleet panels require the separate
[vLLM Fleet Dashboard exporter](https://github.com/awlx/vllm-dashboard).
Choose one Prometheus datasource
that collects both exporters, then select the vLLM job/collector and router
job/instance separately. Use the fleet collector, not both collector and agents.

The shared **Served model** selector uses exact names from either exporter.
**Hardware node** filters host panels only; **Router API route** applies only
where route-level router metrics exist. Availability and router error panels
retain their broader scope so failed nodes and unknown-model rejections stay visible.
Router and engine traffic are shown separately, since engine counters also
include clients that bypass the router. Missing or stale series are not zero-filled.

Sections cover availability/freshness, token and request throughput, concurrency,
latency, chat/answer byte sizes, cache efficiency, errors, hardware and traffic.
Prefill and decode throughput use separate charts with independent scales;
request and response sizes use bucket heatmaps across the selected models.
The two overview gauge panels show one gauge per selected model for decode and
prefill. Values are latest one-minute token rates, including idle time, not
active-generation speed or utilization. Each arc scales to that model's observed
peak over the selected time range, not an assumed hardware capacity.
The overview groups panels into Health & Reliability, Throughput, Concurrency &
Pressure, Latency, Payloads, Efficiency and Infrastructure. Errors and sample
freshness stay visible. Open streams, running and waiting requests are separate
charts; preemptions and KV-cache utilization sit with concurrency pressure.
Payload byte traffic sits with size distributions. Payloads, efficiency and
infrastructure details remain expandable below.
TTFT, inter-token latency and engine end-to-end latency have separate mean-value
charts; engine averages are never presented as histogram distributions.
CPU/GPU utilization and prefix-cache/speculative acceptance are separate panels.
Legends and model selectors retain exact IDs; colors use Grafana's configured palette.
Detail links preserve multi-model selections, relevant node/route filters and time.
Router detail latency and payload rows repeat for each selected model so their
heatmaps do not mix different models; the overview retains aggregate heatmaps.
Scalar stats have explicit display names and value-only presentation, preventing
Grafana from using raw PromQL as their visible label. This is a presentation fix,
not a permission change: authorized users can still inspect panel queries.
Header latency remains distinct from token TTFT, and router percentiles are not
treated as equivalent to engine poll averages. Links to the two detailed dashboards
work after importing the router detail JSON and the
[fleet detail JSON](https://github.com/awlx/vllm-dashboard/blob/main/docs/grafana-dashboard.json).
No service deployment is needed
for this overview; it uses the existing metrics endpoints.

The pure migration in [docs/refine-dashboards.mjs](docs/refine-dashboards.mjs)
updates existing dashboard definitions while preserving datasource settings,
metadata and unknown panels. Run its focused regression checks with:

```sh
node --test docs/refine-dashboards.test.mjs
```

For live updates, read the current definitions first, keep a private backup,
apply the migration locally, and use Grafana's version check with `overwrite: false`.

## Development

Go 1.23 or newer is required; the container builds with Go 1.26.
Dashboard tests use Node.js 22 or newer and need no npm dependencies.

```sh
go test -race ./...
go vet ./...
CGO_ENABLED=0 go build -trimpath -o llm-router .
node --test docs/refine-dashboards.test.mjs
docker compose -f docker-compose.test.yml config --quiet
```

## License

[MIT](LICENSE).
