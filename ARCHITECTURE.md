# Distributed Compute Fabric — Architecture & Technology Decisions

> **Status:** Decisions documented 2026-06-08
> **Based on:** PRD v1 (Draft)
>
> **About this document:** This document describes the **target** architecture.
> Items not yet implemented are present in the aspirational (planned or
> in-flight) but have not landed in code — see `CHANGELOG.md` (especially the `## [Unreleased]` section) and `git log --oneline` for
> the actual delivered surface.
> 
>
> **Phase legend:**
>
> - **`[Phase 1]`** — shipping today: handler / route is registered and callable end-to-end (a deferred effect inside the handler is called out explicitly, e.g. "RDMA registration pending" or "dispatch TODO").
> - **`[Phase 1 stub]`** — handler exists on a registered gRPC service or REST route, accepts the request, and returns ACK / success; the actual effect (real RDMA, task dispatch, scheduler placement, etc.) is deferred to a later phase.
> - **`[Phase 2+]`** — described in proto (`proto/compute/v1/*.proto`), but no service is registered on the control-plane daemon, OR the referenced backing subsystem (scheduler, SDK, Raft, mDNS, FUSE, RDMA transport) is not yet implemented.

---

## Technology Stack Decisions

### Control Plane: **Go**

| Criterion | Decision | Rationale |
|---|---|---|
| Language | Go 1.22+ | Excellent concurrency model (goroutines) for scheduling; fast compilation; strong standard library for HTTP/gRPC; single static binary deployment |
| RPC Framework | gRPC + Protobuf | Industry standard; code generation for Go + Rust; streaming support for health checks and logs |
| Consensus | hashicorp/raft (built-in) | Single binary — no external etcd cluster to manage; sufficient for LAN-scale (3–5 control plane nodes); boltdb storage backend |
| HTTP API | chi router + gRPC-gateway | Lightweight; gRPC-gateway auto-generates REST from proto; single port serves both |
| Metrics | Prometheus client_golang | Standard; integrates with dashboard |
| Logging | zerolog | Zero-allocation structured logging |

### Node Agent: **Rust**

| Criterion | Decision | Rationale |
|---|---|---|
| Language | Rust (stable) | Minimal resource footprint (~5–10 MB binary); memory safety without GC; direct syscall access for hardware probing; ideal for latency-sensitive data path |
| Async Runtime | tokio | Dominant Rust async runtime; excellent RDMA/networking support |
| RDMA Transport | UCX (via `ucx-rs` bindings, or custom FFI) | Mature, multi-transport (RoCE v2, InfiniBand, TCP fallback); actively maintained by NVIDIA/Mellanox |
| TCP Fallback | tokio + zero-copy sendfile | When RDMA hardware unavailable |
| GPU Probing | nvml-wrapper (NVIDIA NVML) + custom sysfs parsing for AMD | NVML is the standard NVIDIA management library |
| Memory Management | `rdma-shim` crate (custom) + hugepages | Page-level remote memory access |
| Serialization | prost (Protobuf) | Fast, generates Rust types from same `.proto` files |

### Networking Fabric

| Criterion | Decision | Rationale |
|---|---|---|
| Primary Transport | RoCE v2 (RDMA over Converged Ethernet) | Works on standard Ethernet switches with PFC/ECN; no InfiniBand hardware required |
| Fallback | TCP with `SO_ZEROCOPY` and `sendfile` | Transparent fallback when RDMA unavailable |
| RDMA Library | UCX (Unified Communication X) | Abstracts RoCE/IB/TCP under single API; NVIDIA-supported; active community |
| Kernel Bypass | UCX busy-polling mode for latency-critical; interrupt-driven for background | Configurable per traffic class |
| Overlay | VXLAN (kernel native) for multi-tenant isolation | Simple, hardware-offloaded on many NICs |

### Storage Layer

| Criterion | Decision | Rationale |
|---|---|---|
| Phase 1 | Custom lightweight distributed FS | Tight integration with memory fabric; no external dependency |
| POSIX Interface | FUSE daemon (Rust `fuser` crate) | Existing apps work without modification |
| S3 Interface | MinIO-compatible API gateway (Go) | Compatibility with S3 ecosystem |
| Metadata | Embedded boltdb per storage node + Raft consensus | Consistent, fast, no external DB |
| Data Replication | Primary-backup with configurable sync/async | Simpler than quorum writes; tunable consistency |
| Erasure Coding | Reed-Solomon (optional, phase 2+) | Storage efficiency for cold data |
| Tiering | Custom promotion/demotion daemon | GPU VRAM ↔ RAM ↔ NVMe ↔ SSD |

### GPU Abstraction

| Criterion | Decision | Rationale |
|---|---|---|
| Primary Backend | CUDA (NVIDIA) | Dominant in ML/AI; best tooling |
| API Layer | Custom `gpu-pool` crate with backend trait | Swap backends without changing application code |
| Future Backends | ROCm (AMD), oneAPI Level Zero (Intel), Vulkan Compute | Vendor-neutral path |
| Peer-to-Peer | GPUDirect RDMA via UCX | Zero-copy GPU-to-GPU across nodes |
| Compute | CUDA streams + MPS (Multi-Process Service) for sharing | Time-slicing and concurrency |
| ML Integration | PyTorch custom backend + `torch.distributed` integration | Most-used ML framework |

### Container Runtime

| Criterion | Decision | Rationale |
|---|---|---|
| Phase 1 | containerd + custom runtime hook (OCI hook) | Direct containerd integration; no Kubernetes dependency |
| Image Format | OCI images (Docker-compatible) | Standard; works with existing registries |
| Resource Injection | OCI prestart hook injects fabric devices and env vars | Transparent to container |
| Phase 2 | Kubernetes device plugin | For K8s-native deployments |

### Build & Infrastructure

| Criterion | Decision | Rationale |
|---|---|---|
| Build System | Make (top-level) + Cargo (Rust) + Go toolchain | Simple, universal, no new tool to learn |
| Proto Codegen | buf | Modern protobuf tooling; linting + breaking change detection |
| CI | GitHub Actions | Standard; free for public repos |
| Container Images | Docker multi-stage builds | Minimal final images |
| Package Registry | crates.io (Rust) + Go modules | Standard registries |

---

## Component Interfaces

### Agent ↔ Control Plane (gRPC) — `[Phase 1]`, with stub handlers called out

> - **`[Phase 1]` (callable, end-to-end):** `Register`, `Heartbeat`.
> - **`[Phase 1 stub]` (callable, returns ACK; RDMA / dispatch pending):** `ExecuteTask`, `AllocateMemory`, `AllocateGPUMemory`, plus proto siblings `FreeMemory`, `FreeGPUMemory`. The handlers are registered on the gRPC server (`control-plane/internal/agent/agent.go`) and acknowledge every request, but the actual memory-region registration, task dispatch, and GPU-memory wiring is deferred — see the in-code TODO comments.

```protobuf
service AgentService {
  // Agent registers with control plane on startup
  rpc Register(RegisterRequest) returns (RegisterResponse);

  // Heartbeat stream: agent sends periodic health + resource updates
  rpc Heartbeat(stream HeartbeatRequest) returns (stream HeartbeatResponse);

  // Control plane instructs agent to execute a task
  rpc ExecuteTask(TaskSpec) returns (TaskAck);

  // Control plane requests memory allocation on this node
  rpc AllocateMemory(MemoryAllocRequest) returns (MemoryAllocResponse);

  // Control plane requests GPU memory allocation  rpc AllocateGPUMemory(GPUMemoryAllocRequest) returns (GPUMemoryAllocResponse);
}
```
### Other gRPC Services (proto-defined; not currently registered) — `[Phase 2+]`

> Beyond `AgentService` (callable end-to-end today — see its preamble above) and the REST surface wired to `/api/v1/nodes` + `/health` + `/metrics`, four more gRPC services are fully defined in `proto/compute/v1/{job,memory,storage,control}.proto`. None of their 14 RPCs are currently registered, so callers can't reach them end-to-end. Readers searching for `MemoryService.AllocateDistributed` should note it is *conceptually distinct* from `AgentService.AllocateMemory`: the agent-side RPC currently returns a stub `MemoryHandle` (see the Distributed Memory Allocation data flow); a real cluster-wide allocation via `MemoryService.AllocateDistributed` does not yet exist. REST endpoints above cover a subset of these operations; this paragraph is the canonical "what proto contracts exist but aren't wired" pointer.
>
> - **`[Phase 2+]` `JobService`** (`proto/compute/v1/job.proto`): `SubmitJob`, `GetJob`, `ListJobs`, `CancelJob`, `StreamLogs`, `GetJobMetrics`.
> - **`[Phase 2+]` `MemoryService`** (`proto/compute/v1/memory.proto`): `AllocateDistributed`, `MigrateRegion`, `GetRegion`.
> - **`[Phase 2+]` `StorageService`** (`proto/compute/v1/storage.proto`): `CreateDataset`, `ListDatasets`, `GetDatasetStatus`.
> - **`[Phase 2+]` `ControlService`** (`proto/compute/v1/control.proto`): `GetClusterState`, `JoinCluster`.

### Client API (REST + gRPC)

> Phase coverage reflects what `control-plane/internal/restapi/` actually serves today (only `/health`, `/api/v1/nodes`, `/metrics`). Routes under `/api/v1/jobs`, `/api/v1/resources`, `/api/v1/allocations` are part of the `JobService` and `MemoryService` proto definitions (`proto/compute/v1/{job,memory}.proto`), but neither service is currently registered (so they're not callable end-to-end even though the contract exists).

```
POST   /api/v1/jobs              # Submit job                       [Phase 2+]
GET    /api/v1/jobs/{id}         # Job status                       [Phase 2+]
DELETE /api/v1/jobs/{id}         # Cancel job                       [Phase 2+]
GET    /api/v1/jobs/{id}/logs    # Stream logs (SSE)                [Phase 2+]
GET    /api/v1/resources         # Cluster resource state           [Phase 2+]
GET    /api/v1/nodes             # Per-node info                    [Phase 1]
POST   /api/v1/allocations       # Distributed memory allocation    [Phase 2+]
GET    /api/v1/allocations       # List allocations                 [Phase 2+]
DELETE /api/v1/allocations/{id}  # Free allocation                  [Phase 2+]
```

### SDK API (C, exposed to Python/Go/Rust bindings) — `[Phase 2+]`

> - **No SDK implementations exist** today: the `sdk/{c,python,go}/` directories contain only `README.md` per language.
> - The C ABI below is design intent. Bindings will materialize once the control-plane endpoints and agent capabilities behind them ship.

```c
// Memory
void* distributed_malloc(size_t size, consistency_mode_t mode);
void  distributed_free(void* ptr);

// GPU
void* gpu_malloc(size_t size);
void  gpu_free(void* ptr);

// Communication
int   fabric_send(node_id_t node, const void* buf, size_t len);
int   fabric_recv(node_id_t node, void* buf, size_t len);
int   fabric_broadcast(const void* buf, size_t len);
int   fabric_reduce(const void* buf, size_t len, reduce_op_t op);
int   fabric_barrier(void);

// Async
int   fabric_submit(fabric_op_t* ops, size_t count, completion_queue_t* cq);
int   fabric_poll(completion_queue_t* cq, fabric_event_t* events, size_t max_events);

// Checkpoint
int   fabric_checkpoint(void);
int   fabric_restore(checkpoint_id_t id);
```

---

## Data Flow: Job Submission

> - **`[Phase 2+]`** step 1 — REST `POST /api/v1/jobs` submit gate. The route is proto-defined (`JobService`) but neither that service nor a REST handler is currently wired.
> - **`[Phase 2+]`** Scheduler evaluates resource requirements / affinity / current load — no scheduler package exists in `control-plane/`.
> - **`[Phase 2+]`** Scheduler selects target node(s).
> - **`[Phase 1 stub]`** Control plane sends `ExecuteTask` RPC to agent. Handler accepts and ACKs requests (`control-plane/internal/agent/agent.go`) — actual task dispatch is deferred (in-code TODO comment).
> - **`[Phase 2+]`** Agent pulls container image, starts task, sets up fabric devices. The agent's executor module was removed in the Phase 1 cleanup.
> - **`[Phase 1]`** Agent streams logs + metrics back to control plane via the existing heartbeat stream (resource updates).
> - **`[Phase 2+]`** Control plane updates job status. `registry.NodeInfo.Tasks` holds per-node task state; the cross-node `Job` aggregate is not persisted yet.
> - **`[Phase 2+]`** User polls `GET /api/v1/jobs/{id}` or streams logs.

```
User/CLI → REST API (control plane)
  → Scheduler evaluates: resource requirements, affinity, current load
  → Scheduler selects target node(s)
  → Control plane sends ExecuteTask RPC to Agent(s)
  → Agent pulls container image, starts task, sets up fabric devices
  → Agent streams logs + metrics back to control plane
  → Control plane updates job status
  → User polls GET /jobs/{id} or streams logs
```

## Data Flow: Distributed Memory Allocation

> - **`[Phase 2+]`** `distributed_malloc` — no SDK implementations yet (planned for C/Python/Go/Rust bindings).
> - **`[Phase 2+]`** SDK POSTs to `/api/v1/allocations` — the route is proto-defined (`MemoryService`) but no REST handler is wired.
> - **`[Phase 1]`** Control plane checks resource map for free RAM — `registry.NodeRegistry` already tracks `NodeResources` accuracy today.
> - **`[Phase 1 stub]`** Control plane sends `AgentService.AllocateMemory`; server returns a `MemoryHandle` for forward-compat — actual RDMA registration on the agent is deferred.
> - **`[Phase 2+]`** Agent allocates hugepages, registers RDMA memory region. The `network/` transport crate is currently the cargo-new default only.
> - **`[Phase 1 stub]`** Agent returns `MemoryHandle` (remote_key, address) — same stub path: identifiers returned before real NIC-side registration lands.
> - **`[Phase 2+]`** Control plane returns handle to SDK.
> - **`[Phase 2+]`** SDK maps remote memory region locally using RDMA.
> - **`[Phase 2+]`** App reads/writes via RDMA to the remote node.

```
App calls distributed_malloc(1GB)
  → SDK contacts control plane: POST /allocations {size: 1GB, consistency: RELAXED}
  → Control plane checks resource map for nodes with free RAM
  → Control plane chooses node(s), sends AllocateMemory RPC
  → Agent allocates hugepages, registers memory region with RDMA NIC
  → Agent returns memory handle (remote key, address) to control plane
  → Control plane returns handle to SDK
  → SDK maps remote memory region locally using RDMA
  → App reads/writes → RDMA read/write to remote node
```

---

## Cluster Bootstrap Flow

> - **Step 1a:** first control-plane node starts (single-node serving). The daemon listens on `--grpc-addr` + `--http-addr` and exposes `/health`, `/api/v1/nodes`, Prometheus `/metrics`, plus `AgentService.Register` / `Heartbeat`. `[Phase 1]` — works today without any Raft being touched.
> - **Step 1b:** Raft log replay + leadership-election init. Needs to run before any other node joins. `[Phase 2+]` — no `hashicorp/raft` import.
> - **Step 2:** additional control-plane nodes join via Raft membership update. `[Phase 2+]`. (`ControlService.JoinCluster` is proto-defined but not registered.)
> - **Step 3a:** `--control-plane` CLI flag discovery. Each agent unicast-connects to the given gRPC address. `[Phase 1]`.
> - **Step 3b:** mDNS auto-discovery of the control plane over LAN. `[Phase 2+]` (`agent/src/discovery.rs` was removed in Phase 1 cleanup).
> - **Step 4:** agent registers with control plane and advertises hardware resources — `[Phase 1]` (`AgentService.Register` → `registry.Register` end-to-end).
> - **Step 5:** control plane's resource map is updated in registry — `[Phase 1]`.
> - **Step 6:** cluster ready — `[Phase 1]`.

```
1. First control plane node starts → initializes Raft cluster (single node)
2. Additional control plane nodes join → Raft membership update
3. Node agents start → discover control plane via mDNS or static config
4. Agent registers with control plane → advertises hardware resources
5. Control plane updates resource map
6. Cluster ready
```

---

## Directory Structure

```
nmonit/
├── README.md
├── ARCHITECTURE.md          # This file
├── Makefile                 # Top-level build orchestration
├── proto/                   # Protobuf definitions (single source of truth)
│   ├── buf.yaml
│   ├── buf.gen.yaml
│   └── compute/
│       └── v1/
│           ├── agent.proto      # Agent ↔ Control plane
│           ├── control.proto    # Inter-control-plane (Raft, consensus)
│           ├── resource.proto   # Resource types, topology
│           ├── job.proto        # Job submission, status
│           ├── memory.proto     # Distributed memory types
│           └── storage.proto    # Storage layer types
├── agent/                   # Rust — node agent
│   ├── Cargo.toml
│   ├── build.rs             # Tonic-prost protobuf codegen hook
│   └── src/
│       ├── main.rs          # CLI entrypoint + connection lifecycle
│       ├── heartbeat.rs     # gRPC heartbeat stream + reconnect backoff
│       ├── resources.rs     # Hardware probing (CPU, RAM, GPU)
│       └── gpu.rs           # GPU management (NVML)
├── control-plane/           # Go — control-plane daemon
│   ├── go.mod
│   ├── go.sum
│   ├── cmd/
│   │   └── control-plane/
│   │       ├── main.go              # gRPC + HTTP server wiring
│   │       └── interceptor_test.go  # Interceptor-chain invariants
│   ├── gen/                 # Protobuf + gRPC generated code (buf-generated; do not edit)
│   │   └── compute/
│   │       └── v1/
│   └── internal/
│       ├── agent/           # gRPC AgentService handlers + auth + validation
│       ├── registry/        # Node state, heartbeat accounting, stale cleanup
│       ├── restapi/         # HTTP handlers (/health, /api/v1/nodes, /metrics)
│       ├── validator/       # Per-field input validation rules
│       ├── metrics/         # Prometheus collectors + interceptors
│       └── tlsreload/       # Hot-reload TLS certificate reloader
├── sdk/                     # Language SDKs (planned; only README.md per language today)
│   ├── c/                   # C bindings (planned)
│   │   └── README.md
│   ├── python/              # Python bindings via pyo3 or ctypes (planned)
│   │   └── README.md
│   └── go/                  # Go client library (planned)
│       └── README.md
├── cli/                     # CLI tool (Go — placeholder; main.go/cmd/ not yet written)
│   ├── go.mod
│   └── go.sum
├── dashboard/               # Web dashboard (future — React/TypeScript)
│   └── README.md
├── storage/                 # Rust crate `compute-storage`; lib.rs is the cargo-new default
│   │                        # (description promises "FUSE + S3-compatible API"; real modules pending)
│   ├── Cargo.toml
│   └── src/
│       └── lib.rs
├── network/                 # Rust crate `compute-network`; lib.rs is the cargo-new default
│   │                        # (description promises "RDMA/TCP transport layer"; real modules pending)
│   ├── Cargo.toml
│   └── src/
│       └── lib.rs
├── scripts/                 # Repo-local lint / utility scripts
│   ├── check-dead-symbols.sh        # Guards against removed-symbol reintroduction
│   └── dead-symbols.json            # Catalog of removed symbols + per-entry allow_paths
├── deploy/                  # Deployment configurations
│   ├── docker/
│   │   ├── Dockerfile.agent
│   │   ├── Dockerfile.cli
│   │   └── Dockerfile.control-plane
│   ├── systemd/
│   │   ├── compute-agent.service
│   │   └── compute-control-plane.service
│   ├── grafana/
│   │   ├── nmonit-dashboard.json
│   │   ├── dashboards/nmonit.yml
│   │   └── datasources/prometheus.yml
│   └── prometheus/
│       └── prometheus.yml
└── docs/                    # Public docs directory (planned; present but empty)
```

---

## Key Design Principles

1. **Single binary where possible.** The agent is one Rust binary. The control plane is one Go binary. No sidecars.
2. **Protobuf is the source of truth.** All interfaces defined in `.proto` files. Code generated for Go, Rust, Python.
3. **Graceful degradation.** RDMA unavailable → TCP. GPU unavailable → CPU fallback. Node fails → restart elsewhere.
4. **Zero-copy everywhere.** RDMA for network. `sendfile` for storage. Shared memory for local IPC.
5. **Observability built-in.** Prometheus metrics, structured logs, distributed tracing from day one.
6. **Security by default.** mTLS between all components. No plaintext anywhere. Least-privilege agent design.
