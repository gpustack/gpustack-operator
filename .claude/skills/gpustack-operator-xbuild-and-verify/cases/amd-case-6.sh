#!/usr/bin/env bash
#
# AMD-CASE 6 — common/ unit tests, one quota across two processes, one container across two cards
#
#   amd-case-6.sh          (part A needs no GPU; parts B and C do, and C needs TWO cards)
#
# Three parts, because the quota has three properties no single-process arm can observe.
#
# PART A runs `common/`'s unit tests and relays their rows. They link **no ROCm at all** — that is
# the point of the rule that `common/` names no `hip*`/`hsa*` type — so they run wherever a C
# compiler ran, with no device and no container. Their own `FAILS=` line is folded into this case's
# count and NOT relayed: the verdict reads the LAST `FAILS=` line a part printed, so a relayed one
# would stand in for this case's own count.
#
# PART B puts two processes in one container against one quota. The holder charges the ledger
# DIRECTLY rather than through `hipMalloc`, and that is deliberate rather than convenient: it links
# no ROCm and therefore occupies no real VRAM, so when the second process is refused, the card
# still has tens of GiB free and **only our accounting can explain the refusal**. A holder that
# took real memory would leave that ambiguous. What makes the composition sound is AMD-CASE 3,
# which is where a `hipMalloc` is shown landing in this same region.
#
# The figures are then read TWO WAYS and required to agree: the library's own denial line, and
# `rocm-monitor`, which is not preloaded and reads the region file itself. The second is the one
# that matters — it is the path a metrics scraper takes, and a reader that only ever agreed with
# the struct it was compiled against would say nothing about that contract.
#
# PART C is the only place **per-card keying** can be observed. Every other row in the whole suite
# gives a container one card, so a shim that ignored the card index and charged one container-wide
# figure would pass all of them. One container holds two cards with two different quotas and a size
# BETWEEN them is asked for on each: refused on the smaller naming that card's own figure, served
# on the larger. One number cannot answer both ways.
#
# WHY THE REFUND ROW IS INSIDE ONE PROCESS. A cross-process form of it would be vacuous, and the
# reason is worth stating because it is not obvious: a charge left behind by a process that has
# exited is reclaimed by the sweep the moment the next admission would not otherwise fit, so a run
# that leaked its refund and one that did not are indistinguishable from outside. The airtight
# observation is the pair of reports either side of a release in the SAME live process, taken while
# a second process holds a charge that must not move.
#
# Assumes `scripts/build.sh xbuild-amd-rocm` already staged and compiled the tree.
#
# Env: XB_IMAGE (default rocm/dev-ubuntu-22.04:7.2.4), XB_STAGE (default /tmp/vrocm, on the
#      TARGET), XB_AMD_GPU (0), XB_AMD_GPUS (`0,1`, part C), XB_AMD_QUOTA_MIB (4096),
#      XB_AMD_QUOTA_A_MIB (2048) / XB_AMD_QUOTA_B_MIB (6144) for part C — A must be the smaller,
#      since the rows turn on a size midway between the two.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
XB_STAGE="${XB_STAGE:-/tmp/vrocm}"
XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"

echo "# AMD-CASE 6 — unit tests, one quota two processes, one container two cards on $(xtarget_desc)"

# ---- Part A: no container, no device -------------------------------------------------------
partA="$(xsh STAGE="${XB_STAGE}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

if [ ! -x "${STAGE}/vrocm_test" ]; then
  row FAIL "A: unit tests staged" "${STAGE}/vrocm_test missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi

# Relayed row by row, and the suite's own FAILS= line dropped: the verdict reads the LAST `FAILS=`
# line in this part's output, so relaying it would put the unit suite's count there instead.
"${STAGE}/vrocm_test" > /tmp/.amd-case-6-unit 2>&1
rc=$?
grep -E '^(PASS|FAIL|SKIP|INFO) \| ' /tmp/.amd-case-6-unit | sed 's/^/A: /' |
  sed -e 's/^A: PASS | /PASS | A: /' -e 's/^A: FAIL | /FAIL | A: /' \
      -e 's/^A: SKIP | /SKIP | A: /' -e 's/^A: INFO | /INFO | A: /'
unit_fails="$(sed -n 's/^FAILS=\([0-9]*\)$/\1/p' /tmp/.amd-case-6-unit | tail -1)"
if [ "${rc}" -eq 0 ] && [ "${unit_fails:-1}" -eq 0 ]; then
  row PASS "A: common/ unit suite" "$(grep -c '^PASS | ' /tmp/.amd-case-6-unit) cases, no ROCm linked and no device"
else
  row FAIL "A: common/ unit suite" "exit ${rc}, FAILS=${unit_fails:-<none>}"
  fails=$((fails + ${unit_fails:-1}))
fi
rm -f /tmp/.amd-case-6-unit
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${partA}" | grep -v '^FAILS='

# ---- Parts B and C: in a container, on the card(s) -----------------------------------------
if ! xctr_resolve; then
  echo "SKIP | B/C: injection | no container runtime on $(xtarget_desc)"
  bc="FAILS=0"
else
  bc="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" STAGE="${XB_STAGE}" \
             PLATFORM="${XB_PLATFORM}" GPU="${XB_AMD_GPU:-0}" GPUS="${XB_AMD_GPUS:-0,1}" \
             QUOTA="${XB_AMD_QUOTA_MIB:-4096}" QA="${XB_AMD_QUOTA_A_MIB:-2048}" \
             QB="${XB_AMD_QUOTA_B_MIB:-6144}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }

for f in libvrocm.so hip_mem_paths ledger_lifecycle rocm-monitor; do
  [ -e "${STAGE}/${f}" ] && continue
  row FAIL "B/C: artifacts staged" "${STAGE}/${f} missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
done
if [ ! -e /dev/kfd ]; then
  row SKIP "B/C: injection" "/dev/kfd absent — no AMD GPU on this target"
  echo "FAILS=0"; exit 0
fi

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
  --device /dev/kfd --device /dev/dri --group-add video --group-add render \
  --security-opt seccomp=unconfined \
  -e "GPU=${GPU}" -e "GPUS=${GPUS}" -e "QUOTA=${QUOTA}" -e "QA=${QA}" -e "QB=${QB}" \
  -v "${STAGE}:/work" -w /work "${IMG}" bash -s <<'INNER'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
W="$(mktemp -d)"

# -------------------------------------------------------------------------------------------
# Part B — two processes, one quota
# -------------------------------------------------------------------------------------------
LEDGER="${W}/ledger.B"
HOLD=$(( QUOTA * 3 / 4 ))

# The holder takes three quarters and stays alive. It links no ROCm, so the card's own free memory
# is untouched and the refusal below can only be ours.
env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
    /work/ledger_lifecycle hold 0 "${HOLD}" > "${W}/hold.log" 2>&1 &
holder=$!
for _ in $(seq 50); do grep -q '^HOLD ready' "${W}/hold.log" && break; sleep 0.1; done
if ! grep -q '^HOLD ready' "${W}/hold.log"; then
  row FAIL "B: a first process holds ${HOLD} MiB" "$(tail -2 "${W}/hold.log" | tr '\n' ' ')"
  kill "${holder}" 2>/dev/null
  echo "FAILS=1"; exit 0
fi
row PASS "B: a first process holds ${HOLD} MiB of a ${QUOTA} MiB quota" \
  "$(grep -m1 '^HOLD ready' "${W}/hold.log")"

# The second process asks through HIP for a size that fits the card many times over and does not
# fit what is left of the quota.
ASK=$(( QUOTA / 2 ))
echo /work/libvrocm.so > /etc/ld.so.preload
env "ROCR_VISIBLE_DEVICES=${GPU}" "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
    "VROCM_LEDGER_PATH=${LEDGER}" LIBVROCM_LOG_LEVEL=2 \
    /work/hip_mem_paths plain "${ASK}" 0 > "${W}/ask.log" 2>&1
rm -f /etc/ld.so.preload 2>/dev/null

deny="$(grep -m1 'bytes refused' "${W}/ask.log" || true)"
if grep -q '^PATH plain result=failed rc=2' "${W}/ask.log" && [ -n "${deny}" ]; then
  row PASS "B: a second process is refused ${ASK} MiB" "${deny#\[vrocm\] }"
else
  row FAIL "B: a second process is refused ${ASK} MiB" \
    "$(grep -E '^PATH plain result' "${W}/ask.log" | tr '\n' ' ')${deny}"
  fails=$((fails+1))
fi

# The denial names bytes; the reader names mebibytes. Both must describe the same card.
deny_used="$(echo "${deny}" | sed -n 's/.*, \([0-9]*\) of [0-9]* already held.*/\1/p')"
deny_quota="$(echo "${deny}" | sed -n 's/.*, [0-9]* of \([0-9]*\) already held.*/\1/p')"
mon="$(/work/rocm-monitor "${LEDGER}" 2>&1)"
mon_used="$(echo "${mon}" | sed -n 's/^card=0 .*mem_used_mib=\([0-9]*\).*/\1/p')"
mon_quota="$(echo "${mon}" | sed -n 's/^card=0 mem_quota_mib=\([0-9]*\).*/\1/p')"

if [ -n "${deny_used}" ] && [ "$(( deny_used / 1048576 ))" = "${mon_used}" ] &&
   [ "$(( deny_quota / 1048576 ))" = "${mon_quota}" ]; then
  row PASS "B: the denial and the reader agree" \
    "used ${mon_used} MiB of ${mon_quota} MiB, read from the library's own line and from the region file"
else
  row FAIL "B: the denial and the reader agree" \
    "denial says ${deny_used:-<none>}/${deny_quota:-<none>} bytes, rocm-monitor says ${mon_used:-<none>}/${mon_quota:-<none>} MiB"
  fails=$((fails+1))
fi

# The reader must have parsed the region rather than called anything of ours: it is not preloaded,
# links no ROCm and takes the path on the command line.
if echo "${mon}" | grep -q "^region path=${LEDGER}" && echo "${mon}" | grep -q "proc pid=${holder} "; then
  row PASS "B: the reader parses the region without the shim" \
    "$(echo "${mon}" | grep -m1 '^region ')"
else
  row FAIL "B: the reader parses the region without the shim" "$(echo "${mon}" | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

# The refund, inside one live process and with the holder's charge required not to move. The pair
# of reports either side of the release is the assertion; see the header for why a cross-process
# form of this row would pass against a broken refund.
TAKE=$(( QUOTA / 5 ))
for attempt in 1 2 3; do
  env "VROCM_DEVICE_MEMORY_LIMIT_0=${QUOTA}" "VROCM_LEDGER_PATH=${LEDGER}" \
      /work/ledger_lifecycle acquire 0 "${TAKE}" > "${W}/acq.${attempt}" 2>&1
  rc=$?
  before="$(grep -c '^RELEASE device=0 result=ok' "${W}/acq.${attempt}")"
  last_used="$(sed -n 's/^LEDGER device=0 .*used_mib=\([0-9]*\) .*/\1/p' "${W}/acq.${attempt}" | tail -1)"
  peak_used="$(sed -n 's/^LEDGER device=0 .*used_mib=\([0-9]*\) .*/\1/p' "${W}/acq.${attempt}" | sort -n | tail -1)"
  if [ "${rc}" -eq 0 ] && [ "${before}" = 1 ] && [ "${last_used}" = "${HOLD}" ] &&
     [ "${peak_used}" = "$(( HOLD + TAKE ))" ]; then
    row PASS "B: refund ${attempt}/3 — ${TAKE} MiB taken and given back" \
      "used rose to ${peak_used} MiB and returned to the holder's ${last_used} MiB"
  else
    row FAIL "B: refund ${attempt}/3 — ${TAKE} MiB taken and given back" \
      "exit ${rc}, peak ${peak_used:-<none>} (wanted $(( HOLD + TAKE ))), final ${last_used:-<none>} (wanted ${HOLD})"
    fails=$((fails+1))
  fi
done

kill "${holder}" 2>/dev/null
wait "${holder}" 2>/dev/null

# -------------------------------------------------------------------------------------------
# Part C — one container, two cards, two different quotas
# -------------------------------------------------------------------------------------------
first="${GPUS%%,*}"
second="${GPUS##*,}"
if [ "${first}" = "${second}" ] || [ ! -e /dev/dri ]; then
  row SKIP "C: per-card keying" "XB_AMD_GPUS=${GPUS} names one card; this part needs two"
  echo "FAILS=${fails}"; exit 0
fi

count="$(ls /sys/class/kfd/kfd/topology/nodes/*/properties 2>/dev/null |
         xargs grep -l 'simd_count [1-9]' 2>/dev/null | wc -l)"
if [ "${count}" -lt 2 ]; then
  row SKIP "C: per-card keying" "this target reports ${count} GPU node(s); this part needs two"
  echo "FAILS=${fails}"; exit 0
fi

LEDGER_C="${W}/ledger.C"
# A size between the two figures: refused by the smaller, served by the larger. One
# container-wide number cannot answer both ways, which is the whole point of the part.
MID=$(( (QA + QB) / 2 ))
row INFO "C: two cards, two quotas" \
  "ROCR_VISIBLE_DEVICES=${GPUS} → container index 0 gets ${QA} MiB, index 1 gets ${QB} MiB; both asked for ${MID} MiB"

for idx in 0 1; do
  echo /work/libvrocm.so > /etc/ld.so.preload
  env "ROCR_VISIBLE_DEVICES=${GPUS}" \
      "VROCM_DEVICE_MEMORY_LIMIT_0=${QA}" "VROCM_DEVICE_MEMORY_LIMIT_1=${QB}" \
      "VROCM_LEDGER_PATH=${LEDGER_C}" LIBVROCM_LOG_LEVEL=2 \
      /work/hip_mem_paths plain "${MID}" "${idx}" > "${W}/c.${idx}" 2>&1
  rm -f /etc/ld.so.preload 2>/dev/null
done

if grep -q '^PATH plain result=failed rc=2' "${W}/c.0"; then
  want=$(( QA * 1048576 ))
  named="$(sed -n 's/^\[vrocm\] card 0: [0-9]* bytes refused, [0-9]* of \([0-9]*\) already held.*/\1/p' "${W}/c.0" | tail -1)"
  if [ "${named}" = "${want}" ]; then
    row PASS "C: the smaller card refuses and names its OWN figure" \
      "card 0 refused ${MID} MiB against ${QA} MiB — the other card's number is what a leaked ledger looks like"
  else
    row FAIL "C: the smaller card refuses and names its OWN figure" \
      "refused, but named ${named:-<nothing>} bytes rather than ${want}"
    fails=$((fails+1))
  fi
else
  row FAIL "C: the smaller card refuses ${MID} MiB" \
    "$(grep -E '^PATH plain result' "${W}/c.0" | tr '\n' ' ')"
  fails=$((fails+1))
fi

if grep -q '^PATH plain result=success' "${W}/c.1"; then
  row PASS "C: the larger card serves the same size" \
    "card 1 served ${MID} MiB against ${QB} MiB, in the same container and the same run"
else
  row FAIL "C: the larger card serves the same size" \
    "$(grep -E '^PATH plain result' "${W}/c.1" | tr '\n' ' ')"
  fails=$((fails+1))
fi

# And the region carries both cards at their own figure, which one number could not.
monc="$(/work/rocm-monitor "${LEDGER_C}" 2>&1)"
q0="$(echo "${monc}" | sed -n 's/^card=0 mem_quota_mib=\([0-9]*\).*/\1/p')"
q1="$(echo "${monc}" | sed -n 's/^card=1 mem_quota_mib=\([0-9]*\).*/\1/p')"
if [ "${q0}" = "${QA}" ] && [ "${q1}" = "${QB}" ]; then
  row PASS "C: the region carries both cards at their own figure" "card 0 ${q0} MiB, card 1 ${q1} MiB"
else
  row FAIL "C: the region carries both cards at their own figure" \
    "card 0 ${q0:-<absent>}, card 1 ${q1:-<absent>}, wanted ${QA} and ${QB}"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
INNER
PAYLOAD
)"
fi
echo "${bc}" | grep -v '^FAILS='

total=$(( $(xb_fails "${partA}") + $(xb_fails "${bc}") ))
echo "FAILS=${total}"
xb_verdict "AMD-CASE 6" "${total}"
