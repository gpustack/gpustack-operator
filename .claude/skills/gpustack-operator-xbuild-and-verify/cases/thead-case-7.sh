#!/usr/bin/env bash
#
# THEAD-CASE 7 — Gate 4: compute throttling   (needs a real PPU)
#
#   thead-case-7.sh
#
# Asks whether the compute cap is a cap. Memory can be judged from one allocation; compute
# cannot be judged at all without a workload that would otherwise take the whole card, so this
# case builds one — csrc/thead/ppu-slicing-shim/testing/hggc_launch_load.cu, a kernel launched
# back to back for a fixed wall-clock time — and reads the utilisation that workload was
# MEASURED using, from its own per-process figure rather than a card total. A card total cannot
# tell one container's share from its neighbour's, so it would pass whether the cap held or not.
#
# Five observations, because no single one of them is enough:
#   1. uncapped, the same load pins the card             (so a low figure later is the cap's doing
#                                                        and not the workload's)
#   2. capped, it settles near the limit                 (the cap holds, and does not starve)
#   3. a launch counter moved                            (the only evidence the launch crossed
#                                                        libhggc.so rather than being throttled
#                                                        somewhere else)
#   4. the loop's own state is readable from the region  (the gains are not fitted to this
#                                                        hardware, so a loop nobody can observe
#                                                        cannot be tuned on it)
#   5. two capped containers on one card each keep their own share
#                                                        (the risk a card-total feedback signal
#                                                        would carry: one container's load squeezes
#                                                        the other's controller to nothing)
# plus a sixth row for the flip this task carries: a container with no compute figure at all is
# REFUSED rather than run with compute uncapped, which is what it was before the controller
# existed;
# and two more for the cap being PER CARD: one container holding two cards at two different
#   caps keeps each card to its own (a shim reading one container-wide figure cannot), and both
#   figures are in the region at their own card's offset rather than one number copied twice.
#
# Observation 1 injects the shim with the limit at 100 rather than leaving it out. That is the
# sharper control: it makes the cap the only difference between the two runs, and it exercises the
# path where a configured-but-uncapping figure must leave the launch path alone.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu, on the
#      TARGET), XB_PPU_CARDS (default: the idle cards — the single-card rows take the first, the
#      two-card arm the first two), XB_PPU_CARD (default: the first of them), XB_PPU_IDLE_MIB
#      (default 64), XB_PPU_QUOTA_MIB (default 4096), XB_PPU_SM_LIMIT (default 25),
#      XB_PPU_SM_LIMIT_A / XB_PPU_SM_LIMIT_B (default 50 / 25, the two-card arm's caps, A above B),
#      XB_PPU_LOAD_SECONDS (default 20), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL. With no PPU every hardware
# row is SKIP and the case still exits 0.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"
xctr_resolve || { echo "thead-case-7: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 7 — Gate 4 compute throttling (image ${XB_IMAGE}) on $(xtarget_desc)"

CARDS="${XB_PPU_CARDS:-$(thead_idle_cards | tr '\n' ' ')}"
CARD="${XB_PPU_CARD:-$(echo "${CARDS}" | awk '{print $1}')}"
CARD2="$(echo "${CARDS}" | awk '{print $2}')"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARD="${CARD}" CARD2="${CARD2}" QUOTA="${XB_PPU_QUOTA_MIB:-4096}" \
  LIMIT="${XB_PPU_SM_LIMIT:-25}" SM_A="${XB_PPU_SM_LIMIT_A:-50}" SM_B="${XB_PPU_SM_LIMIT_B:-25}" \
  SECONDS_="${XB_PPU_LOAD_SECONDS:-20}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

SHIM="${STAGE}/hggc_quota.so"
if [ ! -f "${SHIM}" ]; then
  row FAIL "shim staged" "${SHIM} missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi
row INFO "shim staged" "hggc_quota sha256=$(sha256sum "${SHIM}" | cut -c1-16)…"

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
CTR_RUN="${XB_CTR} ${XB_CTR_ARGS} run --rm --platform linux/amd64 -v ${STAGE}:/work -w /work"

# The load is compiled by the vendor's own device compiler — a kernel is the point, and there is
# no way to occupy a PPU from plain C — which the build step does along with everything else here.
if [ ! -x "${STAGE}/hggc_launch_load" ]; then
  row FAIL "load staged" "${STAGE}/hggc_launch_load missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi
row INFO "load staged" "hggc_launch_load sha256=$(sha256sum "${STAGE}/hggc_launch_load" | cut -c1-16)…"

DUAL_ROWS="two cards in one container keep their own compute caps
both cards' compute caps are in the region"

skip_dual() {
  echo "${DUAL_ROWS}" | while IFS= read -r r; do row SKIP "${r}" "$1"; done
}

skip_all() {
  row SKIP "uncapped load pins the card" "$1"
  row SKIP "capped load settles near the limit" "$1"
  row SKIP "crossed libhggc.so" "$1"
  row SKIP "the loop state is readable from the region" "$1"
  row SKIP "two containers keep their own share" "$1"
  row SKIP "no compute figure refuses the container" "$1"
  skip_dual "$1"
  echo "FAILS=${fails}"
  exit 0
}

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  skip_all "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
fi
[ -n "${CARD}" ] || skip_all "no idle card (every card at or above XB_PPU_IDLE_MIB, or non-zero util)"
[ -e "/dev/alixpu_ppu${CARD}" ] || skip_all "/dev/alixpu_ppu${CARD} absent for the chosen card"

DEV="--device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu${CARD}"

# load <ledger-suffix> <sm-limit|none> [seconds] — one run of the workload with the shim
# injected. The device index is a literal 0: one card node is mounted and the SDK renumbers what
# it sees inside the container, so the host ordinal names the node while the container addresses
# it as 0.
#
# The ledger goes under /work rather than /dev/shm on purpose — /dev/shm is per container, and
# the region has to outlive the run for the state row below to read it.
load() {
  local suffix="$1" limit="$2" secs="${3:-${SECONDS_}}" env_args
  env_args="-e LD_PRELOAD=/work/hggc_quota.so -e HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}"
  env_args="${env_args} -e LIBHGGC_LOG_LEVEL=2 -e HGGC_LEDGER_PATH=/work/ledger-case7-${suffix}"
  if [ "${limit}" != none ]; then
    env_args="${env_args} -e HGGC_DEVICE_SM_LIMIT=${limit}"
  fi
  rm -f "${STAGE}/ledger-case7-${suffix}"
  # shellcheck disable=SC2086
  ${CTR_RUN} ${DEV} ${env_args} "${IMG}" \
    timeout $((secs + 60)) ./hggc_launch_load 0 "${secs}" 2>&1
}

load_field() { echo "$1" | sed -nE "s/^LOAD .*$2=([0-9]+).*/\1/p" | tail -1; }
load_result() { echo "$1" | sed -nE 's/^LOAD result=([a-z]+).*/\1/p' | tail -1; }

# counters_moved — did any interposed LAUNCH entry get called in this run? Every counter line is
# considered, not the last: the preload reaches every process in the container, so the `timeout`
# wrapper prints an all-zero dump of its own and it exits last.
launch_counter() {
  echo "$1" | sed -nE 's/.*hggc_quota counters:(.*)/\1/p' \
    | tr ' ' '\n' | grep -E '^hg(Launch|GraphLaunch)[A-Za-z_0-9]*=[0-9]+$' | grep -vE '=0$' \
    | head -1
}

uncapped_out="$(load uncapped 100)"
uncapped_mean="$(load_field "${uncapped_out}" sm_util_mean)"
if [ "$(load_result "${uncapped_out}")" = success ] && [ "${uncapped_mean:-0}" -ge 60 ]; then
  row PASS "uncapped load pins the card" \
    "${uncapped_mean}% mean smUtil at $(load_field "${uncapped_out}" rate_per_s) launches/s with the limit at 100"
else
  row FAIL "uncapped load pins the card" \
    "mean smUtil ${uncapped_mean:-unread}% — a capped figure below it would prove nothing: $(echo "${uncapped_out}" | grep -E '^LOAD ' | tr '\n' ' ' | cut -c1-220)"
  fails=$((fails+1))
fi

capped_out="$(load capped "${LIMIT}")"
capped_mean="$(load_field "${capped_out}" sm_util_mean)"
# Bounded on BOTH sides, and the lower bound is not slack. Above the cap the throttle does not
# hold; well below it the container is being starved rather than capped, which is its own defect
# and the one a loose floor hides — a card-total feedback signal, for instance, settles a
# container at a fraction of its cap and would sail through a floor of "anything non-zero".
ceiling=$((LIMIT + 15))
floor=$((LIMIT > 8 ? LIMIT - 8 : 1))
if [ "$(load_result "${capped_out}")" = success ] \
   && [ "${capped_mean:-100}" -le "${ceiling}" ] && [ "${capped_mean:-0}" -ge "${floor}" ] \
   && [ $((${uncapped_mean:-0} - ${capped_mean:-0})) -ge 25 ]; then
  row PASS "capped load settles near the limit" \
    "${capped_mean}% mean smUtil against a ${LIMIT}% cap (uncapped ${uncapped_mean}%), $(load_field "${capped_out}" rate_per_s) launches/s"
else
  row FAIL "capped load settles near the limit" \
    "${capped_mean:-unread}% mean smUtil against a ${LIMIT}% cap, uncapped ${uncapped_mean:-unread}% — wanted ${floor}..${ceiling}% and at least 25 points below uncapped"
  fails=$((fails+1))
fi

counter="$(launch_counter "${capped_out}")"
if [ -n "${counter}" ]; then
  row PASS "crossed libhggc.so" "${counter} — the runtime's launch funnels into the interposed driver entry"
else
  row FAIL "crossed libhggc.so" "no launch counter moved, so the throttling above cannot be attributed to this shim"
  fails=$((fails+1))
fi

# The region read the way tools/ and a scraper will read it: by documented offset out of the raw
# file, never through the library's struct. Card 0's slot opens at 96; within it the compute limit
# is at +16, the measured utilisation at +20, and the controller's four words at +32.
state="$(${CTR_RUN} "${IMG}" bash -c '
  f=/work/ledger-case7-capped
  [ -f "${f}" ] || { echo "MISSING"; exit 0; }
  printf "magic=%s limit=%s util=%s window=%s allow=%s step=%s integral=%s error=%s\n" \
    "$(dd if=${f} bs=1 count=8 2>/dev/null)" \
    "$(od -An -tu4 -j112 -N4 ${f} | tr -d " ")" \
    "$(od -An -tu4 -j116 -N4 ${f} | tr -d " ")" \
    "$(od -An -tu8 -j128 -N8 ${f} | tr -d " ")" \
    "$(od -An -tu8 -j136 -N8 ${f} | tr -d " ")" \
    "$(od -An -tu8 -j144 -N8 ${f} | tr -d " ")" \
    "$(od -An -td4 -j152 -N4 ${f} | tr -d " ")" \
    "$(od -An -td4 -j156 -N4 ${f} | tr -d " ")"' 2>&1)"
state_limit="$(echo "${state}" | sed -nE 's/.*limit=([0-9]+).*/\1/p')"
state_allow="$(echo "${state}" | sed -nE 's/.*allow=([0-9]+).*/\1/p')"
if echo "${state}" | grep -q 'magic=VPPUREGN' && [ "${state_limit:-0}" = "${LIMIT}" ] \
   && [ "${state_allow:-0}" -gt 0 ]; then
  row PASS "the loop state is readable from the region" "${state}"
else
  row FAIL "the loop state is readable from the region" \
    "wanted the magic, limit=${LIMIT} and a non-zero allowance, got: ${state}"
  fails=$((fails+1))
fi

# Two capped containers on ONE card, at the same time. Each has its own region — /dev/shm is per
# container and these ledgers are per run — so each runs its own loop against its own share. A
# controller fed a CARD TOTAL would see its neighbour's load as its own and squeeze itself to
# nothing, which is the failure this row exists to catch.
load a "${LIMIT}" > "${STAGE}/case7-a.log" 2>&1 &
pid_a=$!
load b "${LIMIT}" > "${STAGE}/case7-b.log" 2>&1 &
pid_b=$!
wait "${pid_a}"; wait "${pid_b}"
mean_a="$(load_field "$(cat "${STAGE}/case7-a.log")" sm_util_mean)"
mean_b="$(load_field "$(cat "${STAGE}/case7-b.log")" sm_util_mean)"
# Each container must keep its OWN share, near its own cap — not a fraction of it. Two caps of
# 25% ask for half a card between them, so there is nothing here for them to contend over and
# neither has a reason to settle low. A floor of "anything non-zero" would have passed the very
# defect this row exists to catch: fed a card total instead of its own share, each loop reads its
# neighbour's load as its own and settles at about half its cap (measured: 13% and 11%).
neighbour_ceiling=$((LIMIT + 20))
neighbour_floor=$((LIMIT > 8 ? LIMIT - 8 : 1))
if [ "${mean_a:-0}" -ge "${neighbour_floor}" ] && [ "${mean_b:-0}" -ge "${neighbour_floor}" ] \
   && [ "${mean_a:-100}" -le "${neighbour_ceiling}" ] && [ "${mean_b:-100}" -le "${neighbour_ceiling}" ]; then
  row PASS "two containers keep their own share" \
    "${mean_a}% and ${mean_b}% mean smUtil, each against its own ${LIMIT}% cap on card ${CARD}"
else
  row FAIL "two containers keep their own share" \
    "${mean_a:-unread}% and ${mean_b:-unread}% mean smUtil against ${LIMIT}% caps — wanted both in ${neighbour_floor}..${neighbour_ceiling}%, so neither squeezed by its neighbour nor over its own cap"
  fails=$((fails+1))
fi

# The flip this task carries. Before the controller existed a missing compute figure was reported
# and nothing more, because refusing over a dimension the library did not implement would have
# failed closed on the wrong thing. Now it is a refusal, and it has to be visible as one: the
# alternative — a sliced container running with compute uncapped — is flexai's missing-config
# outcome, which this design forbids outright.
none_out="$(load none none 3)"
if [ "$(load_result "${none_out}")" = failed ] && echo "${none_out}" | grep -q 'DENIED'; then
  row PASS "no compute figure refuses the container" \
    "$(echo "${none_out}" | grep -o 'DENIED .*' | head -1 | cut -c1-160)"
elif [ "$(load_result "${none_out}")" = failed ]; then
  row FAIL "no compute figure refuses the container" \
    "refused, but without the shim's DENIED marker — the refusal may not be ours"
  fails=$((fails+1))
else
  row FAIL "no compute figure refuses the container" \
    "the load ran with no HGGC_DEVICE_SM_LIMIT configured: $(echo "${none_out}" | grep -E '^LOAD ' | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

# ---------------------------------------------------------------------------------------
# One container, two cards, a different compute cap on each
# ---------------------------------------------------------------------------------------
# What memory has had since case 6 Part C and compute did not: a figure per card,
# HGGC_DEVICE_SM_LIMIT_<i>. The rows above all inject the un-indexed figure, so a shim reading one
# container-wide number passes every one of them — a container holding two cards could be given
# half of one and a quarter of the other, and nothing here would have noticed.
#
# Two loads at once in ONE container, one per card index, each judged against ITS OWN band. The
# indexed figures are injected WITHOUT the un-indexed one on purpose: a shim that ignores the index
# then finds no figure at all and refuses the container outright, which fails this row loudly
# rather than quietly running both cards at one cap.
#
# The bands are the ones the single-card rows use, both sides bounded for the same reason: over the
# cap the throttle does not hold, well under it the card is being starved rather than capped.
section_of() { echo "$2" | awk -v m="--- $1 ---" '$0==m{on=1;next} /^--- /{on=0} on'; }

if [ -z "${CARD2}" ]; then
  skip_dual "only one idle card — this arm needs two, like case 5"
elif [ ! -e "/dev/alixpu_ppu${CARD2}" ]; then
  skip_dual "/dev/alixpu_ppu${CARD2} absent for the second chosen card"
elif [ "${SM_A}" -le "${SM_B}" ]; then
  skip_dual "XB_PPU_SM_LIMIT_A (${SM_A}) must be above XB_PPU_SM_LIMIT_B (${SM_B}) — the rows turn on the two caps differing"
else
  DEV2="--device /dev/alixpu --device /dev/alixpu_ctl \
--device /dev/alixpu_ppu${CARD} --device /dev/alixpu_ppu${CARD2}"
  rm -f "${STAGE}/ledger-case7-dual"

  # One region for both cards, because it is one container: the shim keeps a window per card in it,
  # and the loads run at the same time so each window has to hold while the other is open.
  # shellcheck disable=SC2086
  dual_out="$(${CTR_RUN} ${DEV2} \
    -e LD_PRELOAD=/work/hggc_quota.so \
    -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" -e "HGGC_DEVICE_MEMORY_LIMIT_1=${QUOTA}" \
    -e "HGGC_DEVICE_SM_LIMIT_0=${SM_A}" -e "HGGC_DEVICE_SM_LIMIT_1=${SM_B}" \
    -e LIBHGGC_LOG_LEVEL=2 -e HGGC_LEDGER_PATH=/work/ledger-case7-dual \
    "${IMG}" timeout $((SECONDS_ + 90)) bash -c "
      ./hggc_launch_load 0 ${SECONDS_} > /work/case7-dual-0.log 2>&1 &
      p0=\$!
      ./hggc_launch_load 1 ${SECONDS_} > /work/case7-dual-1.log 2>&1 &
      p1=\$!
      wait \${p0}; wait \${p1}
      echo '--- CARD0 ---'; cat /work/case7-dual-0.log
      echo '--- CARD1 ---'; cat /work/case7-dual-1.log" 2>&1)"

  out0="$(section_of CARD0 "${dual_out}")"
  out1="$(section_of CARD1 "${dual_out}")"
  mean0="$(load_field "${out0}" sm_util_mean)"
  mean1="$(load_field "${out1}" sm_util_mean)"
  ceil0=$((SM_A + 15)); floor0=$((SM_A > 8 ? SM_A - 8 : 1))
  ceil1=$((SM_B + 15)); floor1=$((SM_B > 8 ? SM_B - 8 : 1))
  if [ "$(load_result "${out0}")" = success ] && [ "$(load_result "${out1}")" = success ] \
     && [ "${mean0:-100}" -le "${ceil0}" ] && [ "${mean0:-0}" -ge "${floor0}" ] \
     && [ "${mean1:-100}" -le "${ceil1}" ] && [ "${mean1:-0}" -ge "${floor1}" ]; then
    row PASS "two cards in one container keep their own compute caps" \
      "card 0 ${mean0}% against ${SM_A}%, card 1 ${mean1}% against ${SM_B}%, both loads at once"
  else
    row FAIL "two cards in one container keep their own compute caps" \
      "card 0 ${mean0:-unread}% (wanted ${floor0}..${ceil0}), card 1 ${mean1:-unread}% (wanted ${floor1}..${ceil1}): $(echo "${dual_out}" | grep -E '^LOAD |DENIED' | tr '\n' ' ' | cut -c1-260)"
    fails=$((fails+1))
  fi

  # And the region itself, by documented offset: the compute limit sits at +16 within a card's slot,
  # the slots are 576 bytes apart and the table opens at 96 — so card 0's figure is at 112 and card
  # 1's at 688. Two DIFFERENT figures there is the proof the shim keyed them per card rather than
  # writing one number into every slot it touched.
  dual_state="$(${CTR_RUN} "${IMG}" bash -c '
    f=/work/ledger-case7-dual
    [ -f "${f}" ] || { echo "MISSING"; exit 0; }
    printf "limit0=%s util0=%s limit1=%s util1=%s\n" \
      "$(od -An -tu4 -j112 -N4 ${f} | tr -d " ")" \
      "$(od -An -tu4 -j116 -N4 ${f} | tr -d " ")" \
      "$(od -An -tu4 -j688 -N4 ${f} | tr -d " ")" \
      "$(od -An -tu4 -j692 -N4 ${f} | tr -d " ")"' 2>&1)"
  if [ "$(echo "${dual_state}" | sed -nE 's/.*limit0=([0-9]+).*/\1/p')" = "${SM_A}" ] \
     && [ "$(echo "${dual_state}" | sed -nE 's/.*limit1=([0-9]+).*/\1/p')" = "${SM_B}" ]; then
    row PASS "both cards' compute caps are in the region" "${dual_state}"
  else
    row FAIL "both cards' compute caps are in the region" \
      "wanted limit0=${SM_A} and limit1=${SM_B} at offsets 112 and 688, got: ${dual_state}"
    fails=$((fails+1))
  fi
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "THEAD-CASE 7" "$(xb_fails "${out}")"
