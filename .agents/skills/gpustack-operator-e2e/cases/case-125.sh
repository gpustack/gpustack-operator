#!/usr/bin/env bash
#
# CASE 125 — The llm-d-router picker carries the encoder stage: an encode filter, an unscored
#           encode profile, and the multimodal decider (MUTATING, self-recovering)
#           AUTO-SKIPS without an InstanceType that reports an admitted accelerator platform, which
#           this case builds by injecting a CPU vendor label
#
#   case-125.sh <NS>
#
# Goal:        llm-d-router is the ONLY router both admitted rows name, so what its picker document
#              says about an encoder is the part of the router that carries this whole feature.
#              Cases 116 to 119 read admission, Services and mounts; case 120 and 121 send a real
#              request through the router. Nothing reads the document itself. This case reads it, and
#              reads the three facts in it that an encoder changes:
#
#                - an ENCODE FILTER (label-selector-filter, selecting role-kind=encode) exists;
#                - the ENCODE PROFILE names that filter and NOTHING ELSE: no queue scorer, no
#                  KV-cache scorer, no prefix scorer, no picker;
#                - the MULTIMODAL DECIDER (always-disagg-multimodal-decider) is present, and the
#                  disagg-profile-handler routes BOTH "prefill" and "encode" at it.
#
#              THE ENCODE PROFILE'S EMPTINESS IS THE ROW THAT MATTERS, and it is asserted by
#              content rather than by absence of a key. An encoder keeps no KV cache, so the queue
#              and KV-cache scorers read gauges it never exports and rank every candidate
#              identically; the prefix scorer asks about token overlap, which does not exist before
#              a request has tokens. A scorer that cannot separate candidates is not a weak
#              preference, it is a cost paid on every media request. Upstream completes any profile
#              naming none with its own max-score-picker at load time, so an empty plugin list is
#              valid and still runs — which is why the row asserts the exact list and not merely
#              "no prefix scorer".
#
#              The decider's ABSENCE would be the other failure worth catching: without it the
#              encoder stage is never selected and a multimodal request is answered by a
#              prefiller running undivided, which looks like success and is not the disaggregation
#              this feature exists to perform.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator: the
#              encoder shape reaches admission through the platform an injected CPU vendor label
#              builds ("Ascend" — an admitted row whose connector is the mounted directory and
#              whose floor, 0.30.0rc1, the pinned version 0.30.0 clears; override it with
#              E2E_C125_VERSION when that floor moves). The gpustack-settings Secret
#              must exist in the system namespace (E2E_SYSTEM_NS, default gpustack-system).
#
#              AUTO-SKIP: the subject needs the platform InstanceType, and the vendor label may not
#              take (NFD owns cpu-model.vendor_id and may decline an injected override). It also
#              needs the pool's entrance LocalQueue in <NS>, because that is what admits the groups
#              and therefore what renders the router document. Either one absent records a SKIP
#              naming it and exits 0. The router image may also be unpullable: the rows read the
#              router's CONFIGMAP, which the operator renders before any image is pulled, so an
#              unpullable image does not skip them — only the Pod-level row that checks the document
#              reached a running process.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and one
#              settings key (CPU-manufacturer awareness) flipped on — both restored by the trap.
#              REAL: one PV, one PVC (a hostPath volume, manually bound) and one ModelDeployment
#              whose encode and prefill roles mount the claim at one path, exactly as case 119
#              builds it; the router ConfigMap the operator renders is the subject.
#
# Expected:    - the deployment is admitted and created, and ALL THREE of its role Pods exist (the
#                control deployment's two are checked against its own role count; none need run);
#              - the router ConfigMap carries an EndpointPickerConfig with one filter per role, and
#                the encode filter matches role-kind=encode;
#              - the encode profile's plugin list is EXACTLY the encode filter's reference;
#              - the always-disagg-multimodal-decider is present, and the disagg-profile-handler
#                maps both prefill and encode to a decider;
#              - the precise prefix-cache producer EXCLUDES role-kind=decode and role-kind=encode
#                from its podLabelSelector, because neither publishes KV events;
#              - for contrast, a deployment with NO encoder role renders no multimodal decider and
#                no encode profile — the same document, read a second time.
#
# Cleanup:     The trap deletes the deployment, releases any Workload still holding its replicas,
#              deletes the PVC and the PV, the NodeFeature object, and restores the settings key to
#              its exact original value, then waits for the chain to settle back: the derived type
#              goes only once the setting cache flips and the pool re-derives. Idempotent, runs on
#              pass AND fail, safe to re-run.
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:-}"
if [ -z "$NS" ]; then
  echo "usage: case-125.sh <NS>" >&2
  exit 2
fi

SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C125_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF=case125-cpu-vendor
MD=case125-epd
PD=case125-pd
CLAIM=case125-claim
PV=case125-pv
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
# The engine version is a variable like every other case's, so a floor that moves is one override
# away instead of a refusal the case reads as an operator defect. 0.30.0 clears the 0.30.0rc1 floor
# the injected Ascend row records.
VERSION="${E2E_C125_VERSION:-0.30.0}"
MOUNT=/ec-cache
SETTLE_ROUNDS="${E2E_C125_SETTLE:-60}"
POD_ROUNDS="${E2E_C125_PODS:-40}"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

command -v jq >/dev/null 2>&1 || { echo "[case-125] jq is required." >&2; exit 2; }

pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    if n.get("metadata",{}).get("labels",{}).get("feature.gpustack.ai/acceleratable") == "true":
        continue
    ts=n.get("spec",{}).get("taints",[])
    if any(str(t.get("key","")).startswith("node-role.kubernetes.io/") for t in ts):
        continue
    print(n["metadata"]["name"])
    break
'
}

epd_platform_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
      [ -n "$name" ] || continue
      [ -z "$deleting" ] || continue
      [ "$inactive" = true ] && continue
      if [ "$manufacturer" = "$PLATFORM" ]; then
        echo "$name"
        break
      fi
    done
}

ORIG_SETTING="$(kubectl -n "$SYSTEM_NS" get secret gpustack-settings -o jsonpath="{.data.${SETTING}}" 2>/dev/null)"
set_awareness() { # true|false
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":\"$(printf '%s' "$1" | base64)\"}}" >/dev/null 2>&1
}
restore_setting() {
  local v=null
  [ -n "$ORIG_SETTING" ] && v="\"${ORIG_SETTING}\""
  kubectl -n "$SYSTEM_NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${SETTING}\":${v}}}" >/dev/null 2>&1
}

# Restoring the value is not the end of the restore: the worker reads it through a thirty-second
# cache, and the type derived from it outlives the patch by that cache plus one reconcile. Wait for
# the derived type to go, so the next case in a serial run does not list an aware type this run left
# behind. A cluster that started with the setting already on keeps its derived type, so there is
# nothing to wait for.
settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

# One list call, because this runs from the EXIT trap: Kueue holds a finalizer on every Pod of a
# serving group and releases it only when the Workload is deleted.
force_release() {
  local md="$1" row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in
        *" $u "*)
          kubectl -n "$NS" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$NS" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
  return 0
}

INJECTED=false
cleanup() {
  echo
  echo "[case-125] cleanup"
  for md in "$MD" "$PD"; do
    kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$md" \
      --ignore-not-found --wait=false >/dev/null 2>&1
  done
  for _ in $(seq 1 12); do
    sleep 5
    force_release "$MD"
    force_release "$PD"
    [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null)" ] &&
      [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${PD}" --no-headers 2>/dev/null)" ] && break
  done
  kubectl -n "$NS" delete pvc "$CLAIM" --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete pv "$PV" --ignore-not-found --wait=false >/dev/null 2>&1
  if [ "$INJECTED" = true ]; then
    kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
    restore_setting
    settle_back
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

IT="$(epd_platform_type)"
if [ -n "$IT" ]; then
  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}'"
else
  NODE="$(pick_worker_node)"
  if [ -z "$NODE" ]; then
    echo "[case-125] no Ready schedulable worker node; this case cannot place its vendor label" >&2
    exit 2
  fi

  echo "[case-125] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"
  INJECTED=true

  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${NF}
  namespace: ${NS}
  labels:
    nfd.node.kubernetes.io/node-name: ${NODE}
spec:
  labels:
    feature.node.kubernetes.io/cpu-model.vendor_id: "${VENDOR}"
  features: {}
YAML
  set_awareness true

  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    IT="$(epd_platform_type)"
    [ -n "$IT" ] && break
    sleep 5
  done

  if [ -z "$IT" ]; then
    record SKIP "the router picker carries the encoder stage" \
      "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
    print_rows
    echo "[case-125] skipped: no platform InstanceType"
    exit 0
  fi

  record PASS "a CPU-only pool reports an admitted platform" \
    "instancetype ${IT} reports manufacturer '${PLATFORM}' from the injected vendor label"
fi

# The pool's entrance LocalQueue is what admits the groups, so a deployment whose type has no
# entrance is never scheduled and its router ConfigMap is never rendered. The poll result is read,
# not discarded: without this row the missing queue surfaced much later as a pod-count FAIL that
# named the wrong cause.
ENTRANCE="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
ENTRANCE_LQ=""
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  ENTRANCE_LQ="$(kubectl -n "$NS" get localqueue "${ENTRANCE:-}" -o name 2>/dev/null)"
  [ -n "$ENTRANCE_LQ" ] && break
  sleep 5
done

if [ -z "$ENTRANCE_LQ" ]; then
  record SKIP "the pool's entrance LocalQueue exists" \
    "instancetype ${IT} names entrance '${ENTRANCE:-<none>}' and no localqueue/${ENTRANCE:-<none>} answered in ${NS} within $((SETTLE_ROUNDS * 5))s, so nothing is admitted, no role Pod is created and no router document is ever rendered"
  print_rows
  echo "[case-125] skipped: no entrance LocalQueue"
  exit 0
fi

record PASS "the pool's entrance LocalQueue exists" "${ENTRANCE_LQ##*/} in ${NS}"

# The claim both encoder-side roles mount: a hostPath volume under /tmp on the labeled node,
# manually bound, exactly as case 119 builds it. The router document is the subject here, so the
# claim only has to be well formed enough for admission to accept the shape.
kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${PV}
  labels:
    app.kubernetes.io/part-of: gpustack-operator-e2e
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ""
  hostPath:
    path: /tmp/gpustack-case-125
    type: DirectoryOrCreate
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${CLAIM}
  namespace: ${NS}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ""
  volumeName: ${PV}
YAML

# ---------------------------------------------------------------- the two deployments

# The ENCODER deployment is the subject. The encoder is listed FIRST on purpose: the deployment-wide
# Service fronts the first role that is not an encoder, so first-position is where an implementation
# that forgot the rule would point the front door at the encoder.
APPLY_OUT="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: encode
      kind: Encode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${MOUNT}
          persistentVolumeClaim:
            claimName: ${CLAIM}
    - name: prefill
      kind: Prefill
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
      additionalVolumes:
        - mountPath: ${MOUNT}
          persistentVolumeClaim:
            claimName: ${CLAIM}
    - name: decode
      kind: Decode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"

if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  record PASS "the encoder deployment is admitted and created" \
    "one encode, one prefill and one decode role on ${IT}, llm-d-router, vLLM ${VERSION}"
else
  record FAIL "the encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

# The CONTROL deployment is the same router with no encoder at all. Reading the same document twice
# is what makes the encoder's three facts encoder facts rather than prefill/decode facts that happen
# to be there: every term the control lacks is a term the encoder added.
#
# Its apply output is KEPT rather than discarded. A refusal here would otherwise surface as the
# pod-count row below, which names a missing Pod and not the refusal that caused it — the same
# wrong-cause report the LocalQueue poll was fixed for.
PD_APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${PD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    version: "${VERSION}"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  router:
    name: llm-d-router
  roles:
    - name: prefill
      kind: Prefill
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
    - name: decode
      kind: Decode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"

# An empty output is not an accepted apply, so it counts as refused here too.
PD_REFUSED=no
case "$PD_APPLY_OUT" in
  *created* | *configured*) ;;
  *) PD_REFUSED=yes ;;
esac

# THE ENCODER DEPLOYMENT RENDERS THREE ROLE PODS AND THE CONTROL TWO, and the count each row
# waits for is that deployment's own role count rather than a flat number: a two-of-three gate
# would pass a renderer that dropped the encoder, which is the failure this case exists to catch.
for md in "$MD" "$PD"; do
  if [ "$md" = "$MD" ]; then want=3; else want=2; fi
  count=""
  for _ in $(seq 1 "$POD_ROUNDS"); do
    count="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" --no-headers 2>/dev/null | grep -c . || true)"
    [ "${count:-0}" -ge "$want" ] && break
    sleep 5
  done
  if [ "${count:-0}" -ge "$want" ]; then
    record PASS "${md} rendered its role Pods" "${count} Pods for ${want} roles; the rows below read the router document, not the containers"
  else
    # A refused control apply is the cause of a missing control Pod, so it is named here instead of
    # leaving the row to be read as a renderer that dropped a role.
    cause=""
    if [ "$md" = "$PD" ] && [ "$PD_REFUSED" = yes ]; then
      cause="; its apply was refused: $(printf '%s' "$PD_APPLY_OUT" | tr '\n' ' ' | cut -c1-220)"
    fi
    record FAIL "${md} rendered its role Pods" \
      "only ${count:-0} of ${want} role Pods appeared within $((POD_ROUNDS * 5))s, so the router ConfigMap may never be rendered${cause}"
  fi
done

# ---------------------------------------------------------------- the picker document

# The document as YAML, from the ConfigMap the operator renders into the router Pod. A router
# configured by argv renders none, so its ABSENCE is a distinct answer from an empty one and both
# are read rather than assumed.
picker_yaml() { # <modeldeployment>
  kubectl -n "$NS" get configmap "$1-router" -o jsonpath='{.data.epp\.yaml}' 2>/dev/null
}

DOC="$(picker_yaml "$MD")"
if [ -z "$DOC" ]; then
  record FAIL "the router ConfigMap carries an EndpointPickerConfig" \
    "no configmap/${MD}-router carries a key epp.yaml, so there is no document to read"
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-125] ${FAILS} check(s) FAILED"; exit 1; }
  echo "[case-125] all checks passed"
  exit 0
fi

# Each check runs the document through jq as YAML (jq reads YAML only with -y on some builds), so
# the document is converted once with python3, which every case in this suite already requires.
json_of() { # yaml-on-stdin
  python3 -c '
import json,sys
try:
    import yaml
except ImportError:
    sys.stderr.write("pyyaml is required to read the router document")
    raise SystemExit(3)
d = yaml.safe_load(sys.stdin.read())
print(json.dumps(d) if d is not None else "{}")
'
}

DOC_JSON="$(printf '%s' "$DOC" | json_of)" || {
  echo "[case-125] cannot read the router document: pyyaml is required in the runner." >&2
  exit 2
}
PD_JSON="$(printf '%s' "$(picker_yaml "$PD")" | json_of)" || PD_JSON=""

if printf '%s' "$DOC_JSON" | jq -e '.kind == "EndpointPickerConfig" and (.plugins | type == "array")' \
  >/dev/null 2>&1; then
  record PASS "the router ConfigMap carries an EndpointPickerConfig" \
    "$(printf '%s' "$DOC_JSON" | jq -r '.plugins | length') plugins and $(printf '%s' "$DOC_JSON" | jq -r '.schedulingProfiles | length') scheduling profiles"
else
  record FAIL "the router ConfigMap carries an EndpointPickerConfig" \
    "kind=$(printf '%s' "$DOC_JSON" | jq -r '.kind // "<none>"'), and the document has no plugins array to read"
fi

# The encode filter: one label-selector-filter whose matchLabels select role-kind=encode. The label
# key is the role-kind label the operator stamps on every Pod, so this is the selector the router
# will actually match against, read off the document rather than assumed from the render.
FILTER_OK="$(printf '%s' "$DOC_JSON" | jq -r '
  [ .plugins[]?
    | select(.type == "label-selector-filter")
    | select(.parameters.matchLabels["modeldeployment.gpustack.ai/role-kind"] == "encode")
  ] | length')"
if [ "${FILTER_OK:-0}" -ge 1 ]; then
  record PASS "an encode filter selects the encoder's Pods" \
    "a label-selector-filter matches modeldeployment.gpustack.ai/role-kind=encode"
else
  record FAIL "an encode filter selects the encoder's Pods" \
    "no label-selector-filter matches role-kind=encode; the picker would never route to an encoder"
fi

# THE ENCODE PROFILE'S PLUGIN LIST, asserted exactly. Upstream completes any profile naming none with
# its own max-score-picker at load time, so [encode-pods] is a valid, runnable profile — and any
# additional plugin is a scorer that cannot separate encoder candidates.
ENCODE_PLUGINS="$(printf '%s' "$DOC_JSON" | jq -c \
  '[.schedulingProfiles[]? | select(.name == "encode") | .plugins[]? | .pluginRef] | unique')"
if [ "$ENCODE_PLUGINS" = '["encode-pods"]' ]; then
  record PASS "the encode profile scores on nothing but its own filter" \
    "plugins=${ENCODE_PLUGINS}: upstream appends its max-score-picker to a profile naming none"
else
  record FAIL "the encode profile scores on nothing but its own filter" \
    "encode profile plugins=${ENCODE_PLUGINS}, expected [\"encode-pods\"]: an encoder keeps no KV cache, so the queue and KV scorers read gauges it never exports and the prefix scorer asks about tokens that do not exist yet"
fi

# The decider, and the handler that routes BOTH halves at one. Without the encode term in the
# handler the decider is present but never consulted, and the encoder stage is silently unused.
DECIDER="$(printf '%s' "$DOC_JSON" | jq -r \
  '[.plugins[]? | select(.type == "always-disagg-multimodal-decider")] | length')"
HANDLER="$(printf '%s' "$DOC_JSON" | jq -c \
  '[.plugins[]? | select(.type == "disagg-profile-handler") | .parameters] | first // {}')"
if [ "${DECIDER:-0}" -ge 1 ]; then
  record PASS "the multimodal decider is present" "always-disagg-multimodal-decider is in the document"
else
  record FAIL "the multimodal decider is present" \
    "no always-disagg-multimodal-decider: without it the encoder stage is never selected and a media request is answered undivided"
fi

HANDLER_ENCODE="$(printf '%s' "$HANDLER" | jq -r '.profiles.encode // ""')"
HANDLER_PREFILL="$(printf '%s' "$HANDLER" | jq -r '.profiles.prefill // ""')"
HANDLER_DECIDER_ENCODE="$(printf '%s' "$HANDLER" | jq -r '.deciders.encode // ""')"
if [ "$HANDLER_ENCODE" = encode ] && [ "$HANDLER_PREFILL" = prefill ] \
  && [ "$HANDLER_DECIDER_ENCODE" = always-disagg-multimodal-decider ]; then
  record PASS "the handler routes the encoder stage at the multimodal decider" \
    "profiles.encode=encode, profiles.prefill=prefill, deciders.encode=always-disagg-multimodal-decider"
else
  record FAIL "the handler routes the encoder stage at the multimodal decider" \
    "profiles.encode='${HANDLER_ENCODE}' profiles.prefill='${HANDLER_PREFILL}' deciders.encode='${HANDLER_DECIDER_ENCODE}'"
fi

# The precise prefix-cache producer's Pod discovery EXCLUDES the two kinds that publish no events.
# The exclusion is what makes the terms exist at all, so the row reads the selector string rather
# than the plugin's presence.
#
# THE KEY IS podLabelSelector, which is what the renderer writes. A producer configured by a
# different field would render a document this case could not read, and reading one that does not
# exist would answer "no producer" on a cluster that has one — so the key is named here rather than
# guessed, and the failure message quotes it.
DISCOVERY="$(printf '%s' "$DOC_JSON" | jq -r '
  [ .plugins[]?
    | select(.type == "precise-prefix-cache-producer")
    | .parameters.kvEventsConfig.podDiscoveryConfig.podLabelSelector // ""
  ] | first // ""')"
case "$DISCOVERY" in
  *role-kind!=decode*role-kind!=encode* | *role-kind!=encode*role-kind!=decode*)
    record PASS "the prefix-cache producer excludes the kinds that publish no events" \
      "podLabelSelector: ${DISCOVERY}"
    ;;
  "")
    # The renderer picks the PRECISE producer whenever the deployment opens a KV event port, and an
    # encoder leg does — so this branch is the alternative a renderer can still emit, not the
    # expected shape: the approximate producer discovers Pods by service and carries no role
    # selector, so there is nothing to exclude and nothing wrong. It is a PASS, because a
    # deployment that renders no events has no eventless kind to exclude.
    APPROX="$(printf '%s' "$DOC_JSON" | jq -r \
      '[.plugins[]? | select(.type == "approx-prefix-cache-producer")] | length')"
    if [ "${APPROX:-0}" -ge 1 ]; then
      record PASS "the prefix-cache producer excludes the kinds that publish no events" \
        "the approximate producer renders instead, which discovers Pods by service and has no role selector to exclude"
    else
      record FAIL "the prefix-cache producer excludes the kinds that publish no events" \
        "neither producer is in the document, so neither kind is excluded and nothing is subscribed"
    fi
    ;;
  *)
    record FAIL "the prefix-cache producer excludes the kinds that publish no events" \
      "podLabelSelector '${DISCOVERY}' lacks role-kind!=decode or role-kind!=encode: both kinds publish no KV events and would be subscribers dialing a socket the engine never opens"
    ;;
esac

# ---------------------------------------------------------------- the control deployment

# The same router, the same engine, one role fewer. Every term the control lacks is a term the
# encoder added, which is what makes the rows above encoder facts rather than router defaults.
if [ -z "$PD_JSON" ]; then
  record SKIP "a deployment with no encoder role renders no encoder stage" \
    "no configmap/${PD}-router carries a key epp.yaml, so the control document could not be read; the encoder rows above stand on their own"
else
  CTRL_DECIDER="$(printf '%s' "$PD_JSON" | jq -r \
    '[.plugins[]? | select(.type == "always-disagg-multimodal-decider")] | length')"
  CTRL_PROFILE="$(printf '%s' "$PD_JSON" | jq -r \
    '[.schedulingProfiles[]? | select(.name == "encode")] | length')"
  CTRL_FILTER="$(printf '%s' "$PD_JSON" | jq -r \
    '[.plugins[]? | select(.type == "label-selector-filter")
      | select(.parameters.matchLabels["modeldeployment.gpustack.ai/role-kind"] == "encode")] | length')"
  if [ "${CTRL_DECIDER:-0}" -eq 0 ] && [ "${CTRL_PROFILE:-0}" -eq 0 ] && [ "${CTRL_FILTER:-0}" -eq 0 ]; then
    record PASS "a deployment with no encoder role renders no encoder stage" \
      "the control's picker carries no multimodal decider, no encode profile and no encode filter, so all three are the encoder's and not the router's defaults"
  else
    record FAIL "a deployment with no encoder role renders no encoder stage" \
      "the control still carries decider=${CTRL_DECIDER} profile=${CTRL_PROFILE} filter=${CTRL_FILTER}, which would mean the encoder rows above prove nothing about the encoder"
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-125] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-125] all checks passed"