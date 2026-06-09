/// GPU discovery and utilization.
///
/// Uses NVIDIA NVML (via nvml-wrapper) for NVIDIA GPUs.
/// AMD and Intel GPU support will be added in future versions.
use crate::compute::v1::{GpuResources, GpuUtilization};
use crate::Args;

/// Discover all GPUs available on this node.
pub async fn discover_gpus(args: &Args) -> anyhow::Result<Vec<GpuResources>> {
    if let Some(count) = args.mock_gpu_count {
        let mut gpus = Vec::with_capacity(count);
        for i in 0..count {
            gpus.push(GpuResources {
                index: i as i32,
                vendor: "nvidia".into(),
                model: "Mock RTX 4090".into(),
                vram_bytes: 24 * 1024 * 1024 * 1024,
                ..Default::default()
            });
        }
        return Ok(gpus);
    }

    let gpus = discover_nvidia_gpus().await;
    if gpus.is_empty() {
        tracing::debug!("no NVIDIA GPUs found on this node");
    }
    Ok(gpus)
}

/// Probe NVIDIA GPUs using NVML.
async fn discover_nvidia_gpus() -> Vec<GpuResources> {
    let nvml = match nvml_wrapper::Nvml::init() {
        Ok(nvml) => nvml,
        Err(e) => {
            tracing::debug!("NVML initialization failed: {e}");
            return vec![];
        }
    };

    let device_count = match nvml.device_count() {
        Ok(c) => c,
        Err(e) => {
            tracing::debug!("NVML device count query failed: {e}");
            return vec![];
        }
    };

    let mut gpus = Vec::with_capacity(device_count as usize);

    for i in 0..device_count {
        let device = match nvml.device_by_index(i) {
            Ok(d) => d,
            Err(e) => {
                tracing::warn!("failed to open GPU {i}: {e}");
                continue;
            }
        };

        let name = device.name().unwrap_or_else(|_| "Unknown NVIDIA GPU".into());
        let mem_info = match device.memory_info() {
            Ok(m) => m,
            Err(e) => {
                tracing::warn!("failed to query GPU {i} memory: {e}");
                continue;
            }
        };

        gpus.push(GpuResources {
            index: i as i32,
            vendor: "nvidia".into(),
            model: name,
            vram_bytes: mem_info.total,
            ..Default::default()
        });
    }

    gpus
}

/// Collect current GPU utilization for heartbeat reporting.
pub fn collect_gpu_utilization() -> Vec<GpuUtilization> {
    let nvml = match nvml_wrapper::Nvml::init() {
        Ok(nvml) => nvml,
        Err(_) => return vec![],
    };

    let device_count = match nvml.device_count() {
        Ok(c) => c,
        Err(_) => return vec![],
    };

    let mut gpus = Vec::with_capacity(device_count as usize);

    for i in 0..device_count {
        let device = match nvml.device_by_index(i) {
            Ok(d) => d,
            Err(_) => continue,
        };

        let utilization = device.utilization_rates().ok();
        let mem_info = device.memory_info().ok();
        let temperature = device
            .temperature(nvml_wrapper::enum_wrappers::device::TemperatureSensor::Gpu)
            .ok();
        let power = device.power_usage().ok();
        let power_limit = device.power_management_limit().ok();
        let clock = device
            .clock_info(nvml_wrapper::enum_wrappers::device::Clock::Graphics)
            .ok();
        let mem_clock = device
            .clock_info(nvml_wrapper::enum_wrappers::device::Clock::Memory)
            .ok();

        // Use as_ref() to avoid moving utilization for the second field access.
        let gpu_pct = utilization.as_ref().map(|u| u.gpu as f64).unwrap_or(0.0);
        let mem_pct = utilization.as_ref().map(|u| u.memory as f64).unwrap_or(0.0);

        gpus.push(GpuUtilization {
            index: i as i32,
            gpu_percent: gpu_pct,
            memory_percent: mem_pct,
            vram_used_bytes: mem_info.as_ref().map(|m| m.used).unwrap_or(0),
            vram_total_bytes: mem_info.as_ref().map(|m| m.total).unwrap_or(0),
            temperature_celsius: temperature.map(|t| t as i32).unwrap_or(0),
            power_draw_watts: (power.unwrap_or(0) / 1000) as i64,
            power_limit_watts: (power_limit.unwrap_or(0) / 1000) as i64,
            clock_mhz: clock.unwrap_or(0) as i64,
            memory_clock_mhz: mem_clock.unwrap_or(0) as i64,
            ..Default::default()
        });
    }

    gpus
}
