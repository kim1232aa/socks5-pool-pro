# Build stage
FROM golang:1.26.5-alpine3.24@sha256:39c3b17beedd6642dcd418279a3a24d1b76b355302921a35952320bd2d9b15ba AS builder
WORKDIR /app
COPY go.mod ./
COPY *.go ./
COPY web ./web
RUN CGO_ENABLED=0 go build -ldflags="-s -w" -o socks5-pool .

# Run stage
FROM alpine:3.24.1@sha256:23405d96454ccc13b5e1a1ba2bab66e3659b703eb6d8df98befca4c93248ff0e
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
