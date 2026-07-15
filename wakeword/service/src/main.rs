//! spawner-wakeword — HTTP sidecar around the LiveKit `livekit-wakeword` Rust runtime.
//!
//! The Go gateway (see `SPAWNER_WAKEWORD_URL`) posts a VAD-gated speech clip as raw PCM and gets
//! back per-model confidence scores; it thresholds them to decide wake ("bump bump") / end
//! ("beep beep") instead of fast-transcribing the clip with Whisper and string-matching. The mel
//! front-end + Google speech embedding are compiled into the crate; only the small classifier
//! `.onnx` files load at runtime (pure-Rust `ort-tract` — no native ONNX lib; CPU only, no GPU).
//!
//! Config (env):
//!   WAKEWORD_ADDR        listen address           (default 0.0.0.0:9060)
//!   WAKEWORD_MODELS      comma-separated classifier .onnx specs, each `path` or `name=path`
//!                        (name defaults to the file stem; the score map is keyed by these names)
//!   WAKEWORD_INPUT_RATE  sample rate of the PCM the gateway sends, Hz (default 16000)
//!   WAKEWORD_WINDOW_SEC  minimum clip length, seconds (default 2.0) — the classifier scores a fixed
//!                        16-embedding window (~2s); shorter clips are left-padded with silence up to
//!                        this length so a token lands at the tail (matching training)
//!   WAKEWORD_HOP_SEC     sliding-window hop, seconds (default 0.32) — how far the detection window
//!                        advances between scores over a longer-than-window clip
//!   WAKEWORD_TAIL_SEC    detect over only the last N seconds of the clip, seconds (default 0 = whole
//!                        clip). The wake/end token lands at the END of a VAD segment (the segment
//!                        closes right after the word), so scoring just the recent tail is both
//!                        correct for hands-free detection and bounds the work to a fixed cost
//!                        (~one window) regardless of how long the clip is — the "streaming" model:
//!                        only recent audio matters. Set to ~2–3s in deployment; 0 keeps the full
//!                        slide (needed only if a token can appear mid-clip, not the hands-free case).
//!
//! Wire contract:
//!   GET  /health   -> 200 `{"status":"ok","models":["bump_bump","beep_beep"]}`
//!   POST /detect   body = little-endian i16 mono PCM at WAKEWORD_INPUT_RATE
//!                  -> 200 `{"scores":{"bump_bump":0.83,"beep_beep":0.01}}`
//!   GET  /stream   WebSocket. Client streams live PCM as binary frames (LE i16 mono at
//!                  WAKEWORD_INPUT_RATE); server keeps a rolling WAKEWORD_WINDOW_SEC buffer and,
//!                  every WAKEWORD_HOP_SEC of fresh audio, replies with a text frame
//!                  `{"scores":{...}}`. Bounded ~one-window cost per score regardless of utterance
//!                  length — the streaming path (vs POST /detect re-slicing a whole clip).
//!
//! Sliding-window mode (why): the classifier scores only the **last 16 embeddings ≈ 2s**, and the
//! models are trained with the token at the **tail** of that window. So a single score over a whole
//! "bump bump <command>" clip MISSES — the wake word has scrolled out of the tail (verified:
//! word-at-tail ~0.98, word-at-start-of-longer-clip ~0.003). To catch the token wherever it lands,
//! we slide the 16-embedding window across the clip and return the **peak** score per model. This is
//! done inside the runtime (`predict_peak`, a vendored addition): the expensive mel + embedding
//! networks run **once** over the whole clip, and only the tiny classifier re-runs per window — so
//! the whole sweep costs ≈ one detection, not N. The `WakeWordModel` is built **once at startup**
//! and reused (behind a mutex) so no per-request model construction or ONNX re-read.

use std::collections::HashMap;
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex};

use axum::{
    extract::{
        ws::{Message, WebSocket, WebSocketUpgrade},
        State,
    },
    http::StatusCode,
    response::Response,
    routing::{get, post},
    Json, Router,
};
use livekit_wakeword::wakeword::WakeWordModel;
use serde::Serialize;
use serde_json::json;

/// Approximate wall-clock spacing of one speech embedding: EMBEDDING_STRIDE (8) mel frames at the
/// mel front-end's ~10 ms/frame. Used only to convert WAKEWORD_HOP_SEC into an integer embedding
/// hop for `predict_peak`.
const EMBEDDING_PERIOD_SEC: f32 = 0.08;

/// A classifier to load: display `name` (score-map key) and its `.onnx` `path`.
#[derive(Clone)]
struct ModelSpec {
    name: String,
    path: PathBuf,
}

#[derive(Clone)]
struct AppState {
    /// The resident detector, built once and reused. `predict_peak` needs `&mut self`, so a mutex
    /// serializes detection — fine for a single-user voice remote (one clip at a time).
    model: Arc<Mutex<WakeWordModel>>,
    /// Classifier names, for /health (the model owns the score-map keys).
    names: Vec<String>,
    /// Minimum window length in samples; shorter clips are left-padded with silence to here.
    window_samples: usize,
    /// Detection-window hop in embeddings (>= 1).
    hop_embeddings: usize,
    /// If > 0, score only the last this-many samples of the clip (the recent tail). 0 = whole clip.
    tail_samples: usize,
    /// Streaming (`/stream`) hop in samples: how much fresh audio must arrive before the rolling
    /// buffer is re-scored. Bounds streaming to ~one detection per hop (>= 1).
    hop_samples: usize,
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
    let window_sec: f32 = env_f32("WAKEWORD_WINDOW_SEC", 2.0);
    let hop_sec: f32 = env_f32("WAKEWORD_HOP_SEC", 0.32);
    let tail_sec: f32 = env_f32("WAKEWORD_TAIL_SEC", 0.0);
    let window_samples = ((window_sec * input_rate as f32) as usize).max(1);
    let hop_embeddings = ((hop_sec / EMBEDDING_PERIOD_SEC).round() as usize).max(1);
    let hop_samples = ((hop_sec * input_rate as f32) as usize).max(1);
    // 0 = disabled (whole clip). Otherwise keep only the last `tail_samples`, never below one window.
    let tail_samples = if tail_sec > 0.0 {
        ((tail_sec * input_rate as f32) as usize).max(window_samples)
    } else {
        0
    };

    let specs = parse_models(&std::env::var("WAKEWORD_MODELS").unwrap_or_default());
    if specs.is_empty() {
        anyhow::bail!("WAKEWORD_MODELS is empty — set at least one classifier .onnx path");
    }
    for m in &specs {
        if !m.path.exists() {
            anyhow::bail!("model {:?} not found at {}", m.name, m.path.display());
        }
    }

    // Build the detector once (mel + embedding front-end + every classifier), keyed by our names.
    let mut model = WakeWordModel::new(&[] as &[&Path], input_rate)
        .map_err(|e| anyhow::anyhow!("model init: {e}"))?;
    for m in &specs {
        model
            .load_model(&m.path, Some(m.name.as_str()))
            .map_err(|e| anyhow::anyhow!("load {}: {e}", m.name))?;
        eprintln!("wakeword: model {} -> {}", m.name, m.path.display());
    }
    let tail_desc = if tail_samples > 0 {
        format!("{tail_sec}s tail")
    } else {
        "whole clip".to_string()
    };
    eprintln!(
        "wakeword: listening on {addr} (input rate {input_rate} Hz, min window {window_sec}s, hop {hop_sec}s = {hop_embeddings} embeddings, scoring {tail_desc})"
    );

    let state = AppState {
        model: Arc::new(Mutex::new(model)),
        names: specs.into_iter().map(|m| m.name).collect(),
        window_samples,
        hop_embeddings,
        tail_samples,
        hop_samples,
    };

    let app = Router::new()
        .route("/health", get(health))
        .route("/detect", post(detect))
        .route("/stream", get(stream))
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
    Json(json!({ "status": "ok", "models": state.names }))
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

    // predict_peak() is sync CPU work; keep it off the async runtime.
    let scores = tokio::task::spawn_blocking(move || run_predict(&state, pcm))
        .await
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, format!("join: {e}")))?
        .map_err(|e| (StatusCode::INTERNAL_SERVER_ERROR, e.to_string()))?;

    Ok(Json(DetectResponse { scores }))
}

/// Live streaming detection over a persistent WebSocket. The client streams raw little-endian i16
/// mono PCM (at `WAKEWORD_INPUT_RATE`) as binary frames — as it captures, not a closed clip. The
/// sidecar keeps a rolling `WAKEWORD_WINDOW_SEC` buffer (the live edge) and, each time
/// `WAKEWORD_HOP_SEC` of fresh audio has arrived, scores just that buffer's tail (one detection
/// window, ~constant cost independent of how long the client talks) and pushes back a text frame
/// `{"scores":{...}}`. This is the "against the grain" streaming use LiveKit's runtime is built for:
/// a small rolling buffer scored often, versus re-sliding an ever-growing clip. The gateway
/// thresholds the scores exactly as it does for `/detect`.
async fn stream(ws: WebSocketUpgrade, State(state): State<AppState>) -> Response {
    ws.on_upgrade(move |socket| stream_socket(socket, state))
}

/// Per-connection streaming loop: append incoming PCM to the rolling buffer, drop everything older
/// than one window, and re-score the live edge once `hop_samples` of fresh audio has accumulated.
async fn stream_socket(mut socket: WebSocket, state: AppState) {
    // The last `window_samples` of audio (the live edge); scored tail-first, so newest audio drives
    // detection. `since_last` gates re-scoring to ~one predict per hop.
    let mut rolling: Vec<i16> = Vec::with_capacity(state.window_samples);
    let mut since_last: usize = 0;

    while let Some(Ok(msg)) = socket.recv().await {
        let mut samples = match msg {
            Message::Binary(bytes) => bytes_to_i16(&bytes),
            Message::Close(_) => break,
            // Ignore text/ping/pong — audio rides binary frames only.
            _ => continue,
        };
        if samples.is_empty() {
            continue;
        }
        since_last += samples.len();
        rolling.append(&mut samples);
        if rolling.len() > state.window_samples {
            rolling.drain(0..rolling.len() - state.window_samples);
        }
        // Only score once the rolling buffer holds a full window of REAL audio. Left-padding a
        // partial buffer with silence (as /detect does for a complete short VAD clip) fabricates
        // high scores on a continuous stream — the silence-heavy window is an artifact, not the
        // deployment distribution. A live stream is always-on, so it simply waits out the first
        // window; thereafter it scores the live edge every hop.
        if rolling.len() < state.window_samples || since_last < state.hop_samples {
            continue;
        }
        since_last = 0;

        // Score the current window off the async runtime (sync CPU work, shares the resident
        // model's mutex). predict() scores the tail — here the whole window is the live edge.
        let buf = rolling.clone();
        let model = state.model.clone();
        let scored = tokio::task::spawn_blocking(move || {
            let mut m = model.lock().map_err(|_| "detector mutex poisoned".to_string())?;
            m.predict(&buf).map_err(|e| e.to_string())
        })
        .await;

        let scores = match scored {
            Ok(Ok(scores)) => scores,
            // A join or predict error ends the stream; the client can reconnect.
            _ => break,
        };
        let payload = match serde_json::to_string(&DetectResponse { scores }) {
            Ok(p) => p,
            Err(_) => break,
        };
        if socket.send(Message::Text(payload)).await.is_err() {
            break;
        }
    }
}

/// Left-pad the clip to the minimum window, then slide the classifier across it (peak per model)
/// on the resident model.
fn run_predict(state: &AppState, mut pcm: Vec<i16>) -> anyhow::Result<HashMap<String, f32>> {
    // Streaming/tail mode: the token lands at the clip's end, so keep only the recent tail. This
    // bounds the embedding + slide work to a fixed cost no matter how long the clip is.
    if state.tail_samples > 0 && pcm.len() > state.tail_samples {
        pcm = pcm.split_off(pcm.len() - state.tail_samples);
    }
    // A token in a short VAD clip ends at the clip's end; pad silence at the FRONT so it lands at
    // the tail of the (>=1) detection window(s), matching training.
    if pcm.len() < state.window_samples {
        let mut padded = vec![0i16; state.window_samples - pcm.len()];
        padded.append(&mut pcm);
        pcm = padded;
    }
    let mut model = state
        .model
        .lock()
        .map_err(|_| anyhow::anyhow!("detector mutex poisoned"))?;
    model
        .predict_peak(&pcm, state.hop_embeddings)
        .map_err(|e| anyhow::anyhow!("predict: {e}"))
}

/// Reinterpret a little-endian i16 PCM byte buffer as samples (trailing odd byte dropped).
fn bytes_to_i16(bytes: &[u8]) -> Vec<i16> {
    bytes
        .chunks_exact(2)
        .map(|b| i16::from_le_bytes([b[0], b[1]]))
        .collect()
}

/// Parse an f32 env var, falling back to `default` when unset or unparseable.
fn env_f32(key: &str, default: f32) -> f32 {
    std::env::var(key)
        .ok()
        .and_then(|s| s.parse().ok())
        .unwrap_or(default)
}

async fn shutdown_signal() {
    let _ = tokio::signal::ctrl_c().await;
}
