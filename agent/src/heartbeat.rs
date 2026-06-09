/// Heartbeat manager: maintains the gRPC connection to the control plane.
///
/// On startup:
///   1. Creates a gRPC channel to the control plane.
///   2. Calls `Register` to advertise this node's hardware resources.
///   3. Opens a bidirectional `Heartbeat` stream.
///
/// Once running:
///   - Sends `HeartbeatRequest` every 5 seconds with current utilization
///     (call `send_heartbeat`).
///   - Receives `HeartbeatResponse` messages with pending tasks / cancels
///     (await `next_instruction`).
use crate::compute::v1::agent_service_client::AgentServiceClient;
use crate::compute::v1::{HeartbeatRequest, NodeResources, NodeUtilization, RegisterRequest};
use anyhow::Context;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;

/// Messages the agent receives from the control plane via the heartbeat stream.
#[derive(Debug)]
pub enum ControlInstruction {
    PendingTasks(Vec<crate::compute::v1::PendingTask>),
    CancelTasks(Vec<String>),
}

/// Active heartbeat connection to the control plane.
///
/// Created by `HeartbeatManager::connect()`. Use `send_heartbeat()` to
/// push utilization updates and `next_instruction()` to receive tasks/cancels.
pub struct HeartbeatConnection {
    /// Send side to feed heartbeat requests into the bidirectional gRPC stream.
    request_tx: mpsc::Sender<HeartbeatRequest>,
    /// Receive side for control-plane instructions pushed through the stream.
    instruction_rx: mpsc::UnboundedReceiver<ControlInstruction>,
    /// Join handle for the background gRPC response reader task.
    ///
    /// The task is intentionally detached on drop (the JoinHandle is not
    /// awaited or aborted). When the connection is shut down the gRPC stream
    /// errors, causing the task to exit naturally.
    _task: tokio::task::JoinHandle<()>,
}

impl HeartbeatConnection {
    /// Send a heartbeat message with current resource utilization.
    pub async fn send_heartbeat(
        &self,
        sequence: u64,
        utilization: NodeUtilization,
    ) -> anyhow::Result<()> {
        let req = HeartbeatRequest {
            node_id: utilization.node_id.clone(),
            sequence,
            utilization: Some(utilization),
            tasks: vec![],
        };
        self.request_tx
            .send(req)
            .await
            .map_err(|_| anyhow::anyhow!("heartbeat send channel closed"))?;
        Ok(())
    }

    /// Await the next control-plane instruction (pending tasks / cancels).
    /// Returns `None` when the heartbeat stream has ended.
    pub async fn next_instruction(&mut self) -> Option<ControlInstruction> {
        self.instruction_rx.recv().await
    }
}

/// Stateless entry point — connects, registers, and starts the heartbeat stream.
pub struct HeartbeatManager;

impl HeartbeatManager {
    /// Connect to the control plane, register this node, and start the
    /// bidirectional heartbeat stream. Returns a `HeartbeatConnection`
    /// that provides send/receive handles for the application loop.
    pub async fn connect(
        control_plane_addr: String,
        resources: NodeResources,
    ) -> anyhow::Result<HeartbeatConnection> {
        let endpoint =
            tonic::transport::Endpoint::from_shared(control_plane_addr.clone())
                .context("invalid control plane address")?
                .connect_timeout(std::time::Duration::from_secs(5))
                .timeout(std::time::Duration::from_secs(10));

        let channel = endpoint
            .connect()
            .await
            .context("failed to connect to control plane")?;

        let mut client = AgentServiceClient::new(channel);

        let node_id = resources.node_id.clone();

        // --- Step 1: Register ---
        let register_req = RegisterRequest {
            node_id: node_id.clone(),
            address: String::new(),
            resources: Some(resources),
            agent_version: env!("CARGO_PKG_VERSION").to_string(),
        };

        let resp = client
            .register(register_req)
            .await
            .context("registration RPC failed")?
            .into_inner();

        if !resp.accepted {
            anyhow::bail!("registration rejected by control plane");
        }

        let heartbeat_ms = resp.heartbeat_interval_ms;

        tracing::info!(
            cluster_id = %resp.cluster_id,
            leader = %resp.control_plane_leader,
            heartbeat_ms,
            "registered with control plane"
        );

        // --- Step 2: Open bidirectional heartbeat stream ---
        let (request_tx, request_rx) = mpsc::channel::<HeartbeatRequest>(16);
        let request_stream = ReceiverStream::new(request_rx);

        let mut response_stream = client
            .heartbeat(request_stream)
            .await
            .context("failed to open heartbeat stream")?
            .into_inner();

        let (instruction_tx, instruction_rx) = mpsc::unbounded_channel();

        // --- Step 3: Spawn background reader for responses ---
        let nid = node_id.clone();
        let task = tokio::spawn(async move {
            loop {
                match response_stream.message().await {
                    Ok(Some(resp)) => {
                        if !resp.pending_tasks.is_empty() {
                            let instruction =
                                ControlInstruction::PendingTasks(resp.pending_tasks);
                            if instruction_tx.send(instruction).is_err() {
                                break; // receiver dropped
                            }
                        }
                        if !resp.cancel_task_ids.is_empty() {
                            let cancel =
                                ControlInstruction::CancelTasks(resp.cancel_task_ids);
                            if instruction_tx.send(cancel).is_err() {
                                break;
                            }
                        }
                    }
                    Ok(None) => {
                        tracing::info!(node_id = %nid, "heartbeat stream closed by server");
                        break;
                    }
                    Err(e) => {
                        tracing::error!(error = %e, "heartbeat stream error");
                        break;
                    }
                }
            }
            tracing::warn!(node_id = %nid, "heartbeat response stream ended");
        });

        tracing::info!(node_id = %node_id, "heartbeat stream established");

        Ok(HeartbeatConnection {
            request_tx,
            instruction_rx,
            _task: task,
        })
    }
}
