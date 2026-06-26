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
#   - both NEEDED libc_sec.so + libascendcl.so (the runtime deps; libc_sec is the
#     securec/libboundscheck library — see references/ascend-ld-preload-and-libdcmi.md)
#   - enpu-monitor carries WEAK UND dcmi_* symbols (informational: that is why
#     libdcmi must be preloaded at runtime — ASCEND-CASE 2 covers it)
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
  nd=$(readelf -d "$f" 2>/dev/null | grep NEEDED)
  for need in libc_sec.so libascendcl.so; do
    echo "$nd" | grep -q "$need" && row PASS "NEEDED $need ($(basename $f))" ok || { row FAIL "NEEDED $need ($(basename $f))" absent; fails=$((fails+1)); }
  done
done
w=$(readelf -sW "$EM" 2>/dev/null | grep -cE "WEAK +DEFAULT +UND +dcmi" || true)
row INFO "enpu-monitor weak UND dcmi syms" "${w} (=> libdcmi must be preloaded at runtime, see ASCEND-CASE 2)"
echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "ASCEND-CASE 1: PASS"; exit 0; } || { echo "ASCEND-CASE 1: FAIL"; exit 1; }
