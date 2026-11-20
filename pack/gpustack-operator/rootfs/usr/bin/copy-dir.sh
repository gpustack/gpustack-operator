#!/usr/bin/env bash
#
# copy-dir.sh <src> <dst>
#
# Recursively stage the contents of <src> into <dst>:
#   - create <dst> (and any missing parents) when absent;
#   - for every regular file, copy only when the destination is missing or its
#     sha256 checksum differs from the source;
#   - stage into a sibling temporary file and atomically rename over <dst> so
#     running containers reading or mmap-ing mounted files never see a partial write.
#
set -euo pipefail

src="${1:?usage: copy-dir.sh <src> <dst>}"
dst="${2:?usage: copy-dir.sh <src> <dst>}"

if [[ ! -d "${src}" ]]; then
    echo "copy-dir.sh: source directory not found: ${src}" >&2
    exit 1
fi

mkdir -p "${dst}"

# Sweep any orphaned staging temp files left behind by an interrupted copy.
find "${dst}" -type f -name '.*.stage-*' -delete 2>/dev/null || true

# Walk regular files under <src>, relative to <src>, NUL-separated for safety.
while IFS= read -r -d '' rel; do
    s="${src}/${rel}"
    d="${dst}/${rel}"
    if [[ -f "${d}" ]]; then
        s_sum="$(sha256sum "${s}" | awk '{ print $1 }')"
        d_sum="$(sha256sum "${d}" | awk '{ print $1 }')"
        if [[ "${s_sum}" == "${d_sum}" ]]; then
            continue
        fi
    fi
    d_dir="$(dirname "${d}")"
    mkdir -p "${d_dir}"
    tmp="$(mktemp "${d_dir}/.$(basename "${d}").stage-XXXXXX")"
    if cp --preserve=mode,timestamps "${s}" "${tmp}" && mv -f "${tmp}" "${d}"; then
        continue
    else
        rm -f "${tmp}"
        exit 1
    fi
done < <(cd "${src}" && find . -type f -printf '%P\0')
