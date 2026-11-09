#!/usr/bin/env bash
#
# copy-dir.sh <src> <dst>
#
# Recursively stage the contents of <src> into <dst>:
#   - create <dst> (and any missing parents) when absent;
#   - remove regular files in <dst> that are no longer present in <src>;
#   - for every regular file, copy only when the destination is missing or its
#     sha256 checksum differs from the source.
#
# The checksum guard keeps the copy idempotent and avoids rewriting a wheel
# file that an engine container is concurrently reading.
#
set -euo pipefail

src="${1:?usage: copy-dir.sh <src> <dst>}"
dst="${2:?usage: copy-dir.sh <src> <dst>}"

if [[ ! -d "${src}" ]]; then
    echo "copy-dir.sh: source directory not found: ${src}" >&2
    exit 1
fi

mkdir -p "${dst}"

# Remove stale files under <dst> that are absent from <src>.
while IFS= read -r -d "" rel; do
    if [[ ! -f "${src}/${rel}" ]]; then
        rm -f "${dst}/${rel}"
    fi
done < <(cd "${dst}" && find . -type f -printf "%P\0")

# Walk regular files under <src>, relative to <src>, NUL-separated for safety.
while IFS= read -r -d "" rel; do
    s="${src}/${rel}"
    d="${dst}/${rel}"
    if [[ -f "${d}" ]]; then
        s_sum="$(sha256sum "${s}" | awk "{ print \$1 }")"
        d_sum="$(sha256sum "${d}" | awk "{ print \$1 }")"
        if [[ "${s_sum}" == "${d_sum}" ]]; then
            continue
        fi
    fi
    mkdir -p "$(dirname "${d}")"
    cp -f --preserve=mode,timestamps "${s}" "${d}"
done < <(cd "${src}" && find . -type f -printf "%P\0")
