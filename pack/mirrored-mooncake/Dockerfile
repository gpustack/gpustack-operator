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
# LIMITED: what this image covers is medium DRAM over protocol Auto, TCP or EFA. RDMA, HIP and
# Ascend each link a runtime it does not carry, so a member group on one of those fabrics cannot
# currently run under high availability at all -- covering them means rebuilding each vendor
# variant with the lease backend, because the vendor axis and the leadership axis are orthogonal.
# Whether that is the right answer, or whether members should keep the leader Service address and
# let readiness move the endpoint, is open:
# https://github.com/gpustack/gpustack-operator/issues/279
#
# EFA sits on the fabric axis, not the vendor axis: it needs no vendor runtime, only libfabric,
# which is why it is compiled into this one image rather than a variant. The libfabric story is
# two-layered by design. Upstream links `fabric` PUBLIC into transfer_engine, so master, engine.so
# and store.so all carry a DT_NEEDED on libfabric.so.1, and upstream's build_wheel.sh excludes
# libfabric from the repaired wheel unconditionally -- so the runtime stage installs the distro
# libfabric1 as the load-time fallback that keeps TCP members starting anywhere. On an EFA node
# the operator mounts the host's /opt/amazon/efa and sets LD_LIBRARY_PATH to its lib/ -- a
# replacement, deliberate because this image defines no LD_LIBRARY_PATH of its own -- which
# outranks the default search path, so the AWS build of libfabric -- matched to the host's EFA
# driver -- is the one that loads. One process, one libfabric, either way.
#
# FORBIDDEN: adding STORE_USE_ETCD. The upstream CMakeLists raises FATAL_ERROR when it is combined
# with STORE_USE_K8S_LEASE, because both backends build a Go c-shared library. That exclusion is the
# reason this image has no oplog: master.cpp refuses to start with `enable_oplog` unless the backend
# is etcd, so oplog following is given up permanently here. A standby rebuilds from snapshot
# bootstrap and remounts instead, which is independent of the backend type. For a cache, that trades
# replay latency, not durability.

# REQUIRED: both stages carry the SAME Python and the SAME Debian release. The wheel's extension
# modules are built WITH_SOABI, so they are tagged for one interpreter version and will not import
# under another, and the binary inside the wheel is linked against the builder's glibc.
#
# Building the wheel here rather than installing one from PyPI is also what sidesteps the manylinux
# constraint upstream's own image documents: the published aarch64 wheel is manylinux_2_39 and needs
# a glibc bookworm does not have. A wheel built on bookworm needs bookworm.
ARG BUILDER_IMAGE="python:3.12-bookworm"
ARG RUNTIME_IMAGE="python:3.12-slim-bookworm"

# The upstream release this image is built from, spelled the way upstream's own images spell it: the
# PyPI version, without the tag's leading "v". The clone adds that back, because the git tag and the
# wheel version are the same release under two naming conventions and carrying both as build
# arguments is how they drift apart.
#
# It is asserted after the build rather than trusted -- the smoke test below reads the version out of
# the INSTALLED package, so a tag that moved to a different release fails the build instead of
# producing an image whose name and contents disagree.
ARG MOONCAKE_VERSION="0.3.13.post1"

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
# USE_CUDA and WITH_EP are off because no member group here contributes VRAM: the API's medium is a
# single-value enum, DRAM. Turning them on pulls large toolchains and a DT_NEEDED on libcuda that
# the runtime stage would then have to satisfy with stubs.
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
    PYTHON_VERSION="${py_version}" BUILD_DIR=build bash scripts/build_wheel.sh

    # One wheel, named. A glob that silently matched two would install whichever pip preferred.
    count="$(find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' | wc -l)"
    if [[ "${count}" != "1" ]]; then
        echo "[ERROR] expected exactly one wheel, found ${count}." >&2
        find /work/mooncake-wheel/dist -maxdepth 1 -name '*.whl' >&2
        exit 1
    fi
EOF

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
# engine consumer (see the builder stage's flag comment). It is NOT the libfabric an EFA member
# runs on: there the operator mounts the host's /opt/amazon/efa and LD_LIBRARY_PATH puts its lib/
# first, so the AWS build matched to the node's driver wins. This copy exists so that a member on
# any other protocol still starts on a node that has no EFA installation at all. When the host
# lib wins it also resolves its provider plugins from its own install prefix, so the arrangement
# relies on the AWS installer shipping those providers under /opt/amazon/efa/lib -- a distro
# provider paired with the host core library is the mixed state this avoids.
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

RUN --mount=from=builder,source=/work/mooncake-wheel/dist,target=/wheel <<EOF
    set -exo pipefail
    pip install --no-cache-dir /wheel/*.whl
    rm -rf /root/.cache/pip
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

    # The Go c-shared half of the lease backend, packaged INSIDE the wheel. Its absence is the one
    # way this image could carry a master that links the backend and then cannot load it.
    pkg="$(python3 -c 'import mooncake, os; print(os.path.dirname(mooncake.__file__))')"
    test -f "${pkg}/libk8s_lease_wrapper.so"

    # A member is a Python entrypoint, so "it is installed" and "it can start" are different
    # claims. --help exercises the import chain, which is where a missing provider library or a
    # wrong-ABI extension actually surfaces.
    mc_store_rest_server --help >/dev/null

    # The EFA transport is compiled IN, asserted by what this run must NOT print. A target-mode
    # bench told protocol=efa has three outcomes. On a device-less build machine it fails inside
    # EfaTransport's own install path with "No EFA devices found" -- a message only an installed
    # transport can produce. On an EFA-capable builder it inits and serves until the timeout
    # kills it, and that is a pass too. The failure this test exists to catch is the third: the
    # transport map's "Invalid protocol", which is what an image WITHOUT the transport prints. A
    # loader error is refused alongside it, because the bench doubles as the regression half of
    # the libfabric story: it is a transfer engine consumer, so it starts only if the DT_NEEDED
    # on libfabric.so.1 resolved against the distro libfabric1 installed above -- the same
    # resolution a TCP member depends on.
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
# statement than it having been linked.
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
    # The refusal is asserted absent with `! grep -q`, NOT with `grep -qv`. The -v form asks whether
    # some line does not match, which every multi-line log satisfies whether or not the refusal is
    # also in there -- an assertion that cannot fail.
    #
    # LIMITED: the four strings below are upstream log text at MOONCAKE_VERSION, not an API. Bumping
    # the version means re-reading them out of that release before trusting a failure here: a
    # reworded message fails this stage twenty minutes into the build, and the grep that fails says
    # nothing about which token went stale.
    grep -q "Master runtime state" /tmp/k8s.log
    grep -q "Master runtime state" /tmp/redis.log
    ! grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/k8s.log
    ! grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/redis.log

    # Controls: etcd must still be refused (proves the exclusion held and the check was not disabled
    # wholesale), and an unparseable value must be refused differently (proves the type parser still
    # runs).
    grep -q "UNAVAILABLE_IN_CURRENT_MODE" /tmp/etcd.log
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
