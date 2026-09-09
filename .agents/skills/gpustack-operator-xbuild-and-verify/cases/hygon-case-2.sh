#!/usr/bin/env bash
#
# HYGON-CASE 2 — Inject one slice: the card reports the quota   (needs a real Hygon DCU)
#
#   hygon-case-2.sh
#
# Reproduces the Hygon logical-slicing injection by hand and asks the container what card it is
# holding. This is the case that turns "the allocator produced an injection" into "the accelerator
# was actually sliced", which is the whole distinction between the simulated and the measured
# depth of `device-manager preflight`.
#
# WHAT IS INJECTED, and why each part. getSlicedContainerAllocateResponse in
# pkg/devicemanager/allocator/hygon/deviceplugin.go emits exactly this and nothing else:
#   /dev/kfd, /dev/mkfd            the node-level control nodes, shared by every allocated card
#   /dev/dri/card<N>, renderD<M>   the accelerator's own two drm nodes
#   /opt/dtk  -> /opt/hygondriver  the DTK user-space runtime (HYGONPATH)
#   /opt/hyhal -> /opt/hyhal       the hyhal runtime, which is where libhsa-runtime64.so lives
#   <podWorkDir>/etc/vdev/docker -> /etc/vdev/docker   the slice itself, read-only
# There is NO preload library and NO environment variable: the cap is the file, and the vendor
# runtime is what enforces it. Omitting /opt/hyhal does not degrade the slice, it removes the
# runtime — a probe image without it fails with "open hydmilib:libhydmi.so error" and reports no
# HIP device at all, which is why the mount list here is the allocator's in full rather than the
# subset a reader might guess is enough.
#
# WHY THE READER IS PYTORCH AND NOT hy-smi. hy-smi answers from the DMI layer and reports the
# PHYSICAL card: measured on an 8-DCU host it prints 65520 MiB inside a container capped at 1024.
# That is not a bug in the slice, it is the wrong question — the cap binds the HSA/HIP runtime the
# workload uses, so the reader has to be a HIP client. torch.cuda.get_device_properties reports
# total_memory and multi_processor_count, which are the two fields the record sets, so one call
# answers both halves. (rocminfo is not usable here either: it enumerates zero GPU agents on this
# platform even on the host.)
#
# THE NEGATIVE ROW IS THE POINT OF THIS CASE. `vdev_id` must equal the ordinal in the file name
# the record was read from. A record that breaks the rule does not yield a smaller slice or a
# warning — the container is left with NO accelerator ("No HIP GPUs are available"). That failure
# mode is invisible to a single-pod test, because a node-wide vdev id pool hands the FIRST pod an
# id that happens to match and only breaks the second. So this case renders the same record twice,
# once named to match and once not, and requires the mismatch to lose the device.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE, then quay.io/gpustack/runner:dtk25.04-vllm0.11.0; must
#      carry torch built against DTK), XB_HCU (hy-smi index, default 0), XB_MEM (MiB, default
#      1024), XB_CU (compute units, default 8), XB_HYHAL (/opt/hyhal), XB_DTK (/opt/dtk),
#      XB_CTR / XB_CTR_ARGS (see scripts/lib.sh).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

xctr_resolve || { echo "hygon-case-2: no container runtime on the target"; exit 2; }
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-quay.io/gpustack/runner:dtk25.04-vllm0.11.0}}"

echo "# HYGON-CASE 2 — inject one slice (image ${IMG}, hcu ${XB_HCU:-0}) on $(xtarget_desc)"

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
trap 'rm -rf "${work}"; ctr rm -f "gpustack-hyc2-${run_tag}" >/dev/null 2>&1' EXIT

# --- resolve the accelerator: bdf -> its two drm minors ---------------------------------------
bdf="$("${HYHAL}/bin/hy-smi" --showbus 2>/dev/null | sed -n "s/^HCU\[${HCU}\][[:space:]]*: PCI Bus: \([0-9a-fA-F:.]*\).*/\1/p" | head -1)"
if [ -z "${bdf}" ]; then row FAIL "resolve HCU ${HCU} bdf" "hy-smi --showbus named none"; echo "FAILS=1"; exit 0; fi
row PASS "resolve HCU ${HCU} bdf" "${bdf}"

drm_minor(){ # $1 = card|renderD
  for p in /sys/class/drm/"$1"*/device; do
    [ -e "${p}" ] || continue
    n="${p#/sys/class/drm/$1}"; n="${n%/device}"
    case "${n}" in ''|*[!0-9]*) continue;; esac
    [ "$(basename "$(readlink -f "${p}")")" = "${bdf}" ] && { echo "${n}"; return; }
  done
}
card="$(drm_minor card)"; render="$(drm_minor renderD)"
if [ -z "${card}" ] || [ -z "${render}" ]; then
  row FAIL "resolve drm nodes" "card=[${card}] renderD=[${render}] for ${bdf}"; echo "FAILS=1"; exit 0
fi
row PASS "resolve drm nodes" "/dev/dri/card${card} /dev/dri/renderD${render}"

# --- the reader ---------------------------------------------------------------------------------
cat > "${work}/reader.py" <<'PY'
import torch
p = torch.cuda.get_device_properties(0)
print("READER total_MiB=%d CU=%d name=%s" % (
    p.total_memory // (1024 * 1024), p.multi_processor_count, p.name))
PY

# CU_LO / CU_HI: the two cu_mask words for $1 contiguous compute units from bit ${2:-0}, which is
# what packCUMask produces for the first slice of a card and keeps cu_count == hamming weight.
#
# Built one bit at a time rather than from a shifted width, because a shell's `<<` is modulo the
# word size: `1 << 64` is 1, not 2^64. A whole-card record on a 128-CU part would otherwise carry
# 64 mask bits under a `cu_count: 128`, and the vendor parser rejects a record for exactly that
# mismatch -- silently, on hardware nobody here has.
cu_words(){
  _lo=0; _hi=0; _b="${2:-0}"; _n=0
  while [ "${_n}" -lt "$1" ]; do
    if [ "${_b}" -lt 64 ]; then _lo=$(( _lo | (1 << _b) )); else _hi=$(( _hi | (1 << (_b - 64)) )); fi
    _b=$(( _b + 1 )); _n=$(( _n + 1 ))
  done
  CU_LO=$(printf '0x%016x' "${_lo}"); CU_HI=$(printf '0x%016x' "${_hi}")
}

# render a record; $1 = target dir, $2 = file ordinal, $3 = vdev_id value, $4 = pipe_id
#
# The pipe id is a parameter because each run below must have its own. The driver keeps a record for
# about a second after the process holding it exits, and a slice reusing that record's pipe id inside
# the window is refused the card outright -- the refusal HYGON-CASE 5 asserts on purpose. Runs that
# followed one another on a single pipe id would take that refusal for the rule they are testing.
render_conf(){
  mkdir -p "$1"
  cu_words "${CU}"
  printf 'PciBusId: %s\ncu_mask: %s\ncu_mask: %s\ncu_count: %d\nmem: %d MiB\ndevice_id: 0\nvdev_id: %d\npipe_id: %d\nenable: 1\n' \
    "${bdf}" "${CU_LO}" "${CU_HI}" "${CU}" "${MEM}" "$3" "$4" > "$1/vdev$2.conf"
}

run_reader(){ # $1 = vdev dir to mount (empty = no slice)
  set -- "$@"
  slice=""
  [ -n "$1" ] && slice="-v $1:/etc/vdev/docker:ro"
  # shellcheck disable=SC2086
  ctr run --rm --name "gpustack-hyc2-${run_tag}" \
    --device=/dev/kfd --device=/dev/mkfd \
    --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
    --group-add video --security-opt seccomp=unconfined \
    -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
    -v "${work}/reader.py:/reader.py:ro" ${slice} \
    "${IMG}" python3 -u /reader.py 2>&1
}

# --- A. baseline: no slice, the container sees the whole card ----------------------------------
base="$(run_reader "")"
base_mem="$(printf '%s\n' "${base}" | sed -n 's/.*total_MiB=\([0-9]*\).*/\1/p' | tail -1)"
base_cu="$(printf '%s\n' "${base}" | sed -n 's/.*CU=\([0-9]*\).*/\1/p' | tail -1)"
if [ -n "${base_mem}" ] && [ -n "${base_cu}" ]; then
  row PASS "baseline reads the whole card" "${base_mem} MiB / ${base_cu} CU"
else
  row FAIL "baseline reads the whole card" "$(printf '%s' "${base}" | tail -3 | tr '\n' ' ')"; fails=$((fails+1))
fi

# --- B. sliced: the container reports the quota, not the card ----------------------------------
render_conf "${work}/ok" 0 0 0
sl="$(run_reader "${work}/ok")"
sl_mem="$(printf '%s\n' "${sl}" | sed -n 's/.*total_MiB=\([0-9]*\).*/\1/p' | tail -1)"
sl_cu="$(printf '%s\n' "${sl}" | sed -n 's/.*CU=\([0-9]*\).*/\1/p' | tail -1)"

[ "${sl_mem}" = "${MEM}" ] \
  && row PASS "sliced total_memory == mem quota" "${sl_mem} MiB" \
  || { row FAIL "sliced total_memory == ${MEM}" "got [${sl_mem:-none}]"; fails=$((fails+1)); }

[ "${sl_cu}" = "${CU}" ] \
  && row PASS "sliced CU count == cu_count" "${sl_cu}" \
  || { row FAIL "sliced CU count == ${CU}" "got [${sl_cu:-none}]"; fails=$((fails+1)); }

# The slice has to be a REDUCTION. Equal figures would pass the two rows above on a card that
# happens to be the quota's size while proving nothing was enforced.
if [ -n "${base_mem}" ] && [ -n "${sl_mem}" ] && [ "${sl_mem}" -lt "${base_mem}" ]; then
  row PASS "the quota is below the whole card" "${sl_mem} < ${base_mem} MiB"
else
  row FAIL "the quota is below the whole card" "sliced=${sl_mem:-none} whole=${base_mem:-none}"; fails=$((fails+1))
fi

# --- C. the vdev_id rule: a record whose id contradicts its file name loses the device ----------
# Same record, same everything, named vdev0.conf while carrying vdev_id 1 -- and its own pipe id,
# so that B's record lingering on pipe 0 cannot be what takes the device away here.
render_conf "${work}/mismatch" 0 1 1
mm="$(run_reader "${work}/mismatch")"
# The vendor's own words, not merely the absence of a reading: a python that would not start, an
# image without the runtime or a pipe id still held all fail to print `total_MiB=` too, and crediting
# any of those to the vdev_id rule lets this row pass while the rule it guards is gone.
if printf '%s\n' "${mm}" | grep -q 'total_MiB='; then
  got="$(printf '%s\n' "${mm}" | sed -n 's/.*total_MiB=\([0-9]*\).*/\1/p' | tail -1)"
  row FAIL "vdev_id mismatch must lose the device" "the container still read ${got} MiB"; fails=$((fails+1))
elif printf '%s\n' "${mm}" | grep -q 'No HIP GPUs are available'; then
  row PASS "vdev_id mismatch loses the device" "No HIP GPUs are available"
else
  row FAIL "vdev_id mismatch loses the device" "the container failed without the vendor's no-device diagnostic: $(printf '%s' "${mm}" | tail -1 | cut -c1-160)"; fails=$((fails+1))
fi

# ...and the same record NAMED to match works, so the row above is about the rule and not about
# some other property of the second record.
render_conf "${work}/named" 1 1 2
nm="$(run_reader "${work}/named")"
nm_mem="$(printf '%s\n' "${nm}" | sed -n 's/.*total_MiB=\([0-9]*\).*/\1/p' | tail -1)"
[ "${nm_mem}" = "${MEM}" ] \
  && row PASS "vdev1.conf carrying vdev_id 1 works" "${nm_mem} MiB" \
  || { row FAIL "vdev1.conf carrying vdev_id 1 works" "got [${nm_mem:-none}]"; fails=$((fails+1)); }

# --- D. the reader `device-manager preflight` itself uses -------------------------------------
# torch answers the rows above because it is unambiguous, but a preflight cannot assume the probe
# image carries it. What it uses instead is BandwidthTest, which comes from the DTK tree the
# ALLOCATOR mounts -- so it is present whatever the image is. Two properties are asserted, because
# preflight depends on both: it reports the quota rather than the card, and starting it is what
# makes the driver publish the vgpu instance preflight reads as its load evidence.
#
# hy-smi is deliberately not that reader and this pins why: under this same injection it answers
# from the DMI layer and reports the PHYSICAL card, so a preflight built on it would call every
# sliced accelerator unsliced.
#
# THE SEQUENCE BELOW IS sliceProbes[hygon].Reader, and has to stay that. Every property of it is
# load-bearing and each was a defect before it was one:
#   * LD_LIBRARY_PATH — without it the binary resolves and its libgalaxyhip does not, which a
#     DTK-based image hides by supplying that library from its own /opt/dtk. Omitted here, this case
#     passes on the default image while the path a non-DTK image takes stays untested.
#   * the record's own name — the kfd vgpu sysfs is node-wide, so reading every entry lets a slice
#     somebody else holds satisfy both assertions below. A record's directory is
#     `0x<gpu_id>@<pipe_id>` and the mounted conf names both halves: its PciBusId maps to the gpu id
#     through the kfd topology's location_id, and its pipe id is unique among the card's live
#     slices. The key is computed before the client starts; unresolvable, the sweep falls back to
#     every record.
#   * the identifier diff — the key names a live slice, not a moment, and the driver keeps a record
#     for about a second after its process exits, so the key can still name a predecessor's. Each
#     record's `Indentifier` (the driver's spelling) is per-process, so the set of them is taken
#     before the client starts and only an unseen one counts.
#   * the claim — two of these run at once under preflight's co-tenancy step, and on the fallback
#     path both would take the first record to appear and report one slice as two.
#   * the sweep before the liveness test, and one more sweep after it fails — a client that has just
#     exited leaves its record for about a second, and a client exits by FINISHING, not only by
#     failing. Breaking straight out on a dead client misses a record published between the two.
#   * `kill -0` — the wait ends when the client dies, not only when it times out.
#   * the client-exit marker — the reader tidies up and exits zero whatever became of its client, so
#     without it a library that would not resolve reaches the judge as an absent record and gets
#     reported as a slice the vendor runtime refused.
cat > "${work}/reader.sh" <<'READER'
conf=/etc/vdev/docker/vdev0.conf
set -- $(awk -F'[.: \t]+' '/^PciBusId:/ { print "0x"$3, "0x"$4, $5; exit }' "$conf" 2>/dev/null)
loc=$(( ${1:-0} * 256 + ${2:-0} * 8 + ${3:-0} ))
pipe=$(awk '/^pipe_id:/ { print $2; exit }' "$conf" 2>/dev/null); mine='*'
[ "$loc" != 0 ] && [ -n "$pipe" ] && for n in /sys/class/kfd/kfd/topology/nodes/*/; do
  grep -qx "location_id $loc" "$n/properties" 2>/dev/null || continue
  g=$(cat "$n/gpu_id" 2>/dev/null); [ "${g:-0}" != 0 ] || continue
  mine=$(printf '0x%x@%s' "$g" "$pipe"); break
done
before=$(grep -h '^Indentifier:' /sys/devices/virtual/kfd/kfd/vgpu/*/entry 2>/dev/null)
claim=/gpustack-preflight-barrier; [ -d "$claim" ] || claim=$(mktemp -d 2>/dev/null || echo /tmp)
log=$(mktemp 2>/dev/null || echo /dev/null)
LD_LIBRARY_PATH=/opt/hygondriver/hip/lib:/opt/hygondriver/lib:/opt/hyhal/lib \
  /opt/hygondriver/bin/BandwidthTest >"$log" 2>&1 &
hip=$!; i=0; new=""; dead=0
while [ -z "$new" ]; do
  for e in /sys/devices/virtual/kfd/kfd/vgpu/$mine/entry; do
    [ -r "$e" ] || continue
    id=$(grep -h '^Indentifier:' "$e" 2>/dev/null)
    [ -n "$id" ] || continue
    case "$before" in *"$id"*) continue ;; esac
    mkdir "$claim/claimed-${id#Indentifier:}" 2>/dev/null || continue
    new="$e"; break
  done
  [ -n "$new" ] && break
  [ "$dead" = 1 ] && break
  kill -0 "$hip" 2>/dev/null || { dead=1; continue; }
  [ "$i" -lt 100 ] || break
  sleep 0.1; i=$((i+1))
done
if [ -n "$new" ]; then
  awk -F: '{ print } /^Vram limit/ { printf "Vram limit MiB: %d\n", $2 / 1048576 }' "$new"
elif ! kill -0 "$hip" 2>/dev/null; then
  wait "$hip" 2>/dev/null; echo gpustack-preflight-client-exit-$?; tail -n 3 "$log" 2>/dev/null
fi
kill "$hip" 2>/dev/null || true
READER

# The same sequence with ONE thing broken: the client itself is not there. That is the branch no
# synthetic judge test can reach -- only a container whose client really cannot start prints the
# marker -- and without it the reader is free to stop reporting a dead client while every row here
# still passes.
#
# The client's PATH is what is broken and not its LD_LIBRARY_PATH, though a library that will not
# resolve is the failure a real probe image produces. On a DTK image the loader finds libgalaxyhip
# through /opt/dtk whatever LD_LIBRARY_PATH says, so that variant runs the client successfully and
# tests nothing -- measured here, the "broken" reader published a record of its own.
sed 's#/opt/hygondriver/bin/BandwidthTest#/opt/hygondriver/bin/no-such-client#' \
  "${work}/reader.sh" > "${work}/reader-broken.sh"

# The runs above each had a pipe id of their own, but this one reuses B's record and therefore its
# pipe id -- so it waits for the card to come free instead of racing the lingering record, the same
# refusal HYGON-CASE 5 asserts deliberately. Without the wait this row fails intermittently and blames
# the reader. Bounded, because a slice somebody else holds is not ours to wait out.
i=0
while [ "${i}" -lt 50 ] && [ -n "$(ls /sys/devices/virtual/kfd/kfd/vgpu/ 2>/dev/null)" ]; do
  sleep 0.1; i=$((i+1))
done

bw="$(ctr run --rm --name "gpustack-hyc2-${run_tag}" \
  --device=/dev/kfd --device=/dev/mkfd \
  --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
  --group-add video --security-opt seccomp=unconfined \
  -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
  -v "${work}/ok:/etc/vdev/docker:ro" -v "${work}/reader.sh:/reader.sh:ro" \
  "${IMG}" sh /reader.sh 2>&1)"

printf '%s\n' "${bw}" | grep -q '^Vgpu device:' \
  && row PASS "a HIP client makes the driver publish the instance" "preflight's load evidence" \
  || { row FAIL "a HIP client makes the driver publish the instance" "no 'Vgpu device:'; container said: $(printf '%s' "${bw}" | tr '\n' ' ' | cut -c1-220)"; fails=$((fails+1)); }

printf '%s\n' "${bw}" | grep -q "^Vram limit MiB: ${MEM}$" \
  && row PASS "the driver record reports the quota in MiB" "${MEM}" \
  || { row FAIL "the driver record reports ${MEM} MiB" "$(printf '%s' "${bw}" | grep -i 'vram limit' | tr '\n' ' ')"; fails=$((fails+1)); }

# A reader that stops reporting its client's fate turns this into the "no record, so the vendor
# runtime refused the slice" verdict -- an operator sent after the accelerator when the answer is the
# probe image.
brk="$(ctr run --rm --name "gpustack-hyc2-${run_tag}" \
  --device=/dev/kfd --device=/dev/mkfd \
  --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
  --group-add video --security-opt seccomp=unconfined \
  -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
  -v "${work}/ok:/etc/vdev/docker:ro" -v "${work}/reader-broken.sh:/reader.sh:ro" \
  "${IMG}" sh /reader.sh 2>&1)"

if printf '%s\n' "${brk}" | grep -q '^gpustack-preflight-client-exit-'; then
  row PASS "a client that cannot start is reported as one" "$(printf '%s' "${brk}" | grep -o 'gpustack-preflight-client-exit-[0-9]*' | head -1)"
else
  row FAIL "a client that cannot start is reported as one" "no client-exit marker; container said: $(printf '%s' "${brk}" | tr '\n' ' ' | cut -c1-220)"; fails=$((fails+1))
fi

printf '%s\n' "${brk}" | grep -q '^Vgpu device:' \
  && { row FAIL "a client that never ran claims no record" "the reader printed load evidence for a client that could not start"; fails=$((fails+1)); } \
  || row PASS "a client that never ran claims no record" "no load evidence, so the judge cannot call this a working slice"

# The key the reader computes is used as a GLOB, so one that quietly failed to resolve degrades to
# sweeping every record on the node -- the fallback, under which every row above still passes. This
# variant reports what it came out as, so the row below can require a resolved one and the pipe id
# this container was actually given.
#
# Derived from the broken client rather than the working one because the key is computed before the
# client starts: mounting a conf reserves nothing, only a HIP process does, so this row costs the
# card no pipe id and does not have to wait for one.
awk '/^before=/ { print "echo \"READER_KEY $mine\"" } { print }' \
  "${work}/reader-broken.sh" > "${work}/reader-keyed.sh"

want_pipe="$(awk '/^pipe_id:/ { print $2; exit }' "${work}/ok/vdev0.conf")"
key="$(ctr run --rm --name "gpustack-hyc2-${run_tag}" \
  --device=/dev/kfd --device=/dev/mkfd \
  --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
  --group-add video --security-opt seccomp=unconfined \
  -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
  -v "${work}/ok:/etc/vdev/docker:ro" -v "${work}/reader-keyed.sh:/reader.sh:ro" \
  "${IMG}" sh /reader.sh 2>&1 | sed -n 's/^READER_KEY //p' | head -1)"
case "${key}" in
  "0x"*"@${want_pipe}") row PASS "the reader addresses its own record by name" "${key}" ;;
  *) row FAIL "the reader addresses its own record by name" "resolved to [${key:-none}], wanted 0x<gpu_id>@${want_pipe}; a bare * is the fallback that sweeps the whole node"; fails=$((fails+1)) ;;
esac

# The path below is the MOUNT POINT and not ${HYHAL}, which every other hy-smi call in this file
# uses because those run on the host. ${HYHAL} is where the tree lives there; inside the container
# the allocator mounts it at /opt/hyhal whatever its host path is, so naming ${HYHAL} here would
# look right, work on a default host, and fail on one where XB_HYHAL points somewhere else.
hs="$(ctr run --rm --name "gpustack-hyc2-${run_tag}" \
  --device=/dev/kfd --device=/dev/mkfd \
  --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
  --group-add video --security-opt seccomp=unconfined \
  -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
  -v "${work}/ok:/etc/vdev/docker:ro" \
  "${IMG}" sh -c "/opt/hyhal/bin/hy-smi --showmeminfo vram" 2>&1 | sed -n 's/.*vram Total Memory (MiB): \([0-9]*\).*/\1/p' | head -1)"
if [ -n "${hs}" ] && [ "${hs}" != "${MEM}" ]; then
  row PASS "hy-smi answers from DMI and is not a slice reader" "reports ${hs} MiB under a ${MEM} MiB slice"
else
  row FAIL "hy-smi answers from DMI and is not a slice reader" "reported [${hs:-none}] where the physical card was expected"; fails=$((fails+1))
fi

echo "--- sliced reader output ---"
printf '%s\n' "${sl}" | tail -3
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 2" "$(xb_fails "${out}")"
