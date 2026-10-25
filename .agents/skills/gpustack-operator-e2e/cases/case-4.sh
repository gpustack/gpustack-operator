#!/usr/bin/env bash
#
# CASE 4 — Mocked accelerator pool: over-admit held by the AdmissionCheck, four-view tracks the ledger   (MUTATING, self-recovering)
#
#   case-4.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any
# Goal:        Two contracts over one mocked accelerator pool. A request for whole exclusive cards the
#              pool cannot satisfy passes the coarse Kueue credits gate but is HELD by the
#              node-devices AdmissionCheck reading the per-card Devices ledger — never becoming an
#              admitted-then-unschedulable Workload: the check returns Retry (transient, re-checked
#              as capacity frees), not Rejected, and the same request is admitted once the cards are
#              freed. The materialized InstanceType's four-view (.status.accelerator /
#              acceleratorShared / acceleratorSliced / acceleratorPartitioned) tracks that same
#              ledger through a six-step pooling sequence and moves live over a native watch; the
#              mocked cards report no hardware partitioning capability, so the partition view stays
#              0 throughout; the unit spec is immutable after create and the rejected write touches
#              no Node/NodeFeature.
# Environment: Any cluster with one Ready, untainted, schedulable node that has no Devices ledger
#              and reports no nvidia.com/gpu, BY APPROXIMATION — no accelerator is needed, and the
#              fake product key nvidia-e2emock never collides with a real GPU's derived pool.
#              AUTO-SKIPS (exit 0, printing NOTHING WAS VERIFIED) when no such node exists.
# Inputs:      - MOCKED: a fake accelerator NodeFeature (nvidia-e2emock, count=8, 24Gi A10G-like
#                card) that drives real derivation of the accelerated ResourceFlavor → ClusterQueue
#                → InstanceType;
#              - MOCKED: a Devices ledger named after the node whose eight cards are 50%-sliced (no
#                clean whole card), stepped through a six-state pooling sequence. The check scopes
#                a card to the node its ledger is named after, so a ledger named after no node
#                serves no demand and holds every request whatever its cards say;
#              - MOCKED: the same occupancy as a Pod bound to the node whose allocation annotation,
#                in the device plugin's own format, charges each card 50% in sliced mode: the check
#                rebuilds the ledger from the Pods bound to the node rather than reading its
#                published Status, and no device manager runs on the node, so nothing rewrites that
#                Status;
#              - MOCKED: the ledger's labels — the accelerated flavor's own node selector (minus its
#                node-batch pin), which is how the AdmissionCheck finds it, and the queue's own
#                schedule labels, which is how the four-view finds it;
#              - MOCKED: the Node's nvidia.com/gpu capacity (8) through the status subresource.
#                Kueue's topology-aware scheduling fits a Pod against the Node's own allocatable, so
#                on a node with no such resource the Workload never reserves quota and the check is
#                never consulted;
#              - real: a raw Pod requesting 5 exclusive nvidia.com/gpu cards on the pool's entrance
#                LocalQueue (a raw Pod, not an Instance, keeps this independent of the Instance
#                webhook);
#              - real probe: a patch editing the InstanceType's unit spec (expects rejection);
#              - NOT mocked (the verification): the CQ→AdmissionCheck wiring, the Workload quota
#                reservation, the per-card feasibility over the ledger, the flavor/CQ/InstanceType
#                derivation and the four-view bin-packing math.
# Expected:    - the gpustack-node-devices AdmissionCheck is Active;
#              - the derived accelerated InstanceType materializes;
#              - its backing ClusterQueue references the AdmissionCheck and carries the mocked flavor;
#              - the Workload's AdmissionCheck state is Retry and it is NOT Admitted (held, not rejected);
#              - positive, with the charging Pod deleted and the ledger freed: the same Workload's
#                check turns Ready and it is Admitted;
#              - the InstanceType's spec.os/spec.arch equal its ClusterQueue's kubernetes.io/os|arch
#                schedule labels;
#              - the four-view matches the oracle at each step of
#                8/80/800/0 → 6/60/600/0 → 4/58/400/0 → 2/38/360/0 → 2/38/356/0 → 1/28/256/0;
#              - a native watch observes the exclusive count move 8 → 4 → 8;
#              - the unit-spec edit is REJECTED (immutable) and the stored value is unchanged;
#              - the worker NodeFeature labels are unchanged.
# Cleanup:     Trap deletes the test Pod, the charging Pod, the mocked Devices, and the injected
#              NodeFeature, removes the nvidia.com/gpu capacity from the Node when this case
#              advertised it, and deletes the derived InstanceType once the mocked flavor is gone.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-4.sh <NS>}"
# A Ready, untainted, schedulable node without a Devices ledger of its own and without an
# nvidia.com/gpu resource. The case writes a node-named ledger and deletes it afterwards, so a node
# that already has one is never chosen, and a node whose nvidia.com/gpu a device plugin reports is
# real hardware whose value is not overwritten.
LEDGERS=$(kubectl get devices.worker.gpustack.ai -o jsonpath='{.items[*].metadata.name}' 2>/dev/null)
NODE=$(kubectl get nodes -o json | LEDGERS="$LEDGERS" python3 -c '
import json, os, sys
taken = set(os.environ["LEDGERS"].split())
ok = []
for n in json.load(sys.stdin)["items"]:
    ready = any(c["type"] == "Ready" and c["status"] == "True" for c in n["status"].get("conditions", []))
    tainted = any(t.get("effect") in ("NoSchedule", "NoExecute") for t in n["spec"].get("taints", []) or [])
    if (ready and not tainted and not n["spec"].get("unschedulable") and n["metadata"]["name"] not in taken
            and "nvidia.com/gpu" not in n["status"].get("capacity", {})):
        ok.append(n["metadata"]["name"])
print(sorted(ok)[0] if ok else "")')
if [ -z "$NODE" ]; then
  echo
  echo "== CASE 4 — SKIPPED: NOTHING WAS VERIFIED =="
  echo "No Ready, untainted, schedulable node without a Devices ledger and without nvidia.com/gpu."
  exit 0
fi
AKEY=nvidia-e2emock   # 'nvidia' is a known manufacturer (gates derivation); 'e2emock' never collides with a real product
# The mocked Devices group id, derived from AKEY so the two can never drift apart. A pool reads only
# the Devices groups whose "<manufacturer>-<id>" equals its own spec.acceleratorGroup (== AKEY), so a
# group id picked independently of AKEY makes every accelerator view read zero.
AGID="${AKEY#*-}"
COUNT=8
MEM_MIB=24576
D=1600000
SLICES_PER_CARD=128                # declared logical-slice capability per card
AC=gpustack-node-devices
ACCEL_NF="${NODE}-gpustack-e2e-accel"
WORKER_NF="${NODE}-gpustack-worker" # the worker NodeFeature the unit-spec write must NOT touch
MOCK_DEV="${NODE}"
HOLDER=gpustack-e2e-overadmit-holds
LABELPFX="acceleratable.feature.gpustack.ai/${AKEY}"
MANAGED_LABEL="gpustack.ai/managed"
EXCL_RES="nvidia.com/gpu"           # exclusive whole-card resource for nvidia
POD=gpustack-e2e-overadmit
# Set to 1 BEFORE the Node is patched, so an interruption between the two still removes it; removing
# a key that was never added fails harmlessly, while leaving an added one advertises cards that do not
# exist to every later case.
ADVERTISED=0
ITNAME=""

restore() {
  echo
  echo "[case-4] cleanup: deleting Pod, charging Pod, mocked Devices, injected NodeFeature"
  kubectl -n default delete pod "$POD" "$HOLDER" --ignore-not-found --force --grace-period=0 2>/dev/null || true
  kubectl delete devices.worker.gpustack.ai "$MOCK_DEV" --ignore-not-found 2>/dev/null || true
  kubectl -n "$NS" delete nodefeature "$ACCEL_NF" --ignore-not-found 2>/dev/null || true
  if [ "$ADVERTISED" -eq 1 ]; then
    echo "[case-4] cleanup: removing the advertised ${EXCL_RES} capacity from node ${NODE}"
    kubectl patch node "$NODE" --subresource=status --type=json \
      -p '[{"op":"remove","path":"/status/capacity/nvidia.com~1gpu"},{"op":"remove","path":"/status/allocatable/nvidia.com~1gpu"}]' \
      >/dev/null 2>&1 || true
  fi
  # The operator never deletes an InstanceType for lack of flavors, so the derived type outlives the
  # mock with zero cards, and a later case that takes the first InstanceType it lists can land on it.
  # Delete it only once the mocked flavor is gone: a derived type deleted while its flavor still
  # exists is authored again.
  if [ -n "$ITNAME" ]; then
    for _ in $(seq 1 30); do
      [ -z "$(kubectl get resourceflavors.kueue.x-k8s.io -l "${LABELPFX}=true" -o name 2>/dev/null)" ] && break
      sleep 2
    done
    echo "[case-4] cleanup: deleting the derived InstanceType ${ITNAME}"
    kubectl delete instancetypes.worker.gpustack.ai "$ITNAME" --ignore-not-found --timeout=60s 2>/dev/null || true
  fi
  sleep 5
}
trap restore EXIT

# set_ledger "<8 space-separated card tokens>" patches the mocked Devices ledger.
# Tokens: f=free, e=exclusive, s9=shared(9 shares free), l80/l78/l50=logically sliced(80/78/50% free).
# No token carries a hardware partitioning capability, so every mocked card is unpartitioned and the
# partition view (PT) stays 0 at every step.
set_ledger() {
  local tokens="$1"
  local groups
  groups=$(D="$D" TOKENS="$tokens" AGID="$AGID" python3 - <<'PY'
import json, os
D = int(os.environ["D"])
tok = {
    "f":  (0, D),                 # free whole card
    "e":  (1, 0),                 # exclusive: nothing left
    "s9": (2, 9 * (D // 10)),     # shared: 9 of 10 ownership shares free
    "l80": (3, 80 * (D // 100)),  # logically sliced: 80% VRAM free
    "l78": (3, 78 * (D // 100)),  # logically sliced: 78% VRAM free
    "l50": (3, 50 * (D // 100)),  # logically sliced: 50% VRAM free (the over-admit fixture)
}
accs = []
for i, t in enumerate(os.environ["TOKENS"].split()):
    mode, rem = tok[t]
    accs.append({"id": "c%d" % i, "index": i, "mode": mode, "remaining": rem})
print(json.dumps({"status": {"groups": [
    {"id": os.environ["AGID"], "manufacturer": "nvidia", "accelerators": accs},
]}}))
PY
)
  # Target the v1alpha1 CRD explicitly: the unversioned/v1 resource is the aggregated
  # proxy, whose /status subresource write returns ServiceUnavailable — only the real
  # v1alpha1 CRD serves the status subresource.
  #
  # Fail loudly. Discarding this write's outcome makes a lost patch indistinguishable from a view
  # that never converged: the next assertion polls to its timeout and reports a four-view mismatch,
  # which reads as a product defect while the ledger it was supposed to compare against was never
  # written. On a remote cluster that is a live risk, not a hypothetical — the API endpoint drops a
  # request often enough to have already sent one run down that path.
  if ! kubectl patch devices.v1alpha1.worker.gpustack.ai "$MOCK_DEV" \
    --subresource=status --type=merge -p "$groups" >/dev/null; then
    echo "[case-4] ledger patch failed for tokens '${tokens}' — the next assertion would blame the four-view for a write that never landed"
    return 1
  fi
  return 0
}

# abort_on_ledger ends the case when a ledger write fails: the four-view cannot be compared against
# a ledger that was never written. The rows collected so far are printed first, so the section that
# already ran keeps its evidence. The shared printer draws that table, so the suite has one format.
abort_on_ledger() {
  echo "[case-4] FATAL: the ledger write never landed — the four-view rows that follow cannot be compared against it"
  print_rows
  exit 1
}

# assert_view <label> <excl> <shared> <sliced> <partitioned> polls the InstanceType four-view
# (EX/SH/SL/PT) until it matches the oracle (Remaining == OnceMaxRequest on a single node), then
# records PASS/FAIL. PT is always 0 here: the mocked cards carry no hardware partitioning capability,
# so none of them belongs to the partition population.
assert_view() {
  local label="$1" wE="$2" wS="$3" wL="$4" wP="$5" e s l p
  # One read per poll, not four. Against a remote API each call costs a round trip plus an exec
  # credential, so four of them turn a poll loop that is supposed to bound the wait at ~2 minutes
  # into one that takes several, and a failing assertion pays it in full. The partition view is
  # absent rather than zero on a pool with no partitioned card, hence the trailing default.
  for _ in $(seq 1 40); do
    read -r e s l p <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" \
      -o jsonpath='{.status.accelerator.remaining}{" "}{.status.acceleratorShared.remaining}{" "}{.status.acceleratorSliced.remaining}{" "}{.status.acceleratorPartitioned.remaining}' 2>/dev/null)"
    p="${p:-0}"
    [ "$e" = "$wE" ] && [ "$s" = "$wS" ] && [ "$l" = "$wL" ] && [ "$p" = "$wP" ] && break
    sleep 3
  done
  p="${p:-0}"
  if [ "$e" = "$wE" ] && [ "$s" = "$wS" ] && [ "$l" = "$wL" ] && [ "$p" = "$wP" ]; then
    record PASS "$label" "four-view ${e}/${s}/${l}/${p} (EX/SH/SL/PT)"
  else
    record FAIL "$label" "got ${e:-?}/${s:-?}/${l:-?}/${p:-?}, want ${wE}/${wS}/${wL}/${wP} — four-view math or ledger reverse-lookup"
  fi
}

# wl_state prints the check state, Admitted, QuotaReserved and its message of the Pod's Workload.
wl_state() {
  kubectl -n default get workloads.kueue.x-k8s.io -o json 2>/dev/null | python3 -c "
import json,sys
for wl in json.load(sys.stdin).get('items',[]):
    if not any(o.get('kind')=='Pod' and o.get('name')=='${POD}' for o in wl['metadata'].get('ownerReferences',[])): continue
    st=wl.get('status',{})
    check=next((c.get('state','') for c in st.get('admissionChecks',[]) if c.get('name')=='${AC}'), '-')
    conds={c.get('type'):c for c in st.get('conditions',[])}
    qr=conds.get('QuotaReserved',{})
    print(check, conds.get('Admitted',{}).get('status','False'), qr.get('status','False'),
          ' '.join((qr.get('message') or '-').split())[:200]); break
" 2>/dev/null
}

# ---------------------------------------------------------------------------
# Shared setup: one node, one synthetic accelerator, one mocked ledger.
# ---------------------------------------------------------------------------

# 0. Precondition: the AdmissionCheck object exists and is Active (installKueue applies it,
#    NodeDevicesAdmissionCheckReconciler activates it).
acActive=$(kubectl get admissioncheck "$AC" -o jsonpath='{.status.conditions[?(@.type=="Active")].status}' 2>/dev/null)
if [ "$acActive" = "True" ]; then
  record PASS "AdmissionCheck Active" "${AC} controllerName=worker.gpustack.ai/node-devices"
else
  record FAIL "AdmissionCheck Active" "got Active=${acActive:-<missing>} — installKueue applies it, the AC reconciler activates it"
fi

# 1. Inject a fake accelerator (count=8) → derived accelerated ResourceFlavor/CQ/InstanceType.
echo "[case-4] injecting fake accelerator ${AKEY} (count=${COUNT}) on node ${NODE}"
cat <<EOF | kubectl apply -f -
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${ACCEL_NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  labels:
    ${LABELPFX}: "true"
    ${LABELPFX}.count: "${COUNT}"
    ${LABELPFX}.product: "A10G"
    ${LABELPFX}.memory: "24Gi"
    ${LABELPFX}.cores: "12"
  features: {}
EOF

# 2. Wait for the derived accelerated InstanceType (its name is the pool name
#    gpustack--${AKEY}-<os>-<arch> when CPU-manufacturer awareness is off) and read its os/arch
#    labels plus the entrance LocalQueue.
for _ in $(seq 1 40); do
  ITNAME=$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
for it in json.load(sys.stdin).get('items',[]):
    n=it['metadata']['name']
    if n.startswith('gpustack--${AKEY}-'):
        print(n); break
" 2>/dev/null)
  [ -n "$ITNAME" ] && break
  sleep 3
done
if [ -z "$ITNAME" ]; then
  echo "[case-4] derived accelerated InstanceType never materialized — is instance-type-derived-from-node on?"
  print_rows
  exit 1
fi
read -r OS ARCH LQ <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o json | python3 -c "
import json,sys
o=json.load(sys.stdin); l=o.get('metadata',{}).get('labels',{})
print(l.get('kubernetes.io/os',''), l.get('kubernetes.io/arch',''), o.get('status',{}).get('entrance',''))
")"
echo "[case-4] derived InstanceType ${ITNAME} (os=${OS} arch=${ARCH} entrance=${LQ})"

# 3. The backing ClusterQueue must reference the node-devices AdmissionCheck (gate-3 wiring).
acRef=""
for _ in $(seq 1 20); do
  acRef=$(kubectl get clusterqueue "$ITNAME" -o jsonpath='{.spec.admissionChecksStrategy.admissionChecks[*].name}' 2>/dev/null | tr ' ' '\n' | grep -x "$AC")
  [ -n "$acRef" ] && break
  sleep 3
done
if [ -n "$acRef" ]; then
  record PASS "CQ references AdmissionCheck" "admissionChecksStrategy → ${AC}"
else
  record FAIL "CQ references AdmissionCheck" "CQ ${ITNAME} does not reference ${AC} — gate-3 not wired (acceleratable+derived+AC-Active)"
fi

# The queue must also CARRY the mocked flavor, and not be draining, before anything is submitted. A
# queue left over from an earlier mock of the same key already references the check while it is
# still draining its old flavor or has emptied its resource groups, and a Workload submitted into
# that window is judged by a queue with no quota at all — which is a different question from the one
# the over-admit section asks.
cqFlavors=""
cqStop=""
for _ in $(seq 1 40); do
  IFS='|' read -r cqStop cqFlavors <<<"$(kubectl get clusterqueue "$ITNAME" \
    -o jsonpath='{.spec.stopPolicy}|{.spec.resourceGroups[*].flavors[*].name}' 2>/dev/null)"
  case "$cqStop" in "" | None) [ -n "$cqFlavors" ] && break ;; esac
  sleep 3
done
case "$cqStop" in "" | None) ;; *) cqFlavors="" ;; esac

# 4. The mocked ledger. Its labels are the accelerated flavor's own node selector (minus its
#    node-batch pin) — that selector is how the AdmissionCheck finds a node's Devices — merged with
#    the queue's own schedule labels, which are how the InstanceType's four-view finds it. A
#    hand-written label set drifts from the flavor: the flavor also pins the CPU group and, under
#    topology-aware scheduling, the topology profile — and a ledger the check cannot find holds
#    every Workload in Retry whatever its cards say, which is the over-admit row passing with the
#    per-card math never consulted.
# The first line of the block says whether a flavor of the queue pins the mocked accelerator's
# key: the AdmissionCheck scopes the request with that flavor's own selector, so without it the
# check has no accelerator to reason about. The rest of the block is the label set itself.
# shellcheck disable=SC2086  # $cqFlavors is a list of flavor names; the word splitting is the point
DEV_LABELS=$(kubectl get resourceflavors.kueue.x-k8s.io $cqFlavors -o json 2>/dev/null | \
  LABELPFX="$LABELPFX" OS="$OS" ARCH="$ARCH" MANAGED="$MANAGED_LABEL" python3 -c '
import json, os, sys
labels = {}
try:
    d = json.load(sys.stdin)
except ValueError:
    d = {}
pinned = False
for rf in d.get("items", [d]):
    fl = rf.get("spec", {}).get("nodeLabels", {})
    if fl.get(os.environ["LABELPFX"]) != "true":
        continue
    for k, v in fl.items():
        if not k.endswith(".count"):
            labels[k] = v
    pinned = True
    break
labels[os.environ["LABELPFX"]] = "true"
labels[os.environ["MANAGED"]] = "true"
for key, env in (("kubernetes.io/os", "OS"), ("kubernetes.io/arch", "ARCH")):
    value = os.environ.get(env, "")
    if value:
        labels[key] = value
print("PINNED" if pinned else "UNPINNED")
for k, v in sorted(labels.items()):
    print("    %s: \"%s\"" % (k, v))
')
NL=$'\n'
FLAVOR_PINNED=0
if [ "${DEV_LABELS%%"$NL"*}" = "PINNED" ]; then
  FLAVOR_PINNED=1
fi
DEV_LABELS="${DEV_LABELS#*"$NL"}"

# The card list a card must carry to be accepted: physicalIndexes/topology/status are required by
# the CRD, and only the capability inside status is what the sliced view is after, the rest is the
# minimum a card must carry. The capability's count only has to be positive: the sliced view is
# denominated in per-card VRAM percent (100 per card), not in this count.
CARDS_YAML=$(for i in $(seq 0 $((COUNT - 1))); do
  printf '        - id: c%d\n          index: %d\n          physicalIndexes: [%d]\n' "$i" "$i" "$i"
  printf '          topology:\n            pciBusId: "0000:%02x:00.0"\n            pciRootId: "0000:%02x:00.0"\n' "$i" "$i"
  printf '            pciClass: "030200"\n            numaAffinity: "0"\n            cpuAffinity: "0-15"\n'
  printf '          status:\n            unhealthy: false\n            logicalSliced:\n              count: %d\n' "$SLICES_PER_CARD"
done)
echo "[case-4] creating mocked Devices ${MOCK_DEV} (os=${OS} arch=${ARCH})"
if ! cat <<EOF | kubectl apply -f -
apiVersion: worker.gpustack.ai/v1alpha1
kind: Devices
metadata:
  name: ${MOCK_DEV}
  labels:
${DEV_LABELS}
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  groups:
    - id: ${AGID}
      manufacturer: nvidia
      name: A10G
      memory: ${MEM_MIB}
      acceleratorSlicedDetail:
        logical:
          count: ${SLICES_PER_CARD}
      accelerators:
${CARDS_YAML}
EOF
then
  echo "[case-4] could not create the mocked Devices ${MOCK_DEV} — neither section can read a ledger"
  print_rows
  exit 1
fi

# ---------------------------------------------------------------------------
# Section 1 — the over-admit hold.
#
# The section ends on its own: a precondition it cannot meet is recorded as a row and the rest of
# the section is skipped, so the four-view section below still produces its rows.
# ---------------------------------------------------------------------------
section1_failed=0
section1_reason=""
if [ -z "$cqFlavors" ]; then
  section1_failed=1
  section1_reason="the CQ never carried a flavor for the mocked accelerator outside a drain (stopPolicy=${cqStop:-<none>}), so no quota exists to pass gate 1"
fi
if [ "$section1_failed" -eq 0 ] && [ "$FLAVOR_PINNED" -ne 1 ]; then
  section1_failed=1
  section1_reason="no flavor of CQ ${ITNAME} pins ${LABELPFX}, so the check cannot scope the ledger and was never asked"
fi
if [ "$section1_failed" -eq 0 ] && [ -z "$LQ" ]; then
  section1_failed=1
  section1_reason="the InstanceType has no entrance LocalQueue, so no Workload would ever be built for the request"
fi

if [ "$section1_failed" -eq 0 ]; then
  # Kueue's topology-aware scheduling fits the Pod against the Node's own allocatable of every resource
  # it requests, so on a node reporting no nvidia.com/gpu the Workload is excluded before quota is
  # reserved and the AdmissionCheck is never consulted. The node was chosen for reporting none. A
  # failed advertisement is not fatal here: the request below then never reserves quota, and the row
  # says exactly that.
  echo "[case-4] advertising ${EXCL_RES}=${COUNT} on node ${NODE} (it reports none)"
  ADVERTISED=1
  if ! kubectl patch node "$NODE" --subresource=status --type=json \
    -p "[{\"op\":\"add\",\"path\":\"/status/capacity/nvidia.com~1gpu\",\"value\":\"${COUNT}\"},{\"op\":\"add\",\"path\":\"/status/allocatable/nvidia.com~1gpu\",\"value\":\"${COUNT}\"}]" \
    >/dev/null; then
    echo "[case-4] could not advertise ${EXCL_RES} on node ${NODE}"
  fi

  # Mock the per-card ledger: 8 cards, each 50%-sliced → no clean whole card for exclusive.
  echo "[case-4] mocking the ledger of ${MOCK_DEV}: 8× card sliced 50% (no clean card)"
  HALF=$((50 * (D / 100)))                     # 50% VRAM free, mode=Sliced(3)
  if ! set_ledger "l50 l50 l50 l50 l50 l50 l50 l50"; then
    section1_failed=1
    section1_reason="the ledger of ${MOCK_DEV} could not be written, so the check never read a card"
  fi
fi

if [ "$section1_failed" -eq 0 ]; then
  # The check rebuilds the ledger from the Pods bound to the node, so the same occupancy is stated as
  # the Pod the device plugin would have recorded: one Pod bound to the node charging each card the
  # half it has taken, in sliced mode.
  alloc=$(COUNT="$COUNT" G="$AGID" U="$((D - HALF))" python3 -c '
import json, os
print(json.dumps({"main": {"devices": {"groups": [{"id": os.environ["G"], "manufacturer": "nvidia", "accelerators": [
    {"id": "c%d" % i, "index": i, "mode": 3, "allocated": int(os.environ["U"])} for i in range(int(os.environ["COUNT"]))]}]}}}))')
  if ! cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${HOLDER}
  namespace: default
  labels:
    app.kubernetes.io/part-of: gpustack-operator-e2e
  annotations:
    device.gpustack.ai/accelerator.allocated: '${alloc}'
spec:
  nodeName: ${NODE}
  containers:
    - name: main
      image: registry.k8s.io/pause:3.10
EOF
  then
    section1_failed=1
    section1_reason="the charging Pod could not be bound to ${NODE}, so the check rebuilt a ledger with no occupancy"
  fi
fi

if [ "$section1_failed" -eq 0 ]; then
  # Submit a raw Pod requesting 5 EXCLUSIVE cards, routed to the accelerated pool's entrance
  # LocalQueue. Credits (5×M ≤ 8×M) reserve quota at gate 1; gate 3 must hold it because no
  # clean whole card exists. (A raw Pod — not an Instance — keeps this independent of the
  # Instance webhook's unit-spec requirements; Kueue's pod integration builds the Workload.)
  echo "[case-4] submitting raw Pod requesting 5 exclusive cards on queue ${LQ}"
  cat <<EOF | kubectl apply -f -
apiVersion: v1
kind: Pod
metadata:
  name: ${POD}
  namespace: default
  labels:
    kueue.x-k8s.io/queue-name: ${LQ}
spec:
  schedulerName: default-scheduler
  containers:
    - name: main
      image: alpine
      command: ["sleep", "86400"]
      resources:
        limits: { ${EXCL_RES}: "5" }
        requests: { ${EXCL_RES}: "5" }
EOF

  # THE assertion: the Workload is held by the AdmissionCheck (state Retry) and NOT Admitted.
  # The Workload is the one owned by this case's Pod, found whether or not it carries the check: a
  # Workload admitted through a queue with no check is the failure that must be reported as such,
  # and skipping it would report "no Workload" instead.
  verdict=""
  for _ in $(seq 1 40); do
    read -r state admitted reserved reservedMsg <<<"$(wl_state)"
    [ "$state" = "Retry" ] && [ "$admitted" != "True" ] && { verdict=1; break; }
    [ "$state" = "Rejected" ] && { verdict=rejected; break; }
    # Admission ends the question, and the state is read at that moment: afterwards the admitted Pod
    # runs into a node with no such device and fails, and what the Workload says then is no longer the
    # verdict gate 3 gave.
    [ "$admitted" = "True" ] && break
    sleep 3
  done
  if [ "$verdict" = 1 ]; then
    record PASS "exclusive over-admit held by gate-3" "AdmissionCheck state=Retry, workload NOT Admitted (per-card infeasible)"
  elif [ "$verdict" = rejected ]; then
    record FAIL "exclusive over-admit held by gate-3" "state=Rejected (should be Retry — transient infeasibility, re-checked as capacity frees)"
  elif [ -z "$state" ]; then
    record FAIL "exclusive over-admit held by gate-3" "no Workload owned by Pod ${POD} appeared, so gate-3 was never asked"
  elif [ "$admitted" = "True" ] && [ "$state" = "-" ]; then
    record FAIL "exclusive over-admit held by gate-3" "admitted with no ${AC} check on the Workload at all — the queue it was admitted through carries none"
  elif [ "$admitted" = "True" ]; then
    record FAIL "exclusive over-admit held by gate-3" "admitted with check state=${state} — gate-3 admitted the over-request (ledger reverse-lookup or feasibility math)"
  elif [ "$reserved" != "True" ]; then
    record FAIL "exclusive over-admit held by gate-3" "the Workload never reserved quota, so gate-3 was never consulted (check state=${state}): QuotaReserved=${reserved} ${reservedMsg}"
  else
    record FAIL "exclusive over-admit held by gate-3" "quota reserved but the check never reported a verdict (state=${state}, admitted=${admitted})"
  fi

  # Positive: the same request once the cards are freed. Deleting the charging Pod frees them for
  # the check, and the Status is written back to free so the published ledger says the same. After
  # its Retry the Workload is reserved again, and this time the check must answer Ready. Without
  # this row the hold above cannot be told apart from a check that holds every request.
  if [ "$verdict" = 1 ]; then
    kubectl -n default delete pod "$HOLDER" --force --grace-period=0 >/dev/null 2>&1
    set_ledger "f f f f f f f f"
    for _ in $(seq 1 60); do
      read -r state admitted reserved reservedMsg <<<"$(wl_state)"
      [ "$admitted" = "True" ] && break
      sleep 3
    done
    if [ "$admitted" = "True" ] && [ "$state" = "Ready" ]; then
      record PASS "with the cards freed the same request is admitted" "AdmissionCheck state=Ready, workload Admitted"
    else
      record FAIL "with the cards freed the same request is admitted" "state=${state:-<none>} admitted=${admitted:-<none>} QuotaReserved=${reserved:-<none>} ${reservedMsg:-}"
    fi
  else
    record SKIP "with the cards freed the same request is admitted" "not attempted: the request was never held"
  fi
fi

if [ "$section1_failed" -eq 1 ]; then
  echo "[case-4] ${section1_reason}"
  record FAIL "exclusive over-admit held by gate-3" "${section1_reason}"
  record SKIP "with the cards freed the same request is admitted" "not attempted: the request was never held"
fi

# The charging Pod is this section's fixture. It goes when the section ends, whether or not the
# hold happened, so the four-view section below reads a ledger nothing else is charging.
kubectl -n default delete pod "$HOLDER" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1

# ---------------------------------------------------------------------------
# Section 2 — the pooled four-view and the watch.
#
# The shared setup above waited for the derived type; the row belongs to this section, so it is
# recorded here, after the over-admit section's rows.
# ---------------------------------------------------------------------------
record PASS "derived accelerated InstanceType" "${ITNAME}"

# The accelerated InstanceType also materializes spec.os/spec.arch from the backing ClusterQueue's
# kubernetes.io/os|arch labels — os/arch live only as schedule labels, never in the CQ notes, so
# they must be read from the labels, not blanked. They must match the schedule labels above.
read -r SPEC_OS SPEC_ARCH <<<"$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.os}{" "}{.spec.arch}')"
if [ -z "$OS" ] || [ -z "$ARCH" ]; then
  record FAIL "InstanceType materializes spec.os/arch" "the InstanceType carries no os/arch labels, so the schedule labels the CQ pins cannot be read back"
elif [ "$SPEC_OS" = "$OS" ] && [ "$SPEC_ARCH" = "$ARCH" ]; then
  record PASS "InstanceType materializes spec.os/arch" "spec os=${SPEC_OS} arch=${SPEC_ARCH} (from CQ labels)"
else
  record FAIL "InstanceType materializes spec.os/arch" "spec os='${SPEC_OS:-}' arch='${SPEC_ARCH:-}' vs labels ${OS}/${ARCH} — must read from the CQ kubernetes.io/os|arch labels, not the notes"
fi

# Walk the five-step pooling sequence; the four-view must match the oracle at each step.
set_ledger "f f f f f f f f" || abort_on_ledger
assert_view "init: 8 free" 8 80 800 0
set_ledger "e e f f f f f f" || abort_on_ledger
assert_view "step 1: +2 exclusive" 6 60 600 0
set_ledger "e e s9 s9 f f f f" || abort_on_ledger
assert_view "step 2: +2 shared (9 free)" 4 58 400 0
set_ledger "e e s9 s9 l80 l80 f f" || abort_on_ledger
assert_view "step 3: +2 logical slices (80% free)" 2 38 360 0
set_ledger "e e s9 s9 l78 l78 f f" || abort_on_ledger
assert_view "step 4: sliced cards drop to 78%" 2 38 356 0
set_ledger "e e e s9 s9 l78 l78 f" || abort_on_ledger
assert_view "step 5: +1 exclusive" 1 28 256 0

# Watch freshness: a native watch on the InstanceType observes the .status four-view move
# as the ledger allocs/frees (exclusive 8 → 4 → 8).
echo "[case-4] asserting watch freshness over kubectl get -w"
set_ledger "f f f f f f f f" || abort_on_ledger
assert_view "watch precondition: back to 8 free" 8 80 800 0 >/dev/null
watchlog=$(mktemp)
# Watch the NATIVE v1alpha1 CRD, not the unversioned name — the latter resolves to the aggregated
# worker.gpustack.ai/v1 apiservice (a proxy), whose watch re-projects/coalesces and drops intermediate
# .status transitions. Promoting InstanceType to a real CRD is precisely what lets a native watch
# deliver them; mirrors set_ledger targeting devices.v1alpha1 for the same aggregated-proxy reason.
( timeout 60 kubectl get instancetypes.v1alpha1.worker.gpustack.ai "$ITNAME" -w \
    -o "jsonpath={.status.accelerator.remaining}{'\n'}" >"$watchlog" 2>/dev/null ) &
wpid=$!
sleep 2
set_ledger "e e s9 s9 f f f f" || abort_on_ledger   # alloc → exclusive drops to 4
sleep 20                                # dwell long enough for a remote cluster's reconcile+watch to surface the drop
set_ledger "f f f f f f f f" || abort_on_ledger     # free → exclusive recovers to 8
sleep 15
wait "$wpid" 2>/dev/null || true
if grep -qx 4 "$watchlog" && grep -qx 8 "$watchlog"; then
  record PASS "watch freshness (kubectl get -w)" "observed exclusive 8→4→8 via native watch"
else
  record FAIL "watch freshness (kubectl get -w)" "watch missed a transition: [$(tr '\n' ' ' <"$watchlog")] — native watch on .status"
fi
rm -f "$watchlog"

# The unit spec is FROZEN after create (declarative-management: unitResources / localStorage are
# immutable on update), and its write path never touches the NodeFeature. Editing the accelerated type's unit spec must be REJECTED by the
# validating webhook and leave the stored spec unchanged.
echo "[case-4] attempting to edit InstanceType unit spec (must be rejected — immutable)"
nfBefore=$(kubectl -n "$NS" get nodefeature "$WORKER_NF" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.dumps(json.load(sys.stdin).get('spec',{}).get('labels',{}),sort_keys=True))
" 2>/dev/null)
cpuBefore=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
errEdit=$(kubectl patch instancetypes.worker.gpustack.ai "$ITNAME" --type=merge \
  -p '{"spec":{"unitResources":{"cpu":"2","ram":"8Gi"},"localStorage":"64Gi"}}' 2>&1)
cpuAfter=$(kubectl get instancetypes.worker.gpustack.ai "$ITNAME" -o jsonpath='{.spec.unitResources.cpu}' 2>/dev/null)
if { echo "$errEdit" | grep -qiE 'immutable' && [ -n "$cpuAfter" ] && [ "$cpuAfter" = "$cpuBefore" ]; }; then
  record PASS "unit-spec edit is rejected (immutable)" "unitResources frozen; spec.unitResources.cpu stayed ${cpuBefore}"
else
  record FAIL "unit-spec edit is rejected (immutable)" "err='${errEdit:0:70}' cpu ${cpuBefore}->${cpuAfter} — the unit spec must be immutable after create"
fi

nfAfter=$(kubectl -n "$NS" get nodefeature "$WORKER_NF" -o json 2>/dev/null | python3 -c "
import json,sys
print(json.dumps(json.load(sys.stdin).get('spec',{}).get('labels',{}),sort_keys=True))
" 2>/dev/null)
if [ "$nfBefore" = "$nfAfter" ]; then
  record PASS "unit-spec write does not touch NodeFeature" "worker NodeFeature spec.labels unchanged"
else
  record FAIL "unit-spec write does not touch NodeFeature" "worker NodeFeature spec.labels changed — the write leaked upward"
fi

echo
echo "== CASE 4 — Mocked accelerator pool: over-admit held, four-view tracks the ledger =="
print_rows

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Confirm the AdmissionCheck is Active, the backing CQ references it,"
  echo "the over-request Workload's check state is Retry (held), not Rejected and not Admitted, and"
  echo "that the four-view tracks the ledger, a native watch surfaces its transitions, and the unit"
  echo "spec is immutable with its rejected write leaving the NodeFeature alone."
  echo "Diagnose: kubectl -n ${NS} logs deploy/gpustack-operator-worker --tail=200 | grep -i admission"
  exit 1
fi
echo "CASE 4 PASS"
