# Build target: the default final stage bundles epp, vllm-router, sgl-model-gateway (frozen) and sgl-router.
# Platforms: linux/amd64 and linux/arm64.
# Run from the repository root:
#   PACKAGE_ARCH=amd64 PACKAGE_TAG=dev make package llm-router
# Active router pins pair *_VERSION tags with optional *_COMMIT overrides below
# (SGLANG_GATEWAY_VERSION is frozen and has no commit override). Pass overrides with Docker Buildx:
#   docker buildx build --platform linux/amd64 --load -t gpustack/llm-router:dev \
#     --build-arg VLLM_ROUTER_VERSION=v0.1.17 --build-arg VLLM_ROUTER_COMMIT="" -f pack/llm-router/Dockerfile .

# One image carrying every router a ModelDeployment can name.
#
# WHAT: four independent router binaries built from upstream sources into one runtime layer.
# `epp` is the llm-d endpoint picker, `vllm-router` is the vLLM project's router,
# `sgl-model-gateway` is the frozen legacy SGLang model gateway, and `sgl-router` is the
# SGLang router. Nothing here couples them: a router Pod runs exactly one of them, named
# by the workload's command.
#
# WHY ONE IMAGE RATHER THAN SEPARATE IMAGES: the router a deployment names is a field on the object, so the
# operator would otherwise resolve one image setting per router value and a cluster would have to
# mirror, pin and redirect all of them to make the field usable at all. One image makes the
# enum's cost flat -- widening it later adds a binary to this file, not an image to every cluster's
# mirroring plan.
#
# WHY NOT BUILD ON THE PUBLISHED llm-d IMAGE: its runtime stage is `gcr.io/distroless/static`,
# which carries no shell, no package manager and no dynamic loader for anything but a static
# binary. The other routers are ordinary dynamically linked Rust binaries, so they cannot be
# added to it. The only shape that would work is copying the picker OUT of a published image, and
# that pins this project to upstream's release cadence: a patch, or a build of an unreleased
# reference, would have nowhere to go. Building all of them from source is what keeps each version
# an argument.
#
# VERSIONS: each active router pairs a *_VERSION tag with an optional *_COMMIT SHA override
# (LLMD_ROUTER_*, VLLM_ROUTER_*, SGL_ROUTER_*). When *_COMMIT is non-empty, its builder fetches
# that commit directly; when empty, it clones the *_VERSION tag. They move independently because
# the upstream projects release independently; nothing here requires them to agree.
#
# NOT IN THIS IMAGE: the llm-d disaggregation sidecar. It runs in a DECODE role's Pod, not in a
# router Pod, so putting it here would pull a router image into every decoder.

ARG UBUNTU_VERSION=24.04
ARG GO_VERSION=1.27-bookworm
ARG RUST_VERSION=1.95-bookworm

ARG LLMD_ROUTER_VERSION=v1.0.0
ARG LLMD_ROUTER_COMMIT=3283d440334109fc785d4380355cc0375fc32755
ARG VLLM_ROUTER_VERSION=v0.1.17
ARG VLLM_ROUTER_COMMIT=c132dbc0cfdf5c642c3dca6caa71218f3c3035cb
# FROZEN: upstream deleted sgl-model-gateway in sgl-project/sglang PR #50732 (commit 2588d80d062).
# Keep gateway-v0.3.1 pinned until sglang-gateway is removed after its deprecation window.
ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1
ARG SGL_ROUTER_VERSION=router-v0.1.0
ARG SGL_ROUTER_COMMIT=51c77c89f889050599b6e6e4838a44c3b95d8384

######################## llm-d endpoint picker ########################

FROM golang:${GO_VERSION} AS epp-builder

SHELL ["/bin/bash", "-c"]

ARG LLMD_ROUTER_VERSION
ARG LLMD_ROUTER_COMMIT

WORKDIR /src

# Shallow single-commit fetch when LLMD_ROUTER_COMMIT is set, or single-tag clone when empty.
# Submodules are initialized with depth 1 so any future upstream submodule checks out cleanly.
RUN <<EOF
    set -exo pipefail

    if [ -n "${LLMD_ROUTER_COMMIT}" ]; then
        git init .
        git remote add origin https://github.com/llm-d/llm-d-router.git
        git fetch --depth 1 origin "${LLMD_ROUTER_COMMIT}"
        git checkout FETCH_HEAD
        git submodule update --init --recursive --depth 1
    else
        git clone --depth 1 --branch "${LLMD_ROUTER_VERSION}" \
            --recurse-submodules --shallow-submodules \
            https://github.com/llm-d/llm-d-router.git .
    fi
    git rev-parse HEAD >/tmp/commit
EOF

# Native observation uses the existing metrics server and preserves routing decisions.
# A version whose source no longer matches the patch fails before compilation.
COPY pack/llm-router/patches/llm-d/ /tmp/patches/
RUN <<EOF
    set -exo pipefail

    git apply /tmp/patches/*.patch
    rm -rf /tmp/patches
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
    CGO_ENABLED=0 go test ./cmd/epp/runner -run TestObserverSupportsRenderedOperatorProfiles
    CGO_ENABLED=0 go test ./pkg/epp/observer/...

    COMMIT="$(cat /tmp/commit)"
    BUILD_REF="${LLMD_ROUTER_VERSION}"
    if [ -n "${LLMD_ROUTER_COMMIT}" ]; then
        BUILD_REF="${LLMD_ROUTER_VERSION}+$(printf '%.7s' "${LLMD_ROUTER_COMMIT}")"
    fi
    CGO_ENABLED=0 go build \
        -ldflags="-s -w \
            -X github.com/llm-d/llm-d-router/version.CommitSHA=${COMMIT} \
            -X github.com/llm-d/llm-d-router/version.BuildRef=${BUILD_REF}" \
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
ARG VLLM_ROUTER_COMMIT

WORKDIR /src

# wit/ comes with the clone and is load-bearing: the wasm middleware's bindgen reads it at compile
# time, so a source tree trimmed to src/ alone does not build.
RUN <<EOF
    set -exo pipefail

    if [ -n "${VLLM_ROUTER_COMMIT}" ]; then
        git init .
        git remote add origin https://github.com/vllm-project/router.git
        git fetch --depth 1 origin "${VLLM_ROUTER_COMMIT}"
        git checkout FETCH_HEAD
        git submodule update --init --recursive --depth 1
    else
        git clone --depth 1 --branch "${VLLM_ROUTER_VERSION}" \
            --recurse-submodules --shallow-submodules \
            https://github.com/vllm-project/router.git .
    fi
EOF

# The patches under pack/llm-router/patches/vllm-router/ repair upstream gaps this
# image cannot wait out; each patch header states what it adds and the upstream reference
# it was written against. The per-router subdirectory is copied rather than the patches root
# because each Rust stage compiles a different upstream tree.
#
# Earlier patches (EPD routing 0001, discovery readiness deselect 0004, and /metrics KV cache
# metadata discovery) were retired once upstream absorbed them. The two active patches (lazy
# Mooncake bootstrap lookup 0002 and read-only observer endpoint 0003) remain until their
# WHEN TO REMOVE condition holds.
#
# Applied with git apply, which fails the build loudly on a version bump whose hunks no
# longer match -- that failure is the tripwire, and git is already in the base stage so no
# package is added for this.
COPY pack/llm-router/patches/vllm-router/ /tmp/patches/
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

# The patch is written with repository-root-relative paths (sgl-model-gateway/src/...),
# which is how the clone is laid out, so it is applied from the CLONE ROOT. git apply
# resolves a patch's paths against the repository root, which means applying from this
# stage's crate directory happens to work today but leaves the correctness of the whole
# stage resting on git's path-resolution fallback rather than on anything stated here.
# Applying from /src is the form that follows from the patch's own paths.
#
# Note that --directory is NOT used: the paths are already root-relative, so it would
# prefix them a second time and fail with a path that does not exist.
#
# The stage therefore leaves WORKDIR for the cargo step and returns to /src to apply.
# The grep afterwards is not decoration. git apply already fails loudly on a pin whose
# hunks no longer match, but a patch skipped for path reasons would not; the grep is
# what makes that a build failure instead of an unpatched build reporting success.
COPY pack/llm-router/patches/sglang-gateway/ /tmp/patches/
RUN <<EOF
    set -exo pipefail

    cd /src
    git apply /tmp/patches/*.patch

    # Fail the build if the observer patch did not actually land: observer.rs is a file
    # patch 0002 itself creates, so an unpatched tree has neither the file nor the type.
    grep -q "struct ObserverRouter" sgl-model-gateway/src/observer.rs

    rm -rf /tmp/patches
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

######################## SGLang router ########################

FROM rust-builder AS sgl-router-builder

SHELL ["/bin/bash", "-c"]

ARG SGL_ROUTER_VERSION
ARG SGL_ROUTER_COMMIT

WORKDIR /src

# sgl-router lives inside sgl-project/sglang and declares path dependencies on sibling crates under
# ../rust/ (sglang-processor, sglang-api-types, sglang-runtime-v1-types), so sparse-checkout pulls
# both sgl-router and rust without downloading the Python engine tree.
RUN <<EOF
    set -exo pipefail

    if [ -n "${SGL_ROUTER_COMMIT}" ]; then
        git init .
        git sparse-checkout init --cone
        git sparse-checkout set sgl-router rust
        git remote add origin https://github.com/sgl-project/sglang.git
        git fetch --depth 1 --filter=blob:none origin "${SGL_ROUTER_COMMIT}"
        git checkout FETCH_HEAD
        git submodule update --init --recursive --depth 1
    else
        git clone --depth 1 --branch "${SGL_ROUTER_VERSION}" \
            --filter=blob:none --sparse \
            https://github.com/sgl-project/sglang.git .
        git sparse-checkout set sgl-router rust
        git submodule update --init --recursive --depth 1
    fi
EOF

# Patches use repository-root-relative paths (sgl-router/src/...) and apply from /src.
COPY pack/llm-router/patches/sgl-router/ /tmp/patches/
RUN <<EOF
    set -exo pipefail

    cd /src
    git apply /tmp/patches/*.patch

    # Fail the build if the observer patch did not land.
    grep -q "struct ObserverRouter" sgl-router/src/observer.rs
    grep -q "pub mod observer;" sgl-router/src/lib.rs
    grep -q "/observer/endpoints" sgl-router/src/server/app.rs

    rm -rf /tmp/patches
EOF

WORKDIR /src/sgl-router

# sgl-router uses pure-Rust zeromq (no libzmq-dev needed) and ships Cargo.lock so --locked keeps
# dependency resolution deterministic. PCRE2_SYS_STATIC=1 statically links vendored pcre2 so the
# minimal runtime stage needs no system libpcre2-8 package.
RUN <<EOF
    set -exo pipefail

    export PCRE2_SYS_STATIC=1
    cargo build --release --locked --bin sgl-router
    mkdir -p /out
    cp target/release/sgl-router /out/sgl-router
EOF

######################## runtime ########################

FROM ubuntu:${UBUNTU_VERSION}

SHELL ["/bin/bash", "-c"]

# libssl3t64 is the runtime half of the openssl crate the gateway links; ca-certificates is what
# lets any of the routers reach an HTTPS endpoint at all.
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
COPY --from=sgl-router-builder /out/sgl-router /usr/local/bin/sgl-router

# nonroot is created with --no-create-home; point HOME and HF_HOME at writable /tmp so
# tokenizers downloaded at startup (such as sgl-router --policy=cache_aware) can write cache files.
ENV HOME=/tmp \
    HF_HOME=/tmp/huggingface

USER 65532:65532

# llm-d endpoint picker: ext_proc gRPC, gRPC health, Prometheus, KV-events ZMQ SUB.
EXPOSE 9002 9003 9090 5557
# vLLM router, SGLang model gateway, and SGLang router: HTTP serving and Prometheus.
EXPOSE 30000 29000

# NO ENTRYPOINT ON PURPOSE. One image holds four programs, so an entrypoint here would name one of
# them as the default and make the others reachable only by overriding it. The workload names
# the binary it wants, which is the same place its arguments already come from.
