#!/usr/bin/env bash

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)"
source "${ROOT_DIR}/hack/lib/init.sh"

SRC_DIR="${ROOT_DIR}/gen/binding"
DST_DIR="${ROOT_DIR}/binding"
mkdir -p "${DST_DIR}"

function generate_api() {
  if ! gpustack::protoc::protoc::validate; then
    gpustack::log::error "cannot execute protoc as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_go::validate; then
    gpustack::log::error "cannot execute protoc-gen-go as it hasn't installed"
    return 1
  fi

  if ! gpustack::protoc::protoc_gen_gogo::validate; then
    gpustack::log::error "cannot execute protoc-gen-gogo as it hasn't installed"
    return 1
  fi

  if ! gpustack::lint::goimports::validate; then
    gpustack::log::error "cannot execute goimports as it hasn't installed"
    return 1
  fi

  PATH="${ROOT_DIR}/.sbin:${ROOT_DIR}/.sbin/protoc/bin:${PATH}" \
  GODEBUG=gotypesalias=0 \
    go run -mod=mod "${ROOT_DIR}/gen/${task}" "$@"
}

function _generate_binding() {
  local runtime="$1"
  shift 1

  local src="${SRC_DIR}/${runtime}"
  if [[ ! -d "${src}" ]]; then
    gpustack::log::error "source directory ${src} does not exist"
    return 1
  fi

  gpustack::log::info "generating binding for ${runtime}"

  local dst="${DST_DIR}/${runtime}"
  mkdir -p "${dst}"

  if [[ ! -f "${src}/config.yaml" ]]; then
    gpustack::log::error "config file ${src}/config.yaml does not exist"
    return 1
  fi

  # Copy all files to the destination directory excludes h files
  for f in "${src}"/*; do
    if [[ -f "${f}" ]] && [[ ! "${f}" =~ \.h$ ]]; then
      cp -f "${f}" "${dst}"
    fi
  done

  # Copy header files to the destination directory
  for h in "${src}"/*.h; do
    cp -f "${h}" "${dst}"
    dst_h="${dst}/$(basename "${h}")"
    # Expand typedef struct to struct with handle field.
    gpustack::util::sed_inplace -E \
      -e 's#(typedef\s+struct)\s+([A-Za-z_][A-Za-z0-9_]*)(\*)\s+(.*_t);#\1\n{\n    struct \2\3 handle;\n} \4;#g' \
      -e 's#^(typedef\s+struct)\s+([A-Za-z_]\w*)\s+\2;#\1\n{\n    struct \2* handle;\n} \2;#g' \
      "${dst_h}"
    # Expand typedef enum to enum.
    # gpustack::util::sed_inplace -E \
    #  -e 'N; s#(typedef\s+enum)\s+([A-Za-z_]\w*)\n\{#\1 {#g' \
    #  "${dst_h}"
    # Convert struct definition to typedef struct.
    gpustack::util::awk_inplace \
      '/^struct[ \t]+[A-Za-z_][A-Za-z0-9_]*[ \t]*\{/ {name=$2; print "typedef struct " name " {"; in_struct=1; next} in_struct && /^[ \t]*\};/ {print "} " name ";"; in_struct=0; next} {print}' \
      "${dst_h}"
    # Convert anonymous struct to a named struct.
    # gpustack::util::awk_inplace \
    #  '/typedef struct {/ {in_struct=1; member_count=0; next} in_struct {if ($0 ~ /^}[[:space:]]*[a-zA-Z_][a-zA-Z0-9_]*[[:space:]]*;/) {line=$0; sub(/^}[[:space:]]*/, "", line); sub(/[[:space:]]*;.*$/, "", line); struct_name=line; print "typedef struct " struct_name "_st {"; for(i=1;i<=member_count;i++) print members[i]; print "} " struct_name ";"; in_struct=0; next} else {members[++member_count]=$0; next}} !in_struct' \
    #  "${dst_h}"
  done

  # Generate
  if [[ "${runtime}" == "amdsmi" ]] || [[ "${runtime}" == "amdgpu" ]] || [[ "${runtime}" == "cndev" ]] || [[ "${runtime}" == "hsa" ]] || [[ "${runtime}" == "rsmi" ]]; then
    if ! gpustack::cgo::c_for_go_c99::validate; then
        gpustack::log::error "cannot execute c-for-go-c99 as it hasn't installed"
        return 1
      fi
    $(gpustack::cgo::c_for_go_c99::bin) -nostamp -out "${DST_DIR}" "${dst}/config.yaml"
  else
    if ! gpustack::cgo::c_for_go::validate; then
      gpustack::log::error "cannot execute c-for-go as it hasn't installed"
      return 1
    fi
    $(gpustack::cgo::c_for_go::bin) -nostamp -out "${DST_DIR}" "${dst}/config.yaml"
  fi
  pushd "${dst}" >/dev/null 2>&1 \
    && go tool cgo -godefs "types.go" > "zz_generated.types.go" \
    && go fmt "zz_generated.types.go" >/dev/null \
    && popd >/dev/null 2>&1

  # Clean up
  rm -rf "${dst}/config.yaml" "${dst}/cgo_helpers.go" "${dst}/types.go" "${dst}/_obj"
}

function generate_binding() {
  local runtimes=()
  IFS=" " read -r -a runtimes <<<"$(gpustack::util::find_subdirs "${SRC_DIR}")"

  for runtime in "${runtimes[@]}"; do
    if ! _generate_binding "${runtime}"; then
      return 1
    fi
  done
}

function generate_chart() {
  local chart="${ROOT_DIR}/deploy/gpustack-operator/chart"

  if ! gpustack::helm::deps "${chart}"; then
    return 1
  fi
  if ! gpustack::helm::docs "${chart}"; then
    return 1
  fi
  if ! gpustack::helm::schema "${chart}"; then
    return 1
  fi
}

function generate() {
  local tasks
  if [[ "$#" -gt 0 ]]; then
    IFS=" " read -r -a tasks <<<"$*"
  else
    tasks=("api")
  fi

  for task in "${tasks[@]}"; do
    gpustack::log::info "generating ${task}"

    if declare -f "generate_${task}" >/dev/null 2>&1; then
      if ! generate_"${task}"; then
        gpustack::log::error "failed to generate ${task}"
        return 1
      fi
    elif ! _generate_binding "${task}"; then
      gpustack::log::error "failed to generate ${task}"
      return 1
    fi
  done
}

gpustack::log::info "+++ GENERATE +++"
generate "$@"
gpustack::log::info "--- GENERATE ---"
