# Changelog

All notable changes to the NMonit project.

---

## [0.2.0] — 2026-06-09

### Security

- **TLS/mTLS for gRPC server and agent** — all traffic encrypted; `--tls-cert`, `--tls-key`, `--tls-ca-cert` flags on both sides; TLS 1.3 enforced; mTLS when CA cert is provided
- **REST API TLS** — HTTP server uses same certs as gRPC when TLS is configured; `Strict-Transport-Security` (HSTS) header sent on all responses with 1-year max-age
- **`--require-tls` flag** — refuses startup unless TLS credentials are configured (production safety)
- **Timing-attack-resistant token comparison** — gRPC bearer token and REST API key use `crypto/subtle.ConstantTimeCompare`
- **Security headers on all REST responses** — `X-Content-Type-Options: nosniff`, `X-Frame-Options: DENY`, `Referrer-Policy: strict-origin-when-cross-origin`
- **HTTP server timeouts** — `ReadHeaderTimeout`, `ReadTimeout`, `WriteTimeout`, `IdleTimeout` prevent Slowloris and resource exhaustion
- **Exposed `/metrics` endpoint now requires API key auth** — wrapped with `requireAPIKey` middleware
- **Input validation on all 7 RPC handlers** — node/task/job/allocation IDs, container image format, memory size bounds, env vars, commands, heartbeat task count; null byte detection; `internal/validator/` package
- **Audit logging for rejected inputs** — logs remote IP, method, and field on every validation failure

### Fixed

- **Unbounded metric cardinality DoS vector** — gRPC method names validated against known service methods before use as Prometheus labels
- **Mutex poisoning panic loop in agent** — `std::sync::Mutex::lock().expect()` replaced with `unwrap_or_else(|e| e.into_inner())`
- **Unbounded mpsc channel in agent heartbeat** — bounded to 256 to prevent OOM
- **Dead code in TLS flag validation** — restructured to three-way switch covering all cases (both empty / one empty / both set)

### Added

- **Certificate hot-reload** — `internal/tlsreload/` package with `CertReloader`; polling-based file watcher (`--tls-reload-interval`); `SIGHUP` triggers immediate reload; old cert continues serving on failure — zero-downtime rotation
- **Prometheus metric `nmonit_tls_cert_load_total`** — labeled by `result` (success/failure), tracks all cert load attempts
- **Grafana dashboard** — `deploy/grafana/nmonit-dashboard.json` covering all metrics: nodes, tasks, heartbeats, gRPC rates/latency/streams, node lifecycle, TLS cert health
- **Docker Compose dev stack** — Prometheus + Grafana + control-plane; dev profile (plaintext) and prod profile (TLS + mTLS + HSTS); single `docker compose up -d` starts everything
- **Multi-stage Dockerfile** for control-plane — `golang:1.25-alpine` builder, `alpine:3.21` runtime, non-root user, static binary
- **`.dockerignore`** — excludes non-control-plane source from Docker build context
- **`scripts/gen-certs.sh`** — generates self-signed CA, server, and agent certs for development
- **`CONTRIBUTING.md`** — development setup, running the stack, tests, certs, monitoring

### Changed

- **Flattened project layout** — removed extra `nmonit/` nesting; all source at project root
- **Prometheus target** — scrape target changed from `host.docker.internal` to `control-plane:8080` (Docker network)
- **HTTP server startup** — conditionally uses `ListenAndServeTLS` when TLS is configured

---

## [Unreleased]

### Changed

- **`ClusterId` wire format: hand-rolled v7 → real v4 UUID** — control-plane's prefix `cluster-<id>` is now produced by `github.com/google/uuid.NewString()` (v4). The old hand-rolled `uuid7()` bit layout placed a `7` at `ClusterId[14]` (the UUID version nibble); v4 places a `4` there. Any client that parses the version digit at index 14 to detect UUID variant should be updated accordingly. Same-millisecond ClusterId collisions (the documented bug the v7 helper fell back on) are eliminated.

---

## [0.1.0] — Initial

### Core

- Go control-plane with gRPC server (AgentService: Register, Heartbeat, ExecuteTask, AllocateMemory, AllocateGPUMemory, FreeMemory, FreeGPUMemory) and REST API gateway
- Rust compute agent with hardware discovery, heartbeat streaming, and task execution stubs
- Node registry with stale-node cleanup
- Prometheus metrics: nodes, registrations, heartbeats, stale removals, tasks running, gRPC requests/latency/streams
- Shared protobuf definitions (`proto/`)
- SDK scaffolding (Rust, Go, Python, C)
- Systemd service units, Dockerfiles, Makefile

### Monitoring

- `internal/metrics/` package — Prometheus counters, gauges, histograms; gRPC unary/stream interceptors; method name sanitization to prevent cardinality explosion
- `/metrics` HTTP endpoint via `promhttp.Handler()`
- `/health` and `/api/v1/nodes` REST endpoints
