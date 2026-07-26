#!/usr/bin/env bash
#
# build-libvnpu.sh <src-dir> <out-dir>
#
# Build the vcann-rt logical-slicing runtime (libvruntime.so + enpu-monitor) inside a
# CANN base image. <src-dir> is the vcann-rt source tree (the Dockerfile stage clones
# ubs-virt and passes its ubs-virt-enpu/vcann-rt subdir); the build products are
# installed under:
#   <out-dir>/lib/libvruntime.so
#   <out-dir>/tools/enpu-monitor
#
# This script is a build-time asset only; it is never shipped into the final image.
#
set -exo pipefail

src="${1:?usage: build-libvnpu.sh <src-dir> <out-dir>}"
out="${2:?usage: build-libvnpu.sh <src-dir> <out-dir>}"

# CANN toolkit path (the base image already exports ASCEND_HOME_PATH; be defensive).
export ASCEND_HOME_PATH="${ASCEND_HOME_PATH:-/usr/local/Ascend/ascend-toolkit/latest}"

# Copy the source into a writable work dir.
work="/tmp/vcann-rt"
rm -rf "${work}"
mkdir -p "${work}"
cp -r "${src}/." "${work}/"
cd "${work}"

# vcann-rt links -ldcmi, but dcmi belongs to the host Ascend driver (HDK), which is
# absent from the CANN toolkit image and only injected into the workload at runtime.
# The dcmi entry points are declared weak in dcmi_wrapper.c, so we satisfy the link
# with a stub libdcmi.so (compiled from the in-tree test stub); --as-needed drops it
# from NEEDED and the real driver libdcmi binds at runtime by SONAME. The stub is
# never shipped.
fake_driver="/opt/enpu-fakedriver"
mkdir -p "${fake_driver}/driver/include" "${fake_driver}/driver/lib64/driver"
cp test/stub/dcmi_interface_api.h "${fake_driver}/driver/include/"
gcc -shared -fPIC -I"${fake_driver}/driver/include" -Wl,-soname,libdcmi.so \
    test/stub/dcmi_stub.c -o "${fake_driver}/driver/lib64/driver/libdcmi.so"
export ENPU_ASCEND_DRIVER_PATH="${fake_driver}"

# enpu-monitor is an executable, so by default ld must fully resolve every symbol —
# including the driver/toolkit symbols that ascendcl pulls in transitively through the
# vendor shared objects: the HAL entry points (drv*/hal*) declared in libascend_hal.so
# and ErrorManager::* declared in liberror_manager.so. In a toolkit-only image (no host
# HDK driver) these transitive cross-references between the vendor .so files cannot all
# be resolved at link time — they bind at runtime, where the real driver and the full
# toolkit are present. Allow undefined symbols that originate from shared libraries so
# the executable links against the SDK as shipped; enpu-monitor's own direct deps (dcmi
# stub, c_sec, ascendcl) are still resolved strictly. This is arch-agnostic — it fixes
# both amd64 and arm64 without arch-specific HAL-stub probing (CMake seeds
# CMAKE_EXE_LINKER_FLAGS from LDFLAGS on first configure).
export LDFLAGS="-Wl,--allow-shlib-undefined ${LDFLAGS:-}"

bash make_build.sh

mkdir -p "${out}/lib" "${out}/tools"
install -m 0644 build/libvruntime.so "${out}/lib/libvruntime.so"
install -m 0755 build/enpu-monitor "${out}/tools/enpu-monitor"
ls -la "${out}/lib/libvruntime.so" "${out}/tools/enpu-monitor"
