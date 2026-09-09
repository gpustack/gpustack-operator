#!/usr/bin/env bash
#
# CASE 52 — The NIC/RDMA inventory is published, is stable across passes, and the RDMA labels agree with it
#   (MUTATING, self-recovering; AUTO-SKIPS without a Devices object; the RDMA-positive checks auto-skip without RDMA hardware)
#
#   case-52.sh <NS>
#
# <NS> is the operator's own namespace, where the device manager pods live.
#
# Goal:        Every node the device manager profiles publishes its network interfaces on
#              `Devices.spec.interfaces`; the published record does not change on a pass that found
#              nothing new — asserted through `.metadata.generation`, which a status subresource
#              keeps free of status churn; and the node's `rdma.*` labels agree with what the same
#              object reports, in both directions.
#              NOT claimed: that the inventory SURVIVES a detector restart. The restart is driven
#              and the record is compared, but an unchanged object is also what a replacement that
#              never reported produces, so the agreeing outcome is a SKIP and the title no longer
#              says otherwise. See the limit stated at that check.
# Environment: Any cluster with at least one node running a device manager (i.e. carrying a
#              `Devices` object). AUTO-SKIPS (exit 0) when there is none — a node with no
#              accelerators runs no device manager, so there is no inventory to check. The
#              RDMA-positive checks skip individually when no interface has an RDMA device bound;
#              the negative label assertion runs either way and is the whole point on such a node.
# Inputs:      All real, nothing mocked. Reads `Devices` and Node labels only; the one mutation is
#              deleting the device manager pod so the DaemonSet recreates it.
# Expected:    - every Devices carries a non-empty interface list, each entry named and bused;
#              - the list is sorted by name, and a virtual interface is marked as one;
#              - a PCI-backed interface's `pciRootId` equals the LAST element of `pciSwitches`,
#                which is what "the outermost bridge" means and what the shared walk guarantees;
#              - `.metadata.generation` does not move over four detect periods of quiet;
#              - the interface record is byte-identical across those reads;
#              - on the ONE node whose device manager is deleted, the HARDWARE-BEARING rows are not
#                corrupted: a change there is a FAIL, while agreement is a SKIP rather than a PASS,
#                because a replacement that never reported produces the same agreement. The raw list
#                and the generation are reported, not asserted — deleting the pod replaces its CNI
#                veth, so both move on a cluster that behaved correctly. Every other node is a SKIP
#                too: nothing restarted there, and a PASS would read exactly like one that had;
#              - a node where neither an interface nor a virtual function reports an RDMA device
#                carries NO `rdma.*` label at all;
#              - where RDMA is present: every bound interface and every bound virtual function
#                carries one of the three link states; a `failed` one carries a reason and a
#                first-seen time; and `rdma.capable` is present whenever AT LEAST ONE ENDPOINT is
#                still usable. The reduction is over endpoints, not over bound interfaces: an
#                endpoint is usable when its verdict is anything but `failed`, falling back to the
#                bound flag only when there is no verdict — so an unreadable-tree record, which is
#                `rdma: false` with a synthesized `unverified` verdict, counts as usable, while a
#                bound device whose verdict is `failed` does not. The aggregation is existential, so
#                a broken NIC beside a working one keeps the node selectable, and only a node with
#                NO usable endpoint loses the key. On a node where that does not happen the
#                withholding half of the gate cannot run, and the case says so as a SKIP rather
#                than counting a PASS it did not earn.
# Cleanup:     Nothing to undo — the only mutation is a pod deletion the DaemonSet reverses, and the
#              case waits for it. No trap needed, and none is installed rather than an empty one.
#
# NOT covered here, deliberately: the `preflight` network section. It runs the same pass that wrote
# the records this case reads -- which makes the two interpret a link the same way, not read the same
# values, since it takes its own sysfs reading when invoked. Reaching it needs a privileged pod with
# host mounts, which is `preflight`'s own operational surface rather than this chain's -- see
# docs/operation/preflight.md.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-52.sh <NS>}"

# Four detect periods at the shipped 15s cadence. The point of the wait is that several passes run
# and none of them writes; a shorter one could pass because no pass happened at all.
QUIET_SECONDS="${CASE52_QUIET_SECONDS:-70}"

# How long the recreated device manager pod gets to come back, and how long the restarted detector
# then gets before it is read. Same caveat as above: the defaults are the values that can catch the
# defect, and lowering either turns a verdict into an answer about scheduling latency.
RESTART_SECONDS="${CASE52_RESTART_SECONDS:-300}"
SETTLE_SECONDS="${CASE52_SETTLE_SECONDS:-40}"

FAILS=0
PASSES=0
SKIPS=0
ROWS=()
# The rows are joined with ASCII UNIT SEPARATOR rather than a pipe. A pipe is correct only while no
# check name or object text ever contains one, which is a property of today's strings rather than of
# the code; 0x1f cannot appear in a node name, a label key or a link reason.
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

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  # Split on the separator the rows were built with. A field-position guess over the flattened line
  # cannot work: it has to locate the check text by content, and a check whose first word occurs
  # earlier in the line lands on the wrong offset.
  for r in "${ROWS[@]}"; do
    IFS="$ROW_SEP" read -r st ck ob <<<"$r"
    printf '%s | %s | %s\n' "$st" "$ck" "$ob"
  done
  [ "$FAILS" -eq 0 ] || {
    echo "[case-52] ${FAILS} check(s) FAILED (${PASSES} passed, ${SKIPS} skipped)"
    exit 1
  }
  # A run of nothing but SKIPs is not a pass, and distinguishing only FAIL>0 from FAIL==0 reported
  # one as a green run. Every check here is existential -- it asserts against whatever hardware the
  # cluster has -- so even a cluster with no RDMA at all still earns the inventory, ordering,
  # coordinate and stability passes. Zero PASSes therefore does not mean "no hardware"; it means no
  # assertion was reached, which on this case's read path is the aggregated view returning nothing.
  # The count has to reach the exit code, or that reads exactly like a clean run.
  if [ "$PASSES" -eq 0 ]; then
    echo "[case-52] NOTHING WAS VERIFIED: 0 passed, 0 failed, ${SKIPS} skipped"
    exit 1
  fi
  echo "[case-52] all checks passed (${PASSES} passed, ${SKIPS} skipped)"
}

# WHICH `Devices` these helpers actually read, because there are two and they are not interchangeable.
#
# `worker.gpustack.ai` is served twice: by an aggregated APIService at version `v1` (the worker
# Deployment backs it) and by the CRD at `v1alpha1`, which is the stored version. An unversioned
# `kubectl get devices.worker.gpustack.ai` resolves to the AGGREGATED one, so that is what every read
# below goes through. Two consequences, both measured:
#
#   - a newly added spec field has to be visible through the aggregated view before these checks can
#     see it. If it is not, every field assertion reads empty and the case reports a clean run about
#     nothing -- so a new field's first check is that the view carries it, not that its value is right;
#   - the aggregated version does not accept writes. `kubectl patch devices.worker.gpustack.ai` fails
#     with `ServiceUnavailable` (reproducibly, while reads succeed), which reads like a broken cluster
#     rather than like the wrong endpoint. Anything that needs to WRITE a `Devices` -- out-of-band
#     edits included -- must name the stored version: `devices.v1alpha1.worker.gpustack.ai`.
#
# inventory is the whole interface record as the object stores it, nested virtual functions and all.
# It is the comparand for the stability and restart checks.
#
# The flattened view below cannot serve that purpose. It lists top-level fields only, so a VF's link
# state, reason or first-seen time could churn -- rewriting the spec on every pass -- while both
# "byte-identical" checks stayed green. An earlier revision used the flattened view and argued it was
# safe because it covered every field the assertions read; that is the containment the other way
# round. A stability check needs to cover everything the DETECTOR WRITES, which is strictly more.
# read_failed ends the case when a read could not be MADE, as opposed to a read that succeeded and
# found nothing. Call it with $? immediately after the assignment it guards.
#
# Every assertion below compares strings, and a failed kubectl substitutes an empty one -- so two
# failed reads compare EQUAL and record a PASS for stability that was never observed, while a failed
# discovery read reports that the cluster runs no device manager. Emptiness is a legitimate answer
# for some of these fields and never an acceptable stand-in for a failure, so the status has to be
# read separately from the value.
#
# It ends the case rather than recording a FAIL row because the failure is not about the software
# under test: nothing was measured, and a verdict about the inventory cannot be reached by asking a
# cluster that did not answer.
#
# It runs in the parent shell on purpose. The same check written as a wrapper INSIDE the command
# substitution cannot work: `exit` there ends only the subshell, and the script carries on with the
# partial value -- measured, not assumed.
read_failed() {
  [ "$1" -eq 0 ] && return 0
  echo "[case-52] ERROR: $2 could not be read (kubectl exit $1)."
  echo "          Ending the case: an unread value is not an empty one, and every check below"
  echo "          compares strings — two unread values would compare equal and pass."
  exit 1
}

inventory() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{.spec.interfaces}' 2>/dev/null
}

# interfaces renders one line per interface, tab-separated, in the object's own order. It is the
# input to every field assertion below, and deliberately NOT the stability comparand.
interfaces() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{range .spec.interfaces[*]}{.name}{"\t"}{.bus}{"\t"}{.virtual}{"\t"}{.pciBusId}{"\t"}{.pciRootId}{"\t"}{.pciSwitches}{"\t"}{.rdma}{"\t"}{.rdmaDevice}{"\t"}{.link.state}{"\t"}{.link.reason}{"\t"}{.link.firstSeenTime}{"\n"}{end}' 2>/dev/null
}

# vf_rows renders one line per virtual function, tab-separated: bus id, rdma, link state, reason,
# first-seen time. Every field assertion that says "every RDMA device" needs these rows, because a
# VF is removed from the top-level list and on an SR-IOV node every RDMA device the node has is a
# VF -- so the flattened view above sees none of them. The parent's name is not reachable from
# inside the nested range and the bus id identifies a VF on its own.
vf_rows() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{range .spec.interfaces[*]}{range .virtualFunctions[*]}{.pciBusId}{"\t"}{.rdma}{"\t"}{.link.state}{"\t"}{.link.reason}{"\t"}{.link.firstSeenTime}{"\n"}{end}{end}' 2>/dev/null
}

# significant_interfaces renders the interface rows with the ones production itself treats as unable
# to affect anything dropped: virtual, with no RDMA device bound and no link verdict. It mirrors
# `triggersDetect`, the predicate the detector uses to decide whether a change is worth a round.
#
# It exists for the RESTART check alone, and the reason is the restart's own side effect: deleting
# the device manager pod destroys its CNI veth pair, and the replacement gets a new one under a new
# name. The published inventory records every kernel interface including the ephemeral ones, so the
# node's list legitimately DIFFERS across the restart and its generation legitimately moves — while
# the hardware and every link state are unchanged. Comparing the raw list there asserts that an
# ephemeral interface kept its name, which is not this case's question and would fail on a cluster
# that behaved correctly.
#
# Column positions come from `interfaces` above: 3 is `virtual`, 7 is `rdma`, 9 is `link.state`. A
# row is kept when it is not virtual, or carries either half of an RDMA record.
significant_interfaces() {
  interfaces "$1" | awk -F'\t' '$3 != "true" || $7 == "true" || $9 != ""'
}

generation() {
  kubectl get devices.worker.gpustack.ai "$1" -o jsonpath='{.metadata.generation}' 2>/dev/null
}

# rdma_labels prints this node's rdma.* label keys, one per line.
rdma_labels() {
  kubectl get node "$1" -o jsonpath='{.metadata.labels}' 2>/dev/null \
    | tr ',' '\n' | grep -o 'feature\.gpustack\.ai/rdma\.[a-z]*' | sort -u
}

# The kubectl call is kept out of the pipeline on purpose. Under `pipefail` the pipeline's status is
# the rightmost non-zero one, and `grep -v '^$'` exits 1 when it filters everything away — so a
# cluster with no Devices and a cluster whose API could not be reached would arrive here with the
# same status AND the same empty value. Separating them is what makes the SKIP below an answer about
# the cluster rather than about the connection.
NODES_RAW=$(kubectl get devices.worker.gpustack.ai -o jsonpath='{range .items[*]}{.metadata.name}{"\n"}{end}' 2>/dev/null)
read_failed $? "the list of Devices objects"
NODES=$(printf '%s' "$NODES_RAW" | grep -v '^$' || true)
if [ -z "$NODES" ]; then
  echo "[case-52] SKIP: no Devices object in this cluster — no node runs a device manager, so"
  echo "          there is no interface inventory to check. That is an answer about the cluster,"
  echo "          not about the inventory."
  exit 0
fi
echo "[case-52] profiling nodes: $(echo "$NODES" | tr '\n' ' ')"

# 1. The inventory exists and every entry is at least named and bused. An empty list here would be
#    the failure mode the pass is built to avoid: a machine always has interfaces, so an empty
#    inventory means either a read that failed and published anyway, or a branch that never wrote.
for node in $NODES; do
  before=$(interfaces "$node")
  read_failed $? "the interface rows of $node"
  vf_before=$(vf_rows "$node")
  read_failed $? "the virtual-function rows of $node"
  if [ -z "$before" ]; then
    record FAIL "${node} publishes an interface inventory" \
      "spec.interfaces is empty; every machine has at least a loopback interface"
    continue
  fi
  count=$(printf '%s\n' "$before" | grep -c .)
  nameless=$(printf '%s\n' "$before" | awk -F'\t' '$1 == "" || $2 == "" {c++} END {print c+0}')
  if [ "$nameless" -eq 0 ]; then
    record PASS "${node} publishes an interface inventory" "${count} interface(s), all named and bused"
  else
    record FAIL "${node} publishes an interface inventory" "${nameless} of ${count} entries lack a name or a bus"
  fi

  # 2. Sorted by name. The comparison that decides whether to write is order-sensitive, so an
  #    unsorted list would report a change on every pass — with correct data in the object the
  #    whole time, which is why the ordering is asserted rather than assumed.
  names=$(printf '%s\n' "$before" | awk -F'\t' '{print $1}')
  if [ "$names" = "$(printf '%s\n' "$names" | LC_ALL=C sort)" ]; then
    record PASS "${node} publishes the list sorted by name" "$(printf '%s' "$names" | tr '\n' ' ')"
  else
    record FAIL "${node} publishes the list sorted by name" \
      "order is $(printf '%s' "$names" | tr '\n' ' ')"
  fi

  # 3. A virtual interface is marked. Loopback is the one interface every machine has, and it is
  #    virtual — so a pass that classified nothing as virtual read the device tree wrongly.
  virtual=$(printf '%s\n' "$before" | awk -F'\t' '$3 == "true" {c++} END {print c+0}')
  if [ "$virtual" -gt 0 ]; then
    record PASS "${node} marks virtual interfaces" "${virtual} of ${count}"
  else
    record FAIL "${node} marks virtual interfaces" \
      "none of ${count} is virtual, but loopback always is — the classifier read the wrong path"
  fi

  # 4. pciRootId IS the outermost bridge. The unit tests take this on faith about sysfs; here it is
  #    checked against a real device tree, and it is the invariant that makes an accelerator's
  #    coordinates and an interface's comparable at all.
  #    The columns are read with awk, not with `read`: tab is IFS whitespace, so `IFS=$'\t' read`
  #    collapses a run of tabs into one delimiter and every field after an EMPTY column arrives one
  #    position early. `virtual` is empty on exactly the interfaces this check exists for, which
  #    made it compare pciSwitches against pciBusId and fail on correct data.
  read -r checked bad <<EOF
$(printf '%s\n' "$before" | awk -F'\t' '
    $2 == "pci" && $4 != "" {
      checked++
      switches = $6
      gsub(/^\[|\]$|"/, "", switches)
      if (switches == "") {
        # No bridge above it: the device is its own outermost PCI component.
        want = $4
      } else {
        n = split(switches, parts, ",")
        want = parts[n]
      }
      if ($5 != want) bad++
    }
    END { print checked+0, bad+0 }')
EOF
  if [ "$checked" -eq 0 ]; then
    record SKIP "${node} pciRootId is the outermost bridge" "no PCI-backed interface on this node"
  elif [ "$bad" -eq 0 ]; then
    record PASS "${node} pciRootId is the outermost bridge" "${checked} PCI interface(s) consistent"
  else
    record FAIL "${node} pciRootId is the outermost bridge" \
      "${bad} of ${checked} PCI interfaces disagree with the last element of pciSwitches"
  fi

  # 5. The negative label case, which is the whole assertion on a node without RDMA hardware:
  #    neither an interface nor a virtual function has an RDMA device, so not one rdma.* key may
  #    appear. A key here would mean the node advertises a capability the same object says it does
  #    not have.
  #
  #    Both counts gate this branch, and reading only the top-level rows made it fail a CORRECT
  #    implementation: a VF is removed from the top-level list, so on an SR-IOV node every RDMA
  #    device is a VF, production rightly publishes `rdma.capable` for them, and this row saw a node
  #    with no hardware and no right to the label. It then skipped checks 6 and 7 as well, so the
  #    node's real gate was never examined at all.
  rdma_count=$(printf '%s\n' "$before" | awk -F'\t' '$7 == "true" {c++} END {print c+0}')
  vf_rdma=$(printf '%s\n' "$vf_before" | awk -F'\t' '$2 == "true" {c++} END {print c+0}')
  # An explicit verdict outranks the flag, so a record with `rdma: false` carrying a synthesized
  # `unverified` link -- the unreadable-tree case -- is RDMA this node has, and production emits
  # `rdma.capable` on its account. Counted alongside the bound ones so this branch does not read
  # such a node as a node with no RDMA at all and then fail a correct implementation, which is the
  # same mistake the VF blindness below made from the other direction.
  stated=$(printf '%s\n' "$before" | awk -F'\t' '$7 != "true" && $9 != "" {c++} END {print c+0}')
  vf_stated=$(printf '%s\n' "$vf_before" | awk -F'\t' '$2 != "true" && $3 != "" {c++} END {print c+0}')
  labels=$(rdma_labels "$node")
  read_failed $? "the rdma.* labels of $node"
  if [ "$((rdma_count + vf_rdma + stated + vf_stated))" -eq 0 ]; then
    if [ -z "$labels" ]; then
      record PASS "${node} carries no rdma label without RDMA hardware" \
        "no rdma.* key; no interface or virtual function of $(printf '%s\n' "$before" | grep -c .) reports RDMA or carries a link verdict"
    else
      record FAIL "${node} carries no rdma label without RDMA hardware" \
        "labels present: $(printf '%s' "$labels" | tr '\n' ' ')"
    fi
    continue
  fi

  # 6. Where RDMA is present: every bound interface AND every bound virtual function has one of the
  #    three states, and a failed one says why and since when. An omitted state is the shape F6
  #    forbids — it reads as a pass.
  #
  #    The VF rows are counted here, not only at the branch above. Over the top-level rows alone a
  #    VF-only node reached this check with `rdma_count` at zero, found nothing unstated in an empty
  #    set, and recorded a PASS reading "every RDMA interface carries a link state, 0 interface(s)"
  #    — a pass about nothing, on the one node shape where every RDMA device the node has is a VF.
  bound=$((rdma_count + vf_rdma))
  unstated=$(printf '%s\n' "$before" \
    | awk -F'\t' '$7 == "true" && $9 != "ok" && $9 != "unverified" && $9 != "failed" {c++} END {print c+0}')
  vf_unstated=$(printf '%s\n' "$vf_before" \
    | awk -F'\t' '$2 == "true" && $3 != "ok" && $3 != "unverified" && $3 != "failed" {c++} END {print c+0}')
  if [ "$bound" -eq 0 ]; then
    # Nothing is bound, so the states this row asserts are the ones a bound device must carry and
    # there is no bound device. Reached on a node whose only RDMA record is the unbound `unverified`
    # one, which already has a state by construction. A PASS here would read "every RDMA interface
    # carries a link state, 0 interface(s)" -- the shape this row exists to catch, printed by the
    # row itself.
    record SKIP "${node} every RDMA interface carries a link state" \
      "no bound RDMA device on this node; ${stated} interface(s) and ${vf_stated} virtual function(s) carry a verdict without one"
  elif [ "$((unstated + vf_unstated))" -eq 0 ]; then
    record PASS "${node} every RDMA interface carries a link state" \
      "${rdma_count} interface(s) and ${vf_rdma} virtual function(s)"
  else
    record FAIL "${node} every RDMA interface carries a link state" \
      "$((unstated + vf_unstated)) of ${bound} report no state, which reads as a pass"
  fi

  failed_count=$(printf '%s\n' "$before" | awk -F'\t' '$9 == "failed" {c++} END {print c+0}')
  vf_failed=$(printf '%s\n' "$vf_before" | awk -F'\t' '$3 == "failed" {c++} END {print c+0}')
  if [ "$((failed_count + vf_failed))" -gt 0 ]; then
    incomplete=$(printf '%s\n' "$before" \
      | awk -F'\t' '$9 == "failed" && ($10 == "" || $11 == "") {c++} END {print c+0}')
    vf_incomplete=$(printf '%s\n' "$vf_before" \
      | awk -F'\t' '$3 == "failed" && ($4 == "" || $5 == "") {c++} END {print c+0}')
    if [ "$((incomplete + vf_incomplete))" -eq 0 ]; then
      record PASS "${node} a failed link carries a reason and a first-seen time" \
        "${failed_count} interface(s) and ${vf_failed} virtual function(s)"
    else
      record FAIL "${node} a failed link carries a reason and a first-seen time" \
        "$((incomplete + vf_incomplete)) of $((failed_count + vf_failed)) are missing one of the two"
    fi
  else
    record SKIP "${node} a failed link carries a reason and a first-seen time" "no link reports failed"
  fi

  # 7. The gate, in both directions. `unverified` must NOT withhold the key — that is the whole
  #    reason the state exists — so the condition is on `failed` alone.
  #
  #    The aggregation across interfaces is EXISTENTIAL: the label says at least one interface is
  #    usable, not that every one is. A node with a broken NIC beside a working one keeps the
  #    label, because it can still serve an RDMA workload — the spec's P12 says withholding there
  #    would let an unplugged management NIC take a working node out of scheduling. An earlier
  #    revision of this case asserted the opposite (`failed_count > 0` withholds), which would have
  #    failed a correct implementation on any mixed node.
  # The predicate MIRRORS rdmaUsable rather than approximating it, over interfaces and virtual
  # functions alike: an endpoint is usable when its verdict is anything but `failed`, falling back to
  # the bound flag only when there is no verdict at all. Counting `rdma == true && state != failed`
  # instead misses the unreadable-tree record on one side and every SR-IOV endpoint on the other,
  # and this row would then assert the opposite of the implementation on both node shapes.
  capable_count=$(printf '%s\n' "$before" \
    | awk -F'\t' '($9 != "" && $9 != "failed") || ($9 == "" && $7 == "true") {c++} END {print c+0}')
  vf_capable=$(printf '%s\n' "$vf_before" \
    | awk -F'\t' '($3 != "" && $3 != "failed") || ($3 == "" && $2 == "true") {c++} END {print c+0}')
  usable=$((capable_count + vf_capable))
  all_failed=$((failed_count + vf_failed))
  has_capable=$(printf '%s\n' "$labels" | grep -c 'rdma\.capable$')

  if [ "$usable" -eq 0 ]; then
    # Every endpoint reports failed, so there is nothing usable and the key must be gone.
    if [ "$has_capable" -eq 0 ]; then
      record PASS "${node} every link failed withholds rdma.capable" \
        "${all_failed} failed, 0 usable, no capable key"
    else
      record FAIL "${node} every link failed withholds rdma.capable" \
        "rdma.capable is present while all ${all_failed} endpoint(s) report failed"
    fi
  else
    if [ "$has_capable" -eq 1 ]; then
      record PASS "${node} a usable link emits rdma.capable" \
        "${usable} usable endpoint(s) of $((bound + stated + vf_stated)), capable key present"
    else
      record FAIL "${node} a usable link emits rdma.capable" \
        "rdma.capable is absent while ${usable} endpoint(s) are usable; only a node with NO usable endpoint may withhold it"
    fi
    # The withholding direction was NOT reachable on this node: something is usable, so the branch
    # that refuses never ran. Recorded rather than omitted, because a gate that is only correct in
    # the direction it was never driven prints exactly the same as a gate that is not there.
    # Driving it needs EVERY bound link down -- admin-down them and restore, never a forged
    # `Devices` status, which is rebuilt wholesale each reconcile.
    if [ "$all_failed" -gt 0 ]; then
      record SKIP "${node} every link failed withholds rdma.capable" \
        "NOT REACHED: ${all_failed} failed but ${usable} still usable, which correctly keeps the key"
    else
      record SKIP "${node} every link failed withholds rdma.capable" \
        "NEVER OBSERVED TO FAIL: no link on this node reported failed, so the withholding branch did not run"
    fi
  fi
done

# 8. Stability. This is the criterion no unit test can establish end to end: several detect passes
#    run against unchanged hardware and none of them writes the spec. `.metadata.generation` is the
#    instrument because Devices carries a status subresource, so it moves on a SPEC write only — the
#    status is rebuilt wholesale every reconcile and would swamp a resourceVersion comparison.
#
#    What the window observes is the MONITOR cadence, and it covers the interface pass only because
#    the detector now runs that pass on every monitor tick. It did not always: the pass used to run
#    inside the report path alone, which the monitor loop re-entered only when an accelerator's
#    identity changed — nothing about the network is in that key. So this assertion held for the
#    interface record without ever recomputing it, which is a pass no different in output from a
#    real one. The window is load-bearing for that wiring and is the only place it is exercised.
declare -a GEN_BEFORE=()
declare -a IFACE_BEFORE=()
declare -a INV_BEFORE=()
idx=0
for node in $NODES; do
  GEN_BEFORE[$idx]=$(generation "$node")
  read_failed $? "the baseline generation of $node"
  IFACE_BEFORE[$idx]=$(interfaces "$node")
  read_failed $? "the baseline interface rows of $node"
  INV_BEFORE[$idx]=$(inventory "$node")
  read_failed $? "the baseline inventory of $node"
  idx=$((idx + 1))
done

echo "[case-52] holding ${QUIET_SECONDS}s (>= four detect periods) to see whether an unchanged pass writes"
sleep "$QUIET_SECONDS"

idx=0
for node in $NODES; do
  gen_now=$(generation "$node")
  read_failed $? "the generation of $node after the quiet hold"
  if [ "$gen_now" = "${GEN_BEFORE[$idx]}" ]; then
    record PASS "${node} an unchanged pass writes nothing" "generation stayed ${gen_now}"
  else
    record FAIL "${node} an unchanged pass writes nothing" \
      "generation moved ${GEN_BEFORE[$idx]} -> ${gen_now} with no hardware change; the detector is rewriting the spec every pass"
  fi
  if [ "$(inventory "$node")" = "${INV_BEFORE[$idx]}" ]; then
    record PASS "${node} the inventory is byte-identical across passes" \
      "$(printf '%s\n' "${IFACE_BEFORE[$idx]}" | grep -c .) interface(s) and their virtual functions"
  else
    record FAIL "${node} the inventory is byte-identical across passes" "the record differs between two reads"
  fi
  idx=$((idx + 1))
done

# 9. Survive a restart. A fresh process has no memory of the first-seen times, so a time taken from
#    the clock rather than merged from the stored record shows up here as a generation bump — which
#    is why the restart is worth the mutation.
#
#    One pod is deleted, so exactly one node's detector restarts and exactly one node earns this
#    verdict. Restarting every profiled node's device manager would widen it, and is deliberately
#    not done: it takes the whole fleet's detection down at once on a cluster this case is expected
#    to run against in passing.
# The chart's own deviceManager.selectorLabels pair. `part-of=gpustack-operator-device-manager` is
# NOT a label anything stamps -- it matched nothing on a live cluster while the pair below matched
# every device manager on it -- and the `grep` fallback two lines down is what hid that: discovery
# kept working by name while the restart wait, which has no fallback, could never succeed.
#
# The pair rather than `component=device-manager` alone, which is what the sibling cases use: the
# component label on its own would also match an application-owned daemonset that happens to use it,
# and this case deletes what it selects.
#
# There is deliberately NO fallback that matches pods by name. An earlier revision fell back to
# `grep -E 'device-manager'` over every pod in the namespace, which was wrong twice over: this case
# DELETES what it selects, so a user pod named `device-manager-backup` was a candidate for deletion;
# and the wait loop below has no such fallback, so a run that reached it was guaranteed to time out
# into a false FAIL anyway. An empty result from the labelled query is an ANSWER -- no device manager
# here -- and the case skips on it.
#
# The victim is chosen from pods that are Running AND already assigned to a node. `head -1` over
# names alone could pick a Pending pod whose `spec.nodeName` is still empty, and the wait loop's
# `--field-selector spec.nodeName=` then matches only unassigned pods, so no Running replacement can
# ever satisfy it -- another false FAIL on a cluster that behaved correctly.
#
# The comparand for THIS check is captured here rather than reused from check 8's baseline, so it
# spans the restart alone: the quiet hold before it is a different question, and a baseline taken
# earlier would fold both windows into one verdict.
declare -a SIG_BEFORE=()
idx=0
for node in $NODES; do
  SIG_BEFORE[$idx]=$(significant_interfaces "$node")
  read_failed $? "the pre-restart interface rows of $node"
  idx=$((idx + 1))
done
#
# The victim is also restricted to a node that carries a `Devices` object. A device manager can be
# Running while its detection is held or failing, in which case its node has no `Devices` at all --
# and it is not in NODES, which the assertion loop below walks. Picking such a pod means the loop
# never reaches the node that restarted: every node is skipped as "not restarted", the restart
# scenario verifies nothing, and the earlier checks' passes still carry the case to exit 0.
DM_PODS=$(kubectl -n "$NS" get pods \
  -l app.kubernetes.io/part-of=gpustack-operator,app.kubernetes.io/component=device-manager \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.spec.nodeName}{"\t"}{.metadata.uid}{"\t"}{.status.phase}{"\n"}{end}' 2>/dev/null \
  | awk -F'\t' -v nodes="$NODES" \
      'BEGIN { n = split(nodes, a, "\n"); for (i = 1; i <= n; i++) profiled[a[i]] = 1 }
       $2 != "" && $4 == "Running" && ($2 in profiled)')

if [ -z "$DM_PODS" ]; then
  record SKIP "the inventory survives a detector restart" \
    "no Running device manager pod in ${NS} is assigned to a node carrying a Devices object, so there is nothing whose restart this case could observe"
else
  # The victim's own node and UID, because the wait below has to observe THIS pod being replaced.
  # Counting Running pods cluster-wide cannot do that: on any multi-node cluster the other nodes'
  # device managers are already Running, so the count is satisfied before the delete has taken
  # effect and the assertion runs against a process that never restarted.
  # `IFS=$'\t' read` collapses a run of tabs, so every field after an empty column would arrive one
  # position early. The awk filter above is what makes that impossible here -- it requires a
  # non-empty nodeName and a Running phase, and the name and uid are never empty -- so a future
  # column that CAN be empty has to be read with awk instead of appended here.
  IFS=$'\t' read -r victim victim_node victim_uid _ <<<"$(printf '%s\n' "$DM_PODS" | head -1)"
  echo "[case-52] deleting ${victim} (node ${victim_node:-unknown}) so its DaemonSet recreates it"
  kubectl -n "$NS" delete pod "$victim" --wait=false >/dev/null 2>&1

  # What is waited for is a REPLACEMENT on the victim's own node: a pod on that node, Running, with
  # a UID that is not the victim's. Both halves matter — the node scopes out every other device
  # manager in the cluster, and the UID is what distinguishes the replacement from the victim still
  # terminating under the same name.
  #
  # The first read comes AFTER a sleep for the same reason: the delete does not wait, so a pod
  # queried immediately is the terminating one still reporting Running. The deadline bounds the
  # retries, and one attempt always runs.
  ready=0
  deadline=$((SECONDS + RESTART_SECONDS))
  while :; do
    sleep 5
    ready=$(kubectl -n "$NS" get pods \
      -l app.kubernetes.io/part-of=gpustack-operator,app.kubernetes.io/component=device-manager \
      --field-selector "spec.nodeName=${victim_node}" \
      -o jsonpath='{range .items[*]}{.metadata.uid}{" "}{.status.phase}{"\n"}{end}' 2>/dev/null \
      | awk -v uid="$victim_uid" '$1 != uid && $2 == "Running" {c++} END {print c+0}')
    [ "${ready:-0}" -ge 1 ] && break
    [ "$SECONDS" -lt "$deadline" ] || break
  done
  if [ "${ready:-0}" -lt 1 ]; then
    record FAIL "the inventory survives a detector restart" \
      "no replacement for ${victim} came back Running on ${victim_node} within ${RESTART_SECONDS}s"
  else
    # Give the restarted detector time for at least two passes of its own.
    #
    # THE LIMIT OF THIS CHECK, and why the agreeing outcome below is a SKIP and not a PASS: a
    # replacement reaching Running plus this sleep does not prove that the replacement's detector
    # completed a report. The `Devices` object survives the pod's deletion, so an unchanged stale
    # object satisfies both comparisons below even if the new process never called its report path
    # at all. A regression where the restarted detector never reports is therefore invisible here,
    # and recording a PASS would claim restart coverage this scenario did not earn.
    #
    # What the comparisons DO establish is the negative: the record was not corrupted or rewritten
    # across the restart. That is worth a row, so the disagreeing outcomes below stay FAILs.
    #
    # A report-specific observable does exist and was measured: corrupting a spec field the detector
    # rederives is repaired within seconds of the pod being replaced, while the same corruption on a
    # running manager went 182 seconds and 20 reads unrepaired -- so a repair that appears can only
    # have come from a process that ran its report path. It is not taken here because the corruption
    # would have to be written to a cluster-scoped `Devices` object this case does not own and the
    # whole scheduling chain reads. Turning this SKIP into a PASS is that trade, not more code.
    sleep "$SETTLE_SECONDS"
    idx=0
    for node in $NODES; do
      gen_now=$(generation "$node")
      read_failed $? "the generation of $node after the restart"
      inv_now=$(inventory "$node")
      read_failed $? "the inventory of $node after the restart"

      # Only the victim's detector was replaced. Judging every node here printed a PASS for a
      # scenario that never touched them — output indistinguishable from a real restart, which is
      # what makes it worse than no row: it reports a verdict the scenario did not drive. What was
      # observed is still stated, so scoping the verdict does not discard the reading.
      if [ "$node" != "$victim_node" ]; then
        if [ "$inv_now" = "${INV_BEFORE[$idx]}" ] && [ "$gen_now" = "${GEN_BEFORE[$idx]}" ]; then
          moved="its record is unchanged, but no restart drove that"
        else
          moved="its record moved anyway, which is check 8's question and not this one"
        fi
        record SKIP "${node} the inventory survives a detector restart" \
          "NOT RESTARTED: only ${victim_node}'s device manager was deleted; ${moved}"
        idx=$((idx + 1))
        continue
      fi

      # Compared on the SIGNIFICANT rows, and the generation is reported rather than asserted.
      # Deleting the pod destroys its CNI veth pair and the replacement gets a new one, so on this
      # node the raw list legitimately differs and the generation legitimately moves while the
      # hardware and every link state hold still. Asserting either would fail on a cluster that
      # behaved correctly, and a check that fails when nothing is wrong gets muted rather than read.
      sig_now=$(significant_interfaces "$node")
      read_failed $? "the post-restart interface rows of $node"
      if [ "$sig_now" != "${SIG_BEFORE[$idx]}" ]; then
        record FAIL "${node} the inventory survives a detector restart" \
          "the hardware-bearing rows changed across the restart, with the hardware unchanged"
      else
        record SKIP "${node} the inventory survives a detector restart" \
          "NOT PROVEN: the hardware-bearing rows are identical (generation ${GEN_BEFORE[$idx]} -> ${gen_now}, which an ephemeral interface moves on its own) — which a replacement that never reported produces too"
      fi
      idx=$((idx + 1))
    done
  fi
fi

results
