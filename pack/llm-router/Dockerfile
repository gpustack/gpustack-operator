# One image carrying every router a ModelDeployment can name.
#
# WHAT: three independent router binaries built from three upstream sources into one runtime layer.
# `epp` is the llm-d endpoint picker, `vllm-router` is the vLLM project's router, and
# `sgl-model-gateway` is the SGLang model gateway. Nothing here couples them: a router Pod runs
# exactly one of the three, named by the workload's command.
#
# WHY ONE IMAGE RATHER THAN THREE: the router a deployment names is a field on the object, so the
# operator would otherwise resolve one image setting per router value and a cluster would have to
# mirror, pin and redirect three of them to make the field usable at all. One image makes the
# enum's cost flat -- widening it later adds a binary to this file, not an image to every cluster's
# mirroring plan.
#
# WHY NOT BUILD ON THE PUBLISHED llm-d IMAGE: its runtime stage is `gcr.io/distroless/static`,
# which carries no shell, no package manager and no dynamic loader for anything but a static
# binary. The other two routers are ordinary dynamically linked Rust binaries, so they cannot be
# added to it. The only shape that would work is copying the picker OUT of a published image, and
# that pins this project to upstream's release cadence: a patch, or a build of an unreleased
# reference, would have nowhere to go. Building all three from source is what keeps each version
# an argument.
#
# VERSIONS: one ARG per router, each naming an upstream git reference. They move independently
# because the three projects release independently; nothing here requires them to agree.
#
# NOT IN THIS IMAGE: the llm-d disaggregation sidecar. It runs in a DECODE role's Pod, not in a
# router Pod, so putting it here would pull a router image into every decoder.

ARG UBUNTU_VERSION=24.04
ARG GO_VERSION=1.26-bookworm
ARG RUST_VERSION=1.95-bookworm

ARG LLMD_ROUTER_VERSION=v0.10.0
ARG VLLM_ROUTER_VERSION=v0.1.15
ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1

######################## llm-d endpoint picker ########################

FROM golang:${GO_VERSION} AS epp-builder

SHELL ["/bin/bash", "-c"]

ARG LLMD_ROUTER_VERSION

WORKDIR /src

# Shallow and single-tag: this build never reads history, and the full clone is large enough for
# the difference to show up on every rebuild.
#
# SUBMODULES ARE RECURSED THOUGH NONE OF THE THREE SOURCES DECLARES ANY at the references pinned
# above. The flag is here for the version bump rather than for today: a source that grows a
# submodule later would otherwise check out short, and the failure is a compile error in a file
# nobody edited rather than anything naming the missing tree. --shallow-submodules keeps the
# recursion as cheap as the --depth above.
RUN <<EOF
    set -exo pipefail

    git clone --depth 1 --branch "${LLMD_ROUTER_VERSION}" \
        --recurse-submodules --shallow-submodules \
        https://github.com/llm-d/llm-d-router.git .
    git rev-parse HEAD >/tmp/commit
EOF

# CGO stays off to match upstream's own build: the picker links nothing from the C world, and a
# static binary is what lets the runtime stage below be an ordinary base image rather than one
# chosen to satisfy this binary's loader.
#
# THE VERSION PACKAGE IS RESOLVED BEFORE IT IS STAMPED, because `go build` accepts an -X flag
# naming a symbol that does not exist and silently writes nothing. A bump that renamed or moved
# that package would therefore produce a binary reporting an empty version, and the build would
# succeed. `go list` on the same path in the same step turns that into a build failure naming the
# path it could not resolve.
RUN <<EOF
    set -exo pipefail

    go list github.com/llm-d/llm-d-router/version

    COMMIT="$(cat /tmp/commit)"
    CGO_ENABLED=0 go build \
        -ldflags="-s -w \
            -X github.com/llm-d/llm-d-router/version.CommitSHA=${COMMIT} \
            -X github.com/llm-d/llm-d-router/version.BuildRef=${LLMD_ROUTER_VERSION}" \
        -o /out/epp ./cmd/epp
EOF

######################## shared Rust build environment ########################

FROM rust:${RUST_VERSION} AS rust-builder

SHELL ["/bin/bash", "-c"]

# protobuf-compiler is required rather than optional: both routers pull tonic/prost, which run
# protoc at build time. libprotobuf-dev goes with it and is NOT redundant -- the compiler package
# ships the binary while the well-known types (google/protobuf/timestamp.proto and struct.proto)
# live in the dev package, and the gateway's schema imports both. Without it protoc reports them as
# "File not found" and the build stops in a generated build script, which reads as a source defect
# rather than a missing package.
#
# libssl-dev and pkg-config serve the openssl crate; the runtime counterpart is installed in the
# final stage.
RUN <<EOF
    set -exo pipefail

    apt-get update -y
    apt-get install -y --no-install-recommends \
        build-essential \
        ca-certificates \
        git \
        libprotobuf-dev \
        libssl-dev \
        pkg-config \
        protobuf-compiler
    rm -rf /var/lib/apt/lists/*
EOF

######################## vLLM router ########################

FROM rust-builder AS vllm-router-builder

SHELL ["/bin/bash", "-c"]

ARG VLLM_ROUTER_VERSION

WORKDIR /src

# wit/ comes with the clone and is load-bearing: the wasm middleware's bindgen reads it at compile
# time, so a source tree trimmed to src/ alone does not build.
RUN <<EOF
    set -exo pipefail

    git clone --depth 1 --branch "${VLLM_ROUTER_VERSION}" \
        --recurse-submodules --shallow-submodules \
        https://github.com/vllm-project/router.git .
EOF

# The patches under pack/llm-router/patches/ repair upstream defects this image cannot wait
# out; each patch header states the defect it repairs and the upstream reference it was
# written against. Applied with git apply, which fails the build loudly on a version bump
# whose hunks no longer match -- that failure is the tripwire, and git is already in the
# base stage so no package is added for this.
COPY pack/llm-router/patches/ /tmp/patches/
RUN <<EOF
    set -exo pipefail

    git apply /tmp/patches/*.patch
    rm -rf /tmp/patches
EOF

# Upstream's own Dockerfile builds this on a nightly toolchain. Stable is used here instead,
# because the source declares no unstable feature and upstream's own lint and format jobs run on a
# pinned stable release -- so nightly is incidental there and would be a moving dependency here.
#
# --locked, because this source ships a Cargo.lock: without it the pinned reference fixes the
# router's own version while leaving its dependency graph free to resolve differently on any two
# builds, which is the one thing the version argument above exists to prevent. It also turns a
# lockfile that upstream lets drift from its manifests into a build failure rather than a silent
# re-resolution. The gateway stage below does NOT take this flag, and that asymmetry is measured
# rather than stylistic -- see the comment there.
RUN <<EOF
    set -exo pipefail

    cargo build --release --locked --bin vllm-router
    mkdir -p /out
    cp target/release/vllm-router /out/vllm-router
EOF

######################## SGLang model gateway ########################

FROM rust-builder AS sglang-gateway-builder

SHELL ["/bin/bash", "-c"]

ARG SGLANG_GATEWAY_VERSION

WORKDIR /src

# The gateway is one crate inside a repository that carries the whole engine, so the checkout is
# narrowed to it. Without the narrowing this stage downloads gigabytes to compile a few megabytes.
#
# SUBMODULES ARE INITIALIZED AFTER THE NARROWING RATHER THAN DURING THE CLONE, which is where this
# stage differs from the other two. A sparse clone checks out the root alone, so recursion at clone
# time would see whatever submodules the root declares and none of the ones the narrowed path holds.
# Running it after sparse-checkout is what makes the recursion cover the tree this stage compiles.
RUN <<EOF
    set -exo pipefail

    git clone --depth 1 --branch "${SGLANG_GATEWAY_VERSION}" \
        --filter=blob:none --sparse \
        https://github.com/sgl-project/sglang.git .
    git sparse-checkout set sgl-model-gateway
    git submodule update --init --recursive --depth 1
EOF

WORKDIR /src/sgl-model-gateway

# No toolchain is pinned in this stage on purpose: it inherits the stable toolchain that
# RUST_VERSION pins once for both Rust stages, and upstream ships no rust-toolchain.toml for
# this crate at the pinned reference, so a second pin here would only drift from the first.
#
# NO --locked HERE, unlike the vLLM router stage, because upstream ships no Cargo.lock for this
# crate at the pinned reference -- neither beside it nor at the repository root. The flag would
# fail this build outright rather than make it reproducible, so the reproducibility the other
# stage gets is simply not available here, and saying so is what keeps the next reader from
# "fixing" the inconsistency.
RUN <<EOF
    set -exo pipefail

    cargo build --release --bin sgl-model-gateway
    mkdir -p /out
    cp target/release/sgl-model-gateway /out/sgl-model-gateway
EOF

######################## runtime ########################

FROM ubuntu:${UBUNTU_VERSION}

SHELL ["/bin/bash", "-c"]

# libssl3t64 is the runtime half of the openssl crate the gateway links; ca-certificates is what
# lets any of the three reach an HTTPS endpoint at all.
RUN <<EOF
    set -exo pipefail

    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        libssl3t64
    rm -rf /var/lib/apt/lists/*

    groupadd --gid 65532 nonroot
    useradd --uid 65532 --gid 65532 --no-create-home --shell /usr/sbin/nologin nonroot
EOF

COPY --from=epp-builder /out/epp /usr/local/bin/epp
COPY --from=vllm-router-builder /out/vllm-router /usr/local/bin/vllm-router
COPY --from=sglang-gateway-builder /out/sgl-model-gateway /usr/local/bin/sgl-model-gateway

USER 65532:65532

# llm-d endpoint picker: ext_proc gRPC, gRPC health, Prometheus, KV-events ZMQ SUB.
EXPOSE 9002 9003 9090 5557
# vLLM router and SGLang model gateway: HTTP serving and Prometheus.
EXPOSE 30000 29000

# NO ENTRYPOINT ON PURPOSE. One image holds three programs, so an entrypoint here would name one of
# them as the default and make the other two reachable only by overriding it. The workload names
# the binary it wants, which is the same place its arguments already come from.
