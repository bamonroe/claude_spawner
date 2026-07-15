//! spawner-wakeword — HTTP sidecar around the LiveKit `livekit-wakeword` Rust runtime.
//!
//! The Go gateway (see `SPAWNER_WAKEWORD_URL`) posts a VAD-gated speech clip as raw PCM and gets
//! back per-model confidence scores; it thresholds them to decide wake ("bump bump") / end
//! ("beep beep") instead of fast-transcribing the clip with Whisper and string-matching. The mel
//! front-end + Google speech embedding are compiled into the crate; only the small classifier
//! `.onnx` files load at runtime (pure-Rust `ort-tract` — no native ONNX lib).
//!
//! Config (env):
//!   WAKEWORD_ADDR        listen address           (default 0.0.0.0:9060)
//!   WAKEWORD_MODELS      comma-separated classifier .onnx specs, each `path` or `name=path`
//!                        (name defaults to the file stem; the score map is keyed by these names)
//!   WAKEWORD_INPUT_RATE  sample rate of the PCM the gateway sends, Hz (default 16000)
//!
//! Wire contract:
//!   GET  /health   -> 200 `{"status":"ok","models":["bump_bump","beep_beep"]}`
//!   POST /detect   body = little-endian i16 mono PCM at WAKEWORD_INPUT_RATE
//!                  -> 200 `{"scores":{"bump_bump":0.83,"beep_beep":0.01}}`
//!
//! Clip mode: the runtime is a stateful streaming detector (`predict(&mut self, &[i16])` keeps a
//! rolling embedding buffer), so to keep VAD clips independent we construct a fresh `WakeWordModel`
//! per request and feed the whole clip in one call — a ~2s clip yields the >=16 embeddings the
//! classifier needs. A future continuous-frame path would instead hold one model and stream frames.

use std::collections::HashMap;
use std::path::PathBuf;
use std::sync::Arc;

use axum::{
    extract::State,
    http::StatusCode,
    routing::{get, post},
    Json, Router,
};
use serde::Serialize;
use serde_json::json;

/// A classifier to load per request: display `name` (score-map key) and its `.onnx` `path`.
#[derive(Clone)]
struct ModelSpec {
    name: String,
    path: PathBuf,
}

#[derive(Clone)]
struct AppState {
    models: Arc<Vec<ModelSpec>>,
    input_rate: u32,
}

#[derive(Serialize)]
struct DetectResponse {
    scores: HashMap<String, f32>,
}

#[tokio::main]
async fn main() -> anyhow::Result<()> {
    let addr = std::env::var("WAKEWORD_ADDR").unwrap_or_else(|_| "0.0.0.0:9060".to_string());
    let input_rate: u32 = std::env::var("WAKEWORD_INPUT_RATE")
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(16_000);

    let models = parse_models(&std::env::var("WAKEWORD_MODELS").unwrap_or_default());
    if models.is_empty() {
        anyhow::bail!("WAKEWORD_MODELS is empty — set at least one classifier .onnx path");
    }
    for m in &models {
        if !m.path.exists() {
            anyhow::bail!("model {:?} not found at {}", m.name, m.path.display());
        }
        eprintln!("wakeword: model {} -> {}", m.name, m.path.display());
    }
    eprintln!("wakeword: listening on {addr} (input rate {input_rate} Hz)");

    let state = AppState {
        models: Arc::new(models),
        input_rate,
    };

    let app = Router::new()
        .route("/health", get(health))
        .route("/detect", post(detect))
        .with_state(state);

    let listener = tokio::net::TcpListener::bind(&addr).await?;
    axum::serve(listener, app)
        .with_graceful_shutdown(shutdown_signal())
        .await?;
    Ok(())
}

/// Parse `name=path,path,...` into specs; a bare `path` names the model after its file stem.
fn parse_models(spec: &str) -> Vec<ModelSpec> {
    spec.split(',')
        .map(str::trim)
        .filter(|s| !s.is_empty())
        .map(|entry| {
            if let Some((name, path)) = entry.split_once('=') {
                ModelSpec {
                    name: name.trim().to_string(),
                    path: PathBuf::from(path.trim()),
                }
            } else {
                let path = PathBuf::from(entry);
                let name = path
                    .file_stem()
                    .and_then(|s| s.to_str())
                    .unwrap_or(entry)
                    .to_string();
                ModelSpec { name, path }
            }
        })
        .collect()
}

async fn health(State(state): State<AppState>) -> Json<serde_json::Value> {
    let names: Vec<&str> = state.models.iter().map(|m| m.name.as_str()).collect();
    Json(json!({ "status": "ok", "models": names }))
}

/// Score one clip. Body is raw little-endian i16 mono PCM at `WAKEWORD_INPUT_RATE`.
async fn detect(
    State(state): State<AppState>,
    body: axum::body::Bytes,
) -> Result<Json<DetectResponse>, (StatusCode, String)> {
    if body.len() < 2 {
        return Err((StatusCode::BAD_REQUEST, "empty PCM body".to_string()));
    }
    let pcm = bytes_to_i16(&body);

    // predict() is sync CPU work + reads the onnx files; keep it off the async runtime.
    let scores = tokio::task::spawn_blocking(move || run_predict(&state, &pcm))
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("join: {e}")))?
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?;

    Ok(Json(DetectResponse { scores }))
}

/// Construct a fresh (clip-independent) model over all classifiers and score the clip.
fn run_predict(state: &AppState, pcm: &[i16]) -> anyhow::Result<HashMap<String, f32>> {
    let paths: Vec<&std::path::Path> = state.models.iter().map(|m| m.path.as_path()).collect();
    let mut model = livekit_wakeword::wakeword::WakeWordModel::new(&paths, state.input_rate)
        .map_err(|e| anyhow::anyhow!("model load: {e}"))?;
    let scores = model
        .predict(pcm)
        .map_err(|e| anyhow::anyhow!("predict: {e}"))?;
    Ok(scores)
}

/// Reinterpret a little-endian i16 PCM byte buffer as samples (trailing odd byte dropped).
fn bytes_to_i16(bytes: &[u8]) -> Vec<i16> {
    bytes
        .chunks_exact(2)
        .map(|b| i16::from_le_bytes([b[0], b[1]]))
        .collect()
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}
