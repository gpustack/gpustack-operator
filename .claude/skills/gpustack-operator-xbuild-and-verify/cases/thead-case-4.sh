#!/usr/bin/env bash
#
# THEAD-CASE 4 — Gate 3: per-process utilisation characterisation   (needs a real PPU)
#
#   thead-case-4.sh
#
# Settles a design input rather than exercising an injected library, so it needs no shim
# and does not depend on any other case: can the compute quota's PID loop be fed THIS
# container's per-process utilisation, or must it fall back to card-total (which couples
# every container's controller on the same card and oscillates)?
#
# Builds csrc/thead/ppu-slicing-shim/testing/hgml_util_probe.c inside the SDK devel image and runs
# it against an IDLE card with the PPU device nodes passed through, then reads two
# verdicts out of its output:
#   - util=supported|empty|unsupported|unavailable. Only `supported` passes. `empty` means
#     the call worked and returned no sample, which is the exact shape a false PASS takes
#     here, so it is a FAIL with its own name rather than a pass.
#   - pidns=container|host|unknown. Either concrete answer passes — the design needs to
#     know WHICH, not a particular one. `unknown` means nothing was reported at all.
# The container gets its own PID namespace (no --pid=host), so the probe's own pid is a
# small number and a host pid is not; that is what makes the comparison decisive.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu,
#      on the TARGET), XB_PPU_CARD (default: the first idle card), XB_PPU_IDLE_MIB
#      (default 64), XB_PROBE_ROUNDS (default 3), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL. With no PPU every
# hardware row is SKIP and the case still exits 0.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"
XB_PROBE_ROUNDS="${XB_PROBE_ROUNDS:-3}"

xctr_resolve || { echo "thead-case-4: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 4 — Gate 3 per-process utilisation (image ${XB_IMAGE}) on $(xtarget_desc)"

CARD="${XB_PPU_CARD:-}"
if [ -z "${CARD}" ]; then
  CARD="$(thead_idle_cards | head -1)"
fi

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARD="${CARD}" ROUNDS="${XB_PROBE_ROUNDS}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

# The probe is a staged artifact, built by the build step along with everything else here, so a
# broken probe is a build failure there rather than a hardware SKIP here.
if [ ! -x "${STAGE}/hgml_util_probe" ]; then
  row FAIL "probe staged" "${STAGE}/hgml_util_probe missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi
row INFO "probe staged" "hgml_util_probe sha256=$(sha256sum "${STAGE}/hgml_util_probe" | cut -c1-16)…"

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  row SKIP "util supported at runtime" "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
  row SKIP "reported pid namespace" "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
  echo "FAILS=${fails}"; exit 0
fi
if [ -z "${CARD}" ]; then
  row SKIP "util supported at runtime" "no idle card (every card busy at or above XB_PPU_IDLE_MIB / non-zero util)"
  row SKIP "reported pid namespace" "no idle card (every card busy at or above XB_PPU_IDLE_MIB / non-zero util)"
  echo "FAILS=${fails}"; exit 0
fi
if [ ! -e "/dev/alixpu_ppu${CARD}" ]; then
  row SKIP "util supported at runtime" "/dev/alixpu_ppu${CARD} absent for the chosen card"
  row SKIP "reported pid namespace" "/dev/alixpu_ppu${CARD} absent for the chosen card"
  echo "FAILS=${fails}"; exit 0
fi

# The two control nodes plus this card only: the shim design mounts exactly these, and a
# probe that saw every card could not tell a per-card answer from a card-total one.
# shellcheck disable=SC2086
probe="$(${XB_CTR} ${XB_CTR_ARGS} run --rm --platform linux/amd64 \
  --device /dev/alixpu --device /dev/alixpu_ctl --device "/dev/alixpu_ppu${CARD}" \
  -v "${STAGE}:/work" -w /work "${IMG}" \
  ./hgml_util_probe "0" "${ROUNDS}" 2>&1)"

echo "--- probe output (card ${CARD}) ---"
echo "${probe}" | grep -E '^PROBE ' || echo "${probe}"

util="$(echo "${probe}" | sed -nE 's/^PROBE VERDICT util=([a-z-]+).*/\1/p' | tail -1)"
pidns="$(echo "${probe}" | sed -nE 's/^PROBE VERDICT pidns=([a-z]+).*/\1/p' | tail -1)"
self_pid="$(echo "${probe}" | sed -nE 's/^PROBE self_pid=([0-9]+).*/\1/p' | tail -1)"

case "${util}" in
  supported)
    row PASS "util supported at runtime" "hgmlDeviceGetProcessUtilization returned samples; PID feedback can be per-process"
    ;;
  empty)
    row FAIL "util supported at runtime" "call succeeded but returned no sample under load — not support; PID feedback falls back to card-total"
    fails=$((fails+1))
    ;;
  others-only)
    # The query asks for all history, so a neighbouring container's stale sample makes the
    # count non-zero without this process ever appearing. Per-process feedback needs OUR
    # sample, so counting someone else's would be the quietest false PASS available here.
    row FAIL "util supported at runtime" "samples returned, but none for this process under its own load — per-process feedback is not available"
    fails=$((fails+1))
    ;;
  unsupported | unavailable)
    row FAIL "util supported at runtime" "verdict ${util} — PID feedback falls back to card-total, accepting the controller-coupling risk"
    fails=$((fails+1))
    ;;
  *)
    row FAIL "util supported at runtime" "no verdict parsed from the probe output"; fails=$((fails+1))
    ;;
esac

case "${pidns}" in
  container)
    row PASS "reported pid namespace" "container (a reported pid equals the probe's own ${self_pid:-?})"
    ;;
  host)
    row PASS "reported pid namespace" "host (no reported pid equals the probe's own ${self_pid:-?}) — the loop must translate pids"
    ;;
  *)
    row FAIL "reported pid namespace" "unknown — neither utilisation nor the process list reported any pid"
    fails=$((fails+1))
    ;;
esac

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "THEAD-CASE 4: PASS"; exit 0; } || { echo "THEAD-CASE 4: FAIL"; exit 1; }
