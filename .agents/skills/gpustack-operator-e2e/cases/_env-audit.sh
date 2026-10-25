#!/usr/bin/env bash
#
# _env-audit.sh — read the cluster once and report, per case, whether this cluster can run it.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no
# results table and no trap. It is the one helper in this directory that IS run directly —
# `bash cases/_env-audit.sh <NS>` — because it answers a question about the cluster rather than
# about a fixture.
#
# Prerequisites: nodes=1; gpu=none; cluster=any
# Goal:        Report, for every cases/case-N.sh, whether the cluster in front of us satisfies the
#              Prerequisites its own header declares, without running a single case.
# Environment: Any cluster, or none. The cluster is read once per resource, with read-only kubectl
#              get calls only: the node inventory, the operator's Devices ledger, the store objects
#              and the objects in the operator's namespace. With no reachable cluster every row is
#              UNKNOWN — the script reports that it cannot tell, and never fails on it.
# Inputs:      <NS> — the operator's namespace, read once to confirm the operator is present.
#              Nothing is written, and no case is run.
# Expected:    One line per case, in case-number order:
#                CASE <N> | RUN|SKIP|UNKNOWN | nodes <have>/<want> | gpu <have>/<want> | features <list>
#              <list> names the features the cluster demonstrably lacks; "ok" means it lacks none,
#              and a "?"-prefixed name is a feature this audit could not decide. A feature nobody
#              has ever pulled (a store image, say) is undecidable rather than missing, so it reads
#              "?" and the row reads UNKNOWN. A CLUSTER line closes the report.
# Cleanup:     None — read-only, no trap.
#
# The verdict is about the cluster only. A case can be RUN and still skip on a runtime check it
# makes itself; this audit says the cluster could host the case, not that the case will pass.

set -uo pipefail

NS="${1:?usage: bash cases/_env-audit.sh <operator-namespace>}"

HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CASES_DIR="$HERE"

trim() {
  # trim <string> — the string without its leading and trailing whitespace.
  local s="${1:-}"
  s="${s#"${s%%[![:space:]]*}"}"
  s="${s%"${s##*[![:space:]]}"}"
  printf '%s' "$s"
}

# --- the cluster, read once per resource -------------------------------------------------
#
# A failed kubectl leaves stdout empty, which is how a missing cluster is told apart from a
# cluster with no nodes: an empty inventory still parses as JSON.

NODES_JSON=""
if ! NODES_JSON="$(kubectl get nodes -o json 2>/dev/null)"; then
  NODES_JSON=""
fi

DEVICES_JSON=""
if ! DEVICES_JSON="$(kubectl get devices.worker.gpustack.ai -o json 2>/dev/null)"; then
  DEVICES_JSON=""
fi

# The store image is the one feature an image pull would decide and no read proves. It is looked
# for where a pulled image shows up: a node's own image list, and the images of running Pods.
STORE_IMAGES=""
STORE_OBJECTS=0
if store_objs="$(kubectl get kvcachebackends.kvcache.worker.gpustack.ai -o json 2>/dev/null)" &&
  [ -n "$store_objs" ]; then
  STORE_OBJECTS="$(printf '%s' "$store_objs" |
    python3 -c 'import json,sys; print(len(json.load(sys.stdin).get("items",[])))' 2>/dev/null || echo 0)"
fi
if pod_imgs="$(kubectl get pods -A \
  -o jsonpath='{range .items[*]}{range .spec.initContainers[*]}{.image}{"\n"}{end}{range .spec.containers[*]}{.image}{"\n"}{end}{end}' \
  2>/dev/null)"; then
  STORE_IMAGES="$(printf '%s\n' "$pod_imgs" | grep -i mooncake | tr '\n' ',' | sed 's/,$//')"
fi

# The operator's namespace, read once. An empty namespace and a missing one are different
# answers, so the two are told apart rather than collapsed.
OPERATOR_STATE="unknown"
if operator_objects="$(kubectl -n "$NS" get deployments,pods -o name 2>/dev/null)"; then
  if [ -n "$operator_objects" ]; then
    OPERATOR_STATE="present"
  else
    OPERATOR_STATE="empty"
  fi
else
  OPERATOR_STATE="missing"
fi

# The facts the probes below read, extracted once so the probes themselves stay small.
FACTS=""
if [ -n "$NODES_JSON" ]; then
  FACTS="$(printf '%s' "$NODES_JSON" | python3 -c '
import json,sys

try:
    nodes = json.load(sys.stdin)
except Exception:
    nodes = {}

CONTROL = ("node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master")

# The accelerator vendors, from the node capacity keys. The operator names a vendor in the
# resource family, so the family is what identifies it.
VENDOR_KEYS = {
    "nvidia.com/": "nvidia",
    "amd.com/": "amd",
    "huawei.com/": "ascend",
    "hygon.com/": "hygon",
    "alibabacloud.com/": "thead",
    "cambricon.com/": "cambricon",
    "iluvatar.com/": "iluvatar",
    "metax-tech.com/": "metax",
    "mthreads.com/": "mthreads",
}

ready = 0
kinds = 0
images = []
rdma = 0
efa = 0
partition = 0
vendors = set()
for n in nodes.get("items") or []:
    md = n.get("metadata") or {}
    labels = md.get("labels") or {}
    if any(c in labels for c in CONTROL):
        continue
    status = n.get("status") or {}
    ready += 1
    name = md.get("name") or ""
    if name.startswith("kind-"):
        kinds += 1
    for img in status.get("images") or []:
        for iname in img.get("names") or []:
            images.append(iname)
    for key, val in (status.get("capacity") or {}).items():
        try:
            qty = int(val)
        except (TypeError, ValueError):
            qty = 0
        if qty <= 0:
            continue
        if key.startswith("device.gpustack.ai/rdma"):
            rdma = 1
        if key == "vpc.amazonaws.com/efa" or key.startswith("efa."):
            efa = 1
        if (".partitioned." in key or key.endswith(".partitioned")) and not key.endswith(".units"):
            partition = 1
        for prefix, vendor in VENDOR_KEYS.items():
            if key.startswith(prefix):
                vendors.add(vendor)

print("READY=%d" % ready)
print("KIND=%d" % kinds)
print("RDMA=%d" % rdma)
print("EFA=%d" % efa)
print("PARTITION=%d" % partition)
print("VENDORS=%s" % ",".join(sorted(vendors)))
print("IMAGES=%s" % ",".join(sorted(set(images))))
' 2>/dev/null)"
fi

if [ -n "$DEVICES_JSON" ]; then
  FACTS="${FACTS}"$'\n'"$(printf '%s' "$DEVICES_JSON" | python3 -c '
import json,sys

try:
    devices = json.load(sys.stdin)
except Exception:
    devices = {}

# A card that reports hardware partition profiles in the ledger, whether or not a partition
# instance exists on the node yet.
partition = 0
for g in (devices.get("spec") or {}).get("groups") or []:
    for a in g.get("accelerators") or []:
        profs = (a.get("status") or {}).get("physicalSliced") or {}
        if profs.get("profiles"):
            partition = 1

print("LEDGER_PARTITION=%d" % partition)
' 2>/dev/null)"
fi

fact() {
  # fact <key> — one value out of the extracted cluster facts.
  printf '%s\n' "$FACTS" | sed -n "s/^$1=//p" | head -1
}

READY_WORKERS="$(fact READY)"
KIND_NODES="$(fact KIND)"
HAVE_RDMA="$(fact RDMA)"
HAVE_EFA="$(fact EFA)"
HAVE_VENDORS="$(fact VENDORS)"
NODE_IMAGES="$(fact IMAGES)"

# A partition-capable card is reported either by the node's own partitioned resource keys or by
# the ledger's partition profiles. Either source answering yes is enough; when neither source
# could be read the answer is unknown rather than no.
NODE_PARTITION="$(fact PARTITION)"
LEDGER_PARTITION="$(fact LEDGER_PARTITION)"
HAVE_PARTITION=""
if [ "$NODE_PARTITION" = "1" ] || [ "$LEDGER_PARTITION" = "1" ]; then
  HAVE_PARTITION=1
elif [ -n "$NODE_PARTITION" ] || [ -n "$LEDGER_PARTITION" ]; then
  HAVE_PARTITION=0
fi

# The store image, from the nodes' own image lists and from the Pods' images.
HAVE_STORE=""
if printf '%s' "$NODE_IMAGES" | grep -qi mooncake; then
  HAVE_STORE=1
elif [ -n "$STORE_IMAGES" ]; then
  HAVE_STORE=1
elif [ "${STORE_OBJECTS:-0}" -gt 0 ] 2>/dev/null; then
  # The store is configured but no mooncake image is visible on any node or pod. The header
  # contract says this is undecidable, not absent: an image nobody has pulled is not evidence
  # the feature is missing, so the probe answers UNKNOWN rather than SKIP.
  HAVE_STORE=""
fi

# --- the case headers --------------------------------------------------------------------

PR_NODES=""
PR_GPU=""
PR_FEATURES=""
PR_CLUSTER=""

parse_prereqs() {
  # parse_prereqs <raw value> — fill the PR_* globals from one Prerequisites value.
  PR_NODES=""
  PR_GPU=""
  PR_FEATURES=""
  PR_CLUSTER=""
  local raw="${1:-}"
  local part key val
  local -a parts=()
  IFS=';' read -r -a parts <<< "$raw"
  for part in "${parts[@]}"; do
    part="$(trim "$part")"
    [ -n "$part" ] || continue
    key="$(trim "${part%%=*}")"
    val="$(trim "${part#*=}")"
    case "$key" in
      nodes) PR_NODES="$val" ;;
      gpu) PR_GPU="$val" ;;
      features) PR_FEATURES="$val" ;;
      cluster) PR_CLUSTER="$val" ;;
      *) ;;
    esac
  done
}

# feature_state <feature> — echo present, absent or unknown for one feature.
feature_state() {
  case "$1" in
    multi-host)
      if [ -z "$READY_WORKERS" ]; then echo unknown
      elif [ "$READY_WORKERS" -ge 2 ]; then echo present
      else echo absent
      fi
      ;;
    kind)
      if [ -z "$KIND_NODES" ]; then echo unknown
      elif [ "$KIND_NODES" -ge 1 ]; then echo present
      else echo absent
      fi
      ;;
    rdma)
      if [ -z "$HAVE_RDMA" ]; then echo unknown
      elif [ "$HAVE_RDMA" -ge 1 ]; then echo present
      else echo absent
      fi
      ;;
    efa)
      if [ -z "$HAVE_EFA" ]; then echo unknown
      elif [ "$HAVE_EFA" -ge 1 ]; then echo present
      else echo absent
      fi
      ;;
    mig|partition)
      if [ -z "$HAVE_PARTITION" ]; then echo unknown
      elif [ "$HAVE_PARTITION" -ge 1 ]; then echo present
      else echo absent
      fi
      ;;
    mooncake-store)
      case "$HAVE_STORE" in
        1) echo present ;;
        0) echo absent ;;
        *) echo unknown ;;
      esac
      ;;
    *) echo unknown ;;
  esac
}

# vendor_present <wanted> — whether the cluster offers a vendor the case can use.
# The wanted list is "|"-separated, so a case that runs against several vendors matches any
# of them. Returns 0 when the cluster offers one, 1 when it offers none of them, and 2 when
# the question cannot be asked.
vendor_present() {
  local want="${1:-}"
  [ -n "$want" ] || return 2
  [ -n "$HAVE_VENDORS" ] || return 1
  local have
  local IFS=','
  for have in $HAVE_VENDORS; do
    case "|$want|" in
      *"|$have|"*) return 0 ;;
    esac
  done
  return 1
}

# --- one row per case --------------------------------------------------------------------

echo "CASE     | VERDICT | nodes             | gpu                 | features"

# The cases in case-number order, which is not the order a glob hands them over.
case_list=""
for case_file in "$CASES_DIR"/case-*.sh; do
  [ -f "$case_file" ] || continue
  base="$(basename "$case_file")"
  num="${base#case-}"
  num="${num%.sh}"
  case_list="${case_list}${num}"$'\t'"${case_file}"$'\n'
done
case_list="$(printf '%s' "$case_list" | sort -n -k1,1)"

while IFS=$'\t' read -r num case_file; do
  [ -n "$num" ] || continue

  raw=""
  while IFS= read -r line; do
    case "$line" in
      '# Prerequisites: '*) raw="${line#\# Prerequisites: }" ;;
    esac
  done < <(sed -n '1,/^set -/p' "$case_file")

  if [ -z "$raw" ]; then
    printf 'CASE %-4s | %-7s | nodes %-11s | gpu %-17s | features %s\n' \
      "$num" "UNKNOWN" "unknown" "unknown" "?prerequisites"
    continue
  fi

  parse_prereqs "$raw"

  want_nodes="${PR_NODES:-1}"
  want_gpu="${PR_GPU:-none}"
  want_features="${PR_FEATURES:-}"
  want_cluster="${PR_CLUSTER:-any}"

  missing=""
  undecided=""
  verdict="RUN"

  # Nodes: the count of Ready worker nodes against the count the case asks for.
  have_nodes="${READY_WORKERS:-unknown}"
  if [ -z "$READY_WORKERS" ]; then
    verdict="UNKNOWN"
  elif [ "$READY_WORKERS" -lt "$want_nodes" ]; then
    missing="nodes"
  fi

  # The accelerator vendor, when the case wants one.
  have_gpu="none"
  if [ -n "$HAVE_VENDORS" ]; then
    have_gpu="$HAVE_VENDORS"
  elif [ -z "$READY_WORKERS" ]; then
    have_gpu="unknown"
  fi
  # The accelerator vendor, when the case wants one. "any" means whatever the cluster offers,
  # so it only fails on a cluster with no accelerator at all.
  if [ "$want_gpu" != "none" ]; then
    if [ -z "$READY_WORKERS" ]; then
      verdict="UNKNOWN"
      undecided="${undecided}${undecided:+,}gpu"
    elif [ "$want_gpu" = "any" ]; then
      if [ -z "$HAVE_VENDORS" ]; then
        missing="${missing}${missing:+,}gpu"
      fi
    else
      vendor_present "$want_gpu"
      case $? in
        0) ;;
        1) missing="${missing}${missing:+,}gpu" ;;
        *) verdict="UNKNOWN"; undecided="${undecided}${undecided:+,}gpu" ;;
      esac
    fi
  fi

  # The cluster kind, when the case pins one. An unreadable cluster decides nothing: the
  # requirement is undecided rather than missed.
  if [ -z "$READY_WORKERS" ]; then
    if [ "$want_cluster" != "any" ]; then
      verdict="UNKNOWN"
      undecided="${undecided}${undecided:+,}cluster"
    fi
  elif [ "$want_cluster" = "kind" ] && [ "${KIND_NODES:-0}" -lt 1 ]; then
    missing="${missing}${missing:+,}cluster"
  elif [ "$want_cluster" = "real" ] && [ "${KIND_NODES:-0}" -ge 1 ]; then
    missing="${missing}${missing:+,}cluster"
  fi

  # The features, one probe each.
  if [ -n "$want_features" ]; then
    IFS=',' read -r -a feature_list <<< "$want_features"
    for feat in "${feature_list[@]}"; do
      feat="$(trim "$feat")"
      [ -n "$feat" ] || continue
      state="$(feature_state "$feat")"
      case "$state" in
        present) ;;
        absent) missing="${missing}${missing:+,}$feat" ;;
        *) undecided="${undecided}${undecided:+,}$feat" ;;
      esac
    done
  fi

  # A definite miss decides the row; an undecidable one only keeps a row from being a RUN.
  if [ -n "$missing" ]; then
    verdict="SKIP"
  elif [ -n "$undecided" ] || [ "$verdict" = "UNKNOWN" ]; then
    verdict="UNKNOWN"
  fi

  feature_cell="$missing"
  if [ -n "$undecided" ]; then
    undecided_list=""
    IFS=',' read -r -a undecided_list <<< "$undecided"
    for feat in "${undecided_list[@]}"; do
      feat="$(trim "$feat")"
      [ -n "$feat" ] || continue
      if [ -n "$feature_cell" ]; then
        feature_cell="${feature_cell},?${feat}"
      else
        feature_cell="?${feat}"
      fi
    done
  fi
  [ -n "$feature_cell" ] || feature_cell="ok"

  printf 'CASE %-4s | %-7s | nodes %-11s | gpu %-17s | features %s\n' \
    "$num" "$verdict" "$have_nodes/$want_nodes" "$have_gpu/$want_gpu" "$feature_cell"
done <<< "$case_list"

printf 'CLUSTER  | ready %-6s | vendors %-17s | operator %-10s | nodes %s\n' \
  "${READY_WORKERS:-unknown}" "${HAVE_VENDORS:-unknown}" "$OPERATOR_STATE" "$NS"
