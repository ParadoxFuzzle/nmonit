# nmonit

> Pre-alpha hardware inventory and live utilization for a LAN compute cluster.

[![Rust](https://img.shields.io/badge/agent-Rust-orange)](agent/)
[![Go](https://img.shields.io/badge/control__plane-Go-00ADD8)](control-plane/)
[![Status](https://img.shields.io/badge/status-pre--alpha-red)]()
[![CI](https://github.com/ParadoxFuzzle/nmonit/actions/workflows/ci.yml/badge.svg)](https://github.com/ParadoxFuzzle/nmonit/actions/workflows/ci.yml)

---

## What works today

nmonit currently gives you a working LAN inventory and monitoring plane:

- A Rust agent discovers CPU, RAM, GPU, storage, and network hardware, registers with the control plane, and streams heartbeats.
- Heartbeats carry live CPU, memory, and NVIDIA GPU utilization (percentage, VRAM, temperature, power, clocks).
- A Go control plane keeps an in-memory node registry, removes stale nodes, exposes Prometheus metrics, and serves a small REST API.
- The `compute` CLI provides `health`, `status`/`nodes`, `watch`, and `version`.
- Optional TLS/mTLS, API-key and agent-token authentication, and certificate hot reload are implemented.
- Docker Compose development and production overlays plus systemd units are included.

This is **not** yet a job-execution or distributed-memory system. Those capabilities are listed in [docs/ROADMAP.md](docs/ROADMAP.md).

## Architecture

```
          compute CLI ────── REST / HTTPS ──────┐
                                                │
          Prometheus ─────── /metrics ──────────┤
                                                ▼
                                   ┌──────────────────────────┐
                                   │ Go control plane         │
                                   │ in-memory node registry  │
                                   │ REST + gRPC + metrics    │
                                   └────────────▲─────────────┘
                                                │
                                  gRPC Register / Heartbeat
                                                │
                     ┌──────────────────────────┼──────────────────────────┐
                     │                          │                          │
              ┌──────▼──────┐           ┌───────▼─────┐            ┌───────▼─────┐
              │ Rust agent  │           │ Rust agent  │            │ Rust agent  │
              │ CPU/RAM/GPU │           │ CPU/RAM/GPU │            │ CPU/RAM/GPU │
              └─────────────┘           └─────────────┘            └─────────────┘
```

The control plane is a single process. There is no scheduler, persistence layer, Raft consensus, or distributed execution yet.

## Getting Started

### Prerequisites

- **Go 1.25+** — control plane and CLI
- **Rust 1.95+** — node agent
- **Linux** — the agent probes Linux sysfs/procfs and NVIDIA NVML
- **NVIDIA drivers + NVML** — only for GPU utilization; CPU/memory monitoring works without a GPU
- **Docker + Docker Compose** — optional containerised stack
- **buf** — only when regenerating protobuf code

### Build and run locally

```bash
make

# Start the control plane (first node, local development)
./bin/control-plane --bootstrap --api-key dev-key --agent-token dev-agent-token

# Start an agent on this machine
./target/release/compute-agent \
  --control-plane http://localhost:9000 \
  --agent-token dev-agent-token

# Inspect the cluster
./bin/compute --server http://localhost:8080 --api-key dev-key health
./bin/compute --server http://localhost:8080 --api-key dev-key status
./bin/compute --server http://localhost:8080 --api-key dev-key watch
```

The plaintext/no-TLS setup above is for local development only. The agent logs a warning when TLS is not configured.

The CLI also accepts `NMONIT_SERVER`, `NMONIT_API_KEY`, and `NMONIT_CA_CERT`, and supports `--json` for scripting.

### Docker development stack

```bash
docker compose --profile dev up -d
```

This starts the control plane, Prometheus, and Grafana with development defaults. Grafana uses `admin / admin` in this profile only.

### Production stack

```bash
cp .env.example .env
# Replace every value in .env. Generate secrets with: openssl rand -hex 32

bash scripts/gen-certs.sh deploy/certs

docker compose \
  -f docker-compose.yml \
  -f docker-compose.prod.yml \
  --profile prod up -d
```

The production overlay requires `NMONIT_API_KEY`, `NMONIT_AGENT_TOKEN`, `GRAFANA_ADMIN_USER`, and `GRAFANA_ADMIN_PASSWORD`. It enables TLS, agent-token and REST API-key auth, authenticated HTTPS Prometheus scraping, and has no default Grafana password. See [CONTRIBUTING.md](CONTRIBUTING.md) for details.

## Project Structure

| Directory | Language | Purpose |
|---|---|---|
| `proto/` | Protobuf | Wire protocol definitions |
| `agent/` | Rust | Node agent: hardware discovery, registration, heartbeats, utilization |
| `control-plane/` | Go | Node registry, gRPC AgentService, REST API, Prometheus metrics |
| `cli/` | Go | `compute` CLI |
| `deploy/` | YAML / Docker | Dockerfiles, Compose, Prometheus, Grafana, systemd |
| `scripts/` | Shell | Development certificate generation and repository checks |
| `docs/` | Markdown | API, development, and roadmap documentation |

## Implemented API surface

- REST: `GET /health`, `GET /api/v1/nodes`, `GET /metrics`
- gRPC: `AgentService.Register` and `AgentService.Heartbeat`; the task/memory RPCs are registered but currently acknowledge without performing work
- CLI: `health`, `status`/`nodes`, `watch`, `version`

See [docs/API.md](docs/API.md) for request and response details.

## Documentation

- [Architecture](ARCHITECTURE.md)
- [API Reference](docs/API.md)
- [Development Guide](docs/DEVELOPMENT.md)
- [Roadmap](docs/ROADMAP.md)
- [Contributing](CONTRIBUTING.md)
- [Changelog](CHANGELOG.md)

## License

Intended license: MIT OR Apache-2.0. License files have not been added to the repository yet.
