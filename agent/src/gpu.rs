use crate::Args;
/// GPU discovery and utilization.
///
/// Uses NVIDIA NVML (via nvml-wrapper) for NVIDIA GPUs.
/// AMD and Intel GPU support will be added in future versions.
use crate::compute::v1::{GpuResources, GpuUtilization};
use std::sync::OnceLock;

/// Lazily-initialized NVML instance, created once and reused.
static NVML: OnceLock<Option<nvml_wrapper::Nvml>> = OnceLock::new();

/// Total unified memory (bytes) on Tegra systems, chosen once at first use.
///
/// Jetson (Tegra) iGPUs expose NVML but return `Not Supported` for
/// `memory_info()` because GPU memory is unified with system RAM. The
/// control plane must still see the node's memory pool attributed to the
/// GPU, so pick the best available figure:
///   1. the module's spec-sheet DRAM total, derived from the device tree
///      model string (true package capacity, e.g. 8 GiB on an Orin Nano 8GB);
///   2. the physical RAM size from the device tree memory `reg` node
///      (matches the spec sheet where present, e.g. 8 GiB);
///   3. `MemTotal` from /proc/meminfo (post-carve-out usable RAM).
fn tegra_unified_mem_total() -> Option<u64> {
    static CACHE: OnceLock<Option<u64>> = OnceLock::new();
    *CACHE.get_or_init(|| {
        if let Some(total) = devicetree_model_dram_total() {
            return Some(total);
        }
        if let Some(total) = devicetree_mem_total() {
            return Some(total);
        }
        let content = std::fs::read_to_string("/proc/meminfo").ok()?;
        let kb = content
            .lines()
            .find(|l| l.starts_with("MemTotal:"))
            .and_then(|l| l.split_whitespace().nth(1))
            .and_then(|v| v.parse::<u64>().ok())?;
        Some(kb * 1024)
    })
}

/// Map a Tegra device tree model string to the module's DRAM total.
///
/// Jetson modules ship in fixed DRAM configurations, so the marketing name
/// pins the capacity: every Orin Nano (dev kit or module) carries 8 GiB,
/// every Orin NX 16 GB variant carries 16 GiB, and the AGX Orin 32 GB
/// variant carries 32 GiB. Suffixes like "Engineering Reference
/// Developer Kit Super" are ignored by the prefix match.
fn devicetree_model_dram_total() -> Option<u64> {
    let raw = std::fs::read("/proc/device-tree/model").ok()?;
    model_dram_total(&String::from_utf8_lossy(&raw))
}

/// Parse a device tree model string (NUL-terminated) to the DRAM total.
fn model_dram_total(model_raw: &str) -> Option<u64> {
    let model = model_raw
        .trim_end_matches('\0')
        .trim()
        .to_ascii_lowercase();
    const GIB: u64 = 1024 * 1024 * 1024;
    if model.contains("orin nano 4gb") {
        Some(4 * GIB)
    } else if model.contains("orin nano") {
        Some(8 * GIB) // all other Orin Nano modules/kits are 8 GB
    } else if model.contains("orin nx 8gb") {
        Some(8 * GIB)
    } else if model.contains("orin nx 16gb") {
        Some(16 * GIB)
    } else if model.contains("agx orin 32gb") {
        Some(32 * GIB)
    } else if model.contains("agx orin 64gb") {
        Some(64 * GIB)
    } else {
        None
    }
}

/// Physical RAM size from the Tegra device tree memory node.
fn devicetree_mem_total() -> Option<u64> {
    let raw = std::fs::read("/proc/device-tree/memory/reg").ok()?;
    let total: u64 = parse_devicetree_mem_sizes(&raw).iter().sum();
    if total == 0 {
        None
    } else {
        Some(total)
    }
}

/// Parse a device tree `reg` blob of (address, size) u64 pairs and return
/// the sizes. Returns empty on unexpected cell layout.
fn parse_devicetree_mem_sizes(bytes: &[u8]) -> Vec<u64> {
    if bytes.len() % 16 != 0 {
        return vec![];
    }
    bytes
        .chunks_exact(16)
        .filter_map(|pair| {
            let addr = u64::from_be_bytes(pair[0..8].try_into().ok()?);
            let size = u64::from_be_bytes(pair[8..16].try_into().ok()?);
            // Sanity: plausible size (< 16 TiB) with a page-aligned base.
            // Tegra memory regions need not start at 0 (Orin's first bank
            // sits at 0x80000000), so don't require a zero base.
            let _ = addr;
            if size > 0 && size < (1u64 << 44) {
                Some(size)
            } else {
                None
            }
        })
        .collect()
}

fn get_nvml() -> Option<&'static nvml_wrapper::Nvml> {
    NVML.get_or_init(|| match nvml_wrapper::Nvml::init() {
        Ok(nvml) => {
            tracing::debug!("NVML initialized successfully");
            Some(nvml)
        }
        Err(e) => {
            tracing::debug!("NVML initialization failed: {e}");
            None
        }
    })
    .as_ref()
}

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

    let gpus = discover_nvidia_gpus();
    if gpus.is_empty() {
        tracing::debug!("no NVIDIA GPUs found on this node");
    }
    Ok(gpus)
}

/// Probe NVIDIA GPUs using NVML.
fn discover_nvidia_gpus() -> Vec<GpuResources> {
    let nvml = match get_nvml() {
        Some(nvml) => nvml,
        None => return vec![],
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

        let name = device
            .name()
            .unwrap_or_else(|_| "Unknown NVIDIA GPU".into());

        // NVML memory_info() is unsupported on Tegra unified-memory iGPUs
        // (Jetson); fall back to the system's total memory there.
        let mem_total = match device.memory_info() {
            Ok(m) => m.total,
            Err(e) => match tegra_unified_mem_total() {
                Some(bytes) => {
                    tracing::info!(
                        gpu = i,
                        unified_mem_bytes = bytes,
                        "GPU {i}: NVML memory_info() unsupported ({e}) — Tegra unified memory, reporting system RAM as GPU memory pool"
                    );
                    bytes
                }
                None => {
                    tracing::warn!("failed to query GPU {i} memory: {e}");
                    continue;
                }
            },
        };

        gpus.push(GpuResources {
            index: i as i32,
            vendor: "nvidia".into(),
            model: name,
            vram_bytes: mem_total,
            ..Default::default()
        });
    }

    gpus
}

/// Collect current GPU utilization for heartbeat reporting.
pub fn collect_gpu_utilization() -> Vec<GpuUtilization> {
    let nvml = match get_nvml() {
        Some(nvml) => nvml,
        None => return vec![],
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
        // NVML memory_info() may be unsupported on Tegra unified memory;
        // fall back to the system's total memory so utilization stays useful.
        let mem_info = device
            .memory_info()
            .ok()
            .map(|m| (m.used, m.total))
            .or_else(|| tegra_unified_mem_total().map(|t| (0u64, t)));

        // Use as_ref() to avoid moving utilization for the second field access.
        let gpu_pct = utilization.as_ref().map(|u| u.gpu as f64).unwrap_or(0.0);
        let mem_pct = utilization.as_ref().map(|u| u.memory as f64).unwrap_or(0.0);

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

        gpus.push(GpuUtilization {
            index: i as i32,
            gpu_percent: gpu_pct,
            memory_percent: mem_pct,
            vram_used_bytes: mem_info.as_ref().map(|m| m.0).unwrap_or(0),
            vram_total_bytes: mem_info.as_ref().map(|m| m.1).unwrap_or(0),
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

// ============================================================================
// Tests
// ============================================================================

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn test_parse_devicetree_mem_sizes_single_region() {
        // base = 0x800000000, size = 0x200000000 (8 GiB)
        let mut blob = 0x800000000u64.to_be_bytes().to_vec();
        blob.extend_from_slice(&0x200000000u64.to_be_bytes());
        assert_eq!(parse_devicetree_mem_sizes(&blob), vec![0x200000000]);
    }

    #[test]
    fn test_parse_devicetree_mem_sizes_multiple_regions() {
        let mut blob = Vec::new();
        blob.extend_from_slice(&0x0u64.to_be_bytes());
        blob.extend_from_slice(&0x80000000u64.to_be_bytes()); // 2 GiB
        blob.extend_from_slice(&0x1000000000u64.to_be_bytes());
        blob.extend_from_slice(&0x180000000u64.to_be_bytes()); // 6 GiB
        assert_eq!(
            parse_devicetree_mem_sizes(&blob),
            vec![0x80000000, 0x180000000]
        );
    }

    #[test]
    fn test_parse_devicetree_mem_sizes_rejects_odd_layout() {
        // Not a multiple of 16 bytes — unknown cell layout.
        assert!(parse_devicetree_mem_sizes(&[0u8; 8]).is_empty());
        assert!(parse_devicetree_mem_sizes(&[]).is_empty());
    }

    #[test]
    fn test_parse_devicetree_mem_sizes_ignores_zero_size() {
        let mut blob = 0x0u64.to_be_bytes().to_vec();
        blob.extend_from_slice(&0x0u64.to_be_bytes()); // empty region
        assert!(parse_devicetree_mem_sizes(&blob).is_empty());
    }

    #[test]
    fn test_devicetree_model_dram_total_known_modules() {
        const GIB: u64 = 1024 * 1024 * 1024;
        // (model string, expected DRAM total)
        let cases = [
            (
                "NVIDIA Jetson Orin Nano Engineering Reference Developer Kit Super\0",
                Some(8 * GIB),
            ),
            ("NVIDIA Jetson Orin Nano 4GB\0", Some(4 * GIB)),
            ("NVIDIA Jetson Orin NX 16GB\0", Some(16 * GIB)),
            ("NVIDIA Jetson AGX Orin 32GB\0", Some(32 * GIB)),
            ("NVIDIA Jetson AGX Orin 64GB\0", Some(64 * GIB)),
            ("NVIDIA Jetson Nano\0", None),
            ("\0", None),
        ];
        for (raw, expected) in cases {
            assert_eq!(
                model_dram_total(raw),
                expected,
                "wrong DRAM total for model {raw:?}"
            );
        }
    }
}