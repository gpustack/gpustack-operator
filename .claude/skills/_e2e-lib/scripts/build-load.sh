#!/usr/bin/env bash
#
# Build the operator dev image locally and load it into the cluster runtime.
# MUTATING (builds an image, imports into the node runtime) — the skill confirms
# before running this.
#
#   build-load.sh <TAG>
#
# Why a per-commit TAG: the operator binary is recompiled whenever the commit
# changes (the Dockerfile's GPUSTACK_GIT_COMMIT build-arg busts that layer), so
# the registry build cache stays warm. A fixed ":dev" tag + imagePullPolicy:
# IfNotPresent lets the kubelet keep a stale cached ":dev" (it matches by name,
# not digest); a per-commit tag forces the new image. Never pushes
# (PACKAGE_PUSH stays false).
set -uo pipefail

TAG="${1:?usage: build-load.sh <TAG>   (e.g. dev-$(git rev-parse --short HEAD))}"
IMAGE="gpustack/gpustack-operator:${TAG}"

echo "== build ${IMAGE} (local only, no push) =="
PACKAGE_TAG="$TAG" make package

echo
echo "== load into cluster runtime =="
ctx=$(kubectl config current-context)
echo "active context: ${ctx}"

# docker-desktop shares the docker image store with the node — no import needed.
# k3s (containerd, separate store) needs an explicit import.
case "$ctx" in
  *k3s*|k3d*)
    echo "k3s detected — importing via 'k3s ctr images import'"
    docker save "$IMAGE" | sudo k3s ctr images import -
    ;;
  *docker-desktop*|docker-for-desktop*)
    echo "docker-desktop detected — node shares the docker store, no import needed"
    ;;
  *)
    echo "unknown runtime for context '${ctx}'."
    echo "If pods later report ErrImagePull/ImagePullBackOff, the node does not share"
    echo "the docker store — import manually, e.g.:"
    echo "  docker save '${IMAGE}' | sudo k3s ctr images import -"
    ;;
esac

echo
echo "built & loaded: ${IMAGE}"
