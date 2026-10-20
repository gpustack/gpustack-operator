#!/usr/bin/env bash
#
# stage_counts.sh — collect stage execution counts and router/engine Prometheus counters for a
# serving ModelDeployment, sourcing the shared serving-lib.sh.
#
#   stage_counts.sh <deployment> <out-dir>
set -uo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
REPO="$(cd "${SCRIPT_DIR}/../../../.." && pwd)"
export PATH="${REPO}/.agents/skills/_e2e-lib/scripts/kubectl-shim:${PATH}"
# shellcheck source=/dev/null
. "${REPO}/.agents/skills/_e2e-lib/scripts/serving-lib.sh"

NS="${BENCH_NS:-gpustack-bench}"
DEPLOY="$1"
OUT="$2"

mkdir -p "$OUT"
COUNTFILE="$OUT/stage-counts.txt"
: > "$COUNTFILE"

{
  echo "# Stage execution counts for $DEPLOY"
  echo "# Each line: role/pod  POST /v1/chat/completions count"
} >> "$COUNTFILE"

while IFS= read -r pod; do
  [ -n "$pod" ] || continue
  for container in $(kubectl -n "$NS" get pod "$pod" -o jsonpath='{.spec.containers[*].name}' 2>/dev/null); do
    tmp_log="$(mktemp)"
    kubectl -n "$NS" logs "$pod" -c "$container" --tail=-1 > "$tmp_log" 2>/dev/null || true
    count="$(grep -c 'POST /v1/chat/completions HTTP/' "$tmp_log" || true)"
    render_count="$(grep -c 'POST /v1/chat/completions/render HTTP/' "$tmp_log" || true)"
    if [ "${count:-0}" -gt 0 ] || [ "${render_count:-0}" -gt 0 ]; then
      echo "${pod#"${DEPLOY}-"} $container $count (render: ${render_count:-0})" >> "$COUNTFILE"
      grep 'POST /v1/chat/completions' "$tmp_log" > "${OUT}/engine-${pod}-${container}.log" || true
    fi
    rm -f "$tmp_log"
  done
done < <(kubectl -n "$NS" get pods -o name 2>/dev/null | cut -d/ -f2 | grep "^${DEPLOY}-" | sort)

# Collect router access logs and router Prometheus counter series via serving-lib.sh.
md_json="$(kubectl -n "$NS" get modeldeployment "$DEPLOY" -o json 2>/dev/null || true)"
router_name="$(jq -r '.spec.router.name // empty' <<<"$md_json" 2>/dev/null || true)"
split_flag="$(jq -r 'any(.spec.roles[]?; .kind == "Prefill")' <<<"$md_json" 2>/dev/null || echo false)"
router_series=""
if [ -n "$router_name" ]; then
  router_series="$(serving_router_requests "$router_name" "$split_flag" 2>/dev/null || true)"
fi

: > "$OUT/router-access.log"
while IFS= read -r pod; do
  [ -n "$pod" ] || continue
  kubectl -n "$NS" logs "$pod" -c envoy --tail=-1 2>/dev/null \
    | grep '"response_code"' >> "$OUT/router-access.log" || true
  kubectl -n "$NS" logs "$pod" -c epp --tail=-1 2>/dev/null \
    | grep '"response_code"' >> "$OUT/router-access.log" || true
  kubectl -n "$NS" logs "$pod" --tail=-1 2>/dev/null \
    | grep -E '"response_code"|POST /v1/chat/completions' >> "$OUT/router-access.log" || true
done < <(kubectl -n "$NS" get pods -o name 2>/dev/null | cut -d/ -f2 | grep "^${DEPLOY}-.*router" | sort)

echo >> "$COUNTFILE"
echo "# router access lines: $(wc -l < "$OUT/router-access.log" 2>/dev/null | tr -d ' ')" >> "$COUNTFILE"
if [ -n "$router_series" ]; then
  echo "# router counter series (${router_name}, split=${split_flag}): ${router_series}" >> "$COUNTFILE"
fi
cat "$COUNTFILE"
