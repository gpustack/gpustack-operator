#!/usr/bin/env bash
#
# gen-mock-configmap.sh — generate a Kubernetes ConfigMap containing a valid multi-node mockspec
#                         document backed by built-in multi-vendor hardware presets.
#
# Usage:
#   gen-mock-configmap.sh [--namespace <NS>] [--name <cm>] [--key <key>]
#                         [--cluster-name <kind-name>] [--workers <N>] [--nodes <csv>]
#                         [--profile <heterogeneous|scale-mig|fault-lab|multi-vendor>]
#                         [--default-preset <preset>] [--cards-per-node <N>]
#                         [--raw-spec] [--out <file>]

set -euo pipefail

NAMESPACE="gpustack-system"
CM_NAME="mock-devices"
CM_KEY="devices.yaml"
CLUSTER_NAME=""
WORKERS=""
NODES_CSV=""
PROFILE="heterogeneous"
DEFAULT_PRESET=""
CARDS_PER_NODE=""
RAW_SPEC=0
OUT_FILE=""

KNOWN_PRESETS=(
  amd-mi300x
  amd-mi308x
  amd-rx-7800-xt
  ascend-310p3
  ascend-910b2
  cambricon-mlu590
  hygon-k100-ai
  hygon-k100-ai-mig
  iluvatar-bi-v150
  metax-c500
  mthreads-s5000
  nvidia-gb10
  nvidia-h100
  nvidia-h100-mig
  nvidia-h200
  nvidia-rtx-4090-48g
  nvidia-rtx-5090d
  thead-ppu
  thead-ppu-mig
)

usage() {
  cat <<'EOF'
Usage: gen-mock-configmap.sh [options]

Emit a Kubernetes ConfigMap YAML (or raw mockspec YAML with --raw-spec) assigning built-in
multi-vendor mock hardware presets across worker nodes.

Options:
  --namespace <NS>        ConfigMap namespace (default: gpustack-system)
  --name <cm>             ConfigMap name (default: mock-devices)
  --key <key>             Data key holding the mock spec (default: devices.yaml)
  --cluster-name <name>   Kind cluster name to discover or synthesize worker node names for
  --workers <N>           Worker count when synthesizing Kind node names offline (default: 3)
  --nodes <csv>           Explicit comma-separated node names (overrides --cluster-name / kubectl)
  --profile <profile>     Hardware profile: heterogeneous | scale-mig | fault-lab | multi-vendor
                          (default: heterogeneous)
  --default-preset <p>    Fallback preset under `default:` (default: nvidia-h100)
  --cards-per-node <N>    Optional positive integer overriding per-preset card counts
  --raw-spec              Emit only the inner mockspec YAML document without ConfigMap wrapper
  --out <file>            Write output to <file> instead of stdout
  -h, --help              Show this help message
EOF
}

is_pos_int() {
  [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]
}

is_known_preset() {
  local want="$1" p
  for p in "${KNOWN_PRESETS[@]}"; do
    [[ "$p" == "$want" ]] && return 0
  done
  return 1
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --namespace|-n)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: $1 requires a value" >&2; exit 2; }
      NAMESPACE="$2"
      shift 2
      ;;
    --namespace=*)
      NAMESPACE="${1#--namespace=}"
      shift
      ;;
    --name)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --name requires a value" >&2; exit 2; }
      CM_NAME="$2"
      shift 2
      ;;
    --name=*)
      CM_NAME="${1#--name=}"
      shift
      ;;
    --key)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --key requires a value" >&2; exit 2; }
      CM_KEY="$2"
      shift 2
      ;;
    --key=*)
      CM_KEY="${1#--key=}"
      shift
      ;;
    --cluster-name)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --cluster-name requires a value" >&2; exit 2; }
      CLUSTER_NAME="$2"
      shift 2
      ;;
    --cluster-name=*)
      CLUSTER_NAME="${1#--cluster-name=}"
      shift
      ;;
    --workers)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --workers requires a positive integer" >&2; exit 2; }
      WORKERS="$2"
      shift 2
      ;;
    --workers=*)
      WORKERS="${1#--workers=}"
      shift
      ;;
    --nodes)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --nodes requires a comma-separated list" >&2; exit 2; }
      NODES_CSV="$2"
      shift 2
      ;;
    --nodes=*)
      NODES_CSV="${1#--nodes=}"
      shift
      ;;
    --profile)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --profile requires a value" >&2; exit 2; }
      PROFILE="$2"
      shift 2
      ;;
    --profile=*)
      PROFILE="${1#--profile=}"
      shift
      ;;
    --default-preset)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --default-preset requires a preset name" >&2; exit 2; }
      DEFAULT_PRESET="$2"
      shift 2
      ;;
    --default-preset=*)
      DEFAULT_PRESET="${1#--default-preset=}"
      shift
      ;;
    --cards-per-node)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --cards-per-node requires a positive integer" >&2; exit 2; }
      CARDS_PER_NODE="$2"
      shift 2
      ;;
    --cards-per-node=*)
      CARDS_PER_NODE="${1#--cards-per-node=}"
      shift
      ;;
    --raw-spec)
      RAW_SPEC=1
      shift
      ;;
    --out)
      [[ $# -ge 2 ]] || { echo "gen-mock-configmap: --out requires a file path" >&2; exit 2; }
      OUT_FILE="$2"
      shift 2
      ;;
    --out=*)
      OUT_FILE="${1#--out=}"
      shift
      ;;
    -h|--help)
      usage
      exit 0
      ;;
    *)
      echo "gen-mock-configmap: unknown argument: $1" >&2
      usage >&2
      exit 2
      ;;
  esac
done

[[ -n "$NAMESPACE" ]] || { echo "gen-mock-configmap: --namespace must not be empty" >&2; exit 2; }
[[ -n "$CM_NAME" ]] || { echo "gen-mock-configmap: --name must not be empty" >&2; exit 2; }
[[ -n "$CM_KEY" ]] || { echo "gen-mock-configmap: --key must not be empty" >&2; exit 2; }

if [[ -n "$DEFAULT_PRESET" ]] && ! is_known_preset "$DEFAULT_PRESET"; then
  echo "gen-mock-configmap: unknown --default-preset '${DEFAULT_PRESET}'" >&2
  exit 2
fi

if [[ -n "$WORKERS" ]] && ! is_pos_int "$WORKERS"; then
  echo "gen-mock-configmap: --workers must be >= 1 (got '${WORKERS}')" >&2
  exit 2
fi

if [[ -n "$CARDS_PER_NODE" ]] && ! is_pos_int "$CARDS_PER_NODE"; then
  echo "gen-mock-configmap: --cards-per-node must be >= 1 (got '${CARDS_PER_NODE}')" >&2
  exit 2
fi

case "$PROFILE" in
  heterogeneous|scale-mig|fault-lab|multi-vendor) ;;
  *)
    echo "gen-mock-configmap: unsupported --profile '${PROFILE}' (expected heterogeneous, scale-mig, fault-lab, or multi-vendor)" >&2
    exit 2
    ;;
esac

synthesize_kind_workers() {
  local cname="$1" cnt="$2" idx
  for ((idx = 1; idx <= cnt; idx++)); do
    if [[ "$idx" -eq 1 ]]; then
      printf '%s-worker\n' "$cname"
    else
      printf '%s-worker%d\n' "$cname" "$idx"
    fi
  done
}

discover_nodes() {
  if [[ -n "$NODES_CSV" ]]; then
    printf '%s\n' "$NODES_CSV" | tr ',' '\n' | sed 's/^[[:space:]]*//;s/[[:space:]]*$//' | awk 'NF'
    return 0
  fi

  if [[ -n "$CLUSTER_NAME" ]]; then
    if [[ -z "$WORKERS" ]] && command -v kind >/dev/null 2>&1; then
      local kind_nodes
      kind_nodes="$(kind get nodes --name "$CLUSTER_NAME" 2>/dev/null | grep -v -- '-control-plane$' | sort || true)"
      if [[ -n "$kind_nodes" ]]; then
        printf '%s\n' "$kind_nodes"
        return 0
      fi
    fi
    synthesize_kind_workers "$CLUSTER_NAME" "${WORKERS:-3}"
    return 0
  fi

  if [[ -n "$WORKERS" ]]; then
    synthesize_kind_workers "gpustack-mock" "$WORKERS"
    return 0
  fi

  if command -v kubectl >/dev/null 2>&1 && command -v jq >/dev/null 2>&1; then
    local nodes_json live_workers
    if nodes_json="$(kubectl get nodes -o json 2>/dev/null)" && [[ -n "$nodes_json" ]]; then
      live_workers="$(printf '%s' "$nodes_json" | jq -r '
        ([.items[]
          | select((.spec.unschedulable // false) | not)
          | select((.metadata.labels // {}) | (has("node-role.kubernetes.io/control-plane") or has("node-role.kubernetes.io/master")) | not)
          | .metadata.name] | sort) as $workers
        | if ($workers | length) > 0 then $workers[]
          else ([.items[] | select((.spec.unschedulable // false) | not) | .metadata.name] | sort | .[])
          end
      ' 2>/dev/null || true)"
      if [[ -n "$live_workers" ]]; then
        printf '%s\n' "$live_workers"
        return 0
      fi
    fi
  fi

  synthesize_kind_workers "gpustack-mock" 3
}

NODES=()
while IFS= read -r nline; do
  [[ -n "$nline" ]] && NODES+=("$nline")
done < <(discover_nodes)

if [[ "${#NODES[@]}" -eq 0 ]]; then
  echo "gen-mock-configmap: resolved zero target worker nodes" >&2
  exit 1
fi

count_or_default() {
  local fallback="$1"
  if [[ -n "$CARDS_PER_NODE" ]]; then
    printf '%s' "$CARDS_PER_NODE"
  else
    printf '%s' "$fallback"
  fi
}

render_node_heterogeneous() {
  local idx="$1" preset count
  case $((idx % 8)) in
    0) preset="nvidia-h100";       count="$(count_or_default 4)" ;;
    1) preset="nvidia-h100-mig";   count="$(count_or_default 2)" ;;
    2) preset="amd-mi308x";        count="$(count_or_default 4)" ;;
    3) preset="ascend-910b2";      count="$(count_or_default 4)" ;;
    4) preset="nvidia-h200";       count="$(count_or_default 4)" ;;
    5) preset="hygon-k100-ai-mig"; count="$(count_or_default 2)" ;;
    6) preset="thead-ppu-mig";     count="$(count_or_default 2)" ;;
    7) preset="metax-c500";        count="$(count_or_default 4)" ;;
  esac
  printf '    preset: %s\n' "$preset"
  printf '    count: %s\n' "$count"
}

render_node_scale_mig() {
  local idx="$1" preset count
  case $((idx % 4)) in
    0) preset="nvidia-h100";       count="$(count_or_default 8)" ;;
    1) preset="nvidia-h100-mig";   count="$(count_or_default 4)" ;;
    2) preset="hygon-k100-ai-mig"; count="$(count_or_default 4)" ;;
    3) preset="thead-ppu-mig";     count="$(count_or_default 4)" ;;
  esac
  printf '    preset: %s\n' "$preset"
  printf '    count: %s\n' "$count"
}

render_node_fault_lab() {
  local idx="$1" count
  count="$(count_or_default 4)"
  case $((idx % 4)) in
    0)
      printf '    preset: nvidia-h100\n'
      printf '    count: %s\n' "$count"
      ;;
    1)
      local deg_count="$count"
      [[ "$deg_count" -lt 2 ]] && deg_count=2
      printf '    preset: nvidia-h100\n'
      printf '    count: %s\n' "$deg_count"
      printf '    cardOverrides:\n'
      printf '      1:\n'
      printf '        unhealthy: true\n'
      ;;
    2)
      printf '    preset: amd-mi308x\n'
      printf '    count: %s\n' "$count"
      printf '    fault:\n'
      printf '      initError:\n'
      printf '        enabled: true\n'
      printf '        failCount: 1\n'
      printf '        message: "transient mock driver warm-up"\n'
      ;;
    3)
      printf '    preset: nvidia-h100-mig\n'
      printf '    count: %s\n' "$(count_or_default 2)"
      ;;
  esac
}

render_node_multi_vendor() {
  local idx="$1" c2
  c2="$(count_or_default 2)"
  printf '    presets:\n'
  case $((idx % 4)) in
    0)
      printf '      - preset: nvidia-h100\n'
      printf '        count: %s\n' "$c2"
      printf '      - preset: amd-mi308x\n'
      printf '        count: %s\n' "$c2"
      ;;
    1)
      printf '      - preset: nvidia-h100-mig\n'
      printf '        count: %s\n' "$c2"
      printf '      - preset: ascend-910b2\n'
      printf '        count: %s\n' "$c2"
      ;;
    2)
      printf '      - preset: hygon-k100-ai-mig\n'
      printf '        count: %s\n' "$c2"
      printf '      - preset: thead-ppu-mig\n'
      printf '        count: %s\n' "$c2"
      ;;
    3)
      printf '      - preset: metax-c500\n'
      printf '        count: %s\n' "$(count_or_default 1)"
      printf '      - preset: cambricon-mlu590\n'
      printf '        count: %s\n' "$(count_or_default 1)"
      printf '      - preset: iluvatar-bi-v150\n'
      printf '        count: %s\n' "$(count_or_default 1)"
      printf '      - preset: mthreads-s5000\n'
      printf '        count: %s\n' "$(count_or_default 1)"
      ;;
  esac
}

render_spec_doc() {
  local def_count
  def_count="$(count_or_default 2)"
  printf '# Generated by gen-mock-configmap.sh: profile=%s nodes=%d\n' "$PROFILE" "${#NODES[@]}"
  printf 'default:\n'
  if [[ -n "$DEFAULT_PRESET" ]]; then
    printf '  preset: %s\n' "$DEFAULT_PRESET"
    printf '  count: %s\n' "$def_count"
  else
    printf '  groups: []\n'
  fi
  printf 'nodes:\n'

  local i node
  for ((i = 0; i < ${#NODES[@]}; i++)); do
    node="${NODES[$i]}"
    printf '  "%s":\n' "$node"
    case "$PROFILE" in
      heterogeneous) render_node_heterogeneous "$i" ;;
      scale-mig)     render_node_scale_mig "$i" ;;
      fault-lab)     render_node_fault_lab "$i" ;;
      multi-vendor)  render_node_multi_vendor "$i" ;;
    esac
  done
}

render_output() {
  if [[ "$RAW_SPEC" -eq 1 ]]; then
    render_spec_doc
    return 0
  fi

  cat <<EOF
apiVersion: v1
kind: ConfigMap
metadata:
  name: ${CM_NAME}
  namespace: ${NAMESPACE}
  labels:
    app.kubernetes.io/part-of: gpustack-operator
    gpustack.ai/mock-profile: ${PROFILE}
data:
  ${CM_KEY}: |
EOF
  render_spec_doc | sed 's/^/    /'
}

if [[ -n "$OUT_FILE" ]]; then
  mkdir -p "$(dirname "$OUT_FILE")"
  render_output >"$OUT_FILE"
else
  render_output
fi
