#!/usr/bin/env bash
#
# CASE 74 — A leader snapshot is refused at admission, on create and on an update that adds it, and
#            the refusal names the field and the reason   (MUTATING, self-recovering)
#
#   case-74.sh <NS>
#
# Goal:        `leader.highAvailability.snapshot` is refused at any replica count, because restoring
#              a snapshot can make the cache serve another key's bytes instead of a miss. This case
#              proves the refusal on a live API server, where the webhook's registration, not only
#              its code, decides whether it fires: a create carrying the field is refused under one
#              replica and under three, an update adding it to a running backend is refused, and
#              each refusal is reported on the field itself with its reason.
#
# Environment: Any cluster with the operator deployed; no GPU, no RDMA, no storage class. <NS> keeps
#              the suite's calling convention and is not read: a backend is cluster-scoped, and no
#              claim is created because admission refuses the field before one would be looked up.
#
# Inputs:      All real, nothing mocked. The creates and the update are sent as server-side dry runs,
#              which pass through admission and persist nothing. The update needs an object to
#              update, so the case creates one backend with high availability and no snapshot, whose
#              member group selects no node: it renders a leader and no member Pod.
#
# Expected:    - the same manifest without the snapshot is accepted, under one replica and under
#              three (the positive baseline: without it a refusal of every backend would pass);
#              - with the snapshot it is refused under one replica and under three, each message
#              naming `spec.connection.managed.leader.highAvailability.snapshot` and the reason;
#              - on the running backend, an update adding the snapshot is refused the same way, and
#              an update changing only the image is accepted.
#
# Cleanup:     Trap deletes the one backend the case created, then its leader Lease by name (the
#              Lease carries no owner reference). Idempotent, runs on pass AND fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-74.sh <NS>}"
CASE_ID=74
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend is cluster-scoped.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-snap-${SFX}"
LEADER="${BACKEND}-leader"
PVC="${BACKEND}-snap"

# The refusal is matched on its field AND its reason. The claim-name rule reports on a path under
# this one, so the path alone would also match that rule.
REFUSAL_RE='spec\.connection\.managed\.leader\.highAvailability\.snapshot: Forbidden: .*can serve another key.s bytes instead of a miss'

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }
# shellcheck source=/dev/null
. "$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)/_rows-lib.sh"

results() {
  print_rows
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

teardown() {
  echo
  echo "[case-74] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  # The leader Lease carries no owner reference, so deleting the backend leaves it behind. Delete it
  # by name after the backend is gone, because a standby still running would campaign it back.
  kubectl wait --for=delete "kvcachebackends.worker.gpustack.ai/${BACKEND}" --timeout=120s >/dev/null 2>&1 || true
  kubectl -n "$NS" delete leases.coordination.k8s.io "$LEADER" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap teardown EXIT

# manifest <replicas> <with-snapshot:yes|no> prints one backend. The member group selects a label no
# node carries, so the one object this case persists renders no member Pod.
manifest() {
  local ha="highAvailability: {}"
  if [ "$2" = yes ]; then
    ha="highAvailability:
          snapshot:
            persistentVolumeClaimName: ${PVC}"
  fi
  cat <<YAML
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata:
  name: ${BACKEND}
spec:
  type: Mooncake
  image: ${IMAGE}
  transport:
    protocol: TCP
  connection:
    managed:
      leader:
        replicas: $1
        ${ha}
      members:
        - nodeSelector: {gpustack.ai/case-74-selects-no-node: "true"}
          medium: DRAM
          capacityPerMember: 1Gi
YAML
}

# ------------------------------------------------------------- create, as dry runs

for replicas in 1 3; do
  if out="$(manifest "$replicas" no | kubectl create --dry-run=server -f - 2>&1)"; then
    record PASS "create without a snapshot is accepted (replicas ${replicas})" "${BACKEND}"
  else
    record FAIL "create without a snapshot is accepted (replicas ${replicas})" "${out}"
  fi

  if out="$(manifest "$replicas" yes | kubectl create --dry-run=server -f - 2>&1)"; then
    record FAIL "create with a snapshot is refused (replicas ${replicas})" "accepted: ${out}"
  elif [[ "$out" =~ $REFUSAL_RE ]]; then
    record PASS "create with a snapshot is refused (replicas ${replicas})" "${BACKEND}"
  else
    record FAIL "create with a snapshot is refused (replicas ${replicas})" "refused by another rule: ${out}"
  fi
done

# ------------------------------------------------------------- update, against a running backend

if ! out="$(manifest 3 no | kubectl create -f - 2>&1)"; then
  record FAIL "the backend to update is created" "${out}"
  results; exit 1
fi

if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --dry-run=server --type=merge \
  -p "{\"spec\":{\"image\":\"${IMAGE%:*}:case-74\"}}" 2>&1)"; then
  record PASS "an update changing only the image is accepted" "${BACKEND}"
else
  record FAIL "an update changing only the image is accepted" "${out}"
fi

if out="$(kubectl patch kvcachebackends.worker.gpustack.ai "$BACKEND" --dry-run=server --type=merge \
  -p "{\"spec\":{\"connection\":{\"managed\":{\"leader\":{\"highAvailability\":{\"snapshot\":{\"persistentVolumeClaimName\":\"${PVC}\"}}}}}}}" 2>&1)"; then
  record FAIL "an update adding a snapshot is refused" "accepted: ${out}"
elif [[ "$out" =~ $REFUSAL_RE ]]; then
  record PASS "an update adding a snapshot is refused" "${BACKEND}"
else
  record FAIL "an update adding a snapshot is refused" "refused by another rule: ${out}"
fi

results
