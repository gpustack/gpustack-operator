#!/usr/bin/env bash
#
# HYGON-CASE 3 — The memory cap is enforced, not just reported   (needs a real Hygon DCU)
#
#   hygon-case-3.sh
#
# HYGON-CASE 2 established that a sliced container REPORTS the quota. That is a weaker claim than
# it looks: a runtime could report a smaller figure and still hand out the whole card, and every
# row of case 2 would pass while nothing bounded the workload. This case allocates.
#
# THE THREE ALLOCATIONS ARE CHOSEN SO THAT ONLY A REAL CAP EXPLAINS THE PATTERN. One below the
# quota must succeed, and two above it must fail — but both of the failing sizes are also FAR
# below the physical card (1024 MiB quota against 65520 MiB of VRAM). A card that was simply full,
# or a runtime that refused every large allocation, would fail the first one too. Succeed-then-fail
# at a boundary that is nowhere near a hardware limit is what distinguishes an enforced quota from
# a coincidence.
#
# THE ERROR TEXT IS ASSERTED, not just the failure. HIP reports "GPU 0 has a total capacity of
# 1024.00 MiB", quoting the quota back. That is the difference between "the allocation failed" and
# "the allocation failed BECAUSE OF THIS SLICE" — the second is what the case is for, and without
# it a transient driver error would read as a passing row.
#
# NOTE ON A BUSY CARD. This case runs happily beside other tenants: it asks for a small quota and
# allocates a fraction of it, so it does not need the card to be idle and does not disturb whoever
# holds the rest. What it must not do is pick a quota so large that the card cannot back it — see
# XB_MEM.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE, then quay.io/gpustack/runner:dtk25.04-vllm0.11.0),
#      XB_HCU (default 0), XB_MEM (quota MiB, default 1024 — must be backable by the card's FREE
#      memory), XB_CU (default 8), XB_HYHAL, XB_DTK, XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

xctr_resolve || { echo "hygon-case-3: no container runtime on the target"; exit 2; }
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-quay.io/gpustack/runner:dtk25.04-vllm0.11.0}}"

echo "# HYGON-CASE 3 — memory cap enforcement (image ${IMG}, hcu ${XB_HCU:-0}) on $(xtarget_desc)"

out="$(xsh \
  CTR="${XB_CTR}" CTR_ARGS="${XB_CTR_ARGS}" IMG="${IMG}" \
  HCU="${XB_HCU:-0}" MEM="${XB_MEM:-1024}" CU="${XB_CU:-8}" \
  HYHAL="${XB_HYHAL:-/opt/hyhal}" DTK="${XB_DTK:-/opt/dtk}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
# shellcheck disable=SC2086
ctr(){ ${CTR} ${CTR_ARGS} "$@"; }

work="$(mktemp -d)"
# run_tag makes every container this case starts unique to this run. The cleanup below removes
# containers BY NAME, and a fixed name would have it tear down whatever a concurrent verification run
# on the same host happened to be holding -- somebody else's card, mid-measurement.
run_tag="$(basename "${work}")"
trap 'rm -rf "${work}"; ctr rm -f "gpustack-hyc3-${run_tag}" >/dev/null 2>&1' EXIT

bdf="$("${HYHAL}/bin/hy-smi" --showbus 2>/dev/null | sed -n "s/^HCU\[${HCU}\][[:space:]]*: PCI Bus: \([0-9a-fA-F:.]*\).*/\1/p" | head -1)"
[ -n "${bdf}" ] || { row FAIL "resolve HCU ${HCU} bdf" "hy-smi named none"; echo "FAILS=1"; exit 0; }
drm_minor(){
  for p in /sys/class/drm/"$1"*/device; do
    [ -e "${p}" ] || continue
    n="${p#/sys/class/drm/$1}"; n="${n%/device}"
    case "${n}" in ''|*[!0-9]*) continue;; esac
    [ "$(basename "$(readlink -f "${p}")")" = "${bdf}" ] && { echo "${n}"; return; }
  done
}
card="$(drm_minor card)"; render="$(drm_minor renderD)"
[ -n "${card}" ] && [ -n "${render}" ] || { row FAIL "resolve drm nodes" "for ${bdf}"; echo "FAILS=1"; exit 0; }
row PASS "accelerator" "${bdf} -> card${card}/renderD${render}, quota ${MEM} MiB"

# Report the card's own size so the rows below can be read as "nowhere near the hardware limit".
whole="$("${HYHAL}/bin/hy-smi" --showmeminfo vram 2>/dev/null | sed -n "s/^HCU\[${HCU}\][[:space:]]*: vram Total Memory (MiB): \([0-9]*\).*/\1/p" | head -1)"
row PASS "card VRAM" "${whole:-unknown} MiB"

# CU_LO / CU_HI: the two cu_mask words for $1 contiguous compute units from bit ${2:-0}. Built one
# bit at a time rather than from a shifted width, because a shell's `<<` is modulo the word size:
# `1 << 64` is 1, not 2^64, so a record on a 128-CU part would carry 64 mask bits under a
# `cu_count: 128` and the vendor parser rejects it for exactly that mismatch.
cu_words(){
  _lo=0; _hi=0; _b="${2:-0}"; _n=0
  while [ "${_n}" -lt "$1" ]; do
    if [ "${_b}" -lt 64 ]; then _lo=$(( _lo | (1 << _b) )); else _hi=$(( _hi | (1 << (_b - 64)) )); fi
    _b=$(( _b + 1 )); _n=$(( _n + 1 ))
  done
  CU_LO=$(printf '0x%016x' "${_lo}"); CU_HI=$(printf '0x%016x' "${_hi}")
}

mkdir -p "${work}/vdev"
cu_words "${CU}"
printf 'PciBusId: %s\ncu_mask: %s\ncu_mask: %s\ncu_count: %d\nmem: %d MiB\ndevice_id: 0\nvdev_id: 0\npipe_id: 0\nenable: 1\n' \
  "${bdf}" "${CU_LO}" "${CU_HI}" "${CU}" "${MEM}" > "${work}/vdev/vdev0.conf"

# One container, three allocations, so all three are judged against the SAME slice instance.
cat > "${work}/alloc.py" <<'PY'
import os, torch
quota = int(os.environ["QUOTA_MIB"])
def attempt(mib):
    try:
        x = torch.empty(mib * 1024 * 1024 // 2, dtype=torch.float16, device="cuda")
        torch.cuda.synchronize()
        del x
        torch.cuda.empty_cache()
        print("ALLOC %d OK" % mib)
    except Exception as e:
        print("ALLOC %d FAILED %s" % (mib, str(e)[:200].replace("\n", " ")))
print("QUOTA_REPORTED %d" % (torch.cuda.get_device_properties(0).total_memory // (1024 * 1024)))
attempt(quota // 2)          # comfortably inside the slice
attempt(quota * 2)           # outside the slice, far inside the card
attempt(quota * 8)           # further out, still far inside the card
PY

# The driver keeps a slice record for about a second after the container holding it is removed, and
# a new slice reusing that record's pipe_id inside the window is refused the card outright -- the
# refusal HYGON-CASE 5 provokes deliberately. This record uses pipe_id 0 like most of them, so it
# waits for the card to come free rather than racing whatever ran before it; observed once as every
# row of this case failing with no device at all. Bounded, because a slice somebody else holds is
# not ours to wait out.
freew=0
while [ "${freew}" -lt 50 ] && [ -n "$(ls /sys/devices/virtual/kfd/kfd/vgpu/ 2>/dev/null)" ]; do
  sleep 0.1; freew=$(( freew + 1 ))
done

log="$(ctr run --rm --name "gpustack-hyc3-${run_tag}" \
  --device=/dev/kfd --device=/dev/mkfd \
  --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
  --group-add video --security-opt seccomp=unconfined \
  -e QUOTA_MIB="${MEM}" \
  -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
  -v "${work}/vdev:/etc/vdev/docker:ro" -v "${work}/alloc.py:/alloc.py:ro" \
  "${IMG}" python3 -u /alloc.py 2>&1)"

reported="$(printf '%s\n' "${log}" | sed -n 's/^QUOTA_REPORTED \([0-9]*\)$/\1/p' | tail -1)"
[ "${reported}" = "${MEM}" ] \
  && row PASS "the slice is in force before allocating" "${reported} MiB" \
  || { row FAIL "the slice is in force before allocating" "reported [${reported:-none}]"; fails=$((fails+1)); }

verdict_of(){ printf '%s\n' "${log}" | sed -n "s/^ALLOC $1 \([A-Z]*\).*/\1/p" | tail -1; }

half=$(( MEM / 2 )); twice=$(( MEM * 2 )); eight=$(( MEM * 8 ))

[ "$(verdict_of "${half}")" = OK ] \
  && row PASS "allocation inside the quota succeeds" "${half} MiB" \
  || { row FAIL "allocation inside the quota succeeds" "${half} MiB -> $(verdict_of "${half}"):-none"; fails=$((fails+1)); }

# The refusal must quote the QUOTA back, and it must do so on EACH size: that is what ties every one
# of these failures to this slice rather than to a card that happened to be full. Searched over the
# whole log instead, one refusal naming the capacity would carry the other, which may have failed for
# a reason that says nothing about slicing at all.
message_of(){ printf '%s\n' "${log}" | sed -n "s/^ALLOC $1 FAILED //p" | tail -1; }

for size in "${twice}" "${eight}"; do
  if [ "$(verdict_of "${size}")" = FAILED ]; then
    row PASS "allocation past the quota fails" "${size} MiB"
  else
    row FAIL "allocation past the quota fails" "${size} MiB -> $(verdict_of "${size}"):-none"; fails=$((fails+1))
  fi
  if printf '%s\n' "$(message_of "${size}")" | grep -q "total capacity of ${MEM}\.00 MiB"; then
    row PASS "the ${size} MiB refusal names the quota as the capacity" "total capacity of ${MEM}.00 MiB"
  else
    row FAIL "the ${size} MiB refusal names the quota as the capacity" "said: $(message_of "${size}" | cut -c1-140)"; fails=$((fails+1))
  fi
  # ...and it must be nowhere near the card's own size, or the refusal says nothing about slicing.
  if [ -n "${whole}" ] && [ "${size}" -lt "${whole}" ]; then
    row PASS "the refused size is far inside the card" "${size} < ${whole} MiB"
  else
    row FAIL "the refused size is far inside the card" "${size} vs card ${whole:-unknown} MiB"; fails=$((fails+1))
  fi
done

echo "--- allocation log ---"
printf '%s\n' "${log}" | grep -E '^(QUOTA_REPORTED|ALLOC)' | cut -c1-160
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 3" "$(xb_fails "${out}")"
