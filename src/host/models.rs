use crate::common::types::*;
use dashmap::DashMap;
use std::sync::Arc;
use uuid::Uuid;

/// Tracks all models available in the cluster and which workers have them cached.
#[derive(Debug, Clone)]
pub struct ModelRegistry {
    /// Map of model name -> set of worker IDs that have it loaded
    model_cache: Arc<DashMap<String, Vec<Uuid>>>,
    /// Map of model name -> model info
    models: Arc<DashMap<String, ModelInfo>>,
}

impl ModelRegistry {
    pub fn new() -> Self {
        Self {
            model_cache: Arc::new(DashMap::new()),
            models: Arc::new(DashMap::new()),
        }
    }

    /// Register a model as available (from LiteLLM or manual addition).
    pub fn register_model(&self, info: ModelInfo) {
        let id = info.id.clone();
        self.models.insert(id, info);
    }

    /// Register models in bulk.
    pub fn register_models(&self, models: Vec<ModelInfo>) {
        for m in models {
            self.register_model(m);
        }
    }

    /// Record that a worker has loaded a particular model.
    pub fn worker_loaded_model(&self, worker_id: Uuid, model: &str) {
        let mut workers = self.model_cache.entry(model.to_string()).or_default();
        if !workers.contains(&worker_id) {
            workers.push(worker_id);
        }
    }

    /// Record that a worker has unloaded a particular model.
    pub fn worker_unloaded_model(&self, worker_id: Uuid, model: &str) {
        if let Some(mut workers) = self.model_cache.get_mut(model) {
            workers.value_mut().retain(|w| *w != worker_id);
        }
    }

    /// Record that a worker has disconnected — remove from all model caches.
    pub fn worker_disconnected(&self, worker_id: Uuid) {
        for mut entry in self.model_cache.iter_mut() {
            entry.value_mut().retain(|w| *w != worker_id);
        }
    }

    /// Get all workers that have a specific model cached.
    pub fn workers_for_model(&self, model: &str) -> Vec<Uuid> {
        self.model_cache
            .get(model)
            .map(|w| w.value().clone())
            .unwrap_or_default()
    }

    /// Get all registered models.
    pub fn all_models(&self) -> Vec<ModelInfo> {
        self.models.iter().map(|m| {
            let mut info = m.value().clone();
            // Update cached_on_workers
            if let Some(workers) = self.model_cache.get(&info.id) {
                info.cached_on_workers = Some(workers.value().iter().map(|id| id.to_string()).collect());
            }
            info
        }).collect()
    }

    /// Check if a model is registered.
    pub fn has_model(&self, model: &str) -> bool {
        self.models.contains_key(model)
    }

    /// Get total number of registered models.
    pub fn count(&self) -> usize {
        self.models.len()
    }

    /// Clear all models and cache data.
    pub fn clear(&self) {
        self.models.clear();
        self.model_cache.clear();
    }
}
