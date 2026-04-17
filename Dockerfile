# syntax=docker/dockerfile:1
#
# Build: podman build -t localhost/lotus-ddo-plugin:local .
#
# The plugin only talks to lotus/miner over RPC, so no FFI at link time.
# CGO_ENABLED=0 gives a static binary and a slim runtime.

FROM docker.io/library/golang:1.24-bookworm AS builder

RUN apt-get update && apt-get install -y --no-install-recommends \
        git ca-certificates \
    && rm -rf /var/lib/apt/lists/*

ARG LOTUS_VERSION=v1.34.2

# go.mod has `replace github.com/filecoin-project/lotus => ../lotus` (and
# similar for filecoin-ffi / test-vectors). Clone lotus beside the plugin
# dir so the relative replace paths resolve.
WORKDIR /src
RUN git clone --depth=1 --branch=${LOTUS_VERSION} \
        https://github.com/filecoin-project/lotus.git lotus \
    && cd lotus \
    && git submodule update --init --depth=1 \
        extern/filecoin-ffi extern/test-vectors

WORKDIR /src/plugin
COPY go.mod go.sum ./
RUN go mod download

COPY *.go ./

ENV CGO_ENABLED=0
RUN go build -trimpath -ldflags='-s -w' -o /out/lotus-ddo-plugin .

FROM docker.io/library/debian:bookworm-slim
RUN apt-get update && apt-get install -y --no-install-recommends \
        ca-certificates \
    && rm -rf /var/lib/apt/lists/*
COPY --from=builder /out/lotus-ddo-plugin /lotus-ddo-plugin
# No ENTRYPOINT -- the k8s manifest wraps with sh -c to source settings.env
# and exec the binary.
