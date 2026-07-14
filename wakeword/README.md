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
