#!/usr/bin/env bash
#
# CASE 22 — Exclusive and shared claims never co-locate on one physical card
#   (MUTATING, self-recovering; AUTO-SKIPS without a real GPU advertising both <vendor>/gpu and <vendor>/gpu.shared)
#
#   case-22.sh <NS>
#
# Goal:        A whole-card exclusive claim (<vendor>/gpu) and a shared claim (<vendor>/gpu.shared) are
#              mutually exclusive on the SAME physical card. When every card on a node is already held
#              exclusively, a new shared claim must be HELD (never placed onto an exclusive card), and
#              vice-versa. This must hold on BOTH request paths:
#                - variant A: the Kueue-managed path (Pod carries the pool's LocalQueue label) — the
#                  node-devices AdmissionCheck / quota reads the per-card ledger and holds the shared claim;
#                - variant B: the RAW path (no kueue.x-k8s.io/queue-name label) — the Pod admission webhook
#                  and the Kueue AdmissionCheck are both bypassed, so ONLY the on-node device plugin decides.
#              The device plugin advertises exclusive and shared as two independent resource names backed by
#              two independent device-ID pools that alias the same physical cards; nothing at Allocate time
#              rejects a shared token on a card already held exclusively. This case proves whether that gap
#              lets an exclusive and a shared claim share one physical card.
# Environment: A node advertising a real accelerator with BOTH the exclusive whole-card resource
#              (<vendor>/gpu > 0) AND the shared resource (<vendor>/gpu.shared > 0). AUTO-SKIPS (exit 0)
#              otherwise — the co-location can only be observed on hardware the device plugin allocates.
# Inputs:      All real, nothing mocked — N = the node's allocatable whole cards. Per variant: N exclusive
#              Pods (<vendor>/gpu:1) created SEQUENTIALLY (one Running on a distinct card before the next) to
#              occupy every card, then 1 shared Pod (<vendor>/gpu.shared:1). Variant A tags every Pod with the
#              accelerated pool's entrance LocalQueue; variant B tags none.
#              Ground truth is each Pod's device.gpustack.ai/accelerator.allocated annotation (the physical
#              card <group>:<device> the plugin placed it on + the mode it stamped), corroborated by
#              NVIDIA_VISIBLE_DEVICES.
# Expected:    Both variants — with every card already exclusive, the shared Pod is HELD (never allocated a
#              card). A shared Pod that is allocated a card already in the exclusive set FAILS the case
#              (mutual exclusion violated). The two variants are expected to DIVERGE on buggy code: the
#              Kueue path (A) holds the shared claim; the raw path (B) co-locates it.
# Cleanup:     Trap deletes every test Pod (both variants) and waits for the accelerated pool's whole-card
#              capacity to free again. Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: case-22.sh <NS>}"
PODPFX=gpustack-e2e-exshare
ANNO=device.gpustack.ai/accelerator.allocated

# --- Skip gate: a node advertising BOTH <vendor>/gpu (exclusive whole card) and <vendor>/gpu.shared. ---
read -r GPU_NODE EXCL SHARED N <<<"$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys,re
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        m=re.match(r'^([^/]+)/gpu\$', k)           # the bare exclusive whole-card resource, e.g. nvidia.com/gpu
        if not m: continue
        try: cnt=int(v)
        except: continue
        if cnt<=0: continue
        shared=k+'.shared'
        try: scnt=int(a.get(shared,'0'))
        except: scnt=0
        if scnt>0:
            print(n['metadata']['name'], k, shared, cnt); sys.exit(0)
" 2>/dev/null)"
if [ -z "${GPU_NODE:-}" ] || [ -z "${N:-}" ]; then
  echo "== CASE 22 — SKIPPED =="
  echo "No node advertises both an exclusive whole-card resource (<vendor>/gpu) and its shared companion"
  echo "(<vendor>/gpu.shared). This case needs a real accelerator the device plugin allocates, so exclusive"
  echo "vs shared co-location on one physical card can be observed. Run it on such a GPU cluster."
  exit 0
fi
echo "[case-22] GPU node ${GPU_NODE}: ${N}× whole card (${EXCL}), shared companion ${SHARED}"

# The accelerated pool that actually backs THIS node's cards. Match the InstanceType whose name carries
# the node's nvidia accelerator group id (robust against leftover mock/sibling pools that another case may
# have left behind with zero capacity), requiring an entrance LocalQueue AND real whole-card capacity. Fall
# back to the highest-capacity acceleratable pool with an entrance queue. Capacity feeds the ledger
# freshness / cards-freed waits.
GROUPID=$(kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
for g in json.load(sys.stdin).get('status',{}).get('groups',[]):
    if g.get('manufacturer')=='nvidia' and g.get('accelerators'):
        print(g.get('id','')); break
" 2>/dev/null)
read -r IT LQ CAP <<<"$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null | NODEGID="$GROUPID" python3 -c "
import json,sys,os
gid=os.environ.get('NODEGID','')
items=json.load(sys.stdin).get('items',[])
def cap(it):
    try: return int(it.get('status',{}).get('accelerator',{}).get('capacity',0) or 0)
    except Exception: return 0
# 1) the exact pool for this node's accelerator group id, with an entrance queue and real capacity.
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and gid and gid in it['metadata']['name'] and st.get('entrance') and cap(it)>0:
        print(it['metadata']['name'], st['entrance'], cap(it)); sys.exit(0)
# 2) fall back to the highest-capacity acceleratable pool with an entrance queue.
best=None
for it in items:
    s=it.get('spec',{}); st=it.get('status',{})
    if s.get('acceleratable') and st.get('entrance') and cap(it)>0 and (best is None or cap(it)>cap(best)):
        best=it
if best is not None:
    st=best.get('status',{}); print(best['metadata']['name'], st['entrance'], cap(best))
")"
[ -n "${IT:-}" ] && [ -n "${LQ:-}" ] || { echo "[case-22] no accelerated InstanceType with an entrance LocalQueue + real capacity — chain not materialized"; exit 1; }
echo "[case-22] accelerated InstanceType ${IT} (entrance LocalQueue ${LQ}, whole-card capacity ${CAP:-?}, node group ${GROUPID:-?})"

TESTPODS=()
restore() {
  echo
  echo "[case-22] cleanup: deleting all test Pods and waiting for whole-card capacity to free"
  for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
  # Best-effort settle: let the device plugin deallocate and the ledger recover.
  for _ in $(seq 1 20); do
    rem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
    [ -n "$CAP" ] && [ "${rem:-0}" = "$CAP" ] && break
    sleep 3
  done
}
trap restore EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# mkpod <name> <resource> <lq-or-empty> — a Pod requesting 1 unit of <resource>, pinned to the GPU node,
# tagged with the LocalQueue label only when <lq> is non-empty (variant A vs variant B).
mkpod() {
  local name="$1" res="$2" lq="$3" labels=""
  [ -n "$lq" ] && labels="{ kueue.x-k8s.io/queue-name: ${lq} }" || labels="{}"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${name}, namespace: default, labels: ${labels} }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  containers:
    - name: main
      image: ubuntu:24.04
      command: ["sleep", "86400"]
      resources:
        limits:   { ${res}: "1" }
        requests: { ${res}: "1" }
EOF
}

running() { [ "$(kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null)" = "Running" ]; }
wait_running() { for _ in $(seq 1 40); do running "$1" && return 0; sleep 3; done; return 1; }

# pod_cards <pod> — the sorted "<group>:<device>" physical card(s) the device plugin allocated to <pod>,
# read from its allocation annotation (present once Allocate ran, even before the container starts). Empty
# when the Pod was never allocated a card (held / still pending).
pod_cards() {
  kubectl -n default get pod "$1" -o json 2>/dev/null | ANNO="$ANNO" python3 -c "
import json,sys,os
try: o=json.load(sys.stdin)
except Exception: sys.exit(0)
ann=o.get('metadata',{}).get('annotations',{}).get(os.environ['ANNO'],'')
if not ann: sys.exit(0)
try: st=json.loads(ann)
except Exception: sys.exit(0)
cards=set()
for g in st.get('groups',[]):
    gid=g.get('id','')
    for a in g.get('accelerators',[]):
        cards.add('%s:%s' % (gid, a.get('id','')))
print(' '.join(sorted(cards)))
"
}

# held_reason <pod> — a CONCRETE signal that the shared claim was held for a legitimate reason, not merely
# slow to schedule: a device-plugin admission refusal (phase Failed / UnexpectedAdmissionError), a Kueue
# scheduling gate (Kueue holding an un-admitted Workload), or an Unschedulable/FailedScheduling verdict.
# Empty when no such signal is present — in which case "not allocated yet" is inconclusive (a Pod merely
# mid-scheduling could still be about to co-locate), so the held branch must not score it a silent PASS.
held_reason() {
  local p="$1" phase reason gates cond ev
  phase="$(kubectl -n default get pod "$p" -o jsonpath='{.status.phase}' 2>/dev/null)"
  reason="$(kubectl -n default get pod "$p" -o jsonpath='{.status.reason}' 2>/dev/null)"
  [ "$phase" = Failed ] && { echo "Failed/${reason:-admission-refused}"; return; }
  gates="$(kubectl -n default get pod "$p" -o jsonpath='{.spec.schedulingGates[*].name}' 2>/dev/null)"
  [ -n "$gates" ] && { echo "scheduling-gated(${gates})"; return; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = Unschedulable ] && { echo "Unschedulable"; return; }
  ev="$(kubectl -n default get events --field-selector involvedObject.name="$p" -o jsonpath='{range .items[*]}{.reason} {end}' 2>/dev/null | tr ' ' '\n' | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1)"
  [ -n "$ev" ] && { echo "$ev"; return; }
  echo ""
}

# variant <tag> <lq-or-empty> — occupy every card exclusively, then submit one shared claim and assert it
# is not co-located onto an exclusive card. Records one PASS/FAIL row.
variant() {
  local tag="$1" lq="$2"
  local tagl="${tag,,}"   # lowercase for RFC-1123 Pod names; ${tag} stays for display
  local path; [ -n "$lq" ] && path="via Kueue LocalQueue ${lq}" || path="raw (no Kueue queue label)"
  echo
  echo "[case-22] === variant ${tag}: ${path} ==="

  # 1. Occupy every physical card with an exclusive claim — SEQUENTIALLY: create one, wait until it is
  #    Running with its allocation annotation recorded on a DISTINCT card, then create the next. Filling one
  #    at a time keeps at most one identical GPU Pod pending, so the "all cards exclusive" precondition is
  #    established deterministically without depending on concurrent admission (the concurrent-admission
  #    pod-identification path is covered directly by the device-plugin unit tests, not here).
  local excl=() i p c cc seen=" "
  for i in $(seq 1 "$N"); do
    p="${PODPFX}-${tagl}-excl-${i}"; excl+=("$p"); TESTPODS+=("$p")
    mkpod "$p" "$EXCL" "$lq"
    if ! wait_running "$p"; then
      record FAIL "variant ${tag}: all cards occupied exclusively" "exclusive Pod ${p} did not reach Running — cannot set up the collision precondition"
      return
    fi
    # Wait until the plugin recorded this Pod's allocation (one physical card), then require it to be a
    # card no earlier exclusive Pod already holds — a repeat means the per-card accounting collided.
    c=""
    for _ in $(seq 1 20); do c="$(pod_cards "$p")"; [ -n "$c" ] && break; sleep 3; done
    if [ -z "$c" ]; then
      record FAIL "variant ${tag}: all cards occupied exclusively" "exclusive Pod ${p} is Running but recorded no allocation annotation — cannot confirm it holds a card"
      return
    fi
    for cc in $c; do
      case "$seen" in
        *" $cc "*)
          record FAIL "variant ${tag}: all cards occupied exclusively" "exclusive Pod ${p} was attributed card ${cc} already held by an earlier Pod — per-card accounting collided"
          return;;
      esac
      seen="${seen}${cc} "
    done
  done
  # Let the per-card ledger reflect the exclusive occupancy (fresh ledger is what the Kueue AdmissionCheck
  # reads to hold the shared claim on variant A; harmless on variant B).
  for _ in $(seq 1 20); do
    rem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
    [ "${rem:-1}" = "0" ] && break
    sleep 3
  done

  # Union of the physical cards the exclusive Pods hold (= every card, since the exclusive pool has one
  # token per card).
  local E=" "
  for p in "${excl[@]}"; do E="${E}$(pod_cards "$p") "; done
  echo "[case-22]   ${N} exclusive Pods occupy card(s):$(printf '%s' "$E")(ledger accelerator.remaining=${rem:-?})"
  record PASS "variant ${tag}: all cards occupied exclusively" "${N} exclusive Pods Running on distinct cards"

  # 2. Submit one shared claim onto the same node.
  local sp="${PODPFX}-${tagl}-shared"; TESTPODS+=("$sp")
  mkpod "$sp" "$SHARED" "$lq"

  # 3. THE assertion: the shared claim must never be allocated a card already held exclusively. Poll for a
  #    device-plugin allocation; a card ∈ E means co-location (bug). No allocation within the window means
  #    it was correctly HELD.
  local scards="" phase="" verdict="held"
  for _ in $(seq 1 20); do
    scards="$(pod_cards "$sp")"
    if [ -n "$scards" ]; then verdict="allocated"; break; fi
    sleep 3
  done
  phase="$(kubectl -n default get pod "$sp" -o jsonpath='{.status.phase}' 2>/dev/null)"
  local nvd; nvd="$(kubectl -n default exec "$sp" -- printenv NVIDIA_VISIBLE_DEVICES 2>/dev/null)"

  if [ "$verdict" = held ]; then
    # Not allocated within the window. Require a CONCRETE held signal (device-plugin refusal / Kueue gate /
    # Unschedulable) before scoring PASS — a bare "no allocation yet" could hide a Pod that is merely slow
    # and about to co-locate. Give a slow verdict a little longer to surface the reason.
    local hr; hr="$(held_reason "$sp")"
    if [ -z "$hr" ]; then for _ in 1 2 3 4 5; do sleep 3; hr="$(held_reason "$sp")"; [ -n "$hr" ] && break; done; fi
    if [ -n "$hr" ]; then
      record PASS "variant ${tag}: shared claim held (no co-location)" "shared Pod not allocated any card, held for a real reason [${hr}] (phase=${phase:-<none>}); every card is exclusive"
    else
      record FAIL "variant ${tag}: shared claim held (no co-location)" "shared Pod not allocated but shows NO concrete held signal (Failed/gated/Unschedulable) — cannot confirm held vs merely slow (phase=${phase:-<none>}); re-check ${path}"
    fi
    return
  fi
  # Allocated a card — is it one of the exclusive cards? (It must be: every card is exclusive.)
  local collide=""
  for c in $scards; do case "$E" in *" $c "*) collide="$c"; break;; esac; done
  if [ -n "$collide" ]; then
    record FAIL "variant ${tag}: shared claim held (no co-location)" "shared Pod allocated card ${collide} (NVIDIA_VISIBLE_DEVICES=${nvd:-?}) already held EXCLUSIVELY — mutual exclusion violated (${path})"
  else
    record FAIL "variant ${tag}: shared claim held (no co-location)" "shared Pod allocated card(s) ${scards} not in the exclusive set — unexpected; every card should be exclusive"
  fi
}

# Variant A first (Kueue path), tear it down and let cards free, then variant B (raw path) on a clean node.
variant A "$LQ"
echo
echo "[case-22] tearing down variant A before variant B (freeing cards)"
for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
TESTPODS=()
for _ in $(seq 1 20); do
  rem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
  [ -n "$CAP" ] && [ "${rem:-0}" = "$CAP" ] && break
  sleep 3
done
variant B ""

echo
echo "== CASE 22 — Exclusive and shared claims never co-locate on one physical card =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). An exclusive whole-card claim and a shared claim must never share one"
  echo "physical card. If variant B (raw, no Kueue label) co-locates while variant A (Kueue) holds, the gap"
  echo "is the on-node device plugin: exclusive and shared are advertised as independent device-ID pools"
  echo "aliasing the same cards, and Allocate stamps its mode without rejecting a card already in another mode."
  echo "Diagnose: kubectl -n default get pods -o wide; kubectl -n default get pod <shared> -o jsonpath='{.metadata.annotations.${ANNO}}';"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-nvidia --tail=200 | grep -i 'conflicting allocation mode'"
  exit 1
fi
echo "CASE 22 PASS"
