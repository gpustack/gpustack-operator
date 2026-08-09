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
#   3. A rented accelerator instance can front its SSH with a proxy that offers an
#      interactive shell and nothing else: `ssh host '<cmd>'` connects and runs
#      nothing, and scp/sftp are absent entirely. XB_MODE=pty drives such a target
#      the way a person does — the script arrives on stdin — and recovers the answer
#      from a stream that also carries the terminal's echo, its escape sequences and
#      the shell prompt. Only that mode is affected; ssh mode is unchanged.
#
# Config via environment (the SKILL.md flow sets these):
#   XB_MODE   local | ssh | pty     (default: local)
#   XB_HOST   user@host             (required when XB_MODE=ssh or pty)
#   XB_SSH_OPTS   extra ssh options, word-split on purpose (e.g. '-J user@bastion'
#                 when the target is only routable through a jump host)
#   XB_BANNER_RE  regex of banner lines to strip from output
#   XB_CTR    container runtime ON THE TARGET (default: probed, docker then nerdctl)
#   XB_CTR_ARGS   global args inserted before the runtime's subcommand, e.g.
#                 '--namespace k8s.io' so nerdctl reaches a k3s/rke2 containerd's images
#   XB_PTY_CHUNK  base64 characters per typed line in pty mode (default 1024)
#
# Contract — the same three calls whatever the mode, which is what lets one case script run
# against a docker host, an ssh host and a PTY-only instance without knowing which it is:
#   xrun "<shell command string>"   run on the target, banner-filtered, returns
#                                    the target command's exit status.
#   xput <local-file> <remote-path> copy a file onto the target (cp | base64+ssh |
#                                    base64 typed in chunks and verified by digest).
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
XB_PTY_CHUNK="${XB_PTY_CHUNK:-1024}"

_xb_filter() { grep -avE "${XB_BANNER_RE}" || true; }

_xb_check() {
  if { [ "${XB_MODE}" = ssh ] || [ "${XB_MODE}" = pty ]; } && [ -z "${XB_HOST}" ]; then
    echo "lib.sh: XB_MODE=${XB_MODE} requires XB_HOST" >&2
    return 2
  fi
}

# --- XB_MODE=pty: the target has an interactive shell and nothing else ---------------------
#
# The answer comes back as ONE self-describing line — a marker, the status, and the whole output
# base64'd — rather than as a region fenced between two markers. Measured on such a target, the
# fenced form does not survive the stream it has to be read out of:
#   - the shell prints its PROMPT before each command, and with the echo suppressed the output
#     lands on the same line, so a marker line reads `root@host:/# __XB_BEGIN__` and an anchored
#     match never fires;
#   - the terminal ECHOES the stream back before the shell has run any of it — three times, on
#     the target measured — so `echo __XB_BEGIN__` appears in the output as well, and a match
#     loose enough for the prompt is then loose enough for the echo;
#   - and the echo arrives WRAPPED and character-corrupted at the terminal's width, so it cannot
#     be recognised and subtracted either.
# One line sidesteps all three: whatever precedes the marker on it is noise by construction, and
# base64 carries bytes the terminal would otherwise interpret.
_XB_SEQ=0

# _xb_pty_feed <stream> — type a stream into the target's shell and print what came back with
# the terminal's own noise removed: CR from the line discipline, CSI sequences from colour, OSC
# sequences from the title the prompt sets. The stream always ends in `exit`, because nothing
# else closes an interactive session and ssh would otherwise wait forever. `stty -echo` is
# best-effort: it does not silence the burst already in flight, only the rest.
_xb_pty_feed() {
  local esc=$'\033' bel=$'\007'
  # shellcheck disable=SC2086  # XB_SSH_OPTS is word-split on purpose
  printf 'stty -echo 2>/dev/null\n%s\nexit\n' "$1" \
    | ssh ${XB_SSH_OPTS} -tt -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 \
        "${XB_HOST}" 2>&1 \
    | tr -d '\r' \
    | sed -e "s/${esc}\[[0-9;?]*[a-zA-Z]//g" -e "s/${esc}][^${bel}]*${bel}//g"
}

# _xb_pty_put_cmds <local> <remote> — the command lines that recreate a local file on the target
# through the terminal.
#
# base64 because the channel IS a terminal: a NUL, a CR or a stray ^C in the payload would be
# interpreted rather than stored. Folded because a line discipline drops whatever overflows its
# buffer and drops it SILENTLY — which is why the callers verify the result by digest instead of
# by exit status.
_xb_pty_put_cmds() {
  local src="$1" dst="$2" chunk
  printf 'mkdir -p "$(dirname %s)"\n: > %s\n' "'${dst}'" "'${dst}.b64'"
  # The `echo` re-terminates the stream `tr` just unterminated: `fold` emits its last piece
  # without a newline otherwise, and `read` discards an unterminated line — which silently
  # dropped the whole payload of any file small enough to be one chunk.
  { base64 < "${src}" | tr -d '\n'; echo; } | fold -w "${XB_PTY_CHUNK}" | while IFS= read -r chunk; do
    [ -n "${chunk}" ] && printf "printf '%%s' '%s' >> '%s'\n" "${chunk}" "${dst}.b64"
  done
  printf "base64 -d < '%s' > '%s' && rm -f '%s'\n" "${dst}.b64" "${dst}" "${dst}.b64"
}

# _xb_pty_send <setup> <command> [teardown] — run <setup> (its output is discarded), then
# <command>; print what <command> printed and return its status. <teardown> runs after the
# status has been reported, which is the only place cleanup can go: anything appended to
# <command> would make its own exit status the one that gets reported.
#
# The marker is assembled from two pieces so the line that PRINTS it never CONTAINS it: the
# echo of `printf '%s%s:...' __XB_ 'RESULT'` carries the two halves apart, and only the shell's
# own output puts them together. Without that, an echoed line and a real one are the same string.
#
# The status is printed and parsed back rather than taken from ssh, whose exit code belongs to
# the session and not to the command — a target that ran nothing at all still exits 0.
_xb_pty_send() {
  local setup="$1" cmd="$2" teardown="${3:-}" raw payload rc
  raw="$(_xb_pty_feed "${setup}
__xb_out=\$(mktemp)
{ ${cmd} ; } > \"\${__xb_out}\" 2>&1
__xb_rc=\$?
printf '%s%s:%s:' __XB_ 'RESULT' \"\${__xb_rc}\"; base64 < \"\${__xb_out}\" | tr -d '\\n'; echo
rm -f \"\${__xb_out}\"
${teardown}")"
  rc="$(printf '%s\n' "${raw}" | sed -n 's/.*__XB_RESULT:\([0-9][0-9]*\):.*/\1/p' | tail -1)"
  if [ -z "${rc}" ]; then
    echo "lib.sh: the interactive session gave no result line; the target may not be reachable" >&2
    printf '%s\n' "${raw}" | tail -5 >&2
    return 1
  fi
  payload="$(printf '%s\n' "${raw}" | sed -n 's/.*__XB_RESULT:[0-9][0-9]*://p' | tail -1)"
  [ -n "${payload}" ] && printf '%s' "${payload}" | base64 -d 2>/dev/null | _xb_filter
  return "${rc}"
}

# _xb_pty_exec <script-body> — put the body on the target as a FILE and run it.
#
# A file rather than commands typed one after another, for two reasons that are both about the
# shell reading the terminal: a body starting `set -e` would otherwise arm the interactive shell
# and a failing line would close the session before the status marker was ever printed; and a
# single line longer than the line discipline's buffer would be truncated. As a file the body is
# one line to the terminal whatever it contains.
_xb_pty_exec() {
  local body="$1" tmp remote rc
  _XB_SEQ=$((_XB_SEQ + 1))
  remote="/tmp/.xb-body.$$.${_XB_SEQ}"
  tmp="$(mktemp)" || return 1
  printf '%s\n' "${body}" > "${tmp}"
  _xb_pty_send "$(_xb_pty_put_cmds "${tmp}" "${remote}")" "bash '${remote}'" "rm -f '${remote}'"
  rc=$?
  rm -f "${tmp}"
  return "${rc}"
}

# xrun "<cmd>" — run a single shell command string on the target.
xrun() {
  _xb_check || return 2
  if [ "${XB_MODE}" = pty ]; then
    _xb_pty_exec "$1"
    return $?
  fi
  if [ "${XB_MODE}" = ssh ]; then
    # shellcheck disable=SC2086  # XB_SSH_OPTS is word-split on purpose
    ssh ${XB_SSH_OPTS} -o StrictHostKeyChecking=accept-new -o ConnectTimeout=20 "${XB_HOST}" "$1" 2>&1 | _xb_filter
    return "${PIPESTATUS[0]}"
  fi
  bash -c "$1" 2>&1 | _xb_filter
  return "${PIPESTATUS[0]}"
}

# _xb_md5 <file> — the digest, however this host spells the tool (GNU md5sum / BSD md5).
_xb_md5() {
  if command -v md5sum >/dev/null 2>&1; then
    md5sum < "$1" | cut -d' ' -f1
  else
    md5 -q "$1"
  fi
}

# xput <local> <remote/dest> — place a file on the target.
xput() {
  _xb_check || return 2
  local src="$1" dst="$2"
  if [ "${XB_MODE}" = pty ]; then
    local want got
    want="$(_xb_md5 "${src}")"
    # The TARGET is asked the same way `_xb_md5` asks this host, and for the same reason: the two
    # spellings are not interchangeable and a rented instance is not guaranteed to carry the GNU
    # one. Hardcoding md5sum here would report a file that arrived perfectly well as corrupt.
    got="$(_xb_pty_send "$(_xb_pty_put_cmds "${src}" "${dst}")" \
             "if command -v md5sum >/dev/null 2>&1; then md5sum < '${dst}' | cut -d' ' -f1; else md5 -q '${dst}'; fi")" \
      || return 1
    got="$(printf '%s\n' "${got}" | tr -d '[:space:]')"
    if [ "${got}" != "${want}" ]; then
      echo "lib.sh: ${dst} arrived corrupt (${got:-<nothing>} != ${want})" >&2
      return 1
    fi
    return 0
  fi
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
  if [ "${XB_MODE}" = pty ]; then
    _xb_pty_exec "${exports}${body}"
    return $?
  fi
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
  # `none` is the sentinel for "use no runtime even if one is here" — the only way to exercise the
  # in-place route on a target that has both. Decided HERE rather than by each caller, because a
  # caller that missed it would run the literal word as a command: measured, that is a case
  # announcing the container route and then dying on `none: command not found`.
  [ "${XB_CTR}" = none ] && return 1
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
xtarget_desc() {
  case "${XB_MODE}" in
    ssh) echo "ssh:${XB_HOST}" ;;
    pty) echo "pty:${XB_HOST}" ;;
    *)   echo "local" ;;
  esac
}

# ---- the verdict --------------------------------------------------------------------------
#
# EVERY CASE DECIDES HERE, and that is the point of putting it here. Each payload ends by
# printing its own count as `FAILS=<n>`, and the two functions below are the only code that
# reads it.
#
# What they replace was `echo "${out}" | grep -q 'FAILS=0'`, which searches EVERY line for the
# token instead of reading the count. A row that happens to print `FAILS=0` in its detail column
# therefore decided the whole case, and one did: AMD-CASE 4 printed it in every passing row, so
# the case built to catch a silently discarded CU mask could not fail. Measured — with one of its
# assertions deliberately broken it printed a red FAIL row, printed `FAILS=1`, and still exited 0
# saying PASS.
#
# Twenty-one copies of a decision is twenty-one chances to write it that way again, which is why
# this is a function rather than a paragraph in a style guide.

# xb_fails <output> — the count a payload printed, as a number.
#
# Anchored to a whole line, so only the payload's own count line can answer. The LAST one, because
# a case that runs two payloads prints two. Nothing found reads as 1, never 0: a payload that died
# before printing its count failed, and reading its silence as success is the same bug in reverse.
xb_fails() {
  local n
  n="$(printf '%s\n' "$1" | sed -n 's/^FAILS=\([0-9]*\)$/\1/p' | tail -1)"
  printf '%s' "${n:-1}"
}

# xb_verdict <label> <count> — print the case's verdict and exit with it. Does not return.
xb_verdict() {
  if [ "${2:-1}" -eq 0 ]; then
    echo "$1: PASS"
    exit 0
  fi
  echo "$1: FAIL"
  exit 1
}
