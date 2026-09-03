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

# One name, fixed, and the same one teardown.sh and case-2.sh use. See teardown.sh for why it is not
# overridable: the chart derives the worker Certificate and its Secret from this name, so an override
# honoured here and not there leaves objects behind that the teardown never looks for.
RELEASE=gpustack-operator
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

# A release that is ALREADY THERE is not residue, and this is what stops the retry below from
# treating it as such.
#
# The loop cannot tell the two apart on its own: an interrupted install and a healthy prior install
# both come back non-zero, and only the first is safe to clean up after. Cleaning up after the second
# uninstalls the release, takes its CRDs and every custom object with them, and then installs
# again — which SUCCEEDS, so the caller sees an ordinary deployment and never learns that the state
# it was standing on is gone.
#
# Asked by querying the release rather than by matching helm's refusal text: "cannot reuse a name" is
# a sentence helm is free to reword, and a check that reads it would go quiet on some future upgrade
# instead of failing.
#
# It also makes the retry's cleanup SOUND rather than merely tolerable: reaching the loop now means
# the release did not exist, so whatever is left behind really is this attempt's own residue —
# exactly the precondition that comment assumed and nothing had established.
# Asked with `list -q`, not `status`, and the difference is the whole guard: `status` exits
# non-zero for "no such release" AND for a timeout, an RBAC denial or an unreachable API server, so a
# guard built on it treats a failed QUESTION as the answer "absent" — and then walks into the very
# path it exists to prevent. `list -q` exits ZERO with empty output when the release is simply not
# there, so absence and failure are separable, and a failure aborts instead of proceeding.
#
# stderr is kept OUT of the captured value. Folding it in with 2>&1 would undo the separation this
# guard is built on: helm exiting 0 with a deprecation warning on stderr would land in the captured
# release list, and a harmless warning would then be indistinguishable from a real match — the same
# conflation as `status`, one level down. The diagnostic is read from a separate capture.
#
# Matched with `grep -Fx` below rather than helm's own --filter, which takes a REGEX. An exact-name
# question deserves an exact-name match; a widened match here refuses an install that should have
# proceeded.
release_query_err="$(mktemp)"
if ! all_releases="$("$HELM" list --all --namespace "$NS" -q \
  2>"$release_query_err")"; then
  echo "cannot query helm releases in ${NS}: $(cat "$release_query_err")" >&2
  echo "refusing to install: whether a release is already there could not be established, and" >&2
  echo "installing over one destroys it and every object it holds." >&2
  rm -f "$release_query_err"
  exit 1
fi
rm -f "$release_query_err"
if printf '%s\n' "$all_releases" | grep -Fxq "$RELEASE"; then
  cat >&2 <<USAGE
release ${RELEASE} already exists in ${NS}. deploy.sh installs; it never upgrades.

Left to run, it would fail on the name, tear that release down as if it were the residue of its own
failed attempt, and install again — a reinstall that reports success while every object the previous
release held, CRDs included, is gone.

  to replace it       bash ${HERE}/teardown.sh ${NS}   (then re-run this)
  to change the image helm upgrade ${RELEASE} ${CHART} -n ${NS} --reuse-values \\
                        --set image.tag=${TAG} --set image.pullPolicy=IfNotPresent
                      kubectl -n ${NS} rollout restart deploy/${RELEASE}-worker
USAGE
  exit 1
fi

for attempt in $(seq 1 "$ATTEMPTS"); do
  echo "== helm install ${RELEASE} into ${NS} (image tag ${TAG})${attempt:+  [attempt ${attempt}/${ATTEMPTS}]} =="
  # The status is captured from the install itself, never from an `if` around it: a compound
  # command whose condition fails and which has no `else` branch exits 0, so `rc=$?` after
  # `if helm install; then exit 0; fi` reads 0 on every failure — and the last attempt would
  # then exit 0 too, reporting a wholly failed install as a successful one.
  "$HELM" install "$RELEASE" "$CHART" \
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
