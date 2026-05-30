use crate::common::types::*;
use anyhow::Result;
use sysinfo::{CpuExt, SystemExt};

/// Collect hardware resource information for the current machine.
pub struct ResourceCollector;

impl ResourceCollector {
    /// Collect all hardware resources in one call.
    pub fn collect_all() -> Result<NodeResources> {
        let hostname = gethostname();
        let platform = std::env::consts::OS.to_string();

        Ok(NodeResources {
            hostname,
            platform,
            cpus: Self::collect_cpu_info(),
            gpus: Self::collect_gpu_info(),
            memory: Self::collect_memory_info(),
        })
    }

    /// Collect CPU information using sysinfo.
    fn collect_cpu_info() -> Vec<CpuInfo> {
        let mut system = sysinfo::System::new();
        system.refresh_cpu();
        std::thread::sleep(std::time::Duration::from_millis(200));
        system.refresh_cpu();

        let cpus = system.cpus();
        if cpus.is_empty() {
            return vec![CpuInfo {
                cores: 1,
                threads: 1,
                utilization_percent: 0.0,
                frequency_ghz: 0.0,
            }];
        }

        let total_usage: f32 = cpus.iter().map(|c| c.cpu_usage()).sum();
        let avg_usage = total_usage / cpus.len() as f32;
        let freq = cpus.first().map(|c| c.frequency() as f32 / 1000.0).unwrap_or(0.0);

        // Group by physical core (assume every 2 threads share a core for hyperthreading)
        let total_threads = cpus.len() as u32;
        let physical_cores = if total_threads > 1 { total_threads / 2 } else { 1 };

        vec![CpuInfo {
            cores: physical_cores.max(1),
            threads: total_threads,
            utilization_percent: avg_usage,
            frequency_ghz: freq,
        }]
    }

    /// Collect GPU information by parsing nvidia-smi output.
    fn collect_gpu_info() -> Vec<GpuInfo> {
        // Try nvidia-smi first
        if let Some(gpu) = Self::nvidia_smi_gpus() {
            return gpu;
        }

        // No GPU detected — return empty
        Vec::new()
    }

    /// Parse nvidia-smi output for GPU information.
    fn nvidia_smi_gpus() -> Option<Vec<GpuInfo>> {
        let output = std::process::Command::new("nvidia-smi")
            .args([
                "--query-gpu=index,name,memory.total,memory.used,memory.free,utilization.gpu,temperature.gpu",
                "--format=csv,noheader,nounits",
            ])
            .output()
            .ok()?;

        if !output.status.success() {
            return None;
        }

        let stdout = String::from_utf8_lossy(&output.stdout);
        let mut gpus = Vec::new();

        for line in stdout.lines() {
            let parts: Vec<&str> = line.split(',').map(|s| s.trim()).collect();
            if parts.len() < 7 {
                continue;
            }

            let index: u32 = parts[0].parse().unwrap_or(0);
            let name = parts[1].to_string();
            let vram_total: u64 = parts[2].parse().unwrap_or(0);
            let vram_used: u64 = parts[3].parse().unwrap_or(0);
            let vram_free: u64 = parts[4].parse().unwrap_or(0);
            let utilization: f32 = parts[5].parse().unwrap_or(0.0);
            let temp: f32 = parts[6].parse().unwrap_or(0.0);

            gpus.push(GpuInfo {
                index,
                name,
                vram_total_mb: vram_total,
                vram_used_mb: vram_used,
                vram_free_mb: vram_free,
                utilization_percent: utilization,
                temperature_celsius: temp,
            });
        }

        if gpus.is_empty() {
            None
        } else {
            Some(gpus)
        }
    }

    /// Collect system memory information.
    fn collect_memory_info() -> MemoryInfo {
        let mut system = sysinfo::System::new();
        system.refresh_memory();

        MemoryInfo {
            total_mb: system.total_memory() / 1024,
            used_mb: system.used_memory() / 1024,
            free_mb: system.free_memory() / 1024,
            available_mb: system.available_memory() / 1024,
            swap_total_mb: system.total_swap() / 1024,
            swap_used_mb: system.used_swap() / 1024,
        }
    }
}

fn gethostname() -> String {
    std::env::var("HOSTNAME")
        .or_else(|_| std::env::var("HOST"))
        .unwrap_or_else(|_| {
            std::process::Command::new("hostname")
                .output()
                .ok()
                .and_then(|o| {
                    String::from_utf8(o.stdout)
                        .ok()
                        .map(|s| s.trim().to_string())
                })
                .unwrap_or_else(|| "unknown".to_string())
        })
}
