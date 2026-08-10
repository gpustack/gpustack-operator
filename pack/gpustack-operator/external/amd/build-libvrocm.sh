#!/usr/bin/env bash
#
# build-libvrocm.sh <src-dir> <out-dir>
#
# Build the AMD ROCm logical-slicing artifacts inside a ROCm devel image
# (rocm/dev-ubuntu-22.04). <src-dir> is csrc/amd/rocm-slicing-shim, this repo's own source tree --
# like the THead wrapper and unlike the Ascend and NVIDIA ones there is no upstream to clone and no
# commit to pin. Products, installed into <out-dir>:
#
#   libvrocm.so         the product: the preloaded quota, over the HIP layer
#   rocm-monitor        the usage reader, mounted beside it and run by hand
#   rocm-cumask-check   the CU-mask self-check, the only in-container answer to a silent fail-open
#
# It carries NO compile recipe of its own. The tree's own build.sh is the one place that knows the
# translation-unit lists, the include roots, which artifact may link a ROCm object and which must
# compile through hipcc; a second copy here is exactly the drift that script was written to end.
#
# This script is a build-time asset only; it is never shipped into the final image.
#
set -exo pipefail

src="${1:?usage: build-libvrocm.sh <src-dir> <out-dir>}"
out="${2:?usage: build-libvrocm.sh <src-dir> <out-dir>}"

mkdir -p "${out}"

# No copy into a writable work dir, unlike build-libvgpu.sh: this tree's build.sh writes only to OUT,
# so a read-only bind mount of the sources is enough. Both verbs are left to their defaults, unlike
# the THead wrapper's explicit names: the defaults here are exactly the product and the two readers,
# and the gate binaries under testing/ are a separate verb that ships nothing.
OUT="${out}" bash "${src}/build.sh" lib
OUT="${out}" bash "${src}/build.sh" tool

# The library's four linkage assertions, run by the tree that owns them rather than re-implemented
# here: exported set exactly the interposed HIP names, DT_NEEDED exactly libc.so.6, no GLIBC_
# requirement above the floor, and no hip*/hsa* name among the undefined symbols. They run inside the
# build so a regression fails the build rather than the container start of a workload nobody controls.
OUT="${out}" bash "${src}/build.sh" check

# The two readers are RECORDED, not asserted. rocm-monitor links libc alone but is an executable, so
# its startup stub carries __libc_start_main@GLIBC_2.34 whatever it calls; rocm-cumask-check links the
# ROCm runtime by design, because reading the hardware back is what it is for. Neither is the product,
# and holding either to the library's contract would only encode a floor that cannot be met.
record_linkage() {
    local path="$1" needed max_glibc

    needed="$(readelf -d "${path}" | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort -u | tr '\n' ' ')"
    echo "${path}: NEEDED = ${needed:-none}"

    # Match every component: a two-component pattern truncates GLIBC_2.2.5 to GLIBC_2.2. `|| true`
    # keeps an artifact that requires no versioned symbol at all from aborting the pipeline.
    max_glibc="$(readelf -W --dyn-syms "${path}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
    echo "${path}: max glibc requirement = ${max_glibc:-none}"
}

[[ -f "${out}/libvrocm.so" ]]

for artifact in "${out}/rocm-monitor" "${out}/rocm-cumask-check"; do
    # The readers are commands someone types, so they are the artifacts that have to be executable.
    [[ -x "${artifact}" ]]
    record_linkage "${artifact}"
done

ls -la "${out}"
