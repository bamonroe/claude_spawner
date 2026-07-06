# Resident whisper.cpp server

Docker images for a **resident whisper.cpp HTTP server** — it builds `whisper-server` from source
and keeps the model loaded in memory, so transcription doesn't reload the model per utterance. The
spawner's `RemoteWhisper` transcriber POSTs audio to it instead of forking `whisper-cli` each time.
Why this exists and how the server chooses between resident vs. CLI transcription lives in
[`../CLAUDE.md`](../CLAUDE.md) (the *Transcription* section) — this file documents the images.

## Two images

| File               | Backend        | Use                                                            |
|--------------------|----------------|---------------------------------------------------------------|
| `Dockerfile.vulkan`| **Vulkan/GPU** | Preferred on this host — whisper.cpp built with `GGML_VULKAN=1`, runs on the AMD RX 550 via Mesa RADV. |
| `Dockerfile`       | **CPU**        | Portable fallback when the GPU is unavailable. Same API, no GPU deps. |

Both build the same `whisper-server` binary and expose the same HTTP API; they differ only in the
compute backend. The Vulkan image also installs newer Khronos Vulkan-Headers (v1.3.290) because
Debian's are too old for current whisper.cpp.

## Interface (contract with the spawner)

- Listens on container port **`8571`** (`--host 0.0.0.0`, `-t 4`). The model is **mounted, not
  baked** — `-v ~/.local/share/whisper:/models:ro` — and selected by the `command:` (`-m
  /models/ggml-<model>.bin`).
- `POST /inference` — multipart `file` (WAV) + `response_format=json`, `temperature=0.0`, optional
  `prompt` (whisper.cpp initial-prompt vocab bias). Returns the transcript as JSON. This is the
  per-utterance call `RemoteWhisper.Transcribe` makes.
- `POST /load` — multipart `model=<path>` — hot-swap the loaded model with no container restart.
  Backs the server-global whisper-model switch (`set_whisper_model` / `SPAWNER_WHISPER_MODEL_NAME`).

## How it's wired

Two servers run side by side (see [`../docker-compose.yml`](../docker-compose.yml)):

- **`whisper`** → host `:8571`, accurate model (`medium.en`) — real dictation.
- **`whisper-fast`** → host `:8572`, fast draft model (`base.en`) — the live hands-free draft +
  end-token detection, so the cheap high-frequency work never blocks the accurate model.

The bare-metal server points at them with `SPAWNER_WHISPER_URL` / `SPAWNER_WHISPER_FAST_URL` (set in
`deploy/spawner-server.env.example`). Start the two servers with `docker compose up -d whisper
whisper-fast`; if neither URL is set the server falls back to forking `whisper-cli` locally.

## Build & run standalone

```bash
# GPU (this host)
docker compose up -d --build whisper whisper-fast

# or a single CPU server by hand
docker build -f whisper/Dockerfile -t whisper-cpu whisper/
docker run --rm -p 8571:8571 -v ~/.local/share/whisper:/models:ro \
  whisper-cpu -m /models/ggml-small.en.bin
```

Measured on the RX 550: `medium.en` ~4.8 s/clip, `small.en` ~2–3 s, `large-v3` ~10.5 s (≈3–4× the
CPU build). Model `ggml-*.bin` files are expected under `~/.local/share/whisper` on the host.
