# Resident whisper.cpp server

Docker images for a **resident whisper.cpp HTTP server** — it builds `whisper-server` from source
and keeps the model loaded in memory, so transcription doesn't reload the model per utterance. The
spawner's `RemoteWhisper` transcriber POSTs audio to it instead of forking `whisper-cli` each time.
Why this exists and how the server chooses between resident vs. CLI transcription lives in
[`../CLAUDE.md`](../CLAUDE.md) (the *Transcription* section) — this file documents the images.

## Two images

| File               | Backend        | Use                                                            |
|--------------------|----------------|---------------------------------------------------------------|
| `Dockerfile.cuda`  | **CUDA/GPU**   | Preferred on this host — whisper.cpp built with `GGML_CUDA=1`; Docker Compose exposes the Nvidia GPU. |
| `Dockerfile.vulkan`| **Vulkan/GPU** | Kept as a fallback for Vulkan-capable hosts. |
| `Dockerfile`       | **CPU**        | Portable fallback when the GPU is unavailable. Same API, no GPU deps. |

All images build the same `whisper-server` binary and expose the same HTTP API; they differ only in
the compute backend.

## Interface (contract with the spawner)

- Listens on container port **`8571`** (`--host 0.0.0.0`, `-t 4`). The model is **mounted, not
  baked** — `-v /data/storage/whisper:/models:ro` — and selected by the `command:` (`-m
  /models/ggml-<model>.bin`).
- `POST /inference` — multipart `file` (WAV) + `response_format=json`, `temperature=0.0`, optional
  `prompt` (whisper.cpp initial-prompt vocab bias). Returns the transcript as JSON. This is the
  per-utterance call `RemoteWhisper.Transcribe` makes.
- `POST /load` — multipart `model=<path>` — hot-swap the loaded model with no container restart.
  Backs the server-global whisper-model switch (`set_whisper_model` / `SPAWNER_WHISPER_MODEL_NAME`).

## How it's wired

The compose stack ships **one** resident server (see [`../docker-compose.yml`](../docker-compose.yml)):

- **`whisper`** → host `:8571`, accurate model (`medium.en`) — it handles both real dictation and
  the live hands-free draft.

The gateway points at it with `SPAWNER_WHISPER_URL`. An optional **second, fast draft server**
(`base.en` on `:8572`) can offload the high-frequency draft + end-token detection so it never
blocks the accurate model: duplicate the `whisper` service block in the compose file (new name,
port `8572:8571`, `-m /models/ggml-base.en.bin`) and set `SPAWNER_WHISPER_FAST_URL` (a commented
line ships in `deploy/spawner-container.env.example`). If neither URL is set the server falls back
to forking `whisper-cli` locally.

## Build & run standalone

```bash
# GPU (this host)
docker compose up -d --build whisper

# or a single CPU server by hand
docker build -f whisper/Dockerfile -t whisper-cpu whisper/
docker run --rm -p 8571:8571 -v /data/storage/whisper:/models:ro \
  whisper-cpu -m /models/ggml-small.en.bin
```

Model `ggml-*.bin` files live under `/data/storage/whisper` on the host. You don't have to place
them by hand: when `SPAWNER_WHISPER_MODELS_DIR` points here, the gateway **downloads catalogue models
on demand** (picked in Settings → Audio, or the boot model on a fresh start) into this directory, and
this container reads them read-only. Pre-placing a `ggml-*.bin` still works and skips the download.
