#!/usr/bin/env bash
# .auto-sync/checks/lib.sh - shared verification helpers for .auto-sync/checks/validate_*.sh.
#
# Provides sanitized command.v1 environment bootstrap, ephemeral git index initialization
# and cleanup for gitless candidate directories, fail-closed diff -U0 shared-file guards,
# and published multi-arch container image verification.
set -euo pipefail
export PYTHONDONTWRITEBYTECODE=1

# autosync::bootstrap_env [stage_dir] [candidate_dir]
# Configures deterministic PATH, Go offline caches, and .sbin tools for command.v1 checks.
autosync::bootstrap_env() {
  local stage_dir="${1:-${AUTOSYNC_STAGE_DIR:-}}"
  local candidate_dir="${2:-${LOOPWRIGHT_CANDIDATE_DIR:-$(pwd -P)}}"

  if [[ -d "${candidate_dir}" ]]; then
    candidate_dir="$(cd "${candidate_dir}" && pwd -P)"
  fi
  export AUTOSYNC_CANDIDATE_ROOT="${candidate_dir}"
  export AUTOSYNC_IN_VALIDATOR=true

  local saved_go_bin_dir=""
  local saved_goroot=""
  local saved_hugo_bin_dir=""
  local saved_node_bin_dir=""
  local saved_uv_bin_dir=""
  local saved_crane_bin_dir=""
  local saved_docker_bin_dir=""

  if [[ -n "${stage_dir}" && -d "${stage_dir}" ]]; then
    stage_dir="$(cd "${stage_dir}" && pwd -P)"
    export AUTOSYNC_STAGE_DIR="${stage_dir}"

    if [[ -f "${stage_dir}/toolchain.env" && ! -L "${stage_dir}/toolchain.env" ]]; then
      local tc_line tc_key tc_val
      while IFS= read -r tc_line || [[ -n "${tc_line}" ]]; do
        [[ -z "${tc_line}" || "${tc_line}" == \#* ]] && continue
        [[ "${tc_line}" == *=* ]] || continue
        tc_key="${tc_line%%=*}"
        tc_val="${tc_line#*=}"
        if [[ "${tc_val}" == \"*\" && "${tc_val}" == *\" ]]; then
          tc_val="${tc_val#\"}"
          tc_val="${tc_val%\"}"
        elif [[ "${tc_val}" == \'*\' && "${tc_val}" == *\' ]]; then
          tc_val="${tc_val#\'}"
          tc_val="${tc_val%\'}"
        fi
        [[ -n "${tc_val}" && -d "${tc_val}" ]] || continue
        case "${tc_key}" in
          AUTOSYNC_SAVED_GO_BIN_DIR)
            saved_go_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_GO_BIN_DIR="${tc_val}"
            ;;
          AUTOSYNC_SAVED_GOROOT)
            saved_goroot="${tc_val}"
            export AUTOSYNC_SAVED_GOROOT="${tc_val}"
            ;;
          AUTOSYNC_SAVED_HUGO_BIN_DIR)
            saved_hugo_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_HUGO_BIN_DIR="${tc_val}"
            ;;
          AUTOSYNC_SAVED_NODE_BIN_DIR)
            saved_node_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_NODE_BIN_DIR="${tc_val}"
            ;;
          AUTOSYNC_SAVED_UV_BIN_DIR)
            saved_uv_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_UV_BIN_DIR="${tc_val}"
            ;;
          AUTOSYNC_SAVED_CRANE_BIN_DIR)
            saved_crane_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_CRANE_BIN_DIR="${tc_val}"
            ;;
          AUTOSYNC_SAVED_DOCKER_BIN_DIR)
            saved_docker_bin_dir="${tc_val}"
            export AUTOSYNC_SAVED_DOCKER_BIN_DIR="${tc_val}"
            ;;
        esac
      done < "${stage_dir}/toolchain.env"
    fi

    if [[ -n "${saved_goroot}" && -d "${saved_goroot}" ]]; then
      export GOROOT="${saved_goroot}"
    fi

    if [[ -z "${GOCACHE:-}" ]]; then
      if [[ -d "${stage_dir}/go-cache" && -w "${stage_dir}/go-cache" ]]; then
        export GOCACHE="${stage_dir}/go-cache"
      else
        local ro_gocache="${TMPDIR:-/tmp}/autosync-go-cache"
        mkdir -p "${ro_gocache}" 2>/dev/null || true
        export GOCACHE="${ro_gocache}"
      fi
    fi

    if [[ -z "${GOMODCACHE:-}" ]]; then
      export GOMODCACHE="${stage_dir}/go-mod"
    fi
    export GPUSTACK_DEPS_CACHE_DIR="${GPUSTACK_DEPS_CACHE_DIR:-${stage_dir}/deps-cache}"
    export GPUSTACK_DEPS_OFFLINE="${GPUSTACK_DEPS_OFFLINE:-true}"
    export GOPROXY="${GOPROXY:-off}"
    export GOTOOLCHAIN="${GOTOOLCHAIN:-local}"

    if [[ -n "${stage_dir}" && -d "${stage_dir}/uv-cache" && -n "$(ls -A "${stage_dir}/uv-cache/" 2>/dev/null)" ]]; then
      if [[ -z "${UV_CACHE_DIR:-}" ]]; then
        if [[ -w "${stage_dir}/uv-cache" ]]; then
          export UV_CACHE_DIR="${stage_dir}/uv-cache"
        else
          local ro_uvcache
          ro_uvcache="$(mktemp -d "${TMPDIR:-/tmp}/autosync-uv-cache-XXXXXX")"
          if ! cp -RLp "${stage_dir}/uv-cache/." "${ro_uvcache}/" 2>/dev/null || \
             [[ -z "$(ls -A "${ro_uvcache}/" 2>/dev/null)" ]]; then
            printf 'WARN: autosync: failed to copy read-only uv-cache from %s\n' "${stage_dir}/uv-cache" >&2
          else
            chmod -R u+w "${ro_uvcache}" 2>/dev/null || true
            export UV_CACHE_DIR="${ro_uvcache}"
          fi
        fi
      fi
      export UV_OFFLINE="${UV_OFFLINE:-1}"
      if [[ -z "${UV_PROJECT_ENVIRONMENT:-}" ]]; then
        export UV_PROJECT_ENVIRONMENT="${TMPDIR:-/tmp}/autosync-uv-venv-$$"
        export AUTOSYNC_EPHEMERAL_UV_ENV="${UV_PROJECT_ENVIRONMENT}"
      fi
    fi
    if [[ -n "${stage_dir}" && -d "${stage_dir}/uv-python" ]] && compgen -G "${stage_dir}/uv-python/cpython-*" >/dev/null; then
      if [[ -z "${UV_PYTHON_INSTALL_DIR:-}" ]]; then
        if [[ -w "${stage_dir}/uv-python" ]]; then
          export UV_PYTHON_INSTALL_DIR="${stage_dir}/uv-python"
        else
          local ro_uvpy
          ro_uvpy="$(mktemp -d "${TMPDIR:-/tmp}/autosync-uv-python-XXXXXX")"
          if ! cp -RPp "${stage_dir}/uv-python/." "${ro_uvpy}/" 2>/dev/null || \
             ! compgen -G "${ro_uvpy}/cpython-*" >/dev/null; then
            printf 'WARN: autosync: failed to copy read-only uv-python from %s\n' "${stage_dir}/uv-python" >&2
          else
            chmod -R u+w "${ro_uvpy}" 2>/dev/null || true
            export UV_PYTHON_INSTALL_DIR="${ro_uvpy}"
          fi
        fi
      fi
      if [[ "${UV_OFFLINE:-}" == "1" ]]; then
        export UV_PYTHON_PREFERENCE="${UV_PYTHON_PREFERENCE:-only-managed}"
      fi
    fi

    # Materialize pre-warmed .sbin entries as regular files (not a directory symlink)
    # when candidate_dir is a distinct writable copy; skip when candidate_dir == LOOPWRIGHT_BASE_DIR.
    local base_phys=""
    if [[ -n "${LOOPWRIGHT_BASE_DIR:-}" && -d "${LOOPWRIGHT_BASE_DIR}" ]]; then
      base_phys="$(cd "${LOOPWRIGHT_BASE_DIR}" && pwd -P)"
    fi
    local cand_phys="${candidate_dir}"
    if [[ -d "${candidate_dir}" ]]; then
      cand_phys="$(cd "${candidate_dir}" && pwd -P)"
    fi
    if [[ -d "${stage_dir}/.sbin" && ! -e "${candidate_dir}/.sbin" && ! -L "${candidate_dir}/.sbin" && "${cand_phys}" != "${base_phys}" ]]; then
      if [[ -w "${candidate_dir}" ]]; then
        mkdir -p "${candidate_dir}/.sbin"
        cp -RLp "${stage_dir}/.sbin/." "${candidate_dir}/.sbin/" 2>/dev/null || true
        : > "${candidate_dir}/.sbin/.autosync-materialized"
      fi
    fi
  fi

  if [[ -n "${LOOPWRIGHT_STAGED_DIR:-}" && -f "${LOOPWRIGHT_STAGED_DIR}/deps-charts-cache.tgz" ]]; then
    if [[ -z "${GPUSTACK_DEPS_CACHE_DIR:-}" ]] || ! compgen -G "${GPUSTACK_DEPS_CACHE_DIR}/charts/*.tgz" >/dev/null 2>&1; then
      local staged_deps_cache
      staged_deps_cache="$(mktemp -d "${TMPDIR:-/tmp}/autosync-deps-cache-XXXXXX")"
      mkdir -p "${staged_deps_cache}/charts"
      if tar -xzf "${LOOPWRIGHT_STAGED_DIR}/deps-charts-cache.tgz" -C "${staged_deps_cache}/charts" 2>/dev/null; then
        export GPUSTACK_DEPS_CACHE_DIR="${staged_deps_cache}"
        export AUTOSYNC_EPHEMERAL_DEPS_CACHE="${staged_deps_cache}"
      else
        rm -rf "${staged_deps_cache}" 2>/dev/null || true
      fi
    fi
  fi

  local new_path=""
  local seg
  local prefix_dirs=()
  if [[ -d "${candidate_dir}/.sbin" ]]; then
    prefix_dirs+=("${candidate_dir}/.sbin")
  fi
  if [[ -n "${stage_dir}" && -d "${stage_dir}/.sbin" ]]; then
    prefix_dirs+=("${stage_dir}/.sbin")
  fi
  if [[ -n "${stage_dir}" && -d "${stage_dir}/.sbin/protoc/bin" ]]; then
    prefix_dirs+=("${stage_dir}/.sbin/protoc/bin")
  fi
  if [[ -n "${saved_go_bin_dir}" && -d "${saved_go_bin_dir}" ]]; then
    prefix_dirs+=("${saved_go_bin_dir}")
  fi
  if [[ -n "${saved_hugo_bin_dir}" && -d "${saved_hugo_bin_dir}" ]]; then
    prefix_dirs+=("${saved_hugo_bin_dir}")
  fi
  if [[ -n "${saved_node_bin_dir}" && -d "${saved_node_bin_dir}" ]]; then
    prefix_dirs+=("${saved_node_bin_dir}")
  fi
  if [[ -n "${saved_uv_bin_dir}" && -d "${saved_uv_bin_dir}" ]]; then
    prefix_dirs+=("${saved_uv_bin_dir}")
  fi
  if [[ -n "${saved_crane_bin_dir}" && -d "${saved_crane_bin_dir}" ]]; then
    prefix_dirs+=("${saved_crane_bin_dir}")
  fi
  if [[ -n "${saved_docker_bin_dir}" && -d "${saved_docker_bin_dir}" ]]; then
    prefix_dirs+=("${saved_docker_bin_dir}")
  fi

  for seg in "${prefix_dirs[@]+"${prefix_dirs[@]}"}"; do
    [[ -n "${seg}" ]] || continue
    case ":${new_path}:" in
      *":${seg}:"*) ;;
      *) new_path="${new_path:+${new_path}:}${seg}" ;;
    esac
  done

  if [[ -n "${PATH:-}" ]]; then
    local old_ifs="${IFS}"
    IFS=':'
    for seg in ${PATH}; do
      [[ -n "${seg}" ]] || continue
      case ":${new_path}:" in
        *":${seg}:"*) ;;
        *) new_path="${new_path:+${new_path}:}${seg}" ;;
      esac
    done
    IFS="${old_ifs}"
  fi

  local fallback_dirs=(
    "/usr/local/go/bin"
    "/opt/homebrew/bin"
    "/usr/local/bin"
    "/usr/bin"
    "/bin"
    "/usr/sbin"
    "/sbin"
  )
  for seg in "${fallback_dirs[@]}"; do
    case ":${new_path}:" in
      *":${seg}:"*) ;;
      *) new_path="${new_path:+${new_path}:}${seg}" ;;
    esac
  done
  export PATH="${new_path}"
}

# autosync::_hydrate_lfs_pointers <dir> <lfs_objects_dir>
# Hydrates Git-LFS pointer files under site/ and docs/ from a staged lfs-objects directory
# after verifying sha256 digests against the pointer oid.
autosync::_hydrate_lfs_pointers() {
  local dir="${1:?directory required}"
  local lfs_objects="${2:-}"
  [[ -n "${lfs_objects}" && -d "${lfs_objects}" ]] || return 0

  python3 - "${dir}" "${lfs_objects}" <<'PY'
import hashlib
import os
import re
import sys

root_dir, lfs_objects = sys.argv[1:3]
OID_RE = re.compile(r"^oid sha256:([0-9a-f]{64})$")
SIZE_RE = re.compile(r"^size (\d+)$")
pending: list[tuple[str, str]] = []

# First pass: validate every pointer's object before modifying any files so failure leaves the tree untouched.
for sub in ("site", "docs"):
    base = os.path.join(root_dir, sub)
    if not os.path.isdir(base) or os.path.islink(base):
        continue
    for cur, dirs, files in os.walk(base, followlinks=False):
        dirs[:] = sorted(d for d in dirs if not os.path.islink(os.path.join(cur, d)))
        for name in sorted(files):
            fpath = os.path.join(cur, name)
            if os.path.islink(fpath):
                continue
            try:
                st = os.lstat(fpath)
            except OSError:
                continue
            if st.st_size == 0 or st.st_size > 1024:
                continue
            with open(fpath, "rb") as f:
                raw = f.read()
            try:
                text = raw.decode("utf-8")
            except UnicodeDecodeError:
                continue
            lines = text.splitlines()
            if not lines or lines[0] != "version https://git-lfs.github.com/spec/v1":
                continue
            oid = ""
            expected_size = None
            for ln in lines[1:]:
                m_oid = OID_RE.fullmatch(ln)
                if m_oid:
                    oid = m_oid.group(1)
                    continue
                m_sz = SIZE_RE.fullmatch(ln)
                if m_sz:
                    expected_size = int(m_sz.group(1))
            if not oid:
                continue
            obj_path = os.path.join(lfs_objects, oid[:2], oid[2:4], oid)
            if not os.path.isfile(obj_path):
                raise SystemExit(
                    f"FAIL: missing staged Git-LFS object for {os.path.relpath(fpath, root_dir)}: {obj_path}"
                )
            h = hashlib.sha256()
            total = 0
            with open(obj_path, "rb") as obj_f:
                while True:
                    chunk = obj_f.read(65536)
                    if not chunk:
                        break
                    h.update(chunk)
                    total += len(chunk)
            actual_oid = h.hexdigest()
            if actual_oid != oid:
                raise SystemExit(
                    f"FAIL: Git-LFS object sha256 mismatch for {os.path.relpath(fpath, root_dir)}: "
                    f"expected {oid}, got {actual_oid}"
                )
            if expected_size is not None and total != expected_size:
                raise SystemExit(
                    f"FAIL: Git-LFS object size mismatch for {os.path.relpath(fpath, root_dir)}: "
                    f"expected {expected_size}, got {total}"
                )
            pending.append((fpath, obj_path))

# Second pass: hydrate verified payloads in place.
for fpath, obj_path in pending:
    with open(obj_path, "rb") as src_f, open(fpath, "wb") as dst_f:
        while True:
            chunk = src_f.read(65536)
            if not chunk:
                break
            dst_f.write(chunk)
PY
}

# autosync::ensure_ephemeral_git <dir>
# Initializes a temporary git repository in a gitless candidate directory so repo linters
# and git status/diff checks can run. Never modifies a pre-existing .git directory or file.
autosync::ensure_ephemeral_git() {
  local dir="${1:?directory required}"
  if [[ ! -d "${dir}" ]]; then
    printf 'FAIL: candidate directory does not exist: %s\n' "${dir}" >&2
    return 1
  fi
  if [[ ! -e "${dir}/.git" && ! -L "${dir}/.git" ]]; then
    git -C "${dir}" init -q -b main 2>/dev/null || {
      git -C "${dir}" init -q
      git -C "${dir}" symbolic-ref HEAD refs/heads/main
    }
    : > "${dir}/.git/.autosync-ephemeral"
    mkdir -p "${dir}/.git/info"
    printf '/.sbin/\n/site/public/\n' >> "${dir}/.git/info/exclude"
    if [[ -d "${dir}/.agents/skills" && ! -e "${dir}/.claude/skills" && ! -L "${dir}/.claude/skills" ]]; then
      mkdir -p "${dir}/.claude"
      ln -s ../.agents/skills "${dir}/.claude/skills"
      printf '.claude/skills\n' >> "${dir}/.git/.autosync-ephemeral-symlinks"
    fi
    if [[ -f "${dir}/.agents/hooks.json" && ! -e "${dir}/.claude/settings.json" && ! -L "${dir}/.claude/settings.json" ]]; then
      mkdir -p "${dir}/.claude"
      ln -s ../.agents/hooks.json "${dir}/.claude/settings.json"
      printf '.claude/settings.json\n' >> "${dir}/.git/.autosync-ephemeral-symlinks"
    fi
    if [[ -f "${dir}/.agents/hooks.json" && ! -e "${dir}/.codex/hooks.json" && ! -L "${dir}/.codex/hooks.json" ]]; then
      mkdir -p "${dir}/.codex"
      ln -s ../.agents/hooks.json "${dir}/.codex/hooks.json"
      printf '.codex/hooks.json\n' >> "${dir}/.git/.autosync-ephemeral-symlinks"
    fi
    local lfs_objects_dir=""
    if [[ -n "${AUTOSYNC_STAGE_DIR:-}" && -d "${AUTOSYNC_STAGE_DIR}/lfs-objects" ]]; then
      lfs_objects_dir="${AUTOSYNC_STAGE_DIR}/lfs-objects"
    fi
    autosync::_hydrate_lfs_pointers "${dir}" "${lfs_objects_dir}" || return 1
    git -C "${dir}" config user.name "autosync-validator"
    git -C "${dir}" config user.email "autosync@gpustack.local"
    git -C "${dir}" config commit.gpgsign false
    git -C "${dir}" config core.autocrlf false
    git -C "${dir}" config gc.auto 0
    git -C "${dir}" add -A
    git -C "${dir}" -c gc.auto=0 commit -q --allow-empty -m "chore(autosync): candidate baseline"
    git -C "${dir}" update-ref refs/remotes/origin/main HEAD
  fi
}

# autosync::cleanup_ephemeral_git <dir>
# Removes ephemeral .git, symlinks, site/public, and materialized .sbin directories created by autosync helpers.
autosync::cleanup_ephemeral_git() {
  local dir="${1:?directory required}"
  if [[ -d "${dir}/.git" && -f "${dir}/.git/.autosync-ephemeral" ]]; then
    if [[ -f "${dir}/.git/.autosync-ephemeral-symlinks" ]]; then
      local rel
      while IFS= read -r rel || [[ -n "${rel}" ]]; do
        [[ -n "${rel}" && -L "${dir}/${rel}" ]] && rm -f "${dir}/${rel}"
      done < "${dir}/.git/.autosync-ephemeral-symlinks"
      rmdir "${dir}/.claude" "${dir}/.codex" 2>/dev/null || true
    fi
    if [[ -d "${dir}/site/public" ]]; then
      rm -rf "${dir}/site/public"
    fi
    rm -rf "${dir}/.git"
  fi
  if [[ -d "${dir}/.sbin" && -f "${dir}/.sbin/.autosync-materialized" ]]; then
    rm -rf "${dir}/.sbin"
  fi
  if [[ -n "${AUTOSYNC_EPHEMERAL_UV_ENV:-}" && -d "${AUTOSYNC_EPHEMERAL_UV_ENV}" ]]; then
    rm -rf "${AUTOSYNC_EPHEMERAL_UV_ENV}" 2>/dev/null || true
  fi
  if [[ -n "${AUTOSYNC_EPHEMERAL_DEPS_CACHE:-}" && -d "${AUTOSYNC_EPHEMERAL_DEPS_CACHE}" ]]; then
    rm -rf "${AUTOSYNC_EPHEMERAL_DEPS_CACHE}" 2>/dev/null || true
  fi
}

# autosync::resolve_baseline_dir [stage_dir]
# Prints canonical LOOPWRIGHT_BASE_DIR when provided by Taskpack 2.0 command.v1,
# falling back to <stage_dir>/baseline for standalone invocations.
autosync::resolve_baseline_dir() {
  local stage_dir="${1:-${AUTOSYNC_STAGE_DIR:-}}"
  if [[ -n "${LOOPWRIGHT_BASE_DIR:-}" && -d "${LOOPWRIGHT_BASE_DIR}" ]]; then
    (cd "${LOOPWRIGHT_BASE_DIR}" && pwd -P)
    return 0
  fi
  if [[ -n "${stage_dir}" && -d "${stage_dir}/baseline" ]]; then
    (cd "${stage_dir}/baseline" && pwd -P)
    return 0
  fi
  return 0
}

# autosync::resolve_staged_file <filename> [stage_dir]
# Prints path in LOOPWRIGHT_STAGED_DIR (Taskpack 2.0 exposeToChecks) when present,
# falling back to <stage_dir>/<filename>.
autosync::resolve_staged_file() {
  local filename="${1:?filename required}"
  local stage_dir="${2:-${AUTOSYNC_STAGE_DIR:-}}"
  if [[ -n "${LOOPWRIGHT_STAGED_DIR:-}" && -f "${LOOPWRIGHT_STAGED_DIR}/${filename}" ]]; then
    printf '%s\n' "${LOOPWRIGHT_STAGED_DIR}/${filename}"
    return 0
  fi
  if [[ -n "${stage_dir}" && -f "${stage_dir}/${filename}" ]]; then
    printf '%s\n' "${stage_dir}/${filename}"
    return 0
  fi
  return 0
}

# autosync::is_check_only_unchanged <baseline_dir> <candidate_dir> <rel_path>...
# Returns 0 when running under LOOPWRIGHT_BASE_DIR and every listed relative file/dir
# in <candidate_dir> is byte-identical to <baseline_dir>.
autosync::is_check_only_unchanged() {
  local base_dir="${1:-}"
  local cand_dir="${2:-}"
  shift 2 || return 1
  [[ -n "${LOOPWRIGHT_BASE_DIR:-}" && "${AUTOSYNC_VERIFY_UNCHANGED:-false}" != "true" ]] || return 1
  [[ -n "${base_dir}" && -d "${base_dir}" && -n "${cand_dir}" && -d "${cand_dir}" ]] || return 1

  local rel bp cp
  for rel in "$@"; do
    bp="${base_dir}/${rel}"
    cp="${cand_dir}/${rel}"
    if [[ ! -e "${bp}" && ! -e "${cp}" ]]; then
      continue
    fi
    if [[ -f "${bp}" && -f "${cp}" ]]; then
      cmp -s "${bp}" "${cp}" || return 1
    elif [[ -d "${bp}" && -d "${cp}" ]]; then
      diff -rq -- "${bp}" "${cp}" >/dev/null 2>&1 || return 1
    else
      return 1
    fi
  done
  return 0
}

# autosync::write_check_out <passed_bool> <summary>
# Writes structured command.v1 output JSON to LOOPWRIGHT_CHECK_OUT when set.
autosync::write_check_out() {
  local passed="${1:?passed boolean required}"
  local summary="${2:?summary required}"
  [[ -n "${LOOPWRIGHT_CHECK_OUT:-}" ]] || return 0
  python3 - "${LOOPWRIGHT_CHECK_OUT}" "${passed}" "${summary}" <<'PY'
import json
import os
import sys

out_path, passed_str, summary = sys.argv[1:4]
os.makedirs(os.path.dirname(os.path.abspath(out_path)), exist_ok=True)
with open(out_path, "w", encoding="utf-8") as f:
    json.dump(
        {
            "passed": passed_str == "true",
            "summary": summary[:1024],
        },
        f,
        indent=2,
    )
    f.write("\n")
PY
}

# autosync::copy_tree_without_git <src_dir> <dst_dir> [extra_excluded_top...]
# Copies <src_dir> into <dst_dir> excluding top-level .git (and any extra top-level names),
# using APFS clonefile (cp -Rc) or Linux reflink (cp -R --reflink=auto) when supported.
autosync::copy_tree_without_git() {
  local src_dir="${1:?src_dir required}"
  local dst_dir="${2:?dst_dir required}"
  shift 2
  mkdir -p "${dst_dir}"

  local cp_mode="tar"
  local probe_root="${dst_dir}/.autosync-cp-probe-$$"
  if mkdir -p "${probe_root}/s" 2>/dev/null && : > "${probe_root}/s/f" 2>/dev/null; then
    if cp -Rc "${probe_root}/s" "${probe_root}/d" 2>/dev/null; then
      cp_mode="apfs"
    elif cp -R --reflink=auto "${probe_root}/s" "${probe_root}/d" 2>/dev/null; then
      cp_mode="reflink"
    fi
  fi
  rm -rf "${probe_root}" 2>/dev/null || true

  if [[ "${cp_mode}" == "apfs" || "${cp_mode}" == "reflink" ]]; then
    local entry base skip ex
    while IFS= read -r -d '' entry; do
      base="$(basename "${entry}")"
      [[ "${base}" == "." || "${base}" == ".." || "${base}" == ".git" ]] && continue
      skip=false
      for ex in "$@"; do
        if [[ "${base}" == "${ex}" ]]; then
          skip=true
          break
        fi
      done
      [[ "${skip}" == "true" ]] && continue
      if [[ "${cp_mode}" == "apfs" ]]; then
        cp -Rc "${entry}" "${dst_dir}/${base}" || return 1
      else
        cp -R --reflink=auto "${entry}" "${dst_dir}/${base}" || return 1
      fi
    done < <(find "${src_dir}" -mindepth 1 -maxdepth 1 -print0)
    return 0
  fi

  local tar_excludes=(--exclude='.git')
  local ex_item
  for ex_item in "$@"; do
    tar_excludes+=(--exclude="${ex_item}")
  done
  (
    set -o pipefail
    tar "${tar_excludes[@]}" -cf - -C "${src_dir}" . | tar -xf - -C "${dst_dir}"
  ) || return 1
}

# autosync::run_in_module_path_copy <target_dir> <cmd> [args...]
# Runs <cmd> inside a physical directory ending in /gpustack.ai/gpustack (required by
# gen/api/main.go) and syncs regular-file additions, edits, and deletions back to <target_dir>.
autosync::run_in_module_path_copy() {
  local target_dir="${1:?target_dir required}"
  shift
  if (( $# == 0 )); then
    printf 'FAIL: run_in_module_path_copy requires a command\n' >&2
    return 1
  fi
  if [[ ! -d "${target_dir}" ]]; then
    printf 'FAIL: target directory does not exist: %s\n' "${target_dir}" >&2
    return 1
  fi

  local phys_target
  phys_target="$(cd "${target_dir}" && pwd -P)"
  if [[ "${phys_target}" == */gpustack.ai/gpustack ]]; then
    (
      cd "${phys_target}"
      "$@"
    )
    return $?
  fi

  local tmp_base mod_base mod_copy
  tmp_base="$(cd "${TMPDIR:-/tmp}" && pwd -P)"
  mod_base="$(mktemp -d "${tmp_base}/autosync-modpath-XXXXXX")"
  mod_copy="${mod_base}/gpustack.ai/gpustack"

  local rc=0
  (
    trap 'rm -rf "${mod_base:-}"' EXIT
    autosync::copy_tree_without_git "${phys_target}" "${mod_copy}"
    local cmd_rc=0
    (
      cd "${mod_copy}"
      "$@"
    ) || cmd_rc=$?
    if (( cmd_rc != 0 )); then
      exit "${cmd_rc}"
    fi
    python3 - "${mod_copy}" "${phys_target}" <<'PY'
import os
import shutil
import sys

src_root, dst_root = sys.argv[1:3]
EXCLUDED_TOP = {".git", ".sbin"}


def collect_regular_files(root: str) -> dict[str, str]:
    out: dict[str, str] = {}
    for cur, dirs, names in os.walk(root, followlinks=False):
        rel_cur = os.path.relpath(cur, root)
        if rel_cur == ".":
            dirs[:] = [d for d in sorted(dirs) if d not in EXCLUDED_TOP]
        else:
            dirs.sort()
        dirs[:] = [d for d in dirs if not os.path.islink(os.path.join(cur, d))]
        for name in sorted(names):
            if rel_cur == "." and name in EXCLUDED_TOP:
                continue
            fp = os.path.join(cur, name)
            if os.path.islink(fp) or not os.path.isfile(fp):
                continue
            rel = name if rel_cur == "." else os.path.join(rel_cur, name)
            out[rel] = fp
    return out


src_files = collect_regular_files(src_root)
dst_files = collect_regular_files(dst_root)

for rel in sorted(set(dst_files.keys()) - set(src_files.keys())):
    dst_fp = dst_files[rel]
    os.unlink(dst_fp)
    parent = os.path.dirname(dst_fp)
    while parent and parent != dst_root:
        try:
            os.rmdir(parent)
        except OSError:
            break
        parent = os.path.dirname(parent)

for rel, src_fp in sorted(src_files.items()):
    dst_fp = os.path.join(dst_root, rel)
    os.makedirs(os.path.dirname(dst_fp), exist_ok=True)
    need_copy = True
    if os.path.isfile(dst_fp) and not os.path.islink(dst_fp):
        with open(src_fp, "rb") as fs, open(dst_fp, "rb") as fd:
            if fs.read() == fd.read() and (os.stat(src_fp).st_mode & 0o777) == (os.stat(dst_fp).st_mode & 0o777):
                need_copy = False
    if need_copy:
        shutil.copy2(src_fp, dst_fp)
PY
    exit "${cmd_rc}"
  ) || rc=$?
  rm -rf "${mod_base}" 2>/dev/null || true
  return "${rc}"
}

# assert_dockerfile_only_args <baseline_file> <candidate_file> <allowed_arg_regex>
# Fails closed unless every modified line in pack/gpustack-operator/Dockerfile is an
# ARG assignment whose name matches <allowed_arg_regex>.
assert_dockerfile_only_args() {
  local baseline_file="${1:?baseline Dockerfile path required}"
  local candidate_file="${2:?candidate Dockerfile path required}"
  local allowed_regex="${3:?allowed ARG regex required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) Dockerfile\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing Dockerfile\n' "${diff_rc}" >&2
    return 1
  fi

  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+\ |---\ |@@\ ) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]] && [[ ! "${line}" =~ ^[+-]ARG\ (${allowed_regex})=\"[0-9A-Za-z._+-]+\"$ ]]; then
      printf 'FAIL: Dockerfile modified outside allowed ARGs (%s): %s\n' \
        "${allowed_regex}" "${line}" >&2
      return 1
    fi
  done <<<"${raw_diff}"

  python3 - "${baseline_file}" "${candidate_file}" "${allowed_regex}" <<'PY'
import re
import sys

baseline_path, candidate_path, allowed_pattern = sys.argv[1:4]
allowed_re = re.compile(allowed_pattern)
arg_re = re.compile(r'^(ARG )([A-Z0-9_]+)(=")([0-9A-Za-z._+-]+)(")$')
from_re = re.compile(r'^\s*FROM\b')


def mask_dockerfile(text: str) -> str:
    out = []
    seen_from = False
    for line in text.splitlines(keepends=True):
        stripped = line.rstrip("\r\n")
        if from_re.match(stripped):
            seen_from = True
        if not seen_from:
            m = arg_re.match(stripped)
            if m and allowed_re.fullmatch(m.group(2)):
                ending = line[len(stripped):]
                line = f"{m.group(1)}{m.group(2)}{m.group(3)}__MASKED__{m.group(5)}{ending}"
        out.append(line)
    return "".join(out)


with open(baseline_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(candidate_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if mask_dockerfile(base_txt) != mask_dockerfile(cand_txt):
    print(
        f"FAIL: Dockerfile modified outside top-level allowed ARGs ({allowed_pattern})",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# assert_value_go_only_keys <baseline_file> <candidate_file> <allowed_setting_keys_regex>
# Verifies that only setting.InitializeFromEnv("...") literals belonging to settings whose
# key matches <allowed_setting_keys_regex> changed in pkg/worker/settings/value.go.
assert_value_go_only_keys() {
  local baseline_file="${1:?baseline value.go path required}"
  local candidate_file="${2:?candidate value.go path required}"
  local allowed_regex="${3:?allowed setting keys regex required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) value.go\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing value.go\n' "${diff_rc}" >&2
    return 1
  fi

  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+|---|@@) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]] && [[ ! "${line}" =~ ^[+-][[:space:]]*setting\.InitializeFromEnv\(\"[^\"]+\"\),$ ]]; then
      printf 'FAIL: value.go modified outside setting.InitializeFromEnv literal: %s\n' "${line}" >&2
      return 1
    fi
  done <<<"${raw_diff}"

  python3 - "${baseline_file}" "${candidate_file}" "${allowed_regex}" <<'PY'
import re
import sys

baseline_path, candidate_path, allowed_pattern = sys.argv[1:4]
allowed_re = re.compile(allowed_pattern)
start_re = re.compile(r'settings\.NewEditable\(\s*"([^"]+)"')
env_re = re.compile(r'(setting\.InitializeFromEnv\()"[^"]*"(\))')


def find_call_end(text: str, open_paren_idx: int) -> int:
    depth = 1
    i = open_paren_idx + 1
    n = len(text)
    while i < n and depth > 0:
        ch = text[i]
        if ch == '"':
            i += 1
            while i < n:
                if text[i] == "\\":
                    i += 2
                    continue
                if text[i] == '"':
                    break
                i += 1
        elif ch == "`":
            i += 1
            while i < n and text[i] != "`":
                i += 1
        elif ch == "/" and i + 1 < n and text[i + 1] == "/":
            nl = text.find("\n", i)
            i = n if nl == -1 else nl
        elif ch == "/" and i + 1 < n and text[i + 1] == "*":
            end_c = text.find("*/", i + 2)
            i = n if end_c == -1 else end_c + 1
        elif ch == "(":
            depth += 1
        elif ch == ")":
            depth -= 1
            if depth == 0:
                return i
        i += 1
    return -1


def mask_allowed(text: str) -> str:
    out = []
    pos = 0
    for m in start_re.finditer(text):
        key = m.group(1)
        open_idx = text.find("(", m.start())
        end_idx = find_call_end(text, open_idx)
        if end_idx == -1:
            raise SystemExit("FAIL: unbalanced parentheses in value.go")
        if m.start() < pos:
            continue
        if allowed_re.fullmatch(key):
            out.append(text[pos : m.start()])
            block = text[m.start() : end_idx + 1]
            masked_block, count = env_re.subn(r'\1"__MASKED__"\2', block, count=1)
            if count != 1:
                raise SystemExit(
                    f"FAIL: allowlisted setting '{key}' lacks single-line setting.InitializeFromEnv(...)"
                )
            out.append(masked_block)
            pos = end_idx + 1
    out.append(text[pos:])
    return "".join(out)


with open(baseline_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(candidate_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if mask_allowed(base_txt) != mask_allowed(cand_txt):
    print(
        f"FAIL: value.go modified outside allowed setting keys ({allowed_pattern})",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# assert_settings_md_only_keys <baseline_file> <candidate_file> <allowed_setting_keys_regex>
# Verifies that only Default: `...` values on table rows whose setting key matches
# <allowed_setting_keys_regex> changed in docs/reference/settings.md.
assert_settings_md_only_keys() {
  local baseline_file="${1:?baseline settings.md path required}"
  local candidate_file="${2:?candidate settings.md path required}"
  local allowed_regex="${3:?allowed setting keys regex required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) settings.md\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing settings.md\n' "${diff_rc}" >&2
    return 1
  fi

  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+|---|@@) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]] && [[ ! "${line}" =~ ^[+-]\|\ \`(${allowed_regex})\`\<br/\> ]]; then
      printf 'FAIL: settings.md modified outside allowed setting rows (%s): %s\n' \
        "${allowed_regex}" "${line}" >&2
      return 1
    fi
  done <<<"${raw_diff}"

  python3 - "${baseline_file}" "${candidate_file}" "${allowed_regex}" <<'PY'
import re
import sys

baseline_path, candidate_path, allowed_pattern = sys.argv[1:4]
allowed_re = re.compile(allowed_pattern)
row_re = re.compile(r'^\| `([a-z0-9-]+)`<br/>')
default_re = re.compile(r'(<br/>Default: `)[^`]+(`)')


def mask_rows(text: str) -> str:
    out = []
    for line in text.splitlines(keepends=True):
        m = row_re.match(line)
        if m and allowed_re.fullmatch(m.group(1)):
            line, count = default_re.subn(r"\1__MASKED__\2", line, count=1)
            if count != 1:
                raise SystemExit(
                    f"FAIL: settings.md row '{m.group(1)}' lacks Default: `...` cell"
                )
        out.append(line)
    return "".join(out)


with open(baseline_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(candidate_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if mask_rows(base_txt) != mask_rows(cand_txt):
    print(
        f"FAIL: settings.md modified outside Default value of allowed keys ({allowed_pattern})",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# assert_values_yaml_only_model_transfer_images <baseline_file> <candidate_file>
# Verifies that only nvidia/amd/ascend image values under top-level modelTransfer.images
# changed in deploy/gpustack-operator/chart/values.yaml.
assert_values_yaml_only_model_transfer_images() {
  local baseline_file="${1:?baseline values.yaml path required}"
  local candidate_file="${2:?candidate values.yaml path required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) values.yaml\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing values.yaml\n' "${diff_rc}" >&2
    return 1
  fi

  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+|---|@@) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]] && [[ ! "${line}" =~ ^[+-]\ \ \ \ (nvidia|amd|ascend):[[:space:]]+[^[:space:]#]+ ]]; then
      printf 'FAIL: values.yaml modified outside modelTransfer.images.{nvidia,amd,ascend}: %s\n' \
        "${line}" >&2
      return 1
    fi
  done <<<"${raw_diff}"

  python3 - "${baseline_file}" "${candidate_file}" <<'PY'
import re
import sys

baseline_path, candidate_path = sys.argv[1:3]
img_re = re.compile(r'^(    (?:nvidia|amd|ascend):\s*)(?:"[^"]+"|\'[^\']+\'|[^#\s]+)(\s*(?:#.*)?)$')


def mask_model_transfer_images(text: str) -> str:
    out = []
    in_mt = False
    in_images = False
    for line in text.splitlines(keepends=True):
        stripped = line.rstrip("\r\n")
        if stripped != "" and not stripped.lstrip().startswith("#"):
            if not stripped.startswith(" "):
                in_mt = stripped == "modelTransfer:" or stripped.startswith("modelTransfer: ")
                in_images = False
            elif in_mt and re.match(r'^  [A-Za-z0-9_-]+:', stripped):
                in_images = stripped == "  images:" or stripped.startswith("  images: ")
            elif in_mt and in_images and re.match(r'^    (nvidia|amd|ascend):', stripped):
                m = img_re.match(stripped)
                if m:
                    ending = line[len(stripped):]
                    line = f"{m.group(1)}__MASKED__{m.group(2)}{ending}"
        out.append(line)
    return "".join(out)


with open(baseline_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(candidate_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if mask_model_transfer_images(base_txt) != mask_model_transfer_images(cand_txt):
    print(
        "FAIL: values.yaml modified outside modelTransfer.images.{nvidia,amd,ascend}",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# assert_values_yaml_only_chart_sections <baseline_file> <candidate_file>
# Verifies that only the 5 vendored subchart blocks (kueue, node-feature-discovery,
# csi-driver-nfs, csi-driver-s3, topograph) and modelManager.registrar.image.{repository,tag}
# changed in deploy/gpustack-operator/chart/values.yaml.
assert_values_yaml_only_chart_sections() {
  local baseline_file="${1:?baseline values.yaml path required}"
  local candidate_file="${2:?candidate values.yaml path required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) values.yaml\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  if cmp -s "${baseline_file}" "${candidate_file}"; then
    return 0
  fi

  python3 - "${baseline_file}" "${candidate_file}" <<'PY'
import copy
import json
import re
import subprocess
import sys

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

OWNED_SUBCHARTS = {
    "kueue",
    "node-feature-discovery",
    "csi-driver-nfs",
    "csi-driver-s3",
    "topograph",
}
OWNED_TOP = OWNED_SUBCHARTS | {"modelManager"}
TOP_KEY_RE = re.compile(r"^([A-Za-z0-9_-]+):(?:\s|$)")
IMG_VAL_RE = re.compile(
    r'^(      (?:repository|tag):\s*)(?:"[^"]+"|\'[^\']+\'|[^#\s]+)(\s*(?:#.*)?)$'
)


def strict_load_yaml(raw: str, label: str):
    if yaml_mod is None:
        ruby_script = r'''
require "yaml"
require "json"
raw = STDIN.read
doc = YAML.parse(raw)
abort("empty YAML document") unless doc
walk = lambda do |node|
  abort("YAML anchor rejected") if node.respond_to?(:anchor) && node.anchor
  abort("YAML alias rejected") if node.is_a?(Psych::Nodes::Alias)
  if node.is_a?(Psych::Nodes::Mapping)
    seen = {}
    node.children.each_slice(2) do |k, v|
      if k.is_a?(Psych::Nodes::Scalar)
        abort("duplicate YAML key: #{k.value}") if seen.key?(k.value)
        seen[k.value] = true
      end
      walk.call(k)
      walk.call(v) if v
    end
  elsif node.respond_to?(:children) && node.children
    node.children.each { |c| walk.call(c) }
  end
end
walk.call(doc)
puts JSON.generate(YAML.safe_load(raw))
'''
        res = subprocess.run(
            ["ruby", "-e", ruby_script],
            input=raw,
            text=True,
            capture_output=True,
        )
        if res.returncode != 0:
            raise SystemExit(f"FAIL: invalid {label} YAML: {res.stderr.strip()}")
        doc = json.loads(res.stdout)
    else:
        docs = list(yaml_mod.safe_load_all(raw))
        if len(docs) != 1:
            raise SystemExit(f"FAIL: expected single YAML document in {label}")
        stack = []

        def mark_container_item():
            if stack and stack[-1][0] == "map":
                kind, seen, expect_key = stack[-1]
                stack[-1] = (kind, seen, not expect_key)

        for ev in yaml_mod.parse(raw):
            if isinstance(ev, yaml_mod.AliasEvent):
                raise SystemExit(f"FAIL: YAML alias rejected in {label}")
            if getattr(ev, "anchor", None) is not None:
                raise SystemExit(f"FAIL: YAML anchor rejected in {label}")
            if isinstance(ev, yaml_mod.MappingStartEvent):
                mark_container_item()
                stack.append(("map", set(), True))
            elif isinstance(ev, yaml_mod.MappingEndEvent):
                stack.pop()
            elif isinstance(ev, yaml_mod.SequenceStartEvent):
                mark_container_item()
                stack.append(("seq", None, False))
            elif isinstance(ev, yaml_mod.SequenceEndEvent):
                stack.pop()
            elif isinstance(ev, yaml_mod.ScalarEvent):
                if stack and stack[-1][0] == "map":
                    kind, seen, expect_key = stack[-1]
                    if expect_key:
                        if ev.value in seen:
                            raise SystemExit(f"FAIL: duplicate YAML key {ev.value!r} in {label}")
                        seen.add(ev.value)
                    stack[-1] = (kind, seen, not expect_key)
        doc = docs[0]
    if not isinstance(doc, dict):
        raise SystemExit(f"FAIL: {label} YAML root must be a mapping")
    return doc


def split_top_sections(text: str):
    lines = text.splitlines(keepends=True)
    raw_starts = []
    for idx, line in enumerate(lines):
        if line.startswith("---"):
            continue
        m = TOP_KEY_RE.match(line)
        if m:
            raw_starts.append((idx, m.group(1)))
    if not raw_starts:
        return text, []

    starts = []
    for idx, key in raw_starts:
        start = idx
        if key in OWNED_SUBCHARTS:
            j = idx - 1
            while j >= 0 and lines[j].startswith("#"):
                j -= 1
            if j < idx - 1 and (j < 0 or lines[j].strip() == ""):
                start = j + 1
        starts.append((start, key))

    preamble = "".join(lines[: starts[0][0]])
    sections = []
    for pos, (start, key) in enumerate(starts):
        end = starts[pos + 1][0] if pos + 1 < len(starts) else len(lines)
        sections.append((key, "".join(lines[start:end])))
    return preamble, sections


def mask_model_manager_dict(mm_obj, label: str):
    if not isinstance(mm_obj, dict):
        raise SystemExit(f"FAIL: {label} modelManager must be a mapping")
    out = copy.deepcopy(mm_obj)
    reg = out.get("registrar")
    if isinstance(reg, dict) and isinstance(reg.get("image"), dict):
        img = reg["image"]
        for key in ("repository", "tag"):
            if key in img:
                val = img[key]
                if not isinstance(val, str) or not val.strip():
                    raise SystemExit(
                        f"FAIL: {label} modelManager.registrar.image.{key} must be a non-empty string"
                    )
                img[key] = "__MASKED__"
    return out


def mask_model_manager_raw(section_text: str) -> str:
    out = []
    in_registrar = False
    in_image = False
    for line in section_text.splitlines(keepends=True):
        stripped = line.rstrip("\r\n")
        if stripped != "" and not stripped.lstrip().startswith("#"):
            if re.match(r"^  [A-Za-z0-9_-]+:", stripped):
                in_registrar = stripped == "  registrar:" or stripped.startswith("  registrar: ")
                in_image = False
            elif in_registrar and re.match(r"^    [A-Za-z0-9_-]+:", stripped):
                in_image = stripped == "    image:" or stripped.startswith("    image: ")
            elif in_registrar and in_image and re.match(r"^      (?:repository|tag):", stripped):
                m = IMG_VAL_RE.match(stripped)
                if m:
                    ending = line[len(stripped):]
                    line = f"{m.group(1)}__MASKED__{m.group(2)}{ending}"
        out.append(line)
    return "".join(out)


base_path, cand_path = sys.argv[1:3]
with open(base_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(cand_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

base_doc = strict_load_yaml(base_txt, "baseline values.yaml")
cand_doc = strict_load_yaml(cand_txt, "candidate values.yaml")

base_unowned = set(base_doc.keys()) - OWNED_TOP
cand_unowned = set(cand_doc.keys()) - OWNED_TOP
if base_unowned != cand_unowned:
    added = sorted(cand_unowned - base_unowned)
    removed = sorted(base_unowned - cand_unowned)
    raise SystemExit(
        f"FAIL: values.yaml unowned top-level keys changed (added={added}, removed={removed})"
    )

for key in sorted(base_unowned):
    if base_doc[key] != cand_doc[key]:
        raise SystemExit(f"FAIL: values.yaml modified outside owned chart sections: {key}")

if "modelManager" in base_doc or "modelManager" in cand_doc:
    if "modelManager" not in base_doc or "modelManager" not in cand_doc:
        raise SystemExit("FAIL: values.yaml modelManager top-level key added or removed")
    if mask_model_manager_dict(base_doc["modelManager"], "baseline") != mask_model_manager_dict(
        cand_doc["modelManager"], "candidate"
    ):
        raise SystemExit(
            "FAIL: values.yaml modelManager modified outside modelManager.registrar.image.{repository,tag}"
        )

base_preamble, base_sections = split_top_sections(base_txt)
cand_preamble, cand_sections = split_top_sections(cand_txt)
if base_preamble != cand_preamble:
    raise SystemExit("FAIL: values.yaml preamble before first section was modified")

base_unowned_order = [k for k, _ in base_sections if k not in OWNED_TOP]
cand_unowned_order = [k for k, _ in cand_sections if k not in OWNED_TOP]
if base_unowned_order != cand_unowned_order:
    raise SystemExit("FAIL: values.yaml unowned top-level section ordering changed")

base_sec_map = dict(base_sections)
cand_sec_map = dict(cand_sections)
for key in base_unowned_order:
    if base_sec_map.get(key) != cand_sec_map.get(key):
        raise SystemExit(
            f"FAIL: values.yaml raw section outside owned chart sections changed: {key}"
        )

if "modelManager" in base_sec_map or "modelManager" in cand_sec_map:
    if mask_model_manager_raw(base_sec_map.get("modelManager", "")) != mask_model_manager_raw(
        cand_sec_map.get("modelManager", "")
    ):
        raise SystemExit(
            "FAIL: values.yaml modelManager raw lines modified outside registrar.image.{repository,tag}"
        )
PY
}

# assert_deps_sh_only_section <baseline_file> <candidate_file> <charts|mods>
# Verifies that only lines inside chart_staging() or mod_staging() heredoc changed in hack/deps.sh.
assert_deps_sh_only_section() {
  local baseline_file="${1:?baseline deps.sh path required}"
  local candidate_file="${2:?candidate deps.sh path required}"
  local section="${3:?section (charts|mods) required}"

  if [[ ! -f "${baseline_file}" || ! -f "${candidate_file}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) deps.sh\n' \
      "${baseline_file}" "${candidate_file}" >&2
    return 1
  fi

  local line_regex=""
  case "${section}" in
    charts)
      line_regex='^[+-]https://[A-Za-z0-9._/%-]+ +v?[0-9][A-Za-z0-9._+-]* +\$\{charts_dir\}/(kueue|node-feature-discovery|csi-driver-nfs|csi-driver-s3|topograph)$'
      ;;
    mods)
      line_regex='^[+-]https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+ +v?[0-9][A-Za-z0-9._+-]* +\$\{staging_dir\}/(k8s\.io|github\.com)/[A-Za-z0-9._/-]+$'
      ;;
    *)
      printf 'FAIL: unsupported deps.sh section "%s" (expected charts or mods)\n' "${section}" >&2
      return 1
      ;;
  esac

  local raw_diff diff_rc=0
  raw_diff="$(diff -U0 -- "${baseline_file}" "${candidate_file}")" || diff_rc=$?
  if (( diff_rc == 0 )); then
    return 0
  fi
  if (( diff_rc > 1 )); then
    printf 'FAIL: diff exited %d comparing deps.sh\n' "${diff_rc}" >&2
    return 1
  fi

  local line
  while IFS= read -r line; do
    if [[ "${line}" =~ ^(\+\+\+\ |---\ |@@\ ) ]]; then
      continue
    fi
    if [[ "${line}" =~ ^[+-] ]]; then
      if [[ "${line}" == *..* ]] || [[ ! "${line}" =~ ${line_regex} ]]; then
        printf 'FAIL: hack/deps.sh modified outside allowed %s heredoc entries: %s\n' \
          "${section}" "${line}" >&2
        return 1
      fi
    fi
  done <<<"${raw_diff}"

  python3 - "${baseline_file}" "${candidate_file}" "${section}" <<'PY'
import re
import sys
import urllib.parse

baseline_path, candidate_path, section = sys.argv[1:4]
URL_RE = re.compile(r'^https://[A-Za-z0-9._/%-]+$')
MOD_URL_RE = re.compile(r'^https://github\.com/[A-Za-z0-9._-]+/[A-Za-z0-9._-]+$')
VER_RE = re.compile(r'^v?[0-9][A-Za-z0-9._+-]*$')
FORBIDDEN_CHARS = set('`();&|<>\\"\'')

if section == "charts":
    fn_name = "chart_staging"
    dest_re = re.compile(
        r'^\$\{charts_dir\}/(kueue|node-feature-discovery|csi-driver-nfs|csi-driver-s3|topograph)$'
    )
    prefix_token = "${charts_dir}/"
else:
    fn_name = "mod_staging"
    dest_re = re.compile(
        r'^\$\{staging_dir\}/(k8s\.io|github\.com)/[A-Za-z0-9._/-]+$'
    )
    prefix_token = "${staging_dir}/"

fn_re = re.compile(
    rf'(function {fn_name}\(\)\s*\{{.*?cat <<EOF\n)(.*?)(\nEOF\n.*?\n\}})',
    re.DOTALL,
)


def chart_repo_identity(url: str) -> str:
    parsed = urllib.parse.urlsplit(url)
    segs = [s for s in parsed.path.split("/") if s]
    host = parsed.netloc.lower()
    if host in ("github.com", "raw.githubusercontent.com") and len(segs) >= 2:
        return f"https://{host}/{segs[0]}/{segs[1]}"
    if len(segs) >= 1:
        return f"https://{host}/{segs[0]}"
    return f"https://{host}"


def mask_entry(entry: str, validate: bool) -> str:
    if ".." in entry or any(ch in FORBIDDEN_CHARS for ch in entry):
        raise SystemExit(f"FAIL: forbidden characters or '..' in {fn_name}() heredoc: {entry}")
    parts = entry.split()
    if len(parts) != 3 or " ".join(parts) != entry:
        raise SystemExit(f"FAIL: malformed entry in {fn_name}() heredoc: {entry}")
    url, version, dest = parts
    if validate:
        if "$" in url or "$" in version:
            raise SystemExit(f"FAIL: '$' not allowed in URL/version in {fn_name}(): {entry}")
        if not dest.startswith(prefix_token) or "$" in dest[len(prefix_token):]:
            raise SystemExit(f"FAIL: invalid '$' in dest in {fn_name}(): {entry}")
        if not URL_RE.fullmatch(url) or not VER_RE.fullmatch(version) or not dest_re.fullmatch(dest):
            raise SystemExit(f"FAIL: invalid entry in {fn_name}() heredoc: {entry}")
        if section == "mods" and not MOD_URL_RE.fullmatch(url):
            raise SystemExit(f"FAIL: invalid mod repo URL in {fn_name}(): {entry}")
    if section == "mods":
        return f"{url} __VER__ {dest}"
    return f"{chart_repo_identity(url)}/__MASKED_CHART__ __VER__ {dest}"


def mask_section(text: str, validate: bool) -> str:
    matches = list(fn_re.finditer(text))
    if len(matches) != 1:
        raise SystemExit(
            f"FAIL: expected exactly 1 {fn_name}() heredoc in deps.sh, found {len(matches)}"
        )
    m = matches[0]
    masked_lines = [mask_entry(line, validate) for line in m.group(2).splitlines()]
    return text[: m.start(2)] + "\n".join(masked_lines) + text[m.end(2) :]


with open(baseline_path, "r", encoding="utf-8") as f:
    base_txt = f.read()
with open(candidate_path, "r", encoding="utf-8") as f:
    cand_txt = f.read()

if mask_section(base_txt, False) != mask_section(cand_txt, True):
    print(
        f"FAIL: hack/deps.sh modified outside allowed {fn_name}() version/URL fields",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# assert_api_only_generated <baseline_api_dir> <candidate_api_dir>
# Fails closed if any file under api/ was added, deleted, or modified whose basename
# is not a generated codegen file.
assert_api_only_generated() {
  local baseline_api_dir="${1:?baseline api dir required}"
  local candidate_api_dir="${2:?candidate api dir required}"

  if [[ ! -d "${baseline_api_dir}" || ! -d "${candidate_api_dir}" ]]; then
    printf 'FAIL: missing baseline (%s) or candidate (%s) api directory\n' \
      "${baseline_api_dir}" "${candidate_api_dir}" >&2
    return 1
  fi

  python3 - "${baseline_api_dir}" "${candidate_api_dir}" <<'PY'
import os
import re
import sys

base_dir, cand_dir = sys.argv[1:3]
gen_re = re.compile(
    r'^(zz_generated.*\.go|zz_generated\.openapi\.violation_exceptions|generated\.pb\.go|generated\.protomessage\.pb\.go|generated\.proto)$'
)


def collect_files(root: str) -> dict[str, bytes]:
    files: dict[str, bytes] = {}
    for cur, dirs, names in os.walk(root, followlinks=False):
        dirs.sort()
        for d in dirs:
            dp = os.path.join(cur, d)
            if os.path.islink(dp):
                raise SystemExit(f"FAIL: symlink not allowed in api dir: {dp}")
        for name in sorted(names):
            fp = os.path.join(cur, name)
            if os.path.islink(fp):
                raise SystemExit(f"FAIL: symlink not allowed in api dir: {fp}")
            if os.path.isfile(fp):
                rel = os.path.relpath(fp, root)
                with open(fp, "rb") as f:
                    files[rel] = f.read()
    return files


base_files = collect_files(base_dir)
cand_files = collect_files(cand_dir)

all_rels = sorted(set(base_files.keys()) | set(cand_files.keys()))
for rel in all_rels:
    if base_files.get(rel) != cand_files.get(rel):
        bn = os.path.basename(rel)
        if not gen_re.fullmatch(bn):
            print(
                f"FAIL: non-generated file changed under api/: {rel}",
                file=sys.stderr,
            )
            sys.exit(1)
PY
}

# autosync::assert_image_published <manifests_json> <image_ref>
# Fails closed unless <manifests_json> confirms <image_ref> is published with non-empty
# sha256 digests for both linux/amd64 and linux/arm64.
autosync::assert_image_published() {
  local manifests_json="${1:?manifests json path required}"
  local image_ref="${2:?image ref required}"

  if [[ ! -f "${manifests_json}" ]]; then
    printf 'FAIL: missing manifests report: %s\n' "${manifests_json}" >&2
    return 1
  fi

  python3 - "${manifests_json}" "${image_ref}" <<'PY'
import json
import re
import sys

manifests_path, image_ref = sys.argv[1:3]
DIGEST_RE = re.compile(r"^sha256:[0-9a-f]{64}$")

try:
    with open(manifests_path, "r", encoding="utf-8") as f:
        data = json.load(f)
except Exception as exc:
    print(f"FAIL: unreadable manifests JSON {manifests_path}: {exc}", file=sys.stderr)
    sys.exit(1)

images = data.get("images") if isinstance(data, dict) else None
if not isinstance(images, dict):
    print(f"FAIL: malformed manifests JSON {manifests_path}: missing 'images' object", file=sys.stderr)
    sys.exit(1)

candidates = [image_ref]
if image_ref.startswith("docker.io/"):
    candidates.append(image_ref[len("docker.io/"):])
else:
    candidates.append("docker.io/" + image_ref)

entry = None
for key in candidates:
    if key in images:
        entry = images[key]
        break

if entry is None:
    for val in images.values():
        if isinstance(val, dict) and (
            val.get("image") in candidates or val.get("normalized") in candidates
        ):
            entry = val
            break

if not isinstance(entry, dict):
    print(f"FAIL: image not found in manifests report: {image_ref}", file=sys.stderr)
    sys.exit(1)

if entry.get("published") is not True:
    print(f"FAIL: image is not published: {image_ref}", file=sys.stderr)
    sys.exit(1)

digests = {}
for field in ("amd64Digest", "arm64Digest"):
    digest = entry.get(field)
    if not isinstance(digest, str) or not DIGEST_RE.fullmatch(digest):
        print(f"FAIL: image {image_ref} missing valid {field}", file=sys.stderr)
        sys.exit(1)
    digests[field] = digest

if digests["amd64Digest"] == digests["arm64Digest"]:
    print(
        f"FAIL: image {image_ref} has identical amd64Digest and arm64Digest (single-arch manifest rejected)",
        file=sys.stderr,
    )
    sys.exit(1)
PY
}

# autosync::parse_dockerfile_arg_commit <dockerfile_path> <arg_name>
# Extracts a single ARG <arg_name>=... assignment (stripping optional surrounding
# single or double quotes) and verifies it is a 40-hex lowercase commit SHA.
autosync::parse_dockerfile_arg_commit() {
  local dockerfile_path="${1:?dockerfile path required}"
  local arg_name="${2:?arg name required}"

  python3 - "${dockerfile_path}" "${arg_name}" <<'PY'
import re
import sys

path, arg_name = sys.argv[1:3]
pat = re.compile(
    rf'^ARG {re.escape(arg_name)}=(?:"([^"]*)"|\'([^\']*)\'|([^\s#"\']+))(?:\s+#.*)?\s*$'
)
matches = []
with open(path, "r", encoding="utf-8") as f:
    for raw in f:
        line = raw.rstrip("\r\n")
        m = pat.match(line)
        if m:
            val = m.group(1) if m.group(1) is not None else (m.group(2) if m.group(2) is not None else m.group(3))
            matches.append(val.strip())
        elif re.match(rf'^ARG {re.escape(arg_name)}=', line):
            print(
                f"FAIL: malformed quoted ARG {arg_name} in {path}: {line!r}",
                file=sys.stderr,
            )
            sys.exit(1)

if len(matches) != 1:
    print(
        f"FAIL: expected exactly 1 ARG {arg_name} in {path}, found {len(matches)}",
        file=sys.stderr,
    )
    sys.exit(1)

val = matches[0]
if not re.fullmatch(r"[0-9a-f]{40}", val):
    print(
        f"FAIL: ARG {arg_name} must be a 40-hex lowercase SHA in {path}, got '{val}'",
        file=sys.stderr,
    )
    sys.exit(1)
print(val)
PY
}

# autosync::count_priors_profiles <priors_yaml_path>
# Counts aggregated and disaggregated profiles in priors.yaml structurally and prints
# "<aggregated> <disaggregated>", failing closed on unknown or missing profile modes.
autosync::count_priors_profiles() {
  local priors_path="${1:?priors.yaml path required}"

  python3 - "${priors_path}" <<'PY'
import re
import sys

try:
    import yaml as yaml_mod
except ImportError:
    yaml_mod = None

priors_path = sys.argv[1]
with open(priors_path, "r", encoding="utf-8") as f:
    raw = f.read()

agg = 0
disagg = 0

if yaml_mod is not None:
    doc = yaml_mod.safe_load(raw)
    if not isinstance(doc, dict) or not isinstance(doc.get("profiles"), list):
        raise SystemExit(f"FAIL: {priors_path} missing top-level 'profiles' list")
    for idx, entry in enumerate(doc["profiles"]):
        if not isinstance(entry, dict):
            raise SystemExit(f"FAIL: non-mapping profile entry #{idx} in {priors_path}")
        mode = entry.get("mode")
        if mode == "aggregated":
            agg += 1
        elif mode == "disaggregated":
            disagg += 1
        else:
            raise SystemExit(f"FAIL: unexpected profile mode in {priors_path}: {mode!r}")
else:
    def count_modes_fallback(text: str) -> tuple[int, int]:
        agg_c = 0
        disagg_c = 0
        in_profiles = False
        item_indent = None
        in_entry = False
        entry_has_mode = False
        saw_profiles = False
        mode_re = re.compile(r'^mode:\s*(?:"([^"]*)"|\'([^\']*)\'|([^\s#]+))(?:\s+#.*)?\s*$')

        def check_mode(m_obj):
            nonlocal agg_c, disagg_c, entry_has_mode
            val = (
                m_obj.group(1)
                if m_obj.group(1) is not None
                else (m_obj.group(2) if m_obj.group(2) is not None else m_obj.group(3))
            ).strip()
            if val == "aggregated":
                agg_c += 1
            elif val == "disaggregated":
                disagg_c += 1
            else:
                raise SystemExit(f"FAIL: unexpected profile mode in {priors_path}: {val!r}")
            entry_has_mode = True

        for line in text.splitlines():
            stripped = line.strip()
            if not stripped or stripped.startswith("#"):
                continue
            indent = len(line) - len(line.lstrip(" "))
            if not in_profiles:
                if indent == 0:
                    m_prof = re.match(r"^profiles:\s*(\[\s*\])?\s*(?:#.*)?$", stripped)
                    if m_prof:
                        saw_profiles = True
                        if m_prof.group(1) is not None:
                            return 0, 0
                        in_profiles = True
                continue
            if indent == 0 and not stripped.startswith("-"):
                break
            if stripped.startswith("- ") or stripped == "-":
                if item_indent is None:
                    item_indent = indent
                if indent == item_indent:
                    if in_entry and not entry_has_mode:
                        raise SystemExit(f"FAIL: profile entry missing mode key in {priors_path}")
                    in_entry = True
                    entry_has_mode = False
                    after_dash = stripped[1:].strip()
                    m = mode_re.match(after_dash)
                    if m:
                        check_mode(m)
                continue
            if item_indent is not None and indent == item_indent + 2:
                m = mode_re.match(stripped)
                if m:
                    check_mode(m)

        if not saw_profiles:
            raise SystemExit(f"FAIL: {priors_path} missing top-level 'profiles' list")
        if in_entry and not entry_has_mode:
            raise SystemExit(f"FAIL: profile entry missing mode key in {priors_path}")
        return agg_c, disagg_c

    agg, disagg = count_modes_fallback(raw)

print(f"{agg} {disagg}")
PY
}

# autosync::go_test <go_test_args...>
# Runs go test in a subshell with -buildvcs=false and, on Linux, the same CGO and lazy
# symbol-binding linker flags as hack/test.sh and hack/lib/build.sh for vendor CGO bindings.
autosync::go_test() (
  local target_os
  target_os="$(go env GOOS)"
  if [[ "${target_os}" == "linux" ]]; then
    export CGO_ENABLED=1
    export CGO_CFLAGS="${CGO_CFLAGS:--O2 -g} -std=gnu17"
    export CGO_LDFLAGS="${CGO_LDFLAGS:--O2 -g} -Wl,-z,lazy"
    go test -buildvcs=false -ldflags="-extldflags=-Wl,-z,lazy" "$@"
  else
    go test -buildvcs=false "$@"
  fi
)
