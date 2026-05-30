use clap::{Parser, Subcommand};

#[derive(Parser, Debug)]
#[command(
    name = "nmonit",
    about = "Distributed LLM Compute Cluster - Share GPU/CPU/RAM/VRAM across devices",
    version,
    long_about = r#"
nmonit — A distributed compute cluster for LLM inference.

Run as HOST to expose models and accept inference requests, distributing
workload across connected worker nodes. Run as WORKER to contribute GPU,
CPU, RAM, and VRAM to the cluster.

Examples:
    # Start a host (orchestrator) node
    nmonit host --config /etc/nmonit/nmonit.yaml

    # Start a worker node connecting to a host
    nmonit worker --host 192.168.1.100 --token my-secret-token

    # List cluster status (requires host connection)
    nmonit status --host http://192.168.1.100:9742
    "#
)]
pub struct Cli {
    #[command(subcommand)]
    pub command: Command,
}

#[derive(Subcommand, Debug)]
pub enum Command {
    /// Run as the host (orchestrator) node
    Host {
        /// Path to configuration file
        #[arg(short, long, env = "NMONIT_CONFIG")]
        config: Option<String>,

        /// Listen address (overrides config)
        #[arg(short, long, env = "NMONIT_LISTEN_ADDR")]
        listen: Option<String>,

        /// Listen port (overrides config)
        #[arg(short = 'P', long, env = "NMONIT_PORT")]
        port: Option<u16>,

        /// LiteLLM base URL (overrides config)
        #[arg(long, env = "NMONIT_LITELLM_URL")]
        litellm_url: Option<String>,
    },
    /// Run as a worker node
    Worker {
        /// Path to configuration file
        #[arg(short, long, env = "NMONIT_CONFIG")]
        config: Option<String>,

        /// Host address to connect to (overrides config)
        #[arg(short, long, env = "NMONIT_HOST_ADDR")]
        host: Option<String>,

        /// Host port (overrides config)
        #[arg(short = 'P', long, env = "NMONIT_HOST_PORT")]
        port: Option<u16>,

        /// Authentication token (overrides config)
        #[arg(short, long, env = "NMONIT_AUTH_TOKEN")]
        token: Option<String>,

        /// Worker name (overrides config)
        #[arg(short = 'n', long, env = "NMONIT_WORKER_NAME")]
        name: Option<String>,
    },
    /// Show cluster status
    Status {
        /// Host URL to query
        #[arg(short, long, default_value = "http://127.0.0.1:9742")]
        host: String,
    },
    /// List models available in the cluster
    Models {
        /// Host URL to query
        #[arg(short, long, default_value = "http://127.0.0.1:9742")]
        host: String,
    },
}
