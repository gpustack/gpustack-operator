#!/usr/bin/env bash
#
# ASCEND-CASE 4 — npu-smi slice visibility   (needs a real Ascend NPU host)
#
#   ascend-case-4.sh [TARGET]
#
# Answers, on hardware, where a logical slice IS and IS NOT observable:
#
#   1. npu-smi is a DRIVER tool: it links libdrvdsmi_host.so + libascend_hal.so and
#      neither libruntime.so nor libdcmi.so. vcann-rt (and hami-vnpu-core) interpose
#      the CANN runtime layer (rt*), which npu-smi never enters — so the rt* hooks
#      alone leave the slice invisible: a LAYER mismatch, not an impossibility.
#   2. GPUStack closes it at the layer npu-smi does use. The vendored patch in
#      external/ascend/vcann-rt/ defines dsmi_get_hbm_info inside libvruntime.so, which
#      the sliced container already preloads, and ENPU_DSMI_HOOK gates it. The case
#      asserts both halves of the pair — gate off => the physical card, gate on => the
#      quota — plus that enpu-monitor and the plain non-CANN binaries survive it.
#   3. The mechanism control: a throwaway shim that halves dsmi_get_hbm_info and makes
#      npu-smi's HBM Capacity halve with it, independent of the patch. A probe, never
#      product code.
#   4. hami-vnpu-core (if staged at XB_HAMI) has the same blind spot the patch fixes:
#      its hooked rtMemGetInfoEx reports the quota to the application while npu-smi
#      keeps reporting the physical card. Rows WARN-skip when it is not staged.
#
# Where XB_HAMI's libvnpu.so comes from: NOTHING in this repo builds it — the
# Dockerfile's LIB_HAMI_CORE_COMMIT is NVIDIA HAMi-core (libvgpu.so), a different
# project. Build Project-HAMi/hami-vnpu-core yourself for linux/arm64 (`cargo build
# --release --locked`, with SONAME stubs for libdcmi/libruntime satisfying link time)
# and stage it as ${XB_HAMI}/libvnpu-needed.so (stubs that *define* the vendor symbols,
# so ld.so auto-loads them and a global preload works) or ${XB_HAMI}/libvnpu.so.
# Absent ⇒ section 5 WARN-skips and the run reports SKIPPED>0.
#
# Card numbering, deliberately both: npu-smi inside a container keeps the PHYSICAL
# NPU id (XB_NPU) — `-i 0` on a container scoped to card 1 fails with "Invalid card
# id" — while ACL renumbers the visible card to device 0.
#
# Every preload is scoped to the container. The HOST's /etc/ld.so.preload is never
# written; see references/ascend-npu-smi-and-aicore.md.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE; must be CANN-based), XB_STAGE
#      (/opt/enpu/vcann-rt), XB_HAMI (/opt/hami-vnpu-core), XB_NPU (0), XB_CHIP (0),
#      XB_VNPU (0), XB_AICORE (20), XB_MEM (1024 MB).
# Prints a STATUS|CHECK|DETAIL table; exits non-zero on FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:-}"
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-}}"
[ -z "${IMG}" ] && [ -n "${TARGET}" ] && IMG="vcann-build:${TARGET#xbuild-ascend-cann-}"
[ -n "${IMG}" ] || { echo "case-4: pass a TARGET (e.g. xbuild-ascend-cann-8-910b) or set XB_WORKLOAD_IMAGE"; exit 2; }

echo "# ASCEND-CASE 4 — npu-smi slice visibility (image ${IMG}, npu ${XB_NPU:-0}) on $(xtarget_desc)"

out="$(xsh \
  IMG="${IMG}" STAGE="${XB_STAGE:-/opt/enpu/vcann-rt}" HAMI="${XB_HAMI:-/opt/hami-vnpu-core}" \
  NPU="${XB_NPU:-0}" CHIP="${XB_CHIP:-0}" VNPU="${XB_VNPU:-0}" \
  AICORE="${XB_AICORE:-20}" MEM="${XB_MEM:-1024}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
# WARN rows do not fail the case, so count them: a run whose coverage collapsed
# (no hami artifact, no gcc) must not look the same as a fully-exercised one.
skips=0
T="${STAGE}/test"; mkdir -p "${T}"

# The vcann-rt artifacts are bind-mounted below. `-v` with a missing source makes
# dockerd mkdir it as a DIRECTORY on the host, which then poisons build.sh's
# `docker cp` into the same path — so refuse up front rather than create rubbish.
for a in "${STAGE}/lib/libvruntime.so" "${STAGE}/tools/enpu-monitor"; do
  [ -f "${a}" ] || { row FAIL "vcann-rt staged at ${STAGE}" "${a} missing — run scripts/build.sh first"; echo "FAILS=1"; exit 0; }
done

# ---- vcann-rt injection assets (same contract as ASCEND-CASE 2/3) ----
vdie="$(npu-smi info -t board -i "${NPU}" -c "${CHIP}" 2>/dev/null | awk -F: '/VDie ID/{print $2}' | xargs)"
if [ -n "${vdie}" ]; then shmid="$(echo "${vdie}" | tr ' ' '-')"
else shmid="UNKNOWN-VDIE"; row WARN "VDie-ID" "not found via npu-smi; using ${shmid}"; skips=$((skips+1)); fi
cfg="${T}/npu_info.config"
printf 'physical-npu-id=%s\nvirtual-npu-id=%s\naicore-quota=%s\nmemory-quota=%s\nshm-id=%s\nscheduling-policy=2\n' \
  "${NPU}" "${VNPU}" "${AICORE}" "${MEM}" "${shmid}" > "${cfg}"; chmod 0644 "${cfg}"
pre="${T}/ld.so.preload"
printf '/usr/local/dcmi/libdcmi.so\n/usr/local/Ascend/driver/lib64/driver/libdcmi.so\n/opt/enpu/vcann-rt/lib/libvruntime.so\n' > "${pre}"; chmod 0644 "${pre}"
VC="-v ${STAGE}/lib/libvruntime.so:/opt/enpu/vcann-rt/lib/libvruntime.so:ro \
 -v ${STAGE}/tools/enpu-monitor:/opt/enpu/vcann-rt/tools/enpu-monitor:ro \
 -v ${cfg}:/etc/enpu/vcann-rt/npu_info.config:ro -v ${pre}:/etc/ld.so.preload:ro -v /dev/shm:/dev/shm"

# ---- throwaway dsmi interposer: halve what npu-smi reports ----
# Shadows only the getters the driver header documents; dsmi_get_memory_info_v2 is
# imported by npu-smi and defined by the driver but declared in NO header, so it is
# left alone rather than called with a guessed signature.
cat > "${T}/dsmi_shim.c" <<'SHIM'
#define _GNU_SOURCE
#include <dlfcn.h>
#include <stdlib.h>
struct dsmi_hbm_info_stru {              /* driver/include/dsmi_common_interface.h */
    unsigned long long memory_size;      /* KB */
    unsigned int freq;
    unsigned long long memory_usage;     /* KB */
    int temp;
    unsigned int bandwith_util_rate;
};
static unsigned long long dv(void){ const char *v=getenv("DSMI_SHIM_DIV");
    unsigned long long d=(v&&*v)?strtoull(v,NULL,10):2ULL; return d?d:1ULL; }
int dsmi_get_hbm_info(int id, struct dsmi_hbm_info_stru *i){
    static int (*real)(int, struct dsmi_hbm_info_stru *);
    if (!real) { real = dlsym(RTLD_NEXT, "dsmi_get_hbm_info"); if (!real) return -1; }
    int rc = real(id, i);
    if (rc == 0 && i) { unsigned long long d = dv(); i->memory_size /= d; i->memory_usage /= d; }
    return rc;
}
int dsmi_get_device_utilization_rate(int id, int type, unsigned int *r){
    static int (*real)(int, int, unsigned int *);
    if (!real) { real = dlsym(RTLD_NEXT, "dsmi_get_device_utilization_rate"); if (!real) return -1; }
    int rc = real(id, type, r);
    if (rc == 0 && r) { *r = (unsigned int)(*r / dv()); }
    return rc;
}
SHIM

# ---- in-process view: acl.rt.get_mem_info == the hooked rtMemGetInfoEx ----
cat > "${T}/meminfo.py" <<'PY'
import acl
def ck(r, m):
    if r != 0:
        print("ERR %s rc=%d" % (m, r)); raise SystemExit(1)
ck(acl.init(), "init"); ck(acl.rt.set_device(0), "set_device")
ctx, r = acl.rt.create_context(0); ck(r, "create_context")
free, total, rc = acl.rt.get_mem_info(1)          # 1 = HBM
print("INPROC rc=%d free=%dMB total=%dMB" % (rc, free // 1048576, total // 1048576))
PY

# ---- allocation loop for the hami enforcement row ----
cat > "${T}/alloc.py" <<'PY'
import sys, acl
def ck(r, m):
    if r != 0:
        print("ERR %s rc=%d" % (m, r)); raise SystemExit(1)
ck(acl.init(), "init"); ck(acl.rt.set_device(0), "set_device")
ctx, r = acl.rt.create_context(0); ck(r, "create_context")
CHUNK = 256 * 1024 * 1024
ptrs = []; total = 0
for _ in range(int(sys.argv[1])):
    p, r = acl.rt.malloc(CHUNK, 0)
    if r != 0:
        print("FAILED at total=%dMB ret=%d" % (total // 1048576, r)); break
    ptrs.append(p); total += CHUNK
else:
    print("reached %dMB without limit" % (total // 1048576))
print("STOP total=%dMB" % (total // 1048576))
for p in ptrs: acl.rt.free(p)
PY

hbm_of(){ echo "$1" | sed -nE 's/.*HBM Capacity\(MB\)[^:]*: *([0-9]+).*/\1/p' | head -1; }

# =================== 1. where npu-smi actually reads from ===================
lnk="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" "${IMG}" \
  sh -c 'ns=$(command -v npu-smi) || { echo "SETUID=nobinary"; exit 0; }
         echo "PATH=$ns"
         if [ -u "$ns" ] || [ -g "$ns" ]; then echo "SETUID=yes"; else echo "SETUID=no $(stat -c %a "$ns")"; fi
         echo "RTIMPORTS=$(readelf -sW "$ns" 2>/dev/null | grep -cE " UND +rt[A-Za-z]")"
         ldd "$ns"' 2>&1)"
echo "${lnk}" | grep -q 'PATH=/' && row PASS "npu-smi present in container" "$(echo "${lnk}" | sed -nE 's/^PATH=//p')" \
  || { row FAIL "npu-smi present in container" absent; fails=$((fails+1)); }
echo "${lnk}" | grep -q libdrvdsmi_host.so && row PASS "npu-smi -> libdrvdsmi_host.so (dsmi)" ok \
  || { row FAIL "npu-smi -> libdrvdsmi_host.so (dsmi)" absent; fails=$((fails+1)); }
if echo "${lnk}" | grep -qE 'libruntime\.so|libdcmi\.so'; then
  row FAIL "npu-smi links neither libruntime nor libdcmi" "one is linked"; fails=$((fails+1))
else
  row PASS "npu-smi links neither libruntime nor libdcmi" "=> the rt* hooks cannot reach it"
fi
case "$(echo "${lnk}" | sed -nE 's/^SETUID=//p')" in
  no*)      row PASS "npu-smi not setuid/setgid" "mode $(echo "${lnk}" | sed -nE 's/^SETUID=no //p') — LD_PRELOAD is honored" ;;
  yes)      row FAIL "npu-smi not setuid/setgid" "setuid/setgid => the loader ignores LD_PRELOAD"; fails=$((fails+1)) ;;
  *)        row FAIL "npu-smi not setuid/setgid" "could not stat npu-smi — not judged"; fails=$((fails+1)) ;;
esac
# The direct evidence for "the rt* hooks cannot reach npu-smi": it imports no rt symbol
# at all. ldd only shows link-time deps, so this is the assertion the claim needs.
ri="$(echo "${lnk}" | sed -nE 's/^RTIMPORTS=//p')"
if [ "${ri:-x}" = "0" ]; then
  row PASS "npu-smi imports no rt* symbol" "0 (so no rt-layer interposer can reach it)"
else
  row FAIL "npu-smi imports no rt* symbol" "${ri:-<unreadable>}"; fails=$((fails+1))
fi

# =================== 2. baseline ===================
base="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" "${IMG}" \
  npu-smi info -t usages -i "${NPU}" -c "${CHIP}" 2>&1)"
BASE_HBM="$(hbm_of "${base}")"
[ -n "${BASE_HBM}" ] && row PASS "baseline npu-smi HBM Capacity(MB)" "${BASE_HBM}" \
  || { row FAIL "baseline npu-smi HBM Capacity(MB)" unreadable; fails=$((fails+1)); }

# =================== 3. vcann-rt: invisible with the gate off, visible with it on ===================
vc="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" -e ENPU_LOG_LEVEL=3 \
  ${VC} "${IMG}" sh -c "npu-smi info -t usages -i ${NPU} -c ${CHIP}; echo '===enpu-monitor==='; /opt/enpu/vcann-rt/tools/enpu-monitor" 2>&1)"
# npu-smi's own -t usages output contains "Usage Rate(%)" lines, so the
# enpu-monitor rows below must look only past the marker.
vc_em="$(echo "${vc}" | sed -n '/===enpu-monitor===/,$p')"
VC_HBM="$(hbm_of "${vc}")"
if [ "${VC_HBM:-x}" = "${BASE_HBM}" ]; then
  row PASS "gate off: npu-smi shows the physical card" "${VC_HBM} (ENPU_DSMI_HOOK unset => off, quota ${MEM}MB not reflected)"
else
  row FAIL "gate off: npu-smi shows the physical card" "${VC_HBM:-<none>} != baseline ${BASE_HBM}"; fails=$((fails+1))
fi
mq="$(echo "${vc_em}" | awk -F: '/Memory Limit quota/{gsub(/ /,"",$2);print $2}')"
[ "${mq}" = "${MEM}" ] && row PASS "vcann-rt: enpu-monitor reports the quota" "${mq}MB" \
  || { row FAIL "vcann-rt: enpu-monitor reports the quota == ${MEM}" "${mq:-none}"; fails=$((fails+1)); }
echo "${vc_em}" | grep -qiE 'Utilization|Usage Rate' \
  && row INFO "vcann-rt: enpu-monitor utilization" "present" \
  || row INFO "vcann-rt: enpu-monitor utilization" "absent — quota + Memory Usage only, no utilization percentage"

# ---- the product: ENPU_DSMI_HOOK=1 makes npu-smi report the slice ----
# Same injection as above plus the gate, so the pair differs by one env var. The rows
# also check what a container-global strong dsmi definition could plausibly break:
# enpu-monitor (which shares the library) and the plain non-CANN binaries.
dh="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" -e ENPU_DSMI_HOOK=1 \
  ${VC} "${IMG}" sh -c "npu-smi info -t usages -i ${NPU} -c ${CHIP}; echo '===enpu-monitor==='; /opt/enpu/vcann-rt/tools/enpu-monitor; echo '===others==='; /bin/true && ls / >/dev/null 2>&1 && python3 -c pass && echo OTHERS=ok" 2>&1)"
DH_HBM="$(hbm_of "${dh}")"
if [ "${DH_HBM:-x}" = "${MEM}" ]; then
  row PASS "gate on: npu-smi reports the slice" "${DH_HBM}MB == the quota (baseline ${BASE_HBM})"
elif [ "${DH_HBM:-x}" = "${BASE_HBM}" ]; then
  row FAIL "gate on: npu-smi reports the slice" "still ${DH_HBM} — is the staged libvruntime.so patched? (ASCEND-CASE 1 judges that)"; fails=$((fails+1))
else
  row FAIL "gate on: npu-smi reports the slice" "${DH_HBM:-<none>}, expected the quota ${MEM}"; fails=$((fails+1))
fi
dh_em="$(echo "${dh}" | sed -n '/===enpu-monitor===/,/===others===/p')"
dmq="$(echo "${dh_em}" | awk -F: '/Memory Limit quota/{gsub(/ /,"",$2);print $2}')"
[ "${dmq}" = "${MEM}" ] && row PASS "gate on: enpu-monitor unaffected" "${dmq}MB" \
  || { row FAIL "gate on: enpu-monitor reports the quota == ${MEM}" "${dmq:-none}"; fails=$((fails+1)); }
echo "${dh}" | grep -q 'OTHERS=ok' \
  && row PASS "gate on: non-CANN processes unaffected" "/bin/true, ls, python3 all ok" \
  || { row FAIL "gate on: non-CANN processes unaffected" "one of /bin/true, ls, python3 failed"; fails=$((fails+1)); }

# =================== 4. the dsmi layer CAN rewrite npu-smi ===================
sh_out="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" \
  -v "${T}/dsmi_shim.c:/dsmi_shim.c:ro" "${IMG}" sh -c "
    command -v gcc >/dev/null 2>&1 || { echo NOGCC; exit 0; }
    gcc -shared -fPIC -O1 -o /tmp/s.so /dsmi_shim.c -ldl 2>&1 || { echo SHIMBUILDFAIL; exit 0; }
    DSMI_SHIM_DIV=2 LD_PRELOAD=/tmp/s.so npu-smi info -t usages -i ${NPU} -c ${CHIP}" 2>&1)"
# "no compiler" is a legitimate skip; "the shim would not compile" is a failure. Keeping
# them apart matters because the shim hard-codes struct dsmi_hbm_info_stru from a driver
# header, so a driver bump that reshapes it lands exactly here.
if echo "${sh_out}" | grep -q NOGCC; then
  row WARN "dsmi interposition rewrites npu-smi" "skipped: no gcc in ${IMG}"; skips=$((skips+1))
elif echo "${sh_out}" | grep -q SHIMBUILDFAIL; then
  row FAIL "dsmi interposition rewrites npu-smi" "shim failed to compile — driver header ABI drift?"; fails=$((fails+1))
else
  SH_HBM="$(hbm_of "${sh_out}")"; HALF=$(( ${BASE_HBM:-0} / 2 ))
  if [ "${SH_HBM:-x}" = "${HALF}" ]; then
    row PASS "dsmi interposition rewrites npu-smi" "${BASE_HBM} -> ${SH_HBM} (VERDICT=rewritten)"
  elif [ "${SH_HBM:-x}" = "${BASE_HBM}" ]; then
    row FAIL "dsmi interposition rewrites npu-smi" "unchanged at ${SH_HBM} (VERDICT=no-op)"; fails=$((fails+1))
  else
    row FAIL "dsmi interposition rewrites npu-smi" "${SH_HBM:-<none>}, expected ${HALF} (VERDICT=broken)"; fails=$((fails+1))
  fi
fi

# =================== 5. hami-vnpu-core: same blind spot ===================
HL=""
[ -f "${HAMI}/libvnpu-needed.so" ] && HL="${HAMI}/libvnpu-needed.so"
[ -z "${HL}" ] && [ -f "${HAMI}/libvnpu.so" ] && HL="${HAMI}/libvnpu.so"
if [ -z "${HL}" ]; then
  row WARN "hami-vnpu-core rows" "skipped: no libvnpu.so under ${HAMI} (see the case header)"; skips=$((skips+1))
else
  # Two quotas on purpose. The question this case exists to answer is a VISIBILITY one
  # ("slice 50% of memory — does npu-smi show half?"), so rows 5a/5b run at a real 50%
  # of the card, derived from npu-smi rather than hardcoded; they are pure reads, so that
  # costs nothing. Enforcement (5c) needs to actually allocate, and proving a 32 GB cap
  # would mean allocating ~33 GB twice on a shared card — so it runs at a small quota.
  # The contract it proves is quota-value-independent: the limiter reads the quota from
  # shared memory with no special-casing of its value.
  HQ_VIEW=$(( ${BASE_HBM:-0} / 2 ))
  HQ_ENF=$(( MEM * 4 ))
  N=$(( HQ_ENF / 256 + 8 ))
  hb="$(basename "${HL}")"
  [ "${HQ_VIEW}" -gt 0 ] || { row FAIL "hami: 50% view quota derivable" "baseline HBM unreadable"; fails=$((fails+1)); HQ_VIEW="${HQ_ENF}"; }
  # 5a. npu-smi under hami's own global-preload activation
  hs="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" \
    -e NPU_GLOBAL_SHM_PATH=/tmp/vnpu_g4 -e NPU_LOCAL_SHM_PATH=/tmp/vnpu_l4a -e NPU_MEM_QUOTA="${HQ_VIEW}" \
    -v "${HL}:/hami/${hb}:ro" "${IMG}" sh -c "
      echo /hami/${hb} > /etc/ld.so.preload
      npu-smi info -t usages -i ${NPU} -c ${CHIP}; rm -f /etc/ld.so.preload" 2>&1)"
  HS_HBM="$(hbm_of "${hs}")"
  if [ "${HS_HBM:-x}" = "${BASE_HBM}" ]; then
    row PASS "hami: npu-smi still shows the physical card" "${HS_HBM} — a 50% slice (${HQ_VIEW}MB) is not reflected"
  elif [ "${HS_HBM:-x}" = "${HQ_VIEW}" ]; then
    row FAIL "hami: npu-smi still shows the physical card" "${HS_HBM} == quota — upstream now hooks dsmi?"; fails=$((fails+1))
  else
    row WARN "hami: npu-smi still shows the physical card" "${HS_HBM:-<none>} (baseline ${BASE_HBM}) — $(echo "${hs}" | grep -iE 'symbol lookup|cannot open' | head -1)"; skips=$((skips+1))
  fi
  # 5b. the application-visible total IS the quota (hooked rtMemGetInfoEx)
  hp="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" \
    -e LD_PRELOAD="/hami/${hb}" -e NPU_GLOBAL_SHM_PATH=/tmp/vnpu_g4 \
    -e NPU_LOCAL_SHM_PATH=/tmp/vnpu_l4b -e NPU_MEM_QUOTA="${HQ_VIEW}" \
    -v "${HL}:/hami/${hb}:ro" -v "${T}/meminfo.py:/meminfo.py:ro" \
    "${IMG}" python3 /meminfo.py 2>&1)"
  ht="$(echo "${hp}" | sed -nE 's/.*INPROC .*total=([0-9]+)MB.*/\1/p')"
  [ "${ht:-x}" = "${HQ_VIEW}" ] && row PASS "hami: in-process total == the 50% quota" "${ht}MB (hooked rtMemGetInfoEx)" \
    || { row FAIL "hami: in-process total == ${HQ_VIEW}MB" "${ht:-<none>}"; fails=$((fails+1)); }
  # 5c. the quota is really enforced. Each scenario runs in its own --rm container, so
  # /tmp (and the shmem files in it) is private already; the distinct names are belt-and-
  # braces for anyone who later folds these runs into one container, where reusing the
  # default NPU_LOCAL_SHM_PATH latches the earlier quota and the row passes vacuously.
  he="$(docker run --rm --runtime=ascend -e ASCEND_VISIBLE_DEVICES="${NPU}" \
    -e LD_PRELOAD="/hami/${hb}" -e NPU_GLOBAL_SHM_PATH=/tmp/vnpu_g4c \
    -e NPU_LOCAL_SHM_PATH=/tmp/vnpu_l4c -e NPU_MEM_QUOTA="${HQ_ENF}" \
    -v "${HL}:/hami/${hb}:ro" -v "${T}/alloc.py:/alloc.py:ro" \
    "${IMG}" python3 /alloc.py "${N}" 2>&1)"
  hn="$(echo "${he}" | sed -nE 's/.*STOP total=([0-9]+)MB.*/\1/p')"
  if [ -n "${hn}" ] && [ "${hn}" -le "${HQ_ENF}" ]; then row PASS "hami: quota enforced" "capped at ${hn}MB of ${HQ_ENF}MB"
  else row FAIL "hami: quota enforced <= ${HQ_ENF}MB" "${hn:-?}MB"; fails=$((fails+1)); fi
fi

echo "--- vcann-rt injected, gate off: npu-smi (physical) then enpu-monitor (the slice) ---"
echo "${vc}" | grep -iE 'HBM Capacity|===enpu-monitor===|Limit [Qq]uota|Memory Usage' | sed 's/^/    /'
echo "--- vcann-rt injected, ENPU_DSMI_HOOK=1: npu-smi now reports the slice ---"
echo "${dh}" | grep -iE 'HBM Capacity|HBM Usage|===enpu-monitor===|Limit [Qq]uota|Memory Usage|OTHERS=' | sed 's/^/    /'
echo "--- dsmi shim (mechanism control) ---"
echo "${sh_out}" | grep -iE 'HBM Capacity|NOGCC|SHIMBUILDFAIL|error' | sed 's/^/    /'
if [ -n "${HL}" ]; then
  echo "--- hami-vnpu-core ---"
  echo "${hs}" | grep -iE 'HBM Capacity|symbol lookup|cannot open' | sed 's/^/    /'
  echo "${hp}" | grep -E 'INPROC|Error' | sed 's/^/    /'
  echo "${he}" | grep -iE 'STOP|FAILED|reached|Quota Exceeded' | sed 's/^/    /'
fi
echo "SKIPPED=${skips}"
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "ASCEND-CASE 4" "$(xb_fails "${out}")"
