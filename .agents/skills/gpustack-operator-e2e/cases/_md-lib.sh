#!/usr/bin/env bash
#
# _md-lib.sh — the ModelDeployment readers, renderers and dry-run harness the cases share.
#
# NOT A CASE. The leading underscore keeps it out of the `case-N.sh` namespace. The rows and the
# verdict stay with the case that sources it: this file prints the readings a row is built from, and
# it never prints the results table, records a row or decides the exit code.
#
# A case uses it as:
#
#     record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
#     # shellcheck source=/dev/null
#     . "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_md-lib.sh"
#
# Every helper here takes what it reads as an argument. Nothing reaches into a case's variables,
# because the same helper serves cases whose names for the same thing differ.
#
# `_epd-lib.sh` holds the CPU-vendor fixture and its platform lookup, and it stays a separate
# library. `md_usable_type_for` below is the same lookup as that library's `epd_platform_type`; the
# two are deliberately not merged, so a case that sources one does not inherit the other's fixture.

# The cluster context every reader in this library talks to. It is empty by default, which is the
# context plain kubectl talks to as well; a case that runs against another context sets it.
MD_KUBE_CONTEXT="${MD_KUBE_CONTEXT:-}"

# md_kubectl runs kubectl against MD_KUBE_CONTEXT when one is named, and against the default
# context otherwise.
md_kubectl() {
  if [ -n "$MD_KUBE_CONTEXT" ]; then
    kubectl --context "$MD_KUBE_CONTEXT" "$@"
  else
    kubectl "$@"
  fi
}

# ---------------------------------------------------------------- the InstanceType a role names

# md_usable_instance_type prints the first InstanceType a deployment can name. A type that is
# deleting or inactive is skipped: naming one is refused at admission, or the deployment is admitted
# and then never scheduled.
#
# Every name passed as an argument is excluded. A case that authors a type of its own excludes it
# here, because a leftover of it would otherwise be picked and measured as if it were a working one.
md_usable_instance_type() {
  local name deleting inactive skip
  md_kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        for skip in "$@"; do
          [ "$name" = "$skip" ] && continue 2
        done
        echo "$name"
        break
      done
}

# md_usable_type_for prints the first InstanceType reporting the named manufacturer. The webhook
# reads the platform the type a role names reports, so a row that needs a real type reads it by the
# manufacturer it expects rather than by list position.
md_usable_type_for() {
  local name deleting inactive manufacturer
  md_kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.status.detail.manufacturer}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive manufacturer; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        [ -n "$manufacturer" ] || continue
        if [ "$manufacturer" = "$1" ]; then
          echo "$name"
          break
        fi
      done
}

# md_usable_instance_types prints every InstanceType a deployment can name.
#
# AN ACCELERATED TYPE WITH NO WHOLE CARD IS EXCLUDED TOO. A role that names no accelerator count is
# defaulted to one card, and the webhook refuses a count above the type's whole-card capacity, so
# naming a type whose pool has no card left is refused for a reason none of the rows is about.
md_usable_instance_types() {
  local name deleting inactive acceleratable capacity
  md_kubectl get instancetypes.worker.gpustack.ai \
    -o jsonpath='{range .items[*]}{.metadata.name}|{.metadata.deletionTimestamp}|{.spec.inactive}|{.spec.acceleratable}|{.status.accelerator.capacity}{"\n"}{end}' \
    2>/dev/null \
    | while IFS='|' read -r name deleting inactive acceleratable capacity; do
        [ -n "$name" ] || continue
        [ -z "$deleting" ] || continue
        [ "$inactive" = true ] && continue
        if [ "$acceleratable" = true ]; then
          case "$capacity" in "" | 0) continue ;; esac
        fi
        echo "$name"
      done
}

# md_cq_of_it prints the ClusterQueue an InstanceType schedules into, resolved the way the operator
# resolves it: the type's published entrance LocalQueue, then that LocalQueue's ClusterQueue.
#
# It prints nothing when the type names no entrance, which is the same reading a lookup of an empty
# LocalQueue name gives.
md_cq_of_it() {
  local lq
  lq="$(md_kubectl get instancetypes.worker.gpustack.ai "$2" \
    -o jsonpath='{.status.entrance}' 2>/dev/null)"
  [ -n "$lq" ] || return 0
  md_kubectl -n "$1" get localqueue.kueue.x-k8s.io "$lq" \
    -o jsonpath='{.spec.clusterQueue}' 2>/dev/null
}

# ---------------------------------------------------------------- the manifest and its roles

# md_role_block prints one role of a ModelDeployment. The image and command are placeholder
# plumbing: no reading that matters is about what the container runs.
md_role_block() {
  printf '  - name: %s\n' "$1"
  [ -n "$2" ] && printf '    kind: %s\n' "$2"
  printf '    instanceType: %s\n    replicas: %s\n' "$3" "$4"
  printf '    image: %s\n    command: ["/pause"]\n' "$5"
  [ -n "${6:-}" ] && printf '    size: %s\n' "$6"
  if [ -n "${7:-}" ]; then
    printf '    topology:\n      requiredLevel: %s\n' "$7"
  fi
}

# md_manifest prints a ModelDeployment manifest whose roles block is supplied whole by the caller.
#
# The poolRef name expands with a bare dash and NOT with a colon-dash when the CALLER resolves it:
# the colon form substitutes the default for an EMPTY value as well as an unset one, which would
# rewrite an empty name a row is trying to submit. The kvCache block is the extra lines under
# kvCache, and it is empty for a manifest that declares nothing but the pool reference.
md_manifest() {
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: ModelDeployment
metadata:
  name: ${1}
  namespace: ${2}
spec:
  engine:
    name: ${3}
    image: ${4}
  model:
    name: ${8:-Qwen/Qwen2.5-0.5B-Instruct}
  kvCache:
    poolRef:
      name: ${5}
${6:-}
  roles:
${7}
YAML
}

# md_server_manifest prints a manifest declaring ONE role named server. The role-fields argument
# carries the extra fields of that role, one per line.
md_server_manifest() {
  local roles
  printf -v roles '  - name: server\n    instanceType: %s\n    replicas: 1\n%s\n' "$7" "${8:-}"
  md_manifest "$1" "$2" "$3" "$4" "$5" "$6" "$roles"
}

# md_epd_manifest prints a manifest declaring a prefill and a decode role on one instanceType. The
# explicit image and command are plumbing, not part of any rule.
md_epd_manifest() {
  local roles
  printf -v roles '  - name: prefill\n    kind: Prefill\n    instanceType: %s\n    replicas: 1\n    image: %s\n    command: ["/pause"]\n  - name: decode\n    kind: Decode\n    instanceType: %s\n    replicas: 1\n    image: %s\n    command: ["/pause"]\n' \
    "$7" "$4" "$7" "$4"
  md_manifest "$1" "$2" "$3" "$4" "$5" "$6" "$roles"
}

# md_apply applies one ModelDeployment and keeps the apply's output in MD_APPLY_OUT. The output is
# kept because a schema or webhook refusal surfaces as text a row can quote instead of as a timeout
# that blames the wrong thing.
md_apply() {
  local name="$1" ns="$2" binding="$3" roles="$4"
  local engine="${5:-vLLM}" image="${6:-registry.k8s.io/pause:3.10}"
  local model="${7:-Qwen/Qwen2.5-0.5B-Instruct}"
  # shellcheck disable=SC2034 # Output consumed by the sourcing case.
  MD_APPLY_OUT="$(md_manifest "$name" "$ns" "$engine" "$image" "$binding" "" "$roles" "$model" \
    | md_kubectl apply -f - 2>&1)"
}

# ---------------------------------------------------------------- what the cluster holds

# md_workloads prints every Workload of one ModelDeployment, one name per line.
#
# THE LOOKUP RUNS WORKLOAD-TO-DEPLOYMENT, never deployment-to-Workload: the Workload is enumerated,
# each of its owner references is read, and the Workload is attributed when one of them is a Pod this
# deployment owns. Looking the other way -- searching for a Workload that carries a direct
# ModelDeployment owner -- finds nothing on any correct cluster, because Kueue's pod integration
# writes no such reference. The UIDs are matched padded so one UID cannot hit a prefix of a longer
# one.
md_workloads() {
  local ns="$1" md="$2" uids row wl owners u
  uids="$(md_kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    owners="${row#*=}"
    for u in $uids; do
      case " $owners " in *" $u "*) echo "$wl"; break ;; esac
    done
  done <<EOF
$(md_kubectl -n "$ns" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# md_workload_verdicts prints every Workload owning one of a deployment's Pods, one
# "<name>=<status>=<role>" line each: status is its QuotaReserved condition, "" while Kueue has not
# judged it yet, and role is the component of the Pod it owns.
#
# TWO API CALLS PER INVOCATION, NOT ONE PER WORKLOAD, and here that is correctness rather than
# tidiness. This is the hot path: a verdict poll calls it every five seconds for up to five minutes,
# over a probe that can hold a hundred replicas. At one get per Workload the poll budget goes on
# round-trips and the case reports a pool Kueue never judged. Names, verdicts and owner uids come
# back together and the match happens locally.
md_workload_verdicts() {
  local ns="$1" md="$2" pods row wl rest status owners p
  pods="$(md_kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}={.metadata.labels.app\.kubernetes\.io/component}{"\n"}{end}' \
    2>/dev/null)"
  [ -n "$pods" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    rest="${row#*=}"
    status="${rest%%=*}"
    owners="${rest#*=}"
    for p in $pods; do
      # Padded on both sides so a uid cannot match a longer one it is a prefix of.
      case " $owners " in *" ${p%%=*} "*) echo "${wl}=${status}=${p#*=}"; break ;; esac
    done
  done <<EOF
$(md_kubectl -n "$ns" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.status.conditions[?(@.type=="QuotaReserved")].status}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF
}

# md_pod_uids prints the Pod name=UID of every Pod of one deployment, sorted.
md_pod_uids() {
  md_kubectl -n "$1" get pods -l "app.kubernetes.io/instance=$2" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort | tr '\n' ' '
}

# md_role_uids prints the Pod name=UID of one role, sorted. The UID is what tells a Pod that STAYED
# from one replaced by an identical render: everything else about the two agrees.
md_role_uids() {
  md_kubectl -n "$1" get pods -l "app.kubernetes.io/instance=$2,app.kubernetes.io/component=$3" \
    -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.uid}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort | tr '\n' ' '
}

# md_group_names prints the distinct pod-group names across a deployment's members: one per
# INSTANCE, so their count is how many instances Kueue is being asked to admit.
md_group_names() {
  md_kubectl -n "$1" get pods -l "app.kubernetes.io/instance=$2" \
    -o jsonpath='{range .items[*]}{.metadata.labels.kueue\.x-k8s\.io/pod-group-name}{"\n"}{end}' 2>/dev/null \
    | grep -v '^$' | sort -u
}

# md_surviving_uids prints how many of a recorded UID set are still present. With a role named, the
# set is compared against that role's Pods; without one, against every Pod of the deployment.
md_surviving_uids() {
  local ns="$1" md="$2" before="$3" role="${4:-}" now n=0 e
  if [ -n "$role" ]; then
    now="$(md_role_uids "$ns" "$md" "$role")"
  else
    now="$(md_pod_uids "$ns" "$md")"
  fi
  for e in $before; do
    case " $now " in *" $e "*) n=$((n + 1)) ;; esac
  done
  echo "$n"
}

# md_force_release deletes every Workload owning a ModelDeployment's Pods, which is what releases
# Kueue's finalizer on a serving group.
#
# EVERY OWNED WORKLOAD, NOT THE FIRST ONE FOUND, and ONE LIST CALL PER INVOCATION. This runs from
# teardown, a deployment of several replicas holds several Workloads, and releasing one of them
# leaves the rest holding finalizers on Pods a later case then inherits.
md_force_release() {
  local ns="$1" md="$2" row wl uids u
  uids="$(md_kubectl -n "$ns" get pods -l "app.kubernetes.io/instance=${md}" \
    -o jsonpath='{range .items[*]}{.metadata.uid}{"\n"}{end}' 2>/dev/null)"
  [ -n "$uids" ] || return 0
  while IFS= read -r row; do
    [ -n "$row" ] || continue
    wl="${row%%=*}"
    for u in $uids; do
      case " ${row#*=} " in
        *" $u "*)
          md_kubectl -n "$ns" delete workloads.kueue.x-k8s.io "$wl" \
            --ignore-not-found --wait=false >/dev/null 2>&1
          break
          ;;
      esac
    done
  done <<EOF
$(md_kubectl -n "$ns" get workloads.kueue.x-k8s.io \
  -o jsonpath='{range .items[*]}{.metadata.name}={.metadata.ownerReferences[*].uid}{"\n"}{end}' 2>/dev/null)
EOF

  return 0
}

# ---------------------------------------------------------------- the server-side dry run

# The four helpers below read the manifest on STDIN, print the OBJECT text of the row they describe
# and return 0 when the outcome is the one the row asserts. The ROW ITSELF STAYS WITH THE CALLING
# CASE: the case turns the exit status into a PASS or a FAIL and records it, so the table and the
# verdict never leave the case that owns them.
#
# The manifest travels on stdin because a caller builds it, and a caller handing it over as an
# argument would have to strip the trailing newlines the builder emitted.

# md_dryrun_refuses asserts that the manifest is REFUSED and that the refusal quotes the given text
# and every further fragment passed after it. A refusal carrying somebody else's message is a
# failure: it means the manifest tripped a rule the row is not about.
#
# AN EMPTY OUTPUT MUST NOT PASS. The refusal is read out of the apply's output, and an output that
# is empty at all means nothing was refused: the row would report a refusal it never saw.
md_dryrun_refuses() {
  local want="$1"
  shift
  local out w frags="$*"
  out="$(md_kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  if [ -z "$out" ]; then
    echo "the apply produced no output at all, so nothing was refused"

    return 1
  fi
  for w in "$want" "$@"; do
    case "$out" in
      *"$w"*) ;;
      *) echo "refused with the wrong message, or accepted (missing '${w}'): ${out:0:220}"; return 1 ;;
    esac
  done
  echo "refused naming: ${want}${frags:+ + $frags}"
}

# md_dryrun_accepts asserts that the manifest is ACCEPTED. It carries the same empty-output trap as
# md_dryrun_refuses for the same reason: "created" must be observed, because a row that passes on no
# output at all reports an acceptance nothing produced.
md_dryrun_accepts() {
  local out
  out="$(md_kubectl apply --dry-run=server -f - 2>&1 | tr '\n' ' ')"
  case "$out" in
    *"created (server dry run)"* | *"configured (server dry run)"*)
      echo "accepted by both the schema and the webhook"

      return 0
      ;;
    *) echo "refused: ${out:0:220}"; return 1 ;;
  esac
}

# md_dryrun_rejects asserts that the manifest is REFUSED, whatever the refusal says. The refusal's
# own text is the row's object, cut to the given number of characters.
md_dryrun_rejects() {
  local max="${1:-160}" out
  if out="$(md_kubectl apply --dry-run=server -f - 2>&1)"; then
    echo "admitted: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"

    return 1
  fi
  echo "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-"$max")"
}

# md_dryrun_admits asserts that the manifest is ACCEPTED, whatever the acceptance says. The
# acceptance's own text is the row's object, cut to the given number of characters.
md_dryrun_admits() {
  local max="${1:-160}" out
  if out="$(md_kubectl apply --dry-run=server -f - 2>&1)"; then
    echo "$(printf '%s' "$out" | tr '\n' ' ' | cut -c1-"$max")"

    return 0
  fi
  echo "refused: $(printf '%s' "$out" | tr '\n' ' ' | cut -c1-160)"
}
