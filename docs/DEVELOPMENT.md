# Development Guide

## Prerequisites

- Go 1.25+
- Rust 1.95+
- Docker + Docker Compose (optional)
- OpenSSL (development certificates)
- buf (only when changing protobuf definitions)

## Build

```bash
make                 # agent, control plane, CLI
make agent           # Rust agent only
make control-plane   # Go control plane only
make cli             # Go CLI only
```

Generated Go protobuf code is checked in under `control-plane/gen/`; the Rust build generates its own bindings through `agent/build.rs`. Run `make proto` after changing a `.proto` file.

## Test and lint

```bash
make test
make lint
```

`make test` runs:

```bash
cd control-plane && go test -race -count=1 ./...
cd cli && go test -race -count=1 ./...
cargo test -p compute-agent
```

## Local run

```bash
./bin/control-plane --bootstrap --api-key dev-key --agent-token dev-agent-token
./target/release/compute-agent --control-plane http://localhost:9000 --agent-token dev-agent-token
./bin/compute --server http://localhost:8080 --api-key dev-key status
```

## Development Compose stack

```bash
docker compose --profile dev up -d
```

Services:

- control plane: `http://localhost:8080`
- Prometheus: `http://localhost:9090`
- Grafana: `http://localhost:3000` (`admin / admin`, development only)

The development profile is plaintext and unauthenticated.

## Production Compose overlay

```bash
cp .env.example .env
# Replace NMONIT_API_KEY, NMONIT_AGENT_TOKEN, GRAFANA_ADMIN_USER,
# and GRAFANA_ADMIN_PASSWORD with non-placeholder values.
openssl rand -hex 32   # use for the API key and agent token

bash scripts/gen-certs.sh deploy/certs

docker compose \
  -f docker-compose.yml \
  -f docker-compose.prod.yml \
  --profile prod up -d
```

The overlay:

- Requires all four values above; Compose refuses to start if any are missing.
- Enables TLS and `--require-tls`.
- Requires the REST API key and agent token.
- Scrapes metrics over HTTPS with the API key supplied as a Docker secret.
- Requires explicit Grafana admin credentials; there is no default password.
- Binds Prometheus and Grafana to `127.0.0.1`.

## Certificates

```bash
bash scripts/gen-certs.sh [output_dir]
```

The default output is `deploy/certs/`. The script creates a CA, a control-plane server certificate, and an agent client certificate for mTLS. Production deployments should use certificates from their own CA rather than the development script.

## CLI environment variables

- `NMONIT_SERVER` — REST base URL
- `NMONIT_API_KEY` — REST API key
- `NMONIT_CA_CERT` — CA certificate for HTTPS

Flags override the environment.

## Useful commands

```bash
# Dev logs
docker compose --profile dev logs -f control-plane

# Prod logs
docker compose -f docker-compose.yml -f docker-compose.prod.yml --profile prod logs -f control-plane-prod

# Rebuild dev stack
docker compose --profile dev up -d --build

# Tear down
docker compose -f docker-compose.yml -f docker-compose.prod.yml --profile prod down

# Check metrics
curl -H "X-API-Key: $NMONIT_API_KEY" -k https://localhost:8080/metrics
```
