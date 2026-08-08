#!/usr/bin/env bash
#
# THEAD-CASE 6 — common/ unit tests, one quota across two processes, one container across two cards
#
#   thead-case-6.sh
#
# Three parts, and only the last two need a PPU.
#
# Part A runs `common/`'s unit tests. They are the first unit tests this library has, and they
# exist because `common/` names no `hg*`/`hggc*`/`hgml*` type: the quota arithmetic, the key
# map and the region can be exercised with no SDK, no vendor library and no device, which is
# true of nothing else in this tree. The rows come from the test binary itself; this case folds
# its failure count into its own and does NOT relay its FAILS= line, because the verdict reads the
# LAST such line and a relayed one would stand in for this case's own count.
#
# Part B asks the question no single-process case can: does the quota belong to the CONTAINER?
# One container, one card, two processes against one figure — the first takes the whole quota
# and holds it, the second asks for 1MiB and must be refused with the shim's own marker. 1MiB
# is the point: the card has tens of GiB free, so a refusal that small can only be ours. With a
# process-local ledger the second process sees an empty card and is granted it, which is exactly
# what this case exists to catch. The region file's magic is read back afterwards, because the
# accounting being in a shared file is the mechanism under test.
#
# While that quota is held, Part B also reads the region three ways and requires the three to agree:
# the shim's own `DENIED` line, `tools/ppu-monitor`, and `od` at the offsets
# references/thead-usage-region.md documents. The third is the point — a reader that only ever
# agreed with the struct it was compiled against would prove nothing about the contract a scraper
# has to write against, and the compute LIMIT the monitor prints appears in no `ppu-smi` field at
# all, so this is the only place any of it is checked against something independent.
#
# Part C is the one case in this suite where **per-card** keying can be observed at all. Every other
# card row in the suite gives a container exactly one card, so a shim that ignored the card index and
# charged one container-wide figure would pass all of them — case 5's two containers included, since
# each holds a single card. Here one container holds TWO cards with two DIFFERENT figures, and a size
# between them is asked for on each: the smaller card must refuse it naming **its own** quota, the
# larger must serve it. One figure cannot produce both answers. The reader then has to show both
# cards at their own quota, which is also the first time its multi-card path meets a real region
# rather than the one case 1 writes with `dd`; and ppu-smi has to show each card its own figure,
# which is the same keying question asked of the hgml shim's handle-to-index step. A second run of
# that container then asks the same two questions of the UN-INDEXED memory figure — one card held to
# `HGGC_DEVICE_MEMORY_LIMIT`, the other still overriding it with its own `_1` — because a fallback
# nothing exercises is a fallback nobody knows works.
#
# Every run here injects the UN-INDEXED HGGC_DEVICE_SM_LIMIT=100 beside the per-card memory figures.
# The library refuses every allocation while the container's quota is incomplete, and the compute
# figure became part of that once the controller landed; 100 is configured-and-uncapping, so nothing
# this case measures changes. Un-indexed on purpose: it is the fallback arm of the compute figure's
# precedence — every card with no HGGC_DEVICE_SM_LIMIT_<i> of its own reads it — while case 7's
# two-card arm exercises the indexed form.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu, on the
#      TARGET), XB_PPU_CARDS (default: the idle cards — Part B takes the first, Part C the first two),
#      XB_PPU_CARD (override Part B's card alone), XB_PPU_IDLE_MIB (default 64),
#      XB_PPU_QUOTA_MIB (Part B, default 4096), XB_PPU_QUOTA_A_MIB / XB_PPU_QUOTA_B_MIB (Part C's two
#      figures, defaults 2048 / 6144 — A must be the smaller), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"
xctr_resolve || { echo "thead-case-6: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 6 — unit tests, one quota across two processes, one container across two cards (image ${XB_IMAGE}) on $(xtarget_desc)"

# Scanned ONCE for both halves: Part B's own container takes a card's memory while it runs, so a
# second scan afterwards could return a different pair — or one card short.
CARDS="${XB_PPU_CARDS:-$(thead_idle_cards | tr '\n' ' ')}"
CARD="${XB_PPU_CARD:-$(echo "${CARDS}" | awk '{print $1}')}"
CARD2="$(echo "${CARDS}" | awk '{print $2}')"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARD="${CARD}" CARD2="${CARD2}" QUOTA="${XB_PPU_QUOTA_MIB:-4096}" \
  QUOTA_A="${XB_PPU_QUOTA_A_MIB:-2048}" QUOTA_B="${XB_PPU_QUOTA_B_MIB:-6144}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
CTR_RUN="${XB_CTR} ${XB_CTR_ARGS} run --rm --platform linux/amd64 -v ${STAGE}:/work -w /work"

# ---------------------------------------------------------------------------------------
# Part A — common/ unit tests, no device required
# ---------------------------------------------------------------------------------------

# Rebuilt here through the tree's own build.sh rather than trusted from the build step, because
# "clean with no SDK header at all" is this case's claim and a warning does not fail a build. That
# recipe carries no -I for the SDK, which is itself the claim: if common/ needed a vendor header
# it would not be unit-testable without a device in the first place. build.sh is silent when it
# succeeds, so anything on this stream is the compiler's.
unit="$(${CTR_RUN} "${IMG}" bash -c '
  [ -x /work/build.sh ] || { echo "BUILD_FAILED /work/build.sh missing — run scripts/build.sh xbuild-thead-ppu first"; exit 0; }
  cc_out="$(/work/build.sh unit 2>&1)" || { echo "BUILD_FAILED ${cc_out}"; exit 0; }
  [ -n "${cc_out}" ] && { echo "BUILD_WARNED ${cc_out}"; exit 0; }
  echo BUILD_CLEAN
  timeout 120 /work/vppu_test 2>&1' 2>&1)"

if echo "${unit}" | grep -q '^BUILD_CLEAN$'; then
  row PASS "unit tests: build clean without any SDK header" "build.sh unit, no diagnostics"
else
  row FAIL "unit tests: build clean without any SDK header" \
    "$(echo "${unit}" | grep -E '^BUILD_(FAILED|WARNED)' | cut -c1-300)"
  fails=$((fails+1))
fi

# The test binary already prints this case's row format, so its rows are relayed verbatim —
# but its FAILS= line is deliberately dropped and its count folded in below.
echo "${unit}" | grep -E '^(PASS|FAIL) \| '
unit_fails="$(echo "${unit}" | sed -nE 's/^FAILS=([0-9]+)$/\1/p' | tail -1)"
if [ -z "${unit_fails}" ]; then
  row FAIL "unit tests: reported a verdict" "no FAILS= line — the binary did not finish"
  fails=$((fails+1))
else
  row INFO "unit tests: verdict" "${unit_fails} failing assertion(s)"
  fails=$((fails+unit_fails))
fi

# ---------------------------------------------------------------------------------------
# Part B — one quota, two processes, one container
# ---------------------------------------------------------------------------------------

B_ROWS="one-quota: the first process takes the whole quota
one-quota: the second process is refused by our marker
one-quota: the ledger region was created in the container
monitor: reports the figures the shim refused against
monitor: an independent parse by documented offset agrees
monitor: the compute cap is readable where ppu-smi has no field
monitor: the charge is attributed to a process"

C_ROWS="per-card: the smaller card refuses a size only the larger can hold
per-card: the larger card serves that same size
per-card: a VMM allocation is charged to the card its prop names
per-card: a pool allocation is charged to its pool's card
per-card: the reader shows both cards at their own figure
per-card: ppu-smi shows each card its own quota
fallback: a card with no figure of its own is held to the un-indexed one
fallback: a card's own figure still overrides the un-indexed one"

# Both parts' rows, because a run without hardware must not read as success by omission: a row that
# is not printed at all is indistinguishable from one that passed.
skip_all() {
  printf '%s\n%s\n' "${B_ROWS}" "${C_ROWS}" | while IFS= read -r r; do row SKIP "${r}" "$1"; done
  echo "FAILS=${fails}"
  exit 0
}

SHIM="${STAGE}/hggc_quota.so"
[ -f "${SHIM}" ] || skip_all "${SHIM} missing — run scripts/build.sh xbuild-thead-ppu first"

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  skip_all "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
fi
[ -n "${CARD}" ] || skip_all "no idle card (every card at or above XB_PPU_IDLE_MIB, or non-zero util)"
[ -e "/dev/alixpu_ppu${CARD}" ] || skip_all "/dev/alixpu_ppu${CARD} absent for the chosen card"

[ -x "${STAGE}/hggc_mem_paths" ] \
  || skip_all "${STAGE}/hggc_mem_paths missing — run scripts/build.sh xbuild-thead-ppu first"
[ -x "${STAGE}/ppu-monitor" ] \
  || skip_all "${STAGE}/ppu-monitor missing — run scripts/build.sh xbuild-thead-ppu first"

DEV="--device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu${CARD}"
BYTES=$((QUOTA * 1024 * 1024))

# Both processes run in ONE container, so they share one /dev/shm and therefore one region.
# The device index is a literal 0: only one card node is mounted and the SDK renumbers what it
# sees inside the container, so the host ordinal names the node while the container addresses
# it as 0. The holder is given a head start, and its own verdict is printed before it waits.
#
# The hold runs long enough to cover the reads that follow it: the monitor and the `od` parse have
# to see the quota while it is still held, or they would report a released card and agree with each
# other about nothing. The monitor is NOT preloaded — it reads the region, which is the whole claim.
# shellcheck disable=SC2086
pair="$(${CTR_RUN} ${DEV} \
  -e LD_PRELOAD=/work/hggc_quota.so \
  -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
  -e "HGGC_DEVICE_SM_LIMIT=100" \
  -e LIBHGGC_LOG_LEVEL=2 \
  "${IMG}" timeout 120 bash -c "
    ./hggc_mem_paths 0 ${BYTES} hold 30 > /tmp/holder.log 2>&1 &
    holder=\$!
    sleep 5
    ./hggc_mem_paths 0 1048576 plain > /tmp/second.log 2>&1
    echo '--- MAGIC ---'
    head -c 8 /dev/shm/vppu-ledger 2>&1 || true
    echo
    echo '--- MONITOR ---'
    LD_PRELOAD= ./ppu-monitor 2>&1; echo \"monitor_exit=\$?\"
    od -A n -t u8 -j 96  -N 16 /dev/shm/vppu-ledger | sed 's/^/OD /'
    od -A n -t u4 -j 112 -N 8  /dev/shm/vppu-ledger | sed 's/^/OD /'
    wait \${holder}
    echo '--- HOLDER ---'; cat /tmp/holder.log
    echo '--- SECOND ---'; cat /tmp/second.log" 2>&1)"

# section <marker> <output> — one named section only. The preload is loaded into every process in the
# container, so the combined output also carries each of their constructor lines and counter dumps; a
# range that ran to the end of the output would mix one process's verdict into another's row. It
# takes the output as an argument because Part C runs a second container.
section() { echo "$2" | awk -v s="--- $1 ---" '$0 == s { on = 1; next } /^--- /{ on = 0 } on'; }
holder_out="$(section HOLDER "${pair}")"
second_out="$(section SECOND "${pair}")"

if echo "${holder_out}" | grep -q 'PATH hold result=success'; then
  row PASS "one-quota: the first process takes the whole quota" "${QUOTA}MiB allocated and held"
else
  row FAIL "one-quota: the first process takes the whole quota" \
    "the holder did not get ${QUOTA}MiB, so nothing was spent to contend for: $(echo "${holder_out}" | grep -E '^PATH' | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

if echo "${second_out}" | grep -q 'PATH plain result=failed' \
   && echo "${second_out}" | grep -q 'DENIED'; then
  row PASS "one-quota: the second process is refused by our marker" \
    "$(echo "${second_out}" | grep -o 'DENIED .*' | head -1 | cut -c1-180)"
elif echo "${second_out}" | grep -q 'PATH plain result=success'; then
  row FAIL "one-quota: the second process is refused by our marker" \
    "1MiB was granted while the whole ${QUOTA}MiB quota was held — the ledger is not shared across processes"
  fails=$((fails+1))
else
  row FAIL "one-quota: the second process is refused by our marker" \
    "refused without our marker, or it never ran: $(echo "${second_out}" | grep -E '^PATH' | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

if echo "${pair}" | grep -q 'VPPUREGN'; then
  row PASS "one-quota: the ledger region was created in the container" \
    "/dev/shm/vppu-ledger opens with the documented magic"
else
  row FAIL "one-quota: the ledger region was created in the container" \
    "no VPPUREGN at /dev/shm/vppu-ledger, so the accounting was not in a shared region"
  fails=$((fails+1))
fi

# ---------------------------------------------------------------------------------------
# The same held quota, read three ways
# ---------------------------------------------------------------------------------------

# kv <line> <key> — one key=value field out of a line of them, whether the reader printed it or
# the shim did. Both formats are key=value, which is what lets one helper read either.
kv() { echo "$1" | tr ' ' '\n' | sed -n "s/^$2=//p" | head -1; }

mon_card="$(section MONITOR "${pair}" | grep '^card=0 ' | head -1)"
denied="$(echo "${second_out}" | grep -o 'DENIED .*' | head -1)"

# od printed the two 8-byte figures at offset 96 and the two 4-byte ones at 112 — quota, charge,
# compute limit, last measured utilisation, in the documented order.
#
# Its lines are TAGGED rather than fenced by a `--- OD ---` marker, and that is not cosmetic: the
# holder is still running and its own [vppu] lines arrive on the same merged stream, so a range
# between markers picks them up and the numbers come out as log text. A tag survives interleaving,
# and a line that did get garbled fails the comparison instead of quietly parsing as something.
# shellcheck disable=SC2086  # split on purpose: four numbers, one per field
set -- $(echo "${pair}" | sed -n 's/^OD //p' | tr '\n' ' ')
od_quota="${1:-}"; od_used="${2:-}"; od_limit="${3:-}"; od_util="${4:-}"

if [ -z "${mon_card}" ]; then
  for r in "monitor: reports the figures the shim refused against" \
           "monitor: an independent parse by documented offset agrees" \
           "monitor: the compute cap is readable where ppu-smi has no field" \
           "monitor: the charge is attributed to a process"; do
    row FAIL "${r}" "ppu-monitor printed no card=0 line: $(section MONITOR "${pair}" | tr '\n' ' ' | cut -c1-200)"
    fails=$((fails+1))
  done
else
  # The shim's own refusal names the figures it decided against; the monitor read the same two out
  # of the region afterwards. Equal is the claim — one number, from one place, whichever half of the
  # library you ask.
  if [ -n "${denied}" ] && [ "$(kv "${mon_card}" mem_quota_bytes)" = "$(kv "${denied}" quota)" ] \
     && [ "$(kv "${mon_card}" mem_used_bytes)" = "$(kv "${denied}" accounted)" ]; then
    row PASS "monitor: reports the figures the shim refused against" \
      "quota=$(kv "${denied}" quota) accounted=$(kv "${denied}" accounted), both as the reader prints them"
  else
    row FAIL "monitor: reports the figures the shim refused against" \
      "shim: ${denied:-no DENIED line} vs reader: ${mon_card}"
    fails=$((fails+1))
  fi

  # The independent half: `od` at the documented offsets, never through a header from common/. A
  # reader that agreed only with the struct it was compiled against would say nothing about the
  # contract a scraper has to write to.
  if [ "${od_quota}" = "$(kv "${mon_card}" mem_quota_bytes)" ] \
     && [ "${od_used}" = "$(kv "${mon_card}" mem_used_bytes)" ] \
     && [ "${od_limit}" = "$(kv "${mon_card}" sm_limit_pct)" ]; then
    row PASS "monitor: an independent parse by documented offset agrees" \
      "od at 96/112: ${od_quota} ${od_used} ${od_limit} ${od_util}"
  else
    row FAIL "monitor: an independent parse by documented offset agrees" \
      "od at 96/112: '${od_quota}' '${od_used}' '${od_limit}' vs reader: ${mon_card}"
    fails=$((fails+1))
  fi

  # The compute cap is the one figure that exists nowhere else: ppu-smi has no maximum-SM column,
  # so without this reader a container's compute limit can only be inferred from an init log.
  if [ "$(kv "${mon_card}" sm_limit_pct)" = 100 ]; then
    row PASS "monitor: the compute cap is readable where ppu-smi has no field" \
      "sm_limit_pct=100, the figure this case injected"
  else
    row FAIL "monitor: the compute cap is readable where ppu-smi has no field" \
      "injected HGGC_DEVICE_SM_LIMIT=100, read sm_limit_pct=$(kv "${mon_card}" sm_limit_pct)"
    fails=$((fails+1))
  fi

  # And the breakdown, which is what makes a charge reclaimable: the card's total has to be
  # attributed to a live pid, not merely counted.
  proc_line="$(section MONITOR "${pair}" | grep '^  proc pid=' | head -1)"
  if [ -n "${proc_line}" ] && [ "$(kv "${proc_line}" mem_bytes)" = "$(kv "${mon_card}" mem_used_bytes)" ]; then
    row PASS "monitor: the charge is attributed to a process" "${proc_line# }"
  else
    row FAIL "monitor: the charge is attributed to a process" \
      "${proc_line:-no proc line} against mem_used_bytes=$(kv "${mon_card}" mem_used_bytes)"
    fails=$((fails+1))
  fi
fi

# ---------------------------------------------------------------------------------------
# Part C — one container, two cards, two different quotas
# ---------------------------------------------------------------------------------------

skip_c() {
  echo "${C_ROWS}" | while IFS= read -r r; do row SKIP "${r}" "$1"; done
}

PROBE=$(( (QUOTA_A + QUOTA_B) / 2 ))

if [ -z "${CARD2}" ]; then
  skip_c "only one idle card — the per-card rows need two, like case 5"
elif [ ! -e "/dev/alixpu_ppu${CARD2}" ]; then
  skip_c "/dev/alixpu_ppu${CARD2} absent for the second chosen card"
elif [ "${QUOTA_A}" -ge "${QUOTA_B}" ]; then
  skip_c "XB_PPU_QUOTA_A_MIB (${QUOTA_A}) must be below XB_PPU_QUOTA_B_MIB (${QUOTA_B}) — the rows turn on a size between the two"
else
  # Two card nodes in ONE container. The indices are literal 0 and 1 and nothing here depends on
  # which host ordinal became which: the SDK renumbers from 0 inside the container, and the claim
  # under test is container-local — index 0 is held to the figure given for index 0.
  DEV2="--device /dev/alixpu --device /dev/alixpu_ctl \
--device /dev/alixpu_ppu${CARD} --device /dev/alixpu_ppu${CARD2}"
  PROBE_BYTES=$((PROBE * 1024 * 1024))
  # A container path, resolved by the payload because the command below is assembled here; the
  # variable is unset on this side, so the default is what reaches the container — the same shape
  # case 2 uses.
  SMI="${PPU_HOME:-/usr/local/PPU_SDK}/ppu-smi/bin/ppu-smi"

  # shellcheck disable=SC2086
  cards="$(${CTR_RUN} ${DEV2} \
    -e LD_PRELOAD=/work/hggc_quota.so \
    -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA_A}" \
    -e "HGGC_DEVICE_MEMORY_LIMIT_1=${QUOTA_B}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" \
    -e LIBHGGC_LOG_LEVEL=2 \
    "${IMG}" timeout 120 bash -c "
      echo '--- SMALL ---';  ./hggc_mem_paths 0 ${PROBE_BYTES} plain 2>&1
      echo '--- LARGE ---';  ./hggc_mem_paths 1 ${PROBE_BYTES} plain 2>&1
      echo '--- VMM ---';    ./hggc_mem_paths 0 ${PROBE_BYTES} vmm 1 2>&1
      echo '--- POOL ---';   ./hggc_mem_paths 0 ${PROBE_BYTES} pool 1 2>&1
      echo '--- READER ---'; LD_PRELOAD= ./ppu-monitor 2>&1
      echo '--- SMI ---';    LD_PRELOAD=/work/hgml_dlsym_hook.so timeout 60 ${SMI} 2>&1" 2>&1)"

  small="$(section SMALL "${cards}")"
  large="$(section LARGE "${cards}")"
  reader="$(section READER "${cards}")"
  smi="$(section SMI "${cards}")"

  # The refusal has to name index 0's OWN quota in bytes. The figure is the whole point: a shim
  # charging one container-wide number would either refuse both indices or serve both, and one that
  # mixed the two cards up would refuse this one against the larger card's figure.
  if echo "${small}" | grep -q 'PATH plain result=failed' \
     && echo "${small}" | grep -q "DENIED.*device=0.*quota=$((QUOTA_A * 1024 * 1024))$"; then
    row PASS "per-card: the smaller card refuses a size only the larger can hold" \
      "$(echo "${small}" | grep -o 'DENIED .*' | head -1 | cut -c1-180)"
  elif echo "${small}" | grep -q 'PATH plain result=success'; then
    row FAIL "per-card: the smaller card refuses a size only the larger can hold" \
      "${PROBE}MiB was granted on index 0 against a ${QUOTA_A}MiB figure — the quota is not keyed per card"
    fails=$((fails+1))
  else
    row FAIL "per-card: the smaller card refuses a size only the larger can hold" \
      "refused without naming quota=$((QUOTA_A * 1024 * 1024)), or it never ran: $(echo "${small}" | grep -E '^PATH|DENIED' | tr '\n' ' ' | cut -c1-220)"
    fails=$((fails+1))
  fi

  if echo "${large}" | grep -q 'PATH plain result=success'; then
    row PASS "per-card: the larger card serves that same size" \
      "${PROBE}MiB on index 1 against its own ${QUOTA_B}MiB figure"
  else
    row FAIL "per-card: the larger card serves that same size" \
      "the same ${PROBE}MiB was refused on index 1, whose figure is ${QUOTA_B}MiB: $(echo "${large}" | grep -E '^PATH|DENIED' | tr '\n' ' ' | cut -c1-220)"
    fails=$((fails+1))
  fi

  # The two entries that carry a card of their OWN. Both run from a context on the SMALLER card and
  # aim the allocation at the larger one, asking for a size only the larger can hold: charged to the
  # card each names, this succeeds; charged to the calling thread's context, as both did before the
  # ship-time review, it is refused against the smaller card's figure. The `context=/target=` line
  # the tool prints is required too, so a row cannot pass because the tool quietly used the context.
  for probe in vmm pool; do
    case "${probe}" in
      vmm)  label="per-card: a VMM allocation is charged to the card its prop names" ;;
      pool) label="per-card: a pool allocation is charged to its pool's card" ;;
    esac
    out_probe="$(section "$(echo "${probe}" | tr '[:lower:]' '[:upper:]')" "${cards}")"
    if echo "${out_probe}" | grep -q "PATH ${probe} context=0 target=1" \
       && echo "${out_probe}" | grep -q "PATH ${probe} result=success"; then
      row PASS "${label}" "${PROBE}MiB from a context on card 0, charged to card 1's ${QUOTA_B}MiB"
    else
      row FAIL "${label}" \
        "$(echo "${out_probe}" | grep -E '^PATH|DENIED' | tr '\n' ' ' | cut -c1-240)"
      fails=$((fails+1))
    fi
  done

  # The reader's first real two-card region — until now its multi-card path had only met the one
  # case 1 writes with dd. Both cards are recorded even though one allocation was refused: the
  # figures are written under the card's lock before the decision.
  r0="$(echo "${reader}" | grep '^card=0 ' | head -1)"
  r1="$(echo "${reader}" | grep '^card=1 ' | head -1)"
  if [ "$(kv "${r0}" mem_quota_mib)" = "${QUOTA_A}" ] \
     && [ "$(kv "${r1}" mem_quota_mib)" = "${QUOTA_B}" ]; then
    row PASS "per-card: the reader shows both cards at their own figure" \
      "card 0 at ${QUOTA_A}MiB, card 1 at ${QUOTA_B}MiB"
  else
    row FAIL "per-card: the reader shows both cards at their own figure" \
      "want ${QUOTA_A}/${QUOTA_B}MiB, read: $(echo "${reader}" | grep '^card=' | tr '\n' ' ' | cut -c1-260)"
    fails=$((fails+1))
  fi

  # The visibility half, in the one shape only a two-card container can show: ppu-smi asks HGML per
  # DEVICE HANDLE, so the hgml shim has to turn each handle back into its own index — case 2 proves
  # it answers with a quota, but with a single card a shim answering card 0's figure for every
  # handle would pass it. Only the total is judged: the probes above have exited, so a used figure
  # here would be measuring the refund path rather than the quota.
  #
  # smi_total <index> — the total side of ppu-smi's "<used>MiB / <total>MiB" for one card of the
  # container's own table. A card occupies two table rows, so the index is carried from the first to
  # the second. ppu-smi exits 0 whatever happens, so only a parsed figure may decide the row.
  smi_total() {
    echo "${smi}" | awk -v want="$1" '
      /^\| *[0-9]+ +PPU-/ { idx = $2; next }
      idx != "" && match($0, /[0-9]+MiB \/ [0-9]+MiB/) {
        m = substr($0, RSTART, RLENGTH); split(m, a, /MiB \/ /); sub(/MiB/, "", a[2])
        if (idx == want) { print a[2] + 0; exit }
        idx = ""
      }'
  }
  smi0="$(smi_total 0)"
  smi1="$(smi_total 1)"
  if [ "${smi0:-0}" = "${QUOTA_A}" ] && [ "${smi1:-0}" = "${QUOTA_B}" ]; then
    row PASS "per-card: ppu-smi shows each card its own quota" \
      "card 0 total=${smi0}MiB, card 1 total=${smi1}MiB, each its own figure"
  else
    row FAIL "per-card: ppu-smi shows each card its own quota" \
      "want ${QUOTA_A}/${QUOTA_B}MiB, parsed ${smi0:-none}/${smi1:-none}MiB: $(echo "${smi}" | grep -E 'MiB /' | tr '\n' ' ' | cut -c1-220)"
    fails=$((fails+1))
  fi

  # The other half of the contract, in the same two-card container: the UN-INDEXED figure. Card 0
  # gets no figure of its own, so it must be held to HGGC_DEVICE_MEMORY_LIMIT; card 1 keeps its
  # indexed figure and must still override. One run answers both, and it has to be a run of its own
  # rather than a re-read of the one above: which variables are set IS the claim.
  #
  # The sizes are the same probe midway between the two figures, so the two rows are the same
  # assertions as the indexed pair above with only the variable names moved — which is what makes
  # "the fallback behaves like the indexed form" a measurement instead of a hope.
  # shellcheck disable=SC2086
  shared="$(${CTR_RUN} ${DEV2} \
    -e LD_PRELOAD=/work/hggc_quota.so \
    -e "HGGC_DEVICE_MEMORY_LIMIT=${QUOTA_A}" \
    -e "HGGC_DEVICE_MEMORY_LIMIT_1=${QUOTA_B}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" \
    -e LIBHGGC_LOG_LEVEL=2 \
    -e HGGC_LEDGER_PATH=/work/ledger-case6-shared \
    "${IMG}" timeout 120 bash -c "
      echo '--- SHARED ---';   ./hggc_mem_paths 0 ${PROBE_BYTES} plain 2>&1
      echo '--- OVERRIDE ---'; ./hggc_mem_paths 1 ${PROBE_BYTES} plain 2>&1" 2>&1)"

  shared_out="$(section SHARED "${shared}")"
  override_out="$(section OVERRIDE "${shared}")"

  if echo "${shared_out}" | grep -q 'PATH plain result=failed' \
     && echo "${shared_out}" | grep -q "DENIED.*device=0.*quota=$((QUOTA_A * 1024 * 1024))$"; then
    row PASS "fallback: a card with no figure of its own is held to the un-indexed one" \
      "$(echo "${shared_out}" | grep -o 'DENIED .*' | head -1 | cut -c1-180)"
  elif echo "${shared_out}" | grep -q 'PATH plain result=success'; then
    row FAIL "fallback: a card with no figure of its own is held to the un-indexed one" \
      "${PROBE}MiB was granted on index 0 against HGGC_DEVICE_MEMORY_LIMIT=${QUOTA_A} — the un-indexed figure is not being read"
    fails=$((fails+1))
  else
    row FAIL "fallback: a card with no figure of its own is held to the un-indexed one" \
      "refused without naming quota=$((QUOTA_A * 1024 * 1024)), or it never ran: $(echo "${shared_out}" | grep -E '^PATH|DENIED' | tr '\n' ' ' | cut -c1-220)"
    fails=$((fails+1))
  fi

  if echo "${override_out}" | grep -q 'PATH plain result=success'; then
    row PASS "fallback: a card's own figure still overrides the un-indexed one" \
      "${PROBE}MiB on index 1 against its own ${QUOTA_B}MiB, beside a ${QUOTA_A}MiB fallback"
  else
    row FAIL "fallback: a card's own figure still overrides the un-indexed one" \
      "the same ${PROBE}MiB was refused on index 1, whose own figure is ${QUOTA_B}MiB: $(echo "${override_out}" | grep -E '^PATH|DENIED' | tr '\n' ' ' | cut -c1-220)"
    fails=$((fails+1))
  fi
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "THEAD-CASE 6" "$(xb_fails "${out}")"
