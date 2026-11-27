FROM golang:1.26-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/headunit ./cmd/headunit

FROM alpine:3.23

RUN apk add --no-cache ffmpeg su-exec

COPY --from=builder /out/headunit /usr/local/bin/headunit
COPY scripts/entrypoint.sh /usr/local/bin/entrypoint.sh

RUN chmod +x /usr/local/bin/entrypoint.sh && mkdir -p /data /music

ENV PORT=4533 \
    MUSIC_DIR=/music \
    DATA_DIR=/data \
    SCAN_INTERVAL=5m \
    PUID=1000 \
    PGID=1000

EXPOSE 4533

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -T 5 -O - "http://127.0.0.1:${PORT:-4533}/healthz" | grep -qx 'ok'

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
