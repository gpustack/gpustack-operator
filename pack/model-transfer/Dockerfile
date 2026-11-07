# Vendor source-worker runtimes for remote model weight delivery.
# Build targets:
#   cuda: NIXL CUDA 12.9 wheels and Torch; linux/amd64 and linux/arm64.
#   rocm: source-built NIXL and UCX with ROCm 7.2; linux/amd64.
#   cann: CANN 9.2 with a patched HIXL host library; linux/amd64 and linux/arm64.
# Select a target explicitly. The packaging script defaults to cuda; a direct
# Docker build without --target reaches the final cann stage.
#
# Run from the repository root:
#   PACKAGE_ARCH=amd64 PACKAGE_TAG=dev MODEL_TRANSFER_BUILD_FLAVOR=cuda \
#     make package model-transfer
#   PACKAGE_ARCH=amd64 PACKAGE_TAG=dev MODEL_TRANSFER_BUILD_FLAVOR=rocm \
#     make package model-transfer
#   PACKAGE_ARCH=arm64 PACKAGE_TAG=dev MODEL_TRANSFER_BUILD_FLAVOR=cann \
#     make package model-transfer
# Output tags: gpustack/model-transfer:dev-cuda12.9, :dev-rocm7.2, :dev-cann9.2-a5.
# PACKAGE_NAMESPACE selects the image namespace; PACKAGE_PUSH=true publishes.
# CUDA_VERSION, ROCM_VERSION and CANN_VERSION change tag suffixes only.
#
# CANN defaults to Atlas A5 / 950. For Atlas A2 / 910B:
#   PACKAGE_ARCH=arm64 PACKAGE_TAG=dev MODEL_TRANSFER_BUILD_FLAVOR=cann \
#     MODEL_TRANSFER_VARIANT=910b \
#     CANN_RUNTIME_IMAGE=quay.io/ascend/cann:9.2.0-910b-ubuntu22.04-py3.12 \
#     make package model-transfer
# For Atlas A3, replace 910b with a3 in the variant and the base-image tag.
#
# All ARG defaults are below. hack/package.sh forwards UBUNTU_IMAGE and the
# selected target's image and NIXL arguments. NIXL_* selects CUDA packages;
# NIXL_ROCM_* selects ROCm packages and source commits. Update the ROCm version
# and source commit together. The Torch indexes and checks target CUDA 12.9 and
# ROCm 7.2. GPUSTACK_GIT_VERSION is supplied by the packaging script.
# CANN forwards HIXL_COMMIT, HIXL_CMAKE_COMMIT and CMAKE_MAX_JOBS. The pinned
# HIXL host build includes the channel-generation fix used by GPUStack Runner;
# CANN supplies its Python binding and device libraries.
#
# ROCm builds only the UCX plugin. Its source-built dispatcher depends on
# nixl-rocm, with no CUDA backend packages or inference-engine image dependency.
# The production source supervisor currently admits nvidia/nixl and ascend/hixl,
# but not amd/nixl; ROCm packaging does not establish AMD source-worker support.
# If overriding LD_LIBRARY_PATH for ROCm, retain /opt/nixl/lib, /opt/ucx/lib
# and /opt/rocm/lib so the packaged native libraries remain loadable.
# Package checks do not establish GPU, NPU or RDMA runtime acceptance.
# See model-transfer/python/README.md for the packaged SDK and its runtime requirements.

ARG UBUNTU_IMAGE="ubuntu:24.04"
ARG GPUSTACK_GIT_VERSION="0.0.0"

# CUDA 12.9 packages.
ARG CUDA_RUNTIME_IMAGE="nvidia/cuda:12.9.1-runtime-ubuntu24.04"
ARG NIXL_VERSION="1.5.2"
ARG NIXL_TORCH_VERSION="2.13.0+cu129"

# ROCm 7.2 packages, built for CPython 3.12.
ARG ROCM_RUNTIME_IMAGE="rocm/dev-ubuntu-24.04:7.2.3"
ARG NIXL_ROCM_VERSION="1.5.0"
ARG NIXL_ROCM_REF="1683cf3b7f3d11674c03c5e861cea22339876c96"
ARG NIXL_ROCM_UCX_REF="2ff0b47ce7626584cb9b56c647d08f712c6c3edf"
ARG NIXL_ROCM_TORCH_VERSION="2.13.0+rocm7.2"

# CANN toolkit and runtime.
ARG CANN_RUNTIME_IMAGE="quay.io/ascend/cann:9.2.0-950-ubuntu22.04-py3.12"

# =============================================================================
# Stage: wheel-builder
# Build the universal gpustack_model_transfer wheel. Pure Python (py3-none-any),
# with vendor imports (torch, torch_npu, nixl, hixl) kept lazy inside the
# runtime backends and engine adapters.
# =============================================================================
FROM ${UBUNTU_IMAGE} AS wheel-builder
SHELL ["/bin/bash", "-c"]

ARG GPUSTACK_GIT_VERSION
ENV DEBIAN_FRONTEND=noninteractive

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates python3 python3-pip \
        python3-hatchling python3-hatch-vcs
    rm -rf /var/lib/apt/lists/*
EOF

COPY model-transfer/python /build/model-transfer/python

RUN <<EOF
    set -exo pipefail
    cd /build/model-transfer/python
    version="$(python3 -c '
import sys
from packaging.version import InvalidVersion, Version
try:
    print(Version(sys.argv[1]))
except InvalidVersion:
    print("0.0.0")
' "${GPUSTACK_GIT_VERSION}")"
    export SETUPTOOLS_SCM_PRETEND_VERSION="${version}"
    export SETUPTOOLS_SCM_PRETEND_VERSION_FOR_GPUSTACK_MODEL_TRANSFER="${version}"
    export PIP_BREAK_SYSTEM_PACKAGES=1
    python3 -m pip wheel --no-deps --no-build-isolation --wheel-dir /out/wheels .
    test -n "$(ls /out/wheels/gpustack_model_transfer-*-py3-none-any.whl 2>/dev/null)"
    ls -la /out/wheels/
EOF

# =============================================================================
# Stage: cuda
# NVIDIA CUDA 12.9 runtime image with NIXL and gpustack_model_transfer.
# =============================================================================
FROM ${CUDA_RUNTIME_IMAGE} AS cuda
SHELL ["/bin/bash", "-c"]

ARG NIXL_VERSION
ARG NIXL_TORCH_VERSION
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PYTHONUNBUFFERED=1 \
    NVIDIA_VISIBLE_DEVICES="all" \
    NVIDIA_DRIVER_CAPABILITIES="compute,utility" \
    GPUSTACK_MODEL_TRANSFER_BACKEND="nixl"

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates python3 python3-pip \
        libibverbs1 librdmacm1 ibverbs-providers
    rm -rf /var/lib/apt/lists/*
EOF

COPY --from=wheel-builder /out/wheels /tmp/wheels

RUN --mount=type=bind,src=pack/model-transfer/rootfs,target=/rootfs <<EOF
    set -exo pipefail
    python3 -m pip install --no-cache-dir \
        --index-url https://download.pytorch.org/whl/cu129 \
        "torch==${NIXL_TORCH_VERSION}"
    python3 -m pip install --no-cache-dir \
        "nixl==${NIXL_VERSION}" \
        "nixl-cu12==${NIXL_VERSION}" \
        /tmp/wheels/gpustack_model_transfer-*-py3-none-any.whl
    rm -rf /tmp/wheels
    install -m 0755 /rootfs/usr/bin/entrypoint.sh /usr/bin/entrypoint.sh
    python3 -m pip check
    python3 - "${NIXL_VERSION}" "${NIXL_TORCH_VERSION}" <<'PY'
import importlib.metadata
import sys

import nixl
import torch

from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI

for package in ("nixl", "nixl-cu12", "nixl-cu13"):
    assert importlib.metadata.version(package) == sys.argv[1], package
assert torch.__version__ == sys.argv[2], torch.__version__
assert torch.version.cuda == "12.9", torch.version.cuda
assert nixl._api.__name__ == "nixl_cu12._api", nixl._api.__name__
RealNixlAPI()._require()
print("NIXL CUDA 12 package and API surface checks passed")
PY
EOF

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
CMD ["python3", "-m", "gpustack_model_transfer.source"]

# Build UCX and the ROCm backend from source without an inference-engine image.
FROM ${ROCM_RUNTIME_IMAGE} AS nixl-rocm-builder
SHELL ["/bin/bash", "-c"]

ARG NIXL_ROCM_REF
ARG NIXL_ROCM_UCX_REF
ARG NIXL_ROCM_VERSION
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_BREAK_SYSTEM_PACKAGES=1

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        autoconf automake build-essential ca-certificates cmake git libtool \
        pkg-config python3 python3-dev python3-pip \
        libaio-dev liburing-dev libibverbs-dev librdmacm-dev
    rm -rf /var/lib/apt/lists/*
    python3 -m pip install --no-cache-dir \
        meson meson-python ninja pybind11 patchelf pyyaml types-PyYAML \
        build tomlkit uv 'setuptools>=80.9.0'
EOF

RUN <<EOF
    set -exo pipefail
    git init /tmp/ucx
    git -C /tmp/ucx remote add origin https://github.com/openucx/ucx.git
    git -C /tmp/ucx fetch --depth 1 origin "${NIXL_ROCM_UCX_REF}"
    git -C /tmp/ucx checkout --detach FETCH_HEAD
    test "$(git -C /tmp/ucx rev-parse HEAD)" = "${NIXL_ROCM_UCX_REF}"
    cd /tmp/ucx
    ./autogen.sh
    ./configure --prefix=/opt/ucx --enable-shared --disable-static \
        --disable-doxygen-doc --enable-optimizations --enable-devel-headers \
        --without-avx --with-rocm=/opt/rocm --with-verbs --with-dm --enable-mt
    make -j "$(nproc)"
    make install
    install -Dm644 LICENSE /opt/ucx/share/licenses/LICENSE
    rm -rf /tmp/ucx
EOF

RUN <<EOF
    set -exo pipefail
    git init /tmp/nixl
    git -C /tmp/nixl remote add origin https://github.com/ai-dynamo/nixl.git
    git -C /tmp/nixl fetch --depth 1 origin "${NIXL_ROCM_REF}"
    git -C /tmp/nixl checkout --detach FETCH_HEAD
    test "$(git -C /tmp/nixl rev-parse HEAD)" = "${NIXL_ROCM_REF}"
    cd /tmp/nixl
    python3 -c 'import sys, tomllib; assert tomllib.load(open("pyproject.toml", "rb"))["project"]["version"] == sys.argv[1]' "${NIXL_ROCM_VERSION}"
    python3 contrib/tomlutil.py --wheel-name nixl-rocm pyproject.toml
    python3 -m pip wheel --no-cache-dir --no-deps --no-build-isolation \
        --wheel-dir /out/wheels \
        -Cbuild-dir=/tmp/nixl-build \
        -Csetup-args=--prefix=/opt/nixl -Csetup-args=--libdir=lib \
        -Csetup-args=-Ducx_path=/opt/ucx -Csetup-args=-Dwheel_variant=rocm \
        -Csetup-args=-Denable_plugins=UCX -Csetup-args=-Drelease_wheel=false \
        -Csetup-args=-Dbuild_tests=false -Csetup-args=-Dbuild_examples=false .
    meson install -C /tmp/nixl-build --no-rebuild
    cp /tmp/nixl-build/src/bindings/python/nixl-meta/nixl-*.whl /out/wheels/
    install -Dm644 LICENSE /opt/nixl/share/licenses/LICENSE
    rm -rf /tmp/nixl /tmp/nixl-build
EOF

# =============================================================================
# Stage: rocm
# AMD ROCm 7.2 runtime image with NIXL and gpustack_model_transfer.
# =============================================================================
FROM ${ROCM_RUNTIME_IMAGE} AS rocm
SHELL ["/bin/bash", "-c"]

ARG NIXL_ROCM_VERSION
ARG NIXL_ROCM_TORCH_VERSION
ENV DEBIAN_FRONTEND=noninteractive \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PYTHONUNBUFFERED=1 \
    AMD_VISIBLE_DEVICES="all" \
    LD_LIBRARY_PATH="/opt/nixl/lib:/opt/ucx/lib:/opt/rocm/lib" \
    NIXL_PLUGIN_DIR="/opt/nixl/lib/plugins" \
    GPUSTACK_MODEL_TRANSFER_BACKEND="nixl"

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates python3 python3-pip \
        libibverbs1 librdmacm1 ibverbs-providers
    rm -rf /var/lib/apt/lists/*
EOF

COPY --from=wheel-builder /out/wheels /tmp/wheels
COPY --from=nixl-rocm-builder /out/wheels /tmp/nixl-wheels
COPY --from=nixl-rocm-builder /opt/nixl/lib /opt/nixl/lib
COPY --from=nixl-rocm-builder /opt/nixl/share/licenses /opt/nixl/share/licenses
COPY --from=nixl-rocm-builder /opt/ucx /opt/ucx

RUN --mount=type=bind,src=pack/model-transfer/rootfs,target=/rootfs <<EOF
    set -exo pipefail
    if [[ "$(uname -m)" != "x86_64" ]]; then
        echo "The ROCm target requires linux/amd64 (found $(uname -m))." >&2
        exit 1
    fi
    python3 -c 'import sys; assert sys.version_info[:2] == (3, 12), sys.version'
    python3 -m pip install --no-cache-dir \
        --index-url https://download.pytorch.org/whl/rocm7.2 \
        "torch==${NIXL_ROCM_TORCH_VERSION}"
    python3 -m pip install --no-cache-dir \
        /tmp/nixl-wheels/*.whl \
        /tmp/wheels/gpustack_model_transfer-*-py3-none-any.whl
    rm -rf /tmp/wheels /tmp/nixl-wheels
    install -m 0755 /rootfs/usr/bin/entrypoint.sh /usr/bin/entrypoint.sh
    python3 -m pip check
    python3 - "${NIXL_ROCM_VERSION}" "${NIXL_ROCM_TORCH_VERSION}" <<'PY'
import importlib.metadata
import importlib.util
import sys

import nixl
import numpy
import torch

from gpustack_model_transfer.backends.nixl.runtime import RealNixlAPI

for package in ("nixl", "nixl-rocm"):
    assert importlib.metadata.version(package) == sys.argv[1], package
assert torch.__version__ == sys.argv[2], torch.__version__
assert torch.version.hip and torch.version.hip.startswith("7.2."), torch.version.hip
assert torch.version.cuda is None, torch.version.cuda
assert nixl._api.__name__ == "nixl_rocm._api", nixl._api.__name__
for module in ("nixl_cu12", "nixl_cu13"):
    assert importlib.util.find_spec(module) is None, module
RealNixlAPI()._require()
print("NIXL ROCm package and API surface checks passed")
PY
EOF

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
CMD ["python3", "-m", "gpustack_model_transfer.source"]

# =============================================================================
# Stage: hixl-host-builder
# Rebuild the host library with the channel-generation fix used by Runner.
# Keep the runtime's Python binding, device kernels and other CANN libraries.
# =============================================================================
FROM ${CANN_RUNTIME_IMAGE} AS hixl-host-builder
SHELL ["/bin/bash", "-c"]

ARG HIXL_COMMIT="60dbe9d1d7d9a7eef04bc3dbf84edbad2188230a"
ARG HIXL_CMAKE_COMMIT="3c4a9da4a0e81faa47dcc1c8e11db63ec7ff6231"
ARG CMAKE_MAX_JOBS="8"
ENV DEBIAN_FRONTEND=noninteractive

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        build-essential ca-certificates cmake curl patch
    rm -rf /var/lib/apt/lists/*
EOF

RUN --mount=type=bind,src=pack/model-transfer/patches/hixl,target=/patches <<EOF
    set -exo pipefail
    source /usr/local/Ascend/cann/set_env.sh
    mkdir -p /tmp/hixl/third_party/cann-cmake /out/hixl
    curl -fL --retry 3 --connect-timeout 30 --max-time 180 \
        "https://codeload.github.com/hicann/hixl/tar.gz/${HIXL_COMMIT}" \
        | tar -xz --strip-components=1 -C /tmp/hixl
    curl -fL --retry 3 --connect-timeout 30 --max-time 180 \
        "https://codeload.github.com/hicann/cmake/tar.gz/${HIXL_CMAKE_COMMIT}" \
        | tar -xz --strip-components=1 -C /tmp/hixl/third_party/cann-cmake
    cd /tmp/hixl
    for patch_file in /patches/*.patch; do
        patch --batch --fuzz=0 -p1 < "${patch_file}"
    done
    cmake -S . -B build -DCMAKE_BUILD_TYPE=Release \
        -DHIXL_BUILD_HOST_ONLY=ON \
        -DCANN_3RD_LIB_PATH=/tmp/hixl/third_party \
        -DASCEND_INSTALL_PATH="${ASCEND_HOME_PATH}"
    cmake --build build --target cann_hixl -j "${CMAKE_MAX_JOBS}"
    install -m 0644 build/src/hixl/libcann_hixl.so /out/hixl/libcann_hixl.so
    install -m 0644 LICENSE /out/hixl/LICENSE
EOF

# =============================================================================
# Stage: cann
# Ascend CANN 9.2 runtime image with HIXL and gpustack_model_transfer.
# Host Ascend driver and DCMI libraries are mounted at runtime under
# /usr/local/Ascend/driver and /usr/local/dcmi so the image's own CANN toolkit
# is never shadowed.
# =============================================================================
FROM ${CANN_RUNTIME_IMAGE} AS cann
SHELL ["/bin/bash", "-c"]

ENV DEBIAN_FRONTEND=noninteractive \
    PIP_BREAK_SYSTEM_PACKAGES=1 \
    PYTHONUNBUFFERED=1 \
    ASCEND_HOME_PATH="/usr/local/Ascend/cann" \
    ASCEND_TOOLKIT_HOME="/usr/local/Ascend/cann" \
    LD_LIBRARY_PATH="/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/driver/lib64/common:/usr/local/dcmi:/usr/local/Ascend/cann/lib64:/usr/local/Ascend/ascend-toolkit/latest/lib64:/usr/local/Ascend/ascend-toolkit/latest/runtime/lib64" \
    GPUSTACK_MODEL_TRANSFER_BACKEND="hixl"

COPY --from=wheel-builder /out/wheels /tmp/wheels
COPY --from=hixl-host-builder /out/hixl /tmp/hixl-host

RUN --mount=type=bind,src=pack/model-transfer/rootfs,target=/rootfs <<EOF
    set -exo pipefail
    test -f /usr/local/Ascend/cann/lib64/libcann_hixl.so
    install -m 0644 /tmp/hixl-host/libcann_hixl.so \
        "$(readlink -f /usr/local/Ascend/cann/lib64/libcann_hixl.so)"
    install -Dm644 /tmp/hixl-host/LICENSE /usr/local/share/licenses/hixl/LICENSE
    rm -rf /tmp/hixl-host
    pip_flags=(--no-cache-dir)
    if python3 -m pip install --help | grep -q -- '--break-system-packages'; then
        pip_flags+=(--break-system-packages)
    fi
    python3 -m pip install "${pip_flags[@]}" \
        /tmp/wheels/gpustack_model_transfer-*-py3-none-any.whl
    rm -rf /tmp/wheels
    install -m 0755 /rootfs/usr/bin/entrypoint.sh /usr/bin/entrypoint.sh
    test -f /usr/local/Ascend/cann/lib64/libcann_hixl.so
    python3 -c 'import importlib.metadata; print("HIXL Python package:", importlib.metadata.version("hixl"))'
    # Use only the vendor driver stub for this import check, then remove it.
    # This checks the Python ABI and adapter API, not NPU initialization or I/O.
    mkdir -p /tmp/hixl-driver-stub
    ln -s /usr/local/Ascend/cann/devlib/libascend_hal.so /tmp/hixl-driver-stub/libascend_hal.so
    LD_LIBRARY_PATH="/tmp/hixl-driver-stub:${LD_LIBRARY_PATH}" \
        python3 -c 'from gpustack_model_transfer.backends.hixl.runtime import RealHixlAPI; RealHixlAPI()._require(); print("HIXL Python ABI and API surface checks passed with vendor driver stub")'
    rm -rf /tmp/hixl-driver-stub
    python3 -c "import importlib.util, sys; sys.exit(0 if importlib.util.find_spec('gpustack_model_transfer') and importlib.util.find_spec('hixl') else 1)"
EOF

ENTRYPOINT ["/usr/bin/entrypoint.sh"]
CMD ["python3", "-m", "gpustack_model_transfer.source"]
