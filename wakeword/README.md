# wakeword — LiveKit keyword-spotting detector (in progress)

Trainer artifacts for the dedicated wake-word / end-token detector that replaces the
Whisper-as-keyword-spotter path. See the **LiveKit wake-word/end-token detector** epic in
[`../TODO.md`](../TODO.md) for the rationale, architecture (offline trainer → Rust sidecar →
gateway `Detector` swap), and open decisions — not restated here.

## What's here

- `Dockerfile.trainer` — CUDA PyTorch base with `livekit-wakeword[train,eval,export]` and its
  system deps (`espeak-ng`, `sox`, `ffmpeg`, `libsndfile1`, `portaudio19-dev`). The training runs
  entirely inside this image (bare-metal install fails on the numba/llvmlite Python-version
  conflict). Build: `docker build -f Dockerfile.trainer -t livekit-wakeword-trainer:latest .`
- `configs/bump.yaml` — START token **"bump bump"**. Fires on the doubled form and its soft
  dropped-consonant variants ("bum bum", "ump ump") — those are how the token is actually
  pronounced, so they're positives, not negatives. Guards lone singles + real doubled neighbors +
  the other token.
- `configs/beep.yaml` — END token **"beep beep"**, same doubled-monosyllable design.

## Running (GPU + RAID-backed corpora)

The clone lives at `/data/livekit-wakeword` (SSD); the ~16GB corpora and model output must land on
the RAID. Overlay RAID dirs onto the config's relative `./data` and `./output`:

```
docker run --rm --gpus all \
  -v /data/livekit-wakeword:/work \
  -v /data/storage/livekit-wakeword/data:/work/data \
  -v /data/storage/livekit-wakeword/output:/work/output \
  livekit-wakeword-trainer:latest \
  bash -lc "livekit-wakeword setup -c configs/bump.yaml && livekit-wakeword run -c configs/bump.yaml"
```

Output model: `output/<model_name>/<model_name>.onnx` (+ `.pt`, metrics, DET curve). `eval` reports
false-positives-per-hour, the metric that validates the doubled-monosyllable choice.

## `service/` — the runtime sidecar (the consumer)

`service/` is a small Rust HTTP service wrapping the [`livekit-wakeword`](https://crates.io/crates/livekit-wakeword)
crate (a workspace member of `livekit/rust-sdks`). The crate compiles the mel front-end + Google
speech-embedding into the binary and loads only the classifier `.onnx` at runtime via a **pure-Rust
ONNX runtime** (`ort-tract`) — no native ONNX library, so the image stays lean. The Go gateway posts
each VAD-gated clip and thresholds the returned scores instead of Whisper-transcribing + string
matching (`SPAWNER_WAKEWORD_URL`; empty = disabled, gateway falls back to Whisper).

Wire contract:

- `GET /health` → `{"status":"ok","models":["bump_bump","beep_beep"]}`
- `POST /detect`, body = little-endian **i16** mono PCM at `WAKEWORD_INPUT_RATE` → `{"scores":{"bump_bump":0.83,...}}`

Config (env): `WAKEWORD_ADDR` (`0.0.0.0:9060`), `WAKEWORD_MODELS` (comma-separated `.onnx` specs,
each `path` or `name=path`; name defaults to the file stem and keys the score map), `WAKEWORD_INPUT_RATE`
(`16000`).

The runtime is a **stateful streaming** detector (`predict(&mut self, &[i16])` keeps a rolling
embedding buffer), so the service constructs a fresh model per request and feeds the whole clip in
one call to keep VAD clips independent — a ~2s clip yields the ≥16 embeddings the classifier needs.
A future continuous-frame path would hold one model and stream ~80ms frames instead. Both the `dnn`
and `conv_attention` classifier heads load through the same embedding→classifier interface.

Build & run:

```
docker build -t spawner-wakeword:latest service/
docker run --rm -p 9060:9060 \
  -v /data/storage/livekit-wakeword/output:/models:ro \
  -e WAKEWORD_MODELS=/models/bump_bump/bump_bump.onnx,/models/beep_beep/beep_beep.onnx \
  spawner-wakeword:latest
```
