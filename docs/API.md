# API Reference

The control plane exposes three REST routes and one gRPC agent service. There are no job, allocation, storage, or dataset endpoints yet.

## Base URL

Development: `http://localhost:8080`

Production: `https://localhost:8080` with the deployed certificate.

## Authentication

`/health` is unauthenticated.

`/api/v1/nodes` and `/metrics` require the configured `--api-key`. Supply it as either:

```http
X-API-Key: <key>
```

or:

```http
Authorization: Bearer <key>
```

If the control plane was started without `--api-key`, these routes are open. The legacy `api_key` query parameter is still accepted but should not be used for new integrations.

## GET /health

Liveness probe. Always returns HTTP 200 when the process is serving.

```bash
curl http://localhost:8080/health
```

```json
{"status":"ok"}
```

## GET /api/v1/nodes

Returns the current in-memory node registry.

```bash
curl -H "X-API-Key: dev-key" http://localhost:8080/api/v1/nodes
```

```json
[
  {
    "node_id": "node-1",
    "hostname": "worker-1",
    "address": "192.168.1.21:9000",
    "cpu": {
      "physical_cores": 8,
      "logical_cores": 16,
      "architecture": "x86_64",
      "model": "AMD Ryzen 7 5800X"
    },
    "memory": {
      "total_gb": 31.25
    },
    "gpus": [
      {
        "index": 0,
        "vendor": "nvidia",
        "model": "NVIDIA GeForce RTX 4070",
        "vram_gb": 12
      }
    ],
    "utilization": {
      "timestamp": "2026-11-07T12:34:56.789Z",
      "cpu": {
        "overall_percent": 33.2,
        "per_core_percent": [30.1, 36.4],
        "load_1m": 2.1,
        "load_5m": 1.9,
        "load_15m": 1.7
      },
      "memory": {
        "used_gb": 7.3,
        "available_gb": 23.9,
        "total_gb": 31.2
      },
      "gpus": [
        {
          "index": 0,
          "gpu_percent": 29,
          "memory_percent": 6.25,
          "vram_used_gb": 0.75,
          "vram_total_gb": 12,
          "temperature_c": 48,
          "power_draw_w": 72,
          "power_limit_w": 200,
          "clock_mhz": 2100,
          "memory_clock_mhz": 10501
        }
      ]
    },
    "last_beat": "2026-11-07T12:34:56Z"
  }
]
```

`utilization` is omitted until the first heartbeat carrying usage data arrives. `gpus` in the static resource section reflects discovery; `utilization.gpus` reflects the latest heartbeat.

## GET /metrics

Prometheus exposition format.

```bash
curl -H "X-API-Key: dev-key" http://localhost:8080/metrics
```

In the production Compose overlay, Prometheus authenticates with a Docker secret and scrapes over HTTPS. See `deploy/prometheus/prometheus.prod.yml`.

## gRPC AgentService

The agent service is defined in `proto/compute/v1/agent.proto`.

- `Register` — register a node and its hardware inventory.
- `Heartbeat` — bidirectional stream for utilization, health, pending tasks, and cancellations.
- `ExecuteTask`, `AllocateMemory`, `AllocateGPUMemory`, `FreeMemory`, `FreeGPUMemory` — currently acknowledge only; they do not perform the requested operation.

Agent authentication uses `--agent-token`, sent as a gRPC bearer token. If the control plane has no agent token configured, authentication is disabled.

Other protobuf services are present as future contracts but are not registered.

## Errors

REST auth failures return HTTP 401 with a JSON body:

```json
{"error":"unauthorized"}
```

Connection, TLS, and decode failures are reported by the CLI on stderr with a non-zero exit status.

## TLS

When TLS is configured, REST is served over HTTPS with TLS 1.3 and includes HSTS. gRPC uses TLS 1.3 and requires client certificates when `--tls-ca-cert` is configured.

The CLI accepts `--ca-cert` (or `NMONIT_CA_CERT`) to trust a private CA. It refuses to combine `--ca-cert` with an `http://` server URL.
