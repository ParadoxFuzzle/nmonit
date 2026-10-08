# Changelog

## [Unreleased] — Public-access Phase 2 (job submission flow)

### Added
- **Job submission layer** (`internal/jobs`): PostgreSQL-backed job store
  (jsonb `command`/`env` columns; `NULL user_id` for owner submissions),
  job states (`queued → scheduling → running → completed/failed/cancelled`),
  and a best-fit scheduler that assigns queued jobs to nodes with enough
  free cpu/mem, dispatching tasks over the existing heartbeat stream.
- **Three consumer quotas**, all enforced at submit time: max running
  (scheduling+running) jobs, max queued jobs, and max per-job resources
  (cores/memory). Per-user overrides via `users.max_jobs_running` /
  `users.max_jobs_queued`; owner/bootstrap submissions bypass quotas.
  Defaults: `--max-jobs-running 2`, `--max-jobs-queued 5`,
  `--max-job-cores 4`, `--max-job-mem-gb 8`.
- **REST jobs API**: `POST/GET /api/v1/jobs` (submit/list) and
  `GET/DELETE /api/v1/jobs/{id}` (inspect/cancel), auth'd the same way as
  the identity layer — owner API key (full access, no quotas) or a
  consumer-role `nmt_` Bearer token (own jobs only, quotas enforced);
  provider-role tokens are rejected.
- **gRPC JobService** (`compute.v1.JobService`): Submit/Get/List/Cancel
  with the same role model, served by the control plane alongside the
  agent service.
- **`nmonitctl job`** subcommands: `submit --name --image [--cpu]
  [--mem-gb]`, `list`, `get`, `cancel` (owner path via API key).
- **Heartbeat dispatch wiring**: control plane drains pending tasks and
  cancels to each node on heartbeat ticks and on job submission; agent
  instruction handling already consumes `pending_tasks`/`cancel_task_ids`.

### Fixed
- `nmonitctl` subcommand flags placed after positional arguments were
  silently ignored by Go's `flag` package (`user add <email> --role
  consumer` created providers); flag-first reordering now applied to
  `user add`, `token issue`, `invite create`, and `job submit`.
- Job store operations log their underlying error instead of returning
  opaque HTTP 500s.

### Security
- Consumer-role tokens cannot register nodes (gRPC Register returns
  PermissionDenied); provider-role tokens cannot use the job API.
- Job ownership enforced on get/cancel: consumers receive 403 for jobs
  they do not own.

> Note: agents currently log dispatched tasks but do not yet execute
> containers — jobs reach `scheduling` and remain there until agent-side
> execution lands (planned follow-up).

---

## [Unreleased] — Public-access Phase 1 (identity layer)

### Added
- **Per-contributor identity layer** (`internal/identity`): users with roles
  (owner / provider / consumer), per-node tokens (`nmt_...`, stored as sha256
  hashes — plaintext shown once at issue), invite codes, node-to-user
  bindings, and a web access-request flow.
- **`internal/audit`**: append-only audit log (Postgres) recording joins,
  leaves, auth failures, token lifecycle, and admin actions; `nmonitctl
  audit tail` to read it.
- **Admin REST API** (owner API key gated): `/api/v1/admin/users`, `/tokens`,
  `/invites`, `/requests`, `/audit`.
- **Public onboarding UI** at `GET/POST /join`: email + optional invite code;
  requests land as pending for operator approval.
- **`nmonitctl`** CLI: user/token/invite/request/audit management against the
  REST API (supports `--ca` for self-signed control planes).
- **`--pg-dsn` flag** on control-plane: enables the identity layer; empty
  (default) preserves the legacy shared-token trusted-LAN mode unchanged.

### Changed
- Agent `Register` now binds the node to the authenticated contributor user
  when a per-node token is used (bootstrap-token registrations are unbound).
- gRPC auth resolves credentials via `agent.Authenticator`: shared bootstrap
  token first, then per-node `nmt_` tokens against the identity store.
  Disabled/pending users are rejected; `last_used_at` is updated per use.

### Security
- Node tokens stored only as sha256 hashes; invites stored hashed.
- Join flow rejects invalid/expired/exhausted invite codes (audit-logged).
- Auth failures audit-logged with outcome "denied".

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

---

## [0.3.0] — 2026-06-22

### Highlights

- **Agent stub modules removed (Phase 2+ deferred work)** — five placeholder modules (`discovery`, `executor`, `memory`, `metrics`, `network`) deleted along with their `Cargo.toml` dependencies (`tower`, `serde`/`serde_json`, `nix`, `tracing-appender`, `metrics-exporter-prometheus`, `uuid`, `tempfile`).
- **Control-plane split into seven cohesive internal packages** — replaces the 641-line monolithic `cmd/control-plane/main.go` with dedicated `internal/{registry,agent,restapi,validator,tlsreload,metrics}` packages, each with focused responsibility and per-package tests.
- **CI tooling landed** — `golangci-lint` config + custom `scripts/check-dead-symbols.sh` verification wired into `make lint`; blocks reintroduction of removed identifiers.
- **Documentation aligned with the native `ParadoxFuzzle/nmonit` namespace** — systemd units, README, CONTRIBUTING, start-cluster script, and Go module path rename.
- **ARCHITECTURE.md tightened** — descriptive `today` removed from three Phase-1 surface claims where it sat close to slim `currently`-register claims; phase tags now carry the temporal register.

### Fixed

- **`ClusterId` wire format: hand-rolled v7 → real v4 UUID** — control-plane's prefix `cluster-<id>` is now produced by `github.com/google/uuid.NewString()` (v4). The old hand-rolled `uuid7()` bit layout placed a `7` at `ClusterId[14]` (the UUID version nibble); v4 places a `4` there. Any client that parses the version digit at index 14 to detect UUID variant should be updated accordingly. Same-millisecond ClusterId collisions (the documented bug the v7 helper fell back on) are eliminated.

### Changed

- **Module path renamed**: `github.com/compute-nmonit/control-plane` → `github.com/ParadoxFuzzle/control-plane` to match the git remote's native namespace. Out-of-scope (proto `go_package`, `buf.gen.yaml`, regenerated `.pb.go`s) deferred to a follow-up `chore(proto): align go_package` commit.

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
