/// Heartbeat manager: maintains the gRPC connection to the control plane.
///
/// On startup:
///   1. Creates a gRPC channel to the control plane.
///   2. Calls `Register` to advertise this node's hardware resources.
///   3. Opens a bidirectional `Heartbeat` stream and immediately sends the
///      first heartbeat so the server's Recv() doesn't block.
///
/// Once running:
///   - Sends `HeartbeatRequest` every N seconds with current utilization
///     (call `send_heartbeat`).
///   - Receives `HeartbeatResponse` messages with pending tasks / cancels
///     (await `next_instruction`).
use crate::compute::v1::agent_service_client::AgentServiceClient;
use crate::compute::v1::{HeartbeatRequest, NodeResources, NodeUtilization, RegisterRequest};
use anyhow::Context;
use tokio::sync::mpsc;
use tokio_stream::wrappers::ReceiverStream;
use tonic::metadata::MetadataValue;
use tonic::transport::{Certificate, ClientTlsConfig, Identity};

/// TLS configuration for the agent's gRPC connection.
#[derive(Clone)]
pub struct TlsConfig {
    /// Path to the CA certificate (PEM) for verifying the control plane.
    pub ca_cert_path: String,
    /// Path to the agent's certificate (PEM) for mTLS.
    pub cert_path: String,
    /// Path to the agent's private key (PEM) for mTLS.
    pub key_path: String,
    /// Expected TLS server name (CN or SAN). Defaults to "control-plane".
    pub domain_name: Option<String>,
}

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
///
/// On drop, the background response reader task is aborted cleanly.
pub struct HeartbeatConnection {
    /// This node's ID, stored once at registration time.
    node_id: String,
    /// The heartbeat interval (milliseconds) as specified by the control plane.
    pub heartbeat_interval_ms: u64,
    /// Send side to feed heartbeat requests into the bidirectional gRPC stream.
    request_tx: mpsc::Sender<HeartbeatRequest>,
    /// Receive side for control-plane instructions pushed through the stream.
    instruction_rx: mpsc::Receiver<ControlInstruction>,
    /// Join handle for the background gRPC response reader task.
    _task: tokio::task::JoinHandle<()>,
}

impl Drop for HeartbeatConnection {
    fn drop(&mut self) {
        self._task.abort();
    }
}

impl HeartbeatConnection {
    /// Send a heartbeat message with current resource utilization.
    /// Uses the stored `node_id` rather than trusting the utilization struct.
    pub async fn send_heartbeat(
        &self,
        sequence: u64,
        utilization: NodeUtilization,
    ) -> anyhow::Result<()> {
        let req = HeartbeatRequest {
            node_id: self.node_id.clone(),
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
    ///
    /// The first heartbeat is sent immediately from this method so the
    /// server's stream.Recv() does not block waiting for data.
    ///
    /// If `tls_config` is provided, the connection uses TLS/mTLS.
    pub async fn connect(
        control_plane_addr: String,
        resources: NodeResources,
        agent_token: Option<String>,
        tls_config: Option<TlsConfig>,
    ) -> anyhow::Result<HeartbeatConnection> {
        let mut endpoint = tonic::transport::Endpoint::from_shared(control_plane_addr.clone())
            .context("invalid control plane address")?
            .connect_timeout(std::time::Duration::from_secs(5))
            .timeout(std::time::Duration::from_secs(10))
            .keep_alive_while_idle(true)
            .http2_keep_alive_interval(std::time::Duration::from_secs(10))
            .keep_alive_timeout(std::time::Duration::from_secs(5));

        // Configure TLS if cert paths are provided.
        if let Some(ref cfg) = tls_config {
            let ca_pem = tokio::fs::read(&cfg.ca_cert_path)
                .await
                .context("failed to read CA certificate")?;
            let cert_pem = tokio::fs::read(&cfg.cert_path)
                .await
                .context("failed to read agent certificate")?;
            let key_pem = tokio::fs::read(&cfg.key_path)
                .await
                .context("failed to read agent key")?;

            let mut tls = ClientTlsConfig::new()
                .ca_certificate(Certificate::from_pem(ca_pem))
                .identity(Identity::from_pem(cert_pem, key_pem));

            // Override domain name to match the server cert CN.
            let domain = cfg.domain_name.as_deref().unwrap_or("control-plane");
            tls = tls.domain_name(domain);

            tracing::info!(domain, "TLS/mTLS enabled for control plane connection");

            endpoint = endpoint
                .tls_config(tls)
                .context("failed to configure TLS")?;
        } else {
            tracing::warn!("TLS not configured — connecting with plaintext (insecure)");
        }

        let channel = endpoint
            .connect()
            .await
            .context("failed to connect to control plane")?;

        let mut client = AgentServiceClient::new(channel);

        let node_id = resources.node_id.clone();

        // --- Step 1: Register (with optional auth token) ---
        let mut register_req = tonic::Request::new(RegisterRequest {
            node_id: node_id.clone(),
            address: String::new(),
            resources: Some(resources),
            agent_version: env!("CARGO_PKG_VERSION").to_string(),
        });

        if let Some(ref token) = agent_token {
            let bearer = MetadataValue::try_from(format!("Bearer {}", token))
                .context("invalid agent token")?;
            register_req.metadata_mut().insert("authorization", bearer);
        }

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

        let mut hb_req = tonic::Request::new(request_stream);
        if let Some(ref token) = agent_token {
            let bearer = MetadataValue::try_from(format!("Bearer {}", token))
                .context("invalid agent token")?;
            hb_req.metadata_mut().insert("authorization", bearer);
        }

        let response = client
            .heartbeat(hb_req)
            .await
            .context("failed to open heartbeat stream")?;

        let mut response_stream = response.into_inner();

        // Bounded channel prevents unbounded memory growth if the agent's
        // main loop falls behind control-plane instruction delivery.
        let (instruction_tx, instruction_rx) = mpsc::channel::<ControlInstruction>(256);

        // --- Step 3: Send the first heartbeat immediately ---
        // This must happen before the server's Recv() times out.
        // We send it from the main task, not a spawned delayed task.
        let init_req = HeartbeatRequest {
            node_id: node_id.clone(),
            sequence: 0,
            utilization: None,
            tasks: vec![],
        };
        request_tx
            .send(init_req)
            .await
            .map_err(|_| anyhow::anyhow!("failed to send initial heartbeat"))?;

        // --- Step 4: Spawn background reader for responses ---
        let nid = node_id.clone();
        let task = tokio::spawn(async move {
            loop {
                match response_stream.message().await {
                    Ok(Some(resp)) => {
                        let seq = resp.sequence;
                        tracing::debug!(node_id = %nid, sequence = seq, "heartbeat ack received");

                        if !resp.pending_tasks.is_empty() {
                            let instruction = ControlInstruction::PendingTasks(resp.pending_tasks);
                            if instruction_tx.send(instruction).await.is_err() {
                                break; // receiver dropped
                            }
                        }
                        if !resp.cancel_task_ids.is_empty() {
                            let cancel = ControlInstruction::CancelTasks(resp.cancel_task_ids);
                            if instruction_tx.send(cancel).await.is_err() {
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
            node_id,
            heartbeat_interval_ms: heartbeat_ms,
            request_tx,
            instruction_rx,
            _task: task,
        })
    }
}

// ============================================================================
// Tests
// ============================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use crate::compute::v1::{HeartbeatResponse, NodeUtilization, PendingTask, TaskSpec};

    #[test]
    fn test_heartbeat_request_node_id_and_sequence() {
        let req = HeartbeatRequest {
            node_id: "test-node-01".into(),
            sequence: 42,
            utilization: Some(NodeUtilization {
                node_id: "should-be-overwritten".into(),
                timestamp_ns: 123456789,
                cpu: None,
                memory: None,
                gpus: vec![],
                storage: None,
                network: None,
            }),
            tasks: vec![],
        };
        assert_eq!(req.node_id, "test-node-01");
        assert_eq!(req.sequence, 42);
        assert!(req.utilization.is_some());
    }

    #[test]
    fn test_heartbeat_sequence_monotonic() {
        let s1 = HeartbeatRequest {
            node_id: "n1".into(),
            sequence: 1,
            utilization: None,
            tasks: vec![],
        };
        let s2 = HeartbeatRequest {
            node_id: "n1".into(),
            sequence: 2,
            utilization: None,
            tasks: vec![],
        };
        assert!(s1.sequence < s2.sequence);
        assert_eq!(s1.node_id, s2.node_id);
    }

    #[test]
    fn test_initial_heartbeat_no_utilization() {
        let init = HeartbeatRequest {
            node_id: "agent-01".into(),
            sequence: 0,
            utilization: None,
            tasks: vec![],
        };
        assert_eq!(init.sequence, 0);
        assert!(init.utilization.is_none());
    }

    #[test]
    fn test_heartbeat_response_echoes_sequence() {
        let resp = HeartbeatResponse {
            acknowledged: true,
            sequence: 7,
            pending_tasks: vec![],
            cancel_task_ids: vec![],
        };
        assert!(resp.acknowledged);
        assert_eq!(resp.sequence, 7);
    }

    #[test]
    fn test_heartbeat_response_pending_tasks() {
        let task = PendingTask {
            spec: Some(TaskSpec {
                task_id: "task-abc".into(),
                job_id: "job-xyz".into(),
                container_image: "alpine".into(),
                command: vec!["echo".into(), "hello".into()],
                ..Default::default()
            }),
        };
        let resp = HeartbeatResponse {
            acknowledged: true,
            sequence: 1,
            pending_tasks: vec![task],
            cancel_task_ids: vec![],
        };
        assert_eq!(resp.pending_tasks.len(), 1);
        let spec = resp.pending_tasks[0].spec.as_ref().unwrap();
        assert_eq!(spec.task_id, "task-abc");
    }

    #[test]
    fn test_heartbeat_response_cancellations() {
        let resp = HeartbeatResponse {
            acknowledged: true,
            sequence: 5,
            pending_tasks: vec![],
            cancel_task_ids: vec!["task-1".into(), "task-2".into()],
        };
        assert_eq!(resp.cancel_task_ids.len(), 2);
    }

    #[tokio::test]
    async fn test_heartbeat_connection_instruction_receive() {
        let (request_tx, _rx) = mpsc::channel::<HeartbeatRequest>(16);
        let (tx, instruction_rx) = mpsc::channel::<ControlInstruction>(1);
        let mut conn = HeartbeatConnection {
            node_id: "test-node".into(),
            heartbeat_interval_ms: 5000,
            request_tx,
            instruction_rx,
            _task: tokio::spawn(async {}),
        };
        tx.send(ControlInstruction::CancelTasks(vec!["task-x".into()]))
            .await
            .unwrap();
        let inst = conn.next_instruction().await;
        assert!(inst.is_some());
        match inst.unwrap() {
            ControlInstruction::CancelTasks(ids) => assert_eq!(ids, vec!["task-x"]),
            _ => panic!("expected CancelTasks"),
        }
    }

    #[tokio::test]
    async fn test_heartbeat_connection_closed_channel_returns_none() {
        let (request_tx, _rx) = mpsc::channel::<HeartbeatRequest>(16);
        let (tx, instruction_rx) = mpsc::channel::<ControlInstruction>(1);
        drop(tx);
        let mut conn = HeartbeatConnection {
            node_id: "n1".into(),
            heartbeat_interval_ms: 5000,
            request_tx,
            instruction_rx,
            _task: tokio::spawn(async {}),
        };
        assert!(conn.next_instruction().await.is_none());
    }

    #[tokio::test]
    async fn test_send_heartbeat_uses_stored_node_id() {
        let (request_tx, mut request_rx) = mpsc::channel::<HeartbeatRequest>(16);
        let (_tx, instruction_rx) = mpsc::channel::<ControlInstruction>(1);
        let conn = HeartbeatConnection {
            node_id: "persistent-id".into(),
            heartbeat_interval_ms: 5000,
            request_tx,
            instruction_rx,
            _task: tokio::spawn(async {}),
        };
        conn.send_heartbeat(
            1,
            NodeUtilization {
                node_id: "wrong-id".into(),
                timestamp_ns: 0,
                cpu: None,
                memory: None,
                gpus: vec![],
                storage: None,
                network: None,
            },
        )
        .await
        .unwrap();
        let sent = request_rx.recv().await.unwrap();
        assert_eq!(sent.node_id, "persistent-id");
        assert_eq!(sent.sequence, 1);
    }

    #[tokio::test]
    async fn test_send_heartbeat_closed_channel_error() {
        let (request_tx, request_rx) = mpsc::channel::<HeartbeatRequest>(1);
        let (_tx, instruction_rx) = mpsc::channel::<ControlInstruction>(1);
        let conn = HeartbeatConnection {
            node_id: "n1".into(),
            heartbeat_interval_ms: 5000,
            request_tx,
            instruction_rx,
            _task: tokio::spawn(async {}),
        };
        drop(request_rx);
        let result = conn
            .send_heartbeat(
                1,
                NodeUtilization {
                    node_id: String::new(),
                    timestamp_ns: 0,
                    cpu: None,
                    memory: None,
                    gpus: vec![],
                    storage: None,
                    network: None,
                },
            )
            .await;
        assert!(result.is_err());
        assert!(result.unwrap_err().to_string().contains("channel closed"));
    }

    #[test]
    fn test_control_instruction_debug() {
        let debug = format!("{:?}", ControlInstruction::PendingTasks(vec![]));
        assert!(debug.contains("PendingTasks"));
        let debug = format!("{:?}", ControlInstruction::CancelTasks(vec![]));
        assert!(debug.contains("CancelTasks"));
    }
}
