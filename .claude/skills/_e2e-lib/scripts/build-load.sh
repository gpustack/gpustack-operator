#!/usr/bin/env bash
#
# Build the operator dev image and make it reachable by the cluster's nodes.
# MUTATING (builds an image; in remote mode also PUSHES it) — the skill confirms
# before running this.
#
#   build-load.sh <TAG>                                    # local mode
#   E2E_BUILDER_SSH=<user@host> build-load.sh <TAG>        # remote mode
#
# Why a per-commit TAG: the operator binary is recompiled whenever the commit
# changes (the Dockerfile's GPUSTACK_GIT_COMMIT build-arg busts that layer), so
# the registry build cache stays warm. A fixed ":dev" tag + imagePullPolicy:
# IfNotPresent lets the kubelet keep a stale cached ":dev" (it matches by name,
# not digest); a per-commit tag forces the new image.
#
# TWO MODES, because "local" only works when the cluster's nodes are this
# machine AND want this machine's architecture:
#
#   LOCAL (default)  — build here, import into the local node runtime, never
#                      push. For k3s / docker-desktop.
#   REMOTE           — E2E_BUILDER_SSH set. The nodes are elsewhere, or need a
#                      different architecture, so the image is built on a
#                      builder host and PUSHED to a registry the nodes pull
#                      from. Set E2E_IMAGE_NAMESPACE to a namespace you can push
#                      to; the builder host and that namespace stay in the live
#                      command, never in a file.
set -uo pipefail

TAG="${1:?usage: build-load.sh <TAG>   (e.g. dev-$(git rev-parse --short HEAD))}"

##############################################################################
# REMOTE MODE
##############################################################################
if [ -n "${E2E_BUILDER_SSH:-}" ]; then
  NSP="${E2E_IMAGE_NAMESPACE:?remote mode needs E2E_IMAGE_NAMESPACE=<registry namespace you can push to>}"
  DIR="${E2E_BUILDER_DIR:-/home/$(echo "$E2E_BUILDER_SSH" | cut -d@ -f1)/gpustack.ai/gpustack}"
  IMAGE="${NSP}/gpustack-operator:${TAG}"
  SHA="$(git rev-parse HEAD)"
  bssh() { ssh -o StrictHostKeyChecking=no -o BatchMode=yes "$E2E_BUILDER_SSH" "$@"; }

  echo "== remote build on the configured builder =="
  echo "commit ${SHA}  ->  ${IMAGE}  (repo dir ${DIR})"
  bssh true || { echo "builder unreachable over SSH"; exit 1; }

  # Sync the EXACT commit. The builder is not a clone that follows this branch:
  # a bare `make package` there builds whatever it happens to have checked out,
  # which silently produces an image of the wrong revision and only surfaces
  # later as assert-core.sh's stale-image guard. A bundle carries the objects
  # without needing the builder to have a remote or credentials.
  BUNDLE="$(mktemp -t e2e-XXXXXX).bundle"
  git bundle create "$BUNDLE" HEAD >/dev/null 2>&1 || { echo "git bundle failed"; exit 1; }
  scp -q -o StrictHostKeyChecking=no "$BUNDLE" "${E2E_BUILDER_SSH}:/tmp/e2e-head.bundle" || exit 1
  rm -f "$BUNDLE"
  # Chain with ';' not '&&': a repo-local LFS post-checkout hook that cannot find
  # git-lfs exits non-zero and would abort the rest of an '&&' chain, leaving the
  # fetch silently unperformed.
  bssh "cd '${DIR}' ; git fetch /tmp/e2e-head.bundle HEAD ; git checkout -f ${SHA} ; git rev-parse HEAD" || exit 1

  echo
  echo "== remote package + push =="
  # `bash -lc`: a non-login SSH shell does not source the profile, so the Go
  # toolchain is off PATH and `make` dies with `go: command not found`.
  # PACKAGE_NAMESPACE retags only what this build produces; the operator's own
  # GPUSTACK_CONTAINER_NAMESPACE is deliberately left alone so the images the
  # operator installs still resolve upstream.
  bssh bash -lc "cd '${DIR}' && PACKAGE_TAG='${TAG}' PACKAGE_PUSH=true PACKAGE_NAMESPACE='${NSP}' make package" || {
    echo "remote package failed"; exit 1; }

  echo
  echo "== resolve the pushed digest =="
  # A same-tag rebuild is invisible to a kubelet holding an IfNotPresent cache
  # entry for that tag. Deploy by digest when reusing a tag.
  DIGEST="$(bssh bash -lc "docker buildx imagetools inspect '${IMAGE}' --format '{{.Manifest.Digest}}' 2>/dev/null" | tr -d '\r')"
  echo
  if [ -n "$DIGEST" ]; then
    echo "built & pushed: ${IMAGE}"
    echo "                ${IMAGE%%:*}@${DIGEST}"
    echo
    echo "Pin the digest when the tag is reused, so the kubelet cannot serve a cached layer:"
    echo "  deploy.sh <NS> '${TAG}'   # then verify the running pod's imageID matches ${DIGEST}"
  else
    echo "built & pushed: ${IMAGE}  (digest not resolvable — verify the running imageID by hand)"
  fi
  exit 0
fi

##############################################################################
# LOCAL MODE
##############################################################################
IMAGE="gpustack/gpustack-operator:${TAG}"

echo "== build ${IMAGE} (local only, no push) =="
# This script runs without errexit, so a failed build would otherwise fall through to
# the load step and report the image as built — the run then deploys whatever stale tag
# the node still has, and the real cause surfaces later as an unrelated rollout or
# version failure.
if ! PACKAGE_TAG="$TAG" make package; then
  echo
  echo "build FAILED — no ${IMAGE} was produced, so nothing was loaded."
  exit 1
fi

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
    echo "The node does not obviously share this machine's image store. If pods later report"
    echo "ErrImagePull/ImagePullBackOff, either import manually, e.g.:"
    echo "  docker save '${IMAGE}' | sudo k3s ctr images import -"
    echo "or re-run in remote mode, which builds on a builder host and pushes to a registry:"
    echo "  E2E_BUILDER_SSH=<user@host> E2E_IMAGE_NAMESPACE=<ns> build-load.sh '${TAG}'"
    ;;
esac

echo
echo "built & loaded: ${IMAGE}"
