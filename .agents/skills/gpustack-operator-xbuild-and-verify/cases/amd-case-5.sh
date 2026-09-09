#!/usr/bin/env bash
#
# AMD-CASE 5 — compute quota semantics   (needs an AMD GPU)
#
#   amd-case-5.sh
#
# WHY THE SINGLE-TENANT CEILING COMES FIRST. Measured, a correct partition, a broken partition and
# no partition at all produce indistinguishable CONCURRENT readings — N tenants sharing one card
# each get about 1/N of it however the masks are drawn, so an aggregate says nothing about whether
# any mask took effect. The difference appears only when one tenant runs alone against an idle
# card: a 50 % mask that took effect cannot reach the unmasked figure, and one that failed open
# reaches it exactly.
#
# WHY OCCUPANCY IS ASSERTED TOO. On a multi-XCC card even the solo reading stops discriminating:
# two tenants sharing 152 CUs and two sharing none measure the same throughput, solo runs
# included. Only the count of physical units the waves actually ran on separates them, so every
# row here reads `units` alongside GFLOP/s and the multi-XCC rows are decided by it.
#
# WHY EVERY TIMED ROW USES A BARRIER AND A SATURATING KERNEL. Both were mistakes made during the
# research and both produced physically impossible numbers: without a start barrier two
# 100 %-masked tenants aggregate to 200 % of the card's peak, because neither is measuring while
# the other runs; and a latency-bound kernel under-fills a small partition, which inflates every
# overlap reading. `cumask_soak` carries both.
#
# WHY THE SPREAD-VS-PACKED ROW ASSERTS AN ORDERING AND NOT A BAND. Measured five times on
# identical inputs, two tenants on complementary halves aggregated to 97-105 % of the whole card
# four times and to 73 % once. The ordering against the packed pair held in every sample by a
# 35 % margin, so that is what is asserted; a band on the aggregate would flake about one run in
# five and teach the reader to ignore it.
#
# There is deliberately NO PyTorch arm. The saturating HIP kernel is what makes these readings
# comparable, and a framework arm would need a torch image the runtime-less multi-XCC target
# cannot run. Were one added it would have to keep warm-up outside the timed window: on this
# hardware a first 8192-square fp16 GEMM was measured spending over 400 s autotuning.
#
# WHY NO CONTAINER — see amd-case-4.sh: the subject is mask semantics, not injection.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree.
#
# Env: XB_STAGE (default /tmp/vrocm, on the TARGET), XB_AMD_GPU (default 0),
#      XB_AMD_CU_PERCENT (default 50 — the share the ceiling and sharing rows are drawn at),
#      XB_AMD_SOAK_SECONDS (default 8 — shorter measures the ramp rather than steady state).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_STAGE="${XB_STAGE:-/tmp/vrocm}"

echo "# AMD-CASE 5 — compute quota semantics on $(xtarget_desc)"

out="$(xsh STAGE="${XB_STAGE}" GPU="${XB_AMD_GPU:-0}" \
           PCT="${XB_AMD_CU_PERCENT:-50}" SECS="${XB_AMD_SOAK_SECONDS:-8}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

SOAK="${STAGE}/cumask_soak"
CHECK="${STAGE}/rocm-cumask-check"
for b in "${SOAK}" "${CHECK}"; do
  if [ ! -x "${b}" ]; then
    row FAIL "binaries staged" "${b} missing — run scripts/build.sh xbuild-amd-rocm first"
    echo "FAILS=1"; exit 0
  fi
done
row INFO "binaries staged" "cumask_soak sha256=$(sha256sum "${SOAK}" | cut -c1-16)…"

if [ ! -e /dev/kfd ]; then
  row SKIP "compute semantics" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

W="$(mktemp -d)"
trap 'rm -rf "${W}"' EXIT

"${CHECK}" --device "${GPU}" --percent 100 > "${W}/topo" 2>&1
topo="$(grep -m1 '^topology ' "${W}/topo" || true)"
if [ -z "${topo}" ]; then
  row FAIL "topology read" "the probe reported no topology line"
  echo "FAILS=1"; exit 0
fi
cu="$(echo "${topo}" | sed -n 's/.*[ ]cu=\([0-9]*\).*/\1/p')"
xcc="$(echo "${topo}" | sed -n 's/.*[ ]xcc=\([0-9]*\).*/\1/p')"
unit="$(echo "${topo}" | sed -n 's/.*[ ]unit=\([a-z]*\).*/\1/p')"
units="$(echo "${topo}" | sed -n 's/.*[ ]units=\([0-9]*\).*/\1/p')"
row INFO "topology read" "${topo#topology }"

# The mask under test is the product's OWN derivation for the requested share, not a literal
# written here: a case carrying its own masks would keep passing after the derivation changed.
SHARE="$("${CHECK}" --device "${GPU}" --percent "${PCT}" 2>/dev/null \
         | sed -n 's/^derive .*mask=\(.*\)$/\1/p' | tail -1)"
if [ -z "${SHARE}" ]; then
  row FAIL "share mask derived" "the probe derived no mask for ${PCT}%"
  echo "FAILS=1"; exit 0
fi
# Its complement — everything the share did not take, read off the share's own top index rather
# than assumed to be the card's second half, so the pair stays genuinely complementary at any
# XB_AMD_CU_PERCENT. It is whole atoms on both architectures by construction: the derivation
# emits aligned runs starting at zero, so the index after one is aligned too.
share_top="$(echo "${SHARE}" | sed -n 's/.*-\([0-9]*\)$/\1/p')"
if [ -z "${share_top}" ] || [ "${share_top}" -ge $((cu - 1)) ]; then
  row FAIL "complement mask" "the ${PCT}% share ${SHARE} leaves no complement to place a second tenant on"
  echo "FAILS=1"; exit 0
fi
COMPLEMENT="${GPU}:$((share_top + 1))-$((cu - 1))"
row INFO "masks under test" "${PCT}% share=${SHARE}, complement=${COMPLEMENT}, ${SECS}s per run"

# run_tenants <tag> <mask|none>... — start every tenant on one barrier and print
# "<gflops> <units> <xccs>" per tenant. One tenant is the same code path with a barrier of one,
# so a solo reading and a shared reading are produced by the same machinery.
run_tenants() {
  local tag="$1"; shift
  local n=$# i=0 pids="" m b="${W}/barrier.${tag}"
  rm -f "${b}"
  for m in "$@"; do
    i=$((i + 1))
    if [ "${m}" = none ]; then unset HSA_CU_MASK; else export HSA_CU_MASK="${m}"; fi
    "${SOAK}" --device "${GPU}" --seconds "${SECS}" --label "${tag}${i}" \
      --barrier "${b}" --tenants "${n}" --result "${W}/r.${tag}.${i}" >/dev/null 2>&1 &
    pids="${pids} $!"
  done
  unset HSA_CU_MASK
  # shellcheck disable=SC2086  # pids is a list on purpose
  wait ${pids} 2>/dev/null || true
  for i in $(seq 1 "${n}"); do
    [ -s "${W}/r.${tag}.${i}" ] && awk '{print $1, $4, $5}' "${W}/r.${tag}.${i}" || echo "0 0 0"
  done
}

f() { awk "BEGIN{printf \"%.1f\", $1}"; }                    # format one figure
fr() { awk "BEGIN{printf \"%.3f\", ($2+0==0)?0:($1/$2)}"; }  # ratio, guarding a zero denominator
within() { awk "BEGIN{exit !($1+0 >= $2+0 && $1+0 <= $3+0)}"; }
gt() { awk "BEGIN{exit !($1+0 > $2+0)}"; }

# --- 1. the denominator, and whether it is worth using ---
base=""
for i in 1 2 3; do base="${base} $(run_tenants "full${i}" none | awk '{print $1}')"; done
# shellcheck disable=SC2086
set -- ${base}
# Defaulted rather than read bare, and handed to awk through `-v` rather than pasted into its
# program text. `run_tenants` prints one line per tenant, so all three figures are normally there;
# a result file left holding nothing but a newline would leave a positional parameter unset, and
# under `set -u` that aborts the payload in the middle of the case rather than failing a row.
# Zero is also the right stand-in: it drives the spread to the sentinel below, so the row reports
# an unusable baseline instead of a number nobody can act on.
r1="${1:-0}" r2="${2:-0}" r3="${3:-0}"
BASE="$(awk -v a="${r1}" -v b="${r2}" -v c="${r3}" 'BEGIN{printf "%.1f", (a+b+c)/3}')"
spread="$(awk -v a="${r1}" -v b="${r2}" -v c="${r3}" 'BEGIN{
            lo=a; hi=a; if(b<lo)lo=b; if(b>hi)hi=b; if(c<lo)lo=c; if(c>hi)hi=c;
            printf "%.3f", (lo==0)?9:(hi/lo) }')"
if within "${spread}" 1.0 1.05; then
  row PASS "unmasked baseline reproducible" "$(f "${r1}") / $(f "${r2}") / $(f "${r3}") GFLOP/s — spread ${spread}x, mean ${BASE}"
else
  row FAIL "unmasked baseline reproducible" "$(f "${r1}") / $(f "${r2}") / $(f "${r3}") GFLOP/s — spread ${spread}x exceeds 1.05, every ratio below is unsound"
  fails=$((fails+1))
fi
BASE_UNITS="$(run_tenants fullu none | awk '{print $2}')"

# --- 2. the single-tenant ceiling: THE row ---
solo="$(run_tenants solo "${SHARE}")"
solo_g="$(echo "${solo}" | awk '{print $1}')"
solo_u="$(echo "${solo}" | awk '{print $2}')"
r="$(fr "${solo_g}" "${BASE}")"
# The band is wide on the upper side on purpose: real compute is SUBLINEAR in unit count, so half
# the card buys more than half the throughput — measured 0.52 on RDNA and 0.60 on CDNA at 50 %.
# What the row separates is a mask that took effect from one that failed open, which reads 1.00.
if within "${r}" 0.35 0.75; then
  row PASS "single-tenant ceiling at ${PCT}%" "$(f "${solo_g}") of ${BASE} GFLOP/s = ${r} of an idle card — a mask that failed open would read 1.000"
else
  row FAIL "single-tenant ceiling at ${PCT}%" "$(f "${solo_g}") of ${BASE} = ${r}, outside 0.35..0.75; at or near 1.0 the mask never took effect"
  fails=$((fails+1))
fi

# --- 3. occupancy tracks the mask, which is what the ceiling alone cannot show on multi-XCC ---
want_u="$(awk "BEGIN{printf \"%d\", ${units} * ${PCT} / 100}")"
if [ "${solo_u}" -lt "${BASE_UNITS}" ] && [ "${solo_u}" -gt 0 ]; then
  row PASS "occupancy tracks the mask" "${solo_u} of ${BASE_UNITS} ${unit}s occupied under ${SHARE} (about ${want_u} asked for)"
else
  row FAIL "occupancy tracks the mask" "occupied ${solo_u} of ${BASE_UNITS} ${unit}s — the waves reached the whole card"
  fails=$((fails+1))
fi

# --- 4/5/6. three tenants on ONE mask: fair, saturating, and repeatable ---
#
# "Fair" has to be read against the card's own granularity. Where the tenants time-slice one pool
# of units they come out even, but on a multi-XCC part the work is distributed per XCC, and three
# tenants over eight XCCs cannot divide evenly — the split is 3:3:2, so one tenant is expected to
# measure about a third less. Measured over four rounds on a gfx942 card the spread was 1.36-1.52
# with the aggregate stable to 0.7 % and the slow tenant rotating between rounds, which is that
# quantisation and not starvation. So the tolerance is DERIVED from the topology rather than
# widened until it passes: a fixed 1.5 would let a genuinely unfair single-XCC card through.
TENANTS=3
qexp="$(awk -v x="${xcc}" -v t="${TENANTS}" 'BEGIN{
          if (x < t) { print "1.000"; exit }
          hi = int((x + t - 1) / t); lo = int(x / t);
          printf "%.3f", (lo == 0) ? 1.0 : hi / lo }')"
qmax="$(awk -v e="${qexp}" 'BEGIN{printf "%.3f", e * 1.15}')"
mean3=""
for round in 1 2; do
  three="$(run_tenants "three${round}" "${SHARE}" "${SHARE}" "${SHARE}")"
  a="$(echo "${three}" | sed -n 1p | awk '{print $1}')"
  b="$(echo "${three}" | sed -n 2p | awk '{print $1}')"
  c="$(echo "${three}" | sed -n 3p | awk '{print $1}')"
  fair="$(awk -v a="${a}" -v b="${b}" -v c="${c}" 'BEGIN{
            lo=a;hi=a; if(b<lo)lo=b; if(b>hi)hi=b; if(c<lo)lo=c; if(c>hi)hi=c;
            printf "%.3f", (lo==0)?9:(hi/lo) }')"
  sum="$(awk "BEGIN{printf \"%.1f\", ${a}+${b}+${c}}")"
  sr="$(fr "${sum}" "${solo_g}")"
  if [ "${round}" = 1 ]; then
    if within "${fair}" 1.0 "${qmax}"; then
      row PASS "three tenants share fairly" "$(f "${a}") / $(f "${b}") / $(f "${c}") GFLOP/s — widest gap ${fair}x; ${xcc} XCC(s) across ${TENANTS} tenants quantise to ${qexp}x, tolerated to ${qmax}x"
    else
      row FAIL "three tenants share fairly" "$(f "${a}") / $(f "${b}") / $(f "${c}") — widest gap ${fair}x exceeds ${qmax}x, and ${xcc} XCC(s) across ${TENANTS} tenants only explains ${qexp}x"
      fails=$((fails+1))
    fi
    if within "${sr}" 0.80 1.20; then
      row PASS "three tenants sum to one" "${sum} GFLOP/s against a solo $(f "${solo_g}") = ${sr} — the mask is a ceiling on the SET, not per tenant"
    else
      row FAIL "three tenants sum to one" "${sum} against a solo $(f "${solo_g}") = ${sr}, outside 0.80..1.20; above it the tenants were not measuring together"
      fails=$((fails+1))
    fi
  fi
  mean3="${mean3} $(awk "BEGIN{printf \"%.1f\", (${a}+${b}+${c})/3}")"
done
# shellcheck disable=SC2086
set -- ${mean3}
rep="$(fr "$1" "$2")"
if within "${rep}" 0.90 1.10; then
  row PASS "sharing is repeatable" "per-tenant mean $(f "$1") then $(f "$2") GFLOP/s across two rounds — ratio ${rep}"
else
  row FAIL "sharing is repeatable" "per-tenant mean $(f "$1") then $(f "$2") — ratio ${rep} outside 0.90..1.10"
  fails=$((fails+1))
fi

# --- 7. spread beats packed: the observable difference between the two placements ---
pack="$(run_tenants packed "${SHARE}" "${SHARE}" | awk '{s+=$1} END{printf "%.1f", s/NR}')"
spread_pair="$(run_tenants spread "${SHARE}" "${COMPLEMENT}" | awk '{s+=$1} END{printf "%.1f", s/NR}')"
if gt "${spread_pair}" "${pack}"; then
  row PASS "spreading beats packing" "complementary halves $(f "${spread_pair}") vs the same half $(f "${pack}") GFLOP/s per tenant — this is what the allocator's disjoint-first policy buys"
else
  row FAIL "spreading beats packing" "complementary halves $(f "${spread_pair}") did not beat the same half $(f "${pack}") per tenant"
  fails=$((fails+1))
fi

# --- 8. two different caps, concurrently: each tenant is held to its OWN share ---
small="$("${CHECK}" --device "${GPU}" --percent 25 2>/dev/null | sed -n 's/^derive .*mask=\(.*\)$/\1/p' | tail -1)"
large="$("${CHECK}" --device "${GPU}" --percent 75 2>/dev/null | sed -n 's/^derive .*mask=\(.*\)$/\1/p' | tail -1)"
if [ -n "${small}" ] && [ -n "${large}" ] && [ "${small}" != "${large}" ]; then
  mixed="$(run_tenants mixed "${small}" "${large}")"
  sg="$(echo "${mixed}" | sed -n 1p | awk '{print $1}')"
  su="$(echo "${mixed}" | sed -n 1p | awk '{print $2}')"
  lg="$(echo "${mixed}" | sed -n 2p | awk '{print $1}')"
  lu="$(echo "${mixed}" | sed -n 2p | awk '{print $2}')"
  if gt "${lg}" "${sg}" && [ "${lu}" -gt "${su}" ]; then
    row PASS "unequal caps stay unequal" "25% ${small} -> $(f "${sg}") GFLOP/s on ${su} ${unit}s, 75% ${large} -> $(f "${lg}") on ${lu}"
  else
    row FAIL "unequal caps stay unequal" "25% -> $(f "${sg}")/${su}${unit}s, 75% -> $(f "${lg}")/${lu}${unit}s; the larger cap must win on both"
    fails=$((fails+1))
  fi
else
  row SKIP "unequal caps stay unequal" "25% and 75% derive the same mask on this card (${small:-none}) — no unequal pair to draw"
fi

# --- 9. multi-XCC only: disjointness is a property of the ATOMS, not of the bit sets ---
# Two "obviously disjoint" ranges of one CU each per XCC were measured occupying 156 CUs apiece
# and overlapping on 152 of them, while the ledger believed it had handed out 4 each. The
# XCC-covering pair below must occupy exactly its own atom and reach every XCC.
if [ "${xcc}" -gt 1 ]; then
  atom_a="${GPU}:0-$((xcc - 1))"
  atom_b="${GPU}:${xcc}-$((2 * xcc - 1))"
  pair="$(run_tenants atoms "${atom_a}" "${atom_b}")"
  au="$(echo "${pair}" | sed -n 1p | awk '{print $2}')"; ax="$(echo "${pair}" | sed -n 1p | awk '{print $3}')"
  bu="$(echo "${pair}" | sed -n 2p | awk '{print $2}')"; bx="$(echo "${pair}" | sed -n 2p | awk '{print $3}')"
  if [ "${au}" = "${xcc}" ] && [ "${bu}" = "${xcc}" ] && [ "${ax}" = "${xcc}" ] && [ "${bx}" = "${xcc}" ]; then
    row PASS "XCC-covering pair occupies its own atom" "${atom_a} and ${atom_b} each occupied ${au} ${unit}s across all ${xcc} XCCs"
  else
    row FAIL "XCC-covering pair occupies its own atom" "${atom_a} occupied ${au} ${unit}s on ${ax} XCCs, ${atom_b} ${bu} on ${bx}; each should be ${xcc} on ${xcc}"
    fails=$((fails+1))
  fi
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "AMD-CASE 5" "$(xb_fails "${out}")"
