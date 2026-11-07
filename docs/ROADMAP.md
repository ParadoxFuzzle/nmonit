# Roadmap

This page records capabilities that are **not implemented**. It is not a release commitment.

The current working surface is documented in [README.md](../README.md), [ARCHITECTURE.md](../ARCHITECTURE.md), and [docs/API.md](API.md).

## Execution and scheduling

- Job submission, lookup, cancellation, and log streaming.
- Scheduler placement, affinity, quotas, and resource accounting beyond inventory.
- Real `ExecuteTask` dispatch to agents.
- Container execution and OCI hooks.

The protobuf contracts under `proto/compute/v1/job.proto` exist, but `JobService` is not registered and there is no REST job API.

## Distributed memory

- Cluster-wide allocation and release.
- Hugepage-backed memory regions.
- RDMA registration and remote memory mapping.
- Memory migration and consistency modes.

`AgentService` has allocation RPCs that currently acknowledge without doing work. `MemoryService` is not registered.

## Storage and networking

- Distributed storage with FUSE and S3-compatible interfaces.
- RDMA/RoCE transport and TCP fallback.
- Multi-node data paths and zero-copy transfers.

There are no storage or network crates in the repository.

## Control-plane resilience

- Raft consensus, replication, and leader election.
- Persistent registry state.
- Multi-control-plane membership and failover.
- `ControlService` registration and cluster join APIs.

The current control plane is a single in-memory process.

## Clients and UI

- C, Go, Python, and Rust SDKs.
- Web dashboard.
- User accounts, RBAC, and quota management.

The only client shipped today is the Go `compute` CLI.

## GPU pooling

- VRAM pooling and cross-node GPU scheduling.
- Model sharding and GPU peer-to-peer transfers.
- Non-NVIDIA backends.

The agent currently reports NVIDIA inventory and utilization through NVML.
