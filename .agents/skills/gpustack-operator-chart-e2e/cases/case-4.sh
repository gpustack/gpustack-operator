#!/usr/bin/env bash
#
# CASE 4 — The control plane survives losing its leader   (MUTATING)
#
#   case-4.sh <NS> [REPLICAS]
#
# Goal:        Raising the replica count of every control-plane component actually buys failover:
#              the replicas roll out, the disruption budgets exist, the standbys stand by on a
#              leader election, and killing the pod that holds the lease hands leadership to a
#              survivor while the aggregated APIs keep answering. HA here is redundancy, never
#              throughput — one holder does the work at any moment.
# Environment: A reachable cluster with the chart installed under the release name below, and at
#              least REPLICAS schedulable nodes — the case AUTO-SKIPS below that, because the
#              documented spread is whenUnsatisfiable: DoNotSchedule and would leave pods Pending.
#              No GPU. Needs a helm client and the chart source tree.
# Inputs:      A values overlay applied with `helm upgrade --reuse-values`, carrying the same knobs
#              docs/operation/high-availability.md ships (worker / kueue.controllerManager /
#              node-feature-discovery.master / both csi-driver-*.controller). Nothing mocked: the
#              leader is a real pod, deleted for real.
# Expected:    - every component reports REPLICAS ready replicas;
#              - a PodDisruptionBudget exists for the worker, Kueue and the NFD master at the
#                minAvailable asked for (the CSI charts render none — asserted absent, not broken);
#              - NFD adds -enable-leader-election to its master above one replica;
#              - deleting the holder of the worker and Kueue leases moves each lease to a
#                different pod, and both Deployments return to full readiness;
#              - the aggregated APIServices stay Available across the failover.
# Cleanup:     A trap restores the release to the user values captured before the case ran, so the
#              replica counts and budgets go back to whatever the install chose. Idempotent.
set -uo pipefail

NS="${1:?usage: case-4.sh <NS> [REPLICAS]}"
REPLICAS="${2:-2}"
MIN_AVAILABLE=$((REPLICAS - 1))
RELEASE=gpustack-operator
CHART=deploy/gpustack-operator/chart
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

report() {
  echo
  echo "== CASE 4 — The control plane survives losing its leader =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

# Preconditions. A single-replica install has nothing to fail over, and too few nodes makes the
# documented hard spread unsatisfiable — both are skips, not failures.
if ! "$HELM" status "$RELEASE" -n "$NS" >/dev/null 2>&1; then
  echo "CASE 4 SKIP — release ${RELEASE} not installed in ${NS}"
  exit 0
fi
if [ "$REPLICAS" -lt 2 ]; then
  echo "CASE 4 SKIP — REPLICAS=${REPLICAS} leaves nothing to fail over"
  exit 0
fi
# Count only nodes the control-plane pods can actually land on: cordoned is not the only way a
# node is out of reach, and a NoSchedule taint is the common one — kind taints its control-plane
# node, and none of these components tolerate it. Counting it would make a hard spread look
# satisfiable and leave a pod Pending instead of skipping cleanly.
schedulable=$(kubectl get nodes -o json 2>/dev/null | python3 -c '
import json, sys
n = 0
for node in json.load(sys.stdin)["items"]:
    spec = node["spec"]
    if spec.get("unschedulable"):
        continue
    if any(t.get("effect") in ("NoSchedule", "NoExecute") for t in spec.get("taints") or []):
        continue
    n += 1
print(n)
')
if [ "${schedulable:-0}" -lt "$REPLICAS" ]; then
  echo "CASE 4 SKIP — ${schedulable:-0} untainted schedulable node(s), REPLICAS=${REPLICAS} needs as many for a DoNotSchedule spread"
  exit 0
fi

# Capture the values the install was given, so the trap can put them back verbatim.
BEFORE=$(mktemp "${TMPDIR:-/tmp}/gpustack-e2e-ha-before.XXXXXX")
HA=$(mktemp "${TMPDIR:-/tmp}/gpustack-e2e-ha-values.XXXXXX")
"$HELM" get values "$RELEASE" -n "$NS" -o yaml > "$BEFORE" 2>/dev/null
[ -s "$BEFORE" ] || echo '{}' > "$BEFORE"
grep -qx 'null' "$BEFORE" && echo '{}' > "$BEFORE"

restore() {
  echo "[case-4] restoring the captured release values"
  "$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" -f "$BEFORE" --timeout 10m >/dev/null 2>&1 \
    || echo "[case-4] restore upgrade failed — inspect: ${HELM} status ${RELEASE} -n ${NS}"
  rm -f "$BEFORE" "$HA"
}
trap restore EXIT

# The same knobs the HA guide documents, at the replica count this run asked for. Kueue's chart
# renders no spread of its own and carries no affinity key, so its labelSelector has to be spelled
# out; the worker's is defaulted from its own pod labels when omitted. NFD renders no spread at all
# and both CSI controllers render neither a budget nor a spread — so neither is asked for here.
cat > "$HA" <<EOF
worker:
  replicas: ${REPLICAS}
  podDisruptionBudget:
    enabled: true
    minAvailable: ${MIN_AVAILABLE}
  topologySpreadConstraints:
    - maxSkew: 1
      topologyKey: kubernetes.io/hostname
      whenUnsatisfiable: DoNotSchedule
kueue:
  controllerManager:
    replicas: ${REPLICAS}
    podDisruptionBudget:
      enabled: true
      minAvailable: ${MIN_AVAILABLE}
    topologySpreadConstraints:
      - maxSkew: 1
        topologyKey: kubernetes.io/hostname
        whenUnsatisfiable: DoNotSchedule
        labelSelector:
          matchLabels:
            app.kubernetes.io/name: kueue
            control-plane: controller-manager
node-feature-discovery:
  master:
    replicaCount: ${REPLICAS}
    podDisruptionBudget:
      enable: true
      minAvailable: ${MIN_AVAILABLE}
csi-driver-nfs:
  controller:
    replicas: ${REPLICAS}
    strategyType: RollingUpdate
csi-driver-s3:
  controller:
    replicas: ${REPLICAS}
    strategyType: RollingUpdate
EOF

echo "== helm upgrade ${RELEASE} to ${REPLICAS} control-plane replicas =="
if ! "$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" --reuse-values -f "$HA" --timeout 10m; then
  record FAIL "ha upgrade" "helm upgrade rejected the HA values"
  report
  exit 1
fi
record PASS "ha upgrade" "${REPLICAS} replicas requested per component"

# Every component reaches the replica count. The CSI controllers are included: they get no budget
# and no spread, but they do honour the count.
for obj in \
  deploy/gpustack-operator-worker \
  deploy/kueue-controller-manager \
  deploy/node-feature-discovery-master \
  deploy/csi-nfs-controller \
  deploy/csi-s3-controller; do
  if ! kubectl -n "$NS" get "$obj" >/dev/null 2>&1; then
    record SKIP "replicas ready" "$obj not installed (switched off)"
    continue
  fi
  if kubectl -n "$NS" rollout status "$obj" --timeout=300s >/dev/null 2>&1 \
    && [ "$(kubectl -n "$NS" get "$obj" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "$REPLICAS" ]; then
    record PASS "replicas ready" "$obj ${REPLICAS}/${REPLICAS}"
  else
    ready=$(kubectl -n "$NS" get "$obj" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
    record FAIL "replicas ready" "$obj ${ready:-0}/${REPLICAS} — check for Pending pods (spread unsatisfiable?)"
  fi
done

# The three budgets the charts do render, at the minAvailable asked for.
for entry in \
  "gpustack-operator-worker" \
  "kueue-manager-pdb" \
  "node-feature-discovery-master"; do
  got=$(kubectl -n "$NS" get pdb "$entry" -o jsonpath='{.spec.minAvailable}' 2>/dev/null)
  if [ "$got" = "$MIN_AVAILABLE" ]; then
    record PASS "disruption budget" "$entry minAvailable=${got}"
  else
    record FAIL "disruption budget" "$entry minAvailable=[${got:-missing}], wanted ${MIN_AVAILABLE}"
  fi
done

# The CSI charts render no budget — assert the absence, so a later chart bump that starts
# rendering one is noticed here rather than silently changing the drain behaviour.
csi_pdb=$(kubectl -n "$NS" get pdb -o name 2>/dev/null | grep -E 'csi-(nfs|s3)' | tr '\n' ' ')
if [ -z "$csi_pdb" ]; then
  record PASS "csi controllers have no budget" "as documented"
else
  record FAIL "csi controllers have no budget" "$csi_pdb appeared — the HA guide says they render none"
fi

# NFD switches its own master to leader election above one replica.
if kubectl -n "$NS" get deploy/node-feature-discovery-master -o yaml 2>/dev/null | grep -q -- '-enable-leader-election'; then
  record PASS "nfd master leader-elects" "-enable-leader-election present"
else
  record FAIL "nfd master leader-elects" "-enable-leader-election missing above one replica"
fi

# Failover. The lease is the ground truth for who holds leadership; controller-runtime and Kueue
# both stamp it as "<pod>_<uuid>", so the holder's pod name is the part before the underscore.
lease_holder_pod() {
  kubectl -n "$NS" get lease "$1" -o jsonpath='{.spec.holderIdentity}' 2>/dev/null | cut -d_ -f1
}

for entry in "worker.gpustack.ai|deploy/gpustack-operator-worker" "c1f6bfd2.kueue.x-k8s.io|deploy/kueue-controller-manager"; do
  lease="${entry%|*}"
  obj="${entry#*|}"
  before=$(lease_holder_pod "$lease")
  if [ -z "$before" ]; then
    record FAIL "leader failover" "$lease has no holder — is leader election on?"
    continue
  fi
  echo "[case-4] deleting ${before}, the holder of lease ${lease}"
  kubectl -n "$NS" delete pod "$before" --wait=false >/dev/null 2>&1
  after=""
  for _ in $(seq 1 40); do
    after=$(lease_holder_pod "$lease")
    [ -n "$after" ] && [ "$after" != "$before" ] && break
    sleep 3
  done
  if [ -n "$after" ] && [ "$after" != "$before" ]; then
    record PASS "leader failover" "$lease ${before} -> ${after}"
  else
    record FAIL "leader failover" "$lease still held by [${after:-none}] after 120s"
  fi
  if kubectl -n "$NS" rollout status "$obj" --timeout=300s >/dev/null 2>&1; then
    record PASS "replaced replica ready" "$obj"
  else
    record FAIL "replaced replica ready" "$obj did not return to full readiness"
  fi
done

# The read path survived: the aggregated APIs the worker serves are still Available, which is the
# part a single-replica install loses outright while its one pod restarts.
for api in v1.gpustack.ai v1.worker.gpustack.ai; do
  st=""
  for _ in $(seq 1 20); do
    st=$(kubectl get apiservice "$api" -o jsonpath='{.status.conditions[?(@.type=="Available")].status}' 2>/dev/null)
    [ "$st" = "True" ] && break
    sleep 3
  done
  if [ "$st" = "True" ]; then
    record PASS "apiservice survived failover" "$api"
  else
    record FAIL "apiservice survived failover" "$api (Available=${st:-missing})"
  fi
done

report

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} get pods -o wide          # Pending pods mean the spread cannot be satisfied"
  echo "  kubectl -n ${NS} get lease                 # who holds leadership now"
  echo "  kubectl -n ${NS} describe deploy/gpustack-operator-worker"
  exit 1
fi
echo "CASE 4 PASS"
