#!/usr/bin/env bash
#
# THEAD-CASE 5 — multi-card per-device quota independence   (needs two idle PPUs)
#
#   thead-case-5.sh
#
# Two containers, one card each, different quotas, run CONCURRENTLY — because "neither
# leaks into the other's accounting" is a claim about simultaneous use, and running them
# in sequence would pass even if they shared state.
#
# The discriminating size sits between the two quotas: the smaller-quota container must
# refuse it while the larger-quota one serves it. A refusal alone is not enough, so the
# smaller container's DENIED marker is also required to name ITS OWN quota — a marker
# naming the other container's figure is exactly what a leaked ledger would look like.
#
# Cards are chosen by being idle, never by a fixed index: the PPU test host runs production
# inference and a hardcoded card can be the one holding 91 GB.
#
# Every run here injects HGGC_DEVICE_SM_LIMIT=100 beside the memory figure. The library refuses
# every allocation while the container's quota is incomplete, and the compute figure became part
# of that once the controller landed; 100 is configured-and-uncapping, so nothing this case
# measures changes.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu,
#      on the TARGET), XB_PPU_CARDS (default: the first two idle cards, space-separated),
#      XB_PPU_IDLE_MIB (default 64), XB_PPU_QUOTA_A_MIB (default 2048),
#      XB_PPU_QUOTA_B_MIB (default 6144), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"

xctr_resolve || { echo "thead-case-5: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 5 — per-card quota independence (image ${XB_IMAGE}) on $(xtarget_desc)"

CARDS="${XB_PPU_CARDS:-}"
if [ -z "${CARDS}" ]; then
  CARDS="$(thead_idle_cards | head -2 | tr '\n' ' ')"
fi

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARDS="${CARDS}" \
  QUOTA_A="${XB_PPU_QUOTA_A_MIB:-2048}" QUOTA_B="${XB_PPU_QUOTA_B_MIB:-6144}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

SHIM="${STAGE}/hggc_quota.so"
if [ ! -f "${SHIM}" ]; then
  row FAIL "shim staged" "${SHIM} missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi

# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
CTR_RUN="${XB_CTR} ${XB_CTR_ARGS} run --rm --platform linux/amd64 -v ${STAGE}:/work -w /work"

if [ ! -x "${STAGE}/hggc_mem_paths" ]; then
  row FAIL "exerciser staged" "${STAGE}/hggc_mem_paths missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi
row INFO "exerciser staged" "hggc_mem_paths sha256=$(sha256sum "${STAGE}/hggc_mem_paths" | cut -c1-16)…"

skip_all() {
  for r in "card A under its own quota" "card B under its own quota" \
           "card A refuses the between-quota size" "card B serves the between-quota size" \
           "card A's denial names card A's quota"; do
    row SKIP "${r}" "$1"
  done
  echo "FAILS=${fails}"
  exit 0
}

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  skip_all "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
fi
# shellcheck disable=SC2086  # CARDS is a space-separated list on purpose
set -- ${CARDS}
if [ "$#" -lt 2 ]; then
  skip_all "fewer than two idle cards (found: ${CARDS:-none}) — this case needs two"
fi

CARD_A="$1"; CARD_B="$2"
# Two DISTINCT indices, or the case is not testing independence at all: the idle picker
# never repeats one, but XB_PPU_CARDS is a hand override and 'N N' would satisfy every row
# below while running both containers against the same card.
if [ "${CARD_A}" = "${CARD_B}" ]; then
  skip_all "the two chosen cards are the same index (${CARD_A}) — independence needs two distinct cards"
fi
for c in "${CARD_A}" "${CARD_B}"; do
  [ -e "/dev/alixpu_ppu${c}" ] || skip_all "/dev/alixpu_ppu${c} absent for a chosen card"
done
row INFO "cards chosen" "A=${CARD_A} quota=${QUOTA_A}MiB, B=${CARD_B} quota=${QUOTA_B}MiB (both idle)"

# The size that separates the two quotas: refused by A, served by B.
BETWEEN=$(( (QUOTA_A + QUOTA_B) / 2 ))
UNDER_A=$(( QUOTA_A / 2 ))
UNDER_B=$(( QUOTA_B / 2 ))

# exercise <card> <quota-mib> <mib> <outfile> — one container, its own card and quota.
#
# stdin comes from /dev/null: these runs are backgrounded and this payload itself arrives
# on a pipe, so a child that read stdin would eat the rest of the script.
# The device index is a literal 0 for the same reason as case 3: one card node per
# container, and the SDK renumbers inside it.
exercise() {
  local card="$1" quota="$2" mib="$3" outfile="$4"
  # shellcheck disable=SC2086
  ${CTR_RUN} --device /dev/alixpu --device /dev/alixpu_ctl --device "/dev/alixpu_ppu${card}" \
    -e LD_PRELOAD=/work/hggc_quota.so -e "HGGC_DEVICE_MEMORY_LIMIT_0=${quota}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" -e "LIBHGGC_LOG_LEVEL=2" \
    "${IMG}" timeout 120 ./hggc_mem_paths 0 "$((mib * 1024 * 1024))" plain \
    > "${outfile}" 2>&1 < /dev/null
}

path_result() { sed -nE 's/^PATH [a-z]+ result=([a-z]+).*/\1/p' "$1" | tail -1; }

W="$(mktemp -d)"
trap 'rm -rf "${W}"' EXIT

# Concurrent by design: sequential runs would pass even with shared accounting.
exercise "${CARD_A}" "${QUOTA_A}" "${UNDER_A}" "${W}/a-under" &
pid_a=$!
exercise "${CARD_B}" "${QUOTA_B}" "${UNDER_B}" "${W}/b-under" &
pid_b=$!
wait "${pid_a}" "${pid_b}" 2>/dev/null || true

exercise "${CARD_A}" "${QUOTA_A}" "${BETWEEN}" "${W}/a-between" &
pid_a=$!
exercise "${CARD_B}" "${QUOTA_B}" "${BETWEEN}" "${W}/b-between" &
pid_b=$!
wait "${pid_a}" "${pid_b}" 2>/dev/null || true

if [ "$(path_result "${W}/a-under")" = success ]; then
  row PASS "card A under its own quota" "${UNDER_A}MiB on card ${CARD_A} (quota ${QUOTA_A}MiB)"
else
  row FAIL "card A under its own quota" "$(grep -E '^PATH' "${W}/a-under" | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

if [ "$(path_result "${W}/b-under")" = success ]; then
  row PASS "card B under its own quota" "${UNDER_B}MiB on card ${CARD_B} (quota ${QUOTA_B}MiB)"
else
  row FAIL "card B under its own quota" "$(grep -E '^PATH' "${W}/b-under" | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

if [ "$(path_result "${W}/a-between")" = failed ] && grep -q DENIED "${W}/a-between"; then
  row PASS "card A refuses the between-quota size" "${BETWEEN}MiB denied on card ${CARD_A}"
else
  row FAIL "card A refuses the between-quota size" "${BETWEEN}MiB was not denied with a marker on card ${CARD_A}"
  fails=$((fails+1))
fi

if [ "$(path_result "${W}/b-between")" = success ]; then
  row PASS "card B serves the between-quota size" "${BETWEEN}MiB allocated on card ${CARD_B} while A refused it"
else
  row FAIL "card B serves the between-quota size" "${BETWEEN}MiB failed on card ${CARD_B}, so A's refusal shows nothing about independence"
  fails=$((fails+1))
fi

# The marker carries the quota the refusal was measured against. A's marker naming B's
# figure is what a leaked ledger would look like, so check the number, not just the word.
marker="$(grep -o 'DENIED .*' "${W}/a-between" | head -1)"
want_quota=$(( QUOTA_A * 1024 * 1024 ))
if echo "${marker}" | grep -q "quota=${want_quota}"; then
  row PASS "card A's denial names card A's quota" "${marker}"
else
  row FAIL "card A's denial names card A's quota" "expected quota=${want_quota}; got: ${marker:-no marker}"
  fails=$((fails+1))
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "THEAD-CASE 5: PASS"; exit 0; } || { echo "THEAD-CASE 5: FAIL"; exit 1; }
