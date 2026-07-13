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
# Silero VAD model, baked in (it's infrastructure, not a user-swappable STT model,
# so it stays out of the /models mount). Pinned to a repo revision.
ADD --chmod=644 https://huggingface.co/ggml-org/whisper-vad/resolve/9ffd54a1e1ee413ddf265af9913beaf518d1639b/ggml-silero-v5.1.2.bin /usr/local/share/whisper-vad/ggml-silero-v5.1.2.bin

EXPOSE 8571
# Model chosen by the compose command (default small.en = same accuracy as the
# current setup, just resident). Swap to a bigger ggml for more accuracy.
# --vad strips silence before decoding and --suppress-nst drops non-speech tokens:
# together they stop Whisper hallucinating text ("Thanks for watching…") on
# silent stretches. Per-request fields can still override both.
ENTRYPOINT ["whisper-server", "--host", "0.0.0.0", "--port", "8571", "-t", "4", \
            "--vad", "--vad-model", "/usr/local/share/whisper-vad/ggml-silero-v5.1.2.bin", \
            "--suppress-nst"]
CMD ["-m", "/models/ggml-small.en.bin"]
