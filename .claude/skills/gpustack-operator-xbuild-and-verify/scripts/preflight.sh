#!/usr/bin/env bash
#
# preflight.sh — detect the target environment (local or ssh).
#
#   XB_MODE=local                      bash .../preflight.sh
#   XB_MODE=ssh XB_HOST=root@host       bash .../preflight.sh
#   XB_MODE=pty XB_HOST=user@proxy      bash .../preflight.sh   (interactive-shell-only target)
#
# Prints a STATUS | CHECK | DETAIL table. STATUS is PASS / WARN / FAIL.
#   - run-capable is the ONLY required check: with nothing able to execute a case, none can run.
#     A container runtime answering on the target => PASS; no runtime but an in-place ROCm
#     toolchain => WARN (the AMD arm works there, no other backend does); neither => FAIL.
#   - build-capable (docker buildx) is a WARN, not a FAIL: a host without it still runs
#     every case against an image built on a docker host and loaded here. No buildkitd
#     is ever started — a PPU/NPU host is usually someone else's production node.
#   - npu-smi / nvidia-smi / ppu-smi / rocm-smi, the vendor runtimes and
#     /dev/{davinci,nvidia,alixpu,kfd}* are required only for the HARDWARE cases; absent => WARN.
# Exits non-zero if any required check FAILs.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

# Resolve the runtime here rather than in the payload so the table can name what was
# probed. An unresolvable runtime is the payload's single FAIL row, not a hard exit.
xctr_resolve || true

echo "# preflight on $(xtarget_desc)"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

# OS
if [ -r /etc/os-release ]; then
  os="$(. /etc/os-release; echo "${NAME} ${VERSION}")"
  row PASS "os-release" "${os} ($(uname -m), $(uname -r))"
else
  row WARN "os-release" "not found ($(uname -m))"
fi

# run-capable (required): the resolved runtime must exist AND its daemon must answer,
# because a present binary with a dead daemon runs no container either.
#
# A target that IS a container has neither, and is still usable for one backend: a rented
# single-card instance carries ROCm on its own filesystem, which is what build.sh's AMD arm
# compiles against in place. That is a WARN and not a PASS, because it is a genuinely degraded
# target — every other backend's cases still need a runtime and cannot run there at all.
sv=""
if [ -n "${XB_CTR}" ] && command -v "${XB_CTR}" >/dev/null 2>&1; then
  # shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
  sv="$("${XB_CTR}" ${XB_CTR_ARGS} info --format '{{.ServerVersion}}' 2>/dev/null | tail -1)"
fi
if [ -n "${sv}" ]; then
  row PASS "run-capable" "${XB_CTR} ${XB_CTR_ARGS} -> server ${sv}"
elif [ -x "${ROCM_PATH:-/opt/rocm}/bin/hipcc" ]; then
  row WARN "run-capable" "no container runtime, but ${ROCM_PATH:-/opt/rocm}/bin/hipcc is here — the AMD arm compiles and runs in place; every other backend needs a runtime"
elif [ -n "${XB_CTR}" ]; then
  row FAIL "run-capable" "${XB_CTR} found but its daemon does not answer 'info'"; fails=$((fails+1))
else
  row FAIL "run-capable" "no container runtime (XB_CTR='${XB_CTR}'; probed docker, nerdctl) and no in-place ROCm"; fails=$((fails+1))
fi

# build-capable (WARN only): buildx builds the multi-stage Dockerfiles (heredoc +
# RUN --mount). A host without it is not blocked — build elsewhere, load here.
if docker buildx version >/dev/null 2>&1; then
  row PASS "build-capable" "$(docker buildx version 2>/dev/null | head -1)"
else
  row WARN "build-capable" "docker buildx absent — build the image on a docker host, then load it here (${XB_CTR:-<runtime>} load < image.tar); no buildkitd is started"
fi

# --- hardware checks (WARN only; needed for ASCEND-CASE 2/3/4) ---
if command -v npu-smi >/dev/null 2>&1; then
  n="$(npu-smi info 2>/dev/null | grep -cE '910|310|Ascend' || true)"
  row PASS "npu-smi" "present (chips matched: ${n})"
else
  row WARN "npu-smi" "absent — ASCEND-CASE 2/3/4 (hardware) unavailable"
fi

if docker info 2>/dev/null | grep -qiE 'Runtimes:.*ascend|Default Runtime: ascend'; then
  row PASS "ascend-runtime" "$(docker info 2>/dev/null | grep -i 'Default Runtime' | tr -s ' ')"
else
  row WARN "ascend-runtime" "ascend docker runtime not detected — ASCEND-CASE 2/3/4 need it"
fi

if ls /dev/davinci0 >/dev/null 2>&1; then
  row PASS "davinci-devices" "$(ls /dev/davinci[0-9]* 2>/dev/null | tr '\n' ' ')"
else
  row WARN "davinci-devices" "/dev/davinci* absent — ASCEND-CASE 2/3/4 need a real NPU"
fi

# --- NVIDIA hardware checks (WARN only; needed for NVIDIA-CASE 2/3) ---
if command -v nvidia-smi >/dev/null 2>&1; then
  g="$(nvidia-smi -L 2>/dev/null | grep -c '^GPU ' || true)"
  row PASS "nvidia-smi" "present (GPUs: ${g}; NVIDIA-CASE 3 needs >= 2)"
else
  row WARN "nvidia-smi" "absent — NVIDIA-CASE 2/3 (hardware) unavailable"
fi

if docker info 2>/dev/null | grep -qiE 'Runtimes:.*nvidia|Default Runtime: nvidia'; then
  row PASS "nvidia-runtime" "$(docker info 2>/dev/null | grep -i 'Default Runtime' | tr -s ' ')"
else
  row WARN "nvidia-runtime" "nvidia docker runtime not detected — NVIDIA-CASE 2/3 need it (or run with --gpus all)"
fi

if ls /dev/nvidia0 >/dev/null 2>&1; then
  row PASS "nvidia-devices" "$(ls /dev/nvidia[0-9]* 2>/dev/null | tr '\n' ' ')"
else
  row WARN "nvidia-devices" "/dev/nvidia* absent — NVIDIA-CASE 2/3 need a real GPU"
fi

# --- THead PPU hardware checks (WARN only; needed for THEAD-CASE 2/3/4/5) ---
# ppu-smi lives in the SDK's own bin subtree, not on PATH in every install, so probe
# both. It also exits 0 when the driver is not loaded, printing "init HGML error" —
# so the row is decided by counting cards in its OUTPUT, never by its exit status.
ppu_smi=""
for p in "${PPU_HOME:-/usr/local/PPU_SDK}/ppu-smi/bin/ppu-smi" "$(command -v ppu-smi 2>/dev/null)"; do
  [ -n "${p}" ] && [ -x "${p}" ] && { ppu_smi="${p}"; break; }
done
if [ -n "${ppu_smi}" ]; then
  # Count the per-card rows ("| 0  PPU-ZW810E ..."), not every PPU- token: the banner
  # says "PPU-SMI" and a column header says "PPU-Util", which would inflate the count.
  n="$("${ppu_smi}" 2>/dev/null | grep -cE '^\| *[0-9]+ +PPU-' || true)"
  if [ "${n}" -gt 0 ]; then
    row PASS "ppu-smi" "${ppu_smi} (cards matched: ${n})"
  else
    row WARN "ppu-smi" "${ppu_smi} present but reports no card (driver not loaded?); its exit status is 0 either way"
  fi
else
  row WARN "ppu-smi" "absent — THEAD-CASE 2/3/5 (hardware) unavailable"
fi

if [ -e /dev/alixpu ] && [ -e /dev/alixpu_ctl ]; then
  row PASS "alixpu-devices" "/dev/alixpu + /dev/alixpu_ctl + $(ls -d /dev/alixpu_ppu[0-9]* 2>/dev/null | wc -l | tr -d ' ') card nodes"
else
  row WARN "alixpu-devices" "/dev/alixpu or /dev/alixpu_ctl absent — THEAD-CASE 2/3/5 need real cards"
fi

if lsmod 2>/dev/null | grep -q '^alixpu '; then
  row PASS "alixpu-module" "loaded (refcount $(lsmod | awk '$1=="alixpu"{print $3}'))"
else
  row WARN "alixpu-module" "kernel module alixpu not loaded — THEAD-CASE 2/3/5 need it"
fi

# --- AMD ROCm hardware checks (WARN only; needed for AMD-CASE 2..7) ---
# rocm-smi is probed BOTH ways: a ROCm install always puts it under the install prefix and only
# some distributions also drop it on PATH, so a row that probed one of the two would report a
# working host as unavailable.
rocm_smi=""
for p in "$(command -v rocm-smi 2>/dev/null)" "${ROCM_PATH:-/opt/rocm}/bin/rocm-smi"; do
  [ -n "${p}" ] && [ -x "${p}" ] && { rocm_smi="${p}"; break; }
done
if [ -n "${rocm_smi}" ]; then
  # Count DISTINCT indices: --showid prints several lines per card, so a plain line count reports
  # a multiple of the card count.
  n="$("${rocm_smi}" --showid 2>/dev/null | grep -oE '^GPU\[[0-9]+\]' | sort -u | wc -l | tr -d ' ')"
  # Decided by the count, not by the file existing. rocm-smi is installed by the ROCm package and
  # runs whether or not the driver is loaded, so a row that stopped at "it is on disk" would
  # advertise a card to AMD-CASE 2..7 on a host that has none.
  if [ "${n}" -gt 0 ]; then
    row PASS "rocm-smi" "${rocm_smi} (cards matched: ${n})"
  else
    row WARN "rocm-smi" "${rocm_smi} present but reports no card (driver not loaded?) — AMD-CASE 2..7 (hardware) unavailable"
  fi
else
  row WARN "rocm-smi" "absent on PATH and under ${ROCM_PATH:-/opt/rocm}/bin — AMD-CASE 2..7 (hardware) unavailable"
fi

# /dev/kfd is what HIP opens; the render nodes are what a container has to be given. Not every
# /dev/dri/renderD* belongs to an AMD card — a host with integrated graphics carries one that
# does not, and counting nodes rather than amdgpu-backed ones advertises a card HIP will never
# enumerate — so the row counts the ones whose driver is amdgpu and says how many it skipped.
if [ -e /dev/kfd ]; then
  amd_nodes=0
  all_nodes=0
  for d in /sys/class/drm/renderD*; do
    [ -e "${d}/device/driver" ] || continue
    all_nodes=$((all_nodes+1))
    [ "$(basename "$(readlink -f "${d}/device/driver")")" = amdgpu ] && amd_nodes=$((amd_nodes+1))
  done
  # /dev/kfd alone is not a card: the amdgpu module creates it, and a host whose only render node
  # belongs to integrated graphics has the file and nothing HIP will enumerate. The count is the
  # row, and zero amdgpu nodes is the same answer as no /dev/kfd.
  if [ "${amd_nodes}" -gt 0 ]; then
    row PASS "kfd-devices" "/dev/kfd present; ${amd_nodes} amdgpu render node(s) of ${all_nodes}"
  else
    row WARN "kfd-devices" "/dev/kfd present but 0 of ${all_nodes} render node(s) are amdgpu — AMD-CASE 2..7 need a real GPU"
  fi
else
  row WARN "kfd-devices" "/dev/kfd absent — AMD-CASE 2..7 need a real GPU"
fi

if lsmod 2>/dev/null | grep -q '^amdgpu '; then
  row PASS "amdgpu-module" "loaded (refcount $(lsmod | awk '$1=="amdgpu"{print $3}'))"
else
  row WARN "amdgpu-module" "kernel module amdgpu not loaded or not visible from here — AMD-CASE 2..7 need it"
fi

# NUM_XCC decides WHICH conformance table AMD-CASE 4/5 assert, so it is reported before anything
# is built: a run that assumed the wrong table would fail rows that are correct for the card in
# front of it, and the two tables share no arithmetic. It is read straight out of the KFD topology
# rather than derived — the one figure in that directory safe to take at face value, since the
# neighbouring array_count has already been multiplied by it and anything divided out of that file
# is wrong on exactly the hardware this row exists to identify. CPU nodes carry no such property,
# so only GPU nodes contribute.
xcc="$(cat /sys/class/kfd/kfd/topology/nodes/*/properties 2>/dev/null \
       | awk '$1=="num_xcc"{print $2}' | sort -u | tr '\n' ',' | sed 's/,$//')"
if [ -n "${xcc}" ]; then
  row PASS "amd-num-xcc" "NUM_XCC=${xcc} (1 selects the RDNA conformance table, >1 the CDNA one)"
else
  row WARN "amd-num-xcc" "no GPU node in the KFD topology — AMD-CASE 4/5 cannot pick a conformance table"
fi

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"

# Mirror the payload's FAILS into our exit code so callers (and CI) can detect a
# failed required check from the return status, not just by reading the table.
echo "${out}" | grep -q 'FAILS=0' && exit 0 || exit 1
