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

FROM ubuntu:24.04@sha256:c4a8d5503dfb2a3eb8ab5f807da5bc69a85730fb49b5cfca2330194ebcc41c7b AS gpustack
SHELL ["/bin/bash", "-c"]

ARG TARGETPLATFORM
ARG TARGETOS
ARG TARGETARCH

## Install Tools

ENV DEBIAN_FRONTEND=noninteractive \
    LANG='en_US.UTF-8' \
    LANGUAGE='en_US:en' \
    LC_ALL='en_US.UTF-8'

RUN <<EOF
    # Tools
    set -exo pipefail

    # Refresh
    apt-get update -y && apt-get install -y --no-install-recommends \
        software-properties-common apt-transport-https \
        ca-certificates gnupg2 lsb-release gnupg-agent \
      && apt-get update -y

    # Install
    apt-get install -y --no-install-recommends \
        build-essential binutils bash openssl \
        curl wget aria2 \
        git git-lfs \
        unzip xz-utils \
        tzdata locales \
        iproute2 iputils-ping ifstat net-tools dnsutils pciutils ipmitool \
        procps sysstat glances htop numactl \
        vim jq bc tree gosu tini \
        logrotate cron netcat-openbsd \
        make ninja-build pkg-config cmake file

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

# Install Go

ARG GO_VERSION
ARG GODOWNLOAD="https://go.dev/dl/"
ARG GOPROXY="https://proxy.golang.org,direct"
ARG GOCACHE="/root/.cache/go-build"
ARG GOMODCACHE="/go/pkg/mod"
ARG GOROOT="/usr/local/go"

ENV PATH="/usr/local/go/bin:${PATH}" \
    GOPROXY="${GOPROXY}" \
    GOCACHE="${GOCACHE}" \
    GOMODCACHE="${GOMODCACHE}" \
    GOROOT="${GOROOT}"

RUN <<EOF
    # Go
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/go.tar.gz "${GODOWNLOAD}/go${GO_VERSION}.${TARGETOS}-${TARGETARCH}.tar.gz"

    # Install
    tar -C /usr/local -xzf /tmp/go.tar.gz --no-same-owner

    # Review
    go version && go env

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && rm -rf /var/cache/apt
EOF

# Install Skopeo

ARG SKOPEO_VERSION

RUN --mount=type=cache,target=${GOCACHE} \
    --mount=type=cache,target=${GOMODCACHE} <<EOF
    # Skopeo
    set -exo pipefail

    # Install dependencies
    apt-get update -y && apt-get install -y --no-install-recommends \
        libgpgme-dev libassuan-dev libbtrfs-dev

    # Download
    git -C /tmp clone --recursive --shallow-submodules \
        --depth 1 --branch v${SKOPEO_VERSION} --single-branch \
        https://github.com/containers/skopeo.git skopeo

    # Build and install
    pushd /tmp/skopeo \
        && sed -i "/export GOPROXY=.*/d" Makefile \
        && make vendor \
        && DISABLE_DOCS=1 PREFIX=/usr make install-binary

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

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Install Helm

ARG HELM_VERSION

RUN <<EOF
    # Helm
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/helm.tar.gz "https://get.helm.sh/helm-v${HELM_VERSION}-${TARGETOS}-${TARGETARCH}.tar.gz"

    # Install
    tar -xzf /tmp/helm.tar.gz --directory /usr/bin/ ${TARGETOS}-${TARGETARCH}/helm --no-same-owner --strip-components=1
    chmod a+x /usr/bin/helm

    # Review
    helm version

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Install vals

ARG VALS_VERSION

RUN <<EOF
    # vals
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/vals.tar.gz "https://github.com/helmfile/vals/releases/download/v${VALS_VERSION}/vals_${VALS_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz"

    # Install
    tar -xzf /tmp/vals.tar.gz --directory /usr/bin/ vals --no-same-owner
    chmod a+x /usr/bin/vals

    # Review
    vals version

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Install Kustomize

ARG KUSTOMIZE_VERSION

RUN <<EOF
    # Kustomize
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/kustomize.tar.gz "https://github.com/kubernetes-sigs/kustomize/releases/download/kustomize%2Fv${KUSTOMIZE_VERSION}/kustomize_v${KUSTOMIZE_VERSION}_${TARGETOS}_${TARGETARCH}.tar.gz"

    # Install
    tar -xzf /tmp/kustomize.tar.gz --directory /usr/bin/ kustomize --no-same-owner
    chmod a+x /usr/bin/kustomize

    # Review
    kustomize version

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Install ytt

ARG YTT_VERSION

RUN <<EOF
    # ytt
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/ytt "https://github.com/carvel-dev/ytt/releases/download/v${YTT_VERSION}/ytt-${TARGETOS}-${TARGETARCH}"

    # Install
    mv /tmp/ytt /usr/bin/ytt
    chmod a+x /usr/bin/ytt

    # Review
    ytt version

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Install Kubectl

ARG KUBECTL_VERSION

RUN <<EOF
    # Kubectl
    set -exo pipefail

    # Download
    aria2c -x $(nproc) -s $(nproc) -c -o /tmp/kubectl "https://dl.k8s.io/release/v${KUBECTL_VERSION}/bin/${TARGETOS}/${TARGETARCH}/kubectl"

    # Install
    mv /tmp/kubectl /usr/bin/kubectl
    chmod a+x /usr/bin/kubectl

    # Review
    kubectl version --client

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
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
ADD https://yandex-cloud.github.io/k8s-csi-s3/charts/csi-s3-0.43.7.tgz "${GPUSTACK_CHARTS_DIR}/csi-driver-s3-0.43.7.tgz"

# Prepare

## Build Context

RUN <<EOF
    # Build Context
    set -exo pipefail

    # Refresh
    apt-get update -y && apt-get install -y --no-install-recommends \
        libdrm-dev

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*

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

    # Try to update PCI IDs
    if ! update-pciids; then
        curl -o /usr/share/misc/pci.ids https://pci-ids.ucw.cz/v2.2/pci.ids || true
    fi

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

## AMD
## -- Tell container runtime to inject all devices.
ENV AMD_VISIBLE_DEVICES="all"
## Ascend
## -- Configure paths.
ENV ASCEND_HOME_PATH="/usr/local/Ascend/ascend-toolkit/latest" \
    LD_LIBRARY_PATH="/usr/local/dcmi:/usr/local/Ascend/driver/lib64/common:/usr/local/Ascend/driver/lib64/driver:/usr/local/Ascend/ascend-toolkit/latest/runtime/lib64:${LD_LIBRARY_PATH}"
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

# Build

ARG GPUSTACK_WORKSPACE=${GOROOT}/src/gpustack.ai/gpustack
ARG BUILDPLATFORM

RUN --mount=type=cache,target=${GOCACHE} \
    --mount=type=cache,target=${GOMODCACHE} \
    --mount=type=bind,target=${GPUSTACK_WORKSPACE},rw \
    --mount=type=cache,target=${GPUSTACK_WORKSPACE}/.sbin <<EOF
    # Build
    set -exo pipefail

    # Link staging
    for dir in $(find ${GPUSTACK_WORKSPACE}/staging -mindepth 1 -maxdepth 1 -type d); do
        target="${GOROOT}/src/$(basename ${dir})"
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
        && cp ${GPUSTACK_WORKSPACE}/.dist/build/gpustack /usr/bin/gpustack

    # Review
    gpustack --version

    # Cleanup
    rm -rf /var/tmp/* \
        && rm -rf /tmp/* \
        && apt-get clean \
        && rm -rf /var/lib/apt/lists/*
EOF

# Entrypoint

ARG GPUSTACK_DATA_DIR

ENV _RUNNING_INSIDE_CONTAINER_="true"

VOLUME ${GPUSTACK_DATA_DIR}
WORKDIR /
EXPOSE 80 443
ENTRYPOINT [ "tini", "--" ]
