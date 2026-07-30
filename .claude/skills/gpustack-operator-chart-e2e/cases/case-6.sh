#!/usr/bin/env bash
#
# CASE 6 — Image mode: the worker installs the bundled chart itself   (MUTATING; run with NO chart release)
#
#   case-6.sh <NS> <TAG>
#
# Goal:        Prove the second install mode. Where no chart deploys the worker, the worker installs
#              the chart packaged into its own image — as the release named below, with itself
#              switched off — and that release carries the applications AND the device-managers.
#              This is the only mode in which the bundled tgz is actually consumed, so it is the one
#              place the version the worker computes is proven to resolve to a file that exists.
# Environment: A reachable cluster where the operator chart is NOT installed — the case AUTO-SKIPS
#              if it is. The two modes are exclusive: both installs render the same chart under the
#              same fullnameOverride, so a worker installing its own release is refused the objects
#              a chart release already owns, and never starts.
#              Needs the TAG image loaded on the cluster's nodes (build-load.sh). No GPU.
# Inputs:      A deliberately minimal worker: ServiceAccount + cluster-admin binding + Service +
#              Deployment at THREE replicas, standing in for whatever deploys the worker when this
#              chart does not. cluster-admin is an E2E shortcut, not the chart's fine-grained role.
#              The worker gets NO --disable-applications, which is what puts it in charge of
#              installing — and the install runs before leader election, so all three drive Helm at
#              once. One manufacturer keeps the DaemonSet set small. Nothing else mocked — the chart
#              it installs is the real tgz inside the real image.
# Expected:    - a Helm release gpustack-operator-device-manager reaches "deployed";
#              - exactly one such release, at revision 1, with no replica having restarted:
#                three concurrent installers converge on one instead of racing;
#              - its chart version equals the running binary's version (the tgz resolved);
#              - Kueue, NFD, the CSI drivers and the device-manager DaemonSet all belong to THAT
#                release (Helm's ownership annotation), not to a chart release and not to four
#                releases of their own;
#              - the device-manager runs the same image as the worker, which is how a device-manager
#                can never drift from the operator that manages it;
#              - the worker's own Deployment is absent from the release (it is already running).
# Cleanup:     A trap deletes the hand-rolled worker and its cluster-admin binding, then runs the
#              shared teardown (which uninstalls that release, its CRDs, finalizers, APIServices and
#              webhooks). Idempotent and safe to re-run.
set -uo pipefail

NS="${1:?usage: case-6.sh <NS> <TAG>}"
TAG="${2:?usage: case-6.sh <NS> <TAG>}"
RELEASE=gpustack-operator-device-manager
CHART_RELEASE=gpustack-operator
WORKER=gpustack-e2e-image-mode-worker
MANUFACTURER=nvidia
IMAGE="gpustack/gpustack-operator:${TAG}"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

report() {
  echo
  echo "== CASE 6 — Image mode: the worker installs the bundled chart itself =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

# The two modes cannot share a cluster: see the Environment note above.
if "$HELM" status "$CHART_RELEASE" -n "$NS" >/dev/null 2>&1; then
  echo "CASE 6 SKIP — chart release ${CHART_RELEASE} is installed; image mode needs a cluster without it"
  echo "            (run this case before the chart install, or after CASE 2's teardown)"
  exit 0
fi

cleanup() {
  echo "[case-6] removing the hand-rolled worker"
  kubectl -n "$NS" delete deploy,svc,sa "$WORKER" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding "$WORKER" --ignore-not-found >/dev/null 2>&1 || true
  bash "$LIB/teardown.sh" "$NS" "${WORKER}-cert"
}
trap cleanup EXIT

kubectl create namespace "$NS" >/dev/null 2>&1 || true

echo "== deploying a worker with no chart behind it (image ${IMAGE}) =="
kubectl apply -f - <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: ${WORKER}
  namespace: ${NS}
---
# cluster-admin: an E2E shortcut. The chart grants the worker a fine-grained role; reproducing it
# here would only test the copy. The trap deletes this binding.
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: ${WORKER}
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: ${WORKER}
    namespace: ${NS}
---
# The worker registers its aggregated APIServices against this Service, so it exists for the same
# reason the chart renders one.
apiVersion: v1
kind: Service
metadata:
  name: ${WORKER}
  namespace: ${NS}
spec:
  selector:
    app: ${WORKER}
  ports:
    - name: https
      port: 31443
      targetPort: 31443
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: ${WORKER}
  namespace: ${NS}
spec:
  # Three at once, because the install runs in the worker's startup — before leader election
  # gates anything — so every replica drives Helm. One release at one revision, with no
  # replica crash-looping, is the whole assertion. One replica would prove none of it, and a
  # rolling update overlaps two even where the count is one.
  replicas: 3
  selector:
    matchLabels:
      app: ${WORKER}
  template:
    metadata:
      labels:
        app: ${WORKER}
    spec:
      serviceAccountName: ${WORKER}
      containers:
        # Named "main" because that is the container the worker reads its own image from when it
        # composes the device-manager's image.
        - name: main
          image: ${IMAGE}
          imagePullPolicy: IfNotPresent
          args:
            - gpustack-operator
            - worker
            - -v=2
            - --secure-port=31443
            - --manufacturer=${MANUFACTURER}
          env:
            - name: KUBERNETES_POD_IP
              valueFrom:
                fieldRef:
                  fieldPath: status.podIP
            - name: KUBERNETES_POD_NAME
              valueFrom:
                fieldRef:
                  fieldPath: metadata.name
            - name: KUBERNETES_POD_NAMESPACE
              valueFrom:
                fieldRef:
                  fieldPath: metadata.namespace
            - name: KUBERNETES_NODE_NAME
              valueFrom:
                fieldRef:
                  fieldPath: spec.nodeName
            - name: KUBERNETES_SERVICE_NAME
              value: ${WORKER}
EOF

if ! kubectl -n "$NS" rollout status "deploy/${WORKER}" --timeout=300s >/dev/null 2>&1; then
  record FAIL "worker rollout" "deploy/${WORKER} not Available — is ${IMAGE} loaded on the nodes?"
  report
  exit 1
fi
record PASS "worker rollout" "deploy/${WORKER} Available"

# The install happens during the worker's startup, before it serves anything, and it waits for the
# applications to become Ready, so poll the release generously.
deployed=""
for _ in $(seq 1 120); do
  if "$HELM" status "$RELEASE" -n "$NS" -o json 2>/dev/null | grep -q '"status":"deployed"'; then
    deployed=1
    break
  fi
  sleep 5
done
if [ -n "$deployed" ]; then
  record PASS "worker installed the bundled chart" "release ${RELEASE} deployed"
else
  st=$("$HELM" status "$RELEASE" -n "$NS" -o json 2>/dev/null | grep -o '"status":"[a-z]*"' | head -1)
  record FAIL "worker installed the bundled chart" "release ${RELEASE} ${st:-absent} after 600s"
  report
  # The install runs inside the worker's startup, so its failure is only in the worker's log — and
  # the trap below is about to delete the pod. Print it here or it is gone.
  echo
  echo "-- worker log (the install runs during startup; the trap deletes this pod next) --"
  kubectl -n "$NS" logs "deploy/${WORKER}" --tail=30 2>&1 | tail -30
  kubectl -n "$NS" logs "deploy/${WORKER}" --previous --tail=30 2>/dev/null | tail -30
  exit 1
fi

# The version the worker computed resolved to a tgz that exists: the release's chart version is that
# same number. This is the assertion CASE 1 can only make indirectly, by comparing file names.
ver=$(kubectl -n "$NS" exec "deploy/${WORKER}" -- gpustack-operator --version 2>/dev/null \
        | awk '{for (i=1;i<NF;i++) if ($i=="version") print $(i+1)}')
ver=${ver#v}
[[ "$ver" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]] || ver=0.0.0
chart=$("$HELM" list -n "$NS" -f "^${RELEASE}\$" -o json 2>/dev/null \
          | python3 -c 'import json,sys; d=json.load(sys.stdin); print(d[0]["chart"] if d else "")' 2>/dev/null)
chart=${chart#gpustack-operator-}
if [ -n "$chart" ] && [ "$ver" = "$chart" ]; then
  record PASS "installed chart version == binary" "$chart"
else
  record FAIL "installed chart version == binary" \
    "binary [$ver] != installed chart [${chart:-none}] — the bundled tgz name and the computed version disagree"
fi

# Everything the release brought belongs to the release, and the worker is not in it.
for entry in \
  "deploy/kueue-controller-manager|required" \
  "deploy/node-feature-discovery-master|required" \
  "daemonset/node-feature-discovery-worker|required" \
  "deploy/csi-nfs-controller|required" \
  "deploy/csi-s3-controller|required" \
  "daemonset/gpustack-operator-device-manager-${MANUFACTURER}|required"; do
  obj="${entry%|*}"
  if ! kubectl -n "$NS" get "$obj" >/dev/null 2>&1; then
    record FAIL "in the worker's own release" "$obj missing"
    continue
  fi
  # Helm's own ownership annotation, not app.kubernetes.io/instance: Helm stamps the annotation on
  # everything it manages, while the label is up to each chart (csi-driver-s3 omits it).
  owner=$(kubectl -n "$NS" get "$obj" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)
  if [ "$owner" = "$RELEASE" ]; then
    record PASS "in the worker's own release" "$obj"
  else
    record FAIL "in the worker's own release" "$obj owner=[${owner:-none}], wanted ${RELEASE}"
  fi
done

if kubectl -n "$NS" get deploy/gpustack-operator-worker >/dev/null 2>&1; then
  record FAIL "release deploys no worker" "deploy/gpustack-operator-worker exists — worker.enabled was not forced off"
else
  record PASS "release deploys no worker" "none, as the overlay forces"
fi

# A device-manager that runs a different build than the worker managing it is the drift this reuse
# exists to prevent.
dm_img=$(kubectl -n "$NS" get "daemonset/gpustack-operator-device-manager-${MANUFACTURER}" \
           -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
if [ "$dm_img" = "$IMAGE" ]; then
  record PASS "device-manager image == worker image" "$dm_img"
else
  record FAIL "device-manager image == worker image" "device-manager [${dm_img:-none}] != worker [${IMAGE}]"
fi

# And it stayed one release: no per-application releases came back alongside it.
legacy=$("$HELM" list -n "$NS" -q 2>/dev/null \
  | grep -E '^gpustack-(kueue|node-feature-discovery|csi-driver-nfs|csi-driver-s3)$' | tr '\n' ' ')
if [ -z "$legacy" ]; then
  record PASS "no per-application releases" "none"
else
  record FAIL "no per-application releases" "$legacy"
fi

# Three replicas installed concurrently and left ONE revision. A second revision means a replica
# upgraded what another had just installed; more than one release row means two of them each
# created their own.
rows=$("$HELM" list -n "$NS" -f "^${RELEASE}\$" -o json 2>/dev/null \
         | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d), d[0]["revision"] if d else "-")' 2>/dev/null)
case "$rows" in
  "1 1") record PASS "one release at one revision" "${RELEASE} revision 1" ;;
  "")    record FAIL "one release at one revision" "${RELEASE} unreadable" ;;
  *)     record FAIL "one release at one revision" "${rows% *} release(s), revision ${rows#* } — replicas raced" ;;
esac

# No replica crash-looped on the way there. A losing replica must converge on the winner's
# release, not fail its startup: a worker whose Prepare fails restarts, and two of them taking
# turns failing is what leaves the applications never installed.
pods=$(kubectl -n "$NS" get pods -l "app=${WORKER}" \
         -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{end}' 2>/dev/null)
running=$(printf '%s\n' "$pods" | grep -c ' Running ' || true)
restarted=$(printf '%s\n' "$pods" | awk '$3 > 0 {printf "%s(%s restarts) ", $1, $3}')
if [ "$running" -eq 3 ] && [ -z "$restarted" ]; then
  record PASS "every replica converged" "3 Running, no restarts"
else
  record FAIL "every replica converged" "${running} Running; ${restarted:-no restarts}"
fi

report

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} logs deploy/${WORKER} --tail=200   # the install runs during startup"
  echo "  ${HELM} list -n ${NS}"
  exit 1
fi
echo "CASE 6 PASS"
