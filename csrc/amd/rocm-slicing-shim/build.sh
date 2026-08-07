#!/usr/bin/env bash
#
# build.sh — the one place that knows how to compile this tree.
#
#   build.sh lib   [<name>...]   the shared object that gets preloaded  (default: all)
#   build.sh tool  [<name>...]   the readers under tools/               (default: all)
#   build.sh test  [<name>...]   the gate binaries under testing/       (default: all)
#   build.sh unit  [mutants]     common/'s unit tests -- no ROCm, no device, runs anywhere
#   build.sh check [<name>]      the four linkage assertions case 1 re-runs
#   build.sh list  <name>        the translation units behind one artifact, one per line
#
# WHY THIS EXISTS. The verification cases would otherwise each carry these recipes -- the
# translation-unit lists, the include roots, which artifact may see a ROCm header and which may
# not. Seven case scripts each holding a copy is how the THead tree's recipes drifted onto
# different source paths before they were moved here. The recipes belong with the sources they
# compile; what stays in the cases is what they are for -- the assertions.
#
# WHY A SHELL SCRIPT AND NOT CMAKE. There is no dependency graph to resolve: one shared object,
# two readers and four gate binaries, each from a fixed list of C files, none linking anything but
# libc, no generated sources, no install step and no second architecture (ROCm ships no aarch64
# user space). That is seven compiler invocations, which CMake would wrap in a configure step and
# a generated build tree for nothing this build has to decide. The repo's own convention is the
# same: every other build recipe here is shell, under hack/ or pack/.
#
# IT DECLARES EVERY ARTIFACT UP FRONT, including ones whose sources do not exist yet. That is
# deliberate: five tasks build into this tree in parallel, and a build.sh each of them appends its
# own recipe to is a file five tasks contend on. The cost is that `lib`, `tool` and `test` fail
# until their sources land, and `unit` and `list` are what work in the meantime.
#
# IT KNOWS NOTHING ABOUT CONTAINERS. hip/ and testing/ need HIP headers, so the CALLER decides
# where this runs: the skill's `scripts/build.sh xbuild-amd-rocm` runs it inside a ROCm devel
# image, and on a host with ROCm installed it runs directly. `unit` needs neither, by design, and
# neither do the tools.
#
# IT IS SILENT WHEN IT SUCCEEDS, and that is load-bearing rather than terse: case 1 decides
# "compiles clean" on empty output, not on the exit status, so anything this script printed of its
# own would read as a compiler diagnostic. `V=1` prints each command for a human.
#
# Env: OUT (where artifacts land, default this directory) · ROCM_PATH (default /opt/rocm) ·
#      CC (default gcc) · V=1 (trace).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-${HERE}}"
ROCM_PATH="${ROCM_PATH:-/opt/rocm}"
CC="${CC:-gcc}"

ROCM_INC="${ROCM_PATH}/include"

# -I"${HERE}" is the tree root, so `#include "common/..."` and `#include "hip/..."` resolve from a
# source in any module directory -- one include root for every artifact, which is what keeps a
# module's own header from needing a relative path out of its directory.
WARN="-Wall -Wextra"
STD="-std=gnu11"

# The HIP headers refuse to define anything until one platform is chosen, and the diagnostic they
# raise is a single #error whose fallout is a page of "unknown type name" further down. Every
# translation unit that includes them needs this; the ones that do not are unaffected.
HIP_PLATFORM="-D__HIP_PLATFORM_AMD__"

# The library's per-thread state is reached WITHOUT __tls_get_addr, and that is what keeps
# DT_NEEDED at exactly libc.so.6. The default general-dynamic model calls into the dynamic loader
# for every thread-local access, which puts ld-linux-x86-64.so.2 in DT_NEEDED and fails the
# assertion below. Initial-exec is the correct model here rather than a trick: this object is
# loaded through /etc/ld.so.preload, which means it is always in the initial link map and never
# dlopen()ed, and initial-exec is only invalid for the latter. Measured: general-dynamic needs
# libc.so.6 AND ld-linux-x86-64.so.2, initial-exec needs libc.so.6 alone; -ldl changes neither,
# because the loader dependency was never about the dl* calls.
TLS_MODEL="-ftls-model=initial-exec"

# The product links NOTHING but libc: it is preloaded into a container that brings its own ROCm,
# so every runtime symbol is reached through the resolver at run time. That also rules out -ldl --
# on a modern glibc it is an empty stub, and on an older one it would be a second DT_NEEDED entry.
COMMON_ALL="common/vrocm_log.c common/vrocm_quota.c common/vrocm_ledger.c"

# srcs <artifact> — the translation units behind one artifact, first one first.
srcs() {
    case "$1" in
        # The product. One object for every quota surface: the resolver and the interception
        # table, the three reported-capacity entry points, the classic allocating family and the
        # stream-ordered/pool family. Each family registers its own entries from its own
        # translation unit, so no two of them edit one file.
        libvrocm) echo "hip/hip_resolve.c hip/hip_table.c hip/hip_query.c hip/hip_mem.c \
hip/hip_pool.c ${COMMON_ALL}" ;;

        # The usage reader. It links none of common/ on purpose: the ledger's mapping helper
        # CREATES the region when none exists and its other entries take the card's lock, and a
        # reader must do neither. It takes the layout from the header and maps it read-only itself.
        rocm_monitor) echo "tools/rocm_monitor.c" ;;
        # The mask self-check. It links none of common/ either -- it has no quota to account and
        # no ledger to read; what it needs is the HSA topology API and a kernel of its own.
        rocm_cumask_check) echo "tools/rocm_cumask_check.c" ;;

        hip_mem_paths) echo "testing/hip_mem_paths.c" ;;
        hip_props_probe) echo "testing/hip_props_probe.c" ;;
        cumask_soak) echo "testing/cumask_soak.c" ;;
        ledger_lifecycle) echo "testing/ledger_lifecycle.c" ;;

        vrocm_test) echo "common/vrocm_test.c ${COMMON_ALL}" ;;
        *) return 1 ;;
    esac
}

LIBS="libvrocm"
TOOLS="rocm_monitor rocm_cumask_check"
TESTS="hip_mem_paths hip_props_probe cumask_soak ledger_lifecycle"

run() {
    [ -n "${V:-}" ] && printf '+ %s\n' "$*" >&2
    "$@"
}

# build_lib — the preloaded shared object, compiled from the tree root so the module include paths
# resolve, and linked against nothing. The HIP headers are on the include path for type-checking
# the wrapper signatures and for offsetof(); they must not produce a DT_NEEDED entry, which is
# what `check` verifies rather than assumes.
build_lib() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown library '${name}'" >&2; return 2; }
    # shellcheck disable=SC2086  # the source list and the flag lists are word-split on purpose
    run "${CC}" -shared -fPIC -O2 ${STD} ${WARN} -fvisibility=hidden ${TLS_MODEL} ${HIP_PLATFORM} \
        "-I${HERE}" "-I${ROCM_INC}" -o "${OUT}/${name}.so" ${list}
}

# build_tool — one reader, with NO ROCm include path for rocm-monitor and one for the mask probe,
# which needs the HSA topology API. The artifacts are named with hyphens where the sources are
# named with underscores, because they are commands someone types and the readers mounted beside
# them are `ppu-monitor` and `enpu-monitor`.
build_tool() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown tool '${name}'" >&2; return 2; }

    case "${name}" in
        rocm_monitor)
            # shellcheck disable=SC2086
            run "${CC}" ${STD} -O2 ${WARN} "-I${HERE}" -o "${OUT}/${name//_/-}" ${list}
            ;;
        rocm_cumask_check)
            # shellcheck disable=SC2086
            run "${CC}" ${STD} -O2 ${WARN} ${HIP_PLATFORM} "-I${HERE}" "-I${ROCM_INC}" \
                -o "${OUT}/${name//_/-}" ${list} \
                "-L${ROCM_PATH}/lib" -lhsa-runtime64 "-Wl,-rpath,${ROCM_PATH}/lib"
            ;;
        *) echo "build.sh: no tool recipe for '${name}'" >&2; return 2 ;;
    esac
}

# build_test — one gate binary. These only ever run on an AMD host, so they link the HIP runtime
# freely: the linker then resolves the headers' R0600 macro mappings and type-checks every
# signature, where a hand-written dlsym table would be a second place to get an ABI name wrong.
build_test() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown test '${name}'" >&2; return 2; }

    case "${name}" in
        # ledger_lifecycle holds a charge and takes SIGKILL. It links common/ because what it is
        # testing IS common/'s reclaim, from a real process rather than a forked stand-in.
        ledger_lifecycle)
            # shellcheck disable=SC2086
            run "${CC}" ${STD} -O2 ${WARN} ${HIP_PLATFORM} "-I${HERE}" "-I${ROCM_INC}" \
                -o "${OUT}/${name}" ${list} ${COMMON_ALL} \
                "-L${ROCM_PATH}/lib" -lamdhip64 "-Wl,-rpath,${ROCM_PATH}/lib"
            ;;
        # cumask_soak needs a kernel to occupy CUs with, so it goes through hipcc rather than cc.
        cumask_soak)
            # shellcheck disable=SC2086
            run "${ROCM_PATH}/bin/hipcc" -O3 ${WARN} "-I${HERE}" -o "${OUT}/${name}" ${list}
            ;;
        *)
            # shellcheck disable=SC2086
            run "${CC}" ${STD} -O2 ${WARN} ${HIP_PLATFORM} "-I${HERE}" "-I${ROCM_INC}" \
                -o "${OUT}/${name}" ${list} "-L${ROCM_PATH}/lib" -lamdhip64 "-Wl,-rpath,${ROCM_PATH}/lib"
            ;;
    esac
}

# build_unit — common/'s unit tests, compiled with NO ROCm include path at all. That is itself part
# of the claim: if this needed a HIP header, common/ would not be testable without a device in the
# first place.
build_unit() {
    local list out="${1:-${OUT}/vrocm_test}" srcdir="${2:-${HERE}}"
    list="$(srcs vrocm_test)"
    # shellcheck disable=SC2086
    run "${CC}" ${STD} -O2 ${WARN} "-I${srcdir}" -o "${out}" $(
        for f in ${list}; do printf '%s ' "${srcdir}/${f}"; done
    )
}

# ---- the four linkage assertions ---------------------------------------------------------
#
# These are the product's whole external contract, and each has a failure behind it. They run here
# as well as in case 1 so a developer sees a violation at build time rather than in verification.

# max_glibc — the highest GLIBC_ version the object requires. Printed rather than compared inline
# because the comparison is a version sort, not a string one: GLIBC_2.34 sorts after GLIBC_2.4
# lexically and before it numerically, and only the second is right.
max_glibc() {
    objdump -T "$1" 2>/dev/null | grep -oE 'GLIBC_[0-9.]+' | sort -uV | tail -1
}

check_artifact() {
    local so="$1" bad=0 max needed undef exported

    if [ ! -f "${so}" ]; then
        echo "build.sh: ${so} has not been built" >&2
        return 2
    fi

    # 1. Exports only the HIP entry points it interposes. `readelf --dyn-syms` with an explicit
    # GLOBAL/DEFAULT/non-UND filter rather than `nm -D | grep`, which also matches an IMPORTED
    # symbol and would pass for any library that merely calls one.
    exported="$(readelf -W --dyn-syms "${so}" 2>/dev/null |
        awk '$4 == "FUNC" && $5 == "GLOBAL" && $6 == "DEFAULT" && $7 != "UND" { print $8 }' |
        sed 's/@.*//' | sort -u | grep -v '^hip' )"
    if [ -n "${exported}" ]; then
        echo "build.sh: ${so} exports symbols that are not interposed HIP entry points:" >&2
        printf '  %s\n' ${exported} >&2
        bad=1
    fi

    # 2. Nothing but libc. A second DT_NEEDED entry is a library the workload image may not have.
    needed="$(readelf -d "${so}" 2>/dev/null | grep NEEDED | grep -oE '\[[^]]+\]' | tr -d '[]')"
    if [ "${needed}" != "libc.so.6" ]; then
        echo "build.sh: ${so} needs '${needed}', expected exactly libc.so.6" >&2
        bad=1
    fi

    # 3. The glibc floor. dlopen/dlsym/dladdr moved into libc at GLIBC_2.34, so without their
    # .symver pins this is the assertion that fails -- which is the whole reason it exists.
    max="$(max_glibc "${so}")"
    if [ -n "${max}" ] && [ "${max}" != "GLIBC_2.4" ] &&
       [ "$(printf '%s\nGLIBC_2.4\n' "${max}" | sort -V | tail -1)" != "GLIBC_2.4" ]; then
        echo "build.sh: ${so} requires ${max}, above the GLIBC_2.4 floor" >&2
        bad=1
    fi

    # 4. No ROCm object is linked, which is what makes one build serve every ROCm version.
    undef="$(nm -D -u "${so}" 2>/dev/null | grep -cE ' (hip|hsa)[A-Za-z_]' )"
    if [ "${undef}" != "0" ]; then
        echo "build.sh: ${so} has ${undef} undefined hip*/hsa* symbols; it must resolve them at run time" >&2
        bad=1
    fi

    return "${bad}"
}

# ---- mutation checks ----------------------------------------------------------------------
#
# Each of common/'s four behavioural properties is re-run against a build broken in exactly the
# way that property forbids, and the named row must FAIL. A test that passes against the broken
# build is decoration, and this is what catches it.
mutants() {
    local tmp rc=0
    tmp="$(mktemp -d)" || return 2
    trap 'rm -rf "${tmp}"' RETURN

    # <label> <row the mutation must break> <sed program applied to common/vrocm_ledger.c>
    while IFS='|' read -r label row program; do
        [ -n "${label}" ] || continue
        local dir="${tmp}/${label}"

        mkdir -p "${dir}/common"
        cp "${HERE}"/common/*.h "${dir}/common/"
        for f in "${HERE}"/common/*.c; do cp "${f}" "${dir}/common/"; done
        sed -e "${program}" < "${HERE}/common/vrocm_ledger.c" > "${dir}/common/vrocm_ledger.c"

        if ! build_unit "${dir}/vrocm_test" "${dir}" 2>"${dir}/build.log"; then
            echo "build.sh: mutant '${label}' does not compile:" >&2
            cat "${dir}/build.log" >&2
            rc=1
            continue
        fi
        "${dir}/vrocm_test" > "${dir}/run.log" 2>&1
        if ! grep -q "^FAIL | ${row} |" "${dir}/run.log"; then
            echo "build.sh: mutant '${label}' did not fail '${row}' -- that test proves nothing" >&2
            rc=1
        fi
    # These programs are coupled to the exact source text they patch, which is a real cost and a
    # deliberate one: a mutation that stops applying is reported as "did not fail its row" rather
    # than passing quietly. That has already happened once -- changing the allocation callback's
    # signature silently un-hooked the lock-split mutant, and this message is what caught it.
    #
    # Every program below is SINGLE-LINE and address-scoped where it has to be. sed matches one
    # line at a time, so a pattern spanning `\n` silently matches nothing -- and a mutation that
    # never applied looks exactly like a property that holds. A `\n` in the REPLACEMENT is worse:
    # GNU sed inserts a newline and BSD sed inserts the letter n, so the same mutant compiles on
    # one host and not the other.
    done <<'MUTANTS'
quota-frozen|ledger/quota_reread_on_attach|s#usage->memory_quota_bytes = quota;#if (usage->memory_quota_bytes != 0) { quota = usage->memory_quota_bytes; } else { usage->memory_quota_bytes = quota; }#
lock-split|ledger/check_allocate_charge_under_one_lock|s#if (!alloc(ctx, &key, &charged)) {#ledger_unlock(device); if (!ledger_lock(device)) { slot_free(index); return VROCM_ADMIT_DENIED_CONFIG; } if (!alloc(ctx, \&key, \&charged)) {#
tracking-silent|ledger/tracking_insert_is_fail_closed|/^static int slot_reserve/,/^}/ s#return -1;#return 0;#
no-sweep|ledger/dead_charge_swept|s#if (kill((pid_t)slot->pid, 0) < 0 \&\& errno == ESRCH) {#if (0) {#
MUTANTS

    return "${rc}"
}

cd "${HERE}" || exit 1
verb="${1:-}"
shift 2>/dev/null || true

case "${verb}" in
    lib)
        names="$*"
        [ -n "${names}" ] || names="${LIBS}"
        for name in ${names}; do build_lib "${name}" || exit $?; done
        ;;
    tool)
        names="$*"
        [ -n "${names}" ] || names="${TOOLS}"
        for name in ${names}; do build_tool "${name}" || exit $?; done
        ;;
    test)
        names="$*"
        [ -n "${names}" ] || names="${TESTS}"
        for name in ${names}; do build_test "${name}" || exit $?; done
        ;;
    unit)
        case "${1:-}" in
            "") build_unit || exit $? ;;
            mutants) mutants || exit $? ;;
            *) echo "build.sh: unknown unit argument '${1}' (expect nothing or 'mutants')" >&2
               exit 2 ;;
        esac
        ;;
    check)
        names="$*"
        [ -n "${names}" ] || names="${LIBS}"
        for name in ${names}; do check_artifact "${OUT}/${name}.so" || exit $?; done
        ;;
    list)
        # Captured before printing rather than piped: a pipeline reports the LAST command's
        # status, so an unknown artifact would exit 0 through `| tr`.
        list="$(srcs "${1:-}")" || { echo "build.sh: unknown artifact '${1:-}'" >&2; exit 2; }
        # shellcheck disable=SC2086  # split on purpose: one translation unit per line
        printf '%s\n' ${list}
        ;;
    *)
        # "${HERE}/build.sh" rather than "$0": the cd above has already happened, so a script
        # invoked by a relative path can no longer read itself through $0.
        sed -n '3,10p' "${HERE}/build.sh" >&2
        exit 2
        ;;
esac
