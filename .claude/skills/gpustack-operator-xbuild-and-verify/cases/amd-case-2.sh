#!/usr/bin/env bash
#
# AMD-CASE 2 — Single-card inject + the reported-capacity surface   (needs an AMD GPU)
#
#   amd-case-2.sh
#
# Preloads `libvrocm.so` into a container holding one card, gives that card a quota, and reads the
# capacity back through every exported way a workload can ask for it: `hipMemGetInfo`,
# `hipDeviceTotalMem`, and `hipDeviceProp_t.totalGlobalMem` through all three property entries.
#
# WHY THE CONTROLS ARE THE POINT. With the whole library loaded, every entry reports the quota —
# and that says nothing about WHICH wrapper did the work. Two control arms take that apart, each
# a library of one function built here rather than shipped, because a control is not a product:
#
#   - `hipMemGetInfo` alone. If the runtime derived its property figures from the same internal
#     accounting `hipMemGetInfo` reports, interposing that one entry would virtualise all three
#     and the property wrappers would be dead weight. Measured, it does not: `totalGlobalMem`
#     still carries the PHYSICAL figure. That is what makes the full arm's property rows evidence
#     for the property wrappers rather than a coincidence.
#
#   - the BARE `hipGetDeviceProperties` alone. ROCm 6+ headers macro-map that name onto
#     `…R0600`, so a workload compiled against them never calls the bare symbol at all. The arm
#     shows both halves at once: the name binds into the control object, and the call still lands
#     on the physical figure. A wrapper written against the plain name would compile, link, load
#     and virtualise nothing.
#
# WHY `multiProcessorCount` IS ASSERTED UNCHANGED. Memory and compute are independent surfaces
# here: the quota is enforced by this library, the CU mask by the runtime. A library that started
# rewriting the CU count would be inventing a compute figure nothing enforces, and a framework
# sizing its launch geometry from it would then run against a number no scheduler agrees with.
#
# WHY THE STRUCT LAYOUT IS A ROW. `hip/hip_query.c` takes `totalGlobalMem`'s offset with
# `offsetof` at build time rather than hard-coding it, and these three numbers are the regression
# fixture for that: if a ROCm release moves them, this row is where it surfaces — as a fact about
# the runtime rather than as a quota that mysteriously stopped applying.
#
# All arms run in ONE container, so the physical figure every control is compared against is the
# same card's, read minutes rather than runs apart. Activation is `/etc/ld.so.preload` INSIDE the
# container, which is the product's own contract; it is written immediately before an arm and
# removed immediately after, so the shell's own commands do not load a library they have no quota
# for.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree.
#
# Env: XB_IMAGE (default rocm/dev-ubuntu-22.04:7.2.4), XB_STAGE (default /tmp/vrocm, on the
#      TARGET), XB_AMD_GPU (default 0), XB_AMD_QUOTA_MIB (default 4096).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
XB_STAGE="${XB_STAGE:-/tmp/vrocm}"
XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"

xctr_resolve || { echo "amd-case-2: no container runtime on $(xtarget_desc); this case injects into a container"; exit 2; }

echo "# AMD-CASE 2 — single-card inject in ${XB_IMAGE} on $(xtarget_desc)"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" STAGE="${XB_STAGE}" \
           PLATFORM="${XB_PLATFORM}" GPU="${XB_AMD_GPU:-0}" QUOTA="${XB_AMD_QUOTA_MIB:-4096}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }

if [ ! -x "${STAGE}/hip_props_probe" ] || [ ! -f "${STAGE}/libvrocm.so" ]; then
  row FAIL "artifacts staged" "${STAGE} lacks libvrocm.so or hip_props_probe — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi
if [ ! -e /dev/kfd ]; then
  row SKIP "single-card inject" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
  --device /dev/kfd --device /dev/dri --group-add video --group-add render \
  --security-opt seccomp=unconfined \
  -e "GPU=${GPU}" -e "QUOTA=${QUOTA}" -v "${STAGE}:/work" -w /work "${IMG}" bash -s <<'INNER'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

W="$(mktemp -d)"
LEDGER="${W}/ledger"

# A figure that is neither the quota nor any card's physical total, so a control row cannot pass
# by coincidence with either.
CONTROL_MIB=777

# ---- the two control libraries -----------------------------------------------------------
#
# Built here rather than shipped. Each is a claim about the RUNTIME, not about the product: the
# tree has no business carrying a library whose only purpose is to be wrong in one specific way.
cat > "${W}/control_meminfo.c" <<'EOF'
/* Interposes hipMemGetInfo and nothing else. Answers from the environment without calling
 * through, because the arm only asks which entry points this one covers. */
#include <stddef.h>
#include <stdlib.h>

int hipMemGetInfo(size_t *free_bytes, size_t *total_bytes)
{
    const char *mib = getenv("XB_CONTROL_MIB");
    size_t bytes = (size_t)(mib != NULL ? strtoull(mib, NULL, 10) : 0) << 20;

    if (free_bytes != NULL) { *free_bytes = bytes; }
    if (total_bytes != NULL) { *total_bytes = bytes; }
    return 0;   /* hipSuccess */
}
EOF
cat > "${W}/control_props.c" <<'EOF'
/* Interposes the BARE hipGetDeviceProperties and nothing else.
 *
 * It returns an ERROR rather than a figure, and that is the whole design of the arm: a workload
 * compiled against ROCm 6+ headers calls ...R0600, so this must never be reached. If it ever is,
 * the probe reports the failure and the row fails loudly — which is a far clearer outcome than
 * returning success and leaving the caller's struct untouched. */
int hipGetDeviceProperties(void *prop, int device)
{
    (void)prop;
    (void)device;
    return 1;   /* hipErrorInvalidValue */
}
EOF
if ! cc_out="$(gcc -shared -fPIC -O2 -o "${W}/control_meminfo.so" "${W}/control_meminfo.c" 2>&1)" ||
   ! cc_out="${cc_out}$(gcc -shared -fPIC -O2 -o "${W}/control_props.so" "${W}/control_props.c" 2>&1)"; then
  row FAIL "controls build" "${cc_out}"
  echo "FAILS=1"; exit 0
fi

# ---- the full-card mask, from the product's own derivation --------------------------------
#
# The injection contract is three variables emitted TOGETHER — the visible list, the mask and the
# quota — and the `<i>` in the last two is a position in the visible list rather than a physical
# ordinal. This arm gives the container one card, so that position is 0 whatever GPU is. The CU
# list comes from `rocm-cumask-check`'s own derivation rather than from a literal here: a literal
# would be a second place to encode the card's geometry, and the wrong one.
/work/rocm-cumask-check --device "${GPU}" --percent 100 > "${W}/derive" 2>&1
culist="$(sed -n 's/^derive .*mask=[0-9]*:\(.*\)$/\1/p' "${W}/derive" | tail -1)"
if [ -z "${culist}" ]; then
  row FAIL "full-card mask derived" "rocm-cumask-check printed no mask: $(head -3 "${W}/derive" | tr '\n' ' ')"
  echo "FAILS=1"; exit 0
fi
MASK="0:${culist}"
row INFO "injection contract" "ROCR_VISIBLE_DEVICES=${GPU} HSA_CU_MASK=${MASK} VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}"

# ---- one probe run --------------------------------------------------------------------------
#
# `probe <preload|none> [VAR=VAL...]` — write the activation file, run the probe under the named
# environment, take the file away again. The removal suppresses its own stderr because it runs
# while the file still exists: the shim would otherwise load into `rm` too and report, correctly,
# that `rm` was given no quota.
probe() {
  local lib="$1"; shift
  local rc
  [ "${lib}" != none ] && echo "${lib}" > /etc/ld.so.preload
  env "$@" /work/hip_props_probe 0 2>&1
  rc=$?
  rm -f /etc/ld.so.preload 2>/dev/null
  return "${rc}"
}

# cap <file> <entry> — the total this entry reported, in MiB.
cap() { sed -n "s/^CAP entry=$2 .*total_mib=\([0-9]*\).*/\1/p" "$1" | tail -1; }
# bind <file> <name> — the object this name resolved into.
bind() { sed -n "s/^BIND name=$2 object=\([^ ]*\).*/\1/p" "$1" | tail -1; }

BASE_ENV="ROCR_VISIBLE_DEVICES=${GPU} HSA_CU_MASK=${MASK}"
# shellcheck disable=SC2086  # BASE_ENV is a list of assignments, word-split on purpose
probe none ${BASE_ENV} > "${W}/baseline" 2>&1
if ! grep -q '^CAP entry=hipMemGetInfo' "${W}/baseline"; then
  row FAIL "baseline probe runs" "$(tail -3 "${W}/baseline" | tr '\n' ' ')"
  echo "FAILS=1"; exit 0
fi

PHYS="$(cap "${W}/baseline" hipMemGetInfo)"
MPC="$(sed -n 's/^CAP entry=hipGetDeviceProperties(mapped) .* mpc=\([0-9]*\).*/\1/p' "${W}/baseline")"

# These two are the yardstick every row below measures against, and an unset yardstick makes
# those rows agree with anything: `[ "${c_bare}" = "${PHYS}" ]` is true when the probe reported
# neither figure, so both control arms -- the ones that give the inject arm its meaning -- would
# pass on a probe that printed nothing at all. Stop here rather than pass vacuously.
if [ -z "${PHYS}" ] || [ -z "${MPC}" ]; then
  row FAIL "baseline: the physical figures were read" \
    "hipMemGetInfo ${PHYS:-<nothing>} MiB, multiProcessorCount ${MPC:-<nothing>} — every comparison below is against these"
  echo "FAILS=1"; exit 0
fi

row INFO "card ${GPU}" "physical ${PHYS} MiB, multiProcessorCount ${MPC}, $(sed -n 's/.*name=\(.*\)$/\1/p' "${W}/baseline" | tail -1)"

# Every capacity name binds into the runtime when nothing is preloaded. Without this row the
# inject arm's binding rows would have no reference: a name that always bound to libvrocm.so
# would be indistinguishable from one this library actually took over.
stray="$(for n in hipMemGetInfo hipDeviceTotalMem hipGetDeviceProperties hipGetDevicePropertiesR0600 hipGetDevicePropertiesR0000; do
           o="$(bind "${W}/baseline" "${n}")"
           case "${o}" in *libamdhip64*) ;; *) printf '%s=%s ' "${n}" "${o:-<unresolved>}" ;; esac
         done)"
if [ -z "${stray}" ]; then
  row PASS "baseline: every capacity name binds libamdhip64" "5 of 5"
else
  row FAIL "baseline: every capacity name binds libamdhip64" "${stray}"; fails=$((fails+1))
fi

# The three paths a framework can take to the same number must agree before any of them is
# virtualised, or a later disagreement could be the runtime's rather than ours.
b_total="$(cap "${W}/baseline" hipDeviceTotalMem)"
b_props="$(cap "${W}/baseline" 'hipGetDeviceProperties(mapped)')"
if [ "${PHYS}" = "${b_total}" ] && [ "${PHYS}" = "${b_props}" ]; then
  row PASS "baseline: the three capacity entries agree" "${PHYS} MiB through all of them"
else
  row FAIL "baseline: the three capacity entries agree" \
    "hipMemGetInfo ${PHYS}, hipDeviceTotalMem ${b_total}, totalGlobalMem ${b_props}"
  fails=$((fails+1))
fi

# The regression fixture behind hip/hip_query.c's offsetof. Recorded on ROCm 7.2/gfx1101 and
# 7.2.4/gfx942 alike; a release that moves them surfaces here as a fact about the runtime.
layout="$(grep -m1 '^LAYOUT ' "${W}/baseline")"
if echo "${layout}" | grep -q 'r0600_size=1472 r0600_total_off=288 r0600_mpc_off=388'; then
  row PASS "hipDeviceProp_tR0600 layout" "${layout#LAYOUT }"
else
  row FAIL "hipDeviceProp_tR0600 layout" \
    "expected r0600_size=1472 r0600_total_off=288 r0600_mpc_off=388, got: ${layout#LAYOUT }"
  fails=$((fails+1))
fi

# ---- the full arm ---------------------------------------------------------------------------
# shellcheck disable=SC2086
probe /work/libvrocm.so ${BASE_ENV} "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
  "VROCM_LEDGER_PATH=${LEDGER}" LIBVROCM_LOG_LEVEL=2 > "${W}/full" 2>&1

if grep -q '^\[vrocm\] loaded' "${W}/full"; then
  row PASS "inject: the library loaded" "its constructor's marker is on the stream"
else
  row FAIL "inject: the library loaded" "no load marker: $(head -3 "${W}/full" | tr '\n' ' ')"; fails=$((fails+1))
fi

missed="$(for n in hipMemGetInfo hipDeviceTotalMem hipGetDeviceProperties hipGetDevicePropertiesR0600 hipGetDevicePropertiesR0000; do
            o="$(bind "${W}/full" "${n}")"
            case "${o}" in *libvrocm*) ;; *) printf '%s=%s ' "${n}" "${o:-<unresolved>}" ;; esac
          done)"
if [ -z "${missed}" ]; then
  row PASS "inject: every capacity name binds libvrocm.so" "5 of 5 — a call would land here, not in the runtime"
else
  row FAIL "inject: every capacity name binds libvrocm.so" "${missed}"; fails=$((fails+1))
fi

# Each entry separately, because each is a path a real framework takes and any one of them left
# alone leaks the whole card to exactly the code most likely to size an arena from it.
for entry in hipMemGetInfo hipDeviceTotalMem 'hipGetDeviceProperties(mapped)' \
             hipGetDevicePropertiesR0600 hipGetDevicePropertiesR0000; do
  got="$(cap "${W}/full" "${entry}")"
  if [ "${got}" = "${QUOTA}" ]; then
    row PASS "inject: ${entry} reports the quota" "${got} MiB"
  else
    row FAIL "inject: ${entry} reports the quota" "${got:-<nothing>} MiB, expected ${QUOTA}"; fails=$((fails+1))
  fi
done

full_mpc="$(sed -n 's/^CAP entry=hipGetDeviceProperties(mapped) .* mpc=\([0-9]*\).*/\1/p' "${W}/full")"
if [ "${full_mpc}" = "${MPC}" ]; then
  row PASS "inject: multiProcessorCount unchanged" "${full_mpc} — memory and compute are independent surfaces"
else
  row FAIL "inject: multiProcessorCount unchanged" "${full_mpc} under the quota, ${MPC} without it"; fails=$((fails+1))
fi

# ---- control A: hipMemGetInfo alone ----------------------------------------------------------
# shellcheck disable=SC2086
probe "${W}/control_meminfo.so" ${BASE_ENV} "XB_CONTROL_MIB=${CONTROL_MIB}" > "${W}/ctl_mem" 2>&1

c_mem="$(cap "${W}/ctl_mem" hipMemGetInfo)"
c_props="$(cap "${W}/ctl_mem" 'hipGetDeviceProperties(mapped)')"
c_total="$(cap "${W}/ctl_mem" hipDeviceTotalMem)"
if [ "${c_mem}" = "${CONTROL_MIB}" ]; then
  row PASS "control/mem-only: the control is in force" "hipMemGetInfo reports ${c_mem} MiB"
else
  row FAIL "control/mem-only: the control is in force" \
    "hipMemGetInfo reports ${c_mem:-<nothing>}, expected ${CONTROL_MIB} — the arm below proves nothing without this"
  fails=$((fails+1))
fi
if [ "${c_props}" = "${PHYS}" ] && [ "${c_total}" = "${PHYS}" ]; then
  row PASS "control/mem-only: the other two still report the physical figure" \
    "totalGlobalMem ${c_props} MiB and hipDeviceTotalMem ${c_total} MiB — so the full arm's figures came from their own wrappers"
else
  row FAIL "control/mem-only: the other two still report the physical figure" \
    "totalGlobalMem ${c_props}, hipDeviceTotalMem ${c_total}, physical ${PHYS} — one entry would then cover all three and the property wrappers would be untested"
  fails=$((fails+1))
fi

# ---- control B: the bare hipGetDeviceProperties alone -----------------------------------------
# shellcheck disable=SC2086
probe "${W}/control_props.so" ${BASE_ENV} > "${W}/ctl_bare" 2>&1

c_bind="$(bind "${W}/ctl_bare" hipGetDeviceProperties)"
c_bare="$(cap "${W}/ctl_bare" 'hipGetDeviceProperties(mapped)')"
case "${c_bind}" in
  *control_props*) row PASS "control/bare-name: the name binds the control object" "${c_bind}" ;;
  *) row FAIL "control/bare-name: the name binds the control object" "${c_bind:-<unresolved>}"; fails=$((fails+1)) ;;
esac
if [ "${c_bare}" = "${PHYS}" ]; then
  row PASS "control/bare-name: the mapped call never reaches it" \
    "the probe still reports ${c_bare} MiB — ROCm 6+ headers rewrite the call to ...R0600, so a wrapper on the plain name interposes a symbol nothing calls"
else
  row FAIL "control/bare-name: the mapped call never reaches it" \
    "expected the physical ${PHYS} MiB, got ${c_bare:-<the probe failed>}: $(grep -m1 hipGetDeviceProperties "${W}/ctl_bare" | tr '\n' ' ')"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
# The verdict is the payload's own count, read as a NUMBER off the last `FAILS=` line. Matching
# the token anywhere in the output would let any row satisfy the verdict by printing it in a
# detail column, and a payload that died before printing the line has to read as failure.
total="$(echo "${out}" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | tail -1)"
[ "${total:-1}" -eq 0 ] && { echo "AMD-CASE 2: PASS"; exit 0; } || { echo "AMD-CASE 2: FAIL"; exit 1; }
