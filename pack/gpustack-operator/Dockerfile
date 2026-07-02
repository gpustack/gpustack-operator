ARG GO_VERSION="1.25.8"
ARG SKOPEO_VERSION="1.22.0"
ARG HELM_VERSION="3.20.0"
ARG VALS_VERSION="0.44.0"
ARG KUSTOMIZE_VERSION="5.8.1"
ARG YTT_VERSION="0.54.0"
ARG KUBECTL_VERSION="1.35.3"
ARG GPUSTACK_DATA_DIR="/var/lib/gpustack"
ARG GPUSTACK_CONF_DIR="/etc/gpustack"
ARG GPUSTACK_CHARTS_DIR="${GPUSTACK_CONF_DIR}/charts"
ARG GPUSTACK_LIB_DIR="${GPUSTACK_CONF_DIR}/lib"
ARG UBUNTU_IMAGE="ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b"

# Soft-slicing preload-library sources, cloned inline per build stage at a pinned
# commit (no git submodule). Bump the commit to update; the ARG busts the build cache.
ARG LIB_UBS_VIRT_ENPU_COMMIT="4d7e4b10ebc2416d761de7d9c1ab0282ae35dae9"
ARG LIB_HAMI_CORE_COMMIT="0831874bce5af56cefca7093dfb2f9f95d1970aa"

# =============================================================================
# Stage: tools
# Download static-linked CLI binaries into
# /out so the final image never has to install aria2/xz-utils.
# =============================================================================
FROM ${UBUNTU_IMAGE} AS tools
SHELL ["/bin/bash", "-c"]

ARG TARGETOS
ARG TARGETARCH

ENV DEBIAN_FRONTEND=noninteractive

RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends ca-certificates aria2
    rm -rf /var/lib/apt/lists/*
    mkdir -p /out/bin
EOF

ARG HELM_VERSION
RUN <<EOF
    set -exo pipefail
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/helm.tar.gz "https://get.helm.sh/helm-v${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz"
    tar -xzf /tmp/helm.tar.gz --directory /out/bin/ ${TARGETOS}-${TARGETARCH}/helm --no-same-owner --strip-components=1
    chmod a+x /out/bin/helm
    rm -f /tmp/helm.tar.gz
EOF

ARG VALS_VERSION
RUN <<EOF
    set -exo pipefail
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/vals.tar.gz "https://github.com/helmfile/vals/releases/download/v${VALS_VERSION}/vals_${VALS_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz"
    tar -xzf /tmp/vals.tar.gz --directory /out/bin/ vals --no-same-owner
    chmod a+x /out/bin/vals
    rm -f /tmp/vals.tar.gz
EOF

ARG KUSTOMIZE_VERSION
RUN <<EOF
    set -exo pipefail
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/kustomize.tar.gz "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz"
    tar -xzf /tmp/kustomize.tar.gz --directory /out/bin/ kustomize --no-same-owner
    chmod a+x /out/bin/kustomize
    rm -f /tmp/kustomize.tar.gz
EOF

ARG YTT_VERSION
RUN <<EOF
    set -exo pipefail
    aria2c -x $(nproc) -s $(nproc) -c -o /out/bin/ytt "https://github.com/carvel-dev/ytt/releases/download/v${YTT_VERSION}/ytt-${TARGETOS}-${TARGETARCH}"
    chmod a+x /out/bin/ytt
EOF

ARG KUBECTL_VERSION
RUN <<EOF
    set -exo pipefail
    aria2c -x $(nproc) -s $(nproc) -c -o /out/bin/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/${TARGETOS}/${TARGETARCH}/kubectl"
    chmod a+x /out/bin/kubectl
EOF

# =============================================================================
# Stage: builder
# Build skopeo and gpustack-operator from source. Uses the official
# golang:<version>-bookworm image (Debian 12 ships glibc 2.36, which is older
# than Ubuntu 24.04's 2.39 — so the resulting binaries link against symbols
# the runtime image still provides).
# =============================================================================
FROM golang:${GO_VERSION}-bookworm AS builder
SHELL ["/bin/bash", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH
ARG BUILDPLATFORM

ENV DEBIAN_FRONTEND=noninteractive \
    GOPROXY="https://proxy.golang.org,direct" \
    GOCACHE="/root/.cache/go-build" \
    GOMODCACHE="/go/pkg/mod"

RUN <<EOF
    # Build deps
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates curl git git-lfs \
        build-essential pkg-config \
        unzip file \
        libgpgme-dev libassuan-dev libbtrfs-dev libdrm-dev
    rm -rf /var/lib/apt/lists/*
    mkdir -p /out/bin
EOF

# Build skopeo

ARG SKOPEO_VERSION

RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    --mount=type=cache,target=/go/pkg/mod,sharing=locked <<EOF
    # Skopeo
    set -exo pipefail

    # Download
    git -C /tmp clone --recursive --shallow-submodules \
        --depth 1 --branch v${SKOPEO_VERSION} --single-branch \
        https://github.com/containers/skopeo.git skopeo

    # Build and install
    pushd /tmp/skopeo \
        && sed -i "/export GOPROXY=.*/d" Makefile \
        && make vendor \
        && DISABLE_DOCS=1 PREFIX=/out make install-binary
    popd

    # Review
    /out/bin/skopeo --version

    # Cleanup
    rm -rf /tmp/skopeo
EOF

# Build gpustack-operator

ARG GPUSTACK_WORKSPACE="/usr/local/go/src/gpustack.ai/gpustack"
ARG GPUSTACK_GIT_COMMIT
ARG GPUSTACK_GIT_VERSION

RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=bind,target=${GPUSTACK_WORKSPACE},rw \
    --mount=type=cache,target=${GPUSTACK_WORKSPACE}/.sbin,sharing=locked <<EOF
    # Build
    set -exo pipefail
    echo "GPUSTACK_GIT_COMMIT=${GPUSTACK_GIT_COMMIT}"
    echo "GPUSTACK_GIT_VERSION=${GPUSTACK_GIT_VERSION}"

    # Stamp the version deterministically from the build-arg (set by hack/package.sh / CI from the git
    # tag), so the builder cache key tracks the VERSION — not just the commit — and a release tag on an
    # already-built commit cannot serve a stale binary. Empty falls back to in-container git detection.
    if [[ -n "${GPUSTACK_GIT_VERSION}" ]]; then
        export VERSION="${GPUSTACK_GIT_VERSION}"
    fi

    # Link staging modules under GOROOT/src for legacy lookups.
    for dir in $(find ${GPUSTACK_WORKSPACE}/staging -mindepth 1 -maxdepth 1 -type d); do
        target="$(go env GOROOT)/src/$(basename ${dir})"
        ln -s ${dir} ${target}
    done

    # Build
    CI="true"
    if [[ "${BUILDPLATFORM}" != "${TARGETPLATFORM}" ]]; then
      CI="false"
    fi
    BUILD_OS=${TARGETOS}
    BUILD_ARCH=${TARGETARCH}
    LINT_DIRTY=${CI}
    pushd ${GPUSTACK_WORKSPACE} \
        && if [[ "${CI}" == "true" ]]; then make ci; else make build; fi \
        && cp ${GPUSTACK_WORKSPACE}/.dist/build/gpustack-operator /out/bin/gpustack-operator
    popd

    # Review
    /out/bin/gpustack-operator --version
EOF

# =============================================================================
# Stages: xbuild-ascend-cann-* (Ascend soft-slicing)
# Build the vcann-rt runtime (libvruntime.so + enpu-monitor) once per CANN/family
# target against the matching quay.io/ascend/cann base image. Each stage installs
# its products under /out/{lib,tools}; the final image copies them into
# ${GPUSTACK_LIB_DIR}/ascend/cann-<major>-<family>. dcmi is stubbed at link time
# (see build-libvnpu.sh) since the driver is host-injected at runtime.
# =============================================================================
FROM quay.io/ascend/cann:8.5.0-910b-ubuntu22.04-py3.11 AS xbuild-ascend-cann-8-910b
ARG LIB_UBS_VIRT_ENPU_COMMIT
RUN --mount=type=bind,src=pack/gpustack-operator/external/ascend/build-libvnpu.sh,target=/build-libvnpu.sh <<EOF
    set -exo pipefail
    command -v git >/dev/null 2>&1 || { apt-get update -y && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*; }
    rm -rf /tmp/ubs-virt && git init -q /tmp/ubs-virt && cd /tmp/ubs-virt
    git remote add origin https://gitcode.com/openeuler/ubs-virt.git
    git fetch --depth 1 origin "${LIB_UBS_VIRT_ENPU_COMMIT}" || git fetch origin
    git checkout -q "${LIB_UBS_VIRT_ENPU_COMMIT}"
    bash /build-libvnpu.sh /tmp/ubs-virt/ubs-virt-enpu/vcann-rt /out
EOF

FROM quay.io/ascend/cann:8.5.0-a3-ubuntu22.04-py3.11 AS xbuild-ascend-cann-8-910c
ARG LIB_UBS_VIRT_ENPU_COMMIT
RUN --mount=type=bind,src=pack/gpustack-operator/external/ascend/build-libvnpu.sh,target=/build-libvnpu.sh <<EOF
    set -exo pipefail
    command -v git >/dev/null 2>&1 || { apt-get update -y && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*; }
    rm -rf /tmp/ubs-virt && git init -q /tmp/ubs-virt && cd /tmp/ubs-virt
    git remote add origin https://gitcode.com/openeuler/ubs-virt.git
    git fetch --depth 1 origin "${LIB_UBS_VIRT_ENPU_COMMIT}" || git fetch origin
    git checkout -q "${LIB_UBS_VIRT_ENPU_COMMIT}"
    bash /build-libvnpu.sh /tmp/ubs-virt/ubs-virt-enpu/vcann-rt /out
EOF

FROM quay.io/ascend/cann:9.1.0-beta.1-910b-ubuntu22.04-py3.12 AS xbuild-ascend-cann-9-910b
ARG LIB_UBS_VIRT_ENPU_COMMIT
RUN --mount=type=bind,src=pack/gpustack-operator/external/ascend/build-libvnpu.sh,target=/build-libvnpu.sh <<EOF
    set -exo pipefail
    command -v git >/dev/null 2>&1 || { apt-get update -y && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*; }
    rm -rf /tmp/ubs-virt && git init -q /tmp/ubs-virt && cd /tmp/ubs-virt
    git remote add origin https://gitcode.com/openeuler/ubs-virt.git
    git fetch --depth 1 origin "${LIB_UBS_VIRT_ENPU_COMMIT}" || git fetch origin
    git checkout -q "${LIB_UBS_VIRT_ENPU_COMMIT}"
    bash /build-libvnpu.sh /tmp/ubs-virt/ubs-virt-enpu/vcann-rt /out
EOF

FROM quay.io/ascend/cann:9.1.0-beta.1-a3-ubuntu22.04-py3.12 AS xbuild-ascend-cann-9-910c
ARG LIB_UBS_VIRT_ENPU_COMMIT
RUN --mount=type=bind,src=pack/gpustack-operator/external/ascend/build-libvnpu.sh,target=/build-libvnpu.sh <<EOF
    set -exo pipefail
    command -v git >/dev/null 2>&1 || { apt-get update -y && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*; }
    rm -rf /tmp/ubs-virt && git init -q /tmp/ubs-virt && cd /tmp/ubs-virt
    git remote add origin https://gitcode.com/openeuler/ubs-virt.git
    git fetch --depth 1 origin "${LIB_UBS_VIRT_ENPU_COMMIT}" || git fetch origin
    git checkout -q "${LIB_UBS_VIRT_ENPU_COMMIT}"
    bash /build-libvnpu.sh /tmp/ubs-virt/ubs-virt-enpu/vcann-rt /out
EOF

FROM quay.io/ascend/cann:9.1.0-beta.1-950-ubuntu22.04-py3.12 AS xbuild-ascend-cann-9-950
ARG LIB_UBS_VIRT_ENPU_COMMIT
RUN --mount=type=bind,src=pack/gpustack-operator/external/ascend/build-libvnpu.sh,target=/build-libvnpu.sh <<EOF
    set -exo pipefail
    command -v git >/dev/null 2>&1 || { apt-get update -y && apt-get install -y --no-install-recommends git ca-certificates && rm -rf /var/lib/apt/lists/*; }
    rm -rf /tmp/ubs-virt && git init -q /tmp/ubs-virt && cd /tmp/ubs-virt
    git remote add origin https://gitcode.com/openeuler/ubs-virt.git
    git fetch --depth 1 origin "${LIB_UBS_VIRT_ENPU_COMMIT}" || git fetch origin
    git checkout -q "${LIB_UBS_VIRT_ENPU_COMMIT}"
    bash /build-libvnpu.sh /tmp/ubs-virt/ubs-virt-enpu/vcann-rt /out
EOF

# =============================================================================
# Stages: xbuild-nvidia-cuda-* (NVIDIA soft-slicing)
# Build the HAMi-core preload library (libvgpu.so) once per CUDA major against the
# matching nvidia/cuda devel image. Each stage installs its product at /out; the
# final image copies it into ${GPUSTACK_LIB_DIR}/nvidia/cuda-<major>. HAMi-core only
# needs the CUDA headers (it hooks the driver/NVML at runtime), so the devel image
# alone suffices once cmake is installed.
# =============================================================================
FROM nvidia/cuda:12.9.2-cudnn-devel-ubi8 AS xbuild-nvidia-cuda-12
ARG LIB_HAMI_CORE_COMMIT
RUN dnf install -y cmake git && dnf clean all
RUN --mount=type=bind,src=pack/gpustack-operator/external/nvidia/build-libvgpu.sh,target=/build-libvgpu.sh <<EOF
    set -exo pipefail
    rm -rf /tmp/hami-core-src && git init -q /tmp/hami-core-src && cd /tmp/hami-core-src
    git remote add origin https://github.com/Project-HAMi/HAMi-core.git
    git fetch --depth 1 origin "${LIB_HAMI_CORE_COMMIT}" || git fetch origin
    git checkout -q "${LIB_HAMI_CORE_COMMIT}"
    bash /build-libvgpu.sh /tmp/hami-core-src /out
EOF

FROM nvidia/cuda:13.0.3-cudnn-devel-ubi8 AS xbuild-nvidia-cuda-13
ARG LIB_HAMI_CORE_COMMIT
RUN dnf install -y cmake git && dnf clean all
RUN --mount=type=bind,src=pack/gpustack-operator/external/nvidia/build-libvgpu.sh,target=/build-libvgpu.sh <<EOF
    set -exo pipefail
    rm -rf /tmp/hami-core-src && git init -q /tmp/hami-core-src && cd /tmp/hami-core-src
    git remote add origin https://github.com/Project-HAMi/HAMi-core.git
    git fetch --depth 1 origin "${LIB_HAMI_CORE_COMMIT}" || git fetch origin
    git checkout -q "${LIB_HAMI_CORE_COMMIT}"
    bash /build-libvgpu.sh /tmp/hami-core-src /out
EOF

# =============================================================================
# Stage: gpustack (final)
# Ubuntu 24.04 with only runtime packages — no Go, no build toolchain, no
# download utilities, no -dev headers. Compiled binaries and CLI tools are
# copied from the earlier stages.
# =============================================================================
FROM ${UBUNTU_IMAGE} AS gpustack
SHELL ["/bin/bash", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

## Install runtime packages

ENV DEBIAN_FRONTEND=noninteractive \
    LANG='en_US.UTF-8' \
    LANGUAGE='en_US:en' \
    LC_ALL='en_US.UTF-8'

RUN <<EOF
    # Runtime packages
    set -exo pipefail

    # Refresh
    apt-get update -y && apt-get install -y --no-install-recommends \
        software-properties-common apt-transport-https \
        ca-certificates gnupg2 lsb-release gnupg-agent \
      && apt-get update -y

    # Install
    #  - core utilities the runtime image is expected to ship for ops/debug
    #  - shared libraries needed by skopeo (libgpgme/libassuan/libbtrfs/libdevmapper)
    #  - libdrm runtime for amdgpu binding dlopen
    apt-get install -y --no-install-recommends \
        bash openssl curl binutils \
        tzdata locales \
        iproute2 iputils-ping ifstat net-tools dnsutils pciutils ipmitool \
        procps sysstat glances htop numactl \
        vim jq bc tree gosu tini \
        logrotate cron netcat-openbsd \
        libgpgme11t64 libassuan0 libbtrfs0 libdevmapper1.02.1 \
        libdrm2 libdrm-amdgpu1

    # Update locale
    localedef -i en_US -c -f UTF-8 -A /usr/share/locale/locale.alias en_US.UTF-8

    # Update timezone
    rm -f /etc/localtime \
        && ln -sf /usr/share/zoneinfo/Asia/Shanghai /etc/localtime \
        && echo "Asia/Shanghai" > /etc/timezone \
        && dpkg-reconfigure --frontend noninteractive tzdata

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

## Copy the Ascend soft-slicing runtime (vcann-rt) per CANN/family target.
## buildkit builds each xbuild-ascend-cann-* stage for the final image's target arch
## (the CANN base images are multi-arch), so these are arch-correct.

ARG GPUSTACK_LIB_DIR

COPY --from=xbuild-ascend-cann-8-910b /out ${GPUSTACK_LIB_DIR}/ascend/cann-8-910b
COPY --from=xbuild-ascend-cann-8-910c /out ${GPUSTACK_LIB_DIR}/ascend/cann-8-910c
COPY --from=xbuild-ascend-cann-9-910b /out ${GPUSTACK_LIB_DIR}/ascend/cann-9-910b
COPY --from=xbuild-ascend-cann-9-910c /out ${GPUSTACK_LIB_DIR}/ascend/cann-9-910c
COPY --from=xbuild-ascend-cann-9-950  /out ${GPUSTACK_LIB_DIR}/ascend/cann-9-950

## Copy the NVIDIA soft-slicing runtime (HAMi-core) per CUDA major.
COPY --from=xbuild-nvidia-cuda-12 /out ${GPUSTACK_LIB_DIR}/nvidia/cuda-12
COPY --from=xbuild-nvidia-cuda-13 /out ${GPUSTACK_LIB_DIR}/nvidia/cuda-13

## Copy CLI binaries from the build stages

COPY --from=tools     /out/bin/helm        /usr/bin/helm
COPY --from=tools     /out/bin/vals        /usr/bin/vals
COPY --from=tools     /out/bin/kustomize   /usr/bin/kustomize
COPY --from=tools     /out/bin/ytt         /usr/bin/ytt
COPY --from=tools     /out/bin/kubectl     /usr/bin/kubectl
COPY --from=builder   /out/bin/skopeo      /usr/bin/skopeo
COPY --from=builder   /out/bin/gpustack-operator /usr/bin/gpustack-operator

## Skopeo policy and sanity checks

RUN <<EOF
    set -exo pipefail

    # Configure Skopeo to allow insecure registries.
    mkdir -p /etc/containers
    cat<<EOT > /etc/containers/policy.json
{
  "default": [
    {
      "type": "insecureAcceptAnything"
    }
  ]
}
EOT

    # Review
    skopeo --version
    helm version
    vals version
    kustomize version
    ytt version
    kubectl version --client
    gpustack-operator --version
EOF

# Prepare Charts and Images for Airgap Installation
## NB(thxCode): Update the following files if changed.
## - pkg/worker/kuberess/apps_*.go.

ARG GPUSTACK_CHARTS_DIR

## CSI Driver S3
ADD https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz "${GPUSTACK_CHARTS_DIR}/csi-driver-s3-0.43.7.tgz"

## CSI Driver NFS
ADD https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.2/csi-driver-nfs-4.13.2.tgz "${GPUSTACK_CHARTS_DIR}/csi-driver-nfs-4.13.2.tgz"

## Kueue
ADD https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.2/kueue-0.18.2.tgz "${GPUSTACK_CHARTS_DIR}/kueue-0.18.2.tgz"

## Node Features Discovery
ADD https://thxcode.github.io/node-feature-discovery/charts/node-feature-discovery-0.18.3.tgz "${GPUSTACK_CHARTS_DIR}/node-feature-discovery-0.18.3.tgz"

## GPUStack Operator
RUN --mount=type=bind,src=deploy/gpustack-operator/chart,target=/tmp/gpustack-operator-chart <<EOF
    set -exo pipefail

    # Extract version from the built binary, fallback to 0.0.0 if not in expected format (e.g. during development).
    VERSION="$(gpustack-operator --version | awk '{for (i = 1; i < NF; i++) if ($i == "version") print $(i + 1)}')"
    CHART_VERSION="${VERSION#v}"
    if [[ ! "${CHART_VERSION}" =~ ^[0-9]+\.[0-9]+\.[0-9]+([-+][0-9A-Za-z.-]+)?$ ]]; then
        CHART_VERSION="0.0.0"
    fi

    # Package the chart with the resolved version.
    helm package /tmp/gpustack-operator-chart \
        --version "${CHART_VERSION}" --app-version "${VERSION}" \
        --destination "${GPUSTACK_CHARTS_DIR}"
EOF

# Runtime

RUN --mount=type=bind,src=pack/gpustack-operator/rootfs,target=/rootfs <<EOF
    # Runtime
    set -exo pipefail

    # Prepare libs
    cp -rf /rootfs/lib/$(uname -m)-linux-gnu/gpustack /lib/$(uname -m)-linux-gnu/
    cat <<EOT >/etc/ld.so.conf.d/zz-zzzz-9999999999-gpustack.conf
/lib/$(uname -m)-linux-gnu/gpustack
EOT
    ldconfig -v

    # Install the host-staging helper used by the device-manager init container.
    install -m 0755 /rootfs/usr/bin/copy-dir.sh /usr/bin/copy-dir.sh

    # Install the soft-slicing ld.so.preload files (point at the in-container library
    # mounts); staged onto the host alongside the per-target preload libs.
    install -D -m 0644 /rootfs/etc/gpustack/lib/ascend/ld.so.preload \
        "${GPUSTACK_LIB_DIR}"/ascend/ld.so.preload
    install -D -m 0644 /rootfs/etc/gpustack/lib/nvidia/ld.so.preload \
        "${GPUSTACK_LIB_DIR}"/nvidia/ld.so.preload

    # Try to update PCI IDs
    if ! update-pciids; then
        curl -o /usr/share/misc/pci.ids https://pci-ids.ucw.cz/v2.2/pci.ids || true
    fi

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/*
EOF

## AMD
## -- Tell container runtime to inject all devices.
ENV AMD_VISIBLE_DEVICES="all"
## Ascend
## -- Configure paths.
ENV LD_LIBRARY_PATH="/usr/local/dcmi:/usr/local/Ascend/driver/lib64/common:/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/ascend-toolkit/latest/runtime/lib64:${LD_LIBRARY_PATH}"
## Cambricon
## -- Tell container runtime to inject all devices, and configure paths.
ENV CAMBRICON_VISIBLE_DEVICES="all" \
    NEUWARE_HOME="/usr/local/neuware" \
    LD_LIBRARY_PATH="/usr/local/neuware/lib64:${LD_LIBRARY_PATH}"
## Hygon
## -- Nothing to do.
## Iluvatar
## -- Tell container runtime to inject all devices, and configure paths.
ENV IX_VISIBLE_DEVICES="all" \
    COREX_HOME="/usr/local/corex" \
    LD_LIBRARY_PATH="/usr/local/corex/lib64:${LD_LIBRARY_PATH}"
## Metax
## -- Configure paths.
ENV LD_LIBRARY_PATH="/opt/maca/lib:/opt/mxdriver/lib:${LD_LIBRARY_PATH}"
## MThreads
## -- Tell container runtime to inject all devices.
ENV MTHREADS_VISIBLE_DEVICES="all" \
    MTHREADS_DRIVER_CAPABILITIES="compute,utility"
## NVIDIA
## -- Tell container runtime to inject all devices.
ENV NVIDIA_DISABLE_REQUIRE="true" \
    NVIDIA_VISIBLE_DEVICES="all" \
    NVIDIA_DRIVER_CAPABILITIES="compute,utility"
## THead
## -- Configure paths.
ENV PPU_HOME="/usr/local/PPU_SDK" \
    LD_LIBRARY_PATH="/usr/local/PPU_SDK/CUDA_SDK/lib64:/usr/local/PPU_SDK/lib:${LD_LIBRARY_PATH}"

# Entrypoint

ARG GPUSTACK_DATA_DIR

ENV _RUNNING_INSIDE_CONTAINER_="true" \
    KUBE_FEATURE_WatchListClient="false"

VOLUME ${GPUSTACK_DATA_DIR}
WORKDIR /
EXPOSE 80 443
ENTRYPOINT [ "tini", "--" ]
