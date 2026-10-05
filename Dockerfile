FROM golang:1.25-bookworm AS build
WORKDIR /src
COPY whatsapp-bridge/go.mod whatsapp-bridge/go.sum ./
RUN go mod download
COPY whatsapp-bridge/ ./
RUN CGO_ENABLED=1 go build -o /whatsapp-bridge .

FROM python:3.12-slim-bookworm
RUN apt-get update \
    && apt-get install -y --no-install-recommends ffmpeg ca-certificates
# The bridge's transcription sweep looks for ../whatsapp-mcp-server/.venv/bin/python3.
RUN python -m venv /app/whatsapp-mcp-server/.venv \
    && /app/whatsapp-mcp-server/.venv/bin/pip install --no-cache-dir requests
COPY whatsapp-mcp-server/transcribe.py /app/whatsapp-mcp-server/
COPY --from=build /whatsapp-bridge /app/whatsapp-bridge/whatsapp-bridge
WORKDIR /app/whatsapp-bridge
ENV BIND_ADDR=0.0.0.0
EXPOSE 8080
HEALTHCHECK --interval=30s --timeout=5s --start-period=60s --retries=3 \
    CMD python -c "import os,urllib.request as u;u.urlopen('http://127.0.0.1:%s/healthz'%os.environ.get('WHATSAPP_BRIDGE_PORT','8080'),timeout=4)"
CMD ["./whatsapp-bridge"]
