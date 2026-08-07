#!/usr/bin/env bash
#
# AMD-CASE 4 — CU-mask conformance   (needs an AMD GPU)
#
#   amd-case-4.sh
#
# Drives `rocm-cumask-check` across every row of the conformance table the card selects, and
# across every fail-open construction for that architecture as NEGATIVE rows.
#
# WHY THE NEGATIVE ROWS ARE THE POINT. A CU mask fails open: the runtime that rejects one returns
# no error, logs no line and changes no return code — the container simply gets the whole card.
# So the thing worth testing is not that a good mask works, it is that a bad one is still
# DETECTED. A case set that quietly stopped detecting the fail-open modes would otherwise stay
# green while the product lost all compute isolation.
#
# WHY THE TABLE IS CHOSEN BY NUM_XCC AND NOT BY A FLAG. The two derivations share no arithmetic
# and each is silently wrong on the other architecture: RDNA's WGP pairing carried onto CDNA
# doubles every slice, and CDNA's atom carried onto RDNA splits WGP pairs, which discards the
# whole mask. Reading the branch off the card means pointing this case at a different host is the
# only thing needed — there is no flag to get wrong.
#
# WHY NO CONTAINER. The subject here is mask semantics, not injection: every row runs a staged
# binary under an environment variable, and a container would add nothing to what is measured. It
# would also cost the only multi-XCC hardware available, which is a rented instance that IS a
# container and has no runtime of its own.
#
# The rows and their expected figures come from `references/amd-cumask-conformance.md`, which is
# where they are explained; this case is the executable half of that page. The percentages below
# are the INTEGER inputs that land on each of that page's rows — the tool takes whole percents,
# while the page states the exact fraction of the card each row consumes.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree; this case only
# runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_STAGE (default /tmp/vrocm, on the TARGET), XB_AMD_GPU (default 0).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_STAGE="${XB_STAGE:-/tmp/vrocm}"

echo "# AMD-CASE 4 — CU-mask conformance on $(xtarget_desc)"

out="$(xsh STAGE="${XB_STAGE}" GPU="${XB_AMD_GPU:-0}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

CHECK="${STAGE}/rocm-cumask-check"
if [ ! -x "${CHECK}" ]; then
  row FAIL "probe staged" "${CHECK} missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi
row INFO "probe staged" "rocm-cumask-check sha256=$(sha256sum "${CHECK}" | cut -c1-16)…"

if [ ! -e /dev/kfd ]; then
  row SKIP "conformance table" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

W="$(mktemp -d)"
trap 'rm -rf "${W}"' EXIT

# The topology comes from the probe's own HSA read rather than from KFD sysfs. Both agree, but
# sysfs is where the trap lives — its shader-engine count is already multiplied by NUM_XCC — so
# the case takes the same source the derivation does.
"${CHECK}" --device "${GPU}" --percent 100 > "${W}/topo" 2>&1
topo="$(grep -m1 '^topology ' "${W}/topo" || true)"
if [ -z "${topo}" ]; then
  row FAIL "topology read" "the probe reported no topology line: $(head -3 "${W}/topo" | tr '\n' ' ')"
  echo "FAILS=1"; exit 0
fi
xcc="$(echo "${topo}" | sed -n 's/.*[ ]xcc=\([0-9]*\).*/\1/p')"
unit="$(echo "${topo}" | sed -n 's/.*[ ]unit=\([a-z]*\).*/\1/p')"
units="$(echo "${topo}" | sed -n 's/.*[ ]units=\([0-9]*\).*/\1/p')"
row INFO "topology read" "${topo#topology }"

# Two tables, chosen by NUM_XCC. Positive rows: "<percent> <expected CU list>". Negative rows:
# "<env assignment> <expected units occupied> <why it fails>" — the occupancy figure matters
# because "the probe failed" is not the claim; the claim is that it failed for THIS reason, and
# every one of these constructions fails by handing the container the whole card.
if [ "${xcc}" -eq 1 ]; then
  table=A
  positives='10 0-5
20 0-11
25 0-11
50 0-29
75 0-41
100 0-59'
  negatives="HSA_CU_MASK=%D%:0-14 ${units} 15 CUs splits WGP 7; an orphaned CU invalidates the whole set
ROC_GLOBAL_CU_MASK=0xC0000000 ${units} every set bit sits at or above the WGP width, so all are ignored
HSA_CU_MASK=GPU-b3a1f0d2c4e5:0-29 ${units} a GPU_list that is not a decimal index is dropped, segment and all"
  # The control for the ROC_GLOBAL row: without it, a probe that simply never accepted that
  # variable would pass the negative and prove nothing.
  control='ROC_GLOBAL_CU_MASK=0x7fff'
else
  table=B
  positives='3 0-7
5 0-7
6 0-15
11 0-31
50 0-151
100 0-303'
  negatives="HSA_CU_MASK=%D%:0 267 one bit reaches one XCC; the other seven receive no mask and run unmasked
HSA_CU_MASK=%D%:0-3 156 four bits reach four XCCs; the other four run unmasked
HSA_CU_MASK=%D%:0,8,16,24 270 all four bits are congruent to 0 mod 8, so they all land on XCC 0
HSA_CU_MASK=%D%:304-400 ${units} every bit is at or above the CU count
HSA_CU_MASK=GPU-b3a1f0d2c4e5:0-151 ${units} a GPU_list that is not a decimal index is dropped whole"
  control=''
fi
row INFO "conformance table" "NUM_XCC=${xcc} selects table ${table}; the unit compared is the ${unit}, ${units} of them"

# --- positive rows: the derivation reproduces the table, and the card agrees ---
while read -r pct culist; do
  [ -n "${pct}" ] || continue
  want="${GPU}:${culist}"
  "${CHECK}" --device "${GPU}" --percent "${pct}" > "${W}/p${pct}" 2>&1
  rc=$?
  got="$(sed -n 's/^derive .*mask=\(.*\)$/\1/p' "${W}/p${pct}" | tail -1)"
  if [ "${rc}" -ne 0 ]; then
    row FAIL "table ${table} · ${pct}%" "the probe rejected its own mask ${got:-<none>}: $(grep -m2 '^FAIL' "${W}/p${pct}" | tr '\n' ' ')"
    fails=$((fails+1))
  elif [ "${got}" != "${want}" ]; then
    row FAIL "table ${table} · ${pct}%" "derived ${got:-<none>}, table says ${want}"
    fails=$((fails+1))
  else
    row PASS "table ${table} · ${pct}%" "${got} — $(grep -c '^PASS' "${W}/p${pct}") rules held, none broken"
  fi
done <<EOF
${positives}
EOF

# A sub-atom request must be REFUSED rather than rounded up, and only on CDNA: below one atom
# there is no mask that covers every XCC, and one that covers some is worse than none. RDNA has
# no such floor — it clamps up to the shader-engine count — so the row would be wrong there.
if [ "${xcc}" -gt 1 ]; then
  "${CHECK}" --device "${GPU}" --percent 1 > "${W}/sub" 2>&1
  rc=$?
  if [ "${rc}" -eq 2 ]; then
    row PASS "table ${table} · sub-atom refused" "--percent 1 exits 2 rather than clamping into a partial-XCC mask"
  else
    row FAIL "table ${table} · sub-atom refused" "--percent 1 exited ${rc}, expected 2: $(grep -m1 'mask=' "${W}/sub" || echo 'no mask line')"
    fails=$((fails+1))
  fi
fi

# --- negative rows: each fail-open construction is still detected, and for its own reason ---
while read -r spec occ why; do
  [ -n "${spec}" ] || continue
  spec="${spec//%D%/${GPU}}"
  name="$(echo "${spec}" | cut -d= -f1)=$(echo "${spec}" | cut -d= -f2-)"
  env "${spec}" "${CHECK}" --device "${GPU}" > "${W}/n" 2>&1
  rc=$?
  seen="$(sed -n 's/.*occupancy\/units_match | [A-Z]*: masked [0-9]*, occupied \([0-9]*\).*/\1/p' "${W}/n" | tail -1)"
  if [ "${rc}" -eq 0 ]; then
    row FAIL "fail-open · ${name}" "the probe ACCEPTED a mask that does not take effect — ${why}"
    fails=$((fails+1))
  elif [ "${seen}" != "${occ}" ]; then
    row FAIL "fail-open · ${name}" "detected, but occupied ${seen:-<unreported>} where the table says ${occ} — ${why}"
    fails=$((fails+1))
  else
    row PASS "fail-open · ${name}" "rejected; occupied ${seen} of ${units} ${unit}s — ${why}"
  fi
done <<EOF
${negatives}
EOF

# The control arm. A negative row only means something if the same mechanism accepts a VALID
# value, otherwise "always rejected" would score the same.
if [ -n "${control}" ]; then
  env "${control}" "${CHECK}" --device "${GPU}" > "${W}/c" 2>&1
  rc=$?
  seen="$(sed -n 's/.*occupancy\/units_match | [A-Z]*: masked \([0-9]*\).*/\1/p' "${W}/c" | tail -1)"
  if [ "${rc}" -eq 0 ] && [ -n "${seen}" ] && [ "${seen}" -lt "${units}" ]; then
    row PASS "control · ${control}" "a valid bitmask is accepted and confines to ${seen} of ${units} ${unit}s"
  else
    row FAIL "control · ${control}" "exit ${rc}, masked ${seen:-<none>} of ${units}; the negative row above shows nothing if no value is ever accepted"
    fails=$((fails+1))
  fi
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
# The verdict is the payload's own count, read as a NUMBER off the last `FAILS=` line. Matching
# the token anywhere in the output would let any row satisfy the verdict by printing it in a
# detail column, and a payload that died before printing the line has to read as failure.
total="$(echo "${out}" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | tail -1)"
[ "${total:-1}" -eq 0 ] && { echo "AMD-CASE 4: PASS"; exit 0; } || { echo "AMD-CASE 4: FAIL"; exit 1; }
