#!/usr/bin/env bash
#
# build.sh — the one place that knows how to compile this tree.
#
#   build.sh lib   [<name>...]   the shared objects that get preloaded  (default: all)
#   build.sh tool  [<name>...]   the readers under tools/               (default: all)
#   build.sh test  [<name>...]   the gate binaries under testing/       (default: all)
#   build.sh unit                common/'s unit tests -- no SDK, no device, runs anywhere
#   build.sh check v1            have hggc.h itself type-check the v1 prototypes
#   build.sh list  <name>        the translation units behind one artifact, one per line
#
# WHY THIS EXISTS. The verification cases used to carry these recipes: the translation-unit
# lists, the include roots, which artifact links the SDK and which may not. Six case scripts
# each held a copy, `hggc_mem_paths` was compiled by three of them, and two of those three had
# already drifted onto different source paths. The recipes belong with the sources they compile,
# the way `pack/gpustack-operator/external/<vendor>/build-*.sh` owns the Ascend and NVIDIA ones;
# what stays in the cases is what they are for — the assertions.
#
# WHY A SHELL SCRIPT AND NOT CMAKE. There is no dependency graph to resolve here: four shared
# objects, one reader and five gate binaries, each from a fixed list of C files, none linking
# anything but libc, no generated sources, no install step and no second architecture (the SDK
# ships x86_64-linux only). That is ten compiler invocations, which CMake would wrap in a configure
# step and a generated build tree for nothing this build has to decide — and it would add a
# dependency the compile environment does not have: `gpustack/thead-ppu-devel:2.1.1` carries make,
# gcc and hgcc, and no cmake, so every verification run would start by installing one. It would
# also cost the silence below, which is load-bearing. The repo's own convention is the same:
# every other build recipe here is shell, under `hack/` or `pack/`. A build worth CMake is the one
# this becomes if it ever grows generated code, install targets or a second target triple.
#
# IT KNOWS NOTHING ABOUT CONTAINERS. The product artifacts need the PPU SDK's headers, which
# ship only inside the vendor image, so the CALLER decides where this runs: the skill's
# `scripts/build.sh xbuild-thead-ppu` runs it inside `gpustack/thead-ppu-devel`, and on a host
# with the SDK installed it runs directly. `unit` and `tool` need neither, by design.
#
# IT IS SILENT WHEN IT SUCCEEDS, and that is load-bearing rather than terse: case 1 decides
# "compiles clean" on empty output, not on the exit status, so anything this script printed of
# its own would read as a compiler diagnostic. `V=1` prints each command for a human.
#
# Env: OUT (where artifacts land, default this directory) · PPU_HOME (SDK root, default
#      /usr/local/PPU_SDK) · CC (default gcc) · HGCC (the vendor's device compiler, default
#      hgcc) · V=1 (trace).
set -uo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
OUT="${OUT:-${HERE}}"
PPU_HOME="${PPU_HOME:-/usr/local/PPU_SDK}"
CC="${CC:-gcc}"
HGCC="${HGCC:-hgcc}"

SDK_INC="${PPU_HOME}/targets/x86_64-linux/include"
SDK_LIB="${PPU_HOME}/targets/x86_64-linux/lib"

# -I"${HERE}" is the tree root, so `#include "common/..."` and `#include "hggc/..."` resolve
# from a source in any module directory — one include root for every artifact, which is what
# keeps a module's own header from needing a relative path out of its directory.
WARN="-Wall -Wextra"
INCLUDES="-I${HERE} -I${SDK_INC}"

# The shared objects. Nothing here may link a vendor library: they are preloaded into a
# container that brings its own SDK, so every vendor symbol is resolved at runtime through the
# dlsym chain and DT_NEEDED stays empty or exactly libc.so.6. That also rules out -ldl.
# Three lists rather than one because each half links what it uses and no more: the controller's
# arithmetic goes only where something calls it, and vppu_quota.c names the gains STRUCT, which
# costs it a header and not a translation unit.
COMMON_QUOTA="common/vppu_log.c common/vppu_quota.c"
COMMON_LEDGER="${COMMON_QUOTA} common/vppu_ledger.c"
COMMON_ALL="${COMMON_LEDGER} common/vppu_pid.c"

# srcs <artifact> — the translation units behind one artifact, first one first.
srcs() {
    case "$1" in
        # The enforcement half: both quotas over the whole driver layer, in one object.
        hggc_quota) echo "hggc/hggc_quota.c hggc/hggc_mem.c hggc/hggc_mem_v1.c hggc/hggc_entry.c \
hggc/hggc_compute.c hggc/hggc_launch.c ${COMMON_ALL}" ;;
        # The visibility half. It reads the ledger — the figure ppu-smi shows is the figure the
        # quota is enforced against — but never the controller: it reports memory, not compute.
        hgml_dlsym_hook) echo "hgml/hgml_dlsym_hook.c ${COMMON_LEDGER}" ;;
        # Gate 1's negative control: the same HGML symbols with no dlsym hook. It links none of
        # common/, which is what keeps it a control rather than a second copy of the product.
        hgml_nohook) echo "testing/hgml_nohook.c" ;;
        # Gate 1's second interposer, so the hook's guards are exercised against a peer that
        # takes dlsym too. Links none of common/ for the same reason the control does not.
        dlsym_stack) echo "testing/dlsym_stack.c" ;;

        # The usage reader. It links none of common/ on purpose: the ledger's read helper creates
        # the region when none exists and its other entries take the card's lock, and a reader must
        # do neither. It takes the layout from the header and maps it read-only itself.
        ppu_monitor) echo "tools/ppu_monitor.c" ;;

        hgml_util_probe) echo "testing/hgml_util_probe.c" ;;
        hggc_mem_paths) echo "testing/hggc_mem_paths.c" ;;
        dlsym_origin) echo "testing/dlsym_origin.c" ;;
        hggc_launch_load) echo "testing/hggc_launch_load.cu" ;;
        vppu_test) echo "common/vppu_test.c ${COMMON_ALL}" ;;
        *) return 1 ;;
    esac
}

LIBS="hggc_quota hgml_dlsym_hook hgml_nohook dlsym_stack"
TOOLS="ppu_monitor"
TESTS="hgml_util_probe hggc_mem_paths dlsym_origin hggc_launch_load"

run() {
    [ -n "${V:-}" ] && printf '+ %s\n' "$*" >&2
    "$@"
}

# build_lib — one preloaded shared object, compiled from the tree root so the module include
# paths resolve, and linked against nothing.
build_lib() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown library '${name}'" >&2; return 2; }
    # shellcheck disable=SC2086  # the source list and the flag lists are word-split on purpose
    run "${CC}" -shared -fPIC -O2 ${WARN} ${INCLUDES} -o "${OUT}/${name}.so" ${list}
}

# build_tool — one reader from tools/, with NO SDK include path and no vendor library, exactly
# like build_unit and for the same kind of reason: a monitor that needed the vendor's headers
# could not be built without the SDK image, and one that linked its libraries could not run in a
# container that has no device. The artifact is named with hyphens (`ppu-monitor`) where the source
# is named with underscores, because it is a command someone types and the Ascend reader mounted
# beside it is `enpu-monitor`.
build_tool() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown tool '${name}'" >&2; return 2; }
    # shellcheck disable=SC2086
    run "${CC}" -std=gnu11 -O2 ${WARN} "-I${HERE}" -o "${OUT}/${name//_/-}" ${list}
}

# build_test — one gate binary. These only ever run inside the vendor image, so they link the
# SDK freely: the linker then resolves the headers' _v2/_v4 macro mappings and type-checks every
# signature, where a hand-written dlsym table would be a second place to get an ABI name wrong.
build_test() {
    local name="$1" list
    list="$(srcs "${name}")" || { echo "build.sh: unknown test '${name}'" >&2; return 2; }

    case "${name}" in
        # A kernel is the only way to occupy a PPU, so this one goes through the vendor's own
        # device compiler. It links libhgml to report its own per-process utilisation.
        hggc_launch_load)
            # shellcheck disable=SC2086
            run "${HGCC}" -O2 "-I${SDK_INC}" -o "${OUT}/${name}" ${list} "-L${SDK_LIB}" -lhgml
            ;;
        # dlsym_origin asks the loader which object won a symbol, so it needs libdl and no SDK.
        dlsym_origin)
            # shellcheck disable=SC2086
            run "${CC}" -O2 ${WARN} -o "${OUT}/${name}" ${list} -ldl
            ;;
        hgml_util_probe)
            # shellcheck disable=SC2086
            run "${CC}" -O2 ${WARN} "-I${SDK_INC}" -o "${OUT}/${name}" ${list} \
                "-L${SDK_LIB}" -lhgml -lhggc -ldl
            ;;
        hggc_mem_paths)
            # shellcheck disable=SC2086
            run "${CC}" -O2 ${WARN} "-I${SDK_INC}" -o "${OUT}/${name}" ${list} \
                "-L${SDK_LIB}" -lhggc -lhggcrt1
            ;;
        *) echo "build.sh: no test recipe for '${name}'" >&2; return 2 ;;
    esac
}

# build_unit — common/'s unit tests, compiled with NO SDK include path at all. That is itself
# part of the claim: if this needed a vendor header, common/ would not be testable without a
# device in the first place.
build_unit() {
    local list
    list="$(srcs vppu_test)"
    # shellcheck disable=SC2086
    run "${CC}" -std=gnu11 -O2 ${WARN} "-I${HERE}" -o "${OUT}/vppu_test" ${list}
}

# check_v1 — the one part of the module a normal compile cannot type-check. The v1 ABI names are
# reached by #undef'ing the header's mapping onto the versioned ones, which removes the header's
# own check; hggc.h declares those prototypes itself, but only under both of these macros, which
# the product must not define because they also change unrelated declarations across the header.
# A syntax-only pass may define them. BOTH are required: with only the first, the declarations
# stay invisible and the check passes for no reason.
check_v1() {
    # shellcheck disable=SC2086
    run "${CC}" -fsyntax-only ${WARN} \
        -D__HGGC_API_VERSION_INTERNAL -D__HGGC_API_VERSION_UMD \
        ${INCLUDES} "${HERE}/hggc/hggc_mem_v1.c"
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
        build_unit || exit $?
        ;;
    check)
        case "${1:-}" in
            v1) check_v1 || exit $? ;;
            *) echo "build.sh: unknown check '${1:-}' (expect v1)" >&2; exit 2 ;;
        esac
        ;;
    list)
        # Captured before printing rather than piped: a pipeline reports the LAST command's
        # status, so an unknown artifact would exit 0 through `| tr`.
        list="$(srcs "${1:-}")" || { echo "build.sh: unknown artifact '${1:-}'" >&2; exit 2; }
        # shellcheck disable=SC2086  # split on purpose: one translation unit per line
        printf '%s\n' ${list}
        ;;
    *)
        sed -n '3,10p' "$0" >&2
        exit 2
        ;;
esac
