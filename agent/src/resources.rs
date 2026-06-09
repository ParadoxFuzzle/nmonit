/// Hardware resource discovery and utilization collection.
///
/// Probes the local machine for CPU, RAM, GPU, storage, and network
/// capabilities. Returns generated proto types for gRPC serialization.
use crate::compute::v1::{
    CpuResources, CpuUtilization, MemoryResources, MemoryUtilization,
    NetworkResources, NetworkUtilization, NodeResources, NodeUtilization,
    StorageResources, StorageUtilization,
};
use crate::Args;
use hostname;
use sysinfo::System;

/// Probe hardware and return a full resource report.
pub async fn discover_resources(args: &Args) -> anyhow::Result<NodeResources> {
    let node_id = args
        .node_id
        .clone()
        .unwrap_or_else(|| {
            hostname::get()
                .ok()
                .and_then(|h| h.into_string().ok())
                .unwrap_or_else(|| "unknown-node".into())
        });

    let mut sys = System::new_all();
    sys.refresh_all();

    let hostname_str = hostname::get()
        .ok()
        .and_then(|h| h.into_string().ok())
        .unwrap_or_else(|| "unknown".into());

    let cpu = discover_cpu(&sys);
    let memory = discover_memory(&sys);
    let gpus = crate::gpu::discover_gpus(args).await?;

    Ok(NodeResources {
        node_id: node_id.clone(),
        hostname: hostname_str,
        cpu: Some(cpu),
        memory: Some(memory),
        gpus,
        storage: Some(StorageResources::default()),
        network: Some(NetworkResources::default()),
        capabilities: vec![],
        kernel_version: String::new(),
        os_release: String::new(),
    })
}

/// Discover CPU resources.
fn discover_cpu(sys: &System) -> CpuResources {
    let physical = sys.physical_core_count().unwrap_or(1) as i32;
    let logical = sys.cpus().len() as i32;
    let model = sys
        .cpus()
        .first()
        .map(|c| c.brand().to_string())
        .unwrap_or_else(|| "Unknown CPU".into());

    CpuResources {
        physical_cores: physical,
        logical_cores: logical,
        architecture: std::env::consts::ARCH.to_string(),
        model_name: model,
        frequency_mhz: 0,
        max_frequency_mhz: 0,
        numa_nodes: vec![],
    }
}

/// Discover memory resources.
fn discover_memory(sys: &System) -> MemoryResources {
    let total_kb = sys.total_memory();

    MemoryResources {
        total_bytes: total_kb * 1024,
        hugepage_2mb_count: 0,
        hugepage_1gb_count: 0,
        speed_mhz: 0,
        r#type: String::new(),
        tiers: vec![],
    }
}

/// Collect current resource utilization for heartbeat reporting.
/// `node_id` identifies this node in the utilization report.
pub fn collect_utilization(node_id: &str) -> NodeUtilization {
    let mut sys = System::new_all();
    sys.refresh_all();

    let cpu = collect_cpu_utilization(&sys);
    let memory = collect_memory_utilization(&sys);
    let gpus = crate::gpu::collect_gpu_utilization();

    NodeUtilization {
        node_id: node_id.to_string(),
        timestamp_ns: std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap_or_default()
            .as_nanos() as u64,
        cpu: Some(cpu),
        memory: Some(memory),
        gpus,
        storage: Some(StorageUtilization::default()),
        network: Some(NetworkUtilization::default()),
    }
}

/// Collect current CPU utilization.
fn collect_cpu_utilization(sys: &System) -> CpuUtilization {
    let per_core: Vec<f64> = sys
        .cpus()
        .iter()
        .map(|c| (100.0 - c.cpu_usage() as f64).clamp(0.0, 100.0))
        .collect();

    let overall = if per_core.is_empty() {
        0.0
    } else {
        per_core.iter().sum::<f64>() / per_core.len() as f64
    };

    let load_avg = System::load_average();

    CpuUtilization {
        overall_percent: overall,
        per_core_percent: per_core,
        load_1m: load_avg.one,
        load_5m: load_avg.five,
        load_15m: load_avg.fifteen,
    }
}

/// Collect current memory utilization.
fn collect_memory_utilization(sys: &System) -> MemoryUtilization {
    let total = sys.total_memory() * 1024;
    let available = sys.available_memory() * 1024;
    let used = total.saturating_sub(available);

    MemoryUtilization {
        used_bytes: used,
        available_bytes: available,
        hugepage_2mb_used: 0,
        hugepage_1gb_used: 0,
    }
}
