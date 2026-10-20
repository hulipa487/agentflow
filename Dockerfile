# AgentFlow is a cgo binary: the Luau VM is compiled from the pinned submodule.
# The build stage therefore needs a C/C++ toolchain, unlike a pure-Go service.
FROM golang:1.26-bookworm AS build

ENV DEBIAN_FRONTEND=noninteractive
RUN apt-get update \
    && apt-get install -y --no-install-recommends build-essential ca-certificates git make \
    && rm -rf /var/lib/apt/lists/*

WORKDIR /src
COPY . .
RUN make VERSION=docker

FROM debian:bookworm-slim

RUN apt-get update \
    && apt-get install -y --no-install-recommends ca-certificates libstdc++6 \
    && rm -rf /var/lib/apt/lists/* \
    && useradd --system --uid 65532 --home-dir /app --shell /usr/sbin/nologin agentflow \
    && mkdir -p /app/data \
    && chown -R agentflow /app

COPY --from=build /src/agentflow /usr/local/bin/agentflow

WORKDIR /app
USER agentflow
EXPOSE 8080 9090
ENTRYPOINT ["/usr/local/bin/agentflow"]
