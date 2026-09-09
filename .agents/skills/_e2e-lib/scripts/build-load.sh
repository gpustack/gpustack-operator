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
#                      push. For kind / k3s / docker-desktop. Which one it is
#                      comes from the nodes' providerID, not from the context
#                      name, which anyone may have renamed.
#   REMOTE           — E2E_BUILDER_SSH set. The nodes are elsewhere, or need a
#                      different architecture, so the image is built on a
#                      builder host and PUSHED to a registry the nodes pull
#                      from. Set E2E_IMAGE_NAMESPACE to a namespace you can push
#                      to; the builder host and that namespace stay in the live
#                      command, never in a file.
set -uo pipefail

# Route every kubectl through the retrying shim. Against a remote API endpoint a read can fail
# on transport alone, and a check that takes such a failure for an answer reports a verdict
# about the network rather than about the operator.
E2E_SHIM_DIR="$(cd "$(dirname "$0")/kubectl-shim" 2>/dev/null && pwd)"
[ -n "$E2E_SHIM_DIR" ] && PATH="$E2E_SHIM_DIR:$PATH"

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
  # ssh joins its arguments with spaces and hands the result to the remote shell, so the
  # quoting of a multi-argument `bssh bash -lc "cd X && Y"` is gone by the time it lands: the
  # login shell runs a bare `cd`, sits in $HOME, and everything after the && runs in the wrong
  # directory. Give the login shell exactly one argument, quoted so it survives the join.
  blogin() { bssh "bash -lc $(printf '%q' "$1")"; }

  echo "== remote build on the configured builder =="
  echo "commit ${SHA}  ->  ${IMAGE}  (repo dir ${DIR})"
  bssh true || { echo "builder unreachable over SSH"; exit 1; }

  # Sync the EXACT commit. The builder is not a clone that follows this branch:
  # a bare `make package` there builds whatever it happens to have checked out,
  # which silently produces an image of the wrong revision and only surfaces
  # later as assert-core.sh's stale-image guard.
  #
  # THE WORKING TREE IS SYNCED BY rsync, AND THE REF IS MOVED WITHOUT A CHECKOUT.
  # `git checkout -f` here would be destructive: this repo routes *.so through
  # git-lfs, and a builder without git-lfs installed has no smudge filter, so a
  # checkout writes the 128-byte POINTER TEXT into the two vendor libraries the
  # image ships (pack/.../gpustack/{libhgml,libuki}.so). The image would then
  # carry text files pretending to be shared libraries, and nothing downstream
  # would notice. rsync carries the real bytes from this machine, which does
  # have them materialized.
  #
  # The ref still has to move, because the image stamps GPUSTACK_GIT_COMMIT from
  # `git rev-parse HEAD` run ON THE BUILDER — a stale ref makes the pushed image
  # lie about its provenance.
  echo "-- mirroring the tracked tree (rsync; no checkout, no --delete)"
  FILES="$(mktemp "${TMPDIR:-/tmp}/e2e-files-XXXXXX")"
  git ls-files -z > "$FILES" || { echo "git ls-files failed"; exit 1; }
  # --files-from sends only tracked paths, so the builder's own untracked state
  # (worktrees, terraform state) is never touched; --delete is deliberately NOT
  # used for the same reason. Deletions this branch made are handled below.
  rsync -az --from0 --files-from="$FILES" --filter='P .git' ./ "${E2E_BUILDER_SSH}:${DIR}/" || {
    echo "rsync to the builder failed"; rm -f "$FILES"; exit 1; }
  rm -f "$FILES"

  # A file this branch DELETED would linger on the builder and can break the
  # build (a stale .go file in a package still compiles). rsync --delete cannot
  # be used here, so the deletions are applied explicitly, from the diff against
  # whatever the builder currently has checked out.
  PREV="$(bssh "cd '${DIR}' && git rev-parse HEAD 2>/dev/null" | tr -d '\r')"
  if [ -n "$PREV" ] && git cat-file -e "${PREV}^{commit}" 2>/dev/null; then
    STALE="$(git diff --name-only --diff-filter=D "$PREV" HEAD)"
    if [ -n "$STALE" ]; then
      echo "-- removing $(printf '%s\n' "$STALE" | wc -l | tr -d ' ') path(s) this branch deleted"
      printf '%s\n' "$STALE" | bssh "cd '${DIR}' && xargs -r rm -f --"
    fi
  else
    echo "-- builder's HEAD is unknown here; skipping the stale-path sweep"
  fi

  # Carry the objects so the ref can be moved to a commit the builder has never
  # seen. A bundle needs no remote and no credentials on that host.
  BUNDLE="$(mktemp "${TMPDIR:-/tmp}/e2e-XXXXXX").bundle"
  git bundle create "$BUNDLE" HEAD >/dev/null 2>&1 || { echo "git bundle failed"; exit 1; }
  scp -q -o StrictHostKeyChecking=no "$BUNDLE" "${E2E_BUILDER_SSH}:/tmp/e2e-head.bundle" || exit 1
  rm -f "$BUNDLE"
  # fetch + update-ref + symbolic-ref + a MIXED reset: every one of these moves
  # refs or the index only. None of them writes a working-tree file, which is
  # the whole point. `git reset -q` without --hard refreshes the index so
  # `git status` is meaningful afterwards.
  BR="e2e/$(git rev-parse --short HEAD)"
  bssh "cd '${DIR}' && \
        git fetch -q /tmp/e2e-head.bundle HEAD && \
        git update-ref refs/heads/${BR} ${SHA} && \
        git symbolic-ref HEAD refs/heads/${BR} && \
        git reset -q && \
        git rev-parse HEAD" || { echo "moving the builder's ref failed"; exit 1; }

  echo
  echo "== remote package + push =="
  # `bash -lc`: a non-login SSH shell does not source the profile, so the Go
  # toolchain is off PATH and `make` dies with `go: command not found`.
  # PACKAGE_NAMESPACE retags only what this build produces; the operator's own
  # GPUSTACK_CONTAINER_NAMESPACE is deliberately left alone so the images the
  # operator installs still resolve upstream.
  blogin "cd '${DIR}' && PACKAGE_TAG='${TAG}' PACKAGE_PUSH=true PACKAGE_NAMESPACE='${NSP}' make package" || {
    echo "remote package failed"; exit 1; }

  echo
  echo "== resolve the pushed digest =="
  # A same-tag rebuild is invisible to a kubelet holding an IfNotPresent cache
  # entry for that tag. Deploy by digest when reusing a tag.
  DIGEST="$(blogin "docker buildx imagetools inspect '${IMAGE}' --format '{{.Manifest.Digest}}' 2>/dev/null" | tr -d '\r')"
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

# Which runtime the nodes actually are is asked of the NODES, not of the context name. A
# context is named by whoever wrote the kubeconfig: a kind cluster reached through a context
# called "docker-desktop" is a real shape, and taking the name at face value there skips the
# import while reporting success — the whole run then verifies a stale image and looks fine.
# kind stamps every node with providerID "kind://docker/<cluster>/<node>", which settles both
# questions at once: that it is kind, and what the cluster is called.
kind_cluster=""
provider=$(kubectl get nodes -o jsonpath='{.items[0].spec.providerID}' 2>/dev/null)
case "$provider" in
  kind://docker/*)
    kind_cluster=$(echo "$provider" | cut -d/ -f4)
    ;;
esac

# docker-desktop shares the docker image store with the node — no import needed.
# k3s (containerd, separate store) needs an explicit import.
# What the branch below keys on: the nodes' answer when they gave one, the context name only
# as a fallback for a runtime that stamps no providerID.
runtime="$ctx"
[ -n "$kind_cluster" ] && runtime="kind"

case "$runtime" in
  kind|kind-*)
    # Every kind node is a container with its OWN containerd store, so sharing this
    # machine's docker store is not enough — the image has to be loaded into each of
    # them, which is what `kind load` does.
    cluster="${kind_cluster:-${ctx#kind-}}"
    echo "kind detected (node providerID) — loading into every node of cluster '${cluster}'"
    if ! command -v kind >/dev/null 2>&1; then
      echo "kind is not on PATH; load by hand: kind load docker-image '${IMAGE}' --name '${cluster}'"
      exit 1
    fi
    if ! kind load docker-image "$IMAGE" --name "$cluster"; then
      echo "kind load FAILED — the nodes do not have ${IMAGE}, so nothing would run it."
      exit 1
    fi
    ;;
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
