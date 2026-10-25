#!/usr/bin/env bash
#
# CASE 2 — Portless Instance admits, writes status, skips Service; the drain stops it, the managed toggle scopes onboarding   (MUTATING, self-recovering)
#
#   case-2.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any
# Goal:        One portless CPU Instance carries three contracts of the general (CPU) pool:
#              - Kueue admits its Workload on the cpu-only queue even though the Pod asks for
#                resources the queue does not cover, and the Instance reaches Ready;
#              - a portless Instance writes its status and creates no Service (a Service without
#                ports is refused by the API server, which failed every reconcile before the
#                status block, so the Pod ran while the Instance status stayed empty);
#              - draining the pool STOPS the running Instance (spec.stop=true) instead of
#                recreating it with a stuck Pending Pod, keeps the pool's InstanceType alive with
#                its backing ClusterQueue emptied and held, and restoring the nodes refills the
#                queue and reactivates the type.
# Environment: A real cluster with a materialized general pool (the fake client cannot reproduce
#              Kueue admission + eviction, nor the API server's Service validation). No GPU. Targets
#              the general (CPU) pool fed by every managed node, so it behaves the same on a 1-node
#              or an N-node cluster, and needs a CONTINUOUSLY RUNNING operator (it must NOT be
#              restarted between the toggle and the assertions).
# Inputs:      All real, nothing mocked —
#              - reads the derived general InstanceType's unit spec, which the Instance webhook needs;
#              - a portless Instance gpustack-e2e-instance (alpine sleep + ephemeral volume, no
#                spec.ports) in ns default;
#              - drains the pool by toggling gpustack.ai/managed=false on every <node>-gpustack-worker
#                NodeFeature (a general pool's capacity is Node CPU count, not a bumpable label),
#                then toggles the same labels back to true.
# Expected:    - the Instance Pod's own Workload reaches Admitted=True on the cpu-only queue
#                (quotaCheckStrategy checks only the covered cpu dimension and ignores the uncovered
#                memory/ephemeral-storage);
#              - the Instance is running (phase Ready, spec.stop unset) before the drain;
#              - the portless Instance writes its status and no Service is created for it;
#              - the drain deletes the pool's general ResourceFlavor and the Instance flips to
#                spec.stop=true (STOPPED, not recreated);
#              - the InstanceType survives the flavor loss with its queue emptied and held, and it
#                reactivates when the nodes come back.
#              The worker log line naming the stop branch and the count of "spec.ports: Required
#              value" refusals are printed for diagnosis, not asserted.
# Cleanup:     Trap restores gpustack.ai/managed=true on all nodes, deletes the test Instance, and
#              waits for the InstanceType to return to Active WITH a non-zero CPU capacity so a
#              following case finds a healthy chain rather than a pool whose flavor is still being
#              rebuilt. Idempotent, runs on pass AND fail.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-2.sh <NS>}"
# The one Instance every scope below reads: it is portless (no spec.ports), so "no Service" is a
# meaningful expectation, and it is the fixture the admission and stop checks run against.
INST=gpustack-e2e-instance
# Target the general (CPU) pool: it is non-accelerated (no GPU needed) and every managed node feeds
# it, so this behaves the same on a 1-node local cluster and an N-node real one.
IT=$(kubectl get instancetypes.worker.gpustack.ai \
  -o jsonpath='{.items[?(@.spec.acceleratable==false)].metadata.name}' 2>/dev/null | tr ' ' '\n' | grep -m1 'gpustack-')
[ -n "$IT" ] || { echo "no general InstanceType found — run case-1 first to materialize the chain"; exit 1; }
# One of the pool's CPU ResourceFlavors (named gpustack--${gKey}-${os}-${arch}-${count}c, plus a
# -fnv64-<16 hex> topology profile on a TAS cluster — keyed by
# the node's real CPU, so its name is independent of the collapsed InstanceType name), kept as a
# full type/name path so `kubectl get "$RF"` works — watched so we see it deleted on de-manage.
RF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -E '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$' | head -1)
# Every <node>-gpustack-worker NodeFeature: draining ALL of them is what removes a pool that spans
# more than one node (de-managing a single node leaves the others' flavors behind).
WORKER_NFS=$(kubectl -n "$NS" get nodefeatures -o name 2>/dev/null | grep -E -- '-gpustack-worker$')
# The derived general InstanceType is created with its unit spec, and that spec is immutable
# afterwards, so there is nothing to set: read it as a precondition. The Instance webhook sizes the
# Pod from it.
unit_ram=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.spec.unitResources.ram}' 2>/dev/null)
echo "general InstanceType: ${IT}; ResourceFlavor: ${RF#*/}; draining $(echo "$WORKER_NFS" | grep -c .) node(s)"

# Preconditions, read once. Each scope is gated on its own: a missing piece fails the rows that
# need it and leaves the other scopes' rows intact, so no scope's abort suppresses another's rows.
NO_UNIT=""
[ -n "$unit_ram" ] || NO_UNIT="no unit spec on ${IT} — the Instance webhook needs unitRAM to size the Pod"
NO_NODES=""
[ -n "$WORKER_NFS" ] || NO_NODES="no <node>-gpustack-worker NodeFeatures found — the pool cannot be toggled"
NO_RF=""
[ -n "$RF" ] || NO_RF="no CPU ResourceFlavor found — run case-1 first to materialize the chain"

restore() {
  echo
  echo "[case-2] restoring gpustack.ai/managed=true on all nodes, deleting test Instance, waiting for rebuild"
  echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
    -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' 2>/dev/null || true
  kubectl -n default delete instance "$INST" --ignore-not-found 2>/dev/null || true
  # managed=true alone reactivates the pool. The drain emptied its queue through the held flavor
  # migration -- HoldAndDrain first, then the resource groups -- and the emptied queue stays held until
  # a flavor returns: it takes the returning flavor while still held and only then gets its saved
  # StopPolicy back. The migration marker keeps syncInactive from mirroring that HoldAndDrain into
  # Spec.Inactive, so there is no Inactive left for anyone to clear.
  #
  # Active alone is too weak a readiness signal: the pool reports Active as soon as its
  # ClusterQueue is admitting again, while its ResourceFlavor is still being rebuilt and its
  # CPU capacity is still zero. A case starting against that pool sees a healthy phase and no
  # room, and fails for a reason that has nothing to do with what it tests. Wait for both.
  for _ in $(seq 1 40); do
    phase=$(kubectl get instancetype "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)
    cpucap=$(kubectl get instancetype "$IT" -o jsonpath='{.status.cpu.capacity}' 2>/dev/null)
    [ "$phase" = "Active" ] && [ -n "$cpucap" ] && [ "$cpucap" != "0" ] && break
    sleep 3
  done
  echo "[case-2] ${IT}: phase=${phase:-<none>} cpu.capacity=${cpucap:-<none>}"
}
trap restore EXIT

# 1. Create ONE portless Instance (no spec.ports) that every scope below reads. alpine is kept
#    alive (sleep) so its Kueue Workload holds quota; the ephemeral volume lets non-type validation
#    pass.
if [ -z "$NO_UNIT" ]; then
  echo "[case-2] creating portless Instance ${INST} of type ${IT}"
  cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1
kind: Instance
metadata: { name: ${INST}, namespace: default }
spec:
  type: ${IT}
  image: alpine
  command: ["sleep", "86400"]
  volume: { ephemeral: { capacity: 1Gi } }
EOF
else
  echo "[case-2] no Instance is created: ${NO_UNIT}"
fi

# 2. The running Instance on the cpu-only queue: Kueue admits it, its status is written, and the
#    portless workload gets no Service.
if [ -z "$NO_UNIT" ]; then
  # The Pod must be admitted by Kueue (holding quota is what lets the drain evict it). The Pod is
  # named after the Instance and owns its Workload, so the Workload is found by that owner: any
  # other admitted Workload in the namespace says nothing about this Instance.
  admitted=""
  for _ in $(seq 1 20); do
    a=$(kubectl -n default get workloads.kueue.x-k8s.io \
          -o jsonpath='{range .items[*]}{.metadata.ownerReferences[0].name}{"|"}{.status.conditions[?(@.type=="Admitted")].status}{"\n"}{end}' 2>/dev/null \
          | grep -m1 -x 'gpustack-e2e-instance|True')
    [ -n "$a" ] && { admitted=1; break; }
    sleep 3
  done
  if [ -n "$admitted" ]; then
    record PASS "workload admitted" "the Workload owned by Pod gpustack-e2e-instance is Admitted=True (holds quota)"
  else
    record FAIL "workload admitted" "the Workload owned by Pod gpustack-e2e-instance never reached Admitted=True — the cpu-only CQ must admit despite the Pod's memory/ephemeral-storage (quotaCheckStrategy: IgnoreUndeclared)"
  fi

  # The drain has to meet a RUNNING Instance, which is what this case claims to stop, and spec.stop
  # unset is not that: an Instance whose Pod is still pulling its image or failing readiness is
  # Starting, and a drain that lands then exercises the stop of a Starting Instance instead. Wait for
  # the phase to reach Ready before draining.
  ready=""
  for _ in $(seq 1 40); do
    IFS='|' read -r phase0 stop0 <<<"$(kubectl -n default get instance "$INST" \
      -o jsonpath='{.status.phase}|{.spec.stop}' 2>/dev/null)"
    [ "$phase0" = "Ready" ] && [ "$stop0" != "true" ] && { ready=1; break; }
    sleep 3
  done
  if [ -n "$ready" ]; then
    record PASS "instance running pre-drain" "phase=Ready spec.stop=${stop0:-<unset>}"
  else
    record FAIL "instance running pre-drain" "phase=${phase0:-<none>} spec.stop=${stop0:-<unset>} — the drain would not meet a running Instance"
  fi

  # THE portless assertion: the Instance status is written (phase non-empty). Before the fix the
  # reconcile errored on Service creation before the status block, leaving .status empty while the
  # Pod ran.
  phase=""
  for _ in $(seq 1 40); do
    phase=$(kubectl -n default get instance "$INST" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$phase" = "Ready" ] && break
    sleep 3
  done
  case "$phase" in
    Ready)    record PASS "instance status written" "phase=Ready (portless Instance progresses)" ;;
    Starting) record PASS "instance status written" "phase=Starting (status written; not Ready within the wait)" ;;
    *)        record FAIL "instance status written" "phase='${phase:-<EMPTY>}' — status never written (portless Service error blocked it)" ;;
  esac

  # No Service must be created for a portless Instance.
  if kubectl -n default get svc "$INST" >/dev/null 2>&1; then
    record FAIL "no Service for portless Instance" "svc/${INST} exists — a portless workload needs none"
  else
    record PASS "no Service for portless Instance" "no svc/${INST} (Service creation skipped)"
  fi

  # A Workload reports Admitted=True a few seconds BEFORE the CQ's reservingWorkloads counter
  # reflects it. Gate the drain on the counted reservation so the drain meets a queue that holds
  # this Instance's reservation, which is the state in which Kueue has an admitted Workload to
  # evict. An unreserved queue is also put on HoldAndDrain before it is emptied, and the Instance is
  # stopped on that StopPolicy either way, so this gate decides what the drain evicts, not whether
  # the stop happens.
  for _ in $(seq 1 20); do
    rw=$(kubectl get clusterqueue "$IT" -o jsonpath='{.status.reservingWorkloads}' 2>/dev/null)
    [ "${rw:-0}" -ge 1 ] && break
    sleep 2
  done
else
  record FAIL "workload admitted" "$NO_UNIT"
  record FAIL "instance running pre-drain" "$NO_UNIT"
  record FAIL "instance status written" "$NO_UNIT"
  record FAIL "no Service for portless Instance" "$NO_UNIT"
fi

# 3. Drain: exclude the nodes from management so the general flavor is deleted and the pool's queue
#    is drained; the InstanceType itself survives. Toggle via the NodeFeature (NFD reverts a direct
#    node label).
if [ -z "$NO_NODES" ]; then
  echo "[case-2] draining: gpustack.ai/managed=false on all worker NodeFeatures"
  echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
    -p '{"spec":{"labels":{"gpustack.ai/managed":"false"}}}'

  # The guaranteed drain effect: the pool's general ResourceFlavor is deleted (node-index driven,
  # independent of the running Instance/Workload). This confirms the managed-toggle propagated.
  rf_gone=""
  for _ in $(seq 1 40); do
    n=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -cE '/gpustack--.*-[0-9]+c(-fnv64-[0-9a-f]{16})?$')
    [ "${n:-0}" -eq 0 ] && { rf_gone=1; break; }
    sleep 3
  done
  if [ -n "$rf_gone" ]; then
    record PASS "drain removes the pool ResourceFlavor" "no CPU (…-Nc) flavor left"
  else
    record FAIL "drain removes the pool ResourceFlavor" "a CPU (…-Nc) flavor persists — managed-toggle did not propagate"
  fi

  # THE assertion: the Instance whose backing queue is now draining (HoldAndDrain) gets STOPPED,
  # not recreated. On a drain the queue evicts the Pod; the reconciler must set spec.stop (which
  # deletes the Pod) instead of recreating a Pod the drained queue can never admit. A ClusterQueue
  # watch re-enqueues the Instance so the stop is prompt even when no Pod event fires (poll — async).
  if [ -z "$NO_UNIT" ]; then
    stopped=""
    for _ in $(seq 1 40); do
      s=$(kubectl -n default get instance "$INST" -o jsonpath='{.spec.stop}' 2>/dev/null)
      [ "$s" = "true" ] && { stopped=1; break; }
      sleep 3
    done
    if [ -n "$stopped" ]; then
      record PASS "instance STOPPED (not recreated)" "spec.stop=true"
    else
      record FAIL "instance STOPPED (not recreated)" "spec.stop still ${s:-<unset>} — a running Instance must stop when its type drains"
    fi
  else
    record FAIL "instance STOPPED (not recreated)" "$NO_UNIT"
  fi

  # The node's general ResourceFlavor is DELETED (no tombstone). NFD can take a few minutes to
  # propagate a NodeFeature label change to the Node object on a busy cluster, and the flavor
  # deletion only follows once the Node is seen unmanaged — wait generously.
  if [ -z "$NO_RF" ]; then
    gone=""
    for _ in $(seq 1 90); do
      kubectl get "$RF" >/dev/null 2>&1 || { gone=1; break; }
      sleep 3
    done
    if [ -n "$gone" ]; then
      record PASS "flavor deleted on de-manage" "${RF#*/} gone (no drain tombstone)"
    else
      record FAIL "flavor deleted on de-manage" "${RF#*/} still present — NodeFlavorReconciler did not drop the unmanaged node"
    fi
  else
    record FAIL "flavor deleted on de-manage" "$NO_RF"
  fi

  # The InstanceType SURVIVES (not deleted); its backing queue's quota is emptied. The emptied
  # queue must still be held: restoring its StopPolicy before a flavor returns reopens a queue with
  # no quota. Both fields come from one read, so they describe the same instant; an absent
  # stopPolicy is Kueue's default, None.
  survived=""
  for _ in $(seq 1 60); do
    exists=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o name 2>/dev/null)
    cqexists=$(kubectl get clusterqueue "$IT" -o name 2>/dev/null)
    read -r rg stop < <(kubectl get clusterqueue "$IT" -o json 2>/dev/null | python3 -c "import json,sys
try:
  spec = json.load(sys.stdin).get('spec', {})
  print(len(spec.get('resourceGroups') or []), spec.get('stopPolicy') or 'None')
except Exception: print(-1, '?')" 2>/dev/null)
    if [ -n "$exists" ] && [ -n "$cqexists" ] && [ "$rg" = "0" ] && [ "$stop" != "None" ]; then survived=1; break; fi
    sleep 3
  done
  if [ -n "$survived" ]; then
    record PASS "InstanceType survives flavor loss; queue emptied and held" "${IT} kept, resourceGroups=0, stopPolicy=${stop} (drain-when-no-flavors, idle)"
  else
    record FAIL "InstanceType survives flavor loss; queue emptied and held" "exists='${exists:-gone}' cqexists='${cqexists:-gone}' resourceGroups='${rg}' stopPolicy='${stop}' — the type must survive with its queue emptied and held, not tear down or reopen"
  fi
else
  record FAIL "drain removes the pool ResourceFlavor" "$NO_NODES"
  record FAIL "instance STOPPED (not recreated)" "$NO_NODES"
  record FAIL "flavor deleted on de-manage" "$NO_NODES"
  record FAIL "InstanceType survives flavor loss; queue emptied and held" "$NO_NODES"
fi

# 4. Restore the nodes: the pool refills its queue and the InstanceType reactivates.
if [ -z "$NO_NODES" ]; then
  echo "[case-2] restoring gpustack.ai/managed=true; expecting the pool to refill and reactivate"
  echo "$WORKER_NFS" | xargs -r -I{} kubectl -n "$NS" patch {} --type=merge \
    -p '{"spec":{"labels":{"gpustack.ai/managed":"true"}}}' >/dev/null 2>&1
  reactivated=""
  for _ in $(seq 1 90); do
    p=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.phase}' 2>/dev/null)
    rg=$(kubectl get clusterqueue "$IT" -o json 2>/dev/null | python3 -c "import json,sys
try: print(len(json.load(sys.stdin).get('spec',{}).get('resourceGroups') or []))
except Exception: print(0)" 2>/dev/null)
    if [ "$p" = "Active" ] && [ "${rg:-0}" -gt 0 ]; then reactivated=1; break; fi
    sleep 3
  done
  if [ -n "$reactivated" ]; then
    record PASS "pool reactivates when nodes return" "${IT} Active, resourceGroups refilled"
  else
    record FAIL "pool reactivates when nodes return" "phase='${p:-?}' resourceGroups='${rg:-0}' — the pool must refill and reactivate"
  fi
else
  record FAIL "pool reactivates when nodes return" "$NO_NODES"
fi

# Which branch set spec.stop is printed for diagnosis only. The stop itself is the behavior and is
# asserted above; a log sentence is wording the controller is free to change, so it is not a verdict
# (grep -c, not -q: under pipefail, -q closes the pipe early and kubectl logs gets SIGPIPE).
cnt=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=10m 2>/dev/null | grep -c "stop instance as its instance type is gone, deleting, or draining")
echo "[case-2] worker log lines naming the stop-on-drain branch: ${cnt:-0}"

# The API server's refusal of a portless Service is printed for diagnosis only. A zero count cannot
# fail the case on its own: it is also what a log line that stops naming the Instance produces, so
# the verdict rests on the status and Service rows above.
errs=$(kubectl -n "$NS" logs deploy/gpustack-operator-worker --since=5m 2>/dev/null | grep -c "spec.ports: Required value")
echo "[case-2] worker log lines with 'spec.ports: Required value' in the last 5m: ${errs:-0}"

echo
echo "== CASE 2 — Portless Instance admits, writes status, skips Service; the drain stops it, the managed toggle scopes onboarding =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). A portless Instance must be admitted, write its status and create"
  echo "no Service; a drained pool must stop the running Instance and keep the InstanceType alive"
  echo "with its queue emptied and held. Confirm the operator was NOT restarted between toggle and"
  echo "assertion. See references/drain-recycle.md (why a real cluster & the drain trigger)."
  exit 1
fi
echo "CASE 2 PASS"
