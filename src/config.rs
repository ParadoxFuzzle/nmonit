use anyhow::{Context, Result};
use serde::{Deserialize, Serialize};
use std::path::PathBuf;

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct HostConfig {
    /// Listen address for the REST API and WebSocket server
    #[serde(default = "default_listen_addr")]
    pub listen_addr: String,
    /// Port for the REST API
    #[serde(default = "default_port")]
    pub port: u16,
    /// If true, route requests to local LiteLLM as last resort
    #[serde(default = "default_true")]
    pub route_to_local: bool,
    /// LiteLLM base URL
    #[serde(default = "default_litellm_url")]
    pub litellm_base_url: String,
    /// LiteLLM API key (if required)
    #[serde(default)]
    pub litellm_api_key: Option<String>,
    /// Heartbeat timeout in seconds before marking a worker as offline
    #[serde(default = "default_heartbeat_timeout")]
    pub heartbeat_timeout_secs: u64,
    /// Task timeout in seconds
    #[serde(default = "default_task_timeout")]
    pub task_timeout_secs: u64,
    /// Max retries for failed tasks
    #[serde(default = "default_max_retries")]
    pub max_task_retries: u32,
    /// Whether to enable TLS
    #[serde(default)]
    pub tls_enabled: bool,
    /// Path to TLS certificate
    #[serde(default)]
    pub tls_cert_path: Option<String>,
    /// Path to TLS key
    #[serde(default)]
    pub tls_key_path: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct WorkerConfig {
    /// Host address to connect to
    pub host_addr: String,
    /// Host port
    #[serde(default = "default_port")]
    pub host_port: u16,
    /// Authentication token
    #[serde(default)]
    pub auth_token: Option<String>,
    /// Worker name (defaults to hostname)
    #[serde(default)]
    pub name: Option<String>,
    /// Heartbeat interval in seconds
    #[serde(default = "default_heartbeat_interval")]
    pub heartbeat_interval_secs: u64,
    /// Max concurrent tasks this worker can handle
    #[serde(default = "default_max_concurrent")]
    pub max_concurrent_tasks: u32,
    /// Path to local LiteLLM (optional — enables local inference)
    #[serde(default)]
    pub litellm_base_url: Option<String>,
    /// Whether to use local LiteLLM for inference
    #[serde(default = "default_true")]
    pub enable_local_inference: bool,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct LoggingConfig {
    /// Log level: trace, debug, info, warn, error
    #[serde(default = "default_log_level")]
    pub level: String,
    /// Log format: plain, json
    #[serde(default = "default_log_format")]
    pub format: String,
    /// Log file path (optional — logs to stderr if not set)
    #[serde(default)]
    pub file: Option<String>,
}

#[derive(Debug, Clone, Serialize, Deserialize)]
pub struct Config {
    /// Mode: host or worker
    #[serde(default)]
    pub mode: Option<String>,
    /// Host-specific configuration
    #[serde(default)]
    pub host: HostConfig,
    /// Worker-specific configuration
    #[serde(default)]
    pub worker: WorkerConfig,
    /// Logging configuration
    #[serde(default)]
    pub logging: LoggingConfig,
}

// ─── Defaults ─────────────────────────────────────────────────────────────────

fn default_listen_addr() -> String {
    "0.0.0.0".to_string()
}
fn default_port() -> u16 {
    9742
}
fn default_true() -> bool {
    true
}
fn default_litellm_url() -> String {
    "http://localhost:4000".to_string()
}
fn default_heartbeat_timeout() -> u64 {
    30
}
fn default_task_timeout() -> u64 {
    300
}
fn default_max_retries() -> u32 {
    3
}
fn default_heartbeat_interval() -> u64 {
    5
}
fn default_max_concurrent() -> u32 {
    4
}
fn default_log_level() -> String {
    "info".to_string()
}
fn default_log_format() -> String {
    "plain".to_string()
}

impl Default for HostConfig {
    fn default() -> Self {
        Self {
            listen_addr: default_listen_addr(),
            port: default_port(),
            route_to_local: true,
            litellm_base_url: default_litellm_url(),
            litellm_api_key: None,
            heartbeat_timeout_secs: default_heartbeat_timeout(),
            task_timeout_secs: default_task_timeout(),
            max_task_retries: default_max_retries(),
            tls_enabled: false,
            tls_cert_path: None,
            tls_key_path: None,
        }
    }
}

impl Default for WorkerConfig {
    fn default() -> Self {
        Self {
            host_addr: "127.0.0.1".to_string(),
            host_port: default_port(),
            auth_token: None,
            name: None,
            heartbeat_interval_secs: default_heartbeat_interval(),
            max_concurrent_tasks: default_max_concurrent(),
            litellm_base_url: None,
            enable_local_inference: true,
        }
    }
}

impl Default for LoggingConfig {
    fn default() -> Self {
        Self {
            level: default_log_level(),
            format: default_log_format(),
            file: None,
        }
    }
}

impl Default for Config {
    fn default() -> Self {
        Self {
            mode: None,
            host: HostConfig::default(),
            worker: WorkerConfig::default(),
            logging: LoggingConfig::default(),
        }
    }
}

impl Config {
    /// Load configuration from a YAML file, falling back to defaults.
    pub fn from_file(path: &str) -> Result<Self> {
        let path = PathBuf::from(path);
        if !path.exists() {
            tracing::warn!("Config file not found at {}, using defaults", path.display());
            return Ok(Config::default());
        }
        let contents = std::fs::read_to_string(&path)
            .with_context(|| format!("Failed to read config file: {}", path.display()))?;
        let config: Config =
            serde_yaml::from_str(&contents).with_context(|| format!("Failed to parse config file: {}", path.display()))?;
        Ok(config)
    }

    /// Find config file in standard locations.
    pub fn resolve_config_path(path: Option<&str>) -> PathBuf {
        if let Some(p) = path {
            return PathBuf::from(p);
        }
        // Standard locations
        let locations = [
            "/etc/nmonit/nmonit.yaml",
            "/etc/nmonit.yaml",
            "./nmonit.yaml",
            "./config/nmonit.yaml",
            &format!(
                "{}/.config/nmonit/nmonit.yaml",
                std::env::var("HOME").unwrap_or_default()
            ),
        ];
        for loc in &locations {
            if std::path::Path::new(loc).exists() {
                return PathBuf::from(loc);
            }
        }
        PathBuf::from("./nmonit.yaml")
    }
}
