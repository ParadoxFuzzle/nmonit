# Architecture

**Status:** pre-alpha. This document describes the code that exists today, not the target system.

For capabilities that are defined in protobuf or discussed as future work, see [docs/ROADMAP.md](docs/ROADMAP.md). The rule for this document is simple: if a component is not wired into the running binaries, it does not belong here.

## Runtime topology

```
CLI / Prometheus
      │  REST (HTTP or HTTPS)
      ▼
┌─────────────────────────────────────────────┐
│ Go control plane                             │
│  - net/http REST API                         │
│  - gRPC AgentService                         │
│  - in-memory node registry                   │
│  - Prometheus collectors                     │
│  - TLS certificate reloader                  │
└──────────────────▲──────────────────────────┘
                   │ gRPC Register + Heartbeat
       ┌───────────┼───────────┐
       │           │           │
  Rust agent  Rust agent  Rust agent
```

Each agent maintains one bidirectional `Heartbeat` stream to the control plane. There is one control-plane process; it is not clustered or replicated.

## Control plane

Language: Go. Entry point: `control-plane/cmd/control-plane/main.go`.

### Startup

- Parses flags for gRPC/HTTP addresses, logging, TLS, auth, and certificate reload.
- Creates an in-memory `registry.NodeRegistry`.
- Starts stale-node cleanup in a goroutine (30-second interval).
- Serves gRPC on `--grpc-addr` (default `:9000`).
- Serves REST on `--http-addr` (default `:8080`).
- With TLS configured, both listeners use the same certificate; gRPC uses TLS 1.3. REST uses TLS 1.3 but does not enforce client certificates.
- `SIGINT`/`SIGTERM` trigger bounded gRPC shutdown and HTTP shutdown; `SIGHUP` forces a certificate reload.

`--bootstrap` currently only affects the generated cluster ID. It does not initialize consensus, persistence, or a multi-node control plane.

### Node registry

Package: `control-plane/internal/registry`.

The registry is entirely in memory. It stores:

- Node identity, hostname, and address.
- CPU, memory, GPU, storage, and network inventory reported at registration.
- The latest utilization snapshot from heartbeats.
- Last heartbeat time and the active stream/connection ID.
- Per-node task state used by the current registry model.

Heartbeats update utilization and the last-seen timestamp. A background cleanup removes nodes that have not heartbeat within the stale threshold. Registry contents are lost on control-plane restart; agents re-register when they reconnect.

### gRPC AgentService

Package: `control-plane/internal/agent`.

`Register` and `Heartbeat` are the functional RPCs:

- `Register` validates and stores the node inventory and returns cluster metadata and heartbeat settings.
- `Heartbeat` is a bidirectional stream. The agent sends periodic updates; the server sends pending-task and cancellation instructions.

`ExecuteTask`, `AllocateMemory`, `AllocateGPUMemory`, `FreeMemory`, and `FreeGPUMemory` are registered and accept requests, but they only acknowledge. They do not dispatch work, allocate memory, register RDMA regions, or free real resources.

Authentication is a shared `--agent-token` sent as a bearer token. If no token is configured, agent auth is disabled. Input validation lives in `internal/validator`; rejected requests are logged.

The other protobuf services (`JobService`, `MemoryService`, `StorageService`, `ControlService`) are defined in `proto/` but are not registered on the gRPC server.

### REST API

Package: `control-plane/internal/restapi`.

Routes currently served:

| Route | Auth | Purpose |
|---|---|---|
| `GET /health` | None | Liveness response |
| `GET /api/v1/nodes` | API key if configured | Registered-node snapshot with live utilization |
| `GET /metrics` | API key if configured | Prometheus metrics |

The API key can be supplied through `X-API-Key` or `Authorization: Bearer`. A legacy `api_key` query parameter is also accepted. Comparison is constant-time. When no API key is configured, these routes are open.

Responses include security headers; HSTS is added when TLS is enabled. See [docs/API.md](docs/API.md) for schemas.

### Metrics

Package: `control-plane/internal/metrics`.

Prometheus metrics cover node counts, registrations, heartbeat activity, stale removals, gRPC request counts/latency/streams, and TLS certificate loads. gRPC method names are validated against known methods before being used as labels.

The development Compose stack scrapes `/metrics` without auth. The production overlay sends the API key via a Docker secret and verifies the control-plane certificate against the configured CA.

### TLS reload

Package: `control-plane/internal/tlsreload`.

The reloader loads the configured certificate, key, and optional CA. It polls file modification times at `--tls-reload-interval` and also reloads on `SIGHUP`. A failed reload keeps the previous certificate serving.

## Agent

Language: Rust (tokio). Entry point: `agent/src/main.rs`.

Responsibilities:

- Discover CPU, memory, GPU, storage, and network inventory at startup (`resources.rs`, `gpu.rs`).
- Register with the control plane.
- Open a heartbeat stream and send utilization every server-provided interval.
- Collect CPU/memory usage and NVIDIA GPU utilization through NVML.
- Reconnect with exponential backoff after stream or connection failures.
- Shut down cleanly on `SIGINT` or `SIGTERM`, including while connecting or sleeping in backoff.

The agent does not execute tasks, allocate distributed memory, register RDMA memory, run containers, or discover peers via mDNS. Those modules do not exist.

TLS is optional. Supplying all three of `--tls-ca-cert`, `--tls-cert`, and `--tls-key` enables mTLS and requires an `https://` control-plane URL. Supplying only part of that set is a startup error.

## Data flow

### Registration

1. Agent detects hardware and node ID.
2. Agent calls `AgentService.Register` with its inventory and bearer token.
3. Control plane validates the request and inserts the node into the in-memory registry.
4. Control plane returns cluster metadata and heartbeat settings.

### Heartbeats and utilization

1. Agent opens the `Heartbeat` stream.
2. Agent sends a heartbeat on each interval with a sequence number and a utilization snapshot.
3. Control plane verifies the node and connection ID, updates the registry, and updates Prometheus state.
4. REST and CLI readers see the latest inventory and utilization immediately.
5. If the stream fails, the agent reconnects with backoff; if the control plane sees no heartbeat for the stale threshold, it removes the node.

### Shutdown

- Control plane: stop accepting new work, bound gRPC graceful shutdown to five seconds, then force-stop and shut down HTTP.
- Agent: the shared SIGINT/SIGTERM future is polled during connect, stream handling, and reconnect backoff, so shutdown is not limited to an active stream.

## Deployment

- `docker-compose.yml` is the development stack. All services use the `dev` profile and run without auth or TLS.
- `docker-compose.prod.yml` is an overlay. It requires secrets from `.env`, enables TLS, API-key and agent-token auth, and HTTPS metrics scraping, and removes the default Grafana credentials.
- Dockerfiles live in `deploy/docker/`.
- systemd units live in `deploy/systemd/`.
- Prometheus development config: `deploy/prometheus/prometheus.yml`.
- Prometheus production config: `deploy/prometheus/prometheus.prod.yml`.
- Grafana dashboards and datasources: `deploy/grafana/`.

## Design constraints today

1. **Single control-plane process.** No consensus, replication, or failover.
2. **Ephemeral state.** The registry lives in memory and is rebuilt from agent registrations.
3. **Inventory and observability first.** Execution, memory, storage, and networking are not implemented.
4. **Authentication is shared-secret based.** There is no user account system, RBAC, or certificate-based REST identity.
5. **Linux-first.** Hardware discovery depends on Linux interfaces and optional NVIDIA NVML.

## Directory map

```
nmonit/
├── agent/                 Rust agent
├── cli/                   Go compute CLI
├── control-plane/         Go control plane
├── proto/                 Protobuf contracts
├── deploy/                Docker, Compose, Prometheus, Grafana, systemd
├── docs/                  API, development, roadmap
├── scripts/               Cert generation and repository checks
├── docker-compose.yml     Development stack
└── docker-compose.prod.yml Production overlay
```
