#!/usr/bin/env bash
#
# HYGON-CASE 5 — Two slices on one card, at the same time   (needs a real Hygon DCU)
#
#   hygon-case-5.sh
#
# Cases 2..4 each measure ONE slice. Co-tenancy is the claim the product actually makes: several
# containers hold carved pieces of a single accelerator, each bounded by its own record. That
# cannot be established by running two containers back to back — the first can be gone before the
# runtime has created the second — so both probes here signal each other through a shared
# directory and only read once the peer has answered. A run where they did not overlap fails
# rather than passing quietly.
#
# WHAT THE TWO RECORDS DIFFER IN, and what that tests. They name the same card, carry DIFFERENT
# memory quotas, and hold DISJOINT CU masks and DISTINCT pipe ids — which is exactly what
# allocateVdev produces for two pods landing on one accelerator. Different quotas are deliberate:
# equal ones would pass every row even if both containers were reading a single shared slice.
#
# THEY ALSO CARRY THE SAME vdev_id, and that is the second thing this case pins. Each container is
# its own pod's first slice, so both records are vdev0.conf carrying vdev_id 0 — required, because
# the runtime checks the id against the file name (HYGON-CASE 2). A node-wide vdev id pool would
# make the second record vdev0.conf carrying vdev_id 1 and cost that container its accelerator
# entirely. This case is where "vdev ids need not be unique across containers" is established:
# both run, and the driver numbers its own instances underneath (0x<gpu_id>@0 and @1 in the kfd
# vgpu sysfs).
#
# THE NEGATIVE HALF. pipe_id is the field that DOES have to be unique per card. Two live slices
# sharing one leaves the second container with no accelerator at all — the same silent shape as a
# vdev id mismatch, and the reason allocateVdev still draws pipe ids from a scan of the node's
# on-disk records. The last rows assert that failure, so a future change that drops the pipe id
# pool is caught here rather than in a cluster.
#
# Env: XB_WORKLOAD_IMAGE (default XB_IMAGE, then quay.io/gpustack/runner:dtk25.04-vllm0.11.0),
#      XB_HCU (default 0), XB_MEM_A / XB_MEM_B (quotas, default 1024 / 2048), XB_CU (per slice,
#      default 8), XB_HYHAL, XB_DTK, XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

xctr_resolve || { echo "hygon-case-5: no container runtime on the target"; exit 2; }
IMG="${XB_WORKLOAD_IMAGE:-${XB_IMAGE:-quay.io/gpustack/runner:dtk25.04-vllm0.11.0}}"

echo "# HYGON-CASE 5 — two slices on one card (image ${IMG}, hcu ${XB_HCU:-0}) on $(xtarget_desc)"

out="$(xsh \
  CTR="${XB_CTR}" CTR_ARGS="${XB_CTR_ARGS}" IMG="${IMG}" \
  HCU="${XB_HCU:-0}" MEM_A="${XB_MEM_A:-1024}" MEM_B="${XB_MEM_B:-2048}" CU="${XB_CU:-8}" \
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
cleanup(){ ctr rm -f "gpustack-hyc5a-${run_tag}" "gpustack-hyc5b-${run_tag}" "gpustack-hyc5p1-${run_tag}" "gpustack-hyc5p2-${run_tag}" >/dev/null 2>&1; rm -rf "${work}"; }
trap cleanup EXIT

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
row PASS "accelerator" "${bdf} -> card${card}/renderD${render}"

mkdir -p "${work}/barrier"
cat > "${work}/tenant.py" <<'PY'
import os, time, torch
p = torch.cuda.get_device_properties(0)
print("READER total_MiB=%d CU=%d" % (p.total_memory // (1024 * 1024), p.multi_processor_count), flush=True)
me, peer = os.environ["SELF"], os.environ["PEER"]
open("/barrier/" + me, "w").close()
deadline = time.time() + 45
while not os.path.exists("/barrier/" + peer) and time.time() < deadline:
    time.sleep(0.1)
if os.path.exists("/barrier/" + peer):
    print("CO_TENANTS_MET", flush=True)
# Hold a quarter of the quota so the peer is looked at while this one is genuinely resident.
hold = int(os.environ["QUOTA_MIB"]) // 4
x = torch.empty(hold * 1024 * 1024 // 2, dtype=torch.float16, device="cuda")
torch.cuda.synchronize()
print("HELD %d" % hold, flush=True)
time.sleep(20)
PY

# CU_LO / CU_HI: the two cu_mask words for $1 contiguous compute units from bit ${2:-0}.
#
# The second tenant's window starts above the first's, so on a wide card its bits cross into the
# high word -- and a shell's `<<` is modulo the word size, so shifting past 63 wraps to the low bits
# instead of moving up. That would hand the two tenants OVERLAPPING masks while this case asserts
# they are independent, which is the one thing it exists to establish.
cu_words(){
  _lo=0; _hi=0; _b="${2:-0}"; _n=0
  while [ "${_n}" -lt "$1" ]; do
    if [ "${_b}" -lt 64 ]; then _lo=$(( _lo | (1 << _b) )); else _hi=$(( _hi | (1 << (_b - 64)) )); fi
    _b=$(( _b + 1 )); _n=$(( _n + 1 ))
  done
  CU_LO=$(printf '0x%016x' "${_lo}"); CU_HI=$(printf '0x%016x' "${_hi}")
}

# $1 dir, $2 file ordinal, $3 vdev_id, $4 pipe_id, $5 first CU bit, $6 quota
render_conf(){
  mkdir -p "$1"
  cu_words "${CU}" "$5"
  printf 'PciBusId: %s\ncu_mask: %s\ncu_mask: %s\ncu_count: %d\nmem: %d MiB\ndevice_id: 0\nvdev_id: %d\npipe_id: %d\nenable: 1\n' \
    "${bdf}" "${CU_LO}" "${CU_HI}" "${CU}" "$6" "$3" "$4" > "$1/vdev$2.conf"
}

start_tenant(){ # $1 name, $2 vdev dir, $3 self, $4 peer, $5 quota
  # shellcheck disable=SC2086
  ctr run -d --name "$1" \
    --device=/dev/kfd --device=/dev/mkfd \
    --device=/dev/dri/card${card} --device=/dev/dri/renderD${render} \
    --group-add video --security-opt seccomp=unconfined \
    -e SELF="$3" -e PEER="$4" -e QUOTA_MIB="$5" \
    -v "${DTK}:/opt/hygondriver:ro" -v "${HYHAL}:/opt/hyhal:ro" \
    -v "$2:/etc/vdev/docker:ro" -v "${work}/barrier:/barrier" \
    -v "${work}/tenant.py:/tenant.py:ro" \
    "${IMG}" python3 -u /tenant.py >/dev/null 2>&1
}

# new_entries prints the kfd vgpu records this run created, and no others.
#
# The sysfs is NODE-WIDE, so a slice another workload holds -- on any card of this node -- carries
# the same 'Vgpu device:' marker and could carry either quota asserted below. Read whole, the driver
# rows would pass with one of these two tenants having produced no record at all: exactly the false
# positive they exist to catch. Only a record whose per-process `Indentifier` was not there before
# the tenants started belongs to this run -- the same "must be new" half sliceProbes[hygon].Reader
# uses. The other half, naming the one record by its `0x<gpu_id>@<pipe_id>` directory, is not used
# here: this function is about BOTH tenants of one card, and the count below is what proves they are
# two rather than one record read twice.
new_entries(){ # $1 = the identifier snapshot taken before the tenants started
  for e in /sys/devices/virtual/kfd/kfd/vgpu/*/entry; do
    [ -r "${e}" ] || continue
    id="$(grep -h '^Indentifier:' "${e}" 2>/dev/null)"
    [ -n "${id}" ] || continue
    case "$1" in *"${id}"*) continue;; esac
    cat "${e}"
  done
}

# --- A. two slices, disjoint CU masks, distinct pipe ids, SAME vdev_id 0 ------------------------
render_conf "${work}/a" 0 0 0 0             "${MEM_A}"
render_conf "${work}/b" 0 0 1 "${CU}"       "${MEM_B}"
before_vgpu="$(grep -h '^Indentifier:' /sys/devices/virtual/kfd/kfd/vgpu/*/entry 2>/dev/null)"
# The driver keeps a slice record for about a second after the container holding it is removed, and
# a new slice reusing that record's pipe_id inside the window is refused the card outright -- the
# refusal HYGON-CASE 5 provokes deliberately. Part A uses pipe_id 0 and 1, so it
# waits for the card to come free rather than racing whatever ran before it; observed in HYGON-CASE 3 as every row failing with no device at all. Bounded, because a slice somebody else holds is
# not ours to wait out.
freew=0
while [ "${freew}" -lt 50 ] && [ -n "$(ls /sys/devices/virtual/kfd/kfd/vgpu/ 2>/dev/null)" ]; do
  sleep 0.1; freew=$(( freew + 1 ))
done

start_tenant "gpustack-hyc5a-${run_tag}" "${work}/a" a b "${MEM_A}"
start_tenant "gpustack-hyc5b-${run_tag}" "${work}/b" b a "${MEM_B}"

waited=0
while [ "${waited}" -lt 90 ]; do
  la="$(ctr logs "gpustack-hyc5a-${run_tag}" 2>&1)"; lb="$(ctr logs "gpustack-hyc5b-${run_tag}" 2>&1)"
  printf '%s' "${la}" | grep -q '^HELD ' && printf '%s' "${lb}" | grep -q '^HELD ' && break
  sleep 3; waited=$(( waited + 3 ))
done
la="$(ctr logs "gpustack-hyc5a-${run_tag}" 2>&1)"; lb="$(ctr logs "gpustack-hyc5b-${run_tag}" 2>&1)"

mem_of(){ printf '%s\n' "$1" | sed -n 's/.*total_MiB=\([0-9]*\).*/\1/p' | tail -1; }
ma="$(mem_of "${la}")"; mb="$(mem_of "${lb}")"

[ "${ma}" = "${MEM_A}" ] \
  && row PASS "tenant A reports its own quota" "${ma} MiB" \
  || { row FAIL "tenant A reports ${MEM_A}" "got [${ma:-none}]"; fails=$((fails+1)); }
[ "${mb}" = "${MEM_B}" ] \
  && row PASS "tenant B reports its own quota" "${mb} MiB" \
  || { row FAIL "tenant B reports ${MEM_B}" "got [${mb:-none}]"; fails=$((fails+1)); }

# Different figures from one card is the isolation claim: one shared slice cannot produce both.
if [ -n "${ma}" ] && [ -n "${mb}" ] && [ "${ma}" != "${mb}" ]; then
  row PASS "the two slices are bounded independently" "${ma} MiB vs ${mb} MiB on ${bdf}"
else
  row FAIL "the two slices are bounded independently" "A=${ma:-none} B=${mb:-none}"; fails=$((fails+1))
fi

# The barrier: both were alive at the same moment, so the rows above describe co-tenancy.
if printf '%s' "${la}" | grep -q CO_TENANTS_MET && printf '%s' "${lb}" | grep -q CO_TENANTS_MET; then
  row PASS "both slices were live at once" "each probe saw its peer"
else
  row FAIL "both slices were live at once" "A met=$(printf '%s' "${la}" | grep -c CO_TENANTS_MET) B met=$(printf '%s' "${lb}" | grep -c CO_TENANTS_MET)"; fails=$((fails+1))
fi

if printf '%s' "${la}" | grep -q '^HELD ' && printf '%s' "${lb}" | grep -q '^HELD '; then
  row PASS "both allocated inside their own quota" "$(printf '%s' "${la}" | grep '^HELD ') / $(printf '%s' "${lb}" | grep '^HELD ')"
else
  row FAIL "both allocated inside their own quota" "A=$(printf '%s' "${la}" | tail -1) B=$(printf '%s' "${lb}" | tail -1)"; fails=$((fails+1))
fi

# Both records carry vdev_id 0 and both containers work: ids are per-container, not node-wide.
if [ "$(awk -F': ' '/^vdev_id:/{print $2}' "${work}/a/vdev0.conf")" = 0 ] &&
   [ "$(awk -F': ' '/^vdev_id:/{print $2}' "${work}/b/vdev0.conf")" = 0 ] &&
   [ -n "${ma}" ] && [ -n "${mb}" ]; then
  row PASS "a shared vdev_id is legal across containers" "both records carry vdev_id 0"
else
  row FAIL "a shared vdev_id is legal across containers" "one of the two lost its device"; fails=$((fails+1))
fi

# The driver's own view, where it is readable: one instance per live slice on this card.
#
# This is also where `device-manager preflight` reads Hygon's answer from, because Hygon injects no
# shared object of ours for a container to have loaded. sliceProbes[hygon].LoadEvidence is the
# "Vgpu device:" line asserted below and its Reader is this same path, so a driver that stopped
# publishing either would fail here rather than silently downgrade every Hygon row to unmeasured.
entries="$(new_entries "${before_vgpu}")"
seen="$(printf '%s\n' "${entries}" | grep -c '^Vgpu device:')"
#
# EXACTLY two, not at least two. The snapshot only rules out records that already existed, so a HIP
# process somebody else starts mid-run -- on any card of the node -- lands in this set too, and the
# quota rows below would then be satisfied by a foreign 1024 or 2048 MiB slice while one of these
# two tenants produced nothing. A third record makes the run inconclusive rather than passing, and
# one record is a tenant that lost its device. Zero is the sysfs being unreadable, which is not this
# case's claim to make.
if [ "${seen}" -eq 2 ]; then
  row PASS "the driver lists both vgpu instances" "2 records created by this run"
elif [ "${seen}" -eq 0 ]; then
  row WARN "the driver lists both vgpu instances" "no records readable under the kfd vgpu sysfs"
else
  row FAIL "the driver lists exactly the two instances this run created" "found ${seen}: a tenant lost its device, or a foreign slice appeared mid-run and the rows below cannot be attributed"
  fails=$((fails+1))
fi

printf '%s\n' "${entries}" | grep -q '^Vgpu device:' \
  && row PASS "the instance record carries 'Vgpu device:'" "preflight's load evidence" \
  || { row FAIL "the instance record carries 'Vgpu device:'" "absent from the kfd vgpu sysfs"; fails=$((fails+1)); }

# The same record carries the cap, in bytes, which is what preflight converts to MiB.
for want in "${MEM_A}" "${MEM_B}"; do
  bytes=$(( want * 1024 * 1024 ))
  printf '%s\n' "${entries}" | grep -q "^Vram limit:${bytes}$" \
    && row PASS "an instance is capped at ${want} MiB" "Vram limit:${bytes}" \
    || { row FAIL "an instance is capped at ${want} MiB" "no 'Vram limit:${bytes}' in the kfd vgpu sysfs"; fails=$((fails+1)); }
done

ctr rm -f "gpustack-hyc5a-${run_tag}" "gpustack-hyc5b-${run_tag}" >/dev/null 2>&1
rm -f "${work}/barrier/"*

# --- B. the negative half: two live slices sharing one pipe_id ---------------------------------
#
# Sharing a pipe id part A did NOT just use. Part A held 0 and 1, and the driver keeps a record for
# about a second after the container holding it is removed -- so starting p1 on 0 here can have it
# refused by part A's own leftover, which looks exactly like the collision this half is trying to
# provoke and would let the row pass without ever testing it. The collision is p1 and p2 sharing an
# id with each other, and any unused id serves for that.
PIPE_B=5
render_conf "${work}/p1" 0 0 "${PIPE_B}" 0       "${MEM_A}"
render_conf "${work}/p2" 0 0 "${PIPE_B}" "${CU}" "${MEM_B}"   # the same pipe_id as p1
start_tenant "gpustack-hyc5p1-${run_tag}" "${work}/p1" p1 p2 "${MEM_A}"
# Wait for the first to be resident before the second asks for the same pipe.
waited=0
while [ "${waited}" -lt 60 ]; do
  printf '%s' "$(ctr logs "gpustack-hyc5p1-${run_tag}" 2>&1)" | grep -q 'total_MiB=' && break
  sleep 3; waited=$(( waited + 3 ))
done
start_tenant "gpustack-hyc5p2-${run_tag}" "${work}/p2" p2 p1 "${MEM_B}"
sleep 20

l1="$(ctr logs "gpustack-hyc5p1-${run_tag}" 2>&1)"; l2="$(ctr logs "gpustack-hyc5p2-${run_tag}" 2>&1)"
[ -n "$(mem_of "${l1}")" ] \
  && row PASS "the pipe_id holder keeps its slice" "$(mem_of "${l1}") MiB" \
  || { row FAIL "the pipe_id holder keeps its slice" "$(printf '%s' "${l1}" | tail -1)"; fails=$((fails+1)); }

# The vendor's own words, not merely the absence of a reading. Anything can fail to print
# `total_MiB=` -- a python that would not start, a torch that would not import, an image without the
# runtime -- and treating every one of those as the collision lets this row pass while the pipe id
# pool it guards is gone. So the diagnostic is required, and any other failure is reported as one.
if printf '%s\n' "${l2}" | grep -q 'total_MiB='; then
  row FAIL "a colliding pipe_id loses the device" "the second container still read $(mem_of "${l2}") MiB"; fails=$((fails+1))
elif printf '%s\n' "${l2}" | grep -q 'No HIP GPUs are available'; then
  row PASS "a colliding pipe_id loses the device" "No HIP GPUs are available"
else
  row FAIL "a colliding pipe_id loses the device" "the second container failed without the vendor's no-device diagnostic: $(printf '%s' "${l2}" | tail -1 | cut -c1-160)"; fails=$((fails+1))
fi

echo "--- tenant A / tenant B ---"
printf '%s\n' "${la}" | grep -E '^(READER|CO_TENANTS_MET|HELD)'
printf '%s\n' "${lb}" | grep -E '^(READER|CO_TENANTS_MET|HELD)'
echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "HYGON-CASE 5" "$(xb_fails "${out}")"
