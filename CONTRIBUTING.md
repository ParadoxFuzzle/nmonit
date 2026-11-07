# Contributing to NMonit

## Prerequisites

- **Go 1.25+** — control plane and CLI
- **Rust 1.95+** — compute agent
- **Docker + Docker Compose** — containerised stack
- **OpenSSL** — development TLS certificates
- **buf** — only for protobuf changes

## Quick Start

```bash
# Clone
git clone https://github.com/ParadoxFuzzle/nmonit.git
cd nmonit

# Build the implemented binaries
make

# Run the control plane and agent locally
./bin/control-plane --bootstrap --api-key dev-key --agent-token dev-agent-token
./target/release/compute-agent --control-plane http://localhost:9000 --agent-token dev-agent-token

# Check it
./bin/compute --server http://localhost:8080 --api-key dev-key health
./bin/compute --server http://localhost:8080 --api-key dev-key status
```

The local plaintext setup is for development only.

## Project Structure

| Directory | Description |
|---|---|
| `control-plane/` | Go gRPC + REST server, in-memory registry, metrics |
| `agent/` | Rust node agent: hardware discovery, heartbeats, utilization |
| `cli/` | Go CLI client for the REST API |
| `proto/` | Shared protobuf definitions |
| `deploy/` | Dockerfiles, Compose, systemd units, Grafana, Prometheus |
| `scripts/` | Certificate generation and repository checks |
| `docs/` | API, development, and roadmap docs |

## Running the Stack

### Development (plaintext, no auth)

```bash
docker compose --profile dev up -d
```

This starts the control plane, Prometheus, and Grafana. All three use the `dev` profile. Grafana uses `admin / admin` in development only.

Verify:

```bash
curl http://localhost:8080/health
curl http://localhost:3000/api/health
curl http://localhost:9090/api/v1/targets
```

### Production (TLS + auth, required secrets)

```bash
cp .env.example .env
# Replace every value. Generate secrets with: openssl rand -hex 32

bash scripts/gen-certs.sh deploy/certs

docker compose \
  -f docker-compose.yml \
  -f docker-compose.prod.yml \
  --profile prod up -d
```

The production overlay refuses to start unless these are set:

- `NMONIT_API_KEY`
- `NMONIT_AGENT_TOKEN`
- `GRAFANA_ADMIN_USER`
- `GRAFANA_ADMIN_PASSWORD`

It also:

- Mounts certs from `deploy/certs/`
- Enables `--require-tls` and `--tls-reload-interval=30s`
- Adds HSTS to REST responses
- Requires the REST API key and agent token
- Scrapes `/metrics` over HTTPS using the API key from a Docker secret
- Has no default Grafana credentials
- Binds Prometheus and Grafana to `127.0.0.1`

Verify:

```bash
curl -sk https://localhost:8080/health
curl -sk -H "X-API-Key: $NMONIT_API_KEY" https://localhost:8080/api/v1/nodes
```

## Running Individual Components

```bash
# Control plane only
(cd control-plane && go run ./cmd/control-plane --bootstrap --log-level=debug)

# Control plane with TLS
(cd control-plane && go run ./cmd/control-plane \
  --bootstrap \
  --tls-cert=../deploy/certs/server.crt \
  --tls-key=../deploy/certs/server.key \
  --tls-ca-cert=../deploy/certs/ca.crt \
  --require-tls)

# Agent (requires a running control plane)
cargo run -p compute-agent -- \
  --control-plane http://localhost:9000 \
  --mock-gpu-count 2
```

## Generating TLS Certificates

```bash
bash scripts/gen-certs.sh [output_dir]
```

Creates:

- `ca.crt` / `ca.key` — self-signed Certificate Authority
- `server.crt` / `server.key` — control-plane server cert (CN=control-plane, SANs: control-plane, localhost, 127.0.0.1, ::1)
- `agent.crt` / `agent.key` — agent client cert for mTLS

Certificates are 4096-bit RSA and valid for 10 years.

## Running Tests

```bash
make test
```

Or individually:

```bash
cd control-plane && go test -race -count=1 ./...
cd cli && go test -race -count=1 ./...
cargo test -p compute-agent
```

Lints and formatting:

```bash
cargo clippy -p compute-agent --all-targets -- -D warnings
cargo fmt -p compute-agent -- --check
```

## Building Docker Images

```bash
docker build -f deploy/docker/Dockerfile.control-plane -t nmonit-control-plane .
docker build -f deploy/docker/Dockerfile.cli -t nmonit-cli .
docker build -f deploy/docker/Dockerfile.agent -t nmonit-agent .
```

## Monitoring

The development stack includes:

- **Prometheus** — `http://localhost:9090`
- **Grafana** — `http://localhost:3000` (`admin / admin`, development only)

The **NMonit — Compute Control Plane** dashboard covers registered nodes, heartbeats, gRPC rates/latency/streams, node lifecycle, and TLS certificate reloads.

In production, Prometheus scrapes over HTTPS and authenticates with the API key; Grafana credentials come from `.env`.

### TLS Cert Reload

Certificates can be reloaded without restart:

- **Polling** — `--tls-reload-interval=30s` in the production overlay
- **SIGHUP** — `kill -HUP <pid>` triggers an immediate reload

On reload failure, the previous cert continues serving. The `nmonit_tls_cert_load_total` metric tracks success/failure.

## Useful Commands

```bash
# View logs (dev)
docker compose --profile dev logs -f control-plane

# View logs (prod)
docker compose -f docker-compose.yml -f docker-compose.prod.yml --profile prod logs -f control-plane-prod

# Rebuild and restart after code changes
docker compose --profile dev up -d --build

# Tear down
docker compose down

# Tear down and remove volumes
docker compose down -v

# Check metric values (dev)
curl http://localhost:8080/metrics | grep nmonit_
```

## Documentation

- [Architecture](ARCHITECTURE.md)
- [API Reference](docs/API.md)
- [Development Guide](docs/DEVELOPMENT.md)
- [Roadmap](docs/ROADMAP.md)

## .dockerignore

The `.dockerignore` excludes source directories that the control-plane and CLI images do not need. If you add a directory required at build time, update the whitelist.
