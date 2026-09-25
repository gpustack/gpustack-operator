#!/usr/bin/env bash
#
# Assert that cleanup.sh and migrate-pre.sh agree on which Helm releases may own a Kueue this
# operator installed. Each script carries its own KUEUE_RELEASES line, because both also run on
# their own inside a hook Pod. When the two lists differ, one script treats a Kueue as ours and
# the other does not. An image-mode teardown once left its Kueue CRDs stuck Terminating for
# exactly that reason.
#
# A self-test runs first: it breaks one copy of the line and requires the comparison to fail,
# because a comparison that has only ever been seen to pass cannot be told from one that cannot
# fail.

set -o errexit
set -o nounset
set -o pipefail

chart_dir="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"

# compare <cleanup.sh> <migrate-pre.sh>: 0 when each carries exactly one KUEUE_RELEASES line and
# the two lines are identical.
function compare() {
  local cleanup migrate
  cleanup="$(grep -E '^KUEUE_RELEASES=' "$1" || true)"
  migrate="$(grep -E '^KUEUE_RELEASES=' "$2" || true)"
  if [[ -z "${cleanup}" || -z "${migrate}" ]]; then
    echo "KUEUE_RELEASES is missing from $1 or $2" >&2
    return 1
  fi
  if [[ "$(grep -c . <<<"${cleanup}")" -ne 1 || "$(grep -c . <<<"${migrate}")" -ne 1 ]]; then
    echo "KUEUE_RELEASES is defined more than once in $1 or $2" >&2
    return 1
  fi
  if [[ "${cleanup}" != "${migrate}" ]]; then
    echo "KUEUE_RELEASES differs:" >&2
    echo "  $1: ${cleanup}" >&2
    echo "  $2: ${migrate}" >&2
    return 1
  fi
}

tmp="$(mktemp -d)"
trap 'rm -rf "${tmp}"' EXIT
cp "${chart_dir}/files/cleanup.sh" "${tmp}/cleanup.sh"
sed 's/,gpustack-kueue"$/"/' "${chart_dir}/files/migrate-pre.sh" >"${tmp}/migrate-pre.sh"
if compare "${tmp}/cleanup.sh" "${tmp}/migrate-pre.sh" 2>/dev/null; then
  echo "self-test: a KUEUE_RELEASES line with one release dropped was not reported" >&2
  exit 1
fi

compare "${chart_dir}/files/cleanup.sh" "${chart_dir}/files/migrate-pre.sh"
