#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

gpustack::hugo::validate
cd "${ROOT_DIR}/site"
if [[ "${1:-}" == "serve" ]]; then
  "$(gpustack::hugo::bin)" server
else
  "$(gpustack::hugo::bin)" --cleanDestinationDir --destination public
  python3 "${ROOT_DIR}/hack/check/site-links.py" "${ROOT_DIR}/site/public"
fi
