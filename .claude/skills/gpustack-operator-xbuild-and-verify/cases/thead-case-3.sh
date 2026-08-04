#!/usr/bin/env bash
#
# THEAD-CASE 3 — Gate 2: memory-path completeness   (needs a real PPU)
#
#   thead-case-3.sh
#
# Asks whether a quota enforced at the DRIVER layer covers every way a workload can take
# device memory. Five paths — plain, async, pool, VMM, procaddr — each judged on three
# observations, because any one of them alone passes for the wrong reason:
#   1. under quota WITH the shim succeeds        (the shim is not simply breaking the path)
#   2. over quota WITHOUT the shim succeeds      (the card can serve it, so a refusal in 3
#                                                is ours and not the platform's)
#   3. over quota WITH the shim is refused AND carries the shim's DENIED marker
#                                                (refused by this quota, not by anything else)
# plus a fourth row reading the shim's per-entry counter, which is the only evidence that
# the call crossed libhggc.so at all.
#
# A fifth row stands outside the four paths because it asks a different question: does a
# FREE give its bytes back? None of the rows above can tell — each runs one allocation in a
# fresh process, so a shim that refunds nothing passes all of them and its ledger dies with
# the process. The refund row fills the quota with two allocations, frees both, and then asks
# for the whole quota again.
#
# The plain and async paths call the RUNTIME entries (hggcMalloc, hggcMallocAsync in
# libhggcrt) while the shim interposes the DRIVER entries — that difference is the
# measurement, not an oversight.
#
# The procaddr path asks the remaining question those four cannot: all of them reach an
# allocation entry by name, so the dynamic linker resolves it and a preloaded definition wins.
# This one asks hgGetProcAddress for the driver's own address and calls THAT — which is how
# libhggcrt binds the entries it needs, and which walks past any interposition of the entry
# point itself unless the resolver is covered too.
#
# When observation 3 is not refused and no counter moved, the row says so in those terms:
# the shim never saw the path, which is either the wrong ABI name (hgMemAlloc also exports
# a v1 symbol with different parameter types) or a path that bypasses libhggc.so. It is
# reported as both possibilities rather than as a settled premise failure.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu,
#      on the TARGET), XB_PPU_CARD (default: the first idle card), XB_PPU_IDLE_MIB
#      (default 64), XB_PPU_QUOTA_MIB (default 4096), XB_PPU_UNDER_MIB (default 1024),
#      XB_PPU_OVER_MIB (default 8192), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"

xctr_resolve || { echo "thead-case-3: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 3 — Gate 2 memory-path completeness (image ${XB_IMAGE}) on $(xtarget_desc)"


CARD="${XB_PPU_CARD:-}"
if [ -z "${CARD}" ]; then
  CARD="$(thead_idle_cards | head -1)"
fi

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARD="${CARD}" \
  QUOTA="${XB_PPU_QUOTA_MIB:-4096}" UNDER="${XB_PPU_UNDER_MIB:-1024}" \
  OVER="${XB_PPU_OVER_MIB:-8192}" <<'PAYLOAD'
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

if [ ! -x "${STAGE}/hggc_mem_paths" ]; then
  row FAIL "exerciser staged" "${STAGE}/hggc_mem_paths missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi
row INFO "exerciser staged" "hggc_mem_paths sha256=$(sha256sum "${STAGE}/hggc_mem_paths" | cut -c1-16)…"

PATHS="plain async pool vmm procaddr"
skip_all() {
  for p in ${PATHS}; do
    row SKIP "${p}: under quota succeeds" "$1"
    row SKIP "${p}: over quota succeeds uninjected" "$1"
    row SKIP "${p}: over quota denied with marker" "$1"
    row SKIP "${p}: crossed libhggc.so" "$1"
  done
  row SKIP "refund: freed bytes are returned to the quota" "$1"
  echo "FAILS=${fails}"
  exit 0
}

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  skip_all "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
fi
[ -n "${CARD}" ] || skip_all "no idle card (every card at or above XB_PPU_IDLE_MIB, or non-zero util)"
[ -e "/dev/alixpu_ppu${CARD}" ] || skip_all "/dev/alixpu_ppu${CARD} absent for the chosen card"

DEV="--device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu${CARD}"

# exercise <path> <mib> <inject:yes|no> — one path, one size, one process.
# The device index passed in is a literal 0, not ${CARD}: only one card node is mounted,
# and the SDK renumbers what it sees inside the container, so the host ordinal names the
# device node while the container always addresses it as index 0.
exercise() {
  local path="$1" mib="$2" inject="$3" env_args=""
  if [ "${inject}" = yes ]; then
    env_args="-e LD_PRELOAD=/work/hggc_quota.so -e HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}"
    # The compute figure is uncapping at 100, and it has to be there: the library refuses every
    # allocation while the container carries an incomplete quota, compute included.
    env_args="${env_args} -e HGGC_DEVICE_SM_LIMIT=100 -e LIBHGGC_LOG_LEVEL=2"
  fi
  # shellcheck disable=SC2086
  ${CTR_RUN} ${DEV} ${env_args} "${IMG}" \
    timeout 120 ./hggc_mem_paths 0 "$((mib * 1024 * 1024))" "${path}" 2>&1
}

path_result() { echo "$1" | sed -nE 's/^PATH [a-z]+ result=([a-z]+).*/\1/p' | tail -1; }

# counters_moved — did any interposed entry get called anywhere in this container run?
#
# Considers EVERY counter line, not the last one. LD_PRELOAD loads the shim into every
# dynamically linked process in the container, so the `timeout` wrapper prints an
# all-zero dump of its own — and it exits after the exerciser, so its line comes last.
# Taking the last line would read the wrapper's zeros as "the call never crossed
# libhggc.so" and fail a path that in fact worked.
counters_moved() {
  echo "$1" | sed -nE 's/.*hggc_quota counters:(.*)/\1/p' \
    | tr ' ' '\n' | grep -E '^hg[A-Za-z_0-9]+=[0-9]+$' | grep -vqE '=0$'
}

# counter_line — the dump that actually shows activity, for the row detail.
counter_line() {
  echo "$1" | grep -o 'hggc_quota counters:.*' | grep -E '=[1-9]' | head -1
}

for path in ${PATHS}; do
  under_out="$(exercise "${path}" "${UNDER}" yes)"
  over_bare_out="$(exercise "${path}" "${OVER}" no)"
  over_out="$(exercise "${path}" "${OVER}" yes)"

  if [ "$(path_result "${under_out}")" = success ]; then
    row PASS "${path}: under quota succeeds" "${UNDER}MiB allocated with the shim injected (quota ${QUOTA}MiB)"
  else
    row FAIL "${path}: under quota succeeds" "${UNDER}MiB refused or failed: $(echo "${under_out}" | grep -E '^PATH' | tr '\n' ' ' | cut -c1-200)"
    fails=$((fails+1))
  fi

  if [ "$(path_result "${over_bare_out}")" = success ]; then
    row PASS "${path}: over quota succeeds uninjected" "${OVER}MiB allocated with no shim — the card can serve it"
  else
    row FAIL "${path}: over quota succeeds uninjected" "${OVER}MiB failed without the shim, so a refusal with it proves nothing: $(echo "${over_bare_out}" | grep -E '^PATH' | tr '\n' ' ' | cut -c1-200)"
    fails=$((fails+1))
  fi

  if [ "$(path_result "${over_out}")" = failed ] && echo "${over_out}" | grep -q 'DENIED'; then
    row PASS "${path}: over quota denied with marker" "$(echo "${over_out}" | grep -o 'DENIED .*' | head -1)"
  elif [ "$(path_result "${over_out}")" = failed ]; then
    row FAIL "${path}: over quota denied with marker" "refused, but without the shim's DENIED marker — the refusal may not be ours"
    fails=$((fails+1))
  elif counters_moved "${over_out}"; then
    row FAIL "${path}: over quota denied with marker" "not refused although the shim was called — the quota did not cover this entry"
    fails=$((fails+1))
  else
    row FAIL "${path}: over quota denied with marker" "not refused and no counter moved — the shim never saw this path: either the wrong ABI name (hgMemAlloc also exports a v1 symbol) or the path bypasses libhggc.so"
    fails=$((fails+1))
  fi

  if counters_moved "${under_out}"; then
    row PASS "${path}: crossed libhggc.so" "$(counter_line "${under_out}" | cut -c1-180)"
  else
    row FAIL "${path}: crossed libhggc.so" "no interposed entry was called under quota, so the crossing is unproven"
    fails=$((fails+1))
  fi
done

# Every row above runs a single allocation in a fresh process, so none of them can see
# whether a free gives its bytes back — a shim that refunds nothing passes all four, because
# its ledger dies with the process before a second allocation could be denied. This is that
# missing observation: half the quota twice (filling it exactly), both freed, then the whole
# quota, which is admitted only if BOTH refunds landed.
refund_mib=$((QUOTA / 2))
refund_out="$(exercise refund "${refund_mib}" yes)"
if [ "$(path_result "${refund_out}")" = success ]; then
  row PASS "refund: freed bytes are returned to the quota" "2 x ${refund_mib}MiB filled the ${QUOTA}MiB quota, both freed, then ${QUOTA}MiB admitted again"
elif echo "${refund_out}" | grep -q 'DENIED'; then
  row FAIL "refund: freed bytes are returned to the quota" "the whole-quota request after both frees was refused by our own quota, so a free did not refund: $(echo "${refund_out}" | grep -o 'DENIED .*' | head -1)"
  fails=$((fails+1))
else
  row FAIL "refund: freed bytes are returned to the quota" "the sequence did not complete: $(echo "${refund_out}" | grep -E '^PATH' | tr '\n' ' ' | cut -c1-220)"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "THEAD-CASE 3: PASS"; exit 0; } || { echo "THEAD-CASE 3: FAIL"; exit 1; }
