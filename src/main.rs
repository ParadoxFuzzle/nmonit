mod api;
mod cli;
mod common;
mod config;
mod host;
mod worker;

use clap::Parser;
use cli::{Cli, Command};
use config::Config;
use tracing_subscriber::EnvFilter;

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let cli = Cli::parse();

    // Initialize tracing
    setup_logging();

    match &cli.command {
        Command::Host {
            config: config_path,
            listen,
            port,
            litellm_url,
        } => {
            let resolved_path = Config::resolve_config_path(config_path.as_deref());
            let cfg = Config::from_file(resolved_path.to_str().unwrap_or("nmonit.yaml")).unwrap_or_default();
            let mut host_cfg = cfg.host;

            // Override from CLI args if provided
            if let Some(addr) = listen {
                host_cfg.listen_addr = addr.clone();
            }
            if let Some(p) = port {
                host_cfg.port = *p;
            }
            if let Some(url) = litellm_url {
                host_cfg.litellm_base_url = url.clone();
            }

            tracing::info!("Starting nmonit host...");
            host::server::run_host(host_cfg).await?;
        }
        Command::Worker {
            config: config_path,
            host: host_addr,
            port,
            token,
            name,
        } => {
            let resolved_path = Config::resolve_config_path(config_path.as_deref());
            let cfg = Config::from_file(resolved_path.to_str().unwrap_or("nmonit.yaml")).unwrap_or_default();
            let mut worker_cfg = cfg.worker;

            if let Some(addr) = host_addr {
                worker_cfg.host_addr = addr.clone();
            }
            if let Some(p) = port {
                worker_cfg.host_port = *p;
            }
            if let Some(t) = token {
                worker_cfg.auth_token = Some(t.clone());
            }
            if let Some(n) = name {
                worker_cfg.name = Some(n.clone());
            }

            let agent = worker::client::WorkerAgent::new(worker_cfg);
            agent.run().await?;
        }
        Command::Status { host } => {
            print_status(host).await?;
        }
        Command::Models { host } => {
            print_models(host).await?;
        }
    }

    Ok(())
}

fn setup_logging() {
    tracing_subscriber::fmt()
        .with_env_filter(
            EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| EnvFilter::new("info,nmonit=debug")),
        )
        .with_target(true)
        .init();
}

/// Display cluster status.
async fn print_status(host: &str) -> anyhow::Result<()> {
    let client = reqwest::Client::new();
    let base_url = host.trim_end_matches('/');

    // Get cluster stats
    let resp = client
        .get(format!("{base_url}/cluster/stats"))
        .send()
        .await?;

    if !resp.status().is_success() {
        anyhow::bail!("Host returned {}: {}", resp.status(), resp.text().await?);
    }

    let stats: serde_json::Value = resp.json().await?;

    println!();
    println!("╔══════════════════════════════════════════╗");
    println!("║       nmonit — Cluster Status           ║");
    println!("╠══════════════════════════════════════════╣");
    println!(
        "║  Workers     {:>3} total, {:>3} online       ║",
        stats["total_workers"].as_u64().unwrap_or(0),
        stats["online_workers"].as_u64().unwrap_or(0)
    );
    println!(
        "║  GPUs        {:>5} total              ║",
        stats["total_gpus"].as_u64().unwrap_or(0)
    );
    println!(
        "║  VRAM        {:>6} MB total, {:>6} MB free ║",
        stats["total_vram_mb"].as_u64().unwrap_or(0),
        stats["available_vram_mb"].as_u64().unwrap_or(0)
    );
    println!(
        "║  RAM         {:>6} MB total, {:>6} MB free ║",
        stats["total_ram_mb"].as_u64().unwrap_or(0),
        stats["available_ram_mb"].as_u64().unwrap_or(0)
    );
    println!(
        "║  CPU Threads {:>5}                     ║",
        stats["total_cpu_threads"].as_u64().unwrap_or(0)
    );
    println!(
        "║  Active Tasks {:>5}                     ║",
        stats["active_tasks"].as_u64().unwrap_or(0)
    );
    println!(
        "║  Compute Score {:>7.1}               ║",
        stats["cluster_compute_score"].as_f64().unwrap_or(0.0)
    );
    println!("╚══════════════════════════════════════════╝");
    println!();

    // Show individual workers
    if let Some(workers) = stats["workers"].as_array() {
        if workers.is_empty() {
            println!("  No workers connected.");
        } else {
            println!("  Connected Workers:");
            println!();
            for w in workers {
                let name = w["name"].as_str().unwrap_or("unknown");
                let status = w["status"].as_str().unwrap_or("unknown");
                let hostname = w["hostname"].as_str().unwrap_or("?");
                let gpus = w["resources"]["gpus"].as_array().map(|g| g.len()).unwrap_or(0);
                let vram_total: u64 = w["resources"]["gpus"]
                    .as_array()
                    .map(|gpus| {
                        gpus.iter()
                            .map(|g| g["vram_total_mb"].as_u64().unwrap_or(0))
                            .sum()
                    })
                    .unwrap_or(0);
                let vram_free: u64 = w["resources"]["gpus"]
                    .as_array()
                    .map(|gpus| {
                        gpus.iter()
                            .map(|g| g["vram_free_mb"].as_u64().unwrap_or(0))
                            .sum()
                    })
                    .unwrap_or(0);
                let ram_total = w["resources"]["memory"]["total_mb"].as_u64().unwrap_or(0);
                let ram_avail = w["resources"]["memory"]["available_mb"].as_u64().unwrap_or(0);
                let tasks = w["current_tasks"].as_u64().unwrap_or(0);
                let models = w["loaded_models"]
                    .as_array()
                    .map(|m| m.len())
                    .unwrap_or(0);

                println!("    📦 {name} ({hostname})");
                println!("       Status: {status} | Tasks: {tasks} | Models: {models}");
                if gpus > 0 {
                    println!("       GPU: {gpus}x | VRAM: {vram_free}/{vram_total} MB");
                }
                println!("       RAM: {ram_avail}/{ram_total} MB");
                println!();
            }
        }
    }

    Ok(())
}

/// Display available models.
async fn print_models(host: &str) -> anyhow::Result<()> {
    let client = reqwest::Client::new();
    let base_url = host.trim_end_matches('/');

    let resp = client
        .get(format!("{base_url}/v1/models"))
        .send()
        .await?;

    if !resp.status().is_success() {
        anyhow::bail!("Host returned {}: {}", resp.status(), resp.text().await?);
    }

    let models: serde_json::Value = resp.json().await?;

    println!();
    println!("╔══════════════════════════════════════════╗");
    println!("║       nmonit — Available Models         ║");
    println!("╠══════════════════════════════════════════╣");

    if let Some(data) = models["data"].as_array() {
        if data.is_empty() {
            println!("║  No models available                     ║");
        } else {
            for m in data {
                let id = m["id"].as_str().unwrap_or("unknown");
                if let Some(workers) = m["cached_on_workers"].as_array() {
                    if !workers.is_empty() {
                        println!("║  ✅ {id:<35} ║");
                        println!("║     (cached on {} worker(s))           ║", workers.len());
                    } else {
                        println!("║  📄 {id:<35} ║");
                    }
                } else {
                    println!("║  📄 {id:<35} ║");
                }
            }
        }
    }
    println!("╚══════════════════════════════════════════╝");
    println!();

    Ok(())
}
