#!/usr/bin/env bash
#
# lib.sh — shared runner abstraction for the xbuild-and-verify skill.
#
# Sourced by preflight.sh / build.sh / cases/case-*.sh. Lets the SAME case logic
# run either on the local docker host or on a remote host over ssh, and hides the
# two operational gotchas found while building this skill:
#   1. A remote login banner (motd / "do not inject conda env" / etc.) pollutes
#      stdout and breaks scp ("Received message too long"). We never use scp; we
#      transfer files via base64 over an ssh stdin pipe, and we strip banner lines
#      from command output.
#   2. ssh runs a non-login shell for `ssh host '<cmd>'`, but image ENV (CANN
#      LD_LIBRARY_PATH etc.) is what matters inside containers, so that is fine.
#
# Config via environment (the SKILL.md flow sets these):
#   XB_MODE   local | ssh           (default: local)
#   XB_HOST   user@host             (required when XB_MODE=ssh)
#   XB_SSH_OPTS   extra ssh options, word-split on purpose (e.g. '-J user@bastion'
#                 when the target is only routable through a jump host)
#   XB_BANNER_RE  regex of banner lines to strip from output
#   XB_CTR    container runtime ON THE TARGET (default: probed, docker then nerdctl)
#   XB_CTR_ARGS   global args inserted before the runtime's subcommand, e.g.
#                 '--namespace k8s.io' so nerdctl reaches a k3s/rke2 containerd's images
#
# Contract:
#   xrun "<shell command string>"   run on the target, banner-filtered, returns
#                                    the target command's exit status.
#   xput <local-file> <remote-path> copy a file onto the target (cp | base64+ssh).
#   xsh  [VAR=val ...] < script      pipe a script to `bash -s` on the target; any
#                                    leading VAR=val tokens are exported first.
#   xctr_resolve                     fill XB_CTR from the target; non-zero if none.
set -uo pipefail

XB_MODE="${XB_MODE:-local}"
XB_HOST="${XB_HOST:-}"
XB_SSH_OPTS="${XB_SSH_OPTS:-}"
XB_BANNER_RE="${XB_BANNER_RE:-logged onto|conda env|secured server|Permanently added|do not inject|Authorized only|activity will be monitored|^!!!}"
XB_CTR="${XB_CTR:-}"
XB_CTR_ARGS="${XB_CTR_ARGS:-}"

_xb_filter() { grep -avE "${XB_BANNER_RE}" || true; }

_xb_check() {
  if [ "${XB_MODE}" = ssh ] && [ -z "${XB_HOST}" ]; then
    echo "lib.sh: XB_MODE=ssh requires XB_HOST" >&2
    return 2
  fi
}

# xrun "<cmd>" — run a single shell command string on the target.
xrun() {
  _xb_check || return 2
  if [ "${XB_MODE}" = ssh ]; then
    # shellcheck disable=SC2086  # XB_SSH_OPTS is word-split on purpose
    ssh ${XB_SSH_OPTS} -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 "${XB_HOST}" "$1" 2>&1 | _xb_filter
    return "${PIPESTATUS[0]}"
  fi
  bash -c "$1" 2>&1 | _xb_filter
  return "${PIPESTATUS[0]}"
}

# xput <local> <remote/dest> — place a file on the target.
xput() {
  _xb_check || return 2
  local src="$1" dst="$2"
  if [ "${XB_MODE}" = ssh ]; then
    # shellcheck disable=SC2086  # XB_SSH_OPTS is word-split on purpose
    ssh ${XB_SSH_OPTS} "${XB_HOST}" "mkdir -p \"\$(dirname '${dst}')\"" >/dev/null 2>&1
    # shellcheck disable=SC2086
    base64 < "${src}" | ssh ${XB_SSH_OPTS} "${XB_HOST}" "base64 -d > '${dst}'" >/dev/null 2>&1
    return "${PIPESTATUS[1]}"
  fi
  mkdir -p "$(dirname "${dst}")" && cp "${src}" "${dst}"
}

# xsh < script — pipe a script body to `bash -s` on the target.
# Optional leading KEY=VALUE args are exported before the body runs (prepended into
# the piped stream as `export` lines, so it works for both local and ssh).
xsh() {
  _xb_check || return 2
  local exports="" body
  while [ "$#" -gt 0 ] && [[ "$1" == *=* ]]; do
    exports+="export ${1%%=*}=$(printf '%q' "${1#*=}")"$'\n'
    shift
  done
  body="$(cat)"
  if [ "${XB_MODE}" = ssh ]; then
    # shellcheck disable=SC2086  # XB_SSH_OPTS is word-split on purpose
    printf '%s%s\n' "${exports}" "${body}" \
      | ssh ${XB_SSH_OPTS} -o StrictHostKeyChecking=accept-new "${XB_HOST}" 'bash -s' 2>&1 | _xb_filter
    return "${PIPESTATUS[1]}"
  fi
  printf '%s%s\n' "${exports}" "${body}" | bash -s 2>&1 | _xb_filter
  return "${PIPESTATUS[1]}"
}

# xctr_resolve — fill XB_CTR by probing the TARGET, because that is where the cases
# run containers: a caller on a docker laptop may drive a PPU host that only has
# nerdctl. An explicit XB_CTR is taken as given — including a bogus one, so the
# no-runtime path stays exercisable.
xctr_resolve() {
  [ -n "${XB_CTR}" ] && return 0
  XB_CTR="$(xrun 'for c in docker nerdctl; do command -v "${c}" >/dev/null 2>&1 && { echo "${c}"; break; }; done' \
    | tail -1 | tr -d '[:space:]')"
  [ -n "${XB_CTR}" ]
}

# thead_idle_cards — echo the index of every idle PPU on the target, one per line; echo
# nothing when there is no ppu-smi or no idle card.
#
# THead-specific, and deliberately not a fixed index: the PPU test host runs production
# inference, so card 0 can be the one holding 91 GB. "Idle" is read out of ppu-smi's own
# table — used memory at or below XB_PPU_IDLE_MIB and 0% utilisation — because ppu-smi is
# the one view of the card that both the host and a container agree on. Parsed from the
# output rather than the exit status, which is 0 even when the driver is not loaded.
#
# The output is filtered to bare indices because callers consume the first line or two AS a
# card index: a login banner XB_BANNER_RE happens not to cover would otherwise become "the
# chosen card" and fail in a way that reads as a device problem rather than a parsing one.
thead_idle_cards() { _thead_idle_cards_raw | grep -E '^[0-9]+$'; }

_thead_idle_cards_raw() {
  xsh PPU_IDLE_MIB="${XB_PPU_IDLE_MIB:-64}" <<'PAYLOAD'
set -u
smi=""
for p in "${PPU_HOME:-/usr/local/PPU_SDK}/ppu-smi/bin/ppu-smi" "$(command -v ppu-smi 2>/dev/null)"; do
  [ -n "${p}" ] && [ -x "${p}" ] && { smi="${p}"; break; }
done
[ -n "${smi}" ] || exit 0

# A card occupies two table rows: the index sits on the first, the memory and utilisation
# figures on the second, so carry the index forward until both figures have been seen.
"${smi}" 2>/dev/null | awk -v maxmib="${PPU_IDLE_MIB}" '
  /^\| *[0-9]+ +PPU-/ { idx = $2; used = -1; util = -1; next }
  idx != "" {
    if (match($0, /[0-9]+MiB \/ [0-9]+MiB/)) {
      m = substr($0, RSTART, RLENGTH); split(m, a, "MiB"); used = a[1] + 0
    }
    if (match($0, /[0-9]+%/)) { util = substr($0, RSTART, RLENGTH) + 0 }
    if (used >= 0 && util >= 0) {
      if (used <= maxmib && util == 0) { print idx }
      idx = ""
    }
  }'
PAYLOAD
}

# xtarget_desc — human label for logs.
xtarget_desc() { [ "${XB_MODE}" = ssh ] && echo "ssh:${XB_HOST}" || echo "local"; }
