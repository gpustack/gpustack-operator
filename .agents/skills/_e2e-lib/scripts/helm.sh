#!/usr/bin/env bash
# Resolves the Helm binary used by the E2E suite.
#
# The suite must use the Docker image's pinned Helm, not whichever Helm happens
# to be on PATH. Keep installation in hack/lib/helm.sh so platform detection,
# download URL construction, and the pin have one implementation.
set -euo pipefail

REPO_ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../../../.." && pwd -P)"

# hack/lib supports an override for its development targets. E2E must instead match the image pin.
unset HELM_VERSION

# hack/lib/init.sh owns ROOT_DIR and loads the Helm installer.
# shellcheck disable=SC1090
source "${REPO_ROOT}/hack/lib/init.sh"

HELM="${ROOT_DIR}/.sbin/helm"
installed_version=""
if [ -x "${HELM}" ]; then
  installed_version="$("${HELM}" version --short 2>/dev/null || true)"
fi

case "${installed_version}" in
"${helm_version}"*) ;;
*)
  gpustack::log::info "installing pinned Helm ${helm_version} for E2E"
  if ! gpustack::helm::helm::install >/dev/null; then
    echo "[e2e] FATAL: Helm ${helm_version} is required at ${HELM}, but its pinned binary could not be installed." >&2
    exit 1
  fi
  ;;
esac

installed_version="$("${HELM}" version --short 2>/dev/null || true)"
case "${installed_version}" in
"${helm_version}"*) ;;
*)
  echo "[e2e] FATAL: expected pinned Helm ${helm_version} at ${HELM}, got ${installed_version:-missing}." >&2
  exit 1
  ;;
esac

printf '%s\n' "${HELM}"
