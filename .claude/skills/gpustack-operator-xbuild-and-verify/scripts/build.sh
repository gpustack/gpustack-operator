#!/usr/bin/env bash
#
# build.sh <TARGET> — build one xbuild-* builder stage on the target and stage its
# /out artifacts onto the target host. Backend is inferred from the target prefix:
#   xbuild-ascend-cann-* -> vcann-rt   (libvruntime.so + enpu-monitor -> XB_STAGE/{lib,tools})
#   xbuild-nvidia-cuda-* -> HAMi-core  (libvgpu.so -> XB_STAGE/libvgpu.so)
#
#   XB_MODE=local                       bash .../build.sh xbuild-ascend-cann-8-910b
#   XB_MODE=ssh XB_HOST=root@host         bash .../build.sh xbuild-nvidia-cuda-13
#
# Env:
#   XB_PLATFORM   linux/arm64 | linux/amd64   (default: detect from the target arch)
#   XB_IMAGE      built image tag             (default: vcann-build:<suffix> | vgpu-build:<suffix>)
#   XB_STAGE      host dir to stage artifacts (default: /opt/enpu/vcann-rt | /opt/vgpu)
#   XB_REMOTE_CTX remote build-context dir    (default: ~/vcann-build, ssh mode only)
#   XB_REPO       local repo root             (default: git rev-parse --show-toplevel)
#
# The build context is minimal: the Dockerfile, the backend's external build script,
# and — for ascend — the vendored vcann-rt patch dir the stage bind-mounts. Each stage
# is `FROM <vendor base>`, independent of the tools/builder stages. The LIB_*_COMMIT
# pins come from the Dockerfile ARG defaults.
# Native build on a matching-arch host is fast (no qemu); cross-arch uses qemu.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=lib.sh
. "${HERE}/lib.sh"

TARGET="${1:?usage: build.sh <TARGET>  e.g. xbuild-ascend-cann-8-910b | xbuild-nvidia-cuda-13}"
XB_REMOTE_CTX="${XB_REMOTE_CTX:-vcann-build}"   # relative to remote $HOME
XB_REPO="${XB_REPO:-$(git rev-parse --show-toplevel 2>/dev/null || pwd)}"
DOCKERFILE_REL="pack/gpustack-operator/Dockerfile"

# Backend-specific config, keyed off the target prefix:
#   xbuild-ascend-cann-* -> vcann-rt   (/out/lib/libvruntime.so + /out/tools/enpu-monitor)
#   xbuild-nvidia-cuda-* -> HAMi-core  (/out/libvgpu.so)
case "${TARGET}" in
  xbuild-ascend-cann-*)
    XB_BACKEND=ascend
    BUILDSH_REL="pack/gpustack-operator/external/ascend/build-libvnpu.sh"
    PATCHES_REL="pack/gpustack-operator/external/ascend/vcann-rt"
    XB_IMAGE="${XB_IMAGE:-vcann-build:${TARGET#xbuild-ascend-cann-}}"
    XB_STAGE="${XB_STAGE:-/opt/enpu/vcann-rt}"
    ;;
  xbuild-nvidia-cuda-*)
    XB_BACKEND=nvidia
    BUILDSH_REL="pack/gpustack-operator/external/nvidia/build-libvgpu.sh"
    PATCHES_REL=""   # HAMi-core is built from a pristine clone
    XB_IMAGE="${XB_IMAGE:-vgpu-build:${TARGET#xbuild-nvidia-cuda-}}"
    XB_STAGE="${XB_STAGE:-/opt/vgpu}"
    ;;
  *) echo "build.sh: unknown target '${TARGET}' (expect xbuild-ascend-cann-* or xbuild-nvidia-cuda-*)" >&2; exit 2 ;;
esac

# Resolve platform from the target arch unless pinned.
if [ -z "${XB_PLATFORM:-}" ]; then
  arch="$(xrun 'uname -m' | tr -d '[:space:]')"
  case "${arch}" in
    x86_64)  XB_PLATFORM=linux/amd64 ;;
    aarch64) XB_PLATFORM=linux/arm64 ;;
    *) echo "build.sh: cannot map target arch '${arch}'" >&2; exit 1 ;;
  esac
fi

echo "# build ${TARGET} → ${XB_IMAGE} (${XB_PLATFORM}) on $(xtarget_desc)"

# Prepare the build context on the target.
if [ "${XB_MODE}" = ssh ]; then
  CTX="${XB_REMOTE_CTX}"
  xput "${XB_REPO}/${DOCKERFILE_REL}" "${CTX}/${DOCKERFILE_REL}"
  xput "${XB_REPO}/${BUILDSH_REL}"    "${CTX}/${BUILDSH_REL}"
  # The patch dir is bind-mounted by the stage, so it must exist in the remote context.
  # Wipe it first: a patch dropped locally would otherwise linger here and be applied.
  if [ -n "${PATCHES_REL}" ]; then
    xrun "rm -rf '${CTX}/${PATCHES_REL}'" >/dev/null
    for p in "${XB_REPO}/${PATCHES_REL}"/*.patch; do
      [ -f "${p}" ] || { echo "build.sh: no *.patch under ${PATCHES_REL}" >&2; exit 1; }
      xput "${p}" "${CTX}/${PATCHES_REL}/$(basename "${p}")"
    done
  fi
else
  CTX="${XB_REPO}"
fi

# Build (BuildKit; --load into the docker image store so we can extract + reuse it
# as the CANN/CUDA-based workload image for the hardware cases).
xrun "set -o pipefail; cd '${CTX}' && DOCKER_BUILDKIT=1 docker buildx build \
  -f '${DOCKERFILE_REL}' --target '${TARGET}' --platform '${XB_PLATFORM}' \
  --load -t '${XB_IMAGE}' . 2>&1 | tail -15"
rc=$?
[ "${rc}" -eq 0 ] || { echo "build.sh: buildx failed (rc=${rc})"; exit 1; }

# Extract /out artifacts onto the target host under XB_STAGE (per backend).
xsh XB_IMAGE="${XB_IMAGE}" XB_STAGE="${XB_STAGE}" XB_BACKEND="${XB_BACKEND}" <<'PAYLOAD'
set -e
cid="$(docker create "${XB_IMAGE}")"
trap 'docker rm "${cid}" >/dev/null 2>&1 || true' EXIT
if [ "${XB_BACKEND}" = nvidia ]; then
  mkdir -p "${XB_STAGE}"
  docker cp "${cid}:/out/libvgpu.so" "${XB_STAGE}/libvgpu.so"
  chmod 0644 "${XB_STAGE}/libvgpu.so"
  echo "staged ->"; ls -la "${XB_STAGE}/libvgpu.so"
else
  mkdir -p "${XB_STAGE}/lib" "${XB_STAGE}/tools"
  docker cp "${cid}:/out/lib/libvruntime.so"   "${XB_STAGE}/lib/libvruntime.so"
  docker cp "${cid}:/out/tools/enpu-monitor"   "${XB_STAGE}/tools/enpu-monitor"
  chmod 0644 "${XB_STAGE}/lib/libvruntime.so"
  chmod 0755 "${XB_STAGE}/tools/enpu-monitor"
  echo "staged ->"; ls -la "${XB_STAGE}/lib/libvruntime.so" "${XB_STAGE}/tools/enpu-monitor"
fi
PAYLOAD

echo "# built ${XB_IMAGE}; artifacts staged at ${XB_STAGE} (XB_WORKLOAD_IMAGE default = ${XB_IMAGE})"
