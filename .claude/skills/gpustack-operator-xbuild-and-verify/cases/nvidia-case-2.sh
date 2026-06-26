#!/usr/bin/env bash
#
# NVIDIA-CASE 2 — Single-card inject + nvidia-smi   (needs a real NVIDIA GPU host)
#
#   nvidia-case-2.sh [TARGET]
#
# Reproduces the GPUStack NVIDIA soft-slicing injection by hand on one card and
# confirms HAMi-core caps both VRAM and compute:
#   1. write ld.so.preload (single line: /usr/local/vgpu/libvgpu.so)
#   2. docker run (nvidia runtime, NVIDIA_VISIBLE_DEVICES=GPU) with
#      CUDA_DEVICE_MEMORY_LIMIT_0=<MEM>m + CUDA_DEVICE_SM_LIMIT=<SM>, the staged
#      libvgpu.so + lock/cache mounts
#   3. assert nvidia-smi reports memory.total == MEM (NVML hook)
#   4. compile + run a tiny CUDA probe (cuDevicePrimaryCtxRetain → cuMemGetInfo) with
#      LIBCUDA_LOG_LEVEL=3 and assert: HAMi logs "core utilization limit = SM", and
#      cuMemGetInfo total == MEM (real CUDA-level enforcement, not just nvidia-smi)
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE), XB_STAGE (/opt/vgpu), XB_GPU (0),
#      XB_MEM (4096 MiB), XB_SM (50 %). Prints STATUS|CHECK|DETAIL; non-zero on FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vgpu-build:${TARGET#xbuild-nvidia-cuda-}"
[ -n "${IMG}" ] || { echo "nvidia-case-2: pass a TARGET (e.g. xbuild-nvidia-cuda-13) or set XB_WORKLOAD_IMAGE"; exit 2; }

echo "# NVIDIA-CASE 2 — single-card inject (image ${IMG}, gpu ${XB_GPU:-0}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/vgpu}" \
  GPU="${XB_GPU:-0}" MEM="${XB_MEM:-4096}" SM="${XB_SM:-50}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
T="${STAGE}/test"; rm -rf "${T}"; mkdir -p "${T}/vgpulock" "${T}/vgpu"
printf '/usr/local/vgpu/libvgpu.so\n' > "${T}/ld.so.preload"; chmod 0644 "${T}/ld.so.preload"

cat > "${T}/probe.c" <<'PY'
#include <cuda.h>
#include <stdio.h>
int main(){
  CUcontext c; size_t f=0,t=0; CUdevice d;
  if(cuInit(0)){printf("PROBE cuInit fail\n");return 1;}
  cuDeviceGet(&d,0);
  if(cuDevicePrimaryCtxRetain(&c,d)){printf("PROBE ctx fail\n");return 1;}
  cuCtxSetCurrent(c);
  cuMemGetInfo(&f,&t);
  printf("PROBE cuMemGetInfo total=%zuMiB free=%zuMiB\n", t/1048576, f/1048576);
  return 0;
}
PY

# common injection (env + mounts)
INJ="-e NVIDIA_VISIBLE_DEVICES=${GPU} \
 -e CUDA_DEVICE_MEMORY_LIMIT_0=${MEM}m -e CUDA_DEVICE_SM_LIMIT=${SM} \
 -e CUDA_DEVICE_MEMORY_SHARED_CACHE=/tmp/vgpu/cudevshr.cache \
 -v ${STAGE}/libvgpu.so:/usr/local/vgpu/libvgpu.so:ro \
 -v ${T}/ld.so.preload:/etc/ld.so.preload:ro \
 -v ${T}/vgpulock:/tmp/vgpulock -v ${T}/vgpu:/tmp/vgpu -v /dev/shm:/dev/shm"

# 1) nvidia-smi memory.total
smi="$(docker run --rm --entrypoint nvidia-smi ${INJ} "${IMG}" \
  --query-gpu=memory.total --format=csv,noheader 2>/dev/null | grep -oE '[0-9]+' | head -1)"
[ "${smi}" = "${MEM}" ] && row PASS "nvidia-smi memory.total(MiB)" "${smi}" || { row FAIL "nvidia-smi memory.total == ${MEM}" "${smi:-none}"; fails=$((fails+1)); }

# 2+3) CUDA probe: SM-limit log + cuMemGetInfo enforcement (mount probe.c read-only)
probe="$(docker run --rm --entrypoint bash -e LIBCUDA_LOG_LEVEL=3 ${INJ} \
  -v "${T}/probe.c:/probe.c:ro" "${IMG}" \
  -c 'nvcc -o /tmp/probe /probe.c -L/usr/local/cuda/lib64/stubs -lcuda 2>/dev/null; /tmp/probe' 2>&1)"

smlog="$(echo "${probe}" | sed -nE 's/.*core utilization limit = ([0-9]+).*/\1/p' | head -1)"
[ "${smlog}" = "${SM}" ] && row PASS "HAMi SM limit logged" "${smlog}" || { row FAIL "HAMi SM limit == ${SM}" "${smlog:-none}"; fails=$((fails+1)); }
ptot="$(echo "${probe}" | sed -nE 's/.*cuMemGetInfo total=([0-9]+)MiB.*/\1/p' | head -1)"
[ "${ptot}" = "${MEM}" ] && row PASS "cuMemGetInfo total(MiB)" "${ptot}" || { row FAIL "cuMemGetInfo total == ${MEM}" "${ptot:-none}"; fails=$((fails+1)); }

echo "--- HAMi-core / probe output ---"
echo "${probe}" | grep -iE 'Initializing|utilization limit|PROBE' || true
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "NVIDIA-CASE 2: PASS"; exit 0; } || { echo "NVIDIA-CASE 2: FAIL"; exit 1; }
