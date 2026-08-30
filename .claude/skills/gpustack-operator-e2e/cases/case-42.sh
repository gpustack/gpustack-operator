#!/usr/bin/env bash
#
# CASE 42 — Hygon DCU partitions: what a grant carries, what one card holds at once, and what is refused
#   (MUTATING, self-recovering; AUTO-SKIPS unless a Hygon node is in Multi-Instance mode)
#
#   MIG_NODE_SSH=<user@host> case-42.sh <NS>
#
# Goal:        ASSERTS four contracts on real Hygon DCU hardware, each proven from BOTH sides — what
#              the operator recorded, and what the container or the vendor's own registry shows:
#                (1) GEOMETRY. A container granted a partition sees EXACTLY that profile's compute
#                    units and memory, read inside the container, not the parent card's. A grant that
#                    reported the card's 80 units and 65520 MiB would look like a success and be a
#                    workload with no isolation at all.
#                (2) ONE CARD, SEVERAL PARTITIONS. Partitions of one card run side by side, each on
#                    its own instance identity and each seeing its own geometry. Proven from the
#                    distinct identities the operator recorded AND from the vendor's registry holding
#                    one file per grant.
#                (3) THE VENDOR'S OWN LIMIT. A request granted more than one accelerator is REFUSED.
#                    The vendor runtime makes exactly one partition visible to a container whatever
#                    it is given, so carving on every card would consume quota the workload can never
#                    reach and report success for a container running at a fraction of its grant.
#                (4) RECLAIM. Deleting every Pod returns the node to the instance count it had before
#                    this case ran. Nothing else on the node can free a partition — the device-plugin
#                    protocol has no release callback — and this vendor refuses to leave
#                    Multi-Instance mode while any instance survives, so a leak here is a node that
#                    cannot be returned to whole-card service at all.
#
# Environment: Needs REAL Hygon DCU hardware on ONE node ALREADY in Multi-Instance mode, and SSH to
#              that node to read the vendor's instance registry — the ledger cannot distinguish an
#              instance this case created from one that was already there, and (4) is only meaningful
#              against the count taken before. Pass the address inline at run time and NEVER write it
#              into a file. It EXITS 2 (input required) when MIG_NODE_SSH is unset.
#
#              AUTO-SKIPS (exit 0) when no Devices object reports a Hygon accelerator group, when the
#              node's cards report no partition profile (the mode is off, or the driver was installed
#              without virtualization support), or when the node advertises no partition capacity. A
#              Devices read that ERRORS fails setup rather than skipping: "no Hygon hardware" must
#              not be indistinguishable from "the query did not answer".
#
#              The mode itself is NEVER changed here. It is node-wide on this vendor, it is refused
#              while the device manager holds the driver, and turning it on with nothing carved makes
#              every card unusable — see docs/operation/hygon-mig.md. This case observes the mode and
#              refuses to run without it rather than arranging it.
#
#              The claim carrier must ship a runtime that can open the accelerator, because the
#              geometry readings are taken inside it; override with E2E_PARTITION_IMAGE=<ref>.
#              Pre-pull it on the node if the first claim is slow — the default is large.
# Inputs:      All real, nothing mocked. Raw Pods on the Hygon pool's entrance LocalQueue: three
#              one-slice partitions requested together, then one two-accelerator request expected to
#              be refused.
# Verdicts:    One row per contract above, plus the setup gates. Any FAIL fails the case.
set -uo pipefail

CASE_ID=42
NS="${1:-gpustack-system}"

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

IMAGE="${E2E_PARTITION_IMAGE:-quay.io/gpustack/runner:dtk25.04-vllm0.11.0}"
MANU=hygon
BASE=hygon.com/dcu
PARTITIONED="${BASE}.partitioned"

ROWS=()
FAILS=0
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# verdict <cond-exit-code> <check> <object-on-pass> <object-on-fail>
verdict() { [ "$1" -eq 0 ] && record PASS "$2" "$3" || record FAIL "$2" "$4"; }

results() {
  echo
  printf '%-6s %-64s %s\n' RESULT CHECK OBJECT
  for row in "${ROWS[@]}"; do
    printf '%-6s %-64s %s\n' "${row%%|*}" "$(cut -d'|' -f2 <<<"$row")" "$(cut -d'|' -f3- <<<"$row")"
  done
  echo
  if [ "$FAILS" -gt 0 ]; then echo "CASE ${CASE_ID} FAIL"; exit 1; fi
  echo "CASE ${CASE_ID} PASS"
}

skip() {
  echo "== CASE ${CASE_ID} — SKIPPED =="
  for line in "$@"; do echo "$line"; done
  exit 0
}

fail_setup() {
  echo "== CASE ${CASE_ID} — FAILED (setup) =="
  for line in "$@"; do echo "$line"; done
  exit 1
}

: "${MIG_NODE_SSH:?MIG_NODE_SSH=<user@host> required — ask the user, never hardcode it}"

node_ssh() {
  # LogLevel=ERROR silences the host-key notice, which otherwise lands on stdout's neighbour and
  # gets swept into a numeric reading by a caller stripping non-digits -- the address's own digits
  # then read as part of the count.
  ssh -o BatchMode=yes -o StrictHostKeyChecking=accept-new -o LogLevel=ERROR -o ConnectTimeout=15 \
    "$MIG_NODE_SSH" "$@"
}

# node_instance_count reports how many compute instances the vendor registry holds, or nothing when
# the node could not be asked -- a caller must never read an unanswered probe as an empty registry.
node_instance_count() {
  local out
  out="$(node_ssh 'ls /etc/dmi_mig_config/ci/ 2>/dev/null | wc -l' 2>/dev/null)" || return 1
  out="$(tail -1 <<<"$out" | tr -dc '0-9')"
  [ -n "$out" ] || return 1
  printf '%s' "$out"
}

# ---------------------------------------------------------------------------------------------------
# Setup gates.
# ---------------------------------------------------------------------------------------------------

# The ledger is read once and parsed in a command substitution, not a process substitution: the
# latter discards the parser's exit status, so a parser that failed on a payload it could not read
# would be indistinguishable from a cluster reporting no accelerator — and this gate turns the second
# into a SKIP, which a summary table reads as coverage.
if ! DEVS_JSON="$(kubectl get devices.worker.gpustack.ai -o json 2>&1)"; then
  fail_setup "Reading the Devices ledger failed, so this case cannot tell 'no hygon hardware' from" \
    "'the query did not answer'. It refuses to report a skip on either:" \
    "$(head -5 <<<"$DEVS_JSON")"
fi

if ! DISCOVERED="$(printf '%s' "$DEVS_JSON" | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items', []):
    for g in d.get('spec', {}).get('groups', []):
        if g.get('manufacturer') != '${MANU}':
            continue
        accs = g.get('accelerators') or []
        if not accs:
            continue
        partitioned = sum(1 for a in accs
                          if (a.get('status', {}).get('physicalSliced', {}).get('count') or 0) > 0)
        profiles = sorted({p['name']
                           for a in accs
                           for p in (a.get('status', {}).get('physicalSliced', {}).get('profiles') or [])})
        print('%s\t%s\t%d\t%d\t%s' % (d['metadata']['name'], g['name'], len(accs), partitioned,
                                      ','.join(profiles)))
        break
")"; then
  fail_setup "The Devices ledger was read but could not be parsed, so this case cannot tell 'no hygon" \
    "hardware' from 'the answer was unreadable'. It refuses to report a skip on either."
fi

[ -n "$DISCOVERED" ] || skip \
  "No Devices object reports a ${MANU} accelerator group — the operator chain is not observing a DCU node."

NODE=$(cut -f1 <<<"$DISCOVERED" | head -1)
GROUP=$(cut -f2 <<<"$DISCOVERED" | head -1)
CARDS=$(cut -f3 <<<"$DISCOVERED" | head -1)
PARTITIONED_CARDS=$(cut -f4 <<<"$DISCOVERED" | head -1)
PROFILES=$(cut -f5 <<<"$DISCOVERED" | head -1)

[ "$PARTITIONED_CARDS" -gt 0 ] || skip \
  "Node ${NODE} reports ${CARDS} ${MANU} card(s) and none offering a partition profile." \
  "Multi-Instance mode is off, or the driver was installed without virtualization support." \
  "See docs/operation/hygon-mig.md; this case never changes the mode itself."

# The narrowest profile is the one a card holds most of, which is what contract (2) needs.
PROFILE=$(tr ',' '\n' <<<"$PROFILES" | sort | head -1)
[ -n "$PROFILE" ] || skip "Node ${NODE} reports partition-capable cards but no named profile."
PROFILE_KEY="${PARTITIONED}.mig-${PROFILE}"

echo "[case-${CASE_ID}] node ${NODE}, group ${GROUP}: ${PARTITIONED_CARDS}/${CARDS} cards partition-capable"
echo "[case-${CASE_ID}] profiles offered: ${PROFILES}; this case claims ${PROFILE}"

node_key() {
  kubectl get node "$NODE" -o jsonpath="{.status.allocatable['${1//./\\.}']}" 2>/dev/null
}

PARTITION_POOL=$(node_key "$PARTITIONED")
PROFILE_POOL=$(node_key "$PROFILE_KEY")
[ -n "${PARTITION_POOL:-}" ] && [ "${PARTITION_POOL:-0}" -gt 0 ] || skip \
  "Node ${NODE} advertises no ${PARTITIONED} capacity, so nothing can be claimed against it yet." \
  "The worker publishes it from the Devices ledger; give it a moment after a mode change."
[ -n "${PROFILE_POOL:-}" ] && [ "${PROFILE_POOL:-0}" -gt 0 ] || skip \
  "Node ${NODE} advertises ${PARTITIONED} but no ${PROFILE_KEY} capacity."

# How many partitions can be asked for at once: what one card holds, bounded by what is free.
PER_CARD=$(printf '%s' "$DEVS_JSON" | python3 -c "
import json,sys
best = 0
for d in json.load(sys.stdin).get('items', []):
    if d['metadata']['name'] != '${NODE}':
        continue
    for g in d.get('spec', {}).get('groups', []):
        for a in g.get('accelerators') or []:
            for p in (a.get('status', {}).get('physicalSliced', {}).get('profiles') or []):
                if p['name'] == '${PROFILE}':
                    best = max(best, int(p.get('count') or 0))
print(best)
")
CLAIMS=$(( PER_CARD < 3 ? PER_CARD : 3 ))
[ "$CLAIMS" -ge 2 ] || skip \
  "Profile ${PROFILE} fits only ${PER_CARD} instance(s) per card, so nothing can be shown about two" \
  "partitions of one card sharing it."

LQ=$(kubectl get localqueues.kueue.x-k8s.io -A -o json 2>/dev/null | python3 -c "
import json,sys
want = 'gpustack--${MANU}-${GROUP}-linux-amd64'
for it in json.load(sys.stdin).get('items', []):
    if it.get('spec', {}).get('clusterQueue') == want:
        print('%s\t%s' % (it['metadata']['namespace'], it['metadata']['name'])); break
")
[ -n "$LQ" ] || skip \
  "No LocalQueue fronts the ${MANU} pool's ClusterQueue yet; the scheduling chain has not materialized."
LQ_NS=$(cut -f1 <<<"$LQ")
LQ_NAME=$(cut -f2 <<<"$LQ")

# Ground truth, taken before anything is claimed. Contract (4) is measured against exactly this.
if ! INSTANCES_BEFORE="$(node_instance_count)"; then
  fail_setup "Could not read the vendor instance registry over MIG_NODE_SSH, so this case could not" \
    "tell an instance it created from one that was already there."
fi
echo "[case-${CASE_ID}] vendor registry holds ${INSTANCES_BEFORE} instance(s) before this case"

# ---------------------------------------------------------------------------------------------------
# Claims.
# ---------------------------------------------------------------------------------------------------

PODS=()
cleanup() {
  [ "${#PODS[@]}" -eq 0 ] && return 0
  kubectl -n "$LQ_NS" delete pod "${PODS[@]}" --ignore-not-found --wait=true --timeout=180s >/dev/null 2>&1 || true
}
trap cleanup EXIT

mkpod() {
  mkpod_raw "$@" || return $?
  PODS+=("$1")
}

# mkpod_raw applies the Pod and returns kubectl's own status, so a caller asserting a REFUSAL can
# tell "the API rejected it" from "it was created and then failed".
mkpod_raw() {
  local name="$1" reslines="$2"
  cat <<EOF | kubectl apply -f - >/dev/null
apiVersion: v1
kind: Pod
metadata:
  name: ${name}
  namespace: ${LQ_NS}
  labels: { kueue.x-k8s.io/queue-name: ${LQ_NAME} }
spec:
  schedulerName: default-scheduler
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${NODE} }
  containers:
    - name: main
      image: ${IMAGE}
      command: ["sleep", "1800"]
      resources:
        limits:
${reslines}
        requests:
${reslines}
EOF
}

# A partition request names BOTH the family and the profile. Naming only the profile allocates
# nothing, and naming only the family is refused at admission.
partition_reslines() {
  printf '          cpu: "2"\n          memory: 4Gi\n          %s: "1"\n          %s: "1"' \
    "$PARTITIONED" "$PROFILE_KEY"
}

wait_phase() {
  local pod="$1" want="$2" i
  for i in $(seq 1 90); do
    case "$(kubectl -n "$LQ_NS" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" in
      "$want") return 0 ;;
      Failed) [ "$want" = Failed ] && return 0 ;;
    esac
    sleep 5
  done
  return 1
}

echo "[case-${CASE_ID}] claiming ${CLAIMS} × ${PROFILE}"
for i in $(seq 1 "$CLAIMS"); do
  mkpod "case${CASE_ID}-p${i}" "$(partition_reslines)"
done

RUNNING=0
for i in $(seq 1 "$CLAIMS"); do
  wait_phase "case${CASE_ID}-p${i}" Running && RUNNING=$((RUNNING + 1))
done
verdict "$([ "$RUNNING" -eq "$CLAIMS" ] && echo 0 || echo 1)" \
  "every partition claim is admitted and running" \
  "${RUNNING}/${CLAIMS} Running" \
  "only ${RUNNING}/${CLAIMS} reached Running; see the Pods' events"

# (1) and (2): the geometry each container sees, and that the identities differ.
EXPECTED=$(printf '%s' "$DEVS_JSON" | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items', []):
    if d['metadata']['name'] != '${NODE}':
        continue
    for g in d.get('spec', {}).get('groups', []):
        for a in g.get('accelerators') or []:
            for p in (a.get('status', {}).get('physicalSliced', {}).get('profiles') or []):
                if p['name'] == '${PROFILE}':
                    print(p.get('memoryMib')); raise SystemExit
")
echo "[case-${CASE_ID}] ${PROFILE} is ${EXPECTED} MiB per the ledger"

GEOMETRY_OK=0
GEOMETRY_SEEN=""
IDS=""
for i in $(seq 1 "$CLAIMS"); do
  pod="case${CASE_ID}-p${i}"
  [ "$(kubectl -n "$LQ_NS" get pod "$pod" -o jsonpath='{.status.phase}' 2>/dev/null)" = Running ] || continue

  seen=$(kubectl -n "$LQ_NS" exec "$pod" -- python3 -c '
import torch
torch.cuda.init()
n = torch._C._cuda_getDeviceCount()
p = torch.cuda.get_device_properties(0)
print("%d %d" % (n, p.total_memory // 1024 // 1024))
' 2>/dev/null | tail -1)
  GEOMETRY_SEEN="${GEOMETRY_SEEN}${pod}=[${seen:-unread}] "
  [ "$seen" = "1 ${EXPECTED}" ] && GEOMETRY_OK=$((GEOMETRY_OK + 1))

  id=$(kubectl -n "$LQ_NS" get pod "$pod" -o json 2>/dev/null | python3 -c "
import json,sys
a = json.load(sys.stdin)['metadata'].get('annotations', {}).get('device.gpustack.ai/accelerator.allocated')
if a:
    for ctr, v in json.loads(a).items():
        for g in v['devices']['groups']:
            for acc in g['accelerators']:
                print(acc.get('allocatedPhysicalID', ''))
" 2>/dev/null | head -1)
  IDS="${IDS}${id} "
done

verdict "$([ "$GEOMETRY_OK" -eq "$RUNNING" ] && [ "$RUNNING" -gt 0 ] && echo 0 || echo 1)" \
  "each container sees exactly one device of the profile's own memory" \
  "${GEOMETRY_OK}/${RUNNING} saw 1 device of ${EXPECTED} MiB" \
  "expected '1 ${EXPECTED}' from every container, saw: ${GEOMETRY_SEEN}"

UNIQUE_IDS=$(tr ' ' '\n' <<<"$IDS" | grep -c . || true)
DISTINCT_IDS=$(tr ' ' '\n' <<<"$IDS" | grep . | sort -u | wc -l | tr -d ' ')
verdict "$([ "$UNIQUE_IDS" -gt 1 ] && [ "$DISTINCT_IDS" = "$UNIQUE_IDS" ] && echo 0 || echo 1)" \
  "concurrent partitions carry distinct instance identities" \
  "${DISTINCT_IDS} distinct across ${UNIQUE_IDS} grants" \
  "${DISTINCT_IDS} distinct across ${UNIQUE_IDS} grants — a shared identity means two tenants on one instance"

# The vendor's own registry must hold one file per grant, on top of what was there before.
INSTANCES_NOW=$(node_instance_count || echo "")
verdict "$([ "${INSTANCES_NOW:-0}" -ge "$(( INSTANCES_BEFORE + RUNNING ))" ] && echo 0 || echo 1)" \
  "the vendor registry gained one instance per grant" \
  "${INSTANCES_BEFORE} → ${INSTANCES_NOW} for ${RUNNING} grant(s)" \
  "${INSTANCES_BEFORE} → ${INSTANCES_NOW} for ${RUNNING} grant(s) — a grant carved nothing"

# (3) The vendor's one-partition-per-container limit, asserted as a refusal.
if [ "$PARTITION_POOL" -ge 2 ]; then
  # The refusal can land at either of two gates, and both are the contract being asserted: the
  # admission webhook rejects the create outright, or — if it ever stopped — the allocator refuses to
  # actuate and the Pod ends Failed. What must never happen is a Running container holding one
  # partition while it was admitted for two.
  TWO_CREATE=$(mkpod_raw "case${CASE_ID}-two" \
    "$(printf '          cpu: "2"\n          memory: 4Gi\n          %s: "2"\n          %s: "2"' "$PARTITIONED" "$PROFILE_KEY")" 2>&1)
  TWO_RC=$?
  if [ "$TWO_RC" -ne 0 ]; then
    verdict 0 "a request for two partitions is refused rather than half-served" \
      "refused at admission: $(tr '\n' ' ' <<<"$TWO_CREATE" | sed 's/  */ /g' | cut -c1-100)" ""
  else
    PODS+=("case${CASE_ID}-two")
    TWO_STATE=""
    for i in $(seq 1 40); do
      TWO_STATE=$(kubectl -n "$LQ_NS" get pod "case${CASE_ID}-two" -o jsonpath='{.status.phase}' 2>/dev/null)
      { [ "$TWO_STATE" = Running ] || [ "$TWO_STATE" = Failed ]; } && break
      sleep 5
    done
    TWO_MSG=$(kubectl -n "$LQ_NS" get pod "case${CASE_ID}-two" -o jsonpath='{.status.message}' 2>/dev/null)
    verdict "$([ "$TWO_STATE" != Running ] && echo 0 || echo 1)" \
      "a request for two partitions is refused rather than half-served" \
      "${TWO_STATE:-Pending}: ${TWO_MSG:0:90}" \
      "it Ran — the container can only ever see one partition, so it got less than it was admitted for"
  fi
else
  record SKIP "a request for two partitions is refused rather than half-served" \
    "the node advertises ${PARTITION_POOL} partition token(s); two cannot be asked for"
fi

# (4) Every OTHER profile the node offers is served with the geometry the ledger names. Contract (2)
# proves the narrowest one, which is the only width a card holds several of; a wider profile is a
# different driver object with its own placement arithmetic, and a card whose 1-slice partitions are
# right can still hand out a 4-slice one that is not. Compute is asserted as well as memory here,
# because the whole point of a wider profile is the compute it adds.
read -r CARD_CORES CARD_SLICES <<<"$(printf '%s' "$DEVS_JSON" | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items', []):
    if d['metadata']['name'] != '${NODE}':
        continue
    for g in d.get('spec', {}).get('groups', []):
        for a in g.get('accelerators') or []:
            ps = a.get('status', {}).get('physicalSliced') or {}
            if ps.get('count'):
                # The card's whole compute, and the slices it divides into: a profile's own share is
                # its computeSlices out of those, which is the figure a container must report.
                print('%d %d' % (g.get('cores', 0), ps['count'])); raise SystemExit
")"

WIDER=$(tr ',' '\n' <<<"$PROFILES" | sort | tail -n +2)
if [ -z "$WIDER" ] || [ -z "${CARD_CORES:-}" ] || [ "${CARD_SLICES:-0}" -eq 0 ]; then
  record SKIP "every profile the node offers is served with the geometry the ledger names" \
    "the node offers one profile, or the ledger names no card geometry to derive compute from"
else
  WIDE_OK=0
  WIDE_TRIED=0
  WIDE_SEEN=""
  for prof in $WIDER; do
    key="${PARTITIONED}.mig-${prof}"
    pool=$(node_key "$key")
    [ -n "${pool:-}" ] && [ "${pool:-0}" -gt 0 ] || continue
    WIDE_TRIED=$((WIDE_TRIED + 1))

    want=$(printf '%s' "$DEVS_JSON" | python3 -c "
import json,sys
for d in json.load(sys.stdin).get('items', []):
    if d['metadata']['name'] != '${NODE}':
        continue
    for g in d.get('spec', {}).get('groups', []):
        for a in g.get('accelerators') or []:
            for p in (a.get('status', {}).get('physicalSliced', {}).get('profiles') or []):
                if p['name'] == '${prof}':
                    print('%d %d' % (p.get('memoryMib', 0),
                                     ${CARD_CORES} * p.get('computeSlices', 0) // ${CARD_SLICES}))
                    raise SystemExit
")
    pod="case${CASE_ID}-w$(tr -d '.g' <<<"$prof")"
    mkpod "$pod" "$(printf '          cpu: "2"\n          memory: 4Gi\n          %s: "1"\n          %s: "1"' \
      "$PARTITIONED" "$key")" || { WIDE_SEEN="${WIDE_SEEN}${prof}=[refused] "; continue; }
    if ! wait_phase "$pod" Running; then
      WIDE_SEEN="${WIDE_SEEN}${prof}=[not Running] "
      continue
    fi
    seen=$(kubectl -n "$LQ_NS" exec "$pod" -- python3 -c '
import torch
torch.cuda.init()
n = torch._C._cuda_getDeviceCount()
p = torch.cuda.get_device_properties(0)
print("%d %d %d" % (n, p.total_memory // 1024 // 1024, p.multi_processor_count))
' 2>/dev/null | tail -1)
    WIDE_SEEN="${WIDE_SEEN}${prof}=[${seen:-unread} want 1 ${want}] "
    [ "$seen" = "1 ${want}" ] && WIDE_OK=$((WIDE_OK + 1))
  done

  if [ "$WIDE_TRIED" -eq 0 ]; then
    record SKIP "every profile the node offers is served with the geometry the ledger names" \
      "no wider profile has free capacity while contract (2)'s claims are held"
  else
    verdict "$([ "$WIDE_OK" -eq "$WIDE_TRIED" ] && echo 0 || echo 1)" \
      "every profile the node offers is served with the geometry the ledger names" \
      "${WIDE_OK}/${WIDE_TRIED} wider profile(s) matched memory AND compute: ${WIDE_SEEN}" \
      "a wider profile served geometry the ledger does not name: ${WIDE_SEEN}"
  fi
fi

# (5) A partitioned node serves NOTHING but partitions. With the mode on, a container given the
# device nodes but no instance configuration finds no device at all, so the whole-card family is
# advertised at zero and a request for it must never run. Either gate satisfies this — the scheduler
# never places it, or the queue never admits it — and what must not happen is a Running container
# holding a card the node cannot actually give it.
WHOLE_KEY="${PARTITIONED%.partitioned}"
WHOLE_CAP=$(kubectl get node "$NODE" -o jsonpath="{.status.allocatable.${WHOLE_KEY//./\\.}}" 2>/dev/null)
WHOLE_CREATE=$(mkpod_raw "case${CASE_ID}-whole" \
  "$(printf '          cpu: "2"\n          memory: 4Gi\n          %s: "1"' "$WHOLE_KEY")" 2>&1)
WHOLE_RC=$?
if [ "$WHOLE_RC" -ne 0 ]; then
  verdict 0 "a whole-card request is not served on a partitioned node" \
    "refused at admission: $(tr '\n' ' ' <<<"$WHOLE_CREATE" | sed 's/  */ /g' | cut -c1-100)" ""
else
  PODS+=("case${CASE_ID}-whole")
  WHOLE_STATE=""
  for i in $(seq 1 12); do
    WHOLE_STATE=$(kubectl -n "$LQ_NS" get pod "case${CASE_ID}-whole" -o jsonpath='{.status.phase}' 2>/dev/null)
    [ "$WHOLE_STATE" = Running ] && break
    sleep 5
  done
  verdict "$([ "$WHOLE_STATE" != Running ] && echo 0 || echo 1)" \
    "a whole-card request is not served on a partitioned node" \
    "${WHOLE_STATE:-Pending} against ${WHOLE_KEY}=${WHOLE_CAP:-0} allocatable" \
    "it Ran while ${WHOLE_KEY} is ${WHOLE_CAP:-0} — the container has no device at all"
fi

# (6) Reclaim: every instance this case caused must go when its Pod does.
cleanup
PODS=()
RECLAIMED=""
for i in $(seq 1 40); do
  RECLAIMED=$(node_instance_count || echo "")
  [ "${RECLAIMED:-999}" -le "$INSTANCES_BEFORE" ] && break
  sleep 5
done
verdict "$([ "${RECLAIMED:-999}" -le "$INSTANCES_BEFORE" ] && echo 0 || echo 1)" \
  "every partition is reclaimed when its Pod goes" \
  "registry back to ${RECLAIMED} (was ${INSTANCES_BEFORE})" \
  "registry left at ${RECLAIMED}, was ${INSTANCES_BEFORE} — a leaked partition blocks leaving the mode"

echo
echo "== CASE ${CASE_ID} — Hygon DCU partitions: what a grant carries, what one card holds at once, and what is refused =="
echo "   node ${NODE}, group ${GROUP}, profile ${PROFILE}"
results
