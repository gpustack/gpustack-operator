#!/usr/bin/env bash
#
# CASE 28 — What the SSH sidecar of a partition-backed workload actually sees
#   (MUTATING, self-recovering; AUTO-SKIPS without a partition-capable node AND a node-SSH address)
#
#   MIG_NODE_SSH=<user@host> case-28.sh <NS>
#
# Goal:        RECORD, on real hardware, what the SSH sidecar of a hardware-partitioned workload
#              receives. The sidecar does not select a device of its own: it co-allocates whatever the
#              workload container holds, by reusing main's reserved CARD and asking the vendor
#              responder for that card's visible-devices environment — while main's own environment
#              names the PARTITION. The reservation carries the profile name and the memory slices the
#              instance occupies, but not the partition's device identity, so the responder cannot
#              reconstruct it; and handing a whole-card identifier to a container on a card that is in
#              a partitioning mode is not obviously safe either. Which of those two actually happens is
#              a measurement, not a guess, so this case captures the sidecar's visible-devices
#              environment, its nvidia-smi device list, and whether a trivial CUDA initialisation
#              succeeds inside it — alongside the same three readings from main, and prints them in a
#              form that can be copied into the design record verbatim.
#              Set F11_EXPECT=own to turn the observation into an assertion once the intended behavior
#              is decided: the case then FAILS if the sidecar sees anything other than exactly main's
#              own partition.
# Environment: A reachable cluster whose active context is the GPU cluster, an nvidia node with at
#              least one card that can be put into a hardware partitioning mode, AND SSH to that node
#              (sudo nvidia-smi) supplied via MIG_NODE_SSH=<user@host>. It EXITS 2 (input required)
#              when MIG_NODE_SSH is unset, and AUTO-SKIPS (exit 0) when the card reports no
#              partitioning mode, the mode switch does not converge, or the pool offers no partition
#              profile. The CUDA-initialisation probe needs an interpreter inside the container; where
#              there is none (the sidecar image is not this case's to choose) the reading is recorded
#              as unavailable rather than guessed.
# Inputs:      All real, nothing mocked — an SSH key secret and one SSH-enabled Instance requesting a
#              hardware partition of the DISCOVERED 4-memory-slice profile through the accelerated
#              pool. The workload image is overridable with F11_IMAGE (default python:3.12-slim, whose
#              interpreter makes the CUDA probe meaningful in main; the vendor runtime class supplies
#              the driver libraries regardless of image).
# Expected:    - the Instance's Pod reaches 2/2 Running (main + sshd) on a partition;
#              - main's readings are captured: visible-devices env, nvidia-smi device list, CUDA init;
#              - the sidecar's three readings are captured;
#              - the comparison verdict is recorded and printed for the design record. Under
#                F11_EXPECT=own the verdict must be "the sidecar sees exactly main's partition".
# Cleanup:     Trap deletes the test Instance and its SSH secret, removes the temporary key directory,
#              waits for the instance to reclaim, and restores the partitioning mode of the card this
#              case toggled (a card found already partitioned is left as found). Idempotent; runs on
#              pass AND fail.
set -uo pipefail

NS="${1:?usage: MIG_NODE_SSH=<user@host> case-28.sh <NS>}"
CASE_ID=28
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_partition-lib.sh"

INST=gpustack-e2e-part-ssh
SECRET=gpustack-e2e-part-ssh-key
KEYDIR="$(mktemp -d)"
F11_IMAGE="${F11_IMAGE:-python:3.12-slim}"
F11_EXPECT="${F11_EXPECT:-observe}"

part_require_node_ssh "case-28.sh"
part_require_mig_capable
part_discover

restore() {
  echo
  echo "[case-28] cleanup: deleting the test Instance and its secret, restoring card(s) '${TOGGLED:-<none>}'"
  kubectl -n default delete instance "$INST" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n default delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1 || true
  rm -rf "$KEYDIR"
  part_restore_toggled
}
trap restore EXIT

part_ensure_partitioned_card

read -r SMALL SMALL_CNT MID MID_CNT FULL FULL_CNT NPART NTOT <<<"$(card_profiles)"
[ -n "${MID:-}" ] || part_skip "The partitioned card offers no 4-memory-slice profile — nothing to request."
echo "[case-28] requesting partition profile ${MID} on ${IT}"

# ---------------------------------------------------------------------------------------------------
# One SSH-enabled Instance whose accelerator request is a hardware partition.
# ---------------------------------------------------------------------------------------------------
ssh-keygen -t ed25519 -f "$KEYDIR/id" -N "" -q
kubectl -n default delete secret "$SECRET" --ignore-not-found >/dev/null 2>&1
kubectl -n default create secret generic "$SECRET" --from-file=authorized_keys="$KEYDIR/id.pub" >/dev/null
cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: ${F11_IMAGE}
  command: ["sleep", "infinity"]
  resources:
    cpu: "2"
    ram: "8Gi"
    localStorage: "20Gi"
    accelerator: "1"
    acceleratorPartitionedProfile: ${MID}
  sshPublicKey: { name: ${SECRET} }
  volume: { ephemeral: { capacity: 10Gi } }
  volumeMount: /workspace
EOF

POD="$INST"
ready=""
for _ in $(seq 1 60); do
  ph="$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null)"
  rd="$(kubectl -n default get pod "$POD" -o jsonpath='{range .status.containerStatuses[*]}{.ready} {end}' 2>/dev/null)"
  if [ "$ph" = "Running" ] && [ "$rd" = "true true " ]; then ready=1; break; fi
  sleep 5
done
if [ -z "$ready" ]; then
  record FAIL "partition-backed SSH Instance reaches 2/2 Running" "${POD} not 2/2 Running (phase=$(kubectl -n default get pod "$POD" -o jsonpath='{.status.phase}' 2>/dev/null), held: $(held_reason "$POD")) — cannot observe the sidecar without a running Pod"
  part_results "What the SSH sidecar of a partition-backed workload actually sees"
  exit 1
fi
record PASS "partition-backed SSH Instance reaches 2/2 Running" "${POD} main+sshd Running on profile ${MID}, card(s) $(pod_cards "$POD")"

# ---------------------------------------------------------------------------------------------------
# The three readings, per container.
# ---------------------------------------------------------------------------------------------------

# vis_env <container> — the visible-devices environment the responder handed that container.
vis_env() {
  kubectl -n default exec "$POD" -c "$1" -- sh -c \
    'echo "NVIDIA_VISIBLE_DEVICES=${NVIDIA_VISIBLE_DEVICES-<unset>}"; echo "CUDA_VISIBLE_DEVICES=${CUDA_VISIBLE_DEVICES-<unset>}"' \
    2>/dev/null
}

# smi_list <container> — nvidia-smi -L verbatim, or a marker when the binary is not present.
smi_list() {
  kubectl -n default exec "$POD" -c "$1" -- sh -c \
    'command -v nvidia-smi >/dev/null 2>&1 && nvidia-smi -L 2>&1 || echo "<no nvidia-smi in this container>"' \
    2>/dev/null
}

# cuda_init <container> — a trivial CUDA initialisation through the driver library. Reported as
# unavailable, never guessed, when the container has no interpreter to drive it.
cuda_init() {
  kubectl -n default exec "$POD" -c "$1" -- sh -c '
py=""
for c in python3 python; do command -v $c >/dev/null 2>&1 && { py=$c; break; }; done
if [ -z "$py" ]; then echo "cuInit=<unavailable: no interpreter in this container>"; exit 0; fi
$py -c "
import ctypes
try:
    lib = ctypes.CDLL(\"libcuda.so.1\")
except OSError as e:
    print(\"cuInit=<unavailable: %s>\" % e); raise SystemExit(0)
print(\"cuInit=%d (0 is CUDA_SUCCESS)\" % lib.cuInit(0))
" 2>&1' 2>/dev/null
}

MAIN_ENV="$(vis_env main)"
MAIN_SMI="$(smi_list main)"
MAIN_CUDA="$(cuda_init main)"
SSHD_ENV="$(vis_env sshd)"
SSHD_SMI="$(smi_list sshd)"
SSHD_CUDA="$(cuda_init sshd)"

flat() { printf '%s' "$1" | tr '\n' ';' | tr -s ' '; }
record PASS "OBSERVED: main's readings" "$(flat "$MAIN_ENV") | nvidia-smi -L: $(flat "$MAIN_SMI") | $(flat "$MAIN_CUDA")"
record PASS "OBSERVED: sidecar's readings" "$(flat "$SSHD_ENV") | nvidia-smi -L: $(flat "$SSHD_SMI") | $(flat "$SSHD_CUDA")"

# ---------------------------------------------------------------------------------------------------
# The verdict: does the sidecar see exactly main's partition, or more?
# ---------------------------------------------------------------------------------------------------
mig_uuids() { printf '%s\n' "$1" | grep -oE 'MIG-[0-9a-fA-F-]+' | sort -u | tr '\n' ' '; }
gpu_uuids() { printf '%s\n' "$1" | grep -oE 'GPU-[0-9a-fA-F-]+' | sort -u | tr '\n' ' '; }

# The verdict rests on the visible-devices ENV, not on `nvidia-smi` inside the sidecar. The env is
# what the container runtime consumes to build the device set, it is readable from every image, and
# the sidecar image legitimately ships no NVIDIA tooling — so an empty `nvidia-smi` there means the
# PROBE could not run, which says nothing about what was injected. Treating that as "no device" would
# be a false clean bill of health, so the in-container reading is corroboration only, and is reported
# as INCONCLUSIVE when the binary is absent.
MAIN_ENV_MIG="$(mig_uuids "$MAIN_ENV")"
SSHD_ENV_MIG="$(mig_uuids "$SSHD_ENV")"
SSHD_ENV_GPU="$(gpu_uuids "$SSHD_ENV")"
MAIN_UUIDS="$(mig_uuids "$MAIN_SMI")"
SSHD_UUIDS="$(mig_uuids "$SSHD_SMI")"

case "$SSHD_SMI" in
  *"no nvidia-smi in this container"* | *"nvidia-smi: not found"* | *"executable file not found"*)
    SSHD_PROBE="INCONCLUSIVE (no nvidia-smi in the sidecar image — absence of the tool is not absence of the device)" ;;
  *)
    SSHD_PROBE="ran; MIG devices=[${SSHD_UUIDS:-none}]" ;;
esac

if [ -n "$MAIN_ENV_MIG" ] && [ "$SSHD_ENV_MIG" = "$MAIN_ENV_MIG" ] && [ -z "$SSHD_ENV_GPU" ]; then
  VERDICT="the sidecar's visible-devices env names EXACTLY main's partition (${MAIN_ENV_MIG%% })"
  VSTATE=own
elif [ -n "$SSHD_ENV_GPU" ]; then
  VERDICT="the sidecar's visible-devices env names the PARENT CARD (${SSHD_ENV_GPU%% }) while main's names the partition (${MAIN_ENV_MIG:-<none>}) — broader than its own partition"
  VSTATE=whole
elif [ -z "$SSHD_ENV_MIG" ] && [ -z "$SSHD_ENV_GPU" ]; then
  VERDICT="the sidecar's visible-devices env is EMPTY while main's names ${MAIN_ENV_MIG:-<none>}"
  VSTATE=none
else
  VERDICT="the sidecar's visible-devices env names a DIFFERENT set (${SSHD_ENV_MIG%% }) than main (${MAIN_ENV_MIG:-<none>})"
  VSTATE=other
fi
VERDICT="${VERDICT}; in-container probe: ${SSHD_PROBE}"

if [ "$F11_EXPECT" = own ]; then
  [ "$VSTATE" = own ] \
    && record PASS "the sidecar sees exactly main's partition" "${VERDICT}" \
    || record FAIL "the sidecar sees exactly main's partition" "${VERDICT} — asserted because F11_EXPECT=own"
else
  record PASS "OBSERVED: verdict" "${VERDICT} (recorded, not asserted; set F11_EXPECT=own to make it a guard)"
fi

part_results "What the SSH sidecar of a partition-backed workload actually sees"

echo
echo "---- fold back into the spec: SSH sidecar on a hardware partition ----"
echo "profile requested : ${MID}"
echo "workload image    : ${F11_IMAGE}"
echo "main   env        : $(flat "$MAIN_ENV")"
echo "main   nvidia-smi : $(flat "$MAIN_SMI")"
echo "main   cuda init  : $(flat "$MAIN_CUDA")"
echo "sshd   env        : $(flat "$SSHD_ENV")"
echo "sshd   nvidia-smi : $(flat "$SSHD_SMI")"
echo "sshd   cuda init  : $(flat "$SSHD_CUDA")"
echo "verdict           : ${VERDICT}"
if [ "$VSTATE" = own ]; then
  echo "next step         : own → the intended behavior already holds; keep this case as the regression"
  echo "                    guard by running it with F11_EXPECT=own"
else
  echo "next step         : ${VSTATE} → the sidecar is NOT confined to main's partition. The fix is to carry"
  echo "                    the actuator's chosen partition identity in the reservation, so the responder"
  echo "                    can answer with the partition's env instead of the parent card's."
fi
echo "----------------------------------------------------------------------"

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n default describe pod ${INST}"
  echo "  kubectl -n default get pod ${INST} -o jsonpath='{.metadata.annotations.${ANNO}}'"
  echo "  kubectl -n ${NS} logs ds/${DM_DS} --tail=200"
  exit 1
fi
echo "CASE 28 PASS"
