# nmonit — Distributed LLM Compute Cluster

> **Share GPU, CPU, VRAM and RAM across multiple devices for LLM inference.**

nmonit turns a network of machines into a unified compute cluster for Large Language Model inference. A **host** node orchestrates workloads across **worker** nodes, pooling their hardware resources — GPU, VRAM, CPU, and RAM.

Built for **LiteLLM** as the inference backend, nmonit exposes an **OpenAI-compatible REST API** so any LLM application can use the cluster without modification.

---

## Architecture

```
┌──────────────────────────────────────────────┐
│              Host Machine                     │
│  ┌────────────────────────────────────────┐  │
│  │         nmonit Host (orchestrator)      │  │
│  │  • REST API (OpenAI-compatible)        │  │
│  │  • WebSocket worker management         │  │
│  │  • Load balancer & scheduler           │  │
│  │  • Model registry                      │  │
│  └────────────────────────────────────────┘  │
│              ↕ HTTP/WebSocket                  │
│  ┌────────────────────────────────────────┐  │
│  │         LiteLLM Proxy                  │  │
│  │  (model routing & inference)           │  │
│  └────────────────────────────────────────┘  │
└──────────────────────────────────────────────┘
         ↕                             ↕
┌──────────────┐          ┌──────────────────┐
│ Worker Node 1 │          │  Worker Node 2   │
│ GPU: RTX 4090 │          │  CPU: 16 cores   │
│ VRAM: 24 GB   │          │  RAM: 64 GB      │
│ RAM: 32 GB    │          │  (CPU-only)      │
└──────────────┘          └──────────────────┘
```

### How It Works

1. **Host** starts and connects to LiteLLM, loading available models
2. **Workers** connect to the host via WebSocket, reporting their hardware capabilities (GPU, VRAM, CPU, RAM)
3. Applications send inference requests to the host's OpenAI-compatible API (`/v1/chat/completions`)
4. The host's **scheduler** selects the optimal worker based on:
   - Model caching (prefer workers with model already loaded)
   - Available VRAM (for GPU workers)
   - Available RAM (for CPU workers)
   - Current task load
   - Priority
5. Workers execute inference using their local LiteLLM and return results

---

## Quick Start

### Prerequisites

- [Rust](https://rustup.rs/) (1.75+)
- [LiteLLM](https://docs.litellm.ai/docs/) running on the host machine
- Linux (Ubuntu 22.04+, Debian 12+, or similar)

### 1. Build & Install

```bash
git clone https://github.com/nmonit/nmonit.git
cd nmonit
cargo build --release
sudo ./scripts/install.sh
```

### 2. Configure

Edit `/etc/nmonit/nmonit.yaml`:

```yaml
host:
  listen_addr: "0.0.0.0"
  port: 9742
  litellm_base_url: "http://localhost:4000"

worker:
  host_addr: "192.168.1.100"  # Host IP
  host_port: 9742
  auth_token: "my-secret"
```

### 3. Start the Host

```bash
nmonit host --config /etc/nmonit/nmonit.yaml
# Or as a service:
sudo systemctl enable --now nmonit-host
```

### 4. Connect Workers

On each worker machine:

```bash
nmonit worker --host 192.168.1.100 --token "my-secret"
# Or as a service:
sudo systemctl enable --now nmonit-worker
```

### 5. Use the Cluster

Send requests to any OpenAI-compatible client:

```bash
curl http://localhost:9742/v1/chat/completions \
  -H "Content-Type: application/json" \
  -d '{
    "model": "gpt-4",
    "messages": [{"role": "user", "content": "Hello!"}]
  }'
```

Check cluster status:

```bash
nmonit status
nmonit models
```

---

## CLI Reference

| Command | Description |
|---------|-------------|
| `nmonit host [options]` | Start as the orchestrator node |
| `nmonit worker [options]` | Start as a worker node |
| `nmonit status [--host <url>]` | Show cluster status |
| `nmonit models [--host <url>]` | List available models |

### Host Options

| Flag | Env Variable | Description |
|------|-------------|-------------|
| `--config <path>` | `NMONIT_CONFIG` | Config file path |
| `--listen <addr>` | `NMONIT_LISTEN_ADDR` | Listen address (default: `0.0.0.0`) |
| `-P, --port <port>` | `NMONIT_PORT` | Listen port (default: `9742`) |
| `--litellm-url <url>` | `NMONIT_LITELLM_URL` | LiteLLM base URL |

### Worker Options

| Flag | Env Variable | Description |
|------|-------------|-------------|
| `--config <path>` | `NMONIT_CONFIG` | Config file path |
| `--host <addr>` | `NMONIT_HOST_ADDR` | Host address to connect to |
| `-P, --port <port>` | `NMONIT_HOST_PORT` | Host port (default: `9742`) |
| `--token <token>` | `NMONIT_AUTH_TOKEN` | Authentication token |
| `-n, --name <name>` | `NMONIT_WORKER_NAME` | Worker display name |

---

## API Endpoints

| Method | Path | Description |
|--------|------|-------------|
| `GET` | `/health` | Health check |
| `GET` | `/v1/models` | List models (OpenAI-compatible) |
| `POST` | `/v1/chat/completions` | Chat completion (OpenAI-compatible) |
| `GET` | `/cluster/nodes` | List connected workers |
| `GET` | `/cluster/stats` | Cluster resource statistics |
| `WS` | `/ws/worker` | WebSocket endpoint for workers |

---

## Scheduler Strategy

The nmonit scheduler distributes workloads using a scoring system:

1. **Model locality** (+200): Workers that already have the model loaded are strongly preferred
2. **VRAM availability** (+100 × ratio): GPU workers with free VRAM are preferred
3. **RAM availability** (+50 × ratio): Workers with free system memory are preferred
4. **Task capacity** (+80 × ratio): Workers with fewer active tasks are preferred
5. **GPU preference** (+30): GPU-equipped workers are preferred over CPU-only

---

## System Requirements

### Host
- Linux with systemd
- LiteLLM installed and running
- Network accessibility for workers
- Optional: NVIDIA GPU with CUDA

### Worker
- Linux
- Network access to host
- Optional: NVIDIA GPU with CUDA for GPU-accelerated inference
- Optional: LiteLLM for local model inference

---

## Security

- **Authentication**: Workers authenticate with the host using a shared token
- **TLS Support**: The host can be configured with TLS certificates
- **No open ports**: Workers initiate outbound connections only (no inbound firewall rules needed)

---

## Resource Monitoring

Workers automatically report:
- **GPU**: Name, VRAM total/used/free, utilization %, temperature
- **CPU**: Cores, threads, utilization %, frequency
- **Memory**: Total RAM, used, free, available, swap

---

## Development

```bash
cargo build
cargo run -- host --port 9742
cargo run -- worker --host 127.0.0.1 --port 9742
```

### Project Structure

```
src/
├── main.rs           # Entry point
├── cli.rs            # CLI argument parsing
├── config.rs         # Configuration
├── common/
│   ├── mod.rs
│   └── types.rs      # Shared types
├── host/
│   ├── mod.rs
│   ├── server.rs     # HTTP/WS server
│   ├── models.rs     # Model registry
│   ├── workers.rs    # Worker management
│   ├── scheduler.rs  # Load balancing
│   └── litellm.rs    # LiteLLM client
├── worker/
│   ├── mod.rs
│   ├── client.rs     # Worker agent
│   └── resources.rs  # Hardware monitoring
└── api/
    ├── mod.rs
    ├── routes.rs     # REST API routes
    └── ws.rs         # WebSocket handler
```

---

## License

MIT
