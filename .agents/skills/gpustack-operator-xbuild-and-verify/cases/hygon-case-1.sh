#!/usr/bin/env bash
#
# HYGON-CASE 1 — vdev.conf shape + the runtime that reads it   (needs /opt/hyhal; NO DCU)
#
#   hygon-case-1.sh
#
# THIS IS THE ONE HYGON CASE THAT NEEDS NO ACCELERATOR. It asks whether the record the Hygon
# allocator renders is the record the vendor runtime parses. Our renderer's side is pinned by Go
# tests (TestVdevConfRenderRoundTrip, TestAllocateVdev_VdevIDIsTheFileOrdinal and TestPackCUMask in
# pkg/devicemanager/allocator/hygon/vdev_test.go); this case reads the other side, the vendor's own
# parser, which was written by different people.
#
# Hygon is unlike the four manufacturers whose slice this skill already measures. They load a
# preload library this repository builds, so a case can assert against a shim we own. Hygon has no
# such artifact — `csrc/` carries none and the operator image has no xbuild-hygon stage. The whole
# slice is one file: the allocator renders `vdev<N>.conf` (vdevConf.render in
# pkg/devicemanager/allocator/hygon/vdev.go) into the pod work dir, mounts it read-only at
# /etc/vdev/docker/, and the vendor's DTK/hyhal user-space runtime reads it. So the artifact under
# test is a FILE FORMAT, and the party that enforces it is libhsa-runtime64.so.
#
# WHY THE PARSER'S OWN STRINGS ARE THE ORACLE. A case that only re-read our renderer would agree
# with itself. libhsa-runtime64.so carries the field names it accepts, the path it reads them
# from, and — most usefully — the diagnostics it emits when a record is wrong. Those diagnostics
# name the two consistency rules that are invisible in the file itself:
#
#   "Parse cu_count field failed ... inconsistent with hamming weight of cu mask field"
#   "Parse vdev_id field failed ... inconsistent with configuration file associated value"
#
# The second one is why this case exists in its current form. `vdev_id` must equal the ordinal in
# the record's OWN FILE NAME, and a mismatch is not a degraded slice — measured in HYGON-CASE 2,
# the container is left with no accelerator at all. A vdev id drawn from a node-wide pool
# therefore breaks the second pod to land on a node while the first stays healthy, which is a
# failure no single-pod test can see. This case confirms the shipped parser still enforces the
# rule, where it is cheap to check, so the expensive cases do not have to be the only thing
# standing between that regression and a cluster.
#
# The rows are the parser's own evidence that each field name our renderer emits, the container
# config directory, the per-file name and both diagnostics are present in the shipped library.
#
# Env: XB_HYHAL (default /opt/hyhal). Prints a STATUS | CHECK | DETAIL table; exits non-zero on
#      any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

echo "# HYGON-CASE 1 — vdev.conf parser evidence on $(xtarget_desc)"

out="$(xsh \
  HYHAL="${XB_HYHAL:-/opt/hyhal}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

# --- the vendor parser's own evidence ---------------------------------------------------------
lib="$(ls "${HYHAL}"/lib/libhsa-runtime64.so.1.* 2>/dev/null | head -1)"
if [ -z "${lib}" ]; then
  row FAIL "libhsa-runtime64 present" "none under ${HYHAL}/lib"; fails=$((fails+1))
else
  row PASS "libhsa-runtime64 present" "${lib}"
  syms="$(strings "${lib}" 2>/dev/null)"
  for tok in 'PciBusId:' 'cu_mask:' 'cu_count:' 'mem:' 'device_id:' 'vdev_id:' 'pipe_id:' 'enable:'; do
    printf '%s\n' "${syms}" | grep -qxF "${tok}" \
      && row PASS "parser knows ${tok}" ok \
      || { row FAIL "parser knows ${tok}" "absent from ${lib}"; fails=$((fails+1)); }
  done
  printf '%s\n' "${syms}" | grep -qxF '/etc/vdev/docker/' \
    && row PASS "parser reads /etc/vdev/docker/" "the path the allocator mounts" \
    || { row FAIL "parser reads /etc/vdev/docker/" absent; fails=$((fails+1)); }
  printf '%s\n' "${syms}" | grep -qxF '%s/vdev%u.conf' \
    && row PASS "parser names vdev<N>.conf" "%s/vdev%u.conf" \
    || { row FAIL "parser names vdev<N>.conf" absent; fails=$((fails+1)); }
  printf '%s\n' "${syms}" | grep -q 'hamming weight of cu mask' \
    && row PASS "parser enforces cu_count rule" "diagnostic present" \
    || { row FAIL "parser enforces cu_count rule" "diagnostic absent"; fails=$((fails+1)); }
  printf '%s\n' "${syms}" | grep -q 'Parse vdev_id field failed' \
    && row PASS "parser enforces vdev_id rule" "diagnostic present" \
    || { row FAIL "parser enforces vdev_id rule" "diagnostic absent"; fails=$((fails+1)); }
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 1" "$(xb_fails "${out}")" "${out}"
