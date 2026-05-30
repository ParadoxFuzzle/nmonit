use crate::common::types::InferenceResponse;
use axum::extract::ws::Message;
use dashmap::DashMap;
use futures::stream::SplitSink;
use std::sync::Arc;
use tokio::sync::{Mutex, mpsc, oneshot};
use uuid::Uuid;

/// Result of a worker task execution.
#[derive(Debug)]
pub enum TaskResult {
    Success(InferenceResponse),
    Failed(String),
}

/// An event in a streaming inference task.
#[derive(Debug, Clone)]
pub enum StreamEvent {
    /// A single token/data chunk (the JSON body of the SSE `data:` line).
    Chunk(String),
    /// The stream is finished — includes the final chunk with usage info.
    Done {
        final_chunk: String,
    },
    /// An error occurred during streaming.
    Error(String),
}

/// Tracks pending tasks and worker WebSocket senders,
/// enabling the HTTP handler to dispatch tasks to workers
/// and await their results (or stream chunks) asynchronously.
#[derive(Debug, Clone)]
pub struct TaskTracker {
    /// Map of task_id -> oneshot sender for the (non-streaming) result
    pending_tasks: Arc<DashMap<Uuid, oneshot::Sender<TaskResult>>>,
    /// Map of task_id -> mpsc sender for streaming task events
    streaming_tasks: Arc<DashMap<Uuid, mpsc::Sender<StreamEvent>>>,
    /// Map of worker_id -> WebSocket sender for sending tasks
    worker_senders: Arc<DashMap<Uuid, Arc<Mutex<SplitSink<axum::extract::ws::WebSocket, Message>>>>>,
}

impl TaskTracker {
    pub fn new() -> Self {
        Self {
            pending_tasks: Arc::new(DashMap::new()),
            streaming_tasks: Arc::new(DashMap::new()),
            worker_senders: Arc::new(DashMap::new()),
        }
    }

    /// Register a worker's WebSocket sender so tasks can be dispatched to it.
    pub fn register_worker(&self, worker_id: Uuid, sender: SplitSink<axum::extract::ws::WebSocket, Message>) {
        self.worker_senders.insert(worker_id, Arc::new(Mutex::new(sender)));
    }

    /// Remove a worker's sender (on disconnect).
    pub fn remove_worker(&self, worker_id: &Uuid) {
        self.worker_senders.remove(worker_id);
    }

    /// Get a worker's sender to dispatch a task.
    pub fn get_worker_sender(&self, worker_id: &Uuid) -> Option<Arc<Mutex<SplitSink<axum::extract::ws::WebSocket, Message>>>> {
        self.worker_senders.get(worker_id).map(|s| s.clone())
    }

    /// Register a pending (non-streaming) task and return the oneshot receiver to await the result.
    pub fn register_task(&self, task_id: Uuid) -> oneshot::Receiver<TaskResult> {
        let (tx, rx) = oneshot::channel();
        self.pending_tasks.insert(task_id, tx);
        rx
    }

    /// Complete a (non-streaming) task — called by the WebSocket handler when a worker returns a result.
    pub fn complete_task(&self, task_id: &Uuid, result: TaskResult) {
        if let Some((_, tx)) = self.pending_tasks.remove(task_id) {
            let _ = tx.send(result);
        }
    }

    /// Register a streaming task and return the mpsc receiver to stream chunks from.
    pub fn register_streaming_task(&self, task_id: Uuid, buffer: usize) -> mpsc::Receiver<StreamEvent> {
        let (tx, rx) = mpsc::channel(buffer);
        self.streaming_tasks.insert(task_id, tx);
        rx
    }

    /// Push a streaming chunk into the task's channel.
    /// Logs a warning if the channel is full or closed.
    pub fn push_stream_chunk(&self, task_id: &Uuid, event: StreamEvent) {
        if let Some(tx) = self.streaming_tasks.get(task_id) {
            if let Err(e) = tx.try_send(event) {
                tracing::warn!("Failed to push stream chunk for task {task_id}: {e}");
            }
        }
    }

    /// Remove a streaming task (called when the stream is done or cancelled).
    pub fn remove_streaming_task(&self, task_id: &Uuid) {
        self.streaming_tasks.remove(task_id);
    }

    /// Check if a task is still pending (non-streaming).
    pub fn is_pending(&self, task_id: &Uuid) -> bool {
        self.pending_tasks.contains_key(task_id)
    }

    /// Cancel a pending (non-streaming) task.
    pub fn cancel_task(&self, task_id: &Uuid) {
        self.pending_tasks.remove(task_id);
        self.streaming_tasks.remove(task_id);
    }

    /// Number of pending (non-streaming) tasks.
    pub fn pending_count(&self) -> usize {
        self.pending_tasks.len()
    }

    /// Number of active streaming tasks.
    pub fn streaming_count(&self) -> usize {
        self.streaming_tasks.len()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use tokio_stream::wrappers::ReceiverStream;
    use tokio_stream::StreamExt;

    #[tokio::test]
    async fn test_register_streaming_task() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        let mut rx = tracker.register_streaming_task(task_id, 16);
        assert!(tracker.streaming_count() == 1);

        // Push a chunk
        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("hello".to_string()));

        // Receive it
        let event = rx.recv().await.unwrap();
        match event {
            StreamEvent::Chunk(data) => assert_eq!(data, "hello"),
            _ => panic!("Expected Chunk"),
        }
    }

    #[tokio::test]
    async fn test_streaming_multiple_chunks() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        let mut rx = tracker.register_streaming_task(task_id, 16);

        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("chunk1".to_string()));
        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("chunk2".to_string()));
        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("chunk3".to_string()));

        assert_eq!(receive_chunk(&mut rx).await, "chunk1");
        assert_eq!(receive_chunk(&mut rx).await, "chunk2");
        assert_eq!(receive_chunk(&mut rx).await, "chunk3");
    }

    #[tokio::test]
    async fn test_streaming_done_event() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        let mut rx = tracker.register_streaming_task(task_id, 16);

        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("token".to_string()));
        tracker.push_stream_chunk(&task_id, StreamEvent::Done { final_chunk: "final".to_string() });

        // Receive chunk
        let event = rx.recv().await.unwrap();
        assert!(matches!(event, StreamEvent::Chunk(_)));

        // Receive done
        let event = rx.recv().await.unwrap();
        match event {
            StreamEvent::Done { final_chunk } => assert_eq!(final_chunk, "final"),
            _ => panic!("Expected Done"),
        }
    }

    #[tokio::test]
    async fn test_streaming_error_event() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        let mut rx = tracker.register_streaming_task(task_id, 16);

        tracker.push_stream_chunk(&task_id, StreamEvent::Error("something went wrong".to_string()));

        let event = rx.recv().await.unwrap();
        match event {
            StreamEvent::Error(msg) => assert_eq!(msg, "something went wrong"),
            _ => panic!("Expected Error"),
        }
    }

    #[tokio::test]
    async fn test_remove_streaming_task() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        tracker.register_streaming_task(task_id, 16);
        assert!(tracker.streaming_count() == 1);

        tracker.remove_streaming_task(&task_id);
        assert!(tracker.streaming_count() == 0);

        // Pushing after removal should be a no-op (no panic)
        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("data".to_string()));
    }

    #[tokio::test]
    async fn test_cancel_task_cleans_streaming() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        tracker.register_streaming_task(task_id, 16);
        assert!(tracker.streaming_count() == 1);

        tracker.cancel_task(&task_id);
        assert!(tracker.streaming_count() == 0);
        assert!(!tracker.is_pending(&task_id));
    }

    #[tokio::test]
    async fn test_multiple_streaming_tasks() {
        let tracker = TaskTracker::new();
        let id1 = Uuid::new_v4();
        let id2 = Uuid::new_v4();

        let mut rx1 = tracker.register_streaming_task(id1, 16);
        let mut rx2 = tracker.register_streaming_task(id2, 16);

        assert!(tracker.streaming_count() == 2);

        tracker.push_stream_chunk(&id1, StreamEvent::Chunk("task1-data".to_string()));
        tracker.push_stream_chunk(&id2, StreamEvent::Chunk("task2-data".to_string()));

        assert_eq!(receive_chunk(&mut rx1).await, "task1-data");
        assert_eq!(receive_chunk(&mut rx2).await, "task2-data");
    }

    #[tokio::test]
    async fn test_push_after_receiver_dropped() {
        let tracker = TaskTracker::new();
        let task_id = Uuid::new_v4();

        let rx = tracker.register_streaming_task(task_id, 16);
        drop(rx); // Drop the receiver

        // Pushing after receiver is dropped should not panic
        tracker.push_stream_chunk(&task_id, StreamEvent::Chunk("orphan".to_string()));
    }

    /// Helper to receive a Chunk event and return its data.
    async fn receive_chunk(rx: &mut mpsc::Receiver<StreamEvent>) -> String {
        let event = rx.recv().await.unwrap();
        match event {
            StreamEvent::Chunk(data) => data,
            _ => panic!("Expected Chunk event, got something else"),
        }
    }
}
