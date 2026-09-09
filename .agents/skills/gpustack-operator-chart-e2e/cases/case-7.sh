#!/usr/bin/env bash
#
# CASE 7 — A live older install rolls forward onto this build   (MUTATING; run with NO chart release)
#
#   case-7.sh <NS> <TAG> [OLD_TAG]
#
# Goal:        Prove the upgrade every existing image-mode cluster has to make. The default OLD_TAG
#              predates the subchart layout, so its worker installs Kueue, NFD and the two CSI
#              drivers as four releases of their own, alongside a fifth that carries only the
#              device-managers. Rolling that Deployment onto this build must converge on the ONE
#              release that now owns all of it — adopting the objects rather than re-creating them,
#              with no human uninstalling anything first — and must leave the device-managers
#              pointing at an image tag that actually exists.
# Environment: A reachable cluster where the operator chart is NOT installed — the case AUTO-SKIPS if
#              it is, for the reason CASE 6 does: both renders claim the same cluster-scoped objects.
#              Needs the TAG image loaded on the nodes (build-load.sh), and outbound network for the
#              published OLD_TAG image and for the applications' own images on both sides of the
#              upgrade. No GPU.
# Inputs:      The minimal worker CASE 6 uses — ServiceAccount + cluster-admin binding + Service +
#              Deployment — first at the published OLD_TAG image, then rolled onto TAG with
#              `kubectl set image`. cluster-admin is an E2E shortcut, not the chart's fine-grained
#              role. The readiness probe and maxUnavailable: 0 are the chart's own posture, and they
#              are what holds the old replica live for the whole of the new one's install: that
#              overlap of two ReplicaSets is the state the upgrade has to survive. Nothing is
#              mocked — each side installs the real chart bundled in its own image.
# Expected:    - the old build converges first, leaving the five releases it is known to install;
#              - an old and a new replica are live at the same time during the rollout;
#              - after it: exactly one release row for gpustack-operator-device-manager, "deployed",
#                at a revision above 1 — upgraded in place, never uninstalled and re-created, which
#                on a release owning Kueue would strand finalizer-pinned CRDs;
#              - Kueue, NFD, the CSI drivers and the device-manager DaemonSet all name THAT release
#                in Helm's ownership annotation: adopted, not left behind;
#              - the four per-application release records are retired;
#              - the device-manager runs the new worker's image, and nothing in the namespace is
#                stuck pulling an image — the tag the worker composes has to resolve;
#              - the surviving replica is Running and has never restarted.
# Cleanup:     A trap deletes the hand-rolled worker and its cluster-admin binding, then runs the
#              shared teardown (which uninstalls whichever releases survive, plus their CRDs,
#              finalizers, APIServices and webhooks). Idempotent and safe to re-run.
set -uo pipefail

NS="${1:?usage: case-7.sh <NS> <TAG> [OLD_TAG]}"
TAG="${2:?usage: case-7.sh <NS> <TAG> [OLD_TAG]}"
OLD_TAG="${3:-v0.5.4}"
RELEASE=gpustack-operator-device-manager
CHART_RELEASE=gpustack-operator
WORKER=gpustack-e2e-upgrade-worker
MANUFACTURER=nvidia
IMAGE="gpustack/gpustack-operator:${TAG}"
OLD_IMAGE="gpustack/gpustack-operator:${OLD_TAG}"
LEGACY_RELEASES="gpustack-kueue gpustack-node-feature-discovery gpustack-csi-driver-nfs gpustack-csi-driver-s3"
LIB="$(cd "$(dirname "$0")/../../_e2e-lib/scripts" && pwd)"
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

report() {
  echo
  echo "== CASE 7 — A live older install rolls forward onto this build (${OLD_TAG} -> ${TAG}) =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

# Everything a failure has to be diagnosed from, printed while it still exists: the trap tears
# the namespace down on the way out, and the install runs during the worker's startup, so its
# failure is only ever in a log belonging to a pod that is about to be deleted.
dump_state() {
  echo
  echo "-- releases and migration hooks (the trap removes them next) --"
  "$HELM" list -n "$NS" -a 2>&1
  kubectl -n "$NS" get jobs,pods 2>&1 | tail -30
  echo
  echo "-- worker restarts --"
  kubectl -n "$NS" get pods -l "app=${WORKER}" -o jsonpath='{range .items[*]}{.metadata.name}{" restarts="}{.status.containerStatuses[*].restartCount}{" last="}{.status.containerStatuses[*].lastState.terminated.reason}{"/exit "}{.status.containerStatuses[*].lastState.terminated.exitCode}{"\n"}{end}' 2>&1
  echo
  echo "-- worker logs --"
  for p in $(kubectl -n "$NS" get pods -l "app=${WORKER}" -o name 2>/dev/null); do
    echo "---- ${p} ----"
    kubectl -n "$NS" logs "$p" --tail=40 2>&1 | tail -40
    # A restart is only ever explained by the log of the container that died.
    prev=$(kubectl -n "$NS" logs "$p" --previous --tail=60 2>/dev/null)
    if [ -n "$prev" ]; then
      echo "---- ${p} (previous container) ----"
      printf '%s\n' "$prev"
    fi
  done
}

# The two install modes cannot share a cluster: see the Environment note above.
if "$HELM" status "$CHART_RELEASE" -n "$NS" >/dev/null 2>&1; then
  echo "CASE 7 SKIP — chart release ${CHART_RELEASE} is installed; image mode needs a cluster without it"
  echo "            (run this case before the chart install, or after CASE 2's teardown)"
  exit 0
fi

cleanup() {
  echo "[case-7] removing the hand-rolled worker"
  kubectl -n "$NS" delete deploy,svc,sa "$WORKER" --ignore-not-found >/dev/null 2>&1 || true
  kubectl delete clusterrolebinding "$WORKER" --ignore-not-found >/dev/null 2>&1 || true
  bash "$LIB/teardown.sh" "$NS"
}
trap cleanup EXIT

kubectl create namespace "$NS" >/dev/null 2>&1 || true

echo "== deploying a worker with no chart behind it, at the OLD build (${OLD_IMAGE}) =="
# FOUR DOCUMENTS, AND ALL FOUR ARE READ BACK. `kubectl apply` reports them in ONE stream, so a run
# where the ServiceAccount or the binding was refused and the Deployment was not still reaches the
# rollout check below — which then spends 1200s and names the old image as the suspect, a diagnosis
# about something that was never wrong.
#
# The gate is the objects' EXISTENCE, not the words the apply printed. Counting ` created` lines
# instead makes the gate depend on how they came to be there: this case's namespace is created with
# `|| true` a few lines above precisely so a crashed run can be re-run, and over a surviving
# namespace apply reports `configured`/`unchanged`. The retrying kubectl shim does the same when an
# apply lands but its response is lost. The output is still captured, because a refusal's text is the
# only diagnosis of why an object is missing.
worker_out="$(kubectl apply -f - 2>&1 <<EOF
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
  # One replica, as a single-worker deployment in the field has. The overlap this case is about
  # comes from the rollout, not from the count: maxUnavailable 0 keeps the old replica serving
  # until the new one reports ready, and the new one cannot report ready until its install
  # returns. So the whole upgrade runs with both builds live, which is the state the field
  # incident was in.
  replicas: 1
  strategy:
    rollingUpdate:
      maxSurge: 1
      maxUnavailable: 0
  # The install is part of startup and waits for the applications to become Ready, which is far
  # longer than the 10-minute default allows. Exceeding it would only mark the Deployment failed,
  # but it would do so while the upgrade is still legitimately working.
  progressDeadlineSeconds: 1800
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
          image: ${OLD_IMAGE}
          imagePullPolicy: IfNotPresent
          args:
            - gpustack-operator
            - worker
            - -v=2
            - --secure-port=31443
            - --manufacturer=${MANUFACTURER}
          # The chart's own readiness probe. It is what makes "ready" mean "finished installing",
          # which both paces the rollout and lets this case wait on the rollout instead of on a
          # release poll. There is deliberately no startup or liveness probe: a readiness failure
          # never restarts the container, so a slow install cannot be mistaken for a crash loop.
          readinessProbe:
            failureThreshold: 3
            timeoutSeconds: 5
            periodSeconds: 5
            httpGet:
              scheme: HTTPS
              port: 31443
              path: /readyz
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
)"
# Split with parameter expansion rather than `set --`, which would clobber the script's own
# positional parameters. Nothing reads them after this point today, so the difference is a footgun
# rather than a bug -- but it is one a later wrapper passing arguments would step on silently.
worker_missing=""
for spec in "serviceaccount ${NS}" "service ${NS}" "deployment ${NS}" "clusterrolebinding -"; do
  spec_kind="${spec%% *}"
  spec_ns="${spec##* }"
  if [ "$spec_ns" = "-" ]; then
    kubectl get "$spec_kind" "$WORKER" -o name >/dev/null 2>&1 \
      || worker_missing="${worker_missing} ${spec_kind}/${WORKER}"
  else
    kubectl -n "$spec_ns" get "$spec_kind" "$WORKER" -o name >/dev/null 2>&1 \
      || worker_missing="${worker_missing} ${spec_ns}/${spec_kind}/${WORKER}"
  fi
done
if [ -n "$worker_missing" ]; then
  record FAIL "the hand-rolled worker exists" \
    "absent after the apply:${worker_missing} — so the rollout below has no subject. The apply said: \
$(printf '%s' "${worker_out:-<no output at all>}" | tr '\n' ' ' | cut -c1-220)"
  report
  exit 1
fi

# The old worker is ready only once its own install returned, so this waits out the whole of it.
if ! kubectl -n "$NS" rollout status "deploy/${WORKER}" --timeout=1200s >/dev/null 2>&1; then
  record FAIL "old build converged" "deploy/${WORKER} at ${OLD_IMAGE} never became Available"
  dump_state
  report
  exit 1
fi
record PASS "old build converged" "deploy/${WORKER} at ${OLD_IMAGE} Available"

# The layout the old build leaves behind is the premise of everything below: one release per
# application, plus a device-manager-only one. Assert it rather than assume it — a different
# starting layout would make every later check mean something else.
before=$("$HELM" list -n "$NS" -aq 2>/dev/null | sort | tr '\n' ' ')
missing=""
for r in $LEGACY_RELEASES $RELEASE; do
  case " $before " in
    *" $r "*) ;;
    *) missing="${missing}${r} " ;;
  esac
done
if [ -z "$missing" ]; then
  record PASS "old layout is per-application" "${before% }"
else
  record FAIL "old layout is per-application" "missing ${missing}— found [${before% }]"
  dump_state
  report
  exit 1
fi

echo "== rolling the worker onto this build (${IMAGE}) =="
kubectl -n "$NS" set image "deploy/${WORKER}" "main=${IMAGE}" >/dev/null

# Both builds live at once is the state under test, so observe it rather than infer it from the
# strategy. maxUnavailable 0 plus a readiness probe that only passes after the install makes the
# window minutes wide; not seeing it means the new replica never got as far as installing.
overlapped=""
for _ in $(seq 1 90); do
  imgs=$(kubectl -n "$NS" get pods -l "app=${WORKER}" \
           --field-selector=status.phase=Running \
           -o jsonpath='{range .items[*]}{.spec.containers[0].image}{"\n"}{end}' 2>/dev/null | sort -u)
  if [ "$(printf '%s\n' "$imgs" | grep -c .)" -ge 2 ]; then
    overlapped=$(printf '%s' "$imgs" | tr '\n' ' ')
    break
  fi
  sleep 2
done
if [ -n "$overlapped" ]; then
  record PASS "both builds live during the rollout" "${overlapped% }"
else
  record FAIL "both builds live during the rollout" "never saw two images Running within 180s"
fi

if ! kubectl -n "$NS" rollout status "deploy/${WORKER}" --timeout=1800s >/dev/null 2>&1; then
  record FAIL "new build converged" "deploy/${WORKER} at ${IMAGE} never became Available"
  dump_state
  report
  exit 1
fi
record PASS "new build converged" "deploy/${WORKER} at ${IMAGE} Available"

# One release row, deployed, past revision 1. Revision 1 would mean the release was uninstalled
# and re-created rather than upgraded — the path that strands Kueue's finalizer-pinned CRDs.
rows=$("$HELM" list -n "$NS" -a -f "^${RELEASE}\$" -o json 2>/dev/null \
         | python3 -c 'import json,sys; d=json.load(sys.stdin); print(len(d), d[0]["revision"] if d else "-", d[0]["status"] if d else "-")' 2>/dev/null)
read -r nrel rev rst <<<"$rows"
if [ "${nrel:-}" = 1 ] && [ "${rst:-}" = deployed ] && [ "${rev:-0}" -gt 1 ]; then
  record PASS "upgraded in place" "${RELEASE} deployed at revision ${rev}"
else
  record FAIL "upgraded in place" "${rows:-unreadable} — wanted 1 release, deployed, revision > 1"
fi

# The per-application records are retired by the post-upgrade hook, so `helm list` stops offering
# an uninstall that would now delete objects the parent release owns.
survivors=""
after=$("$HELM" list -n "$NS" -aq 2>/dev/null | tr '\n' ' ')
for r in $LEGACY_RELEASES; do
  case " $after " in
    *" $r "*) survivors="${survivors}${r} " ;;
  esac
done
if [ -z "$survivors" ]; then
  record PASS "per-application records retired" "only [${after% }] remains"
else
  record FAIL "per-application records retired" "${survivors% } still recorded"
fi

# Adoption, object by object: what the old releases created is now owned by the new one, still
# in place. Helm's own ownership annotation, not app.kubernetes.io/instance — Helm stamps the
# annotation on everything it manages, while the label is up to each chart.
for obj in \
  deploy/kueue-controller-manager \
  deploy/node-feature-discovery-master \
  daemonset/node-feature-discovery-worker \
  deploy/csi-nfs-controller \
  deploy/csi-s3-controller \
  "daemonset/gpustack-operator-device-manager-${MANUFACTURER}"; do
  if ! kubectl -n "$NS" get "$obj" >/dev/null 2>&1; then
    record FAIL "adopted by the new release" "$obj missing"
    continue
  fi
  owner=$(kubectl -n "$NS" get "$obj" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null)
  if [ "$owner" = "$RELEASE" ]; then
    record PASS "adopted by the new release" "$obj"
  else
    record FAIL "adopted by the new release" "$obj owner=[${owner:-none}], wanted ${RELEASE}"
  fi
done

# The tag the worker composed for the device-managers has to be one that exists. A tag that does
# not is the defect this branch fixes, and its only symptom is here.
dm_img=$(kubectl -n "$NS" get "daemonset/gpustack-operator-device-manager-${MANUFACTURER}" \
           -o jsonpath='{.spec.template.spec.containers[0].image}' 2>/dev/null)
if [ "$dm_img" = "$IMAGE" ]; then
  record PASS "device-manager image == worker image" "$dm_img"
else
  record FAIL "device-manager image == worker image" "device-manager [${dm_img:-none}] != worker [${IMAGE}]"
fi

unpullable=$(kubectl -n "$NS" get pods -o json 2>/dev/null | python3 -c '
import json, sys
bad = []
for p in json.load(sys.stdin).get("items", []):
    st = p.get("status", {})
    for key in ("containerStatuses", "initContainerStatuses"):
        for c in st.get(key) or []:
            reason = (c.get("state", {}).get("waiting") or {}).get("reason", "")
            if reason in ("ImagePullBackOff", "ErrImagePull", "InvalidImageName"):
                bad.append("%s/%s(%s)" % (p["metadata"]["name"], c.get("name"), reason))
print(" ".join(bad))
' 2>/dev/null)
if [ -z "$unpullable" ]; then
  record PASS "every image resolves" "nothing waiting on a pull"
else
  record FAIL "every image resolves" "$unpullable"
fi

# The replica that survives the rollout is the new build, running, and has never restarted: a
# worker whose Prepare fails restarts, and restarting is how the applications end up never
# installed at all.
#
# Only Running pods are judged. The old replica's container exits 0 when the rollout terminates
# it, and its Pod lingers Completed for a few seconds afterwards — that is the rollout working,
# not an old worker still up.
pods=$(kubectl -n "$NS" get pods -l "app=${WORKER}" \
         -o jsonpath='{range .items[*]}{.metadata.name}{" "}{.status.phase}{" "}{.spec.containers[0].image}{" "}{.status.containerStatuses[*].restartCount}{"\n"}{end}' 2>/dev/null)
live=$(printf '%s\n' "$pods" | grep -cF " Running ${IMAGE} " || true)
stale=$(printf '%s\n' "$pods" | awk -v old="$OLD_IMAGE" '$2 == "Running" && $3 == old {printf "%s ", $1}')
restarted=$(printf '%s\n' "$pods" | awk '$2 == "Running" && $4 > 0 {printf "%s(%s restarts) ", $1, $4}')
if [ "$live" -eq 1 ] && [ -z "$stale" ] && [ -z "$restarted" ]; then
  record PASS "the new replica is the only one" "1 Running on ${IMAGE}, no restarts"
else
  record FAIL "the new replica is the only one" \
    "${live} Running on the new image; ${stale:+old still up: ${stale}}${restarted:-no restarts}"
fi

report

if [ "$FAILS" -ne 0 ]; then
  dump_state
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  kubectl -n ${NS} logs deploy/${WORKER} --tail=200   # the install runs during startup"
  echo "  kubectl -n ${NS} get jobs -l app.kubernetes.io/name=gpustack-operator   # the migration hooks"
  echo "  ${HELM} list -n ${NS} -a"
  exit 1
fi
echo "CASE 7 PASS"
