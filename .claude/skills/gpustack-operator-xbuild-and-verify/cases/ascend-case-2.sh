#!/usr/bin/env bash
#
# ASCEND-CASE 2 — Inject + enpu-monitor   (needs a real Ascend NPU host)
#
#   ascend-case-2.sh [TARGET]
#
# Reproduces the GPUStack soft-slicing injection by hand and confirms vcann-rt
# initializes and reports the configured quota inside a container:
#   1. read the card VDie-ID (npu-smi info -t board) -> shm-id (spaces -> '-')
#   2. render npu_info.config (0644) — the same 6 fields the Ascend allocator emits
#      (renderNPUInfoConfig in pkg/devicemanager/allocator/ascend/deviceplugin.go)
#   3. write ld.so.preload listing the two host-injected libdcmi paths BEFORE
#      libvruntime.so (libdcmi must be loaded or the weak dcmi_* symbols segfault —
#      see references/ascend-ld-preload-and-libdcmi.md)
#   4. docker run (ascend runtime, ASCEND_VISIBLE_DEVICES) the staged enpu-monitor
#   5. assert: all 6 config fields loaded, "Successfully to initialize", and the
#      enpu-monitor Aicore/Memory quota lines match the config
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE; must be CANN-based), XB_STAGE
#      (/opt/enpu/vcann-rt), XB_NPU (0), XB_CHIP (0), XB_VNPU (0), XB_AICORE (20),
#      XB_MEM (1024 MB). Prints a STATUS|CHECK|DETAIL table; non-zero on FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vcann-build:${TARGET#xbuild-ascend-cann-}"
[ -n "${IMG}" ] || { echo "case-2: pass a TARGET (e.g. xbuild-ascend-cann-8-910b) or set XB_WORKLOAD_IMAGE"; exit 2; }

echo "# ASCEND-CASE 2 — inject + enpu-monitor (image ${IMG}, npu ${XB_NPU:-0}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/enpu/vcann-rt}" \
  NPU="${XB_NPU:-0}" CHIP="${XB_CHIP:-0}" VNPU="${XB_VNPU:-0}" \
  AICORE="${XB_AICORE:-20}" MEM="${XB_MEM:-1024}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

vdie="$(npu-smi info -t board -i "${NPU}" -c "${CHIP}" 2>/dev/null | awk -F: '/VDie ID/{print $2}' | xargs)"
if [ -n "${vdie}" ]; then shmid="$(echo "${vdie}" | tr ' ' '-')"; row PASS "VDie-ID -> shm-id" "${shmid}";
else shmid="UNKNOWN-VDIE"; row WARN "VDie-ID" "not found via npu-smi; using ${shmid}"; fi

mkdir -p "${STAGE}/test"
cfg="${STAGE}/test/npu_info.config"
printf 'physical-npu-id=%s\nvirtual-npu-id=%s\naicore-quota=%s\nmemory-quota=%s\nshm-id=%s\nscheduling-policy=2\n' \
  "${NPU}" "${VNPU}" "${AICORE}" "${MEM}" "${shmid}" > "${cfg}"
chmod 0644 "${cfg}"
[ "$(stat -c '%a' "${cfg}")" = 644 ] && row PASS "npu_info.config mode 0644" ok || { row FAIL "npu_info.config mode 0644" "$(stat -c '%a' "${cfg}")"; fails=$((fails+1)); }

pre="${STAGE}/test/ld.so.preload"
printf '/usr/local/dcmi/libdcmi.so\n/usr/local/Ascend/driver/lib64/driver/libdcmi.so\n/opt/enpu/vcann-rt/lib/libvruntime.so\n' > "${pre}"
chmod 0644 "${pre}"

log="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" -e ENPU_LOG_LEVEL=3 \
  -v "${STAGE}/lib/libvruntime.so:/opt/enpu/vcann-rt/lib/libvruntime.so:ro" \
  -v "${STAGE}/tools/enpu-monitor:/opt/enpu/vcann-rt/tools/enpu-monitor:ro" \
  -v "${cfg}:/etc/enpu/vcann-rt/npu_info.config:ro" \
  -v "${pre}:/etc/ld.so.preload:ro" \
  -v /dev/shm:/dev/shm \
  "${IMG}" /opt/enpu/vcann-rt/tools/enpu-monitor 2>&1)"

n="$(echo "${log}" | grep -c 'Success to load config')"
[ "${n}" -ge 6 ] && row PASS "config fields loaded" "${n}/6" || { row FAIL "config fields loaded" "${n}/6"; fails=$((fails+1)); }
echo "${log}" | grep -q 'Successfully to initialize' && row PASS "vcann-rt initialize" ok || { row FAIL "vcann-rt initialize" "no init log"; fails=$((fails+1)); }
echo "${log}" | grep -q 'Successfully to initialize all module' && row PASS "all-module init" ok || row WARN "all-module init" "only vnpu-device init seen (enpu-monitor standalone)"
ac="$(echo "${log}" | awk -F: '/Aicore Limit Quota/{gsub(/ /,"",$2);print $2}')"
[ "${ac}" = "${AICORE}" ] && row PASS "Aicore Limit Quota" "${ac}" || { row FAIL "Aicore Limit Quota == ${AICORE}" "${ac:-none}"; fails=$((fails+1)); }
mq="$(echo "${log}" | awk -F: '/Memory Limit quota/{gsub(/ /,"",$2);print $2}')"
[ "${mq}" = "${MEM}" ] && row PASS "Memory Limit quota(MB)" "${mq}" || { row FAIL "Memory Limit quota == ${MEM}" "${mq:-none}"; fails=$((fails+1)); }

echo "--- enpu-monitor output ---"
echo "${log}" | grep -iE 'Quota|Usage|initialize all module' || true
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "ASCEND-CASE 2: PASS"; exit 0; } || { echo "ASCEND-CASE 2: FAIL"; exit 1; }
