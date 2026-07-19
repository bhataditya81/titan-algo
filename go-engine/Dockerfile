# Multi-stage build for the Go trading engine.
#
# NOTE (paper/live safety): this image runs cmd/main.go. It does not run with
# -live and does not bake in any credentials — supply ANGEL_* / TITAN_API_TOKEN
# via environment variables at `docker run`/compose time (see config.example.yaml).
#
# Runtime base: Alpine rather than distroless. Alpine ships `wget`, which the
# HEALTHCHECK below (and docker-compose.yml's go-engine healthcheck) needs to
# probe the /health endpoint; distroless/base has no shell/tools at all to do
# that without shipping a second binary just for health probing.

FROM golang:1.22-bookworm AS builder
WORKDIR /src

# Cache module downloads separately from source changes.
COPY go.mod go.sum ./
RUN go mod download

COPY . .
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/titan-engine ./cmd

FROM alpine:3.20 AS runtime
RUN apk add --no-cache ca-certificates wget \
    && addgroup -S titan && adduser -S titan -G titan
WORKDIR /app

COPY --from=builder /out/titan-engine /app/titan-engine
COPY --from=builder /src/config.example.yaml /app/config.example.yaml

# Positions/risk/ledger DB files and logs — mount a volume here.
RUN mkdir -p /app/data /app/logs && chown -R titan:titan /app
VOLUME ["/app/data", "/app/logs"]

USER titan
EXPOSE 8080

HEALTHCHECK --interval=15s --timeout=3s --start-period=10s --retries=3 \
    CMD wget -q -O- http://127.0.0.1:8080/health || exit 1

ENTRYPOINT ["/app/titan-engine"]
CMD ["-paper"]
