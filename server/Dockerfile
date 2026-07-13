# The claude_spawner server, containerized. SSH-native execution is unconditional: the
# container drives the host over SSH — claude runs on the host, not in this image — so
# this is deliberately lean: just the static binary. There is no host broker; the
# container reaches the host's sshd like any other SSH target.
#
# Run it with host networking (so localhost:22 is the host sshd). The server reads
# transcripts, project files, and discovers sessions on the host over SSH — no host home
# or project mount is needed (only durable state + the whisper models dir are mounted).
# See deploy/ for the compose/run recipe.
FROM golang:1.25-alpine AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . ./
RUN CGO_ENABLED=0 go build -trimpath -o /spawner ./

FROM alpine:3.20
# ca-certificates for any outbound TLS. No openssh client: all SSH (turns AND the
# restart button's host rebuild) is Go-native over the connection pool. ffmpeg
# decodes the app's Ogg/Opus voice clips to PCM/WAV before whisper (the server execs
# SPAWNER_FFMPEG_BIN in-process, so it must live in this image, not just on the host).
RUN apk add --no-cache ca-certificates ffmpeg
COPY --from=build /spawner /usr/local/bin/spawner
# The browser client bundle (Kotlin/Wasm), baked into the image and served at
# SPAWNER_WEB_DIR=/srv/web — so one artifact ships both the API and the web client,
# with no host mount. It's staged into this build context (server/webdist/) by
# deploy/rebuild-container.sh from the Gradle output (:app:wasmJsBrowserDistribution),
# built out-of-band — this image build does NOT run Gradle. Empty (.gitkeep only) when
# no bundle has been staged, which just serves no client.
COPY webdist/ /srv/web/
ENTRYPOINT ["/usr/local/bin/spawner"]
