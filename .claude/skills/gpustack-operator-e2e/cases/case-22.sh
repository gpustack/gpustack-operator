#!/usr/bin/env bash
#
# CASE 22 — Cross-mode claims never co-locate on one physical card
#   (MUTATING, self-recovering; AUTO-SKIPS without a real accelerator advertising both <base> and <base>.shared)
#
#   case-22.sh <NS>
#
# Goal:        Whole-card exclusive, shared, and sliced claims are mutually exclusive on the SAME physical
#              card. The device plugin advertises each mode as an independent resource name backed by an
#              independent device-ID pool aliasing the same cards, and kubelet picks tokens freely
#              (GetPreferredAllocation is advisory and never runs under the default TopologyManager policy
#              "none"), so the invariant is enforced at two levels, both exercised here:
#                1. ListAndWatch reports a card held in ANOTHER mode as Unhealthy (its tokens stay advertised
#                   but can never be assigned to a new pod) — so a claim steers onto a FREE card, and a claim
#                   with no free card is held at the scheduler (Unschedulable);
#                2. the Allocate gate rejects a same-card cross-mode assignment with FailedPrecondition
#                   (UnexpectedAdmissionError) as the backstop for the residual race.
#              Two cross-mode directions are proven on BOTH request paths where applicable:
#                - variant A/B (exclusive→shared): every card held exclusive, a shared claim is HELD —
#                  variant A via the Kueue LocalQueue label, variant B raw (no label; webhook and Kueue
#                  AdmissionCheck bypassed, only the on-node device plugin decides);
#                - variant C (exclusive→sliced): with one card left free a sliced claim must land EXACTLY on
#                  the free card and reach Running (the production regression: it used to die with
#                  UnexpectedAdmissionError on a held card), and with every card held it must be HELD;
#                - variant D (sliced→exclusive): one card held sliced, exclusive claims must avoid that card,
#                  and with no card left free an exclusive claim must be HELD.
#              Variants C/D run on the raw path: the card-pick mechanism they prove lives in kubelet + the
#              device plugin and is identical with or without a queue label.
# Environment: A node advertising a real accelerator with BOTH the exclusive whole-card resource (<base> > 0)
#              AND the shared companion (<base>.shared > 0); variants C/D additionally need <base>.sliced > 0
#              (the sliced section AUTO-SKIPS independently without it). Whole-case AUTO-SKIPS (exit 0)
#              without the exclusive+shared pair — the co-location can only be observed on hardware the
#              device plugin allocates.
# Inputs:      All real, nothing mocked — N = the node's allocatable whole cards. Per variant: exclusive Pods
#              (<base>:1) created SEQUENTIALLY (one Running on a distinct card before the next), then the
#              cross-mode claim. Sliced Pods carry <base>.sliced:1 plus <base>.sliced.memory-percentage:20
#              (the plugin refuses a slice with no memory budget). Variant A tags Pods with the accelerated
#              pool's entrance LocalQueue; B/C/D tag none.
#              Ground truth is each Pod's device.gpustack.ai/accelerator.allocated annotation (the physical
#              card <group>:<device> the plugin placed it on + the mode it stamped). Images: IMAGE for
#              exclusive/shared Pods (default ubuntu:24.04), SLICED_IMAGE for sliced Pods — override it
#              with a vendor runtime image when the slicing preload needs in-image libraries (Ascend).
# Expected:    A/B — with every card exclusive, the shared Pod is HELD (never allocated a card; concrete
#              signal Unschedulable, or Failed/UnexpectedAdmissionError from the Allocate backstop).
#              C — the sliced Pod runs on the one free card when there is one (a Failed pod or a card in the
#              exclusive set FAILS the case — that is the fixed regression); then, with the sliced card
#              released and every card exclusive, the next sliced Pod is HELD. D — every exclusive Pod
#              avoids the sliced-held card (a Failed pod or attribution onto it FAILS the case), and the
#              last exclusive claim is HELD once no card is free.
# Cleanup:     Trap deletes every test Pod (all variants) and waits for the accelerated pool's whole-card
#              capacity to free again. Idempotent; runs on pass AND fail.
set -uo pipefail

NS="${1:?usage: case-22.sh <NS>}"
PODPFX=gpustack-e2e-exshare
ANNO=device.gpustack.ai/accelerator.allocated
IMAGE="${IMAGE:-ubuntu:24.04}"
# Image for sliced Pods. A soft-slicing runtime whose preload needs vendor libraries inside the image
# (e.g. Ascend vcann-rt needs the CANN runtime — a bare distro exits 127 at container start, AFTER the
# allocation succeeds) must override this with a vendor runtime image, e.g.
# SLICED_IMAGE=quay.io/ascend/cann:9.0.1-910b-ubuntu22.04-py3.11-devel. Image-agnostic preloads
# (NVIDIA libvgpu) can leave the default.
SLICED_IMAGE="${SLICED_IMAGE:-$IMAGE}"

# --- Skip gate: a node advertising BOTH <base> (exclusive whole card) and <base>.shared. The base name is
# vendor-specific (nvidia.com/gpu, huawei.com/npu, ...), so match any bare "<vendor>/<card>" resource whose
# name has no dots (ruling out the .sliced/.visibility families) and that has a .shared companion. ---
read -r GPU_NODE EXCL SHARED N <<<"$(kubectl get nodes -o json 2>/dev/null | python3 -c "
import json,sys,re
for n in json.load(sys.stdin).get('items',[]):
    a=n.get('status',{}).get('allocatable',{})
    for k,v in a.items():
        if not re.match(r'^[^/]+/[^.]+$', k): continue  # the bare exclusive whole-card resource
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
  echo "No node advertises both an exclusive whole-card resource (<base>) and its shared companion"
  echo "(<base>.shared). This case needs a real accelerator the device plugin allocates, so exclusive"
  echo "vs shared co-location on one physical card can be observed. Run it on such a GPU cluster."
  exit 0
fi
echo "[case-22] GPU node ${GPU_NODE}: ${N}× whole card (${EXCL}), shared companion ${SHARED}"

# The node manufacturer's same-named RuntimeClass (ascend/nvidia/...) — real accelerator Pods run under
# it, and on some vendors the slicing injection only resolves under it (Ascend's ld.so.preload points at
# driver paths the runtime class mounts; without it the container exits 127 AFTER a successful
# allocation). Auto-detect from the Devices group; override with RUNTIME_CLASS=... or disable with
# RUNTIME_CLASS="" via env when the cluster has no such class.
MANUF=$(kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if g.get('accelerators'):
        print(g.get('manufacturer','')); break
" 2>/dev/null)
RUNTIME_CLASS="${RUNTIME_CLASS-__auto__}"
if [ "$RUNTIME_CLASS" = "__auto__" ]; then
  RUNTIME_CLASS=""
  if [ -n "${MANUF:-}" ] && kubectl get runtimeclass "$MANUF" >/dev/null 2>&1; then
    RUNTIME_CLASS="$MANUF"
  fi
fi
[ -n "$RUNTIME_CLASS" ] && echo "[case-22] manufacturer ${MANUF:-?}, running Pods under RuntimeClass ${RUNTIME_CLASS}"

# The sliced companion enables variants C/D (exclusive↔sliced); that section skips independently.
SLICED="${EXCL}.sliced"
NSLICED=$(kubectl get node "$GPU_NODE" -o jsonpath="{.status.allocatable.${SLICED//./\\.}}" 2>/dev/null)
if [ -n "${NSLICED:-}" ] && [ "${NSLICED}" != "0" ]; then
  echo "[case-22] sliced companion ${SLICED} advertised (${NSLICED} tokens) — variants C/D enabled"
else
  SLICED=""
  echo "[case-22] no sliced companion advertised — variants C/D will skip"
fi

# The accelerated pool that actually backs THIS node's cards. Match the InstanceType whose name carries
# the node's accelerator group id (robust against leftover mock/sibling pools that another case may
# have left behind with zero capacity), requiring an entrance LocalQueue AND real whole-card capacity. Fall
# back to the highest-capacity acceleratable pool with an entrance queue. Capacity feeds the ledger
# freshness / cards-freed waits.
GROUPID=$(kubectl get devices "$GPU_NODE" -o json 2>/dev/null | python3 -c "
import json,sys
for g in json.load(sys.stdin).get('spec',{}).get('groups',[]):
    if g.get('accelerators'):
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

# mkpod <name> <resource> <lq-or-empty> [extra-limits] [image] — a Pod requesting 1 unit of <resource>,
# pinned to the GPU node, tagged with the LocalQueue label only when <lq> is non-empty (variant A vs the
# raw variants). [extra-limits] adds request/limit entries (e.g. the sliced memory budget); [image]
# defaults to $IMAGE (sliced Pods pass $SLICED_IMAGE).
mkpod() {
  local name="$1" res="$2" lq="$3" extra="${4:-}" image="${5:-$IMAGE}" labels="" reslines="" rcline=""
  [ -n "$lq" ] && labels="{ kueue.x-k8s.io/queue-name: ${lq} }" || labels="{}"
  reslines="${res}: \"1\""
  [ -n "$extra" ] && reslines="${reslines}, ${extra}"
  [ -n "$RUNTIME_CLASS" ] && rcline="runtimeClassName: ${RUNTIME_CLASS}"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata: { name: ${name}, namespace: default, labels: ${labels} }
spec:
  ${rcline}
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${GPU_NODE} }
  containers:
    - name: main
      image: ${image}
      command: ["sleep", "86400"]
      resources:
        limits:   { ${reslines} }
        requests: { ${reslines} }
EOF
}

phase() { kubectl -n default get pod "$1" -o jsonpath='{.status.phase}' 2>/dev/null; }
running() { [ "$(phase "$1")" = "Running" ]; }
wait_running() { for _ in $(seq 1 40); do running "$1" && return 0; sleep 3; done; return 1; }
# wait_settled <pod> — wait until the Pod reaches a terminal-for-this-case state: Running (allocated and
# started) or Failed (admission refused). Prints the phase.
wait_settled() { for _ in $(seq 1 40); do local ph; ph="$(phase "$1")"; case "$ph" in Running|Failed) echo "$ph"; return 0;; esac; sleep 3; done; phase "$1"; return 1; }

# pod_cards <pod> — the sorted "<group>:<device>" physical card(s) the device plugin allocated to <pod>,
# read from its allocation annotation (present once Allocate ran, even before the container starts). Empty
# when the Pod was never allocated a card (held / still pending). Device IDs may contain spaces (e.g.
# Ascend card serials), so each card token is emitted with spaces folded to '~' — the tokens are compared
# by shell word-splitting everywhere, and a literal space would shatter one card ID into false extra
# "cards" whose shared suffixes collide across cards.
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
# The annotation is keyed by container name; each entry carries that container's own allocation.
for c in st.values():
    for g in (c.get('devices') or {}).get('groups',[]):
        gid=g.get('id','')
        for a in g.get('accelerators',[]):
            cards.add(('%s:%s' % (gid, a.get('id',''))).replace(' ','~'))
print(' '.join(sorted(cards)))
"
}

# held_reason <pod> — a CONCRETE signal that a claim was held for a legitimate reason, not merely slow to
# schedule: an Unschedulable/FailedScheduling verdict (the withheld-token path — an opposite-mode-held card
# advertises Unhealthy, so no free card exists to assign), a device-plugin admission refusal (phase
# Failed / UnexpectedAdmissionError — the Allocate-gate backstop), or a Kueue scheduling gate. Empty when no
# such signal is present — in which case "not allocated yet" is inconclusive (a Pod merely mid-scheduling
# could still be about to co-locate), so the held branch must not score it a silent PASS.
held_reason() {
  local p="$1" ph reason gates cond ev
  ph="$(phase "$p")"
  reason="$(kubectl -n default get pod "$p" -o jsonpath='{.status.reason}' 2>/dev/null)"
  [ "$ph" = Failed ] && { echo "Failed/${reason:-admission-refused}"; return; }
  gates="$(kubectl -n default get pod "$p" -o jsonpath='{.spec.schedulingGates[*].name}' 2>/dev/null)"
  [ -n "$gates" ] && { echo "scheduling-gated(${gates})"; return; }
  cond="$(kubectl -n default get pod "$p" -o jsonpath='{.status.conditions[?(@.type=="PodScheduled")].reason}' 2>/dev/null)"
  [ "$cond" = Unschedulable ] && { echo "Unschedulable"; return; }
  ev="$(kubectl -n default get events --field-selector involvedObject.name="$p" -o jsonpath='{range .items[*]}{.reason} {end}' 2>/dev/null | tr ' ' '\n' | grep -iE 'UnexpectedAdmissionError|FailedScheduling' | head -1)"
  [ -n "$ev" ] && { echo "$ev"; return; }
  echo ""
}

# assert_held <tag> <check> <pod> <path> — the claim must be HELD: never allocated a card, with a concrete
# held signal. Records one PASS/FAIL row.
assert_held() {
  local tag="$1" check="$2" sp="$3" path="$4"
  local scards="" ph=""
  for _ in $(seq 1 20); do
    scards="$(pod_cards "$sp")"
    [ -n "$scards" ] && break
    sleep 3
  done
  ph="$(phase "$sp")"
  if [ -z "$scards" ]; then
    # Not allocated within the window. Require a CONCRETE held signal before scoring PASS — a bare
    # "no allocation yet" could hide a Pod that is merely slow and about to co-locate.
    local hr; hr="$(held_reason "$sp")"
    if [ -z "$hr" ]; then for _ in 1 2 3 4 5; do sleep 3; hr="$(held_reason "$sp")"; [ -n "$hr" ] && break; done; fi
    if [ -n "$hr" ]; then
      record PASS "$tag: $check" "claim not allocated any card, held for a real reason [${hr}] (phase=${ph:-<none>})"
    else
      record FAIL "$tag: $check" "claim not allocated but shows NO concrete held signal (Failed/gated/Unschedulable) — cannot confirm held vs merely slow (phase=${ph:-<none>}); re-check ${path}"
    fi
    return
  fi
  record FAIL "$tag: $check" "claim allocated card(s) ${scards} although no free card exists — cross-mode co-location possible (${path})"
}

# fill_exclusive <tag> <check> <count> <lq> <forbidden> — create <count> exclusive Pods SEQUENTIALLY (one
# Running with its allocation annotation on a DISTINCT card before the next), requiring none of them lands
# on a card in <forbidden> (a leading/trailing-space-padded union like " g:d1 g:d2 "). Filling one at a
# time keeps at most one identical GPU Pod pending, so the occupancy precondition is established
# deterministically without depending on concurrent admission (the concurrent-admission pod-identification
# path is covered directly by the device-plugin unit tests, not here). The union of the cards the
# exclusive Pods hold is returned in the FILL_CARDS global — NOT via stdout, because a $(...) capture
# would run the function in a subshell and silently drop its TESTPODS/record mutations (including every
# FAIL it records). On failure records FAIL and returns 1.
FILL_CARDS=""
fill_exclusive() {
  local tag="$1" check="$2" count="$3" lq="$4" forbidden="$5"
  local tagl="${tag,,}" i p c cc seen=" "
  FILL_CARDS=""
  for i in $(seq 1 "$count"); do
    p="${PODPFX}-${tagl}-excl-${i}"; TESTPODS+=("$p")
    mkpod "$p" "$EXCL" "$lq"
    if [ "$(wait_settled "$p")" != "Running" ]; then
      record FAIL "$tag: $check" "exclusive Pod ${p} did not reach Running (phase=$(phase "$p")) — cannot set up the occupancy precondition"
      return 1
    fi
    c=""
    for _ in $(seq 1 20); do c="$(pod_cards "$p")"; [ -n "$c" ] && break; sleep 3; done
    if [ -z "$c" ]; then
      record FAIL "$tag: $check" "exclusive Pod ${p} is Running but recorded no allocation annotation — cannot confirm it holds a card"
      return 1
    fi
    for cc in $c; do
      case "$seen" in
        *" $cc "*)
          record FAIL "$tag: $check" "exclusive Pod ${p} was attributed card ${cc} already held by an earlier Pod — per-card accounting collided"
          return 1;;
      esac
      case "$forbidden" in
        *" $cc "*)
          record FAIL "$tag: $check" "exclusive Pod ${p} was attributed card ${cc} that is off-limits (held by an earlier batch or in another mode) — kubelet assigned a card ListAndWatch must withhold"
          return 1;;
      esac
      seen="${seen}${cc} "
    done
  done
  FILL_CARDS="$seen"
  return 0
}

# wait_ledger <want> — wait until the accelerated pool's ledger reports <want> whole cards remaining.
wait_ledger() {
  local want="$1" rem=""
  for _ in $(seq 1 20); do
    rem=$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.accelerator.remaining}' 2>/dev/null)
    [ "${rem:-}" = "$want" ] && { echo "$rem"; return 0; }
    sleep 3
  done
  echo "${rem:-?}"; return 1
}

# --- variant A/B: exclusive→shared, every card held exclusive, the shared claim is HELD. ---
variant_shared() {
  local tag="$1" lq="$2"
  local path; [ -n "$lq" ] && path="via Kueue LocalQueue ${lq}" || path="raw (no Kueue queue label)"
  echo
  echo "[case-22] === variant ${tag}: exclusive→shared, ${path} ==="

  fill_exclusive "$tag" "all cards occupied exclusively" "$N" "$lq" " " || return
  local E="$FILL_CARDS"
  wait_ledger "0" >/dev/null || true
  echo "[case-22]   ${N} exclusive Pods occupy card(s):${E}"
  record PASS "variant ${tag}: all cards occupied exclusively" "${N} exclusive Pods Running on distinct cards"

  local sp="${PODPFX}-${tag,,}-shared"; TESTPODS+=("$sp")
  mkpod "$sp" "$SHARED" "$lq"
  assert_held "variant ${tag}" "shared claim held (no co-location)" "$sp" "$path"
}

# --- variant C: exclusive→sliced (raw) — a sliced claim lands EXACTLY on the free card, and is HELD once
#     every card is exclusive. The free-card placement is the production regression: the sliced Pod used to
#     die with UnexpectedAdmissionError on a held card. ---
variant_sliced_after_exclusive() {
  local tag="C"
  echo
  echo "[case-22] === variant ${tag}: exclusive→sliced, raw ==="

  # 1. Occupy N-1 cards exclusively, leaving exactly one free card.
  fill_exclusive "$tag" "N-1 cards occupied exclusively" "$((N - 1))" "" " " || return
  local E="$FILL_CARDS"
  echo "[case-22]   $((N - 1)) exclusive Pods occupy card(s):${E}"
  record PASS "variant ${tag}: N-1 cards occupied exclusively" "$((N - 1)) exclusive Pods Running on distinct cards, one card free"

  # 2. THE regression assertion: the sliced claim must reach Running on the one free card — never die on a
  #    held card (Unhealthy-withheld tokens make the held cards unassignable), never co-locate.
  local sp="${PODPFX}-c-sliced"; TESTPODS+=("$sp")
  mkpod "$sp" "$SLICED" "" "${SLICED}.memory-percentage: \"20\"" "$SLICED_IMAGE"
  local outcome; outcome="$(wait_settled "$sp")"
  local scards=""
  for _ in $(seq 1 10); do scards="$(pod_cards "$sp")"; [ -n "$scards" ] && break; sleep 3; done
  if [ "$outcome" != "Running" ]; then
    record FAIL "variant ${tag}: sliced claim placed on the free card" "sliced Pod ${sp} settled to ${outcome:-<none>} instead of Running although a free card exists — kubelet was offered a held card (phase reason: $(kubectl -n default get pod "$sp" -o jsonpath='{.status.reason}' 2>/dev/null))"
  else
    local bad=""
    for c in $scards; do case "$E" in *" $c "*) bad="$c"; break;; esac; done
    if [ -n "$bad" ]; then
      record FAIL "variant ${tag}: sliced claim placed on the free card" "sliced Pod Running on card ${bad} held EXCLUSIVELY — cross-mode co-location"
    else
      record PASS "variant ${tag}: sliced claim placed on the free card" "sliced Pod Running on card(s) ${scards} (outside the exclusive set)"
    fi
  fi

  # 3. Free the sliced card again, then top up to all-exclusive with one more exclusive Pod (tag C2 for
  #    unique names, forbidden from the N-1 cards). With every card exclusive, the next sliced claim must
  #    be HELD. (Order matters: a sliced claim may LEGITIMATELY stack onto a sliced-held card — same-mode
  #    co-allocation — so "held" can only be asserted against a fully-exclusive node.)
  kubectl -n default delete pod "$sp" --wait=false >/dev/null 2>&1 || true
  wait_ledger "1" >/dev/null || true
  fill_exclusive "C2" "all cards occupied exclusively" 1 "" "$E" || return
  wait_ledger "0" >/dev/null || true
  local sp2="${PODPFX}-c-sliced-full"; TESTPODS+=("$sp2")
  mkpod "$sp2" "$SLICED" "" "${SLICED}.memory-percentage: \"20\"" "$SLICED_IMAGE"
  assert_held "variant ${tag}" "sliced claim held when no card is free" "$sp2" "raw (no Kueue queue label)"
}

# --- variant D: sliced→exclusive (raw) — exclusive claims avoid the sliced-held card, and the last one is
#     HELD once no card is free. ---
variant_exclusive_after_sliced() {
  local tag="D"
  echo
  echo "[case-22] === variant ${tag}: sliced→exclusive, raw ==="

  # 1. Hold one card with a sliced claim.
  local sp="${PODPFX}-d-sliced"; TESTPODS+=("$sp")
  mkpod "$sp" "$SLICED" "" "${SLICED}.memory-percentage: \"20\"" "$SLICED_IMAGE"
  if [ "$(wait_settled "$sp")" != "Running" ]; then
    record FAIL "variant ${tag}: one card held sliced" "sliced Pod ${sp} did not reach Running — cannot set up the occupancy precondition"
    return
  fi
  local scards=""
  for _ in $(seq 1 20); do scards="$(pod_cards "$sp")"; [ -n "$scards" ] && break; sleep 3; done
  if [ -z "$scards" ]; then
    record FAIL "variant ${tag}: one card held sliced" "sliced Pod is Running but recorded no allocation annotation"
    return
  fi
  local forbidden=" "
  for c in $scards; do forbidden="${forbidden}${c} "; done
  echo "[case-22]   sliced Pod holds card(s):${forbidden}"
  record PASS "variant ${tag}: one card held sliced" "sliced Pod Running on card(s) ${scards}"

  # 2. N-1 exclusive claims must each land on a DISTINCT card and never on the sliced-held one (the
  #    exclusive server withholds that card's token as Unhealthy).
  fill_exclusive "$tag" "exclusive claims avoid the sliced-held card" "$((N - 1))" "" "$forbidden" || return
  local E="$FILL_CARDS"
  echo "[case-22]   $((N - 1)) exclusive Pods occupy card(s):${E}(sliced card avoided)"
  record PASS "variant ${tag}: exclusive claims avoid the sliced-held card" "$((N - 1)) exclusive Pods Running on distinct cards, none on the sliced card"

  # 3. Every card is now held (N-1 exclusive + 1 sliced): the next exclusive claim must be HELD.
  wait_ledger "0" >/dev/null || true
  local ep="${PODPFX}-d-excl-full"; TESTPODS+=("$ep")
  mkpod "$ep" "$EXCL" ""
  assert_held "variant ${tag}" "exclusive claim held when no card is free" "$ep" "raw (no Kueue queue label)"
}

# Variant A first (Kueue path), tear it down and let cards free, then variant B (raw) on a clean node.
variant_shared A "$LQ"
echo
echo "[case-22] tearing down variant A before variant B (freeing cards)"
for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
TESTPODS=()
wait_ledger "$CAP" >/dev/null || true
variant_shared B ""

# Variants C/D (exclusive↔sliced) — each on a clean node, only when the sliced companion is advertised.
if [ -n "$SLICED" ]; then
  echo
  echo "[case-22] tearing down variant B before variant C (freeing cards)"
  for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
  TESTPODS=()
  wait_ledger "$CAP" >/dev/null || true
  variant_sliced_after_exclusive

  echo
  echo "[case-22] tearing down variant C before variant D (freeing cards)"
  for p in "${TESTPODS[@]}"; do kubectl -n default delete pod "$p" --ignore-not-found --force --grace-period=0 2>/dev/null || true; done
  TESTPODS=()
  wait_ledger "$CAP" >/dev/null || true
  variant_exclusive_after_sliced
fi

echo
echo "== CASE 22 — Cross-mode claims never co-locate on one physical card =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Whole-card exclusive, shared, and sliced claims must never share one"
  echo "physical card: ListAndWatch must report an opposite-mode-held card's tokens Unhealthy (so kubelet"
  echo "can only pick a FREE card, and a claim with no free card is Unschedulable), and the Allocate gate"
  echo "must reject a residual cross-mode assignment with FailedPrecondition. A claim dying with"
  echo "UnexpectedAdmissionError while a FREE card exists is the regression this case guards."
  echo "Diagnose: kubectl -n default get pods -o wide; kubectl -n default get pod <claim> -o jsonpath='{.metadata.annotations.${ANNO}}';"
  echo "kubectl -n ${NS} logs ds/gpustack-operator-device-manager-* --tail=200 | grep -i 'cross-mode allocation rejected'"
  exit 1
fi
echo "CASE 22 PASS"
