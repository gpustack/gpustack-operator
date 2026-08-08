#!/usr/bin/env bash
#
# AMD-CASE 3 — Memory-path completeness   (needs an AMD GPU)
#
#   amd-case-3.sh
#
# Walks every allocation family through its OWN entry point — classic, managed, ext, pitch, 3D
# pitched, 2D and 3D array, the virtual-memory-management sequence, the three DRIVER-API halves,
# pinned host, stream-ordered async and the pool family — and requires each to be refused by this
# library when the request crosses the quota, and served when it does not.
#
# THE RUNTIME API AND THE DRIVER API ARE COUNTED SEPARATELY, because they are separate exported
# symbols reaching the same memory: `drvpitch`, `drvarray` and `drvarray3d` are the halves of
# `pitch`, `array` and `array3d` a caller reaches through `hipMemAllocPitch`, `hipArrayCreate` and
# `hipArray3DCreate`. Each was measured satisfying a 512 MiB request under a 64 MiB quota while its
# runtime-API twin was correctly refused one, which is why covering one half of a pair is not
# covering the family.
#
# ONE FAMILY PER PROCESS, EACH WITH ITS OWN LEDGER. A refusal has to be attributable to the size
# under test and to nothing else: two families in one process would let the first family's charge
# explain the second family's refusal, and the case could not tell which. That is also why every
# arm gets a fresh region file rather than sharing one.
#
# THE QUOTA IS DELIBERATELY BELOW ONE REQUEST. This is the whole design of the case. Run with a
# quota larger than every request and a free between entries, the same table passes whether or not
# an entry is charged at all — which is exactly the state the pool family was measured in: with
# only the classic family wrapped, `hipMallocFromPoolAsync` took another 10 GiB out of a card whose
# quota was 2 GiB, and no row that only ever asked for what fit would have seen it.
#
# EVERY REFUSAL IS CHECKED TWICE. The status must be `hipErrorOutOfMemory`, and the library's own
# per-entry counter must show a denial on THAT entry. The second half is what makes the row about
# this library: a runtime that refused for its own reasons produces the same status and moves no
# counter, and on a card with tens of GiB free the two are easy to confuse.
#
# THE ARRAY FAMILIES CARRY THEIR OWN, SMALLER FIGURES, and the reason is a hardware ceiling rather
# than a preference: an array request has a SHAPE as well as a size, and `max_w * max_h * 4` is
# about 1 GiB on a part reporting 16384 each way. Asking for the standard over-quota figure there
# is refused for its shape (`hipErrorInvalidValue`) before the quota is ever consulted, and a case
# that read that as the quota working would pass with the ledger switched off. Where the part
# refuses the family whatever the shape — measured on gfx942, `hipMallocArray` is unsupported —
# the arm reports SKIP with the runtime's own status rather than a verdict it cannot reach.
#
# THE LAST TWO ROWS STAND OUTSIDE THE FAMILIES. `refund` fills most of the quota, frees it and
# asks again, which is the only observation here that needs two allocations in one process. And
# the caller-origin baseline reads the library's own `<entry> <- <object>` diagnostics across every
# run: a preload cannot decline to fire on a call the runtime makes to its own public entry point,
# so what stands between that and a problem is the measured fact that the runtime does not make
# one on any allocating entry. This is where that stops being an assumption.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree.
#
# Env: XB_IMAGE (default rocm/dev-ubuntu-22.04:7.2.4), XB_STAGE (default /tmp/vrocm, on the
#      TARGET), XB_AMD_GPU (0), XB_AMD_QUOTA_MIB (4096), XB_AMD_UNDER_MIB (1024),
#      XB_AMD_OVER_MIB (8192).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
XB_STAGE="${XB_STAGE:-/tmp/vrocm}"
XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"

xctr_resolve || { echo "amd-case-3: no container runtime on $(xtarget_desc); this case injects into a container"; exit 2; }

echo "# AMD-CASE 3 — memory-path completeness in ${XB_IMAGE} on $(xtarget_desc)"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" STAGE="${XB_STAGE}" \
           PLATFORM="${XB_PLATFORM}" GPU="${XB_AMD_GPU:-0}" QUOTA="${XB_AMD_QUOTA_MIB:-4096}" \
           UNDER="${XB_AMD_UNDER_MIB:-1024}" OVER="${XB_AMD_OVER_MIB:-8192}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }

if [ ! -x "${STAGE}/hip_mem_paths" ] || [ ! -f "${STAGE}/libvrocm.so" ]; then
  row FAIL "artifacts staged" "${STAGE} lacks libvrocm.so or hip_mem_paths — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi
if [ ! -e /dev/kfd ]; then
  row SKIP "memory-path completeness" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
  --device /dev/kfd --device /dev/dri --group-add video --group-add render \
  --security-opt seccomp=unconfined \
  -e "GPU=${GPU}" -e "QUOTA=${QUOTA}" -e "UNDER=${UNDER}" -e "OVER=${OVER}" \
  -v "${STAGE}:/work" -w /work "${IMG}" bash -s <<'INNER'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

W="$(mktemp -d)"

# The full-card mask from the product's own derivation. The three injected variables are one
# tuple — the visible list, the mask and the quota — and this case changes only the quota, so the
# other two are set exactly as every other case sets them.
/work/rocm-cumask-check --device "${GPU}" --percent 100 > "${W}/derive" 2>&1
culist="$(sed -n 's/^derive .*mask=[0-9]*:\(.*\)$/\1/p' "${W}/derive" | tail -1)"
if [ -z "${culist}" ]; then
  row FAIL "full-card mask derived" "rocm-cumask-check printed no mask: $(head -3 "${W}/derive" | tr '\n' ' ')"
  echo "FAILS=1"; exit 0
fi
MASK="0:${culist}"

# run_path <family> <mib> <quota-mib> <tag> — one family, one process, its own region file.
#
# The activation file is written immediately before the run and taken away immediately after: left
# in place, the shim would load into every `sed` and `rm` this script runs and report, correctly,
# that each was given no quota. The removal suppresses its own stderr for the same reason — it
# runs while the file still exists.
run_path() {
  local family="$1" mib="$2" quota="$3" tag="$4"
  # A second statement, because one `local` expands every word before it assigns any of them:
  # `log` would read a `tag` that is not set yet, and under `set -u` that aborts the container.
  local log="${W}/${family}.${tag}"

  echo /work/libvrocm.so > /etc/ld.so.preload
  env "ROCR_VISIBLE_DEVICES=${GPU}" "HSA_CU_MASK=${MASK}" \
      "VROCM_DEVICE_MEMORY_LIMIT_0=${quota}" "VROCM_LEDGER_PATH=${W}/ledger.${family}.${tag}" \
      LIBVROCM_LOG_LEVEL=2 \
      /work/hip_mem_paths "${family}" "${mib}" 0 > "${log}" 2>&1
  rm -f /etc/ld.so.preload 2>/dev/null
}

# result <log> <family> — success | failed
result() { sed -n "s/^PATH $2 result=\([a-z]*\) .*/\1/p" "$1" | tail -1; }
# status <log> <family> — the HIP status the family finished with.
status() { sed -n "s/^PATH $2 result=[a-z]* rc=\([0-9]*\).*/\1/p" "$1" | tail -1; }
# calls / denials <log> <entry> — what this library counted on that entry.
calls() { sed -n "s/^\[vrocm\] counter $2 calls=\([0-9]*\) .*/\1/p" "$1" | tail -1; }
denials() { sed -n "s/^\[vrocm\] counter $2 calls=[0-9]* denials=\([0-9]*\).*/\1/p" "$1" | tail -1; }

# hipErrorOutOfMemory. Every refusal that is ours becomes this status, because that is what it is
# from the caller's side; anything else came from the runtime and means something different.
OOM=2

# <family> <entry the family enters through> <quota> <under> <over>
#
# The array families take their own trio for the texture-ceiling reason in the header comment.
# Every other family takes the case's own knobs, so a run can be re-pointed at a smaller card
# without editing this table.
TABLE="plain hipMalloc ${QUOTA} ${UNDER} ${OVER}
managed hipMallocManaged ${QUOTA} ${UNDER} ${OVER}
ext hipExtMallocWithFlags ${QUOTA} ${UNDER} ${OVER}
pitch hipMallocPitch ${QUOTA} ${UNDER} ${OVER}
malloc3d hipMalloc3D ${QUOTA} ${UNDER} ${OVER}
array hipMallocArray 64 32 512
array3d hipMalloc3DArray 64 32 512
vmm hipMemCreate ${QUOTA} ${UNDER} ${OVER}
drvpitch hipMemAllocPitch ${QUOTA} ${UNDER} ${OVER}
drvarray hipArrayCreate 64 32 512
drvarray3d hipArray3DCreate 64 32 512
host hipHostMalloc ${QUOTA} ${UNDER} ${OVER}
async hipMallocAsync ${QUOTA} ${UNDER} ${OVER}
pool hipMallocFromPoolAsync ${QUOTA} ${UNDER} ${OVER}"

while read -r family entry quota under over; do
  [ -n "${family}" ] || continue

  # The under-quota arm first, and it decides whether the over-quota arm means anything: a family
  # this part does not support refuses whatever the quota, and reading that as the quota working
  # is the one mistake this case is built to avoid.
  run_path "${family}" "${under}" "${quota}" under
  log="${W}/${family}.under"
  got="$(result "${log}" "${family}")"

  # No result line at all is its own answer, and it is not "the runtime refused". The program
  # crashed, failed to load, or no longer knows this family's name -- and reading that as a
  # refusal would send it down the SKIP arm below, which counts nothing and skips the over-quota
  # arm too, so the family would disappear from the suite without anything turning red.
  if [ -z "${got}" ]; then
    row FAIL "${family}: the probe reported a result" \
      "read no 'PATH ${family} result=' line: $(tail -3 "${log}" | tr '\n' ' ' | cut -c1-200)"
    fails=$((fails+1))
    continue
  fi

  if [ "${got}" != success ]; then
    rc="$(status "${log}" "${family}")"
    if [ "${rc}" = "${OOM}" ]; then
      row FAIL "${family}: ${under} MiB under a ${quota} MiB quota is served" \
        "refused with hipErrorOutOfMemory — this library charged more than the request"
      fails=$((fails+1))
    else
      row SKIP "${family}: this part supports the family" \
        "the runtime refused ${under} MiB with rc=${rc:-<none>} whatever the quota: $(grep -m1 "^PATH ${family} step=" "${log}" | tr '\n' ' ')"
    fi
    continue
  fi
  # The entry's own counter as well as the program's result: "served" would also be true of a
  # family that reached the runtime without passing through this library at all, which is exactly
  # the state the pool family was measured in before it was wrapped.
  hits="$(calls "${log}" "${entry}")"
  if [ "${hits:-0}" -ge 1 ]; then
    row PASS "${family}: ${under} MiB under a ${quota} MiB quota is served through ${entry}" \
      "the entry counted ${hits} call(s)"
  else
    row FAIL "${family}: ${under} MiB under a ${quota} MiB quota is served through ${entry}" \
      "served, but ${entry} counted ${hits:-<no counter>} calls — the allocation did not pass through this library"
    fails=$((fails+1))
  fi

  run_path "${family}" "${over}" "${quota}" over
  log="${W}/${family}.over"
  got="$(result "${log}" "${family}")"
  rc="$(status "${log}" "${family}")"
  den="$(denials "${log}" "${entry}")"

  # Pinned host pages are not device memory, so the library counts the call and charges nothing.
  # Running the family is how a case proves that is still true: a `host` arm that started failing
  # under quota would mean system memory had begun being charged against a device figure.
  if [ "${family}" = host ]; then
    if [ "${got}" = success ] && [ "${den:-0}" = 0 ]; then
      row PASS "host: ${over} MiB over a ${quota} MiB quota is still served" \
        "pinned host pages are counted and not charged; ${entry} denials=${den:-0}"
    else
      row FAIL "host: ${over} MiB over a ${quota} MiB quota is still served" \
        "result=${got} rc=${rc} ${entry} denials=${den:-0} — system memory is being charged against a device quota"
      fails=$((fails+1))
    fi
    continue
  fi

  if [ "${got}" = failed ] && [ "${rc}" = "${OOM}" ] && [ "${den:-0}" -ge 1 ]; then
    row PASS "${family}: ${over} MiB over a ${quota} MiB quota is refused by this library" \
      "hipErrorOutOfMemory, and ${entry} counted ${den} denial(s)"
  else
    row FAIL "${family}: ${over} MiB over a ${quota} MiB quota is refused by this library" \
      "result=${got:-<none>} rc=${rc:-<none>} ${entry} denials=${den:-<no counter>}"
    fails=$((fails+1))
  fi
done <<EOF
${TABLE}
EOF

# The pitch family's real footprint is not the number the caller passed: the runtime pads each row
# to its own alignment. The library admits on the caller's figure and reconciles to pitch x height
# under the same lock, and recording both is what would make a change to that visible.
pitch_line="$(grep -m1 '^PATH pitch width=' "${W}/pitch.under" || true)"
[ -n "${pitch_line}" ] && row INFO "pitch: row padding" "${pitch_line#PATH pitch }"

# ---- refund ---------------------------------------------------------------------------------
#
# Just over half the quota, twice, with a free between: the second allocation can only succeed if
# the first was actually refunded, and the pair cannot both be held.
REFUND_MIB=$(( QUOTA * 5 / 8 ))
run_path refund "${REFUND_MIB}" "${QUOTA}" once
if [ "$(result "${W}/refund.once" refund)" = success ]; then
  row PASS "refund: a freed charge is returned to the quota" \
    "${REFUND_MIB} MiB taken, freed and taken again under ${QUOTA} MiB — the pair together is $(( REFUND_MIB * 2 )) MiB and could not both be held"
else
  row FAIL "refund: a freed charge is returned to the quota" \
    "the second ${REFUND_MIB} MiB request was refused: $(grep '^PATH refund step=' "${W}/refund.once" | tr '\n' ' ')"
  fails=$((fails+1))
fi

# ---- the caller-origin baseline ---------------------------------------------------------------
#
# A preload cannot decline to fire on a call the runtime makes to its own public entry point, so
# the one thing standing between that and a re-entrancy problem is the measured fact that the
# runtime makes no such call on any allocating entry. Read across EVERY run above rather than one,
# because a family that did provoke one would otherwise have to be guessed at in advance.
grep -h '^\[vrocm\] .* <- ' "${W}"/*.under "${W}"/*.over "${W}"/refund.once 2>/dev/null \
  | sed 's/^\[vrocm\] //' | sort -u > "${W}/origins"

self="$(awk '$3 ~ /libamdhip64/ { print $1 }' "${W}/origins" | sort -u | tr '\n' ' ')"
alloc_self="$(awk '$3 ~ /libamdhip64/ && $1 != "hipMemGetInfo" { print $1 }' "${W}/origins" | sort -u | tr '\n' ' ')"
if [ ! -s "${W}/origins" ]; then
  # An empty file and a clean baseline look identical to the test below, and only one of them is
  # evidence. Every run above sets LIBVROCM_LOG_LEVEL=2, so no origin line at all means the
  # diagnostic never fired and this row has read nothing.
  row FAIL "caller origin: the runs recorded who called each entry" \
    "not one origin line across $(ls "${W}"/*.under "${W}"/*.over "${W}"/refund.once 2>/dev/null | wc -l | tr -d ' ') run(s) — the baseline below would be drawn from an empty file"
  fails=$((fails+1))
elif [ -z "${alloc_self}" ]; then
  row PASS "caller origin: the runtime calls no allocating entry of its own" \
    "entries reached from libamdhip64: ${self:-none}"
else
  row FAIL "caller origin: the runtime calls no allocating entry of its own" \
    "${alloc_self}— a wrapper firing on the runtime's own call is the re-entrancy this design cannot decline"
  fails=$((fails+1))
fi
row INFO "caller origin: what called each entry" \
  "$(awk '{ n = $3; sub(/.*\//, "", n); print $1 " <- " n }' "${W}/origins" | sort -u | tr '\n' ' ')"

echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
xb_verdict "AMD-CASE 3" "$(xb_fails "${out}")"
