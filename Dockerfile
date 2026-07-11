# Build stage
FROM golang:1.26.5-alpine3.24 AS builder
WORKDIR /app
COPY go.mod ./
COPY *.go ./
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o socks5-pool .

# Run stage
FROM alpine:3.24.1
RUN apk --no-cache add ca-certificates su-exec \
    && addgroup -S -g 10001 socks5 \
    && adduser -S -D -H -u 10001 -G socks5 socks5 \
    && mkdir -p /app/data \
    && chown socks5:socks5 /app/data
WORKDIR /app
COPY --from=builder /app/socks5-pool .
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

EXPOSE 1080 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null -T 3 'http://127.0.0.1:8080/healthz' || exit 1

# The entrypoint starts as root solely to repair a legacy/root-owned data
# volume, then execs this command as the unprivileged socks5 user.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/socks5-pool", "-listen", "0.0.0.0:1080", "-status", "0.0.0.0:8080", "-data-dir", "/app/data"]
