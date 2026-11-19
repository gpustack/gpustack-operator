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

# Copy the source into a writable work dir and leave /tmp/hami-core + build intact
# so nvidia-case-4.sh Part B can locate test_postinit_owner_death.
work="/tmp/hami-core"
rm -rf "${work}"
mkdir -p "${work}"
cp -r "${src}/." "${work}/"
mkdir -p "${work}/build"
cd "${work}/build"

J="$(nproc 2>/dev/null || getconf _NPROCESSORS_ONLN 2>/dev/null || echo 4)"

# Configure and build only the libvgpu.so library target (plus test_postinit_owner_death
# when present) instead of running upstream build.sh's all-targets make, which compiles
# CUDA-13-only test binaries that fail on CUDA 12 devel builders.
cmake .. \
  -DDLSYM_HOOK_ENABLE=1 \
  -DMULTIPROCESS_LIMIT_ENABLE=1 \
  -DHOOK_MEMINFO_ENABLE=1 \
  -DHOOK_NVML_ENABLE=1 \
  -DCMAKE_BUILD_TYPE=Debug \
  -DTEST_DEVICE_ID=0

cmake --build . --target vgpu -j "${J}"
if [[ -f "${work}/test/test_postinit_owner_death.c" ]]; then
  cmake --build . --target test_postinit_owner_death -j "${J}" || true
fi

mkdir -p "${out}"
install -m 0644 "${work}/build/libvgpu.so" "${out}/libvgpu.so"
ls -la "${out}/libvgpu.so"
