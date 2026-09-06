#!/usr/bin/env bash

function gpustack::util::find_subdirs() {
  local path="$1"
  if [[ -z "$path" ]]; then
    path="./"
  fi
  # shellcheck disable=SC2010
  ls -l "$path" | grep "^d" | awk '{print $NF}' | xargs echo
}

function gpustack::util::is_empty_dir() {
  local path="$1"
  if [[ ! -d "${path}" ]]; then
    return 0
  fi

  # shellcheck disable=SC2012
  if [[ $(ls "${path}" | wc -l) -eq 0 ]]; then
    return 0
  fi
  return 1
}

function gpustack::util::join_array() {
  local IFS="$1"
  shift 1
  echo "$*"
}

function gpustack::util::get_os() {
  local os
  if go env GOOS >/dev/null 2>&1; then
    os=$(go env GOOS)
  else
    os=$(echo -n "$(uname -s)" | tr '[:upper:]' '[:lower:]')
  fi

  case ${os} in
  cygwin_nt*) os="windows" ;;
  mingw*) os="windows" ;;
  msys_nt*) os="windows" ;;
  esac

  echo -n "${os}"
}

function gpustack::util::get_raw_os() {
  local os
  os=$(echo -n "$(uname -s)" | tr '[:upper:]' '[:lower:]')

  case ${os} in
  cygwin_nt*) os="windows" ;;
  mingw*) os="windows" ;;
  msys_nt*) os="windows" ;;
  esac

  echo -n "${os}"
}

function gpustack::util::get_arch() {
  local arch
  if go env GOARCH >/dev/null 2>&1; then
    arch=$(go env GOARCH)
    if [[ "${arch}" == "arm" ]]; then
      arch="${arch}v$(go env GOARM)"
    fi
  else
    arch=$(uname -m)
  fi

  case ${arch} in
  armv5*) arch="armv5" ;;
  armv6*) arch="armv6" ;;
  armv7*)
    if [[ "${1:-}" == "--full-name" ]]; then
      arch="armv7"
    else
      arch="arm"
    fi
    ;;
  aarch64) arch="arm64" ;;
  x86) arch="386" ;;
  i686) arch="386" ;;
  i386) arch="386" ;;
  x86_64) arch="amd64" ;;
  esac

  echo -n "${arch}"
}

function gpustack::util::get_raw_arch() {
  local arch
  arch=$(uname -m)

  case ${arch} in
  armv5*) arch="armv5" ;;
  armv6*) arch="armv6" ;;
  armv7*)
    if [[ "${1:-}" == "--full-name" ]]; then
      arch="armv7"
    else
      arch="arm"
    fi
    ;;
  aarch64) arch="arm64" ;;
  x86) arch="386" ;;
  i686) arch="386" ;;
  i386) arch="386" ;;
  x86_64) arch="amd64" ;;
  esac

  echo -n "${arch}"
}

function gpustack::util::get_random_port_start() {
  local offset="${1:-1}"
  if [[ ${offset} -le 0 ]]; then
    offset=1
  fi

  while true; do
    random_port=$((RANDOM % 10000 + 50000))
    for ((i = 0; i < offset; i++)); do
      if nc -z 127.0.0.1 $((random_port + i)); then
        random_port=0
        break
      fi
    done

    if [[ ${random_port} -ne 0 ]]; then
      echo -n "${random_port}"
      break
    fi
  done
}

function gpustack::util::sed_inplace() {
  # In-place edit with GNU sed, if not available, back off to gnu-sed.
  if ! sed -i "$@" >/dev/null 2>&1; then
    # back off GNU sed, brew install gnu-sed.
    gsed -i "$@"
  fi
}

function gpustack::util::awk_inplace() {
  # In-place edit with GNU awk, if not available, back off to gawk.
  r=${1:-}
  f=${2:-}
  if [[ -n "${f}" ]]; then
    # shellcheck disable=SC2012
    if [[ ! -f "${f}" ]]; then
      return 1
    fi
  fi
  tf="$f.tmp"
  if ! awk "$r" "$f" >"$tf" 2>/dev/null; then
    # back off GNU awk, brew install gawk.
    gawk "$r" "$f" >"$tf" 2>/dev/null
  fi
  if [[ -n "${tf}" ]]; then
    mv "$tf" "$f"
  else
    return 1
  fi
}

# gpustack::util::go_module_version prints the module version stamped into a binary built by
# `go install`, e.g. "v0.49.0" or "v1.3.3-0.20221024144010-f67b8970b736". It is how a tool with
# no --version flag is still compared against its pin.
#
# Prints nothing when the name resolves to no executable, when that executable is not a Go binary,
# or when it carries no build info. A caller reads the empty string as "does not match the pin",
# which reinstalls -- the safe direction, since the alternative is running a generator whose
# version nobody established.
function gpustack::util::go_module_version() {
  local path
  path="$(command -v "${1}" 2>/dev/null)"
  if [[ -z "${path}" ]]; then
    return 0
  fi
  go version -m "${path}" 2>/dev/null | awk '$1 == "mod" { print $3; exit }'
}

# gpustack::util::go_module_version_is reports whether a go-installed binary matches its pin.
# Two pin forms are in use in hack/lib: a tag, which go stamps verbatim, and a commit, which go
# stamps as the trailing field of a pseudo-version.
#
# THE TWO SIDES OF A COMMIT PIN ARE ABBREVIATED TO DIFFERENT LENGTHS, and that is the whole
# subtlety here. `hack/lib/cgo.sh` pins full 40-character hashes while go stamps 12 characters
# (`v0.0.0-20220810182948-cef5ec7833f3`), so neither equality nor a `-<pin>` suffix test can ever
# hold. Comparing them as abbreviations of one another is what they actually are.
#
# Both sides must be at least 7 characters, which is git's own floor for an unambiguous
# abbreviation -- without it a truncated or empty pin would prefix-match every commit.
function gpustack::util::go_module_version_is() {
  local installed="${1}" pin="${2}" commit
  if [[ -z "${installed}" ]] || [[ -z "${pin}" ]]; then
    return 1
  fi
  if [[ "${installed}" == "${pin}" ]]; then
    return 0
  fi

  commit="${installed##*-}"
  if [[ "${commit}" == "${installed}" ]]; then
    return 1
  fi
  if [[ ${#commit} -lt 7 ]] || [[ ${#pin} -lt 7 ]]; then
    return 1
  fi
  if [[ "${pin}" == "${commit}"* ]] || [[ "${commit}" == "${pin}"* ]]; then
    return 0
  fi
  return 1
}

function gpustack::util::decode64() {
  if [[ $# -eq 0 ]]; then
    cat | base64 --decode
  else
    printf '%s' "$1" | base64 --decode
  fi
}

function gpustack::util::encode64() {
  if [[ $# -eq 0 ]]; then
    cat | base64
  else
    printf '%s' "$1" | base64
  fi
}

function gpustack::util::kill_jobs() {
  for job in $(jobs -p); do
    kill -9 "$job"
  done
}

function gpustack::util::wait_jobs() {
  trap gpustack::util::kill_jobs TERM INT
  local fail=0
  local job
  for job in $(jobs -p); do
    wait "${job}" || fail=$((fail + 1))
  done
  return ${fail}
}

function gpustack::util::dismiss() {
  echo "" 1>/dev/null 2>&1
}
