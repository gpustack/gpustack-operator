#!/usr/bin/env bash
#
# HYGON-CASE 4 — The CU mask bounds compute, not just the CU count   (needs a real Hygon DCU)
#
#   hygon-case-4.sh
#
# The `cores-percentage` half of a Hygon sliced request becomes a bitmask: sliceCUCount turns the
# percent into a count of compute units and packCUMask picks that many free bits (both in
# pkg/devicemanager/allocator/hygon/vdev.go). A container then reports the count back as
# multi_processor_count — which HYGON-CASE 2 already checks.
#
# REPORTING A COUNT IS NOT BOUNDING A WORKLOAD. The runtime could publish a reduced figure and
# still schedule across every unit on the card, and every existing row would pass. So this case
# does not read a number; it measures work done. It runs the same matmul under several mask widths
# and requires the throughput to follow the mask.
#
# THE FOUR ASSERTIONS, and why each is needed:
#   * per width, the reported CU count equals the mask's hamming weight — the cheap check, kept
#     because a mismatch here explains a throughput row that would otherwise look like noise;
#   * throughput rises with the mask, comparing the NARROWEST against the WIDEST rather than each
#     adjacent pair. Adjacent widths on a card shared with other tenants can invert on scheduling
#     jitter alone; the ends cannot, and a flaky case is worse than no case;
#   * the narrowest width must be well under the widest — a specific ratio, not merely "less" —
#     because "less" is satisfied by measurement noise;
#   * a FULL-width mask must match the unsliced card. This is the row that separates "the mask
#     bounds compute" from "the vdev.conf costs performance": if a full mask were also slower,
#     every other row here would be explained by overhead rather than by isolation.
#
# THE CARD DOES NOT NEED TO BE IDLE. Every figure is compared against another figure from this
# same run, so a card busy with someone else's work moves all of them together. What would break
# the case is the OTHER tenant's load changing sharply mid-run; the ratio thresholds are loose
# enough to absorb the ordinary case and the widths are run cheapest-last so a drifting card shows
# up as a failed monotonicity row rather than a false pass.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE, then quay.io/gpustack/runner:dtk25.04-vllm0.11.0),
#      XB_HCU (default 0), XB_MEM (quota MiB, default 3072 — must hold three 4096^2 fp16
#      matrices), XB_WIDTHS (default "8 20 40"; the full width is added automatically),
#      XB_RATIO (narrowest must be under this fraction of widest, default 50 percent),
#      XB_HYHAL, XB_DTK, XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

xctr_resolve || { echo "hygon-case-4: no container runtime on the target"; exit 2; }
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-quay.io/gpustack/runner:dtk25.04-vllm0.11.0}}"

echo "# HYGON-CASE 4 — CU mask bounds compute (image ${IMG}, hcu ${XB_HCU:-0}) on $(xtarget_desc)"

out="$(xsh \
  CTR="${XB_CTR}" CTR_ARGS="${XB_CTR_ARGS}" IMG="${IMG}" \
  HCU="${XB_HCU:-0}" MEM="${XB_MEM:-3072}" WIDTHS="${XB_WIDTHS:-8 20 40}" \
  RATIO="${XB_RATIO:-50}" \
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
trap 'rm -rf "${work}"; ctr rm -f "gpustack-hyc4-${run_tag}" >/dev/null 2>&1' EXIT

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

cat > "${work}/bench.py" <<'PY'
import time, torch
p = torch.cuda.get_device_properties(0)
n, iters = 4096, 30
a = torch.randn(n, n, dtype=torch.float16, device="cuda")
b = torch.randn(n, n, dtype=torch.float16, device="cuda")
for _ in range(3):
    torch.mm(a, b)
torch.cuda.synchronize()
t0 = time.time()
for _ in range(iters):
    torch.mm(a, b)
torch.cuda.synchronize()
dt = time.time() - t0
# Report throughput in whole GFLOPS so the shell can compare with integer arithmetic.
print("BENCH CU=%d GFLOPS=%d" % (p.multi_processor_count, (2.0 * n**3 * iters) / dt / 1e9))
PY

run_bench(){ # $1 = vdev dir ("" for the unsliced card)
  slice=""
  [ -n "$1" ] && slice="-v $1:/etc/vdev/docker:ro"
  # shellcheck disable=SC2086
  ctr run --rm --name "gpustack-hyc4-${run_tag}" \
    --device=/dev/kfd --device=/dev/mkfd \
    --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
    --group-add video --security-opt seccomp=unconfined \
    -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
    -v "${work}/bench.py:/bench.py:ro" ${slice} \
    "${IMG}" python3 -u /bench.py 2>&1
}
field(){ printf '%s\n' "$1" | sed -n "s/.*BENCH CU=\([0-9]*\) GFLOPS=\([0-9]*\).*/\\$2/p" | tail -1; }

# CU_LO / CU_HI: the two cu_mask words for $1 contiguous compute units from bit ${2:-0}.
#
# Built one bit at a time rather than from a shifted width, because a shell's `<<` is modulo the
# word size: `1 << 64` is 1, not 2^64. The widest width this case runs is the card's own, so on a
# 128-CU part the full-width record would have carried 64 mask bits under a `cu_count: 128` and been
# rejected by the vendor parser for exactly that mismatch -- invisible on the 80-CU part this was
# developed against, where the high word is a normal shift.
cu_words(){
  _lo=0; _hi=0; _b="${2:-0}"; _n=0
  while [ "${_n}" -lt "$1" ]; do
    if [ "${_b}" -lt 64 ]; then _lo=$(( _lo | (1 << _b) )); else _hi=$(( _hi | (1 << (_b - 64)) )); fi
    _b=$(( _b + 1 )); _n=$(( _n + 1 ))
  done
  CU_LO=$(printf '0x%016x' "${_lo}"); CU_HI=$(printf '0x%016x' "${_hi}")
}

# A FRESH pipe_id per width, not a fixed one. The driver keeps a slice record for about a second
# after the process holding it exits (HYGON-CASE 5), and a new slice reusing that record's pipe id
# inside the window is refused the card outright -- so a fixed id makes each width race the previous
# width's teardown, and a lost race reads as a throughput row that failed for CU-mask reasons.
# pipe_id has to stay unique per card among LIVE slices, which a counter satisfies for the handful of
# widths this runs; it does not have to be zero.
#
# The id is a PARAMETER and the counter lives in the caller, because this function is called from a
# command substitution -- a subshell -- so a counter it advanced itself would be discarded and every
# width would write pipe_id 0 again while looking like it had been fixed.
mask_dir(){ # $1 = cu count, $2 = pipe id -> a dir holding vdev0.conf with that many low bits set
  d="${work}/cu$1"; mkdir -p "${d}"
  cu_words "$1"
  printf 'PciBusId: %s\ncu_mask: %s\ncu_mask: %s\ncu_count: %d\nmem: %d MiB\ndevice_id: 0\nvdev_id: 0\npipe_id: %d\nenable: 1\n' \
    "${bdf}" "${CU_LO}" "${CU_HI}" "$1" "${MEM}" "$2" > "${d}/vdev0.conf"
  echo "${d}"
}

# The unsliced card first: it gives the full CU count the widest mask is built from.
whole="$(run_bench "")"
whole_cu="$(field "${whole}" 1)"; whole_gf="$(field "${whole}" 2)"
if [ -n "${whole_cu}" ] && [ -n "${whole_gf}" ]; then
  row PASS "unsliced card" "${whole_cu} CU / ${whole_gf} GFLOPS"
else
  row FAIL "unsliced card" "$(printf '%s' "${whole}" | tail -2 | tr '\n' ' ')"; echo "FAILS=1"; exit 0
fi

narrow_gf=""; wide_gf=""; prev_gf=0; prev_w=""; pipe=0
for w in ${WIDTHS} "${whole_cu}"; do
  [ "${w}" -le "${whole_cu}" ] || { row WARN "width ${w}" "beyond the card's ${whole_cu} CU, skipped"; continue; }
  o="$(run_bench "$(mask_dir "${w}" "${pipe}")")"
  pipe=$(( pipe + 1 ))
  cu="$(field "${o}" 1)"; gf="$(field "${o}" 2)"
  if [ -z "${cu}" ] || [ -z "${gf}" ]; then
    row FAIL "width ${w} runs" "$(printf '%s' "${o}" | tail -2 | tr '\n' ' ')"; fails=$((fails+1)); continue
  fi
  [ "${cu}" = "${w}" ] \
    && row PASS "width ${w}: reported CU == mask weight" "${cu}" \
    || { row FAIL "width ${w}: reported CU == mask weight" "got ${cu}"; fails=$((fails+1)); }
  row PASS "width ${w}: throughput" "${gf} GFLOPS"
  [ -z "${narrow_gf}" ] && narrow_gf="${gf}"
  wide_gf="${gf}"; prev_gf="${gf}"; prev_w="${w}"
done

# The ends, not the adjacent pairs: see the header.
if [ -n "${narrow_gf}" ] && [ -n "${wide_gf}" ] && [ "${narrow_gf}" -lt "${wide_gf}" ]; then
  row PASS "throughput rises with the mask" "${narrow_gf} -> ${wide_gf} GFLOPS"
else
  row FAIL "throughput rises with the mask" "narrow=${narrow_gf:-none} wide=${wide_gf:-none}"; fails=$((fails+1))
fi

# A specific ratio, so noise cannot satisfy it.
if [ -n "${narrow_gf}" ] && [ -n "${wide_gf}" ] && [ $(( narrow_gf * 100 )) -lt $(( wide_gf * RATIO )) ]; then
  row PASS "the narrow mask is well under the wide one" "${narrow_gf} < ${RATIO}% of ${wide_gf}"
else
  row FAIL "the narrow mask is under ${RATIO}% of the wide one" "narrow=${narrow_gf:-none} wide=${wide_gf:-none}"; fails=$((fails+1))
fi

# A full mask must cost nothing: otherwise the rows above measure overhead, not isolation.
# Compared within a 25% band in BOTH directions, which is the run-to-run spread of this matmul on a
# card carrying other tenants. The upper bound is not symmetry for its own sake: a full-masked slice
# reading arbitrarily FASTER than the unsliced card means the two runs did not measure the same
# thing -- the baseline was taken while the card was busy, or the figures are not comparable at all
# -- and the row would report a match it did not observe.
if [ "${prev_w}" = "${whole_cu}" ] &&
   [ $(( prev_gf * 100 )) -gt $(( whole_gf * 75 )) ] &&
   [ $(( prev_gf * 100 )) -lt $(( whole_gf * 125 )) ]; then
  row PASS "a full mask matches the unsliced card" "${prev_gf} vs ${whole_gf} GFLOPS"
else
  row FAIL "a full mask matches the unsliced card (within 25% either way)" "sliced=${prev_gf} unsliced=${whole_gf} GFLOPS"; fails=$((fails+1))
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 4" "$(xb_fails "${out}")"
