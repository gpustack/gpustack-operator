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
# Goal:        Report, for every cases/case-N.sh (1..37), whether the cluster in front of us
#              satisfies the Prerequisites its own header declares, without running a single case.
# Environment: Any cluster, or none. The cluster is read once per resource, with read-only kubectl
#              get calls only: the node inventory, the operator's Devices ledger, the Mock Device
#              Manager DaemonSet, the store objects, and the objects in the operator's namespace.
#              With no reachable cluster every row is UNKNOWN — the script reports that it cannot
#              tell, and never fails on it.
# Inputs:      <NS> — the operator's namespace, read once to confirm the operator and any mock
#              device-manager DaemonSet are present. Nothing is written, and no case is run.
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

MOCK_DS_JSON=""
if ! MOCK_DS_JSON="$(kubectl -n "$NS" get daemonset \
  -l app.kubernetes.io/component=device-manager,gpustack.ai/manufacturer=mock \
  -o json 2>/dev/null)"; then
  MOCK_DS_JSON=""
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
  FACTS="$(NODES_ENV="$NODES_JSON" DEVICES_ENV="$DEVICES_JSON" MOCK_DS_ENV="$MOCK_DS_JSON" python3 -c '
import json, os

def load_obj(key):
    raw = os.environ.get(key) or ""
    if not raw:
        return {}
    try:
        return json.loads(raw)
    except Exception:
        return {}

nodes = load_obj("NODES_ENV")
devices_doc = load_obj("DEVICES_ENV")
mock_ds_doc = load_obj("MOCK_DS_ENV")

CONTROL = ("node-role.kubernetes.io/control-plane", "node-role.kubernetes.io/master")
ALL_KNOWN_VENDORS = (
    "amd", "ascend", "cambricon", "hygon", "iluvatar", "metax", "mthreads", "nvidia", "thead"
)
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

dev_items = devices_doc.get("items")
if dev_items is None and devices_doc.get("spec"):
    dev_items = [devices_doc]
dev_items = dev_items or []

mock_nodes = set()
real_dev_vendors = set()
mock_dev_vendors = set()
ledger_partition = 0
for d in dev_items:
    md = d.get("metadata") or {}
    ann = md.get("annotations") or {}
    is_mock = ann.get("device.gpustack.ai/mock") == "true"
    if is_mock and md.get("name"):
        mock_nodes.add(md["name"])
    for g in (d.get("spec") or {}).get("groups") or []:
        mfr = g.get("manufacturer") or ""
        accs = g.get("accelerators") or []
        if mfr and accs:
            if is_mock:
                mock_dev_vendors.add(mfr)
            else:
                real_dev_vendors.add(mfr)
        for a in accs:
            profs = ((a.get("status") or {}).get("physicalSliced") or {}).get("profiles") or []
            if profs:
                ledger_partition = 1

mock_ds_ready = 0
mock_ds_vendors = set()
for ds in mock_ds_doc.get("items") or []:
    st = ds.get("status") or {}
    if int(st.get("numberReady") or 0) >= 1:
        mock_ds_ready = 1
        ctrs = (((ds.get("spec") or {}).get("template") or {}).get("spec") or {}).get("containers") or []
        args = (ctrs[0].get("args") or []) if ctrs else []
        mfrs = [a.split("=", 1)[1] for a in args if a.startswith("--manufacturer=")]
        if mfrs:
            for entry in mfrs:
                for tok in entry.split(","):
                    tok = tok.strip()
                    if tok:
                        mock_ds_vendors.add(tok)
        else:
            mock_ds_vendors.update(ALL_KNOWN_VENDORS)

ready = 0
kinds = 0
images = []
rdma = 0
efa = 0
partition = 0
node_real_vendors = set()
node_mock_vendors = set()
for n in nodes.get("items") or []:
    md = n.get("metadata") or {}
    labels = md.get("labels") or {}
    taints = (n.get("spec") or {}).get("taints") or []
    cp_tainted = any(
        t.get("key") in CONTROL and t.get("effect") in ("NoSchedule", "NoExecute")
        for t in taints
    )
    if any(c in labels for c in CONTROL) and cp_tainted:
        continue
    spec = n.get("spec") or {}
    status = n.get("status") or {}
    conds = status.get("conditions") or []
    is_ready = any(c.get("type") == "Ready" and c.get("status") == "True" for c in conds)
    if not is_ready or spec.get("unschedulable"):
        continue
    ready += 1
    name = md.get("name") or ""
    provider_id = spec.get("providerID") or ""
    if name.startswith("kind-") or provider_id.startswith("kind://"):
        kinds += 1
    for img in status.get("images") or []:
        for iname in img.get("names") or []:
            images.append(iname)
    is_mock_node = name in mock_nodes
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
                if is_mock_node:
                    node_mock_vendors.add(vendor)
                else:
                    node_real_vendors.add(vendor)

real_vendors = sorted(real_dev_vendors | node_real_vendors)
all_vendors = sorted(set(real_vendors) | mock_dev_vendors | node_mock_vendors | mock_ds_vendors)
display_vendors = sorted(set(real_vendors) | mock_dev_vendors | node_mock_vendors)
if not display_vendors and mock_ds_ready:
    display_vendors = ["mock"]

print("READY=%d" % ready)
print("KIND=%d" % kinds)
print("RDMA=%d" % rdma)
print("EFA=%d" % efa)
print("PARTITION=%d" % (1 if (partition or ledger_partition) else 0))
print("MOCK_DM=%d" % (1 if (mock_ds_ready or mock_nodes) else 0))
print("REAL_VENDORS=%s" % ",".join(real_vendors))
print("ALL_VENDORS=%s" % ",".join(all_vendors))
print("DISPLAY_VENDORS=%s" % ",".join(display_vendors))
print("IMAGES=%s" % ",".join(sorted(set(images))))
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
HAVE_PARTITION="$(fact PARTITION)"
REAL_VENDORS="$(fact REAL_VENDORS)"
ALL_VENDORS="$(fact ALL_VENDORS)"
DISPLAY_VENDORS="$(fact DISPLAY_VENDORS)"
NODE_IMAGES="$(fact IMAGES)"

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

# vendor_present <wanted> <available_csv> — whether the cluster offers a vendor the case can use.
# The wanted list is "|"-separated, so a case that runs against several vendors matches any
# of them. Returns 0 when the cluster offers one, 1 when it offers none of them, and 2 when
# the question cannot be asked.
vendor_present() {
  local want="${1:-}"
  local avail="${2:-}"
  [ -n "$want" ] || return 2
  [ -n "$avail" ] || return 1
  local have
  local IFS=','
  for have in $avail; do
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

  # Real-cluster GPU cases require physical vendor devices; cluster=any cases (such as Mock DM
  # cases 4 and 5) accept either real vendor devices or an active Mock Device Manager.
  eligible_vendors="$ALL_VENDORS"
  if [ "$want_cluster" = "real" ]; then
    eligible_vendors="$REAL_VENDORS"
  fi

  have_gpu="none"
  if [ "$want_cluster" = "real" ]; then
    [ -n "$REAL_VENDORS" ] && have_gpu="$REAL_VENDORS"
  elif [ -n "$DISPLAY_VENDORS" ]; then
    have_gpu="$DISPLAY_VENDORS"
  fi
  if [ -z "$READY_WORKERS" ]; then
    have_gpu="unknown"
  fi

  if [ "$want_gpu" != "none" ]; then
    if [ -z "$READY_WORKERS" ]; then
      verdict="UNKNOWN"
      undecided="${undecided}${undecided:+,}gpu"
    elif [ "$want_gpu" = "any" ]; then
      if [ -z "$eligible_vendors" ]; then
        missing="${missing}${missing:+,}gpu"
      fi
    else
      vendor_present "$want_gpu" "$eligible_vendors"
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
  "${READY_WORKERS:-unknown}" "${DISPLAY_VENDORS:-none}" "$OPERATOR_STATE" "$NS"
