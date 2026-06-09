/// Distributed Compute Fabric — Node Agent
///
/// Runs on every node in the cluster. Responsible for:
/// - Hardware resource discovery and reporting
/// - Heartbeat / health reporting to control plane
/// - Task execution (containerd runtime hook)
/// - Distributed memory management (RDMA)
/// - GPU memory management
/// - Metrics collection
use clap::Parser;
use tracing_subscriber::EnvFilter;

mod discovery;
mod executor;
mod gpu;
mod heartbeat;
mod memory;
mod metrics;
mod network;
mod resources;

/// Generated protobuf + gRPC types from the shared .proto definitions.
#[allow(clippy::all)]
mod compute {
    pub mod v1 {
        tonic::include_proto!("compute.v1");
    }
}

/// CLI arguments for the node agent.
#[derive(Parser, Debug)]
#[command(name = "compute-agent")]
#[command(about = "Distributed compute fabric node agent", long_about = None)]
struct Args {
    /// Address of the control plane (host:port)
    #[arg(short, long, default_value = "http://localhost:9000")]
    control_plane: String,

    /// Node ID (auto-detected if not set)
    #[arg(short, long)]
    node_id: Option<String>,

    /// Directory for agent state
    #[arg(long, default_value = "/var/lib/compute-agent")]
    data_dir: String,

    /// mDNS domain for auto-discovery (LAN only)
    #[arg(long, default_value = "_compute-fabric._tcp.local")]
    mdns_domain: String,

    /// Log level (trace, debug, info, warn, error)
    #[arg(long, default_value = "info")]
    log_level: String,

    /// Enable RDMA (requires compatible hardware)
    #[arg(long, default_value = "true")]
    rdma: bool,

    /// Override GPU detection (useful for testing without GPUs)
    #[arg(long)]
    mock_gpu_count: Option<usize>,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    // Initialize structured logging (JSON to stdout)
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new(&args.log_level)),
        )
        .json()
        .init();

    tracing::info!(
        node_id = ?args.node_id,
        control_plane = %args.control_plane,
        "compute-agent starting"
    );

    // --- Phase 1: Discover hardware resources ---
    let resources = resources::discover_resources(&args).await?;
    let node_id = resources.node_id.clone();

    tracing::info!(
        node_id = %node_id,
        cpu_cores = resources.cpu.as_ref().map(|c| c.physical_cores).unwrap_or(0),
        ram_gb = resources
            .memory
            .as_ref()
            .map(|m| m.total_bytes / (1024 * 1024 * 1024))
            .unwrap_or(0),
        gpu_count = resources.gpus.len(),
        "hardware resources discovered"
    );

    // --- Phase 1: Connect to control plane and start heartbeat ---
    let mut conn = heartbeat::HeartbeatManager::connect(
        args.control_plane.clone(),
        resources,
    )
    .await?;

    let mut sequence: u64 = 0;
    let mut heartbeat_tick = tokio::time::interval(std::time::Duration::from_secs(5));

    tracing::info!(node_id = %node_id, "compute-agent initialized and running");

    // Main event loop: heartbeat sending, instruction receiving, shutdown.
    loop {
        tokio::select! {
            // --- Send heartbeat every 5 seconds ---
            _ = heartbeat_tick.tick() => {
                sequence += 1;
                let util = resources::collect_utilization(&node_id);
                if let Err(e) = conn.send_heartbeat(sequence, util).await {
                    tracing::error!(error = %e, "failed to send heartbeat");
                    break;
                }
                tracing::debug!(sequence, "heartbeat sent");
            }

            // --- Receive instructions from control plane ---
            instruction = conn.next_instruction() => {
                match instruction {
                    Some(heartbeat::ControlInstruction::PendingTasks(tasks)) => {
                        tracing::info!(
                            pending_count = tasks.len(),
                            "received pending tasks from control plane"
                        );
                    }
                    Some(heartbeat::ControlInstruction::CancelTasks(ids)) => {
                        tracing::info!(
                            cancel_count = ids.len(),
                            cancel_ids = ?ids,
                            "received task cancellations from control plane"
                        );
                    }
                    None => {
                        tracing::warn!("heartbeat stream ended, shutting down");
                        break;
                    }
                }
            }

            // --- Shutdown signal ---
            _ = tokio::signal::ctrl_c() => {
                tracing::info!("shutdown signal received");
                break;
            }
        }
    }

    tracing::info!("compute-agent shut down cleanly");
    Ok(())
}
