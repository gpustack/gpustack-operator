#!/usr/bin/env bash
#
# NVIDIA-CASE 3 — Multi-card per-device limits   (needs >= 2 NVIDIA GPUs)
#
#   nvidia-case-3.sh [TARGET]
#
# Proves the per-card VRAM limit is independent across cards: exposes N cards
# (NVIDIA_VISIBLE_DEVICES=XB_GPUS) and gives card at slot j a DISTINCT limit
# (MEM*(j+1) MiB) via CUDA_DEVICE_MEMORY_LIMIT_<j>, then asserts the container's
# nvidia-smi reports each card's memory.total at exactly its own limit. This is the
# multi-card analogue the allocator emits (one CUDA_DEVICE_MEMORY_LIMIT_<i> per
# allocated card — pkg/devicemanager/allocator/nvidia/deviceplugin.go).
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE), XB_STAGE (/opt/vgpu),
#      XB_GPUS (0,1), XB_MEM (4096 MiB base; slot j gets MEM*(j+1)), XB_SM (30 %).
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vgpu-build:${TARGET#xbuild-nvidia-cuda-}"
[ -n "${IMG}" ] || { echo "nvidia-case-3: pass a TARGET (e.g. xbuild-nvidia-cuda-13) or set XB_WORKLOAD_IMAGE"; exit 2; }

echo "# NVIDIA-CASE 3 — multi-card per-device limits (image ${IMG}, gpus ${XB_GPUS:-0,1}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/vgpu}" \
  GPUS="${XB_GPUS:-0,1}" MEM="${XB_MEM:-4096}" SM="${XB_SM:-30}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
T="${STAGE}/test"; rm -rf "${T}"; mkdir -p "${T}/vgpulock" "${T}/vgpu"
printf '/usr/local/vgpu/libvgpu.so\n' > "${T}/ld.so.preload"; chmod 0644 "${T}/ld.so.preload"

# physical card count gate
phys="$(nvidia-smi -L 2>/dev/null | grep -c '^GPU ' || echo 0)"
IFS=',' read -ra G <<< "${GPUS}"
n="${#G[@]}"
if [ "${phys}" -lt "${n}" ]; then
  row WARN "GPU count" "need ${n}, host has ${phys}; skipping"
  echo "FAILS=0"; echo "SKIP_FEWER_GPUS"; exit 0
fi

# per-slot DISTINCT limit envs: slot j -> MEM*(j+1)
envlim=""
for j in "${!G[@]}"; do
  lim=$(( MEM * (j + 1) ))
  envlim+=" -e CUDA_DEVICE_MEMORY_LIMIT_${j}=${lim}m"
done

INJ="-e NVIDIA_VISIBLE_DEVICES=${GPUS} -e CUDA_DEVICE_SM_LIMIT=${SM} \
 -e CUDA_DEVICE_MEMORY_SHARED_CACHE=/tmp/vgpu/cudevshr.cache ${envlim} \
 -v ${STAGE}/libvgpu.so:/usr/local/vgpu/libvgpu.so:ro \
 -v ${T}/ld.so.preload:/etc/ld.so.preload:ro \
 -v ${T}/vgpulock:/tmp/vgpulock -v ${T}/vgpu:/tmp/vgpu -v /dev/shm:/dev/shm"

smi="$(docker run --rm --entrypoint nvidia-smi ${INJ} "${IMG}" \
  --query-gpu=index,memory.total --format=csv,noheader 2>/dev/null | grep -aE '^[0-9]+,')"

for j in "${!G[@]}"; do
  want=$(( MEM * (j + 1) ))
  got="$(echo "${smi}" | awk -F'[ ,]+' -v i="${j}" '$1==i{print $2}')"
  [ "${got}" = "${want}" ] && row PASS "GPU slot ${j} memory.total(MiB)" "${got}" || { row FAIL "GPU slot ${j} total == ${want}" "${got:-none}"; fails=$((fails+1)); }
done

echo "--- container nvidia-smi ---"; echo "${smi}"
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'SKIP_FEWER_GPUS' && { echo "NVIDIA-CASE 3: SKIP (insufficient GPUs)"; exit 0; }
echo "${out}" | grep -q 'FAILS=0' && { echo "NVIDIA-CASE 3: PASS"; exit 0; } || { echo "NVIDIA-CASE 3: FAIL"; exit 1; }
