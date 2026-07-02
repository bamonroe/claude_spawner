# Self-contained resident whisper.cpp server. Builds whisper-server from source
# (no dependency on host-extracted binaries) and keeps the model loaded, so
# transcription doesn't reload it per utterance. Only the model is mounted.
FROM debian:bookworm-slim AS build
RUN apt-get update \
    && apt-get install -y --no-install-recommends git cmake g++ make ca-certificates \
    && rm -rf /var/lib/apt/lists/*
RUN git clone --depth 1 https://github.com/ggml-org/whisper.cpp /src \
    && cmake -S /src -B /src/build -DCMAKE_BUILD_TYPE=Release \
    && cmake --build /src/build -j --target whisper-server \
    && mkdir -p /out/lib \
    && cp /src/build/bin/whisper-server /out/ \
    && find /src/build -name '*.so*' -exec cp -av {} /out/lib/ \;

FROM debian:bookworm-slim
RUN apt-get update \
    && apt-get install -y --no-install-recommends libgomp1 libstdc++6 ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=build /out/whisper-server /usr/local/bin/whisper-server
COPY --from=build /out/lib/ /usr/local/lib/
RUN ldconfig

EXPOSE 8571
# Model chosen by the compose command (default small.en = same accuracy as the
# current setup, just resident). Swap to a bigger ggml for more accuracy.
ENTRYPOINT ["whisper-server", "--host", "0.0.0.0", "--port", "8571", "-t", "4"]
CMD ["-m", "/models/ggml-small.en.bin"]
