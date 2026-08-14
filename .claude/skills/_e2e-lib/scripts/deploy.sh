#!/usr/bin/env bash
#
# Install the operator via its Helm chart, pinned to the locally-built dev image.
# MUTATING — the skill confirms before running this.
#
#   deploy.sh <NS> <TAG> [extra helm --set args...]
#
# image.tag defaults to v<Chart.AppVersion>; pinning it to the per-build TAG
# (plus IfNotPresent) is what makes the kubelet run the locally-loaded image
# instead of pulling from a registry. The chart deploys everything itself — the
# worker, the per-manufacturer device-manager DaemonSets, and Kueue / Node
# Feature Discovery / the two CSI drivers as subcharts of this one release — and
# passes --disable-applications=* to the worker, so the worker installs nothing
# at runtime. Leave that wildcard alone: a partial list makes the worker install
# a second release of this same chart, whose objects this release already owns —
# Helm refuses them and the worker's startup fails with the install.
#
# Pass extra --set flags to vary the install, e.g.:
#   deploy.sh gpustack-system dev-abc123 --set deviceManager.enabled=false
#   deploy.sh gpustack-system dev-abc123 --set cleanupOnUninstall=true
#   deploy.sh gpustack-system dev-abc123 --set csi-driver-s3.enabled=false
#
# deviceManager.enabled=false now only means "render no device-manager
# DaemonSets"; it no longer hands that install to the worker. The worker's own
# install of the bundled chart happens only where no chart deploys the worker —
# gpustack-operator-chart-e2e CASE 6 stands that topology up on purpose.
#
# To deploy an image that was packaged and PUSHED to a registry (not locally loaded) — e.g.
# `make package PACKAGE_NAMESPACE=<ns> PACKAGE_PUSH=true`, which emits <ns>/gpustack-operator:<tag> —
# point the chart at it and force a pull (the trailing --set overrides the IfNotPresent below):
#   deploy.sh gpustack-system <tag> --set image.repository=<ns>/gpustack-operator --set image.pullPolicy=Always
# See gpustack-operator-e2e/references/packaged-image-deploy.md for the package<->chart image contract.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

NS="${1:?usage: deploy.sh <NS> <TAG> [extra --set args...]}"
TAG="${2:?usage: deploy.sh <NS> <TAG> [extra --set args...]}"
shift 2

CHART="deploy/gpustack-operator/chart"
# Prefer the client hack/lib/helm.sh pins (3.21+). A PATH helm can be old enough to lack
# flags this suite needs — a 3.13 client has no --take-ownership at all.
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

# An install that does not complete is the expensive failure here, not one that fails outright:
# helm creates the subcharts' cluster-scoped RBAC early, and if the run is interrupted — a jittery
# API endpoint, a cancelled command, a control-plane restart — `helm uninstall` has no release
# record naming those objects, so the NEXT install refuses to adopt them and the harness is wedged
# until somebody removes them by hand. So each attempt cleans up after itself before the next one.
#
# Only the whole install is retried, never a partial resume: helm has no resumable install, and
# `--atomic` would roll back on the first jitter and cost the same time again.
ATTEMPTS="${E2E_DEPLOY_ATTEMPTS:-3}"
HERE="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"

for attempt in $(seq 1 "$ATTEMPTS"); do
  echo "== helm install gpustack-operator into ${NS} (image tag ${TAG})${attempt:+  [attempt ${attempt}/${ATTEMPTS}]} =="
  # The status is captured from the install itself, never from an `if` around it: a compound
  # command whose condition fails and which has no `else` branch exits 0, so `rc=$?` after
  # `if helm install; then exit 0; fi` reads 0 on every failure — and the last attempt would
  # then exit 0 too, reporting a wholly failed install as a successful one.
  "$HELM" install gpustack-operator "$CHART" \
    -n "$NS" --create-namespace \
    --set image.tag="$TAG" \
    --set image.pullPolicy=IfNotPresent \
    "$@"
  rc=$?
  [ "$rc" -eq 0 ] && exit 0

  if [ "$attempt" -ge "$ATTEMPTS" ]; then
    echo "install failed after ${ATTEMPTS} attempt(s)" >&2
    exit "$rc"
  fi

  # Clear whatever the failed attempt left: the release record if one exists, and the orphaned
  # cluster-scoped objects that block re-adoption. teardown.sh is the same cleanup the suite ends
  # with, so this needs no second implementation to drift from it.
  echo "-- attempt ${attempt} failed; clearing its residue before retrying" >&2
  bash "${HERE}/teardown.sh" "$NS" >/dev/null 2>&1 || true
  sleep "${E2E_DEPLOY_BACKOFF:-20}"
done
