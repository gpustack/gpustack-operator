#!/usr/bin/env bash
#
# CASE 74 — The API has no leader snapshot, and the store's snapshot flags are refused in the leader's
#            extraArgs with the reason   (MUTATING, self-recovering)
#
#   case-74.sh <NS>
#
# Goal:        The store's snapshot is not offered, because restoring one can make the cache serve
#              another key's bytes instead of a miss. This case proves both halves on a live API
#              server: `leader.highAvailability.snapshot` is not a field of the installed schema, so a
#              strict client is refused on it as an unknown field and a lenient one has it pruned; and
#              the flags that would turn the snapshot on through the escape hatch are refused by the
#              webhook with that reason, on create and on an update to a running backend.
#
# Environment: Any cluster with the operator deployed; no GPU, no RDMA, no storage class. <NS> keeps
#              the suite's calling convention and is read only to clean up the leader Lease: a backend
#              is cluster-scoped.
#
# Inputs:      All real, nothing mocked. The creates and the updates are sent as server-side dry runs,
#              which pass through admission and persist nothing. The update needs an object to
#              update, so the case creates one backend with high availability, whose member group
#              selects no node: it renders a leader and no member Pod.
#
# Expected:    - the manifest without a snapshot is accepted (the positive baseline: without it a
#              refusal of every backend would pass);
#              - with `leader.highAvailability.snapshot`, a strict create is refused naming that
#              path as an unknown field, and a lenient create is accepted with the block pruned from
#              the object the server returns;
#              - `-enable_snapshot=true` in leader.extraArgs is refused on that entry with the
#              wrong-data reason, and `-snapshot_interval_seconds=60` with the reason that it is
#              read only under a refused switch;
#              - on the running backend, an update changing only the image is accepted, and an update
#              adding `-enable_snapshot_restore=true` is refused with the wrong-data reason.
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

# Every refusal is matched on its field AND its reason: extraArgs carries several refusals for other
# keys, so the path alone would pass on any of them.
UNKNOWN_RE='unknown field "spec\.connection\.managed\.leader\.highAvailability\.snapshot"'
SWITCH_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: snapshots are not supported: .*can serve another key.s bytes instead of a miss'
COMPANION_RE='spec\.connection\.managed\.leader\.extraArgs\[0\]: Forbidden: it is read only when enable_snapshot or enable_snapshot_restore is set'

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

# manifest <leader-extra-yaml> prints one backend with three elected leaders. The argument lands
# under `leader:`, so it can add a snapshot block or an extraArgs list. The member group selects a
# label no node carries, so the one object this case persists renders no member Pod.
manifest() {
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
        replicas: 3
        highAvailability:
          memberAddressing: Service
${1:-}
      members:
        - nodeSelector: {gpustack.ai/case-74-selects-no-node: "true"}
          medium: DRAM
          capacityPerMember: 1Gi
YAML
}

SNAPSHOT_YAML='          snapshot:
            persistentVolumeClaimName: mooncake-snapshots'

# expect_refused <label> <regex> <leader-extra-yaml> sends a server-side dry-run create and records
# whether it was refused for the reason the regex names, rather than for any other.
expect_refused() {
  local out
  if out="$(manifest "$3" | kubectl create --dry-run=server -f - 2>&1)"; then
    record FAIL "$1" "accepted: ${out}"
  elif [[ "$out" =~ $2 ]]; then
    record PASS "$1" "${BACKEND}"
  else
    record FAIL "$1" "refused by another rule: ${out}"
  fi
}

# ------------------------------------------------------------- create, as dry runs

if out="$(manifest | kubectl create --dry-run=server -f - 2>&1)"; then
  record PASS "create without a snapshot is accepted" "${BACKEND}"
else
  record FAIL "create without a snapshot is accepted" "${out}"
fi

if out="$(manifest "$SNAPSHOT_YAML" | kubectl create --dry-run=server --validate=strict -f - 2>&1)"; then
  record FAIL "a strict create with a snapshot is refused as an unknown field" "accepted: ${out}"
elif [[ "$out" =~ $UNKNOWN_RE ]]; then
  record PASS "a strict create with a snapshot is refused as an unknown field" "${BACKEND}"
else
  record FAIL "a strict create with a snapshot is refused as an unknown field" "refused by another rule: ${out}"
fi

# The lenient path: the warning goes to stderr and the object the server would store to stdout, so
# the two are read apart.
warn_file="$(mktemp)"
if obj="$(manifest "$SNAPSHOT_YAML" | kubectl create --dry-run=server --validate=warn -o json -f - 2>"$warn_file")"; then
  ha="$(printf '%s' "$obj" | jq -c '.spec.connection.managed.leader.highAvailability')"
  if [[ "$(cat "$warn_file")" =~ $UNKNOWN_RE ]] && [ "$(printf '%s' "$obj" | jq '.spec.connection.managed.leader.highAvailability | has("snapshot")')" = false ]; then
    record PASS "a lenient create with a snapshot is accepted with the block pruned" "highAvailability=${ha}"
  else
    record FAIL "a lenient create with a snapshot is accepted with the block pruned" "highAvailability=${ha} warnings=$(cat "$warn_file")"
  fi
else
  record FAIL "a lenient create with a snapshot is accepted with the block pruned" "refused: $(cat "$warn_file")"
fi
rm -f "$warn_file"

expect_refused "-enable_snapshot in extraArgs is refused for serving wrong data" "$SWITCH_RE" \
  '        extraArgs: ["-enable_snapshot=true"]'
expect_refused "-snapshot_interval_seconds in extraArgs is refused as read only under a refused switch" "$COMPANION_RE" \
  '        extraArgs: ["-snapshot_interval_seconds=60"]'

# ------------------------------------------------------------- update, against a running backend

if ! out="$(manifest | kubectl create -f - 2>&1)"; then
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
  -p '{"spec":{"connection":{"managed":{"leader":{"extraArgs":["-enable_snapshot_restore=true"]}}}}}' 2>&1)"; then
  record FAIL "an update adding -enable_snapshot_restore is refused" "accepted: ${out}"
elif [[ "$out" =~ $SWITCH_RE ]]; then
  record PASS "an update adding -enable_snapshot_restore is refused" "${BACKEND}"
else
  record FAIL "an update adding -enable_snapshot_restore is refused" "refused by another rule: ${out}"
fi

results
