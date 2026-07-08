#!/usr/bin/env bash
#
# Install the operator via its Helm chart, pinned to the locally-built dev image.
# MUTATING — the skill confirms before running this.
#
#   deploy.sh <NS> <TAG> [extra helm --set args...]
#
# image.tag defaults to v<Chart.AppVersion>; pinning it to the per-build TAG
# (plus IfNotPresent) is what makes the kubelet run the locally-loaded image
# instead of pulling from a registry. The chart deploys the worker + the
# per-manufacturer device-manager DaemonSets and passes
# --disable-applications=device-manager to the worker; the worker self-installs
# Kueue / NFD / CSI at runtime.
#
# Pass extra --set flags to vary the install, e.g.:
#   deploy.sh gpustack-system dev-abc123 --set deviceManager.enabled=false
#   deploy.sh gpustack-system dev-abc123 --set cleanupOnUninstall=true
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

echo "== helm install gpustack-operator into ${NS} (image tag ${TAG}) =="
helm install gpustack-operator "$CHART" \
  -n "$NS" --create-namespace \
  --set image.tag="$TAG" \
  --set image.pullPolicy=IfNotPresent \
  "$@"
