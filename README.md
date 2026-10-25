# LLM Router

Use one API address for several model servers.
The router reads each server's model list and sends requests to the matching server.

- Works with vLLM, TensorFold and LM Studio.
- Keeps model names and request bodies unchanged.
- Supports streaming, chat completions, completions, responses and embeddings.
- Rejects duplicate model IDs instead of choosing a server.
- Returns errors in the OpenAI error format.

## Quick start

You need Go 1.23 or newer.

```sh
git clone https://github.com/awlx/llm-router.git
cd llm-router
go build -trimpath -o llm-router .
cp config.example.json router.json
```

Edit `router.json`. Replace the example addresses with your server addresses,
such as `http://127.0.0.1:8000`. The router removes a trailing `/v1`.

```sh
./llm-router -config ./router.json
```

You can also set the config path in `LLM_ROUTER_CONFIG`. The `-config` flag
has priority.

Set your client's base URL to `http://127.0.0.1:1234/v1`.
Use a model ID from `GET /v1/models`.
`GET /v1/models/{id}` returns one model.

## Configuration

| Key | Default | Description |
| --- | --- | --- |
| `listen` | `127.0.0.1:1234` | Address and port for the router. |
| `discovery_interval_seconds` | `15` | Time between model list updates. |
| `discovery_timeout_seconds` | `4` | Time limit for one model list request. |
| `discovery_grace_seconds` | `60` | Time to keep the models of a server after its model list fails. Set `0` to remove the models at once. |
| `max_request_bytes` | `33554432` | Maximum request body size. |
| `shutdown_timeout_seconds` | `30` | Time limit for open requests and streams after `SIGTERM` or `SIGINT`. |
| `response_header_timeout_seconds` | `600` | Time limit for the response headers of a server. Streams after the headers have no time limit. |
| `endpoints[].name` | | Unique server name. Metrics and logs use this name. |
| `endpoints[].url` | | Server address. The router removes a trailing `/v1`. |
| `endpoints[].api_key` | | API key for a server that needs authentication, for example vLLM with `--api-key`. |
| `endpoints[].api_key_env` | | Name of an environment variable that contains the API key. Use this or `api_key`, not both. |
| `model_metadata` | | Extra fields for model entries in `GET /v1/models`. |
| `cors_allowed_origins` | | Browser origins for CORS. See [Browser clients](#browser-clients). |

If a server has an API key, the router sends `Authorization: Bearer <key>`
to that server. This replaces the `Authorization` header of the client for
that server only. For other servers, the router sends the client header unchanged.

## Server failures

If the model list of a server fails, the router keeps the last known models of
that server for `discovery_grace_seconds`. During this time:

- Requests for these models get `503` with the error code `model_unavailable`
  and a `Retry-After` header.
- `GET /v1/models` continues to show these models.
- A model ID that is also on a different server continues to be a duplicate.
- `/health` shows these models in `unavailable_models`.

When the server is available again, the router sends requests to it again.
After the grace period, the router removes the models, and requests get `404`.

## Shutdown

On `SIGTERM` or `SIGINT`, the router stops new connections. Open requests and
streams continue until they finish or `shutdown_timeout_seconds` ends. Then
the router closes the remaining connections. Set the stop timeout of your
service manager to a longer time, for example `TimeoutStopSec` in systemd.

In a container, set `listen` to `0.0.0.0:<port>`. If `listen` is a loopback
address in a container, the router logs a warning.

## Docker demo

Try the router with two mock servers. No GPU or model download is needed.

```sh
docker compose -f docker-compose.test.yml up --build -d
# Wait a few seconds for model discovery.
curl -q -fsS http://127.0.0.1:18235/v1/models
docker compose -f docker-compose.test.yml down
```

For HTTPS, use the [Traefik example with Let's Encrypt and Hetzner DNS](examples/traefik).

## Browser clients

CORS is off by default. To let a web app call the router from a browser, set
`cors_allowed_origins` in the configuration or `LLM_ROUTER_CORS_ORIGINS`:

```sh
LLM_ROUTER_CORS_ORIGINS=https://app.example.com,http://localhost:3000
```

Use `*` to allow all origins. The environment variable replaces the
configuration value. When CORS is on, the router removes CORS headers from
server responses and sends its own headers.

## Limits and access

- No load balancing, failover or audio routes.
- No built-in authentication or TLS. Keep the router on loopback, or put an
  authenticated TLS proxy in front of all routes.
- Logs include client IPs and model IDs. Health errors can include server addresses.

## Monitoring

Use `/health` for discovery status and `/metrics` for Prometheus metrics.
`/health` always returns 200 while the process runs. Use it as a liveness check.
`/ready` returns 503 when no model is routable. Use it as a readiness check.
Metrics cover requests, errors, active streams, duration and body sizes.

Import the [router dashboard](docs/grafana-dashboard.json) into Grafana.
The optional [fleet overview](docs/grafana-overview.json) also needs
[vLLM Fleet Dashboard](https://github.com/awlx/vllm-dashboard).

Example dashboards with sample data:

![Router dashboard](docs/images/grafana-router.png)

![Fleet overview](docs/images/grafana-fleet.png)

## Tests

Dashboard tests need Node.js 22 or newer.

```sh
go test -race ./...
go vet ./...
node --test docs/refine-dashboards.test.mjs
```

## License

[MIT](LICENSE).
