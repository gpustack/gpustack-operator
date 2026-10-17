#!/usr/bin/env bash
#
# CASE 117 — The encoder's own Service exists, and the front door never points at it
#   (MUTATING, self-recovering)
#   AUTO-SKIPS without an InstanceType that reports an admitted accelerator platform, which this
#   case builds by injecting a CPU vendor label.
#
#   case-117.sh <NS>
#
# Goal:        A deployment with an encoder role gets one Service per role, and the address a
#              caller dials never resolves to the encoder. The encoder answers no request of its
#              own: it turns media into embeddings for a prefiller, so a front door pointed at one
#              sends traffic to a process that serves none of it — with endpoints that resolve and
#              answer health checks, which is what makes that failure quiet. So the case asserts
#              the three per-role Services by NAME, each selecting only its own role's Pods, and
#              the deployment-wide Service selecting the prefiller while the encoder sits FIRST in
#              the roles list — the position where a "front the first role" implementation would
#              pick it.
#
#              THE SELECTOR IS THE OBSERVABLE, not the Service's existence alone. A Service whose
#              selector matched every role of the deployment would exist under the same name and
#              pass an existence check, so each row resolves the Service's own selector against the
#              live Pods and reads back which role's Pods it fronts.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator: the
#              encoder shape reaches admission through the platform the injected CPU vendor builds.
#              The gpustack-settings Secret must exist in the system namespace (E2E_SYSTEM_NS,
#              default gpustack-system). The replicas are placeholders that never serve (the
#              explicit pause image carries no engine), and the router image may be unpullable:
#              nothing here reads the router beyond the endpoint string.
#
#              AUTO-SKIP: the whole subject needs the encoder shape ADMITTED, which needs a
#              platform InstanceType. If the vendor label does not take (NFD owns cpu-model.vendor_id
#              and may decline an injected override) or the pool never derives, the case prints one
#              SKIP row naming that and exits 0.
#
# Existing platform: if an InstanceType already reports the manufacturer named by E2E_C117_VENDOR
#              (an accelerator cluster, for example E2E_C117_VENDOR=Ascend), the case uses it and
#              injects nothing, so the label, the setting and the settle wait are skipped.
#
# Inputs:      MOCKED: one node label (the CPU vendor, "AMD" — the platform whose admitted row
#              needs no shared claim) injected through a NodeFeature object, and one settings key
#              (CPU-manufacturer awareness) flipped on; both restored by the trap. REAL: one
#              ModelDeployment with the encode role listed first, engine vLLM, router
#              llm-d-router, one replica per role on the platform type.
#
# Expected:    - the deployment is created and its three role Pods exist (gated counts, they never
#                need to run);
#              - Services <deployment>-encode, <deployment>-prefill and <deployment>-decode exist,
#                each selecting exactly its own role's Pod and nobody else's;
#              - the deployment-wide Service exists and selects the prefill Pod, NOT the encode
#                Pod, with the encode role first in the list;
#              - status.roleSummary carries the ready counts in the fixed order E, P, D;
#              - the RoleKindsReady condition accounts the roles (KindsNotReady while the pause
#                placeholders fail the engine probes, AllKindsReady if something serves) rather
#                than reporting NoRoleStatuses;
#              - status.endpoint, when published, does not name the encoder;
#              - the v1 ModelDeployment kubectl table carries the Endpoint column alongside Engine,
#                Roles and Phase.
#
# Cleanup:     The trap deletes the deployment, releases any Workload still holding its replicas
#              (Kueue keeps a finalizer on every Pod of a serving group), deletes the NodeFeature
#              object, restores the settings key to its exact original value and waits for the chain
#              to settle back: the derived type goes only once the setting cache flips and the pool
#              re-derives. Idempotent, runs on pass AND fail, safe to re-run.
#
set -uo pipefail

E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-117.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
SETTING=instance-type-aware-cpu-manufacturer
VENDOR="${E2E_C117_VENDOR:-AMD}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"

# A platform whose encoder-cache connector carries the output through a directory (Ascend) is
# admitted only when the encode and prefill roles mount one shared claim. Name an existing
# ReadWriteMany claim in E2E_C117_EC_CLAIM; it stays empty for platforms that need none.
EC_CLAIM="${E2E_C117_EC_CLAIM:-}"
EC_VOLUMES=""
if [ -n "$EC_CLAIM" ]; then
  EC_VOLUMES="$(printf '      additionalVolumes:\n        - mountPath: /ec-cache\n          persistentVolumeClaim:\n            claimName: %s' "$EC_CLAIM")"
fi
NF="case117-cpu-vendor"
MD=case117-epd
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
SETTLE_ROUNDS="${E2E_C117_SETTLE:-60}"   # 60 x 5s = 5 min for the settings cache and re-derivation
POD_ROUNDS="${E2E_C117_PODS:-40}"        # 40 x 5s for the group to form and its Pods to exist
STATUS_ROUNDS="${E2E_C117_STATUS:-12}"   # 12 x 5s for the observed roles to reach the status

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

# One Ready, schedulable worker node (not a control-plane one): the vendor label lands there and
# that node's CPU becomes the pool whose manufacturer the case controls.
pick_worker_node() {
  kubectl get nodes -o json 2>/dev/null | python3 -c '
import json,sys
for n in json.load(sys.stdin).get("items",[]):
    cs=n.get("status",{}).get("conditions",[])
    if not any(c.get("type")=="Ready" and c.get("status")=="True" for c in cs):
        continue
    if n.get("spec",{}).get("unschedulable"):
        continue
    # A CPU-vendor probe only reads back on a node that feeds the CPU-only catalog. An
    # acceleratable node feeds the GPU InstanceType instead, whose manufacturer is the
    # accelerator vendor, so an injected cpu-model.vendor_id never surfaces here and the
    # platform-type poll below times out. Skip those nodes, exactly as case-18 reads the
    # CPU pool (acceleratable=false) rather than the accelerator one.
    if n.get("metadata",{}).get("labels",{}).get("feature.gpustack.ai/acceleratable") == "true":
        continue
    ts=n.get("spec",{}).get("taints",[])
    if any(str(t.get("key","")).startswith("node-role.kubernetes.io/") for t in ts):
        continue
    print(n["metadata"]["name"])
    break
'
}

# The InstanceType reporting the injected platform, skipping types on their way out and Inactive
# ones for the reasons case-51 records: naming either is refused at admission for a reason no row
# here is about.
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
# behind (case-18's wait_generic, on this same fixture). A cluster that started with the setting
# already on keeps its derived type, so there is nothing to wait for and the wait is skipped.
settle_back() {
  [ "$(printf '%s' "$ORIG_SETTING" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    [ -z "$(epd_platform_type)" ] && return 0
    sleep 5
  done
  return 0
}

# One list call, because this runs from the EXIT trap: Kueue holds a finalizer on every Pod of a
# serving group and releases it only when the Workload is deleted, so a cleanup that blocks on the
# deployment would block for as long as the caller allows (case-50's mechanic).
force_release() {
  local row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in *" $u "*)
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
  echo "[case-117] cleanup"
  kubectl -n "$NS" delete modeldeployments.worker.gpustack.ai "$MD" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  for _ in $(seq 1 12); do
    sleep 5
    force_release
    [ -z "$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null)" ] && break
  done
  if [ "$INJECTED" = true ]; then
    kubectl -n "$NS" delete nodefeatures.nfd.k8s-sigs.io "$NF" --ignore-not-found >/dev/null 2>&1
    restore_setting
    settle_back
  fi
}
trap cleanup EXIT

# ---------------------------------------------------------------- the fixture

IT="$(epd_platform_type)"
if [ -z "$IT" ]; then
  NODE="$(pick_worker_node)"
  if [ -z "$NODE" ]; then
    echo "[case-117] no Ready schedulable worker node; this case cannot place its vendor label" >&2
    exit 2
  fi

  echo "[case-117] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"
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

  # The poll IS the wait for the settings cache (30s), the node flavor re-derivation and the type
  # authoring. A timeout is a skip, not a failure: NFD owns cpu-model.vendor_id and may decline an
  # injected override, which is an environment answer this case reports rather than argues with.
  for _ in $(seq 1 "$SETTLE_ROUNDS"); do
    IT="$(epd_platform_type)"
    [ -n "$IT" ] && break
    sleep 5
  done
fi

if [ -z "$IT" ]; then
  record SKIP "an encoder deployment exists with one Service per role" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  print_rows
  echo "[case-117] skipped: no platform InstanceType"
  exit 0
fi

# The type's entrance LocalQueue in this namespace is what routes the group into its queue; the
# operator materializes it in every non-reserved namespace once the queue exists, so this poll is
# part of the fixture rather than a subject.
ENTRANCE="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  [ -n "$(kubectl -n "$NS" get localqueue "$ENTRANCE" -o name 2>/dev/null)" ] && break
  sleep 5
done

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' from the node labels; entrance ${ENTRANCE}"

# ---------------------------------------------------------------- the subject

# The encoder is FIRST in the list on purpose: the deployment-wide Service fronts the first role
# that is not an encoder, and first-position is where an implementation that forgot the rule would
# point the front door at the encoder.
APPLY_OUT="$(kubectl apply -f - 2>&1 <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${MD}
  namespace: ${NS}
spec:
  engine:
    name: vLLM
    image: ${IMAGE}
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
${EC_VOLUMES}
    - name: prefill
      kind: Prefill
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
${EC_VOLUMES}
    - name: decode
      kind: Decode
      instanceType: ${IT}
      replicas: 1
      image: ${IMAGE}
YAML
)"

# The gate is that the object is THERE, not that the apply printed a nice word: a refusal surfaces
# here as one message instead of nine timeouts (case-61's lesson).
if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  record PASS "the encoder deployment is admitted and created" \
    "one encode, one prefill and one decode role on ${IT}, llm-d-router, image ${IMAGE}"
else
  record FAIL "the encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

# The three Pods need to EXIST, not to run: every selector row reads labels the Pod carries from
# render time, and the group renders its Pods gated while Kueue has not admitted them.
PODS=""
for _ in $(seq 1 "$POD_ROUNDS"); do
  PODS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | grep -c . || true)"
  [ "${PODS:-0}" -ge 3 ] && break
  sleep 5
done
if [ "${PODS:-0}" -ge 3 ]; then
  record PASS "all three role Pods exist" "${PODS} Pods rendered for the three roles"
else
  record FAIL "all three role Pods exist" \
    "only ${PODS:-0} Pod(s) of the group appeared within $((POD_ROUNDS * 5))s, so the selector rows below have nothing to resolve"
fi

# ---------------------------------------------------------------- the selector instrument

# A Service's selector as one kubectl -l string. The selector is the thing under test, so it is
# read from the Service rather than assumed from the render.
selector_string() { # <svc-name>
  # shellcheck disable=SC2016  # the dollar names are kubectl jsonpath map variables, and must not expand
  kubectl -n "$NS" get svc "$1" -o go-template='{{range $k,$v := .spec.selector}}{{$k}}={{$v}},{{end}}' 2>/dev/null \
    | sed 's/,$//'
}

# Which roles' Pods one selector resolves to, sorted and space-joined: the answer a caller's
# connection would land on, read off the live Pods rather than off the selector's spelling.
selected_components() { # <selector-string>
  [ -n "$1" ] || return 0
  kubectl -n "$NS" get pods -l "$1" \
    -o jsonpath='{range .items[*]}{.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' 2>/dev/null \
    | sort -u | tr '\n' ' '
}

# One row per per-role Service: the Service exists AND its own selector resolves to that role's
# Pods alone. An existence-only row would pass a selector that fronted every role.
role_service_fronts_only() { # <svc-name> <component> <row-title>
  local sel comps
  if [ -z "$(kubectl -n "$NS" get svc "$1" -o name 2>/dev/null)" ]; then
    record FAIL "$3" "no svc/$1 exists"
    return 0
  fi
  sel="$(selector_string "$1")"
  comps="$(selected_components "$sel")"
  case "$comps" in
    "$2 ")
      record PASS "$3" "svc/$1 selects exactly the ${2} Pod(s)"
      ;;
    *)
      record FAIL "$3" "svc/$1 exists but its selector resolves to [${comps:-no Pod}] rather than only '${2}'"
      ;;
  esac
  return 0
}

role_service_fronts_only "${MD}-encode" encode "the encoder's own Service exists and fronts only the encoder"
role_service_fronts_only "${MD}-prefill" prefill "the prefiller's own Service exists and fronts only the prefiller"
role_service_fronts_only "${MD}-decode" decode "the decoder's own Service exists and fronts only the decoder"

# ---------------------------------------------------------------- the front door

# The deployment-wide Service carries the deployment's own name and must front the first role that
# is not an encoder — with the encoder listed FIRST, the sharpest position for the rule.
if [ -z "$(kubectl -n "$NS" get svc "$MD" -o name 2>/dev/null)" ]; then
  record FAIL "the deployment-wide Service fronts the prefiller, never the encoder" \
    "no svc/${MD} exists"
else
  FRONT_COMPS="$(selected_components "$(selector_string "$MD")")"
  case "$FRONT_COMPS" in
    *encode*)
      record FAIL "the deployment-wide Service fronts the prefiller, never the encoder" \
        "svc/${MD} resolves to [${FRONT_COMPS}], which includes the encoder although encode is first in the roles list"
      ;;
    *prefill*)
      record PASS "the deployment-wide Service fronts the prefiller, never the encoder" \
        "svc/${MD} resolves to [${FRONT_COMPS}] with the encode role listed first"
      ;;
    *)
      record FAIL "the deployment-wide Service fronts the prefiller, never the encoder" \
        "svc/${MD} resolves to [${FRONT_COMPS:-no Pod}], which is neither the prefiller nor a front door this case recognizes"
      ;;
  esac
fi

# ---------------------------------------------------------------- the status view

# The summary's letters are ordered by kind, not by the roles' own order, so an encoder-first
# deployment and an encoder-last one render the same string. The router count leads it because a
# router is declared; whether the router image pulls on this cluster is not this case's subject,
# so the count itself is left open.
#
# BOTH STATUS ROWS POLL. They are built from OBSERVED Pods, and the pass that creates the Pods is
# not the pass that publishes the roles, so a single read can land in the window between the two and
# see the empty summary and NoRoleStatuses of a cluster that is merely still catching up. A cluster
# this eventually consistent is not read once.
RS=""
for _ in $(seq 1 "$STATUS_ROUNDS"); do
  RS="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o jsonpath='{.status.roleSummary}' 2>/dev/null)"
  printf '%s' "$RS" | command grep -Eq '^[0-9]+R[0-9]+E[0-9]+P[0-9]+D$' && break
  sleep 5
done
if printf '%s' "$RS" | command grep -Eq '^[0-9]+R[0-9]+E[0-9]+P[0-9]+D$'; then
  record PASS "roleSummary reports the kinds in the fixed order E, P, D" \
    "'${RS}' — the counts are zero while the pause placeholders fail the engine probes, and the letters' order is what the row pins"
else
  record FAIL "roleSummary reports the kinds in the fixed order E, P, D" \
    "roleSummary read '${RS:-<unset>}' and never settled within $((STATUS_ROUNDS * 5))s, which is not <router>R<encode>E<prefill>P<decode>D"
fi

# RoleKindsReady must ACCOUNT the roles — including the encoder — rather than report that no role
# was ever observed. KindsNotReady is the expected verdict for placeholders that never pass the
# engine probes; AllKindsReady would mean the image was overridden by something that serves. Both
# prove the accounting; NoRoleStatuses proves its absence, and it is also what an unobserved role
# set reads, so this row polls for the same reason the summary row above does.
RKR=""
for _ in $(seq 1 "$STATUS_ROUNDS"); do
  RKR="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" \
    -o jsonpath='{.status.conditions[?(@.type=="RoleKindsReady")].reason}' 2>/dev/null)"
  case "$RKR" in
    KindsNotReady | AllKindsReady) break ;;
  esac
  sleep 5
done
case "$RKR" in
  KindsNotReady | AllKindsReady)
    record PASS "RoleKindsReady accounts the encoder role" \
      "reason=${RKR}: the roles were accounted; only KindsNotReady-vs-AllKindsReady depends on what the placeholders do"
    ;;
  NoRoleStatuses)
    record FAIL "RoleKindsReady accounts the encoder role" \
      "reason=NoRoleStatuses after $((STATUS_ROUNDS * 5))s: no role was accounted at all, so the encoder cannot be in the kind view"
    ;;
  *)
    record FAIL "RoleKindsReady accounts the encoder role" \
      "reason='${RKR:-<no condition>}' after $((STATUS_ROUNDS * 5))s"
    ;;
esac

# The published endpoint is the front door as a string. For a routed deployment it is derived from
# the router's objects, and on a cluster that cannot pull the router image it may never publish —
# which is a NO-READ rather than a FAIL: the router image is deliberately not this case's subject
# (case 71 isolates it). An endpoint that names the encoder role, on any cluster, is a FAIL.
EP="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o jsonpath='{.status.endpoint}' 2>/dev/null)"
if [ -z "$EP" ]; then
  record NO-READ "status.endpoint never names the encoder" \
    "no endpoint published (the router is not ready on this cluster); the selector row above carries the front-door rule for this run"
else
  case "$EP" in
    *encode*)
      record FAIL "status.endpoint never names the encoder" "endpoint '${EP}' names the encoder role"
      ;;
    *)
      record PASS "status.endpoint never names the encoder" "endpoint '${EP}'"
      ;;
  esac
fi

# The v1 view's kubectl table must carry the same Endpoint column the v1alpha1 CRD declares, so
# `kubectl get md` prints the front-door address alongside Engine, Roles and Phase.
HDR="$(kubectl -n "$NS" get modeldeployments.v1.worker.gpustack.ai "$MD" 2>/dev/null | head -n 1)"
case "$HDR" in
  *ENGINE*ROLES*PHASE*ENDPOINT*)
    record PASS "v1 table carries the Endpoint column" "header: ${HDR}"
    ;;
  *)
    record FAIL "v1 table carries the Endpoint column" "header missing ENDPOINT: ${HDR:-<empty>}"
    ;;
esac

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-117] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-117] all checks passed"
