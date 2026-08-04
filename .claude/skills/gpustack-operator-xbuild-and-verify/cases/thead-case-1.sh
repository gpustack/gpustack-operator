#!/usr/bin/env bash
#
# THEAD-CASE 1 — Shim build + linkage   (no PPU; runs on any container host)
#
#   thead-case-1.sh
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image — the same contract ascend-case-1 and nvidia-case-1 have with their own targets. What is
# left here is what a case is for: the assertions.
#
# It re-invokes the tree's own `build.sh` for the rows that are claims ABOUT the build — the
# sources compile with no diagnostics at all, not merely exit 0, and the v1 prototypes match the
# header. That script is silent when it succeeds, which is what makes "no diagnostics" decidable
# on empty output. It carries no compiler flags and no translation-unit list of its own: those
# live with the sources, so a case cannot drift from what actually ships.
#
# Per artifact it asserts:
#   - it compiles with no diagnostics at all (not merely exit 0)
#   - DT_NEEDED is empty or exactly libc.so.6 — the shim is preloaded into a container that
#     brings its own SDK, so it must never hard-link an HGGC/HGML library. This is also what
#     rules a pthread mutex or a POSIX semaphore out of common/'s lock: both need -lpthread on
#     the glibc floor below, which would put a second entry here
#   - the highest GLIBC_ version it requires is <= 2.17, the SDK's own floor; the base image tag
#     is not the compatibility guard, this is
#   - the symbols it is supposed to interpose are DEFINED and exported (GLOBAL DEFAULT,
#     non-UND) and the ones it must not define are absent. `nm -D | grep` cannot decide this:
#     it also lists the IMPORTED dlsym the hook shim calls, so it would pass for any library
#     that merely calls dlsym
#   - it carries the load marker its constructor prints, which is what case 2's control arm
#     greps to prove the library actually loaded
# and records each artifact's translation units, staged path and sha256, so the later cases
# consume exactly what this case judged.
#
# The `tools/` reader is judged differently, because it is preloaded into nothing: the same linkage
# and glibc floor (it is mounted into the same containers), no SDK include path at all, and then the
# claim that matters — it parses a usage region this case writes with `dd` from the offsets in
# `references/thead-usage-region.md`, never from a header in the tree, and refuses a layout version
# it does not know instead of misparsing it.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu, on the
#      TARGET), XB_CTR / XB_CTR_ARGS (see scripts/lib.sh).
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"

xctr_resolve || { echo "thead-case-1: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 1 — Shim build + linkage (image ${XB_IMAGE}) on $(xtarget_desc)"

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" STAGE="${XB_STAGE}" <<'PAYLOAD'
set -u
# The image is linux/amd64 only (the SDK ships targets/x86_64-linux and nothing else),
# so pin the platform rather than inheriting an arm64 caller's default.
# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
${XB_CTR} ${XB_CTR_ARGS} run --rm -i --platform linux/amd64 \
  -e "STAGE=${STAGE}" -v "${STAGE}:/work" -w /work "${IMG}" bash -s <<'INNER'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

if [ ! -x /work/build.sh ]; then
  row FAIL "shim tree staged" "/work/build.sh missing — run scripts/build.sh xbuild-thead-ppu first"
  echo "FAILS=1"; exit 0
fi

# defines <so> <symbol> — echo the section index if the symbol is a GLOBAL DEFAULT
# definition in this object, nothing if it is absent or merely referenced (UND).
defines() {
  readelf -W --dyn-syms "$1" 2>/dev/null \
    | awk -v s="$2" '{ n=$8; sub(/@.*/, "", n);
                       if (n == s && $5 == "GLOBAL" && $6 == "DEFAULT" && $7 != "UND") print $7 }' \
    | head -1
}

# linkage <path> <label> — the two claims every artifact in this tree has to satisfy whatever kind
# of artifact it is: it links nothing but libc, and it needs no glibc newer than the SDK's own
# floor. Shared by the preloaded objects and by the tools/ reader — the reader is mounted into the
# same workload containers, so it lives under the same floor, and a second copy of these two
# pipelines is exactly how the two artifact classes would drift apart.
linkage() {
  path="$1"; label="$2"

  needed="$(readelf -d "${path}" | awk '/NEEDED/ {gsub(/[][]/, "", $NF); print $NF}' | sort -u | tr '\n' ' ')"
  case "${needed}" in
    "" | "libc.so.6 ") row PASS "${label}: DT_NEEDED" "${needed:-none}" ;;
    *) row FAIL "${label}: DT_NEEDED empty or libc.so.6" "${needed}"; fails=$((fails+1)) ;;
  esac

  # Match every version component: a two-component pattern truncates GLIBC_2.2.5 to
  # GLIBC_2.2 and can only make the ceiling too lenient. `|| true` keeps an artifact that
  # requires no versioned symbol from aborting under pipefail — grep exits 1 on no match.
  max_glibc="$(readelf -W --dyn-syms "${path}" | grep -oE 'GLIBC_[0-9]+(\.[0-9]+)+' | sort -uV | tail -1 || true)"
  highest="$(printf '%s\nGLIBC_2.17\n' "${max_glibc:-GLIBC_2.2.5}" | sort -uV | tail -1)"
  if [ "${highest}" = "GLIBC_2.17" ]; then
    row PASS "${label}: glibc requirement <= 2.17" "${max_glibc:-none}"
  else
    row FAIL "${label}: glibc requirement <= 2.17" "${max_glibc}"; fails=$((fails+1))
  fi
}

# check <artifact> <symbol...> — a symbol prefixed with '!' must NOT be defined.
check() {
  base="$1"; shift
  so="/work/${base}.so"

  # Recompiled here rather than trusted from the build step, because "compiles clean" is a claim
  # this case makes and gcc does not fail on a warning. The tree's build.sh prints nothing of its
  # own on success, so anything on this stream is the compiler's.
  if cc_out="$(/work/build.sh lib "${base}" 2>&1)"; then
    if [ -n "${cc_out}" ]; then
      row FAIL "${base}: compiles clean" "the build succeeded but warned: ${cc_out}"; fails=$((fails+1))
    else
      row PASS "${base}: compiles clean" "build.sh lib ${base}, no diagnostics"
    fi
  else
    row FAIL "${base}: compiles" "${cc_out}"; fails=$((fails+1)); return
  fi

  linkage "${so}" "${base}"

  for want in "$@"; do
    case "${want}" in
      '!'*)
        sym="${want#\!}"; ndx="$(defines "${so}" "${sym}")"
        if [ -z "${ndx}" ]; then
          row PASS "${base}: does not define ${sym}" "absent or UND"
        else
          row FAIL "${base}: does not define ${sym}" "defined in section ${ndx}"; fails=$((fails+1))
        fi
        ;;
      *)
        ndx="$(defines "${so}" "${want}")"
        if [ -n "${ndx}" ]; then
          row PASS "${base}: exports ${want}" "GLOBAL DEFAULT, section ${ndx}"
        else
          row FAIL "${base}: exports ${want}" "not defined (absent, local, or UND)"; fails=$((fails+1))
        fi
        ;;
    esac
  done

  # The constructor's marker is how case 2 proves an arm's library actually loaded, so a
  # missing marker has to fail here rather than silently weaken that arm.
  if strings -a "${so}" | grep -q "${base} loaded"; then
    row PASS "${base}: load marker" "\"${base} loaded\" present"
  else
    row FAIL "${base}: load marker" "\"${base} loaded\" absent"; fails=$((fails+1))
  fi

  # Which translation units went in is part of what this case records: which half of the library
  # needs the ledger, and which needs the controller, is a fact about the product rather than a
  # build detail — and it now comes from the build itself instead of a copy kept here.
  row INFO "${base}: translation units" "$(/work/build.sh list "${base}" | tr '\n' ' ')"
  row INFO "${base}: staged" "${STAGE}/${base}.so sha256=$(sha256sum "${so}" | cut -d' ' -f1)"
}

# The hook shim interposes dlsym and nothing else: its HGML wrappers are static, so it
# must NOT export them. That is the sharpest form of the mechanism claim — visibility
# comes from the dlsym hook, not from defining HGML symbols.
#
# The '!vppu_' entries are the other half of that claim. common/ is linked in, so its symbols
# exist in the object; a preloaded library that EXPORTED them would be interposable by the
# workload, and the two halves would interpose each other's copy once both are loaded into one
# process. They are declared hidden, and this is where that is checked rather than assumed.
check hgml_dlsym_hook dlsym '!hgmlDeviceGetMemoryInfo' '!hgmlDeviceGetMemoryInfo_v2' \
  '!vppu_quota_memory_bytes' '!vppu_log_level' '!vppu_ledger_used'

# The control is the mirror image: it defines both HGML getters and must not touch dlsym. It
# links none of common/, which is what keeps it a control rather than a second copy of the
# product.
check hgml_nohook hgmlDeviceGetMemoryInfo hgmlDeviceGetMemoryInfo_v2 '!dlsym'

# Gate 1's second interposer, checked here for the same reason the control is: case 2 stacks it
# against the hook to exercise the hook's guards, and an arm is only as good as the library it
# preloads. It takes dlsym and, like the hook, keeps its own HGML wrappers static.
check dlsym_stack dlsym '!hgmlDeviceGetMemoryInfo' '!hgmlDeviceGetMemoryInfo_v2'

# The quota module interposes the DRIVER-layer ABI names, which is why hgMemAlloc_v2 AND the
# plain hgMemAlloc are both asserted: libhggc.so exports the pair, hggc.h maps the plain source
# name onto the _v2 symbol, and a shim written against the header alone therefore defines only
# one of the two. Every name libhggc.so exports on the memory path is listed, because coverage
# is the claim being made and a name silently dropped from the module looks exactly like a name
# the workload never called.
#
# The launch names carry the compute cap, and the same argument applies to them twice over: a
# launch entry left uninterposed spends the card without the controller ever seeing it, and looks
# from the outside exactly like a workload that never used that entry.
#
# It calls dlsym(RTLD_NEXT, ...) but must not define it — the one artifact here that makes the
# difference between "defines" and "references" observable, and exactly what `nm -D | grep
# dlsym` would get wrong. The '!vppu_' entries are the module's own seam: it spans six
# translation units now, so its internals have external linkage and would be interposable by
# the workload if they were not hidden.
check hggc_quota \
  hgMemAlloc_v2 hgMemAlloc hgMemAllocAsync hgMemAllocAsync_ptsz \
  hgMemAllocFromPoolAsync hgMemAllocFromPoolAsync_ptsz hgMemAllocManaged \
  hgMemAllocPitch_v2 hgMemAllocPitch hgMemCreate \
  hgMemFree_v2 hgMemFree hgMemFreeAsync hgMemFreeAsync_ptsz hgMemRelease \
  hgMemGetInfo_v2 hgMemGetInfo \
  hgMemAllocHost_v2 hgMemAllocHost hgMemFreeHost \
  hgMemMap hgMemMapArrayAsync hgMemMapArrayAsync_ptsz hgMemUnmap \
  hgMemPoolCreate hgMemPoolDestroy hgMemPoolExportPointer \
  hgMemPoolExportToShareableHandle hgMemPoolGetAccess hgMemPoolGetAttribute \
  hgMemPoolImportFromShareableHandle hgMemPoolImportPointer hgMemPoolSetAccess \
  hgMemPoolSetAttribute hgMemPoolTrimTo \
  hgGetProcAddress_v2 hgGetProcAddress hgGetExportTable \
  hgLaunchKernel hgLaunchKernel_ptsz hgLaunchKernelEx hgLaunchKernelEx_ptsz \
  hgLaunchKernelExAD hgLaunchKernelExAD_ptsz \
  hgLaunchCooperativeKernel hgLaunchCooperativeKernel_ptsz \
  hgLaunchCooperativeKernelMultiDevice hgLaunch hgLaunchGrid hgLaunchGridAsync \
  hgGraphLaunch hgGraphLaunch_ptsz hgLaunchHostFunc hgLaunchHostFunc_ptsz \
  '!dlsym' '!vppu_ledger_lock' '!vppu_ledger_charge' '!vppu_quota_memory_bytes' \
  '!vppu_hggc_admit' '!vppu_hggc_next' '!vppu_hggc_self' '!vppu_hggc_name' \
  '!vppu_hggc_gate' '!vppu_hggc_device' '!vppu_pid_step' '!vppu_ledger_control'

# The v1 ABI signatures are the one part of the module the compile above cannot type-check.
# hggc.h declares them only under __HGGC_API_VERSION_INTERNAL together with
# __HGGC_API_VERSION_UMD, and the product build must not define those: they also change
# unrelated declarations and enum members across the header. A syntax-only pass can define
# them, and then the header itself decides whether the five hand-written prototypes match —
# which is the difference between a v1 wrapper that reads its caller's arguments correctly and
# one that reads them off by whole registers.
#
# This row is verified non-vacuous: retyping one size to `unsigned long` makes it fail with
# "conflicting types for 'hgMemAlloc'". Judged on empty output rather than exit status, like
# the compile rows above, so a warning counts too.
v1_out="$(/work/build.sh check v1 2>&1)"
if [ -z "${v1_out}" ]; then
  row PASS "hggc_mem_v1: v1 prototypes match hggc.h" \
    "checked against the header's own __HGGC_API_VERSION_INTERNAL declarations"
else
  row FAIL "hggc_mem_v1: v1 prototypes match hggc.h" "${v1_out}"; fails=$((fails+1))
fi

# has_string <so> <label> <string...> — assert each string is in the object; a '!' prefix
# asserts it is absent. Reading the built object rather than the source is deliberate: it
# catches a name that was changed in a comment but not in the code.
has_string() {
  so="$1"; label="$2"; shift 2
  for want in "$@"; do
    case "${want}" in
      '!'*)
        s="${want#\!}"
        if strings -a "${so}" | grep -qF "${s}"; then
          row FAIL "${label}: no \"${s}\"" "still present"; fails=$((fails+1))
        else
          row PASS "${label}: no \"${s}\"" "absent"
        fi
        ;;
      *)
        if strings -a "${so}" | grep -qF "${want}"; then
          row PASS "${label}: \"${want}\"" "present"
        else
          row FAIL "${label}: \"${want}\"" "absent"; fails=$((fails+1))
        fi
        ;;
    esac
  done
}

# Case 3 reads the first two out of the shim's output: without the counter it cannot say a
# call crossed libhggc.so, and without the denial marker it cannot tell a refusal by this
# quota from a failure for any other reason.
#
# The rest is the injection contract. hgCtxGetDevice is the sharp one: the env name alone
# would also be present in a shim that renamed the variable and went on charging one
# container-wide total, so what proves the quota is keyed PER CARD is that the object
# resolves the current device at all. hgMemAlloc carries no device argument, so there is no
# other way to know which card an allocation belongs to.
#
# The next three are the cross-process ledger. The region magic and the default ledger path
# only exist in the object if common/'s ledger is actually linked into it, and the accounting
# being in a shared region is what stops two processes in one container each being granted the
# whole quota — which no single-process case can observe.
#
# The last three are the compute controller, and the pair matters more than either half: the
# library names libhgml.so and the utilisation entry it resolves out of it, while the DT_NEEDED
# row above says it links neither. That is the only evidence that the loop reads HGML at RUNTIME
# rather than by linkage — a shim that linked it would satisfy the string and fail DT_NEEDED, and
# one that skipped the feedback entirely would satisfy DT_NEEDED and lack the strings.
has_string /work/hggc_quota.so hggc_quota \
  'DENIED' 'hggc_quota counters:' \
  'HGGC_DEVICE_MEMORY_LIMIT_' 'LIBHGGC_LOG_LEVEL' 'hgCtxGetDevice' \
  'VPPUREGN' 'HGGC_LEDGER_PATH' '/dev/shm/vppu-ledger' \
  'HGGC_DEVICE_SM_LIMIT' 'libhgml.so' 'hgmlDeviceGetProcessUtilization' \
  '!VPPU_DEVICE_MEMORY_LIMIT_MIB'

# The visibility half is handed the device it is asked about, so its per-card evidence is
# the index lookup rather than a context query.
#
# The three ledger strings are the linkage half of "one number, not two": the figure ppu-smi
# reports for `used` comes out of the same region the enforcement half admits against, and the
# region magic and the default ledger path only exist in this object if common/'s ledger is
# actually linked into it. What the number IS belongs to case 2; that it can be read at all is
# decidable here.
has_string /work/hgml_dlsym_hook.so hgml_dlsym_hook \
  'HGGC_DEVICE_MEMORY_LIMIT_' 'LIBHGGC_LOG_LEVEL' 'hgmlDeviceGetIndex' \
  'VPPUREGN' 'HGGC_LEDGER_PATH' '/dev/shm/vppu-ledger' \
  '!VPPU_DEVICE_MEMORY_LIMIT_MIB'

# ---------------------------------------------------------------------------------------
# tools/ — the reader, and the layout it is a reader OF
# ---------------------------------------------------------------------------------------
# The reader is the one artifact here that is preloaded into nothing, so what is claimed about it
# differs. It must compile with NO SDK include path — its recipe carries none, and that is the
# claim: a monitor that needed a vendor header could not be built outside the SDK image, and one
# that linked a vendor library could not run in a container that has no device. The linkage rows
# above apply to it unchanged, because it is mounted into the same workload containers.
if tool_out="$(/work/build.sh tool ppu_monitor 2>&1)"; then
  if [ -n "${tool_out}" ]; then
    row FAIL "ppu-monitor: compiles clean with no SDK header" \
      "the build succeeded but warned: ${tool_out}"; fails=$((fails+1))
  else
    row PASS "ppu-monitor: compiles clean with no SDK header" \
      "build.sh tool ppu_monitor, no diagnostics"
  fi
else
  row FAIL "ppu-monitor: compiles" "${tool_out}"; fails=$((fails+1))
fi

linkage /work/ppu-monitor ppu-monitor
row INFO "ppu-monitor: translation units" "$(/work/build.sh list ppu_monitor | tr '\n' ' ')"
row INFO "ppu-monitor: staged" "${STAGE}/ppu-monitor sha256=$(sha256sum /work/ppu-monitor | cut -d' ' -f1)"

# Then the part that carries the most: the reader has to parse a region written from the DOCUMENTED
# OFFSETS ALONE. The bytes below come from references/thead-usage-region.md's table, not from any
# header in this tree, so a field the reader looks for in the wrong place cannot be hidden by the
# writer and the reader agreeing with each other. Card 3 is there for the 576-byte stride
# (96 + 576 * N) and the process slot for the offset every other reader will hard-code.
synth=/tmp/region-by-the-book
dd if=/dev/zero of="${synth}" bs=36960 count=1 2>/dev/null
# magic, version 1, header_bytes 96, 64 cards, 32 processes per card
printf 'VPPUREGN\x01\x00\x00\x00\x60\x00\x00\x00\x40\x00\x00\x00\x20\x00\x00\x00' \
  | dd of="${synth}" bs=1 seek=0 conv=notrunc 2>/dev/null
# card 0 at 96: quota 4096MiB, 1024MiB charged, compute limit 25%, last measured 7%
printf '\x00\x00\x00\x00\x01\x00\x00\x00\x00\x00\x00\x40\x00\x00\x00\x00\x19\x00\x00\x00\x07\x00\x00\x00' \
  | dd of="${synth}" bs=1 seek=96 conv=notrunc 2>/dev/null
# card 0's first process slot at 96 + 64: pid 4242, charged 1024MiB
printf '\x92\x10\x00\x00\x00\x00\x00\x00\x00\x00\x00\x40\x00\x00\x00\x00' \
  | dd of="${synth}" bs=1 seek=160 conv=notrunc 2>/dev/null
# card 3 at 96 + 576 * 3 = 1824: quota 2048MiB, nothing charged
printf '\x00\x00\x00\x80\x00\x00\x00\x00' | dd of="${synth}" bs=1 seek=1824 conv=notrunc 2>/dev/null

book="$(HGGC_LEDGER_PATH="${synth}" /work/ppu-monitor 2>&1)"
if echo "${book}" | grep -qF 'card=0 mem_quota_mib=4096 mem_used_mib=1024 mem_free_mib=3072 sm_limit_pct=25 sm_util_pct=7' \
   && echo "${book}" | grep -qF 'proc pid=4242 mem_mib=1024' \
   && echo "${book}" | grep -qF 'card=3 mem_quota_mib=2048' \
   && [ "$(echo "${book}" | grep -c '^card=')" = 2 ]; then
  row PASS "ppu-monitor: reads a region written from the documented offsets" \
    "both cards, the 576-byte stride, the compute limit and the process slot all land"
else
  row FAIL "ppu-monitor: reads a region written from the documented offsets" \
    "$(echo "${book}" | tr '\n' ' ' | cut -c1-300)"; fails=$((fails+1))
fi

# An unknown layout version must REFUSE rather than misparse — the reason the layout carries one at
# all. The library's own refusal is a unit test; this is the reader's, and it is the behaviour a
# scraper written against the same contract inherits.
cp "${synth}" "${synth}.v99"
printf '\x63' | dd of="${synth}.v99" bs=1 seek=8 conv=notrunc 2>/dev/null
refused="$(HGGC_LEDGER_PATH="${synth}.v99" /work/ppu-monitor 2>&1)"; rc=$?
if [ "${rc}" -ne 0 ] && echo "${refused}" | grep -q 'layout version 99' \
   && ! echo "${refused}" | grep -q '^card='; then
  row PASS "ppu-monitor: refuses a layout version it does not know" \
    "exit ${rc}, and no card figures printed"
else
  row FAIL "ppu-monitor: refuses a layout version it does not know" \
    "exit ${rc}: $(echo "${refused}" | tr '\n' ' ' | cut -c1-200)"; fails=$((fails+1))
fi

# A container nobody sliced has no region at all, and a scraper has to tell that from a corrupt
# one: absent is exit 1, unparseable is exit 2. Collapsing the two would make every unsliced
# container look like a broken ledger.
absent="$(HGGC_LEDGER_PATH=/tmp/region-that-is-not-there /work/ppu-monitor 2>&1)"; rc=$?
if [ "${rc}" -eq 1 ] && echo "${absent}" | grep -q 'has been sliced'; then
  row PASS "ppu-monitor: reports an absent region as its own outcome" \
    "exit 1, distinct from the refusal's 2"
else
  row FAIL "ppu-monitor: reports an absent region as its own outcome" \
    "exit ${rc}: $(echo "${absent}" | tr '\n' ' ' | cut -c1-200)"; fails=$((fails+1))
fi

# And the third outcome a scraper has to tell from the other two: a path that EXISTS and cannot be
# read. A directory is the cheapest reproducible one — open() succeeds on it, pread() answers
# EISDIR — and the contract files "unreadable" with "no region" (exit 1), not with "unparseable"
# (exit 2), so an I/O error must not be reported as a truncated header.
unreadable="$(HGGC_LEDGER_PATH=/work /work/ppu-monitor 2>&1)"; rc=$?
if [ "${rc}" -eq 1 ] && echo "${unreadable}" | grep -q 'cannot read the region header'; then
  row PASS "ppu-monitor: an unreadable path is exit 1, with the reason" \
    "$(echo "${unreadable}" | tr '\n' ' ' | cut -c1-140)"
else
  row FAIL "ppu-monitor: an unreadable path is exit 1, with the reason" \
    "exit ${rc} (wanted 1 and the read error named, not \"too small\"): $(echo "${unreadable}" | tr '\n' ' ' | cut -c1-200)"; fails=$((fails+1))
fi

echo "FAILS=${fails}"
INNER
PAYLOAD
)"
echo "${out}"
echo "${out}" | grep -q 'FAILS=0' && { echo "THEAD-CASE 1: PASS"; exit 0; } || { echo "THEAD-CASE 1: FAIL"; exit 1; }
