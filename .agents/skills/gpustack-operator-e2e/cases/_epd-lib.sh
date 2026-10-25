#!/usr/bin/env bash
#
# _epd-lib.sh — the CPU-vendor fixture a platform case stands on, and the readers a live
#                three-role serving group is inspected through.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Every one of those stays with the case that sources it.
# What it removes is the duplication between the cases that prove the same two things: the fixture
# that manufactures a CPU platform on a cluster that has none, and the inspection of a live
# encoder/prefill/decode group.
#
# A case uses it as:
#
#     set -uo pipefail
#     NS="${1:?usage: ...}"
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_epd-lib.sh"
#
#     # the fixture: a CPU platform when the cluster carries none
#     NODE="$(epd_pick_worker_node)"
#     epd_setup_cpu_vendor_fixture "$VENDOR" "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" "$NODE"
#     IT="$EPD_FIXTURE_TYPE"
#     ...checks, each `record`ing one row...
#     epd_teardown_cpu_vendor_fixture "$NS" "$NF" "$SETTLE_ROUNDS" "$SYSTEM_NS" cleanup_deployments
#
#     # a live serving group
#     epd_poll_role "$NS" "$MD" "$READY_BOUND" encode; rc=$?
#     DOC="$(epd_ec_document "$NS" "$EN_POD")"
#
# ------------------------------------------------------------------------------------------------
# PART ONE: THE CPU-VENDOR FIXTURE
#
# A case that asserts something about a platform needs a platform. On a cluster that carries one,
# the case reads it. On a CPU-only cluster it has to manufacture one: the operator derives an
# InstanceType from the CPU vendor a node reports, so a case can inject that vendor through a
# NodeFeature object and turn on the setting that makes the worker read it.
#
# The fixture is three mutations and their undo: a NodeFeature object carrying the vendor label on
# one node, the CPU-manufacturer awareness setting flipped on, and the InstanceType that follows
# from them. The undo is not a delete of the settings key but a patch of the exact bytes that were
# there, and it is not finished until the type this run derived is gone: the worker reads the
# setting through a thirty-second cache, and the type outlives the patch by that cache plus one
# reconcile. A case that returned while its type was still there would hand the next case in a
# serial run a platform it did not choose.
# ------------------------------------------------------------------------------------------------

# The settings key the operator reads the CPU-manufacturer awareness from. One key serves every
# case that builds the fixture, so it is written once here.
EPD_AWARENESS_KEY=instance-type-aware-cpu-manufacturer

# The fixture's own state. EPD_FIXTURE_INJECTED says whether this run changed anything at all.
# EPD_FIXTURE_ORIG_SETTING holds the bytes the settings Secret carried before, still encoded, so
# the restore can put back exactly what was there. EPD_FIXTURE_TYPES holds the InstanceTypes this
# run derived, and the settle waits for those to go.
EPD_FIXTURE_INJECTED=false
EPD_FIXTURE_ORIG_SETTING=""
EPD_FIXTURE_TYPES=()
# shellcheck disable=SC2034 # Output consumed by the sourcing case.
EPD_FIXTURE_TYPE=""

# epd_pick_worker_node prints the name of one Ready, schedulable worker node (not a control-plane
# one): the vendor label lands there and that node's CPU becomes the pool whose manufacturer the
# case controls.
epd_pick_worker_node() {
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
    # platform-type poll below times out. Skip those nodes, exactly as case-16 reads the
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

# epd_platform_type <manufacturer> prints the InstanceType reporting that manufacturer, skipping
# types on their way out and Inactive ones for the reasons case-45 records: naming either is
# refused at admission for a reason no row here is about.
#
# The manufacturer is read from status, not from spec: the platform of a role is whatever its
# InstanceType reports, which is the same fact the webhook reads. A type reporting nothing is not
# a platform of its own — that is the not-yet-synced state, and the webhook refuses it.
epd_platform_type() { # <manufacturer>
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        [ -n "$manufacturer" ] || continue
        if [ "$manufacturer" = "$1" ]; then
          echo "$name"
          break
        fi
      done
}

# epd_type_exists <type-name> succeeds while the InstanceType is still there.
epd_type_exists() { kubectl get instancetypes.worker.gpustack.ai "$1" >/dev/null 2>&1; }

# epd_any_type_exists <type-name>... prints the first of the named InstanceTypes that is still
# there, and fails when none of them is.
epd_any_type_exists() {
  local t
  for t in "$@"; do
    [ -n "$t" ] || continue
    if epd_type_exists "$t"; then
      printf '%s' "$t"
      return 0
    fi
  done
  return 1
}

# epd_set_awareness <system-ns> <true|false> turns CPU-manufacturer awareness on or off in the
# settings Secret. The value rides the Secret base64-encoded (case-50's mechanic).
epd_set_awareness() { # <system-ns> <true|false>
  kubectl -n "$1" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${EPD_AWARENESS_KEY}\":\"$(printf '%s' "$2" | base64)\"}}" >/dev/null 2>&1
}

# epd_restore_setting <system-ns> <original-encoded> puts the setting back exactly as it was: a
# patch of the prior bytes rather than a delete of the key, because a key the cluster shipped
# absent is indistinguishable from one a user set.
epd_restore_setting() { # <system-ns> <original-encoded>
  local v=null
  [ -n "$2" ] && v="\"$2\""
  kubectl -n "$1" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${EPD_AWARENESS_KEY}\":${v}}}" >/dev/null 2>&1
}

# epd_settle_back <original-encoded> <rounds> <type-name>...
#
# Restoring the value is not the end of the restore: the worker reads it through a thirty-second
# cache, and the type derived from it outlives the patch by that cache plus one reconcile. Wait for
# the derived type to go, so the next case in a serial run does not list an aware type this run
# left behind (case-16's wait_generic, on this same fixture). A cluster that started with the
# setting already on keeps its derived type, so there is nothing to wait for and the wait is
# skipped.
epd_settle_back() { # <original-encoded> <rounds> <type-name>...
  local orig="$1" rounds="$2"
  shift 2
  [ "$(printf '%s' "$orig" | base64 -d 2>/dev/null)" = "true" ] && return 0
  for _ in $(seq 1 "$rounds"); do
    if epd_any_type_exists "$@" >/dev/null; then
      sleep 5
      continue
    fi
    return 0
  done
  return 0
}

# epd_setup_cpu_vendor_fixture <vendor-id> <namespace> <nodefeature-name> <settle-rounds> <system-ns> <node> [platform]
#
# Builds the fixture. The InstanceType that reported the platform is left in EPD_FIXTURE_TYPE,
# empty when none appeared within the rounds.
#
# The call must not be made in a command substitution: the fixture's state — the settings value it
# found, the types it derived and the fact that it changed anything at all — is left in this
# shell, and a subshell would take it with it, leaving the teardown with nothing to undo.
#
# The label rides a NodeFeature object (case-4's mechanism): the object stays and keeps
# re-asserting the label, where a bare `kubectl label` is a cpuid-owned value NFD reverts on its
# next pass. The same object carries a second vendor when a case needs one, so one delete restores
# both.
#
# The poll IS the wait for the settings cache (30s), the node flavor re-derivation and the type
# authoring. A timeout is a skip, not a failure: NFD owns cpu-model.vendor_id and may decline an
# injected override, which is an environment answer the case reports rather than argues with.
#
# The platform waited for defaults to the lowercased vendor; a case that injects one vendor and
# waits for another names the platform it waits for.
epd_setup_cpu_vendor_fixture() { # <vendor-id> <namespace> <nodefeature-name> <settle-rounds> <system-ns> <node> [platform]
  local vendor="$1" ns="$2" nf="$3" rounds="$4" system_ns="$5" node="$6"
  local platform="${7:-}" type=""
  [ -n "$platform" ] || platform="$(printf '%s' "$vendor" | tr '[:upper:]' '[:lower:]')"

  # The settings value is read before the first patch, and only the first time: a second vendor on
  # the same object must not capture the value this run already changed.
  if [ "$EPD_FIXTURE_INJECTED" != true ]; then
    EPD_FIXTURE_ORIG_SETTING="$(kubectl -n "$system_ns" get secret gpustack-settings \
      -o jsonpath="{.data.${EPD_AWARENESS_KEY}}" 2>/dev/null)"
  fi
  EPD_FIXTURE_INJECTED=true

  kubectl apply -f - >/dev/null 2>&1 <<YAML
apiVersion: nfd.k8s-sigs.io/v1alpha1
kind: NodeFeature
metadata:
  name: ${nf}
  namespace: ${ns}
  labels:
    nfd.node.kubernetes.io/node-name: ${node}
spec:
  labels:
    feature.node.kubernetes.io/cpu-model.vendor_id: "${vendor}"
  features: {}
YAML

  epd_set_awareness "$system_ns" true

  for _ in $(seq 1 "$rounds"); do
    type="$(epd_platform_type "$platform")"
    [ -n "$type" ] && break
    sleep 5
  done
  if [ -n "$type" ]; then
    EPD_FIXTURE_TYPES+=("$type")
  fi
  # shellcheck disable=SC2034 # Output consumed by the sourcing case.
  EPD_FIXTURE_TYPE="$type"
  return 0
}

# epd_teardown_cpu_vendor_fixture <namespace> <nodefeature-name> <settle-rounds> <system-ns> [cleanup-hook]
#
# Removes the fixture: the NodeFeature object is deleted, the settings value the fixture found is
# put back exactly as it was, and the types this run derived are waited for.
#
# The cleanup hook runs FIRST, so a case can delete the ModelDeployments it made while the platform
# they were admitted on still exists: the type goes with the label, and a group whose platform has
# gone is one the operator can no longer serve.
#
# Nothing is changed when no fixture was set up.
epd_teardown_cpu_vendor_fixture() { # <namespace> <nodefeature-name> <settle-rounds> <system-ns> [cleanup-hook]
  local ns="$1" nf="$2" rounds="$3" system_ns="$4" hook="${5:-}"
  if [ -n "$hook" ]; then
    "$hook"
  fi
  [ "$EPD_FIXTURE_INJECTED" = true ] || return 0
  kubectl -n "$ns" delete nodefeatures.nfd.k8s-sigs.io "$nf" --ignore-not-found >/dev/null 2>&1
  epd_restore_setting "$system_ns" "$EPD_FIXTURE_ORIG_SETTING"
  if [ "${#EPD_FIXTURE_TYPES[@]}" -gt 0 ]; then
    epd_settle_back "$EPD_FIXTURE_ORIG_SETTING" "$rounds" "${EPD_FIXTURE_TYPES[@]}"
  else
    epd_settle_back "$EPD_FIXTURE_ORIG_SETTING" "$rounds"
  fi
  return 0
}

# ------------------------------------------------------------------------------------------------
# PART TWO: THE LIVE EPD SERVING HELPERS
#
# A three-role encoder/prefill/decode group is read from the outside: the Pods it rendered, the
# router that fronts it, the connector document the engine was given and the claim its roles
# mount. The readings are the same in every case that runs one, and the ways they fail are quiet:
# a Pod that never became Ready, a router image that would not pull, a connector handed a directory
# where the platform wanted a network leg.
#
# The image override belongs to the same reading. The router and the sidecar are images the
# operator chooses from the settings Secret, so a case that wants a specific one pins it there and
# puts the prior bytes back afterwards; the restore is finished only when the router Deployment has
# been re-rendered without it.
# ------------------------------------------------------------------------------------------------

# The bytes the settings Secret carried for the two image settings, kept encoded so the restore can
# put back exactly what was there, and the flag that says the pin ran at all.
EPD_ROUTER_ORIG=""
EPD_SIDECAR_ORIG=""
EPD_IMAGES_PINNED=false

# epd_cluster_registry <system-ns> prints the registry the operator prefixes onto every image it
# chooses, empty when the cluster sets none.
epd_cluster_registry() { # <system-ns>
  kubectl -n "$1" get secret gpustack-settings \
    -o jsonpath='{.data.container-registry}' 2>/dev/null | base64 -d 2>/dev/null
}

# epd_registry_relative <image> prints the image with its registry host dropped.
#
# The operator prefixes `container-registry` onto every image it chooses, and the two settings
# below take a reference of the form `namespace/name:tag`. A value that already names a host would
# render as `<registry>/<host>/...` on a cluster that sets a registry, so drop the host there.
epd_registry_relative() { # <image>
  case "${1%%/*}" in
    *.*|*:*|localhost) printf '%s' "${1#*/}" ;;
    *) printf '%s' "$1" ;;
  esac
}

# epd_set_image_setting <system-ns> <key> <value> writes a value into the settings Secret.
epd_set_image_setting() { # <system-ns> <key> <value>
  kubectl -n "$1" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"$2\":\"$(printf '%s' "$3" | base64)\"}}" >/dev/null 2>&1
}

# epd_restore_image_setting <system-ns> <key> <original-encoded> puts a value back exactly as it
# was: a patch of the prior bytes rather than a delete of the key, because a key the cluster
# shipped at its default is indistinguishable from one a user set, and deleting the former would
# leave this run's baseline changed for every case after it.
epd_restore_image_setting() { # <system-ns> <key> <original-encoded>
  local v=null
  [ -n "$3" ] && v="\"$3\""
  kubectl -n "$1" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"$2\":${v}}}" >/dev/null 2>&1
}

# epd_pin_router_sidecar_images <system-ns> <router-setting> <sidecar-setting> <router-image> <sidecar-image>
#
# Pins the router and the routing sidecar into the settings Secret.
#
# The bytes the Secret carried before are read first and kept encoded in EPD_ROUTER_ORIG and
# EPD_SIDECAR_ORIG, so the restore can put back exactly what was there.
epd_pin_router_sidecar_images() { # <system-ns> <router-setting> <sidecar-setting> <router-image> <sidecar-image>
  local system_ns="$1" router_setting="$2" sidecar_setting="$3" router_image="$4" sidecar_image="$5"
  EPD_IMAGES_PINNED=true
  EPD_ROUTER_ORIG="$(kubectl -n "$system_ns" get secret gpustack-settings \
    -o jsonpath="{.data.${router_setting}}" 2>/dev/null)"
  EPD_SIDECAR_ORIG="$(kubectl -n "$system_ns" get secret gpustack-settings \
    -o jsonpath="{.data.${sidecar_setting}}" 2>/dev/null)"
  epd_set_image_setting "$system_ns" "$router_setting" "$router_image"
  epd_set_image_setting "$system_ns" "$sidecar_setting" "$sidecar_image"
}

# epd_restore_router_sidecar_images <system-ns> <router-setting> <sidecar-setting>
#
# Puts the two image settings back exactly as they were, from the bytes the pin read. Nothing is
# written when no image was pinned.
epd_restore_router_sidecar_images() { # <system-ns> <router-setting> <sidecar-setting>
  [ "$EPD_IMAGES_PINNED" = true ] || return 0
  epd_restore_image_setting "$1" "$2" "$EPD_ROUTER_ORIG"
  epd_restore_image_setting "$1" "$3" "$EPD_SIDECAR_ORIG"
}

# epd_router_image <namespace> <deployment> prints the image the router Deployment runs.
epd_router_image() { # <namespace> <deployment>
  kubectl -n "$1" get deploy "$2-router" \
    -o jsonpath='{.spec.template.spec.containers[*].image}' 2>/dev/null
}

# epd_settle_router_image <namespace> <deployment> <bound> <image>
#
# Restoring the bytes is not the end of the restore: the worker reads settings through a cache and
# re-renders the router Deployment by the value, so a case that returned while the router still ran
# this case's image would hand the next one a router it did not choose. Wait for the restore to be
# READ BACK, which is what the Deployment's own image shows.
#
# Returns 1 when the bound was spent with the router still on this case's image: the restore was
# written but the worker has not read it back. Cleanup cannot record a row from here, so the case
# says it on stderr.
epd_settle_router_image() { # <namespace> <deployment> <bound> <image>
  local ns="$1" deployment="$2" bound="$3" image="$4"
  local deadline=$((SECONDS + bound)) rendered
  while [ "$SECONDS" -lt "$deadline" ]; do
    rendered="$(epd_router_image "$ns" "$deployment")"
    # No router was ever rendered — an apply that was refused, or a run that never reached it — so
    # nothing carries this case's image and there is nothing to wait for.
    [ -n "$rendered" ] || return 0
    # The restored value is not always comparable: a key the cluster shipped absent is restored by
    # deleting it, and what the Deployment then carries is the operator's own default, which this
    # case never knew. "No longer this case's image" is the condition both spellings satisfy.
    case "$rendered" in
      *"$image"*) ;;
      *) return 0 ;;
    esac
    sleep 5
  done
  return 1
}

# epd_pull_reason <namespace> <deployment> <component> prints what any container of the role's
# Pods is waiting on, joined; empty when none is waiting on an image.
epd_pull_reason() { # <namespace> <deployment> <component>
  kubectl -n "$1" get pods -l "app.kubernetes.io/instance=$2,app.kubernetes.io/component=$3" \
    -o jsonpath='{range .items[*]}{.status.containerStatuses[*].state.waiting.reason}{" "}{end}' 2>/dev/null
}

# epd_pod_pull_reason <namespace> <pod> prints what the Pod's first container is waiting on. A Pod
# outside the deployment's own labels — a witness Pod of a case's own making — is read by name.
epd_pod_pull_reason() { # <namespace> <pod>
  kubectl -n "$1" get pod "$2" \
    -o jsonpath='{.status.containerStatuses[0].state.waiting.reason}' 2>/dev/null
}

# epd_wait_pods <namespace> <deployment> <ready-bound> <component>
#
# Prints the role's Pod name once it is Ready, and returns 0 on arrival, 2 when an image would not
# pull, 1 once the bound is spent.
#
# A pull failure is read on every round, because it is the one answer that is cheaper to give than
# to wait for: a container nobody could start is an environment answer, not a verdict about the
# operator, and the case turns it into a skip naming the image.
epd_wait_pods() { # <namespace> <deployment> <ready-bound> <component>
  local ns="$1" deployment="$2" bound="$3" comp="$4"
  local deadline=$((SECONDS + bound)) pods ready p
  while [ "$SECONDS" -lt "$deadline" ]; do
    pods="$(kubectl -n "$ns" get pods \
      -l "app.kubernetes.io/instance=${deployment},app.kubernetes.io/component=${comp}" \
      -o json 2>/dev/null)"
    ready="$(printf '%s' "$pods" | jq -r \
      '[.items[] | select([.status.conditions // [] | .[] | select(.type=="Ready" and .status=="True")] | length > 0)] | length')"
    if [ "${ready:-0}" -ge 1 ]; then
      printf '%s' "$pods" | jq -r '.items[0].metadata.name'
      return 0
    fi
    for p in ImagePullBackOff ErrImagePull; do
      case "$(epd_pull_reason "$ns" "$deployment" "$comp")" in
        *"$p"*) return 2 ;;
      esac
    done
    sleep 15
  done
  return 1
}

# epd_poll_role <namespace> <deployment> <ready-bound> <component>
#
# Polls one role and returns the wait code: 0 on arrival, 2 when an image would not pull, 1 on a
# timeout. The Pod name is left in EN_POD, PF_POD or DC_POD, the variable the role owns.
#
# Each role is polled separately and the exit code is read IMMEDIATELY, because a pull failure on
# the first role means the other two polls would sit out the whole bound before anything reported
# it.
epd_poll_role() { # <namespace> <deployment> <ready-bound> <component>
  local ns="$1" deployment="$2" bound="$3" comp="$4" pod rc varname=""
  pod="$(epd_wait_pods "$ns" "$deployment" "$bound" "$comp")"; rc=$?
  case "$comp" in
    encode) varname=EN_POD ;;
    prefill) varname=PF_POD ;;
    decode) varname=DC_POD ;;
  esac
  if [ -n "$varname" ]; then
    printf -v "$varname" '%s' "$pod"
  fi
  return "$rc"
}

# epd_router_pods <namespace> <deployment> prints the router's own Pods as JSON.
#
# The router's Pods are selected by the router Deployment's OWN selector rather than by a label
# spelled here. The router carries the same instance label the roles carry, plus a router key of
# its own, so an instance-only selector would return the role Pods too and learn nothing about the
# router; the Deployment's selector is the one that cannot drift from what was rendered.
epd_router_pods() { # <namespace> <deployment>
  local sel
  # shellcheck disable=SC2016  # the dollar names are kubectl jsonpath map variables, and must not expand
  sel="$(kubectl -n "$1" get deploy "$2-router" \
    -o go-template='{{range $k,$v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}' 2>/dev/null | sed 's/,$//')"
  [ -n "$sel" ] || return 0
  kubectl -n "$1" get pods -l "$sel" -o json 2>/dev/null
}

# epd_router_pull_reason <namespace> <deployment> prints what any router container is WAITING on,
# joined; empty when none is waiting on an image. Read for the same two reasons the role Pods' own
# reasons are read.
epd_router_pull_reason() { # <namespace> <deployment>
  epd_router_pods "$1" "$2" | jq -r \
    '[.items[]? | .status.containerStatuses[]? | .state.waiting?.reason // empty] | join(" ")' 2>/dev/null
}

# epd_router_pod <namespace> <deployment> prints the Ready router Pod's name, or empty when the
# router is not up. Read once when the verdict is formed, because by then the bound is spent and
# the router's state is settled.
epd_router_pod() { # <namespace> <deployment>
  epd_router_pods "$1" "$2" | jq -r \
    '[.items[]? | select([.status.conditions // [] | .[] | select(.type=="Ready" and .status=="True")] | length > 0) | .metadata.name] | if length > 0 then .[0] else "" end' 2>/dev/null
}

# epd_wait_endpoint <namespace> <deployment> <ready-bound>
#
# Prints the endpoint the deployment published and returns 0 on arrival, 2 when the router image
# would not pull (the same code the role wait returns for a role image), 1 once the bound is spent.
#
# The endpoint is written once the ROUTER's OWN Deployment reports a ready replica — it is the
# router's status the operator copies, not a fact about the role Pods — so a single read here
# races that writer by design, and the wait is bounded because the router is a fourth workload no
# role poll covers.
epd_wait_endpoint() { # <namespace> <deployment> <ready-bound>
  local ns="$1" deployment="$2" bound="$3"
  local deadline=$((SECONDS + bound)) ep p
  while [ "$SECONDS" -lt "$deadline" ]; do
    ep="$(kubectl -n "$ns" get modeldeployments.worker.gpustack.ai "$deployment" \
      -o jsonpath='{.status.endpoint}' 2>/dev/null)"
    if [ -n "$ep" ]; then
      printf '%s' "$ep"
      return 0
    fi
    for p in ImagePullBackOff ErrImagePull; do
      case "$(epd_router_pull_reason "$ns" "$deployment")" in
        *"$p"*) return 2 ;;
      esac
    done
    sleep 5
  done
  return 1
}

# epd_ec_document <namespace> <pod> prints the document the engine was handed.
#
# The document is the argument AFTER --ec-transfer-config, which is how the operator renders it:
# the flag has no environment equivalent, so it is always two argv entries. Reading the pair off
# the live argv rather than pattern-matching the JSON keeps the row a question about the flag.
epd_ec_document() { # <namespace> <pod>
  kubectl -n "$1" get pod "$2" -o json 2>/dev/null | jq -r \
    --arg f "--ec-transfer-config" \
    '[(.spec.containers[] | select(.name=="main") | (.command // []) + (.args // []))][0] // []
     | (index($f) // -1) as $i | if $i < 0 then "" else .[$i + 1] end' 2>/dev/null
}

# epd_ec_env <namespace> <pod> <name> prints the value the engine was given for one variable.
epd_ec_env() { # <namespace> <pod> <name>
  kubectl -n "$1" get pod "$2" -o json 2>/dev/null | jq -r \
    --arg n "$3" '[(.spec.containers[] | select(.name=="main") | .env // [])[] | select(.name==$n)][0].value // ""' 2>/dev/null
}

# epd_ec_env_field_path <namespace> <pod> <name> prints the downward-API half of the same read:
# the field selector the kubelet resolves, not the value it resolved. Rendered by
# valueFrom.fieldRef.fieldPath, so the variable has no .value to read.
epd_ec_env_field_path() { # <namespace> <pod> <name>
  kubectl -n "$1" get pod "$2" -o json 2>/dev/null | jq -r \
    --arg n "$3" '[(.spec.containers[] | select(.name=="main") | .env // [])[] | select(.name==$n)][0].valueFrom.fieldRef.fieldPath // ""' 2>/dev/null
}

# epd_role_pod_claim <namespace> <deployment> <component> prints the claim one role's FIRST Pod
# mounts, as "claimName|mountPath|readOnly".
#
# EVERY CONTAINER IS SEARCHED, not just the first. A direct decode renders the routing sidecar as
# the FIRST regular container, and reading that one would let the decoder's row pass whatever the
# engine mounts: the sidecar carries no mounts of its own, so a claim the decode role grew would
# still read as "no claim" there. Looking through all of them keeps every row a question about the
# whole Pod.
epd_role_pod_claim() { # <namespace> <deployment> <component>
  kubectl -n "$1" get pods \
    -l "app.kubernetes.io/instance=$2,app.kubernetes.io/component=$3" \
    -o json 2>/dev/null | python3 -c '
import json,sys
items=json.load(sys.stdin).get("items",[])
if not items:
    print("")
    raise SystemExit
pod=items[0]
claims={}
for v in pod["spec"].get("volumes",[]):
    p=v.get("persistentVolumeClaim")
    if p and p.get("claimName"):
        claims[v["name"]]=p["claimName"]
for ctr in pod["spec"].get("containers",[]):
    for m in ctr.get("volumeMounts",[]):
        claim=claims.get(m.get("name",""))
        if claim:
            print(claim+"|"+m.get("mountPath","")+"|"+str(m.get("readOnly",False)).lower())
            raise SystemExit
print("")'
}
