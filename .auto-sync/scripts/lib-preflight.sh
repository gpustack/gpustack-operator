#!/usr/bin/env bash
# .auto-sync/scripts/lib-preflight.sh - shared host-side preflight helpers.
#
# Sourced by .auto-sync/scripts/preflight-<domain>.sh before invoking Loopwright.
set -euo pipefail

# autosync::preflight::repo_root
# Prints the canonical physical path of the repository root.
autosync::preflight::repo_root() {
  local top=""
  if command -v git >/dev/null 2>&1 && top="$(git rev-parse --show-toplevel 2>/dev/null)"; then
    (cd "${top}" && pwd -P)
    return 0
  fi
  (cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)
}

# autosync::preflight::init_stage <domain> [explicit_stage_dir]
# Creates an external staging directory (refusing any path inside repo_root) with
# mode 0700 and a baseline/ subdirectory, then prints its canonical path.
autosync::preflight::init_stage() {
  local domain="${1:?domain required}"
  local explicit_dir="${2:-}"

  if [[ ! "${domain}" =~ ^[a-z0-9-]+$ ]]; then
    printf 'FAIL: invalid domain name: %s\n' "${domain}" >&2
    return 1
  fi

  local repo_root
  repo_root="$(autosync::preflight::repo_root)" || return 1
  local raw_stage="${explicit_dir:-/tmp/gpustack-autosync-stage/${domain}}"

  local real_stage
  if ! real_stage="$(python3 - "${repo_root}" "${raw_stage}" <<'PY'
import os
import sys

repo_root = os.path.realpath(sys.argv[1])
stage_dir = os.path.realpath(sys.argv[2])
if stage_dir == repo_root or stage_dir.startswith(repo_root + os.sep):
    print(
        f"FAIL: stage_dir ({stage_dir}) must reside outside repo_root ({repo_root})",
        file=sys.stderr,
    )
    sys.exit(1)
print(stage_dir)
PY
)"; then
    return 1
  fi

  mkdir -p "${real_stage}" "${real_stage}/baseline" || return 1
  if [[ "$(basename "$(dirname "${real_stage}")")" == "gpustack-autosync-stage" ]]; then
    chmod 0700 "$(dirname "${real_stage}")" || return 1
  fi
  chmod 0700 "${real_stage}" || return 1
  printf '%s\n' "${real_stage}"
}

# autosync::preflight::snapshot_baselines <stage_dir> <rel_path>...
# Copies each relative path (file or directory) from repo_root into <stage_dir>/baseline/.
autosync::preflight::snapshot_baselines() {
  local stage_dir="${1:?stage_dir required}"
  shift
  if (( $# == 0 )); then
    printf 'FAIL: snapshot_baselines requires at least one relative path\n' >&2
    return 1
  fi

  local rel src dst
  for rel in "$@"; do
    case "${rel}" in
      ""|.|./*|/*|*../*|*/..|..|*/.|*/./*|*/)
        printf 'FAIL: snapshot_baselines path must be safe repo-relative: %s\n' "${rel}" >&2
        return 1
        ;;
    esac
  done

  local repo_root
  repo_root="$(autosync::preflight::repo_root)"

  for rel in "$@"; do
    src="${repo_root}/${rel}"
    dst="${stage_dir}/baseline/${rel}"
    if [[ ! -e "${src}" ]]; then
      printf 'FAIL: baseline source does not exist: %s\n' "${src}" >&2
      return 1
    fi
    mkdir -p "$(dirname "${dst}")"
    if [[ -d "${src}" ]]; then
      rm -rf "${dst}"
      cp -Rp "${src}" "${dst}"
    else
      cp -fp "${src}" "${dst}"
    fi
  done
}

# autosync::preflight::_write_toolchain_env <stage_dir>
# Captures verified host toolchain directories into <stage_dir>/toolchain.env while
# the host PATH is intact so command.v1 sanitized PATH runs retain setup-go/hugo/node.
autosync::preflight::_write_toolchain_env() {
  local stage_dir="${1:?stage_dir required}"
  local tc_file="${stage_dir}/toolchain.env"
  local tmp_tc
  tmp_tc="$(mktemp "${stage_dir}/.toolchain.env.XXXXXXXX")"

  local go_bin="" go_bin_dir="" goroot_val="" hugo_bin="" hugo_bin_dir="" node_bin="" node_bin_dir="" uv_bin="" uv_bin_dir="" crane_bin="" crane_bin_dir="" docker_bin="" docker_bin_dir=""
  if go_bin="$(command -v go 2>/dev/null || true)" && [[ -n "${go_bin}" ]]; then
    go_bin_dir="$(cd "$(dirname "${go_bin}")" && pwd -P)"
    goroot_val="$(go env GOROOT 2>/dev/null || true)"
  fi
  if hugo_bin="$(command -v hugo 2>/dev/null || true)" && [[ -n "${hugo_bin}" ]]; then
    hugo_bin_dir="$(cd "$(dirname "${hugo_bin}")" && pwd -P)"
  fi
  if node_bin="$(command -v node 2>/dev/null || true)" && [[ -n "${node_bin}" ]]; then
    node_bin_dir="$(cd "$(dirname "${node_bin}")" && pwd -P)"
  fi
  if uv_bin="$(command -v uv 2>/dev/null || true)" && [[ -n "${uv_bin}" ]]; then
    uv_bin_dir="$(cd "$(dirname "${uv_bin}")" && pwd -P)"
  fi
  if crane_bin="$(command -v crane 2>/dev/null || true)" && [[ -n "${crane_bin}" ]]; then
    crane_bin_dir="$(cd "$(dirname "${crane_bin}")" && pwd -P)"
  fi
  if docker_bin="$(command -v docker 2>/dev/null || true)" && [[ -n "${docker_bin}" ]]; then
    docker_bin_dir="$(cd "$(dirname "${docker_bin}")" && pwd -P)"
  fi

  {
    if [[ -n "${go_bin_dir}" && -d "${go_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_GO_BIN_DIR=%s\n' "${go_bin_dir}"
    fi
    if [[ -n "${goroot_val}" && -d "${goroot_val}" ]]; then
      printf 'AUTOSYNC_SAVED_GOROOT=%s\n' "${goroot_val}"
    fi
    if [[ -n "${hugo_bin_dir}" && -d "${hugo_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_HUGO_BIN_DIR=%s\n' "${hugo_bin_dir}"
    fi
    if [[ -n "${node_bin_dir}" && -d "${node_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_NODE_BIN_DIR=%s\n' "${node_bin_dir}"
    fi
    if [[ -n "${uv_bin_dir}" && -d "${uv_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_UV_BIN_DIR=%s\n' "${uv_bin_dir}"
    fi
    if [[ -n "${crane_bin_dir}" && -d "${crane_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_CRANE_BIN_DIR=%s\n' "${crane_bin_dir}"
    fi
    if [[ -n "${docker_bin_dir}" && -d "${docker_bin_dir}" ]]; then
      printf 'AUTOSYNC_SAVED_DOCKER_BIN_DIR=%s\n' "${docker_bin_dir}"
    fi
  } > "${tmp_tc}"
  chmod 0600 "${tmp_tc}"
  mv -f "${tmp_tc}" "${tc_file}"
}

# autosync::preflight::_ensure_tools <repo_root> <shared_root> <tools_csv>
# Validates or installs requested hack/lib tools into <shared_root>/.sbin.
autosync::preflight::_ensure_tools() {
  local repo_root="${1:?repo_root required}"
  local shared_root="${2:?shared_root required}"
  local tools_csv="${3:?tools_csv required}"

  (
    export ROOT_DIR="${shared_root}"
    mkdir -p "${shared_root}/.sbin"
    export PATH="${shared_root}/.sbin:${shared_root}/.sbin/protoc/bin:${PATH}"

    local lib_f
    for lib_f in log util build helm hugo proto style cgo; do
      if [[ -f "${repo_root}/hack/lib/${lib_f}.sh" ]]; then
        # shellcheck disable=SC1090
        source "${repo_root}/hack/lib/${lib_f}.sh"
      fi
    done

    local tool validate_fn bin_fn target_path resolved_bin=""
    local old_ifs="${IFS}"
    IFS=','
    for tool in ${tools_csv}; do
      IFS="${old_ifs}"
      [[ -n "${tool}" ]] || continue
      case "${tool}" in
        shellcheck)
          validate_fn="gpustack::lint::shellcheck::validate"
          bin_fn="gpustack::lint::shellcheck::bin"
          target_path="${shared_root}/.sbin/shellcheck"
          ;;
        helm)
          validate_fn="gpustack::helm::helm::validate"
          bin_fn="gpustack::helm::helm::bin"
          target_path="${shared_root}/.sbin/helm"
          ;;
        helm-docs)
          validate_fn="gpustack::helm::docs::validate"
          bin_fn="gpustack::helm::docs::bin"
          target_path="${shared_root}/.sbin/helm-docs"
          ;;
        helm-schema)
          validate_fn="gpustack::helm::schema::validate"
          bin_fn="gpustack::helm::schema::bin"
          target_path="${shared_root}/.sbin/helm-schema"
          ;;
        hugo)
          validate_fn="gpustack::hugo::validate"
          bin_fn="gpustack::hugo::bin"
          target_path="${shared_root}/.sbin/hugo"
          ;;
        golangci-lint)
          validate_fn="gpustack::lint::golangci_lint::validate"
          bin_fn="gpustack::lint::golangci_lint::bin"
          target_path="${shared_root}/.sbin/golangci-lint"
          ;;
        goimports)
          validate_fn="gpustack::lint::goimports::validate"
          bin_fn="gpustack::lint::goimports::bin"
          target_path="${shared_root}/.sbin/goimports"
          ;;
        goimports-reviser)
          validate_fn="gpustack::lint::goimports_reviser::validate"
          bin_fn="gpustack::lint::goimports_reviser::bin"
          target_path="${shared_root}/.sbin/goimports-reviser"
          ;;
        commitsar)
          validate_fn="gpustack::commit::commitsar::validate"
          bin_fn="gpustack::commit::commitsar::bin"
          target_path="${shared_root}/.sbin/commitsar"
          ;;
        protoc)
          validate_fn="gpustack::protoc::protoc::validate"
          bin_fn="gpustack::protoc::protoc::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc"
          ;;
        protoc-gen-go)
          validate_fn="gpustack::protoc::protoc_gen_go::validate"
          bin_fn="gpustack::protoc::protoc_gen_go::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc-gen-go"
          ;;
        protoc-gen-gogo)
          validate_fn="gpustack::protoc::protoc_gen_gogo::validate"
          bin_fn="gpustack::protoc::protoc_gen_gogo::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc-gen-gogo"
          ;;
        protoc-gen-go-grpc)
          validate_fn="gpustack::protoc::protoc_gen_go_grpc::validate"
          bin_fn="gpustack::protoc::protoc_gen_go_grpc::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc-gen-go-grpc"
          ;;
        protoc-gen-validate)
          validate_fn="gpustack::protoc::protoc_gen_validate::validate"
          bin_fn="gpustack::protoc::protoc_gen_validate::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc-gen-validate"
          ;;
        protoc-gen-grpc-gateway)
          validate_fn="gpustack::protoc::protoc_gen_grpc_gateway::validate"
          bin_fn="gpustack::protoc::protoc_gen_grpc_gateway::bin"
          target_path="${shared_root}/.sbin/protoc/bin/protoc-gen-grpc-gateway"
          ;;
        *)
          printf 'FAIL: unsupported tool in warm_caches --tools: %s\n' "${tool}" >&2
          return 1
          ;;
      esac

      if ! "${validate_fn}" >/dev/null 2>&1; then
        printf 'FAIL: unable to validate/install tool: %s\n' "${tool}" >&2
        return 1
      fi
      if [[ ! -f "${target_path}" ]]; then
        resolved_bin="$(command -v "$("${bin_fn}")" 2>/dev/null || true)"
        if [[ -n "${resolved_bin}" && -f "${resolved_bin}" ]]; then
          mkdir -p "$(dirname "${target_path}")"
          cp -Lp "${resolved_bin}" "${target_path}"
          chmod a+x "${target_path}"
        fi
      fi
    done
    IFS="${old_ifs}"
  )
}

# autosync::preflight::_stage_lfs_objects <repo_root> <shared_root> <stage_dir>
# Stages .git/lfs/objects into <shared_root>/lfs-objects and links <stage_dir>/lfs-objects
# without smudging <repo_root>'s working tree.
autosync::preflight::_stage_lfs_objects() {
  local repo_root="${1:?repo_root required}"
  local shared_root="${2:?shared_root required}"
  local stage_dir="${3:?stage_dir required}"

  command -v git >/dev/null 2>&1 || return 0
  local git_common_dir=""
  git_common_dir="$(git -C "${repo_root}" rev-parse --git-common-dir 2>/dev/null || true)"
  [[ -n "${git_common_dir}" ]] || return 0
  if [[ "${git_common_dir}" != /* ]]; then
    git_common_dir="${repo_root}/${git_common_dir}"
  fi

  local logo_file="${repo_root}/site/assets/images/logo.png"
  local logo_oid=""
  if [[ -f "${logo_file}" ]] && (( $(wc -c < "${logo_file}") < 1024 )); then
    logo_oid="$(awk '$1 == "oid" && $2 ~ /^sha256:[0-9a-f]+$/ && length($2) == 71 { print substr($2, 8); exit }' "${logo_file}")"
  fi
  if [[ -z "${logo_oid}" ]]; then
    logo_oid="$(git -C "${repo_root}" show HEAD:site/assets/images/logo.png 2>/dev/null | awk '$1 == "oid" && $2 ~ /^sha256:[0-9a-f]+$/ && length($2) == 71 { print substr($2, 8); exit }' || true)"
  fi

  if [[ -n "${logo_oid}" ]] && \
     [[ ! -f "${git_common_dir}/lfs/objects/${logo_oid:0:2}/${logo_oid:2:2}/${logo_oid}" ]] && \
     [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" && "${GOPROXY:-}" != "off" ]] && \
     git -C "${repo_root}" remote get-url origin >/dev/null 2>&1 && \
     git lfs version >/dev/null 2>&1; then
    git -C "${repo_root}" lfs fetch origin HEAD >/dev/null 2>&1 || true
  fi

  if [[ -d "${git_common_dir}/lfs/objects" ]]; then
    mkdir -p "${shared_root}/lfs-objects"
    cp -RL "${git_common_dir}/lfs/objects/." "${shared_root}/lfs-objects/" 2>/dev/null || true
    if [[ ! -e "${stage_dir}/lfs-objects" || -L "${stage_dir}/lfs-objects" ]]; then
      ln -sfn "${shared_root}/lfs-objects" "${stage_dir}/lfs-objects"
    fi
  fi
}

# autosync::preflight::warm_caches <stage_dir> [--sbin] [--go] [--go-all] [--tools <comma-list>]
# Populates shared /tmp/gpustack-autosync-stage/{.sbin,go-cache,go-mod,deps-cache,uv-cache,uv-python} caches,
# records host toolchain paths into <stage_dir>/toolchain.env, and links caches into <stage_dir>.
autosync::preflight::warm_caches() {
  local stage_dir="${1:?stage_dir required}"
  shift

  local want_sbin=false
  local want_go=false
  local want_go_all=false
  local saw_mode=false
  local tools_csv=""
  while (( $# > 0 )); do
    case "$1" in
      --sbin)
        want_sbin=true
        saw_mode=true
        shift
        ;;
      --go)
        want_go=true
        saw_mode=true
        shift
        ;;
      --go-all)
        want_go=true
        want_go_all=true
        saw_mode=true
        shift
        ;;
      --tools)
        if (( $# < 2 )) || [[ -z "${2}" ]]; then
          printf 'FAIL: --tools requires a comma-separated tool list\n' >&2
          return 1
        fi
        tools_csv="${2}"
        want_sbin=true
        shift 2
        ;;
      --tools=*)
        tools_csv="${1#--tools=}"
        if [[ -z "${tools_csv}" ]]; then
          printf 'FAIL: --tools= requires a non-empty comma-separated tool list\n' >&2
          return 1
        fi
        want_sbin=true
        shift
        ;;
      *)
        printf 'FAIL: unknown warm_caches flag: %s\n' "$1" >&2
        return 1
        ;;
    esac
  done
  if [[ "${saw_mode}" == "false" && -z "${tools_csv}" ]]; then
    want_sbin=true
    want_go=true
  fi

  local repo_root parent_dir shared_root
  repo_root="$(autosync::preflight::repo_root)"
  mkdir -p "${stage_dir}"
  autosync::preflight::_write_toolchain_env "${stage_dir}"

  parent_dir="$(cd "$(dirname "${stage_dir}")" && pwd -P)"
  case "${parent_dir}" in
    /|/tmp|/private/tmp|/var/tmp|/private/var/tmp)
      shared_root="${stage_dir}/.shared"
      mkdir -p "${shared_root}"
      chmod 0700 "${shared_root}"
      ;;
    *)
      shared_root="${parent_dir}"
      if [[ "$(basename "${parent_dir}")" == "gpustack-autosync-stage" ]]; then
        chmod 0700 "${parent_dir}"
      fi
      ;;
  esac

  autosync::preflight::_stage_lfs_objects "${repo_root}" "${shared_root}" "${stage_dir}"

  if [[ "${want_sbin}" == "true" ]]; then
    mkdir -p "${shared_root}/.sbin"
    if [[ -d "${repo_root}/.sbin" ]]; then
      cp -RLp "${repo_root}/.sbin/." "${shared_root}/.sbin/" 2>/dev/null || true
    fi
    if [[ -n "${tools_csv}" ]]; then
      autosync::preflight::_ensure_tools "${repo_root}" "${shared_root}" "${tools_csv}"
    fi
    if [[ ! -e "${stage_dir}/.sbin" || -L "${stage_dir}/.sbin" ]]; then
      ln -sfn "${shared_root}/.sbin" "${stage_dir}/.sbin"
    fi
  fi

  if [[ "${want_go}" == "true" ]]; then
    local cache_name
    for cache_name in go-cache go-mod deps-cache uv-cache uv-python; do
      mkdir -p "${shared_root}/${cache_name}"
      if [[ -d "${stage_dir}/${cache_name}" && ! -L "${stage_dir}/${cache_name}" ]]; then
        if cp -Rp "${stage_dir}/${cache_name}/." "${shared_root}/${cache_name}/" 2>/dev/null; then
          rm -rf "${stage_dir:?}/${cache_name:?}"
        else
          printf 'WARN: autosync: failed to migrate %s into %s\n' "${cache_name}" "${shared_root}" >&2
        fi
      fi
      if [[ ! -e "${stage_dir}/${cache_name}" || -L "${stage_dir}/${cache_name}" ]]; then
        ln -sfn "${shared_root}/${cache_name}" "${stage_dir}/${cache_name}"
      fi
    done

    if [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" ]] && command -v go >/dev/null 2>&1; then
      if [[ -z "$(ls -A "${shared_root}/go-mod/" 2>/dev/null)" ]]; then
        local host_gomodcache=""
        host_gomodcache="$(go env GOMODCACHE 2>/dev/null || true)"
        if [[ "${GOPROXY:-}" == "off" ]]; then
          if [[ -n "${host_gomodcache}" && -d "${host_gomodcache}" && -r "${host_gomodcache}" && -n "$(ls -A "${host_gomodcache}/" 2>/dev/null)" ]]; then
            cp -RLp "${host_gomodcache}/." "${shared_root}/go-mod/" 2>/dev/null || true
          fi
        elif [[ -f "${repo_root}/go.mod" ]]; then
          local effective_goproxy="${GOPROXY:-https://proxy.golang.org,direct}"
          if [[ -n "${host_gomodcache}" && -d "${host_gomodcache}/cache/download" && -r "${host_gomodcache}/cache/download" ]]; then
            effective_goproxy="file://${host_gomodcache}/cache/download,${effective_goproxy}"
          fi
          local mod_dl_args=(mod download)
          if [[ "${want_go_all}" == "true" ]]; then
            mod_dl_args+=(all)
          fi
          local go_dl_tmp=""
          go_dl_tmp="$(mktemp -d "${TMPDIR:-/tmp}/autosync-gomod-dl.XXXXXXXX")"
          (
            trap 'chmod -R u+w "${go_dl_tmp}" 2>/dev/null || true; rm -rf "${go_dl_tmp}"' EXIT
            trap 'exit 130' INT
            trap 'exit 143' TERM
            trap 'exit 129' HUP
            cp -f "${repo_root}/go.mod" "${go_dl_tmp}/go.mod"
            if [[ -f "${repo_root}/go.sum" ]]; then
              cp -f "${repo_root}/go.sum" "${go_dl_tmp}/go.sum"
            fi
            if [[ -d "${repo_root}/staging" ]]; then
              cp -R "${repo_root}/staging" "${go_dl_tmp}/staging"
            fi
            cd "${go_dl_tmp}"
            GOMODCACHE="${shared_root}/go-mod" \
            GOCACHE="${shared_root}/go-cache" \
            GOPROXY="${effective_goproxy}" \
              go "${mod_dl_args[@]}"
          )
        fi
      fi
    fi

    if [[ "${want_go_all}" == "true" || "${AUTOSYNC_WARM_UV:-false}" == "true" ]] && \
       [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" ]] && \
       [[ "${GOPROXY:-}" != "off" ]] && \
       command -v uv >/dev/null 2>&1 && \
       [[ -d "${repo_root}/model-transfer/python" ]] && \
       { [[ -z "$(ls -A "${shared_root}/uv-cache/" 2>/dev/null)" ]] || \
         ! compgen -G "${shared_root}/uv-python/cpython-*" >/dev/null; }; then
      local uv_tmp_root uv_tmp_cache uv_tmp_env uv_tmp_verify_env uv_tmp_py uv_tmp_home
      uv_tmp_root="$(mktemp -d "${TMPDIR:-/tmp}/autosync-uv-warm.XXXXXXXX")"
      uv_tmp_cache="${uv_tmp_root}/cache"
      uv_tmp_env="${uv_tmp_root}/venv"
      uv_tmp_verify_env="${uv_tmp_root}/venv-verify"
      uv_tmp_py="${uv_tmp_root}/python"
      uv_tmp_home="${uv_tmp_root}/home"
      mkdir -p "${uv_tmp_cache}" "${uv_tmp_py}" "${uv_tmp_home}"
      if [[ -n "$(ls -A "${shared_root}/uv-cache/" 2>/dev/null)" ]]; then
        cp -RLp "${shared_root}/uv-cache/." "${uv_tmp_cache}/" 2>/dev/null || true
      fi
      if compgen -G "${shared_root}/uv-python/cpython-*" >/dev/null; then
        cp -RPp "${shared_root}/uv-python/." "${uv_tmp_py}/" 2>/dev/null || true
      fi
      if (
        cd "${repo_root}/model-transfer/python"
        UV_PYTHON_INSTALL_DIR="${uv_tmp_py}" uv python install >/dev/null 2>&1 && \
        UV_CACHE_DIR="${uv_tmp_cache}" UV_PROJECT_ENVIRONMENT="${uv_tmp_env}" UV_PYTHON_INSTALL_DIR="${uv_tmp_py}" UV_PYTHON_PREFERENCE=only-managed uv sync --frozen >/dev/null 2>&1 && \
        HOME="${uv_tmp_home}" UV_CACHE_DIR="${uv_tmp_cache}" UV_PROJECT_ENVIRONMENT="${uv_tmp_verify_env}" UV_PYTHON_INSTALL_DIR="${uv_tmp_py}" UV_PYTHON_PREFERENCE=only-managed UV_OFFLINE=1 uv run --frozen --offline ruff --version >/dev/null 2>&1
      ); then
        local staged_uv_cache staged_uv_py
        staged_uv_cache="$(mktemp -d "${shared_root}/.uv-cache-stage.XXXXXXXX")"
        staged_uv_py="$(mktemp -d "${shared_root}/.uv-python-stage.XXXXXXXX")"
        if cp -RLp "${uv_tmp_cache}/." "${staged_uv_cache}/" 2>/dev/null && \
           [[ -n "$(ls -A "${staged_uv_cache}/" 2>/dev/null)" ]] && \
           cp -RPp "${uv_tmp_py}/." "${staged_uv_py}/" 2>/dev/null && \
           compgen -G "${staged_uv_py}/cpython-*" >/dev/null; then
          if [[ -z "$(ls -A "${shared_root}/uv-cache/" 2>/dev/null)" ]] && \
             rmdir "${shared_root}/uv-cache" 2>/dev/null; then
            mv "${staged_uv_cache}" "${shared_root}/uv-cache"
          fi
          if ! compgen -G "${shared_root}/uv-python/cpython-*" >/dev/null && \
             [[ -z "$(ls -A "${shared_root}/uv-python/" 2>/dev/null)" ]] && \
             rmdir "${shared_root}/uv-python" 2>/dev/null; then
            mv "${staged_uv_py}" "${shared_root}/uv-python"
          fi
        else
          printf 'WARN: autosync: failed to stage uv cache/python into %s\n' "${shared_root}" >&2
        fi
        rm -rf "${staged_uv_cache}" "${staged_uv_py}" 2>/dev/null || true
      fi
      rm -rf "${uv_tmp_root}" 2>/dev/null || true
    fi
  fi
}

# autosync::preflight::probe_image_multiarch <image_ref>
# Probes linux/amd64 and linux/arm64 digests via crane (or docker buildx fallback, or
# AUTOSYNC_MOCK_MANIFESTS_JSON for offline tests) and prints compact JSON.
autosync::preflight::probe_image_multiarch() {
  local image_ref="${1:?image_ref required}"
  local normalized=""
  if [[ "${image_ref}" != *"/"* ]]; then
    normalized="docker.io/library/${image_ref}"
  else
    local first_seg="${image_ref%%/*}"
    if [[ "${first_seg}" == *.* || "${first_seg}" == *:* || "${first_seg}" == "localhost" ]]; then
      normalized="${image_ref}"
    else
      normalized="docker.io/${image_ref}"
    fi
  fi

  if [[ -n "${AUTOSYNC_MOCK_MANIFESTS_JSON:-}" ]]; then
    python3 - "${AUTOSYNC_MOCK_MANIFESTS_JSON}" "${image_ref}" "${normalized}" <<'PY'
import json
import re
import sys

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
mock_path, image_ref, normalized = sys.argv[1:4]
with open(mock_path, "r", encoding="utf-8") as f:
    data = json.load(f)

images = data.get("images", data) if isinstance(data, dict) else {}
entry = None
for key in (image_ref, normalized):
    if isinstance(images, dict) and key in images:
        entry = images[key]
        break

amd64 = ""
arm64 = ""
published = False
amd64_published = False
if isinstance(entry, dict):
    amd64 = str(entry.get("amd64Digest") or "").strip()
    arm64 = str(entry.get("arm64Digest") or "").strip()
    if not DIGEST_RE.fullmatch(amd64):
        amd64 = ""
    if not DIGEST_RE.fullmatch(arm64):
        arm64 = ""
    raw_pub = entry.get("published") is True
    amd64_published = raw_pub and bool(amd64)
    published = raw_pub and bool(amd64) and bool(arm64) and amd64 != arm64

print(json.dumps({
    "image": image_ref,
    "normalized": normalized,
    "published": published,
    "amd64Published": amd64_published,
    "amd64Digest": amd64,
    "arm64Digest": arm64,
}, separators=(",", ":")))
PY
    return 0
  fi

  local crane_bin="${AUTOSYNC_CRANE_BIN:-crane}"
  local amd64_digest=""
  local arm64_digest=""

  if command -v "${crane_bin}" >/dev/null 2>&1; then
    amd64_digest="$("${crane_bin}" digest --platform linux/amd64 "${normalized}" 2>/dev/null || true)"
    arm64_digest="$("${crane_bin}" digest --platform linux/arm64 "${normalized}" 2>/dev/null || true)"
  elif command -v docker >/dev/null 2>&1; then
    local raw_manifest=""
    raw_manifest="$(docker buildx imagetools inspect --raw "${normalized}" 2>/dev/null || true)"
    if [[ -n "${raw_manifest}" ]]; then
      local parsed_digests=""
      parsed_digests="$(python3 - "${raw_manifest}" <<'PY' 2>/dev/null || true
import json
import sys

doc = json.loads(sys.argv[1])
amd64 = ""
arm64 = ""
for m in doc.get("manifests", []):
    plat = m.get("platform") or {}
    if plat.get("os") == "linux":
        arch = plat.get("architecture")
        if arch == "amd64" and not amd64:
            amd64 = m.get("digest", "")
        elif arch == "arm64" and not arm64:
            arm64 = m.get("digest", "")
print(f"{amd64}|{arm64}")
PY
)"
      amd64_digest="${parsed_digests%%|*}"
      arm64_digest="${parsed_digests#*|}"
    fi
  fi

  python3 - "${image_ref}" "${normalized}" "${amd64_digest}" "${arm64_digest}" <<'PY'
import json
import re
import sys

DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")
image_ref, normalized, amd64, arm64 = sys.argv[1:5]
amd64 = amd64.strip()
arm64 = arm64.strip()
if not DIGEST_RE.fullmatch(amd64):
    amd64 = ""
if not DIGEST_RE.fullmatch(arm64):
    arm64 = ""
amd64_published = bool(amd64)
published = bool(amd64 and arm64 and amd64 != arm64)

print(json.dumps({
    "image": image_ref,
    "normalized": normalized,
    "published": published,
    "amd64Published": amd64_published,
    "amd64Digest": amd64,
    "arm64Digest": arm64,
}, separators=(",", ":")))
PY
}

# autosync::preflight::record_image_probes <output_json_path> <image_ref>...
# Probes all given image references and writes a schemaVersion=1 probe report JSON.
autosync::preflight::record_image_probes() {
  local output_json="${1:?output_json_path required}"
  shift
  mkdir -p "$(dirname "${output_json}")"

  local tmp_lines
  tmp_lines="$(mktemp "${TMPDIR:-/tmp}/autosync-probes.XXXXXXXX")"
  local ref
  for ref in "$@"; do
    if ! autosync::preflight::probe_image_multiarch "${ref}" >> "${tmp_lines}"; then
      rm -f "${tmp_lines}"
      return 1
    fi
  done

  if ! python3 - "${tmp_lines}" "${output_json}" <<'PY'
import json
import sys

lines_path, out_path = sys.argv[1:3]
images = {}
with open(lines_path, "r", encoding="utf-8") as f:
    for raw in f:
        raw = raw.strip()
        if not raw:
            continue
        item = json.loads(raw)
        images[item["image"]] = item

with open(out_path, "w", encoding="utf-8") as f:
    json.dump({"schemaVersion": 1, "images": images}, f, indent=2)
    f.write("\n")
PY
  then
    rm -f "${tmp_lines}"
    return 1
  fi
  rm -f "${tmp_lines}"
}

# autosync::preflight::write_github_output <key> <value>
# Appends a delimiter-safe output block to GITHUB_OUTPUT when set.
autosync::preflight::write_github_output() {
  local key="${1:?output key required}"
  local value="${2-}"
  if [[ -z "${GITHUB_OUTPUT:-}" ]]; then
    return 0
  fi
  local delim="AUTOSYNC_EOF_$$_${RANDOM:-0}"
  while [[ "${value}" == *"${delim}"* ]]; do
    delim="${delim}_X"
  done
  {
    printf '%s<<%s\n' "${key}" "${delim}"
    printf '%s\n' "${value}"
    printf '%s\n' "${delim}"
  } >> "${GITHUB_OUTPUT}"
}

# autosync::preflight::_restore_paths <repo_root> <base_sha> <rel_path>...
# Removes <rel_path>... from <repo_root> (including .gitignore-matched artifacts) and
# restores <base_sha>:<rel_path> cleanly without touching .git/index.
autosync::preflight::_restore_paths() {
  local rr="${1:?repo_root required}"
  local bs="${2:?base_sha required}"
  shift 2
  local p restore_rc=0
  for p in "$@"; do
    rm -rf -- "${rr:?}/${p:?}"
    if git -C "${rr}" cat-file -e "${bs}:${p}" 2>/dev/null; then
      if ! git -C "${rr}" archive "${bs}" -- "${p}" | tar -x -C "${rr}"; then
        restore_rc=1
      fi
    fi
  done
  return "${restore_rc}"
}

# autosync::preflight::commit_detached_source_revision <repo_root> <base_sha> <commit_msg> <rel_path>...
# Captures modifications under <rel_path>... into a detached commit object parented on
# <base_sha> using a temporary index, restores the working tree cleanly, and prints the SHA.
autosync::preflight::commit_detached_source_revision() {
  local repo_root="${1:?repo_root required}"
  local base_sha="${2:?base_sha required}"
  local commit_msg="${3:?commit_msg required}"
  shift 3
  if (( $# == 0 )); then
    printf 'FAIL: commit_detached_source_revision requires at least one relative path\n' >&2
    return 1
  fi

  local rel
  for rel in "$@"; do
    case "${rel}" in
      ""|.|./*|/*|*../*|*/..|..|*/.|*/./*|*/)
        printf 'FAIL: commit_detached_source_revision path must be safe repo-relative: %s\n' "${rel}" >&2
        return 1
        ;;
    esac
  done

  local base_tree tmp_dir tmp_index new_tree
  base_tree="$(git -C "${repo_root}" rev-parse "${base_sha}^{tree}")"
  tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/autosync-gitidx.XXXXXXXX")"
  tmp_index="${tmp_dir}/index"

  if ! GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" read-tree "${base_sha}" || \
     ! GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" add -A -- "$@" || \
     ! new_tree="$(GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" write-tree)"; then
    rm -rf "${tmp_dir}"
    autosync::preflight::_restore_paths "${repo_root}" "${base_sha}" "$@" || true
    return 1
  fi
  rm -rf "${tmp_dir}"

  if ! autosync::preflight::_restore_paths "${repo_root}" "${base_sha}" "$@"; then
    return 1
  fi

  if [[ "${new_tree}" == "${base_tree}" ]]; then
    printf '%s\n' "${base_sha}"
    return 0
  fi

  local new_commit
  if ! new_commit="$(
    GIT_AUTHOR_NAME="autosync-preflight" \
    GIT_AUTHOR_EMAIL="autosync@gpustack.local" \
    GIT_COMMITTER_NAME="autosync-preflight" \
    GIT_COMMITTER_EMAIL="autosync@gpustack.local" \
    git -C "${repo_root}" commit-tree "${new_tree}" -p "${base_sha}" -m "${commit_msg}"
  )"; then
    return 1
  fi
  printf '%s\n' "${new_commit}"
}

# autosync::preflight::commit_staged_tree_revision <repo_root> <base_sha> <commit_msg> <staged_root> <rel_path>...
# Constructs a detached commit parented on <base_sha> purely in <repo_root>'s object
# database using a temporary GIT_INDEX_FILE, replacing <rel_path>... from <staged_root>
# without ever modifying <repo_root>'s working tree or .git/index.
autosync::preflight::commit_staged_tree_revision() {
  local repo_root="${1:?repo_root required}"
  local base_sha="${2:?base_sha required}"
  local commit_msg="${3:?commit_msg required}"
  local staged_root="${4:?staged_root required}"
  shift 4
  if (( $# == 0 )); then
    printf 'FAIL: commit_staged_tree_revision requires at least one relative path\n' >&2
    return 1
  fi
  if [[ ! -d "${staged_root}" ]]; then
    printf 'FAIL: commit_staged_tree_revision staged_root does not exist: %s\n' "${staged_root}" >&2
    return 1
  fi

  local rel
  for rel in "$@"; do
    case "${rel}" in
      ""|.|./*|/*|*../*|*/..|..|*/.|*/./*|*/)
        printf 'FAIL: commit_staged_tree_revision path must be safe repo-relative: %s\n' "${rel}" >&2
        return 1
        ;;
    esac
  done

  local tmp_dir
  tmp_dir="$(mktemp -d "${TMPDIR:-/tmp}/autosync-gitidx.XXXXXXXX")"

  (
    trap 'rm -rf "${tmp_dir}"' EXIT
    trap 'exit 130' INT
    trap 'exit 143' TERM
    trap 'exit 129' HUP
    local tmp_index="${tmp_dir}/index"
    local base_tree new_tree

    base_tree="$(git -C "${repo_root}" rev-parse "${base_sha}^{tree}")"
    GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" read-tree "${base_sha}"
    GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" rm -q --cached -r --ignore-unmatch -- "$@"

    python3 - "${repo_root}" "${base_sha}" "${tmp_index}" "${staged_root}" "$@" <<'PY'
import os
import subprocess
import sys

repo_root, base_sha, tmp_index, staged_root = sys.argv[1:5]
rel_paths = sys.argv[5:]

tracked_res = subprocess.run(
    ["git", "-C", repo_root, "ls-tree", "-r", "-z", "--name-only", base_sha, "--", *rel_paths],
    capture_output=True,
    check=True,
)
tracked = {
    p.decode("utf-8", errors="surrogateescape")
    for p in tracked_res.stdout.split(b"\0")
    if p
}

candidates: list[tuple[str, str, bool]] = []
for rel in rel_paths:
    target = os.path.join(staged_root, rel)
    if os.path.islink(target):
        candidates.append((rel, target, True))
    elif os.path.isfile(target):
        candidates.append((rel, target, False))
    elif os.path.isdir(target):
        for cur, dirs, names in os.walk(target, followlinks=False):
            dirs[:] = sorted(d for d in dirs if d not in (".git", ".sbin"))
            for name in sorted(names):
                fp = os.path.join(cur, name)
                sub_rel = os.path.relpath(fp, staged_root).replace(os.sep, "/")
                if os.path.islink(fp):
                    candidates.append((sub_rel, fp, True))
                elif os.path.isfile(fp):
                    candidates.append((sub_rel, fp, False))

if candidates:
    untracked_rels = [
        r
        for r, _, _ in candidates
        if r not in tracked and r != ".auto-sync/.stage" and not r.startswith(".auto-sync/.stage/")
    ]
    ignored: set[str] = set()
    if untracked_rels:
        ci_in = b"".join(r.encode("utf-8", errors="surrogateescape") + b"\0" for r in untracked_rels)
        ci_res = subprocess.run(
            ["git", "-C", repo_root, "check-ignore", "-z", "--stdin"],
            input=ci_in,
            capture_output=True,
        )
        if ci_res.returncode not in (0, 1):
            raise SystemExit(f"FAIL: git check-ignore failed: {ci_res.stderr.decode('utf-8', errors='replace')}")
        if ci_res.returncode == 0:
            ignored = {
                p.decode("utf-8", errors="surrogateescape")
                for p in ci_res.stdout.split(b"\0")
                if p
            }

    kept = [(r, fp, is_link) for r, fp, is_link in candidates if r not in ignored]
    reg_entries = [(r, fp) for r, fp, is_link in kept if not is_link]
    link_entries = [(r, fp) for r, fp, is_link in kept if is_link]

    index_lines: list[bytes] = []
    if reg_entries:
        for _, fp in reg_entries:
            if "\n" in fp:
                raise SystemExit(f"FAIL: newline in staged file path: {fp!r}")
        stdin_paths = "\n".join(fp for _, fp in reg_entries) + "\n"
        ho_res = subprocess.run(
            ["git", "-C", repo_root, "hash-object", "-w", "--no-filters", "--stdin-paths"],
            input=stdin_paths.encode("utf-8", errors="surrogateescape"),
            capture_output=True,
            check=True,
        )
        blobs = [ln.strip() for ln in ho_res.stdout.decode("utf-8").splitlines() if ln.strip()]
        if len(blobs) != len(reg_entries):
            raise SystemExit(f"FAIL: hash-object count mismatch ({len(blobs)} != {len(reg_entries)})")
        for (r, fp), blob in zip(reg_entries, blobs):
            mode = "100755" if bool(os.stat(fp).st_mode & 0o111) else "100644"
            index_lines.append(f"{mode} {blob}\t{r}".encode("utf-8", errors="surrogateescape") + b"\0")

    for r, fp in link_entries:
        link_target = os.readlink(fp).encode("utf-8", errors="surrogateescape")
        ho_res = subprocess.run(
            ["git", "-C", repo_root, "hash-object", "-w", "--stdin"],
            input=link_target,
            capture_output=True,
            check=True,
        )
        blob = ho_res.stdout.decode("utf-8").strip()
        index_lines.append(f"120000 {blob}\t{r}".encode("utf-8", errors="surrogateescape") + b"\0")

    if index_lines:
        env = os.environ.copy()
        env["GIT_INDEX_FILE"] = tmp_index
        subprocess.run(
            ["git", "-C", repo_root, "update-index", "-z", "--index-info"],
            input=b"".join(index_lines),
            env=env,
            check=True,
        )
PY

    new_tree="$(GIT_INDEX_FILE="${tmp_index}" git -C "${repo_root}" write-tree)"
    if [[ "${new_tree}" == "${base_tree}" ]]; then
      printf '%s\n' "${base_sha}"
      exit 0
    fi

    local new_commit
    new_commit="$(
      GIT_AUTHOR_NAME="autosync-preflight" \
      GIT_AUTHOR_EMAIL="autosync@gpustack.local" \
      GIT_COMMITTER_NAME="autosync-preflight" \
      GIT_COMMITTER_EMAIL="autosync@gpustack.local" \
      git -C "${repo_root}" commit-tree "${new_tree}" -p "${base_sha}" -m "${commit_msg}"
    )"
    printf '%s\n' "${new_commit}"
  )
}

# autosync::preflight::has_available_upstreams <upstream_records_json>
# Returns 0 when at least one record has status == "available", otherwise 1.
autosync::preflight::has_available_upstreams() {
  local records_json="${1:?upstream_records_json required}"
  [[ -f "${records_json}" ]] || return 1
  python3 - "${records_json}" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    recs = json.load(f)
if isinstance(recs, list) and any(isinstance(r, dict) and r.get("status") == "available" for r in recs):
    sys.exit(0)
sys.exit(1)
PY
}

# autosync::preflight::apply_force_to_upstream_records <upstream_records_json>
# When AUTOSYNC_FORCE=true and no record currently has status == "available",
# promotes the first "up_to_date" record to "available" while keeping version unchanged.
autosync::preflight::apply_force_to_upstream_records() {
  local records_json="${1:?upstream_records_json required}"
  [[ "${AUTOSYNC_FORCE:-false}" == "true" ]] || return 0
  [[ -f "${records_json}" ]] || return 0
  python3 - "${records_json}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    recs = json.load(f)
if not isinstance(recs, list):
    sys.exit(0)
if any(isinstance(r, dict) and r.get("status") == "available" for r in recs):
    sys.exit(0)
for rec in recs:
    if isinstance(rec, dict) and rec.get("status") == "up_to_date":
        rec["status"] = "available"
        reason = str(rec.get("reason") or "").strip()
        suffix = "(forced via AUTOSYNC_FORCE=true)"
        rec["reason"] = f"{reason} {suffix}".strip()
        print(
            f"INFO: AUTOSYNC_FORCE=true force-promoted upstream '{rec.get('upstreamId', '')}' "
            f"({rec.get('version', '')}) from up_to_date to available",
            file=sys.stderr,
        )
        break
with open(path, "w", encoding="utf-8") as f:
    json.dump(recs, f, indent=2)
    f.write("\n")
PY
}

# autosync::preflight::populate_stage_bundle <staged_tree_dir> <domain> <file_or_dir_or_spec>...
# Copies read-only host context into <staged_tree_dir>/.auto-sync/.stage/<domain>/ as 0644
# regular files (resolving symlinks, skipping .git, rejecting unsafe paths and size > 8 MiB).
# Each entry may be "<src>" (lands at basename) or "<src>::<rel_dest>".
autosync::preflight::populate_stage_bundle() {
  local staged_tree_dir="${1:?staged_tree_dir required}"
  local domain="${2:?domain required}"
  shift 2
  if [[ ! "${domain}" =~ ^[a-z0-9-]+$ ]]; then
    printf 'FAIL: invalid domain name for stage bundle: %s\n' "${domain}" >&2
    return 1
  fi
  if (( $# == 0 )); then
    printf 'FAIL: populate_stage_bundle requires at least one source path\n' >&2
    return 1
  fi

  local max_bytes="${AUTOSYNC_STAGE_BUNDLE_MAX_BYTES:-8388608}"
  python3 - "${staged_tree_dir}" "${domain}" "${max_bytes}" "$@" <<'PY'
import os
import posixpath
import shutil
import sys

staged_tree_dir, domain, max_bytes_str = sys.argv[1:4]
specs = sys.argv[4:]
max_bytes = int(max_bytes_str)
if max_bytes <= 0:
    raise SystemExit(f"FAIL: invalid AUTOSYNC_STAGE_BUNDLE_MAX_BYTES: {max_bytes_str}")

bundle_root = os.path.join(staged_tree_dir, ".auto-sync", ".stage", domain)
os.makedirs(bundle_root, exist_ok=True)


def validate_rel(rel: str) -> str:
    if not rel or "\n" in rel or "\r" in rel or "\x00" in rel:
        raise SystemExit(f"FAIL: unsafe stage bundle relative path: {rel!r}")
    if rel.startswith("/") or rel.startswith("\\"):
        raise SystemExit(f"FAIL: absolute stage bundle relative path rejected: {rel!r}")
    norm = posixpath.normpath(rel.replace("\\", "/"))
    if norm in ("", ".", "..") or norm.startswith("../") or "/../" in norm:
        raise SystemExit(f"FAIL: path traversal rejected in stage bundle path: {rel!r}")
    if ".git" in norm.split("/"):
        raise SystemExit(f"FAIL: '.git' path segment rejected in stage bundle path: {rel!r}")
    return norm


total_bytes = 0
for cur, _, names in os.walk(bundle_root, followlinks=False):
    for name in names:
        fp = os.path.join(cur, name)
        if os.path.isfile(fp):
            total_bytes += os.path.getsize(fp)


def copy_regular(src_file: str, rel_dest: str) -> None:
    global total_bytes
    if not os.path.exists(src_file):
        kind = "dangling symlink" if os.path.islink(src_file) else "missing path"
        raise SystemExit(f"FAIL: stage bundle source is a {kind}: {src_file}")
    real_src = os.path.realpath(src_file)
    if not os.path.isfile(real_src):
        raise SystemExit(f"FAIL: stage bundle source is not a regular file: {src_file}")
    safe_rel = validate_rel(rel_dest)
    size = os.path.getsize(real_src)
    dst = os.path.join(bundle_root, *safe_rel.split("/"))
    if os.path.isfile(dst):
        if os.path.samefile(real_src, dst):
            raise SystemExit(f"FAIL: stage bundle source and destination are the same file: {src_file}")
        total_bytes -= os.path.getsize(dst)
    total_bytes += size
    if total_bytes > max_bytes:
        raise SystemExit(
            f"FAIL: stage bundle for {domain} exceeds {max_bytes} byte cap ({total_bytes} bytes)"
        )
    os.makedirs(os.path.dirname(dst), exist_ok=True)
    with open(real_src, "rb") as sf, open(dst, "wb") as df:
        shutil.copyfileobj(sf, df)
    os.chmod(dst, 0o644)


# Non-cyclic sibling directory symlinks are intentionally materialized into regular files at each distinct
# destination path (bounded by max_bytes); tracking ancestors avoids dropping valid aliased subtrees.
def walk_bundle_dir(
    cur_dir: str, rel_prefix: str, ancestors: frozenset[tuple[int, int]], depth: int = 0
) -> None:
    if depth > 64:
        raise SystemExit(
            f"FAIL: stage bundle directory nesting exceeds max depth 64: {cur_dir}"
        )
    if os.path.islink(cur_dir) and not os.path.exists(cur_dir):
        raise SystemExit(f"FAIL: stage bundle source contains dangling symlink: {cur_dir}")
    st = os.stat(cur_dir)
    inode_key = (st.st_dev, st.st_ino)
    if inode_key in ancestors:
        print(
            f"WARN: skipping directory symlink cycle in stage bundle: {cur_dir}",
            file=sys.stderr,
        )
        return
    next_ancestors = ancestors | {inode_key}
    for entry in sorted(os.listdir(cur_dir)):
        # Git refuses any '.git' path segment (directory or worktree/submodule gitfile) when staging trees.
        if entry == ".git":
            continue
        entry_path = os.path.join(cur_dir, entry)
        if os.path.islink(entry_path) and not os.path.exists(entry_path):
            raise SystemExit(
                f"FAIL: stage bundle source contains dangling symlink: {entry_path}"
            )
        entry_rel = (
            posixpath.join(rel_prefix, entry)
            if rel_prefix and rel_prefix != "."
            else entry
        )
        if os.path.isdir(entry_path):
            walk_bundle_dir(entry_path, entry_rel, next_ancestors, depth + 1)
        else:
            copy_regular(entry_path, entry_rel)


for spec in specs:
    if "::" in spec:
        raw_src, raw_dest = spec.split("::", 1)
    else:
        raw_src = spec
        raw_dest = os.path.basename(os.path.normpath(raw_src))
    if os.path.islink(raw_src) and not os.path.exists(raw_src):
        raise SystemExit(f"FAIL: stage bundle source is a dangling symlink: {raw_src}")
    if not os.path.exists(raw_src):
        raise SystemExit(f"FAIL: stage bundle source does not exist: {raw_src}")
    safe_base = validate_rel(raw_dest)
    real_root = os.path.realpath(raw_src)
    if os.path.isfile(real_root):
        copy_regular(real_root, safe_base)
    elif os.path.isdir(real_root):
        walk_bundle_dir(real_root, safe_base, frozenset())
    else:
        raise SystemExit(f"FAIL: unsupported stage bundle source type: {raw_src}")
PY
}
