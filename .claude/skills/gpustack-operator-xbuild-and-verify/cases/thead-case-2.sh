#!/usr/bin/env bash
#
# THEAD-CASE 2 — Gate 1: ppu-smi slice visibility   (mechanism rows need no PPU)
#
#   thead-case-2.sh
#
# Consumes the artifacts cases/thead-case-1.sh staged and answers Gate 1 in two groups.
#
# MECHANISM (no hardware). ppu-smi reaches HGML by dlopen plus dlsym on the explicit
# handle, and dlsym's return address says which object won. A tiny inline program does the
# same dlopen and reports, via dladdr, where each memory getter resolved:
#   - with hgml_dlsym_hook.so preloaded, both getters must resolve INSIDE the shim;
#   - with hgml_nohook.so preloaded they must still resolve inside libhgml.so, which is
#     the spec's central claim — defining the HGML symbols alone is inert against a
#     dlopen-based lookup — and it is checkable without a card;
#   - with nothing preloaded, libhgml.so, as the reference point for the other two;
#   - with a SECOND dlsym interposer stacked beside the hook, both orders: the front one wins the
#     outer resolution and is the ONLY one in the chain — two libraries that interpose dlsym with
#     a versioned dlvsym step over each other rather than chaining, so the one behind is loaded
#     and never entered, and no lookup can hand a peer a pointer into the hook.
#
# VISIBILITY (needs a real PPU). Every preload CONTAINER-SCOPED via LD_PRELOAD; the host
# /etc/ld.so.preload is never written:
#   - baseline, no preload: records the card's physical figures, so the arms are compared
#     against measured values and not the literal 98304;
#   - arm (a) hgml_dlsym_hook.so: the card's memory field must equal the quota EXACTLY,
#     and the shim's interception marker must be present;
#   - arm (b) hgml_nohook.so: must equal the baseline AND prove its own library loaded —
#     without that proof, a control that silently failed to load also shows the physical
#     value and would pass for the wrong reason, which destroys the control;
#   - arm (c) hook plus the vendor libhggc_wrapper.so: no recursion, no deadlock,
#     timeout-bounded;
#   - arms (d) and (e) hook plus the second interposer, both orders: the quota when the hook is in
#     front, the card's PHYSICAL figure when the peer is — the ordering constraint the injection
#     contract now carries, pinned here as a measured fact;
#   - arm (f) the used side: one process spends half the quota under the enforcement shim and
#     holds it, and ppu-smi under the visibility shim must report THAT figure — the one number
#     both halves work from, read out of the shared region rather than from the vendor.
# Every row is decided by PARSED OUTPUT: ppu-smi exits 0 even when it prints
# "init HGML error: driver is not loaded", so its exit status carries no verdict.
#
# Every run here injects HGGC_DEVICE_SM_LIMIT=100 beside the memory figure. The library refuses
# every allocation while the container's quota is incomplete, and the compute figure became part
# of that once the controller landed; 100 is configured-and-uncapping, so nothing this case
# measures changes.
#
# Assumes `build.sh xbuild-thead-ppu` already staged the shim tree and compiled it inside the SDK
# image; this case only inspects and runs what that produced. Edit a source, re-run the build step.
#
# Env: XB_IMAGE (default gpustack/thead-ppu-devel:2.1.1), XB_STAGE (default /tmp/vppu,
#      on the TARGET), XB_PPU_CARD (default: the first idle card), XB_PPU_IDLE_MIB
#      (default 64), XB_PPU_QUOTA_MIB (default 4096), XB_CTR / XB_CTR_ARGS.
# Prints a STATUS | CHECK | DETAIL table; exits non-zero on any FAIL.
set -uo pipefail
HERE="$(cd "$(dirname "$0")" && pwd)"
# shellcheck source=../scripts/lib.sh
. "${HERE}/../scripts/lib.sh"

XB_IMAGE="${XB_IMAGE:-gpustack/thead-ppu-devel:2.1.1}"
XB_STAGE="${XB_STAGE:-/tmp/vppu}"
XB_PPU_QUOTA_MIB="${XB_PPU_QUOTA_MIB:-4096}"

xctr_resolve || { echo "thead-case-2: no container runtime on $(xtarget_desc)"; exit 2; }

echo "# THEAD-CASE 2 — Gate 1 ppu-smi visibility (image ${XB_IMAGE}) on $(xtarget_desc)"

CARD="${XB_PPU_CARD:-}"
if [ -z "${CARD}" ]; then
  CARD="$(thead_idle_cards | head -1)"
fi

out="$(xsh XB_CTR="${XB_CTR}" XB_CTR_ARGS="${XB_CTR_ARGS}" IMG="${XB_IMAGE}" \
  STAGE="${XB_STAGE}" CARD="${CARD}" QUOTA="${XB_PPU_QUOTA_MIB}" <<'PAYLOAD'
set -u
row() { printf '%s | %s | %s\n' "$1" "$2" "$3"; }
fails=0

HOOK="${STAGE}/hgml_dlsym_hook.so"
NOHOOK="${STAGE}/hgml_nohook.so"
STACK="${STAGE}/dlsym_stack.so"
PROBE="${STAGE}/dlsym_origin"
# The enforcement shim and the memory workload are needed by one row only — the one that spends
# quota in one process and reads it back with ppu-smi in another — but the build step produces
# every artifact in one pass, so a missing one means it never ran rather than that this row
# cannot be judged.
QUOTA_SO="${STAGE}/hggc_quota.so"
MEM_PATHS="${STAGE}/hggc_mem_paths"
for a in "${HOOK}" "${NOHOOK}" "${STACK}" "${PROBE}" "${QUOTA_SO}" "${MEM_PATHS}"; do
  if [ ! -f "${a}" ]; then
    row FAIL "artifacts staged" "${a} missing — run scripts/build.sh xbuild-thead-ppu first"
    echo "FAILS=1"; exit 0
  fi
done
row INFO "artifacts staged" "hook sha256=$(sha256sum "${HOOK}" | cut -c1-16)… control sha256=$(sha256sum "${NOHOOK}" | cut -c1-16)…"

# The mechanism probe is testing/dlsym_origin.c, built by the build step like every other
# artifact here. It resolves the two getters exactly the way ppu-smi does and reports, per
# symbol, which object the returned address lives in; dladdr is what makes that unambiguous.
# shellcheck disable=SC2086  # XB_CTR_ARGS is word-split on purpose
CTR_RUN="${XB_CTR} ${XB_CTR_ARGS} run --rm --platform linux/amd64 -v ${STAGE}:/work -w /work"

# mech <label> <preload-or-empty> <expected-object-substring>
mech() {
  local label="$1" preload="$2" expect="$3" env_args="" got=""
  [ -n "${preload}" ] && env_args="-e LD_PRELOAD=${preload}"
  # shellcheck disable=SC2086
  got="$(${CTR_RUN} ${env_args} -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" -e "LIBHGGC_LOG_LEVEL=2" \
    "${IMG}" ./dlsym_origin 2>&1)"

  local n_ok=0 n=0 line origin
  while IFS= read -r line; do
    case "${line}" in
      "MECH hgmlDeviceGetMemoryInfo"*)
        n=$((n+1)); origin="${line##*origin=}"
        case "${origin}" in *"${expect}"*) n_ok=$((n_ok+1)) ;; esac
        ;;
    esac
  done <<< "${got}"

  if [ "${n}" -eq 2 ] && [ "${n_ok}" -eq 2 ]; then
    row PASS "mechanism: ${label}" "both getters resolve in ${expect}"
  else
    row FAIL "mechanism: ${label}" "expected both getters in ${expect}; got: $(echo "${got}" | tr '\n' ' ')"
    fails=$((fails+1))
  fi

  # The control arm is only a control if it was actually loaded.
  case "${preload}" in
    *hgml_nohook.so*)
      if echo "${got}" | grep -q 'hgml_nohook loaded'; then
        row PASS "mechanism: control proved loaded" "constructor marker present"
      else
        row FAIL "mechanism: control proved loaded" "no marker — the control may not have loaded at all"
        fails=$((fails+1))
      fi
      ;;
  esac
}

mech "no preload" "" "libhgml.so"
mech "hook preloaded" "/work/hgml_dlsym_hook.so" "hgml_dlsym_hook.so"
mech "control preloaded (defining the symbols alone is inert)" "/work/hgml_nohook.so" "libhgml.so"

# ---- the hook stacked with a second dlsym interposer, in both orders ----
#
# Arm (c) below preloads the vendor's libhggc_wrapper.so, which interposes no dlsym at all: it
# proves one peer in one order, not that no order recurses. testing/dlsym_stack.so is a peer that
# does take dlsym — the legal way, with dlvsym(RTLD_NEXT, "dlsym", "GLIBC_x.y"), which is the only
# way to interpose dlsym without calling yourself — and wraps the same two getters, so it is in
# the call chain and not only in the resolution chain.
#
# WHAT THIS MEASURES, and it is not what the module design expected. Two interposers built this
# way do NOT chain through each other: a versioned dlvsym lookup does not match an unversioned
# definition in an object that carries a version table (every one of these does, from its libc
# imports), so each one's RTLD_NEXT reaches libc directly and steps over the other. Whichever is
# preloaded FIRST is the only one in the chain; the one behind it is loaded, initialised and
# never entered. Two consequences, and both are rows here rather than notes:
#   - a peer can never recurse back into the hook through the hook's own chain, which is a
#     stronger answer than the guard being asked for. The hook keeps the guard anyway, for the
#     paths this argument does not cover — a second thread, and any caller that reaches the hook
#     other than through its own chain;
#   - the hook is INERT behind another dlsym interposer, so the injection contract has an
#     ordering constraint: our preload must come first. Arm (e) below is that constraint, pinned
#     as a measured fact rather than left for a support thread to rediscover.
# Timeout-bounded: a recursion here would end the process, but a deadlock would hang the case.
stacked() {
  local label="$1" preload="$2" expect="$3" order="$4"
  local got="" n=0 n_ok=0 line origin hook_in=0 peer_in=0 into_hook=0 want_hook=0
  local peer_ok=no peer_want=""
  # shellcheck disable=SC2086
  got="$(${CTR_RUN} -e "LD_PRELOAD=${preload}" -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" -e "LIBHGGC_LOG_LEVEL=2" \
    "${IMG}" timeout 30 ./dlsym_origin 2>&1)"

  while IFS= read -r line; do
    case "${line}" in
      "MECH hgmlDeviceGetMemoryInfo"*)
        n=$((n+1)); origin="${line##*origin=}"
        case "${origin}" in *"${expect}"*) n_ok=$((n_ok+1)) ;; esac
        ;;
    esac
  done <<< "${got}"

  if [ "${n}" -eq 2 ] && [ "${n_ok}" -eq 2 ]; then
    row PASS "mechanism: ${label} — the front interposer wins" "both getters resolve in ${expect}"
  else
    row FAIL "mechanism: ${label} — the front interposer wins" \
      "expected both getters in ${expect}; got: $(echo "${got}" | tr '\n' ' ')"
    fails=$((fails+1))
  fi

  # Each library says when it was ENTERED, which is what separates "loaded and stepped over" from
  # "not loaded at all": the hook logs one interception per getter, the peer one wrap per getter.
  hook_in="$(echo "${got}" | grep -cF '[vppu] intercepted dlsym(' || true)"
  peer_in="$(echo "${got}" | grep -cF '[stack] wrapped ' || true)"
  # The peer also resolves each getter once through the GLOBAL scope, the way a library reaches
  # something it did not link. That pointer must never live in the hook: handing a peer our own
  # wrapper is what a chain alternating between two libraries is made of.
  into_hook="$(echo "${got}" | grep -F '[stack] chained' | grep -cF 'hgml_dlsym_hook.so' || true)"

  # The hook's count is exact: twice when it is in front, never when it is behind. The peer's is
  # a floor rather than a count, because when the peer is in front its own global-scope lookup
  # lands on itself and it wraps each getter a second time — its own doing, and nothing the hook
  # can be judged on.
  if [ "${order}" = hook ]; then
    want_hook=2; peer_want="never entered"
    [ "${peer_in}" -eq 0 ] && peer_ok=yes
  else
    want_hook=0; peer_want="entered at least once per getter"
    [ "${peer_in}" -ge 2 ] && peer_ok=yes
  fi

  if ! echo "${got}" | grep -q 'dlsym_stack loaded' \
    || ! echo "${got}" | grep -q 'hgml_dlsym_hook loaded'; then
    row FAIL "mechanism: ${label} — both libraries loaded" \
      "a constructor marker is missing, so 'loaded but never entered' cannot be told from 'never loaded'"
    fails=$((fails+1))
  elif [ "${hook_in}" -eq "${want_hook}" ] && [ "${peer_ok}" = yes ] \
    && [ "${into_hook}" -eq 0 ]; then
    row PASS "mechanism: ${label} — only the front one is in the chain" \
      "hook entered ${hook_in}×, peer entered ${peer_in}×, and no global-scope lookup pointed into the hook"
  else
    row FAIL "mechanism: ${label} — only the front one is in the chain" \
      "hook entered ${hook_in}× (want ${want_hook}), peer entered ${peer_in}× (want: ${peer_want}), ${into_hook} lookup(s) into the hook"
    fails=$((fails+1))
  fi
}

stacked "two interposers, hook first" \
  "/work/hgml_dlsym_hook.so:/work/dlsym_stack.so" "hgml_dlsym_hook.so" hook
stacked "two interposers, peer first" \
  "/work/dlsym_stack.so:/work/hgml_dlsym_hook.so" "dlsym_stack.so" peer

# ---------------- visibility rows (need a real PPU) ----------------
skip_all() {
  for r in "baseline physical figure" "arm a: hook reports the quota" \
           "arm b: control reports the physical figure" "arm c: vendor wrapper coexists" \
           "arm d: peer behind the hook — the quota" "arm e: peer in front — the hook is inert" \
           "arm f: the holder spent half the quota" \
           "arm f: ppu-smi reports the ledger's figure for used"; do
    row SKIP "${r}" "$1"
  done
  echo "FAILS=${fails}"
  exit 0
}

if [ ! -e /dev/alixpu ] || [ ! -e /dev/alixpu_ctl ]; then
  skip_all "/dev/alixpu or /dev/alixpu_ctl absent — no PPU on this target"
fi
[ -n "${CARD}" ] || skip_all "no idle card (every card at or above XB_PPU_IDLE_MIB, or non-zero util)"
[ -e "/dev/alixpu_ppu${CARD}" ] || skip_all "/dev/alixpu_ppu${CARD} absent for the chosen card"

SMI="${PPU_HOME:-/usr/local/PPU_SDK}/ppu-smi/bin/ppu-smi"
DEV="--device /dev/alixpu --device /dev/alixpu_ctl --device /dev/alixpu_ppu${CARD}"
WRAPPER="${PPU_HOME:-/usr/local/PPU_SDK}/targets/x86_64-linux/lib/libhggc_wrapper.so"

# LD_PRELOAD is read by the dynamic loader INSIDE the container, so it has to name the
# mount point, not the host path the artifacts were staged at. ${HOOK}/${NOHOOK} stay
# host-side for the existence check and the sha256.
HOOK_IN=/work/hgml_dlsym_hook.so
NOHOOK_IN=/work/hgml_nohook.so

# smi_mem <used|total> — one side of the "<used>MiB / <total>MiB" figure ppu-smi prints, for the
# row matching ${CARD} when the container sees several cards and for the only row when it sees
# one. A card occupies two table rows, so the index is carried forward from the first to the
# second. Both sides are parsed by one function because both are now the shim's to answer: the
# total is the card's quota and the used side is this container's accounted total.
smi_mem() {
  awk -v want="${CARD}" -v pick="$1" '
    /^\| *[0-9]+ +PPU-/ { idx = $2; next }
    idx != "" && match($0, /[0-9]+MiB \/ [0-9]+MiB/) {
      m = substr($0, RSTART, RLENGTH); split(m, a, /MiB \/ /); sub(/MiB/, "", a[2])
      used[idx] = a[1] + 0; total[idx] = a[2] + 0; n++; last = idx; idx = ""
    }
    END {
      i = (want in total) ? want : ((n == 1) ? last : "")
      if (i != "") print (pick == "used") ? used[i] : total[i]
    }'
}

# run_smi <label> <preload-or-empty> — ppu-smi by ABSOLUTE path, timeout-bounded so a
# deadlocking preload combination fails instead of hanging the case.
run_smi() {
  local preload="$1" env_args=""
  [ -n "${preload}" ] && env_args="-e LD_PRELOAD=${preload}"
  # shellcheck disable=SC2086
  ${CTR_RUN} ${DEV} ${env_args} -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
    -e "HGGC_DEVICE_SM_LIMIT=100" -e "LIBHGGC_LOG_LEVEL=2" \
    "${IMG}" timeout 60 "${SMI}" 2>&1
}

base_out="$(run_smi '')"
base_total="$(echo "${base_out}" | smi_mem total)"
if [ -n "${base_total}" ] && [ "${base_total}" -gt 0 ]; then
  row PASS "baseline physical figure" "card ${CARD} total=${base_total}MiB (measured, not assumed)"
else
  row FAIL "baseline physical figure" "no memory figure parsed: $(echo "${base_out}" | tr '\n' ' ' | cut -c1-160)"
  fails=$((fails+1))
  echo "FAILS=${fails}"; exit 0
fi

hook_out="$(run_smi "${HOOK_IN}")"
hook_total="$(echo "${hook_out}" | smi_mem total)"
if [ "${hook_total:-0}" = "${QUOTA}" ] && echo "${hook_out}" | grep -q 'intercepted dlsym('; then
  row PASS "arm a: hook reports the quota" "total=${hook_total}MiB == quota, interception marker present"
elif [ "${hook_total:-0}" = "${QUOTA}" ]; then
  row FAIL "arm a: hook reports the quota" "total matches the quota but no interception marker — something else changed it"
  fails=$((fails+1))
else
  row FAIL "arm a: hook reports the quota" "total=${hook_total:-none}MiB, expected exactly ${QUOTA}MiB"
  fails=$((fails+1))
fi

ctl_out="$(run_smi "${NOHOOK_IN}")"
ctl_total="$(echo "${ctl_out}" | smi_mem total)"
ctl_loaded=no
echo "${ctl_out}" | grep -q 'hgml_nohook loaded' && ctl_loaded=yes
if [ "${ctl_total:-0}" = "${base_total}" ] && [ "${ctl_loaded}" = yes ]; then
  row PASS "arm b: control reports the physical figure" "total=${ctl_total}MiB == baseline, and the control proved it loaded"
elif [ "${ctl_loaded}" != yes ]; then
  row FAIL "arm b: control reports the physical figure" "the control never proved it loaded, so its physical reading proves nothing"
  fails=$((fails+1))
else
  row FAIL "arm b: control reports the physical figure" "total=${ctl_total:-none}MiB, expected the baseline ${base_total}MiB"
  fails=$((fails+1))
fi

# Arm (c) has to prove the wrapper LOADED, the same discipline arm (b) applies to the
# control: if libhggc_wrapper.so is absent or the loader rejects it, the hook goes on
# working by itself and the row reports the quota — a PASS that says nothing at all about
# coexistence. Ask the loader directly with LD_DEBUG=libs on `true`, so ppu-smi's table is
# not in the way; "calling init:" appears only for an object that actually initialised,
# where the search lines appear either way.
# shellcheck disable=SC2086
wrap_dbg="$(${CTR_RUN} ${DEV} -e "LD_DEBUG=libs" -e "LD_PRELOAD=${HOOK_IN}:${WRAPPER}" \
  -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" -e "HGGC_DEVICE_SM_LIMIT=100" \
  -e "LIBHGGC_LOG_LEVEL=2" "${IMG}" timeout 30 true 2>&1)"
if echo "${wrap_dbg}" | grep -q 'calling init:.*libhggc_wrapper\.so'; then
  wrap_loaded=yes
elif echo "${wrap_dbg}" | grep -qE 'libhggc_wrapper\.so.*cannot be preloaded'; then
  wrap_loaded=absent
else
  wrap_loaded=unproven
fi

if [ "${wrap_loaded}" = absent ]; then
  row SKIP "arm c: vendor wrapper coexists" "${WRAPPER} is not loadable in this image — there is nothing to coexist with"
elif [ "${wrap_loaded}" != yes ]; then
  row FAIL "arm c: vendor wrapper coexists" "could not prove libhggc_wrapper.so loaded, so a quota reading here proves nothing about coexistence"
  fails=$((fails+1))
else
  wrap_out="$(run_smi "${HOOK_IN}:${WRAPPER}")"
  wrap_total="$(echo "${wrap_out}" | smi_mem total)"
  if [ "${wrap_total:-0}" = "${QUOTA}" ]; then
    row PASS "arm c: vendor wrapper coexists" "total=${wrap_total}MiB == quota with libhggc_wrapper.so also preloaded, and it proved it loaded"
  elif echo "${wrap_out}" | grep -qiE 'terminated|timed out'; then
    row FAIL "arm c: vendor wrapper coexists" "timed out — recursion or deadlock against libhggc_wrapper.so"
    fails=$((fails+1))
  else
    row FAIL "arm c: vendor wrapper coexists" "total=${wrap_total:-none}MiB, expected ${QUOTA}MiB"
    fails=$((fails+1))
  fi
fi

# Arms (d) and (e) are the call-time half of the stacking rows above: the mechanism rows decide
# which object each pointer came from, and only a real query decides that CALLING the result
# terminates and reports something. Each arm names the figure it expects, because the two orders
# do not expect the same one — which is the finding, not a convenience:
#   - (d) hook first: the hook is in the chain, so the quota;
#   - (e) peer first: the hook is loaded and stepped over, so the card's PHYSICAL figure. That is
#     the ordering constraint on the injection contract, asserted rather than described. If this
#     row ever reports the quota the constraint has changed, and the two-sided comparison is what
#     makes that visible instead of absorbing it.
# The peer must prove it loaded either way, for the same reason arm (b)'s control must.
peer() {
  local label="$1" preload="$2" want="$3" out="" total=""
  out="$(run_smi "${preload}")"
  total="$(echo "${out}" | smi_mem total)"

  if ! echo "${out}" | grep -q 'dlsym_stack loaded'; then
    row FAIL "${label}" "the peer never proved it loaded, so this reading proves nothing"
    fails=$((fails+1))
  elif echo "${out}" | grep -qiE 'terminated|timed out'; then
    row FAIL "${label}" "timed out — recursion or deadlock between the two interposers"
    fails=$((fails+1))
  elif [ "${total:-0}" = "${want}" ]; then
    row PASS "${label}" "total=${total}MiB, as expected with this preload order"
  else
    row FAIL "${label}" "total=${total:-none}MiB, expected ${want}MiB"
    fails=$((fails+1))
  fi
}

peer "arm d: peer behind the hook — the quota" "${HOOK_IN}:/work/dlsym_stack.so" "${QUOTA}"
peer "arm e: peer in front — the hook is inert" "/work/dlsym_stack.so:${HOOK_IN}" "${base_total}"

# Arm (f) is the other half of a slice's figures, and the one arms (a)–(e) cannot see: `used`.
# It must come from common/'s ledger, so that what ppu-smi shows and what an allocation is
# admitted against are one number. Two processes in ONE container, which is what makes the point:
# the first spends half the quota with the ENFORCEMENT shim preloaded and holds it, then ppu-smi
# runs with the VISIBILITY shim preloaded and must report that figure — a number it can only have
# read out of the shared region, because it is the other process's allocation.
#
# Non-vacuous by construction: the card was chosen idle (below XB_PPU_IDLE_MIB, 64 by default),
# so a reading of half the quota — 2048MiB at the default — cannot be the vendor's own figure for
# the card. The baseline used side is printed beside it so the contrast is on the record.
HOLD_MIB=$((QUOTA / 2))
base_used="$(echo "${base_out}" | smi_mem used)"
# shellcheck disable=SC2086
held="$(${CTR_RUN} ${DEV} -e "HGGC_DEVICE_MEMORY_LIMIT_0=${QUOTA}" \
  -e "HGGC_DEVICE_SM_LIMIT=100" -e "LIBHGGC_LOG_LEVEL=2" \
  "${IMG}" timeout 120 bash -c "
    LD_PRELOAD=/work/hggc_quota.so ./hggc_mem_paths 0 $((HOLD_MIB * 1024 * 1024)) hold 25 \
      > /tmp/holder.log 2>&1 &
    holder=\$!
    sleep 8
    echo '--- SMI ---'
    LD_PRELOAD=${HOOK_IN} timeout 60 ${SMI} 2>&1
    echo '--- HOLDER ---'
    wait \${holder}
    cat /tmp/holder.log" 2>&1)"

# One named section, so the holder's own log cannot be parsed as ppu-smi's table.
section() { echo "${held}" | awk -v s="--- $1 ---" '$0 == s { on = 1; next } /^--- /{ on = 0 } on'; }
held_smi="$(section SMI)"
held_used="$(echo "${held_smi}" | smi_mem used)"
held_total="$(echo "${held_smi}" | smi_mem total)"

if section HOLDER | grep -q 'PATH hold result=success'; then
  row PASS "arm f: the holder spent half the quota" "${HOLD_MIB}MiB allocated and held"
else
  row FAIL "arm f: the holder spent half the quota" \
    "nothing was spent, so there is no ledger figure to read: $(section HOLDER | grep -E '^PATH' | tr '\n' ' ' | cut -c1-200)"
  fails=$((fails+1))
fi

if [ "${held_used:-none}" = "${HOLD_MIB}" ] && [ "${held_total:-none}" = "${QUOTA}" ]; then
  row PASS "arm f: ppu-smi reports the ledger's figure for used" \
    "used=${held_used}MiB / total=${held_total}MiB — another process's charge, where the card itself reads ${base_used:-?}MiB used"
else
  row FAIL "arm f: ppu-smi reports the ledger's figure for used" \
    "used=${held_used:-none}MiB / total=${held_total:-none}MiB, expected ${HOLD_MIB}MiB / ${QUOTA}MiB (card baseline used=${base_used:-?}MiB)"
  fails=$((fails+1))
fi

echo "--- ppu-smi arm a (card ${CARD}) ---"
echo "${hook_out}" | grep -E 'vppu|MiB /' | head -12

echo "FAILS=${fails}"
PAYLOAD
)"
echo "${out}"
xb_verdict "THEAD-CASE 2" "$(xb_fails "${out}")"
