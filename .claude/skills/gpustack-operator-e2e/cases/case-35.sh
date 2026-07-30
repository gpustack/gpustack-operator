#!/usr/bin/env bash
#
# CASE 35 — Ascend logical-slice placement: claims pack, spill only on a misfit, and never cross into an exclusive card   (MUTATING, self-recovering; AUTO-SKIPS without >=3 logically sliceable ascend cards)
#
#   case-35.sh <NS>
#
# Goal:        ASSERTS the per-card placement contract on Ascend, over sliced, exclusive and mixed
#              claims. Two properties are asserted SEPARATELY for every sliced claim, because they
#              fail for different reasons and one must not mask the other:
#                (1) THE DEFECT — when some card is already serving slices AND still has room for the
#                    claim, the claim must land on one of those cards. Opening a fresh card instead
#                    strands the node: many part-used cards, none able to host one large claim.
#                (2) THE POLICY — among every card that fits, the claim must take the FULLEST one
#                    (least remaining), so occupancy coalesces from the top down.
#              Placement is decided by the device plugin's GetPreferredAllocation hint, which kubelet
#              may only honour if every device id in it is one kubelet actually offered. A hint
#              kubelet cannot parse is discarded silently and every card choice degrades to arbitrary,
#              which is what this case is here to catch — on the vendor where it was first seen.
#
#              Expectations are COMPUTED from the ledger immediately before each claim, never
#              hardcoded, so the case is valid on a pool that already carries unrelated workloads.
#              That is deliberate: an accelerator cluster in use rarely offers a pristine pool, and a
#              case that demands one is a case that never runs. When a step's precondition is absent
#              (nothing in-use fits, so (1) cannot discriminate) it records SKIP, never a vacuous PASS.
# Environment: Needs REAL Ascend hardware with >=3 LOGICALLY SLICEABLE, HEALTHY cards in ONE accelerator
#              group on one node — a card in a hardware partitioning mode serves no logical slice and
#              does not count, and an unhealthy card's tokens are never offered to the kubelet.
#              AUTO-SKIPS (exit 0) on any of those. It additionally needs one card FREE at start, to
#              read a whole card's unit budget back; missing that it FAILS SETUP (exit 1) rather than
#              skipping, because every claim size would otherwise be computed against a zero. A
#              Devices query that errors also fails setup — "no ascend hardware" must not be
#              indistinguishable from "the query failed". Ascend-only: the runtime-confinement
#              cross-check reads the visible-devices variable the runtime injected, and each vendor
#              injects its own name. The placement assertions themselves read the allocation the
#              plugin recorded on the Pod, which is vendor-neutral — a sibling case for another vendor
#              needs only that variable's name.
#
#              The claim carrier must ship the Ascend userspace runtime (a CANN image); a bare base
#              image cannot run a slice at all — see the IMAGE assignment. Override with
#              E2E_SLICE_IMAGE=<ref> for another Ascend generation or a registry mirror. It is a large
#              image, so pre-pull it on the node if the first step is slow.
#
#              Presumes the node's kubelet runs topologyManagerPolicy=none, the suite's baseline.
#              GetPreferredAllocation is advisory by API contract, and under a restrictive policy
#              kubelet picks the NUMA-aligned set BEFORE consulting the plugin, bypassing the hint.
# Inputs:      All real, nothing mocked. Six raw Pods pinned to the target node's entrance LocalQueue,
#              submitted in one sequence that builds the state each later step needs:
#                s1 50%  — establishes an in-use card.
#                s2 30%  — fits beside s1, so it must join s1's card.
#                s3 50%  — s1's card can no longer hold it, so it must go elsewhere.
#                s4 10%  — two in-use cards now differ in how full they are; the fuller one must win.
#                e1 1 whole card (exclusive) — must come from outside the sliced population.
#                s5 10%  — packing must still hold while a card is held exclusively, and must not
#                          touch it.
# Expected:    - every sliced claim lands on an in-use card whenever one has room (the defect guard);
#              - every sliced claim lands on the fullest card that fits (the policy);
#              - the runtime's visible-devices agrees with the card the plugin recorded, every time;
#              - the exclusive claim lands outside the sliced population, is recorded exclusive, and
#                lowers the pool's Accelerator remaining;
#              - the sliced claim placed alongside it does not land on the exclusively held card;
#              - no card is over-committed, and the ledger's per-card usage equals the sum the Pods
#                placed there record for themselves.
# Cleanup:     Trap deletes the six test Pods and waits for the pool's SL remaining to return to the
#              value captured at setup, so the next case does not start on a pool this one still holds.
set -uo pipefail

NS="${1:?usage: case-35.sh <NS>}"
# Ascend's resource family. Hardcoded rather than derived: the case is gated on the ascend
# manufacturer below, and deriving the family here would duplicate a mapping the allocators own.
BASE=huawei.com/npu
SLICED=${BASE}.sliced
CORESPCT=${BASE}.sliced.cores-percentage
MEMPCT=${BASE}.sliced.memory-percentage
PFX=gpustack-e2e-ascend-pack
# The claim carrier. NOT a bare base image: allocating an Ascend logical slice installs an
# /etc/ld.so.preload that pulls in the Ascend userspace runtime (libdcmi.so, and libc_sec.so behind
# it), and in an image that does not ship those every process in the container — `sleep` included —
# dies with exit 127 (`error while loading shared libraries: libc_sec.so`). The vendor runtimeClass
# does NOT rescue it; the libraries have to be in the image. So this defaults to a CANN image, and is
# overridable for a different Ascend generation or a registry mirror.
IMAGE="${E2E_SLICE_IMAGE:-quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11}"

# --- Skip gate: a node with >=3 LOGICALLY SLICEABLE, HEALTHY ascend cards in ONE accelerator group.
#     The per-card slicing CAPABILITY lives in Devices.spec (the runtime ledger in .status carries no
#     capability), and only a healthy card reporting a logical slice count belongs to the population the
#     kubelet is offered tokens from — an unhealthy card would otherwise be counted toward the gate and
#     could be picked as the expected placement below, failing the case for a correct allocation.
#
#     Read the ledger ONCE and fail loudly if that read errors: silencing it turns an RBAC or
#     connectivity failure into "no ascend hardware", i.e. a clean exit 0, which is the worst outcome
#     for a regression guard. `pipefail` does not help here — the failure would reach an outer `read`
#     as an empty here-string, which succeeds. ---
if ! DEVICES_JSON=$(kubectl get devices -o json 2>&1); then
  echo "== CASE 35 — FAILED (setup) =="
  echo "Reading the Devices ledger failed, so this case cannot tell 'no ascend hardware' from 'the query"
  echo "did not answer'. It refuses to report a skip on either:"
  printf '%s\n' "$DEVICES_JSON" | head -5
  exit 1
fi
read -r SLICE_NODE NCARDS GROUPID NGROUPS <<<"$(printf '%s' "$DEVICES_JSON" | python3 -c "
import json,sys
best=('-',0,'-',0)
for d in json.load(sys.stdin).get('items',[]):
    groups=[]
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('manufacturer')!='ascend': continue
        n=sum(1 for a in g.get('accelerators',[])
              if (a.get('status',{}).get('logicalSliced',{}).get('count',0) or 0)>0
              and not a.get('status',{}).get('unhealthy'))
        if n>0: groups.append((n, g.get('id','')))
    if groups:
        n,gid=max(groups)
        if n>best[1]: best=(d['metadata']['name'], n, gid, len(groups))
print(best[0], best[1], best[2], best[3])
")"
if [ "${SLICE_NODE:--}" = "-" ] || [ "${NCARDS:-0}" -lt 3 ]; then
  echo "== CASE 35 — SKIPPED =="
  echo "No node reports >=3 healthy, logically sliceable ascend cards in one group (best='${SLICE_NODE:-none}',"
  echo "cards=${NCARDS:-0}). This case needs three so that two can carry slices while a third is taken"
  echo "whole, which is what makes the mixed sliced+exclusive step meaningful. Run it on such a node."
  exit 0
fi
if [ "${NGROUPS:-1}" -gt 1 ]; then
  echo "== CASE 35 — SKIPPED =="
  echo "${SLICE_NODE} carries sliceable ascend cards in ${NGROUPS} accelerator groups. The plugin computes"
  echo "'the fullest card that fits' WITHIN a group, walking the groups in spec order, so on a multi-group"
  echo "node the expected placement this case derives from one group would not be the choice the"
  echo "implementation makes — it would fail a correct allocation. Run it on a single-group node."
  exit 0
fi
echo "[case-35] target: ${SLICE_NODE} with ${NCARDS} healthy logically sliceable ascend card(s) in group '${GROUPID}'"

# The group's cards in SPEC order, and the unhealthy ones to exclude. The plugin orders candidates by
# their position in Devices.spec (reading each card's remaining from .status by id), so the tie-break
# below must be that position — a numeric card index only coincides with it when the spec list happens
# to be index-ordered.
read -r SPEC_ORDER UNHEALTHY_IDX <<<"$(printf '%s' "$DEVICES_JSON" | NODE="$SLICE_NODE" GID="$GROUPID" python3 -c "
import json,os,sys
node=os.environ['NODE']; gid=os.environ['GID']
order=[]; bad=[]
for d in json.load(sys.stdin).get('items',[]):
    if d['metadata']['name']!=node: continue
    for g in d.get('spec',{}).get('groups',[]):
        if g.get('id')!=gid: continue
        for a in g.get('accelerators',[]):
            order.append(str(a.get('index')))
            if a.get('status',{}).get('unhealthy'): bad.append(str(a.get('index')))
print(','.join(order) or '-', ','.join(bad) or '-')
")"
echo "[case-35] group card order (spec): ${SPEC_ORDER}; unhealthy excluded: ${UNHEALTHY_IDX}"

# The accelerated InstanceType whose POOL this node belongs to (its name carries the node's
# accelerator group id), and its entrance LocalQueue. This must match the target node, or the queue
# would route the claim to a different pool's nodes.
read -r IT LQ <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | GID="$GROUPID" python3 -c "
import json,sys,os
gid=os.environ.get('GID','')
for it in json.load(sys.stdin).get('items',[]):
    s=it.get('spec',{}); name=it['metadata']['name']; st=it.get('status',{})
    sd=(st.get('detail',{}) or {}).get('slicedDetail',{}) or {}
    if s.get('acceleratable') and ((sd.get('logical',{}) or {}).get('count',0) or 0)>0 and gid and gid in name:
        print(name, st.get('entrance','')); break
")"
[ -n "$IT" ] && [ -n "$LQ" ] || { echo "no logically sliceable ascend InstanceType with an entrance LocalQueue found"; exit 1; }

# The logical-slicing runtime needs the vendor runtimeClass to mount its driver-lib dependencies — a
# bare image without it fails to start. Identity map from the pool manufacturer; guard on existence.
RUNTIMECLASS=""
kubectl get runtimeclass.node.k8s.io ascend >/dev/null 2>&1 && RUNTIMECLASS=ascend
RTC_LINE=""; [ -n "$RUNTIMECLASS" ] && RTC_LINE="runtimeClassName: ${RUNTIMECLASS}"
echo "[case-35] pool ${IT} via LocalQueue ${LQ}; slice pods runtimeClass: ${RUNTIMECLASS:-<none>}"

# ledger_json — the target node's per-card runtime ledger for this accelerator group, unhealthy cards
# excluded, each card carrying its position in the SPEC list (the order the plugin's stable sort keeps
# on a tie). A missing `remaining` means ZERO, not a whole card: the field is omitempty, so a fully
# allocated card omits it — defaulting the other way reads a 100%-full card as untouched.
ledger_json() {
  kubectl get devices "$SLICE_NODE" -o json 2>/dev/null \
    | GID="$GROUPID" SPEC_ORDER="$SPEC_ORDER" SKIP_IDX="$UNHEALTHY_IDX" python3 -c "
import json,os,sys
gid=os.environ.get('GID','')
order=[x for x in os.environ.get('SPEC_ORDER','').split(',') if x and x!='-']
skip={x for x in os.environ.get('SKIP_IDX','').split(',') if x and x!='-'}
out=[]
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if gid and g.get('id')!=gid: continue
    for a in g.get('accelerators',[]):
        idx=str(a.get('index'))
        if idx in skip: continue
        out.append({'index':a.get('index'),
                    'pos':order.index(idx) if idx in order else 1<<30,
                    'mode':a.get('mode',0),
                    'remaining':int(a.get('remaining') or 0)})
print(json.dumps(out))
"
}

# One whole card's unit budget, read from a FREE card (whose remaining is exactly that budget) rather
# than assumed. Captured once, before this case dirties anything.
MAX_UNITS=$(ledger_json | python3 -c "
import json,sys
free=[c['remaining'] for c in json.load(sys.stdin) if c['mode']==0]
print(max(free) if free else 0)
")
if [ "${MAX_UNITS:-0}" -le 0 ]; then
  echo "== CASE 35 — FAILED (setup) =="
  echo "No free card on ${SLICE_NODE}, so one card's unit budget cannot be read back, and every claim"
  echo "size below would be computed against a zero — making the fit assertions vacuous. Free a card"
  echo "on this node (or wait for the ledger to settle) and re-run."
  ledger_json
  exit 1
fi
SL_BASE=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
echo "[case-35] one card = ${MAX_UNITS} units; pool SL remaining at start = ${SL_BASE:-?}"

restore() {
  echo
  echo "[case-35] cleanup: deleting test Pods"
  local i
  for i in 1 2 3 4 5 6; do
    kubectl -n default delete pod "${PFX}-${i}" --ignore-not-found --force --grace-period=0 >/dev/null 2>&1 || true
  done
  # Wait for the pool to give back what this case took, so the next case does not misread the ledger.
  local now
  for _ in $(seq 1 20); do
    now=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.acceleratorSliced.remaining}' 2>/dev/null)
    [ -n "$now" ] && [ -n "${SL_BASE:-}" ] && [ "$now" = "$SL_BASE" ] && { echo "[case-35] pool SL remaining back to ${now}"; return 0; }
    sleep 3
  done
  echo "[case-35] WARNING: pool SL remaining is '${now:-?}', expected '${SL_BASE:-?}' — the ledger has not settled back"
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

# plan_for <claim-units> -> "<fullestFitting|-> <inUseFittingCsv|->"
#
# The two expectations, computed from the ledger as it stands right now. A card can serve a logical
# slice only from the logical population: free (may join it) or already sliced (has joined). A card
# held exclusively, shared or partitioned is not a candidate at all, which is why the mixed step needs
# no special case here. Ties go to the earlier SPEC position, not the lower card index — that is what
# the implementation's stable sort preserves.
plan_for() {
  ledger_json | CLAIM="$1" python3 -c "
import json,os,sys
claim=int(os.environ['CLAIM'])
cards=json.load(sys.stdin)
cand=[c for c in cards if c['mode'] in (0,3) and c['remaining']>=claim]
inuse=[c['index'] for c in cand if c['mode']==3]
best=min(cand, key=lambda c:(c['remaining'], c['pos']))['index'] if cand else '-'
print(best, ','.join(str(i) for i in inuse) or '-')
"
}

# submit <index> <pct> — a sliced claim; "" pct means an exclusive whole-card claim.
submit() {
  local idx="$1" pct="$2" res
  if [ -n "$pct" ]; then
    res="{ ${SLICED}: \"1\", ${CORESPCT}: \"${pct}\", ${MEMPCT}: \"${pct}\" }"
  else
    res="{ ${BASE}: \"1\" }"
  fi
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${PFX}-${idx}, namespace: default, labels: { kueue.x-k8s.io/queue-name: ${LQ} } }
spec:
  schedulerName: default-scheduler
  ${RTC_LINE}
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${SLICE_NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "86400"]
      resources:
        limits:   ${res}
        requests: ${res}
EOF
  for _ in $(seq 1 40); do
    [ "$(kubectl -n default get pod "${PFX}-${idx}" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] && return 0
    sleep 3
  done
  # The Running gate matters beyond liveness: the plugin writes the allocation annotation during
  # Allocate, BEFORE the container runs, so a container that dies still advertises a placement. Reading
  # it back without this gate would report a confident card for a slice that never worked.
  echo "[case-35] ${PFX}-${idx} never reached Running"
  kubectl -n default get pod "${PFX}-${idx}" \
    -o jsonpath='  exit={.status.containerStatuses[0].state.terminated.exitCode} reason={.status.containerStatuses[0].state.terminated.reason}{"\n"}' 2>/dev/null
  kubectl -n default logs "${PFX}-${idx}" --tail=5 2>&1 | sed 's/^/  /' | head -6
  echo "  exit 127 with a missing shared library means ${IMAGE} does not ship the Ascend userspace"
  echo "  runtime — set E2E_SLICE_IMAGE to a CANN image (see the IMAGE assignment)."
  kubectl -n default describe pod "${PFX}-${idx}" 2>/dev/null | tail -12
  return 1
}

# placement_of <index> -> "<cardIndex> <tokenIndex> <allocatedUnits> <mode>", from the allocation the
# plugin recorded on the Pod. This is the operator's own view of where the claim went.
placement_of() {
  kubectl -n default get pod "${PFX}-$1" -o json 2>/dev/null | python3 -c "
import json,sys
ann=(json.load(sys.stdin)['metadata'].get('annotations') or {}).get('device.gpustack.ai/accelerator.allocated')
if not ann: sys.exit(1)
a=json.loads(ann).get('main') or {}
ids=a.get('deviceIDs') or []
tok=ids[0].rsplit(':',1)[-1] if ids else '-'
modes={0:'free',1:'exclusive',2:'shared',3:'sliced',4:'partitioned'}
for g in a.get('devices',{}).get('groups',[]):
    for c in g.get('accelerators',[]):
        print(c.get('index'), tok, c.get('allocated'), modes.get(c.get('mode'),'?')); sys.exit(0)
sys.exit(1)
"
}

# visible_of <index> -> the card the RUNTIME confined the container to.
# Retries: a Pod reports Running slightly before its container is attachable, and the Ascend driver
# writes a "DrvMngGetConsoleLogLevel failed" notice to STDOUT inside the container, so the value is
# the last non-empty line rather than the whole capture.
visible_of() {
  local out
  for _ in $(seq 1 10); do
    out=$(kubectl -n default exec "${PFX}-$1" -c main -- printenv ASCEND_VISIBLE_DEVICES 2>/dev/null \
          | tr -d '\r' | grep -v "DrvMng" | awk 'NF{last=$0} END{print last}')
    [ -n "$out" ] && { echo "$out"; return 0; }
    sleep 3
  done
  return 1
}

# step_sliced <index> <pct> <label> — submit a sliced claim and assert both placement properties.
step_sliced() {
  local idx="$1" pct="$2" label="$3"
  local units=$((pct * MAX_UNITS / 100))
  read -r want_best want_inuse <<<"$(plan_for "$units")"
  echo "[case-35] ${label}: ${pct}% = ${units} units; fullest-that-fits=${want_best} in-use-that-fit=${want_inuse}"

  if ! submit "$idx" "$pct"; then
    record FAIL "${label}: joins an in-use card when one has room" "the claim never reached Running (reason logged above)"
    record FAIL "${label}: takes the fullest card that fits" "not evaluated — the claim did not run"
    return 1
  fi
  local card token alloc mode
  read -r card token alloc mode <<<"$(placement_of "$idx")"
  if [ -z "${card:-}" ]; then
    record FAIL "${label}: joins an in-use card when one has room" "no allocation recorded on the Pod"
    record FAIL "${label}: takes the fullest card that fits" "not evaluated — no allocation recorded"
    return 1
  fi
  echo "[case-35] ${label} → card ${card} token ${token} (${alloc} units, ${mode})"

  # (1) The defect guard. Only discriminating when some in-use card had room; otherwise SKIP rather
  # than record a PASS that asserted nothing.
  if [ "$want_inuse" = "-" ]; then
    record SKIP "${label}: joins an in-use card when one has room" \
      "no card was already serving slices with room for ${pct}% — nothing to join, so this cannot discriminate"
  else
    case ",${want_inuse}," in *",${card},"*) rc=0 ;; *) rc=1 ;; esac
    (exit $rc)
    verdict $? "${label}: joins an in-use card when one has room" \
      "card ${card}, one of the in-use cards that fit {${want_inuse}}" \
      "card ${card}, but cards {${want_inuse}} were already serving slices with room for ${pct}% — a fresh card was opened instead"
  fi

  # (2) The policy.
  [ "$card" = "$want_best" ]
  verdict $? "${label}: takes the fullest card that fits" \
    "card ${card} was the fullest card that fits" \
    "card ${card}, but card ${want_best} was the fullest that fits ${pct}%"

  # The runtime must agree with what the operator recorded, or the ledger is bookkeeping fiction.
  local vis; vis=$(visible_of "$idx")
  [ -n "$vis" ] && [ "$vis" = "$card" ]
  verdict $? "${label}: runtime confinement agrees with the ledger" \
    "ASCEND_VISIBLE_DEVICES=${vis} matches card ${card}" \
    "ASCEND_VISIBLE_DEVICES='${vis:-<unreadable>}' vs recorded card ${card}"
  LAST_CARD="$card"
  # Let the per-card ledger reflect this placement before the next claim is planned against it.
  sleep 6
  return 0
}

# --- Sliced: pack, then spill only on a genuine misfit, then prefer the fuller of two in-use cards. ---
step_sliced 1 50 "s1 50%"
step_sliced 2 30 "s2 30%"
step_sliced 3 50 "s3 50%"
step_sliced 4 10 "s4 10%"

# --- Exclusive: a whole-card claim must come from outside the sliced population. ---
echo "[case-35] e1: exclusive whole card"
EXCL_CARD=""
excl_before=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
# The cards serving slices right now — the set the exclusive claim must avoid.
sliced_cards=$(ledger_json | python3 -c "
import json,sys
print(','.join(str(c['index']) for c in json.load(sys.stdin) if c['mode']==3) or '-')")
# A whole-card claim needs a card no mode holds. With only three sliceable cards, and a pool that
# already carried slices when this case started, the sequence above can legitimately have taken the
# last free one — the claim would then sit Pending forever and fail a cluster that met every
# documented prerequisite. Skip the exclusive and mixed steps instead of reporting a defect.
free_now=$(ledger_json | python3 -c "
import json,sys
print(sum(1 for c in json.load(sys.stdin) if c['mode']==0))")
if [ "${free_now:-0}" -le 0 ]; then
  for chk in "exclusive claim lands outside the sliced population" \
             "exclusive claim is recorded exclusive" \
             "exclusive claim: runtime confinement agrees with the ledger" \
             "exclusive claim lowers the pool's Accelerator remaining"; do
    record SKIP "$chk" "no free card left on ${SLICE_NODE} — the sliced steps consumed them, so a whole-card claim has nowhere to land through no fault of the implementation"
  done
elif submit 5 ""; then
  read -r ecard etoken ealloc emode <<<"$(placement_of 5)"
  EXCL_CARD="${ecard:-}"
  echo "[case-35] e1 → card ${ecard:-?} (${ealloc:-?} units, ${emode:-?}); sliced cards were {${sliced_cards}}"
  case ",${sliced_cards}," in *",${ecard},"*) rc=1 ;; *) rc=0 ;; esac
  (exit $rc)
  verdict $? "exclusive claim lands outside the sliced population" \
    "card ${ecard}, not one of the sliced cards {${sliced_cards}}" \
    "card ${ecard} is already serving logical slices — a whole-card claim must not share it"
  [ "${emode:-}" = "exclusive" ]
  verdict $? "exclusive claim is recorded exclusive" \
    "mode=${emode} allocated=${ealloc}" \
    "mode='${emode:-?}', expected exclusive"
  evis=$(visible_of 5)
  [ -n "$evis" ] && [ "$evis" = "$ecard" ]
  verdict $? "exclusive claim: runtime confinement agrees with the ledger" \
    "ASCEND_VISIBLE_DEVICES=${evis} matches card ${ecard}" \
    "ASCEND_VISIBLE_DEVICES='${evis:-<unreadable>}' vs recorded card ${ecard}"
  # The pool's EX view must lose a whole card. Reconciled after the Pod is already Running, so poll.
  #
  # NOT the node's <base> allocatable: in the device-plugin model that counts ADVERTISED devices and
  # kubelet accounts an allocated one as in-use, so it does not move here. It falls only when a card
  # LEAVES the exclusive population — e.g. when it starts serving logical slices.
  excl_after=""
  for _ in $(seq 1 20); do
    excl_after=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
    [ -n "$excl_after" ] && [ -n "$excl_before" ] && [ "$excl_after" -lt "$excl_before" ] && break
    sleep 3
  done
  { [ -n "$excl_after" ] && [ -n "$excl_before" ] && [ "$excl_after" -lt "$excl_before" ]; }
  verdict $? "exclusive claim lowers the pool's Accelerator remaining" \
    "accelerator.remaining ${excl_before} -> ${excl_after}" \
    "accelerator.remaining '${excl_before:-?}' -> '${excl_after:-?}' — a whole-card claim did not consume pool capacity"
else
  record FAIL "exclusive claim lands outside the sliced population" "the exclusive claim never reached Running"
  record FAIL "exclusive claim is recorded exclusive" "not evaluated"
  record FAIL "exclusive claim lowers the pool's Accelerator remaining" "not evaluated"
fi

# --- Mixed: packing must still hold with a card held exclusively, and must not touch that card. ---
LAST_CARD=""
step_sliced 6 10 "s5 10% (mixed)"
if [ -n "$EXCL_CARD" ] && [ -n "$LAST_CARD" ]; then
  [ "$LAST_CARD" != "$EXCL_CARD" ]
  verdict $? "mixed: a sliced claim never lands on the exclusively held card" \
    "sliced card ${LAST_CARD} != exclusive card ${EXCL_CARD}" \
    "sliced card ${LAST_CARD} == exclusive card ${EXCL_CARD} — cross-mode isolation broken"
else
  record SKIP "mixed: a sliced claim never lands on the exclusively held card" \
    "not evaluated — the exclusive claim or the mixed sliced claim did not place"
fi

# --- The invariant the whole ledger exists to guarantee. ---
echo "[case-35] per-card invariant (ledger vs the Pods' own records)"
INV=$(python3 - "$SLICE_NODE" "$GROUPID" "$MAX_UNITS" <<'EOF'
import json, subprocess, sys

node, gid, mx = sys.argv[1], sys.argv[2], int(sys.argv[3])
ANN = "device.gpustack.ai/accelerator.allocated"

def kget(*args):
    return json.loads(subprocess.check_output(["kubectl", "get", *args, "-o", "json"]))

# What the Pods themselves record, per card.
claimed, holders = {}, {}
# Only this node's Pods. Card indices restart at 0 on every node, so summing the cluster's Pods by
# group and index would fold a sibling node's allocations into this node's cards and report a phantom
# over-commit or ledger disagreement.
for pod in kget("pods", "-A", "--field-selector", "spec.nodeName=" + node).get("items", []):
    raw = (pod["metadata"].get("annotations") or {}).get(ANN)
    if not raw:
        continue
    ref = pod["metadata"]["namespace"] + "/" + pod["metadata"]["name"]
    for cname, cval in json.loads(raw).items():
        for g in cval.get("devices", {}).get("groups", []):
            if gid and g.get("id") != gid:
                continue
            for c in g.get("accelerators", []):
                i = c.get("index")
                claimed[i] = claimed.get(i, 0) + int(c.get("allocated") or 0)
                holders.setdefault(i, []).append(ref)

bad = 0
for g in kget("devices", node).get("status", {}).get("groups", []):
    if gid and g.get("id") != gid:
        continue
    for c in g.get("accelerators", []):
        i = c.get("index")
        rem = int(c.get("remaining") or 0)
        ledger_used, pods_used = mx - rem, claimed.get(i, 0)
        note = []
        if pods_used > mx:
            note.append("OVER-COMMITTED"); bad += 1
        if rem < 0:
            note.append("NEGATIVE-REMAINING"); bad += 1
        # Compare only cards serving logical slices: an exclusive or shared card spends its whole
        # budget in the ledger while the Pod records that mode's own accounting.
        if c.get("mode") == 3 and ledger_used != pods_used:
            note.append("LEDGER!=PODS"); bad += 1
        print("  card={:<2} ledger_used={:>9} pods_used={:>9} {:<18} {}".format(
            i, ledger_used, pods_used, " ".join(note) or "ok", ",".join(holders.get(i, [])) or "-"))
sys.exit(1 if bad else 0)
EOF
)
inv_rc=$?
echo "$INV"
(exit $inv_rc)
verdict $? "no card over-committed, and the ledger matches the Pods" \
  "every card within its budget and every sliced card's ledger usage equal to the sum its Pods record" \
  "see the table above — an over-committed card, a negative remaining, or a ledger/Pod disagreement"

echo
echo "== CASE 35 — Ascend logical-slice placement: claims pack, spill only on a misfit, and never cross into an exclusive card =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Placement is decided by the device plugin's GetPreferredAllocation"
  echo "hint; a hint kubelet cannot read is discarded silently and every card choice becomes arbitrary,"
  echo "which shows up here as a claim opening a fresh card while an in-use one had room, or as a card"
  echo "over-committed. Raise the device-manager's verbosity BEFORE re-running to see the decision (see"
  echo "the shared troubleshooting reference, 'Component log verbosity') — the hint is logged in full,"
  echo "and a device id in it WITHOUT a trailing ':<token>' segment is the defect itself."
  echo "Diagnose: kubectl get devices ${SLICE_NODE} -o yaml;"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-ascend --tail=100"
  exit 1
fi
echo "CASE 35 PASS"
