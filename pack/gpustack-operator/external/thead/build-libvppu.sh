#!/usr/bin/env bash
#
# build-libvppu.sh <src-dir> <out-dir>
#
# Build the THead PPU logical-slicing artifacts inside the PPU SDK devel image
# (gpustack/thead-ppu-devel). <src-dir> is csrc/thead/ppu-slicing-shim, this repo's own source
# tree -- unlike the Ascend and NVIDIA wrappers there is no upstream to clone and no commit to
# pin. Products, installed into <out-dir>:
#
#   hggc_quota.so        the enforcement half: both quotas over the driver layer
#   hgml_dlsym_hook.so   the visibility half: makes the quota what ppu-smi reports
#   ppu-monitor          the usage reader, mounted beside them and run by hand
#
# It carries NO compile recipe of its own. The tree's own build.sh is the one place that knows
# the translation-unit lists, the include roots and which artifact may link the SDK; a second
# copy here is exactly the drift that script was written to end. `lib` and `tool` are given
# explicit names rather than left to default, because the defaults also build the two
# gate-only controls under testing/, which must never ship.
#
# This script is a build-time asset only; it is never shipped into the final image.
#
set -exo pipefail

src="${1:?usage: build-libvppu.sh <src-dir> <out-dir>}"
out="${2:?usage: build-libvppu.sh <src-dir> <out-dir>}"

mkdir -p "${out}"

# No copy into a writable work dir, unlike build-libvgpu.sh: this tree's build.sh writes only
# to OUT, so a read-only bind mount of the sources is enough.
OUT="${out}" bash "${src}/build.sh" lib hggc_quota hgml_dlsym_hook
OUT="${out}" bash "${src}/build.sh" tool ppu_monitor

# Assert the two properties that let these artifacts load inside a workload container nobody
# controls -- the same pair pack/thead-ppu-devel/Dockerfile asserts for its own smoke object,
# and the same pair cases/thead-case-1.sh decides on. They are checked here, in the build,
# because a shim that lost either one fails at container start with a loader error naming
# neither the quota nor us.
#
# ppu-monitor is held to them too: it is mounted into the same container, so its floor matters
# for the same reason the libraries' does.
assert_linkage() {
    local path="$1" needed max_glibc highest

    needed="$(readelf -d "${path}" | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort -u | tr '\n' ' ')"
    echo "${path}: NEEDED = ${needed:-none}"
    # Nothing but libc, ever: every vendor symbol is resolved at runtime out of the container's
    # own SDK through the dlsym chain, so naming an HGGC/HGML library here would bind us to a
    # copy the workload may not even have.
    [[ -z "${needed}" || "${needed}" == "libc.so.6 " ]]

    # Match every component: a two-component pattern truncates GLIBC_2.2.5 to GLIBC_2.2, which
    # can only ever under-report and so only ever make this ceiling too lenient. `|| true`
    # keeps an artifact that requires no versioned symbol at all from aborting the pipeline.
    max_glibc="$(readelf -W --dyn-syms "${path}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
    echo "${path}: max glibc requirement = ${max_glibc:-none}"
    highest="$(printf '%s\nGLIBC_2.17\n' "${max_glibc:-GLIBC_2.2.5}" | sort -uV | tail -1)"
    [[ "${highest}" == "GLIBC_2.17" ]]
}

for artifact in "${out}/hggc_quota.so" "${out}/hgml_dlsym_hook.so" "${out}/ppu-monitor"; do
    [[ -f "${artifact}" ]]
    assert_linkage "${artifact}"
done

# The reader is a command someone types, so it is the one artifact that has to be executable.
[[ -x "${out}/ppu-monitor" ]]

ls -la "${out}"
