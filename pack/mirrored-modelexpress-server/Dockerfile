# syntax=docker/dockerfile:1

# The mirrored ModelExpress metadata server: the rendezvous one ModelDeployment's engines
# read when a role loads weights through the modelexpress load format. Built from source at
# a pinned upstream commit -- the operator renders the client half against this server's
# exact contract, so a moving upstream would move that contract underneath it.
# Build target: modelexpress-server (default final stage); the builder stage is its input.
# Platforms: linux/amd64 and linux/arm64.
# Run from the repository root:
#   PACKAGE_ARCH=amd64 PACKAGE_TAG=dev make package mirrored-modelexpress-server
# Set PACKAGE_NAMESPACE to select the image namespace; PACKAGE_PUSH=true publishes.
# MODELEXPRESS_COMMIT re-pins the upstream commit; the default is the reviewed one.

# =============================================================================
# Stage: builder
# Compile the pinned ModelExpress server with its memory backend.
# =============================================================================
FROM rust:1.95-bookworm AS builder
SHELL ["/bin/bash", "-c"]

ARG MODELEXPRESS_COMMIT=9410f3efbaa7aae57917d670de731a79f36684f3

RUN <<EOF
    set -exo pipefail
    # Build deps: cmake drives a dependency's build, protobuf-compiler compiles the
    # metadata protocol, libssl-dev is what the TLS session layer links against.
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        cmake \
        curl \
        g++ \
        git \
        libprotobuf-dev \
        libssl-dev \
        pkg-config \
        protobuf-compiler
    rm -rf /var/lib/apt/lists/*
EOF

WORKDIR /src

# The clone is its own layer so a re-build with the same commit skips it, and the checkout
# is by commit rather than by tag so a force-push upstream cannot change what this image
# serves.
RUN <<EOF
    set -exo pipefail
    git clone https://github.com/ai-dynamo/modelexpress.git .
    git checkout "${MODELEXPRESS_COMMIT}"
EOF

# --locked builds exactly the Cargo.lock the reviewed commit ships: the server is the
# reviewed entry only when its dependency graph is, too.
RUN cargo build --release --locked --bin modelexpress-server --features memory-backend

# =============================================================================
# Stage: modelexpress-server (final)
# Debian slim with only the TLS runtime; the server binary is copied from the
# builder stage.
# =============================================================================
FROM debian:bookworm-slim AS modelexpress-server
SHELL ["/bin/bash", "-c"]

RUN <<EOF
    set -exo pipefail
    # ca-certificates lets the server present and validate the TLS its clients expect;
    # libssl3 is the shared library the build linked against.
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        libssl3
    rm -rf /var/lib/apt/lists/*
EOF

COPY --from=builder /src/target/release/modelexpress-server /usr/local/bin/modelexpress-server

# The in-memory backend is the whole state story: one deployment's rendezvous, not a
# store -- what a replacement loses, the replicas republish as they load. The port is
# fixed because every client env var and probe names it.
ENV MX_METADATA_BACKEND=memory
ENV MODEL_EXPRESS_SERVER_PORT=8001

EXPOSE 8001

ENTRYPOINT ["modelexpress-server"]
