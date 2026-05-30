use crate::common::types::*;
use crate::config::WorkerConfig;
use crate::worker::resources::ResourceCollector;
use anyhow::{Context, Result};
use futures::{SinkExt, StreamExt};
use tokio::time::{interval, Duration};
use tokio_tungstenite::connect_async;
use tokio_tungstenite::MaybeTlsStream;
use tracing::{error, info, warn};
use url::Url;

type WsStream =
    tokio_tungstenite::WebSocketStream<MaybeTlsStream<tokio::net::TcpStream>>;

/// The worker agent that connects to the nmonit host and contributes compute.
pub struct WorkerAgent {
    config: WorkerConfig,
    resources: ResourceCollector,
}

impl WorkerAgent {
    pub fn new(config: WorkerConfig) -> Self {
        Self {
            config,
            resources: ResourceCollector,
        }
    }

    /// Run the worker agent — connects to host and processes messages.
    pub async fn run(&self) -> Result<()> {
        let ws_url = format!(
            "ws://{}:{}/ws/worker",
            self.config.host_addr, self.config.host_port
        );
        info!("Connecting to host at {ws_url}");

        let url = Url::parse(&ws_url).context("Invalid WebSocket URL")?;
        let (ws_stream, _) = connect_async(url)
            .await
            .context("Failed to connect to host")?;

        info!("Connected to host, registering...");

        let (mut sender, mut receiver) = ws_stream.split();

        // Register with the host
        let hostname = ResourceCollector::collect_all()
            .map(|r| r.hostname)
            .unwrap_or_else(|_| "unknown".to_string());

        let name = self
            .config
            .name
            .clone()
            .unwrap_or_else(|| hostname.clone());

        let register_msg = WsWorkerMessage::Register {
            name: name.clone(),
            hostname,
            capabilities: vec![WorkerCapability::LlmInference],
            auth_token: self.config.auth_token.clone(),
        };

        sender
            .send(
                tokio_tungstenite::tungstenite::Message::Text(
                    serde_json::to_string(&register_msg).unwrap(),
                ),
            )
            .await
            .context("Failed to send registration")?;

        // Wait for registration acknowledgment
        let mut worker_id = None;
        let mut heartbeat_interval = Duration::from_secs(self.config.heartbeat_interval_secs);

        // Main message loop
        let mut heartbeat_timer = interval(heartbeat_interval);

        loop {
            tokio::select! {
                _ = heartbeat_timer.tick() => {
                    // Send heartbeat with current resource stats
                    match ResourceCollector::collect_all() {
                        Ok(resources) => {
                            let heartbeat = WsWorkerMessage::Heartbeat {
                                resources,
                                current_tasks: 0,
                                loaded_models: Vec::new(),
                            };
                            if let Err(e) = sender.send(
                                tokio_tungstenite::tungstenite::Message::Text(
                                    serde_json::to_string(&heartbeat).unwrap(),
                                ),
                            ).await {
                                warn!("Failed to send heartbeat: {e}");
                                break;
                            }
                        }
                        Err(e) => {
                            warn!("Failed to collect resources: {e}");
                        }
                    }
                }
                msg = receiver.next() => {
                    match msg {
                        Some(Ok(tokio_tungstenite::tungstenite::Message::Text(text))) => {
                            if let Err(e) = self.handle_host_message(&text, &mut sender, &mut worker_id, &mut heartbeat_interval).await {
                                warn!("Error handling host message: {e}");
                            }
                        }
                        Some(Ok(tokio_tungstenite::tungstenite::Message::Close(_))) | None => {
                            info!("Host closed the connection");
                            break;
                        }
                        Some(Err(e)) => {
                            error!("WebSocket error: {e}");
                            break;
                        }
                        _ => {}
                    }
                }
            }
        }

        info!("Worker '{name}' disconnected from host");
        Ok(())
    }

    async fn handle_host_message(
        &self,
        text: &str,
        sender: &mut futures::stream::SplitSink<
            WsStream,
            tokio_tungstenite::tungstenite::Message,
        >,
        worker_id: &mut Option<uuid::Uuid>,
        heartbeat_interval: &mut Duration,
    ) -> Result<()> {
        let msg: WsHostMessage = serde_json::from_str(text)?;

        match msg {
            WsHostMessage::RegisterAck {
                worker_id: id,
                heartbeat_interval_secs,
            } => {
                *worker_id = Some(id);
                *heartbeat_interval = Duration::from_secs(heartbeat_interval_secs);
                info!("Registered with host as worker {id}");
            }
            WsHostMessage::TaskAssign { task_id, request } => {
                info!("Received task {task_id}: model={}", request.model);
                self.execute_task(task_id, request, sender).await?;
            }
            WsHostMessage::TaskCancel { task_id } => {
                info!("Task {task_id} cancelled by host");
            }
            WsHostMessage::ModelLoad { model } => {
                info!("Host requested loading model: {model}");
                // TODO: Implement model loading via LiteLLM if available
            }
            WsHostMessage::ModelUnload { model } => {
                info!("Host requested unloading model: {model}");
            }
            WsHostMessage::Shutdown { reason } => {
                info!("Host shutdown requested: {reason}");
                // This will cause the main loop to exit
                std::process::exit(0);
            }
        }

        Ok(())
    }

    async fn execute_task(
        &self,
        task_id: uuid::Uuid,
        request: InferenceRequest,
        sender: &mut futures::stream::SplitSink<
            WsStream,
            tokio_tungstenite::tungstenite::Message,
        >,
    ) -> Result<()> {
        if request.stream {
            self.execute_task_streaming(task_id, request, sender).await
        } else {
            self.execute_task_non_streaming(task_id, request, sender).await
        }
    }

    /// Execute a non-streaming inference task via local LiteLLM.
    async fn execute_task_non_streaming(
        &self,
        task_id: uuid::Uuid,
        request: InferenceRequest,
        sender: &mut futures::stream::SplitSink<
            WsStream,
            tokio_tungstenite::tungstenite::Message,
        >,
    ) -> Result<()> {
        let result = if let Some(litellm_url) = &self.config.litellm_base_url {
            let client = crate::host::litellm::LiteLlmClient::new(litellm_url, None);
            match client
                .chat_completion(
                    &request.model,
                    &request.messages,
                    &request.parameters,
                    false,
                )
                .await
            {
                Ok(resp) => {
                    info!("Task {task_id} completed via local LiteLLM");
                    (true, Some(resp), None)
                }
                Err(e) => {
                    warn!(
                        "Local LiteLLM failed for task {task_id}: {e}. Reporting failure."
                    );
                    (false, None, Some(e.to_string()))
                }
            }
        } else {
            (
                false,
                None,
                Some("No LiteLLM endpoint configured on worker".to_string()),
            )
        };

        let result_msg = WsWorkerMessage::TaskResult {
            task_id,
            success: result.0,
            result: result.1,
            error: result.2,
        };

        sender
            .send(tokio_tungstenite::tungstenite::Message::Text(
                serde_json::to_string(&result_msg).unwrap(),
            ))
            .await
            .context("Failed to send task result")?;

        info!("Task {task_id} result sent to host");
        Ok(())
    }

    /// Execute a streaming inference task via local LiteLLM, sending
    /// `TaskProgress` messages for each chunk and a final `TaskResult`.
    async fn execute_task_streaming(
        &self,
        task_id: uuid::Uuid,
        request: InferenceRequest,
        sender: &mut futures::stream::SplitSink<
            WsStream,
            tokio_tungstenite::tungstenite::Message,
        >,
    ) -> Result<()> {
        let litellm_url = match &self.config.litellm_base_url {
            Some(url) => url.clone(),
            None => {
                let err = "No LiteLLM endpoint configured on worker for streaming".to_string();
                let result_msg = WsWorkerMessage::TaskResult {
                    task_id,
                    success: false,
                    result: None,
                    error: Some(err.clone()),
                };
                sender
                    .send(tokio_tungstenite::tungstenite::Message::Text(
                        serde_json::to_string(&result_msg).unwrap(),
                    ))
                    .await?;
                return Ok(());
            }
        };

        let client = crate::host::litellm::LiteLlmClient::new(&litellm_url, None);
        let stream = client.chat_completion_stream(
            &request.model,
            request.messages,
            request.parameters,
        );

        info!("Task {task_id} streaming via local LiteLLM");

        let mut last_chunk_json = String::new();

        // Use tokio::pin! to pin the stream
        tokio::pin!(stream);

        while let Some(data) = stream.next().await {
            // Check if this is the [DONE] sentinel
            if data == crate::common::types::SSE_DONE_SENTINEL {
                // Send final TaskProgress with done=true
                let progress = WsWorkerMessage::TaskProgress {
                    task_id,
                    chunk: last_chunk_json.clone(),
                    done: true,
                };
                if let Err(e) = sender
                    .send(tokio_tungstenite::tungstenite::Message::Text(
                        serde_json::to_string(&progress).unwrap(),
                    ))
                    .await
                {
                    warn!("Failed to send streaming progress for {task_id}: {e}");
                    break;
                }

                // Also send TaskResult to signal completion with usage
                // Parse the last chunk to extract usage if present
                let mut final_response = None;
                if let Ok(chunk) = serde_json::from_str::<serde_json::Value>(&last_chunk_json) {
                    if let Some(choices) = chunk.get("choices").and_then(|c| c.as_array()) {
                        if let Some(last_choice) = choices.last() {
                            let content = last_choice
                                .get("delta")
                                .and_then(|d| d.get("content"))
                                .and_then(|c| c.as_str())
                                .unwrap_or("")
                                .to_string();
                            let finish_reason = last_choice
                                .get("finish_reason")
                                .and_then(|r| r.as_str())
                                .unwrap_or("stop")
                                .to_string();
                            let usage = chunk.get("usage").and_then(|u| {
                                Some(Usage {
                                    prompt_tokens: u.get("prompt_tokens").and_then(|v| v.as_u64()).unwrap_or(0) as u32,
                                    completion_tokens: u.get("completion_tokens").and_then(|v| v.as_u64()).unwrap_or(0) as u32,
                                    total_tokens: u.get("total_tokens").and_then(|v| v.as_u64()).unwrap_or(0) as u32,
                                })
                            });

                            let model = chunk.get("model").and_then(|m| m.as_str()).unwrap_or("unknown").to_string();
                            let response_id = chunk.get("id").and_then(|id| id.as_str()).unwrap_or("unknown").to_string();

                            final_response = Some(InferenceResponse {
                                id: response_id,
                                model,
                                choices: vec![Choice {
                                    index: 0,
                                    message: ChatMessage {
                                        role: "assistant".to_string(),
                                        content,
                                    },
                                    finish_reason,
                                }],
                                usage: usage.unwrap_or(Usage {
                                    prompt_tokens: 0,
                                    completion_tokens: 0,
                                    total_tokens: 0,
                                }),
                                worker_id: None,
                                created_at: chrono::Utc::now(),
                            });
                        }
                    }
                }

                let result_msg = WsWorkerMessage::TaskResult {
                    task_id,
                    success: true,
                    result: final_response,
                    error: None,
                };
                if let Err(e) = sender
                    .send(tokio_tungstenite::tungstenite::Message::Text(
                        serde_json::to_string(&result_msg).unwrap(),
                    ))
                    .await
                {
                    warn!("Failed to send streaming result for {task_id}: {e}");
                }
                break;
            }

            // Store the last chunk JSON (for the final progress message)
            last_chunk_json = data.clone();

            // Send streaming progress
            let progress = WsWorkerMessage::TaskProgress {
                task_id,
                chunk: data,
                done: false,
            };

            if let Err(e) = sender
                .send(tokio_tungstenite::tungstenite::Message::Text(
                    serde_json::to_string(&progress).unwrap(),
                ))
                .await
            {
                warn!("Failed to send streaming progress for {task_id}: {e}");
                break;
            }
        }

        info!("Task {task_id} streaming completed");
        Ok(())
    }
}
