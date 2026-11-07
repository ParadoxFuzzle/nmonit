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

### Added

- **GitHub Actions CI** — `.github/workflows/ci.yml` now runs Go race tests and vet, builds the CLI, runs the Rust tests, and runs `clippy -D warnings`.
- **`compute` CLI** — `status`, `health`, and `version` commands with API-key auth, optional CA certificate, JSON output, and a node utilization table. Buildable with `make cli` and `deploy/docker/Dockerfile.cli`.
- **Live node utilization in REST** — `/api/v1/nodes` now includes a `utilization` object with CPU load/percentages, memory usage, and GPU/VRAM/temperature/power/clock data from heartbeats.

### Fixed

- **Heartbeat stream never opened** — the agent awaited the bidirectional `Heartbeat` call before sending its first message, while the server waited for that message before returning response headers. The stream timed out after 10s and no heartbeats were delivered. The initial request is now queued before opening the stream.
- **mTLS panicked during startup** — `tonic` was compiled with native roots but no rustls crypto provider. Enabled the `tls-ring` feature so the agent can install a provider.
- **Control-plane crash on nil registration resources** — `Register` now rejects a nil `NodeResources` with `InvalidArgument` before it reaches the registry.
- **Heartbeat streams could outlive registration state** — unregistered nodes, mismatched node IDs, and stale connection IDs are now rejected, and a removed node receives `NotFound` instead of silently accepting updates.
- **TLS hot-reload race and missing ALPN** — reload work is serialized, and `GetConfigForClient` advertises `h2` so gRPC handshakes continue to work after reload.
- **SIGTERM could hang on active heartbeat streams** — gRPC graceful shutdown is bounded to 5s, then forced to stop.
- **Development certificates failed hostname verification** — generated server certs now carry SANs for `control-plane`, `localhost`, `127.0.0.1`, and `::1`, plus server/client EKU and CA constraints.
- **Agent could panic on a poisoned resource mutex** — `discover_resources` now recovers the poisoned guard like `collect_utilization` already did.
- **Agent Docker build used Rust 1.78** — bumped to Rust 1.95, matching edition 2024 and the `sysinfo` MSRV.
- **CLI silently ignored `--ca-cert` over HTTP** — it now rejects a CA certificate unless the server URL is `https://`, preventing an API key from being sent in plaintext while TLS was expected.
- **CLI hid GPU utilization without static GPU metadata** — utilization rows are now shown even when the resources list is empty.
- **Heartbeat handler left nodes registered on early returns** — cleanup now runs on every stream exit path, while connection-ID matching protects re-registered nodes.
- **A wedged heartbeat stream could block the agent loop** — sends are bounded by the heartbeat interval, and missed ticks are skipped instead of bursting afterward.
- **REST summaries panicked on nil GPU entries** — nil repeated-message entries are skipped in both static and utilization mappings.
- **Stale cleanup logged while holding the registry write lock** — logging now happens after unlock so a slow log sink cannot stall heartbeats.

### Changed

- **CI now cancels superseded runs and has per-job timeouts** — Rust formatting is checked and clippy covers all targets.
- **Docker Compose dev profile documented correctly** — the development control plane requires `--profile dev`; plain `docker compose up` only starts monitoring.
- **Proto Go package paths and Cargo repository now match the real module** — `go_package`/`go_package_prefix` point at `github.com/ParadoxFuzzle/control-plane`, and the workspace repository points at `ParadoxFuzzle/nmonit`.
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
