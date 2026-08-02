# Build stage
FROM golang:1.26.5-alpine3.24@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS builder
WORKDIR /app
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o socks5-pool .

# Run stage
FROM alpine:3.24.1@sha256:28bd5fe8b56d1bd048e5babf5b10710ebe0bae67db86916198a6eec434943f8b
RUN apk --no-cache add ca-certificates=20260611-r0 su-exec=0.3-r0 \
    && addgroup -S -g 10001 socks5 \
    && adduser -S -D -H -u 10001 -G socks5 socks5 \
    && mkdir -p /app/data \
    && chown socks5:socks5 /app/data
WORKDIR /app
COPY --from=builder /app/socks5-pool .
COPY --chmod=755 docker-entrypoint.sh /usr/local/bin/docker-entrypoint.sh

EXPOSE 1080 1081-1180 8080

HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
    CMD su-exec socks5:socks5 wget -q -O /dev/null -T 3 'http://127.0.0.1:8080/healthz' || exit 1

# The entrypoint starts as root solely to repair a legacy/root-owned data
# volume, then execs this command as the unprivileged socks5 user.
ENTRYPOINT ["/usr/local/bin/docker-entrypoint.sh"]
CMD ["/app/socks5-pool", "-listen", "0.0.0.0:1080", "-status", "0.0.0.0:8080", "-data-dir", "/app/data"]
