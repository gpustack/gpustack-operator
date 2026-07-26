#!/usr/bin/env bash
#
# build-libvgpu.sh <src-dir> <out-dir>
#
# Build the HAMi-core logical-slicing library (libvgpu.so) inside an NVIDIA CUDA devel
# image. <src-dir> is the HAMi-core source tree (the Dockerfile stage clones it); the
# product is installed at <out-dir>/libvgpu.so.
#
# HAMi-core only includes the CUDA headers (CUDA_HOME) and hooks the CUDA driver /
# NVML at runtime; it does not link the CUDA libraries, so any CUDA devel base with
# the headers can build it.
#
# This script is a build-time asset only; it is never shipped into the final image.
#
set -exo pipefail

src="${1:?usage: build-libvgpu.sh <src-dir> <out-dir>}"
out="${2:?usage: build-libvgpu.sh <src-dir> <out-dir>}"

export CUDA_HOME="${CUDA_HOME:-/usr/local/cuda}"

# Copy the source into a writable work dir.
work="/tmp/hami-core"
rm -rf "${work}"
mkdir -p "${work}"
cp -r "${src}/." "${work}/"
cd "${work}"

bash build.sh

mkdir -p "${out}"
install -m 0644 build/libvgpu.so "${out}/libvgpu.so"
ls -la "${out}/libvgpu.so"
