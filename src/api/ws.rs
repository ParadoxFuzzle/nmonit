use crate::api::tasks::{StreamEvent, TaskResult, TaskTracker};
use crate::common::types::*;
use crate::host::models::ModelRegistry;
use crate::host::workers::WorkerManager;
use axum::extract::ws::{Message, WebSocket};
use futures::stream::SplitStream;
use futures::{SinkExt, StreamExt};
use std::sync::Arc;
use tracing::{info, warn};
use uuid::Uuid;

/// Handle an incoming WebSocket connection from a worker.
pub async fn handle_worker_connection(
    socket: WebSocket,
    worker_manager: Arc<WorkerManager>,
    model_registry: Arc<ModelRegistry>,
    task_tracker: TaskTracker,
    auth_token: Option<String>,
) {
    let (mut sender, mut receiver) = socket.split();

    // Wait for registration message
    let registration = match receive_registration(&mut receiver).await {
        Some(reg) => reg,
        None => {
            warn!("Worker connection closed before registration");
            return;
        }
    };

    // Extract fields from the Register enum variant
    let (worker_name, hostname, capabilities, worker_auth_token) = match &registration {
        WsWorkerMessage::Register {
            name,
            hostname,
            capabilities,
            auth_token,
        } => (name.clone(), hostname.clone(), capabilities.clone(), auth_token.clone()),
        _ => {
            warn!("Expected Register message");
            return;
        }
    };

    // Authenticate
    if let Some(ref required_token) = auth_token {
        match &worker_auth_token {
            Some(token) if token == required_token => {
                info!("Worker '{worker_name}' authenticated successfully");
            }
            _ => {
                warn!("Worker '{worker_name}' authentication failed — closing connection");
                let _ = sender
                    .send(Message::Text(
                        serde_json::to_string(&WsHostMessage::Shutdown {
                            reason: "Authentication failed".to_string(),
                        })
                        .unwrap(),
                    ))
                    .await;
                return;
            }
        }
    }

    let worker_id = Uuid::new_v4();

    // Register the worker
    let worker_info = WorkerInfo {
        id: worker_id,
        name: worker_name.clone(),
        hostname: hostname.clone(),
        status: WorkerStatus::Online,
        resources: NodeResources {
            hostname: hostname.clone(),
            platform: std::env::consts::OS.to_string(),
            cpus: Vec::new(),
            gpus: Vec::new(),
            memory: MemoryInfo {
                total_mb: 0,
                used_mb: 0,
                free_mb: 0,
                available_mb: 0,
                swap_total_mb: 0,
                swap_used_mb: 0,
            },
        },
        capabilities,
        loaded_models: Vec::new(),
        current_tasks: 0,
        max_concurrent_tasks: 4,
        last_heartbeat: chrono::Utc::now(),
        connected_at: chrono::Utc::now(),
        auth_token: worker_auth_token,
    };

    worker_manager.register(worker_info);

    // Register the WebSocket sender in TaskTracker so the host can send tasks
    task_tracker.register_worker(worker_id, sender);

    // Send acknowledgment
    let ack = WsHostMessage::RegisterAck {
        worker_id,
        heartbeat_interval_secs: 5,
    };

    // Get sender from tracker to send ack
    let ack_text = serde_json::to_string(&ack).unwrap();
    if let Some(sender) = task_tracker.get_worker_sender(&worker_id) {
        if let Err(e) = sender.lock().await.send(Message::Text(ack_text)).await {
            warn!("Failed to send registration ack to '{worker_name}': {e}");
            task_tracker.remove_worker(&worker_id);
            worker_manager.remove(&worker_id);
            return;
        }
    }

    info!("Worker '{worker_name}' connected with ID {worker_id}");

    // Process messages from the worker
    process_worker_messages(receiver, worker_id, &worker_manager, &model_registry, &task_tracker).await;

    // Cleanup on disconnect
    task_tracker.remove_worker(&worker_id);
    worker_manager.disconnect(worker_id);
    model_registry.worker_disconnected(worker_id);
}

/// Process incoming messages from a connected worker.
async fn process_worker_messages(
    mut receiver: SplitStream<WebSocket>,
    worker_id: Uuid,
    worker_manager: &WorkerManager,
    model_registry: &ModelRegistry,
    task_tracker: &TaskTracker,
) {
    loop {
        match receiver.next().await {
            Some(Ok(Message::Text(text))) => {
                if let Err(e) = handle_worker_message(
                    &text,
                    worker_id,
                    worker_manager,
                    model_registry,
                    task_tracker,
                )
                .await
                {
                    warn!("Error handling worker message: {e}");
                }
            }
            Some(Ok(Message::Close(_))) | None => {
                info!("Worker {worker_id} disconnected");
                break;
            }
            Some(Err(e)) => {
                warn!("WebSocket error for worker {worker_id}: {e}");
                break;
            }
            _ => {}
        }
    }
}

/// Receive and parse the registration message from a worker.
async fn receive_registration(
    receiver: &mut SplitStream<WebSocket>,
) -> Option<WsWorkerMessage> {
    loop {
        match receiver.next().await {
            Some(Ok(Message::Text(text))) => {
                match serde_json::from_str::<WsWorkerMessage>(&text) {
                    Ok(msg) => {
                        if matches!(&msg, WsWorkerMessage::Register { .. }) {
                            return Some(msg);
                        }
                        warn!("Expected Register message, got something else");
                    }
                    Err(e) => {
                        warn!("Failed to parse registration message: {e}");
                    }
                }
            }
            Some(Ok(Message::Close(_))) | None => return None,
            Some(Err(e)) => {
                warn!("WebSocket error during registration: {e}");
                return None;
            }
            _ => {}
        }
    }
}

/// Handle a parsed message from a worker.
async fn handle_worker_message(
    text: &str,
    worker_id: Uuid,
    worker_manager: &WorkerManager,
    _model_registry: &ModelRegistry,
    task_tracker: &TaskTracker,
) -> Result<(), anyhow::Error> {
    let msg: WsWorkerMessage = serde_json::from_str(text)?;

    match msg {
        WsWorkerMessage::Heartbeat {
            resources,
            current_tasks,
            loaded_models,
        } => {
            worker_manager.heartbeat(worker_id, resources, current_tasks, loaded_models);
        }
        WsWorkerMessage::TaskResult {
            task_id,
            success,
            result,
            error,
        } => {
            if success {
                if let Some(response) = result {
                    info!("Task {task_id} completed on worker {worker_id}");
                    task_tracker.complete_task(&task_id, TaskResult::Success(response));
                } else {
                    warn!("Task {task_id} returned success but no result from worker {worker_id}");
                    task_tracker.complete_task(
                        &task_id,
                        TaskResult::Failed("No result data".to_string()),
                    );
                }
            } else {
                let err_msg = error.unwrap_or_else(|| "Unknown error".to_string());
                warn!("Task {task_id} failed on worker {worker_id}: {err_msg}");
                task_tracker.complete_task(&task_id, TaskResult::Failed(err_msg));
            }
        }
        WsWorkerMessage::TaskProgress {
            task_id,
            chunk,
            done,
        } => {
            if done {
                info!("Task {task_id} streaming completed on worker {worker_id}");
                // Send the final chunk as a Done event with the chunk data
                task_tracker.push_stream_chunk(
                    &task_id,
                    StreamEvent::Done {
                        final_chunk: chunk.clone(),
                    },
                );
                task_tracker.remove_streaming_task(&task_id);
            } else {
                // Forward the chunk to the streaming task channel
                task_tracker.push_stream_chunk(&task_id, StreamEvent::Chunk(chunk));
            }
        }
        WsWorkerMessage::Register { .. } => {
            warn!("Worker {worker_id} sent duplicate registration");
        }
    }

    Ok(())
}
