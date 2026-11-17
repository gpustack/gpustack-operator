#!/usr/bin/env bash
# .auto-sync/checks/validate_router.sh - command.v1 verification gate for router-sync.
set -euo pipefail

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
# shellcheck source=./lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env "${AUTOSYNC_STAGE_DIR:-}" "${LOOPWRIGHT_CANDIDATE_DIR:-$(pwd -P)}"
cd "${AUTOSYNC_CANDIDATE_ROOT}"

_CLEANUP_DIRS=()

cleanup_on_exit() {
  local d
  for d in "${_CLEANUP_DIRS[@]+"${_CLEANUP_DIRS[@]}"}"; do
    [[ -n "${d}" && -e "${d}" ]] && rm -rf "${d}"
  done
  autosync::cleanup_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"
}
trap cleanup_on_exit EXIT

autosync::ensure_ephemeral_git "${AUTOSYNC_CANDIDATE_ROOT}"

STAGE_DIR="${AUTOSYNC_STAGE_DIR:-}"
if [[ -z "${STAGE_DIR}" || ! -d "${STAGE_DIR}/baseline" ]]; then
  printf 'FAIL: AUTOSYNC_STAGE_DIR must point to a preflight stage directory with baseline/\n' >&2
  exit 1
fi

BASELINE_DIR="${STAGE_DIR}/baseline"

# 1. Shared-file variable-level diff guards.
ROUTER_SETTING_KEYS='model-deployment-router-image|model-deployment-router-proxy-image|model-deployment-routing-sidecar-image'
assert_value_go_only_keys \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${ROUTER_SETTING_KEYS}"

assert_settings_md_only_keys \
  "${BASELINE_DIR}/docs/reference/settings.md" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${ROUTER_SETTING_KEYS}"

# 2. Parse router Dockerfile version pins.
CAND_DOCKERFILE="${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/Dockerfile"
BASE_DOCKERFILE="${BASELINE_DIR}/pack/llm-router/Dockerfile"
if [[ ! -f "${CAND_DOCKERFILE}" || ! -f "${BASE_DOCKERFILE}" ]]; then
  printf 'FAIL: missing baseline (%s) or candidate (%s) Dockerfile\n' "${BASE_DOCKERFILE}" "${CAND_DOCKERFILE}" >&2
  exit 1
fi

extract_df_arg() {
  local df_path="$1"
  local arg_name="$2"
  python3 - "${df_path}" "${arg_name}" <<'PY'
import re
import sys

path, name = sys.argv[1:3]
with open(path, "r", encoding="utf-8") as f:
    txt = f.read()
matches = re.findall(rf'^ARG {re.escape(name)}=(?:"([^"]+)"|([^\s#]+))', txt, re.MULTILINE)
if len(matches) != 1:
    print(f"FAIL: expected exactly 1 ARG {name} in {path}, found {len(matches)}", file=sys.stderr)
    sys.exit(1)
val = (matches[0][0] or matches[0][1]).strip()
if not val:
    print(f"FAIL: empty ARG {name} in {path}", file=sys.stderr)
    sys.exit(1)
print(val)
PY
}

CAND_LLMD_VER="$(extract_df_arg "${CAND_DOCKERFILE}" "LLMD_ROUTER_VERSION")"
CAND_VLLM_VER="$(extract_df_arg "${CAND_DOCKERFILE}" "VLLM_ROUTER_VERSION")"
CAND_SGLANG_VER="$(extract_df_arg "${CAND_DOCKERFILE}" "SGLANG_GATEWAY_VERSION")"

BASE_LLMD_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "LLMD_ROUTER_VERSION")"
BASE_VLLM_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "VLLM_ROUTER_VERSION")"
BASE_SGLANG_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "SGLANG_GATEWAY_VERSION")"

# 3. Validate 0001-epd-routing.patch retirement invariant when router-preflight-report.json exists.
EPD_PATCH="${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"
PREFLIGHT_REPORT="${STAGE_DIR}/router-preflight-report.json"
if [[ -f "${PREFLIGHT_REPORT}" ]]; then
  RETIRE_INFO="$(python3 - "${PREFLIGHT_REPORT}" <<'PY'
import json
import sys

with open(sys.argv[1], "r", encoding="utf-8") as f:
    rep = json.load(f)
retire = "true" if rep.get("retire_0001_epd_routing") is True else "false"
target_vllm = str((rep.get("target_tags") or {}).get("vllm-router") or "")
print(f"{retire}|{target_vllm}")
PY
)"
  RETIRE_0001="${RETIRE_INFO%%|*}"
  TARGET_VLLM_VER="${RETIRE_INFO#*|}"
  if [[ "${RETIRE_0001}" == "false" && ! -f "${EPD_PATCH}" ]]; then
    printf 'FAIL: 0001-epd-routing.patch was deleted while retire_0001_epd_routing is false\n' >&2
    exit 1
  fi
  if [[ "${RETIRE_0001}" == "true" && -f "${EPD_PATCH}" ]]; then
    if [[ -n "${TARGET_VLLM_VER}" && "${CAND_VLLM_VER}" == "${TARGET_VLLM_VER}" ]] || \
       [[ "${CAND_VLLM_VER}" != "${BASE_VLLM_VER}" ]]; then
      printf 'FAIL: 0001-epd-routing.patch must be retired when VLLM_ROUTER_VERSION advances to %s (516f1cb ancestor)\n' \
        "${CAND_VLLM_VER}" >&2
      exit 1
    fi
  fi
fi

# 4. Apply patches cleanly against staged upstream clones and verify struct ObserverRouter.
apply_staged_patches() {
  local name="$1"
  local stage_repo="$2"
  local target_ver="$3"
  local patch_dir="$4"
  local sparse_subpath="${5:-}"

  local patches=()
  local pf
  if [[ -d "${patch_dir}" ]]; then
    while IFS= read -r pf; do
      [[ -n "${pf}" ]] && patches+=("${pf}")
    done < <(LC_ALL=C find "${patch_dir}" -maxdepth 1 -type f -name '*.patch' | LC_ALL=C sort)
  fi

  if (( ${#patches[@]} == 0 )); then
    printf 'FAIL: no *.patch files found under %s\n' "${patch_dir}" >&2
    return 1
  fi

  if [[ ! -d "${stage_repo}" ]]; then
    if [[ "${name}" == "sglang" ]]; then
      if ! grep -q "struct ObserverRouter" "${patches[@]}"; then
        printf 'FAIL: struct ObserverRouter missing from sglang-gateway patches\n' >&2
        return 1
      fi
    fi
    return 0
  fi

  if ! git -C "${stage_repo}" rev-parse --verify -q "${target_ver}^{commit}" >/dev/null 2>&1; then
    printf 'FAIL: %s version %s not found in staged repo %s\n' "${name}" "${target_ver}" "${stage_repo}" >&2
    return 1
  fi

  local tmp_tree
  tmp_tree="$(mktemp -d "${TMPDIR:-/tmp}/autosync-router-${name}.XXXXXXXX")"
  _CLEANUP_DIRS+=("${tmp_tree}")

  local is_promisor="false"
  if [[ "$(git -C "${stage_repo}" config --bool remote.origin.promisor 2>/dev/null || echo false)" == "true" ]]; then
    is_promisor="true"
  fi

  if [[ "${is_promisor}" == "true" && -n "${sparse_subpath}" ]]; then
    git -C "${stage_repo}" archive "${target_ver}" -- "${sparse_subpath}" | tar -x -C "${tmp_tree}"
  else
    git -C "${stage_repo}" archive "${target_ver}" | tar -x -C "${tmp_tree}"
  fi

  git -C "${tmp_tree}" init -q
  for pf in "${patches[@]}"; do
    if ! git -C "${tmp_tree}" apply --check "${pf}"; then
      printf 'FAIL: git apply --check failed for %s (%s at %s)\n' \
        "$(basename "${pf}")" "${name}" "${target_ver}" >&2
      return 1
    fi
    if ! git -C "${tmp_tree}" apply "${pf}"; then
      printf 'FAIL: git apply failed for %s (%s at %s)\n' \
        "$(basename "${pf}")" "${name}" "${target_ver}" >&2
      return 1
    fi
  done

  if [[ "${name}" == "sglang" ]]; then
    local observer_rs="${tmp_tree}/sgl-model-gateway/src/observer.rs"
    if [[ ! -f "${observer_rs}" ]] || ! grep -q "struct ObserverRouter" "${observer_rs}"; then
      printf 'FAIL: struct ObserverRouter missing in %s after applying sglang-gateway patches\n' \
        "sgl-model-gateway/src/observer.rs" >&2
      return 1
    fi
  fi
}

apply_staged_patches "llm-d-router" "${STAGE_DIR}/llm-d-router" "${CAND_LLMD_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/llm-d"

apply_staged_patches "vllm-router" "${STAGE_DIR}/vllm-router" "${CAND_VLLM_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/vllm-router"

apply_staged_patches "sglang" "${STAGE_DIR}/sglang" "${CAND_SGLANG_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/sglang-gateway" "sgl-model-gateway"

# 5. Detect modified router stages and run targeted docker buildx verification.
CHANGED_STAGES="$(python3 - \
  "${BASE_DOCKERFILE}" "${CAND_DOCKERFILE}" \
  "${BASELINE_DIR}/pack/llm-router/patches" "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches" \
  "${BASE_LLMD_VER}" "${CAND_LLMD_VER}" \
  "${BASE_VLLM_VER}" "${CAND_VLLM_VER}" \
  "${BASE_SGLANG_VER}" "${CAND_SGLANG_VER}" <<'PY'
import os
import re
import subprocess
import sys

(
    base_df,
    cand_df,
    base_patches_root,
    cand_patches_root,
    base_llmd,
    cand_llmd,
    base_vllm,
    cand_vllm,
    base_sglang,
    cand_sglang,
) = sys.argv[1:11]

ALL_BUILDERS = ("epp-builder", "vllm-router-builder", "sglang-gateway-builder")
stages = set()

if cand_llmd != base_llmd:
    stages.add("epp-builder")
if cand_vllm != base_vllm:
    stages.add("vllm-router-builder")
if cand_sglang != base_sglang:
    stages.add("sglang-gateway-builder")

patch_map = (
    ("llm-d", "epp-builder"),
    ("vllm-router", "vllm-router-builder"),
    ("sglang-gateway", "sglang-gateway-builder"),
)
for subdir, builder in patch_map:
    bdir = os.path.join(base_patches_root, subdir)
    cdir = os.path.join(cand_patches_root, subdir)
    if not os.path.isdir(bdir) or not os.path.isdir(cdir):
        stages.add(builder)
        continue
    rc = subprocess.run(["diff", "-rq", "--", bdir, cdir], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode
    if rc != 0:
        stages.add(builder)


def split_dockerfile(path: str) -> tuple[list[str], dict[str, list[str]]]:
    preamble = []
    sections: dict[str, list[str]] = {}
    cur_name = None
    router_arg_re = re.compile(r"^ARG (?:LLMD_ROUTER_VERSION|VLLM_ROUTER_VERSION|SGLANG_GATEWAY_VERSION)=")
    from_re = re.compile(r"^\s*FROM\s+\S+(?:\s+AS\s+([A-Za-z0-9._-]+))?", re.IGNORECASE)
    with open(path, "r", encoding="utf-8") as f:
        for line in f:
            stripped = line.rstrip("\r\n")
            m = from_re.match(stripped)
            if m:
                cur_name = m.group(1) or "runtime"
                sections.setdefault(cur_name, []).append(line)
                continue
            if cur_name is None:
                if router_arg_re.match(stripped):
                    key = stripped.split("=", 1)[0]
                    preamble.append(f"{key}=__ROUTER_PIN__\n")
                else:
                    preamble.append(line)
            else:
                sections[cur_name].append(line)
    return preamble, sections


base_pre, base_sec = split_dockerfile(base_df)
cand_pre, cand_sec = split_dockerfile(cand_df)

if base_pre != cand_pre:
    # Check specific shared preamble ARGs before falling back to all builders.
    base_non_arg = [ln for ln in base_pre if not ln.startswith("ARG GO_VERSION=") and not ln.startswith("ARG RUST_VERSION=")]
    cand_non_arg = [ln for ln in cand_pre if not ln.startswith("ARG GO_VERSION=") and not ln.startswith("ARG RUST_VERSION=")]
    if base_non_arg != cand_non_arg:
        stages.update(ALL_BUILDERS)
    else:
        if [ln for ln in base_pre if ln.startswith("ARG GO_VERSION=")] != [ln for ln in cand_pre if ln.startswith("ARG GO_VERSION=")]:
            stages.add("epp-builder")
        if [ln for ln in base_pre if ln.startswith("ARG RUST_VERSION=")] != [ln for ln in cand_pre if ln.startswith("ARG RUST_VERSION=")]:
            stages.add("vllm-router-builder")
            stages.add("sglang-gateway-builder")

all_sec_names = set(base_sec.keys()) | set(cand_sec.keys())
for sec_name in all_sec_names:
    if base_sec.get(sec_name) != cand_sec.get(sec_name):
        if sec_name == "epp-builder":
            stages.add("epp-builder")
        elif sec_name == "vllm-router-builder":
            stages.add("vllm-router-builder")
        elif sec_name == "sglang-gateway-builder":
            stages.add("sglang-gateway-builder")
        elif sec_name == "rust-builder":
            stages.add("vllm-router-builder")
            stages.add("sglang-gateway-builder")
        else:
            stages.update(ALL_BUILDERS)

for b in ALL_BUILDERS:
    if b in stages:
        print(b)
PY
)"

if [[ -n "${CHANGED_STAGES}" ]]; then
  read -r -a BUILD_CMD <<< "${AUTOSYNC_ROUTER_BUILD_RUNNER:-docker buildx build}"
  while IFS= read -r target_stage; do
    [[ -n "${target_stage}" ]] || continue
    if ! "${BUILD_CMD[@]}" -f pack/llm-router/Dockerfile --platform linux/amd64 --target "${target_stage}" .; then
      printf 'FAIL: router builder target %s failed\n' "${target_stage}" >&2
      exit 1
    fi
  done <<<"${CHANGED_STAGES}"
fi

# 6. Two-Stage Image Publication Gate and value.go <-> settings.md alignment.
MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
IMAGE_META_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-router-images.XXXXXXXX")"
_CLEANUP_DIRS+=("${IMAGE_META_FILE}")

python3 - \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  > "${IMAGE_META_FILE}" <<'PY'
import re
import sys

base_vg_p, cand_vg_p, cand_sm_p = sys.argv[1:4]


def read(p: str) -> str:
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


def vg_image(txt: str, var_name: str) -> str:
    m = re.search(
        rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
        txt,
        re.DOTALL,
    )
    if not m:
        raise SystemExit(f"FAIL: missing {var_name} in value.go")
    return m.group(1).strip()


def sm_default(txt: str, key: str) -> str:
    m = re.search(rf'^\| `{re.escape(key)}`<br/>.*?<br/>Default: `([^`]+)`', txt, re.MULTILINE)
    if not m:
        raise SystemExit(f"FAIL: missing {key} Default row in settings.md")
    return m.group(1).strip()


base_vg = read(base_vg_p)
cand_vg = read(cand_vg_p)
cand_sm = read(cand_sm_p)

pairs = (
    ("ModelDeploymentRouterImage", "model-deployment-router-image", "ROUTER_IMG"),
    ("ModelDeploymentRouterProxyImage", "model-deployment-router-proxy-image", "PROXY_IMG"),
    ("ModelDeploymentRoutingSidecarImage", "model-deployment-routing-sidecar-image", "SIDECAR_IMG"),
)

changed = False
for var_name, key_name, out_name in pairs:
    cand_val = vg_image(cand_vg, var_name)
    base_val = vg_image(base_vg, var_name)
    doc_val = sm_default(cand_sm, key_name)
    if cand_val != doc_val:
        raise SystemExit(f"FAIL: value.go {var_name} ({cand_val}) disagrees with settings.md {key_name} Default ({doc_val})")
    if cand_val != base_val:
        changed = True
    print(f"{out_name}={cand_val}")

print(f"IMAGES_CHANGED={'true' if changed else 'false'}")
PY

IMAGES_CHANGED=""
ROUTER_IMG=""
PROXY_IMG=""
SIDECAR_IMG=""
while IFS='=' read -r k v; do
  case "${k}" in
    IMAGES_CHANGED) IMAGES_CHANGED="${v}" ;;
    ROUTER_IMG) ROUTER_IMG="${v}" ;;
    PROXY_IMG) PROXY_IMG="${v}" ;;
    SIDECAR_IMG) SIDECAR_IMG="${v}" ;;
  esac
done < "${IMAGE_META_FILE}"

if [[ "${IMAGES_CHANGED}" == "true" || -f "${MANIFESTS_JSON}" ]]; then
  autosync::assert_image_published "${MANIFESTS_JSON}" "${ROUTER_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${PROXY_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${SIDECAR_IMG}"
fi

# 7. Go settings tests and docs lint when Stage B files changed.
PROMOTED_FILES_CHANGED="false"
for rel in "pkg/worker/settings/value.go" "docs/reference/settings.md"; do
  if [[ -f "${BASELINE_DIR}/${rel}" && -f "${AUTOSYNC_CANDIDATE_ROOT}/${rel}" ]]; then
    if ! cmp -s "${BASELINE_DIR}/${rel}" "${AUTOSYNC_CANDIDATE_ROOT}/${rel}"; then
      PROMOTED_FILES_CHANGED="true"
      break
    fi
  fi
done

if [[ "${PROMOTED_FILES_CHANGED}" == "true" && "${AUTOSYNC_FAST_SELFTEST:-false}" != "true" ]]; then
  go test ./pkg/worker/settings/...
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint docs
  git -C "${AUTOSYNC_CANDIDATE_ROOT}" diff --exit-code
fi

printf 'OK: validate_router passed (images_changed=%s)\n' "${IMAGES_CHANGED}"
