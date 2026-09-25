#!/usr/bin/env bash
#
# _rdma-lib.sh — shared precondition gating, host probing and ledger discovery for the RDMA cases.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Every one of those stays with the case that sources it, so a
# case still reads end to end on its own terms; this file only removes five copies of the same node
# selection, host probe and precondition report.
#
# A case uses it as:
#
#     set -uo pipefail
#     NS="${1:?usage: ...}"
#     CASE_ID=80
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rdma-lib.sh"
#     rdma_select endpoint endpoint-whole      # picks RDMA_NODE, or skips naming what every node lacked
#     rdma_probe_start                         # a host reading the ledger cannot supply
#     ...checks, each `record`ing one row...
#     rdma_results
#
# ------------------------------------------------------------------------------------------------
# WHY A PRECONDITION GATE IS THE CENTRE OF THIS FILE
#
# Every one of these cases can be made to report a clean run on a machine that cannot exercise the
# mechanism it is about, and in each case the green comes from the SAME shape: the assertion compares
# two values that agree for a reason that has nothing to do with the code.
#
#   - NUMA alignment on a single-socket host. The accelerator and the interface are on the one NUMA
#     node the machine has, so they agree with the TopologyManager switched off entirely. The check
#     passes; nothing was aligned.
#   - NUMA alignment under the `none` policy. The hint is computed and discarded, and the placement
#     is whatever the allocator happened to pick — which on most hosts is the same node anyway.
#   - The virtual-function counts on a host with no virtual functions configured. Zero equals zero.
#   - The whole-function counts on a host whose every interface is a partitioned physical function.
#     Zero equals zero again, from the opposite branch.
#   - A transport check on an Ethernet-attached RoCE adapter, standing in for what classic InfiniBand
#     additionally needs. The verbs library opens the same node either way.
#
# So a requirement here is never "the cluster has RDMA". It is a POSITIVE, DISCRIMINATING reading:
# a value that differs between a host that can answer and a host that cannot. Where no such reading
# exists the requirement is not written — the row is recorded as unanswerable instead, which is the
# one honest outcome for a question no machine in reach can put.
# ------------------------------------------------------------------------------------------------

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

CASE_ID="${CASE_ID:-?}"

# Where the test and probe Pods go. `default` rather than the operator's namespace, matching the
# sibling accelerator cases: the operator namespace carries the components under test and a Pod of
# ours competing for a node there is a variable this suite does not want.
RDMA_TEST_NS="${RDMA_TEST_NS:-default}"

# The probe image. It needs a shell, `ls`, `cat` and a working `exec <>` redirection and nothing
# else — every reading this image takes is out of sysfs or /dev. It is deliberately NOT the image
# any verbs check runs in: those need rdma-core and are named separately, so a cluster that can take
# the host readings is not also required to have an RDMA userspace image.
RDMA_PROBE_IMAGE="${E2E_RDMA_PROBE_IMAGE:-busybox:1.36}"

# How long a Pod this library creates gets to reach Running before the wait gives up. A grant is
# recorded during Allocate, which runs before the container does, so a lower bound here turns a
# verdict about the allocator into one about image-pull latency.
RDMA_POD_TIMEOUT="${E2E_RDMA_POD_TIMEOUT:-180}"

FAILS=0
PASSES=0
SKIPS=0
ROWS=()
TESTPODS=()
RDMA_PROBE_READY=0
RDMA_PROBE_SKIP_REASON=""

# The rows are joined with ASCII UNIT SEPARATOR rather than a pipe. A pipe is correct only while no
# check name or object text ever contains one, which is a property of today's strings rather than of
# the code; 0x1f cannot appear in a node name, a device name or a link reason.
ROW_SEP=$'\x1f'

record() {
  ROWS+=("$1${ROW_SEP}$2${ROW_SEP}$3")
  case "$1" in
  FAIL) FAILS=$((FAILS + 1)) ;;
  PASS) PASSES=$((PASSES + 1)) ;;
  SKIP) SKIPS=$((SKIPS + 1)) ;;
  esac
  return 0
}

# rdma_results — the results table, the counts and the exit code.
#
# Zero PASSes exits non-zero even with zero FAILs. Distinguishing only FAIL>0 from FAIL==0 reports a
# run in which every check skipped as a green run, and on this family that is the likely outcome
# rather than the unlikely one: most of these checks are gated on hardware, and a case that reached
# its table with nothing asserted has answered nothing. The marker line is what the block runner
# greps for, so the outcome survives being read from a file after a compaction.
rdma_results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  if [ "${#ROWS[@]}" -gt 0 ]; then
    for r in "${ROWS[@]}"; do
      IFS="$ROW_SEP" read -r st ck ob <<<"$r"
      printf '%s | %s | %s\n' "$st" "$ck" "$ob"
    done
  fi
  if [ "$FAILS" -ne 0 ]; then
    echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED (${PASSES} passed, ${SKIPS} skipped)"
    exit 1
  fi
  if [ "$PASSES" -eq 0 ]; then
    echo "[case-${CASE_ID}] NOTHING WAS VERIFIED: 0 passed, 0 failed, ${SKIPS} skipped"
    exit 1
  fi
  echo "[case-${CASE_ID}] all checks passed (${PASSES} passed, ${SKIPS} skipped)"
}

# rdma_skip <line...> — the whole case self-skips: this cluster does not carry what the case needs.
#
# Exit 0 is the suite's auto-skip convention and is kept, but the banner always carries the literal
# NOTHING WAS VERIFIED so a reader — and `run-rdma-block.sh`, which greps for it — can tell a skipped
# case from a passing one without reading the prose. A skip that looks like a pass is the failure
# this whole family is built to avoid; it must not be reintroduced by the exit code.
rdma_skip() {
  echo
  echo "[case-${CASE_ID}] SKIPPED — NOTHING WAS VERIFIED"
  local line
  for line in "$@"; do
    echo "           ${line}"
  done
  exit 0
}

# read_failed ends the case when a read could not be MADE, as opposed to a read that succeeded and
# found nothing. Call it with $? immediately after the assignment it guards.
#
# Every assertion below compares strings, and a failed kubectl substitutes an empty one — so two
# failed reads compare EQUAL and record a PASS that was never observed. Emptiness is a legitimate
# answer for some of these fields and never an acceptable stand-in for a failure, so the status has
# to be read separately from the value.
read_failed() {
  [ "$1" -eq 0 ] && return 0
  echo "[case-${CASE_ID}] ERROR: $2 could not be read (exit $1)."
  echo "           Ending the case: an unread value is not an empty one, and the checks below"
  echo "           compare values — two unread ones would compare equal and pass."
  exit 1
}

# ------------------------------------------------------------------------------------------------
# The ledger facts every requirement and every check reads.
# ------------------------------------------------------------------------------------------------

# The aggregated view is what an unversioned `kubectl get devices.worker.gpustack.ai` resolves to,
# and it is read-only: a write has to name the stored version, `devices.v1alpha1.worker.gpustack.ai`.
# Nothing in this family writes a Devices object, so every read here goes through the aggregated one.
rdma_devices_json() {
  kubectl get devices.worker.gpustack.ai "$1" -o json 2>/dev/null
}

rdma_nodes() {
  kubectl get devices.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null
}

# The accelerator resource key of a manufacturer, in the exclusive (whole-accelerator) family.
#
# This map is a SECOND COPY of the one in pkg/nodefeature/knowns.go, and it is a copy because a
# shell case cannot import Go. Add a manufacturer there and add it here in the same change: a name
# missing here does not fail, it makes the accelerator half of a case skip with "no accelerator
# resource key", which reads like a cluster without accelerators.
rdma_accelerator_key() {
  case "$1" in
  amd) echo "amd.com/gpu" ;;
  ascend) echo "huawei.com/npu" ;;
  cambricon) echo "cambricon.com/mlu" ;;
  hygon) echo "hygon.com/dcu" ;;
  iluvatar) echo "iluvatar.com/gpu" ;;
  metax) echo "metax-tech.com/gpu" ;;
  mthreads) echo "mthreads.com/gpu" ;;
  nvidia) echo "nvidia.com/gpu" ;;
  thead) echo "alibabacloud.com/ppu" ;;
  *) echo "" ;;
  esac
}

# The three RDMA keys this operator advertises. The `.sliced` name is listed separately below and is
# never expected to be served: it was retired, and an extended resource that has once entered a
# node's status is not removed when the plugin stops serving it — so a node that ever ran the older
# build carries it at zero forever, and a node that never did carries no such key at all. Both are
# correct; only a NON-ZERO value is a finding.
RDMA_KEY_WHOLE="device.gpustack.ai/rdma"
RDMA_KEY_SHARED="device.gpustack.ai/rdma.shared"
RDMA_KEY_PARTITIONED="device.gpustack.ai/rdma.partitioned"
RDMA_KEY_SLICED_RETIRED="device.gpustack.ai/rdma.sliced"

# How many concurrent holders one endpoint is advertised for in the shared family. This is RDMA's
# own ceiling, not the accelerator sharing constant: the two were once the same symbol and were
# separated precisely because nothing about an HCA followed from how many owners an accelerator can
# be split among. Overridable so a build that moves the ceiling can still be measured against it,
# but the default is the shipped number and a mismatch is a finding, not a configuration difference.
RDMA_SHARED_POOL_SIZE="${E2E_RDMA_SHARED_POOL_SIZE:-64}"

# rdma_facts <node> — read the node's ledger and node object once, and set the globals every
# requirement and most checks read. Ends the case on an unreadable object rather than reporting a
# verdict about a node nobody could read.
#
# The endpoint model mirrors the implementation rather than approximating it, and the two places it
# would be easy to get wrong are the two this family is about:
#
#   - an SR-IOV physical function with virtual functions configured serves the PARTITIONED family
#     only, its virtual functions being its endpoints; every other interface serves the whole-
#     function families, itself the one endpoint. Reading the virtual-function count alone, or the
#     SR-IOV flag alone, puts one real host shape in the wrong branch;
#   - an endpoint is USABLE when its link verdict is anything but `failed`, falling back to the
#     bound flag only when there is no verdict at all. Testing the flag first reads "this port could
#     not be interrogated" as "this node has no RDMA".
rdma_facts() {
  local node="$1" json facts
  json=$(rdma_devices_json "$node")
  read_failed $? "the Devices object of ${node}"
  [ -n "$json" ] || read_failed 1 "the Devices object of ${node}"

  facts=$(printf '%s' "$json" | python3 -c '
import json, sys

d = json.load(sys.stdin)
spec = d.get("spec", {}) or {}

endpoints = []          # (kind, iface, name, rdma_device, numa, link_state)
sriov_ifaces = 0
whole_ifaces = 0
# Every RDMA device name the inventory carries, endpoint or not. A partitioned physical function is
# not an endpoint, but the detector still records its own rdmaDevice and the host still lists it, so
# the host-side correspondence must read this set rather than the endpoint names.
rdma_names = set()
vf_total = 0            # virtual functions under partitioned physical functions, rdmaDevice or not
for i in spec.get("interfaces", []) or []:
    vfs = i.get("virtualFunctions", []) or []
    if i.get("rdmaDevice"):
        rdma_names.add(i.get("rdmaDevice"))
    for vf in vfs:
        if vf.get("rdmaDevice"):
            rdma_names.add(vf.get("rdmaDevice"))
    partitioned = bool(i.get("sriov")) and len(vfs) > 0
    if partitioned:
        sriov_ifaces += 1
        vf_total += len(vfs)
        for vf in vfs:
            if not vf.get("rdmaDevice"):
                continue
            endpoints.append(("vf", i.get("name", ""), vf.get("name", ""),
                              vf.get("rdmaDevice", ""),
                              vf.get("numaAffinity") or i.get("numaAffinity") or "",
                              ((vf.get("link") or {}).get("state") or "")))
    else:
        whole_ifaces += 1
        if i.get("rdmaDevice"):
            endpoints.append(("whole", i.get("name", ""), i.get("name", ""),
                              i.get("rdmaDevice", ""),
                              i.get("numaAffinity") or "",
                              ((i.get("link") or {}).get("state") or "")))

def usable(e):
    return e[5] != "failed"

whole = [e for e in endpoints if e[0] == "whole"]
vf = [e for e in endpoints if e[0] == "vf"]
ok_whole = [e for e in whole if usable(e)]
ok_vf = [e for e in vf if usable(e)]
failed = [e for e in endpoints if e[5] == "failed"]
ep_numa = sorted({e[4] for e in endpoints if usable(e) and e[4]})

accelerators = []       # (group id, manufacturer, accelerator id, numa, partitioned?)
for g in spec.get("groups", []) or []:
    for a in g.get("accelerators", []) or []:
        profiles = ((a.get("status", {}) or {}).get("physicalSliced", {}) or {}).get("profiles") or []
        accelerators.append((g.get("id", ""), g.get("manufacturer", ""), a.get("id", ""),
                             ((a.get("topology", {}) or {}).get("numaAffinity") or ""),
                             bool(profiles)))

acc_numa = sorted({a[3] for a in accelerators if a[3]})
on_ep = [a for a in accelerators if a[3] and a[3] in ep_numa]
off_ep = [a for a in accelerators if a[3] and a[3] not in ep_numa]

# The largest set of accelerators a SINGLE NUMA node carrying a usable endpoint can offer, and that
# node. The kubelet merges the two hints into one NUMA node, so this figure is the boundary: a
# request for it can be satisfied there and a request for one more cannot be satisfied anywhere,
# because every NUMA node with more accelerators carries no endpoint. Counting accelerators over the
# whole endpoint NUMA SET instead would put the boundary above the number one node can serve, and a
# request between the two is admitted — which reads as the alignment failing to refuse.
best_numa, best_count = "", 0
for n in ep_numa:
    c = len([a for a in accelerators if a[3] == n])
    if c > best_count:
        best_numa, best_count = n, c
manufacturers = sorted({a[1] for a in accelerators if a[1]})
groups = sorted({a[0] for a in accelerators if a[0]})

def emit(k, v):
    print("%s=%s" % (k, v))

emit("F_EP_TOTAL", len(endpoints))
emit("F_EP_WHOLE", len(whole))
emit("F_EP_WHOLE_OK", len(ok_whole))
emit("F_EP_VF", len(vf))
emit("F_EP_VF_OK", len(ok_vf))
emit("F_EP_FAILED", len(failed))
emit("F_SRIOV_IFACES", sriov_ifaces)
emit("F_WHOLE_IFACES", whole_ifaces)
emit("F_EP_NUMA", ",".join(ep_numa))
emit("F_EP_NAMES", ",".join(sorted(e[3] for e in endpoints)))
emit("F_EP_NAMES_OK", ",".join(sorted(e[3] for e in endpoints if usable(e))))
emit("F_RDMA_NAMES", ",".join(sorted(rdma_names)))
emit("F_VF_TOTAL", vf_total)
emit("F_EP_ROWS", ";".join("|".join(str(x) for x in e) for e in endpoints))
emit("F_ACC_TOTAL", len(accelerators))
emit("F_ACC_NUMA", ",".join(acc_numa))
emit("F_ACC_ON_EP_NUMA", len(on_ep))
emit("F_ACC_OFF_EP_NUMA", len(off_ep))
emit("F_ACC_BEST_NUMA", best_numa)
emit("F_ACC_BEST_COUNT", best_count)
emit("F_ACC_PARTITIONED", len([a for a in accelerators if a[4]]))
emit("F_MANUFACTURERS", ",".join(manufacturers))
emit("F_GROUPS", ",".join(groups))
')
  read_failed $? "the derived facts of ${node}"

  # Cleared first: a global left over from a previous node would be read as this node's answer by
  # any requirement whose python branch did not emit it.
  F_EP_TOTAL=0 F_EP_WHOLE=0 F_EP_WHOLE_OK=0 F_EP_VF=0 F_EP_VF_OK=0 F_EP_FAILED=0
  F_SRIOV_IFACES=0 F_WHOLE_IFACES=0 F_EP_NUMA= F_EP_NAMES= F_EP_NAMES_OK= F_EP_ROWS=
  F_RDMA_NAMES= F_VF_TOTAL=0
  F_ACC_TOTAL=0 F_ACC_NUMA= F_ACC_ON_EP_NUMA=0 F_ACC_OFF_EP_NUMA=0 F_ACC_PARTITIONED=0
  F_ACC_BEST_NUMA= F_ACC_BEST_COUNT=0 F_MANUFACTURERS= F_GROUPS=
  # Every line is KEY=VALUE, and the value is assigned with `printf -v` rather than with `eval`.
  # `eval` was measured to be wrong here rather than merely risky: one of these values is the packed
  # endpoint list, whose fields are joined with `|` and `;`, so `eval` ran it as a pipeline — and on
  # an unterminated one it reported a syntax error and left the variable UNSET, which every reader
  # below would have taken for "this node has no endpoints".
  local line key val
  while IFS= read -r line; do
    key="${line%%=*}"
    val="${line#*=}"
    case "$key" in
    F_[A-Z_]*) printf -v "$key" '%s' "$val" ;;
    esac
  done <<<"$facts"

  RDMA_KUBELET_POLICY=$(rdma_kubelet_policy "$node")
  RDMA_ALLOC_WHOLE=$(rdma_node_quantity "$node" allocatable "$RDMA_KEY_WHOLE")
  RDMA_ALLOC_SHARED=$(rdma_node_quantity "$node" allocatable "$RDMA_KEY_SHARED")
  RDMA_ALLOC_PART=$(rdma_node_quantity "$node" allocatable "$RDMA_KEY_PARTITIONED")
}

# rdma_node_quantity <node> <allocatable|capacity> <key> — the integer the node reports, or an empty
# string when the key is absent. Absent and zero are DIFFERENT answers and are never folded: a key
# at zero is a plugin serving nothing, a key that is not there is a plugin that never served it.
rdma_node_quantity() {
  kubectl get node "$1" -o json 2>/dev/null | KIND="$2" KEY="$3" python3 -c '
import json, os, sys
st = json.load(sys.stdin).get("status", {}) or {}
print((st.get(os.environ["KIND"], {}) or {}).get(os.environ["KEY"], ""))
' 2>/dev/null
}

# rdma_kubelet_policy <node> — the TopologyManager policy the node's kubelet is ACTUALLY running,
# read from the kubelet's own live configuration endpoint.
#
# Not from a drop-in file, and not from the distribution's managed tree. Those are what a node is
# configured with, and reading them to judge whether the node enforces alignment asks the same
# source twice: a policy written where the reader does not look produces the same answer as a policy
# nobody set. `configz` is the kubelet reporting its own running value, which is the only reading
# that separates the two. An empty answer means the endpoint could not be reached.
rdma_kubelet_policy() {
  kubectl get --raw "/api/v1/nodes/$1/proxy/configz" 2>/dev/null | python3 -c '
import json, sys
try:
    print((json.load(sys.stdin).get("kubeletconfig", {}) or {}).get("topologyManagerPolicy", "") or "")
except Exception:
    print("")
' 2>/dev/null
}

# ------------------------------------------------------------------------------------------------
# The precondition gate.
# ------------------------------------------------------------------------------------------------

# rdma_requirement_text <id> — what a host must carry, how it is read, and why the requirement is
# written the way it is. Kept beside the predicate so a requirement cannot acquire a description
# that stopped matching it.
#
# The FIRST line always stands alone as a complete sentence: it is what a skip report quotes when it
# has room for one line, and a first line that trails off mid-clause there is the one place this
# text is read by someone who has just been told their machine cannot answer.
rdma_requirement_text() {
  case "$1" in
  devices)
    echo "a node running a device manager, i.e. carrying a Devices object."
    echo "read: kubectl get devices.worker.gpustack.ai" ;;
  endpoint)
    echo "at least one RDMA endpoint — an interface or a virtual function carrying an rdmaDevice."
    echo "read: Devices.spec.interfaces[].rdmaDevice and .virtualFunctions[].rdmaDevice" ;;
  endpoint-whole)
    echo "at least one interface that is NOT an SR-IOV physical function with virtual functions."
    echo "why: only those serve the exclusive and shared keys. A fully partitioned host reports"
    echo "both at zero, which equals what a host with no RDMA at all reports, and proves nothing."
    echo "read: Devices.spec.interfaces[].sriov together with the length of .virtualFunctions" ;;
  sriov)
    echo "at least one SR-IOV physical function with virtual functions configured."
    echo "why: a host with none exercises the other branch of the mode judgment and says nothing"
    echo "about this one — its partitioned count is zero because it has no virtual functions,"
    echo "not because the count was derived correctly."
    echo "read: Devices.spec.interfaces[].sriov and the length of .virtualFunctions" ;;
  failed-link)
    echo "at least one endpoint whose link verdict is ALREADY 'failed'."
    echo "why: the case never induces one. Driving a link down is a host mutation with no reliable"
    echo "restore, and an endpoint simply ABSENT from the inventory is a detector outcome rather"
    echo "than the gate this reading is about."
    echo "read: Devices.spec link.state on interfaces and virtual functions" ;;
  accelerator)
    echo "at least one accelerator in the ledger, of a manufacturer this suite has a key for."
    echo "read: Devices.spec.groups[].accelerators[] and the manufacturer's resource key" ;;
  numa-split)
    echo "accelerators on two or more NUMA nodes, some sharing one with an endpoint and some not."
    echo "why: both halves are required. Without the first there is no satisfiable request; without"
    echo "the second every placement is aligned whatever the kubelet does, so the admitted half"
    echo "passes with the alignment mechanism switched off — the exact reading the criterion"
    echo "excludes, and what a single-socket host produces."
    echo "read: Devices.spec accelerator topology.numaAffinity against the endpoints' numaAffinity" ;;
  topology-enforced)
    echo "a kubelet running TopologyManager on 'single-numa-node' or 'restricted'."
    echo "why: under 'none' the hint is computed and discarded, and under 'best-effort' a misaligned"
    echo "container is admitted anyway. On either, the refusal can never fire and the two values"
    echo "agreeing is a coincidence of placement."
    echo "read: kubectl get --raw /api/v1/nodes/<node>/proxy/configz .kubeletconfig.topologyManagerPolicy" ;;
  partitioned-card)
    echo "an accelerator ALREADY in a hardware partitioning mode, offering profiles the node serves."
    echo "why: a card that merely could be partitioned cannot serve a profile request."
    echo "read: Devices.spec accelerator status.physicalSliced.profiles, and the node's"
    echo "'<base>.partitioned.<kind>-<profile>' allocatable keys" ;;
  *)
    echo "unknown requirement '$1'."
    echo "read: nothing — this is a bug in the case that named it" ;;
  esac
}

# rdma_requirement_met <id> — the predicate, evaluated against the facts of the node rdma_facts was
# last called for. Sets REQ_READING to what was actually read, so an unmet requirement reports the
# value that disqualified the node rather than only the rule.
rdma_requirement_met() {
  REQ_READING=""
  case "$1" in
  devices)
    REQ_READING="Devices object present"
    return 0 ;;
  endpoint)
    REQ_READING="${F_EP_TOTAL} endpoint(s): ${F_EP_WHOLE} whole-function, ${F_EP_VF} virtual-function"
    [ "$F_EP_TOTAL" -gt 0 ] ;;
  endpoint-whole)
    REQ_READING="${F_EP_WHOLE} whole-function endpoint(s) over ${F_WHOLE_IFACES} non-partitioned interface(s)"
    [ "$F_EP_WHOLE" -gt 0 ] ;;
  sriov)
    REQ_READING="${F_SRIOV_IFACES} SR-IOV physical function(s) with virtual functions, ${F_EP_VF} virtual-function endpoint(s)"
    [ "$F_SRIOV_IFACES" -gt 0 ] && [ "$F_EP_VF" -gt 0 ] ;;
  failed-link)
    REQ_READING="${F_EP_FAILED} endpoint(s) report a failed link"
    [ "$F_EP_FAILED" -gt 0 ] ;;
  accelerator)
    REQ_READING="${F_ACC_TOTAL} accelerator(s), manufacturer(s) ${F_MANUFACTURERS:-<none>}"
    [ "$F_ACC_TOTAL" -gt 0 ] ;;
  numa-split)
    REQ_READING="accelerators on NUMA {${F_ACC_NUMA:-unknown}}, usable endpoints on NUMA {${F_EP_NUMA:-none}}; ${F_ACC_ON_EP_NUMA} accelerator(s) share a NUMA node with an endpoint and ${F_ACC_OFF_EP_NUMA} do not; the richest endpoint-bearing NUMA node is ${F_ACC_BEST_NUMA:-<none>} with ${F_ACC_BEST_COUNT} accelerator(s), which is where the boundary sits"
    [ "$F_ACC_ON_EP_NUMA" -gt 0 ] && [ "$F_ACC_OFF_EP_NUMA" -gt 0 ] ;;
  topology-enforced)
    REQ_READING="the kubelet reports topologyManagerPolicy=${RDMA_KUBELET_POLICY:-<unreadable>}"
    [ "$RDMA_KUBELET_POLICY" = "single-numa-node" ] || [ "$RDMA_KUBELET_POLICY" = "restricted" ] ;;
  partitioned-card)
    REQ_READING="${F_ACC_PARTITIONED} accelerator(s) currently report hardware partition profiles"
    [ "$F_ACC_PARTITIONED" -gt 0 ] ;;
  *)
    REQ_READING="unknown requirement"
    return 1 ;;
  esac
}

# rdma_select <requirement...> — pick the node this case runs on: the first that meets EVERY listed
# requirement. RDMA_NODE_NAME pins one instead, and a pinned node that does not qualify is a skip
# rather than a silent fallback to another.
#
# When no node qualifies the case skips, and the skip prints every candidate with the requirement it
# failed and the reading that failed it. That report is the deliverable of a skip: "no suitable node"
# sends a reader to look for hardware, while "every node has its accelerators and its endpoints on
# one NUMA node" sends them to the one machine shape that would answer.
rdma_select() {
  local reqs=("$@") nodes node req unmet report=""
  nodes=$(rdma_nodes)
  read_failed $? "the list of Devices objects"
  nodes=$(printf '%s' "$nodes" | grep -v '^$' || true)

  if [ -z "$nodes" ]; then
    rdma_skip "No node in this cluster carries a Devices object, so no device manager is running" \
      "and there is no RDMA inventory to read. That is an answer about the cluster, not about" \
      "the code."
  fi
  if [ -n "${RDMA_NODE_NAME:-}" ]; then
    nodes="$RDMA_NODE_NAME"
  fi

  for node in $nodes; do
    rdma_facts "$node"
    unmet=""
    for req in "${reqs[@]}"; do
      if ! rdma_requirement_met "$req"; then
        unmet="${unmet}${req} "
        report="${report}
  ${node}: UNMET '${req}'
    needs: $(rdma_requirement_text "$req" | head -1)
    read:  ${REQ_READING}"
      fi
    done
    if [ -z "$unmet" ]; then
      RDMA_NODE="$node"
      echo "[case-${CASE_ID}] node ${RDMA_NODE} meets: ${reqs[*]}"
      echo "[case-${CASE_ID}]   ${F_EP_WHOLE} whole-function and ${F_EP_VF} virtual-function endpoint(s); ${F_ACC_TOTAL} accelerator(s); kubelet policy ${RDMA_KUBELET_POLICY:-<unreadable>}"
      return 0
    fi
  done

  echo
  echo "[case-${CASE_ID}] No node meets every requirement of this case. What each candidate lacked:"
  printf '%s\n' "$report"
  echo
  echo "[case-${CASE_ID}] What a host must carry for this case to answer anything:"
  for req in "${reqs[@]}"; do
    echo "  - ${req}:"
    rdma_requirement_text "$req" | sed 's/^/      /'
  done
  rdma_skip "The host shapes that answer these readings — and the one reading no host in reach" \
    "answers at all — are written up in references/rdma-host-shapes.md."
}

# rdma_require_after_select <requirement...> — the same gate, applied to the node already selected.
# For a requirement a case needs but does not want to select on, so the skip names the node.
rdma_require_after_select() {
  local req
  for req in "$@"; do
    if ! rdma_requirement_met "$req"; then
      rdma_skip "${RDMA_NODE} does not meet '${req}'." \
        "needs: $(rdma_requirement_text "$req" | head -1)" \
        "read:  ${REQ_READING}"
    fi
  done
}

# rdma_require_image <VAR> <what the image must carry> — an image-gated check. Skips naming the
# variable and the executable, because "no image" and "an image without the tool" are one state to
# the case and two to whoever fixes it.
#
# It sets RDMA_IMAGE rather than printing the value, and the caller reads that global. Printing it
# would force the caller into a command substitution, where the `exit` inside rdma_skip ends only
# the subshell — the script would carry on with an empty image and report a verdict about a Pod it
# could never have started.
rdma_require_image() {
  local var="$1" carries="$2"
  RDMA_IMAGE="${!1:-}"
  if [ -z "$RDMA_IMAGE" ]; then
    rdma_skip "This case runs its readings inside a container and needs an image carrying ${carries}." \
      "Set ${var}=<image ref> and re-run. There is no default: no image this suite could name" \
      "ships that userspace, and defaulting to one that does not would report an absent tool as" \
      "an absent capability."
  fi
}

# rdma_has_tool <pod> <executable> — whether the running container carries the executable.
#
# Naming an image is not the same as the image carrying the tool, and the difference must not reach
# a verdict: a base image with no RDMA userspace answers `not found`, which is a fact about the
# image and not a reading of the operator. Every caller therefore PROBES before it measures and
# skips naming the executable and the packages that carry it, rather than recording the tool's own
# "command not found" as a failure of the thing under test.
#
# `command -v` rather than `which`: `which` is a separate package on several base images, so asking
# with it turns one missing tool into two.
rdma_has_tool() {
  kubectl -n "$RDMA_TEST_NS" exec "$1" -- sh -c "command -v $2 >/dev/null 2>&1" >/dev/null 2>&1
}

# The packages that carry each executable, named once so every skip says the same thing. They are
# the Debian-family names because that is what the base images these cases are handed use; an image
# built another way needs the equivalent, and the executable is what the check actually looks for.
RDMA_VERBS_PACKAGES="ibverbs-utils (plus rdma-core and ibverbs-providers)"
RDMA_PERFTEST_PACKAGES="perftest (plus rdma-core, libibverbs and ibverbs-providers)"

# ------------------------------------------------------------------------------------------------
# The host probe — the readings the ledger cannot supply.
# ------------------------------------------------------------------------------------------------

# rdma_probe_start — a long-lived Pod on the selected node, mounting the host's /sys read-only.
#
# It is the INDEPENDENT instrument. The correspondence checks ask whether the published inventory
# names the devices the host has, and reading both sides out of the ledger would make that check
# compare a value with itself — it would pass on a node whose inventory names nothing real. So the
# host side comes from the kernel's own device model, through a mount this Pod takes itself.
#
# Read-only, unprivileged, and it is NOT the Pod any grant check runs in. A privileged Pod, or one
# carrying a host mount, opens an RDMA device node whatever the device cgroup says — so a grant
# check run in this Pod would pass with the allocation doing nothing. The two Pods stay separate for
# that reason and the separation is not an implementation detail.
#
# Two entry points. `rdma_probe_start` ends the case when the probe cannot run, for a case whose
# every reading depends on it. `rdma_probe_try` returns non-zero instead, for a case that has checks
# it can still make — those cases record the probe-dependent rows as skips naming the missing
# instrument, rather than discarding the readings that never needed it. RDMA_PROBE_READY says which
# happened, so a helper below cannot be called against a Pod that is not there.
rdma_probe_try() {
  RDMA_PROBE_SKIP_REASON=""
  rdma_probe_start_inner && return 0
  return 1
}

rdma_probe_start() {
  rdma_probe_start_inner && return 0
  rdma_skip "The host probe Pod never reached Running on ${RDMA_NODE} within ${RDMA_POD_TIMEOUT}s." \
    "${RDMA_PROBE_SKIP_REASON}" \
    "It mounts the host's /sys read-only, which a restricted PodSecurity level refuses; on such a" \
    "cluster the independent host reading cannot be taken at all, and the correspondence this case" \
    "asserts would otherwise be the ledger compared with itself." \
    "Run this case in a namespace that permits a hostPath volume, or set RDMA_TEST_NS to one."
}

rdma_probe_start_inner() {
  RDMA_PROBE_READY=0
  RDMA_PROBE_POD="gpustack-e2e-rdma-probe-${RANDOM}"
  TESTPODS+=("$RDMA_PROBE_POD")
  cat <<EOF | kubectl apply -f - >/dev/null 2>&1
apiVersion: v1
kind: Pod
metadata:
  name: ${RDMA_PROBE_POD}
  namespace: ${RDMA_TEST_NS}
spec:
  restartPolicy: Never
  nodeSelector: { kubernetes.io/hostname: ${RDMA_NODE} }
  tolerations:
    - operator: Exists
  containers:
    - name: probe
      image: ${RDMA_PROBE_IMAGE}
      command: ["sleep", "86400"]
      volumeMounts:
        - { name: hostsys, mountPath: /host/sys, readOnly: true }
  volumes:
    - { name: hostsys, hostPath: { path: /sys, type: Directory } }
EOF
  local i phase
  for i in $(seq 1 "$((RDMA_POD_TIMEOUT / 3))"); do
    phase=$(kubectl -n "$RDMA_TEST_NS" get pod "$RDMA_PROBE_POD" -o jsonpath='{.status.phase}' 2>/dev/null)
    if [ "$phase" = Running ]; then
      RDMA_PROBE_READY=1
      return 0
    fi
    sleep 3
  done
  RDMA_PROBE_SKIP_REASON="Last phase: ${phase:-<none>}; the Pod mounts hostPath /sys read-only in namespace ${RDMA_TEST_NS}."
  return 1
}

# rdma_probe <cmd...> — run a command in the probe Pod. Its stdout is the value; its exit code is
# the caller's to read, and a caller that needs to tell "the command said no" from "the command
# could not run" must read both.
rdma_probe() {
  # A probe that never came up produces no reading, and returning silence here would let a caller
  # treat "the instrument is absent" as "the host has none of these", which is the substitution this
  # whole family exists to refuse.
  [ "${RDMA_PROBE_READY:-0}" -eq 1 ] || return 1
  kubectl -n "$RDMA_TEST_NS" exec "$RDMA_PROBE_POD" -- sh -c "$*" 2>/dev/null
}

# rdma_host_rdma_devices — what the host's own RDMA subsystem lists, one name per line. This is the
# left-hand side of the inventory correspondence, and it comes from the kernel rather than from us.
rdma_host_rdma_devices() {
  rdma_probe 'ls /host/sys/class/infiniband 2>/dev/null' | grep -v '^$' | LC_ALL=C sort
}

# rdma_host_link_layer <device> — InfiniBand or Ethernet, from the port attribute the kernel writes.
# Port 1 is the port every one of these adapters carries; a second port would be a separate reading
# and no criterion here asks for one.
rdma_host_link_layer() {
  rdma_probe "cat /host/sys/class/infiniband/$1/ports/1/link_layer 2>/dev/null" | tr -d '[:space:]'
}

# rdma_host_sriov_state <device> — the device's SR-IOV state as the kernel presents it, as one of:
#
#   absent    the counting file does not exist. The capability is NOT PRESENT on this guest at all:
#             measured on a host where the adapter is passed through and SR-IOV was consumed on the
#             hypervisor side, so the guest cannot see it. No command run inside makes it appear.
#   0         the file exists and reads zero. The capability is present and UNCONFIGURED, which an
#             administrator can change on this same machine.
#   <n>       n virtual functions are configured.
#
# The first two are kept apart deliberately, and folding them was the earlier shape. Both make the
# virtual-function reading unanswerable, and they mean opposite things to whoever has to fix it:
# "configure this machine" versus "this machine can never answer, find another". A skip that cannot
# say which sends a reader to run a command that cannot work.
rdma_host_sriov_state() {
  rdma_probe "f=/host/sys/class/infiniband/$1/device/sriov_numvfs; if [ -e \"\$f\" ]; then cat \"\$f\" 2>/dev/null || echo unreadable; else echo absent; fi" \
    | tr -d '[:space:]'
}

# rdma_host_numa_node <device> — the NUMA node the kernel attributes to the RDMA device itself.
rdma_host_numa_node() {
  rdma_probe "cat /host/sys/class/infiniband/$1/device/numa_node 2>/dev/null" | tr -d '[:space:]'
}

# ------------------------------------------------------------------------------------------------
# Test Pods.
# ------------------------------------------------------------------------------------------------

# rdma_wait_pod <name> <phase...> — wait for the Pod to reach one of the phases; print the phase it
# reached, or the empty string on timeout. A caller asserting a REFUSAL must not wait for Running:
# the refusal it is looking for is a Pod that stays Pending or goes Failed, which is what this
# returns rather than treating as an error.
rdma_wait_pod() {
  local name="$1" i phase
  shift
  for i in $(seq 1 "$((RDMA_POD_TIMEOUT / 3))"); do
    phase=$(kubectl -n "$RDMA_TEST_NS" get pod "$name" -o jsonpath='{.status.phase}' 2>/dev/null)
    local want
    for want in "$@"; do
      [ "$phase" = "$want" ] && {
        printf '%s' "$phase"
        return 0
      }
    done
    sleep 3
  done
  printf '%s' ""
}

# rdma_pod_reason <name> — the kubelet's own words about why a Pod is not running: the Pod-level
# reason and message, plus the first container's waiting/terminated reason. This is where
# TopologyAffinityError and UnexpectedAdmissionError appear, and a case asserting a refusal asserts
# on this text rather than on the absence of a Running phase, which every unrelated failure produces.
rdma_pod_reason() {
  kubectl -n "$RDMA_TEST_NS" get pod "$1" -o jsonpath='{.status.reason}{" "}{.status.message}{" "}{.status.containerStatuses[0].state.waiting.reason}{" "}{.status.containerStatuses[0].state.terminated.reason}' 2>/dev/null
}

# rdma_cleanup — delete everything this case created. Idempotent, safe on a re-run, and it never
# waits: a Pod of this family holds no finalizer, and a case that blocked here on a stuck delete
# would leave the NEXT case reading a baseline this one was still holding.
rdma_cleanup() {
  [ "${#TESTPODS[@]}" -gt 0 ] || return 0
  kubectl -n "$RDMA_TEST_NS" delete pod "${TESTPODS[@]}" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  return 0
}
