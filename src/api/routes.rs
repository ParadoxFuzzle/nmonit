use crate::api::tasks::{StreamEvent, TaskResult, TaskTracker};
use crate::common::types::*;
use crate::host::litellm::LiteLlmClient;
use crate::host::models::ModelRegistry;
use crate::host::scheduler::Scheduler;
use crate::host::workers::WorkerManager;
use axum::{
    extract::State,
    http::StatusCode,
    response::{
        IntoResponse,
        sse::{Event, KeepAlive, Sse},
    },
    Json,
};
use futures::SinkExt;
use std::convert::Infallible;
use std::pin::Pin;
use std::sync::Arc;
use std::time::Instant;
use tokio::time::{timeout, Duration};
use tokio_stream::wrappers::ReceiverStream;
use tokio_stream::StreamExt as _;
use uuid::Uuid;

/// Shared application state for the host server.
#[derive(Clone)]
pub struct AppState {
    pub worker_manager: Arc<WorkerManager>,
    pub model_registry: Arc<ModelRegistry>,
    pub scheduler: Arc<Scheduler>,
    pub litellm_client: LiteLlmClient,
    pub task_tracker: TaskTracker,
    pub start_time: Instant,
    pub route_to_local: bool,
}

/// GET /health — Health check
pub async fn health(State(state): State<AppState>) -> Json<HealthResponse> {
    Json(HealthResponse {
        status: "ok".to_string(),
        version: env!("CARGO_PKG_VERSION").to_string(),
        uptime_seconds: state.start_time.elapsed().as_secs(),
        mode: "host".to_string(),
        workers_connected: state.worker_manager.online().len(),
    })
}

/// GET /v1/models — List available models
pub async fn list_models(
    State(state): State<AppState>,
) -> Result<Json<ModelsListResponse>, AppError> {
    let mut models = state.model_registry.all_models();

    if models.is_empty() {
        match state.litellm_client.list_models().await {
            Ok(litellm_models) => {
                state.model_registry.register_models(litellm_models.clone());
                models = litellm_models;
            }
            Err(e) => {
                tracing::warn!("Failed to fetch models from LiteLLM: {e}");
            }
        }
    }

    Ok(Json(ModelsListResponse {
        object: "list".to_string(),
        data: models,
    }))
}

/// POST /v1/chat/completions — OpenAI-compatible chat completion
/// Supports both streaming (SSE) and non-streaming responses.
pub async fn chat_completion(
    State(state): State<AppState>,
    Json(req): Json<ChatCompletionRequest>,
) -> Result<axum::response::Response, AppError> {
    if req.stream {
        Ok(chat_completion_streaming(state, req).await.into_response())
    } else {
        chat_completion_non_streaming(state, req)
            .await
            .map(|r| r.into_response())
    }
}

// ─── Non-Streaming Path ───────────────────────────────────────────────────────

async fn chat_completion_non_streaming(
    state: AppState,
    req: ChatCompletionRequest,
) -> Result<Json<ChatCompletionResponse>, AppError> {
    let model_name = req.model.clone();
    let messages = req.messages.clone();
    let temperature = req.temperature;
    let max_tokens = req.max_tokens;
    let top_p = req.top_p;
    let frequency_penalty = req.frequency_penalty;
    let presence_penalty = req.presence_penalty;
    let stop = req.stop.clone();
    let priority = req.priority;

    let task_id = Uuid::new_v4();

    let inference_request = InferenceRequest {
        id: task_id,
        model: model_name.clone(),
        messages: messages.clone(),
        parameters: ModelParameters {
            temperature,
            max_tokens,
            top_p,
            frequency_penalty,
            presence_penalty,
            stop: stop.clone(),
        },
        priority: priority.unwrap_or(TaskPriority::Normal),
        stream: false,
        created_at: chrono::Utc::now(),
    };

    // Try to schedule to a worker
    if let Some(worker_id) = state.scheduler.select_worker(&inference_request) {
        if let Some(sender) = state.task_tracker.get_worker_sender(&worker_id) {
            let worker_name = state
                .worker_manager
                .get(&worker_id)
                .map(|w| w.name)
                .unwrap_or_else(|| "unknown".to_string());

            tracing::info!(
                "Dispatching request for model '{}' to worker '{}' ({})",
                model_name,
                worker_name,
                worker_id
            );

            let rx = state.task_tracker.register_task(task_id);

            let task_msg = WsHostMessage::TaskAssign {
                task_id,
                request: inference_request.clone(),
            };

            let msg_text = serde_json::to_string(&task_msg).map_err(|e| AppError {
                status: StatusCode::INTERNAL_SERVER_ERROR,
                message: format!("Failed to serialize task: {e}"),
            })?;

            if let Err(e) = sender.lock().await.send(axum::extract::ws::Message::Text(msg_text)).await {
                state.task_tracker.cancel_task(&task_id);
                tracing::warn!("Failed to send task to worker '{worker_name}': {e}");
            } else {
                let task_timeout = Duration::from_secs(300);
                match timeout(task_timeout, rx).await {
                    Ok(Ok(task_result)) => match task_result {
                        TaskResult::Success(response) => {
                            return Ok(Json(ChatCompletionResponse {
                                id: response.id,
                                object: "chat.completion".to_string(),
                                created: response.created_at.timestamp(),
                                model: response.model,
                                choices: response.choices,
                                usage: response.usage,
                                worker_id: Some(worker_id),
                            }));
                        }
                        TaskResult::Failed(err) => {
                            tracing::warn!("Worker '{worker_name}' failed task: {err}");
                        }
                    },
                    Ok(Err(_)) => {
                        tracing::warn!("Worker '{worker_name}' dropped the task response channel");
                    }
                    Err(_) => {
                        tracing::warn!("Task to worker '{worker_name}' timed out");
                        state.task_tracker.cancel_task(&task_id);
                    }
                }
            }
        } else {
            tracing::warn!("Worker {worker_id} has no active WebSocket sender — falling back");
        }
    }

    // Route locally via LiteLLM as fallback
    if state.route_to_local {
        let local_params = ModelParameters {
            temperature,
            max_tokens,
            top_p,
            frequency_penalty,
            presence_penalty,
            stop,
        };

        let result = state
            .litellm_client
            .chat_completion(&model_name, &messages, &local_params, false)
            .await
            .map_err(|e| AppError {
                status: StatusCode::BAD_GATEWAY,
                message: format!("LiteLLM error: {e}"),
            })?;

        return Ok(Json(ChatCompletionResponse {
            id: result.id,
            object: "chat.completion".to_string(),
            created: result.created_at.timestamp(),
            model: result.model,
            choices: result.choices,
            usage: result.usage,
            worker_id: None,
        }));
    }

    Err(AppError {
        status: StatusCode::SERVICE_UNAVAILABLE,
        message: "No workers available and local routing is disabled".to_string(),
    })
}

// ─── Streaming Path ───────────────────────────────────────────────────────────

/// Type alias for a boxed SSE event stream.
type SseEventStream = Pin<Box<dyn tokio_stream::Stream<Item = Result<Event, Infallible>> + Send>>;

async fn chat_completion_streaming(
    state: AppState,
    req: ChatCompletionRequest,
) -> Sse<SseEventStream> {
    let model_name = req.model.clone();
    let messages = req.messages.clone();
    let temperature = req.temperature;
    let max_tokens = req.max_tokens;
    let top_p = req.top_p;
    let frequency_penalty = req.frequency_penalty;
    let presence_penalty = req.presence_penalty;
    let stop = req.stop.clone();
    let priority = req.priority;

    let task_id = Uuid::new_v4();

    let inference_request = InferenceRequest {
        id: task_id,
        model: model_name.clone(),
        messages: messages.clone(),
        parameters: ModelParameters {
            temperature,
            max_tokens,
            top_p,
            frequency_penalty,
            presence_penalty,
            stop: stop.clone(),
        },
        priority: priority.unwrap_or(TaskPriority::Normal),
        stream: true,
        created_at: chrono::Utc::now(),
    };

    // Try dispatching to a worker first
    if let Some(worker_id) = state.scheduler.select_worker(&inference_request) {
        if let Some(sender) = state.task_tracker.get_worker_sender(&worker_id) {
            let worker_name = state
                .worker_manager
                .get(&worker_id)
                .map(|w| w.name)
                .unwrap_or_else(|| "unknown".to_string());

            tracing::info!(
                "Dispatching streaming request for model '{}' to worker '{}' ({})",
                model_name,
                worker_name,
                worker_id
            );

            let rx = state.task_tracker.register_streaming_task(task_id, 64);

            let task_msg = WsHostMessage::TaskAssign {
                task_id,
                request: inference_request.clone(),
            };

            let msg_text = match serde_json::to_string(&task_msg) {
                Ok(t) => t,
                Err(e) => {
                    let err_data = error_as_sse_data(&format!("Failed to serialize task: {e}"));
                    let stream: SseEventStream = Box::pin(tokio_stream::iter(vec![Ok(Event::default().data(err_data))]));
                    return Sse::new(stream).keep_alive(KeepAlive::default());
                }
            };

            match sender.lock().await.send(axum::extract::ws::Message::Text(msg_text)).await {
                Ok(()) => {
                    let stream: SseEventStream = Box::pin(
                        ReceiverStream::new(rx).map(move |event| {
                            match event {
                                StreamEvent::Chunk(data) => {
                                    Ok(Event::default().data(data))
                                }
                                StreamEvent::Done { .. } => {
                                    Ok(Event::default().data(SSE_DONE_SENTINEL.to_string()))
                                }
                                StreamEvent::Error(err) => {
                                    let data = error_as_sse_data(&err);
                                    Ok(Event::default().data(data))
                                }
                            }
                        }),
                    );

                    return Sse::new(stream).keep_alive(KeepAlive::default());
                }
                Err(e) => {
                    state.task_tracker.remove_streaming_task(&task_id);
                    tracing::warn!("Failed to send streaming task to worker '{worker_name}': {e}");
                }
            }
        }
    }

    // Fall back to local LiteLLM streaming
    let local_params = ModelParameters {
        temperature,
        max_tokens,
        top_p,
        frequency_penalty,
        presence_penalty,
        stop,
    };

    let litellm_stream = state
        .litellm_client
        .chat_completion_stream(&model_name, messages, local_params);

    let stream: SseEventStream = Box::pin(litellm_stream.map(|data| Ok(Event::default().data(data))));
    Sse::new(stream).keep_alive(KeepAlive::default())
}

/// Format an error message as an SSE data line.
fn error_as_sse_data(message: &str) -> String {
    format!(r#"{{"error":"{}"}}"#, message.replace('"', "\\\""))
}

// ─── Cluster Endpoints ────────────────────────────────────────────────────────

/// GET /cluster/nodes — List all cluster nodes
pub async fn cluster_nodes(
    State(state): State<AppState>,
) -> Json<Vec<WorkerInfo>> {
    Json(state.worker_manager.all())
}

/// GET /cluster/stats — Cluster statistics
pub async fn cluster_stats(
    State(state): State<AppState>,
) -> Json<ClusterStats> {
    let workers = state.worker_manager.online();
    let total_gpus: u32 = workers.iter().map(|w| w.resources.gpus.len() as u32).sum();
    let total_vram: u64 = workers
        .iter()
        .flat_map(|w| w.resources.gpus.iter())
        .map(|g| g.vram_total_mb)
        .sum();
    let available_vram: u64 = workers
        .iter()
        .flat_map(|w| w.resources.gpus.iter())
        .map(|g| g.vram_free_mb)
        .sum();
    let total_ram: u64 = workers.iter().map(|w| w.resources.memory.total_mb).sum();
    let available_ram: u64 = workers.iter().map(|w| w.resources.memory.available_mb).sum();
    let total_threads: u32 = workers
        .iter()
        .map(|w| w.resources.cpus.iter().map(|c| c.threads).sum::<u32>())
        .sum();
    let active_tasks: u32 = workers.iter().map(|w| w.current_tasks).sum();
    let score: f64 = workers.iter().map(|w| w.resources.compute_score()).sum();
    let all_workers = state.worker_manager.all();
    let pending_tasks = state.task_tracker.pending_count() as u32;
    let streaming_tasks = state.task_tracker.streaming_count() as u32;

    Json(ClusterStats {
        total_workers: all_workers.len(),
        online_workers: workers.len(),
        total_gpus,
        total_vram_mb: total_vram,
        available_vram_mb: available_vram,
        total_ram_mb: total_ram,
        available_ram_mb: available_ram,
        total_cpu_threads: total_threads,
        active_tasks,
        pending_tasks: pending_tasks + streaming_tasks,
        completed_tasks: 0,
        failed_tasks: 0,
        cluster_compute_score: score,
        workers: all_workers,
    })
}

/// GET /cluster/stats/min — Lightweight cluster stats for CLI display
pub async fn cluster_stats_min(State(state): State<AppState>) -> Json<serde_json::Value> {
    let online = state.worker_manager.online();
    let all = state.worker_manager.all();
    let total_vram: u64 = all
        .iter()
        .flat_map(|w| w.resources.gpus.iter())
        .map(|g| g.vram_total_mb)
        .sum();
    let available_vram: u64 = all
        .iter()
        .flat_map(|w| w.resources.gpus.iter())
        .map(|g| g.vram_free_mb)
        .sum();
    let total_ram: u64 = all.iter().map(|w| w.resources.memory.total_mb).sum();
    let available_ram: u64 = all.iter().map(|w| w.resources.memory.available_mb).sum();

    Json(serde_json::json!({
        "workers": {
            "total": all.len(),
            "online": online.len()
        },
        "resources": {
            "total_vram_mb": total_vram,
            "available_vram_mb": available_vram,
            "total_ram_mb": total_ram,
            "available_ram_mb": available_ram
        }
    }))
}

/// App error type for consistent error responses.
#[derive(Debug)]
pub struct AppError {
    pub status: StatusCode,
    pub message: String,
}

impl IntoResponse for AppError {
    fn into_response(self) -> axum::response::Response {
        let body = serde_json::json!({
            "error": {
                "message": self.message,
                "type": "error",
            }
        });
        (self.status, Json(body)).into_response()
    }
}
