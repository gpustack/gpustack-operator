# THead PPU SDK development base image.
#
# WHAT: a build-time base carrying the T-Head PPU SDK (HGGC toolchain, headers and
# user-space libraries) at ${PPU_HOME}. It exists so the operator image's
# logical-slicing builder stage can compile a small preload library against the SDK
# headers without every build fetching a ~1.5 GiB archive.
#
# WHY a separate image: Ascend and NVIDIA can build against a published vendor devel
# image (quay.io/ascend/cann:*, nvidia/cuda:*-devel). T-Head publishes no equivalent,
# and the SDK is only distributed as a presigned object-storage link, so we pin it into
# an image once per SDK release instead of re-downloading per build.
#
# AMD64 ONLY: the SDK ships targets/x86_64-linux and nothing else. Do not add this
# image to an arm64 build matrix — see .github/workflows/pack.yml.
#
# PPU_SDK_URL: T-Head ships the SDK only as a presigned Aliyun OSS link (~7-day
# expiry); there is no public registry or stable download URL. Pass it at build time
# and never commit or log it:
#
#   PPU_SDK_URL='<presigned url>' make package thead-ppu-devel
#
# In CI it arrives through the pack workflow's build-argument override input
# ("args: PPU_SDK_URL=<presigned url>"). Once the archive is mirrored to a stable
# internal location, point PPU_SDK_URL at that instead and the expiry problem is gone.

# Deliberately an older base than the operator runtime image (Ubuntu 24.04): the
# library compiled here is preloaded into a *workload* container whose glibc we do not
# control, so a lower glibc floor widens compatibility. Default matches the SDK
# package's own target (ubuntu2004 / glibc 2.31); lower it (e.g. ubuntu:18.04, glibc
# 2.27, verified working) when an older workload base must be supported. The binding
# guard is not this value but the GLIBC assertion in the smoke-test stage below.
ARG BASE_IMAGE="ubuntu:20.04"

# =============================================================================
# Stage: base
# Package set shared by the SDK stage and the final image, so the toolchain is
# installed once and both stages resolve to the same layer.
# =============================================================================
FROM ${BASE_IMAGE} AS base
SHELL ["/bin/bash", "-c"]

ENV DEBIAN_FRONTEND=noninteractive

# Toolchain for compiling the preload library. The SDK also ships its own compiler
# (hgcc/clang) under ${PPU_HOME}/bin for device code, but the host-side shim is plain
# C, so a host gcc is what the builder stage actually uses.
RUN <<EOF
    set -exo pipefail
    apt-get update -y
    apt-get install -y --no-install-recommends \
        binutils \
        ca-certificates \
        curl \
        file \
        g++ \
        gcc \
        libc6-dev \
        make
    rm -rf /var/lib/apt/lists/*
EOF

# =============================================================================
# Stage: sdk
# Download and extract the SDK into /out, which the final image copies in.
#
# The download deliberately happens in a stage that is never pushed, because
# BuildKit writes every build argument's value into the image history of the stage
# that declares it — `docker history` shows both the "ARG PPU_SDK_URL=<value>" entry
# and the argument list prefixed onto every later RUN. Declaring PPU_SDK_URL in the
# final stage would therefore publish the presigned link, signature included, to
# anyone who pulls the image. Keep it in this stage only.
# =============================================================================
FROM base AS sdk

# Expected HGGC runtime API generation. Asserted below so a mismatched SDK fails the
# build instead of silently producing a library compiled against the wrong generation.
ARG PPU_SDK_EXPECT_RT="v3"
ARG PPU_SDK_URL

RUN <<EOF
    # Tracing stays OFF across every command that reads the URL. `set -x` prints every
    # expanded word, so tracing the emptiness guard or the curl publishes the presigned
    # link -- signature included -- into the build log, which for a CI build is
    # world-readable. PPU_SDK_URL is an ARG, so it stays set for the whole stage; what
    # makes the re-enable below safe is that no command after it reads the URL.
    set -eo pipefail

    if [[ -z "${PPU_SDK_URL}" ]]; then
        echo "[ERROR] PPU_SDK_URL is required; see the header comment in this Dockerfile." >&2
        exit 1
    fi
    if [[ "$(uname -m)" != "x86_64" ]]; then
        echo "[ERROR] the PPU SDK ships only targets/x86_64-linux, cannot build for $(uname -m)." >&2
        exit 1
    fi

    curl -fsSL --retry 3 --retry-delay 5 -o /tmp/ppu-sdk.run "${PPU_SDK_URL}"

    set -x

    # Self-extract instead of running the bundled install-ppu.pl: that installer needs
    # the Perl module Tie::File, which a clean Ubuntu base does not carry (it ships
    # perl-base only), and it exits 2 before creating anything. Extraction needs no perl.
    sh /tmp/ppu-sdk.run --noexec --keep --target /tmp/ppu-sdk-extract >/dev/null
    mv /tmp/ppu-sdk-extract/PPU_SDK /out
    for f in envsetup.sh LICENSE NOTICE; do
        if [[ -f "/tmp/ppu-sdk-extract/${f}" ]]; then
            cp "/tmp/ppu-sdk-extract/${f}" /out/
        fi
    done
    chmod -R 0755 /out

    # Fail closed on the SDK generation. The older ppu-sdk-1.0 layout has no
    # VERSION.txt at all, so treat a missing file as a failure rather than a skip.
    if [[ ! -f /out/VERSION.txt ]]; then
        echo "[ERROR] the extracted SDK has no VERSION.txt; cannot verify the HGGC runtime generation." >&2
        exit 1
    fi
    grep -q "hggcrt_version:${PPU_SDK_EXPECT_RT}" /out/VERSION.txt

    rm -rf /tmp/ppu-sdk.run /tmp/ppu-sdk-extract
EOF

# =============================================================================
# Stage: thead-ppu-devel (final)
# The base package set plus the extracted SDK. No build argument carrying the SDK
# location is in scope here, so nothing recoverable from the published image points
# back at the presigned link.
# =============================================================================
FROM base AS thead-ppu-devel

ARG PPU_HOME="/usr/local/PPU_SDK"

COPY --from=sdk /out ${PPU_HOME}

# LD_LIBRARY_PATH is appended with ${LD_LIBRARY_PATH:+:...} rather than a bare colon:
# an empty entry (which a trailing colon leaves behind when the base image sets no
# LD_LIBRARY_PATH, as ubuntu:20.04 does not) means the current directory to the dynamic
# loader, so anything run from a writable directory could be made to load a library
# from it.
ENV PPU_HOME="${PPU_HOME}" \
    PPU_SDK="${PPU_HOME}" \
    PATH="${PPU_HOME}/bin:${PATH}" \
    LD_LIBRARY_PATH="${PPU_HOME}/targets/x86_64-linux/lib${LD_LIBRARY_PATH:+:${LD_LIBRARY_PATH}}" \
    C_INCLUDE_PATH="${PPU_HOME}/targets/x86_64-linux/include" \
    CPLUS_INCLUDE_PATH="${PPU_HOME}/targets/x86_64-linux/include"

# Build-time smoke test: prove the headers and toolchain can actually produce the kind
# of library the builder stage needs, and that it stays loadable in an old workload
# container. Three properties are asserted, all of which have bitten before:
#
#   1. hgml.h includes no header at all, so it supplies neither the `bool` its own
#      hgmlDeviceDestroyVgpuInstance declaration uses nor the NULL a caller needs; both
#      must be pulled in ahead of it or plain C compilation fails. The generated source
#      below does that with explicit <stdbool.h> and <stddef.h> includes; a consumer that
#      cannot edit its sources gets the same effect from gcc's -include. Pinning the
#      requirement here means a future SDK regression shows up as a build failure rather
#      than as a confusing failure in the operator build.
#   2. NEEDED must never name an HGGC/HGML library: the product is preloaded into a
#      container that brings its own SDK, so it must not hard-link one. libc.so.6 is
#      the only entry a correct build may carry — and none at all is equally correct,
#      because a stub this small references no libc symbol and --as-needed (Ubuntu's
#      default) then records no DT_NEEDED entry.
#   3. The highest GLIBC_ version the product requires is the real compatibility
#      guard, not the base image tag. It must not exceed the SDK's own floor (2.17).
RUN <<EOF
    set -exo pipefail

    inc="${PPU_HOME}/targets/x86_64-linux/include"
    printf '%s\n' \
        '#include <stdbool.h>' \
        '#include <stddef.h>' \
        '#include "hgml.h"' \
        'hgmlReturn_t hgmlDeviceGetMemoryInfo(hgmlDevice_t device, hgmlMemory_t *memory)' \
        '{' \
        '    (void)device;' \
        '    if (memory == NULL) {' \
        '        return HGML_ERROR_INVALID_ARGUMENT;' \
        '    }' \
        '    memory->total = 0;' \
        '    memory->free = 0;' \
        '    memory->used = 0;' \
        '    return HGML_SUCCESS;' \
        '}' >/tmp/smoke.c
    gcc -shared -fPIC -O2 -I"${inc}" -o /tmp/smoke.so /tmp/smoke.c

    needed="$(readelf -d /tmp/smoke.so | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort -u | tr '\n' ' ')"
    echo "smoke: NEEDED = ${needed:-none}"
    [[ -z "${needed}" || "${needed}" == "libc.so.6 " ]]

    # Match every component, not just the first two: a two-component pattern truncates
    # GLIBC_2.2.5 to GLIBC_2.2, which only ever reports a lower requirement than the real
    # one and so can only make the ceiling below too lenient. The `|| true` keeps a
    # library that requires no versioned symbol at all from aborting the pipeline under
    # `set -o pipefail` — grep exits 1 on no match, and that case is legitimate: it falls
    # back to the floor below. sort -V orders these correctly, including a three-component
    # GLIBC_2.2.5 below GLIBC_2.17.
    max_glibc="$(readelf -W --dyn-syms /tmp/smoke.so | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
    echo "smoke: max glibc requirement = ${max_glibc:-none}"
    highest="$(printf '%s\nGLIBC_2.17\n' "${max_glibc:-GLIBC_2.2.5}" | sort -uV | tail -1)"
    [[ "${highest}" == "GLIBC_2.17" ]]

    rm -f /tmp/smoke.c /tmp/smoke.so
EOF

WORKDIR "${PPU_HOME}"

CMD ["/bin/bash"]
