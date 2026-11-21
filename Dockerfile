# AgentFlow is a pure-Go binary (CGO_ENABLED=0).
FROM golang:1.26-bookworm AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build -trimpath -ldflags "-X main.version=docker" -o /src/agentflow ./cmd/agentflow

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /app --shell /usr/sbin/nologin agentflow \
    && mkdir -p /app/data \
    && chown -R agentflow /app

COPY --from=build /src/agentflow /usr/local/bin/agentflow

WORKDIR /app
USER agentflow
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/agentflow"]
