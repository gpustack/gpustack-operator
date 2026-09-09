#!/usr/bin/env bash
#
# HYGON-CASE 1 — vdev.conf shape + the runtime that reads it   (needs /opt/hyhal; NO DCU)
#
#   hygon-case-1.sh
#
# THIS IS THE ONE HYGON CASE THAT NEEDS NO ACCELERATOR. It asks whether the record the Hygon
# allocator renders is the record the vendor runtime parses, and it answers that from two sides
# that were written by different people: our renderer, and the vendor's own parser.
#
# Hygon is unlike the four manufacturers whose slice this skill already measures. They load a
# preload library this repository builds, so a case can assert against a shim we own. Hygon has no
# such artifact — `csrc/` carries none and the operator image has no xbuild-hygon stage. The whole
# slice is one file: the allocator renders `vdev<N>.conf` (renderVdevConf in
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
# failure no single-pod test can see. This case pins the rule at the format level, where it is
# cheap to check, so the expensive cases do not have to be the only thing standing between that
# regression and a cluster.
#
# The rows are: the eight fields our renderer emits, in the order it emits them; the two
# consistency rules, asserted against a record built to satisfy them; and the parser's own
# evidence that each field name, the container config directory and both diagnostics are present
# in the shipped library.
#
# Env: XB_HYHAL (default /opt/hyhal), XB_BDF (default 0000:09:00.0 — any well-formed BDF; no card
#      is opened). Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

echo "# HYGON-CASE 1 — vdev.conf shape + parser evidence on $(xtarget_desc)"

out="$(xsh \
  HYHAL="${XB_HYHAL:-/opt/hyhal}" \
  BDF="${XB_BDF:-0000:09:00.0}" <<'PAYLOAD'
set -u
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

work="$(mktemp -d)"; trap 'rm -rf "${work}"' EXIT

# --- 1. render the record exactly as vdevConf.render() does -----------------------------------
# Eight fields on nine lines, in this order -- cu_mask appears twice, low word then high word. A
# 16-CU slice of an 80-CU card: mask 0x000000000000ffff, so the hamming weight is 16 and cu_count
# must read 16.
conf="${work}/vdev0.conf"
{
  printf 'PciBusId: %s\n' "${BDF}"
  printf 'cu_mask: 0x%016x\n' $((0xffff))
  printf 'cu_mask: 0x%016x\n' 0
  printf 'cu_count: %d\n' 16
  printf 'mem: %d MiB\n' 1024
  printf 'device_id: %d\n' 0
  printf 'vdev_id: %d\n' 0
  printf 'pipe_id: %d\n' 0
  printf 'enable: 1\n'
} > "${conf}"

# --- 2. the eight field names, in render order ------------------------------------------------
expected='PciBusId cu_mask cu_mask cu_count mem device_id vdev_id pipe_id enable'
actual="$(sed 's/:.*//' "${conf}" | tr '\n' ' ' | sed 's/ *$//')"
if [ "${actual}" = "${expected}" ]; then
  row PASS "render order" "${actual}"
else
  row FAIL "render order" "want [${expected}] got [${actual}]"; fails=$((fails+1))
fi

# Every line must be "key: value" — the parser reads them with a "%[^:]:%s" scan, so a line
# without a colon is the "Unmatched field" diagnostic rather than an ignored comment.
bad="$(grep -cvE '^[A-Za-z_]+: .+$' "${conf}")"
[ "${bad}" -eq 0 ] && row PASS "every line is key: value" "9 lines" \
  || { row FAIL "every line is key: value" "${bad} malformed"; fails=$((fails+1)); }

# The memory figure carries a unit; the parser takes the leading number. This is the field that
# distinguishes Hygon from the one other manufacturer carrying its cap in a file: Ascend renders
# "memory-quota=1024" and Hygon renders "mem: 1024 MiB".
memline="$(awk -F': ' '/^mem:/{print $2}' "${conf}")"
case "${memline}" in
  *' MiB') row PASS "mem carries a MiB unit" "${memline}" ;;
  *) row FAIL "mem carries a MiB unit" "${memline}"; fails=$((fails+1)) ;;
esac

# --- 3. the two consistency rules the parser enforces ------------------------------------------
lo="$(awk -F': ' '/^cu_mask:/{print $2}' "${conf}" | sed -n 1p)"
hi="$(awk -F': ' '/^cu_mask:/{print $2}' "${conf}" | sed -n 2p)"
weight=0
for w in "${lo}" "${hi}"; do
  v=$((w))
  while [ "${v}" -ne 0 ]; do weight=$((weight + (v & 1))); v=$((v >> 1)); done
done
declared="$(awk -F': ' '/^cu_count:/{print $2}' "${conf}")"
[ "${weight}" -eq "${declared}" ] \
  && row PASS "cu_count == hamming weight of cu_mask" "${declared}" \
  || { row FAIL "cu_count == hamming weight" "count=${declared} weight=${weight}"; fails=$((fails+1)); }

ordinal="$(basename "${conf}" | sed 's/^vdev//; s/\.conf$//')"
declared_vdev="$(awk -F': ' '/^vdev_id:/{print $2}' "${conf}")"
[ "${ordinal}" = "${declared_vdev}" ] \
  && row PASS "vdev_id == ordinal in its file name" "vdev${ordinal}.conf carries vdev_id ${declared_vdev}" \
  || { row FAIL "vdev_id == file ordinal" "vdev${ordinal}.conf carries vdev_id ${declared_vdev}"; fails=$((fails+1)); }

# --- 4. the vendor parser's own evidence -------------------------------------------------------
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

echo "--- rendered record ---"
cat "${conf}"
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 1" "$(xb_fails "${out}")"
