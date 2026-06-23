#!/usr/bin/env bash
#
# CASE 4 — Accelerated chain & drain-recycle (approx.)   (MUTATING, self-recovering)
#
#   case-4.sh <NS>
#
# Exercises the ACCELERATED chain and drain-recycle (the ResourceFlavor tombstone,
# ClusterQueue HoldAndDrain) on a GPU-less cluster BY APPROXIMATION: it injects a
# fake accelerator by creating a NodeFeature carrying the minimal acceleratable
# label set and letting NFD merge it onto the node. There is no real Devices CR or
# device-plugin allocation, so this validates the controller/label algebra, not
# physical device handling. See references/drain-recycle.md.
#
# Minimal label set (validated empirically against pkg/nodefeature/helper.go):
#   acceleratable.feature.gpustack.ai/<manu>-<id>       = "true"   (manu must be known)
#   acceleratable.feature.gpustack.ai/<manu>-<id>.count = "1"      (>0; gates derivation)
# cpu/ram/storage fall back to the node's Status.Capacity, so the Worker derives
# .z-flavor=<cpu>c-<ram>g-<stg>g-<acc>d — the "-<acc>d" segment the chain names carry.
#
# Self-recovering: deletes the injected NodeFeature on exit (trap). The drained
# accelerated flavor/CQ are then reclaimed by the operator (or removed by teardown).
set -uo pipefail

NS="${1:?usage: case-4.sh <NS>}"
NODE=$(kubectl get nodes -o jsonpath='{.items[0].metadata.name}')
AKEY=nvidia-t4                       # manufacturer 'nvidia' is a known acceleratable manufacturer
NF="${NODE}-gpustack-e2e-accel"
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"

restore() {
  echo
  echo "[case-4] deleting injected NodeFeature ${NF}"
  kubectl -n "$NS" delete nodefeature "$NF" --ignore-not-found 2>/dev/null || true
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# 1. Inject a fake accelerator: create a NodeFeature NFD merges onto the node.
echo "[case-4] injecting fake accelerator ${AKEY} on node ${NODE}"
cat <<EOF | kubectl apply -f -
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  labels:
    ${LABELPFX}: "true"
    ${LABELPFX}.count: "1"
    ${LABELPFX}.product: "Tesla-T4"
    ${LABELPFX}.memory: "16Gi"
    ${LABELPFX}.cores: "40"
  features: {}
EOF

# 2. Assert the accelerated chain materializes ACTIVE. Names carry the --<aKey>-<acc>d
#    segment. Poll until the accelerated ClusterQueue exists AND is active (stopPolicy not
#    HoldAndDrain) and its ResourceFlavor is not draining — a leftover draining tombstone from
#    a prior run is re-activated by the re-injection, so we wait for the active state, not mere
#    presence. (CQ exists active => RF exists active.)
accCQ=""
for _ in $(seq 1 40); do
  read -r accCQ sp < <(kubectl get clusterqueues.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    n=it['metadata']['name']
    if '${AKEY}-' in n and n.endswith('d'):
        print(n, it['spec'].get('stopPolicy','None')); break
")
  [ -n "$accCQ" ] && [ "$sp" != "HoldAndDrain" ] && break
  accCQ=""
  sleep 3
done
accRF=$(kubectl get resourceflavors.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    n=it['metadata']['name']
    if '${AKEY}-' in n and n.endswith('d') and it['metadata'].get('annotations',{}).get('schedule.gpustack.ai/drain')!='true':
        print(n); break
")
[ -n "$accRF" ] && record PASS "accelerated ResourceFlavor (active)" "$(echo "$accRF" | cut -c1-52)" \
  || record FAIL "accelerated ResourceFlavor (active)" "no active flavor for ${AKEY} — label set may be incomplete (see references)"
[ -n "$accCQ" ] && record PASS "accelerated ClusterQueue (active)" "$(echo "$accCQ" | cut -c1-52)" \
  || record FAIL "accelerated ClusterQueue (active)" "no active ClusterQueue for ${AKEY}"

# 3. Drain: remove the injected NodeFeature so the profile no longer matches any node.
echo "[case-4] removing injected NodeFeature to drain the accelerated profile"
kubectl -n "$NS" delete nodefeature "$NF" --ignore-not-found

# 4. Assert the drain tombstone — the DURABLE drain-recycle signal: the ResourceFlavor is NOT
#    deleted but annotated draining. (The ClusterQueue's HoldAndDrain is only a transient step
#    on its way to removal, so it is not asserted here — the flavor tombstone is the contract.)
drained=""
for _ in $(seq 1 30); do
  drained=$(kubectl get resourceflavors.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin)['items']:
    n=it['metadata']['name']
    if '${AKEY}-' in n and n.endswith('d') and it['metadata'].get('annotations',{}).get('schedule.gpustack.ai/drain')=='true':
        print(n); break
" 2>/dev/null)
  [ -n "$drained" ] && break
  sleep 3
done
[ -n "$drained" ] && record PASS "flavor drain tombstone (not deleted)" "schedule.gpustack.ai/drain=true" \
  || record FAIL "flavor drain tombstone (not deleted)" "flavor was deleted or never drained (pre-drain-recycle behavior?)"

echo
echo "== CASE 4 — Accelerated chain & drain-recycle (approx.) =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). See references/drain-recycle.md (confirm the acceleratable label set"
  echo "against pkg/nodefeature/helper.go), or kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200"
  exit 1
fi
echo "CASE 4 PASS"
