# Distributed Compute Fabric

> Pool VRAM, RAM, CPU, and GPU across networked devices into a unified compute layer.

[![Rust](https://img.shields.io/badge/agent-Rust-orange)](agent/)
[![Go](https://img.shields.io/badge/control__plane-Go-00ADD8)](control-plane/)
[![License](https://img.shields.io/badge/license-MIT%2FApache--2.0-blue)](LICENSE)
[![Status](https://img.shields.io/badge/status-pre--alpha-red)]()

---

## What Is This?

A distributed compute fabric that aggregates hardware resources across multiple machines on a LAN and presents them as a single unified pool. Applications request what they need (memory, compute, GPU), and the fabric decides where to place it — the application doesn't need to know the cluster topology.

**Example:** Run an LLM that needs 48GB VRAM on a cluster where no single GPU has more than 24GB. The fabric shards the model across two GPUs on different nodes transparently.

## Architecture

```
Application (SDK/CLI/REST)
         │
    ┌────▼─────┐
    │ Control   │   Go  ·  gRPC  ·  Raft  ·  Prometheus
    │ Plane     │   Scheduler, API, Resource Manager
    └────┬─────┘
         │
    ┌────┼────┐
    │    │    │
┌───▼─┐┌─▼───┐┌▼───┐
│Node ││Node ││Node │   Rust  ·  UCX  ·  NVML  ·  containerd
│  A  ││  B  ││  C  │   Agent, Executor, Memory Manager
└─────┘└─────┘└─────┘
    │    │    │
    └────┼────┘
   RDMA / RoCE / TCP
```

## Getting Started

> ⚠️ **Pre-alpha.** This project is in active development. Phase 1 implementation is underway.

### Prerequisites

- **All nodes:** Linux kernel 5.15+, systemd
- **GPU nodes:** NVIDIA drivers 535+, NVML
- **RDMA (optional):** RDMA-capable NICs (Mellanox ConnectX-4+, RoCE v2)
- **Build tools:** Rust 1.80+, Go 1.22+, Protocol Buffers (buf)

### Quick Start (Coming Soon)

```bash
# Build everything
make

# Start the control plane (first node)
./bin/control-plane --bootstrap

# Start node agents on each machine
./bin/compute-agent --control-plane 192.168.1.10:9000

# Submit a job
./bin/compute submit --gpus 2 --vram 24G my-job.py

# Check cluster status
./bin/compute status
```

## Project Structure

| Directory | Language | Purpose |
|---|---|---|
| `proto/` | Protobuf | Wire protocol definitions (single source of truth) |
| `agent/` | Rust | Node agent: resource reporting, task execution, memory management |
| `control-plane/` | Go | Scheduler, REST API, resource manager, Raft consensus |
| `cli/` | Go | CLI tool (`compute`) |
| `sdk/c/` | C | Native C SDK |
| `sdk/python/` | Python | Python bindings + numpy-compatible distributed arrays |
| `sdk/go/` | Go | Go client library |
| `network/` | Rust | Network fabric library (RDMA/TCP) |
| `storage/` | Rust | Distributed storage (FUSE + S3) |
| `dashboard/` | — | Web dashboard (future) |

## Implementation Phases

| Phase | Duration | Focus |
|---|---|---|
| 1 — Foundation | Weeks 1-4 | Agent, control plane, basic scheduler, REST API, C SDK, CLI |
| 2 — Memory & Storage | Weeks 5-8 | Distributed shared memory, storage, tiering, consistency |
| 3 — GPU Integration | Weeks 9-12 | VRAM pooling, GPU scheduling, ML model sharding |
| 4 — Resilience | Weeks 13-16 | Checkpoint/restart, HA, mTLS, RBAC, quotas |
| 5 — Polish | Weeks 17-20 | Dashboard, tracing, alerting, performance tuning |

## Technology Stack

| Component | Choice | Rationale |
|---|---|---|
| Agent | Rust (tokio) | Minimal footprint, memory safety, direct hardware access |
| Control Plane | Go (gRPC, Raft) | Excellent concurrency, fast iteration, single binary |
| Wire Protocol | Protobuf + gRPC | Code gen for Go, Rust, Python; streaming support |
| Networking | UCX (RoCE v2 + TCP) | Mature RDMA library, multi-transport |
| GPU | CUDA (NVIDIA first) | ML ecosystem dominance |
| Containers | containerd + OCI hooks | Simple, no K8s dependency for v1 |
| Metrics | Prometheus | Industry standard |

## Documentation

- [Architecture & Technology Decisions](ARCHITECTURE.md)
- [API Reference](docs/API.md) (coming soon)
- [Development Guide](docs/DEVELOPMENT.md) (coming soon)

## License

MIT OR Apache-2.0
