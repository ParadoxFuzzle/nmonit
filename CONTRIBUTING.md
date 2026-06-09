# Contributing to NMonit

## Prerequisites

- **Go 1.25+** — control plane and CLI
- **Rust** (stable) — compute agent
- **Docker + Docker Compose** — containerised stack
- **OpenSSL** — development TLS certificates

## Quick Start

```bash
# Clone
git clone https://github.com/compute-nmonit/nmonit.git
cd nmonit

# Generate development TLS certs (optional)
bash scripts/gen-certs.sh deploy/certs

# Start the full stack (dev mode — plaintext, no TLS)
docker compose up -d

# Verify
curl http://localhost:8080/health         # → {"status":"ok"}
curl http://localhost:3000/api/health     # Grafana health check (login: admin / admin)
curl http://localhost:9090/api/v1/targets # Prometheus
```

## Project Structure

| Directory | Description |
|---|---|
| `control-plane/` | Go gRPC + REST server (cluster management, metrics) |
| `agent/` | Rust node agent (resource discovery, heartbeats, task execution) |
| `cli/` | Go CLI client for interacting with the control plane |
| `proto/` | Shared protobuf definitions (buf) |
| `sdk/` | Client SDKs (Rust, Go, Python, C) |
| `storage/` | Distributed storage engine (Rust) |
| `network/` | RDMA networking layer (Rust) |
| `deploy/` | Dockerfiles, systemd units, Grafana dashboards, Prometheus config |
| `scripts/` | `gen-certs.sh` — development TLS certificate generator |

## Running the Stack

### Development (plaintext, no TLS)

```bash
docker compose up -d
```

The control plane runs with debug logging, no authentication, and no TLS.
Prometheus scrapes `/metrics` without credentials.

### Production (TLS + mTLS + HSTS)

```bash
# Generate certs first
bash scripts/gen-certs.sh deploy/certs

# Start with TLS
docker compose --profile prod up -d

# Verify HTTPS
curl -sk https://localhost:8080/health     # -k skips self-signed verification
```

The prod profile:
- Mounts certs from `deploy/certs/`
- Enables `--require-tls` (refuses to start without valid certs)
- Adds `Strict-Transport-Security` header to all REST responses
- Sets 30-second cert reload polling (plus SIGHUP trigger)

### Running Individual Components

```bash
# Control plane only (local development)
go run ./control-plane/cmd/control-plane --bootstrap --log-level=debug

# Control plane with TLS
go run ./control-plane/cmd/control-plane \
  --bootstrap \
  --tls-cert=deploy/certs/server.crt \
  --tls-key=deploy/certs/server.key \
  --tls-ca-cert=deploy/certs/ca.crt \
  --require-tls

# Agent (requires a running control plane)
cargo run -p compute-agent -- \
  --control-plane http://localhost:9000 \
  --mock-gpu-count 2

# Monitoring only (when control plane runs separately)
docker compose up prometheus grafana
```

## Generating TLS Certificates

```bash
bash scripts/gen-certs.sh [output_dir]
```

Creates:
- `ca.crt` / `ca.key` — self-signed Certificate Authority
- `server.crt` / `server.key` — control-plane server cert (CN=control-plane, SANs: localhost, 127.0.0.1, ::1)
- `agent.crt` / `agent.key` — agent client cert for mTLS

Certificates are 4096-bit RSA, valid for 10 years.

## Running Tests

```bash
# Go tests (with race detector)
cd control-plane && go test -race -count=1 ./...

# Rust tests
cargo test -p compute-agent

# Rust lints + formatting
cargo clippy -p compute-agent -- -D warnings
cargo fmt -p compute-agent -- --check
```

## Building Docker Images

```bash
# Control plane
docker build -f deploy/docker/Dockerfile.control-plane -t nmonit-control-plane .

# Full stack
docker compose build
```

## Monitoring

The compose stack includes:
- **Prometheus** — `http://localhost:9090` (scrapes control-plane `/metrics`)
- **Grafana** — `http://localhost:3000` (admin / admin, auto-provisioned dashboard)

The **NMonit — Compute Control Plane** dashboard covers:
- Registered nodes, tasks running, heartbeat rate
- gRPC request rates, latency (p50/p95/p99), active streams
- Node lifecycle (registrations, stale removals)
- TLS certificate load/reload attempts (success/failure)

### TLS Cert Reload

Certificates can be reloaded without restart:
- **Polling** — `--tls-reload-interval=60s` (default), checks file mtimes
- **SIGHUP** — `kill -HUP <pid>` triggers an immediate reload

On reload failure, the previous cert continues serving — no downtime. The `nmonit_tls_cert_load_total` metric tracks success/failure.

## Useful Commands

All `docker compose` commands default to the dev profile. For production, add
`--profile prod` and use `control-plane-prod` as the service name.

```bash
# View logs (dev)
docker compose logs -f control-plane

# View logs (prod)
docker compose --profile prod logs -f control-plane-prod

# Rebuild and restart after code changes
docker compose up -d --build

# Tear down
docker compose down

# Tear down and remove volumes (clean state)
docker compose down -v

# Check metric values
curl http://localhost:8080/metrics | grep nmonit_
```

## Architecture

See [ARCHITECTURE.md](ARCHITECTURE.md) for the full system design.

## .dockerignore

The `.dockerignore` excludes all source directories except `control-plane/` from
the Docker build context. If you add new directories needed at build time, update
the `.dockerignore` to whitelist them.
