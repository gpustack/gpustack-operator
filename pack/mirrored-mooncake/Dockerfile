# Mooncake Store, rebuilt with the leadership backends turned on.
#
# WHAT: one image carrying both of the store's entrypoints, from one compile of one source tree.
# `mooncake_master` runs a KVCacheBackend's leader; `mc_store_rest_server` runs a member. Both are
# console scripts of the same wheel, which is how the upstream published image is shaped too, and is
# why this operator resolves a single image for both roles.
#
# WHY this image exists: the published kvcacheai/mooncake images carry NO leadership backend at all.
# Availability is a compile-time switch (ValidateHABackendAvailability in ha/ha_types.h is a plain
# #ifdef dispatch) and all three options default to OFF, so on a published image every one of etcd,
# redis and k8s answers UNAVAILABLE_IN_CURRENT_MODE and the store cannot run more than one master.
# Nothing in this operator can lift that; only a rebuild can.
#
# REQUIRED: under high availability BOTH roles need this image, not just the leader. An earlier
# version of this comment said the rebuild was the leader's alone, on the reasoning that a member
# runs mc_store_rest_server and never elects. That is true of electing and false of FINDING the
# leader: this operator hands a member `MOONCAKE_MASTER=k8s://<namespace>/<lease>`, and the client
# validates that scheme against the same compile-time switch the master does. Measured on the
# published image, whose client answers `Invalid HA backend entry` and exits after twenty retries.
#
# LIMITED: the DEFAULT build of this file covers medium DRAM over protocol Auto, TCP or EFA.
# The vendor variants are separate build targets of this same file (cuda, cann and
# rocm, defined between the default builder and the final stage below): each recompiles the
# store on the vendor's own base with the vendor runtime linked in, and each keeps the lease
# backend on, because the vendor axis and the leadership axis are orthogonal. Whether
# rebuilding per vendor is the right answer, or whether members should keep the leader Service
# address and let readiness move the endpoint, is open:
# https://github.com/gpustack/gpustack-operator/issues/279
#
# EFA sits on the fabric axis, not the vendor axis: it needs no vendor runtime, only libfabric,
# which is why it is compiled into this one image rather than a variant. The libfabric story is
# two-layered by design. Upstream links `fabric` PUBLIC into transfer_engine, so master, engine.so
# and store.so all carry a DT_NEEDED on libfabric.so.1, and upstream's build_wheel.sh excludes
# libfabric from the repaired wheel unconditionally -- so the runtime stage installs the distro
# libfabric1 as the load-time fallback that keeps TCP members starting anywhere. The second layer
# is AWS's own libfabric, installed into this image beside it and ranked ahead of it in the loader
# cache, so an EFA member loads a build that can drive the device while every other protocol keeps
# starting on a node that has no EFA at all. The node contributes only what a node can: the efa
# kernel module and the device under /dev/infiniband.
#
# FORBIDDEN: adding STORE_USE_ETCD. The upstream CMakeLists raises FATAL_ERROR when it is combined
# with STORE_USE_K8S_LEASE, because both backends build a Go c-shared library. That exclusion is the
# reason this image has no oplog: master.cpp refuses to start with `enable_oplog` unless the backend
# is etcd, so oplog following is given up permanently here. A standby rebuilds from snapshot
# bootstrap and remounts instead, which is independent of the backend type. For a cache, that trades
# replay latency, not durability. The exclusion arrived in 0.3.11: it does not exist in 0.3.10,
# which is why the smoke test's etcd control is version-conditional below.
#
# VERSIONS: MOONCAKE_VERSION selects the release, and the version-dependent steps below each say
# which releases they apply to and why. The matrix they implement, read out of the upstream tags:
#
#   - 0.3.10: no k8s-lease backend at all, so an image of it serves a single-replica leader with
#     election off (the shape leaderNeedsAPIAccess in pkg/worker/kvcache/mooncake/ha_rbac.go
#     renders for it). It is built because some vLLM releases bundle a 0.3.10 Mooncake client,
#     and a store that version-mismatched with its engines fails their registration handshake.
#   - 0.3.11: the backend exists but does not compile as shipped -- the top-level
#     CMakeLists.txt never puts mooncake-common/k8s-lease on the include path, so the build
#     patches that in first. build_wheel.sh then neither packages the wrapper .so nor
#     excludes libfabric from the repaired wheel, and mc_store_rest_server is still the broken
#     async entry point. The build patches all four below.
#   - 0.3.12: packaging is whole -- wrapper copied, libfabric excluded, sync_main entry point.
#   - 0.3.13 (the default): as 0.3.12.
#
# Build evidence exists for 0.3.10.post2 and 0.3.13.post1; 0.3.11 and 0.3.12 follow from reading
# the tags, and their first build is the check.

# REQUIRED: both stages carry the SAME Python and the SAME Debian release. The wheel's extension
# modules are built WITH_SOABI, so they are tagged for one interpreter version and will not import
# under another, and the binary inside the wheel is linked against the builder's glibc.
#
# Building the wheel here rather than installing one from PyPI is also what sidesteps the manylinux
# constraint upstream's own image documents: the published aarch64 wheel is manylinux_2_39 and needs
# a glibc bookworm does not have. A wheel built on bookworm needs bookworm.
ARG BUILDER_IMAGE="python:3.12-bookworm"
ARG RUNTIME_IMAGE="python:3.12-slim-bookworm"

# The vendor variant bases, one builder/runtime pair per variant target, following the base
# choices of the gpustack/runner project (github.com/gpustack/runner, pack/{cuda,cann,rocm}),
# which are also the bases upstream Mooncake's own vendor release jobs build on.
#
# REQUIRED: the targets stay vendor-generic -- cuda, cann, rocm -- because every one of these
# ARGs is overridable at dispatch time through the base-image workflow's `args` input (plumbed
# to docker build as build-args). Repointing a variant at another toolchain release, say a
# CUDA 12.9 base, is then a dispatch decision that touches no file here, and the toolchain
# version lives only in the dispatch-time image tag (e.g. 0.3.13.post1-cuda), never in a
# target or stage name.
#
#   - cuda: upstream's own CUDA image (docker/master-cuda13.Dockerfile) takes its CUDA
#     libraries from this exact devel tag, so the toolchain and the stub libcuda the runtime
#     stage copies out of it are the ones upstream ships against. Runner's cudnn flavor is not
#     needed: nothing in a store build links cuDNN.
#   - cann: runner's CANN base pattern, quay.io/ascend/cann:<version>-<arch>-ubuntu22.04-py<python>.
#     The image exports ASCEND_HOME_PATH, which is exactly what common.cmake reads to locate the
#     toolkit, and the "-devel" half carries the headers the compile needs while the plain half
#     carries the same runtime libraries for the final stage. The py3.12 flavor keeps ONE
#     interpreter across both stages, which the wheel's SOABI tag requires. The default base
#     targets the 910b SoC family; another Ascend family is a dispatch-time override of the
#     CANN_*_IMAGE ARGs, the same vendor-generic contract the other two vendors keep.
#   - rocm: the builder is the container upstream Mooncake's own ROCm release job builds in.
#     The runtime is that image's non-complete sibling, roughly a seventh of the size, which
#     still carries the ROCm core runtime the HIP build links against (libamdhip64,
#     libhsa-runtime64 -- the runtime stage below gates exactly those two). The rocm/dev
#     images publish linux/amd64 only, which is why the base-image workflow builds this
#     target for amd64 alone.
ARG CUDA_BUILDER_IMAGE="nvidia/cuda:13.0.3-devel-ubuntu22.04"
ARG CUDA_RUNTIME_IMAGE="nvidia/cuda:13.0.3-runtime-ubuntu22.04"
ARG CANN_BUILDER_IMAGE="quay.io/ascend/cann:9.1.1-910b-ubuntu22.04-py3.12-devel"
ARG CANN_RUNTIME_IMAGE="quay.io/ascend/cann:9.1.1-910b-ubuntu22.04-py3.12"
ARG ROCM_BUILDER_IMAGE="rocm/dev-ubuntu-22.04:7.2.3-complete"
ARG ROCM_RUNTIME_IMAGE="rocm/dev-ubuntu-22.04:7.2.3"

# The upstream release this image is built from, spelled the way upstream's own images spell it: the
# PyPI version, without the tag's leading "v". The clone adds that back, because the git tag and the
# wheel version are the same release under two naming conventions and carrying both as build
# arguments is how they drift apart.
#
# It is asserted after the build rather than trusted -- the smoke test below reads the version out of
# the INSTALLED package, so a tag that moved to a different release fails the build instead of
# producing an image whose name and contents disagree.
ARG MOONCAKE_VERSION="0.3.13.post1"

# The AWS EFA installer release whose libfabric the runtime stage installs. AWS also serves an
# "-latest" name, which is what this must NOT use: the userspace the image carries has to stay a
# property of this Dockerfile rather than of the day it was built, and a userspace that moves
# underneath a fixed MOONCAKE_VERSION is the shape where a build that passes and a build that fails
# have the same inputs on paper.
#
# Integrity rests on HTTPS to AWS's own host, and the version is the only thing pinned. Neither of
# the two stronger options pays for itself here. AWS publishes no checksum beside the archive, so a
# digest would have to be self-taken -- it would prove the bytes never changed, not who made them --
# and it is a second value to re-read on every bump. The installer's own --check-signatures reads its
# GPG key from EFA_INSTALLER_KEY, and that key ships neither in the archive nor with a published
# fingerprint, so fetching it from the same host the archive came from checks the archive against
# itself.
ARG EFA_INSTALLER_VERSION="1.50.0"

# =============================================================================
# Stage: builder
# Compile the pinned source once, then package that same build tree as a wheel.
# =============================================================================
FROM ${BUILDER_IMAGE} AS builder
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# dependencies.sh installs several hundred packages in one apt transaction, which is long enough
# that a single dropped connection fails the whole build after it has already fetched most of a
# couple of hundred megabytes. apt does not retry by default. The settings land in apt.conf.d rather
# than on a command line because the transaction that needs them is inside dependencies.sh, which
# takes no apt arguments.
RUN <<EOF
    set -exo pipefail
    cat >/etc/apt/apt.conf.d/99-mooncake-retries <<'CONF'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
CONF
EOF

# FORBIDDEN: a base image that ships Go. Upstream's dependencies.sh installs its own toolchain at
# /usr/local/go, which is exactly where golang:* images put theirs. The two collide and the
# collision does not surface at install time -- it surfaces much later as a link failure in the Go
# c-shared wrapper, which reads like a source problem. A python:* image has no Go, which is one more
# reason it is the base here.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        libfabric-dev \
        sudo \
        wget
    rm -rf /var/lib/apt/lists/*
EOF

RUN <<EOF
    set -exo pipefail

    # Assert the release before anything consumes it: every version-conditional block below is
    # written against 0.3.10 through 0.3.13, so a dispatch of a newer release must fail here
    # rather than take branches that were never reviewed against it.
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*|0.3.12*|0.3.13*) ;;
        *)
            echo "[ERROR] unrecognized release '${MOONCAKE_VERSION}': the version-conditional blocks know 0.3.10 through 0.3.13; audit them before dispatching a newer one." >&2
            exit 1
            ;;
    esac

    # Submodules are included because upstream carries extern/pybind11 and the Python integration
    # this build now needs is one of its consumers. A CMake configure that reaches an empty
    # submodule directory fails during configuration rather than at the point of use.
    git clone --depth 1 --branch "v${MOONCAKE_VERSION}" --recurse-submodules --shallow-submodules \
        https://github.com/kvcache-ai/Mooncake.git /work
    cd /work

    # Recorded, not asserted: the commit this release resolved to, printed into the build log so a
    # rebuilt image can be traced back to a tree. What the tag NAMES is checked at the other end,
    # against the version the installed package reports.
    echo "[info] v${MOONCAKE_VERSION} resolves to $(git rev-parse HEAD)"
EOF

WORKDIR /work

# dependencies.sh owns the whole toolchain, including Go and libhiredis-dev. Hand-listing apt
# packages instead is a losing game -- the attempts that preceded this died on yaml-cpp, then on
# zstd, each one a separate ten-minute round trip.
#
# The retry loop is inside ONE RUN on purpose. dependencies.sh pulls roughly 200 MB in a single apt
# transaction, and a connection dropped anywhere in it fails the whole thing; what makes retrying
# worth anything is that apt leaves what it already fetched in /var/cache/apt/archives and
# dependencies.sh never cleans it, so each attempt starts closer to done. A layer boundary between
# attempts would throw that cache away and every attempt would restart from zero.
RUN <<EOF
    set -eo pipefail

    installed=0
    for attempt in 1 2 3 4 5 6; do
        if bash dependencies.sh -y; then
            installed=1
            break
        fi
        echo "[warn] dependencies.sh attempt ${attempt} failed; retrying over the apt cache" >&2
        sleep 10
    done

    if [[ "${installed}" != "1" ]]; then
        echo "[ERROR] dependencies.sh did not succeed in 6 attempts." >&2
        exit 1
    fi
EOF

ENV PATH=/usr/local/go/bin:$PATH

# One configure, one build, and the whole project rather than a single target: the wheel is packaged
# out of this build tree, and it takes the Python extensions (engine.so, store.so) as well as the
# master binary.
#
# The flag set is not free choice -- three of these are pinned by what packaging does afterwards:
#
#   - BUILD_EXAMPLES stays ON. scripts/build_wheel.sh copies transfer_engine_bench with no guard
#     around it, so a build without examples fails during packaging rather than during compilation,
#     which is the expensive place to find out.
#   - WITH_STORE_RUST goes OFF. It defaults ON and would need a Rust toolchain that dependencies.sh
#     does not install; nothing the wheel ships comes from it.
#   - BUILD_DIR for the packaging script defaults to "build" RELATIVE to the source root, so the
#     build tree is /work/build rather than somewhere outside it.
#
# USE_CUDA and WITH_EP are off because this stage builds the DRAM variant, which is the default
# image: no vendor runtime belongs in it. The vendor variants are separate build targets below,
# each with its own base and flag set. Turning CUDA on here instead would pull a large toolchain
# into every default build and leave a DT_NEEDED on libcuda that the default runtime stage has
# no way and no reason to satisfy.
#
# USE_EFA is on with USE_CUDA off, which upstream supports as a first-class combination (it is how
# their mooncake-transfer-engine-efa-non-cuda wheel is built): EFA moves DRAM over libfabric's SRD
# provider, no CUDA anywhere in the path. Compiling it in needs only libfabric headers at build
# time -- common.cmake's find_path/find_library fall back to the system paths, so the distro
# libfabric-dev installed above satisfies it and no AWS EFA installer runs in this stage. What it
# costs every consumer of this image, TCP included, is a DT_NEEDED on libfabric.so.1: `fabric` is
# linked PUBLIC into transfer_engine, and build_wheel.sh excludes libfabric from the repaired
# wheel, so the runtime stage installs libfabric1 to close that.
RUN <<EOF
    set -exo pipefail

    # 0.3.11 ships the k8s-lease backend but does not compile it: its top-level CMakeLists.txt
    # puts mooncake-common/etcd on the include path and never mooncake-common/k8s-lease, so
    # k8s_lease_helper.cpp cannot resolve libk8s_lease_wrapper.h, which the Go build copies into
    # that source directory. Upstream never noticed because nothing in 0.3.11's own packaging
    # turns STORE_USE_K8S_LEASE on. 0.3.12 fixes it with exactly the line inserted here; 0.3.10
    # predates the backend and has no such line to add. The grep afterwards fails the build on a
    # silent no-op, because a sed that matched nothing would surface as the same missing-header
    # error twenty minutes later.
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            sed -i 's|^include_directories(mooncake-common/etcd)$|include_directories(mooncake-common/etcd)\ninclude_directories(mooncake-common/k8s-lease)|' \
                /work/CMakeLists.txt
            grep -q '^include_directories(mooncake-common/k8s-lease)$' /work/CMakeLists.txt
            ;;
    esac

    cmake -S /work -B /work/build -G Ninja \
        -DSTORE_USE_K8S_LEASE=ON \
        -DSTORE_USE_REDIS=ON \
        -DUSE_CUDA=OFF \
        -DUSE_EFA=ON \
        -DWITH_EP=OFF \
        -DWITH_STORE_RUST=OFF \
        -DUSE_HTTP=ON \
        -DBUILD_UNIT_TESTS=OFF \
        -DBUILD_EXAMPLES=ON \
        -DCMAKE_BUILD_TYPE=Release

    # REQUIRED: bound the parallelism by MEMORY, not by core count. Measured: -j16 on a builder with
    # 31 GB is killed partway through, `c++: fatal error: Killed signal terminated program cc1plus`,
    # because the pybind translation units in this project each want a couple of gigabytes and
    # nothing about the core count knows that. Three gigabytes per job is what fits them.
    #
    # The failure this avoids reads as a compiler crash rather than as an out-of-memory, which is
    # why the reason is here and not left for the next person to rediscover on a 20-minute build.
    mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
    jobs=$(( mem_kb / (3 * 1024 * 1024) ))
    if [[ "${jobs}" -lt 1 ]]; then jobs=1; fi
    if [[ "${jobs}" -gt "$(nproc)" ]]; then jobs="$(nproc)"; fi
    echo "[info] building with -j${jobs} ($(nproc) cores, ${mem_kb} kB memory)"

    cmake --build /work/build -j"${jobs}"
EOF

# Package the build tree just compiled. The script installs its own pip tooling (build, auditwheel,
# patchelf) and repairs the wheel, vendoring what the extensions resolve outside the base.
#
# REQUIRED: it must see THIS build tree. It reads ${BUILD_DIR} relative to the current directory,
# and the master binary it copies into the wheel is /work/build/mooncake-store/src/mooncake_master --
# the one compiled with the leadership backends above. Pointing it elsewhere, or installing the
# published wheel beside it, yields an image whose two copies of the binary differ in exactly the
# switch this whole image exists for.
#
# patchelf goes in UP FRONT, unconditionally: 0.3.10's build_wheel.sh does not install it from PyPI
# (0.3.13's does), and bookworm's distro patchelf 0.14.3 fails auditwheel >= 6.8. For a release
# whose script installs its own, this is a redundant upgrade of the same package and costs one
# small download.
RUN pip install --no-cache-dir -U patchelf

RUN <<EOF
    set -exo pipefail

    # REQUIRED: PYTHON_VERSION is passed in the ENVIRONMENT, not as the positional argument the
    # script documents. The script reads ${PYTHON_VERSION:-${1:-...}}, and the official python image
    # exports PYTHON_VERSION=3.12.14 of its own -- so the environment wins, the script forms the
    # interpreter name `python3.12.14`, and the build dies at "command not found" after the whole
    # compile has already succeeded. Two-component here because that is what the binary is called.
    #
    # DERIVED from the interpreter that is about to compile the extensions rather than written out,
    # so that bumping BUILDER_IMAGE cannot leave it behind. A literal that no longer matches tags
    # the wheel for another interpreter, and that failure does not appear here -- it appears in the
    # runtime stage as an import error, or as a silently incompatible install.
    py_version="$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"

    # 0.3.11's build_wheel.sh puts ${BUILD_DIR}/mooncake-common on LD_LIBRARY_PATH but not its
    # k8s-lease subdirectory (0.3.12's does), so auditwheel cannot locate libk8s_lease_wrapper.so,
    # which the store extension links against, and the repair dies with "required library could
    # not be located". Pointing the variable at the build output lets auditwheel vendor the
    # wrapper into the wheel's .libs; the runtime stage's manual copy for this release stays,
    # because the wrapper assertion below reads that path.
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:/work/build/mooncake-common/k8s-lease"
            ;;
    esac

    PYTHON_VERSION="${py_version}" BUILD_DIR=build bash scripts/build_wheel.sh

    # One wheel, named. A glob that silently matched two would install whichever pip preferred.
    count="$(find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' | wc -l)"
    if [[ "${count}" != "1" ]]; then
        echo "[ERROR] expected exactly one wheel, found ${count}." >&2
        find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' >&2
        exit 1
    fi

    # The lease backend's Go half, staged where the runtime stage can mount it. 0.3.11 COMPILES
    # the backend but its build_wheel.sh does not copy the wrapper into the wheel (that arrived
    # in 0.3.12; 0.3.10 has no backend at all), so the runtime stage installs it by hand for
    # exactly that release. The directory is created even when nothing goes in it, because the
    # runtime stage mounts it unconditionally and a missing mount source fails the build.
    mkdir -p /work/lease-out
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            wrapper="/work/build/mooncake-common/k8s-lease/libk8s_lease_wrapper.so"
            test -f "${wrapper}"
            cp "${wrapper}" /work/lease-out/
            ;;
    esac

    # The wrapper is asserted here, beside the packaging that stages it, not only in the
    # runtime stage's smoke test: a release that stops producing the file, or a build_wheel.sh
    # that stops copying it into the wheel tree, fails next to the step responsible.
    case "${MOONCAKE_VERSION}" in
        0.3.10*) echo "[info] 0.3.10 has no k8s-lease backend; no wrapper to stage." ;;
        0.3.11*) test -f /work/lease-out/libk8s_lease_wrapper.so ;;
        *) test -f /work/mooncake-wheel/mooncake/libk8s_lease_wrapper.so ;;
    esac
EOF

# =============================================================================
# Target: cuda
# The NVIDIA variant: the store compiled with USE_CUDA on the CUDA 13.0 toolchain, plus
# USE_VRAM_SEGMENT where the release offers it, for member groups whose segments live in
# device memory. Dispatch with --target cuda; the default build never enters these stages
# because the final stage stays mooncake-store.
#
# The variant stage pairs repeat the default builder's blocks on purpose -- one Dockerfile,
# per-variant stage pairs, no shared scripts. The repeated blocks MUST be edited in lockstep
# across all four targets (default, cuda, cann, rocm): the dependencies.sh retry loop, the
# 0.3.11 CMakeLists patch, the cmake job calculation, the wheel packaging and one-wheel
# assertion, the patchelf repair, the k8s-lease wrapper staging and assertion, and the two
# smoke-test halves.
# =============================================================================
FROM ${CUDA_BUILDER_IMAGE} AS builder-cuda
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# The apt retry configuration is the default builder's, kept for the same reason:
# dependencies.sh runs its apt transaction inside this stage.
RUN <<EOF
    set -exo pipefail
    cat >/etc/apt/apt.conf.d/99-mooncake-retries <<'CONF'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
CONF
EOF

# python3, python3-dev and python3-pip join the default builder's package list because this
# base ships no Python at all. libfabric-dev is absent because the variant compiles with
# USE_EFA=OFF, so nothing in the stage needs libfabric headers.
#
# build_wheel.sh probes `command -v pip`, which ubuntu's python3-pip does not provide (it
# ships pip3 only); upgrading pip through itself installs the pip entrypoint.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        python3 \
        python3-dev \
        python3-pip \
        sudo \
        wget
    rm -rf /var/lib/apt/lists/*
    python3 -m pip install --no-cache-dir -U pip
EOF

# The clone is the default builder's, submodules included for the same reason.
RUN <<EOF
    set -exo pipefail

    # Assert the release before anything consumes it: every version-conditional block below is
    # written against 0.3.10 through 0.3.13, so a dispatch of a newer release must fail here
    # rather than take branches that were never reviewed against it.
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*|0.3.12*|0.3.13*) ;;
        *)
            echo "[ERROR] unrecognized release '${MOONCAKE_VERSION}': the version-conditional blocks know 0.3.10 through 0.3.13; audit them before dispatching a newer one." >&2
            exit 1
            ;;
    esac

    git clone --depth 1 --branch "v${MOONCAKE_VERSION}" --recurse-submodules --shallow-submodules \
        https://github.com/kvcache-ai/Mooncake.git /work
    cd /work
    echo "[info] v${MOONCAKE_VERSION} resolves to $(git rev-parse HEAD)"
EOF

WORKDIR /work

# dependencies.sh owns the toolchain here exactly as in the default builder, retry loop and
# reason included.
RUN <<EOF
    set -eo pipefail

    installed=0
    for attempt in 1 2 3 4 5 6; do
        if bash dependencies.sh -y; then
            installed=1
            break
        fi
        echo "[warn] dependencies.sh attempt ${attempt} failed; retrying over the apt cache" >&2
        sleep 10
    done

    if [[ "${installed}" != "1" ]]; then
        echo "[ERROR] dependencies.sh did not succeed in 6 attempts." >&2
        exit 1
    fi
EOF

ENV PATH=/usr/local/go/bin:$PATH

# The flag set is the default builder's with the vendor axis swapped in:
#
#   - USE_CUDA goes ON. It is what the variant exists for: GPU buffers in the transfer engine,
#     and the prerequisite of USE_VRAM_SEGMENT.
#   - USE_VRAM_SEGMENT is what lands a member's segment in device memory (allocation switches
#     to cudaMalloc under the ifdef), and it forces USE_CUDA on upstream. It exists only from
#     0.3.13: the 0.3.10 line has neither the option nor the allocation path, so there the
#     flag is omitted and the build is the CUDA transfer engine without VRAM segments. The
#     decision reads the source rather than the version string, so a backport cannot go
#     silently unflagged; passing the flag unconditionally would let cmake swallow the unknown
#     -D with a warning and ship a DRAM-segment build under a VRAM name. When the flag IS
#     passed, the grep on the configure log proves cmake honored it.
#   - USE_EFA goes OFF. EFA is compiled into the default image only: an EFA member runs a DRAM
#     segment, which the default image already serves, and EFA plus VRAM is a combination
#     upstream does not test. With it off, nothing in the variant carries a DT_NEEDED on
#     libfabric, which is why the runtime stage below installs neither libfabric1 nor the AWS
#     installer.
#
# STORE_USE_K8S_LEASE stays ON, as it does in the default build: the vendor axis and the
# leadership axis are orthogonal. On the 0.3.10 line the option does not exist and cmake
# ignores it with a warning -- the backend arrived in 0.3.11 -- and the runtime stage's smoke
# test asserts exactly that inversion from the built artifact, the same contract the default
# image keeps.
RUN <<EOF
    set -exo pipefail

    # 0.3.11 needs the same include_directories patch as the default builder; the reason and
    # the failure mode are documented there.
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            sed -i 's|^include_directories(mooncake-common/etcd)$|include_directories(mooncake-common/etcd)\ninclude_directories(mooncake-common/k8s-lease)|' \
                /work/CMakeLists.txt
            grep -q '^include_directories(mooncake-common/k8s-lease)$' /work/CMakeLists.txt
            ;;
    esac

    vram_flag=""
    if grep -q '^option(USE_VRAM_SEGMENT ' /work/mooncake-common/common.cmake; then
        vram_flag="-DUSE_VRAM_SEGMENT=ON"
    else
        echo "[info] this release predates USE_VRAM_SEGMENT (added in 0.3.13): building the CUDA transfer engine without VRAM segments."
    fi

    cmake -S /work -B /work/build -G Ninja \
        -DSTORE_USE_K8S_LEASE=ON \
        -DSTORE_USE_REDIS=ON \
        -DUSE_CUDA=ON \
        ${vram_flag} \
        -DUSE_EFA=OFF \
        -DWITH_EP=OFF \
        -DWITH_STORE_RUST=OFF \
        -DUSE_HTTP=ON \
        -DBUILD_UNIT_TESTS=OFF \
        -DBUILD_EXAMPLES=ON \
        -DCMAKE_BUILD_TYPE=Release 2>&1 | tee /tmp/configure.log
    if [[ -n "${vram_flag}" ]]; then
        grep -q "VRAM SEGMENT is ON" /tmp/configure.log
    fi
    rm -f /tmp/configure.log

    # The memory-bounded parallelism is the default builder's mechanism, kept for the same
    # measured reason.
    mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
    jobs=$(( mem_kb / (3 * 1024 * 1024) ))
    if [[ "${jobs}" -lt 1 ]]; then jobs=1; fi
    if [[ "${jobs}" -gt "$(nproc)" ]]; then jobs="$(nproc)"; fi
    echo "[info] building with -j${jobs} ($(nproc) cores, ${mem_kb} kB memory)"

    cmake --build /work/build -j"${jobs}"
EOF

# patchelf goes in up front for the same reason as in the default builder.
RUN pip install --no-cache-dir -U patchelf

# The packaging step is the default builder's: PYTHON_VERSION passed in the environment for the
# same reason, the same 0.3.11 auditwheel workaround, the same one-wheel assertion, and the
# same lease-out staging for 0.3.11.
RUN <<EOF
    set -exo pipefail

    py_version="$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:/work/build/mooncake-common/k8s-lease"
            ;;
    esac

    PYTHON_VERSION="${py_version}" BUILD_DIR=build bash scripts/build_wheel.sh

    count="$(find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' | wc -l)"
    if [[ "${count}" != "1" ]]; then
        echo "[ERROR] expected exactly one wheel, found ${count}." >&2
        find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' >&2
        exit 1
    fi

    mkdir -p /work/lease-out
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            wrapper="/work/build/mooncake-common/k8s-lease/libk8s_lease_wrapper.so"
            test -f "${wrapper}"
            cp "${wrapper}" /work/lease-out/
            ;;
    esac

    # The wrapper is asserted here, beside the packaging that stages it, not only in the
    # runtime stage's smoke test: a release that stops producing the file, or a build_wheel.sh
    # that stops copying it into the wheel tree, fails next to the step responsible.
    case "${MOONCAKE_VERSION}" in
        0.3.10*) echo "[info] 0.3.10 has no k8s-lease backend; no wrapper to stage." ;;
        0.3.11*) test -f /work/lease-out/libk8s_lease_wrapper.so ;;
        *) test -f /work/mooncake-wheel/mooncake/libk8s_lease_wrapper.so ;;
    esac
EOF

FROM ${CUDA_RUNTIME_IMAGE} AS cuda
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# The package list is the default runtime stage's minus libfabric1, plus the Python this base
# does not ship. libfabric1 and the AWS EFA installer are both absent because the variant
# compiles with USE_EFA=OFF: there is no DT_NEEDED on libfabric to satisfy, and the installer
# would be dead weight. What remains is what this build links against -- ibverbs included,
# because the master binary carries that DT_NEEDED on every build, fabric or no fabric.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        ibverbs-providers \
        libcurl4 \
        libibverbs1 \
        libnuma1 \
        python3 \
        python3-pip
    rm -rf /var/lib/apt/lists/*
    python3 -m pip install --no-cache-dir -U pip
EOF

# The CUDA build leaves a DT_NEEDED on libcuda.so.1 and libcudart.so.* on engine.so, store.so
# and the master binary, and build_wheel.sh's auditwheel excludes both from the repaired wheel,
# so the stage must resolve them itself. libcudart is the runtime base's own; the gate below
# asks only that SOME libcudart.so.* resolves, because the CUDA_* base ARGs are dispatch-time
# overridable and the soname's version suffix moves with the toolkit. libcuda is the driver
# library: the real one is injected by the container runtime on a GPU node, and what the image
# carries is the devel image's stub, exactly as upstream's own CUDA runtime image does.
#
# REQUIRED: the stub lives in its own directory, registered by an ld.so.conf.d file whose name
# sorts LAST. ldconfig scans the conf files in glob order and the loader takes the FIRST cache
# entry for a soname, so a stub under /usr/local/lib (libc.conf) would outrank the real driver
# the nvidia-container-toolkit mounts under /usr/lib/x86_64-linux-gnu
# (x86_64-linux-gnu.conf) -- measured: with the same soname in both slots, ldd resolves to
# whichever conf sorts earlier.
#
# The ordering holds under both known injection modes of the toolkit. In LD_LIBRARY_PATH mode
# the toolkit puts the driver directory in the process environment, which the loader searches
# BEFORE the ldconfig cache, so cache ordering is moot and the real driver wins outright. In
# ldconfig mode the toolkit registers the driver through /etc/ld.so.conf.d/nvidia.conf, which
# sorts before zz-*, so the cache's first entry for libcuda.so.1 is still the real one; no
# known toolkit version registers the driver in a conf that sorts after zz-. The zz- prefix
# keeps the contract this stage needs either way: the mounted real driver wins where one
# exists, the stub fills in everywhere else.
COPY --from=builder-cuda /usr/local/cuda/lib64/stubs/libcuda.so /usr/local/lib/mooncake-cuda-stub/libcuda.so.1
RUN <<EOF
    set -exo pipefail
    echo "/usr/local/lib/mooncake-cuda-stub" >/etc/ld.so.conf.d/zz-mooncake-cuda-stub.conf
    ldconfig
    cache="$(ldconfig -p)"
    grep -q "libcuda.so.1 " <<<"${cache}"
    grep -q "libcudart.so." <<<"${cache}"
EOF

RUN --mount=from=builder-cuda,source=/work/mooncake-wheel/dist,target=/wheel \
    --mount=from=builder-cuda,source=/work/lease-out,target=/lease <<EOF
    set -exo pipefail
    pip install --no-cache-dir /wheel/*.whl
    rm -rf /root/.cache/pip

    # The version-conditional repairs are the default stage's, unchanged: the 0.3.10/0.3.11
    # wheel repairs no-op here where nothing vendored libefa or libfabric (USE_EFA is off), and
    # the mc_store_rest_server shim is a version-axis fix, needed by every variant that builds
    # a release before 0.3.12.
    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*)
            pip install --no-cache-dir patchelf
            sp="$(dirname "${pkg}")"
            libs="${sp}/mooncake_transfer_engine.libs"
            for short in libefa libfabric; do
                for vendored in "${libs}/${short}"-*.so.*; do
                    [[ -e "${vendored}" ]] || continue
                    mangled="$(basename "${vendored}")"
                    find "${sp}" -type f | while read -r so; do
                        if patchelf --print-needed "${so}" 2>/dev/null | grep -qx "${mangled}"; then
                            patchelf --replace-needed "${mangled}" "${short}.so.1" "${so}"
                            echo "[info] ${so}: ${mangled} -> ${short}.so.1"
                        fi
                    done
                    rm -fv "${vendored}"
                done
            done
            pip uninstall -y patchelf

            shim="$(command -v mc_store_rest_server)"
            cat >"${shim}" <<'SHIM'
#!/bin/bash
exec python3 -m mooncake.mooncake_store_service "$@"
SHIM
            chmod +x "${shim}"
            ;;
    esac

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            cp /lease/libk8s_lease_wrapper.so "${pkg}/"
            ;;
    esac
EOF

# The smoke test is the default stage's, minus the EFA bench: this variant compiles without
# USE_EFA, so there is no EFA transport to prove. What stays is the entrypoint check, the
# version check, the lease wrapper assertion, the member entrypoint's import, and an ldd gate
# over the wheel's extensions and the master binary. The import alone cannot serve as the
# loader check here: a stub libcuda resolves just as well as a real driver, by design, so
# "import succeeds" says nothing about the non-driver dependencies -- libcudart above all.
# ldd prints "not found" for any soname the loader cannot resolve, and any such line fails
# the build, the same gate the cann variant runs for its dlopened transport.
RUN <<EOF
    set -exo pipefail

    command -v mooncake_master
    command -v mc_store_rest_server
    command -v python3

    installed="$(python3 -c 'import importlib.metadata as m; print(m.version("mooncake-transfer-engine"))')"
    if [[ "${installed}" != "${MOONCAKE_VERSION}" ]]; then
        echo "[ERROR] installed mooncake-transfer-engine is ${installed}, asked for ${MOONCAKE_VERSION}." >&2
        exit 1
    fi

    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            echo "[info] 0.3.10 has no k8s-lease backend; skipping the wrapper assertion."
            ;;
        *)
            test -f "${pkg}/libk8s_lease_wrapper.so"
            ;;
    esac

    mc_store_rest_server --help >/dev/null

    missing="$(ldd "${pkg}/engine.so" "${pkg}/store.so" "$(command -v mooncake_master)" | grep "not found" || true)"
    if [[ -n "${missing}" ]]; then
        echo "[ERROR] unresolved libraries in the wheel's extensions or mooncake_master:" >&2
        echo "${missing}" >&2
        exit 1
    fi
EOF

# Half two is the default stage's leadership probe set, unchanged: the same four cases, the
# same per-version polarity (0.3.10 inverts k8s and etcd, 0.3.11's etcd control CHECK-fails,
# later releases refuse etcd), and the same bogus-value control. The refusal is asserted
# absent with an if-guarded grep, never with `grep -qv` or `! grep -q`; the default stage's
# half two explains why both forms cannot fail.
RUN <<EOF
    set -exo pipefail

    function probe() {
        timeout -k 5 25 mooncake_master \
            -enable_ha=true \
            -ha_backend_type="$1" \
            -ha_backend_connstring="$2" \
            -rpc_port=50051 -metrics_port=9003 -logtostderr=1 \
            >"/tmp/$1.log" 2>&1 || true
        echo "--- $1 ---"
        cat "/tmp/$1.log"
    }

    probe k8s default/mooncake-master-lease
    probe redis 127.0.0.1:6379
    probe etcd http://127.0.0.1:2379
    probe bogus unused

    grep -q "Master runtime state" /tmp/k8s.log
    grep -q "Master runtime state" /tmp/redis.log
    if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/redis.log; then
        echo "[ERROR] redis metadata backend is unavailable in this build." >&2
        exit 1
    fi

    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log; then
                echo "[ERROR] etcd metadata backend is unavailable in this build." >&2
                exit 1
            fi
            ;;
        0.3.11*)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "Etcd is not enabled in compilation" /tmp/etcd.log
            ;;
        *)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log
            ;;
    esac

    grep -q "INVALID_PARAMS" /tmp/bogus.log

    rm -f /tmp/k8s.log /tmp/redis.log /tmp/etcd.log /tmp/bogus.log
EOF

# The leader ports are the default stage's; a member's are not declared for the same reason.
EXPOSE 50051 9003

CMD ["bash"]

# =============================================================================
# Target: cann
# The Ascend variant: the store compiled with one Ascend flavor on the CANN 9.1 toolkit, for
# member groups on Ascend NPU nodes. The flavor defaults to USE_ASCEND_DIRECT, upstream's
# recommended Ascend path -- it is what upstream's own Ascend CI and NPU release jobs
# configure -- and with it a segment on the ascend protocol allocates in device memory, the
# Ascend counterpart of the CUDA variant's VRAM segment; ASCEND_TRANSPORT=ubshmem builds the
# CANN VMM shared-memory flavor for the supernode domain instead. Dispatch with
# --target cann.
# =============================================================================
FROM ${CANN_BUILDER_IMAGE} AS builder-cann
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

# One build carries one Ascend flavor: upstream's CMake picks the ascend_transport sources on
# a mutually exclusive if/elseif chain, so the flavors cannot coexist in one image. The
# default is upstream's recommended path; ubshmem selects the CANN VMM shared-memory
# transport instead, which serves the supernode (UnifiedBus) shared-memory domain and needs
# CANN >= 9.0, driver >= 26.0 and Lingqu >= 1.5 at runtime. Dispatch it with
# args "ASCEND_TRANSPORT=ubshmem" under its own tag.
ARG ASCEND_TRANSPORT="ascend_direct"

ENV DEBIAN_FRONTEND=noninteractive

# The apt retry configuration is the default builder's, kept for the same reason.
RUN <<EOF
    set -exo pipefail
    cat >/etc/apt/apt.conf.d/99-mooncake-retries <<'CONF'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
CONF
EOF

# No python3-* packages: the base's interpreter is a python.org 3.12 under /usr/local, already
# first on PATH, and installing the distro python3 would add a second interpreter to the stage
# for nothing. libfabric-dev is absent because the variant compiles with USE_EFA=OFF. pip
# ships with the base's interpreter; ensurepip is the fallback, and the self-upgrade installs
# the `pip` entrypoint build_wheel.sh probes for.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        sudo \
        wget
    rm -rf /var/lib/apt/lists/*
    python3 -m pip --version || python3 -m ensurepip --upgrade
    python3 -m pip install --no-cache-dir -U pip
EOF

# The clone is the default builder's, submodules included for the same reason.
RUN <<EOF
    set -exo pipefail

    # Assert the release before anything consumes it: every version-conditional block below is
    # written against 0.3.10 through 0.3.13, so a dispatch of a newer release must fail here
    # rather than take branches that were never reviewed against it.
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*|0.3.12*|0.3.13*) ;;
        *)
            echo "[ERROR] unrecognized release '${MOONCAKE_VERSION}': the version-conditional blocks know 0.3.10 through 0.3.13; audit them before dispatching a newer one." >&2
            exit 1
            ;;
    esac

    git clone --depth 1 --branch "v${MOONCAKE_VERSION}" --recurse-submodules --shallow-submodules \
        https://github.com/kvcache-ai/Mooncake.git /work
    cd /work
    echo "[info] v${MOONCAKE_VERSION} resolves to $(git rev-parse HEAD)"
EOF

WORKDIR /work

# dependencies.sh owns the toolchain here exactly as in the default builder, retry loop and
# reason included.
RUN <<EOF
    set -eo pipefail

    installed=0
    for attempt in 1 2 3 4 5 6; do
        if bash dependencies.sh -y; then
            installed=1
            break
        fi
        echo "[warn] dependencies.sh attempt ${attempt} failed; retrying over the apt cache" >&2
        sleep 10
    done

    if [[ "${installed}" != "1" ]]; then
        echo "[ERROR] dependencies.sh did not succeed in 6 attempts." >&2
        exit 1
    fi
EOF

ENV PATH=/usr/local/go/bin:$PATH

# The flag set is the default builder's with the vendor axis swapped in:
#
#   - ASCEND_TRANSPORT picks the one Ascend flavor compiled in. ascend_direct goes ON by
#     default: the transport links only CANN toolkit libraries (ascendcl, llm_datadist,
#     metadef), and the toolkit location comes from ASCEND_HOME_PATH, which the base image
#     exports and common.cmake reads -- no CANN install or set_env step runs in this stage.
#     Either flavor switches the project to BUILD_SHARED_LIBS, which is what produces the
#     ascend_transport.so the wheel ships. The case validates the value because cmake would
#     swallow an unknown flavor's -D with a warning and ship a direct build under a ubshmem
#     name.
#   - USE_CUDA stays OFF and USE_EFA goes OFF, for the same reasons as the cuda variant.
#   - CMAKE_EXE_LINKER_FLAGS carries --allow-shlib-undefined. The CANN toolkit libraries in
#     the devel base (libascend_dump, libtsdclient, libhccl_v2, libruntime) reference
#     driver-only symbols -- libascend_hal.so and the drv*/hal* set -- that exist only where
#     the Ascend driver is installed, and linking the transfer_engine_bench executable
#     verifies that whole transitive chain. BUILD_EXAMPLES stays ON because packaging copies
#     the bench unguarded, so the link has to pass without a driver. The flag scopes the
#     relaxation to executables: the shared libraries the wheel ships still link under the
#     default checks, and a real node resolves those symbols from its installed driver.
#
# STORE_USE_K8S_LEASE stays ON exactly as in the cuda variant; the 0.3.10 caveat spelled
# out there applies here unchanged.
RUN <<EOF
    set -exo pipefail

    # 0.3.11 needs the same include_directories patch as the default builder.
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            sed -i 's|^include_directories(mooncake-common/etcd)$|include_directories(mooncake-common/etcd)\ninclude_directories(mooncake-common/k8s-lease)|' \
                /work/CMakeLists.txt
            grep -q '^include_directories(mooncake-common/k8s-lease)$' /work/CMakeLists.txt
            ;;
    esac

    case "${ASCEND_TRANSPORT}" in
        ascend_direct) transport_flag="-DUSE_ASCEND_DIRECT=ON" ;;
        ubshmem)       transport_flag="-DUSE_UBSHMEM=ON" ;;
        *)
            echo "[ERROR] unknown ASCEND_TRANSPORT '${ASCEND_TRANSPORT}': know ascend_direct and ubshmem." >&2
            exit 1
            ;;
    esac

    cmake -S /work -B /work/build -G Ninja \
        -DSTORE_USE_K8S_LEASE=ON \
        -DSTORE_USE_REDIS=ON \
        ${transport_flag} \
        -DUSE_CUDA=OFF \
        -DUSE_EFA=OFF \
        -DWITH_EP=OFF \
        -DWITH_STORE_RUST=OFF \
        -DUSE_HTTP=ON \
        -DBUILD_UNIT_TESTS=OFF \
        -DBUILD_EXAMPLES=ON \
        -DCMAKE_EXE_LINKER_FLAGS=-Wl,--allow-shlib-undefined \
        -DCMAKE_BUILD_TYPE=Release

    # The memory-bounded parallelism is the default builder's mechanism, kept for the same
    # measured reason.
    mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
    jobs=$(( mem_kb / (3 * 1024 * 1024) ))
    if [[ "${jobs}" -lt 1 ]]; then jobs=1; fi
    if [[ "${jobs}" -gt "$(nproc)" ]]; then jobs="$(nproc)"; fi
    echo "[info] building with -j${jobs} ($(nproc) cores, ${mem_kb} kB memory)"

    cmake --build /work/build -j"${jobs}"
EOF

# patchelf goes in up front for the same reason as in the default builder.
RUN pip install --no-cache-dir -U patchelf

# The packaging step is the default builder's, for the same reasons.
RUN <<EOF
    set -exo pipefail

    py_version="$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:/work/build/mooncake-common/k8s-lease"
            ;;
    esac

    PYTHON_VERSION="${py_version}" BUILD_DIR=build bash scripts/build_wheel.sh

    count="$(find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' | wc -l)"
    if [[ "${count}" != "1" ]]; then
        echo "[ERROR] expected exactly one wheel, found ${count}." >&2
        find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' >&2
        exit 1
    fi

    mkdir -p /work/lease-out
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            wrapper="/work/build/mooncake-common/k8s-lease/libk8s_lease_wrapper.so"
            test -f "${wrapper}"
            cp "${wrapper}" /work/lease-out/
            ;;
    esac

    # The wrapper is asserted here, beside the packaging that stages it, not only in the
    # runtime stage's smoke test: a release that stops producing the file, or a build_wheel.sh
    # that stops copying it into the wheel tree, fails next to the step responsible.
    case "${MOONCAKE_VERSION}" in
        0.3.10*) echo "[info] 0.3.10 has no k8s-lease backend; no wrapper to stage." ;;
        0.3.11*) test -f /work/lease-out/libk8s_lease_wrapper.so ;;
        *) test -f /work/mooncake-wheel/mooncake/libk8s_lease_wrapper.so ;;
    esac
EOF

FROM ${CANN_RUNTIME_IMAGE} AS cann
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

# The smoke below gates a second artifact on this value; the builder stage's comment carries
# what the flavors mean.
ARG ASCEND_TRANSPORT="ascend_direct"

ENV DEBIAN_FRONTEND=noninteractive

# The package list is the default runtime stage's minus libfabric1, with no Python packages at
# all: the base carries the same python.org 3.12 the builder used. libfabric1 and the AWS EFA
# installer are absent for the same reason as in the cuda variant. The CANN runtime
# libraries the build links against come from the toolkit the base itself carries, reached
# through the LD_LIBRARY_PATH the base exports.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        ibverbs-providers \
        libcurl4 \
        libibverbs1 \
        libnuma1
    rm -rf /var/lib/apt/lists/*
    python3 -m pip --version || python3 -m ensurepip --upgrade
    python3 -m pip install --no-cache-dir -U pip
EOF

RUN --mount=from=builder-cann,source=/work/mooncake-wheel/dist,target=/wheel \
    --mount=from=builder-cann,source=/work/lease-out,target=/lease <<EOF
    set -exo pipefail
    pip install --no-cache-dir /wheel/*.whl
    rm -rf /root/.cache/pip

    # The version-conditional repairs are the default stage's, unchanged.
    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*)
            pip install --no-cache-dir patchelf
            sp="$(dirname "${pkg}")"
            libs="${sp}/mooncake_transfer_engine.libs"
            for short in libefa libfabric; do
                for vendored in "${libs}/${short}"-*.so.*; do
                    [[ -e "${vendored}" ]] || continue
                    mangled="$(basename "${vendored}")"
                    find "${sp}" -type f | while read -r so; do
                        if patchelf --print-needed "${so}" 2>/dev/null | grep -qx "${mangled}"; then
                            patchelf --replace-needed "${mangled}" "${short}.so.1" "${so}"
                            echo "[info] ${so}: ${mangled} -> ${short}.so.1"
                        fi
                    done
                    rm -fv "${vendored}"
                done
            done
            pip uninstall -y patchelf

            shim="$(command -v mc_store_rest_server)"
            cat >"${shim}" <<'SHIM'
#!/bin/bash
exec python3 -m mooncake.mooncake_store_service "$@"
SHIM
            chmod +x "${shim}"
            ;;
    esac

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            cp /lease/libk8s_lease_wrapper.so "${pkg}/"
            ;;
    esac
EOF

# The smoke test is the cuda variant's plus one Ascend-specific gate: ascend_transport.so
# is dlopened by the transfer engine only when an ascend transfer runs, so the import below
# never loads it. ldd is the build-time proof that its DT_NEEDEDs -- libascendcl,
# libllm_datadist, libmetadef and the rest of the CANN toolkit set -- resolve against this
# stage.
#
# The devlib export stands in for the driver. The wheel's engine links libascend_hal.so
# directly, and the CANN base keeps the toolkit's devlib -- the driver libraries' in-image
# home -- off LD_LIBRARY_PATH: a real node resolves the name from the driver mount the Ascend
# container runtime adds under /usr/local/Ascend/driver, already on the path ahead of this
# append. The smoke runs driverless, so it borrows the toolkit's own copy, the same handling
# runner's cann pack gives its mooncake build. Appended last, it only ever fills what a
# mounted driver left unresolved.
RUN <<EOF
    set -exo pipefail

    export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:${ASCEND_HOME_PATH}/$(uname -m)-linux/devlib"

    command -v mooncake_master
    command -v mc_store_rest_server
    command -v python3

    installed="$(python3 -c 'import importlib.metadata as m; print(m.version("mooncake-transfer-engine"))')"
    if [[ "${installed}" != "${MOONCAKE_VERSION}" ]]; then
        echo "[ERROR] installed mooncake-transfer-engine is ${installed}, asked for ${MOONCAKE_VERSION}." >&2
        exit 1
    fi

    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            echo "[info] 0.3.10 has no k8s-lease backend; skipping the wrapper assertion."
            ;;
        *)
            test -f "${pkg}/libk8s_lease_wrapper.so"
            ;;
    esac

    mc_store_rest_server --help >/dev/null

    test -f "${pkg}/ascend_transport.so"
    missing="$(ldd "${pkg}/ascend_transport.so" | grep "not found" || true)"
    if [[ -n "${missing}" ]]; then
        echo "[ERROR] ascend_transport.so has unresolved libraries:" >&2
        echo "${missing}" >&2
        exit 1
    fi

    # The ubshmem wheel additionally ships ubshmem_fabric_allocator.so -- built by the
    # allocator's own build script and installed only under USE_UBSHMEM -- so a ubshmem
    # build asserts it the same way, or a partially packaged wheel ships under the flavor's
    # name.
    if [[ "${ASCEND_TRANSPORT}" == "ubshmem" ]]; then
        test -f "${pkg}/ubshmem_fabric_allocator.so"
        missing="$(ldd "${pkg}/ubshmem_fabric_allocator.so" | grep "not found" || true)"
        if [[ -n "${missing}" ]]; then
            echo "[ERROR] ubshmem_fabric_allocator.so has unresolved libraries:" >&2
            echo "${missing}" >&2
            exit 1
        fi
    fi
EOF

# Half two is the default stage's leadership probe set, unchanged.
RUN <<EOF
    set -exo pipefail

    function probe() {
        timeout -k 5 25 mooncake_master \
            -enable_ha=true \
            -ha_backend_type="$1" \
            -ha_backend_connstring="$2" \
            -rpc_port=50051 -metrics_port=9003 -logtostderr=1 \
            >"/tmp/$1.log" 2>&1 || true
        echo "--- $1 ---"
        cat "/tmp/$1.log"
    }

    probe k8s default/mooncake-master-lease
    probe redis 127.0.0.1:6379
    probe etcd http://127.0.0.1:2379
    probe bogus unused

    grep -q "Master runtime state" /tmp/k8s.log
    grep -q "Master runtime state" /tmp/redis.log
    if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/redis.log; then
        echo "[ERROR] redis metadata backend is unavailable in this build." >&2
        exit 1
    fi

    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log; then
                echo "[ERROR] etcd metadata backend is unavailable in this build." >&2
                exit 1
            fi
            ;;
        0.3.11*)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "Etcd is not enabled in compilation" /tmp/etcd.log
            ;;
        *)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log
            ;;
    esac

    grep -q "INVALID_PARAMS" /tmp/bogus.log

    rm -f /tmp/k8s.log /tmp/redis.log /tmp/etcd.log /tmp/bogus.log
EOF

# The leader ports are the default stage's; a member's are not declared for the same reason.
EXPOSE 50051 9003

CMD ["bash"]

# =============================================================================
# Target: rocm
# The AMD variant: the store compiled with USE_HIP on the ROCm 7.2 SDK, for member groups on
# ROCm nodes -- the flag set upstream's own ROCm release job configures. Dispatch with
# --target rocm. The base publishes linux/amd64 only, so this target builds for amd64
# alone; the base-image workflow's matrix follows that.
# =============================================================================
FROM ${ROCM_BUILDER_IMAGE} AS builder-rocm
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# /opt/rocm/bin joins PATH because the USE_HIP configure runs hipify-perl over the transfer
# engine sources, and the base does not export the directory -- the same line upstream's ROCm
# release job runs before configuring.
ENV PATH=/opt/rocm/bin:$PATH

# The apt retry configuration is the default builder's, kept for the same reason.
RUN <<EOF
    set -exo pipefail
    cat >/etc/apt/apt.conf.d/99-mooncake-retries <<'CONF'
Acquire::Retries "8";
Acquire::http::Timeout "30";
Acquire::https::Timeout "30";
CONF
EOF

# python3-dev and python3-pip join the default builder's package list because the base's system
# python3.10 ships without the development headers or pip. libfabric-dev is absent because the
# variant compiles with USE_EFA=OFF. The pip self-upgrade installs the `pip` entrypoint
# build_wheel.sh probes for, as in the cuda variant.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
        file \
        git \
        python3-dev \
        python3-pip \
        sudo \
        wget
    rm -rf /var/lib/apt/lists/*
    python3 -m pip install --no-cache-dir -U pip
EOF

# The clone is the default builder's, submodules included for the same reason.
RUN <<EOF
    set -exo pipefail

    # Assert the release before anything consumes it: every version-conditional block below is
    # written against 0.3.10 through 0.3.13, so a dispatch of a newer release must fail here
    # rather than take branches that were never reviewed against it.
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*|0.3.12*|0.3.13*) ;;
        *)
            echo "[ERROR] unrecognized release '${MOONCAKE_VERSION}': the version-conditional blocks know 0.3.10 through 0.3.13; audit them before dispatching a newer one." >&2
            exit 1
            ;;
    esac

    git clone --depth 1 --branch "v${MOONCAKE_VERSION}" --recurse-submodules --shallow-submodules \
        https://github.com/kvcache-ai/Mooncake.git /work
    cd /work
    echo "[info] v${MOONCAKE_VERSION} resolves to $(git rev-parse HEAD)"
EOF

WORKDIR /work

# dependencies.sh owns the toolchain here exactly as in the default builder, retry loop and
# reason included.
RUN <<EOF
    set -eo pipefail

    installed=0
    for attempt in 1 2 3 4 5 6; do
        if bash dependencies.sh -y; then
            installed=1
            break
        fi
        echo "[warn] dependencies.sh attempt ${attempt} failed; retrying over the apt cache" >&2
        sleep 10
    done

    if [[ "${installed}" != "1" ]]; then
        echo "[ERROR] dependencies.sh did not succeed in 6 attempts." >&2
        exit 1
    fi
EOF

ENV PATH=/usr/local/go/bin:$PATH

# The flag set is the default builder's with the vendor axis swapped in:
#
#   - USE_HIP goes ON and USE_CUDA stays OFF: upstream's ROCm pairing. The configure locates
#     the HIP SDK under /opt/rocm and hipifies the transfer engine sources with the hipify-perl
#     now on PATH.
#   - USE_EFA goes OFF, for the same reason as the cuda variant.
#
# STORE_USE_K8S_LEASE stays ON exactly as in the cuda variant; the 0.3.10 caveat spelled
# out there applies here unchanged.
RUN <<EOF
    set -exo pipefail

    # 0.3.11 needs the same include_directories patch as the default builder.
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            sed -i 's|^include_directories(mooncake-common/etcd)$|include_directories(mooncake-common/etcd)\ninclude_directories(mooncake-common/k8s-lease)|' \
                /work/CMakeLists.txt
            grep -q '^include_directories(mooncake-common/k8s-lease)$' /work/CMakeLists.txt
            ;;
    esac

    cmake -S /work -B /work/build -G Ninja \
        -DSTORE_USE_K8S_LEASE=ON \
        -DSTORE_USE_REDIS=ON \
        -DUSE_HIP=ON \
        -DUSE_CUDA=OFF \
        -DUSE_EFA=OFF \
        -DWITH_EP=OFF \
        -DWITH_STORE_RUST=OFF \
        -DUSE_HTTP=ON \
        -DBUILD_UNIT_TESTS=OFF \
        -DBUILD_EXAMPLES=ON \
        -DCMAKE_BUILD_TYPE=Release

    # The memory-bounded parallelism is the default builder's mechanism, kept for the same
    # measured reason.
    mem_kb="$(awk '/^MemTotal:/ {print $2}' /proc/meminfo)"
    jobs=$(( mem_kb / (3 * 1024 * 1024) ))
    if [[ "${jobs}" -lt 1 ]]; then jobs=1; fi
    if [[ "${jobs}" -gt "$(nproc)" ]]; then jobs="$(nproc)"; fi
    echo "[info] building with -j${jobs} ($(nproc) cores, ${mem_kb} kB memory)"

    cmake --build /work/build -j"${jobs}"
EOF

# patchelf goes in up front for the same reason as in the default builder.
RUN pip install --no-cache-dir -U patchelf

# The packaging step is the default builder's, for the same reasons.
RUN <<EOF
    set -exo pipefail

    py_version="$(python3 -c 'import sys; print("%d.%d" % sys.version_info[:2])')"

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            export LD_LIBRARY_PATH="${LD_LIBRARY_PATH}:/work/build/mooncake-common/k8s-lease"
            ;;
    esac

    PYTHON_VERSION="${py_version}" BUILD_DIR=build bash scripts/build_wheel.sh

    count="$(find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' | wc -l)"
    if [[ "${count}" != "1" ]]; then
        echo "[ERROR] expected exactly one wheel, found ${count}." >&2
        find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' >&2
        exit 1
    fi

    mkdir -p /work/lease-out
    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            wrapper="/work/build/mooncake-common/k8s-lease/libk8s_lease_wrapper.so"
            test -f "${wrapper}"
            cp "${wrapper}" /work/lease-out/
            ;;
    esac

    # The wrapper is asserted here, beside the packaging that stages it, not only in the
    # runtime stage's smoke test: a release that stops producing the file, or a build_wheel.sh
    # that stops copying it into the wheel tree, fails next to the step responsible.
    case "${MOONCAKE_VERSION}" in
        0.3.10*) echo "[info] 0.3.10 has no k8s-lease backend; no wrapper to stage." ;;
        0.3.11*) test -f /work/lease-out/libk8s_lease_wrapper.so ;;
        *) test -f /work/mooncake-wheel/mooncake/libk8s_lease_wrapper.so ;;
    esac
EOF

FROM ${ROCM_RUNTIME_IMAGE} AS rocm
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# The package list is the default runtime stage's minus libfabric1, plus pip for the base's
# system python3.10. libfabric1 and the AWS EFA installer are absent for the same reason as in
# the cuda variant.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        ibverbs-providers \
        libcurl4 \
        libibverbs1 \
        libnuma1 \
        python3-pip
    rm -rf /var/lib/apt/lists/*
    python3 -m pip install --no-cache-dir -U pip
EOF

# The HIP build leaves DT_NEEDEDs on libamdhip64 and libhsa-runtime64, and build_wheel.sh's
# auditwheel excludes them from the repaired wheel, so the stage must resolve them itself. The
# default runtime base already does -- measured on the 7.2.3 non-complete image, where ldconfig
# resolves both from /opt/rocm-7.2.3/lib -- but the registration stays explicit because the
# base ARGs are dispatch-time overridable and a repointed image may not set the loader up. The
# lib path comes from the base's own layout: ROCM_PATH when the image exports it, else the
# /opt/rocm symlink dereferenced. The gate fails the build if either library stops resolving,
# and its message names the path assumption so a repointed base says what to fix.
RUN <<EOF
    set -exo pipefail
    rocm_root="${ROCM_PATH:-$(readlink -f /opt/rocm)}"
    echo "${rocm_root}/lib" >/etc/ld.so.conf.d/zz-mooncake-rocm.conf
    ldconfig
    cache="$(ldconfig -p)"
    if ! grep -q "libamdhip64.so" <<<"${cache}" || ! grep -q "libhsa-runtime64.so" <<<"${cache}"; then
        echo "[ERROR] libamdhip64/libhsa-runtime64 do not resolve after registering ${rocm_root}/lib." >&2
        echo "        The stage assumes ROCm under ${rocm_root} (ROCM_PATH, else the /opt/rocm symlink); repoint ROCM_RUNTIME_IMAGE accordingly." >&2
        exit 1
    fi
EOF

RUN --mount=from=builder-rocm,source=/work/mooncake-wheel/dist,target=/wheel \
    --mount=from=builder-rocm,source=/work/lease-out,target=/lease <<EOF
    set -exo pipefail
    pip install --no-cache-dir /wheel/*.whl
    rm -rf /root/.cache/pip

    # The version-conditional repairs are the default stage's, unchanged.
    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*)
            pip install --no-cache-dir patchelf
            sp="$(dirname "${pkg}")"
            libs="${sp}/mooncake_transfer_engine.libs"
            for short in libefa libfabric; do
                for vendored in "${libs}/${short}"-*.so.*; do
                    [[ -e "${vendored}" ]] || continue
                    mangled="$(basename "${vendored}")"
                    find "${sp}" -type f | while read -r so; do
                        if patchelf --print-needed "${so}" 2>/dev/null | grep -qx "${mangled}"; then
                            patchelf --replace-needed "${mangled}" "${short}.so.1" "${so}"
                            echo "[info] ${so}: ${mangled} -> ${short}.so.1"
                        fi
                    done
                    rm -fv "${vendored}"
                done
            done
            pip uninstall -y patchelf

            shim="$(command -v mc_store_rest_server)"
            cat >"${shim}" <<'SHIM'
#!/bin/bash
exec python3 -m mooncake.mooncake_store_service "$@"
SHIM
            chmod +x "${shim}"
            ;;
    esac

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            cp /lease/libk8s_lease_wrapper.so "${pkg}/"
            ;;
    esac
EOF

# The smoke test is the cuda variant's, unchanged: the same entrypoint, version, and lease
# wrapper checks, the member entrypoint's import, and the same ldd gate over the wheel's
# extensions and the master binary. The import does resolve the ROCm DT_NEEDEDs for whatever
# it loads, but the gate checks the binaries directly and names the missing library, where an
# import failure would only surface as a Python traceback.
RUN <<EOF
    set -exo pipefail

    command -v mooncake_master
    command -v mc_store_rest_server
    command -v python3

    installed="$(python3 -c 'import importlib.metadata as m; print(m.version("mooncake-transfer-engine"))')"
    if [[ "${installed}" != "${MOONCAKE_VERSION}" ]]; then
        echo "[ERROR] installed mooncake-transfer-engine is ${installed}, asked for ${MOONCAKE_VERSION}." >&2
        exit 1
    fi

    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            echo "[info] 0.3.10 has no k8s-lease backend; skipping the wrapper assertion."
            ;;
        *)
            test -f "${pkg}/libk8s_lease_wrapper.so"
            ;;
    esac

    mc_store_rest_server --help >/dev/null

    missing="$(ldd "${pkg}/engine.so" "${pkg}/store.so" "$(command -v mooncake_master)" | grep "not found" || true)"
    if [[ -n "${missing}" ]]; then
        echo "[ERROR] unresolved libraries in the wheel's extensions or mooncake_master:" >&2
        echo "${missing}" >&2
        exit 1
    fi
EOF

# Half two is the default stage's leadership probe set, unchanged.
RUN <<EOF
    set -exo pipefail

    function probe() {
        timeout -k 5 25 mooncake_master \
            -enable_ha=true \
            -ha_backend_type="$1" \
            -ha_backend_connstring="$2" \
            -rpc_port=50051 -metrics_port=9003 -logtostderr=1 \
            >"/tmp/$1.log" 2>&1 || true
        echo "--- $1 ---"
        cat "/tmp/$1.log"
    }

    probe k8s default/mooncake-master-lease
    probe redis 127.0.0.1:6379
    probe etcd http://127.0.0.1:2379
    probe bogus unused

    grep -q "Master runtime state" /tmp/k8s.log
    grep -q "Master runtime state" /tmp/redis.log
    if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/redis.log; then
        echo "[ERROR] redis metadata backend is unavailable in this build." >&2
        exit 1
    fi

    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log; then
                echo "[ERROR] etcd metadata backend is unavailable in this build." >&2
                exit 1
            fi
            ;;
        0.3.11*)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "Etcd is not enabled in compilation" /tmp/etcd.log
            ;;
        *)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log
            ;;
    esac

    grep -q "INVALID_PARAMS" /tmp/bogus.log

    rm -f /tmp/k8s.log /tmp/redis.log /tmp/etcd.log /tmp/bogus.log
EOF

# The leader ports are the default stage's; a member's are not declared for the same reason.
EXPOSE 50051 9003

CMD ["bash"]

# =============================================================================
# Stage: mooncake-store (final)
# Install the wheel, and prove both entrypoints work and the backends are what was asked for.
# =============================================================================
FROM ${RUNTIME_IMAGE} AS mooncake-store
SHELL ["/bin/bash", "-c"]

ARG MOONCAKE_VERSION

ENV DEBIAN_FRONTEND=noninteractive

# libibverbs1 and ibverbs-providers travel together and must come from the same apt transaction:
# both are built from the rdma-core source package and share a private provider ABI. libibverbs
# alone is a library that claims no devices -- ibv_get_device_list() returns 0 even where the
# hardware is visible -- because devices are claimed through the provider plugins.
#
# They are here even though this image documents itself as TCP: the master binary carries a
# DT_NEEDED on libibverbs.so.1 whether or not a fabric is used, so the loader needs it to start at
# all. That is measured on the binary, not assumed from the transport setting.
#
# libcurl4 is the member's, and it is bookworm's spelling of what upstream's trixie image installs
# as libcurl4t64. auditwheel does not vendor it, so without it `import mooncake.store` fails and the
# member entrypoint dies before parsing an argument -- which is why the smoke test below imports
# rather than only checking that the script is on PATH.
# libfabric1 is the load-time fallback for the DT_NEEDED that USE_EFA puts on every transfer
# engine consumer (see the builder stage's flag comment). Which copy a process gets is the loader's
# decision and never the configured protocol's, so once the AWS build below is registered it
# outranks this one for EVERY process in the image, EFA member or not -- that build starts on a node
# with no EFA just as this one does, it merely also drives the device when there is one. This copy
# is what keeps the image starting at all if the AWS install ever lands where the loader does not
# look, which is the failure the gate at the end of that step refuses to ship.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        ca-certificates \
        ibverbs-providers \
        libcurl4 \
        libfabric1 \
        libibverbs1 \
        libnuma1
    rm -rf /var/lib/apt/lists/*
EOF

# The EFA userspace an EFA member runs on, taken from AWS's own installer so that libfabric and the
# rdma-core providers beneath it come from one build.
#
# IT IS IN THE IMAGE RATHER THAN MOUNTED FROM THE HOST, and that is a correctness requirement, not a
# preference. A host install is not self-contained: its libfabric resolves libefa and libibverbs
# from the host's own /usr prefix, which a container does not mount, so a loader pointed at the host
# tree fails on symbol versions instead of loading it -- measured as
# `libefa.so.1: version 'EFA_1.3' not found`. The prefix also moves by distribution, lib on Debian
# and Ubuntu against lib64 on Amazon Linux, so one hard-coded path is wrong on the other. An image
# that carries its own removes both, and the node then supplies only what only a node can: the efa
# kernel module and the device under /dev/infiniband.
#
# The installer registers its prefix with ldconfig, so nothing downstream sets LD_LIBRARY_PATH. The
# final check is a build-time gate on exactly that: an install that lands somewhere the loader does
# not look leaves the distro libfabric in charge, which starts and then finds no EFA provider.
#
# No toolchain is needed here and none is installed: the archive ships PREBUILT packages per
# distribution, this base image is Debian 12, and the installer's Debian branch apt-installs the
# DEBS/DEBIAN12 set -- present for both architectures this image is built for. Nothing is compiled.
# The installer pulls the rest of what it wants from the distro repositories itself, and under
# --skip-kmod that list is userspace only: neither dkms nor kernel headers.
#
# --skip-kmod        the efa module belongs to the node, and a container cannot load one.
# --skip-limit-conf  locked-memory limits are the node's; the member asks through its capabilities.
# --skip-mpi
# --skip-plugin      this image runs a store, not an MPI job and not NCCL.
# --no-verify        the installer's self test opens a device, and none is attached at build time.
ARG EFA_INSTALLER_VERSION
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        curl \
        pciutils
    curl -fsSL -o /tmp/aws-efa-installer.tar.gz \
        "https://efa-installer.amazonaws.com/aws-efa-installer-${EFA_INSTALLER_VERSION}.tar.gz"
    tar -xf /tmp/aws-efa-installer.tar.gz -C /tmp
    pushd /tmp/aws-efa-installer >/dev/null
    ./efa_installer.sh -y --skip-kmod --skip-limit-conf --skip-mpi --skip-plugin --no-verify
    popd >/dev/null
    # Both were installed for this step alone, and neither was pulled in as a dependency, so
    # autoremove would leave them standing in the runtime image.
    apt-get purge -y curl pciutils
    apt-get autoremove -y
    rm -rf /tmp/aws-efa-installer /tmp/aws-efa-installer.tar.gz /var/lib/apt/lists/*
    ldconfig
    # The gate names libfabric.so.1 and reads the FIRST cache entry for it, rather than asking
    # whether anything at all sits under the prefix. The installer also puts libefa and the
    # rdma-core providers there, so a prefix-only test passes on those while libfabric.so.1 still
    # resolves to the distro copy -- which is the single outcome this gate exists to catch.
    cache="$(ldconfig -p)"
    winner="$(grep -m 1 -E '^[[:space:]]*libfabric\.so\.1 ' <<<"${cache}")" || winner=""
    if [[ "${winner}" != *"=> /opt/amazon/efa/lib/"* ]]; then
        echo "[ERROR] libfabric.so.1 resolves to '${winner:-nothing}', not AWS's /opt/amazon/efa/lib." >&2
        exit 1
    fi
EOF

RUN --mount=from=builder,source=/work/mooncake-wheel/dist,target=/wheel \
    --mount=from=builder,source=/work/lease-out,target=/lease <<EOF
    set -exo pipefail
    pip install --no-cache-dir /wheel/*.whl
    rm -rf /root/.cache/pip

    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*|0.3.11*)
            # Two repairs for the releases before 0.3.12; 0.3.12 vendors neither library and its
            # console script works, so for it and later this whole branch is skipped.
            #
            # One: auditwheel vendored BOTH libfabric and the AWS libefa provider INTO the wheel
            # under mangled sonames, chained engine .so -> libfabric -> libefa -- where the
            # vendored libefa binds the distro libibverbs, which is too old for it
            # (IBVERBS_PRIVATE_34). Give the wheel the same resolution 0.3.12+ gets natively
            # (build_wheel.sh gained `--exclude libfabric.so*` in 0.3.12): rewrite every
            # DT_NEEDED that names a vendored copy onto the plain soname (which ldconfig
            # resolves to the AWS install gated above), then drop the vendored files. Deleting
            # without the rewrite leaves a dangling DT_NEEDED and every import dies on the
            # mangled name -- measured on 0.3.10. The scan is by file TYPE, not by name: the
            # bundled C++ binaries (mooncake_master, transfer_engine_bench) carry the
            # dependency too, and they have no .so suffix.
            pip install --no-cache-dir patchelf
            sp="$(dirname "${pkg}")"
            libs="${sp}/mooncake_transfer_engine.libs"
            for short in libefa libfabric; do
                for vendored in "${libs}/${short}"-*.so.*; do
                    [[ -e "${vendored}" ]] || continue
                    mangled="$(basename "${vendored}")"
                    find "${sp}" -type f | while read -r so; do
                        if patchelf --print-needed "${so}" 2>/dev/null | grep -qx "${mangled}"; then
                            patchelf --replace-needed "${mangled}" "${short}.so.1" "${so}"
                            echo "[info] ${so}: ${mangled} -> ${short}.so.1"
                        fi
                    done
                    rm -fv "${vendored}"
                done
            done
            pip uninstall -y patchelf

            # Two: the mc_store_rest_server console script is BROKEN before 0.3.12 --
            # mooncake_store_service.main is `async def` and the generated wrapper calls it
            # without awaiting, so it prints an unawaited coroutine and exits 1 on EVERY
            # invocation. (0.3.12 added sync_main and pointed the entry point at it.) The
            # operator renders memberEntrypoint="mc_store_rest_server" unconditionally
            # (pkg/worker/kvcache/mooncake/member_workload.go), so the image replaces the
            # script with a shim that reaches the module's own `asyncio.run(main())` guard.
            # That keeps the member command version-agnostic: the workload and the smoke test
            # below name the same entrypoint for every release.
            shim="$(command -v mc_store_rest_server)"
            cat >"${shim}" <<'SHIM'
#!/bin/bash
exec python3 -m mooncake.mooncake_store_service "$@"
SHIM
            chmod +x "${shim}"
            ;;
    esac

    case "${MOONCAKE_VERSION}" in
        0.3.11*)
            # 0.3.11 compiles the lease backend but its build_wheel.sh does not package the
            # wrapper .so (the copy arrived in 0.3.12; 0.3.10 has no backend to package). The
            # builder stage staged it; install it beside the package exactly where the 0.3.12+
            # wheel would put it, because the master loads it from there and the smoke test
            # below asserts it is present.
            cp /lease/libk8s_lease_wrapper.so "${pkg}/"
            ;;
    esac
EOF

# Build-time smoke test, in two halves: the entrypoints exist, and the backends are the ones asked
# for. Both halves re-run on every build rather than living in a report.
RUN <<EOF
    set -exo pipefail

    # Half one: the two roles this image serves. `mooncake_master` is a console script that
    # os.execv's the bundled binary, so it keeps its PID and the argv reaches the real process --
    # which is what makes the probes below equivalent to running the binary directly.
    command -v mooncake_master
    command -v mc_store_rest_server
    command -v python3

    # What got installed is the release that was asked for. This is the check that replaced pinning
    # a commit: it reads the version out of the BUILT artifact rather than out of the argument that
    # requested it, so a tag pointing somewhere else fails here instead of shipping an image whose
    # name and contents disagree.
    installed="$(python3 -c 'import importlib.metadata as m; print(m.version("mooncake-transfer-engine"))')"
    if [[ "${installed}" != "${MOONCAKE_VERSION}" ]]; then
        echo "[ERROR] installed mooncake-transfer-engine is ${installed}, asked for ${MOONCAKE_VERSION}." >&2
        exit 1
    fi

    # The Go c-shared half of the lease backend, present INSIDE the installed package. Its absence
    # is the one way this image could carry a master that links the backend and then cannot load
    # it. 0.3.10 predates the backend entirely, so for it the wrapper cannot exist and the
    # assertion is skipped -- that image serves a single-replica leader with election off, which
    # is also what the inverted k8s probe in half two asserts. For 0.3.11 the file is the copy
    # the install stage made; for 0.3.12+ it came in the wheel.
    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            echo "[info] 0.3.10 has no k8s-lease backend; skipping the wrapper assertion."
            ;;
        *)
            test -f "${pkg}/libk8s_lease_wrapper.so"
            ;;
    esac

    # A member is a Python entrypoint, so "it is installed" and "it can start" are different
    # claims. --help exercises the import chain, which is where a missing provider library or a
    # wrong-ABI extension actually surfaces. This names the console script for every release: in
    # 0.3.10 that script is the shim installed above, so the smoke test exercises exactly the
    # entrypoint the operator's member workload will invoke.
    mc_store_rest_server --help >/dev/null

    # The EFA transport is compiled IN, asserted by what this run must NOT print. A target-mode
    # bench told protocol=efa has three outcomes. On a device-less build machine it fails inside
    # EfaTransport's own install path with "No EFA devices found" -- a message only an installed
    # transport can produce. On an EFA-capable builder it inits and serves until the timeout
    # kills it, and that is a pass too. The failure this test exists to catch is the third: the
    # transport map's "Invalid protocol", which is what an image WITHOUT the transport prints. A
    # loader error is refused alongside it, because the bench doubles as the regression half of
    # the libfabric story: it is a transfer engine consumer, so it starts only if the DT_NEEDED on
    # libfabric.so.1 resolved. It resolves against the AWS build, which outranks the distro copy
    # for every process here, so this is also the check that AWS's libfabric carries a consumer
    # that is not on the EFA path -- which is every other member.
    #
    # The timeout must outlast the device-less error path; if this check ever flakes, that is the
    # knob to revisit. docs/kv-cache/backend.md quotes "No EFA devices found" in its HA section,
    # so an upstream rewording of that message must be reflected in both places.
    command -v transfer_engine_bench
    rc=0
    timeout -k 5 25 transfer_engine_bench \
        --mode=target --protocol=efa --metadata_server=P2PHANDSHAKE \
        >/tmp/efa.log 2>&1 || rc=$?
    cat /tmp/efa.log
    # Written as an if and NOT as `! grep ...`: a pipeline inverted with ! is exempt from
    # set -e, so the negated form would never stop the build.
    if grep -qE "Invalid protocol|error while loading shared libraries" /tmp/efa.log; then
        echo "[ERROR] EFA transport is not compiled in, or libfabric failed to load." >&2
        exit 1
    fi
    # Device-less: the transport's own install path must be what stopped it. EFA-capable: the
    # bench kept running until the timeout -- 124, or 137 when the SIGKILL had to deliver it.
    grep -q "No EFA devices found" /tmp/efa.log || [[ $rc -eq 124 || $rc -eq 137 ]]
    rm -f /tmp/efa.log
EOF

# Half two: four cases differing in exactly one token, because the two controls carry as much
# information as the subjects. Without the etcd case a wholesale disabling of the availability check
# would look identical to success, and without the bogus case a parser that accepts everything
# would too.
#
# REQUIRED: do not read the exit codes here. The k8s and redis cases are expected to keep running
# and be killed by the timeout, and a timeout's exit status arrives for whatever reason the process
# stopped -- under emulation, `timeout` without -k has been observed waiting forever on a SIGTERM
# that was never answered, so 124 means only that time passed. The load-bearing evidence is what the
# process printed. -k bounds that case.
#
# The k8s backend is expected to fail to reach an API server in a build container: there is no
# kubeconfig and no service account. That failure is the strongest evidence in the whole test,
# because it can only be produced by the Go wrapper having been CALLED, which is a stronger
# statement than it having been linked. For 0.3.10, which predates the backend, the k8s and etcd
# assertions invert, and for 0.3.11 the etcd control CHECK-fails instead of answering the
# refusal -- the case block below spells out all three polarities.
RUN <<EOF
    set -exo pipefail

    function probe() {
        timeout -k 5 25 mooncake_master \
            -enable_ha=true \
            -ha_backend_type="$1" \
            -ha_backend_connstring="$2" \
            -rpc_port=50051 -metrics_port=9003 -logtostderr=1 \
            >"/tmp/$1.log" 2>&1 || true
        echo "--- $1 ---"
        cat "/tmp/$1.log"
    }

    probe k8s default/mooncake-master-lease
    probe redis 127.0.0.1:6379
    probe etcd http://127.0.0.1:2379
    probe bogus unused

    # Subjects: both must clear the availability check. "Master runtime state" is printed by the
    # supervisor loop, which is only reached after BuildHABackendSpec returns.
    #
    # The refusal is asserted absent with an if-guarded grep, NOT with `grep -qv` and NOT with
    # `! grep -q`. The -v form asks whether some line does not match, which every multi-line log
    # satisfies whether or not the refusal is also in there -- an assertion that cannot fail. The
    # inverted form fails identically: a pipeline inverted with ! is exempt from set -e, so the
    # negated command returns 1 and the build walks on. Naming only the -v trap here once read as
    # though the ! form had been reviewed; a comment arguing against one spelling says nothing
    # for the spelling it leaves standing.
    #
    # LIMITED: the strings below are upstream log text at MOONCAKE_VERSION, not an API. Bumping
    # the version means re-reading them out of that release before trusting a failure here: a
    # reworded message fails this stage twenty minutes into the build, and the grep that fails says
    # nothing about which token went stale.
    grep -q "Master runtime state" /tmp/k8s.log
    grep -q "Master runtime state" /tmp/redis.log
    if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/redis.log; then
        echo "[ERROR] redis metadata backend is unavailable in this build." >&2
        exit 1
    fi

    case "${MOONCAKE_VERSION}" in
        0.3.10*)
            # The k8s subject is INVERTED: the flag parses and the supervisor runs, but
            # coordinator creation answers UNAVAILABLE_IN_CURRENT_MODE because the k8s-lease
            # backend does not exist in this release. Asserting the refusal documents that from
            # the artifact instead of from the source tree -- this image serves a single-replica
            # leader with election off. The etcd control inverts with it: the K8S_LEASE/ETCD
            # exclusion does not exist yet either, so etcd is compiled in and clears the check
            # exactly as redis does.
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log; then
                echo "[ERROR] etcd metadata backend is unavailable in this build." >&2
                exit 1
            fi
            ;;
        0.3.11*)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            # The K8S_LEASE/ETCD exclusion does not exist yet either, so the etcd control is
            # not refused -- it CHECK-fails instead: this build compiles etcd out for the
            # lease backend, and the stub helper aborts on first use.
            grep -q "Etcd is not enabled in compilation" /tmp/etcd.log
            ;;
        *)
            if grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log; then
                echo "[ERROR] k8s-lease metadata backend is unavailable in this build." >&2
                exit 1
            fi
            # Control: etcd must still be refused (proves the exclusion held and the check was
            # not disabled wholesale).
            grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log
            ;;
    esac

    # Control: an unparseable value must be refused differently (proves the type parser still
    # runs).
    grep -q "INVALID_PARAMS" /tmp/bogus.log

    rm -f /tmp/k8s.log /tmp/redis.log /tmp/etcd.log /tmp/bogus.log
EOF

# The two ports this operator renders for a leader: LeaderRPCPort and LeaderMetricsPort in
# pkg/worker/kvcache/mooncake/leader_flags.go. The admin HTTP surface, which the readiness probe and
# the tenant ledger both drive, is the second one. A member's ports are deliberately not declared:
# its transfer engine binds them at random, which is why this operator states a range in
# documentation instead of a list here.
EXPOSE 50051 9003

# No ENTRYPOINT. The image serves two roles and this operator names the one it wants in the
# workload's `command`, exactly as the upstream image expects -- baking one in would make the other
# reachable only by overriding it.
CMD ["bash"]
