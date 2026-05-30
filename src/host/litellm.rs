use crate::common::types::*;
use anyhow::{Context, Result};
use futures::StreamExt as _;
use reqwest::Client;
use serde::{Deserialize, Serialize};
use std::time::Duration;
use tokio_stream::wrappers::UnboundedReceiverStream;

/// Parse SSE events from accumulated byte data in a streaming fashion.
///
/// Takes a mutable buffer (to handle partial events across chunks) and a new
/// chunk of text data. Returns any complete `data:` lines found.
///
/// After the final chunk, call [`flush_sse_buffer`] to drain any remaining
/// data (if no trailing `\n\n` was sent).
pub fn parse_sse_chunk(buf: &mut String, chunk: &str) -> Vec<String> {
    buf.push_str(chunk);
    let mut results = Vec::new();
    while let Some(pos) = buf.find("\n\n") {
        let event_text = buf[..pos].to_string();
        *buf = buf[pos + 2..].to_string();
        for line in event_text.lines() {
            if let Some(data) = line.strip_prefix("data: ") {
                results.push(data.trim().to_string());
            }
        }
    }
    results
}

/// Flush any remaining data in the SSE buffer as a data line (if present).
pub fn flush_sse_buffer(buf: &mut String) -> Option<String> {
    if buf.is_empty() {
        return None;
    }
    let remaining = buf.trim().to_string();
    buf.clear();
    if remaining.is_empty() {
        return None;
    }
    // Try stripping "data: " prefix (with or without trailing space after trimming)
    if let Some(data) = remaining
        .strip_prefix("data: ")
        .or_else(|| remaining.strip_prefix("data:"))
    {
        let trimmed = data.trim();
        if trimmed.is_empty() {
            None
        } else {
            Some(trimmed.to_string())
        }
    } else {
        Some(remaining)
    }
}

/// Client for communicating with a LiteLLM proxy server.
#[derive(Debug, Clone)]
pub struct LiteLlmClient {
    client: Client,
    base_url: String,
    api_key: Option<String>,
}

#[derive(Debug, Deserialize)]
struct LiteLlmModelList {
    object: String,
    data: Vec<LiteLlmModel>,
}

#[derive(Debug, Deserialize)]
struct LiteLlmModel {
    id: String,
    object: String,
    created: i64,
    owned_by: String,
}

#[derive(Debug, Serialize)]
struct LiteLlmChatRequest {
    model: String,
    messages: Vec<ChatMessage>,
    #[serde(skip_serializing_if = "Option::is_none")]
    temperature: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    max_tokens: Option<u32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    top_p: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    frequency_penalty: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    presence_penalty: Option<f32>,
    #[serde(skip_serializing_if = "Option::is_none")]
    stop: Option<Vec<String>>,
    #[serde(skip_serializing_if = "is_false")]
    stream: bool,
}

fn is_false(b: &bool) -> bool {
    !*b
}

#[derive(Debug, Deserialize)]
struct LiteLlmChatResponse {
    id: String,
    object: String,
    created: i64,
    model: String,
    choices: Vec<LiteLlmChoice>,
    usage: LiteLlmUsage,
}

#[derive(Debug, Deserialize)]
struct LiteLlmChoice {
    index: u32,
    message: LiteLlmMessage,
    finish_reason: String,
}

#[derive(Debug, Deserialize)]
struct LiteLlmMessage {
    role: String,
    content: String,
}

#[derive(Debug, Deserialize)]
struct LiteLlmUsage {
    prompt_tokens: u32,
    completion_tokens: u32,
    total_tokens: u32,
}

impl LiteLlmClient {
    pub fn new(base_url: &str, api_key: Option<String>) -> Self {
        let client = Client::builder()
            .timeout(Duration::from_secs(300))
            .connect_timeout(Duration::from_secs(10))
            .build()
            .expect("Failed to create HTTP client");

        Self {
            client,
            base_url: base_url.trim_end_matches('/').to_string(),
            api_key,
        }
    }

    fn auth_header(&self) -> Option<(&'static str, String)> {
        self.api_key
            .as_ref()
            .map(|key| ("Authorization", format!("Bearer {}", key)))
    }

    /// List available models from LiteLLM.
    pub async fn list_models(&self) -> Result<Vec<ModelInfo>> {
        let url = format!("{}/v1/models", self.base_url);
        let mut req = self.client.get(&url);
        if let Some((k, v)) = self.auth_header() {
            req = req.header(k, v);
        }

        let resp = req
            .send()
            .await
            .context("Failed to fetch models from LiteLLM")?;

        if !resp.status().is_success() {
            let status = resp.status();
            let body = resp.text().await.unwrap_or_default();
            anyhow::bail!("LiteLLM returned {status}: {body}");
        }

        let models: LiteLlmModelList = resp
            .json()
            .await
            .context("Failed to parse LiteLLM model list")?;

        Ok(models
            .data
            .into_iter()
            .map(|m| ModelInfo {
                id: m.id,
                object: "model".to_string(),
                created: m.created,
                owned_by: m.owned_by,
                cached_on_workers: None,
            })
            .collect())
    }

    /// Send a (non-streaming) chat completion request to LiteLLM.
    pub async fn chat_completion(
        &self,
        model: &str,
        messages: &[ChatMessage],
        params: &ModelParameters,
        stream: bool,
    ) -> Result<InferenceResponse> {
        let url = format!("{}/v1/chat/completions", self.base_url);
        let body = LiteLlmChatRequest {
            model: model.to_string(),
            messages: messages.to_vec(),
            temperature: params.temperature,
            max_tokens: params.max_tokens,
            top_p: params.top_p,
            frequency_penalty: params.frequency_penalty,
            presence_penalty: params.presence_penalty,
            stop: params.stop.clone(),
            stream,
        };

        let mut req = self.client.post(&url).json(&body);
        if let Some((k, v)) = self.auth_header() {
            req = req.header(k, v);
        }

        let resp = req
            .send()
            .await
            .context("Failed to send request to LiteLLM")?;

        if !resp.status().is_success() {
            let status = resp.status();
            let resp_body = resp.text().await.unwrap_or_default();
            anyhow::bail!("LiteLLM returned {status}: {resp_body}");
        }

        let llm_resp: LiteLlmChatResponse = resp
            .json()
            .await
            .context("Failed to parse LiteLLM response")?;

        Ok(InferenceResponse {
            id: llm_resp.id,
            model: llm_resp.model,
            choices: llm_resp
                .choices
                .into_iter()
                .map(|c| Choice {
                    index: c.index,
                    message: ChatMessage {
                        role: c.message.role,
                        content: c.message.content,
                    },
                    finish_reason: c.finish_reason,
                })
                .collect(),
            usage: Usage {
                prompt_tokens: llm_resp.usage.prompt_tokens,
                completion_tokens: llm_resp.usage.completion_tokens,
                total_tokens: llm_resp.usage.total_tokens,
            },
            worker_id: None,
            created_at: chrono::Utc::now(),
        })
    }

    /// Send a streaming chat completion request to LiteLLM and return an
    /// `UnboundedReceiverStream` of SSE data lines (the JSON after `data: `).
    ///
    /// Each item is a JSON-serialized [`StreamChunk`] matching the OpenAI SSE format.
    /// The stream ends when `[DONE]` is received.
    pub fn chat_completion_stream(
        &self,
        model: &str,
        messages: Vec<ChatMessage>,
        params: ModelParameters,
    ) -> UnboundedReceiverStream<String> {
        let (tx, rx) = tokio::sync::mpsc::unbounded_channel();

        let url = format!("{}/v1/chat/completions", self.base_url);
        let body = LiteLlmChatRequest {
            model: model.to_string(),
            messages,
            temperature: params.temperature,
            max_tokens: params.max_tokens,
            top_p: params.top_p,
            frequency_penalty: params.frequency_penalty,
            presence_penalty: params.presence_penalty,
            stop: params.stop.clone(),
            stream: true,
        };

        let mut req = self.client.post(&url).json(&body);
        if let Some((k, v)) = self.auth_header() {
            req = req.header(k, v);
        }

        let client = self.client.clone();

        // Spawn a background task to read the streaming response
        tokio::spawn(async move {
            let resp = match req.send().await {
                Ok(r) => r,
                Err(e) => {
                    let _ = tx.send(format!(r#"{{"error":"LiteLLM request failed: {e}"}}"#));
                    return;
                }
            };

            if !resp.status().is_success() {
                let status = resp.status();
                let body = resp.text().await.unwrap_or_default();
                let _ = tx.send(format!(r#"{{"error":"LiteLLM returned {status}: {body}"}}"#));
                return;
            }

            let mut byte_stream = resp.bytes_stream();
            let mut buf = String::new();

            while let Some(chunk_result) = futures::StreamExt::next(&mut byte_stream).await {
                match chunk_result {
                    Ok(bytes) => {
                        let text = String::from_utf8_lossy(&bytes);
                        let lines = parse_sse_chunk(&mut buf, &text);
                        for data in lines {
                            if data == "[DONE]" {
                                let _ = tx.send(SSE_DONE_SENTINEL.to_string());
                                return;
                            }
                            if tx.send(data).is_err() {
                                return;
                            }
                        }
                    }
                    Err(e) => {
                        let _ = tx.send(format!(r#"{{"error":"Stream read error: {e}"}}"#));
                        return;
                    }
                }
            }

            // Flush any remaining data and check for [DONE]
            if let Some(remaining) = flush_sse_buffer(&mut buf) {
                if remaining != "[DONE]" {
                    let _ = tx.send(remaining);
                }
            }
            let _ = tx.send(SSE_DONE_SENTINEL.to_string());
        });

        UnboundedReceiverStream::new(rx)
    }

    /// Check if LiteLLM is reachable.
    pub async fn health_check(&self) -> Result<bool> {
        let url = format!("{}/health", self.base_url);
        match self.client.get(&url).send().await {
            Ok(resp) => Ok(resp.status().is_success()),
            Err(e) => {
                tracing::warn!("LiteLLM health check failed: {e}");
                Ok(false)
            }
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_sse_single_event() {
        let mut buf = String::new();
        let chunk = "data: {\"key\": \"value\"}\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "{\"key\": \"value\"}");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_multiple_events() {
        let mut buf = String::new();
        let chunk = "data: chunk1\n\ndata: chunk2\n\ndata: chunk3\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 3);
        assert_eq!(lines[0], "chunk1");
        assert_eq!(lines[1], "chunk2");
        assert_eq!(lines[2], "chunk3");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_split_across_chunks() {
        let mut buf = String::new();

        // First chunk: partial event (no \n\n yet)
        let lines = parse_sse_chunk(&mut buf, "data: hello");
        assert!(lines.is_empty());
        assert_eq!(buf, "data: hello");

        // Second chunk: completes the event
        let lines = parse_sse_chunk(&mut buf, " world\n\n");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "hello world");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_multiple_events_split_across_chunks() {
        let mut buf = String::new();

        // First event complete, second event partial
        let lines = parse_sse_chunk(&mut buf, "data: first\n\ndata: sec");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "first");
        assert_eq!(buf, "data: sec");

        // Complete the second event
        let lines = parse_sse_chunk(&mut buf, "ond\n\n");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "second");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_with_done_sentinel() {
        let mut buf = String::new();
        let chunk = format!("data: hello\n\ndata: [DONE]\n\n");
        let lines = parse_sse_chunk(&mut buf, &chunk);
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0], "hello");
        assert_eq!(lines[1], "[DONE]");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_ignores_non_data_lines() {
        let mut buf = String::new();
        let chunk = "event: message\ndata: important\nid: 123\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "important");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_empty_chunk() {
        let mut buf = String::new();
        let lines = parse_sse_chunk(&mut buf, "");
        assert!(lines.is_empty());
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_buffer_reuse() {
        // Simulate reusing the same buffer across multiple calls
        let mut buf = String::new();

        // Partial event
        let lines = parse_sse_chunk(&mut buf, "data: tok");
        assert!(lines.is_empty());

        // Another partial
        let lines = parse_sse_chunk(&mut buf, "en1\n\n");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "token1");
        assert!(buf.is_empty());

        // New event
        let lines = parse_sse_chunk(&mut buf, "data: token2\n\n");
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "token2");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_flush_sse_buffer_remaining_data() {
        let mut buf = String::new();
        // Push data without trailing \n\n
        parse_sse_chunk(&mut buf, "data: hello");
        assert!(!buf.is_empty());

        // Flush should extract the remaining data
        let remaining = flush_sse_buffer(&mut buf);
        assert_eq!(remaining, Some("hello".to_string()));
        assert!(buf.is_empty());
    }

    #[test]
    fn test_flush_sse_buffer_empty() {
        let mut buf = String::new();
        let remaining = flush_sse_buffer(&mut buf);
        assert!(remaining.is_none());
    }

    #[test]
    fn test_flush_sse_buffer_just_data_prefix() {
        let mut buf = String::new();
        buf.push_str("data: ");
        let remaining = flush_sse_buffer(&mut buf);
        assert!(remaining.is_none());
    }

    #[test]
    fn test_flush_sse_buffer_done_sentinel() {
        let mut buf = String::new();
        buf.push_str("data: [DONE]");
        let remaining = flush_sse_buffer(&mut buf);
        assert_eq!(remaining, Some("[DONE]".to_string()));
    }

    #[test]
    fn test_parse_sse_empty_data_line() {
        let mut buf = String::new();
        // Some SSE endpoints send empty data lines
        let chunk = "data: \n\ndata: hello\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0], "");
        assert_eq!(lines[1], "hello");
    }

    #[test]
    fn test_parse_sse_json_with_newlines() {
        let mut buf = String::new();
        // JSON data with embedded newlines should be handled
        let chunk = "data: {\"text\":\"line1\\nline2\"}\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 1);
        assert!(lines[0].contains("line1"));
        assert!(lines[0].contains("line2"));
    }

    #[test]
    fn test_parse_sse_trailing_whitespace_stripped() {
        let mut buf = String::new();
        let chunk = "data: hello world  \n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 1);
        assert_eq!(lines[0], "hello world");
    }

    #[test]
    fn test_parse_sse_multiple_newlines_between_events() {
        let mut buf = String::new();
        let chunk = "data: first\n\n\n\ndata: second\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert_eq!(lines.len(), 2);
        assert_eq!(lines[0], "first");
        assert_eq!(lines[1], "second");
        assert!(buf.is_empty());
    }

    #[test]
    fn test_parse_sse_no_data_prefix() {
        let mut buf = String::new();
        // Lines without 'data: ' prefix should be ignored
        let chunk = "event: message\n:comment\n\n";
        let lines = parse_sse_chunk(&mut buf, chunk);
        assert!(lines.is_empty());
    }
}
