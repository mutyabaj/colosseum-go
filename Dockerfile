# syntax=docker/dockerfile:1

# ── Build UI ─────────────────────────────────────────────────────────────────
FROM node:22-slim AS ui-build
WORKDIR /app/ui
COPY ui/package*.json ./
RUN npm ci
COPY ui/ ./
RUN npm run build

# ── Build Go binary ───────────────────────────────────────────────────────────
FROM golang:1.25-bookworm AS go-build
WORKDIR /app
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/root/go/pkg/mod \
    go mod download
COPY . .
COPY --from=ui-build /app/ui/dist internal/api/ui/dist
RUN --mount=type=cache,target=/root/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=1 GOOS=linux go build -trimpath -ldflags="-s -w" -o bin/colosseum ./cmd/colosseum

# ── Final image ───────────────────────────────────────────────────────────────
FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        sqlite3 \
        ripgrep \
    && rm -rf /var/lib/apt/lists/*

RUN groupadd --gid 1001 colosseum \
    && useradd --uid 1001 --gid colosseum --no-create-home colosseum

WORKDIR /app
RUN mkdir -p /data/db /data/artifacts /data/workspaces \
    && chown -R colosseum:colosseum /data /app

COPY --from=go-build /app/bin/colosseum /usr/local/bin/colosseum
COPY presentation/ /app/presentation/

ENV COLOSSEUM_PORT=8080
ENV COLOSSEUM_DB_PATH=/data/db/colosseum.db
ENV COLOSSEUM_ARTIFACT_PATH=/data/artifacts
ENV COLOSSEUM_WORKSPACE_ROOT=/data/workspaces

USER colosseum

EXPOSE 8080
VOLUME ["/data"]

CMD ["colosseum", "server"]
