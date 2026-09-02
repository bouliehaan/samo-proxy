# Multi-stage: compile the proxy, then ship it next to ffmpeg and nothing else.
#
# ffmpeg is a hard runtime dependency, not an optional extra — it is the entire
# reason this is a Go service rather than a Caddy config. The alpine ffmpeg
# package carries libopus, which is the default encoder.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Dependencies first so a source-only change does not re-resolve modules.
# samo-proxy has no third-party dependencies today; this still costs nothing and
# keeps the layer boundary right if that changes.
COPY go.mod ./
RUN go mod download

COPY cmd ./cmd
COPY internal ./internal

# CGO off so the binary runs on a bare alpine layer with no libc surprises.
# Trimpath keeps build-host paths out of panics.
RUN CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o /out/samo-proxy ./cmd/samo-proxy

FROM alpine:3.21

RUN apk add --no-cache ffmpeg ca-certificates tzdata wget \
    && adduser -D -u 10001 samoproxy

COPY --from=build /out/samo-proxy /usr/local/bin/samo-proxy

# The cache is a volume in compose. Create it here so the container also works
# when run without one.
RUN mkdir -p /var/cache/samo-proxy && chown samoproxy:samoproxy /var/cache/samo-proxy

USER samoproxy

EXPOSE 6767

# Checks the proxy AND its view of the origin: a proxy that is up but cannot see
# samo-server is not serving anything, and `restart: unless-stopped` alone never
# notices because the process is alive.
HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget -q -O /dev/null http://127.0.0.1:6767/_samoproxy/health || exit 1

ENTRYPOINT ["/usr/local/bin/samo-proxy"]
