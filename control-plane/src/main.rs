//! Freehold control plane — scaffold (Phase A1).
//!
//! The management layer for ONE relay scope: provisioner, grants, readiness,
//! and the local web UI. Phase A just proves the crate builds and serves;
//! provisioner (B), UI (F) land in later phases.

use axum::{routing::get, Json, Router};
use serde_json::{json, Value};

async fn root() -> Json<Value> {
    Json(json!({
        "name": "freehold-control-plane",
        "status": "bootstrap",
        "services": []
    }))
}

async fn healthz() -> &'static str {
    "ok"
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    tracing_subscriber::fmt()
        .with_env_filter(
            tracing_subscriber::EnvFilter::try_from_default_env()
                .unwrap_or_else(|_| "info".into()),
        )
        .init();

    let addr = std::env::var("FREEFOLD_CP_ADDR").unwrap_or_else(|_| "127.0.0.1:8080".into());
    let app = Router::new()
        .route("/", get(root))
        .route("/healthz", get(healthz));

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    tracing::info!(%addr, "control plane listening");
    axum::serve(listener, app)
        .with_graceful_shutdown(async {
            tokio::signal::ctrl_c().await.ok();
        })
        .await?;
    Ok(())
}
