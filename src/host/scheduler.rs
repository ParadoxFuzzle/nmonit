use crate::common::types::*;
use crate::host::models::ModelRegistry;
use crate::host::workers::WorkerManager;
use std::sync::Arc;
use uuid::Uuid;

/// The scheduler determines which worker should handle each inference request.
#[derive(Debug, Clone)]
pub struct Scheduler {
    worker_manager: Arc<WorkerManager>,
    model_registry: Arc<ModelRegistry>,
}

impl Scheduler {
    pub fn new(worker_manager: Arc<WorkerManager>, model_registry: Arc<ModelRegistry>) -> Self {
        Self {
            worker_manager,
            model_registry,
        }
    }

    /// Select the best worker for a given inference request.
    ///
    /// Selection criteria (in order of priority):
    /// 1. Workers that already have the model cached/litellmed
    /// 2. Workers with higher available VRAM
    /// 3. Workers with higher available RAM
    /// 4. Workers with fewer current tasks
    /// 5. Workers with higher compute score
    pub fn select_worker(&self, request: &InferenceRequest) -> Option<Uuid> {
        let online = self.worker_manager.online();
        if online.is_empty() {
            return None;
        }

        // Score each worker
        let scored: Vec<(f64, &WorkerInfo)> = online
            .iter()
            .map(|w| {
                let mut score = 0.0;

                // Strongly prefer workers with the model cached
                if w.loaded_models.iter().any(|m| m == &request.model) {
                    score += 200.0;
                }

                // Favor workers with more VRAM available (for GPU workers)
                for gpu in &w.resources.gpus {
                    let vram_ratio = if gpu.vram_total_mb > 0 {
                        gpu.vram_free_mb as f64 / gpu.vram_total_mb as f64
                    } else {
                        0.0
                    };
                    score += vram_ratio * 100.0;
                }

                // Favor workers with more RAM available
                let mem_ratio = if w.resources.memory.total_mb > 0 {
                    w.resources.memory.available_mb as f64 / w.resources.memory.total_mb as f64
                } else {
                    0.0
                };
                score += mem_ratio * 50.0;

                // Favor workers with fewer current tasks
                let task_ratio = if w.max_concurrent_tasks > 0 {
                    1.0 - (w.current_tasks as f64 / w.max_concurrent_tasks as f64)
                } else {
                    0.0
                };
                score += task_ratio * 80.0;

                // Preference for GPU vs CPU
                let has_gpu = !w.resources.gpus.is_empty();
                if has_gpu {
                    score += 30.0;
                }

                (score, w)
            })
            .collect();

        // Return the worker with the highest score
        scored
            .into_iter()
            .max_by(|(a, _), (b, _)| a.partial_cmp(b).unwrap_or(std::cmp::Ordering::Equal))
            .map(|(_, w)| w.id)
    }

    /// Determine if a task should be handled locally (by the host's LiteLLM)
    /// vs. being dispatched to a remote worker.
    pub fn should_route_locally(&self, request: &InferenceRequest) -> bool {
        // Route locally if:
        // 1. No workers are online
        // 2. The model is not available on any worker but is available locally
        let online = self.worker_manager.online();
        if online.is_empty() {
            return true;
        }

        let workers_with_model = self.worker_manager.workers_with_model(&request.model);
        if workers_with_model.is_empty() {
            // Check if any worker has capacity at all
            let has_capacity = online.iter().any(|w| w.current_tasks < w.max_concurrent_tasks);
            if !has_capacity {
                return true; // All workers saturated, use local fallback
            }
        }

        false
    }

    /// Get the total cluster capacity info for reporting.
    pub fn cluster_stats(&self) -> (usize, u32, u64, u64, u64, u64, u32, f64) {
        let workers = self.worker_manager.online();
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
        let total_threads: u32 = workers.iter().map(|w| w.resources.cpus.iter().map(|c| c.threads).sum::<u32>()).sum();
        let score: f64 = workers.iter().map(|w| w.resources.compute_score()).sum();

        (workers.len(), total_gpus, total_vram, available_vram, total_ram, available_ram, total_threads, score)
    }
}
