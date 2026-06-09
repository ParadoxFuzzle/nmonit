# Distributed Compute Fabric — Architecture & Technology Decisions

> **Status:** Decisions documented 2026-06-08
> **Based on:** PRD v1 (Draft)

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

### Agent ↔ Control Plane (gRPC)

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

  // Control plane requests GPU memory allocation
  rpc AllocateGPUMemory(GPUMemoryAllocRequest) returns (GPUMemoryAllocResponse);
}
```

### Client API (REST + gRPC)

```
POST   /api/v1/jobs              # Submit job
GET    /api/v1/jobs/{id}         # Job status
DELETE /api/v1/jobs/{id}         # Cancel job
GET    /api/v1/jobs/{id}/logs    # Stream logs (SSE)
GET    /api/v1/resources          # Cluster resource state
GET    /api/v1/nodes             # Per-node info
POST   /api/v1/allocations       # Distributed memory allocation
GET    /api/v1/allocations       # List allocations
DELETE /api/v1/allocations/{id}  # Free allocation
```

### SDK API (C, exposed to Python/Go/Rust bindings)

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
compute-nmonit/
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
│   └── src/
│       ├── main.rs
│       ├── discovery.rs     # mDNS / static peer discovery
│       ├── heartbeat.rs     # Health reporting to control plane
│       ├── resources.rs     # Hardware probing (CPU, RAM, GPU, NIC, storage)
│       ├── gpu.rs           # GPU management (NVML, CUDA)
│       ├── memory.rs        # RDMA memory registration, hugepages
│       ├── network.rs       # UCX/RDMA setup, TCP fallback
│       ├── executor.rs      # Task execution (containerd runtime hook)
│       └── metrics.rs       # Per-node metrics collection
├── control-plane/           # Go — scheduler, API, resource manager
│   ├── go.mod
│   ├── go.sum
│   ├── cmd/
│   │   └── control-plane/
│   │       └── main.go
│   └── internal/
│       ├── api/             # REST + gRPC server
│       │   ├── server.go
│       │   ├── jobs.go
│       │   ├── resources.go
│       │   └── allocations.go
│       ├── scheduler/       # Job scheduler
│       │   ├── scheduler.go
│       │   ├── first_fit.go
│       │   ├── gang.go
│       │   └── affinity.go
│       ├── resources/       # Resource tracking
│       │   ├── manager.go
│       │   ├── node.go
│       │   └── topology.go
│       ├── consensus/       # Raft cluster management
│       │   ├── raft.go
│       │   └── fsm.go       # Finite state machine for Raft
│       ├── health/          # Health monitoring
│       │   └── monitor.go
│       └── auth/            # AuthN/AuthZ (future)
│           └── rbac.go
├── sdk/
│   ├── c/                   # C SDK (libfabric_client)
│   │   ├── Makefile
│   │   ├── include/
│   │   │   └── fabric.h
│   │   └── src/
│   │       ├── malloc.c
│   │       ├── gpu.c
│   │       ├── comms.c
│   │       └── client.c
│   ├── python/              # Python bindings (pyo3 or ctypes)
│   │   ├── pyproject.toml
│   │   └── src/
│   │       └── compute_fabric/
│   │           ├── __init__.py
│   │           ├── array.py
│   │           └── gpu.py
│   └── go/                  # Go SDK (client library)
│       ├── go.mod
│       └── fabric/
│           ├── client.go
│           ├── memory.go
│           └── gpu.go
├── cli/                     # CLI tool (Go)
│   ├── go.mod
│   ├── main.go
│   └── cmd/
│       ├── submit.go
│       ├── status.go
│       ├── nodes.go
│       ├── logs.go
│       └── alloc.go
├── dashboard/               # Web dashboard (future — React/TypeScript)
│   └── README.md
├── storage/                 # Distributed storage (Rust)
│   ├── Cargo.toml
│   └── src/
│       ├── lib.rs
│       ├── fs.rs            # FUSE filesystem
│       ├── s3.rs            # S3-compatible API
│       ├── replication.rs
│       └── tiering.rs
├── network/                 # Network fabric library (Rust)
│   ├── Cargo.toml
│   └── src/
│       ├── lib.rs
│       ├── rdma.rs          # UCX/RDMA transport
│       ├── tcp.rs           # TCP fallback
│       ├── qos.rs           # Traffic classes
│       └── topology.rs      # Network topology discovery
├── deploy/                  # Deployment configurations
│   ├── docker/
│   │   ├── Dockerfile.agent
│   │   └── Dockerfile.control-plane
│   └── systemd/
│       ├── compute-agent.service
│       └── compute-control-plane.service
└── docs/
    ├── ARCHITECTURE.md
    ├── API.md
    └── DEVELOPMENT.md
```

---

## Key Design Principles

1. **Single binary where possible.** The agent is one Rust binary. The control plane is one Go binary. No sidecars.
2. **Protobuf is the source of truth.** All interfaces defined in `.proto` files. Code generated for Go, Rust, Python.
3. **Graceful degradation.** RDMA unavailable → TCP. GPU unavailable → CPU fallback. Node fails → restart elsewhere.
4. **Zero-copy everywhere.** RDMA for network. `sendfile` for storage. Shared memory for local IPC.
5. **Observability built-in.** Prometheus metrics, structured logs, distributed tracing from day one.
6. **Security by default.** mTLS between all components. No plaintext anywhere. Least-privilege agent design.
