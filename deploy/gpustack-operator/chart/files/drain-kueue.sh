#!/usr/bin/env bash
#
# Delete the Kueue objects the worker creates for itself, and wait for Kueue to finalize them,
# WHILE KUEUE IS STILL RUNNING.
#
#   drain-kueue.sh <NS> <RELEASE> <WORKER_DEPLOYMENT> [BUDGET_SECONDS]
#
# Run by the chart's pre-delete hook after drain.sh, and only where this release installed Kueue.
#
# Kueue pins every ClusterQueue, ResourceFlavor, Topology and AdmissionCheck it considers in use
# with kueue.x-k8s.io/resource-in-use, and only its own controller removes that finalizer. helm
# deletes the Kueue controller and the Kueue CRDs in the same pass, so whatever such object still
# exists at that moment is left Terminating with nobody to finalize it, and the CRD behind it with
# it. `helm uninstall --wait` then waits on those CRDs until --timeout expires and fails the
# uninstall; the post-delete cleanup.sh strips the finalizers only after that wait has given up.
#
# drain.sh takes the ClusterQueues away, because deleting an InstanceType makes the worker delete
# its queue. Nothing takes the rest away: the two AdmissionChecks are applied once at worker
# startup and never deleted, and a ResourceFlavor or Topology lives as long as the Node it was
# derived from. So they are deleted here, while Kueue can still finalize them.
#
# The worker is SCALED TO ZERO FIRST, and that is why this runs after drain.sh rather than inside
# it: the worker recreates a deleted ResourceFlavor or Topology from its Node straight away, and
# the drain needs the worker running. The release is being removed either way, so nothing is lost
# by stopping its worker a step early.
#
# Every object is chosen by the label the worker itself puts on it, never by a name pattern, so a
# user's own queues and flavors in the same Kueue are not touched. LocalQueues carry no such label
# and need none: each one is owned by the ClusterQueue it points at, and goes with it.
#
# UNLIKE drain.sh, IT FAILS LOUDLY. Where this step does not finish, the uninstall would hang on the
# Kueue CRDs until --timeout anyway, so a non-zero exit here only states that sooner and names the
# objects. The uninstall can be retried as is; `helm uninstall --no-hooks` bypasses the step.
set -uo pipefail

NS="${1:?usage: drain-kueue.sh <NS> <RELEASE> <WORKER_DEPLOYMENT> [BUDGET_SECONDS]}"
RELEASE="${2:?usage: drain-kueue.sh <NS> <RELEASE> <WORKER_DEPLOYMENT> [BUDGET_SECONDS]}"
WORKER="${3:?usage: drain-kueue.sh <NS> <RELEASE> <WORKER_DEPLOYMENT> [BUDGET_SECONDS]}"
BUDGET="${4:-120}"
DEADLINE=$(( $(date +%s) + BUDGET ))

echo "[drain-kueue] namespace=${NS} release=${RELEASE} worker=${WORKER} budget=${BUDGET}s"

# The same preflight as drain.sh and cleanup.sh, for the same reason: every question below would
# otherwise read an unreachable API server as "nothing left".
healthz=(kubectl get --raw=/healthz)
if command -v timeout >/dev/null 2>&1; then
  healthz=(timeout 10 "${healthz[@]}")
fi
if ! "${healthz[@]}" >/dev/null 2>&1; then
  echo "[drain-kueue] FAILED: cannot reach the API server" >&2
  exit 1
fi

# The template renders this step only where kueue.enabled is set, but that says this release
# SHIPPED a Kueue, not that the Kueue in the cluster is the one it shipped. The CRD's Helm ownership
# says that, and it is read the same way cleanup.sh's owned_crds reads it: both annotations, as a
# pair.
owner="$(kubectl get crd clusterqueues.kueue.x-k8s.io \
  -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-namespace}/{.metadata.annotations.meta\.helm\.sh/release-name}' \
  2>/dev/null)"
if [ "${owner}" != "${NS}/${RELEASE}" ]; then
  echo "[drain-kueue] skipped: Kueue is not owned by this release (owner=${owner:-none})"
  exit 0
fi

remaining_left() { # seconds left in the budget, never negative
  local left=$(( DEADLINE - $(date +%s) ))
  [ "${left}" -gt 0 ] && echo "${left}" || echo 0
}

# 1. Stop the worker. A release installed with worker.enabled=false has no Deployment to stop.
if kubectl -n "${NS}" get deployment "${WORKER}" >/dev/null 2>&1; then
  selector="$(kubectl -n "${NS}" get deployment "${WORKER}" \
    -o go-template='{{range $k, $v := .spec.selector.matchLabels}}{{$k}}={{$v}},{{end}}' 2>/dev/null)"
  selector="${selector%,}"
  if [ -z "${selector}" ]; then
    echo "[drain-kueue] FAILED: could not read the selector of deployment/${WORKER}" >&2
    exit 1
  fi
  if ! kubectl -n "${NS}" scale deployment "${WORKER}" --replicas=0 >/dev/null; then
    echo "[drain-kueue] FAILED: could not scale deployment/${WORKER} to zero" >&2
    exit 1
  fi
  # Waited for until the Pods are GONE, not until the Deployment reports zero: a terminating
  # worker still reconciles, and would recreate what step 2 deletes. Polled rather than
  # `kubectl wait --for=delete`, which fails outright when the Pods are already gone.
  until pods="$(kubectl -n "${NS}" get pods -l "${selector}" -o name 2>/dev/null)" && [ -z "${pods}" ]; do
    if [ "$(remaining_left)" -eq 0 ]; then
      echo "[drain-kueue] FAILED: the worker Pods (${selector}) did not stop within ${BUDGET}s" >&2
      exit 1
    fi
    sleep 3
  done
  echo "[drain-kueue] worker stopped"
fi

# 2. Delete, then wait. Each line is "resource selector"; the resource types are the ones the worker
#    names in systemmeta.NoteResource, and the AdmissionChecks carry the operator's part-of label.
OBJECTS="
clusterqueues.kueue.x-k8s.io resource.gpustack.ai/type=instancetypes
resourceflavors.kueue.x-k8s.io resource.gpustack.ai/type=nodes
topologies.kueue.x-k8s.io resource.gpustack.ai/type=topology
admissionchecks.kueue.x-k8s.io app.kubernetes.io/part-of=gpustack-operator
"

# Every delete is issued before any wait, because the finalizers clear in dependency order - a
# flavor only once no queue names it, a topology only once no flavor does - and Kueue works that
# out itself once all of them are marked.
while read -r res sel; do
  [ -n "${res}" ] || continue
  if ! kubectl delete "${res}" -l "${sel}" --ignore-not-found --wait=false >/dev/null; then
    echo "[drain-kueue] FAILED: the delete of ${res} (${sel}) was refused" >&2
    exit 1
  fi
done <<EOF
${OBJECTS}
EOF

# A failed list is not an empty one: it is treated as "still there", and the budget ends the wait.
left_over() {
  local res sel names
  while read -r res sel; do
    [ -n "${res}" ] || continue
    if ! names="$(kubectl get "${res}" -l "${sel}" -o name 2>/dev/null)"; then
      echo "${res} (could not be listed)"
      continue
    fi
    [ -z "${names}" ] || printf '%s\n' "${names}"
  done <<EOF
${OBJECTS}
EOF
}

while :; do
  remaining="$(left_over)"
  if [ -z "${remaining}" ]; then
    echo "[drain-kueue] done"
    exit 0
  fi
  [ "$(remaining_left)" -gt 0 ] || break
  sleep 3
done

echo "[drain-kueue] FAILED: Kueue did not finalize these within ${BUDGET}s:" >&2
printf '%s\n' "${remaining}" | sed 's|^|[drain-kueue]   |' >&2
exit 1
