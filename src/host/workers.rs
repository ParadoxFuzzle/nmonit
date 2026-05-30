use crate::common::types::*;
use dashmap::DashMap;
use std::sync::Arc;
use tokio::sync::broadcast;
use uuid::Uuid;

/// Manages all connected worker nodes in the cluster.
#[derive(Debug, Clone)]
pub struct WorkerManager {
    /// Map of worker ID -> WorkerInfo
    workers: Arc<DashMap<Uuid, WorkerInfo>>,
    /// Channel for broadcasting events
    event_tx: broadcast::Sender<WorkerEvent>,
    /// Heartbeat timeout in seconds
    heartbeat_timeout_secs: u64,
}

#[derive(Debug, Clone)]
pub enum WorkerEvent {
    WorkerConnected(Uuid),
    WorkerDisconnected(Uuid),
    WorkerHeartbeat(Uuid, NodeResources),
}

impl WorkerManager {
    pub fn new(heartbeat_timeout_secs: u64) -> Self {
        let (event_tx, _) = broadcast::channel(256);
        Self {
            workers: Arc::new(DashMap::new()),
            event_tx,
            heartbeat_timeout_secs,
        }
    }

    /// Subscribe to worker events.
    pub fn subscribe(&self) -> broadcast::Receiver<WorkerEvent> {
        self.event_tx.subscribe()
    }

    /// Register a new worker or update an existing one.
    pub fn register(&self, info: WorkerInfo) {
        let id = info.id;
        self.workers.insert(id, info);
        let _ = self.event_tx.send(WorkerEvent::WorkerConnected(id));
        tracing::info!("Worker registered: {id}");
    }

    /// Update worker heartbeat and resource information.
    pub fn heartbeat(&self, id: Uuid, resources: NodeResources, current_tasks: u32, loaded_models: Vec<String>) {
        if let Some(mut worker) = self.workers.get_mut(&id) {
            worker.resources = resources;
            worker.current_tasks = current_tasks;
            worker.loaded_models = loaded_models;
            worker.last_heartbeat = chrono::Utc::now();
            worker.status = WorkerStatus::Online;
            let _ = self.event_tx.send(WorkerEvent::WorkerHeartbeat(id, worker.resources.clone()));
        }
    }

    /// Mark a worker as offline.
    pub fn disconnect(&self, id: Uuid) {
        if let Some(mut worker) = self.workers.get_mut(&id) {
            worker.status = WorkerStatus::Offline;
            let _ = self.event_tx.send(WorkerEvent::WorkerDisconnected(id));
            tracing::info!("Worker disconnected: {id}");
        }
    }

    /// Remove a worker entirely.
    pub fn remove(&self, id: &Uuid) {
        if let Some((_, worker)) = self.workers.remove(id) {
            let _ = self.event_tx.send(WorkerEvent::WorkerDisconnected(*id));
            tracing::info!("Worker removed: {id} (was {})", worker.name);
        }
    }

    /// Get info for a specific worker.
    pub fn get(&self, id: &Uuid) -> Option<WorkerInfo> {
        self.workers.get(id).map(|w| w.clone())
    }

    /// Get all workers.
    pub fn all(&self) -> Vec<WorkerInfo> {
        self.workers
            .iter()
            .map(|w| w.clone())
            .collect()
    }

    /// Get online workers only.
    pub fn online(&self) -> Vec<WorkerInfo> {
        self.workers
            .iter()
            .filter(|w| w.status == WorkerStatus::Online && !self.is_stale(&w.last_heartbeat))
            .map(|w| w.clone())
            .collect()
    }

    /// Get workers that have a specific model loaded.
    pub fn workers_with_model(&self, model: &str) -> Vec<WorkerInfo> {
        self.workers
            .iter()
            .filter(|w| {
                w.status == WorkerStatus::Online
                    && !self.is_stale(&w.last_heartbeat)
                    && w.loaded_models.iter().any(|m| m == model)
            })
            .map(|w| w.clone())
            .collect()
    }

    /// Number of connected workers.
    pub fn count(&self) -> usize {
        self.workers.len()
    }

    /// Total compute score across all online workers.
    pub fn total_compute_score(&self) -> f64 {
        self.online()
            .iter()
            .map(|w| w.resources.compute_score())
            .sum()
    }

    /// Prune stale workers (no heartbeat within timeout).
    pub fn prune_stale(&self) -> Vec<Uuid> {
        let now = chrono::Utc::now();
        let mut stale = Vec::new();
        for entry in self.workers.iter() {
            let elapsed = (now - entry.last_heartbeat).num_seconds() as u64;
            if elapsed > self.heartbeat_timeout_secs && entry.status != WorkerStatus::Offline {
                stale.push(entry.id);
            }
        }
        for id in &stale {
            self.disconnect(*id);
        }
        stale
    }

    fn is_stale(&self, last: &chrono::DateTime<chrono::Utc>) -> bool {
        let elapsed = (chrono::Utc::now() - *last).num_seconds() as u64;
        elapsed > self.heartbeat_timeout_secs
    }
}
