#!/usr/bin/env bash
#
# CASE 82 — An accelerator and an RDMA endpoint in one container land on one NUMA node, or the
#   container is refused   (MUTATING, self-recovering; AUTO-SKIPS without a node whose accelerators
#   straddle its RDMA endpoints' NUMA nodes under an enforcing kubelet policy)
#
#   case-82.sh <NS>
#
# <NS> is the operator's own namespace. The test Pods run in `default` (or RDMA_TEST_NS).
#
# Goal:        The feature's headline promise, measured at its boundary rather than at a point. A
#              container asking for whole accelerators and one RDMA endpoint together is admitted
#              while the pair fits on one NUMA node, and REFUSED by the kubelet the moment it does
#              not. Both directions are required: an admission on its own is what a host with all
#              its devices on one NUMA node produces with the alignment mechanism switched off.
#              The boundary is computed from the node's own ledger — the largest number of
#              accelerators any single NUMA node carrying a usable endpoint has — so the two
#              requests differ by exactly one accelerator and nothing else. A pair of requests far
#              apart would be refused for a reason the case cannot separate from the one it means.
#              ALSO RECORDED, never passed: the same shape with a hardware PARTITION in place of a
#              whole accelerator. A partition token carries no NUMA hint at all, so the merge sees
#              one hint — ours — and admits the container aligned to the RDMA side alone, with the
#              partition free to sit on the other socket. Nothing fails and no refusal can ever
#              fire, which means this row cannot pass or fail; it is an observation that makes the
#              limit stop being theoretical. A run that happens to land them on the same NUMA node
#              proves nothing and is not written up as a pass.
# Environment: A node running a device manager, with:
#                - at least one whole-function RDMA endpoint;
#                - accelerators on at least two NUMA nodes, at least one sharing a NUMA node with a
#                  usable endpoint and at least one not. WITHOUT THE SECOND HALF every placement is
#                  aligned whatever the kubelet does and the admitted half passes vacuously;
#                - a kubelet reporting `single-numa-node` or `restricted` from its own configz.
#                  Under `none` the hint is computed and discarded and under `best-effort` a
#                  misaligned container is admitted anyway, so on either the refusal can never fire
#                  and the agreement is a coincidence of placement.
#              AUTO-SKIPS (exit 0, printing NOTHING WAS VERIFIED) when no node qualifies, naming per
#              node which of the three it lacked. The partition observation skips on its own when no
#              accelerator is currently in a partitioning mode.
# Inputs:      All real, nothing mocked. Two workload Pods differing by one accelerator, submitted
#              through the entrance LocalQueue of the pool that backs the node, plus an optional
#              third for the partition observation.
# Expected:    - the fitting request runs, and the accelerator it was granted and the RDMA device it
#                was granted report the same numaAffinity in the ledger;
#              - the one-larger request is refused BY THE KUBELET with an admission error naming
#                TopologyAffinityError. A request that never reached the kubelet — held by the queue
#                or unschedulable — is recorded as NOT REACHED rather than as a refusal: those are a
#                different mechanism and would pass this row without exercising it;
#              - the partition observation prints where the partition and the endpoint landed and
#                whether anything objected, as a recording.
# Cleanup:     Trap deletes the test Pods and the probe Pod. No accelerator mode is toggled and no
#              setting is edited, so there is no baseline to restore; the trap runs on pass AND fail
#              and is safe to re-run.
set -uo pipefail

NS="${1:?usage: case-82.sh <NS>}"
CASE_ID=82
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"

trap rdma_cleanup EXIT

WORK_IMAGE="${E2E_RDMA_WORKLOAD_IMAGE:-$RDMA_PROBE_IMAGE}"

rdma_select devices endpoint endpoint-whole accelerator numa-split topology-enforced

ACC_MANUF=$(printf '%s' "$F_MANUFACTURERS" | cut -d, -f1)
ACC_KEY=$(rdma_accelerator_key "$ACC_MANUF")
if [ -z "$ACC_KEY" ]; then
  rdma_skip "${RDMA_NODE} carries accelerators of manufacturer '${ACC_MANUF}', which this case has" \
    "no resource key for. The key map lives in _rdma-lib.sh and is a second copy of the one in" \
    "pkg/nodefeature/knowns.go; a manufacturer added there has to be added here too."
fi

FIT="$F_ACC_BEST_COUNT"
OVER=$((FIT + 1))
ACC_ALLOC=$(rdma_node_quantity "$RDMA_NODE" allocatable "$ACC_KEY")
if [ -z "$ACC_ALLOC" ] || [ "$ACC_ALLOC" -lt "$OVER" ]; then
  # The over-request has to be one the node could serve if topology were not in the way. When the
  # node cannot serve it for capacity reasons, a refusal proves nothing about alignment.
  rdma_skip "${RDMA_NODE} advertises ${ACC_KEY}=${ACC_ALLOC:-<absent>} and the boundary request needs ${OVER}." \
    "The over-request must be one the node has the raw capacity for, or its refusal is about" \
    "capacity rather than about topology and the two are indistinguishable from the outside." \
    "Free the accelerators held by other workloads and re-run."
fi
echo "[case-82] boundary on NUMA ${F_ACC_BEST_NUMA}: ${FIT} accelerator(s) fit beside an endpoint, ${OVER} do not; node advertises ${ACC_KEY}=${ACC_ALLOC}"

# The pool backing this node, and its entrance LocalQueue. A pool BACKS a node when every
# discriminator it carries is a label the node carries too — that is the pool's whole identity, and
# matching on a substring of its name instead picks a neighbouring pool on any cluster whose product
# tokens nest. The queue matters because it routes the claim: through the wrong pool's queue the
# ResourceFlavor selects other nodes and the node-pinned Pod is simply never admitted, which reads
# like a placement defect rather than like a lookup mistake.
read -r IT LQ <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODE_JSON="$(kubectl get node "$RDMA_NODE" -o json 2>/dev/null)" python3 -c '
import json, os, sys
nl = (json.loads(os.environ.get("NODE_JSON") or "{}").get("metadata", {}) or {}).get("labels", {}) or {}
def backs(it):
    d = {k: v for k, v in (it["metadata"].get("labels") or {}).items()
         if not k.startswith("schedule.gpustack.ai/")}
    return bool(d) and all(nl.get(k) == v for k, v in d.items())
for it in json.load(sys.stdin).get("items", []):
    if it.get("spec", {}).get("acceleratable") and backs(it):
        print(it["metadata"]["name"], it.get("status", {}).get("entrance", ""))
        break
')"
if [ -z "${IT:-}" ] || [ -z "${LQ:-}" ]; then
  rdma_skip "No accelerated InstanceType with an entrance LocalQueue backs ${RDMA_NODE}." \
    "The claim has nowhere to be submitted through, so neither direction of this case can be" \
    "attempted. Run the mandatory chain case first and re-check that the node is managed."
fi
echo "[case-82] pool ${IT} via LocalQueue ${LQ}"

# submit <name> <accelerator count> — one container asking for both, which is what the guarantee is
# about: the kubelet's default topology scope is the container, so an accelerator in one container
# and an endpoint in another are aligned with each other by nothing.
submit() {
  local name="$1" n="$2"
  TESTPODS+=("$name")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${RDMA_TEST_NS}
  labels: { kueue.x-k8s.io/queue-name: ${LQ} }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: main
      image: ${WORK_IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${ACC_KEY}: "${n}", ${RDMA_KEY_SHARED}: "1" }
        requests: { ${ACC_KEY}: "${n}", ${RDMA_KEY_SHARED}: "1" }
EOF
}

# ---------------------------------------------------------------------------------------------------
# 1. The fitting request runs, and the two grants name the same NUMA node.
# ---------------------------------------------------------------------------------------------------
FIT_POD="gpustack-e2e-rdma-fit-${RANDOM}"
submit "$FIT_POD" "$FIT"
FPHASE=$(rdma_wait_pod "$FIT_POD" Running Failed)
if [ "$FPHASE" != Running ]; then
  record FAIL "a request that fits on one NUMA node is admitted" \
    "${FIT} accelerator(s) + one endpoint: phase ${FPHASE:-<timeout>}; kubelet says: $(rdma_pod_reason "$FIT_POD")"
else
  record PASS "a request that fits on one NUMA node is admitted" \
    "${FIT} accelerator(s) + one endpoint Running on ${RDMA_NODE}"

  # Which accelerator, from the durable allocation record the plugin writes on the Pod; which RDMA
  # device, from the environment the allocation response set. There is no RDMA ledger to read the
  # second from — the RDMA side writes nothing anywhere — so the container is the record.
  GRANTED_ACC_NUMA=$(kubectl -n "$RDMA_TEST_NS" get pod "$FIT_POD" -o json 2>/dev/null \
    | DEVJSON="$(rdma_devices_json "$RDMA_NODE")" python3 -c '
import json, os, sys
pod = json.load(sys.stdin)
ann = (pod["metadata"].get("annotations") or {}).get("device.gpustack.ai/accelerator.allocated", "")
ids = set()
if ann:
    # The annotation is keyed by container name, and each value carries the allocation under
    # "devices". Reading the groups one level higher finds nothing and reports an empty NUMA set,
    # which the caller treats as an unreadable grant rather than as agreement.
    for _ctr, cval in (json.loads(ann) or {}).items():
        for g in ((cval.get("devices", {}) or {}).get("groups") or []):
            for a in g.get("accelerators", []) or []:
                ids.add(a.get("id", ""))
numa = set()
for g in (json.loads(os.environ["DEVJSON"]).get("spec", {}) or {}).get("groups", []) or []:
    for a in g.get("accelerators", []) or []:
        if a.get("id") in ids:
            numa.add(((a.get("topology", {}) or {}).get("numaAffinity") or ""))
print(",".join(sorted(x for x in numa if x)))
' 2>/dev/null)

  GRANTED_DEV=$(kubectl -n "$RDMA_TEST_NS" exec "$FIT_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
  GRANTED_EP_NUMA=$(printf '%s' "$F_EP_ROWS" | tr ';' '\n' | DEVS="$GRANTED_DEV" python3 -c '
import os, sys
want = {d for d in os.environ.get("DEVS", "").split(",") if d}
numa = set()
for line in sys.stdin:
    parts = line.rstrip("\n").split("|")
    if len(parts) >= 5 and parts[3] in want and parts[4]:
        numa.add(parts[4])
print(",".join(sorted(numa)))
' 2>/dev/null)

  if [ -z "$GRANTED_ACC_NUMA" ] || [ -z "$GRANTED_EP_NUMA" ]; then
    # One side reported nothing. Comparing here would compare two empty strings, which are equal --
    # the shape that turns an unreadable grant into a pass.
    record FAIL "the granted accelerator and the granted endpoint name one NUMA node" \
      "could not read both sides: accelerator NUMA '${GRANTED_ACC_NUMA:-<none>}' from the Pod's allocation record, endpoint NUMA '${GRANTED_EP_NUMA:-<none>}' for NCCL_IB_HCA=${GRANTED_DEV:-<none>}"
  elif [ "$GRANTED_ACC_NUMA" = "$GRANTED_EP_NUMA" ]; then
    record PASS "the granted accelerator and the granted endpoint name one NUMA node" \
      "both on NUMA ${GRANTED_ACC_NUMA} (device ${GRANTED_DEV}); the node's accelerators span {${F_ACC_NUMA}}, so agreement here is a placement and not an arithmetic identity"
  else
    record FAIL "the granted accelerator and the granted endpoint name one NUMA node" \
      "accelerator on NUMA ${GRANTED_ACC_NUMA}, endpoint ${GRANTED_DEV} on NUMA ${GRANTED_EP_NUMA}"
  fi
fi

# ---------------------------------------------------------------------------------------------------
# 2. One more accelerator, and the kubelet refuses.
#
#    The refusal has to be THE KUBELET'S. A queue that never admitted the workload, or a scheduler
#    that found no node, produces a Pod that also never runs — and reading "did not run" as "was
#    refused for topology" would pass this row on a cluster where the alignment gate never ran.
# ---------------------------------------------------------------------------------------------------
OVER_POD="gpustack-e2e-rdma-over-${RANDOM}"
submit "$OVER_POD" "$OVER"
OPHASE=$(rdma_wait_pod "$OVER_POD" Failed Running)
OREASON=$(rdma_pod_reason "$OVER_POD")
case "$OPHASE" in
Running)
  record FAIL "one accelerator more than fits is refused for topology" \
    "${OVER} accelerator(s) + one endpoint were ADMITTED on a node whose richest endpoint-bearing NUMA node has ${FIT}; the alignment is not being enforced"
  ;;
Failed)
  if printf '%s' "$OREASON" | grep -q 'TopologyAffinityError'; then
    record PASS "one accelerator more than fits is refused for topology" \
      "${OVER} accelerator(s) + one endpoint refused by the kubelet: ${OREASON}"
  else
    record FAIL "one accelerator more than fits is refused for topology" \
      "the Pod failed for a reason that is not the topology gate: ${OREASON}"
  fi
  ;;
*)
  SCHED=$(kubectl -n "$RDMA_TEST_NS" get pod "$OVER_POD" -o jsonpath='{.spec.nodeName}' 2>/dev/null)
  record SKIP "one accelerator more than fits is refused for topology" \
    "NOT REACHED: the Pod never got a verdict from a kubelet within ${RDMA_POD_TIMEOUT}s (nodeName '${SCHED:-<unscheduled>}'), so the topology gate was never consulted. A Pod held by the queue or left unschedulable is a different mechanism refusing, and counting it here would pass this row without running it. Conditions: $(kubectl -n "$RDMA_TEST_NS" get pod "$OVER_POD" -o jsonpath='{range .status.conditions[*]}{.type}={.status}/{.reason} {end}' 2>/dev/null)"
  ;;
esac

# ---------------------------------------------------------------------------------------------------
# 3. The partition observation. Never a pass, never a fail.
# ---------------------------------------------------------------------------------------------------
if [ "$F_ACC_PARTITIONED" -eq 0 ]; then
  record SKIP "a hardware partition beside an endpoint is aligned by nothing" \
    "UNANSWERABLE ON THIS HOST: no accelerator on ${RDMA_NODE} is currently in a hardware partitioning mode, so a container cannot ask for a partition profile and an endpoint together. The observation needs a host that is partitionable AND has its accelerators straddling its endpoints' NUMA nodes AND runs an enforcing kubelet policy — all three at once. See references/rdma-host-shapes.md; no machine this suite has been run on satisfies all three"
else
  PROFILE_KEY=$(kubectl get node "$RDMA_NODE" -o json 2>/dev/null | BASE="$ACC_KEY" python3 -c '
import json, os, sys
base = os.environ["BASE"] + ".partitioned."
st = json.load(sys.stdin).get("status", {}) or {}
for k, v in sorted((st.get("allocatable", {}) or {}).items()):
    if k.startswith(base) and not k.endswith(".units"):
        try:
            if int(v) > 0:
                print(k)
                break
        except ValueError:
            pass
' 2>/dev/null)
  if [ -z "$PROFILE_KEY" ]; then
    record SKIP "a hardware partition beside an endpoint is aligned by nothing" \
      "NOT ATTEMPTED: ${F_ACC_PARTITIONED} accelerator(s) report partition profiles but the node advertises no profile key with a free instance, so there is no partition to ask for"
  else
    PART_POD="gpustack-e2e-rdma-part-${RANDOM}"
    TESTPODS+=("$PART_POD")
    cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${PART_POD}
  namespace: ${RDMA_TEST_NS}
  labels: { kueue.x-k8s.io/queue-name: ${LQ} }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: main
      image: ${WORK_IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${PROFILE_KEY}: "1", ${RDMA_KEY_SHARED}: "1" }
        requests: { ${PROFILE_KEY}: "1", ${RDMA_KEY_SHARED}: "1" }
EOF
    PPHASE=$(rdma_wait_pod "$PART_POD" Running Failed)
    PDEV=$(kubectl -n "$RDMA_TEST_NS" exec "$PART_POD" -- sh -c 'printenv NCCL_IB_HCA' 2>/dev/null | tr -d '[:space:]')
    record SKIP "a hardware partition beside an endpoint is aligned by nothing" \
      "OBSERVATION, not a verdict: ${PROFILE_KEY} + one endpoint reached phase ${PPHASE:-<timeout>}, granted device '${PDEV:-<none>}'; kubelet said '$(rdma_pod_reason "$PART_POD")'. A partition token carries no NUMA hint, so the merge sees one hint and no refusal can fire — an admission here is the documented gap being visible, and landing on one NUMA node would be luck rather than alignment"
  fi
fi

rdma_results
