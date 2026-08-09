#!/usr/bin/env bash
#
# AMD-CASE 7 — lifecycle and cross-version reach   (part A needs no GPU; part B needs one)
#
#   amd-case-7.sh
#
# PART A — a charge outlives the process that took it, and the next process takes it back.
#
# `common/`'s unit tests already cover the sweep from a forked child the test itself kills. What a
# fork cannot reproduce is the case the sweep exists for: a charge left in a region file that
# OUTLIVES every process that knew about it, reclaimed by a process started later with no memory of
# the first. That needs two real processes and a signal from outside.
#
# The failure is measured, not hypothetical: before the sweep existed, a process `SIGKILL`ed while
# holding 4 GiB of a 6 GiB quota left the next process able to claim only 2, and the shortfall
# survived for as long as the region file did — for a Pod, until it was deleted.
#
# THE CONTROL IS THE HALF THAT MAKES IT MEAN ANYTHING. Before the kill, the same request must be
# REFUSED. A sweep that reclaimed from a LIVE process would satisfy the reclaim row and be a far
# worse bug than the one it was written for — two containers would then take turns evicting each
# other's accounting.
#
# PART B — one artifact, every ROCm version.
#
# The product links no ROCm object, resolves every runtime symbol at run time and is claimed to
# serve any version. That claim is what removes the per-version build fan-out the other backends
# need, so it is worth a case rather than a sentence. The SAME `libvrocm.so` — asserted by digest,
# not by assumption — is preloaded into a ROCm 7.x container and a ROCm 6.x one, and must enforce
# the same quota in both.
#
# THE WORKLOAD IS REBUILT PER IMAGE AND THE SHIM IS NOT, which is the whole shape of the claim. A
# gate binary links `libamdhip64.so.7` and cannot even load in a 6.x container; a real workload is
# built against the runtime it ships beside, exactly like this. If the shim needed the same
# treatment there would be nothing to test. The workload's own `DT_NEEDED` is what the case reads
# to prove the two arms really did face different runtimes — `.so.7` in one and `.so.6` in the
# other. The library's `resolving through` line is NOT that evidence and is not looked for: it is
# printed only on the `dlopen` fallback, which a workload that LINKS the runtime never takes.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree.
#
# Env: XB_IMAGE (default rocm/dev-ubuntu-22.04:7.2.4), XB_AMD_ROCM_OLD_IMAGE (default
#      rocm/dev-ubuntu-22.04:6.4.4 — the point of that arm is that it is the SAME artifact, so the
#      image is the only thing that changes), XB_STAGE (default /tmp/vrocm, on the TARGET),
#      XB_AMD_GPU (0), XB_AMD_QUOTA_MIB (4096), XB_AMD_UNDER_MIB (1024), XB_AMD_OVER_MIB (8192).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
XB_AMD_ROCM_OLD_IMAGE="${XB_AMD_ROCM_OLD_IMAGE:-rocm/dev-ubuntu-22.04:6.4.4}"
XB_STAGE="${XB_STAGE:-/tmp/vrocm}"
XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"

echo "# AMD-CASE 7 — lifecycle, and ${XB_IMAGE} vs ${XB_AMD_ROCM_OLD_IMAGE} on $(xtarget_desc)"

# ---- Part A: no container, no device -------------------------------------------------------
partA="$(xsh STAGE="${XB_STAGE}" QUOTA="${XB_AMD_QUOTA_MIB:-4096}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

if [ ! -x "${STAGE}/ledger_lifecycle" ]; then
  row FAIL "A: ledger_lifecycle staged" "${STAGE}/ledger_lifecycle missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi

W="$(mktemp -d)"
trap 'rm -rf "${W}"' EXIT
LEDGER="${W}/ledger"

# Three quarters held and never released, and a reclaim asking for half: the two together are more
# than the quota, so the request can only be admitted if the stranded charge is recovered first.
HOLD=$(( QUOTA * 3 / 4 ))
TAKE=$(( QUOTA / 2 ))

env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
    "${STAGE}/ledger_lifecycle" hold 0 "${HOLD}" > "${W}/hold.log" 2>&1 &
holder=$!
for _ in $(seq 50); do grep -q '^HOLD ready' "${W}/hold.log" && break; sleep 0.1; done
if ! grep -q '^HOLD ready' "${W}/hold.log"; then
  row FAIL "A: a process holds ${HOLD} MiB" "$(tail -2 "${W}/hold.log" | tr '\n' ' ')"
  kill "${holder}" 2>/dev/null
  echo "FAILS=1"; exit 0
fi
row PASS "A: a process holds ${HOLD} MiB of a ${QUOTA} MiB quota" "pid ${holder}, and it will not release it"

# The control. While the holder is ALIVE the same request must be refused: a sweep that took a
# charge back from a live process would pass the reclaim row below and be the worse bug.
env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
    "${STAGE}/ledger_lifecycle" reclaim 0 "${TAKE}" > "${W}/live.log" 2>&1
live_rc=$?
if [ "${live_rc}" -ne 0 ]; then
  row PASS "A: the sweep takes nothing from a LIVE holder" \
    "${TAKE} MiB refused while ${HOLD} MiB is held by a running process"
else
  row FAIL "A: the sweep takes nothing from a LIVE holder" \
    "admitted — a charge was reclaimed from a process that still holds it: $(grep -m1 '^LEDGER' "${W}/live.log")"
  fails=$((fails+1))
fi

# SIGKILL, which is the whole point: a signal the process can neither catch nor clean up after.
# Anything gentler would let an exit path tidy up and prove nothing.
kill -9 "${holder}" 2>/dev/null
wait "${holder}" 2>/dev/null
for _ in $(seq 50); do kill -0 "${holder}" 2>/dev/null || break; sleep 0.1; done

stranded="$(env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
              "${STAGE}/ledger_lifecycle" probe 0 2>&1 | sed -n 's/^LEDGER device=0 .*used_mib=\([0-9]*\) .*/\1/p')"
if [ "${stranded}" = "${HOLD}" ]; then
  row PASS "A: the charge outlives the process that took it" \
    "${stranded} MiB still accounted after SIGKILL — the region has no idea its owner is gone"
else
  row FAIL "A: the charge outlives the process that took it" \
    "the region reports ${stranded:-<nothing>} MiB, expected ${HOLD}"
  fails=$((fails+1))
fi

env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
    "${STAGE}/ledger_lifecycle" reclaim 0 "${TAKE}" > "${W}/dead.log" 2>&1
dead_rc=$?
if [ "${dead_rc}" -eq 0 ]; then
  row PASS "A: a later process reclaims it" \
    "${TAKE} MiB admitted with no memory of the first process — $(grep -m1 '^LEDGER' "${W}/dead.log")"
else
  row FAIL "A: a later process reclaims it" \
    "still refused; the shortfall survives as long as the region file does: $(tail -2 "${W}/dead.log" | tr '\n' ' ')"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${partA}" | grep -v '^FAILS='

# ---- Part B: the same artifact under two ROCm versions --------------------------------------
if ! xctr_resolve; then
  echo "SKIP | B: cross-version reach | no container runtime on $(xtarget_desc)"
  partB="FAILS=0"
else
  partB="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" STAGE="${XB_STAGE}" \
               NEW="${XB_IMAGE}" OLD="${XB_AMD_ROCM_OLD_IMAGE}" PLATFORM="${XB_PLATFORM}" \
               GPU="${XB_AMD_GPU:-0}" QUOTA="${XB_AMD_QUOTA_MIB:-4096}" \
               UNDER="${XB_AMD_UNDER_MIB:-1024}" OVER="${XB_AMD_OVER_MIB:-8192}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

for f in libvrocm.so build.sh; do
  [ -e "${STAGE}/${f}" ] && continue
  row FAIL "B: artifacts staged" "${STAGE}/${f} missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
done
if [ ! -e /dev/kfd ]; then
  row SKIP "B: cross-version reach" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

# arm <image> — one image: rebuild the WORKLOAD against that image's ROCm, preload the SAME shim,
# and report the digest it loaded, the soname it resolved through and both quota outcomes.
arm() {
  local img="$1"
  # shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
  ${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
    --device /dev/kfd --device /dev/dri --group-add video --group-add render \
    --security-opt seccomp=unconfined \
    -e "GPU=${GPU}" -e "QUOTA=${QUOTA}" -e "UNDER=${UNDER}" -e "OVER=${OVER}" \
    -v "${STAGE}:/work" -w /work "${img}" bash -s <<'INNER'
set -u
W="$(mktemp -d)"
echo "ARM rocm=$(cat /opt/rocm/.info/version 2>/dev/null || echo unknown)"
echo "ARM shim_sha256=$(sha256sum /work/libvrocm.so | cut -d' ' -f1)"
echo "ARM runtime=$(ls /opt/rocm/lib/libamdhip64.so.* 2>/dev/null | head -1)"

# The workload is built HERE, against this image's ROCm, through the tree's own recipe. A gate
# binary staged from the 7.x build links libamdhip64.so.7 and could not load in a 6.x container —
# which is exactly how a real workload behaves, and exactly what the shim does not have to do.
if ! build_out="$(OUT="${W}" /work/build.sh test hip_mem_paths 2>&1)"; then
  echo "ARM build_failed=${build_out}"
  exit 0
fi
# The soname the freshly built workload links. This is the evidence the two arms exercised
# DIFFERENT runtimes: the shim is one file in both, and what it was preloaded ahead of is not.
echo "ARM workload_needs=$(readelf -d "${W}/hip_mem_paths" | sed -n 's/.*Shared library: \[\(libamdhip64[^]]*\)\].*/\1/p' | head -1)"
echo "ARM workload_built=ok"

for arm_size in "${UNDER}" "${OVER}"; do
  echo /work/libvrocm.so > /etc/ld.so.preload
  env "ROCR_VISIBLE_DEVICES=${GPU}" "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
      "VROCM_LEDGER_PATH=${W}/ledger.${arm_size}" LIBVROCM_LOG_LEVEL=2 \
      "${W}/hip_mem_paths" plain "${arm_size}" 0 > "${W}/run.${arm_size}" 2>&1
  rm -f /etc/ld.so.preload 2>/dev/null
  echo "ARM size=${arm_size} $(sed -n 's/^PATH plain \(result=.*\)$/\1/p' "${W}/run.${arm_size}" | tail -1)"
  echo "ARM size=${arm_size} $(sed -n 's/^\[vrocm\] \(counter hipMalloc calls=.*\)$/\1/p' "${W}/run.${arm_size}" | tail -1)"
done
INNER
}

field() { sed -n "s/^ARM $1=\(.*\)$/\1/p" "$2" | tail -1; }

W="$(mktemp -d)"
trap 'rm -rf "${W}"' EXIT
arm "${NEW}" > "${W}/new" 2>&1
arm "${OLD}" > "${W}/old" 2>&1

for tag in new old; do
  img="${NEW}"; [ "${tag}" = old ] && img="${OLD}"
  f="${W}/${tag}"
  rocm="$(field rocm "${f}")"
  runtime="$(field runtime "${f}")"

  if [ "$(field workload_built "${f}")" != ok ]; then
    row FAIL "B/${rocm:-${img}}: the workload builds against this image's ROCm" \
      "$(field build_failed "${f}" | tr '\n' ' ' | cut -c1-300)"
    fails=$((fails+1)); continue
  fi
  row INFO "B/${rocm:-?}: image" "${img}, runtime $(basename "${runtime:-unknown}")"

  served="$(grep -m1 "^ARM size=${UNDER} result=" "${f}")"
  refused="$(grep -m1 "^ARM size=${OVER} result=" "${f}")"
  if [ "${served#*result=}" = "success rc=0" ]; then
    row PASS "B/${rocm}: ${UNDER} MiB under a ${QUOTA} MiB quota is served" "${served#ARM size=}"
  else
    row FAIL "B/${rocm}: ${UNDER} MiB under a ${QUOTA} MiB quota is served" "${served:-<no result line>}"
    fails=$((fails+1))
  fi
  if [ "${refused#*result=}" = "failed rc=2" ]; then
    row PASS "B/${rocm}: ${OVER} MiB over the same quota is refused" "${refused#ARM size=}"
  else
    row FAIL "B/${rocm}: ${OVER} MiB over the same quota is refused" "${refused:-<no result line>}"
    fails=$((fails+1))
  fi

done

# The claim in one row: byte for byte the same object in both containers.
new_sha="$(field shim_sha256 "${W}/new")"
old_sha="$(field shim_sha256 "${W}/old")"
if [ -n "${new_sha}" ] && [ "${new_sha}" = "${old_sha}" ]; then
  row PASS "B: it is the SAME artifact in both containers" \
    "libvrocm.so sha256=$(echo "${new_sha}" | cut -c1-16)… — one build, no per-version fan-out"
else
  row FAIL "B: it is the SAME artifact in both containers" "${new_sha:-<none>} vs ${old_sha:-<none>}"
  fails=$((fails+1))
fi

new_so="$(field workload_needs "${W}/new")"
old_so="$(field workload_needs "${W}/old")"
if [ -n "${new_so}" ] && [ -n "${old_so}" ] && [ "${new_so}" != "${old_so}" ]; then
  row PASS "B: and it was preloaded ahead of a DIFFERENT runtime in each" \
    "the workload links ${new_so} in one container and ${old_so} in the other — the resolver's soname list is what makes one shim serve both"
else
  row FAIL "B: and it was preloaded ahead of a DIFFERENT runtime in each" \
    "${new_so:-<none>} and ${old_so:-<none>} — if these match, the two arms did not exercise different runtimes"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
PAYLOAD
)"
fi
echo "${partB}" | grep -v '^FAILS='

total=$(( $(xb_fails "${partA}") + $(xb_fails "${partB}") ))
echo "FAILS=${total}"
xb_verdict "AMD-CASE 7" "${total}"
