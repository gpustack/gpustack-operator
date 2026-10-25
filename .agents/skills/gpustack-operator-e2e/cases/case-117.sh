#!/usr/bin/env bash
#
# CASE 117 — An encoder deployment renders one Service per role, a router picker document that
#           carries the encoder stage, and one shared claim under the encoder and the prefiller
#   (MUTATING, self-recovering)
#   AUTO-SKIPS without an InstanceType that reports an admitted accelerator platform, which this
#   case builds by injecting a CPU vendor label; its witness row additionally skips, naming the
#   image, when the witness image cannot be pulled.
#
#   case-117.sh <NS>
#
# Prerequisites: nodes=1; gpu=none; cluster=any; inputs=E2E_C117_PODS,E2E_C117_SETTLE,E2E_C117_STATUS,E2E_C117_VENDOR,E2E_C117_WITNESS_IMAGE,E2E_MD_IMAGE,E2E_SYSTEM_NS
# Goal:        THREE READINGS OF ONE RENDER, all on the same three-role deployment. The operator
#              answers three different questions about one encoder deployment, and each answer has
#              a failure that is quiet, so the case reads all three.
#
#              THE ADDRESSES. The encoder answers no request of its own: it turns media into
#              embeddings for a prefiller, so a front door pointed at one sends traffic to a
#              process that serves none of it — with endpoints that resolve and answer health
#              checks, which is what makes that failure quiet. So the case asserts the three
#              per-role Services by NAME, each selecting only its own role's Pods (the encoder's
#              may select none once the operator withdraws a placeholder that never becomes
#              Ready), and the deployment-wide Service selecting the prefiller while the encoder
#              sits FIRST in the roles list — the position where a "front the first role"
#              implementation would pick it.
#
#              THE SELECTOR IS THE OBSERVABLE, not the Service's existence alone. A Service whose
#              selector matched every role of the deployment would exist under the same name and
#              pass an existence check, so each row resolves the Service's own selector against the
#              live Pods and reads back which role's Pods it fronts.
#
#              THE PICKER DOCUMENT. llm-d-router is the only router the admitted rows name, so
#              what its picker document says about an encoder is the part of the router that
#              carries this whole feature. The document must carry an ENCODE FILTER (a
#              label-selector-filter selecting role-kind=encode), an ENCODE PROFILE that names that
#              filter and NOTHING ELSE — no queue scorer, no KV-cache scorer, no prefix scorer,
#              because an encoder keeps no KV cache: the scorers read gauges it never exports and
#              the prefix scorer asks about token overlap that does not exist before a request has
#              tokens — and the MULTIMODAL DECIDER that the disagg-profile-handler routes BOTH
#              "prefill" and "encode" at. A control deployment with no encoder role renders none of
#              the three, which is what makes them the encoder's facts and not the router's
#              defaults.
#
#              THE SHARED CLAIM. On the platform whose encoder leg runs through a mounted
#              directory rather than the network, the encoder writes its embeddings into a claim
#              and the prefiller reads them out of the same one. Reading only the rendered specs
#              proves the operator points both roles at one claim name; it cannot tell a claim
#              that really shares bytes from two claims sharing a name. So the rows assert the
#              volumes and mounts the two role Pods render, and then a pair of case-owned witness
#              Pods — pinned to one node, mounting the same claim — writes through one mount and
#              reads back through the other.
#
# Environment: Any cluster with a materialized scheduling chain (run case-1 first) and at least one
#              Ready, schedulable worker node that is not a control-plane node. NO accelerator: the
#              encoder shape reaches admission through the platform an injected CPU vendor label
#              builds ("Ascend" — the admitted row whose connector is a mounted directory rather
#              than a network leg). The gpustack-settings Secret must exist in the system namespace
#              (E2E_SYSTEM_NS, default gpustack-system). The case creates its own PV and PVC (a
#              hostPath volume under /tmp on the labeled node, manually bound), so no StorageClass
#              is needed. The runner needs jq and pyyaml to read the router document.
#
#              AUTO-SKIP: the subject needs the encoder shape ADMITTED, which needs a platform
#              InstanceType. If the vendor label does not take (NFD owns cpu-model.vendor_id and
#              may decline an injected override) or the pool never derives, the case prints SKIP
#              rows naming that and exits 0. It also skips, naming the queue, when the pool's
#              entrance LocalQueue is missing from the namespace: nothing is admitted and no
#              document is ever rendered. The witness row additionally skips, naming the image,
#              when the witness image cannot be pulled — a container nobody could start is a
#              reading nobody took, not a verdict about the operator.
#
#              Existing platform: if an InstanceType already reports the manufacturer named by
#              E2E_C117_VENDOR (an accelerator cluster, for example E2E_C117_VENDOR=Ascend), the
#              case uses it and injects nothing, so the label, the setting and the settle wait
#              are skipped.
#
# Inputs:      MOCKED: one node label (the CPU vendor) injected through a NodeFeature object and
#              one settings key (CPU-manufacturer awareness) flipped on — both restored by the
#              trap. REAL: one PV, one PVC, one three-role ModelDeployment (encode first, engine
#              vLLM, router llm-d-router, one replica per role on the platform type, the encode and
#              prefill roles mounting the claim at one path), the control deployment the same
#              router with no encoder role, and two one-shot witness Pods mounting the same claim.
#
# Expected:    - the deployment is admitted and created, and its three role Pods exist (gated
#                counts, they never need to run);
#              - Services <deployment>-encode, <deployment>-prefill and <deployment>-decode exist,
#                each selecting exactly its own role's Pod and nobody else's;
#              - the deployment-wide Service exists and selects the prefill Pod, NOT the encode
#                Pod, with the encode role first in the list;
#              - status.roleSummary carries the ready counts in the fixed order E, P, D, and the
#                RoleKindsReady condition accounts the roles rather than reporting NoRoleStatuses;
#              - status.endpoint, when published, does not name the encoder, and the v1 table
#                carries the Endpoint column;
#              - the router's ConfigMap carries an EndpointPickerConfig whose encode filter selects
#                role-kind=encode, whose encode profile names that filter alone, whose multimodal
#                decider the handler routes both prefill and encode at, and whose prefix-cache
#                producer excludes the kinds that publish no events — while a deployment with no
#                encoder role renders none of that;
#              - the encoder Pod renders a volume naming the claim at the shared path and NOT
#                read-only, the prefiller Pod renders the same claim at the same path, and the
#                decoder Pod renders no volume naming the claim;
#              - the PVC reaches Bound against the case's own PV, and a file written through the
#                claim by one witness Pod is read back byte-for-byte by the other, proving one
#                filesystem behind both mounts.
#
# Cleanup:     The trap deletes the witness Pods and both deployments, releases any Workload still
#              holding the replicas (Kueue keeps a finalizer on every Pod of a serving group),
#              deletes the PVC and the PV, deletes the NodeFeature object, restores the settings
#              key to its exact original value and waits for the chain to settle back: the
#              derived type goes only once the setting cache flips and the pool re-derives.
#              Idempotent, runs on pass AND fail, safe to re-run.
#
set -uo pipefail

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

NS="${1:?usage: case-117.sh <NS>}"
SYSTEM_NS="${E2E_SYSTEM_NS:-gpustack-system}"
VENDOR="${E2E_C117_VENDOR:-Ascend}"
PLATFORM="$(printf '%s' "$VENDOR" | tr '[:upper:]' '[:lower:]')"
NF=case117-cpu-vendor
MD=case117-epd
PD=case117-pd
CLAIM=case117-claim
PV=case117-pv
IMAGE="${E2E_MD_IMAGE:-registry.k8s.io/pause:3.10}"
WITNESS_IMAGE="${E2E_C117_WITNESS_IMAGE:-busybox:1.36}"
MOUNT=/ec-cache
SETTLE_ROUNDS="${E2E_C117_SETTLE:-60}"   # 60 x 5s for the settings cache and re-derivation
POD_ROUNDS="${E2E_C117_PODS:-40}"        # 40 x 5s for the group to render its Pods
STATUS_ROUNDS="${E2E_C117_STATUS:-12}"   # 12 x 5s for the observed roles to reach the status

# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_epd-lib.sh"

# The picker document is YAML, so the assertions on the converted document need jq and a YAML
# reader. jq is a runner requirement the whole suite shares. A missing YAML reader instead
# degrades only the picker-document section: the platform, deployment, Service, status and mount
# rows need no YAML parse, so they still run - the section SKIPs its rows loudly instead of
# taking the whole case down (the base case ran its full surface without this dependency; the
# dependency arrived with the absorbed picker checks and must not widen past them).
command -v jq >/dev/null 2>&1 || { echo "[case-117] jq is required to read the router document." >&2; exit 2; }
PICKER_YAML_OK=yes
python3 -c 'import yaml' >/dev/null 2>&1 || {
  echo "[case-117] pyyaml is absent: the picker-document rows will SKIP." >&2
  PICKER_YAML_OK=no
}

# One Ready, schedulable worker node (not a control-plane one): the vendor label lands there and
# that node's CPU becomes the pool whose manufacturer the case controls.
#
# The InstanceType reporting the injected platform, the label that builds it, the setting it turns
# on, the type it waits for and the undo of all three are the library's: epd_pick_worker_node,
# epd_platform_type and epd_setup_cpu_vendor_fixture.

# One list call, because this runs from the EXIT trap: Kueue holds a finalizer on every Pod of a
# serving group and releases it only when the Workload is deleted, so a cleanup that blocks on the
# deployment would block for as long as the caller allows (case-50's mechanic).
force_release() { # <modeldeployment>
  local md="$1" row wl uids owners u
  uids="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" \
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

cleanup() {
  echo
  echo "[case-117] cleanup"
  epd_teardown_cpu_vendor_fixture "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" cleanup_deployments
}
trap cleanup EXIT

# The deployments and the claim this run made, removed while the platform they were admitted on
# still exists. The fixture teardown runs after this, so the type is still there while the
# Workloads are released.
cleanup_deployments() {
  kubectl -n "$NS" delete pod case117-witness-write case117-witness-read \
    --ignore-not-found --force --grace-period=0 >/dev/null 2>&1
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
}

# ---------------------------------------------------------------- the shared fixture

# The platform this deployment is admitted on: an InstanceType reporting the injected vendor, or
# one the cluster already carried. It is the ONE platform all three readings below share.
IT="$(epd_platform_type "$PLATFORM")"
if [ -z "$IT" ]; then
  NODE="$(epd_pick_worker_node)"
  if [ -z "$NODE" ]; then
    echo "[case-117] no Ready schedulable worker node; this case cannot place its vendor label" >&2
    exit 2
  fi

  echo "[case-117] injecting CPU vendor '${VENDOR}' on node ${NODE} and turning CPU-manufacturer awareness on"

  # The poll IS the wait for the settings cache (30s), the node flavor re-derivation and the type
  # authoring. A timeout is a skip, not a failure: NFD owns cpu-model.vendor_id and may decline an
  # injected override, which is an environment answer this case reports rather than argues with.
  epd_setup_cpu_vendor_fixture "$VENDOR" "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE"
  IT="$EPD_FIXTURE_TYPE"
fi

if [ -z "$IT" ]; then
  # No platform means no admitted shape, so every reading below has nothing to read. Each subject
  # says so in its own words, and the run ends here.
  record SKIP "an encoder deployment exists with one Service per role" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  record SKIP "the shared-claim encoder shape exists" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  record SKIP "the router picker carries the encoder stage" \
    "no InstanceType reported manufacturer '${PLATFORM}' within $((SETTLE_ROUNDS * 5))s, so the encoder shape cannot be admitted on this run: NFD did not keep the injected cpu-model.vendor_id, or the pool never derived"
  print_rows
  echo "[case-117] skipped: no platform InstanceType"
  exit 0
fi

# Where the platform came from, which the platform row below states and the sections repeat.
if [ "$EPD_FIXTURE_INJECTED" = true ]; then
  ORIGIN="the injected vendor label"
else
  ORIGIN="an InstanceType the cluster already carried"
fi

# The witness pair needs one node to share the claim on. When the case labeled a node itself,
# that node is the pool's only node and the one the role Pods land on; otherwise the first node of
# the cluster is taken.
if [ -z "${NODE:-}" ]; then
  NODE="$(kubectl get nodes -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null | head -n 1)"
fi

# The type's entrance LocalQueue in this namespace is what routes the group into its queue; the
# operator materializes it in every non-reserved namespace once the queue exists, so this poll is
# part of the fixture rather than a subject.
ENTRANCE="$(kubectl get instancetypes.worker.gpustack.ai "$IT" -o jsonpath='{.status.entrance}' 2>/dev/null)"
ENTRANCE_LQ=""
for _ in $(seq 1 "$SETTLE_ROUNDS"); do
  ENTRANCE_LQ="$(kubectl -n "$NS" get localqueue "${ENTRANCE:-}" -o name 2>/dev/null)"
  [ -n "$ENTRANCE_LQ" ] && break
  sleep 5
done

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' from ${ORIGIN}; entrance ${ENTRANCE}"

if [ -z "$ENTRANCE_LQ" ]; then
  # Without the entrance queue nothing is admitted, no role Pod is created and no router document
  # is ever rendered, so every reading below would report the same absence three times.
  record SKIP "the pool's entrance LocalQueue exists" \
    "instancetype ${IT} names entrance '${ENTRANCE:-<none>}' and no localqueue/${ENTRANCE:-<none>} answered in ${NS} within $((SETTLE_ROUNDS * 5))s, so nothing is admitted, no role Pod is created and no router document is ever rendered"
  print_rows
  echo "[case-117] skipped: no entrance LocalQueue"
  exit 0
fi

record PASS "the pool's entrance LocalQueue exists" "${ENTRANCE_LQ##*/} in ${NS}"

# The claim the two encoder-side roles mount: a hostPath volume under /tmp on the labeled node,
# manually bound. The pool the labeled node feeds is the only pool the deployment names, so every
# Pod that mounts this claim lands on that node and ReadWriteOnce is enough; DirectoryOrCreate
# spares the case any need to reach the node's shell. A storage class is deliberately not used:
# binding by volumeName needs none, and the default class of an arbitrary cluster is not this
# case's to depend on.
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
    path: /tmp/gpustack-case-117
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

# ---------------------------------------------------------------- the shared subject

# The encoder is FIRST in the list on purpose: the deployment-wide Service fronts the first role
# that is not an encoder, and first-position is where an implementation that forgot the rule would
# point the front door at the encoder. The encode and prefill roles mount the shared claim at the
# same path, because the platform whose encoder leg runs through a mounted directory is admitted
# only when both roles mount one claim; the decode role mounts nothing, because that leg ends at
# the prefiller.
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

# The gate is that the object is THERE, not that the apply printed a nice word: a refusal surfaces
# here as one message instead of nine timeouts (case-49's lesson).
DEPLOYED=no
if kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o name >/dev/null 2>&1; then
  DEPLOYED=yes
fi

# The three Pods need to EXIST, not to run: every selector row reads labels the Pod carries from
# render time, and the group renders its Pods gated while Kueue has not admitted them. A refused
# apply is not waited on — there is nothing that would arrive.
PODS=0
if [ "$DEPLOYED" = yes ]; then
  for _ in $(seq 1 "$POD_ROUNDS"); do
    PODS="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${MD}" --no-headers 2>/dev/null | grep -c . || true)"
    [ "${PODS:-0}" -ge 3 ] && break
    sleep 5
  done
fi

# ---------------------------------------------------------------- the addresses

# The role Services, the front door and the status view: what a caller's connection lands on.

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' from the node labels; entrance ${ENTRANCE}"

if [ "$DEPLOYED" = yes ]; then
  record PASS "the encoder deployment is admitted and created" \
    "one encode, one prefill and one decode role on ${IT}, llm-d-router, image ${IMAGE}"
else
  record FAIL "the encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  print_rows
  exit 1
fi

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

# Whether a selector string carries one label equality. The terms are matched whole, because a
# substring of the whole string would also match a key that merely ends in the one asked about.
selector_has() { # <selector-string> <key> <value>
  case ",$1," in
    *",$2=$3,"*) return 0 ;;
  esac
  return 1
}

# Whether one selector can reach anything but the given role, answered from the Service's own
# selector and the live Pods. It answers 0 when only that role is reachable and leaves the reason a
# caller reports in SELECT_REASON.
#
# AN EMPTY SELECTED SET IS AN ANSWER, NOT A FAILURE, WHEN THE SELECTOR DEMANDS ELIGIBILITY. That is
# designed withdrawal: the operator adds the eligibility equality to the Service of a role whose
# members the reconciler has not qualified, and a placeholder that never becomes Ready matches
# nothing once that is decided. Reading the term rather than the role is what keeps a row true on
# both sides of the decision instead of racing it. A selector demanding no eligibility must still
# select its own role's Pods, so an empty set is a failure there.
selector_fronts_only() { # <selector-string> <component>
  local sel="$1" comp="$2" comps
  SELECT_REASON=""
  if [ -z "$sel" ]; then
    SELECT_REASON="carries no selector at all, so it fronts every Pod in the namespace"
    return 1
  fi
  # The identity terms, read off the Service rather than assumed from the render: a selector that
  # did not pin the role and this deployment would still resolve, and would still resolve wrongly.
  if ! selector_has "$sel" app.kubernetes.io/component "$comp"; then
    SELECT_REASON="requires no component=${comp}, so nothing keeps it off another role"
    return 1
  fi
  if ! selector_has "$sel" app.kubernetes.io/instance "$MD" ||
    ! selector_has "$sel" app.kubernetes.io/name model-deployment; then
    SELECT_REASON="does not pin itself to this deployment with the instance and name terms"
    return 1
  fi
  comps="$(selected_components "$sel")"
  case "$comps" in
    "$comp ")
      SELECT_REASON="resolves to the ${comp} Pod(s) alone"
      return 0
      ;;
    "")
      if selector_has "$sel" modeldeployment.gpustack.ai/endpoint-eligible true; then
        SELECT_REASON="resolves to no Pod, which is the designed withdrawal of a placeholder that never becomes Ready"
        return 0
      fi
      SELECT_REASON="resolves to no Pod and demands no eligibility that would explain it"
      return 1
      ;;
    *)
      SELECT_REASON="resolves to [${comps}], which includes a Pod that is not the ${comp} role"
      return 1
      ;;
  esac
}

# One row per per-role Service: the Service exists AND its own selector can reach that role's Pods
# and nothing else. An existence-only row would pass a selector that fronted every role.
role_service_fronts_only() { # <svc-name> <component> <row-title>
  if [ -z "$(kubectl -n "$NS" get svc "$1" -o name 2>/dev/null)" ]; then
    record FAIL "$3" "no svc/$1 exists"
    return 0
  fi
  if selector_fronts_only "$(selector_string "$1")" "$2"; then
    record PASS "$3" "svc/$1 ${SELECT_REASON}"
  else
    record FAIL "$3" "svc/$1 exists but its selector ${SELECT_REASON}"
  fi
  return 0
}

role_service_fronts_only "${MD}-encode" encode "the encoder's own Service exists and fronts only the encoder"
role_service_fronts_only "${MD}-prefill" prefill "the prefiller's own Service exists and fronts only the prefiller"
role_service_fronts_only "${MD}-decode" decode "the decoder's own Service exists and fronts only the decoder"

# A check that cannot fail proves nothing, so the same one is fed selectors that reach another role.
# Each row below passes only when the check refuses the selector it is given, so the rows above
# passing means the check told the roles apart rather than accepting whatever it was handed.
#
# The first is a real one: the prefiller's own selector, offered as the encoder's. It is refused for
# naming no encoder term at all.
if selector_fronts_only "$(selector_string "${MD}-prefill")" encode; then
  record FAIL "the per-role Service check refuses a selector that reaches another role" \
    "the prefiller's own selector was accepted as the encoder's, so the rows above cannot tell the roles apart"
else
  record PASS "the per-role Service check refuses a selector that reaches another role" \
    "the prefiller's selector was refused as the encoder's: it ${SELECT_REASON}"
fi

# The second guards the one rule this check relaxed. An empty selected set is accepted only when the
# selector demands eligibility. This selector names the encoder, this deployment and the engine, and
# adds a term no Pod carries, so it resolves to nothing and demands no eligibility that would explain
# it. A check that accepted it would accept any broken selector as withdrawal.
#
# A selector that names the encoder cannot also reach another role: label terms are a conjunction,
# so component=encode already excludes every other component. That is why the control is built on the
# empty set and not on a foreign Pod.
DANGLING="app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=${MD},app.kubernetes.io/component=encode,case117.gpustack.ai/no-such-label=true"
if selector_fronts_only "$DANGLING" encode; then
  record FAIL "the per-role Service check refuses an empty set that nothing explains" \
    "a selector resolving to no Pod and demanding no eligibility was accepted as designed withdrawal"
else
  record PASS "the per-role Service check refuses an empty set that nothing explains" \
    "a selector resolving to no Pod without an eligibility term was refused: it ${SELECT_REASON}"
fi

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
# (case 70 isolates it). An endpoint that names the encoder role, on any cluster, is a FAIL.
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

# ---------------------------------------------------------------- the router picker document

# What the router is TOLD about the encoder. The rows read the document the operator renders into
# the router Pod, so an unpullable router image does not skip them: the ConfigMap exists before
# any image is pulled.

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}' from ${ORIGIN}"

# The refusal itself is reported by the addresses section above, which stops the run on it; this
# row states the same admission in the words of this subject.
if [ "$DEPLOYED" = yes ]; then
  record PASS "the encoder deployment is admitted and created" \
    "one encode, one prefill and one decode role on ${IT}, llm-d-router"
else
  record FAIL "the encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
fi

# The CONTROL deployment is the same router with no encoder at all. Reading the same document twice
# is what makes the encoder's facts encoder facts rather than prefill/decode facts that happen to
# be there: every term the control lacks is a term the encoder added.
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
    image: ${IMAGE}
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
  if [ "$md" = "$MD" ]; then
    # The shared fixture already waited for this deployment's three role Pods.
    count="$PODS"
  else
    count=""
    for _ in $(seq 1 "$POD_ROUNDS"); do
      count="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/instance=${md}" --no-headers 2>/dev/null | grep -c . || true)"
      [ "${count:-0}" -ge "$want" ] && break
      sleep 5
    done
  fi
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

# The document as YAML, from the ConfigMap the operator renders into the router Pod. A router
# configured by argv renders none, so its ABSENCE is a distinct answer from an empty one and both
# are read rather than assumed.
picker_yaml() { # <modeldeployment>
  kubectl -n "$NS" get configmap "$1-router" -o jsonpath='{.data.epp\.yaml}' 2>/dev/null
}

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

if [ "$PICKER_YAML_OK" != yes ]; then
  record SKIP "the router ConfigMap carries an EndpointPickerConfig" \
    "pyyaml is absent in the runner, so the rendered document cannot be read here"
else
DOC="$(picker_yaml "$MD")"
if [ -z "$DOC" ]; then
  record FAIL "the router ConfigMap carries an EndpointPickerConfig" \
    "no configmap/${MD}-router carries a key epp.yaml, so there is no document to read"
else
  DOC_JSON="$(printf '%s' "$DOC" | json_of)" || {
    echo "[case-117] cannot read the router document: pyyaml is required in the runner." >&2
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

  # The encode filter: one label-selector-filter whose matchLabels select role-kind=encode. The
  # label key is the role-kind label the operator stamps on every Pod, so this is the selector the
  # router will actually match against, read off the document rather than assumed from the render.
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

  # THE ENCODE PROFILE'S PLUGIN LIST, asserted exactly. Upstream completes any profile naming none
  # with its own max-score-picker at load time, so [encode-pods] is a valid, runnable profile — and
  # any additional plugin is a scorer that cannot separate encoder candidates.
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
    if [ "$CTRL_DECIDER" -eq 0 ] && [ "$CTRL_PROFILE" -eq 0 ] && [ "$CTRL_FILTER" -eq 0 ]; then
      record PASS "a deployment with no encoder role renders no encoder stage" \
        "the control's picker carries no multimodal decider, no encode profile and no encode filter, so all three are the encoder's and not the router's defaults"
    else
      record FAIL "a deployment with no encoder role renders no encoder stage" \
        "the control still carries decider=${CTRL_DECIDER} profile=${CTRL_PROFILE} filter=${CTRL_FILTER}, which would mean the encoder rows above prove nothing about the encoder"
    fi
  fi
fi
fi  # PICKER_YAML_OK: the picker section is closed here; its SKIP shape sits above

# ---------------------------------------------------------------- the shared claim mounts

# The claim one role's FIRST Pod mounts, as "claimName|mountPath|readOnly", or "" when it mounts no
# claim. Pending Pods count: the render is what these rows pin, and a gated Pod carries its full
# spec (case-89's reading posture). The reading itself is the library's: epd_role_pod_claim.

record PASS "a CPU-only pool reports an admitted platform" \
  "instancetype ${IT} reports manufacturer '${PLATFORM}'; entrance ${ENTRANCE}"

# The refusal itself is reported by the addresses section above, which stops the run on it; this
# row states the same admission in the words of this subject.
if [ "$DEPLOYED" = yes ]; then
  record PASS "the shared-claim encoder deployment is admitted and created" \
    "encode and prefill mount ${CLAIM} at ${MOUNT} on ${IT}"
else
  record FAIL "the shared-claim encoder deployment is admitted and created" \
    "no modeldeployment/${MD} after the apply: $(printf '%s' "${APPLY_OUT:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
fi

if [ "${PODS:-0}" -ge 3 ]; then
  record PASS "all three role Pods exist" "${PODS} Pods rendered; the mount rows below read specs, not running containers"
else
  record FAIL "all three role Pods exist" \
    "only ${PODS:-0} Pod(s) of the group appeared within $((POD_ROUNDS * 5))s, so the mount rows below have nothing to read"
fi

ENCODE_CLAIM="$(epd_role_pod_claim "$NS" "$MD" encode)"
PREFILL_CLAIM="$(epd_role_pod_claim "$NS" "$MD" prefill)"
DECODE_CLAIM="$(epd_role_pod_claim "$NS" "$MD" decode)"

case "$ENCODE_CLAIM" in
  "${CLAIM}|${MOUNT}|false")
    record PASS "the encoder Pod mounts the claim at the shared path, writable" \
    "rendered ${ENCODE_CLAIM}"
    ;;
  *)
    record FAIL "the encoder Pod mounts the claim at the shared path, writable" \
      "rendered '${ENCODE_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|false'"
    ;;
esac

case "$PREFILL_CLAIM" in
  "${CLAIM}|${MOUNT}|"*)

    # Read-only is the PREFILLER's own choice — it only reads — so the row pins the claim name and
    # the path and leaves the flag open, unlike the encoder's row above.
    record PASS "the prefiller Pod mounts the same claim at the same path" \
      "rendered ${PREFILL_CLAIM}, matching the encoder's claim and path"
    ;;
  *)
    record FAIL "the prefiller Pod mounts the same claim at the same path" \
      "rendered '${PREFILL_CLAIM:-no claim mount}', expected '${CLAIM}|${MOUNT}|<any readOnly>'"
    ;;
esac

if [ -z "$DECODE_CLAIM" ]; then
  record PASS "the decoder Pod mounts no claim" \
    "the mounted-directory leg ends at the prefiller, and the decode role declares no volume"
else
  record FAIL "the decoder Pod mounts no claim" "rendered ${DECODE_CLAIM}"
fi

# ---------------------------------------------------------------- the claim is one filesystem

PVC_PHASE="$(kubectl -n "$NS" get pvc "$CLAIM" -o jsonpath='{.status.phase}' 2>/dev/null)"
if [ "$PVC_PHASE" = Bound ]; then
  record PASS "the claim is Bound" "pvc/${CLAIM} bound to pv/${PV}"
else
  record FAIL "the claim is Bound" "pvc/${CLAIM} phase='${PVC_PHASE:-<none>}', so no witness can mount it"
fi

# Two one-shot witnesses pinned to the labeled node: the pool's only node, which is also where the
# hostPath lives. The writer records a token through its mount; the reader cats the same path. That
# round-trip is the reading that separates one shared filesystem from two claims sharing a name.
#
# THE WRITER IS CREATED FIRST AND THE READER ONLY AFTER IT HAS RUN, because the reader cats a file
# that does not exist until the writer has written it: created together, whichever the kubelet starts
# first decides the verdict, and on a cluster that already holds the image the reader usually loses,
# failing a claim that shares perfectly well. Nothing in the manifest, the node or the loop orders
# them, so the case orders them.
#
# This is the LAST reading of the case, so an unpullable witness image skips this row alone and
# leaves every row above it standing.
if [ "$PVC_PHASE" = Bound ]; then
  TOKEN="case117-$(date +%s)"
  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: case117-witness-write
  namespace: ${NS}
spec:
  restartPolicy: Never
  tolerations:
    - operator: Exists
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  containers:
    - name: write
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "printf '%s' '${TOKEN}' > /shared/written-by-the-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${CLAIM}
YAML

  # An image that cannot be pulled is a SKIP naming the image (case-81's rule): a container nobody
  # could start is a reading nobody took, not a verdict about the claim. The reason a witness Pod
  # is waiting on is read by name, because it carries none of the deployment's own labels.
  W_ROUNDS=$((POD_ROUNDS))
  W_OK=no
  for _ in $(seq 1 "$W_ROUNDS"); do
    case "$(kubectl -n "$NS" get pod case117-witness-write -o jsonpath='{.status.phase}' 2>/dev/null)" in
      Succeeded) W_OK=yes; break ;;
      Failed) break ;;
    esac
    case "$(epd_pod_pull_reason "$NS" case117-witness-write)" in ImagePullBackOff | ErrImagePull) break ;; esac
    sleep 5
  done

  if [ "$W_OK" != yes ]; then
    case "$(epd_pod_pull_reason "$NS" case117-witness-write)" in
      ImagePullBackOff | ErrImagePull)
        record SKIP "a file written through one mount is read back through the other" \
          "the witness image '${WITNESS_IMAGE}' could not be pulled ($(epd_pod_pull_reason "$NS" case117-witness-write)), so no reading was taken; the render rows above stand on their own"
        ;;
      *)
        record FAIL "a file written through one mount is read back through the other" \
          "the writer witness never completed (phase '$(kubectl -n "$NS" get pod case117-witness-write -o jsonpath='{.status.phase}' 2>/dev/null)')"
        ;;
    esac
  else
    # The file exists now, so the reader is safe to start.
    kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: v1
kind: Pod
metadata:
  name: case117-witness-read
  namespace: ${NS}
spec:
  restartPolicy: Never
  tolerations:
    - operator: Exists
  nodeSelector:
    kubernetes.io/hostname: ${NODE}
  containers:
    - name: read
      image: ${WITNESS_IMAGE}
      command: ["sh", "-c", "cat /shared/written-by-the-other-mount"]
      volumeMounts:
        - name: shared
          mountPath: /shared
  volumes:
    - name: shared
      persistentVolumeClaim:
        claimName: ${CLAIM}
YAML

    R_OK=no
    for _ in $(seq 1 "$W_ROUNDS"); do
      case "$(kubectl -n "$NS" get pod case117-witness-read -o jsonpath='{.status.phase}' 2>/dev/null)" in
        Succeeded | Failed) R_OK=yes; break ;;
      esac
      case "$(epd_pod_pull_reason "$NS" case117-witness-read)" in ImagePullBackOff | ErrImagePull) break ;; esac
      sleep 5
    done

    READ_BACK=""
    if [ "$R_OK" = yes ]; then
      READ_BACK="$(kubectl -n "$NS" logs case117-witness-read -c read 2>/dev/null)"
    fi
    if [ "$READ_BACK" = "$TOKEN" ]; then
      record PASS "a file written through one mount is read back through the other" \
        "witness pair exchanged '${READ_BACK}' through pvc/${CLAIM}: one filesystem behind both mounts"
    else
      record FAIL "a file written through one mount is read back through the other" \
        "reader witness returned '${READ_BACK:-<nothing>}', expected '${TOKEN}'"
    fi
  fi
fi

# Results.
print_rows
[ "$FAILS" -eq 0 ] || { echo "[case-117] ${FAILS} check(s) FAILED"; exit 1; }
echo "[case-117] all checks passed"
