use crate::api::routes::{self, AppState};
use crate::api::tasks::TaskTracker;
use crate::api::ws;
use crate::config::HostConfig;
use crate::host::litellm::LiteLlmClient;
use crate::host::models::ModelRegistry;
use crate::host::scheduler::Scheduler;
use crate::host::workers::WorkerManager;
use axum::{
    extract::ws::WebSocketUpgrade,
    extract::State,
    response::IntoResponse,
    routing::{get, post},
    Router,
};
use std::sync::Arc;
use std::time::Instant;
use tokio::signal;
use tower_http::cors::CorsLayer;
use tower_http::trace::TraceLayer;
use tracing::info;

/// Start the host (orchestrator) server.
pub async fn run_host(config: HostConfig) -> anyhow::Result<()> {
    let start_time = Instant::now();

    // Initialize services
    let litellm_client = LiteLlmClient::new(&config.litellm_base_url, config.litellm_api_key.clone());

    let worker_manager = Arc::new(WorkerManager::new(config.heartbeat_timeout_secs));
    let model_registry = Arc::new(ModelRegistry::new());
    let task_tracker = TaskTracker::new();
    let scheduler = Arc::new(Scheduler::new(
        worker_manager.clone(),
        model_registry.clone(),
    ));

    // Pre-fetch models from LiteLLM
    match litellm_client.list_models().await {
        Ok(models) => {
            info!("Loaded {} models from LiteLLM", models.len());
            model_registry.register_models(models);
        }
        Err(e) => {
            info!("LiteLLM not reachable at startup: {e} (models will load on demand)");
        }
    }

    let state = AppState {
        worker_manager: worker_manager.clone(),
        model_registry: model_registry.clone(),
        scheduler: scheduler.clone(),
        litellm_client,
        task_tracker: task_tracker.clone(),
        start_time,
        route_to_local: config.route_to_local,
    };

    // Build router
    let app = Router::new()
        // Health & info
        .route("/health", get(routes::health))
        // OpenAI-compatible API
        .route("/v1/models", get(routes::list_models))
        .route("/v1/chat/completions", post(routes::chat_completion))
        // Cluster management
        .route("/cluster/nodes", get(routes::cluster_nodes))
        .route("/cluster/stats", get(routes::cluster_stats))
        .route("/cluster/stats/min", get(routes::cluster_stats_min))
        // WebSocket for workers
        .route("/ws/worker", get(ws_worker_handler))
        .layer(TraceLayer::new_for_http())
        .layer(CorsLayer::permissive())
        .with_state(state);

    let addr = format!("{}:{}", config.listen_addr, config.port);
    info!("nmonit host starting on {addr}");
    info!("📡 REST API: http://{addr}/v1");
    info!("🔌 WebSocket: ws://{addr}/ws/worker");
    info!("🔋 LiteLLM: {}", config.litellm_base_url);

    let listener = tokio::net::TcpListener::bind(&addr).await?;

    // Run server with graceful shutdown
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;

    info!("nmonit host shut down gracefully");
    Ok(())
}

/// Handle WebSocket upgrade requests from workers.
async fn ws_worker_handler(
    ws: WebSocketUpgrade,
    State(state): State<AppState>,
) -> impl IntoResponse {
    ws.on_upgrade(move |socket| {
        ws::handle_worker_connection(
            socket,
            state.worker_manager,
            state.model_registry,
            state.task_tracker,
            None, // auth token handled by config
        )
    })
}

/// Listen for SIGTERM/SIGINT to gracefully shut down.
async fn shutdown_signal() {
    let ctrl_c = async {
        signal::ctrl_c()
            .await
            .expect("Failed to install Ctrl+C handler");
    };

    #[cfg(unix)]
    let terminate = async {
        signal::unix::signal(signal::unix::SignalKind::terminate())
            .expect("Failed to install SIGTERM handler")
            .recv()
            .await;
    };

    #[cfg(not(unix))]
    let terminate = std::future::pending::<()>();

    tokio::select! {
        _ = ctrl_c => {
            info!("Received Ctrl+C, shutting down gracefully...");
        }
        _ = terminate => {
            info!("Received SIGTERM, shutting down gracefully...");
        }
    }
}
