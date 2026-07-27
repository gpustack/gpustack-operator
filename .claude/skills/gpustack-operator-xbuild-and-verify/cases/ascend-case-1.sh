#!/usr/bin/env bash
#
# ASCEND-CASE 1 — Build artifacts + linking   (no NPU; runs on any docker host)
#
#   ascend-case-1.sh <TARGET>
#
# Assumes build.sh already built XB_IMAGE for <TARGET>. A successful build IS the
# link test: the enpu-monitor executable only links because build-libvnpu.sh passes
# -Wl,--allow-shlib-undefined (the vendor .so cross-refs — HAL drv*, ErrorManager::* —
# resolve at runtime, not link time). This case then inspects the produced artifacts:
#   - /out/lib/libvruntime.so   exists, mode 0644
#   - /out/tools/enpu-monitor   exists, mode 0755
#   - both ELF machine == the build platform's arch
#   - both NEEDED libc_sec.so (the securec/libboundscheck library — see
#     references/ascend-ld-preload-and-libdcmi.md)
#   - neither NEEDED libascendcl.so: since ubs-virt 476bb968 both targets dropped
#     `ascendcl` from their link line and enpu-monitor dlopen()s libruntime.so and
#     resolves the rt entry table itself (src/tools/monitor.c::load_rt_for_monitor).
#     Asserted in the negative so a silent revert upstream is caught.
#   - libvruntime.so defines the rt interposition surface (80 global rt-prefixed FUNCs
#     at 476bb968 = 65 rt[A-Z] + 15 lowercase-s rts* STARS/RTS entries) and NO
#     dcmi_*/dsmi_* definitions — it hooks the CANN runtime layer only and is a
#     *client* of dcmi. See references/ascend-npu-smi-and-aicore.md.
#   - both carry WEAK UND dcmi_* symbols: that is why libdcmi must be preloaded at
#     runtime (with nothing preloaded, enpu-monitor SIGSEGVs — ASCEND-CASE 2/4)
#
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:?usage: ascend-case-1.sh <TARGET>}"
XB_IMAGE="${XB_IMAGE:-vcann-build:${TARGET#xbuild-ascend-cann-}}"
XB_PLATFORM="${XB_PLATFORM:-}"

# Expected ELF machine from the platform; if unset, derive from the target host arch.
if [ -z "${XB_PLATFORM}" ]; then
  a="$(xrun 'uname -m' | tr -d '[:space:]')"
  case "${a}" in x86_64) XB_PLATFORM=linux/amd64 ;; aarch64) XB_PLATFORM=linux/arm64 ;; esac
fi
case "${XB_PLATFORM}" in
  linux/amd64) EXPECT_MACHINE="X86-64" ;;
  linux/arm64) EXPECT_MACHINE="AArch64" ;;
  *) EXPECT_MACHINE="" ;;
esac

echo "# ASCEND-CASE 1 — ${XB_IMAGE} (expect ${EXPECT_MACHINE:-any}) on $(xtarget_desc)"

out="$(xsh XB_IMAGE="${XB_IMAGE}" EXPECT_MACHINE="${EXPECT_MACHINE}" <<'PAYLOAD'
set -u
docker run --rm -i "${XB_IMAGE}" bash -s "${EXPECT_MACHINE}" <<'INNER'
set -u
EXPECT="${1:-}"
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
LV=/out/lib/libvruntime.so
EM=/out/tools/enpu-monitor
chk_mode(){ local f="$1" want="$2" name="$3" m; if [ -f "$f" ]; then m=$(stat -c '%a' "$f"); [ "$m" = "$want" ] && row PASS "$name mode $want" "$m" || { row FAIL "$name mode $want" "$m"; fails=$((fails+1)); }; else row FAIL "$name exists" missing; fails=$((fails+1)); fi; }
chk_mode "$LV" 644 "libvruntime.so"
chk_mode "$EM" 755 "enpu-monitor"
for f in "$LV" "$EM"; do
  mach=$(readelf -h "$f" 2>/dev/null | awk -F: '/Machine/{print $2}' | xargs)
  if [ -z "$EXPECT" ] || echo "$mach" | grep -q "$EXPECT"; then row PASS "arch $(basename $f)" "$mach"; else row FAIL "arch $(basename $f)==$EXPECT" "$mach"; fails=$((fails+1)); fi
done
for f in "$LV" "$EM"; do
  # Gate the absence assertions on readelf having produced output at all: an empty
  # $nd makes every `grep -q` fail, which would otherwise print PASS on no evidence.
  nd=$(readelf -d "$f" 2>/dev/null | grep NEEDED)
  if [ -z "$nd" ]; then
    row FAIL "readelf -d ($(basename $f))" "no NEEDED output — cannot judge linking"; fails=$((fails+1)); continue
  fi
  echo "$nd" | grep -q libc_sec.so && row PASS "NEEDED libc_sec.so ($(basename $f))" ok || { row FAIL "NEEDED libc_sec.so ($(basename $f))" absent; fails=$((fails+1)); }
  if echo "$nd" | grep -q libascendcl.so; then
    row FAIL "NEEDED libascendcl.so absent ($(basename $f))" "present — upstream reverted the dlopen change?"; fails=$((fails+1))
  else
    row PASS "NEEDED libascendcl.so absent ($(basename $f))" "ok (dropped at ubs-virt 476bb968)"
  fi
done
# libvruntime.so's rt exports ARE the interposition surface; dcmi/dsmi are only called.
# Match rt[A-Za-z], not rt[A-Z]: 15 of the 80 hooks are the lowercase-s STARS/RTS entry
# points (rtsLaunchKernelWithConfig, rtsModelExecute, …), which rt[A-Z] silently drops.
syms=$(readelf -sW "$LV" 2>/dev/null)
if [ -z "$syms" ]; then
  row FAIL "readelf -sW libvruntime.so" "no symbol output — cannot judge the hook surface"; fails=$((fails+1))
else
  h=$(echo "$syms" | grep -cE "FUNC +GLOBAL +DEFAULT +[0-9]+ rt[A-Za-z]" || true)
  hs=$(echo "$syms" | grep -cE "FUNC +GLOBAL +DEFAULT +[0-9]+ rts[A-Z]" || true)
  [ "${h}" -gt 0 ] && row PASS "libvruntime.so rt hooks defined" "${h} (incl. ${hs} rts* STARS entries)" \
                   || { row FAIL "libvruntime.so rt hooks defined" 0; fails=$((fails+1)); }
  # A numeric Ndx means defined; UND would mean merely called. WEAK/IFUNC included so a
  # weak *definition* of dcmi_*/dsmi_* cannot slip past (upstream already emits weak dcmi UNDs).
  d=$(echo "$syms" | grep -cE "(FUNC|IFUNC) +(GLOBAL|WEAK) +DEFAULT +[0-9]+ (dcmi|dsmi)" || true)
  [ "${d}" -eq 0 ] && row PASS "libvruntime.so defines no dcmi/dsmi" "0 (dcmi client, not interposer)" || { row FAIL "libvruntime.so defines no dcmi/dsmi" "${d}"; fails=$((fails+1)); }
fi
for f in "$LV" "$EM"; do
  w=$(readelf -sW "$f" 2>/dev/null | grep -cE "WEAK +DEFAULT +UND +dcmi" || true)
  [ "${w}" -gt 0 ] && row PASS "weak UND dcmi syms ($(basename $f))" "${w} (=> libdcmi must be preloaded, see ASCEND-CASE 2/4)" \
                   || { row FAIL "weak UND dcmi syms ($(basename $f))" 0; fails=$((fails+1)); }
done
echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "ASCEND-CASE 1: PASS"; exit 0; } || { echo "ASCEND-CASE 1: FAIL"; exit 1; }
