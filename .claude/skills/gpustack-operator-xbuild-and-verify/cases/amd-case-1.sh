#!/usr/bin/env bash
#
# AMD-CASE 1 — Shim build + linkage   (no GPU; no container runtime where ROCm is installed)
#
#   amd-case-1.sh
#
# Assumes `build.sh xbuild-amd-rocm` already staged the shim tree and compiled it — the same
# contract the other three CASE-1s have with their own targets. What is left here is what a case
# is for: the assertions.
#
# It re-invokes the tree's own `build.sh` for the rows that are claims ABOUT the build — the
# sources compile with no diagnostics at all, not merely exit 0 — and for the four linkage
# assertions the tree carries itself. That script is silent when it succeeds, which is what makes
# "no diagnostics" decidable on empty output. This case carries no compiler flags and no
# translation-unit list of its own: those live with the sources, so a case cannot drift from what
# ships.
#
# EVERY RECOMPILE HERE GOES TO A SCRATCH DIRECTORY, and the linkage rows then judge the STAGED
# artifacts rather than the scratch ones. Two reasons, and both are load-bearing. The staged
# artifacts are what cases 2..7 load, so a case that rebuilt over them would be verifying
# something no other case runs; and on the in-place route the staged files were written by a
# container running as root while this case runs as the login user, so a rebuild in place would
# fail on permissions rather than on anything about the code.
#
# WHY THE FOUR ASSERTIONS ARE MADE TWICE. `build.sh check` runs them at build time so a developer
# sees a violation before verification; this case runs its own `readelf` pipelines over the staged
# object so the suite does not inherit its verdict from the thing it is verifying. The export row
# is where the two differ in substance: the tree's own check asks only that nothing NON-HIP is
# exported, while this case asserts the exact set — which is the half that catches an entry
# silently dropped from a family, and a dropped entry looks from the outside exactly like an entry
# no workload happened to call.
#
# WHY `readelf -W --dyn-syms` AND NEVER `nm -D | grep`. The latter also lists a symbol the object
# merely IMPORTS, so it would pass for any library that calls the entry point instead of defining
# it — which is precisely the failure this case exists to catch. Every export row here requires
# `GLOBAL DEFAULT` with a non-`UND` section.
#
# It runs in the ROCm devel image where a container runtime resolves, and in place against
# ${XB_ROCM_PATH} where none does — the same rule `scripts/build.sh xbuild-amd-rocm` follows, so
# the case reaches every target the build step reaches. It needs no GPU either way.
#
# Env: XB_IMAGE (default rocm/dev-ubuntu-22.04:7.2.4), XB_STAGE (default /tmp/vrocm, on the
#      TARGET), XB_ROCM_PATH (default /opt/rocm), XB_CTR / XB_CTR_ARGS (see scripts/lib.sh;
#      XB_CTR=none forces the in-place route).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-rocm/dev-ubuntu-22.04:7.2.4}"
XB_STAGE="${XB_STAGE:-/tmp/vrocm}"
XB_ROCM_PATH="${XB_ROCM_PATH:-/opt/rocm}"
XB_PLATFORM="${XB_PLATFORM:-linux/amd64}"   # ROCm ships no aarch64 user space

if [ "${XB_CTR}" != none ] && xctr_resolve; then
  use_ctr=yes
  echo "# AMD-CASE 1 — shim build + linkage in ${XB_IMAGE} on $(xtarget_desc)"
else
  use_ctr=no
  echo "# AMD-CASE 1 — shim build + linkage in place against ${XB_ROCM_PATH} on $(xtarget_desc)"
fi

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" STAGE="${XB_STAGE}" \
           ROCM="${XB_ROCM_PATH}" PLATFORM="${XB_PLATFORM}" USE_CTR="${use_ctr}" <<'PAYLOAD'
set -u
# One body, two routes. The heredoc hangs off the `fi` so both branches read the same script:
# a second copy is how the container arm and the in-place arm would come to assert different
# things about the same artifacts.
# WORK is where the tree IS from inside, STAGE is where it is on the target. They differ on the
# container route, and every recorded path is the STAGE one: a later case is pointed at the
# target's filesystem, not at a mount that existed for the length of one `run`.
if [ "${USE_CTR}" = yes ]; then
  # shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
  ${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform "${PLATFORM}" \
    -e WORK=/work -e "STAGE=${STAGE}" -e ROCM_PATH=/opt/rocm \
    -v "${STAGE}:/work" -w /work "${IMG}" bash -s
else
  WORK="${STAGE}" STAGE="${STAGE}" ROCM_PATH="${ROCM}" bash -s
fi <<'INNER'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

if [ ! -x "${WORK}/build.sh" ]; then
  row FAIL "shim tree staged" "${STAGE}/build.sh missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=1"; exit 0
fi

# Every ELF assertion in this case reads the object through one of these. A tool that is not
# installed answers with nothing, and each assertion reads that empty answer as "nothing wrong":
# `nm -D -u | grep -c` counts zero, the version list has no entry above GLIBC_2.4, the export
# list is empty. That is a whole green case built on a missing package, so stop here instead.
for t in readelf nm strings; do
  if ! command -v "${t}" > /dev/null 2>&1; then
    row FAIL "binutils available" "${t} is not installed in this image — every ELF assertion below would pass without reading anything"
    echo "FAILS=1"; exit 0
  fi
done

SCRATCH="$(mktemp -d)"
trap 'rm -rf "${SCRATCH}"' EXIT

# compiles <verb> [args...] — one build.sh verb, judged on EMPTY OUTPUT rather than exit status.
# gcc does not fail on a warning, and the tree's build.sh prints nothing of its own when it
# succeeds, so anything on this stream is a compiler diagnostic.
compiles() {
  local verb="$1" label="$2"; shift 2
  local cc_out
  if cc_out="$(OUT="${SCRATCH}" "${WORK}/build.sh" "${verb}" "$@" 2>&1)"; then
    if [ -n "${cc_out}" ]; then
      row FAIL "${label}: compiles clean" "the build succeeded but warned: $(echo "${cc_out}" | tr '\n' ' ' | cut -c1-300)"
      fails=$((fails+1))
    else
      row PASS "${label}: compiles clean" "build.sh ${verb} $*, no diagnostics"
    fi
  else
    row FAIL "${label}: compiles" "$(echo "${cc_out}" | tr '\n' ' ' | cut -c1-300)"
    fails=$((fails+1))
  fi
}

compiles lib  "libvrocm"
compiles tool "tools"
compiles test "testing"
compiles unit "common unit tests"

# The tree's own four assertions, over the object this case just built. They run at build time so
# a developer sees a violation before verification ever starts; running them here as well is what
# makes "the build is still self-checking" a row rather than an assumption.
if check_out="$(OUT="${SCRATCH}" "${WORK}/build.sh" check 2>&1)"; then
  if [ -n "${check_out}" ]; then
    row FAIL "build.sh check" "exited 0 but printed: $(echo "${check_out}" | tr '\n' ' ' | cut -c1-300)"
    fails=$((fails+1))
  else
    row PASS "build.sh check" "the tree's own four linkage assertions hold on a fresh build"
  fi
else
  row FAIL "build.sh check" "$(echo "${check_out}" | tr '\n' ' ' | cut -c1-300)"
  fails=$((fails+1))
fi

# Each of common/'s four behavioural properties re-run against a build broken in exactly the way
# that property forbids, with the named row required to fail. A test that passes against the
# broken build is decoration, and this is what catches it. Silent on success, like every other
# verb, so the row is decidable on empty output.
if mut_out="$(OUT="${SCRATCH}" "${WORK}/build.sh" unit mutants 2>&1)" && [ -z "${mut_out}" ]; then
  row PASS "build.sh unit mutants" "each of the four mutants still fails its own named row"
else
  row FAIL "build.sh unit mutants" "$(echo "${mut_out}" | tr '\n' ' ' | cut -c1-300)"
  fails=$((fails+1))
fi

# ---------------------------------------------------------------------------------------
# The staged artifacts — what cases 2..7 actually load
# ---------------------------------------------------------------------------------------

# exported <path> — every symbol this object DEFINES and exports, version tags stripped, one per
# line. GLOBAL DEFAULT with a non-UND section is the whole point: an imported symbol is UND and
# must not count.
# LC_ALL=C, because this list is compared against a literal written in one order and diffed with
# `comm`, which requires both sides in the same collation. Measured, glibc gives these 26 names
# the same order under C, C.UTF-8, en_US.UTF-8 and POSIX, so this pins an order rather than
# changing one — but the list grows with the interception table, and a name carrying `_` or
# differing only in case is where collations start to disagree.
exported() {
  readelf -W --dyn-syms "$1" 2>/dev/null |
    awk '$4 == "FUNC" && $5 == "GLOBAL" && $6 == "DEFAULT" && $7 != "UND" { n = $8; sub(/@.*/, "", n); print n }' |
    LC_ALL=C sort -u
}

# needed_is_libc <path> <label> — DT_NEEDED is exactly libc.so.6.
#
# This is the whole basis of the claim that one build serves every ROCm version: a second entry is
# a library the workload image may not have, and a ROCm entry would tie the artifact to the
# runtime it was built beside. It is also what rules a pthread mutex or a POSIX semaphore out of
# common/'s lock — both need a second entry on the glibc floor below.
needed_is_libc() {
  local path="$1" label="$2" needed
  needed="$(readelf -d "${path}" 2>/dev/null | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort -u | tr '\n' ' ')"
  if [ "${needed}" = "libc.so.6 " ]; then
    row PASS "${label}: DT_NEEDED is exactly libc.so.6" "${needed}"
  else
    row FAIL "${label}: DT_NEEDED is exactly libc.so.6" "${needed:-none}"; fails=$((fails+1))
  fi
}

# glibc_floor <path> <label> — no GLIBC_ requirement above 2.4.
#
# Held by three `.symver` pins rather than by the base image's tag, so a current devel image
# cannot silently raise the floor that Ubuntu 20.04 and RHEL 8 workload images depend on. The
# comparison is a VERSION sort, not a string one: GLIBC_2.34 sorts after GLIBC_2.4 lexically and
# before it numerically, and only the second reading is right. Every component is matched, because
# a two-component pattern truncates GLIBC_2.2.5 to GLIBC_2.2 and can only make the ceiling too
# lenient.
glibc_floor() {
  local path="$1" label="$2" max highest
  max="$(readelf -W --dyn-syms "${path}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
  highest="$(printf '%s\nGLIBC_2.4\n' "${max:-GLIBC_2.2.5}" | sort -uV | tail -1)"
  if [ "${highest}" = "GLIBC_2.4" ]; then
    row PASS "${label}: glibc requirement <= 2.4" "${max:-none}"
  else
    row FAIL "${label}: glibc requirement <= 2.4" "${max} — the .symver pins on dlopen/dlsym/dladdr are what hold this"
    fails=$((fails+1))
  fi
}

# glibc_note <path> <label> — the same figure, RECORDED rather than asserted.
#
# For an EXECUTABLE the floor is not a property of the code and cannot be held by pinning
# anything: `__libc_start_main` was re-versioned at GLIBC_2.34 and `fstat` became a real exported
# symbol at 2.33, so every binary built in a glibc-2.35 image carries both whatever it calls. The
# shared object escapes only because it has no startup stub of its own. The consequence is a
# deployment fact rather than a defect, and it is worth a line: `libvrocm.so` loads into an
# Ubuntu 20.04 or RHEL 8 workload image and a reader built beside it does not, so a reader meant
# to run inside one has to be built on an older base.
glibc_note() {
  local path="$1" label="$2" max
  max="$(readelf -W --dyn-syms "${path}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
  row INFO "${label}: glibc requirement" \
    "${max:-none} — an executable's floor comes from its startup stub, not from what it calls; only the preloaded object can hold GLIBC_2.4"
}

# no_rocm_undef <path> <label> — zero hip*/hsa* symbols among the UNDEFINED ones.
#
# `nm -D -u` lists only undefined symbols, which is the one place a `grep` over `nm` output means
# what it looks like it means. An artifact carrying any of these resolves ROCm by linkage rather
# than through the resolver at run time, and one build would then stop serving every version.
no_rocm_undef() {
  local path="$1" label="$2" undef
  undef="$(nm -D -u "${path}" 2>/dev/null | grep -cE ' (hip|hsa)[A-Za-z_]')"
  if [ "${undef}" = "0" ]; then
    row PASS "${label}: no undefined hip*/hsa* symbols" "resolved at run time, not by linkage"
  else
    row FAIL "${label}: no undefined hip*/hsa* symbols" "${undef} present"; fails=$((fails+1))
  fi
}

# has_string <path> <label> <string...> — assert each string is in the built object. Reading the
# object rather than the source is deliberate: it catches a name changed in a comment but not in
# the code.
has_string() {
  local path="$1" label="$2" want; shift 2
  for want in "$@"; do
    if strings -a "${path}" | grep -qF "${want}"; then
      row PASS "${label}: \"${want}\"" "present"
    else
      row FAIL "${label}: \"${want}\"" "absent"; fails=$((fails+1))
    fi
  done
}

record() {
  local path="$1" label="$2" unit="$3"
  row INFO "${label}: translation units" "$("${WORK}/build.sh" list "${unit}" | tr '\n' ' ')"
  row INFO "${label}: staged" "${STAGE}/$(basename "${path}") sha256=$(sha256sum "${path}" | cut -d' ' -f1)"
}

LIB="${WORK}/libvrocm.so"
if [ ! -f "${LIB}" ]; then
  row FAIL "libvrocm staged" "${STAGE}/libvrocm.so missing — run scripts/build.sh xbuild-amd-rocm first"
  echo "FAILS=$((fails+1))"; exit 0
fi

# The exact interposed surface. Asserted as a SET rather than as a membership test, and that is
# what earns the row: the tree's own check already refuses a non-HIP export, so what only this
# form can see is an entry that stopped being compiled in — and a dropped entry looks from the
# outside exactly like an entry no workload happened to call. Every name here is a door somebody
# measured open. The last five came from one method rather than from five hunches: listing every
# allocating name `libamdhip64` exports for `references/amd-hip-symbol-manifest.md` and subtracting
# the interposed set left `hipMalloc3D`, `hipMemCreate` — the virtual-memory-management sequence a
# tuned PyTorch job allocates through — and the DRIVER-API halves `hipMemAllocPitch`,
# `hipArrayCreate` and `hipArray3DCreate`, which are separate symbols from their runtime-API twins.
# Each was then measured taking 512 MiB out of a 64 MiB quota before it was closed.
#
# `hipGetDeviceProperties` and `hipGetDevicePropertiesR0000` are here because ROCm's own header
# does not declare either under a ROCm 6+ build: the plain name is macro-mapped away before the
# header can declare it, and R0000 is not declared at all. Both compiled clean, exported nothing
# and interposed nothing until the wrappers were given explicit default visibility, so their
# presence in this set is the regression test for that.
WANT="hipArray3DCreate
hipArrayCreate
hipArrayDestroy
hipDeviceTotalMem
hipExtMallocWithFlags
hipFree
hipFreeArray
hipFreeAsync
hipGetDeviceProperties
hipGetDevicePropertiesR0000
hipGetDevicePropertiesR0600
hipHostFree
hipHostMalloc
hipMalloc
hipMalloc3D
hipMalloc3DArray
hipMallocArray
hipMallocAsync
hipMallocFromPoolAsync
hipMallocManaged
hipMallocPitch
hipMemAllocPitch
hipMemCreate
hipMemGetInfo
hipMemPoolImportPointer
hipMemRelease"

got="$(exported "${LIB}")"
if [ "${got}" = "${WANT}" ]; then
  row PASS "libvrocm: exports exactly the interposed entry points" \
    "$(echo "${WANT}" | wc -l | tr -d ' ') names, GLOBAL DEFAULT and defined"
else
  # Same collation as the sort that produced `got`, or comm reports names as both missing and
  # unexpected on inputs that differ only in the order it reads them.
  missing="$(LC_ALL=C comm -23 <(echo "${WANT}") <(echo "${got}") | tr '\n' ' ')"
  extra="$(LC_ALL=C comm -13 <(echo "${WANT}") <(echo "${got}") | tr '\n' ' ')"
  row FAIL "libvrocm: exports exactly the interposed entry points" \
    "missing: ${missing:-none} · unexpected: ${extra:-none}"
  fails=$((fails+1))
fi

needed_is_libc "${LIB}" libvrocm
glibc_floor "${LIB}" libvrocm
no_rocm_undef "${LIB}" libvrocm

# The load marker is how cases 2 and 3 prove an arm's library actually loaded, and the counter
# prefix is how they read which entries fired, so a missing string has to fail here rather than
# silently weaken those arms. The env names are the injection contract; the sonames are the
# resolver's fallback list, and their presence beside a DT_NEEDED of exactly libc.so.6 is the only
# evidence that the runtime is reached at RUN TIME rather than by linkage — a library that linked
# it would satisfy the string and fail DT_NEEDED, and one that never resolved anything would
# satisfy DT_NEEDED and lack the strings.
has_string "${LIB}" libvrocm \
  'loaded' 'counter ' 'cannot resolve' \
  'VROCM_DEVICE_MEMORY_LIMIT' 'VROCM_LEDGER_PATH' 'LIBVROCM_LOG_LEVEL' \
  'libamdhip64.so.7' 'libamdhip64.so.6' 'VROCMRGN'

record "${LIB}" libvrocm libvrocm

# The reader is preloaded into nothing, so what is claimed about it differs. Two of the three
# rows still apply and are the reason it can be run at all where the slice is: it links nothing
# but libc, and it resolves no ROCm symbol — a monitor that did either could not run in a
# container that has no device, which is exactly where a metrics scraper runs. The glibc figure is
# recorded rather than asserted, for the reason `glibc_note` states.
MON="${WORK}/rocm-monitor"
if [ -x "${MON}" ]; then
  needed_is_libc "${MON}" rocm-monitor
  glibc_note "${MON}" rocm-monitor
  no_rocm_undef "${MON}" rocm-monitor
  record "${MON}" rocm-monitor rocm_monitor
else
  row FAIL "rocm-monitor staged" "${STAGE}/rocm-monitor missing — run scripts/build.sh xbuild-amd-rocm first"
  fails=$((fails+1))
fi

# The mask probe is the one artifact in this tree that SHOULD link ROCm: it reads the HSA topology
# API and carries a kernel of its own, so it is a developer tool that runs beside the runtime
# rather than something preloaded into a workload. Its linkage is recorded rather than asserted,
# and recording it is what would make a change to that shape visible.
CHK="${WORK}/rocm-cumask-check"
if [ -x "${CHK}" ]; then
  row INFO "rocm-cumask-check: DT_NEEDED" \
    "$(readelf -d "${CHK}" | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort | tr '\n' ' ')— links ROCm on purpose"
  record "${CHK}" rocm-cumask-check rocm_cumask_check
else
  row FAIL "rocm-cumask-check staged" "${STAGE}/rocm-cumask-check missing — run scripts/build.sh xbuild-amd-rocm first"
  fails=$((fails+1))
fi

# The gate binaries, recorded so cases 2..7 can be read against exactly what this case judged.
for t in hip_mem_paths hip_props_probe cumask_soak ledger_lifecycle vrocm_test; do
  if [ -x "${WORK}/${t}" ]; then
    row INFO "${t}: staged" "${STAGE}/${t} sha256=$(sha256sum "${WORK}/${t}" | cut -d' ' -f1)"
  else
    row FAIL "${t} staged" "${STAGE}/${t} missing — run scripts/build.sh xbuild-amd-rocm first"
    fails=$((fails+1))
  fi
done

echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
# The verdict is the payload's own count, read as a NUMBER off the last `FAILS=` line. Matching
# the token anywhere in the output would let any row satisfy the verdict by printing it in a
# detail column, and a payload that died before printing the line has to read as failure.
total="$(echo "${out}" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | tail -1)"
[ "${total:-1}" -eq 0 ] && { echo "AMD-CASE 1: PASS"; exit 0; } || { echo "AMD-CASE 1: FAIL"; exit 1; }
