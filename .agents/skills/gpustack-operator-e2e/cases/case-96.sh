#!/usr/bin/env bash
#
# CASE 96 — A logical slice skips a fragmented node through the per-card fit labels
#           (MUTATING, self-recovering; AUTO-SKIPS without two plain schedulable nodes)
#
#   case-96.sh <NS>
#
# Goal:        Prove that the fit labels plus the Workload fit pin let Kueue's topology-aware
#              scheduling skip a node whose free slice units are spread over accelerators none of
#              which fits, and that the pin alone is what makes the difference.
#              Node A's four accelerators each keep 40 % free; node B's four are empty. Both carry
#              the same summed capacity, and A sorts first, so TAS picks A without the pin.
# Environment: Any cluster with two Ready, untainted, schedulable nodes that report no real
#              nvidia.com/gpu.sliced pool, BY APPROXIMATION — no accelerator is needed. The fake
#              product key nvidia-e2efit never collides with a real GPU's pool. AUTO-SKIPS (exit 0,
#              printing NOTHING WAS VERIFIED, which records as pending and never as a pass) when two
#              such nodes do not exist.
# Inputs:      - MOCKED: a fake accelerator NodeFeature per node (nvidia-e2efit, four accelerators,
#                16Gi each) that drives real derivation of the flavor, ClusterQueue and InstanceType;
#              - MOCKED: the bare nvidia.com/gpu.sliced pool on each node, through the status
#                subresource, which gates the operator's own sliced capacity keys;
#              - MOCKED: a node-named Devices ledger per node, labelled with the flavor's selector:
#                A at 640000 units on each accelerator in sliced mode, B at 1600000 each and free;
#              - MOCKED: the same occupancy as a Pod bound to each fragmented node whose allocation
#                annotation, in the device plugin's own format, charges every accelerator the units
#                its ledger shows taken. The fit labels read the published ledger Status, while the
#                node-devices check rebuilds each ledger from the Pods bound to the node, so a Status
#                written without its Pods reads as free to the check. No device manager runs on
#                these nodes, so nothing rewrites the published Status from those Pods;
#              - real: the fit labels the worker derives, the Workload webhook, TAS, the node-devices
#                check, and a raw Pod asking for a 50 % slice (800000 units after the Pod webhook).
# Expected:    - the worker publishes sliced-max-free-units 640000 on A and 1600000 on B;
#              - must fail, with workload-fit-affinity=false: the Workload reserves quota on A, the
#                check answers Retry, and it is reserved on A again at least twice, three times in
#                all, while A keeps its fit labels;
#              - positive, with the setting back on: the Workload's template carries exactly the
#                expression sliced-max-free-units Gt 799999, it is
#                reserved on B in its first cycle, the check is Ready, and it is admitted;
#              - the admitted Pod's spec carries no fit.gpustack.ai key;
#              - no fit, with B's ledger fragmented too: the Workload reserves no quota and its
#                pending message names node affinity.
# Cleanup:     Trap deletes the probe Pods and Workloads, the charging Pods, both ledgers and
#              NodeFeatures, removes the pool it advertised, restores the setting's previous value,
#              and deletes the derived InstanceType once its flavor is gone.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-96.sh <NS>}"
AKEY=nvidia-e2efit
AGID="${AKEY#*-}"
COUNT=4
SLICES=10
MEM_MIB=16384
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
FITKEY="sliced-max-free-units.fit.gpustack.ai/${AKEY}"
POOL="nvidia.com/gpu.sliced"
POOL_PATH="nvidia.com~1gpu.sliced"
SETTING=workload-fit-affinity
AC=gpustack-node-devices
PREFIX="case96-$$"
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

A=""
B=""
ADVERTISED=""
ITNAME=""
SETTING_BEFORE=""
PODS=()

set_setting() {
  kubectl -n "$NS" patch setting "$SETTING" --type merge -p "{\"spec\":{\"value\":\"$1\"}}" >/dev/null
}

restore() {
  echo
  echo "[case-96] cleanup"
  for p in ${PODS[@]+"${PODS[@]}"}; do
    kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    for wl in $(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$p" python3 -c '
import json, os, sys
for wl in json.load(sys.stdin).get("items", []):
    if any(o.get("kind") == "Pod" and o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        print(wl["metadata"]["name"])'); do
      kubectl -n default delete workloads.kueue.x-k8s.io "$wl" --ignore-not-found --wait=false >/dev/null 2>&1 || true
    done
  done
  for n in $A $B; do
    kubectl -n default delete pod "${PREFIX}-holds-${n}" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
    kubectl delete devices.worker.gpustack.ai "$n" --ignore-not-found >/dev/null 2>&1 || true
    kubectl -n "$NS" delete nodefeature "${n}-${PREFIX}-accel" --ignore-not-found >/dev/null 2>&1 || true
  done
  for n in $ADVERTISED; do
    kubectl patch node "$n" --subresource=status --type=json \
      -p "[{\"op\":\"remove\",\"path\":\"/status/capacity/${POOL_PATH}\"},{\"op\":\"remove\",\"path\":\"/status/allocatable/${POOL_PATH}\"}]" \
      >/dev/null 2>&1 || true
  done
  [ -n "$SETTING_BEFORE" ] && set_setting "$SETTING_BEFORE"
  if [ -n "$ITNAME" ]; then
    for _ in $(seq 1 30); do
      [ -z "$(kubectl get resourceflavors.kueue.x-k8s.io -l "${LABELPFX}=true" -o name 2>/dev/null)" ] && break
      sleep 2
    done
    kubectl delete instancetypes.worker.gpustack.ai "$ITNAME" --ignore-not-found --timeout=60s >/dev/null 2>&1 || true
  fi
}
trap restore EXIT

skip() {
  echo
  echo "== CASE 96 — SKIPPED — NOTHING WAS VERIFIED =="
  echo "$1"
  exit 0
}

# Two Ready, untainted, schedulable nodes without a real sliced pool and without a Devices ledger of
# their own, sorted by name so A sorts first. The case writes a node-named ledger and deletes it
# afterwards, so a node that already has one is never chosen.
LEDGERS=$(kubectl get devices.worker.gpustack.ai -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
read -r A B <<<"$(kubectl get nodes -o json | POOL="$POOL" LEDGERS="$LEDGERS" python3 -c '
import json, os, sys
taken = set(os.environ["LEDGERS"].split())
ok = []
for n in json.load(sys.stdin)["items"]:
    ready = any(c["type"] == "Ready" and c["status"] == "True" for c in n["status"].get("conditions", []))
    tainted = any(t.get("effect") in ("NoSchedule", "NoExecute") for t in n["spec"].get("taints", []) or [])
    if (ready and not tainted and not n["spec"].get("unschedulable") and n["metadata"]["name"] not in taken
            and os.environ["POOL"] not in n["status"].get("capacity", {})):
        ok.append(n["metadata"]["name"])
print(" ".join(sorted(ok)[:2]))')"
[ -n "$A" ] && [ -n "$B" ] || skip "Fewer than two Ready, untainted, schedulable nodes without a real ${POOL} pool or a Devices ledger."
echo "[case-96] fragmented node A=${A}, roomy node B=${B}"

SETTING_BEFORE=$(kubectl -n "$NS" get setting "$SETTING" -o jsonpath='{.status.value}' 2>/dev/null)
[ -n "$SETTING_BEFORE" ] || { echo "[case-96] setting ${SETTING} is not served; the build under test predates it"; exit 1; }

# 1. Fake accelerator on both nodes, and the bare sliced pool that gates the operator's sliced keys.
for n in $A $B; do
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${n}-${PREFIX}-accel
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${n}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  labels:
    ${LABELPFX}: "true"
    ${LABELPFX}.count: "${COUNT}"
    ${LABELPFX}.product: "E2E-Fit"
    ${LABELPFX}.memory: "16Gi"
    ${LABELPFX}.cores: "12"
  features: {}
EOF
  ADVERTISED="${ADVERTISED} ${n}"
  kubectl patch node "$n" --subresource=status --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/status/capacity/${POOL_PATH}\",\"value\":\"$((COUNT * SLICES))\"},{\"op\":\"add\",\"path\":\"/status/allocatable/${POOL_PATH}\",\"value\":\"$((COUNT * SLICES))\"}]" \
    >/dev/null || { echo "[case-96] could not advertise ${POOL} on ${n}"; exit 1; }
done

for _ in $(seq 1 40); do
  ITNAME=$(kubectl get instancetypes.worker.gpustack.ai -o name 2>/dev/null | sed 's#.*/##' | grep "^gpustack--${AKEY}-" | head -1)
  [ -n "$ITNAME" ] && break
  sleep 3
done
[ -n "$ITNAME" ] || { echo "[case-96] the derived InstanceType never materialized"; exit 1; }
LQ=""
FLAVORS=""
for _ in $(seq 1 40); do
  LQ=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.status.entrance}' 2>/dev/null)
  FLAVORS=$(kubectl get clusterqueue "$ITNAME" -o jsonpath='{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)
  [ -n "$LQ" ] && [ -n "$FLAVORS" ] && break
  sleep 3
done
[ -n "$LQ" ] && [ -n "$FLAVORS" ] || { echo "[case-96] ${ITNAME} has no entrance LocalQueue or its ClusterQueue no flavor"; exit 1; }
echo "[case-96] InstanceType ${ITNAME}, LocalQueue ${LQ}, flavors ${FLAVORS}"

# 2. A node-named ledger per node, labelled with the flavor's selector minus its batch pin, which is
#    how the node-devices check finds it.
DEV_LABELS=$(kubectl get resourceflavors.kueue.x-k8s.io $FLAVORS -o json | LABELPFX="$LABELPFX" python3 -c '
import json, os, sys
d = json.load(sys.stdin)
for rf in d.get("items", [d]):
    labels = rf.get("spec", {}).get("nodeLabels", {})
    if labels.get(os.environ["LABELPFX"]) == "true":
        for k, v in sorted(labels.items()):
            if not k.endswith(".count"):
                print("    %s: \"%s\"" % (k, v))
        break')
[ -n "$DEV_LABELS" ] || { echo "[case-96] no flavor pins ${LABELPFX}"; exit 1; }

# ledger <node> <remaining> <mode> writes the node's ledger with every accelerator at that state.
ledger() {
  local n=$1 remaining=$2 mode=$3
  local spec status
  spec=$(N="$n" C="$COUNT" S="$SLICES" python3 -c '
import json, os
print(json.dumps([{
    "id": "%s-%d" % (os.environ["N"], i), "index": i, "physicalIndexes": [i],
    "topology": {"pciBusId": "0000:%02x:00.0" % (i + 1), "pciRootId": "", "pciClass": "0302", "numaAffinity": "0", "cpuAffinity": ""},
    "status": {"unhealthy": False, "logicalSliced": {"count": int(os.environ["S"])}},
} for i in range(int(os.environ["C"]))]))')
  status=$(N="$n" C="$COUNT" R="$remaining" M="$mode" python3 -c '
import json, os
print(json.dumps([{"id": "%s-%d" % (os.environ["N"], i), "index": i, "mode": int(os.environ["M"]), "remaining": int(os.environ["R"])} for i in range(int(os.environ["C"]))]))')
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: Devices
metadata:
  name: ${n}
  labels:
${DEV_LABELS}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  groups:
    - id: ${AGID}
      manufacturer: nvidia
      name: E2E-Fit
      memory: ${MEM_MIB}
      accelerators: ${spec}
      acceleratorSlicedDetail:
        logical:
          count: ${SLICES}
EOF
  kubectl patch devices.v1alpha1.worker.gpustack.ai "$n" --subresource=status --type=merge \
    -p "{\"status\":{\"groups\":[{\"id\":\"${AGID}\",\"manufacturer\":\"nvidia\",\"accelerators\":${status}}]}}" >/dev/null
  charge "$n" "$remaining" "$mode"
}

# charge <node> <remaining> <mode> states the ledger's occupancy as the Pod the node-devices check
# rebuilds it from: one Pod bound to the node whose allocation record charges every accelerator
# 1600000 minus remaining units in mode. A free ledger has no such Pod.
charge() {
  local n=$1 remaining=$2 mode=$3 pod="${PREFIX}-holds-$1"
  if [ "$remaining" -ge 1600000 ]; then
    kubectl -n default delete pod "$pod" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1
    return 0
  fi
  local alloc
  alloc=$(N="$n" C="$COUNT" R="$remaining" M="$mode" G="$AGID" python3 -c '
import json, os
print(json.dumps({"main": {"devices": {"groups": [{"id": os.environ["G"], "manufacturer": "nvidia", "accelerators": [
    {"id": "%s-%d" % (os.environ["N"], i), "index": i, "mode": int(os.environ["M"]),
     "allocated": 1600000 - int(os.environ["R"])} for i in range(int(os.environ["C"]))]}]}}}))')
  cat <<EOF | kubectl apply -f - >/dev/null || { echo "[case-96] could not bind the charging Pod to ${n}"; exit 1; }
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  namespace: default
  labels:
    app.kubernetes.io/part-of: gpustack-operator-e2e
  annotations:
    device.gpustack.ai/accelerator.allocated: '${alloc}'
spec:
  nodeName: ${n}
  containers:
    - name: main
      image: ${IMAGE}
EOF
}
ledger "$A" 640000 3
ledger "$B" 1600000 0

# wait_label <node> <value> waits for the node's sliced fit label to read value.
wait_label() {
  local got=""
  for _ in $(seq 1 30); do
    got=$(kubectl get node "$1" -o json | K="$FITKEY" python3 -c 'import json,os,sys; print(json.load(sys.stdin)["metadata"]["labels"].get(os.environ["K"], ""))')
    [ "$got" = "$2" ] && break
    sleep 2
  done
  echo "$got"
}
gotA=$(wait_label "$A" 640000)
gotB=$(wait_label "$B" 1600000)
[ "$gotA" = 640000 ] && [ "$gotB" = 1600000 ] \
  && record PASS "fit labels follow the ledgers" "${FITKEY}: A=${gotA} B=${gotB}" \
  || record FAIL "fit labels follow the ledgers" "want A=640000 B=1600000, got A=${gotA:-<none>} B=${gotB:-<none>}"

for _ in $(seq 1 30); do
  ready=$(kubectl get nodes "$A" "$B" -o json | python3 -c '
import json, sys
print(all("nvidia.com/gpu.sliced.units" in n["status"].get("allocatable", {}) for n in json.load(sys.stdin)["items"]))')
  [ "$ready" = True ] && break
  sleep 2
done
[ "$ready" = True ] || { echo "[case-96] the nodes never reported allocatable nvidia.com/gpu.sliced.units"; exit 1; }

# probe <name> submits a raw Pod asking for a 50 % slice on the pool's LocalQueue.
probe() {
  PODS+=("$1")
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: $1
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: ${LQ}
spec:
  containers:
    - name: main
      image: ${IMAGE}
      resources:
        limits: { ${POOL}: "1", ${POOL}.memory-percentage: "50" }
        requests: { ${POOL}: "1", ${POOL}.memory-percentage: "50" }
EOF
}

# observe <pod> <seconds> samples the Pod's Workload every 3s and prints one JSON summary: every
# reservation it saw with its node, the check states, whether it was admitted, the last pending
# message, and whether its template carries the fit key.
observe() {
  local pod=$1 secs=$2 samples=()
  local end=$((SECONDS + secs))
  while [ $SECONDS -lt $end ]; do
    samples+=("$(kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | P="$pod" AC="$AC" K="$FITKEY" python3 -c '
import json, os, sys

# pinned: every required term of the PodSet carries exactly the expression the 50 % slice needs, so
# no alternative term still admits the fragmented node.
def pinned(ps, key):
    affinity = ps["template"]["spec"].get("affinity") or {}
    required = (affinity.get("nodeAffinity") or {}).get("requiredDuringSchedulingIgnoredDuringExecution") or {}
    terms = required.get("nodeSelectorTerms", [])
    want = {"key": key, "operator": "Gt", "values": ["799999"]}
    return bool(terms) and all(want in t.get("matchExpressions", []) for t in terms)

for wl in json.load(sys.stdin).get("items", []):
    if not any(o.get("kind") == "Pod" and o.get("name") == os.environ["P"] for o in wl["metadata"].get("ownerReferences", [])):
        continue
    st = wl.get("status", {})
    conds = {c["type"]: c for c in st.get("conditions", [])}
    qr = conds.get("QuotaReserved", {})
    host = ""
    for psa in (st.get("admission") or {}).get("podSetAssignments", []):
        for sl in (psa.get("topologyAssignment") or {}).get("slices", []):
            for v in sl.get("valuesPerLevel", []):
                host = v.get("universal", host)
    check = next((c.get("state", "") for c in st.get("admissionChecks", []) if c.get("name") == os.environ["AC"]), "")
    print(json.dumps({"reserved": qr.get("status") == "True", "reservedAt": qr.get("lastTransitionTime", ""),
                      "host": host, "check": check, "admitted": conds.get("Admitted", {}).get("status") == "True",
                      "message": " ".join((qr.get("message") or "").split())[:240],
                      "pinned": pinned(wl["spec"]["podSets"][0], os.environ["K"])}))
    break')")
    last="${samples[${#samples[@]}-1]}"
    case "$last" in *'"admitted": true'*) [ "${3:-}" = until-admitted ] && break ;; esac
    sleep 3
  done
  printf '%s\n' "${samples[@]}" | python3 -c '
import json, sys
rows = [json.loads(l) for l in sys.stdin if l.strip()]
reservations, retried = {}, set()
for r in rows:
    if r["reserved"]:
        reservations[r["reservedAt"]] = r["host"]
        if r["check"] == "Retry":
            retried.add(r["reservedAt"])
order = sorted(reservations)
print(json.dumps({
    "reservations": [reservations[k] for k in order],
    # every reservation but the last, which may still be in its check, answered Retry
    "retriedEach": all(k in retried for k in order[:-1]),
    "checks": sorted({r["check"] for r in rows if r["check"]}),
    "admitted": any(r["admitted"] for r in rows),
    "pinned": any(r["pinned"] for r in rows),
    "message": rows[-1]["message"] if rows else "",
}))'
}

field() { python3 -c "import json,sys; v=json.loads(sys.argv[1])[sys.argv[2]]; print(' '.join(v) if isinstance(v, list) else v)" "$1" "$2"; }

# 3. Must fail: the pin switched off, the labels still published.
set_setting false
echo "[case-96] ${SETTING}=false; waiting 35s for the webhook's setting cache"
sleep 35
probe "${PREFIX}-off"
off=$(observe "${PREFIX}-off" 110)
echo "[case-96] pin off: ${off}"
offRes=$(field "$off" reservations)
offCount=$(echo "$offRes" | wc -w | tr -d ' ')
offOnA=$(echo "$offRes" | tr ' ' '\n' | grep -c -x "$A")
offLabelA=$(wait_label "$A" 640000)
if [ "$(field "$off" pinned)" = False ] && [ "$offCount" -ge 3 ] && [ "$offOnA" = "$offCount" ] \
  && [ "$(field "$off" retriedEach)" = True ] && [ "$(field "$off" admitted)" = False ]; then
  record PASS "without the pin the fragmented node livelocks" "reserved ${offCount}x, every time on A, each answered Retry, never admitted"
else
  record FAIL "without the pin the fragmented node livelocks" "$off"
fi
[ "$offLabelA" = 640000 ] \
  && record PASS "the labels stay published with the pin off" "${FITKEY} on A=${offLabelA}" \
  || record FAIL "the labels stay published with the pin off" "want 640000 on A, got ${offLabelA:-<none>}"
kubectl -n default delete pod "${PREFIX}-off" --force --grace-period=0 >/dev/null 2>&1 || true
sleep 5

# 4. Positive: the pin back on.
set_setting true
echo "[case-96] ${SETTING}=true; waiting 35s for the webhook's setting cache"
sleep 35
probe "${PREFIX}-on"
on=$(observe "${PREFIX}-on" 90 until-admitted)
echo "[case-96] pin on: ${on}"
onRes=$(field "$on" reservations)
if [ "$(field "$on" pinned)" = True ] && [ "$onRes" = "$B" ] && [ "$(field "$on" admitted)" = True ] \
  && echo "$(field "$on" checks)" | grep -qw Ready && ! echo "$(field "$on" checks)" | grep -qw Retry; then
  record PASS "with the pin TAS takes the roomy node first" "reserved once on B, check $(field "$on" checks), admitted"
else
  record FAIL "with the pin TAS takes the roomy node first" "$on"
fi
podFit=$(kubectl -n default get pod "${PREFIX}-on" -o json 2>/dev/null | grep -c 'fit\.gpustack\.ai')
podSel=$(kubectl -n default get pod "${PREFIX}-on" -o jsonpath='{.spec.nodeSelector.kubernetes\.io/hostname}' 2>/dev/null)
[ "$podFit" = 0 ] && [ "$podSel" = "$B" ] \
  && record PASS "the Pod carries no fit key" "nodeSelector hostname=${podSel}, no fit.gpustack.ai anywhere in its spec" \
  || record FAIL "the Pod carries no fit key" "fit mentions=${podFit}, hostname selector=${podSel:-<none>}"
kubectl -n default delete pod "${PREFIX}-on" --force --grace-period=0 >/dev/null 2>&1 || true
sleep 5

# 5. No fit: B fragmented too.
ledger "$B" 640000 3
gotB=$(wait_label "$B" 640000)
[ "$gotB" = 640000 ] || record FAIL "B's fit label follows its ledger" "want 640000, got ${gotB:-<none>}"
probe "${PREFIX}-nofit"
nofit=$(observe "${PREFIX}-nofit" 45)
echo "[case-96] no fit: ${nofit}"
if [ -z "$(field "$nofit" reservations)" ] && echo "$(field "$nofit" message)" | grep -qi affinity; then
  record PASS "with no node fitting the Workload holds no quota" "pending: $(field "$nofit" message)"
else
  record FAIL "with no node fitting the Workload holds no quota" "$nofit"
fi

echo
echo "== CASE 96 — A logical slice skips a fragmented node through the per-card fit labels =="
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-96] ${FAILS} check(s) FAILED"; exit 1; }
echo "CASE 96 PASS"
