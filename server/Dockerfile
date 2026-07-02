# Dev/execution container for the claude_spawner server.
#
# Bakes in everything the server shells out to — the `claude` CLI, tmux, and
# whisper.cpp + a model — plus the Go toolchain. Source is bind-mounted at
# runtime (see docker-compose.yml), so you keep editing on the host; only the
# execution environment is containerized.
#
# Build model is selectable:  --build-arg WHISPER_MODEL=small.en  (default base.en)

ARG GO_VERSION=1.23
FROM golang:${GO_VERSION}-bookworm

ARG WHISPER_MODEL=base.en

# System deps: tmux (sessions), build tools (whisper.cpp), git/curl.
RUN apt-get update && apt-get install -y --no-install-recommends \
        tmux cmake build-essential git curl ca-certificates \
    && rm -rf /var/lib/apt/lists/*

# Node + the Claude Code CLI (the server drives `claude` headless).
RUN curl -fsSL https://deb.nodesource.com/setup_22.x | bash - \
    && apt-get install -y --no-install-recommends nodejs \
    && npm install -g @anthropic-ai/claude-code \
    && rm -rf /var/lib/apt/lists/*

# whisper.cpp: build the CLI and keep the samples (jfk.wav) for testing.
RUN git clone --depth 1 https://github.com/ggml-org/whisper.cpp /opt/whisper.cpp \
    && cmake -S /opt/whisper.cpp -B /opt/whisper.cpp/build -DCMAKE_BUILD_TYPE=Release \
    && cmake --build /opt/whisper.cpp/build -j --config Release \
    && cp /opt/whisper.cpp/build/bin/whisper-cli /usr/local/bin/whisper-cli \
    && whisper-cli --help >/dev/null 2>&1 || true

# Fetch the whisper model into /models.
RUN mkdir -p /models \
    && /opt/whisper.cpp/models/download-ggml-model.sh ${WHISPER_MODEL} \
    && cp /opt/whisper.cpp/models/ggml-${WHISPER_MODEL}.bin /models/

# Run as a NON-root user: claude refuses --dangerously-skip-permissions as root.
# uid 1000 matches the host user, so mounted ~/.claude auth lines up by ownership.
RUN useradd -m -u 1000 -s /bin/bash dev \
    && mkdir -p /workspace /app \
    && chown -R dev:dev /workspace /app /go

# Pre-cache Go modules for fast rebuilds (source itself is bind-mounted).
WORKDIR /app
COPY --chown=dev:dev go.mod go.sum ./
USER dev
ENV HOME=/home/dev
RUN go mod download

ENV SPAWNER_WHISPER_BIN=whisper-cli \
    SPAWNER_WHISPER_MODEL=/models/ggml-${WHISPER_MODEL}.bin \
    SPAWNER_WHISPER_LANG=en

EXPOSE 8080
CMD ["go", "run", "."]
