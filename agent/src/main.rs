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
use std::time::Duration;
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
        include!(concat!(env!("OUT_DIR"), "/compute.v1.rs"));
    }
}

/// CLI arguments for the node agent.
#[derive(Parser, Debug)]
#[command(name = "compute-agent")]
#[command(about = "Distributed compute fabric node agent", long_about = None)]
struct Args {
    /// Address of the control plane (host:port, e.g. "localhost:9000")
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

    /// Agent token for control plane authentication
    #[arg(long, default_value = "")]
    agent_token: String,

    /// Path to CA certificate (PEM) for verifying control plane TLS
    #[arg(long)]
    tls_ca_cert: Option<String>,

    /// Path to agent certificate (PEM) for mTLS client authentication
    #[arg(long)]
    tls_cert: Option<String>,

    /// Path to agent private key (PEM) for mTLS client authentication
    #[arg(long)]
    tls_key: Option<String>,

    /// Expected TLS server name (CN/SAN) for certificate validation
    #[arg(long, default_value = "control-plane")]
    tls_domain: String,

    /// Max reconnection attempts before giving up (0 = unlimited)
    #[arg(long, default_value = "10")]
    max_reconnect_attempts: u32,

    /// Initial reconnection backoff (seconds)
    #[arg(long, default_value = "1")]
    reconnect_backoff_secs: u64,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let args = Args::parse();

    // Initialize structured logging (JSON to stdout)
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env().unwrap_or_else(|_| EnvFilter::new(&args.log_level)),
        )
        .json()
        .init();

    tracing::info!(
        node_id = ?args.node_id,
        control_plane = %args.control_plane,
        "compute-agent starting"
    );

    // --- Phase 1: Discover hardware resources (once) ---
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

    // --- Main connection loop with reconnection ---
    let mut attempt: u32 = 0;
    let mut backoff = Duration::from_secs(args.reconnect_backoff_secs);
    let max_backoff = Duration::from_secs(60);

    let agent_token = if args.agent_token.is_empty() {
        None
    } else {
        Some(args.agent_token.clone())
    };

    // Build TLS config if cert paths are provided.
    let tls_config = match (&args.tls_ca_cert, &args.tls_cert, &args.tls_key) {
        (Some(ca), Some(cert), Some(key)) => Some(heartbeat::TlsConfig {
            ca_cert_path: ca.clone(),
            cert_path: cert.clone(),
            key_path: key.clone(),
            domain_name: Some(args.tls_domain.clone()),
        }),
        _ => {
            tracing::warn!("TLS not configured — connecting with plaintext (insecure)");
            None
        }
    };

    loop {
        attempt += 1;

        match heartbeat::HeartbeatManager::connect(
            args.control_plane.clone(),
            resources.clone(),
            agent_token.clone(),
            tls_config.clone(),
        )
        .await
        {
            Ok(mut conn) => {
                // Reset reconnection state on successful connection
                attempt = 0;
                backoff = Duration::from_secs(args.reconnect_backoff_secs);

                let heartbeat_interval_ms = conn.heartbeat_interval_ms;
                let heartbeat_dur = Duration::from_millis(heartbeat_interval_ms.max(1000));

                let mut heartbeat_tick = tokio::time::interval(heartbeat_dur);
                let mut sequence: u64 = 0; // Start at 0 since connect() sends seq=0

                // Consume the first immediate tick so we don't double-fire
                // right after the initial heartbeat sent in connect().
                heartbeat_tick.tick().await;

                tracing::info!(
                    node_id = %node_id,
                    interval_ms = heartbeat_interval_ms,
                    "compute-agent initialized and running"
                );

                // Main event loop
                let stream_ended = loop {
                    tokio::select! {
                        // --- Send heartbeat periodically ---
                        _ = heartbeat_tick.tick() => {
                            sequence += 1;
                            let util = tokio::task::spawn_blocking(resources::collect_utilization)
                                .await
                                .unwrap_or_else(|e| {
                                    tracing::error!(error = %e, "spawn_blocking panicked in collect_utilization");
                                    crate::compute::v1::NodeUtilization::default()
                                });
                            if let Err(e) = conn.send_heartbeat(sequence, util).await {
                                tracing::error!(error = %e, "failed to send heartbeat");
                                break true; // stream ended, reconnect
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
                                    tracing::warn!("heartbeat stream ended, reconnecting");
                                    break true;
                                }
                            }
                        }

                        // --- Shutdown signal ---
                        _ = tokio::signal::ctrl_c() => {
                            tracing::info!("shutdown signal received");
                            return Ok(());
                        }
                    }
                };

                if stream_ended {
                    // conn is dropped here, aborting the background task
                    drop(conn);
                }
            }
            Err(e) => {
                tracing::error!(error = %e, attempt, "connection to control plane failed");
            }
        }

        // Check max attempts
        if args.max_reconnect_attempts > 0 && attempt >= args.max_reconnect_attempts {
            tracing::error!(
                attempts = attempt,
                "max reconnection attempts reached, exiting"
            );
            anyhow::bail!(
                "max reconnection attempts ({}) reached",
                args.max_reconnect_attempts
            );
        }

        tracing::info!(
            backoff_secs = backoff.as_secs(),
            attempt,
            "reconnecting to control plane"
        );
        tokio::time::sleep(backoff).await;

        // Exponential backoff with cap
        backoff = (backoff * 2).min(max_backoff);
    }
}
