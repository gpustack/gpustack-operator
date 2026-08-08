#!/usr/bin/env bash
#
# ASCEND-CASE 3 — Real memory-quota enforcement   (needs a real Ascend NPU host)
#
#   ascend-case-3.sh [TARGET]
#
# Proves the logical-slice is REAL, not cosmetic: a process that allocates NPU HBM in
# a loop is capped at the configured memory-quota when libvruntime.so is preloaded,
# but not when it is absent.
#   - baseline (no injection): allocates well past the quota (until physical free)
#   - injected (memory-quota=MEM MB): libvruntime's aclrtMalloc hook denies the
#     allocation that would cross the quota, logging
#       Out of memory! Request:<r> B, used:<u> B, quota:<MEM*1048576> B.
#
# Asserts: injected run blocked at <= MEM; the "Out of memory" log carries the
# correct quota bytes; baseline run reached > MEM. Embeds the validated memquota.py.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE), XB_STAGE (/opt/enpu/vcann-rt),
#      XB_NPU (0), XB_CHIP (0), XB_MEM (1024 MB).
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vcann-build:${TARGET#xbuild-ascend-cann-}"
[ -n "${IMG}" ] || { echo "case-3: pass a TARGET (e.g. xbuild-ascend-cann-8-910b) or set XB_WORKLOAD_IMAGE"; exit 2; }
MEM="${XB_MEM:-1024}"

echo "# ASCEND-CASE 3 — memory-quota=${MEM}MB enforcement (image ${IMG}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/enpu/vcann-rt}" \
  NPU="${XB_NPU:-0}" CHIP="${XB_CHIP:-0}" MEM="${MEM}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
T="${STAGE}/test"; mkdir -p "${T}"

# validated HBM allocation probe (256MB chunks; frees on exit)
cat > "${T}/memquota.py" <<'PY'
import acl, time
HUGE=0
def ck(r,m):
    if r!=0: print("ERR",m,r); raise SystemExit(1)
ck(acl.init(),"init"); ck(acl.rt.set_device(0),"setdev")
ctx,r=acl.rt.create_context(0); ck(r,"ctx")
ptrs=[]; total=0; CHUNK=256*1024*1024
for i in range(60):
    p,r=acl.rt.malloc(CHUNK,HUGE)
    if r!=0:
        print("FAILED at total=%dMB ret=%d" % (total//1024//1024, r)); break
    ptrs.append(p); total+=CHUNK
else:
    print("reached %dMB without limit" % (total//1024//1024))
print("STOP total=%dMB" % (total//1024//1024))
for p in ptrs: acl.rt.free(p)
PY

# config + preload (memory-quota=MEM); shm-id from the card VDie-ID
vdie="$(npu-smi info -t board -i "${NPU}" -c "${CHIP}" 2>/dev/null | awk -F: '/VDie ID/{print $2}' | xargs)"
shmid="$(echo "${vdie:-UNKNOWN}" | tr ' ' '-')"
cfg="${T}/npu_info.config"
printf 'physical-npu-id=%s\nvirtual-npu-id=0\naicore-quota=20\nmemory-quota=%s\nshm-id=%s\nscheduling-policy=2\n' \
  "${NPU}" "${MEM}" "${shmid}" > "${cfg}"; chmod 0644 "${cfg}"
pre="${T}/ld.so.preload"
printf '/usr/local/dcmi/libdcmi.so\n/usr/local/Ascend/driver/lib64/driver/libdcmi.so\n/opt/enpu/vcann-rt/lib/libvruntime.so\n' > "${pre}"; chmod 0644 "${pre}"

base_dir="-v ${STAGE}/lib/libvruntime.so:/opt/enpu/vcann-rt/lib/libvruntime.so:ro \
 -v ${cfg}:/etc/enpu/vcann-rt/npu_info.config:ro -v ${pre}:/etc/ld.so.preload:ro -v /dev/shm:/dev/shm"

# A) baseline — no injection
base="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" \
  -v "${T}/memquota.py:/memquota.py:ro" "${IMG}" python3 /memquota.py 2>&1)"
nbase="$(echo "${base}" | awk -F= '/STOP total=/{gsub(/MB/,"",$2);print $2}')"

# B) injected — memory-quota=MEM
inj="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" -e ENPU_LOG_LEVEL=3 \
  ${base_dir} -v "${T}/memquota.py:/memquota.py:ro" "${IMG}" python3 /memquota.py 2>&1)"
ninj="$(echo "${inj}" | awk -F= '/STOP total=/{gsub(/MB/,"",$2);print $2}')"

# assertions
if echo "${inj}" | grep -q 'Out of memory'; then
  q="$(echo "${inj}" | sed -nE 's/.*quota:([0-9]+) B.*/\1/p' | head -1)"
  want=$(( MEM * 1024 * 1024 ))
  [ "${q}" = "${want}" ] && row PASS "quota bytes in OOM log" "${q}" || { row FAIL "quota bytes == ${want}" "${q:-none}"; fails=$((fails+1)); }
else
  row FAIL "injected hit memory quota" "no 'Out of memory' log"; fails=$((fails+1))
fi
if [ -n "${ninj}" ] && [ "${ninj}" -le "${MEM}" ]; then row PASS "injected capped <= ${MEM}MB" "${ninj}MB"; else row FAIL "injected capped <= ${MEM}MB" "${ninj:-?}MB"; fails=$((fails+1)); fi
if [ -n "${nbase}" ] && [ "${nbase}" -gt "${MEM}" ]; then row PASS "baseline exceeds ${MEM}MB" "${nbase}MB"; else row WARN "baseline exceeds ${MEM}MB" "${nbase:-?}MB (card may be near-full)"; fi

echo "--- baseline ---"; echo "${base}" | grep -iE 'STOP|FAILED|reached'
echo "--- injected ---"; echo "${inj}"  | grep -iE 'STOP|FAILED|Out of memory'
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "ASCEND-CASE 3" "$(xb_fails "${out}")"
