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

  local go_bin="" go_bin_dir="" goroot_val="" hugo_bin="" hugo_bin_dir="" node_bin="" node_bin_dir="" uv_bin="" uv_bin_dir=""
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

# autosync::preflight::warm_caches <stage_dir> [--sbin] [--go] [--tools <comma-list>]
# Populates shared /tmp/gpustack-autosync-stage/{.sbin,go-cache,go-mod,deps-cache} caches,
# records host toolchain paths into <stage_dir>/toolchain.env, and links caches into <stage_dir>.
autosync::preflight::warm_caches() {
  local stage_dir="${1:?stage_dir required}"
  shift

  local want_sbin=false
  local want_go=false
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
    for cache_name in go-cache go-mod deps-cache uv-cache; do
      mkdir -p "${shared_root}/${cache_name}"
      if [[ ! -e "${stage_dir}/${cache_name}" || -L "${stage_dir}/${cache_name}" ]]; then
        ln -sfn "${shared_root}/${cache_name}" "${stage_dir}/${cache_name}"
      fi
    done

    if [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" ]] && command -v go >/dev/null 2>&1; then
      if [[ -z "$(ls -A "${shared_root}/go-mod" 2>/dev/null)" ]]; then
        local host_gomodcache=""
        host_gomodcache="$(go env GOMODCACHE 2>/dev/null || true)"
        if [[ "${GOPROXY:-}" == "off" ]]; then
          if [[ -n "${host_gomodcache}" && -d "${host_gomodcache}" && -r "${host_gomodcache}" && -n "$(ls -A "${host_gomodcache}" 2>/dev/null)" ]]; then
            cp -RLp "${host_gomodcache}/." "${shared_root}/go-mod/" 2>/dev/null || true
          fi
        elif [[ -f "${repo_root}/go.mod" ]]; then
          local effective_goproxy="${GOPROXY:-https://proxy.golang.org,direct}"
          if [[ -n "${host_gomodcache}" && -d "${host_gomodcache}/cache/download" && -r "${host_gomodcache}/cache/download" ]]; then
            effective_goproxy="file://${host_gomodcache}/cache/download,${effective_goproxy}"
          fi
          (
            cd "${repo_root}"
            GOMODCACHE="${shared_root}/go-mod" \
            GOCACHE="${shared_root}/go-cache" \
            GOPROXY="${effective_goproxy}" \
              go mod download
          )
        fi
      fi
    fi

    if [[ "${AUTOSYNC_SKIP_GO_MOD_DOWNLOAD:-false}" != "true" ]] && \
       [[ "${GOPROXY:-}" != "off" ]] && \
       command -v uv >/dev/null 2>&1 && \
       [[ -d "${repo_root}/model-transfer/python" ]] && \
       [[ -z "$(ls -A "${shared_root}/uv-cache" 2>/dev/null)" ]]; then
      (
        cd "${repo_root}/model-transfer/python"
        UV_CACHE_DIR="${shared_root}/uv-cache" uv sync --frozen >/dev/null 2>&1 || true
      )
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
if isinstance(entry, dict):
    amd64 = str(entry.get("amd64Digest") or "").strip()
    arm64 = str(entry.get("arm64Digest") or "").strip()
    if not DIGEST_RE.fullmatch(amd64):
        amd64 = ""
    if not DIGEST_RE.fullmatch(arm64):
        arm64 = ""
    published = bool(entry.get("published")) and bool(amd64) and bool(arm64) and amd64 != arm64

print(json.dumps({
    "image": image_ref,
    "normalized": normalized,
    "published": published,
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
published = bool(amd64 and arm64 and amd64 != arm64)

print(json.dumps({
    "image": image_ref,
    "normalized": normalized,
    "published": published,
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
