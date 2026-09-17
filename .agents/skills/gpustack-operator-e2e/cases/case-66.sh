#!/usr/bin/env bash
#
# CASE 66 — A pre-identity member listing survives the CRD upgrade while its leader cannot list
#   segments, then resumes normal reporting when the listing returns   (MUTATING,
#   self-recovering; AUTO-SKIPS without Kubernetes 1.23 and both upgrade assets)
#
#   case-66.sh <NS>
#
# Goal:        A stored member listing from before segmentID and clientID became required must not
#              make a later status update invalid. While the leader's segment listing fails, the
#              current controller omits the WHOLE legacy listing, marks MembersMounted False with
#              LegacyMemberStatus, and degrades the backend. A successful listing later replaces
#              the old rows with rows carrying real identities and clears that migration reason.
#
# Environment: Kubernetes 1.23 only: this is the lowest supported API server and exercises the
#              pre-ratcheting stored-object path. AUTO-SKIPS unless E2E_CASE66_OLD_CRD and
#              E2E_CASE66_NEW_CRD name the two CRD manifests, and E2E_CASE66_OLD_IMAGE_REPOSITORY,
#              E2E_CASE66_OLD_IMAGE_TAG, E2E_CASE66_NEW_IMAGE_REPOSITORY, and
#              E2E_CASE66_NEW_IMAGE_TAG name images already available to every node. The case
#              never builds or loads images. It requires helm, jq, and a cluster where the named
#              operator release is safe to upgrade.
#
# Inputs:      All real. The case installs the old CRD and controller, creates one external
#              backend and writes its old-format status through that schema, then installs the
#              current CRD and controller. A local HTTP fixture returns a healthy leader response
#              but fails only /get_segments_detail. Creating a KVCachePool causes the independent
#              pool controller usedBy status writer to run during the failed-listing window.
#
# Expected:    - both the backend controller and the pool writer complete status updates after the
#                new CRD is installed;
#              - status.members is absent, not a partial or fabricated listing;
#              - phase is Degraded and MembersMounted is False with LegacyMemberStatus while the
#                fixture fails the listing;
#              - after the fixture serves two identified segments, both rows carry segmentID and
#                clientID, LegacyMemberStatus is gone, and MembersMounted becomes True.
#
# Cleanup:     Trap deletes the test backend, pool, fixture Service, Deployment and ConfigMap. It
#              restores the current CRD and controller image, not the old ones, because the caller
#              began from the current installed release. Idempotent, runs on pass AND fail, safe
#              to re-run after its previous objects have finished deleting.
set -uo pipefail

NS="${1:-}"
CASE_ID=66
RELEASE="${E2E_CASE66_RELEASE:-gpustack-operator}"
CHART="${E2E_CASE66_CHART:-deploy/gpustack-operator/chart}"
OLD_CRD="${E2E_CASE66_OLD_CRD:-}"
NEW_CRD="${E2E_CASE66_NEW_CRD:-}"
OLD_REPOSITORY="${E2E_CASE66_OLD_IMAGE_REPOSITORY:-}"
OLD_TAG="${E2E_CASE66_OLD_IMAGE_TAG:-}"
NEW_REPOSITORY="${E2E_CASE66_NEW_IMAGE_REPOSITORY:-}"
NEW_TAG="${E2E_CASE66_NEW_IMAGE_TAG:-}"
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-legacy-${SFX}"
POOL="kvcp-legacy-${SFX}"
FIXTURE="case66-admin-${SFX}"

FAILS=0
SKIPS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); [ "$1" = SKIP ] && SKIPS=$((SKIPS + 1)); return 0; }
results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for row in ${ROWS[@]+"${ROWS[@]}"}; do echo "$row" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed (${SKIPS} skipped)"
}
skip() { record SKIP "$1" "$2"; results; exit 0; }

assert_legacy_omitted() {
  local object="$1"
  printf '%s' "$object" | jq -e '.status | has("members") | not' >/dev/null || return 1
  [ "$(printf '%s' "$object" | jq -r '.status.phase')" = Degraded ] || return 1
  printf '%s' "$object" | jq -e '
    [.status.conditions[]? | select(.type == "MembersMounted")]
    | length == 1 and .[0].status == "False" and .[0].reason == "LegacyMemberStatus"
  ' >/dev/null
}

assert_recovered() {
  local object="$1"
  printf '%s' "$object" | jq -e '
    (.status.phase == "Ready") and
    ([.status.members[] | .segmentID] | sort == ["segment-a", "segment-b"]) and
    ([.status.members[] | .clientID] | sort == ["client-a", "client-b"]) and
    ([.status.conditions[]? | select(.type == "MembersMounted")]
      | length == 1 and .[0].status == "True" and .[0].reason != "LegacyMemberStatus")
  ' >/dev/null
}

assert_pool_use() {
  local object="$1"
  printf '%s' "$object" | jq -e --arg pool "$POOL" \
    '.status.usedBy[]? | select(.name == $pool)' >/dev/null
}

if [ "${CASE_66_LIB_ONLY:-}" = 1 ]; then
  return 0
fi

NS="${NS:?usage: case-66.sh <NS>}"

wait_for() {
  local name="$1" seconds="$2" predicate="$3" object i
  for ((i = 0; i < seconds; i += 3)); do
    object="$(kubectl -n "$NS" get kvcachebackend "$name" -o json 2>/dev/null)"
    if [ -n "$object" ] && "$predicate" "$object"; then
      printf '%s' "$object"
      return 0
    fi
    sleep 3
  done
  return 1
}

upgrade_operator() {
  local repository="$1" tag="$2"
  helm upgrade "$RELEASE" "$CHART" -n "$NS" --reuse-values \
    --set worker.image.repository="$repository" --set worker.image.tag="$tag" --set worker.image.pullPolicy=IfNotPresent >/dev/null &&
    kubectl -n "$NS" rollout status "deployment/${RELEASE}-worker" --timeout=300s >/dev/null
}

teardown() {
  kubectl -n "$NS" delete kvcachepool "$POOL" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete kvcachebackend "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete service,deploy,configmap -l "gpustack-e2e-case=${CASE_ID}-${SFX}" \
    --ignore-not-found --wait=false >/dev/null 2>&1 || true
  if [ -n "$NEW_CRD" ] && [ -n "$NEW_REPOSITORY" ] && [ -n "$NEW_TAG" ]; then
    kubectl apply -f "$NEW_CRD" >/dev/null 2>&1 || true
    upgrade_operator "$NEW_REPOSITORY" "$NEW_TAG" || true
  fi
}
server_minor="$(kubectl version -o json 2>/dev/null | jq -r '.serverVersion.minor' | tr -cd '0-9')"
[ "$server_minor" = 23 ] || skip "Kubernetes 1.23 migration path" "server minor is ${server_minor:-unreadable}; this case does not substitute a newer API server"
if ! [ -r "$OLD_CRD" ] || ! [ -r "$NEW_CRD" ]; then
  skip "old and new CRD assets" "set E2E_CASE66_OLD_CRD and E2E_CASE66_NEW_CRD to readable manifests"
fi
if [ -z "$OLD_REPOSITORY" ] || [ -z "$OLD_TAG" ] || [ -z "$NEW_REPOSITORY" ] || [ -z "$NEW_TAG" ]; then
  skip "old and new controller images" "set the four E2E_CASE66_*_IMAGE_{REPOSITORY,TAG} variables to images loaded or pullable on every node"
fi
command -v helm >/dev/null || skip "helm" "helm is required to exchange the controller image"
command -v jq >/dev/null || skip "jq" "jq is required to read status predicates"
trap teardown EXIT

kubectl -n "$NS" create configmap "$FIXTURE" --from-literal=health='{"status":"ok","role":"leader","ha_state":"serving","service_ready":true}' \
  --from-literal=metrics=$'master_total_capacity_bytes 1\nmaster_allocated_bytes 0\n' >/dev/null
kubectl -n "$NS" label configmap "$FIXTURE" "gpustack-e2e-case=${CASE_ID}-${SFX}" >/dev/null
kubectl -n "$NS" apply -f - <<YAML >/dev/null
apiVersion: apps/v1
kind: Deployment
metadata: {name: ${FIXTURE}, labels: {gpustack-e2e-case: ${CASE_ID}-${SFX}}}
spec:
  replicas: 1
  selector: {matchLabels: {app: ${FIXTURE}}}
  template:
    metadata: {labels: {app: ${FIXTURE}}}
    spec:
      containers:
        - name: admin
          image: busybox:1.36
          command: ["httpd", "-f", "-p", "9003", "-h", "/www"]
          volumeMounts: [{name: responses, mountPath: /www}]
      volumes: [{name: responses, configMap: {name: ${FIXTURE}}}]
---
apiVersion: v1
kind: Service
metadata: {name: ${FIXTURE}, labels: {gpustack-e2e-case: ${CASE_ID}-${SFX}}}
spec: {selector: {app: ${FIXTURE}}, ports: [{port: 9003, targetPort: 9003}]}
YAML
kubectl -n "$NS" rollout status deployment/"$FIXTURE" --timeout=180s >/dev/null || { record FAIL "fixture starts" "$FIXTURE"; results; exit 1; }

kubectl apply -f "$OLD_CRD" >/dev/null || { record FAIL "old CRD installs" "$OLD_CRD"; results; exit 1; }
upgrade_operator "$OLD_REPOSITORY" "$OLD_TAG" || { record FAIL "old controller installs" "$OLD_REPOSITORY:$OLD_TAG"; results; exit 1; }
kubectl -n "$NS" apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCacheBackend
metadata: {name: ${BACKEND}}
spec:
  type: Mooncake
  connection:
    external:
      endpoints:
        - {name: Client, address: ${FIXTURE}.${NS}.svc:50051}
        - {name: Admin, address: ${FIXTURE}.${NS}.svc:9003}
YAML
kubectl -n "$NS" scale deployment/"${RELEASE}-worker" --replicas=0 >/dev/null &&
  kubectl -n "$NS" rollout status deployment/"${RELEASE}-worker" --timeout=180s >/dev/null || {
  record FAIL "old controller quiesces" "${RELEASE}-worker"
  results
  exit 1
}
kubectl -n "$NS" get kvcachebackend "$BACKEND" -o json | jq \
  '.status = {"phase":"Ready","members":[{"segmentName":"legacy-address","state":"Ready","protocol":"TCP"}]}' | \
  kubectl replace --raw "/apis/worker.gpustack.ai/v1alpha1/kvcachebackends/${BACKEND}/status" -f - >/dev/null || {
  record FAIL "old-schema status writes" "$BACKEND"
  results
  exit 1
}

kubectl apply -f "$NEW_CRD" >/dev/null || { record FAIL "new CRD installs" "$NEW_CRD"; results; exit 1; }
upgrade_operator "$NEW_REPOSITORY" "$NEW_TAG" || { record FAIL "current controller installs" "$NEW_REPOSITORY:$NEW_TAG"; results; exit 1; }
kubectl -n "$NS" apply -f - <<YAML >/dev/null
apiVersion: worker.gpustack.ai/v1alpha1
kind: KVCachePool
metadata: {name: ${POOL}}
spec:
  backends: [${BACKEND}]
  quota: {total: 1Gi}
YAML

if legacy="$(wait_for "$BACKEND" 120 assert_legacy_omitted)"; then
  record PASS "the backend writer omits a legacy listing" "$BACKEND is Degraded with LegacyMemberStatus"
else
  record FAIL "the backend writer omits a legacy listing" "$BACKEND did not reach the guarded status while /get_segments_detail returned 404"
fi
if [ -z "$legacy" ]; then
  record SKIP "the independent pool writer completes" "guarded backend status never observed; cannot judge usedBy"
elif wait_for "$BACKEND" 60 assert_pool_use >/dev/null; then
  record PASS "the independent pool writer completes" "status.usedBy names $POOL without reviving members"
else
  record FAIL "the independent pool writer completes" "status.usedBy never named $POOL"
fi

SEGMENTS='{"total_segments":2,"segments":[{"segment_id":"segment-a","client_id":"client-a","segment_name":"10.0.0.1","status":"OK","protocol":"tcp","te_endpoint":"10.0.0.1:1"},{"segment_id":"segment-b","client_id":"client-b","segment_name":"10.0.0.2","status":"OK","protocol":"tcp","te_endpoint":"10.0.0.2:1"}]}'
printf '%s' "$SEGMENTS" | jq -e . >/dev/null || { record FAIL "recovery fixture payload" "$FIXTURE"; results; exit 1; }
kubectl -n "$NS" patch configmap "$FIXTURE" --type=merge \
  -p "$(jq -nc --arg segments "$SEGMENTS" '{data: {get_segments_detail: $segments}}')" >/dev/null || {
  record FAIL "recovery fixture update" "$FIXTURE"
  results
  exit 1
}
kubectl -n "$NS" rollout restart deployment/"$FIXTURE" >/dev/null &&
  kubectl -n "$NS" rollout status deployment/"$FIXTURE" --timeout=180s >/dev/null || {
  record FAIL "recovery fixture restarts" "$FIXTURE"
  results
  exit 1
}
if wait_for "$BACKEND" 120 assert_recovered >/dev/null; then
  record PASS "a successful listing replaces the migration status" "$BACKEND publishes two identified rows and clears LegacyMemberStatus"
else
  record FAIL "a successful listing replaces the migration status" "$BACKEND did not recover after the fixture served identified rows"
fi
results
