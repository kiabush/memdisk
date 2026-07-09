FROM golang:1.23-alpine AS builder
WORKDIR /src
COPY go.mod ./
COPY main.go ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/memdisk .

FROM alpine:3.20
LABEL org.opencontainers.image.title="memDisk" \
      org.opencontainers.image.description="Tiny RAM-backed HTTP file store for Docker tmpfs" \
      org.opencontainers.image.version="0.1.0" \
      org.opencontainers.image.licenses="MIT"
RUN addgroup -S memdisk && adduser -S memdisk -G memdisk \
    && mkdir -p /memdisk \
    && chown -R memdisk:memdisk /memdisk
COPY --from=builder /out/memdisk /usr/local/bin/memdisk
USER memdisk
EXPOSE 6380
ENV MEMDISK_ROOT=/memdisk \
    MEMDISK_PORT=6380 \
    MEMDISK_MAX_UPLOAD=512m \
    MEMDISK_CLEANUP_INTERVAL=30s
HEALTHCHECK --interval=30s --timeout=3s --start-period=5s --retries=3 \
  CMD wget -qO- http://localhost:6380/health || exit 1
ENTRYPOINT ["/usr/local/bin/memdisk"]
