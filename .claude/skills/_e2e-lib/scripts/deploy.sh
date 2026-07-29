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

NS="${1:?usage: deploy.sh <NS> <TAG> [extra --set args...]}"
TAG="${2:?usage: deploy.sh <NS> <TAG> [extra --set args...]}"
shift 2

CHART="deploy/gpustack-operator/chart"
# Prefer the client hack/lib/helm.sh pins (3.21+). A PATH helm can be old enough to lack
# flags this suite needs — a 3.13 client has no --take-ownership at all.
HELM=helm
[ -x .sbin/helm ] && HELM=.sbin/helm

echo "== helm install gpustack-operator into ${NS} (image tag ${TAG}) =="
"$HELM" install gpustack-operator "$CHART" \
  -n "$NS" --create-namespace \
  --set image.tag="$TAG" \
  --set image.pullPolicy=IfNotPresent \
  "$@"
