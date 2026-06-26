#!/usr/bin/env bash
#
# NVIDIA-CASE 1 — Build artifacts + linking   (no GPU; runs on any docker host)
#
#   nvidia-case-1.sh <TARGET>      e.g. nvidia-case-1.sh xbuild-nvidia-cuda-13
#
# Assumes build.sh already built XB_IMAGE for the NVIDIA <TARGET>. Inspects the
# produced HAMi-core artifact:
#   - /out/libvgpu.so exists, mode 0644
#   - ELF machine == the build platform's arch
#   - NEEDED libcuda.so.1 + libnvidia-ml.so.1 (the runtime libs the NVIDIA container
#     runtime injects; the build stage links against them but never ships them — see
#     references/nvidia-hami-core-vgpu.md). These are HARD deps, not weak UND (contrast Ascend).
#
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

TARGET="${1:?usage: nvidia-case-1.sh <TARGET>}"
XB_IMAGE="${XB_IMAGE:-vgpu-build:${TARGET#xbuild-nvidia-cuda-}}"
XB_PLATFORM="${XB_PLATFORM:-}"

if [ -z "${XB_PLATFORM}" ]; then
  a="$(xrun 'uname -m' | tr -d '[:space:]')"
  case "${a}" in x86_64) XB_PLATFORM=linux/amd64 ;; aarch64) XB_PLATFORM=linux/arm64 ;; esac
fi
case "${XB_PLATFORM}" in
  linux/amd64) EXPECT_MACHINE="X86-64" ;;
  linux/arm64) EXPECT_MACHINE="AArch64" ;;
  *) EXPECT_MACHINE="" ;;
esac

echo "# NVIDIA-CASE 1 — ${XB_IMAGE} (expect ${EXPECT_MACHINE:-any}) on $(xtarget_desc)"

out="$(xsh XB_IMAGE="${XB_IMAGE}" EXPECT_MACHINE="${EXPECT_MACHINE}" <<'PAYLOAD'
set -u
docker run --rm -i --entrypoint bash "${XB_IMAGE}" -s "${EXPECT_MACHINE}" <<'INNER'
set -u
EXPECT="${1:-}"
row(){ printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0
LV=/out/libvgpu.so
if [ -f "$LV" ]; then
  m=$(stat -c '%a' "$LV"); [ "$m" = 644 ] && row PASS "libvgpu.so mode 0644" "$m" || { row FAIL "libvgpu.so mode 0644" "$m"; fails=$((fails+1)); }
else row FAIL "libvgpu.so exists" missing; fails=$((fails+1)); fi
mach=$(readelf -h "$LV" 2>/dev/null | awk -F: '/Machine/{print $2}' | xargs)
if [ -z "$EXPECT" ] || echo "$mach" | grep -q "$EXPECT"; then row PASS "arch libvgpu.so" "$mach"; else row FAIL "arch libvgpu.so==$EXPECT" "$mach"; fails=$((fails+1)); fi
nd=$(readelf -d "$LV" 2>/dev/null | grep NEEDED)
for need in libcuda.so.1 libnvidia-ml.so.1; do
  echo "$nd" | grep -q "$need" && row PASS "NEEDED $need" ok || { row FAIL "NEEDED $need" absent; fails=$((fails+1)); }
done
row INFO "deps are HARD (runtime-injected)" "no weak-UND preload needed (contrast Ascend dcmi)"
echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "NVIDIA-CASE 1: PASS"; exit 0; } || { echo "NVIDIA-CASE 1: FAIL"; exit 1; }
