use chrono::{DateTime, Utc};
use serde::{Deserialize, Serialize};

use uuid::Uuid;

// ─── Resource & Hardware Types ───────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct GpuInfo {
    pub index: u32,
    pub name: String,
    pub vram_total_mb: u64,
    pub vram_used_mb: u64,
    pub vram_free_mb: u64,
    pub utilization_percent: f32,
    pub temperature_celsius: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct CpuInfo {
    pub cores: u32,
    pub threads: u32,
    pub utilization_percent: f32,
    pub frequency_ghz: f32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct MemoryInfo {
    pub total_mb: u64,
    pub used_mb: u64,
    pub free_mb: u64,
    pub available_mb: u64,
    pub swap_total_mb: u64,
    pub swap_used_mb: u64,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct NodeResources {
    pub hostname: String,
    pub platform: String,
    pub cpus: Vec<CpuInfo>,
    pub gpus: Vec<GpuInfo>,
    pub memory: MemoryInfo,
}

impl NodeResources {
    /// Compute a "compute score" — higher means more available capacity.
    /// Used by the scheduler for load-balancing decisions.
    pub fn compute_score(&self) -> f64 {
        let mut score = 0.0;

        // GPU contribution: VRAM available * GPU count contribution
        for gpu in &self.gpus {
            let vram_ratio = if gpu.vram_total_mb > 0 {
                gpu.vram_free_mb as f64 / gpu.vram_total_mb as f64
            } else {
                0.0
            };
            let util_penalty = (100.0 - gpu.utilization_percent as f64) / 100.0;
            score += vram_ratio * util_penalty * 100.0;
        }

        // CPU contribution: available cores * inverse of utilization
        for cpu in &self.cpus {
            let util_penalty = (100.0 - cpu.utilization_percent as f64) / 100.0;
            score += cpu.threads as f64 * util_penalty * 10.0;
        }

        // Memory contribution
        let mem_ratio = if self.memory.total_mb > 0 {
            self.memory.available_mb as f64 / self.memory.total_mb as f64
        } else {
            0.0
        };
        score += mem_ratio * 50.0;

        score
    }
}

// ─── Worker Types ─────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerStatus {
    Online,
    Offline,
    Busy,
    Draining,
}

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum WorkerCapability {
    LlmInference,
    Embedding,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerInfo {
    pub id: Uuid,
    pub name: String,
    pub hostname: String,
    pub status: WorkerStatus,
    pub resources: NodeResources,
    pub capabilities: Vec<WorkerCapability>,
    pub loaded_models: Vec<String>,
    pub current_tasks: u32,
    pub max_concurrent_tasks: u32,
    pub last_heartbeat: DateTime<Utc>,
    pub connected_at: DateTime<Utc>,
    pub auth_token: Option<String>,
}

// ─── Task Types ───────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Copy, PartialEq, Eq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskStatus {
    Pending,
    Assigned,
    Running,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Debug, Clone, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskPriority {
    Low,
    Normal,
    High,
    Critical,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ModelParameters {
    pub temperature: Option<f32>,
    pub max_tokens: Option<u32>,
    pub top_p: Option<f32>,
    pub frequency_penalty: Option<f32>,
    pub presence_penalty: Option<f32>,
    pub stop: Option<Vec<String>>,
}

impl Default for ModelParameters {
    fn default() -> Self {
        Self {
            temperature: Some(0.7),
            max_tokens: Some(2048),
            top_p: Some(0.9),
            frequency_penalty: None,
            presence_penalty: None,
            stop: None,
        }
    }
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct ChatMessage {
    pub role: String,
    pub content: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InferenceRequest {
    pub id: Uuid,
    pub model: String,
    pub messages: Vec<ChatMessage>,
    pub parameters: ModelParameters,
    pub priority: TaskPriority,
    pub stream: bool,
    pub created_at: DateTime<Utc>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Usage {
    pub prompt_tokens: u32,
    pub completion_tokens: u32,
    pub total_tokens: u32,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Choice {
    pub index: u32,
    pub message: ChatMessage,
    pub finish_reason: String,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct InferenceResponse {
    pub id: String,
    pub model: String,
    pub choices: Vec<Choice>,
    pub usage: Usage,
    pub worker_id: Option<Uuid>,
    pub created_at: DateTime<Utc>,
}

// ─── Task Tracking ────────────────────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct TaskInfo {
    pub id: Uuid,
    pub inference_request: InferenceRequest,
    pub status: TaskStatus,
    pub assigned_worker: Option<Uuid>,
    pub result: Option<InferenceResponse>,
    pub error: Option<String>,
    pub created_at: DateTime<Utc>,
    pub started_at: Option<DateTime<Utc>>,
    pub completed_at: Option<DateTime<Utc>>,
    pub retry_count: u32,
}

// ─── WebSocket Protocol Messages ──────────────────────────────────────────────

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum WsHostMessage {
    /// Sent to worker: assign a task
    TaskAssign {
        task_id: Uuid,
        request: InferenceRequest,
    },
    /// Sent to worker: cancel a running task
    TaskCancel {
        task_id: Uuid,
    },
    /// Sent to worker: request to load a model
    ModelLoad {
        model: String,
    },
    /// Sent to worker: request to unload a model
    ModelUnload {
        model: String,
    },
    /// Sent to worker: acknowledge registration
    RegisterAck {
        worker_id: Uuid,
        heartbeat_interval_secs: u64,
    },
    /// Sent to worker: host is shutting down
    Shutdown {
        reason: String,
    },
}

#[derive(Debug, Clone, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case")]
pub enum WsWorkerMessage {
    /// Worker registers with the host
    Register {
        name: String,
        hostname: String,
        capabilities: Vec<WorkerCapability>,
        auth_token: Option<String>,
    },
    /// Worker heartbeat with current resource stats
    Heartbeat {
        resources: NodeResources,
        current_tasks: u32,
        loaded_models: Vec<String>,
    },
    /// Worker reports task completion
    TaskResult {
        task_id: Uuid,
        success: bool,
        result: Option<InferenceResponse>,
        error: Option<String>,
    },
    /// Worker reports task progress (for streaming)
    TaskProgress {
        task_id: Uuid,
        chunk: String,
        done: bool,
    },
}

// ─── SSE Streaming Chunk Types ───────────────────────────────────────────────

/// Delta content for a streaming chunk (OpenAI SSE format).
#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeltaMessage {
    #[serde(skip_serializing_if = "Option::is_none")]
    pub role: Option<String>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub content: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct DeltaChoice {
    pub index: u32,
    pub delta: DeltaMessage,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub finish_reason: Option<String>,
}

/// A single streaming chunk matching the OpenAI SSE chat.completion.chunk schema.
#[derive(Debug, Clone, Serialize)]
pub struct StreamChunk {
    pub id: String,
    pub object: String,
    pub created: i64,
    pub model: String,
    pub choices: Vec<DeltaChoice>,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub usage: Option<Usage>,
}

impl StreamChunk {
    /// Serialize this chunk as an SSE `data: ...` line (without the trailing `\n\n`).
    pub fn to_sse_data(&self) -> String {
        serde_json::to_string(self).unwrap_or_else(|_| "{}".to_string())
    }
}

/// SSE sentinel indicating the stream is complete.
pub const SSE_DONE_SENTINEL: &str = "[DONE]";

// ─── REST API Types ───────────────────────────────────────────────────────────

#[derive(Debug, Deserialize)]
pub struct ChatCompletionRequest {
    pub model: String,
    pub messages: Vec<ChatMessage>,
    #[serde(default)]
    pub temperature: Option<f32>,
    #[serde(default)]
    pub max_tokens: Option<u32>,
    #[serde(default)]
    pub top_p: Option<f32>,
    #[serde(default)]
    pub frequency_penalty: Option<f32>,
    #[serde(default)]
    pub presence_penalty: Option<f32>,
    #[serde(default)]
    pub stop: Option<Vec<String>>,
    #[serde(default)]
    pub stream: bool,
    #[serde(default)]
    pub priority: Option<TaskPriority>,
}

#[derive(Debug, Serialize)]
pub struct ChatCompletionResponse {
    pub id: String,
    pub object: String,
    pub created: i64,
    pub model: String,
    pub choices: Vec<Choice>,
    pub usage: Usage,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub worker_id: Option<Uuid>,
}

#[derive(Debug, Clone, Serialize)]
pub struct ModelInfo {
    pub id: String,
    pub object: String,
    pub created: i64,
    pub owned_by: String,
    #[serde(skip_serializing_if = "Option::is_none")]
    pub cached_on_workers: Option<Vec<String>>,
}

#[derive(Debug, Serialize)]
pub struct ModelsListResponse {
    pub object: String,
    pub data: Vec<ModelInfo>,
}

#[derive(Debug, Serialize)]
pub struct ClusterStats {
    pub total_workers: usize,
    pub online_workers: usize,
    pub total_gpus: u32,
    pub total_vram_mb: u64,
    pub available_vram_mb: u64,
    pub total_ram_mb: u64,
    pub available_ram_mb: u64,
    pub total_cpu_threads: u32,
    pub active_tasks: u32,
    pub pending_tasks: u32,
    pub completed_tasks: u64,
    pub failed_tasks: u64,
    pub cluster_compute_score: f64,
    pub workers: Vec<WorkerInfo>,
}

#[derive(Debug, Serialize)]
pub struct HealthResponse {
    pub status: String,
    pub version: String,
    pub uptime_seconds: u64,
    pub mode: String,
    pub workers_connected: usize,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_delta_message_role_only() {
        let msg = DeltaMessage {
            role: Some("assistant".to_string()),
            content: None,
        };
        let json = serde_json::to_string(&msg).unwrap();
        assert_eq!(json, r#"{"role":"assistant"}"#);
    }

    #[test]
    fn test_delta_message_content_only() {
        let msg = DeltaMessage {
            role: None,
            content: Some("Hello".to_string()),
        };
        let json = serde_json::to_string(&msg).unwrap();
        assert_eq!(json, r#"{"content":"Hello"}"#);
    }

    #[test]
    fn test_delta_message_both() {
        let msg = DeltaMessage {
            role: Some("assistant".to_string()),
            content: Some("Hello world".to_string()),
        };
        let json = serde_json::to_string(&msg).unwrap();
        assert!(json.contains(r#""role":"assistant""#));
        assert!(json.contains(r#""content":"Hello world""#));
    }

    #[test]
    fn test_delta_choice_with_finish_reason() {
        let choice = DeltaChoice {
            index: 0,
            delta: DeltaMessage {
                role: None,
                content: Some("Hello".to_string()),
            },
            finish_reason: Some("stop".to_string()),
        };
        let json = serde_json::to_string(&choice).unwrap();
        assert!(json.contains(r#""finish_reason":"stop""#));
        assert!(json.contains(r#""content":"Hello""#));
    }

    #[test]
    fn test_delta_choice_without_finish_reason() {
        let choice = DeltaChoice {
            index: 0,
            delta: DeltaMessage {
                role: None,
                content: Some("Hello".to_string()),
            },
            finish_reason: None,
        };
        let json = serde_json::to_string(&choice).unwrap();
        assert!(!json.contains("finish_reason"));
    }

    #[test]
    fn test_stream_chunk_to_sse_data() {
        let chunk = StreamChunk {
            id: "chatcmpl-123".to_string(),
            object: "chat.completion.chunk".to_string(),
            created: 1700000000,
            model: "gpt-4".to_string(),
            choices: vec![
                DeltaChoice {
                    index: 0,
                    delta: DeltaMessage {
                        role: None,
                        content: Some("Hello".to_string()),
                    },
                    finish_reason: None,
                },
            ],
            usage: None,
        };

        let sse = chunk.to_sse_data();
        let parsed: serde_json::Value = serde_json::from_str(&sse).unwrap();
        assert_eq!(parsed["id"], "chatcmpl-123");
        assert_eq!(parsed["object"], "chat.completion.chunk");
        assert_eq!(parsed["model"], "gpt-4");
        assert_eq!(parsed["choices"][0]["delta"]["content"], "Hello");
        assert!(parsed["usage"].is_null());
    }

    #[test]
    fn test_stream_chunk_with_usage() {
        let chunk = StreamChunk {
            id: "chatcmpl-456".to_string(),
            object: "chat.completion.chunk".to_string(),
            created: 1700000001,
            model: "gpt-4".to_string(),
            choices: vec![
                DeltaChoice {
                    index: 0,
                    delta: DeltaMessage {
                        role: None,
                        content: None,
                    },
                    finish_reason: Some("stop".to_string()),
                },
            ],
            usage: Some(Usage {
                prompt_tokens: 10,
                completion_tokens: 20,
                total_tokens: 30,
            }),
        };

        let sse = chunk.to_sse_data();
        let parsed: serde_json::Value = serde_json::from_str(&sse).unwrap();
        assert_eq!(parsed["usage"]["prompt_tokens"], 10);
        assert_eq!(parsed["usage"]["completion_tokens"], 20);
        assert_eq!(parsed["usage"]["total_tokens"], 30);
        assert_eq!(parsed["choices"][0]["finish_reason"], "stop");
    }

    #[test]
    fn test_stream_chunk_valid_json() {
        let chunk = StreamChunk {
            id: String::new(),
            object: "chat.completion.chunk".to_string(),
            created: 0,
            model: String::new(),
            choices: vec![],
            usage: None,
        };
        // Should always produce valid JSON
        let sse = chunk.to_sse_data();
        assert!(serde_json::from_str::<serde_json::Value>(&sse).is_ok());
    }

    #[test]
    fn test_sse_done_sentinel() {
        assert_eq!(SSE_DONE_SENTINEL, "[DONE]");
    }

    #[test]
    fn test_stream_chunk_roundtrip() {
        let chunk = StreamChunk {
            id: "test-id".to_string(),
            object: "chat.completion.chunk".to_string(),
            created: 1234567890,
            model: "test-model".to_string(),
            choices: vec![
                DeltaChoice {
                    index: 0,
                    delta: DeltaMessage {
                        role: Some("assistant".to_string()),
                        content: Some("Hello".to_string()),
                    },
                    finish_reason: Some("stop".to_string()),
                },
            ],
            usage: Some(Usage {
                prompt_tokens: 5,
                completion_tokens: 10,
                total_tokens: 15,
            }),
        };

        let json = chunk.to_sse_data();
        let parsed: serde_json::Value = serde_json::from_str(&json).unwrap();

        assert_eq!(parsed["id"], "test-id");
        assert_eq!(parsed["object"], "chat.completion.chunk");
        assert_eq!(parsed["created"], 1234567890);
        assert_eq!(parsed["model"], "test-model");
        assert_eq!(parsed["choices"][0]["index"], 0);
        assert_eq!(parsed["choices"][0]["delta"]["role"], "assistant");
        assert_eq!(parsed["choices"][0]["delta"]["content"], "Hello");
        assert_eq!(parsed["choices"][0]["finish_reason"], "stop");
        assert_eq!(parsed["usage"]["prompt_tokens"], 5);
        assert_eq!(parsed["usage"]["completion_tokens"], 10);
        assert_eq!(parsed["usage"]["total_tokens"], 15);
    }
}
