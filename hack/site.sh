#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

gpustack::hugo::validate
cd "${ROOT_DIR}/site"
if [[ "${1:-}" == "serve" ]]; then
  "$(gpustack::hugo::bin)" server --renderToMemory --disableFastRender
else
  destination="${SITE_DESTINATION:-${ROOT_DIR}/site/public}"
  base_url="${SITE_BASE_URL:-/}"
  "$(gpustack::hugo::bin)" --cleanDestinationDir --destination "${destination}" --baseURL "${base_url}"
  python3 "${ROOT_DIR}/hack/check/site-links.py" "${destination}" --base-url "${base_url}"
  python3 "${ROOT_DIR}/hack/check/agent-index.py" "${ROOT_DIR}" --site "${destination}" --base-url "${base_url}"
fi
