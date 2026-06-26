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
#   XB_BANNER_RE  regex of banner lines to strip from output
#
# Contract:
#   xrun "<shell command string>"   run on the target, banner-filtered, returns
#                                    the target command's exit status.
#   xput <local-file> <remote-path> copy a file onto the target (cp | base64+ssh).
#   xsh  [VAR=val ...] < script      pipe a script to `bash -s` on the target; any
#                                    leading VAR=val tokens are exported first.
set -uo pipefail

XB_MODE="${XB_MODE:-local}"
XB_HOST="${XB_HOST:-}"
XB_BANNER_RE="${XB_BANNER_RE:-logged onto|conda env|secured server|Permanently added|do not inject|^!!!}"

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
    ssh -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 "${XB_HOST}" "$1" 2>&1 | _xb_filter
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
    ssh "${XB_HOST}" "mkdir -p \"\$(dirname '${dst}')\"" >/dev/null 2>&1
    base64 < "${src}" | ssh "${XB_HOST}" "base64 -d > '${dst}'" >/dev/null 2>&1
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
    printf '%s%s\n' "${exports}" "${body}" \
      | ssh -o StrictHostKeyChecking=accept-new "${XB_HOST}" 'bash -s' 2>&1 | _xb_filter
    return "${PIPESTATUS[1]}"
  fi
  printf '%s%s\n' "${exports}" "${body}" | bash -s 2>&1 | _xb_filter
  return "${PIPESTATUS[1]}"
}

# xtarget_desc — human label for logs.
xtarget_desc() { [ "${XB_MODE}" = ssh ] && echo "ssh:${XB_HOST}" || echo "local"; }
