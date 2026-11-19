#!/usr/bin/env bash
# hack/check/autosync-selftest.sh - self-tests and schema/workflow gates for .auto-sync.
#
# Usage:
#   bash hack/check/autosync-selftest.sh [all|fast|common|priors|slicing|kv-transfer|router|charts|k8s-deps|workflow] [repo-root]
#   bash hack/check/autosync-selftest.sh <repo-root>
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage:
  bash hack/check/autosync-selftest.sh [all|fast|common|priors|slicing|kv-transfer|router|charts|k8s-deps|workflow] [repo-root]
  bash hack/check/autosync-selftest.sh <repo-root>
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"

MODE="all"
ROOT_DIR="${DEFAULT_ROOT}"

if [[ $# -ge 1 ]]; then
  if [[ -d "${1}" ]]; then
    MODE="all"
    ROOT_DIR="$(cd "${1}" && pwd -P)"
  else
    case "${1}" in
      all|fast|common|priors|slicing|kv-transfer|router|charts|k8s-deps|workflow)
        MODE="${1}"
        if [[ $# -ge 2 ]]; then
          if [[ ! -d "${2}" ]]; then
            echo "FAIL: repo-root does not exist: ${2}" >&2
            exit 2
          fi
          ROOT_DIR="$(cd "${2}" && pwd -P)"
        fi
        ;;
      -h|--help)
        usage
        exit 0
        ;;
      *)
        echo "FAIL: unknown mode or missing directory: ${1}" >&2
        usage
        exit 2
        ;;
    esac
  fi
fi

FAILS=0
PASSES=0
_CLEANUP_DIRS=()

cleanup_on_exit() {
  local d
  for d in "${_CLEANUP_DIRS[@]+"${_CLEANUP_DIRS[@]}"}"; do
    if [[ -n "${d}" && -e "${d}" ]]; then
      chmod -R u+w "${d}" 2>/dev/null || true
      rm -rf "${d}"
    fi
  done
}
trap cleanup_on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

resolve_loopwright_dist_js() {
  if [[ -n "${LOOPWRIGHT_DIST_JS+set}" ]]; then
    if [[ -n "${LOOPWRIGHT_DIST_JS}" && -r "${LOOPWRIGHT_DIST_JS}" ]]; then
      printf '%s\n' "${LOOPWRIGHT_DIST_JS}"
      return 0
    fi
    return 1
  fi

  local main_repo_root=""
  if command -v git >/dev/null 2>&1; then
    main_repo_root="$(git -C "${ROOT_DIR}" worktree list --porcelain 2>/dev/null | awk '/^worktree /{print substr($0, 10); exit}')"
  fi

  local candidates=(
    "${ROOT_DIR}/../loopwright/packages/github-app/dist/index.js"
  )
  local glob_cand
  for glob_cand in "${ROOT_DIR}/../../github.com"/*/loopwright/packages/github-app/dist/index.js; do
    [[ -e "${glob_cand}" ]] && candidates+=("${glob_cand}")
  done
  if [[ -n "${main_repo_root}" ]]; then
    candidates+=("${main_repo_root}/../loopwright/packages/github-app/dist/index.js")
    for glob_cand in "${main_repo_root}/../../github.com"/*/loopwright/packages/github-app/dist/index.js; do
      [[ -e "${glob_cand}" ]] && candidates+=("${glob_cand}")
    done
  fi

  local cand
  for cand in "${candidates[@]}"; do
    if [[ -r "${cand}" ]]; then
      printf '%s\n' "${cand}"
      return 0
    fi
  done
  return 1
}

pass() {
  printf 'PASS  %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
}

assert_ok() {
  local label="$1"
  shift
  if "$@"; then
    pass "${label}"
  else
    fail "${label}"
  fi
}

assert_fails() {
  local label="$1"
  shift
  if "$@" >/dev/null 2>&1; then
    fail "${label} (expected non-zero exit)"
  else
    pass "${label}"
  fi
}

resolve_shellcheck() {
  if [[ -n "${SHELLCHECK_BIN:-}" ]]; then
    if [[ ! -x "${SHELLCHECK_BIN}" ]]; then
      echo "COULD-NOT-RUN: SHELLCHECK_BIN (${SHELLCHECK_BIN}) is not executable." >&2
      return 2
    fi
    printf '%s\n' "${SHELLCHECK_BIN}"
    return 0
  fi

  local lib_dir="${ROOT_DIR}/hack/lib"
  # shellcheck source=../lib/util.sh
  # shellcheck disable=SC1091
  source "${lib_dir}/util.sh"
  # shellcheck source=../lib/log.sh
  # shellcheck disable=SC1091
  source "${lib_dir}/log.sh"
  # shellcheck source=../lib/style.sh
  # shellcheck disable=SC1091
  source "${lib_dir}/style.sh"
  if ! gpustack::lint::shellcheck::validate >/dev/null 2>&1; then
    echo "COULD-NOT-RUN: pinned shellcheck could not be resolved or installed." >&2
    return 2
  fi
  gpustack::lint::shellcheck::bin
}

run_static_checks() {
  echo "== shell and python static checks =="

  local shellcheck_bin
  if ! shellcheck_bin="$(resolve_shellcheck)"; then
    exit 2
  fi

  local sh_files=()
  sh_files+=("${ROOT_DIR}/hack/check/autosync-selftest.sh")
  if [[ -d "${ROOT_DIR}/.auto-sync" ]]; then
    local f
    while IFS= read -r f; do
      [[ -n "${f}" ]] && sh_files+=("${f}")
    done < <(find "${ROOT_DIR}/.auto-sync" -type f -name '*.sh' | sort)
  fi

  local file
  for file in "${sh_files[@]}"; do
    local rel="${file#"${ROOT_DIR}/"}"
    if [[ ! -f "${file}" ]]; then
      fail "missing shell script: ${rel}"
      continue
    fi
    if bash -n "${file}"; then
      pass "bash -n ${rel}"
    else
      fail "bash -n ${rel}"
    fi
    if "${shellcheck_bin}" -x --severity=info "${file}"; then
      pass "shellcheck ${rel}"
    else
      fail "shellcheck ${rel}"
    fi
  done

  if [[ -d "${ROOT_DIR}/.auto-sync" ]]; then
    local py_files=()
    local pf
    while IFS= read -r pf; do
      [[ -n "${pf}" ]] && py_files+=("${pf}")
    done < <(find "${ROOT_DIR}/.auto-sync" -type f -name '*.py' | sort)
    for pf in "${py_files[@]}"; do
      local py_rel="${pf#"${ROOT_DIR}/"}"
      if python3 - "${pf}" <<'PY'
import os
import py_compile
import sys
import tempfile

src = sys.argv[1]
fd, cfile = tempfile.mkstemp(suffix=".pyc")
os.close(fd)
try:
    py_compile.compile(src, cfile=cfile, doraise=True)
finally:
    if os.path.exists(cfile):
        os.unlink(cfile)
PY
      then
        pass "py_compile ${py_rel}"
      else
        fail "py_compile ${py_rel}"
      fi
    done
  fi
}

run_common_tests() {
  echo
  echo "== common library and offline guard self-tests =="

  local checks_lib="${ROOT_DIR}/.auto-sync/checks/lib.sh"
  local preflight_lib="${ROOT_DIR}/.auto-sync/scripts/lib-preflight.sh"
  local models_script="${ROOT_DIR}/.auto-sync/scripts/write_models_json.py"

  local req
  for req in "${checks_lib}" "${preflight_lib}" "${models_script}"; do
    if [[ ! -f "${req}" ]]; then
      fail "missing required deliverable: ${req#"${ROOT_DIR}/"}"
      return 1
    fi
  done

  if [[ -x "${models_script}" ]]; then
    pass "write_models_json.py is executable"
  else
    fail "write_models_json.py must be executable (0755)"
  fi

  local scratch
  scratch="$(cd "$(mktemp -d)" && pwd -P)"
  _CLEANUP_DIRS+=("${scratch}")

  # shellcheck source=../../.auto-sync/checks/lib.sh
  # shellcheck disable=SC1091
  source "${checks_lib}"
  # shellcheck source=../../.auto-sync/scripts/lib-preflight.sh
  # shellcheck disable=SC1091
  source "${preflight_lib}"

  # 1. Sanitized bootstrap_env under minimal env -i PATH.
  local stage_boot="${scratch}/stage-boot"
  local cand_boot="${scratch}/cand-boot"
  mkdir -p "${stage_boot}/.sbin" "${stage_boot}/go-cache" "${stage_boot}/go-mod" "${stage_boot}/deps-cache" "${cand_boot}"
  printf '#!/bin/sh\necho fake-tool\n' > "${stage_boot}/.sbin/fake-tool"
  chmod +x "${stage_boot}/.sbin/fake-tool"
  ln -s "${stage_boot}/.sbin/fake-tool" "${stage_boot}/.sbin/linked-tool"

  # Inner bash -c intentionally expands its own variables after sourcing lib.sh.
  # shellcheck disable=SC2016
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    bash -c '
      set -euo pipefail
      stage="$1"
      cand="$2"
      lib="$3"
      # shellcheck disable=SC1090
      source "${lib}"
      autosync::bootstrap_env "${stage}" "${cand}"
      [[ "${GOCACHE}" == "${stage}/go-cache" ]]
      [[ "${GOMODCACHE}" == "${stage}/go-mod" ]]
      [[ "${GPUSTACK_DEPS_CACHE_DIR}" == "${stage}/deps-cache" ]]
      [[ "${GPUSTACK_DEPS_OFFLINE}" == "true" ]]
      [[ "${GOPROXY}" == "off" ]]
      [[ "${GOTOOLCHAIN}" == "local" ]]
      [[ -f "${cand}/.sbin/fake-tool" && ! -L "${cand}/.sbin" && ! -L "${cand}/.sbin/linked-tool" ]]
      command -v fake-tool >/dev/null
      autosync::ensure_ephemeral_git "${cand}"
      [[ -d "${cand}/.git" ]]
      [[ -z "$(git -C "${cand}" ls-files .sbin)" ]]
      [[ -z "$(git -C "${cand}" status --porcelain)" ]]
      autosync::cleanup_ephemeral_git "${cand}"
      [[ ! -e "${cand}/.git" ]]
      [[ ! -e "${cand}/.sbin" ]]
    ' _ "${stage_boot}" "${cand_boot}" "${checks_lib}"; then
    pass "bootstrap_env sanitized PATH, offline env, .sbin materialization, and cleanup"
  else
    fail "bootstrap_env sanitized PATH, offline env, .sbin materialization, and cleanup"
  fi

  # 1b. toolchain.env precedence over /usr/local/bin:/usr/bin:/bin and read-only sandbox fallback.
  local shadow_bin="${scratch}/shadow-bin"
  local saved_go_dir="${scratch}/saved-go/bin"
  local saved_goroot="${scratch}/saved-go/root"
  local saved_hugo_dir="${scratch}/saved-hugo/bin"
  local saved_node_dir="${scratch}/saved-node/bin"
  local saved_uv_dir="${scratch}/saved-uv/bin"
  local saved_crane_dir="${scratch}/saved-crane/bin"
  local saved_docker_dir="${scratch}/saved-docker/bin"
  local stage_tc="${scratch}/stage-tc"
  local cand_tc="${scratch}/cand-tc"
  mkdir -p "${shadow_bin}" "${saved_go_dir}" "${saved_goroot}" "${saved_hugo_dir}" "${saved_node_dir}" "${saved_uv_dir}" "${saved_crane_dir}" "${saved_docker_dir}" "${stage_tc}/baseline" "${cand_tc}"
  printf '#!/bin/sh\necho shadow-go\n' > "${shadow_bin}/go"
  printf '#!/bin/sh\necho saved-go\n' > "${saved_go_dir}/go"
  printf '#!/bin/sh\necho saved-hugo\n' > "${saved_hugo_dir}/hugo"
  printf '#!/bin/sh\necho saved-node\n' > "${saved_node_dir}/node"
  printf '#!/bin/sh\necho saved-uv\n' > "${saved_uv_dir}/uv"
  printf '#!/bin/sh\necho saved-crane\n' > "${saved_crane_dir}/crane"
  printf '#!/bin/sh\necho saved-docker\n' > "${saved_docker_dir}/docker"
  chmod +x "${shadow_bin}/go" "${saved_go_dir}/go" "${saved_hugo_dir}/hugo" "${saved_node_dir}/node" "${saved_uv_dir}/uv" "${saved_crane_dir}/crane" "${saved_docker_dir}/docker"
  cat > "${stage_tc}/toolchain.env" <<EOF
AUTOSYNC_SAVED_GO_BIN_DIR=${saved_go_dir}
AUTOSYNC_SAVED_GOROOT=${saved_goroot}
AUTOSYNC_SAVED_HUGO_BIN_DIR=${saved_hugo_dir}
AUTOSYNC_SAVED_NODE_BIN_DIR=${saved_node_dir}
AUTOSYNC_SAVED_UV_BIN_DIR=${saved_uv_dir}
AUTOSYNC_SAVED_CRANE_BIN_DIR=${saved_crane_dir}
AUTOSYNC_SAVED_DOCKER_BIN_DIR=${saved_docker_dir}
EOF
  mkdir -p "${stage_tc}/.sbin/protoc/bin" "${stage_tc}/go-cache" "${stage_tc}/go-mod" "${stage_tc}/deps-cache" "${stage_tc}/uv-cache"
  printf 'cached\n' > "${stage_tc}/uv-cache/sentinel"
  printf '#!/bin/sh\necho protoc-ok\n' > "${stage_tc}/.sbin/protoc/bin/protoc"
  chmod +x "${stage_tc}/.sbin/protoc/bin/protoc"

  # shellcheck disable=SC2016
  if env -i PATH="${shadow_bin}:/usr/local/bin:/usr/bin:/bin" \
    bash -c '
      set -euo pipefail
      source "$3"
      autosync::bootstrap_env "$1" "$2"
      [[ "$(go)" == "saved-go" ]]
      [[ "$(hugo)" == "saved-hugo" ]]
      [[ "$(node)" == "saved-node" ]]
      [[ "$(uv)" == "saved-uv" ]]
      [[ "$(crane)" == "saved-crane" ]]
      [[ "$(docker)" == "saved-docker" ]]
      [[ "$(protoc)" == "protoc-ok" ]]
      [[ "${GOROOT:-}" == "$4" ]]
      [[ "${AUTOSYNC_IN_VALIDATOR:-}" == "true" ]]
      [[ "${UV_CACHE_DIR:-}" == "$1/uv-cache" ]]
      [[ "${UV_OFFLINE:-}" == "1" ]]
    ' _ "${stage_tc}" "${cand_tc}" "${checks_lib}" "${saved_goroot}"; then
    pass "bootstrap_env toolchain.env prepends saved toolchain/uv/crane/docker/protoc and sets offline uv cache"
  else
    fail "bootstrap_env toolchain.env prepends saved toolchain/uv/crane/docker/protoc and sets offline uv cache"
  fi

  local ro_stage="${scratch}/ro-stage"
  local ro_cand="${scratch}/ro-cand"
  local ro_tmp="${scratch}/ro-tmp"
  mkdir -p "${ro_stage}/.sbin" "${ro_stage}/go-cache" "${ro_stage}/go-mod" "${ro_stage}/deps-cache" "${ro_cand}" "${ro_tmp}"
  printf '#!/bin/sh\necho ro-tool\n' > "${ro_stage}/.sbin/ro-tool"
  chmod +x "${ro_stage}/.sbin/ro-tool"
  chmod 0555 "${ro_stage}/go-cache" "${ro_cand}"
  # shellcheck disable=SC2016
  if env -i PATH=/usr/local/bin:/usr/bin:/bin TMPDIR="${ro_tmp}" \
    bash -c '
      set -euo pipefail
      source "$3"
      autosync::bootstrap_env "$1" "$2"
      [[ "${GOCACHE}" == "$4/autosync-go-cache" ]]
      [[ -d "${GOCACHE}" && -w "${GOCACHE}" ]]
      [[ ! -e "$2/.sbin" ]]
      [[ "$(command -v ro-tool)" == "$1/.sbin/ro-tool" ]]
    ' _ "${ro_stage}" "${ro_cand}" "${checks_lib}" "${ro_tmp}"; then
    pass "bootstrap_env falls back safely when go-cache and candidate_dir are read-only"
  else
    fail "bootstrap_env falls back safely when go-cache and candidate_dir are read-only"
  fi
  chmod 0755 "${ro_stage}/go-cache" "${ro_cand}"

  local dangling_dir="${scratch}/dangling-symlinks"
  mkdir -p "${dangling_dir}"
  ln -s "${scratch}/missing-sbin-target" "${dangling_dir}/.sbin"
  ln -s "${scratch}/missing-git-target" "${dangling_dir}/.git"
  if (
    autosync::bootstrap_env "${stage_boot}" "${dangling_dir}" &&
    autosync::ensure_ephemeral_git "${dangling_dir}" &&
    [[ -L "${dangling_dir}/.sbin" && -L "${dangling_dir}/.git" ]]
  ); then
    pass "bootstrap_env and ensure_ephemeral_git ignore dangling symlinks safely"
  else
    fail "bootstrap_env and ensure_ephemeral_git ignore dangling symlinks safely"
  fi

  # 2. Ephemeral git initialization and cleanup safety.
  local gitless_dir="${scratch}/gitless"
  mkdir -p "${gitless_dir}"
  printf 'hello\n' > "${gitless_dir}/file.txt"
  local sc_ephem
  sc_ephem="$(resolve_shellcheck)"
  local pre_agents_rc=0
  SHELLCHECK_BIN="${sc_ephem}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${gitless_dir}" >/dev/null 2>&1 || pre_agents_rc=$?
  autosync::ensure_ephemeral_git "${gitless_dir}"
  local post_agents_rc=0
  SHELLCHECK_BIN="${sc_ephem}" bash "${ROOT_DIR}/hack/check/agents-shell.sh" "${gitless_dir}" >/dev/null 2>&1 || post_agents_rc=$?
  if (( pre_agents_rc == 2 )) && \
     (( post_agents_rc == 0 )) && \
     [[ -d "${gitless_dir}/.git" ]] && \
     [[ -z "$(git -C "${gitless_dir}" status --porcelain)" ]] && \
     git -C "${gitless_dir}" diff --exit-code >/dev/null && \
     git -C "${gitless_dir}" rev-parse --verify refs/heads/main >/dev/null && \
     git -C "${gitless_dir}" rev-parse --verify refs/remotes/origin/main >/dev/null && \
     [[ "$(git -C "${gitless_dir}" rev-parse refs/heads/main)" == "$(git -C "${gitless_dir}" rev-parse refs/remotes/origin/main)" ]]; then
    pass "ensure_ephemeral_git initializes clean main + origin/main git index in gitless dir and unblocks agents-shell.sh"
  else
    fail "ensure_ephemeral_git initializes clean main + origin/main git index in gitless dir and unblocks agents-shell.sh (pre_rc=${pre_agents_rc}, post_rc=${post_agents_rc})"
  fi

  # 2b. run_in_module_path_copy helper (F-03).
  local mod_cand="${scratch}/mod-cand"
  local mod_tmp="${scratch}/mod-tmp"
  mkdir -p "${mod_cand}/.git" "${mod_cand}/.sbin" "${mod_cand}/sub" "${mod_tmp}"
  printf 'keep-git\n' > "${mod_cand}/.git/sentinel"
  printf 'keep-sbin\n' > "${mod_cand}/.sbin/sentinel"
  printf 'v1\n' > "${mod_cand}/existing.txt"
  printf 'bye\n' > "${mod_cand}/sub/obsolete.txt"
  # Inner bash -c intentionally expands its own variables after sourcing lib.sh.
  # shellcheck disable=SC2016
  if TMPDIR="${mod_tmp}" autosync::run_in_module_path_copy "${mod_cand}" bash -c '
      set -euo pipefail
      phys="$(pwd -P)"
      [[ "${phys}" == */gpustack.ai/gpustack ]]
      printf "v2\n" > existing.txt
      mkdir -p gen
      printf "gen\n" > gen/new.txt
      rm -f sub/obsolete.txt
    ' && \
    [[ "$(cat "${mod_cand}/existing.txt")" == "v2" ]] && \
    [[ "$(cat "${mod_cand}/gen/new.txt")" == "gen" ]] && \
    [[ ! -e "${mod_cand}/sub/obsolete.txt" ]] && \
    [[ "$(cat "${mod_cand}/.git/sentinel")" == "keep-git" ]] && \
    [[ "$(cat "${mod_cand}/.sbin/sentinel")" == "keep-sbin" ]] && \
    [[ -z "$(ls -A "${mod_tmp}")" ]]; then
    pass "run_in_module_path_copy executes inside physical gpustack.ai/gpustack and syncs files back"
  else
    fail "run_in_module_path_copy executes inside physical gpustack.ai/gpustack and syncs files back"
  fi

  local fail_rc=0
  TMPDIR="${mod_tmp}" autosync::run_in_module_path_copy "${mod_cand}" bash -c 'exit 7' || fail_rc=$?
  if (( fail_rc == 7 )) && [[ -z "$(ls -A "${mod_tmp}")" ]]; then
    pass "run_in_module_path_copy preserves non-zero exit code and cleans scratch dir"
  else
    fail "run_in_module_path_copy preserves non-zero exit code and cleans scratch dir"
  fi

  local direct_mod="${scratch}/direct/gpustack.ai/gpustack"
  mkdir -p "${direct_mod}"
  # shellcheck disable=SC2016
  if autosync::run_in_module_path_copy "${direct_mod}" bash -c '[[ "$(pwd -P)" == "$1" ]]' _ "${direct_mod}"; then
    pass "run_in_module_path_copy runs directly when already under /gpustack.ai/gpustack"
  else
    fail "run_in_module_path_copy runs directly when already under /gpustack.ai/gpustack"
  fi
  autosync::cleanup_ephemeral_git "${gitless_dir}"
  if [[ ! -e "${gitless_dir}/.git" ]]; then
    pass "cleanup_ephemeral_git removes ephemeral .git"
  else
    fail "cleanup_ephemeral_git removes ephemeral .git"
  fi

  local real_git_dir="${scratch}/real-git"
  mkdir -p "${real_git_dir}"
  git -C "${real_git_dir}" init -q
  git -C "${real_git_dir}" config user.name "real"
  git -C "${real_git_dir}" config user.email "real@example.com"
  printf 'real\n' > "${real_git_dir}/a.txt"
  git -C "${real_git_dir}" add a.txt
  git -C "${real_git_dir}" commit -q -m "init"
  autosync::ensure_ephemeral_git "${real_git_dir}"
  autosync::cleanup_ephemeral_git "${real_git_dir}"
  if [[ -d "${real_git_dir}/.git" ]]; then
    pass "cleanup_ephemeral_git never deletes pre-existing .git"
  else
    fail "cleanup_ephemeral_git never deletes pre-existing .git"
  fi

  local worktree_git_dir="${scratch}/worktree-git"
  mkdir -p "${worktree_git_dir}"
  printf 'gitdir: /tmp/nonexistent-worktree-git\n' > "${worktree_git_dir}/.git"
  autosync::ensure_ephemeral_git "${worktree_git_dir}"
  autosync::cleanup_ephemeral_git "${worktree_git_dir}"
  if [[ -f "${worktree_git_dir}/.git" ]]; then
    pass "cleanup_ephemeral_git never deletes pre-existing .git worktree file"
  else
    fail "cleanup_ephemeral_git never deletes pre-existing .git worktree file"
  fi

  # 3. Shared-file diff guards (positive & negative).
  local base_dir="${scratch}/baseline"
  local cand_dir="${scratch}/candidate"
  mkdir -p "${base_dir}" "${cand_dir}"

  # 3a. Dockerfile guard.
  local df_base="${base_dir}/Dockerfile"
  local df_cand="${cand_dir}/Dockerfile"
  cp "${ROOT_DIR}/pack/gpustack-operator/Dockerfile" "${df_base}"
  cp "${df_base}" "${df_cand}"
  assert_ok "assert_dockerfile_only_args unchanged passes" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" 'LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT'

  sed 's/^ARG LIB_HAMI_CORE_COMMIT="[^"]*"/ARG LIB_HAMI_CORE_COMMIT="1111111111111111111111111111111111111111"/' "${df_base}" > "${df_cand}"
  assert_ok "assert_dockerfile_only_args LIB_HAMI_CORE_COMMIT passes slicing regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" 'LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT'
  assert_fails "assert_dockerfile_only_args LIB_HAMI_CORE_COMMIT fails k8s-deps regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  sed 's/^ARG HELM_VERSION="[^"]*"/ARG HELM_VERSION="3.99.0"/' "${df_base}" > "${df_cand}"
  assert_ok "assert_dockerfile_only_args HELM_VERSION passes k8s-deps regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'
  assert_fails "assert_dockerfile_only_args HELM_VERSION fails slicing regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" 'LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT'

  sed 's/^ARG GO_VERSION="[^"]*"/ARG GO_VERSION="1.99.0"/' "${df_base}" > "${df_cand}"
  assert_fails "assert_dockerfile_only_args GO_VERSION fails slicing regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" 'LIB_(UBS_VIRT_ENPU|HAMI_CORE)_COMMIT'
  assert_fails "assert_dockerfile_only_args GO_VERSION fails k8s-deps regex" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  assert_fails "assert_dockerfile_only_args missing baseline fails" \
    assert_dockerfile_only_args "${scratch}/missing-dockerfile" "${df_cand}" 'LIB_HAMI_CORE_COMMIT'

  sed 's/^ARG HELM_VERSION="[^"]*"/ARG HELM_VERSION="3.21.4;id"/' "${df_base}" > "${df_cand}"
  assert_fails "assert_dockerfile_only_args rejects metacharacter injection in ARG value" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  sed 's/^ARG HELM_VERSION="[^"]*"/ARG HELM_VERSION="3.21.4" GO_VERSION="1.99.0"/' "${df_base}" > "${df_cand}"
  assert_fails "assert_dockerfile_only_args rejects multi-ARG on single line" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  grep -v '^ARG HELM_VERSION=' "${df_base}" > "${df_cand}"
  assert_fails "assert_dockerfile_only_args rejects ARG deletion" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  python3 - "${df_base}" "${df_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace("ARG HELM_VERSION\n", 'ARG HELM_VERSION="3.99.0"\n', 1)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_fails "assert_dockerfile_only_args rejects stage-level ARG override after FROM" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  printf '+++injected\n' >> "${df_cand}"
  assert_fails "assert_dockerfile_only_args rejects +++ content line spoofing diff header" \
    assert_dockerfile_only_args "${df_base}" "${df_cand}" '(SKOPEO|HELM|VALS|KUSTOMIZE|YTT|KUBECTL)_VERSION'

  # 3b. value.go guard (zero-context trap).
  local vg_base="${base_dir}/value.go"
  local vg_cand="${cand_dir}/value.go"
  cp "${ROOT_DIR}/pkg/worker/settings/value.go" "${vg_base}"
  cp "${vg_base}" "${vg_cand}"
  local kv_keys_re='kv-cache-backend-image|model-deployment-modelexpress-server-image'
  local router_keys_re='model-deployment-router-image|model-deployment-router-proxy-image|model-deployment-routing-sidecar-image'

  assert_ok "assert_value_go_only_keys unchanged passes" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${kv_keys_re}"

  sed 's|gpustack/mirrored-mooncake:0.3.13.post1-cpu|gpustack/mirrored-mooncake:0.3.14-cpu|' "${vg_base}" > "${vg_cand}"
  assert_ok "assert_value_go_only_keys KVCacheBackendImage passes kv-transfer regex" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${kv_keys_re}"
  assert_fails "assert_value_go_only_keys KVCacheBackendImage fails router regex" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${router_keys_re}"

  sed 's|gpustack/llm-router:v0.4.0|gpustack/llm-router:v0.5.0|' "${vg_base}" > "${vg_cand}"
  assert_ok "assert_value_go_only_keys ModelDeploymentRouterImage passes router regex" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${router_keys_re}"
  assert_fails "assert_value_go_only_keys ModelDeploymentRouterImage fails kv-transfer regex" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${kv_keys_re}"

  sed 's|// KV cache\.|// KV cache edited.|' "${vg_base}" > "${vg_cand}"
  assert_fails "assert_value_go_only_keys comment edit fails kv-transfer" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${kv_keys_re}"
  assert_fails "assert_value_go_only_keys comment edit fails router" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${router_keys_re}"

  sed 's|"instance-ssh-server-image,"+|"instance-ssh-server-image-edited,"+|' "${vg_base}" > "${vg_cand}"
  assert_fails "assert_value_go_only_keys DefaultTracking edit fails" \
    assert_value_go_only_keys "${vg_base}" "${vg_cand}" "${kv_keys_re}"

  # 3c. settings.md guard.
  local sm_base="${base_dir}/settings.md"
  local sm_cand="${cand_dir}/settings.md"
  cp "${ROOT_DIR}/docs/reference/settings.md" "${sm_base}"
  cp "${sm_base}" "${sm_cand}"
  assert_ok "assert_settings_md_only_keys unchanged passes" \
    assert_settings_md_only_keys "${sm_base}" "${sm_cand}" "${kv_keys_re}"

  python3 - "${sm_base}" "${sm_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(
    "`kv-cache-backend-image`<br/>(`GPUSTACK_KV_CACHE_BACKEND_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/mirrored-mooncake:0.3.13.post1-cpu`",
    "`kv-cache-backend-image`<br/>(`GPUSTACK_KV_CACHE_BACKEND_IMAGE`) | Effect window: Next trigger<br/>Default: `gpustack/mirrored-mooncake:0.3.14-cpu`",
    1,
)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_ok "assert_settings_md_only_keys kv-cache-backend-image Default passes kv-transfer regex" \
    assert_settings_md_only_keys "${sm_base}" "${sm_cand}" "${kv_keys_re}"
  assert_fails "assert_settings_md_only_keys kv-cache-backend-image Default fails router regex" \
    assert_settings_md_only_keys "${sm_base}" "${sm_cand}" "${router_keys_re}"

  python3 - "${sm_base}" "${sm_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(
    "Image every role of a [KV cache backend]",
    "Image every role of a modified [KV cache backend]",
    1,
)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_fails "assert_settings_md_only_keys Usage edit on allowlisted row fails" \
    assert_settings_md_only_keys "${sm_base}" "${sm_cand}" "${kv_keys_re}"

  # 3d. values.yaml modelTransfer.images guard.
  local vy_base="${base_dir}/values.yaml"
  local vy_cand="${cand_dir}/values.yaml"
  cp "${ROOT_DIR}/deploy/gpustack-operator/chart/values.yaml" "${vy_base}"
  cp "${vy_base}" "${vy_cand}"
  assert_ok "assert_values_yaml_only_model_transfer_images unchanged passes" \
    assert_values_yaml_only_model_transfer_images "${vy_base}" "${vy_cand}"

  sed 's|nvidia: gpustack/model-transfer:v0.1.0-cuda12.9|nvidia: gpustack/model-transfer:v0.2.0-cuda12.9|' "${vy_base}" > "${vy_cand}"
  assert_ok "assert_values_yaml_only_model_transfer_images nvidia edit passes" \
    assert_values_yaml_only_model_transfer_images "${vy_base}" "${vy_cand}"

  sed 's|cleanupDrainBudgetSeconds: 210|cleanupDrainBudgetSeconds: 220|' "${vy_base}" > "${vy_cand}"
  assert_fails "assert_values_yaml_only_model_transfer_images unrelated key edit fails" \
    assert_values_yaml_only_model_transfer_images "${vy_base}" "${vy_cand}"

  python3 - "${vy_base}" "${vy_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
# Mutate a subchart or operator image outside modelTransfer.images.
lines = txt.splitlines(keepends=True)
changed = False
for i, line in enumerate(lines):
    if "repository:" in line and "gpustack/" in line:
        lines[i] = line.replace("gpustack/", "gpustack/changed-", 1)
        changed = True
        break
assert changed
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write("".join(lines))
PY
  assert_fails "assert_values_yaml_only_model_transfer_images operator/subchart image edit fails" \
    assert_values_yaml_only_model_transfer_images "${vy_base}" "${vy_cand}"

  # 3d-2. values.yaml chart sections guard (M-1).
  cp "${vy_base}" "${vy_cand}"
  assert_ok "assert_values_yaml_only_chart_sections unchanged passes" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  python3 - "${vy_base}" "${vy_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(
    "# -- [Kueue](https://kueue.sigs.k8s.io) subchart (chart 0.18.11).",
    "# -- [Kueue](https://kueue.sigs.k8s.io) subchart (chart 0.18.12).",
    1,
)
out = out.replace(
    "      repository: docker.io/gpustack/mirrored-csi-node-driver-registrar\n      tag: v2.17.0",
    "      repository: docker.io/gpustack/mirrored-csi-node-driver-registrar-next\n      tag: v2.18.0",
    1,
)
out = out.replace(
    "  repository: docker.io/gpustack/mirrored-nvidia-topograph",
    "  repository: docker.io/gpustack/mirrored-nvidia-topograph-next",
    1,
)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_ok "assert_values_yaml_only_chart_sections owned subcharts and modelManager.registrar.image edits pass" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  sed 's|nvidia: gpustack/model-transfer:v0.1.0-cuda12.9|nvidia: gpustack/model-transfer:v0.2.0-cuda12.9|' "${vy_base}" > "${vy_cand}"
  assert_fails "assert_values_yaml_only_chart_sections modelTransfer.images edit fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  sed 's|cleanupDrainBudgetSeconds: 210|cleanupDrainBudgetSeconds: 220|' "${vy_base}" > "${vy_cand}"
  assert_fails "assert_values_yaml_only_chart_sections unowned top-level key edit fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  sed 's|rootPath: /var/lib/gpustack/models|rootPath: /var/lib/gpustack/models-v2|' "${vy_base}" > "${vy_cand}"
  assert_fails "assert_values_yaml_only_chart_sections modelManager.rootPath edit fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  sed 's|# -- Migration hook Jobs\.|# -- Migration hook Jobs edited.|' "${vy_base}" > "${vy_cand}"
  assert_fails "assert_values_yaml_only_chart_sections unowned comment edit fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  python3 - "${vy_base}" "${vy_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(
    "      tag: v2.17.0\n",
    "      tag: v2.17.0\n      pullPolicy: Always\n",
    1,
)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_fails "assert_values_yaml_only_chart_sections extra key under modelManager.registrar.image fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  cp "${vy_base}" "${vy_cand}"
  printf '\nextraUnownedKey: true\n' >> "${vy_cand}"
  assert_fails "assert_values_yaml_only_chart_sections adding unowned top-level key fails" \
    assert_values_yaml_only_chart_sections "${vy_base}" "${vy_cand}"

  # 3e. hack/deps.sh section guard.
  local ds_base="${base_dir}/deps.sh"
  local ds_cand="${cand_dir}/deps.sh"
  cp "${ROOT_DIR}/hack/deps.sh" "${ds_base}"
  cp "${ds_base}" "${ds_cand}"
  assert_ok "assert_deps_sh_only_section unchanged passes charts" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts

  sed 's|kueue-0.18.11.tgz 0.18.11|kueue-0.18.12.tgz 0.18.12|' "${ds_base}" > "${ds_cand}"
  assert_ok "assert_deps_sh_only_section kueue chart edit passes charts" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts
  assert_fails "assert_deps_sh_only_section kueue chart edit fails mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  sed 's|kubernetes/api v0.36.5|kubernetes/api v0.36.6|' "${ds_base}" > "${ds_cand}"
  assert_ok "assert_deps_sh_only_section k8s.io/api edit passes mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods
  assert_fails "assert_deps_sh_only_section k8s.io/api edit fails charts" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts

  sed 's|gpustack::log::info "+++ MOD +++"|gpustack::log::info "+++ MOD EDITED +++"|' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section shell code edit fails charts" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts
  assert_fails "assert_deps_sh_only_section shell code edit fails mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  # Literal command substitution/variable tokens are test fixtures for assert_deps_sh_only_section.
  # shellcheck disable=SC2016
  sed 's|kueue-0.18.11.tgz 0.18.11|kueue-0.18.11.tgz $(id)|' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects \$(id) command substitution in charts version" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts

  # shellcheck disable=SC2016
  sed 's|https://github.com/kubernetes-sigs/kueue/releases/download/v0.18.11/kueue-0.18.11.tgz|https://github.com/kubernetes-sigs/kueue/releases/download/$(id)/kueue-0.18.11.tgz|' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects \$(id) command substitution in charts URL" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts

  # shellcheck disable=SC2016
  sed 's|kubernetes/api v0.36.5|kubernetes/api `id`|' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects backtick substitution in mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  # shellcheck disable=SC2016
  sed 's|\${staging_dir}/k8s.io/api|\${staging_dir}/k8s.io/../evil|' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects .. traversal in mods dest" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  grep -v 'topograph-1.0.0.tgz' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects row deletion in charts" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" charts

  grep -v 'gogo/protobuf' "${ds_base}" > "${ds_cand}"
  assert_fails "assert_deps_sh_only_section rejects row deletion in mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  python3 - "${ds_base}" "${ds_cand}" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    txt = f.read()
out = txt.replace(
    "https://github.com/gogo/protobuf v1.3.2 ${staging_dir}/github.com/gogo/protobuf\n",
    "https://github.com/gogo/protobuf v1.3.2 ${staging_dir}/github.com/gogo/protobuf\n"
    "https://github.com/evil/extra v1.0.0 ${staging_dir}/github.com/evil/extra\n",
    1,
)
assert out != txt
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.write(out)
PY
  assert_fails "assert_deps_sh_only_section rejects row addition in mods" \
    assert_deps_sh_only_section "${ds_base}" "${ds_cand}" mods

  # 3f. api/ generated-only guard.
  local api_base="${base_dir}/api"
  local api_cand="${cand_dir}/api"
  mkdir -p "${api_base}/v1" "${api_cand}/v1"
  printf 'package v1\n// hand\n' > "${api_base}/v1/model_deployment_types.go"
  printf 'package v1\n// gen\n' > "${api_base}/v1/zz_generated.deepcopy.go"
  cp -R "${api_base}/." "${api_cand}/"
  printf 'package v1\n// gen v2\n' > "${api_cand}/v1/zz_generated.deepcopy.go"
  printf 'package v1\n// pb\n' > "${api_cand}/v1/generated.pb.go"
  printf 'package v1\n// protomsg\n' > "${api_cand}/v1/generated.protomessage.pb.go"
  printf 'syntax = "proto3";\n' > "${api_cand}/v1/generated.proto"
  printf '[]\n' > "${api_cand}/zz_generated.openapi.violation_exceptions"
  assert_ok "assert_api_only_generated allows zz_generated*.go, violation_exceptions, protomessage.pb.go, pb.go, proto" \
    assert_api_only_generated "${api_base}" "${api_cand}"

  printf 'apiVersion: v1\n' > "${api_cand}/v1/zz_generated.extra.yaml"
  assert_fails "assert_api_only_generated rejects zz_generated*.yaml" \
    assert_api_only_generated "${api_base}" "${api_cand}"
  rm -f "${api_cand}/v1/zz_generated.extra.yaml"

  printf 'package v1\n// hand edited\n' > "${api_cand}/v1/model_deployment_types.go"
  assert_fails "assert_api_only_generated rejects hand-written types edit" \
    assert_api_only_generated "${api_base}" "${api_cand}"

  # 4. Published image manifest gate & preflight probe helpers.
  local manifests_json="${scratch}/image-manifests.json"
  local mock_json="${scratch}/mock-manifests.json"
  cat > "${mock_json}" <<'JSON'
{
  "images": {
    "docker.io/gpustack/mirrored-mooncake:0.3.14-cpu": {
      "published": true,
      "amd64Digest": "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
      "arm64Digest": "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
    },
    "docker.io/gpustack/llm-router:v0.5.0": {
      "published": true,
      "amd64Digest": "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc",
      "arm64Digest": ""
    },
    "docker.io/gpustack/single-arch:v1.0.0": {
      "published": true,
      "amd64Digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd",
      "arm64Digest": "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
    },
    "docker.io/gpustack/truncated-digest:v1.0.0": {
      "published": true,
      "amd64Digest": "sha256:deadbeef",
      "arm64Digest": "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee"
    },
    "docker.io/gpustack/unpublished:v0.0.1": {
      "published": false,
      "amd64Digest": "",
      "arm64Digest": ""
    }
  }
}
JSON
  AUTOSYNC_MOCK_MANIFESTS_JSON="${mock_json}" \
    autosync::preflight::record_image_probes "${manifests_json}" \
    "gpustack/mirrored-mooncake:0.3.14-cpu" \
    "gpustack/llm-router:v0.5.0" \
    "gpustack/single-arch:v1.0.0" \
    "gpustack/truncated-digest:v1.0.0" \
    "gpustack/unpublished:v0.0.1"

  assert_ok "assert_image_published accepts bare multi-arch published ref" \
    autosync::assert_image_published "${manifests_json}" "gpustack/mirrored-mooncake:0.3.14-cpu"
  assert_ok "assert_image_published accepts docker.io-prefixed multi-arch published ref" \
    autosync::assert_image_published "${manifests_json}" "docker.io/gpustack/mirrored-mooncake:0.3.14-cpu"
  assert_fails "assert_image_published rejects missing arm64Digest" \
    autosync::assert_image_published "${manifests_json}" "gpustack/llm-router:v0.5.0"
  assert_fails "assert_image_published rejects identical single-arch digest" \
    autosync::assert_image_published "${manifests_json}" "gpustack/single-arch:v1.0.0"
  assert_fails "assert_image_published rejects truncated sha256 digest" \
    autosync::assert_image_published "${manifests_json}" "gpustack/truncated-digest:v1.0.0"
  assert_fails "assert_image_published rejects unpublished image" \
    autosync::assert_image_published "${manifests_json}" "gpustack/unpublished:v0.0.1"
  assert_fails "assert_image_published rejects unknown image" \
    autosync::assert_image_published "${manifests_json}" "gpustack/unknown:v1.0.0"
  assert_fails "assert_image_published fails closed when manifests file missing" \
    autosync::assert_image_published "${scratch}/nonexistent.json" "gpustack/mirrored-mooncake:0.3.14-cpu"
  assert_fails "assert_image_published rejects direct identical digests even if published=true" \
    autosync::assert_image_published "${mock_json}" "gpustack/single-arch:v1.0.0"
  assert_fails "assert_image_published rejects direct truncated sha256 even if published=true" \
    autosync::assert_image_published "${mock_json}" "gpustack/truncated-digest:v1.0.0"

  # 5. preflight::init_stage, snapshot_baselines, warm_caches, fake crane, and write_github_output.
  assert_fails "preflight::init_stage refuses stage dir inside repo root" \
    autosync::preflight::init_stage "priors" "${ROOT_DIR}/.auto-sync/bad-stage"
  local ext_stage="${scratch}/ext-stage/priors"
  if autosync::preflight::init_stage "priors" "${ext_stage}" >/dev/null && [[ -d "${ext_stage}/baseline" ]]; then
    pass "preflight::init_stage creates external stage dir"
  else
    fail "preflight::init_stage creates external stage dir"
  fi

  if autosync::preflight::snapshot_baselines "${ext_stage}" "pack/gpustack-operator/Dockerfile" && \
     [[ -f "${ext_stage}/baseline/pack/gpustack-operator/Dockerfile" ]]; then
    pass "preflight::snapshot_baselines copies shared baseline into stage dir"
  else
    fail "preflight::snapshot_baselines copies shared baseline into stage dir"
  fi
  assert_fails "preflight::snapshot_baselines rejects empty path" \
    autosync::preflight::snapshot_baselines "${ext_stage}" ""
  assert_fails "preflight::snapshot_baselines rejects dot path" \
    autosync::preflight::snapshot_baselines "${ext_stage}" "."
  assert_fails "preflight::snapshot_baselines rejects ./ prefix" \
    autosync::preflight::snapshot_baselines "${ext_stage}" "./pack/gpustack-operator/Dockerfile"
  assert_fails "preflight::snapshot_baselines rejects traversal path" \
    autosync::preflight::snapshot_baselines "${ext_stage}" "../outside"

  if (
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true autosync::preflight::warm_caches "${ext_stage}" --sbin --go --tools shellcheck &&
    [[ -e "${ext_stage}/.sbin" && -e "${ext_stage}/go-cache" && -e "${ext_stage}/go-mod" && -e "${ext_stage}/deps-cache" && -e "${ext_stage}/uv-cache" ]] &&
    [[ -f "${ext_stage}/toolchain.env" && -x "${ext_stage}/.sbin/shellcheck" ]] &&
    grep -q '^AUTOSYNC_SAVED_GO_BIN_DIR=' "${ext_stage}/toolchain.env" &&
    grep -q '^AUTOSYNC_SAVED_GOROOT=' "${ext_stage}/toolchain.env"
  ); then
    pass "preflight::warm_caches links shared .sbin/go caches, populates tools, and records toolchain.env"
  else
    fail "preflight::warm_caches links shared .sbin/go caches, populates tools, and records toolchain.env"
  fi

  local single_stage
  single_stage="$(mktemp -d "/tmp/autosync-single-stage.XXXXXXXX")"
  _CLEANUP_DIRS+=("${single_stage}")
  if (
    AUTOSYNC_SKIP_GO_MOD_DOWNLOAD=true autosync::preflight::warm_caches "${single_stage}" --sbin --go &&
    [[ -d "${single_stage}/.shared/.sbin" && -L "${single_stage}/.sbin" && ! -e "/tmp/.sbin" ]]
  ); then
    pass "preflight::warm_caches keeps single-level /tmp stage caches inside .shared"
  else
    fail "preflight::warm_caches keeps single-level /tmp stage caches inside .shared"
  fi

  local gomod_stage="${scratch}/gomod-stage/priors"
  local fake_host_mod="${scratch}/fake-host-gomod"
  mkdir -p "${gomod_stage}" "${fake_host_mod}/cache/download"
  printf 'sentinel\n' > "${fake_host_mod}/cache/download/marker"
  if (
    GOPROXY=off GOMODCACHE="${fake_host_mod}" autosync::preflight::warm_caches "${gomod_stage}" --go &&
    [[ -f "${gomod_stage}/go-mod/cache/download/marker" ]]
  ); then
    pass "preflight::warm_caches seeds empty go-mod from host GOMODCACHE when GOPROXY=off"
  else
    fail "preflight::warm_caches seeds empty go-mod from host GOMODCACHE when GOPROXY=off"
  fi

  local fake_crane="${scratch}/fake-crane"
  cat > "${fake_crane}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
if [[ "$1" == "digest" && "$2" == "--platform" ]]; then
  case "$3" in
    linux/amd64) echo "sha256:1111111111111111111111111111111111111111111111111111111111111111" ;;
    linux/arm64) echo "sha256:2222222222222222222222222222222222222222222222222222222222222222" ;;
    *) exit 1 ;;
  esac
fi
SH
  chmod +x "${fake_crane}"
  local crane_manifests="${scratch}/crane-manifests.json"
  AUTOSYNC_MOCK_MANIFESTS_JSON="" AUTOSYNC_CRANE_BIN="${fake_crane}" \
    autosync::preflight::record_image_probes "${crane_manifests}" "gpustack/llm-router:v0.4.0"
  assert_ok "preflight::probe_image_multiarch resolves via crane binary" \
    autosync::assert_image_published "${crane_manifests}" "gpustack/llm-router:v0.4.0"

  local fake_crane_single="${scratch}/fake-crane-single"
  cat > "${fake_crane_single}" <<'SH'
#!/usr/bin/env bash
set -euo pipefail
echo "sha256:3333333333333333333333333333333333333333333333333333333333333333"
SH
  chmod +x "${fake_crane_single}"
  local crane_single_manifests="${scratch}/crane-single-manifests.json"
  AUTOSYNC_MOCK_MANIFESTS_JSON="" AUTOSYNC_CRANE_BIN="${fake_crane_single}" \
    autosync::preflight::record_image_probes "${crane_single_manifests}" "gpustack/single-arch:v0.4.0"
  assert_fails "preflight::probe_image_multiarch rejects single-arch identical crane digests" \
    autosync::assert_image_published "${crane_single_manifests}" "gpustack/single-arch:v0.4.0"

  local preflight_gha="${scratch}/preflight-gha.txt"
  : > "${preflight_gha}"
  GITHUB_OUTPUT="${preflight_gha}" autosync::preflight::write_github_output "upstream_records" $'line1\nline2'
  if grep -q '^upstream_records<<AUTOSYNC_EOF_' "${preflight_gha}" && grep -q '^line2$' "${preflight_gha}"; then
    pass "preflight::write_github_output writes delimiter-safe multiline output"
  else
    fail "preflight::write_github_output writes delimiter-safe multiline output"
  fi

  # 6. preflight::commit_detached_source_revision keeps HEAD/main clean.
  local det_repo="${scratch}/detached-repo"
  mkdir -p "${det_repo}/sub"
  git -C "${det_repo}" init -q -b main
  git -C "${det_repo}" config user.name "test"
  git -C "${det_repo}" config user.email "test@example.com"
  printf '*.lock\n' > "${det_repo}/.gitignore"
  printf 'v1\n' > "${det_repo}/sub/data.txt"
  git -C "${det_repo}" add .gitignore sub/data.txt
  git -C "${det_repo}" commit -q -m "base"
  local base_sha main_before noop_sha src_sha
  base_sha="$(git -C "${det_repo}" rev-parse HEAD)"
  main_before="$(git -C "${det_repo}" rev-parse refs/heads/main)"

  noop_sha="$(autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "noop" "sub/data.txt")"
  if [[ "${noop_sha}" == "${base_sha}" ]]; then
    pass "commit_detached_source_revision returns base_sha on no-op"
  else
    fail "commit_detached_source_revision returns base_sha on no-op"
  fi

  printf 'v2\n' > "${det_repo}/sub/data.txt"
  printf 'new\n' > "${det_repo}/sub/added.txt"
  printf 'lock-bytes\n' > "${det_repo}/sub/Chart.lock"
  src_sha="$(autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "chore: staged" "sub")"
  if [[ "${src_sha}" != "${base_sha}" ]] && \
     [[ "${src_sha}" =~ ^[0-9a-f]{40}$ ]] && \
     [[ "$(git -C "${det_repo}" rev-parse HEAD)" == "${base_sha}" ]] && \
     [[ "$(git -C "${det_repo}" rev-parse refs/heads/main)" == "${main_before}" ]] && \
     [[ -z "$(git -C "${det_repo}" status --ignored --porcelain)" ]] && \
     [[ ! -e "${det_repo}/sub/Chart.lock" ]] && \
     [[ "$(git -C "${det_repo}" show "${src_sha}:sub/data.txt")" == "v2" ]] && \
     [[ "$(git -C "${det_repo}" show "${src_sha}:sub/added.txt")" == "new" ]]; then
    pass "commit_detached_source_revision writes detached tree and clears ignored *.lock without dirtying tree"
  else
    fail "commit_detached_source_revision writes detached tree and clears ignored *.lock without dirtying tree"
  fi

  assert_fails "commit_detached_source_revision rejects empty path" \
    autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" ""
  assert_fails "commit_detached_source_revision rejects dot path" \
    autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" "."
  assert_fails "commit_detached_source_revision rejects ./ prefix" \
    autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" "./sub"
  assert_fails "commit_detached_source_revision rejects absolute path" \
    autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" "/tmp/evil"
  assert_fails "commit_detached_source_revision rejects .. traversal" \
    autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" "../evil"

  local det_tmp="${scratch}/det-tmp"
  mkdir -p "${det_tmp}"
  printf 'v3-dirty\n' > "${det_repo}/sub/data.txt"
  if ! TMPDIR="${det_tmp}" autosync::preflight::commit_detached_source_revision "${det_repo}" "${base_sha}" "bad" "sub" "nonexistent-path" >/dev/null 2>&1 && \
     [[ -z "$(ls -A "${det_tmp}")" ]] && \
     [[ "$(cat "${det_repo}/sub/data.txt")" == "v1" ]] && \
     [[ -z "$(git -C "${det_repo}" status --porcelain)" ]]; then
    pass "commit_detached_source_revision cleans up tmp_dir and restores paths on git failure"
  else
    fail "commit_detached_source_revision cleans up tmp_dir and restores paths on git failure"
  fi

  # 6b. preflight::commit_staged_tree_revision commits purely via temporary index without touching det_repo.
  local staged_root="${scratch}/staged-tree-root"
  mkdir -p "${staged_root}/sub"
  printf 'v1\n' > "${staged_root}/sub/data.txt"
  local staged_noop
  staged_noop="$(autosync::preflight::commit_staged_tree_revision "${det_repo}" "${base_sha}" "noop" "${staged_root}" "sub/data.txt")"
  if [[ "${staged_noop}" == "${base_sha}" ]]; then
    pass "commit_staged_tree_revision returns base_sha on no-op"
  else
    fail "commit_staged_tree_revision returns base_sha on no-op"
  fi

  printf 'uncommitted-local-edit\n' > "${det_repo}/sub/data.txt"
  printf 'v2-staged\n' > "${staged_root}/sub/data.txt"
  printf 'added-staged\n' > "${staged_root}/sub/added.txt"
  printf 'lock-ignored\n' > "${staged_root}/sub/Chart.lock"
  local staged_sha
  staged_sha="$(autosync::preflight::commit_staged_tree_revision "${det_repo}" "${base_sha}" "chore: staged tree" "${staged_root}" "sub")"
  if [[ "${staged_sha}" != "${base_sha}" ]] && \
     [[ "${staged_sha}" =~ ^[0-9a-f]{40}$ ]] && \
     [[ "$(git -C "${det_repo}" rev-parse HEAD)" == "${base_sha}" ]] && \
     [[ "$(git -C "${det_repo}" rev-parse refs/heads/main)" == "${main_before}" ]] && \
     [[ "$(cat "${det_repo}/sub/data.txt")" == "uncommitted-local-edit" ]] && \
     [[ "$(git -C "${det_repo}" show "${staged_sha}:sub/data.txt")" == "v2-staged" ]] && \
     [[ "$(git -C "${det_repo}" show "${staged_sha}:sub/added.txt")" == "added-staged" ]] && \
     ! git -C "${det_repo}" cat-file -e "${staged_sha}:sub/Chart.lock" 2>/dev/null; then
    pass "commit_staged_tree_revision writes detached commit without mutating repo_root working tree"
  else
    fail "commit_staged_tree_revision writes detached commit without mutating repo_root working tree"
  fi
  git -C "${det_repo}" checkout -q -- sub/data.txt

  assert_fails "commit_staged_tree_revision rejects .. traversal" \
    autosync::preflight::commit_staged_tree_revision "${det_repo}" "${base_sha}" "bad" "${staged_root}" "../evil"

  # 6b-2. has_available_upstreams + populate_stage_bundle + .gitignore exemption for .auto-sync/.stage/
  local bundle_upstreams="${scratch}/bundle-upstreams.json"
  cat > "${bundle_upstreams}" <<'EOF'
[{"upstreamId": "foo", "status": "up_to_date"}, {"upstreamId": "bar", "status": "up_to_date"}]
EOF
  if ! autosync::preflight::has_available_upstreams "${bundle_upstreams}"; then
    pass "has_available_upstreams returns non-zero when all upstreams are up_to_date"
  else
    fail "has_available_upstreams returns non-zero when all upstreams are up_to_date"
  fi
  cat > "${bundle_upstreams}" <<'EOF'
[{"upstreamId": "foo", "status": "up_to_date"}, {"upstreamId": "bar", "status": "available"}]
EOF
  if autosync::preflight::has_available_upstreams "${bundle_upstreams}"; then
    pass "has_available_upstreams returns zero when at least one upstream is available"
  else
    fail "has_available_upstreams returns zero when at least one upstream is available"
  fi

  local bundle_repo="${scratch}/bundle-repo"
  local bundle_src="${scratch}/bundle-src"
  local bundle_stage="${scratch}/bundle-stage"
  mkdir -p "${bundle_repo}" "${bundle_src}/dir/.git" "${bundle_stage}"
  git -C "${bundle_repo}" init -q -b main
  git -C "${bundle_repo}" config user.name test
  git -C "${bundle_repo}" config user.email test@example.com
  printf '.auto-sync/.stage/\n' > "${bundle_repo}/.gitignore"
  printf 'base\n' > "${bundle_repo}/README.md"
  git -C "${bundle_repo}" add .gitignore README.md
  git -C "${bundle_repo}" commit -q -m init
  local bundle_base
  bundle_base=$(git -C "${bundle_repo}" rev-parse HEAD)

  printf 'report\n' > "${bundle_src}/report.json"
  printf 'real-content\n' > "${bundle_src}/dir/real.txt"
  printf 'git-internal\n' > "${bundle_src}/dir/.git/HEAD"
  ln -s "${bundle_src}/dir/real.txt" "${bundle_src}/link.txt"

  autosync::preflight::populate_stage_bundle "${bundle_stage}" "demo" \
    "${bundle_src}/report.json" \
    "${bundle_src}/dir" \
    "${bundle_src}/link.txt::mapped/link.txt"

  assert_fails "populate_stage_bundle rejects .. destination traversal" \
    autosync::preflight::populate_stage_bundle "${bundle_stage}" "demo" "${bundle_src}/report.json::../escape.json"

  assert_fails "populate_stage_bundle enforces AUTOSYNC_STAGE_BUNDLE_MAX_BYTES cap" \
    env AUTOSYNC_STAGE_BUNDLE_MAX_BYTES=5 \
    autosync::preflight::populate_stage_bundle "${scratch}/bundle-stage-cap" "demo" "${bundle_src}/dir/real.txt"

  mkdir -p "${scratch}/bundle-broken-src/sub"
  ln -s "${scratch}/nonexistent-target" "${scratch}/bundle-broken-src/sub/broken-link"
  local broken_err=""
  if ! broken_err="$(autosync::preflight::populate_stage_bundle "${scratch}/bundle-broken-stage" "demo" "${scratch}/bundle-broken-src" 2>&1)" && \
     grep -q '^FAIL: ' <<<"${broken_err}" && \
     ! grep -q 'Traceback' <<<"${broken_err}"; then
    pass "populate_stage_bundle fails cleanly with FAIL: on dangling symlink inside directory walk"
  else
    fail "populate_stage_bundle fails cleanly with FAIL: on dangling symlink inside directory walk (${broken_err})"
  fi

  mkdir -p "${scratch}/bundle-cycle-src/subdir"
  printf 'cycle-ok\n' > "${scratch}/bundle-cycle-src/subdir/keep.txt"
  ln -s . "${scratch}/bundle-cycle-src/subdir/loop"
  if autosync::preflight::populate_stage_bundle "${scratch}/bundle-cycle-stage" "demo" "${scratch}/bundle-cycle-src" && \
     [[ -f "${scratch}/bundle-cycle-stage/.auto-sync/.stage/demo/bundle-cycle-src/subdir/keep.txt" ]] && \
     [[ ! -e "${scratch}/bundle-cycle-stage/.auto-sync/.stage/demo/bundle-cycle-src/subdir/loop" ]]; then
    pass "populate_stage_bundle terminates cleanly on directory symlink cycle"
  else
    fail "populate_stage_bundle terminates cleanly on directory symlink cycle"
  fi

  local bundle_rev
  bundle_rev=$(autosync::preflight::commit_staged_tree_revision "${bundle_repo}" "${bundle_base}" "stage demo bundle" "${bundle_stage}" ".auto-sync/.stage/demo")
  local bundle_ls
  bundle_ls=$(git -C "${bundle_repo}" ls-tree -r "${bundle_rev}" .auto-sync/.stage/demo)
  if [[ "${bundle_rev}" != "${bundle_base}" ]] \
    && grep -q '100644 blob .*	\.auto-sync/\.stage/demo/report\.json$' <<<"${bundle_ls}" \
    && grep -q '100644 blob .*	\.auto-sync/\.stage/demo/dir/real\.txt$' <<<"${bundle_ls}" \
    && grep -q '100644 blob .*	\.auto-sync/\.stage/demo/mapped/link\.txt$' <<<"${bundle_ls}" \
    && ! grep -q '^120000 ' <<<"${bundle_ls}" \
    && ! grep -q '\.git/HEAD' <<<"${bundle_ls}" \
    && [[ -z "$(git -C "${bundle_repo}" status --porcelain)" ]] \
    && [[ "$(git -C "${bundle_repo}" rev-parse HEAD)" == "${bundle_base}" ]]; then
    pass "populate_stage_bundle + commit_staged_tree_revision bypass .gitignore for .auto-sync/.stage/, dereference symlinks to 100644, skip .git, and leave tree clean"
  else
    fail "populate_stage_bundle + commit_staged_tree_revision bypass .gitignore for .auto-sync/.stage/, dereference symlinks to 100644, skip .git, and leave tree clean"
  fi

  # 6b-3. build-libvgpu.sh compiles vgpu + opportunistic test_postinit_owner_death instead of broken all-tests bash build.sh
  local libvgpu_script="${ROOT_DIR}/pack/gpustack-operator/external/nvidia/build-libvgpu.sh"
  if bash -n "${libvgpu_script}" \
    && bash "${ROOT_DIR}/hack/check/symbols.sh" "${libvgpu_script}" >/dev/null 2>&1 \
    && grep -q -- '--target vgpu' "${libvgpu_script}" \
    && grep -q 'test_postinit_owner_death' "${libvgpu_script}" \
    && ! grep -q 'bash build\.sh' "${libvgpu_script}"; then
    pass "build-libvgpu.sh targets vgpu + opportunistic test_postinit_owner_death and passes bash -n + symbols check"
  else
    fail "build-libvgpu.sh targets vgpu + opportunistic test_postinit_owner_death and passes bash -n + symbols check"
  fi

  # 6c. GPUSTACK_IMAGE_BUILD=true runs static checks and skips domain simulation suites; missing .auto-sync still fails closed otherwise.
  local img_ctx="${scratch}/img-build-ctx"
  mkdir -p "${img_ctx}/hack/check" "${img_ctx}/.auto-sync"
  cp "${ROOT_DIR}/hack/check/autosync-selftest.sh" "${img_ctx}/hack/check/autosync-selftest.sh"
  printf '#!/usr/bin/env bash\necho ok\n' > "${img_ctx}/.auto-sync/ok.sh"
  local sc_bin img_out="" img_bad_out=""
  sc_bin="$(resolve_shellcheck)"
  if img_out="$(SHELLCHECK_BIN="${sc_bin}" GPUSTACK_IMAGE_BUILD=true bash "${img_ctx}/hack/check/autosync-selftest.sh" all "${img_ctx}" 2>&1)" && \
     grep -q 'PASS  bash -n \.auto-sync/ok\.sh' <<<"${img_out}" && \
     grep -q 'PASS  shellcheck \.auto-sync/ok\.sh' <<<"${img_out}" && \
     grep -q 'SKIP  domain simulation suites skipped inside Docker image build (GPUSTACK_IMAGE_BUILD=true)' <<<"${img_out}"; then
    pass "GPUSTACK_IMAGE_BUILD=true runs static checks and skips domain simulation suites"
  else
    fail "GPUSTACK_IMAGE_BUILD=true runs static checks and skips domain simulation suites (${img_out})"
  fi

  printf '#!/usr/bin/env bash\nif true; then\n' > "${img_ctx}/.auto-sync/bad.sh"
  if ! img_bad_out="$(SHELLCHECK_BIN="${sc_bin}" GPUSTACK_IMAGE_BUILD=true bash "${img_ctx}/hack/check/autosync-selftest.sh" all "${img_ctx}" 2>&1)" && \
     grep -q 'FAIL  bash -n \.auto-sync/bad\.sh' <<<"${img_bad_out}"; then
    pass "GPUSTACK_IMAGE_BUILD=true still fails closed on .auto-sync syntax error"
  else
    fail "GPUSTACK_IMAGE_BUILD=true still fails closed on .auto-sync syntax error (${img_bad_out})"
  fi

  local img_ctx_missing="${scratch}/img-build-ctx-missing"
  mkdir -p "${img_ctx_missing}/hack/check"
  cp "${ROOT_DIR}/hack/check/autosync-selftest.sh" "${img_ctx_missing}/hack/check/autosync-selftest.sh"
  if ! SHELLCHECK_BIN="${sc_bin}" GPUSTACK_IMAGE_BUILD=false bash "${img_ctx_missing}/hack/check/autosync-selftest.sh" all "${img_ctx_missing}" >/dev/null 2>&1 && \
     ! env -u GPUSTACK_IMAGE_BUILD SHELLCHECK_BIN="${sc_bin}" bash "${img_ctx_missing}/hack/check/autosync-selftest.sh" all "${img_ctx_missing}" >/dev/null 2>&1; then
    pass "missing .auto-sync still fails closed when GPUSTACK_IMAGE_BUILD is unset or false"
  else
    fail "missing .auto-sync still fails closed when GPUSTACK_IMAGE_BUILD is unset or false"
  fi

  # 7. Offline hack/deps.sh cache self-test (GPUSTACK_DEPS_CACHE_DIR + GPUSTACK_DEPS_OFFLINE=true).
  local deps_repo="${scratch}/deps-repo"
  local deps_cache="${scratch}/deps-cache"
  mkdir -p "${deps_repo}/hack" "${deps_repo}/deploy/gpustack-operator/chart/charts" "${deps_cache}/charts"
  cp "${ROOT_DIR}/hack/deps.sh" "${deps_repo}/hack/deps.sh"
  cp -R "${ROOT_DIR}/hack/lib" "${deps_repo}/hack/lib"

  local chart_spec_list=()
  while IFS= read -r cline; do
    [[ -n "${cline}" ]] && chart_spec_list+=("${cline}")
  done < <(sed -n '/function chart_staging/,/^}/p' "${deps_repo}/hack/deps.sh" | sed -n '/cat <<EOF/,/^EOF/p' | grep -E '^https://')

  local sample_src="${scratch}/sample-chart"
  local line ver dest cname
  for line in "${chart_spec_list[@]}"; do
    read -r _ ver dest <<<"${line}"
    cname="$(basename "${dest}")"
    rm -rf "${sample_src}"
    mkdir -p "${sample_src}/${cname}"
    printf 'apiVersion: v2\nname: %s\nversion: %s\n' "${cname}" "${ver}" > "${sample_src}/${cname}/Chart.yaml"
    printf 'hello\n' > "${sample_src}/${cname}/README.md"
    tar -zcf "${deps_cache}/charts/${cname}-${ver}.tgz" -C "${sample_src}" "${cname}"
  done

  if GPUSTACK_DEPS_STAGE_ONLY=charts GPUSTACK_DEPS_OFFLINE=true GPUSTACK_DEPS_CACHE_DIR="${deps_cache}" \
    bash "${deps_repo}/hack/deps.sh" >/dev/null 2>&1 && \
    [[ "$(cat "${deps_repo}/deploy/gpustack-operator/chart/charts/kueue/_VERSION_")" == "0.18.11" ]]; then
    pass "hack/deps.sh offline charts staging succeeds from GPUSTACK_DEPS_CACHE_DIR"
  else
    fail "hack/deps.sh offline charts staging succeeds from GPUSTACK_DEPS_CACHE_DIR"
  fi

  rm -rf "${deps_repo}/deploy/gpustack-operator/chart/charts/kueue" "${deps_cache}/charts/kueue-0.18.11.tgz"
  if GPUSTACK_DEPS_STAGE_ONLY=charts GPUSTACK_DEPS_OFFLINE=true GPUSTACK_DEPS_CACHE_DIR="${deps_cache}" \
    bash "${deps_repo}/hack/deps.sh" >/dev/null 2>&1; then
    fail "hack/deps.sh offline cache-miss fails closed (expected non-zero)"
  else
    pass "hack/deps.sh offline cache-miss fails closed"
  fi

  # 7b. Offline hack/deps.sh mods git-mirror with submodule (protocol.file.allow=always).
  local mods_sub_src="${scratch}/mods-sub-src"
  local mods_main_src="${scratch}/mods-main-src"
  mkdir -p "${mods_sub_src}" "${mods_main_src}" "${deps_cache}/staging" "${deps_cache}/mods/github.com/gogo"
  git -C "${mods_sub_src}" init -q -b main
  git -C "${mods_sub_src}" config user.name "test"
  git -C "${mods_sub_src}" config user.email "test@example.com"
  printf 'sub\n' > "${mods_sub_src}/sub.txt"
  git -C "${mods_sub_src}" add sub.txt
  git -C "${mods_sub_src}" commit -q -m "sub"

  git -C "${mods_main_src}" init -q -b main
  git -C "${mods_main_src}" config user.name "test"
  git -C "${mods_main_src}" config user.email "test@example.com"
  printf 'module example.com/pb\n' > "${mods_main_src}/go.mod"
  git -C "${mods_main_src}" add go.mod
  git -C "${mods_main_src}" -c protocol.file.allow=always submodule add -q "file://${mods_sub_src}" submod
  git -C "${mods_main_src}" commit -q -m "main"
  git -C "${mods_main_src}" tag v1.3.2
  git clone --bare -q "${mods_main_src}" "${deps_cache}/mods/github.com/gogo/protobuf"

  local mod_spec_list=()
  while IFS= read -r mline; do
    [[ -n "${mline}" ]] && mod_spec_list+=("${mline}")
  done < <(sed -n '/function mod_staging/,/^}/p' "${deps_repo}/hack/deps.sh" | sed -n '/cat <<EOF/,/^EOF/p' | grep -E '^https://')

  local m_ver m_dest m_rel m_sample="${scratch}/sample-mod"
  for line in "${mod_spec_list[@]}"; do
    read -r _ m_ver m_dest <<<"${line}"
    m_rel="${m_dest#\$\{staging_dir\}/}"
    if [[ "${m_rel}" == "github.com/gogo/protobuf" ]]; then
      continue
    fi
    rm -rf "${m_sample}"
    mkdir -p "${m_sample}"
    printf 'module %s\n' "${m_rel}" > "${m_sample}/go.mod"
    mkdir -p "$(dirname "${deps_cache}/staging/${m_rel}-${m_ver}.tar.gz")"
    tar -zcf "${deps_cache}/staging/${m_rel}-${m_ver}.tar.gz" -C "${m_sample}" .
  done

  if GPUSTACK_DEPS_STAGE_ONLY=mods GPUSTACK_DEPS_OFFLINE=true GPUSTACK_DEPS_CACHE_DIR="${deps_cache}" \
    bash "${deps_repo}/hack/deps.sh" >/dev/null 2>&1 && \
    [[ "$(cat "${deps_repo}/staging/github.com/gogo/protobuf/_VERSION_")" == "v1.3.2" ]] && \
    [[ -f "${deps_repo}/staging/github.com/gogo/protobuf/submod/sub.txt" ]]; then
    pass "hack/deps.sh offline mods staging clones file:// mirror with submodule"
  else
    fail "hack/deps.sh offline mods staging clones file:// mirror with submodule"
  fi

  # 8. write_models_json.py self-tests.
  local gha_out="${scratch}/gha-out.txt"
  local models_out="${scratch}/models.json"
  : > "${gha_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    [[ ! -e "${models_out}" ]] && \
    grep -q '^configured=false$' "${gha_out}" && \
    grep -q '^provider=$' "${gha_out}"; then
    pass "write_models_json.py empty env exits 0 without writing models.json"
  else
    fail "write_models_json.py empty env exits 0 without writing models.json"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://open.bigmodel.cn/api/coding/paas/v4/chat/completions" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="glm-5.3" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    [[ -f "${models_out}" ]] && \
    grep -q '^provider=zai-coding-cn$' "${gha_out}" && \
    grep -q '^model=glm-5.3$' "${gha_out}" && \
    grep -q '^thinking=low$' "${gha_out}" && \
    grep -q '^api=openai-completions$' "${gha_out}" && \
    grep -q '^base-url=https://open.bigmodel.cn/api/coding/paas/v4$' "${gha_out}" && \
    grep -q '^provider-token-env=LOOPWRIGHT_PROVIDER_TOKEN$' "${gha_out}" && \
    grep -q '^allow-custom-base-url=true$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import os
import stat
import sys

p = sys.argv[1]
mode = stat.S_IMODE(os.stat(p).st_mode)
assert mode == 0o600, f"expected 0600 mode, got {oct(mode)}"
with open(p, "r", encoding="utf-8") as f:
    doc = json.load(f)
prov = doc["providers"]["zai-coding-cn"]
assert prov["baseUrl"] == "https://open.bigmodel.cn/api/coding/paas/v4"
assert prov["api"] == "openai-completions"
assert prov["apiKey"] == "$LOOPWRIGHT_PROVIDER_TOKEN"
m = prov["models"][0]
assert m["id"] == "glm-5.3"
assert m["reasoning"] is True
assert m["compat"]["thinkingFormat"] == "zai"
assert m["compat"]["supportsReasoningEffort"] is True
PY
  then
    pass "write_models_json.py Loopwright ZAI coding gateway resolution"
  else
    fail "write_models_json.py Loopwright ZAI coding gateway resolution"
  fi

  local lw_dist=""
  if lw_dist="$(resolve_loopwright_dist_js)" && command -v node >/dev/null 2>&1; then
    if LW_DIST="${lw_dist}" MODELS_PATH="${models_out}" node --input-type=module <<'JS'
const { loadPiModelsJson } = await import(process.env.LW_DIST);
const parsed = await loadPiModelsJson(process.env.MODELS_PATH);
const provider = parsed.providers.get('zai-coding-cn');
if (!provider || !provider.models.has('glm-5.3')) {
  throw new Error('loadPiModelsJson missing zai-coding-cn/glm-5.3');
}
JS
    then
      pass "write_models_json.py output accepted by Loopwright loadPiModelsJson"
    else
      fail "write_models_json.py output accepted by Loopwright loadPiModelsJson"
    fi
  else
    echo "INFO: skipping Loopwright loadPiModelsJson check (LOOPWRIGHT_DIST_JS unset or unreadable)"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://my-gateway.example.com/v1/messages" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="claude-sonnet-4-5" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_USE_ANTHROPIC="true" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_THINKING="max" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^api=anthropic-messages$' "${gha_out}" && \
    grep -q '^base-url=https://my-gateway.example.com/v1$' "${gha_out}" && \
    grep -q '^thinking=max$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    doc = json.load(f)
prov = doc["providers"]["custom-gateway"]
assert prov["baseUrl"] == "https://my-gateway.example.com/v1"
m = prov["models"][0]
assert m["thinkingLevelMap"] == {"max": "high"}, m
PY
  then
    pass "write_models_json.py normalizes /v1/messages and emits thinkingLevelMap for thinking=max"
  else
    fail "write_models_json.py normalizes /v1/messages and emits thinkingLevelMap for thinking=max"
  fi

  if [[ -n "${lw_dist}" ]] && command -v node >/dev/null 2>&1; then
    if LW_DIST="${lw_dist}" MODELS_PATH="${models_out}" node --input-type=module <<'JS'
const {
  loadPiModelsJson,
  resolveTaskpackModelSelection,
  DEFAULT_HOST_BUDGETS,
  LOCAL_COMMAND_GATE_ENDPOINT_ID,
  CANONICAL_TASKPACK_STEP_GATE_URL,
  DEFAULT_LOCAL_COMMAND_GATE_ENV,
} = await import(process.env.LW_DIST);
const modelsConfig = await loadPiModelsJson(process.env.MODELS_PATH);
const dummyManifest = {
  apiVersion: 'loopwright.dev/v1alpha1',
  kind: 'Taskpack',
  metadata: { name: 'priors-sync' },
  spec: {
    goal: { candidate: 'c', noChange: 'n' },
    scope: { writable: ['.auto-sync/proposals/priors.json'], protected: ['.auto-sync/checks'] },
    proposal: { path: '.auto-sync/proposals/priors.json', requiredFields: ['summary'], schemaVersion: 1 },
    upstreams: [{ id: 'u', kind: 'http-json', source: 'https://example.com', watchedPaths: [] }],
    verification: { localChecks: [], externalGates: [] },
    publication: { mode: 'pull_request', targetBranch: 'main', headBranchPrefix: 'auto-sync-priors/', titleTemplate: 't' },
    memory: { directory: '.auto-sync/memory/priors-sync', apply: 'separate-pull-request', onNoModelBudget: 'skip' },
    triggers: { schedule: true, workflowDispatch: true, releasePublished: false, commentCommands: [] },
  },
};
const resolved = resolveTaskpackModelSelection(
  dummyManifest,
  {
    mode: 'candidate',
    repository: 'gpustack/gpustack-operator',
    baseRevision: '0'.repeat(40),
    sourceRevision: '0'.repeat(40),
    budgets: DEFAULT_HOST_BUDGETS,
    allowManifestCustomBaseUrl: true,
    modelsJsonPath: process.env.MODELS_PATH,
    roleSelection: { provider: 'custom-gateway', model: 'claude-sonnet-4-5', thinking: 'max', api: 'anthropic-messages' },
    credentialEnvMap: { provider: 'LOOPWRIGHT_PROVIDER_TOKEN', localCommandGate: DEFAULT_LOCAL_COMMAND_GATE_ENV },
    gateEndpoints: [{
      id: LOCAL_COMMAND_GATE_ENDPOINT_ID,
      url: CANONICAL_TASKPACK_STEP_GATE_URL,
      signingCredentialRef: 's',
      authCredentialRef: null,
      timeoutMs: 60_000,
      maxPollMs: 75_000,
      pollInitialMs: 100,
      pollMaxMs: 500,
      jitter: 0,
    }],
  },
  modelsConfig,
);
if (resolved.selection.thinking !== 'max' || resolved.selection.provider !== 'custom-gateway') {
  throw new Error(`unexpected selection: ${JSON.stringify(resolved)}`);
}
JS
    then
      pass "write_models_json.py thinking=max custom gateway accepted by resolveTaskpackModelSelection"
    else
      fail "write_models_json.py thinking=max custom gateway accepted by resolveTaskpackModelSelection"
    fi
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://my-gateway.example.com/messages" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="claude-sonnet-4-5" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_PROTOCOL="anthropic-messages" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^base-url=https://my-gateway.example.com$' "${gha_out}"; then
    pass "write_models_json.py strips trailing /messages for anthropic-messages"
  else
    fail "write_models_json.py strips trailing /messages for anthropic-messages"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://my-gateway.example.com/v1/responses" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-5" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_PROTOCOL="openai-responses" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^base-url=https://my-gateway.example.com/v1$' "${gha_out}"; then
    pass "write_models_json.py strips trailing /responses for openai-responses"
  else
    fail "write_models_json.py strips trailing /responses for openai-responses"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="http://[::1]:8080/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="local-model" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^base-url=http://\[::1\]:8080/v1$' "${gha_out}"; then
    pass "write_models_json.py accepts explicit-port IPv6 loopback http://[::1]:8080/v1"
  else
    fail "write_models_json.py accepts explicit-port IPv6 loopback http://[::1]:8080/v1"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_CODEREVIEW_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_CODEREVIEW_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^provider=openai$' "${gha_out}" && \
    grep -q '^model=gpt-4o$' "${gha_out}" && \
    grep -q '^thinking=off$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    doc = json.load(f)
m = doc["providers"]["openai"]["models"][0]
assert m["reasoning"] is False, m
PY
  then
    pass "write_models_json.py fallback to CI_GPUSTACK_CODEREVIEW_LLM_* defaults non-ZAI to thinking=off"
  else
    fail "write_models_json.py fallback to CI_GPUSTACK_CODEREVIEW_LLM_* defaults non-ZAI to thinking=off"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_CODEREVIEW_LLM_URL="https://open.bigmodel.cn/api/coding/paas/v4" \
    CI_GPUSTACK_CODEREVIEW_LLM_MODEL="glm-5.3" \
    CI_GPUSTACK_CODEREVIEW_LLM_THINKING="enabled" \
    CI_GPUSTACK_CODEREVIEW_LLM_REASONING_EFFORT="low" \
    CI_GPUSTACK_CODEREVIEW_LLM_TEMPERATURE="0.4" \
    CI_GPUSTACK_CODEREVIEW_LLM_TOP_P="0.9" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^provider=zai-coding-cn$' "${gha_out}" && \
    grep -q '^model=glm-5.3$' "${gha_out}" && \
    grep -q '^thinking=low$' "${gha_out}" && \
    grep -q '^api=openai-completions$' "${gha_out}" && \
    grep -q '^base-url=https://open.bigmodel.cn/api/coding/paas/v4$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    doc = json.load(f)
prov = doc["providers"]["zai-coding-cn"]
m = prov["models"][0]
assert m["id"] == "glm-5.3", m
assert m["reasoning"] is True, m
assert m["samplingParams"] == {"temperature": 0.4, "top_p": 0.9}, m
assert m["compat"] == {"thinkingFormat": "zai", "supportsReasoningEffort": True}, m
PY
  then
    pass "write_models_json.py live org CODEREVIEW ZAI/glm-5.3 THINKING=enabled + REASONING_EFFORT=low -> thinking=low"
  else
    fail "write_models_json.py live org CODEREVIEW ZAI/glm-5.3 THINKING=enabled + REASONING_EFFORT=low -> thinking=low"
  fi

  if [[ -n "${lw_dist}" ]] && command -v node >/dev/null 2>&1; then
    if LW_DIST="${lw_dist}" MODELS_PATH="${models_out}" node --input-type=module <<'JS'
const {
  loadPiModelsJson,
  resolveTaskpackModelSelection,
  DEFAULT_HOST_BUDGETS,
  LOCAL_COMMAND_GATE_ENDPOINT_ID,
  CANONICAL_TASKPACK_STEP_GATE_URL,
  DEFAULT_LOCAL_COMMAND_GATE_ENV,
} = await import(process.env.LW_DIST);
const modelsConfig = await loadPiModelsJson(process.env.MODELS_PATH);
const dummyManifest = {
  apiVersion: 'loopwright.dev/v1alpha1',
  kind: 'Taskpack',
  metadata: { name: 'priors-sync' },
  spec: {
    goal: { candidate: 'c', noChange: 'n' },
    scope: { writable: ['.auto-sync/proposals/priors.json'], protected: ['.auto-sync/checks'] },
    proposal: { path: '.auto-sync/proposals/priors.json', requiredFields: ['summary'], schemaVersion: 1 },
    upstreams: [{ id: 'u', kind: 'http-json', source: 'https://example.com', watchedPaths: [] }],
    verification: { localChecks: [], externalGates: [] },
    publication: { mode: 'pull_request', targetBranch: 'main', headBranchPrefix: 'auto-sync-priors/', titleTemplate: 't' },
    memory: { directory: '.auto-sync/memory/priors-sync', apply: 'separate-pull-request', onNoModelBudget: 'skip' },
    triggers: { schedule: true, workflowDispatch: true, releasePublished: false, commentCommands: [] },
  },
};
const resolved = resolveTaskpackModelSelection(
  dummyManifest,
  {
    mode: 'candidate',
    repository: 'gpustack/gpustack-operator',
    baseRevision: '0'.repeat(40),
    sourceRevision: '0'.repeat(40),
    budgets: DEFAULT_HOST_BUDGETS,
    allowManifestCustomBaseUrl: true,
    modelsJsonPath: process.env.MODELS_PATH,
    roleSelection: { provider: 'zai-coding-cn', model: 'glm-5.3', thinking: 'low', api: 'openai-completions' },
    credentialEnvMap: { provider: 'LOOPWRIGHT_PROVIDER_TOKEN', localCommandGate: DEFAULT_LOCAL_COMMAND_GATE_ENV },
    gateEndpoints: [{
      id: LOCAL_COMMAND_GATE_ENDPOINT_ID,
      url: CANONICAL_TASKPACK_STEP_GATE_URL,
      signingCredentialRef: 's',
      authCredentialRef: null,
      timeoutMs: 60_000,
      maxPollMs: 75_000,
      pollInitialMs: 100,
      pollMaxMs: 500,
      jitter: 0,
    }],
  },
  modelsConfig,
);
if (resolved.selection.thinking !== 'low' || resolved.selection.provider !== 'zai-coding-cn') {
  throw new Error(`unexpected selection: ${JSON.stringify(resolved)}`);
}
JS
    then
      pass "write_models_json.py live org zai-coding-cn/glm-5.3 thinking=low accepted by resolveTaskpackModelSelection"
    else
      fail "write_models_json.py live org zai-coding-cn/glm-5.3 thinking=low accepted by resolveTaskpackModelSelection"
    fi
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_THINKING="enabled" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^thinking=low$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    doc = json.load(f)
assert doc["providers"]["openai"]["models"][0]["reasoning"] is True
PY
  then
    pass "write_models_json.py THINKING=enabled alone resolves to low with reasoning=true"
  else
    fail "write_models_json.py THINKING=enabled alone resolves to low with reasoning=true"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://open.bigmodel.cn/api/coding/paas/v4" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="glm-5.3" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_THINKING="disabled" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_REASONING_EFFORT="high" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^thinking=off$' "${gha_out}" && \
    python3 - "${models_out}" <<'PY'
import json
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    doc = json.load(f)
assert doc["providers"]["zai-coding-cn"]["models"][0]["reasoning"] is False
PY
  then
    pass "write_models_json.py THINKING=disabled overrides REASONING_EFFORT=high to off"
  else
    fail "write_models_json.py THINKING=disabled overrides REASONING_EFFORT=high to off"
  fi

  : > "${gha_out}"
  rm -f "${models_out}"
  if env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://open.bigmodel.cn/api/coding/paas/v4" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="glm-5.3" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_REASONING_EFFORT="medium" \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${gha_out}" >/dev/null && \
    grep -q '^thinking=high$' "${gha_out}"
  then
    pass "write_models_json.py snaps unsupported glm-5.3 thinking=medium to high"
  else
    fail "write_models_json.py snaps unsupported glm-5.3 thinking=medium to high"
  fi

  assert_fails "write_models_json.py rejects invalid THINKING=bogus" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_THINKING="bogus" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects invalid REASONING_EFFORT=bogus" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_REASONING_EFFORT="bogus" \
    python3 "${models_script}" --models-out "${models_out}"

  assert_fails "write_models_json.py rejects partial config (URL without MODEL)" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects insecure http:// non-loopback URL" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="http://example.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects invalid https:// port 99999" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com:99999/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects non-numeric https:// port" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com:abc/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects embedded URL credentials" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://user:pass@api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects OAuth provider openai-codex" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-5" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_PROVIDER="openai-codex" \
    python3 "${models_script}" --models-out "${models_out}"
  assert_fails "write_models_json.py rejects out-of-range temperature" \
    env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_TEMPERATURE="9.9" \
    python3 "${models_script}" --models-out "${models_out}"

  local sym_target="${scratch}/sym-target.txt"
  local sym_models="${scratch}/sym-models.json"
  local sym_gha="${scratch}/sym-gha.txt"
  printf 'SENTINEL\n' > "${sym_target}"
  ln -s "${sym_target}" "${sym_models}"
  ln -s "${sym_target}" "${sym_gha}"
  if ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
    CI_GPUSTACK_LOOPWRIGHT_LLM_URL="https://api.openai.com/v1" \
    CI_GPUSTACK_LOOPWRIGHT_LLM_MODEL="gpt-4o" \
    python3 "${models_script}" --models-out "${sym_models}" >/dev/null 2>&1 && \
    ! env -i PATH=/usr/local/bin:/usr/bin:/bin \
    python3 "${models_script}" --models-out "${models_out}" --github-output "${sym_gha}" >/dev/null 2>&1 && \
    [[ "$(cat "${sym_target}")" == "SENTINEL" ]]; then
    pass "write_models_json.py refuses symlink --models-out and --github-output without overwriting target"
  else
    fail "write_models_json.py refuses symlink --models-out and --github-output without overwriting target"
  fi

  # 9. Synthetic Taskpack + workflow validator self-tests (PyYAML and Ruby fallback).
  local synth_repo="${scratch}/synth-repo"
  mkdir -p "${synth_repo}/.auto-sync/instructions" \
    "${synth_repo}/.auto-sync/proposals" \
    "${synth_repo}/.auto-sync/scripts" \
    "${synth_repo}/.auto-sync/checks" \
    "${synth_repo}/.github/workflows"
  printf '# Priors instructions\n' > "${synth_repo}/.auto-sync/instructions/priors.md"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${synth_repo}/.auto-sync/scripts/preflight-priors.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' > "${synth_repo}/.auto-sync/checks/validate_priors.sh"
  cat > "${synth_repo}/.auto-sync/proposals/priors.json" <<'JSON'
{
  "schema_version": 1,
  "summary": "No changes",
  "versions": {},
  "subscriptions": []
}
JSON
  cat > "${synth_repo}/.auto-sync/priors-taskpack.yaml" <<'YAML'
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: priors-sync
spec:
  goal:
    candidate: Update capacity priors.
    noChange: Confirm priors match upstream.
  instructions: .auto-sync/instructions/priors.md
  scope:
    writable:
      - pkg/worker/scaling/priors/priors.yaml
      - .auto-sync/proposals/priors.json
    protected:
      - .auto-sync/checks
  upstreams:
    - id: inferencex
      kind: http-json
      source: https://inferencex.example.com/priors.json
      watchedPaths: []
      allowPrerelease: false
  proposal:
    path: .auto-sync/proposals/priors.json
    requiredFields:
      - schema_version
      - summary
      - versions
      - subscriptions
    schemaVersion: 1
  verification:
    localChecks:
      - id: proposal-schema
        implementation: json-artifact.v1
      - id: validate-priors
        implementation: command.v1
        parameters:
          script: .auto-sync/checks/validate_priors.sh
          timeoutSeconds: 600
          allowReadOnlyGitHubToken: false
          env:
            - AUTOSYNC_STAGE_DIR
    externalGates: []
  publication:
    mode: pull_request
    targetBranch: main
    headBranchPrefix: auto-sync-priors/
    titleTemplate: "chore(scaling): sync InferenceX capacity priors"
  memory:
    directory: .auto-sync/memory/priors-sync
    apply: separate-pull-request
    onNoModelBudget: deterministic-observations-only
  triggers:
    schedule: true
    workflowDispatch: true
    releasePublished: false
    commentCommands: []
YAML

  validate_domain_isolated() {
    (
      _sub_fail=0
      pass() { :; }
      fail() { _sub_fail=1; }
      validate_domain "$1" "$2" >/dev/null 2>&1 || _sub_fail=1
      (( _sub_fail == 0 ))
    )
  }

  assert_ok "validate_domain accepts valid synthetic loopwright.dev/v1alpha1 Taskpack" \
    validate_domain_isolated "priors" "${synth_repo}"
  if command -v ruby >/dev/null 2>&1; then
    AUTOSYNC_FORCE_RUBY_YAML=1 assert_ok "validate_domain accepts valid Taskpack under Ruby YAML fallback" \
      validate_domain_isolated "priors" "${synth_repo}"
  fi

  local synth_bad="${scratch}/synth-bad"
  cp -R "${synth_repo}" "${synth_bad}"
  sed 's/- AUTOSYNC_STAGE_DIR/- OTHER_VAR/' "${synth_repo}/.auto-sync/priors-taskpack.yaml" > "${synth_bad}/.auto-sync/priors-taskpack.yaml"
  assert_fails "validate_domain rejects Taskpack missing AUTOSYNC_STAGE_DIR in verification.localChecks" \
    validate_domain_isolated "priors" "${synth_bad}"

  sed 's|script: .auto-sync/checks/validate_priors.sh|script: .auto-sync/checks/other.sh|' "${synth_repo}/.auto-sync/priors-taskpack.yaml" > "${synth_bad}/.auto-sync/priors-taskpack.yaml"
  assert_fails "validate_domain rejects Taskpack with wrong command.v1 script path" \
    validate_domain_isolated "priors" "${synth_bad}"

  python3 - "${synth_repo}/.auto-sync/priors-taskpack.yaml" "${synth_bad}/.auto-sync/priors-taskpack.yaml" <<'PY'
import sys
with open(sys.argv[1], "r", encoding="utf-8") as f:
    lines = f.readlines()
out = [ln for ln in lines if "upstreams:" not in ln and "inferencex" not in ln and "kind: http-json" not in ln and "source:" not in ln and "watchedPaths:" not in ln and "allowPrerelease:" not in ln]
with open(sys.argv[2], "w", encoding="utf-8") as f:
    f.writelines(out)
PY
  assert_fails "validate_domain rejects Taskpack missing spec.upstreams" \
    validate_domain_isolated "priors" "${synth_bad}"

  sed 's/timeoutSeconds: 600/timeoutSeconds: \&tmo 600/' "${synth_repo}/.auto-sync/priors-taskpack.yaml" > "${synth_bad}/.auto-sync/priors-taskpack.yaml"
  assert_fails "validate_domain rejects Taskpack using YAML anchor" \
    validate_domain_isolated "priors" "${synth_bad}"
  if command -v ruby >/dev/null 2>&1; then
    AUTOSYNC_FORCE_RUBY_YAML=1 assert_fails "validate_domain rejects YAML anchor under Ruby fallback" \
      validate_domain_isolated "priors" "${synth_bad}"
  fi

  sed "s/noChange: Confirm priors match upstream./noChange: Confirm \${VAR} priors./" "${synth_repo}/.auto-sync/priors-taskpack.yaml" > "${synth_bad}/.auto-sync/priors-taskpack.yaml"
  assert_fails "validate_domain rejects Taskpack using interpolation token" \
    validate_domain_isolated "priors" "${synth_bad}"

  cat > "${synth_bad}/.auto-sync/priors-taskpack.yaml" <<'YAML'
apiVersion: loopwright.dev/v1alpha1
kind: Taskpack
metadata:
  name: priors-sync
  name: priors-sync-dup
YAML
  assert_fails "validate_domain rejects Taskpack with duplicate YAML keys" \
    validate_domain_isolated "priors" "${synth_bad}"
  if command -v ruby >/dev/null 2>&1; then
    AUTOSYNC_FORCE_RUBY_YAML=1 assert_fails "validate_domain rejects duplicate YAML keys under Ruby fallback" \
      validate_domain_isolated "priors" "${synth_bad}"
  fi

  cat > "${synth_repo}/.github/workflows/auto-sync.yml" <<'YAML'
name: Auto Sync
on:
  schedule:
    - cron: "0 1 * * 1"
    - cron: "0 2 * * 1"
    - cron: "0 2 * * 2"
    - cron: "0 3 * * 2"
    - cron: "0 2 * * 3"
    - cron: "0 2 * * 4"
  workflow_dispatch:
    inputs:
      domain:
        type: choice
      force:
        type: boolean
      dry-run:
        type: boolean
      runner_profile:
        default: "8x"
  issue_comment:
    types: [created]
jobs:
  sync:
    runs-on: ubuntu-24.04-8x
    if: github.ref == 'refs/heads/main'
    steps:
      - run: python3 .auto-sync/scripts/write_models_json.py --models-out /tmp/models.json
      - uses: gpustack/loopwright@main
        with:
          taskpack-path: .auto-sync/priors-taskpack.yaml
          upstream-records: "{}"
          source-revision: abc
          event-name: schedule
          event-path: /tmp/event.json
          models-json: /tmp/models.json
          provider-token-env: LOOPWRIGHT_PROVIDER_TOKEN
          allow-custom-base-url: "true"
YAML

  validate_workflow_isolated() {
    (
      _sub_fail=0
      pass() { :; }
      fail() { _sub_fail=1; }
      validate_workflow "$1" >/dev/null 2>&1 || _sub_fail=1
      (( _sub_fail == 0 ))
    )
  }
  assert_ok "validate_workflow accepts valid synthetic workflow" \
    validate_workflow_isolated "${synth_repo}"
  if command -v ruby >/dev/null 2>&1; then
    AUTOSYNC_FORCE_RUBY_YAML=1 assert_ok "validate_workflow handles Ruby 'true' key for 'on:'" \
      validate_workflow_isolated "${synth_repo}"
  fi

  local synth_wf_bad="${scratch}/synth-wf-bad"
  cp -R "${synth_repo}" "${synth_wf_bad}"
  sed 's/runs-on: ubuntu-24.04-8x/runs-on: \&runner ubuntu-24.04-8x/' "${synth_repo}/.github/workflows/auto-sync.yml" > "${synth_wf_bad}/.github/workflows/auto-sync.yml"
  assert_fails "validate_workflow rejects YAML anchor" \
    validate_workflow_isolated "${synth_wf_bad}"
  if command -v ruby >/dev/null 2>&1; then
    AUTOSYNC_FORCE_RUBY_YAML=1 assert_fails "validate_workflow rejects YAML anchor under Ruby fallback" \
      validate_workflow_isolated "${synth_wf_bad}"
  fi

  grep -v 'models-json:' "${synth_repo}/.github/workflows/auto-sync.yml" > "${synth_wf_bad}/.github/workflows/auto-sync.yml"
  assert_fails "validate_workflow rejects workflow missing models-json input" \
    validate_workflow_isolated "${synth_wf_bad}"

  # 10. Standalone gate-script.sh resolution of .auto-sync/checks/lib.sh from candidate cwd.
  local gate_cand="${scratch}/gate-cand"
  mkdir -p "${gate_cand}/.git" "${gate_cand}/.auto-sync/checks"
  cp "${ROOT_DIR}/.auto-sync/checks/lib.sh" "${gate_cand}/.auto-sync/checks/lib.sh"
  cp "${ROOT_DIR}/.auto-sync/checks/dockerfile_arg.py" "${gate_cand}/.auto-sync/checks/dockerfile_arg.py"
  local expected_checks_dir
  expected_checks_dir="$(cd "${gate_cand}/.auto-sync/checks" && pwd -P)"
  local clean_path="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin"
  local gate_domain gate_exec_dir gate_script gate_preamble gate_out="" expected_fail_re=""
  for gate_domain in charts k8s_deps kv_transfer priors router slicing; do
    gate_exec_dir="${scratch}/gate-exec-${gate_domain}"
    mkdir -p "${gate_exec_dir}"
    gate_script="${gate_exec_dir}/gate-script.sh"
    gate_preamble="${gate_exec_dir}/gate-preamble.sh"
    cp "${ROOT_DIR}/.auto-sync/checks/validate_${gate_domain}.sh" "${gate_script}"
    chmod +x "${gate_script}"

    if ! awk '
      { print }
      /^source "\$\{SCRIPT_DIR\}\/lib\.sh"$/ { found = 1; exit }
      END { exit !found }
    ' "${gate_script}" > "${gate_preamble}"; then
      fail "standalone gate-script.sh (validate_${gate_domain}.sh) contains expected source \"\${SCRIPT_DIR}/lib.sh\" line"
      continue
    fi
    cat >> "${gate_preamble}" <<'EOF'
declare -F autosync::bootstrap_env >/dev/null
[[ -f "${SCRIPT_DIR}/dockerfile_arg.py" ]]
printf 'SCRIPT_DIR=%s\n' "${SCRIPT_DIR}"
EOF

    if gate_out="$(
      cd "${gate_cand}" && \
      env -i PATH="${clean_path}" HOME="${scratch}" bash "${gate_preamble}" 2>&1
    )" && [[ "${gate_out}" == "SCRIPT_DIR=${expected_checks_dir}" ]]; then
      pass "standalone gate-script.sh (validate_${gate_domain}.sh) resolves lib.sh from candidate cwd"
    else
      fail "standalone gate-script.sh (validate_${gate_domain}.sh) resolves lib.sh from candidate cwd (${gate_out})"
    fi

    if gate_out="$(
      cd "${scratch}" && \
      env -i PATH="${clean_path}" HOME="${scratch}" LOOPWRIGHT_CANDIDATE_DIR="${gate_cand}" bash "${gate_preamble}" 2>&1
    )" && [[ "${gate_out}" == "SCRIPT_DIR=${expected_checks_dir}" ]]; then
      pass "standalone gate-script.sh (validate_${gate_domain}.sh) resolves lib.sh via LOOPWRIGHT_CANDIDATE_DIR"
    else
      fail "standalone gate-script.sh (validate_${gate_domain}.sh) resolves lib.sh via LOOPWRIGHT_CANDIDATE_DIR (${gate_out})"
    fi

    if [[ "${gate_domain}" == "charts" ]]; then
      if gate_out="$(
        cd "${gate_cand}" && \
        env -i PATH="${clean_path}" HOME="${scratch}" bash "${gate_script}" \
          --extract-images \
          "${ROOT_DIR}/deploy/gpustack-operator/chart/values.yaml" \
          "${ROOT_DIR}/deploy/gpustack-operator/chart/charts/topograph/Chart.yaml" 2>&1
      )" && [[ -n "${gate_out}" ]] && ! grep -q 'lib\.sh: No such file or directory' <<<"${gate_out}"; then
        pass "standalone gate-script.sh (validate_charts.sh --extract-images) executes from candidate cwd"
      else
        fail "standalone gate-script.sh (validate_charts.sh --extract-images) executes from candidate cwd (${gate_out})"
      fi

      if gate_out="$(
        cd "${scratch}" && \
        env -i PATH="${clean_path}" HOME="${scratch}" LOOPWRIGHT_CANDIDATE_DIR="${gate_cand}" bash "${gate_script}" \
          --extract-images \
          "${ROOT_DIR}/deploy/gpustack-operator/chart/values.yaml" \
          "${ROOT_DIR}/deploy/gpustack-operator/chart/charts/topograph/Chart.yaml" 2>&1
      )" && [[ -n "${gate_out}" ]] && ! grep -q 'lib\.sh: No such file or directory' <<<"${gate_out}"; then
        pass "standalone gate-script.sh (validate_charts.sh --extract-images) executes via LOOPWRIGHT_CANDIDATE_DIR"
      else
        fail "standalone gate-script.sh (validate_charts.sh --extract-images) executes via LOOPWRIGHT_CANDIDATE_DIR (${gate_out})"
      fi
    else
      expected_fail_re='AUTOSYNC_STAGE_DIR'
      if [[ "${gate_domain}" == "priors" ]]; then
        expected_fail_re='missing priors\.yaml or records\.md'
      fi

      if ! gate_out="$(
        cd "${gate_cand}" && \
        env -i PATH="${clean_path}" HOME="${scratch}" bash "${gate_script}" 2>&1
      )" && grep -q 'FAIL:' <<<"${gate_out}" && grep -q "${expected_fail_re}" <<<"${gate_out}" && ! grep -q 'lib\.sh: No such file or directory' <<<"${gate_out}"; then
        pass "standalone gate-script.sh (validate_${gate_domain}.sh) sources lib.sh from candidate cwd"
      else
        fail "standalone gate-script.sh (validate_${gate_domain}.sh) sources lib.sh from candidate cwd (${gate_out})"
      fi

      if ! gate_out="$(
        cd "${scratch}" && \
        env -i PATH="${clean_path}" HOME="${scratch}" LOOPWRIGHT_CANDIDATE_DIR="${gate_cand}" bash "${gate_script}" 2>&1
      )" && grep -q 'FAIL:' <<<"${gate_out}" && grep -q "${expected_fail_re}" <<<"${gate_out}" && ! grep -q 'lib\.sh: No such file or directory' <<<"${gate_out}"; then
        pass "standalone gate-script.sh (validate_${gate_domain}.sh) sources lib.sh via LOOPWRIGHT_CANDIDATE_DIR"
      else
        fail "standalone gate-script.sh (validate_${gate_domain}.sh) sources lib.sh via LOOPWRIGHT_CANDIDATE_DIR (${gate_out})"
      fi
    fi
  done
}

strict_yaml_to_json() {
  local yaml_path="$1"
  python3 - "${yaml_path}" <<'PY'
import json
import os
import subprocess
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    raw = f.read()

use_pyyaml = os.environ.get("AUTOSYNC_FORCE_RUBY_YAML", "") != "1"
yaml_mod = None
if use_pyyaml:
    try:
        import yaml as yaml_mod  # type: ignore
    except ImportError:
        yaml_mod = None

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
        sys.stderr.write(res.stderr)
        sys.exit(res.returncode or 1)
    print(res.stdout, end="")
else:
    stack = []
    for ev in yaml_mod.parse(raw):
        if isinstance(ev, yaml_mod.AliasEvent):
            raise SystemExit("YAML alias rejected")
        if getattr(ev, "anchor", None) is not None:
            raise SystemExit("YAML anchor rejected")
        if isinstance(ev, yaml_mod.MappingStartEvent):
            stack.append(("map", set(), True))
        elif isinstance(ev, yaml_mod.MappingEndEvent):
            stack.pop()
        elif isinstance(ev, yaml_mod.SequenceStartEvent):
            stack.append(("seq", None, False))
        elif isinstance(ev, yaml_mod.SequenceEndEvent):
            stack.pop()
        else:
            if stack and stack[-1][0] == "map":
                kind, seen, expect_key = stack[-1]
                if expect_key and isinstance(ev, yaml_mod.ScalarEvent):
                    if ev.value in seen:
                        raise SystemExit(f"duplicate YAML key: {ev.value}")
                    seen.add(ev.value)
                stack[-1] = (kind, seen, not expect_key)
    doc = yaml_mod.safe_load(raw)
    print(json.dumps(doc))
PY
}

validate_domain() {
  local domain="$1"
  local root_dir="${2:-${ROOT_DIR}}"
  local domain_under="${domain//-/_}"
  local taskpack="${root_dir}/.auto-sync/${domain}-taskpack.yaml"
  local instruction="${root_dir}/.auto-sync/instructions/${domain}.md"
  local proposal="${root_dir}/.auto-sync/proposals/${domain}.json"
  local preflight="${root_dir}/.auto-sync/scripts/preflight-${domain}.sh"
  local validator="${root_dir}/.auto-sync/checks/validate_${domain_under}.sh"
  local selftest="${root_dir}/.auto-sync/checks/selftest_${domain_under}.sh"

  local req
  for req in "${taskpack}" "${instruction}" "${proposal}" "${preflight}" "${validator}"; do
    if [[ ! -f "${req}" ]]; then
      fail "domain ${domain}: missing ${req#"${root_dir}/"}"
      return 1
    fi
  done

  local parsed_json=""
  if [[ ! -L "${taskpack}" && ! -L "${proposal}" ]] && \
     parsed_json="$(strict_yaml_to_json "${taskpack}")" && \
     python3 - "${domain}" "${taskpack}" "${proposal}" "${parsed_json}" "${root_dir}" <<'PY'
import json
import os
import sys

domain, taskpack_path, proposal_path, parsed_json, root_dir = sys.argv[1:6]
domain_under = domain.replace("-", "_")

for p in (taskpack_path, proposal_path):
    if os.path.islink(p):
        raise SystemExit(f"symlink rejected: {p}")

with open(taskpack_path, "r", encoding="utf-8") as f:
    raw = f.read()

if "${" in raw:
    raise SystemExit("interpolation token '${' rejected in taskpack")

doc = json.loads(parsed_json)
assert isinstance(doc, dict)
assert doc.get("apiVersion") == "loopwright.dev/v1alpha1", f"bad apiVersion: {doc.get('apiVersion')}"
assert doc.get("kind") == "Taskpack", f"bad kind: {doc.get('kind')}"
meta = doc.get("metadata", {})
assert meta.get("name") == f"{domain}-sync", f"metadata.name must be {domain}-sync"
spec = doc.get("spec", {})

goal = spec.get("goal", {})
pub = spec.get("publication", {})
for label, val in (
    ("spec.goal.candidate", goal.get("candidate")),
    ("spec.goal.noChange", goal.get("noChange")),
    ("spec.publication.titleTemplate", pub.get("titleTemplate")),
):
    assert isinstance(val, str) and val.strip() != "", f"missing {label}"
    assert "\n" not in val and "\r" not in val, f"{label} must be single-line"

instr = spec.get("instructions")
if isinstance(instr, str):
    instr_list = [instr]
elif isinstance(instr, list) and len(instr) >= 1 and all(isinstance(x, str) for x in instr):
    instr_list = instr
else:
    raise SystemExit("spec.instructions must be a non-empty string or list of paths")
for rel_instr in instr_list:
    instr_abs = os.path.join(root_dir, rel_instr)
    if os.path.islink(instr_abs) or not os.path.isfile(instr_abs):
        raise SystemExit(f"spec.instructions file missing or symlink: {rel_instr}")

upstreams = spec.get("upstreams")
assert isinstance(upstreams, list) and 1 <= len(upstreams) <= 32, "spec.upstreams must contain 1..32 entries"
for idx, up in enumerate(upstreams):
    assert isinstance(up, dict), f"spec.upstreams[{idx}] must be a mapping"
    for req_k in ("id", "kind", "source"):
        assert isinstance(up.get(req_k), str) and up[req_k].strip() != "", f"spec.upstreams[{idx}] missing {req_k}"

expected_proposal = f".auto-sync/proposals/{domain}.json"
prop = spec.get("proposal", {})
assert prop.get("path") == expected_proposal, f"spec.proposal.path must be {expected_proposal}"

scope = spec.get("scope", {})
writable = scope.get("writable", [])
protected = scope.get("protected", [])
assert isinstance(writable, list) and expected_proposal in writable, f"writable must include {expected_proposal}"
for entry in writable + protected:
    assert isinstance(entry, str) and "/**" not in entry, f"literal /** rejected in scope entry: {entry}"
for entry in writable:
    if entry != expected_proposal and (entry == ".auto-sync" or entry.startswith(".auto-sync/")):
        raise SystemExit(f"writable may not include other .auto-sync path: {entry}")

assert pub.get("headBranchPrefix") == f"auto-sync-{domain}/", f"bad headBranchPrefix: {pub.get('headBranchPrefix')}"
mem = spec.get("memory", {})
assert mem.get("directory") == f".auto-sync/memory/{domain}-sync", f"bad memory.directory: {mem.get('directory')}"
assert mem.get("apply") == "separate-pull-request", f"bad memory.apply: {mem.get('apply')}"
assert mem.get("onNoModelBudget") in ("deterministic-observations-only", "skip"), (
    f"bad memory.onNoModelBudget: {mem.get('onNoModelBudget')}"
)

triggers = spec.get("triggers")
assert isinstance(triggers, dict), "missing spec.triggers"
for bkey in ("schedule", "workflowDispatch", "releasePublished"):
    assert isinstance(triggers.get(bkey), bool), f"spec.triggers.{bkey} must be boolean"
assert isinstance(triggers.get("commentCommands"), list), "spec.triggers.commentCommands must be a list"

verification = spec.get("verification", {})
assert isinstance(verification, dict), "missing spec.verification"
checks = verification.get("localChecks", [])
assert isinstance(checks, list), "spec.verification.localChecks must be a list"
ext_gates = verification.get("externalGates")
assert isinstance(ext_gates, list), "spec.verification.externalGates must be a list"
expected_script = f".auto-sync/checks/validate_{domain_under}.sh"
found_cmd = False
for chk in checks:
    if isinstance(chk, dict) and chk.get("implementation") == "command.v1":
        params = chk.get("parameters", {})
        assert isinstance(params, dict), "command.v1 parameters must be a mapping"
        assert params.get("script") == expected_script, (
            f"command.v1 parameters.script must be {expected_script}, got {params.get('script')}"
        )
        env_list = params.get("env", [])
        assert isinstance(env_list, list) and "AUTOSYNC_STAGE_DIR" in env_list, "command.v1 must pass AUTOSYNC_STAGE_DIR in parameters.env"
        found_cmd = True
assert found_cmd, "at least one command.v1 check required in spec.verification.localChecks"

with open(proposal_path, "r", encoding="utf-8") as f:
    prop_doc = json.load(f)
assert isinstance(prop_doc, dict)
for req_field in prop.get("requiredFields", []):
    assert req_field in prop_doc, f"proposal missing requiredField: {req_field}"
PY
  then
    pass "domain ${domain}: strict YAML 1.2 + schema + proposal check"
  else
    fail "domain ${domain}: strict YAML 1.2 + schema + proposal check"
  fi

  local lw_dist=""
  if lw_dist="$(resolve_loopwright_dist_js)" && command -v node >/dev/null 2>&1; then
    if LW_DIST="${lw_dist}" REPO_ROOT="${root_dir}" TASKPACK_REL=".auto-sync/${domain}-taskpack.yaml" DOMAIN="${domain}" \
      node --input-type=module <<'JS'
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
const mod = await import(process.env.LW_DIST);
const {
  parseTaskpackDocument,
  validateTaskpackManifest,
  resolveTaskpackMemoryLocation,
  compileDeclarativeTaskpack,
} = mod;
const rel = process.env.TASKPACK_REL;
const bytes = readFileSync(join(process.env.REPO_ROOT, rel));
const parsed = parseTaskpackDocument(bytes, 'yaml');
const manifest = validateTaskpackManifest(parsed, { controlRoot: '.auto-sync', manifestPath: rel });
const mem = resolveTaskpackMemoryLocation(manifest, rel);
const expectedMemDir = `.auto-sync/memory/${process.env.DOMAIN}-sync`;
if (mem.controlRoot !== '.auto-sync' || mem.memoryRoot !== '.auto-sync/memory' || mem.memoryDirectory !== expectedMemDir) {
  throw new Error(`unexpected memory location: ${JSON.stringify(mem)}`);
}
await compileDeclarativeTaskpack(manifest, {
  mode: 'candidate',
  executionMode: 'delegated',
  repository: 'gpustack/gpustack-operator',
  taskpackPath: rel,
  repositoryRoot: process.env.REPO_ROOT,
  baseRevision: '0'.repeat(40),
  sourceRevision: '0'.repeat(40),
  budgets: mod.DEFAULT_HOST_BUDGETS,
  credentialEnvMap: {
    localCommandGate: mod.DEFAULT_LOCAL_COMMAND_GATE_ENV,
    publication: 'LOOPWRIGHT_PUBLICATION_TOKEN',
  },
  gateEndpoints: [{
    id: mod.LOCAL_COMMAND_GATE_ENDPOINT_ID,
    url: mod.CANONICAL_TASKPACK_STEP_GATE_URL,
    signingCredentialRef: 'taskpack-local-command-signing',
    authCredentialRef: null,
    timeoutMs: 60_000,
    maxPollMs: 75_000,
    pollInitialMs: 100,
    pollMaxMs: 500,
    jitter: 0,
  }],
  publicationEndpoint: {
    id: 'default',
    url: 'https://127.0.0.1:65535/publish',
    signingCredentialRef: 'pub-sign',
  },
});
JS
    then
      pass "domain ${domain}: Loopwright compiler validation"
    else
      fail "domain ${domain}: Loopwright compiler validation"
    fi
  else
    echo "INFO: skipping domain ${domain} Loopwright compiler check (LOOPWRIGHT_DIST_JS unset or unreadable)"
  fi

  if [[ "${MODE}" != "fast" && -f "${selftest}" ]]; then
    if bash "${selftest}" "${root_dir}"; then
      pass "domain ${domain}: selftest_${domain_under}.sh"
    else
      fail "domain ${domain}: selftest_${domain_under}.sh"
    fi
  fi
}

run_domain_discovery() {
  echo
  echo "== domain taskpack and validator checks =="

  if [[ -d "${ROOT_DIR}/.auto-sync" ]]; then
    local entry_count
    entry_count="$(find "${ROOT_DIR}/.auto-sync" -mindepth 1 | wc -l | tr -d ' ')"
    if (( entry_count < 120 )); then
      pass ".auto-sync entry count (${entry_count} < 120)"
    else
      fail ".auto-sync entry count (${entry_count} >= 120, approaches Loopwright 128 cap)"
    fi
  fi

  local manifests=()
  if [[ -d "${ROOT_DIR}/.auto-sync" ]]; then
    local mf
    while IFS= read -r mf; do
      [[ -n "${mf}" ]] && manifests+=("${mf}")
    done < <(find "${ROOT_DIR}/.auto-sync" -maxdepth 1 -type f -name '*-taskpack.yaml' | sort)
  fi

  if [[ "${#manifests[@]}" -eq 0 ]]; then
    echo "INFO: 0 taskpack manifests present yet"
    return 0
  fi

  local mf domain
  for mf in "${manifests[@]}"; do
    domain="$(basename "${mf}" "-taskpack.yaml")"
    validate_domain "${domain}" || true
  done
}

validate_workflow() {
  local root_dir="${1:-${ROOT_DIR}}"
  echo
  echo "== workflow validation =="
  local wf="${root_dir}/.github/workflows/auto-sync.yml"
  if [[ ! -f "${wf}" ]]; then
    fail "missing workflow file: .github/workflows/auto-sync.yml"
    return 1
  fi

  local wf_json=""
  if wf_json="$(strict_yaml_to_json "${wf}")" && \
     python3 - "${wf}" "${wf_json}" <<'PY'
import json
import sys

wf_path, wf_json = sys.argv[1:3]
with open(wf_path, "r", encoding="utf-8") as f:
    raw = f.read()

doc = json.loads(wf_json)
assert isinstance(doc, dict)

on_block = doc.get("on") or doc.get(True) or doc.get("true") or {}
assert isinstance(on_block, dict) and on_block, "missing workflow 'on' block"

schedules = on_block.get("schedule", [])
crons = {item.get("cron") for item in schedules if isinstance(item, dict)}
expected_crons = {"0 1 * * 1", "0 2 * * 1", "0 2 * * 2", "0 3 * * 2", "0 2 * * 3", "0 2 * * 4"}
missing_crons = expected_crons - crons
assert not missing_crons, f"missing schedule crons: {sorted(missing_crons)}"

wd = on_block.get("workflow_dispatch", {})
inputs = wd.get("inputs", {})
for key in ("domain", "force", "dry-run", "runner_profile"):
    assert key in inputs, f"missing workflow_dispatch input: {key}"
assert str(inputs["runner_profile"].get("default")) == "8x", "runner_profile default must be 8x"
assert "ubuntu-24.04-8x" in raw, "workflow must route runner_profile 8x to ubuntu-24.04-8x"

assert "issue_comment" in on_block, "missing issue_comment trigger"
assert "refs/heads/main" in raw, "issue_comment routing must preserve refs/heads/main"
assert "write_models_json.py" in raw, "workflow must run write_models_json.py"

jobs = doc.get("jobs", {})
found_lw = False
required_lw_inputs = {
    "taskpack-path",
    "upstream-records",
    "source-revision",
    "event-name",
    "event-path",
    "models-json",
    "provider-token-env",
    "allow-custom-base-url",
}
for job in jobs.values():
    if not isinstance(job, dict):
        continue
    for step in job.get("steps", []):
        if isinstance(step, dict) and str(step.get("uses", "")).startswith("gpustack/loopwright@"):
            found_lw = True
            lw_with = set((step.get("with") or {}).keys())
            missing_in = required_lw_inputs - lw_with
            assert not missing_in, f"loopwright step missing inputs: {sorted(missing_in)}"
assert found_lw, "missing gpustack/loopwright step"
PY
  then
    pass "workflow .github/workflows/auto-sync.yml schema and triggers"
  else
    fail "workflow .github/workflows/auto-sync.yml schema and triggers"
  fi
}

run_static_checks

if [[ "${GPUSTACK_IMAGE_BUILD:-}" == "true" ]]; then
  if (( FAILS > 0 )); then
    echo "AUTOSYNC SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
    exit 1
  fi
  echo "SKIP  domain simulation suites skipped inside Docker image build (GPUSTACK_IMAGE_BUILD=true)"
  exit 0
fi

case "${MODE}" in
  all)
    run_common_tests
    run_domain_discovery
    if [[ -f "${ROOT_DIR}/.github/workflows/auto-sync.yml" ]]; then
      validate_workflow
    fi
    ;;
  fast)
    run_domain_discovery
    if [[ -f "${ROOT_DIR}/.github/workflows/auto-sync.yml" ]]; then
      validate_workflow
    fi
    ;;
  common)
    run_common_tests
    ;;
  priors|slicing|kv-transfer|router|charts|k8s-deps)
    validate_domain "${MODE}"
    ;;
  workflow)
    validate_workflow
    ;;
esac

echo
if (( FAILS > 0 )); then
  echo "AUTOSYNC SELFTEST FAILED: ${FAILS} failure(s), ${PASSES} passed" >&2
  exit 1
fi
echo "AUTOSYNC SELFTEST PASSED: ${PASSES} check(s) passed"
