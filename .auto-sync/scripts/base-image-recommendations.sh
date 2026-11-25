#!/usr/bin/env bash
# .auto-sync/scripts/base-image-recommendations.sh - shared CLI wrapper for Base Image build recommendations.
set -euo pipefail

SCRIPT_DIR="$(CDPATH="" cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
exec python3 -B "${SCRIPT_DIR}/../checks/base_image_builds.py" "$@"
