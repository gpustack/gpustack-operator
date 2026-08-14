#!/usr/bin/env bash
#
# CASE 18 — CPU-manufacturer awareness reshapes the catalog   (MUTATING: flips an editable setting, self-restoring)
#
#   case-18.sh <NS>
#
# Goal:        On the general (CPU) pool, prove instance-type-aware-cpu-manufacturer drives the
#              collapse<->split of the aggregated view while the ResourceFlavor stays the finest,
#              setting-independent grain —
#              A: the CPU ResourceFlavor is named gpustack--${gKey}-${os}-${arch}-${count}c (the
#                 double-dash finest grain) and always carries a cpuDetail note recording the raw CPU
#                 manufacturer/product/family;
#              B: with awareness OFF the InstanceTypeFlavor catalog collapses every CPU into one
#                 CPU-agnostic row named gpustack--generic (acceleratable=false, generalGroup=generic);
#              C: flipping awareness ON splits it — the generic row is replaced by a real-CPU-keyed row
#                 named gpustack--${gKey} (generalGroup=${gKey}, not "generic"), and the generic row is gone.
# Environment: Any cluster with a materialized general (CPU) pool. No GPU. The ResourceFlavor and the
#              InstanceTypeFlavor catalog exist whenever a node is managed, so this runs anywhere; the
#              catalog recomputes live at list time, so the toggle needs no redeploy.
# Inputs:      Nothing mocked — patches the gpustack-settings Secret's
#              instance-type-aware-cpu-manufacturer key true/false (the editable-setting store the
#              aggregated apiserver reads, with a <=30s value cache) and reads the live catalog.
# Expected:    - A — a gpustack--...-Nc CPU flavor whose note.gpustack.ai/cpuDetail parses as JSON;
#              - B — awareness off shows exactly the gpustack--generic (generalGroup=generic) row;
#              - C — awareness on shows a gpustack--${gKey} (generalGroup!=generic) row and no generic row.
# Cleanup:     Trap restores the setting to its original value, waits for the catalog to re-collapse so
#              the reconciler's setting cache has flipped back, then deletes any operator-derived
#              InstanceType (+ its ClusterQueue) created while awareness was on, so the cluster is left
#              exactly as found.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-18.sh <NS>}"
AWARE_KEY=instance-type-aware-cpu-manufacturer
DERIVED_LABEL=schedule.gpustack.ai/derived-from-node

# --- Helpers (defined before the trap so cleanup can use them). ---

# generic_row prints the name of the collapsed, CPU-agnostic catalog row (acceleratable=false,
# generalGroup=="generic"), else empty.
generic_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
try: items=json.load(sys.stdin).get('items',[])
except Exception: items=[]
for it in items:
    s=it.get('spec',{})
    if not s.get('acceleratable') and s.get('generalGroup')=='generic':
        print(it['metadata']['name']); break
" 2>/dev/null
}

# split_row prints 'name|generalGroup' of a per-CPU catalog row (acceleratable=false, generalGroup a
# real CPU key, not the "generic" sentinel), else empty.
split_row() {
  kubectl get instancetypeflavors.worker.gpustack.ai -o json 2>/dev/null | python3 -c "
import json,sys
try: items=json.load(sys.stdin).get('items',[])
except Exception: items=[]
for it in items:
    s=it.get('spec',{}); gg=s.get('generalGroup','')
    if not s.get('acceleratable') and gg and gg!='generic':
        print(it['metadata']['name']+'|'+gg); break
" 2>/dev/null
}

# derived_its prints the names of the operator-authored (derived-from-node) InstanceTypes.
derived_its() {
  kubectl get instancetypes.worker.gpustack.ai -l "${DERIVED_LABEL}=true" \
    -o jsonpath='{.items[*].metadata.name}' 2>/dev/null
}

# set_aware patches the delegated settings Secret's awareness key to $1 (true|false).
set_aware() {
  local b64; b64=$(printf '%s' "$1" | base64 | tr -d '\n')
  kubectl -n "$NS" patch secret gpustack-settings --type=merge \
    -p "{\"data\":{\"${AWARE_KEY}\":\"${b64}\"}}" >/dev/null 2>&1
}

# wait_generic waits for the collapsed generic row to appear (rides out the <=30s setting cache).
wait_generic() { for _ in $(seq 1 24); do [ -n "$(generic_row)" ] && return 0; sleep 3; done; return 1; }

# The CPU ResourceFlavor is keyed by the node's real CPU (gpustack--${gKey}-...-${count}c),
# independent of any collapsed InstanceType name — the finest, setting-independent grain.
CPURF=$(kubectl get resourceflavors.kueue.x-k8s.io -o name 2>/dev/null | grep -E '/gpustack--.*-[0-9]+c$' | head -1)
[ -n "$CPURF" ] || { echo "no CPU ResourceFlavor found — run case-1 first to materialize the chain"; exit 1; }

# Capture the original setting (absent/other → the documented "false" default) and the derived types
# present now, so cleanup restores the setting and removes only what the aware window created.
orig=$(kubectl -n "$NS" get secret gpustack-settings -o jsonpath="{.data.${AWARE_KEY}}" 2>/dev/null | base64 -d 2>/dev/null)
[ "$orig" = "true" ] || orig=false
before_its=" $(derived_its) "

cleanup() {
  echo
  echo "[case-18] cleanup: restoring ${AWARE_KEY}=${orig}"
  set_aware "$orig"
  # Let the setting cache flip back (the catalog re-collapsing is the signal both the aggregated read
  # and the reconciler read have flipped) before removing the aware-named derived types, so the
  # reconciler will not recreate one after we delete it.
  [ "$orig" = "true" ] || wait_generic || true
  for it in $(derived_its); do
    case "$before_its" in
      *" $it "*) : ;;
      *)
        echo "[case-18] removing derived type created during the aware window: ${it}"
        kubectl patch instancetype "$it" --type=merge -p '{"metadata":{"finalizers":[]}}' >/dev/null 2>&1 || true
        kubectl delete instancetype "$it" --wait=false >/dev/null 2>&1 || true
        kubectl delete clusterqueue "$it" --wait=false >/dev/null 2>&1 || true
        ;;
    esac
  done
}
trap cleanup EXIT

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

# --- A. The ResourceFlavor is the finest, double-dash grain and always records its raw CPU detail. ---
rfname=${CPURF#*/}
echo "$rfname" | grep -qE '^gpustack--.+-[0-9]+c$' \
  && record PASS "CPU ResourceFlavor is the finest, double-dash grain" "${rfname}" \
  || record FAIL "CPU ResourceFlavor is the finest, double-dash grain" "${rfname} — expected gpustack--\${gKey}-\${os}-\${arch}-\${count}c"

cpudetail=$(kubectl get "$CPURF" -o json 2>/dev/null | python3 -c "
import json,sys
try: a=json.load(sys.stdin).get('metadata',{}).get('annotations',{}) or {}
except Exception: a={}
v=a.get('note.gpustack.ai/cpuDetail','')
if not v: print('MISSING'); raise SystemExit
try: d=json.loads(v)
except Exception: print('BADJSON'); raise SystemExit
print('OK|manufacturer=%s product=%s'%(d.get('manufacturer','') or '<empty>', d.get('product','') or '<empty>'))
" 2>/dev/null)
case "$cpudetail" in
  OK*) record PASS "CPU ResourceFlavor carries a cpuDetail note" "${cpudetail#OK|}" ;;
  *)   record FAIL "CPU ResourceFlavor carries a cpuDetail note" "${cpudetail:-<none>} — a CPU flavor must always record its raw CPU detail" ;;
esac

# --- B. Awareness OFF collapses the CPU pool to one CPU-agnostic generic row. ---
echo "[case-18] awareness OFF: expecting the catalog to collapse the CPU pool to gpustack--generic"
set_aware false
gname=""
wait_generic && gname=$(generic_row)
[ "$gname" = "gpustack--generic" ] \
  && record PASS "awareness off collapses to one generic row" "${gname} (acceleratable=false, generalGroup=generic)" \
  || record FAIL "awareness off collapses to one generic row" "generic row='${gname:-<none>}' — expected gpustack--generic"

# --- C. Awareness ON splits the CPU pool into a real-CPU-keyed row (generic row gone). ---
echo "[case-18] awareness ON: expecting the generic row to split into a real-CPU-keyed row"
set_aware true
split=""
for _ in $(seq 1 24); do
  split=$(split_row)
  [ -n "$split" ] && [ -z "$(generic_row)" ] && break
  split=""
  sleep 3
done
if [ -n "$split" ]; then
  record PASS "awareness on splits the CPU pool by manufacturer" "${split%%|*} (generalGroup=${split#*|}, generic row gone)"
else
  record FAIL "awareness on splits the CPU pool by manufacturer" "no real-CPU row (or the generic row lingered) — the catalog must recompute per the setting"
fi

echo
echo "== CASE 18 — CPU-manufacturer awareness reshapes the catalog =="
{
  echo "STATUS|CHECK|OBJECT"
  printf '%s\n' "${ROWS[@]}"
} | column -t -s '|'

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). The ResourceFlavor is the finest, setting-independent grain (always"
  echo "carrying cpuDetail); instance-type-aware-cpu-manufacturer collapses (off) or splits (on) only the"
  echo "aggregated InstanceTypeFlavor/InstanceType/ClusterQueue view. See pkg/nodefeature/helper.go and"
  echo "pkg/worker/extensionapis/worker/instance_type_flavor.go."
  exit 1
fi
echo "CASE 18 PASS"
