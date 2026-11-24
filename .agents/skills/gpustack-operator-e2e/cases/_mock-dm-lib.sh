#!/usr/bin/env bash
#
# _mock-dm-lib.sh — shared discovery, ConfigMap snapshot/restore, hot-reload, and convergence
#                   helpers for CPU-only Mock Device Manager E2E and chaos scripts.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap, and no results table. Callers source it alongside `_rows-lib.sh` and wire
# `mock_dm_restore_cm` into their own `EXIT` trap whenever they mutate the mock ConfigMap.
#
# Functions provided:
#   mock_dm_detect_env <NS>
#   mock_dm_snapshot_cm <NS> <CM> <OUTFILE>
#   mock_dm_restore_cm <NS> <CM> <OUTFILE>
#   mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT>
#   mock_dm_wait_devices <NS> <NODE> <JQ_EXPR> [TIMEOUT_SECS]
#   mock_dm_wait_it <JQ_EXPR> [TIMEOUT_SECS]
#   mock_dm_wait_node_allocatable <NODE> <JQ_EXPR> [TIMEOUT_SECS]
#   mock_dm_touch_pods <NS>
#   mock_dm_manufacturer_allowed <MANUFACTURER>

# Globals populated by mock_dm_detect_env:
MOCK_DM_DS=""
MOCK_DM_CM=""
MOCK_DM_KEY="devices.yaml"
MOCK_DM_MANUFACTURERS=""
MOCK_DM_NODE_LIST=""
MOCK_DM_PRIMARY_NODE=""
MOCK_DM_NODES=()

# mock_dm_detect_env <NS> discovers the running mock device-manager DaemonSet
# (gpustack.ai/manufacturer=mock), its mounted ConfigMap name and key, any --manufacturer filter,
# and Ready schedulable nodes whose Devices carry device.gpustack.ai/mock="true" (falling back to
# Ready schedulable nodes hosting Ready mock device-manager Pods when the active spec currently
# declares zero groups). Returns 0 when mock mode is active, non-zero otherwise.
mock_dm_detect_env() {
  local ns="${1:?usage: mock_dm_detect_env <NS>}"
  MOCK_DM_DS=""
  MOCK_DM_CM=""
  MOCK_DM_KEY="devices.yaml"
  MOCK_DM_MANUFACTURERS=""
  MOCK_DM_NODE_LIST=""
  MOCK_DM_PRIMARY_NODE=""
  MOCK_DM_NODES=()

  local ds_json
  if ! ds_json="$(kubectl -n "$ns" get daemonset \
    -l app.kubernetes.io/component=device-manager,gpustack.ai/manufacturer=mock \
    -o json 2>/dev/null)"; then
    return 1
  fi

  local parsed
  parsed="$(printf '%s' "$ds_json" | jq -r '
    (.items // []) as $items
    | if ($items | length) == 0 then ""
      else
        ($items | sort_by(.metadata.name) | .[0]) as $ds
        | if (($ds.status.numberReady // 0) < 1) then ""
          else
            ($ds.spec.template.spec.containers[0] // {}) as $ctr
            | (
                [($ctr.args // [])[] | select(startswith("--mock-devices-file=")) | sub("^--mock-devices-file="; "")]
                + [($ctr.env // [])[] | select(.name == "GPUSTACK_MOCK_DEVICES_FILE" and (.value // "") != "") | .value]
                | .[0] // "/etc/gpustack/mock/devices.yaml"
              ) as $filepath
            | ($filepath | sub("/[^/]+$"; "")) as $mountdir
            | ($filepath | sub("^.*/"; "")) as $key
            | (
                [($ctr.volumeMounts // [])[] | select(.mountPath == $mountdir or .name == "mock-devices") | .name]
                | .[0] // "mock-devices"
              ) as $volname
            | (
                [($ds.spec.template.spec.volumes // [])[] | select(.name == $volname) | .configMap.name // ""]
                | .[0] // ""
              ) as $cm
            | (
                [($ctr.args // [])[] | select(startswith("--manufacturer=")) | sub("^--manufacturer="; "")]
                | join(",")
              ) as $mfrs
            | [$ds.metadata.name, $cm, $key, $mfrs] | @tsv
          end
      end
  ' 2>/dev/null)" || return 1

  [ -n "$parsed" ] || return 1

  local ds_name cm_name cm_key mfrs_csv
  IFS=$'\t' read -r ds_name cm_name cm_key mfrs_csv <<<"$parsed"
  [ -n "$ds_name" ] && [ -n "$cm_name" ] || return 1

  local nodes_json devs_json pods_json
  nodes_json="$(kubectl get nodes -o json 2>/dev/null)" || return 1
  devs_json="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null)" || return 1
  pods_json="$(kubectl -n "$ns" get pods \
    -l app.kubernetes.io/component=device-manager,gpustack.ai/manufacturer=mock \
    -o json 2>/dev/null)" || return 1

  local discovered_nodes
  discovered_nodes="$(jq -rn \
    --argjson nodes "$nodes_json" \
    --argjson devs "$devs_json" \
    --argjson pods "$pods_json" '
      def schedulable($n):
        (any(($n.status.conditions // [])[]; .type == "Ready" and .status == "True"))
        and (($n.spec.unschedulable // false) | not);
      ([($nodes.items // [])[] | select(schedulable(.)) | .metadata.name]) as $ready_nodes
      | ([($devs.items // [])[]
          | select((.metadata.annotations["device.gpustack.ai/mock"] // "") == "true")
          | .metadata.name
          | select(. as $n | $ready_nodes | index($n))]) as $mock_dev_nodes
      | ([($pods.items // [])[]
          | select(.status.phase == "Running"
              and any((.status.conditions // [])[]; .type == "Ready" and .status == "True"))
          | .spec.nodeName // ""
          | select(. != "" and (. as $n | $ready_nodes | index($n)))]) as $mock_pod_nodes
      | (if ($mock_dev_nodes | length) > 0 then $mock_dev_nodes else $mock_pod_nodes end)
      | unique | sort | .[]
    ' 2>/dev/null)" || return 1

  local node_line
  while IFS= read -r node_line; do
    [ -n "$node_line" ] && MOCK_DM_NODES+=("$node_line")
  done <<<"$discovered_nodes"

  [ "${#MOCK_DM_NODES[@]}" -ge 1 ] || return 1

  MOCK_DM_DS="$ds_name"
  MOCK_DM_CM="$cm_name"
  MOCK_DM_KEY="${cm_key:-devices.yaml}"
  MOCK_DM_MANUFACTURERS="${mfrs_csv//,/ }"
  MOCK_DM_NODE_LIST="${MOCK_DM_NODES[*]}"
  MOCK_DM_PRIMARY_NODE="${MOCK_DM_NODES[0]}"
  export MOCK_DM_DS MOCK_DM_CM MOCK_DM_KEY MOCK_DM_MANUFACTURERS MOCK_DM_NODE_LIST MOCK_DM_PRIMARY_NODE
  return 0
}

# mock_dm_manufacturer_allowed <MANUFACTURER> returns 0 when the running mock device-manager serves
# <MANUFACTURER> (either --manufacturer was omitted, meaning all known manufacturers are served, or
# <MANUFACTURER> appears in MOCK_DM_MANUFACTURERS).
mock_dm_manufacturer_allowed() {
  local want="${1:?usage: mock_dm_manufacturer_allowed <MANUFACTURER>}"
  [ -z "$MOCK_DM_MANUFACTURERS" ] && return 0
  local m
  for m in $MOCK_DM_MANUFACTURERS; do
    [ "$m" = "$want" ] && return 0
  done
  return 1
}

# mock_dm_touch_pods <NS> updates a harmless sync annotation on running mock device-manager Pods
# (never the DaemonSet template, so zero Pod restarts occur). Kubelet watches bound Pods and
# immediately refreshes projected/ConfigMap volume mounts on a Pod metadata update instead of
# waiting out its periodic ConfigMap cache TTL.
mock_dm_touch_pods() {
  local ns="${1:?usage: mock_dm_touch_pods <NS>}"
  local stamp
  stamp="$(date +%s)-$$-${RANDOM}"
  kubectl -n "$ns" annotate pod \
    -l app.kubernetes.io/component=device-manager,gpustack.ai/manufacturer=mock \
    "gpustack.ai/mock-sync=${stamp}" --overwrite >/dev/null 2>&1 || true
}

# mock_dm_snapshot_cm <NS> <CM> <OUTFILE> saves the ConfigMap's exact data and binaryData maps to
# <OUTFILE> so mock_dm_restore_cm can restore them inside an EXIT trap.
mock_dm_snapshot_cm() {
  local ns="${1:?usage: mock_dm_snapshot_cm <NS> <CM> <OUTFILE>}"
  local cm="${2:?usage: mock_dm_snapshot_cm <NS> <CM> <OUTFILE>}"
  local outfile="${3:?usage: mock_dm_snapshot_cm <NS> <CM> <OUTFILE>}"
  local raw
  raw="$(kubectl -n "$ns" get configmap "$cm" -o json 2>/dev/null)" || return 1
  printf '%s' "$raw" | jq -c '{data: (.data // null), binaryData: (.binaryData // null)}' >"$outfile"
}

# mock_dm_restore_cm <NS> <CM> <OUTFILE> restores the ConfigMap's data and binaryData maps in place
# from <OUTFILE> (preserving the ConfigMap UID and volume mounts) and nudges kubelet volume sync.
mock_dm_restore_cm() {
  local ns="${1:?usage: mock_dm_restore_cm <NS> <CM> <OUTFILE>}"
  local cm="${2:?usage: mock_dm_restore_cm <NS> <CM> <OUTFILE>}"
  local outfile="${3:?usage: mock_dm_restore_cm <NS> <CM> <OUTFILE>}"
  [ -s "$outfile" ] || return 0

  local cur_json ops
  cur_json="$(kubectl -n "$ns" get configmap "$cm" -o json 2>/dev/null)" || return 1
  ops="$(jq -cn \
    --argjson cur "$cur_json" \
    --slurpfile snap "$outfile" '
      ($snap[0] // {}) as $s
      | (
          if $s.data == null then
            (if ($cur | has("data")) then [{op: "remove", path: "/data"}] else [] end)
          else
            [{op: (if ($cur | has("data")) then "replace" else "add" end), path: "/data", value: $s.data}]
          end
        )
      + (
          if $s.binaryData == null then
            (if ($cur | has("binaryData")) then [{op: "remove", path: "/binaryData"}] else [] end)
          else
            [{op: (if ($cur | has("binaryData")) then "replace" else "add" end), path: "/binaryData", value: $s.binaryData}]
          end
        )
    ' 2>/dev/null)" || return 1

  if [ -n "$ops" ] && [ "$ops" != "[]" ]; then
    kubectl -n "$ns" patch configmap "$cm" --type=json -p "$ops" >/dev/null || return 1
  fi
  mock_dm_touch_pods "$ns"
  return 0
}

# mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT> updates <KEY> inside ConfigMap <CM> without
# restarting device-manager Pods and nudges kubelet to hot-reload the mounted ConfigMap file.
mock_dm_apply_spec() {
  local ns="${1:?usage: mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT>}"
  local cm="${2:?usage: mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT>}"
  local key="${3:?usage: mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT>}"
  local content="${4:?usage: mock_dm_apply_spec <NS> <CM> <KEY> <YAML_CONTENT>}"

  local patch_payload
  patch_payload="$(jq -cn --arg k "$key" --arg v "$content" '{data: {($k): $v}}')" || return 1
  kubectl -n "$ns" patch configmap "$cm" --type=merge -p "$patch_payload" >/dev/null || return 1
  mock_dm_touch_pods "$ns"
  return 0
}

# mock_dm_wait_devices <NS> <NODE> <JQ_EXPR> [TIMEOUT_SECS] polls Devices/<NODE> until <JQ_EXPR>
# evaluates truthy via `jq -e` (default timeout 90s to cover kubelet ConfigMap sync + monitor tick).
mock_dm_wait_devices() {
  local _ns="${1:?usage: mock_dm_wait_devices <NS> <NODE> <JQ_EXPR> [TIMEOUT_SECS]}"
  local node="${2:?usage: mock_dm_wait_devices <NS> <NODE> <JQ_EXPR> [TIMEOUT_SECS]}"
  local jq_expr="${3:?usage: mock_dm_wait_devices <NS> <NODE> <JQ_EXPR> [TIMEOUT_SECS]}"
  local timeout_secs="${4:-90}"
  local elapsed=0 dev_json=""

  # Keep _ns referenced explicitly for positional signature consistency across callers.
  : "$_ns"
  while [ "$elapsed" -lt "$timeout_secs" ]; do
    if dev_json="$(kubectl get devices.worker.gpustack.ai "$node" -o json 2>/dev/null)" && [ -n "$dev_json" ]; then
      if printf '%s' "$dev_json" | jq -e "$jq_expr" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# mock_dm_wait_it <JQ_EXPR> [TIMEOUT_SECS] polls InstanceTypes and AcceleratorTypes until <JQ_EXPR>
# evaluates truthy against `{items: <instancetypes>, instanceTypes: <instancetypes>, acceleratorTypes: <acceleratortypes>}`.
mock_dm_wait_it() {
  local jq_expr="${1:?usage: mock_dm_wait_it <JQ_EXPR> [TIMEOUT_SECS]}"
  local timeout_secs="${2:-90}"
  local elapsed=0 it_json="" at_json="" combined=""

  while [ "$elapsed" -lt "$timeout_secs" ]; do
    it_json="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>/dev/null)" || it_json=""
    at_json="$(kubectl get acceleratortypes.worker.gpustack.ai -o json 2>/dev/null)" || at_json='{"items":[]}'
    if [ -n "$it_json" ]; then
      combined="$(jq -cn \
        --argjson it "$it_json" \
        --argjson at "${at_json:-{\"items\":[]}}" \
        '$it + {instanceTypes: ($it.items // []), acceleratorTypes: ($at.items // [])}' 2>/dev/null)" || combined=""
      if [ -n "$combined" ] && printf '%s' "$combined" | jq -e "$jq_expr" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}

# mock_dm_wait_node_allocatable <NODE> <JQ_EXPR> [TIMEOUT_SECS] polls Node/<NODE> until <JQ_EXPR>
# evaluates truthy against `.status.allocatable` (also exposing `$node` as the full Node JSON).
mock_dm_wait_node_allocatable() {
  local node="${1:?usage: mock_dm_wait_node_allocatable <NODE> <JQ_EXPR> [TIMEOUT_SECS]}"
  local jq_expr="${2:?usage: mock_dm_wait_node_allocatable <NODE> <JQ_EXPR> [TIMEOUT_SECS]}"
  local timeout_secs="${3:-90}"
  local elapsed=0 node_json=""

  while [ "$elapsed" -lt "$timeout_secs" ]; do
    if node_json="$(kubectl get node "$node" -o json 2>/dev/null)" && [ -n "$node_json" ]; then
      if printf '%s' "$node_json" | jq -e ". as \$node | (.status.allocatable // {}) | (${jq_expr})" >/dev/null 2>&1; then
        return 0
      fi
    fi
    sleep 3
    elapsed=$((elapsed + 3))
  done
  return 1
}
