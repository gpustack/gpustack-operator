#!/usr/bin/env bash
#
# CASE 74 — A snapshot claim declared ReadWriteOnce is refused loudly, not fatally: the condition
#            names the claim while the backend still serves   (MUTATING, self-recovering)
#
#   case-74.sh <NS>
#
# Goal:        A replicated leader under high availability writes snapshots a standby must be able
#              to read, so the snapshot claim has to be shared storage. When the claim is bound to
#              a volume offering only ReadWriteOnce, the operator must say so through its condition
#              -- SnapshotStorageShared=False with reason ClaimNotShared and a message naming the
#              claim and the missing mode -- while the backend itself still reaches Ready with
#              exactly one serving leader replica. The failure is loud but non-fatal, and this case
#              proves both halves of that sentence in one run: the condition fires, and nothing
#              stops serving because of it.
#
# Environment: Any cluster; no GPU, no RDMA (members are DRAM over TCP). <NS> must be the
#              operator's system namespace, because the backend renders its leader and members
#              there and the snapshot claim must sit beside them. The fixture builds its own
#              in-cluster NFS server and a static PV through the installed NFS CSI driver, whose
#              registered name on this fleet is nfs.csi.gpustack.ai -- NOT the upstream default
#              nfs.csi.k8s.io: a PV naming the upstream driver still binds its claim but fails at
#              kubelet mount time, which would turn this case's Ready row into a mount error.
#              The pod-level half (a second leader pod refused by Multi-Attach /
#              FailedAttachVolume) is ENV-GATED: it asserts only when some CSIDriver on the cluster
#              sets AttachRequired=true; with every driver non-attachable the attach controller
#              never participates, no refusal is producible, and the row SKIPs naming the storage
#              fabric as the carrier rather than silently dropping the half.
#
# Inputs:      All real, nothing mocked. The case creates: an NFS server (Deployment + Service,
#              emptyDir backing, the privileged gists/nfs-server image), a static PV declared
#              ReadWriteOnce through nfs.csi.gpustack.ai, an RWO claim on it, and one KVCacheBackend
#              (3-replica HA leader, multi-tenancy on, snapshot claim = the PVC). No failover and
#              no client: the claim's modes are read by the reconciler from the BOUND volume, which
#              is why the case waits for Bound before creating the backend.
#
# Expected:    - the claim reaches Bound (the reconciler reads the bound volume's access modes, so
#              a claim that never binds says nothing and this row fires first);
#              - SnapshotStorageShared is False with reason ClaimNotShared;
#              - the condition's message names the claim and the missing ReadWriteMany mode;
#              - the backend still reaches Ready with exactly one ready leader replica of three;
#              - ENV-GATED: on a cluster with an attach-enforcing CSIDriver, at least one leader
#              pod carries a Multi-Attach / FailedAttachVolume event; otherwise an explicit SKIP
#              names the carrier.
#
# Cleanup:     Trap deletes the backend, the claim, the PV (Retain policy, so it needs its own
#              delete), and the NFS server's Deployment and Service. Idempotent, runs on pass AND
#              fail, safe to re-run.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail on
# transport alone, and a check that takes such a failure for an answer reports a verdict about the
# network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/../../_e2e-lib/scripts/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: case-74.sh <NS>}"
CASE_ID=74
IMAGE="${E2E_MOONCAKE_IMAGE:-gpustack/mirrored-mooncake:0.3.13.post1-cpu}"

# The suffix keeps two runs of this case apart: the backend and the PV are cluster-scoped, and the
# claim, the server and the storage class all derive their names from it.
SFX="$(set +o pipefail; LC_ALL=C tr -dc 'a-z0-9' </dev/urandom 2>/dev/null | head -c 5)"
[ -n "$SFX" ] || SFX="$$$(date +%s)"
BACKEND="kvcb-rwo-${SFX}"
NFS="${BACKEND}-nfs"
PV="${BACKEND}-pv"
PVC="${BACKEND}-snap"
CLASS="${BACKEND}-nfs"

LEADER_SEL="app.kubernetes.io/name=kv-cache-backend,app.kubernetes.io/instance=${BACKEND},app.kubernetes.io/component=leader"

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

results() {
  echo
  echo "STATUS | CHECK | OBJECT"
  for r in ${ROWS[@]+"${ROWS[@]}"}; do echo "$r" | awk -F'|' '{printf "%s | %s | %s\n", $1, $2, $3}'; done
  [ "$FAILS" -eq 0 ] || { echo "[case-${CASE_ID}] ${FAILS} check(s) FAILED"; return 1; }
  echo "[case-${CASE_ID}] all checks passed"
  return 0
}

teardown() {
  echo
  echo "[case-74] cleanup"
  kubectl delete kvcachebackends.worker.gpustack.ai "$BACKEND" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete pvc "$PVC" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl delete pv "$PV" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete deployment "$NFS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
  kubectl -n "$NS" delete service "$NFS" --ignore-not-found --wait=false >/dev/null 2>&1 || true
}
trap teardown EXIT

# wait_for polls one jsonpath until it equals what is wanted, and prints the LAST value seen, so a
# timeout says what the object was doing rather than only that it timed out.
wait_for() {
  local kind="$1" name="$2" path="$3" want="$4" secs="${5:-180}"
  local got="" i
  for ((i = 0; i < secs; i += 3)); do
    got="$(kubectl -n "$NS" get "$kind" "$name" -o "jsonpath=$path" 2>/dev/null)"
    [ "$got" = "$want" ] && { echo "$got"; return 0; }
    sleep 3
  done
  echo "$got"
  return 1
}

# ------------------------------------------------------------- shared-storage fixture

# An in-cluster NFS server, the same shape this repo's own testing infra ships: one privileged pod
# exporting an emptyDir. A static PV then presents it through the installed NFS CSI driver, so the
# claim can bind without any dynamic provisioner existing on the cluster.
kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: Service
metadata:
  name: ${NFS}
  namespace: ${NS}
  labels:
    app.kubernetes.io/part-of: ${NFS}
spec:
  type: ClusterIP
  selector:
    app.kubernetes.io/part-of: ${NFS}
  ports:
    - name: tcp-2049
      port: 2049
      protocol: TCP
    - name: udp-111
      port: 111
      protocol: UDP
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${NFS}
  namespace: ${NS}
spec:
  replicas: 1
  selector:
    matchLabels:
      app.kubernetes.io/part-of: ${NFS}
      app.kubernetes.io/component: server
  template:
    metadata:
      labels:
        app.kubernetes.io/part-of: ${NFS}
        app.kubernetes.io/component: server
    spec:
      nodeSelector:
        kubernetes.io/os: linux
      containers:
        - name: main
          image: gists/nfs-server:2.6.4
          env:
            - name: NFS_DIR
              value: /nfs-share
            - name: NFS_DOMAIN
              value: "*"
            - name: NFS_OPTION
              value: "fsid=0,rw,sync,insecure,all_squash,anonuid=65534,anongid=65534,no_subtree_check,nohide"
          ports:
            - name: tcp-2049
              containerPort: 2049
              protocol: TCP
            - name: udp-111
              containerPort: 111
              protocol: UDP
          volumeMounts:
            - name: nfs-data-dir
              mountPath: /nfs-share
          securityContext:
            privileged: true
            capabilities:
              add: ["SYS_ADMIN", "SETPCAP"]
      volumes:
        - name: nfs-data-dir
          emptyDir: {}
YAML

if ! wait_for deployment "$NFS" '{.status.readyReplicas}' 1 180 >/dev/null; then
  record FAIL "the fixture NFS server serves" \
    "readyReplicas settled at '$(kubectl -n "$NS" get deployment "$NFS" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1; nothing below can run"
  results; exit 1
fi

# The static PV is declared ReadWriteOnce ON PURPOSE: on this driver RWO is advisory (non-attachable,
# so nothing refuses a second node's mount), and the operator's check reads the bound volume's
# modes, which makes an NFS-backed RWO claim indistinguishable from a block RWO one.
kubectl apply -f - <<YAML >/dev/null
apiVersion: v1
kind: PersistentVolume
metadata:
  name: ${PV}
spec:
  capacity:
    storage: 1Gi
  accessModes:
    - ReadWriteOnce
  persistentVolumeReclaimPolicy: Retain
  storageClassName: ${CLASS}
  csi:
    driver: nfs.csi.gpustack.ai
    volumeHandle: ${PV}
    volumeAttributes:
      server: ${NFS}.${NS}.svc.cluster.local
      share: /
---
apiVersion: v1
kind: PersistentVolumeClaim
metadata:
  name: ${PVC}
  namespace: ${NS}
spec:
  accessModes:
    - ReadWriteOnce
  resources:
    requests:
      storage: 1Gi
  storageClassName: ${CLASS}
YAML

# The claim must be Bound BEFORE the backend exists: the reconciler reads the BOUND volume's access
# modes, not the claim's request, so the ordering is what makes the condition's content this case's
# doing rather than a race between binding and reconciliation.
if wait_for pvc "$PVC" '{.status.phase}' Bound 90 >/dev/null; then
  record PASS "the claim reaches Bound before the backend reads it" \
    "pvc ${PVC} phase=Bound, volume modes read from the bound PV"
else
  record FAIL "the claim reaches Bound before the backend reads it" \
    "phase settled at '$(kubectl -n "$NS" get pvc "$PVC" -o jsonpath='{.status.phase}' 2>/dev/null)'; a claim that never binds is never judged"
  results; exit 1
fi

# ------------------------------------------------------------- the backend under test

kubectl apply -f - <<YAML >/dev/null
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
        multiTenancy: true
        highAvailability:
          snapshot:
            persistentVolumeClaimName: ${PVC}
      members:
        - nodeSelector: {kubernetes.io/os: linux}
          medium: DRAM
          capacityPerMember: 2Gi
YAML

if ! wait_for kvcachebackends.worker.gpustack.ai "$BACKEND" '{.status.phase}' Ready 300 >/dev/null; then
  record FAIL "the backend still reaches Ready on an unshared claim" \
    "phase never became Ready in 300s; if no leader Pod schedules, check the fixture PV's driver name first (must be nfs.csi.gpustack.ai): $(kubectl -n "$NS" get pod -l "$LEADER_SEL" -o jsonpath='{range .items[*]}{.metadata.name}={.status.phase}{" "}{end}' 2>/dev/null)"
  results; exit 1
fi
record PASS "the backend still reaches Ready on an unshared claim" \
  "phase=Ready for ${BACKEND} -- the refusal is loud but non-fatal"

if [ "$(kubectl -n "$NS" get deployment "${BACKEND}-leader" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)" = "1" ]; then
  record PASS "exactly one leader replica is Ready" "readyReplicas=1 with 3 desired -- the election gate holds its shape"
else
  record FAIL "exactly one leader replica is Ready" \
    "readyReplicas='$(kubectl -n "$NS" get deployment "${BACKEND}-leader" -o jsonpath='{.status.readyReplicas}' 2>/dev/null)' instead of 1"
fi

# ------------------------------------------------------------- the loud half

COND="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{.status.conditions[?(@.type=="SnapshotStorageShared")].status}|{.status.conditions[?(@.type=="SnapshotStorageShared")].reason}' 2>/dev/null)"
COND_STATUS="${COND%%|*}"
COND_REASON="${COND##*|}"
COND_MSG="$(kubectl get kvcachebackends.worker.gpustack.ai "$BACKEND" \
  -o jsonpath='{.status.conditions[?(@.type=="SnapshotStorageShared")].message}' 2>/dev/null)"

if [ "$COND_STATUS" = "False" ] && [ "$COND_REASON" = "ClaimNotShared" ]; then
  record PASS "the condition fires: SnapshotStorageShared=False/ClaimNotShared" \
    "observed '${COND_STATUS}/${COND_REASON}'"
else
  record FAIL "the condition fires: SnapshotStorageShared=False/ClaimNotShared" \
    "observed '${COND_STATUS}/${COND_REASON}'"
fi

if [[ "$COND_MSG" == *"$PVC"* && "$COND_MSG" == *"ReadWriteMany"* ]]; then
  record PASS "the message names the claim and the missing mode" "${COND_MSG}"
else
  record FAIL "the message names the claim and the missing mode" \
    "message lacks the claim name ('${PVC}') or 'ReadWriteMany': ${COND_MSG}"
fi

# ------------------------------------------------------------- the env-gated pod-level half

# The second pod's own error only exists where an attach-enforcing driver can refuse the mount: the
# kubelet's Multi-Attach rejection rides the attach controller, which only runs for drivers with
# AttachRequired=true. With every driver non-attachable the absence of the error is the storage
# fabric's, not the operator's, and the half is SKIPped naming that carrier instead of dropped.
ATTACH_ENFORCERS="$(kubectl get csidrivers \
  -o jsonpath='{range .items[?(@.spec.attachRequired==true)]}{.metadata.name}{" "}{end}' 2>/dev/null)"

if [ -z "$ATTACH_ENFORCERS" ]; then
  record SKIP "a second leader pod is refused by Multi-Attach (env-gated)" \
    "no CSIDriver on this cluster sets AttachRequired=true, so the attach controller never participates and the pod-level refusal is not producible here; carrier: the cluster's storage fabric (non-attachable NFS driver). Observed drivers: $(kubectl get csidrivers -o jsonpath='{range .items[*]}{.metadata.name}(attachRequired={.spec.attachRequired}){" "}{end}' 2>/dev/null)"
else
  MULTI_ATTACH_EVENT=""
  for ((i = 0; i < 120; i += 5)); do
    MULTI_ATTACH_EVENT="$(kubectl -n "$NS" get events --field-selector involvedObject.kind=Pod \
      -o jsonpath='{range .items[?(@.reason=="FailedAttachVolume")]}{.involvedObject.name}|{.message}{"\n"}{end}' 2>/dev/null \
      | /usr/bin/grep -F "${BACKEND}-leader" | head -1)"
    [ -n "$MULTI_ATTACH_EVENT" ] && break
    sleep 5
  done
  if [ -n "$MULTI_ATTACH_EVENT" ]; then
    record PASS "a second leader pod is refused by Multi-Attach (env-gated)" \
      "attach-enforcing drivers: ${ATTACH_ENFORCERS}; ${MULTI_ATTACH_EVENT}"
  else
    record FAIL "a second leader pod is refused by Multi-Attach (env-gated)" \
      "attach-enforcing drivers exist (${ATTACH_ENFORCERS}) but no leader pod showed a FailedAttachVolume/Multi-Attach event in 120s"
  fi
fi

results
