#!/usr/bin/env bash
#
# _topology-tas-lib.sh — shared TAS fixture, reading and ModelDeployment-join helpers.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace: it carries no case
# header, no trap and no results table. Every one of those stays with the case that sources it.
# What it removes is the duplication between the cases that prove Kueue topology-aware scheduling
# against hand-built fixtures: the zone-shape measurement, the sizing arithmetic, the apply and the
# teardown of a disposable Topology/ResourceFlavor/ClusterQueue/LocalQueue set, the readers for a
# Kueue-created Workload's admission state, and the join that attributes a Workload to a
# ModelDeployment through the Pods it owns.
#
# A case uses it as:
#
#     set -uo pipefail
#     NS="${1:?usage: ...}"
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_topology-tas-lib.sh"
#     tas_zone_shape                    # measures the cluster; fills TAS_* globals
#     tas_apply_fixtures caseNNS "$NS" 4000m 6000Mi
#     ...checks, each `record`ing one row...
#     tas_delete_fixtures caseNNS "$NS"
#
# ------------------------------------------------------------------------------------------------
# WHY THE CONTROL FIXTURES ARE HAND-BUILT, AND WHY THEY ARE DELETED
#
# The operator now creates TAS queues. These hand-built objects remain an independent control for
# Kueue's own placement semantics, isolated from the operator's profile and joint-admission logic.
# Every caller builds a case-prefixed Topology/ResourceFlavor/ClusterQueue/LocalQueue set and this
# library applies and deletes exactly the four objects it applied -- by name, with
# --ignore-not-found, never by label and never by list, so two cases sourcing this file in one
# namespace cannot sweep each other's fixtures away.
#
# The hand-built ClusterQueue references no AdmissionCheck on purpose. An operator-own check such
# as the joint-admission gate would hold these fixtures' workloads for reasons that have nothing
# to do with topology, and a case that passed through such a queue would be reading the operator's
# barrier where it meant to read Kueue's TAS.
# ------------------------------------------------------------------------------------------------

# Route every kubectl through the retrying shim. A TAS case runs many short polls against a remote
# API endpoint, and one transport stall inside a poll loop is a verdict about the network.
E2E_SHIM_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

# The label key that carries the topology this library schedules on. Zone is a standard Kubernetes
# key every cloud writes; hostname is the level Kueue requires last when it is present at all. The
# fixtures below spell both levels out, in that order.
TAS_ZONE_LABEL="topology.kubernetes.io/zone"

# The share of one node's allocatable a single case Pod asks for. It has to clear two bars at once:
# at sixty percent one Pod always fits the smallest schedulable node, and two never fit one node --
# which is what turns "N Pods placed in one zone" into a statement about the ZONE and not about
# bin-packing inside a single host. Both are asserted by the caller's shape row, not assumed here.
TAS_POD_SHARE_PERCENT=60

# Millicores from a Kubernetes CPU quantity, or 0 when the value is not one this can read.
#
# NOT BASH ARITHMETIC, and the shape is matched before conversion, for the reasons case-50 records:
# a CPU quantity is legally fractional, `$(( 1.5 * 1000 ))` is a syntax error rather than a wrong
# number, and awk's numeric prefix would silently turn "1Ki" into a confident 1.
tas_millis() {
  case "$1" in
    *[!0-9.m]* | "" | m | *m*m) echo 0 ;;
    *m) printf '%s' "${1%m}" | awk '{printf "%d", $0+0}' ;;
    *) printf '%s' "$1" | awk '{printf "%d", ($0+0)*1000}' ;;
  esac
}

# Mebibytes from a Kubernetes memory quantity, or 0 when the value is not one this can read. The
# accepted units are the ones allocatable and requests can legally carry; everything else reads as
# "could not read", which the caller's guards already treat as a skip condition rather than a zero.
tas_mib() {
  local v u
  if [[ "$1" =~ ^([0-9]+)(Ki|Mi|Gi|k|M|G)?$ ]]; then
    v="${BASH_REMATCH[1]}"
    u="${BASH_REMATCH[2]}"
    case "$u" in
      Ki) awk -v n="$v" 'BEGIN{printf "%d", n/1024}' ;;
      Mi) echo "$v" ;;
      Gi) awk -v n="$v" 'BEGIN{printf "%d", n*1024}' ;;
      k) awk -v n="$v" 'BEGIN{printf "%d", n*1000/1048576}' ;;
      M) awk -v n="$v" 'BEGIN{printf "%d", n*1000000/1048576}' ;;
      G) awk -v n="$v" 'BEGIN{printf "%d", n*1000000000/1048576}' ;;
      "") awk -v n="$v" 'BEGIN{printf "%d", n/1048576}' ;;
    esac
  else
    echo 0
  fi
}

# Measure the zone shape the TAS fixtures will schedule over.
#
# A node counts when a plain case Pod could land on it: Linux (the ResourceFlavor selects it), zone
# labeled (a zone-required Pod cannot use a node that carries no zone), schedulable, and free of
# NoSchedule/NoExecute taints (the case Pods tolerate nothing). The result is four globals:
#
#   TAS_NODES            how many such nodes exist, cluster-wide
#   TAS_MAX_ZONE_NODES   the largest count any one zone holds
#   TAS_MIN_CPU_M        the smallest allocatable CPU among them, millicores
#   TAS_MIN_MEM_MI       the smallest allocatable memory among them, mebibytes
#   TAS_ZONES_SUMMARY    "zone=count" per zone, space separated, for row text only
#
# TAS_NODES is 0 when nothing matched, which is a verdict the caller skips on; it is never left
# unset, so a caller that forgets to run this function fails loudly rather than scheduling on an
# empty string.
tas_zone_shape() {
  TAS_NODES=0
  TAS_MAX_ZONE_NODES=0
  TAS_MIN_CPU_M=0
  TAS_MIN_MEM_MI=0
  TAS_ZONES_SUMMARY=""

  local out name zone os cpu mem unsched taints m zone_list
  zone_list=""
  out="$(kubectl get nodes -o jsonpath="{range .items[*]}{.metadata.name}|{.metadata.labels.topology\\.kubernetes\\.io/zone}|{.metadata.labels.kubernetes\\.io/os}|{.status.allocatable.cpu}|{.status.allocatable.memory}|{.spec.unschedulable}|{range .spec.taints[*]}{.effect}{\" \"}{end}{\"\n\"}{end}" 2>/dev/null)"
  [ -n "$out" ] || return 0

  while IFS='|' read -r name zone os cpu mem unsched taints; do
    [ -n "$name" ] || continue
    [ "$os" = linux ] || continue
    [ -n "$zone" ] || continue
    [ "$unsched" = true ] && continue
    case " $taints " in *" NoSchedule "* | *" NoExecute "*) continue ;; esac

    TAS_NODES=$((TAS_NODES + 1))
    zone_list="${zone_list}${zone}
"
    m="$(tas_millis "$cpu")"
    if [ "$TAS_MIN_CPU_M" -eq 0 ] || { [ "$m" -gt 0 ] && [ "$m" -lt "$TAS_MIN_CPU_M" ]; }; then
      TAS_MIN_CPU_M="$m"
    fi
    m="$(tas_mib "$mem")"
    if [ "$TAS_MIN_MEM_MI" -eq 0 ] || { [ "$m" -gt 0 ] && [ "$m" -lt "$TAS_MIN_MEM_MI" ]; }; then
      TAS_MIN_MEM_MI="$m"
    fi
  done <<EOF
$out
EOF

  # The per-zone tallies run in awk rather than in a bash associative array: nothing else in this
  # suite requires bash 4, and one sourced library is not the place to change that.
  if [ -n "$zone_list" ]; then
    TAS_ZONES_SUMMARY="$(printf '%s' "$zone_list" \
      | awk '{c[$1]++} END {for (z in c) printf "%s=%d ", z, c[z]}')"
    # shellcheck disable=SC2034 # Output consumed by the sourcing case.
    TAS_MAX_ZONE_NODES="$(printf '%s' "$TAS_ZONES_SUMMARY" | tr ' ' '\n' \
      | sed 's/.*=//' | grep -v '^$' | sort -rn | head -1)"
  fi

  return 0
}

# One case Pod's requests, derived from the measured shape: sixty percent of the smallest node's
# allocatable. Two of these never fit one node; one always fits every counted node. Both facts are
# what the caller's shape row asserts, so the numbers printed here are inputs to an assertion, not
# an assumption smuggled past one.
tas_pod_cpu_m() { awk -v m="$TAS_MIN_CPU_M" -v p="$TAS_POD_SHARE_PERCENT" 'BEGIN{printf "%d", int((m*p+99)/100)}'; }
tas_pod_mem_mi() { awk -v m="$TAS_MIN_MEM_MI" -v p="$TAS_POD_SHARE_PERCENT" 'BEGIN{printf "%d", int((m*p+99)/100)}'; }

# Apply the four disposable scheduling fixtures under one case prefix.
#
#   tas_apply_fixtures <prefix> <ns> <cpu-quota> <mem-quota>
#
# The quota strings are applied verbatim ("4000m", "6000Mi") because the caller computed them from
# the measured shape and asserts them back from the applied ClusterQueue; this function does no
# arithmetic of its own. Every object name starts with the prefix, so two cases never collide, and
# nothing operator-managed is touched: the ClusterQueue carries no management label for the
# operator's reconcilers to adopt and no AdmissionCheck for its barriers to hold.
#
# The apply's combined output is kept in TAS_APPLY_OUT: a webhook or schema refusal -- the likely
# failure on a Kueue that does not serve these APIs -- then reaches the caller as text it can quote
# in a row, instead of as an empty string behind a timeout.
tas_apply_fixtures() {
  local prefix="$1" ns="$2" cpu="$3" mem="$4"
  TAS_APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: kueue.x-k8s.io/v1beta2
kind: Topology
metadata:
  name: ${prefix}-topo
spec:
  levels:
  - nodeLabel: ${TAS_ZONE_LABEL}
  - nodeLabel: kubernetes.io/hostname
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ResourceFlavor
metadata:
  name: ${prefix}-rf
spec:
  topologyName: ${prefix}-topo
  nodeLabels:
    kubernetes.io/os: linux
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: ClusterQueue
metadata:
  name: ${prefix}-cq
spec:
  namespaceSelector: {}
  queueingStrategy: BestEffortFIFO
  resourceGroups:
  - coveredResources:
    - cpu
    - memory
    flavors:
    - name: ${prefix}-rf
      resources:
      - name: cpu
        nominalQuota: ${cpu}
      - name: memory
        nominalQuota: ${mem}
---
apiVersion: kueue.x-k8s.io/v1beta2
kind: LocalQueue
metadata:
  name: ${prefix}-lq
  namespace: ${ns}
spec:
  clusterQueue: ${prefix}-cq
YAML
)"
}

# Delete exactly the four fixtures tas_apply_fixtures created, by name, in the order that lets each
# referencing object go first. Idempotent, safe on pass and fail, and safe to re-run.
tas_delete_fixtures() {
  local prefix="$1" ns="$2"
  kubectl -n "$ns" delete localqueue.kueue.x-k8s.io "${prefix}-lq" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete clusterqueue.kueue.x-k8s.io "${prefix}-cq" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete resourceflavor.kueue.x-k8s.io "${prefix}-rf" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  kubectl delete topology.kueue.x-k8s.io "${prefix}-topo" \
    --ignore-not-found --wait=false >/dev/null 2>&1
  return 0
}

# Apply one batch Job whose Pods form a single Kueue PodSet required inside one zone.
#
#   tas_apply_zone_job <name> <ns> <localqueue> <pods> <cpu-m> <mem-mi> [level-label]
#
# The queue-name label routes the Job into the caller's LocalQueue, and the required-topology
# annotation is the public Kueue contract that makes the Workload's PodSet demand one domain of the
# named level. The image never exits, so an admitted group holds its quota for exactly as long as
# the caller keeps the Job. The apply's output is kept in TAS_APPLY_OUT for the same reason the
# fixtures' is.
tas_apply_zone_job() {
  local name="$1" ns="$2" lq="$3" pods="$4" cpu="$5" mem="$6" level="${7:-$TAS_ZONE_LABEL}"
  TAS_APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: batch/v1
kind: Job
metadata:
  name: ${name}
  namespace: ${ns}
  labels:
    kueue.x-k8s.io/queue-name: ${lq}
spec:
  parallelism: ${pods}
  completions: ${pods}
  template:
    metadata:
      annotations:
        kueue.x-k8s.io/podset-required-topology: ${level}
    spec:
      restartPolicy: Never
      containers:
      - name: work
        image: registry.k8s.io/pause:3.10
        resources:
          requests:
            cpu: ${cpu}m
            memory: ${mem}Mi
YAML
)"
}

# --- readers for a Kueue-created Workload -------------------------------------------------------
#
# All of them take the Workload's name and return "" for every shape this library cannot read,
# which callers already treat as "not admitted"/"not reserved" -- the states a negative probe
# asserts and a positive probe waits past.

tas_wl_admitted() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" \
    -o jsonpath='{.status.admission}' 2>/dev/null
}

tas_wl_condition() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" \
    -o jsonpath="{range .status.conditions[?(@.type==\"$3\")]}{.status}{end}" 2>/dev/null
}

tas_job_workload() {
  local uid
  uid="$(kubectl -n "$1" get job.batch "$2" -o jsonpath='{.metadata.uid}' 2>/dev/null)"
  [ -n "$uid" ] || return 0
  kubectl -n "$1" get workloads.kueue.x-k8s.io -l "kueue.x-k8s.io/job-uid=${uid}" \
    -o jsonpath='{.items[0].metadata.name}' 2>/dev/null
}

tas_wl_check_state() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" \
    -o jsonpath="{range .status.admissionChecks[?(@.name==\"$3\")]}{.state}{end}" 2>/dev/null
}

# Whether a Workload can be read at all. A negative probe asserts ABSENT admission and reservation,
# and the readers above return "" for an unreadable Workload too, so every negative sample also
# records this: a refusal read off an object that could not be fetched is no reading.
tas_wl_readable() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" -o name >/dev/null 2>&1
}

# Whether an admitted Workload carries a TAS topology assignment, plus its level keys for row text.
# Kueue writes topologyAssignment only for a PodSet it placed through a topology, so presence is
# the direct evidence that admission was TAS admission and not plain quota admission.
tas_wl_topology_levels() {
  kubectl -n "$1" get workloads.kueue.x-k8s.io "$2" -o json 2>/dev/null | tas_topology_levels_of_workload
}

# The assigned levels of every PodSet in one Workload JSON on stdin, one PodSet per space-separated
# entry with its levels comma-joined, or nothing when ANY PodSet lacks a non-empty
# topologyAssignment.levels. A jsonpath range cannot answer this: it prints its separator once per
# PodSet whether or not the field exists, so an absent assignment still reads as a non-empty string.
tas_topology_levels_of_workload() {
  jq -r '[.status.admission.podSetAssignments[]? | (.topologyAssignment.levels // [])] as $levels
    | if ($levels | length) > 0 and ($levels | all(length > 0))
      then $levels | map(join(",")) | join(" ") else empty end' 2>/dev/null
}

# Every Pod of a label selector as "name nodename", one per line, Pods without a node included
# with an empty nodename. The zone proof reads the NODE's zone label afterwards, never the
# requested annotation: placement is proven where the Pod actually landed.
tas_pod_nodes() {
  kubectl -n "$1" get pods -l "$2" \
    -o jsonpath='{range .items[*]}{.metadata.name} {.spec.nodeName}{"\n"}{end}' 2>/dev/null
}

# Whether a group's "name nodename" rows (tas_pod_nodes) hold exactly $2 Pods, every one bound. An
# unbound Pod renders with an empty nodename, so it is COUNTED here and fails the check; a later loop
# that reads only non-empty nodenames would silently skip it.
tas_rows_all_bound() {
  printf '%s\n' "$1" | awk -v want="$2" '
    NF == 0 { next }
    { n++; if (NF < 2) unbound++ }
    END { exit !(n == want && unbound == 0) }'
}

tas_node_zone() {
  kubectl get node "$1" \
    -o jsonpath="{.metadata.labels.topology\\.kubernetes\\.io/zone}" 2>/dev/null
}

# The verdict on a ClusterQueue watch reduced to TSV rows of
#
#   uid  stopPolicy  reservingWorkloads  oldFlavorsAllPresent  stoppedAtCurrentGeneration
#
# printed as four yes/no words, in order:
#
#   held      HoldAndDrain was observed while every old flavor was still referenced
#   reserved  that held old plan was observed while Kueue still counted a reservation
#   drained   AFTER such a reserved observation, the held old plan read stopped with zero reservations
#   dropped   an old flavor disappeared before the drain was observed
#
# A migration that holds an already-empty queue proves nothing about keeping a reservation's flavor,
# which is why `drained` counts only after `reserved`.
tas_cq_transition_verdict() {
  awk -F '\t' '
    $2 == "HoldAndDrain" && $4 == "true" {
      held = 1
      if ($3 > 0 && !drained) reserved = 1
      if (reserved && $3 == 0 && $5 == "true") drained = 1
    }
    $4 == "false" && !switched { switched = 1; if (!drained) dropped = 1 }
    END { print (held ? "yes" : "no"), (reserved ? "yes" : "no"), (drained ? "yes" : "no"), (dropped ? "yes" : "no") }
  ' "$1"
}

# The sorted "node value" pairs a snapshot file assigns to one label key. The snapshot is the YAML
# this case family writes: a two-space-indented Node name line, then four-space-indented labels.
tas_snapshot_pairs() {
  awk -v key="$2" '
    /^  [^ ]/ { node = $1; sub(/:$/, "", node) }
    /^    [^ ]/ { k = $1; sub(/:$/, "", k); if (k == key) print node, $2 }
  ' "$1" | sort
}

# The sorted "node value" pairs of one label key on the Nodes a selector matches. A Node without the
# label is listed with an empty value, so a missing projection reads as a mismatch, not an omission.
tas_node_label_pairs() {
  kubectl get nodes -l "$1" -o json 2>/dev/null \
    | jq -r --arg key "$2" '.items[] | .metadata.name + " " + (.metadata.labels[$key] // "")' | sort
}

# How many NodeFeatures one TopologySource owns, or "" when they cannot be listed.
tas_source_nodefeatures() {
  kubectl -n "$1" get nodefeatures.nfd.k8s-sigs.io -l "topology.gpustack.ai/source-uid=$2" -o json 2>/dev/null \
    | jq '[.items[]?] | length' 2>/dev/null
}

# --- ModelDeployment helpers --------------------------------------------------------------------
#
# These are shared because the join they carry is the one every multi-role ModelDeployment reading
# needs: a Kueue pod-group Workload carries plain owner references to the Pods of its group and NO
# reference of any kind to the ModelDeployment, so the only road from a Workload to its deployment
# runs through the owner Pods and the deployment label those Pods carry.

# The first InstanceType a deployment can actually name. The list comes back sorted by name and
# carries types on their way out (a sibling case creates and deletes `caseNN-nowhere` without
# waiting, and that name sorts ahead of ordinary derived types); inactive types are excluded for
# the mirror reason -- a deployment on one is admitted and then never scheduled.
tas_md_usable_instance_type() {
  kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        echo "$name"
        break
      done
}

# The ClusterQueue an InstanceType schedules into, resolved the way the operator resolves it: the
# type's published entrance LocalQueue, then that LocalQueue's ClusterQueue.
tas_md_cq_of_it() {
  local lq
  lq="$(kubectl get instancetypes.worker.gpustack.ai "$2" \
    -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$lq" ] || return 0
  kubectl -n "$1" get localqueue.kueue.x-k8s.io "$lq" \
    -o jsonpath='{.spec.clusterQueue}' 2>/dev/null
}

# One role block for a ModelDeployment. The image and command are placeholder plumbing: a CPU-only
# InstanceType has observed no accelerator and the operator can synthesize no engine image, and no
# reading that matters is about what the container runs.
tas_md_role_block() {
  printf '  - name: %s\n' "$1"
  [ -n "$2" ] && printf '    kind: %s\n' "$2"
  printf '    instanceType: %s\n    replicas: %s\n' "$3" "$4"
  printf '    image: %s\n    command: ["/pause"]\n' "$5"
  [ -n "${6:-}" ] && printf '    size: %s\n' "$6"
  if [ -n "${7:-}" ]; then
    printf '    topology:\n      requiredLevel: %s\n' "$7"
  fi
}

# Apply one ModelDeployment. The InstanceType, the image and the command live in the role blocks
# the caller builds with tas_md_role_block; what this function owns is the wrapper they ride in.
# The apply's output is kept in TAS_APPLY_OUT: a schema or webhook refusal surfaces as text a row
# can quote instead of as a timeout that blames the wrong thing.
tas_md_apply() {
  local name="$1" ns="$2" binding="$3" roles="$4"
  # shellcheck disable=SC2034 # Output consumed by the sourcing case.
  TAS_APPLY_OUT="$(cat <<YAML | kubectl apply -f - 2>&1
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${name}
  namespace: ${ns}
spec:
  engine:
    name: vllm
    version: "0.11.0"
  model:
    name: Qwen/Qwen2.5-0.5B-Instruct
  kvCache:
    poolRef:
      name: ${binding}
  roles:
${roles}
YAML
)"
}

# Every Workload of one ModelDeployment, one name per line.
#
# THE LOOKUP RUNS WORKLOAD-TO-DEPLOYMENT, never deployment-to-Workload: the Workload is enumerated,
# each of its owner references is read as a Pod NAME and UID, and the Workload is attributed when
# either matches a Pod this deployment owns. Looking the other way -- searching for a Workload that
# carries a direct ModelDeployment owner -- finds nothing on any correct cluster, because Kueue's
# pod integration writes no such reference; a case built on it would pass as vacuously as it
# failed. The UIDs are matched padded so one UID cannot hit a prefix of a longer one.
tas_md_workloads() {
  local ns="$1" md="$2" pods uids names row wl refs t
  pods="$(kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$pods" ] || return 0
  uids="$(printf '%s\n' "$pods" | cut -d= -f2 | tr '\n' ' ')"
  names="$(printf '%s\n' "$pods" | cut -d= -f1 | tr '\n' ' ')"

  kubectl -n "$ns" get workloads.kueue.x-k8s.io \
    -o jsonpath='{range .items[*]}{.metadata.name}|{range .metadata.ownerReferences[*]}{.name}/{.uid}{";"}{end}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r wl refs; do
        [ -n "$wl" ] || continue
        for t in ${refs//;/ }; do
          case " ${uids} " in *" ${t##*/} "*) echo "$wl"; continue 2 ;; esac
          case " ${names} " in *" ${t%/*} "*) echo "$wl"; continue 2 ;; esac
        done
      done
}

# Wait until a ModelDeployment holds exactly $3 Pods. Kueue composes nothing for a group short of
# its declared total, so this is the precondition for every Workload reading that follows.
tas_md_wait_pods() {
  local ns="$1" md="$2" want="$3"
  for _ in $(seq 1 45); do
    [ "$(kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
      --no-headers 2>/dev/null | wc -l | tr -d ' ')" = "$want" ] && return 0
    sleep 2
  done
  return 1
}

# Wait until at least $3 Workloads are attributable to a ModelDeployment.
tas_md_wait_workloads() {
  local ns="$1" md="$2" want="$3"
  for _ in $(seq 1 45); do
    [ "$(tas_md_workloads "$ns" "$md" | grep -c . || true)" -ge "$want" ] && return 0
    sleep 3
  done
  return 1
}

# Delete every Workload owning a ModelDeployment's Pods, which is what releases Kueue's finalizer
# on a serving group. ONE LIST CALL PER INVOCATION, and EVERY owned Workload rather than the first:
# this runs from teardown, a deployment of several replicas holds several Workloads, and releasing
# one of them leaves the rest holding finalizers on Pods a later case then inherits.
tas_md_force_release() {
  local ns="$1" md="$2" uids row wl owners u
  uids="$(kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in
        *" $u "*)
          kubectl -n "$ns" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(kubectl -n "$ns" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF

  return 0
}
