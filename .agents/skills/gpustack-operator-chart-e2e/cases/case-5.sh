#!/usr/bin/env bash
#
# CASE 5 — Adopting another release's objects needs --take-ownership   (MUTATING)
#
#   case-5.sh <NS>
#
# Goal:        The one-time upgrade that folds a separately-installed application into the operator
#              release behaves both ways round. WITHOUT --take-ownership it must FAIL, and fail on
#              ownership metadata rather than half-adopting anything — that failure is the guard: it
#              is what stops the flag being quietly dropped from the migration and the objects being
#              taken over by accident. WITH the flag the same upgrade succeeds, the objects change
#              hands in place, and the upgrade's own hook retires the release record they came from.
# Environment: A reachable cluster with the chart installed under the release name below, and a helm
#              client that HAS --take-ownership (3.21+) — the case AUTO-SKIPS on an older client,
#              since the mechanism cannot be exercised at all without it. No GPU. Needs the chart
#              source tree. Leaves the release upgraded twice; the trap puts the values back.
# Inputs:      csi-driver-nfs stands in for all four applications — the smallest of them, and
#              ownership metadata is generic, so what holds for its objects holds for Kueue's. It is
#              installed from the SAME vendored subchart the parent renders, as a release named the
#              way earlier versions named it, which is what makes the object names collide. Nothing
#              mocked: real Helm releases, real ownership metadata.
# Expected:    - the stand-in release owns csi-nfs-controller (release-name annotation);
#              - re-enabling the subchart WITHOUT --take-ownership fails, and the error says
#                "invalid ownership metadata";
#              - the same upgrade WITH --take-ownership succeeds;
#              - afterwards the objects carry instance=<release> and its release-name annotation;
#              - the stand-in release record is gone, retired by the post-upgrade hook rather than
#                by a `helm uninstall` (which would have deleted the objects just adopted).
# Cleanup:     A trap re-upgrades to the values captured before the case ran, WITH --take-ownership
#              so it converges from a half-adopted state too, then deletes any stand-in release
#              record still present. It never runs `helm uninstall` on the stand-in — that would
#              take the adopted objects with it. Idempotent.
set -uo pipefail

NS="${1:?usage: case-5.sh <NS>}"
RELEASE=gpustack-operator
LEGACY=gpustack-csi-driver-nfs
CHART=deploy/gpustack-operator/chart
SUBCHART="${CHART}/charts/csi-driver-nfs"
# The driver name the parent gives the subchart; passing it to the stand-in keeps even the
# cluster-scoped CSIDriver object name identical, so nothing is left behind under another name.
DRIVER=nfs.csi.gpustack.ai
PROBE=deploy/csi-nfs-controller
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

FAILS=0
ROWS=()
record() { ROWS+=("$1|$2|$3"); [ "$1" = FAIL ] && FAILS=$((FAILS + 1)); return 0; }

report() {
  echo
  echo "== CASE 5 — Adopting another release's objects needs --take-ownership =="
  {
    echo "STATUS|CHECK|OBJECT"
    printf '%s\n' "${ROWS[@]}"
  } | column -t -s '|'
}

owner_of() { # release-name annotation of the probe object
  kubectl -n "$NS" get "$PROBE" -o jsonpath='{.metadata.annotations.meta\.helm\.sh/release-name}' 2>/dev/null
}

if ! "$HELM" status "$RELEASE" -n "$NS" >/dev/null 2>&1; then
  echo "CASE 5 SKIP — release ${RELEASE} not installed in ${NS}"
  exit 0
fi
if ! "$HELM" upgrade --help 2>/dev/null | grep -q -- '--take-ownership'; then
  echo "CASE 5 SKIP — ${HELM} ($("$HELM" version --short 2>/dev/null)) has no --take-ownership; needs 3.21+"
  exit 0
fi
if ! [ -d "$SUBCHART" ]; then
  echo "CASE 5 SKIP — vendored subchart ${SUBCHART} missing; run make deps"
  exit 0
fi

BEFORE=$(mktemp "${TMPDIR:-/tmp}/gpustack-e2e-adopt-before.XXXXXX")
"$HELM" get values "$RELEASE" -n "$NS" -o yaml > "$BEFORE" 2>/dev/null
[ -s "$BEFORE" ] || echo '{}' > "$BEFORE"
grep -qx 'null' "$BEFORE" && echo '{}' > "$BEFORE"

restore() {
  echo "[case-5] restoring the captured release values (with --take-ownership, so a half-adopted state converges)"
  "$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" -f "$BEFORE" --take-ownership --timeout 10m >/dev/null 2>&1 \
    || echo "[case-5] restore upgrade failed — inspect: ${HELM} status ${RELEASE} -n ${NS}"
  # Retire the stand-in release RECORD only. `helm uninstall` here would delete the very objects
  # the operator release has adopted — the same trap the migration guide warns about.
  kubectl -n "$NS" delete secret -l "owner=helm,name=${LEGACY}" --ignore-not-found >/dev/null 2>&1 || true
  rm -f "$BEFORE"
}
trap restore EXIT

# 1. Hand csi-driver-nfs out of the operator release, so the stand-in can install it instead.
echo "== helm upgrade ${RELEASE}: csi-driver-nfs.enabled=false =="
if ! "$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" --reuse-values \
  --set 'csi-driver-nfs.enabled=false' --timeout 10m; then
  record FAIL "release drops the subchart" "helm upgrade failed"
  report
  exit 1
fi
gone=""
for _ in $(seq 1 20); do
  kubectl -n "$NS" get "$PROBE" >/dev/null 2>&1 || { gone=1; break; }
  sleep 3
done
if [ -n "$gone" ]; then
  record PASS "release drops the subchart" "$PROBE removed"
else
  record FAIL "release drops the subchart" "$PROBE still present after 60s"
  report
  exit 1
fi

# 2. Install it the way earlier versions did: its own release, same chart, same object names.
echo "== helm install ${LEGACY} (the stand-in for a pre-subchart install) =="
if ! "$HELM" install "$LEGACY" "$SUBCHART" -n "$NS" --set "driver.name=${DRIVER}" --timeout 10m; then
  record FAIL "stand-in release installed" "helm install ${LEGACY} failed"
  report
  exit 1
fi
if [ "$(owner_of)" = "$LEGACY" ]; then
  record PASS "stand-in release owns the objects" "$PROBE -> ${LEGACY}"
else
  record FAIL "stand-in release owns the objects" "$PROBE -> [$(owner_of)], wanted ${LEGACY}"
  report
  exit 1
fi

# 3. The negative half: re-enabling the subchart must be REFUSED without the flag. A pass here is a
#    failed upgrade — anything else means Helm silently took objects it does not own.
echo "== helm upgrade ${RELEASE}: re-enable csi-driver-nfs WITHOUT --take-ownership (must fail) =="
err=$("$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" --reuse-values \
  --set 'csi-driver-nfs.enabled=true' --timeout 10m 2>&1)
rc=$?
if [ "$rc" -eq 0 ]; then
  record FAIL "upgrade refused without the flag" "helm upgrade SUCCEEDED — it adopted objects it does not own"
elif echo "$err" | grep -q 'invalid ownership metadata'; then
  record PASS "upgrade refused without the flag" "invalid ownership metadata"
else
  record FAIL "upgrade refused without the flag" \
    "failed for another reason: $(echo "$err" | tr '\n' ' ' | cut -c1-120)"
fi
# The objects must be untouched by the refusal — a partial adoption is the worst outcome of all.
if [ "$(owner_of)" = "$LEGACY" ]; then
  record PASS "refusal changed nothing" "$PROBE still -> ${LEGACY}"
else
  record FAIL "refusal changed nothing" "$PROBE -> [$(owner_of)] after a refused upgrade"
fi

# 4. The positive half: the documented one-time upgrade.
echo "== helm upgrade ${RELEASE}: re-enable csi-driver-nfs WITH --take-ownership =="
if "$HELM" upgrade "$RELEASE" "$CHART" -n "$NS" --reuse-values \
  --set 'csi-driver-nfs.enabled=true' --take-ownership --timeout 10m; then
  record PASS "upgrade accepted with the flag" "--take-ownership"
else
  record FAIL "upgrade accepted with the flag" "helm upgrade failed even with --take-ownership"
  report
  exit 1
fi

if [ "$(owner_of)" = "$RELEASE" ]; then
  record PASS "objects changed hands" "$PROBE -> ${RELEASE}"
else
  record FAIL "objects changed hands" "$PROBE -> [$(owner_of)], wanted ${RELEASE}"
fi
inst=$(kubectl -n "$NS" get "$PROBE" -o jsonpath='{.metadata.labels.app\.kubernetes\.io/instance}' 2>/dev/null)
if [ "$inst" = "$RELEASE" ]; then
  record PASS "instance label rewritten" "$PROBE instance=${inst}"
else
  record FAIL "instance label rewritten" "$PROBE instance=[${inst:-none}], wanted ${RELEASE}"
fi

# 5. The post-upgrade hook retires the record the objects came from, so `helm list` stops offering a
#    `helm uninstall <legacy>` that would delete them.
if [ -z "$("$HELM" list -n "$NS" -q 2>/dev/null | grep -Fx "$LEGACY")" ]; then
  record PASS "stand-in release retired" "${LEGACY} gone from helm list"
else
  record FAIL "stand-in release retired" "${LEGACY} still listed — did the post-upgrade hook run?"
fi
# And the workload is running under its new owner.
if kubectl -n "$NS" rollout status "$PROBE" --timeout=300s >/dev/null 2>&1; then
  record PASS "adopted workload healthy" "$PROBE rolled out"
else
  record FAIL "adopted workload healthy" "$PROBE did not roll out after the adoption"
fi

report

if [ "$FAILS" -ne 0 ]; then
  echo
  echo "FAILED ${FAILS} check(s). Diagnose:"
  echo "  ${HELM} history ${RELEASE} -n ${NS}"
  echo "  kubectl -n ${NS} get ${PROBE} -o jsonpath='{.metadata.annotations}'"
  echo "  kubectl -n ${NS} logs job/gpustack-operator-migrate-post   # the record-retiring hook"
  exit 1
fi
echo "CASE 5 PASS"
