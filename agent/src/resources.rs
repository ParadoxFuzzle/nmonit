use crate::Args;
/// Hardware resource discovery and utilization collection.
///
/// Probes the local machine for CPU, RAM, GPU, storage, and network
/// capabilities. Returns generated proto types for gRPC serialization.
use crate::compute::v1::{
    CpuResources, CpuUtilization, MemoryResources, MemoryUtilization, NetworkInterface,
    NetworkResources, NetworkUtilization, NodeResources, NodeUtilization, StorageDevice,
    StorageResources, StorageUtilization,
};
use std::sync::Mutex;
use sysinfo::System;

/// Shared system handle, created once and reused across discovery and utilization.
/// Wrapped in a Mutex because sysinfo::System::refresh_*() takes &mut self.
static SYSTEM: std::sync::LazyLock<Mutex<System>> = std::sync::LazyLock::new(|| {
    let mut sys = System::new_all();
    sys.refresh_all();
    Mutex::new(sys)
});

/// Probe hardware and return a full resource report.
pub async fn discover_resources(args: &Args) -> anyhow::Result<NodeResources> {
    let node_id = args.node_id.clone().unwrap_or_else(|| {
        hostname::get()
            .ok()
            .and_then(|h| h.into_string().ok())
            .unwrap_or_else(|| "unknown-node".into())
    });

    let hostname_str = hostname::get()
        .ok()
        .and_then(|h| h.into_string().ok())
        .unwrap_or_else(|| "unknown".into());

    let cpu;
    let memory;
    {
        let sys_guard = SYSTEM.lock().expect("SYSTEM mutex poisoned");
        cpu = discover_cpu(&sys_guard);
        memory = discover_memory(&sys_guard);
    } // MutexGuard dropped here before any await

    let gpus = crate::gpu::discover_gpus(args).await?;
    let storage = discover_storage();
    let network = discover_network();
    let kernel = discover_kernel_version();
    let os_release = discover_os_release();

    Ok(NodeResources {
        node_id: node_id.clone(),
        hostname: hostname_str,
        cpu: Some(cpu),
        memory: Some(memory),
        gpus,
        storage: Some(storage),
        network: Some(network),
        capabilities: vec![],
        kernel_version: kernel,
        os_release,
    })
}

/// Discover CPU resources.
fn discover_cpu(sys: &System) -> CpuResources {
    let physical = System::physical_core_count().unwrap_or(1) as i32;
    let logical = sys.cpus().len() as i32;
    let model = sys
        .cpus()
        .first()
        .map(|c| c.name().to_string())
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
    let total_bytes = sys.total_memory();

    MemoryResources {
        total_bytes,
        hugepage_2mb_count: 0,
        hugepage_1gb_count: 0,
        speed_mhz: 0,
        r#type: String::new(),
        tiers: vec![],
    }
}

/// Discover storage devices from /proc/mounts.
fn discover_storage() -> StorageResources {
    let devices = std::fs::read_to_string("/proc/mounts")
        .map(|content| {
            content
                .lines()
                .filter_map(|line| {
                    let parts: Vec<&str> = line.split_whitespace().collect();
                    if parts.len() < 3 {
                        return None;
                    }
                    let device = parts[0];
                    let mount = parts[1];
                    let fs_type = parts[2];

                    // Skip pseudo filesystems
                    if fs_type == "proc"
                        || fs_type == "sysfs"
                        || fs_type == "devtmpfs"
                        || fs_type == "tmpfs"
                        || fs_type == "cgroup"
                        || fs_type == "cgroup2"
                        || fs_type == "pstore"
                        || fs_type == "bpf"
                        || fs_type == "debugfs"
                        || fs_type == "tracefs"
                        || fs_type == "securityfs"
                        || fs_type == "configfs"
                        || fs_type == "hugetlbfs"
                        || fs_type == "fusectl"
                        || fs_type == "mqueue"
                    {
                        return None;
                    }

                    // Only include real block devices or FUSE mounts
                    if !device.starts_with('/') {
                        return None;
                    }

                    let dev_type = if device.contains("nvme") {
                        "nvme".to_string()
                    } else if fs_type == "fuse" || fs_type == "fuse.sshfs" {
                        "network".to_string()
                    } else if device.contains("sd") || device.contains("vd") {
                        // Detect SSD vs HDD via sysfs rotational flag
                        detect_disk_type(device)
                    } else {
                        "unknown".to_string()
                    };

                    Some(StorageDevice {
                        path: mount.to_string(),
                        r#type: dev_type,
                        filesystem: fs_type.to_string(),
                        total_bytes: 0,
                        available_bytes: 0,
                        read_iops: 0,
                        write_iops: 0,
                        read_throughput_mbps: 0,
                        write_throughput_mbps: 0,
                    })
                })
                .collect()
        })
        .unwrap_or_default();

    StorageResources { devices }
}

fn detect_disk_type(device: &str) -> String {
    // Extract the base device name (e.g., sda from /dev/sda1)
    let base = device
        .trim_start_matches("/dev/")
        .trim_end_matches(|c: char| c.is_ascii_digit());
    let rotational_path = format!("/sys/block/{}/queue/rotational", base);
    match std::fs::read_to_string(&rotational_path) {
        Ok(content) if content.trim() == "0" => "ssd".to_string(),
        _ => "hdd".to_string(),
    }
}

/// Discover network interfaces.
fn discover_network() -> NetworkResources {
    let mut networks = sysinfo::Networks::new();
    networks.refresh(true);
    let interfaces = networks
        .iter()
        .map(|(name, net)| NetworkInterface {
            name: name.clone(),
            driver: String::new(),
            mac_address: net.mac_address().to_string(),
            ip_addresses: net
                .ip_networks()
                .iter()
                .map(|ip| ip.addr.to_string())
                .collect(),
            speed_mbps: 0,
            supports_rdma: false,
            supports_roce: false,
            supports_infiniband: false,
            pci_address: String::new(),
            numa_node: 0,
        })
        .collect();

    NetworkResources {
        interfaces,
        total_bandwidth_mbps: 0,
    }
}

/// Read kernel version from uname.
fn discover_kernel_version() -> String {
    let mut uname: libc::utsname = unsafe { std::mem::zeroed() };
    if unsafe { libc::uname(&mut uname) } == 0 {
        let release = unsafe { std::ffi::CStr::from_ptr(uname.release.as_ptr()) };
        release.to_string_lossy().into_owned()
    } else {
        String::new()
    }
}

/// Read OS release from /etc/os-release.
fn discover_os_release() -> String {
    std::fs::read_to_string("/etc/os-release")
        .ok()
        .and_then(|content| {
            content.lines().find_map(|line| {
                line.strip_prefix("PRETTY_NAME=")
                    .map(|v| v.trim_matches('"').to_string())
            })
        })
        .unwrap_or_default()
}

/// Collect current resource utilization for heartbeat reporting.
/// Reuses a shared System handle to avoid expensive System::new_all() on every call.
pub fn collect_utilization() -> NodeUtilization {
    let mut sys = SYSTEM.lock().unwrap_or_else(|e| e.into_inner());
    sys.refresh_cpu_all();
    sys.refresh_memory();

    let cpu = collect_cpu_utilization(&sys);
    let memory = collect_memory_utilization(&sys);
    let gpus = crate::gpu::collect_gpu_utilization();

    NodeUtilization {
        node_id: String::new(), // filled in by send_heartbeat
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
    let per_core: Vec<f64> = sys.cpus().iter().map(|c| c.cpu_usage() as f64).collect();

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
    let total = sys.total_memory();
    let available = sys.available_memory();
    let used = total.saturating_sub(available);

    MemoryUtilization {
        used_bytes: used,
        available_bytes: available,
        hugepage_2mb_used: 0,
        hugepage_1gb_used: 0,
    }
}

// ============================================================================
// Tests
// ============================================================================

#[cfg(test)]
mod tests {
    use super::*;
    use sysinfo::System;

    #[test]
    fn test_discover_cpu_returns_valid_data() {
        let mut sys = System::new_all();
        sys.refresh_all();
        let cpu = discover_cpu(&sys);
        assert!(cpu.physical_cores >= 1, "expected at least 1 physical core");
        assert!(cpu.logical_cores >= cpu.physical_cores);
        assert!(!cpu.architecture.is_empty());
    }

    #[test]
    fn test_cpu_architecture_matches_compile_target() {
        let mut sys = System::new_all();
        sys.refresh_all();
        let cpu = discover_cpu(&sys);
        assert_eq!(cpu.architecture, std::env::consts::ARCH);
    }

    #[test]
    fn test_discover_memory_returns_positive_total() {
        let mut sys = System::new_all();
        sys.refresh_all();
        let mem = discover_memory(&sys);
        assert!(mem.total_bytes > 0);
        assert!(
            mem.total_bytes >= 64 * 1024 * 1024,
            "expect at least 64MB RAM"
        );
    }

    #[test]
    fn test_collect_cpu_utilization_percent_in_range() {
        let mut sys = System::new_all();
        sys.refresh_all();
        std::thread::sleep(std::time::Duration::from_millis(100));
        sys.refresh_cpu_all();
        let util = collect_cpu_utilization(&sys);
        assert!((0.0..=100.0).contains(&util.overall_percent));
        assert_eq!(util.per_core_percent.len(), sys.cpus().len());
        for &pct in &util.per_core_percent {
            assert!((0.0..=100.0).contains(&pct));
        }
        assert!(util.load_1m >= 0.0);
    }

    #[test]
    fn test_cpu_utilization_not_inverted() {
        let mut sys = System::new_all();
        sys.refresh_all();
        std::thread::sleep(std::time::Duration::from_millis(100));
        sys.refresh_cpu_all();
        let util = collect_cpu_utilization(&sys);
        for &pct in &util.per_core_percent {
            assert!(
                pct >= 0.0,
                "CPU percent should not be negative (inversion bug)"
            );
        }
    }

    #[test]
    fn test_collect_memory_utilization_bounds() {
        let mut sys = System::new_all();
        sys.refresh_all();
        let util = collect_memory_utilization(&sys);
        let total = sys.total_memory();
        assert!(
            util.used_bytes <= total,
            "used bytes {} > total {}",
            util.used_bytes,
            total
        );
        assert!(
            util.available_bytes <= total,
            "available bytes {} > total {}",
            util.available_bytes,
            total
        );
    }

    #[test]
    fn test_discover_storage_parses_proc_mounts() {
        if !cfg!(target_os = "linux") {
            return;
        }
        let storage = discover_storage();
        assert!(
            !storage.devices.is_empty(),
            "should find at least one device"
        );
        assert!(
            storage.devices.iter().any(|d| d.path == "/"),
            "should find root"
        );
        assert!(
            !storage.devices.iter().any(|d| d.path == "/proc"),
            "proc filtered"
        );
        assert!(
            !storage.devices.iter().any(|d| d.filesystem == "tmpfs"),
            "tmpfs filtered"
        );
    }

    #[test]
    fn test_discover_storage_field_validity() {
        if !cfg!(target_os = "linux") {
            return;
        }
        for device in &discover_storage().devices {
            assert!(!device.path.is_empty());
            assert!(!device.filesystem.is_empty());
            assert!(!device.r#type.is_empty());
        }
    }

    #[test]
    fn test_detect_disk_type_valid_result() {
        let result = detect_disk_type("/dev/sda1");
        assert!(result == "ssd" || result == "hdd");
    }

    #[test]
    fn test_collect_utilization_timestamp() {
        let util = collect_utilization();
        assert!(util.timestamp_ns > 0);
        let now_ns = std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos() as u64;
        let diff = now_ns.saturating_sub(util.timestamp_ns);
        assert!(diff < 5_000_000_000, "timestamp diff {}ns too large", diff);
    }

    #[test]
    fn test_collect_utilization_node_id_empty() {
        let util = collect_utilization();
        assert!(util.node_id.is_empty(), "filled by send_heartbeat");
    }

    #[test]
    fn test_discover_kernel_version_not_empty() {
        if !cfg!(target_os = "linux") {
            return;
        }
        let v = discover_kernel_version();
        assert!(!v.is_empty());
        assert!(v.chars().any(|c| c.is_ascii_digit()));
    }

    #[test]
    fn test_discover_os_release_does_not_panic() {
        if !cfg!(target_os = "linux") {
            return;
        }
        let _ = discover_os_release();
    }
}
