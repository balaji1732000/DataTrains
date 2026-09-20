# syntax=docker/dockerfile:1.7

FROM golang:1.27.1-bookworm AS build

WORKDIR /src/services/api
COPY services/api/go.mod services/api/go.sum ./
RUN go mod download

COPY services/api ./
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trajectory-api ./cmd/api \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trajectory-worker ./cmd/worker \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trajectory-migrate ./cmd/migrate \
    && CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" -o /out/trajectory-bootstrap-admin ./cmd/bootstrap-admin

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install --yes --no-install-recommends ca-certificates ffmpeg \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --no-create-home --shell /usr/sbin/nologin datatrains

WORKDIR /app
COPY --from=build /out/trajectory-api /app/trajectory-api
COPY --from=build /out/trajectory-worker /app/trajectory-worker
COPY --from=build /out/trajectory-migrate /app/trajectory-migrate
COPY --from=build /out/trajectory-bootstrap-admin /app/trajectory-bootstrap-admin
COPY services/api/migrations /app/migrations
COPY packages/trajectory-schema/schemas/trajectory-v1.json /app/schema/trajectory-v1.json

ENV TRAJECTORY_MIGRATIONS_DIR=/app/migrations \
    TRAJECTORY_SCHEMA_PATH=/app/schema/trajectory-v1.json \
    TRAJECTORY_BLOB_ROOT=/tmp/trajectory/blobstore \
    TRAJECTORY_RUN_MIGRATIONS=false

EXPOSE 8080
USER 65532:65532
ENTRYPOINT ["/app/trajectory-api"]
