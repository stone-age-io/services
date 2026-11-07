# One Dockerfile for every service in this repository. SERVICE picks which
# ./cmd/<service> to build, and the binary always lands at /service, so the
# entrypoint never changes.
#
#   docker build --build-arg SERVICE=watchdog -t watchdog .
#   docker run -d --name watchdog \
#     -v /etc/watchdog/user.creds:/creds/user.creds:ro \
#     -e NATS_URL=nats://hub:4222 \
#     -e NATS_CREDS=/creds/user.creds \
#     -p 8080:8080 \
#     watchdog

# --platform=$BUILDPLATFORM: with CGO_ENABLED=0 Go cross-compiles for free, so
# the compiler runs natively and only the tiny runtime stage is per-arch.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine AS build
WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .
ARG SERVICE=watchdog
ARG VERSION=0.0.0-dev
ARG TARGETOS
ARG TARGETARCH
RUN CGO_ENABLED=0 GOOS=${TARGETOS} GOARCH=${TARGETARCH} go build \
      -trimpath \
      -ldflags "-s -w -X main.version=${VERSION}" \
      -o /out/service ./cmd/${SERVICE}

# Alpine rather than distroless for busybox wget, which the HEALTHCHECK needs.
# The floating alpine:3 tag picks up base security fixes without anyone
# watching Alpine releases.
FROM alpine:3
RUN apk add --no-cache ca-certificates tzdata \
    && adduser -D -H -u 10001 service
COPY --from=build /out/service /service
USER service

# Every service serves /api/ready and /metrics on HTTP_ADDR, :8080 by default.
EXPOSE 8080

# Marks the container unhealthy on a 503; it does not restart it. Most of what
# readiness reports (NATS unreachable, bad creds) is not fixed by a restart.
HEALTHCHECK --interval=30s --timeout=5s --start-period=30s --retries=3 \
  CMD wget -q -O /dev/null "http://127.0.0.1:8080/api/ready" || exit 1

ENTRYPOINT ["/service"]
