#!/usr/bin/env bash
#
# CASE 89 — A ModelDeployment role's rendered Pod requests the selected fabric device.
#
# case-89.sh <NS> <MODEL_DEPLOYMENT> <ROLE> <COUNT> <KEY|none>
#
# Goal:        Read the actual main-container limits of every owned Pod for one role. Run once for
#              absent or zero, RDMA one, RDMA multiple, EFA, and a pure P/D fixture. A rejected
#              mixed-fabric manifest can be checked with E2E_REJECT_MANIFEST.
# Environment: An explicitly selected cluster and namespace with the named deployment already
#              reconciled. Pods may be Pending; this case reads their spec. Input is required when
#              no owned role Pod exists. A real device grant and byte transfer need a later run.
# Inputs:      Real ModelDeployment and Pods; no mocked resources. COUNT is the expected interface
#              count and KEY is one of the three device-plugin keys or `none`. Set
#              E2E_EXPECT_PURE_PD=1 when checking a deployment without a cache binding. Set
#              E2E_REJECT_MANIFEST to a local YAML file for server-side dry-run rejection.
# Expected:    The stored role count and every owned main-container limit match the input; no
#              second fabric key is present. Optional pure P/D and rejection checks hold.
# Cleanup:     Read-only, except the API server's non-persisting dry-run; no cleanup is needed.
set -uo pipefail

NS="${1:-}"
MD="${2:-}"
ROLE="${3:-}"
COUNT="${4:-}"
KEY="${5:-}"
if [ -z "$NS" ] || [ -z "$MD" ] || [ -z "$ROLE" ] || [ -z "$COUNT" ] || [ -z "$KEY" ]; then
  echo "usage: case-89.sh <NS> <MODEL_DEPLOYMENT> <ROLE> <COUNT> <KEY|none>" >&2
  exit 2
fi
command -v jq >/dev/null 2>&1 || { echo "jq is required" >&2; exit 2; }
case "$COUNT" in ''|*[!0-9]*) echo "COUNT must be a nonnegative integer" >&2; exit 2 ;; esac
case "$KEY" in
  none|device.gpustack.ai/rdma.shared|device.gpustack.ai/rdma|vpc.amazonaws.com/efa) ;;
  *) echo "unknown fabric resource key: $KEY" >&2; exit 2 ;;
esac
if { [ "$COUNT" -eq 0 ] && [ "$KEY" != none ]; } ||
   { [ "$COUNT" -gt 0 ] && [ "$KEY" = none ]; }; then
  echo "COUNT and KEY disagree" >&2
  exit 2
fi

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); }
print_rows() {
  echo "STATUS | CHECK | OBJECT"
  for row in "${ROWS[@]}"; do
    IFS='|' read -r status check object <<<"$row"
    printf '%s | %s | %s\n' "$status" "$check" "$object"
  done
}

md_json="$(kubectl -n "$NS" get modeldeployments.worker.gpustack.ai "$MD" -o json 2>/dev/null)" || {
  echo "ModelDeployment $NS/$MD is unavailable" >&2
  exit 2
}
md_uid="$(jq -r '.metadata.uid' <<<"$md_json")"
actual_count="$(jq -r --arg role "$ROLE" '.spec.roles[] | select(.name == $role) | .resources.interface // 0 | tonumber' <<<"$md_json")"
if [ -z "$actual_count" ]; then
  echo "role $ROLE is absent from $NS/$MD" >&2
  exit 2
fi
if [ "$actual_count" = "$COUNT" ]; then
  record PASS "stored interface count" "$MD/$ROLE=$actual_count"
else
  record FAIL "stored interface count" "$MD/$ROLE=$actual_count, expected $COUNT"
fi

if [ "${E2E_EXPECT_PURE_PD:-0}" = 1 ]; then
  if jq -e '(.spec.kvCache == null) and
    ([.spec.roles[].kind] | index("prefill") != null and index("decode") != null)' <<<"$md_json" >/dev/null; then
    record PASS "pure P/D fixture" "$MD"
  else
    record FAIL "pure P/D fixture" "$MD"
  fi
fi

pods_json="$(kubectl -n "$NS" get pods -l "app.kubernetes.io/name=model-deployment,app.kubernetes.io/instance=$MD,app.kubernetes.io/component=$ROLE" -o json)" || exit 2
pod_rows="$(jq -c --arg uid "$md_uid" '.items[] | select(any(.metadata.ownerReferences[]?; .uid == $uid and .kind == "ModelDeployment"))' <<<"$pods_json")"
if [ -z "$pod_rows" ]; then
  echo "no owned Pod for $MD/$ROLE; fixture is not reconciled" >&2
  exit 2
fi
while IFS= read -r pod; do
  [ -n "$pod" ] || continue
  name="$(jq -r '.metadata.name' <<<"$pod")"
  fabric="$(jq -c '(.spec.containers[] | select(.name == "main") | .resources.limits // {}) |
    with_entries(select(.key == "device.gpustack.ai/rdma.shared" or
      .key == "device.gpustack.ai/rdma" or .key == "vpc.amazonaws.com/efa"))' <<<"$pod")"
  if [ "$KEY" = none ]; then
    expected='{}'
  else
    expected="$(jq -nc --arg key "$KEY" --arg count "$COUNT" '{($key): $count}')"
  fi
  if [ "$fabric" = "$expected" ]; then
    record PASS "rendered fabric limit" "$name $fabric"
  else
    record FAIL "rendered fabric limit" "$name $fabric, expected $expected"
  fi
done <<<"$pod_rows"

if [ -n "${E2E_REJECT_MANIFEST:-}" ]; then
  [ -f "$E2E_REJECT_MANIFEST" ] || { echo "rejection fixture file is absent" >&2; exit 2; }
  if rejection="$(kubectl apply --dry-run=server -f "$E2E_REJECT_MANIFEST" 2>&1)"; then
    record FAIL "mixed-fabric rejection" "$rejection"
  elif [[ "$rejection" == *"conflict"* || "$rejection" == *"mixed"* ]]; then
    record PASS "mixed-fabric rejection" "$E2E_REJECT_MANIFEST"
  else
    record FAIL "mixed-fabric rejection" "$rejection"
  fi
fi

print_rows
[ "$FAILS" -eq 0 ]
