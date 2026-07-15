# spawner-wakeword sidecar — LiveKit wake-word runtime behind an HTTP endpoint.
# Pure-Rust ONNX (ort-tract) means no native ONNX Runtime lib in the final image.
FROM rust:1-bookworm AS build
WORKDIR /src
# Cache deps: copy manifest, fetch, then the real sources.
COPY Cargo.toml ./
RUN mkdir src && echo 'fn main() {}' > src/main.rs && cargo fetch
COPY src ./src
RUN cargo build --release && cp target/release/spawner-wakeword /spawner-wakeword

FROM debian:bookworm-slim
# ort-tract is pure Rust; only need certs for good measure. Models are mounted at runtime.
RUN apt-get update && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /spawner-wakeword /usr/local/bin/spawner-wakeword
EXPOSE 9060
ENV WAKEWORD_ADDR=0.0.0.0:9060
ENTRYPOINT ["/usr/local/bin/spawner-wakeword"]
