# syntax=docker/dockerfile:1

FROM golang:1.23-alpine AS builder

WORKDIR /src

COPY go.mod go.sum ./
RUN go mod download

COPY . .

RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
    -o /out/fluxgate \
    ./cmd/fluxgate

FROM alpine:3.22

# su-exec lets the entrypoint drop from the brief root bootstrap to the
# unprivileged gateway user after the data directory has been prepared.
RUN apk add --no-cache ca-certificates su-exec tzdata \
    && addgroup -S -g 10001 gateway \
    && adduser -S -D -H -u 10001 -G gateway gateway \
    && mkdir -p /data \
    && chown gateway:gateway /data

WORKDIR /app

COPY --from=builder --chown=gateway:gateway /out/fluxgate /app/fluxgate
COPY --chmod=0755 entrypoint.sh /usr/local/bin/entrypoint.sh

ENV FLUXGATE_ADDRESS=:8081 \
    FLUXGATE_DATABASE_PATH=/data/hub.db

EXPOSE 8081

VOLUME ["/data"]

HEALTHCHECK --interval=30s --timeout=5s --start-period=10s --retries=3 \
    CMD wget --no-verbose --tries=1 --spider http://127.0.0.1:8081/healthz || exit 1

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
