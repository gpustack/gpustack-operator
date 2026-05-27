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
ARG UBUNTU_IMAGE="ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b"

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

RUN --mount=type=cache,target=/root/.cache/go-build,sharing=locked \
    --mount=type=cache,target=/go/pkg/mod,sharing=locked \
    --mount=type=bind,target=${GPUSTACK_WORKSPACE},rw \
    --mount=type=cache,target=${GPUSTACK_WORKSPACE}/.sbin,sharing=locked <<EOF
    # Build
    set -exo pipefail

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

    # Link gpustack-operator to gpustack for backward compatibility.
    # TODO: Remove this later.
    cp /usr/bin/gpustack-operator /usr/bin/gpustack

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

## Kueue

ADD https://github.com/kubernetes-sigs/kueue/releases/download/v0.17.2/kueue-0.17.2.tgz "${GPUSTACK_CHARTS_DIR}/kueue-0.17.2.tgz"

## Node Features Discovery

ADD https://github.com/kubernetes-sigs/node-feature-discovery/releases/download/v0.18.3/node-feature-discovery-chart-0.18.3.tgz "${GPUSTACK_CHARTS_DIR}/node-feature-discovery-0.18.3.tgz"

## CSI Driver NFS
ADD https://raw.githubusercontent.com/kubernetes-csi/csi-driver-nfs/refs/heads/master/charts/v4.13.2/csi-driver-nfs-4.13.2.tgz "${GPUSTACK_CHARTS_DIR}/csi-driver-nfs-4.13.2.tgz"

## CSI Driver S3
ADD https://thxcode.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz "${GPUSTACK_CHARTS_DIR}/csi-driver-s3-0.43.7.tgz"

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
