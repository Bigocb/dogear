# dogear-tts: Piper neural read-aloud for the Dogear reader.
#
# The rhasspy Piper release binary is glibc-linked and bundles its own
# onnxruntime/espeak libs, so a Debian base is used (not Alpine).
FROM debian:bookworm-slim

RUN apt-get update \
 && apt-get install -y --no-install-recommends ca-certificates wget python3 \
 && rm -rf /var/lib/apt/lists/*

ARG PIPER_URL=https://github.com/rhasspy/piper/releases/download/2023.11.14-2/piper_linux_x86_64.tar.gz
RUN mkdir -p /opt/piper \
 && wget -qO- "$PIPER_URL" | tar -xz -C /opt/piper --strip-components=1 \
 && chmod +x /opt/piper/piper

# Bundle the default voice (~63MB). Add more by dropping .onnx + .onnx.json
# into the mounted /voices volume at runtime.
ARG VOICE_URL=https://huggingface.co/rhasspy/piper-voices/resolve/main/en/en_US/lessac/medium/en_US-lessac-medium.onnx
RUN mkdir -p /voices \
 && wget -q -O /voices/en_US-lessac-medium.onnx "$VOICE_URL" \
 && wget -q -O /voices/en_US-lessac-medium.onnx.json "$VOICE_URL.json"

COPY app.py /app/app.py

ENV PIPER_BIN=/opt/piper/piper \
    VOICES_DIR=/voices \
    PORT=8097
EXPOSE 8097
ENTRYPOINT ["python3", "/app/app.py"]
