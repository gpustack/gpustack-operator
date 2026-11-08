#!/usr/bin/env bash

function gpustack::build::add_go_paths() {
  local dirs=(/usr/local/go/bin "${HOME:+${HOME}/go/bin}")
  if [[ $# -gt 0 ]]; then dirs=("$@"); fi
  local directory
  for directory in "${dirs[@]}"; do
    if [[ -n "${directory}" && -d "${directory}" && ":${PATH}:" != *":${directory}:"* ]]; then
      export PATH="${PATH}:${directory}"
    fi
  done
}

# Run in a subshell so flags and cross compilers cannot leak to the next platform.
function gpustack::build::setup_env() {
  local target_os="$1" target_arch="$2"
  export CGO_ENABLED=1
  export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g}"
  export CGO_LDFLAGS="${CGO_LDFLAGS:--O2 -g}"
  if [[ "${target_os}" == "linux" ]]; then
    # Last wins, including when a caller supplied a newer C dialect or eager linking.
    export CGO_CFLAGS="${CGO_CFLAGS} -std=gnu17"
    export CGO_LDFLAGS="${CGO_LDFLAGS} -Wl,-z,lazy"
    if [[ -z "${CC:-}" && "${target_os}/${target_arch}" != "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]]; then
      local compiler
      case "${target_arch}" in
        arm64) compiler=aarch64-linux-gnu-gcc ;;
        amd64) compiler=x86_64-linux-gnu-gcc ;;
        *) gpustack::log::fatal "Set CC to a C compiler targeting ${target_os}/${target_arch}" ;;
      esac
      if ! command -v "${compiler}" >/dev/null 2>&1; then
        gpustack::log::fatal "Cross compiler ${compiler} is missing; install it or set CC"
      fi
      export CC="${compiler}"
    fi
  fi
}

function gpustack::build::verify_binary() {
  local output="$1" target_os="$2" target_arch="$3"
  if [[ "${target_os}" == "linux" ]]; then
    local reader dynamic candidate
    reader="${READELF:-}"
    if [[ -z "${reader}" ]]; then
      for candidate in readelf llvm-readelf greadelf; do
        if command -v "${candidate}" >/dev/null 2>&1; then
          reader="${candidate}"
          break
        fi
      done
    fi
    if [[ -z "${reader}" ]]; then
      gpustack::log::fatal "Install readelf or set READELF to verify ${output}"
    fi
    if ! dynamic="$("${reader}" -d "${output}")"; then
      gpustack::log::fatal "Cannot inspect the ELF dynamic section of ${output}"
    fi
    # Consume the full output before matching, so pipefail cannot mask a match via SIGPIPE.
    if [[ -z "${dynamic}" ]] || grep -Eq 'BIND_NOW|\(FLAGS_1\).*NOW' <<<"${dynamic}"; then
      gpustack::log::fatal "${output} lacks verified lazy symbol binding; vendor CGO bindings require -Wl,-z,lazy"
    fi
  fi
  if [[ "${target_os}/${target_arch}" == "$(go env GOHOSTOS)/$(go env GOHOSTARCH)" ]]; then
    "${output}" --version >/dev/null
  fi
}
