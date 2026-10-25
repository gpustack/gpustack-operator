#!/usr/bin/env bash
#
# _pool-lib.sh — the shared pool lookup: which pool backs this node.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. It answers one question, and every case that submits a claim
# through a queue must answer it the same way — through the wrong pool's queue the ResourceFlavor
# selects other nodes and a node-pinned Pod is simply never admitted, which reads like a placement
# defect rather than like a lookup mistake.
#
# A case uses it as:
#
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_pool-lib.sh"
#     read -r IT LQ <<<"$(discover_node_pool "$NODE" "$GROUP_ID" --sliceable name entrance)"
#
# A pool BACKS a node when every discriminator the pool carries is a label the node carries too.
# Those are the schedule labels the webhook stamps from PoolScheduleLabels, so this is the pool's
# whole identity — os/arch, the acceleratable boolean, the accelerator group and, under CPU-aware
# grouping, the general group. schedule.gpustack.ai/* is the pool's own bookkeeping and never a node
# label. Matching on a substring of the pool's NAME instead picks a neighbouring pool on any cluster
# whose product tokens nest.

# discover_node_pool <node> <group-id> [--sliceable] [--require-entrance] [--fallback-any] <field>...
#
# Prints the pool that backs <node> on one line: the requested fields, separated by a space, in the
# order asked for. The accelerator group the node's cards belong to is <group-id>; the pool's
# spec.acceleratorGroup must end with it.
#
#   --sliceable         the pool must serve logical slices (status.detail.slicedDetail.logical.count)
#   --require-entrance  the pool must name an entrance LocalQueue
#   --fallback-any      when nothing backs the node, accept any accelerated pool with an entrance
#
# Fields: name, entrance, manufacturer, memory, unit-cpu, unit-ram.
#
# Exit status: 0 a pool was found, 1 no pool backs the node, 2 the InstanceTypes or the Node could
# not be read, 3 the InstanceTypes could not be parsed. The last two mean "cannot tell" and must
# never be read as "no pool": a caller that folds them into an absent pool turns a broken query into
# a clean skip, which a summary table reads as coverage.
discover_node_pool() {
  [ "$#" -ge 2 ] || return 1
  local node="$1" gid="$2"
  shift 2
  local want_sliceable=0 want_entrance=0 fallback_any=0
  local fields=()
  while [ "$#" -gt 0 ]; do
    case "$1" in
      --sliceable) want_sliceable=1 ;;
      --require-entrance) want_entrance=1 ;;
      --fallback-any) fallback_any=1 ;;
      *) fields+=("$1") ;;
    esac
    shift
  done

  local its node_json
  if ! its="$(kubectl get instancetypes.worker.gpustack.ai -o json 2>&1)"; then
    return 2
  fi
  if ! node_json="$(kubectl get node "$node" -o json 2>&1)"; then
    return 2
  fi

  # A Node JSON that does not parse is an instrument failure, not a no-match: the matcher below
  # distinguishes them for the InstanceType half (parse error exits 3), and the Node half must not
  # silently turn a broken read into an empty match - an empty match reads as an environmental
  # SKIP in the caller, and a broken instrument must not SKIP.
  if ! printf '%s' "$node_json" | python3 -c 'import json,sys; json.load(sys.stdin)' 2>/dev/null; then
    return 3
  fi

  printf '%s' "$its" \
    | NODE_GID="$gid" NODE_JSON="$node_json" \
      WANT_SLICEABLE="$want_sliceable" WANT_ENTRANCE="$want_entrance" \
      FALLBACK_ANY="$fallback_any" FIELDS="${fields[*]}" \
      python3 -c '
import json, os, sys

gid = os.environ.get("NODE_GID", "")
node_json = os.environ.get("NODE_JSON") or "{}"
nl = (json.loads(node_json).get("metadata", {}) or {}).get("labels", {}) or {}
fields = (os.environ.get("FIELDS") or "").split()
want_sliceable = os.environ.get("WANT_SLICEABLE") == "1"
want_entrance = os.environ.get("WANT_ENTRANCE") == "1"
fallback_any = os.environ.get("FALLBACK_ANY") == "1"


def backs(it):
    d = {k: v for k, v in (it["metadata"].get("labels") or {}).items()
         if not k.startswith("schedule.gpustack.ai/")}
    return bool(d) and all(nl.get(k) == v for k, v in d.items())


def in_group(s):
    return bool(gid) and (s.get("acceleratorGroup") or "").endswith("-" + gid)


def sliceable(st):
    sd = (st.get("detail") or {}).get("slicedDetail") or {}
    return ((sd.get("logical") or {}).get("count", 0) or 0) > 0


def emit(it):
    st = it.get("status", {}) or {}
    d = st.get("detail") or {}
    u = (it.get("spec", {}) or {}).get("unitResources") or {}
    vals = {
        "name": it["metadata"]["name"],
        "entrance": st.get("entrance", ""),
        "manufacturer": d.get("manufacturer", ""),
        "memory": d.get("memory", ""),
        "unit-cpu": u.get("cpu", "") or "-",
        "unit-ram": u.get("ram", "") or "-",
    }
    print(" ".join(vals.get(f, "") for f in fields))


try:
    items = json.load(sys.stdin).get("items", [])
except Exception:
    sys.exit(3)

for it in items:
    s = it.get("spec", {}) or {}
    st = it.get("status", {}) or {}
    if not s.get("acceleratable"):
        continue
    if want_entrance and not st.get("entrance"):
        continue
    if want_sliceable and not sliceable(st):
        continue
    if not (in_group(s) and backs(it)):
        continue
    emit(it)
    sys.exit(0)

if fallback_any:
    for it in items:
        s = it.get("spec", {}) or {}
        st = it.get("status", {}) or {}
        if s.get("acceleratable") and st.get("entrance"):
            emit(it)
            sys.exit(0)

sys.exit(1)
'
}
