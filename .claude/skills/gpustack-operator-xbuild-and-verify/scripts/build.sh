#!/usr/bin/env bash
#
# build.sh <TARGET> — produce one backend's artifacts and stage them on the target host, so
# every case that follows only has to inspect and run them. Backend is inferred from the target:
#   xbuild-ascend-cann-* -> vcann-rt   (libvruntime.so + enpu-monitor -> XB_STAGE/{lib,tools})
#   xbuild-nvidia-cuda-* -> HAMi-core  (libvgpu.so -> XB_STAGE/libvgpu.so)
#   xbuild-thead-ppu     -> the slicing shims (csrc/thead/ppu-slicing-shim -> XB_STAGE)
#
#   XB_MODE=local                       bash .../build.sh xbuild-ascend-cann-8-910b
#   XB_MODE=ssh XB_HOST=root@host         bash .../build.sh xbuild-nvidia-cuda-13
#   XB_MODE=ssh XB_HOST=root@ppu-host XB_CTR=nerdctl  bash .../build.sh xbuild-thead-ppu
#
# THEAD IS BUILT DIFFERENTLY, and the difference is the backend's own: there is no builder stage
# for it in the Dockerfile yet, and its host needs none — the sources are compiled INSIDE the
# published SDK image with `run`, because that image is where hggc.h lives, and `run` is all a
# docker-less host with nerdctl can do. The recipes themselves are not here: the shim tree owns
# them in its own `build.sh`, which this arm stages and calls. Once the `xbuild-thead-ppu` stage
# exists in the Dockerfile, this arm can switch to buildx under the same target name.
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

TARGET="${1:?usage: build.sh <TARGET>  e.g. xbuild-ascend-cann-8-910b | xbuild-nvidia-cuda-13 | xbuild-thead-ppu}"
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
  xbuild-thead-ppu)
    XB_BACKEND=thead
    XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
    XB_STAGE="${XB_STAGE:-/tmp/vppu}"
    SHIM_REL="csrc/thead/ppu-slicing-shim"
    ;;
  *) echo "build.sh: unknown target '${TARGET}' (expect xbuild-ascend-cann-*, xbuild-nvidia-cuda-* or xbuild-thead-ppu)" >&2; exit 2 ;;
esac

# THead: stage the shim tree onto the target and compile it inside the SDK image. It shares
# nothing with the buildx path below, so it returns rather than threading conditionals through it.
if [ "${XB_BACKEND}" = thead ]; then
  XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"   # the SDK ships targets/x86_64-linux and nothing else
  xctr_resolve || { echo "build.sh: no container runtime on $(xtarget_desc)" >&2; exit 2; }
  echo "# build ${TARGET} in ${XB_IMAGE} (${XB_PLATFORM}) on $(xtarget_desc)"

  # Every source, by the tree's own layout: the module directories keep their names because the
  # sources include each other by that path, and the compile has to resolve them exactly as a
  # host build would.
  for f in build.sh \
           common/vppu.h common/vppu_log.c common/vppu_quota.h common/vppu_quota.c \
           common/vppu_ledger.h common/vppu_ledger.c common/vppu_pid.h common/vppu_pid.c \
           common/vppu_test.c \
           hggc/hggc_quota.h hggc/hggc_quota.c hggc/hggc_mem.c hggc/hggc_mem_v1.c \
           hggc/hggc_entry.c hggc/hggc_compute.c hggc/hggc_launch.c \
           hgml/hgml_dlsym_hook.c \
           tools/ppu_monitor.c \
           testing/hgml_nohook.c testing/hgml_util_probe.c testing/hggc_mem_paths.c \
           testing/dlsym_origin.c testing/dlsym_stack.c testing/hggc_launch_load.cu; do
    [ -f "${XB_REPO}/${SHIM_REL}/${f}" ] \
      || { echo "build.sh: source not found: ${SHIM_REL}/${f}" >&2; exit 2; }
    xput "${XB_REPO}/${SHIM_REL}/${f}" "${XB_STAGE}/${f}" \
      || { echo "build.sh: failed to stage ${f}" >&2; exit 2; }
  done

  # V=1 so the log shows what was compiled; the tree's build.sh is otherwise silent, which is
  # what lets thead-case-1.sh judge "compiles clean" on empty output.
  xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
      STAGE="${XB_STAGE}" PLATFORM="${XB_PLATFORM}" <<'PAYLOAD'
set -u
# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
  -v "${STAGE}:/work" -w /work "${IMG}" bash -lc 'set -e
  chmod +x /work/build.sh
  V=1 /work/build.sh lib
  V=1 /work/build.sh tool
  V=1 /work/build.sh test
  V=1 /work/build.sh unit
  ls -la /work/*.so /work/ppu-monitor'
PAYLOAD
  rc=$?
  [ "${rc}" -eq 0 ] || { echo "build.sh: the shim build failed (rc=${rc})"; exit 1; }
  echo "# built the slicing shims; artifacts staged at ${XB_STAGE} on $(xtarget_desc)"
  exit 0
fi

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
