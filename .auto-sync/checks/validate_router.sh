#!/usr/bin/env bash
# .auto-sync/checks/validate_router.sh - command.v1 verification gate for router-sync.
set -euo pipefail

SCRIPT_DIR="$(CDPATH="" cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
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
BASELINE_DIR="$(autosync::resolve_baseline_dir "${STAGE_DIR}")"
if [[ -z "${BASELINE_DIR}" || ! -d "${BASELINE_DIR}" ]]; then
  printf 'FAIL: LOOPWRIGHT_BASE_DIR or AUTOSYNC_STAGE_DIR/baseline must point to baseline directory\n' >&2
  exit 1
fi

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
CAND_SGL_ROUTER_VER="$(extract_df_arg "${CAND_DOCKERFILE}" "SGL_ROUTER_VERSION")"

BASE_LLMD_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "LLMD_ROUTER_VERSION")"
BASE_VLLM_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "VLLM_ROUTER_VERSION")"
BASE_SGLANG_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "SGLANG_GATEWAY_VERSION")"
BASE_SGL_ROUTER_VER="$(extract_df_arg "${BASE_DOCKERFILE}" "SGL_ROUTER_VERSION")"

# 2b. Enforce permanent freeze on legacy sglang-gateway (deleted upstream in PR #50732).
FROZEN_SGLANG_GATEWAY_VERSION="gateway-v0.3.1"
if [[ "${CAND_SGLANG_VER}" != "${BASE_SGLANG_VER}" ]] || [[ "${CAND_SGLANG_VER}" != "${FROZEN_SGLANG_GATEWAY_VERSION}" ]]; then
  printf 'FAIL: SGLANG_GATEWAY_VERSION is frozen at %s (upstream deleted sgl-model-gateway in PR #50732); candidate has %s (baseline %s)\n' \
    "${FROZEN_SGLANG_GATEWAY_VERSION}" "${CAND_SGLANG_VER}" "${BASE_SGLANG_VER}" >&2
  exit 1
fi

BASE_SGLANG_PATCHES="${BASELINE_DIR}/pack/llm-router/patches/sglang-gateway"
CAND_SGLANG_PATCHES="${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/sglang-gateway"
if [[ ! -d "${BASE_SGLANG_PATCHES}" || ! -d "${CAND_SGLANG_PATCHES}" ]] || \
   ! diff -rq -- "${BASE_SGLANG_PATCHES}" "${CAND_SGLANG_PATCHES}" >/dev/null 2>&1; then
  printf 'FAIL: pack/llm-router/patches/sglang-gateway/ is frozen and must not be modified\n' >&2
  exit 1
fi

# 3. Validate 0001-epd-routing.patch and 0004-discovery-readiness-and-deselect-removal.patch retirement invariants when router-preflight-report.json exists.
EPD_PATCH="${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/vllm-router/0001-epd-routing.patch"
DISCOVERY_PATCH="${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/vllm-router/0004-discovery-readiness-and-deselect-removal.patch"
PREFLIGHT_REPORT="$(autosync::resolve_staged_file "router-preflight-report.json" "${STAGE_DIR}")"
if [[ -z "${PREFLIGHT_REPORT}" || ! -f "${PREFLIGHT_REPORT}" ]]; then
  printf 'FAIL: missing preflight report (router-preflight-report.json)\n' >&2
  exit 1
fi
if ! RETIRE_INFO="$(python3 - "${PREFLIGHT_REPORT}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    rep = json.load(f)
if not isinstance(rep, dict):
    raise SystemExit(f"FAIL: {path} must be a JSON object")
stage = rep.get("stage")
if not isinstance(stage, str) or stage not in ("recipe_sync", "tag_promotion", "no_change"):
    raise SystemExit(
        f"FAIL: {path} stage must be one of recipe_sync, tag_promotion, no_change (got {stage!r})"
    )
retire_0001 = "true" if rep.get("retire_0001_epd_routing") is True else "false"
retire_0004 = "true" if rep.get("retire_0004_discovery_readiness") is True else "false"
target_vllm = str((rep.get("target_tags") or {}).get("vllm-router") or "")
print(f"{retire_0001}|{retire_0004}|{target_vllm}|{stage}")
PY
)"; then
  printf 'FAIL: unable to parse %s\n' "${PREFLIGHT_REPORT}" >&2
  exit 1
fi
IFS='|' read -r RETIRE_0001 RETIRE_0004 TARGET_VLLM_VER PREFLIGHT_STAGE <<<"${RETIRE_INFO}"
if [[ -z "${PREFLIGHT_STAGE}" ]]; then
  printf 'FAIL: empty stage in %s\n' "${PREFLIGHT_REPORT}" >&2
  exit 1
fi
if [[ "${RETIRE_0001}" == "false" && ! -f "${EPD_PATCH}" ]]; then
  printf 'FAIL: 0001-epd-routing.patch was deleted while retire_0001_epd_routing is false\n' >&2
  exit 1
fi
if [[ "${RETIRE_0001}" == "true" && ( -z "${TARGET_VLLM_VER}" || "${CAND_VLLM_VER}" == "${TARGET_VLLM_VER}" ) && -f "${EPD_PATCH}" ]]; then
  printf 'FAIL: 0001-epd-routing.patch must be retired (retire_0001_epd_routing=true); target vllm-router version %s contains 516f1cb\n' \
    "${TARGET_VLLM_VER:-${CAND_VLLM_VER:-unknown}}" >&2
  exit 1
fi
if [[ "${RETIRE_0004}" == "true" && ( -z "${TARGET_VLLM_VER}" || "${CAND_VLLM_VER}" == "${TARGET_VLLM_VER}" ) && -f "${DISCOVERY_PATCH}" ]]; then
  printf 'FAIL: 0004-discovery-readiness-and-deselect-removal.patch must be retired (retire_0004_discovery_readiness=true) when targeting vllm-router %s\n' \
    "${TARGET_VLLM_VER:-${CAND_VLLM_VER:-unknown}}" >&2
  exit 1
fi

# 4. Apply patches cleanly against staged upstream clones and verify struct ObserverRouter.
apply_staged_patches() {
  local name="$1"
  local stage_repo="$2"
  local target_ver="$3"
  local patch_dir="$4"
  shift 4
  local sparse_subpaths=("$@")

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
    if [[ "${AUTOSYNC_ALLOW_MISSING_STAGED_REPO:-false}" == "true" ]]; then
      if [[ "${name}" == "sglang-gateway" || "${name}" == "sgl-router" ]]; then
        if ! grep -q "struct ObserverRouter" "${patches[@]}"; then
          printf 'FAIL: struct ObserverRouter missing from %s patches\n' "${name}" >&2
          return 1
        fi
      fi
      return 0
    fi
    printf 'FAIL: staged upstream repo missing for %s: %s\n' "${name}" "${stage_repo}" >&2
    return 1
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

  if [[ "${is_promisor}" == "true" ]] && (( ${#sparse_subpaths[@]} > 0 )); then
    git -C "${stage_repo}" archive "${target_ver}" -- "${sparse_subpaths[@]}" | tar -x -C "${tmp_tree}"
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

  if [[ "${name}" == "sglang-gateway" ]]; then
    local observer_rs="${tmp_tree}/sgl-model-gateway/src/observer.rs"
    if [[ ! -f "${observer_rs}" ]] || ! grep -q "struct ObserverRouter" "${observer_rs}"; then
      printf 'FAIL: struct ObserverRouter missing in %s after applying sglang-gateway patches\n' \
        "sgl-model-gateway/src/observer.rs" >&2
      return 1
    fi
  elif [[ "${name}" == "sgl-router" ]]; then
    local observer_rs="${tmp_tree}/sgl-router/src/observer.rs"
    local lib_rs="${tmp_tree}/sgl-router/src/lib.rs"
    local app_rs="${tmp_tree}/sgl-router/src/server/app.rs"
    if [[ ! -f "${observer_rs}" ]] || ! grep -q "struct ObserverRouter" "${observer_rs}"; then
      printf 'FAIL: struct ObserverRouter missing in %s after applying sgl-router patches\n' \
        "sgl-router/src/observer.rs" >&2
      return 1
    fi
    if [[ ! -f "${lib_rs}" ]] || ! grep -q "pub mod observer;" "${lib_rs}"; then
      printf 'FAIL: pub mod observer; missing in %s after applying sgl-router patches\n' \
        "sgl-router/src/lib.rs" >&2
      return 1
    fi
    if [[ ! -f "${app_rs}" ]] || ! grep -q "/observer/endpoints" "${app_rs}"; then
      printf 'FAIL: /observer/endpoints missing in %s after applying sgl-router patches\n' \
        "sgl-router/src/server/app.rs" >&2
      return 1
    fi
  fi
}

apply_staged_patches "llm-d-router" "${STAGE_DIR}/llm-d-router" "${CAND_LLMD_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/llm-d"

apply_staged_patches "vllm-router" "${STAGE_DIR}/vllm-router" "${CAND_VLLM_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/vllm-router"

SGLANG_GW_STAGE="${STAGE_DIR}/sglang-gateway"
if [[ ! -d "${SGLANG_GW_STAGE}" && -d "${STAGE_DIR}/sglang" ]]; then
  SGLANG_GW_STAGE="${STAGE_DIR}/sglang"
fi
apply_staged_patches "sglang-gateway" "${SGLANG_GW_STAGE}" "${CAND_SGLANG_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/sglang-gateway" "sgl-model-gateway"

apply_staged_patches "sgl-router" "${STAGE_DIR}/sgl-router" "${CAND_SGL_ROUTER_VER}" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches/sgl-router" "sgl-router" "rust"

# 5. Detect modified router stages and run targeted docker buildx verification.
CHANGED_STAGES="$(python3 - \
  "${BASE_DOCKERFILE}" "${CAND_DOCKERFILE}" \
  "${BASELINE_DIR}/pack/llm-router/patches" "${AUTOSYNC_CANDIDATE_ROOT}/pack/llm-router/patches" \
  "${BASE_LLMD_VER}" "${CAND_LLMD_VER}" \
  "${BASE_VLLM_VER}" "${CAND_VLLM_VER}" \
  "${BASE_SGLANG_VER}" "${CAND_SGLANG_VER}" \
  "${BASE_SGL_ROUTER_VER}" "${CAND_SGL_ROUTER_VER}" <<'PY'
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
    base_sgl_router,
    cand_sgl_router,
) = sys.argv[1:13]

ALL_BUILDERS = (
    "epp-builder",
    "vllm-router-builder",
    "sglang-gateway-builder",
    "sgl-router-builder",
)
stages = set()

if cand_llmd != base_llmd:
    stages.add("epp-builder")
if cand_vllm != base_vllm:
    stages.add("vllm-router-builder")
if cand_sglang != base_sglang:
    stages.add("sglang-gateway-builder")
if cand_sgl_router != base_sgl_router:
    stages.add("sgl-router-builder")

patch_map = (
    ("llm-d", "epp-builder"),
    ("vllm-router", "vllm-router-builder"),
    ("sglang-gateway", "sglang-gateway-builder"),
    ("sgl-router", "sgl-router-builder"),
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
    router_arg_re = re.compile(
        r"^ARG (?:LLMD_ROUTER_VERSION|VLLM_ROUTER_VERSION|SGLANG_GATEWAY_VERSION|SGL_ROUTER_VERSION)="
    )
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

if base_sec.get("sglang-gateway-builder") != cand_sec.get("sglang-gateway-builder"):
    print(
        "FAIL: sglang-gateway-builder stage in pack/llm-router/Dockerfile is frozen and must not be modified",
        file=sys.stderr,
    )
    sys.exit(1)

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
            stages.add("sgl-router-builder")

all_sec_names = set(base_sec.keys()) | set(cand_sec.keys())
for sec_name in all_sec_names:
    if base_sec.get(sec_name) != cand_sec.get(sec_name):
        if sec_name == "epp-builder":
            stages.add("epp-builder")
        elif sec_name == "vllm-router-builder":
            stages.add("vllm-router-builder")
        elif sec_name == "sglang-gateway-builder":
            stages.add("sglang-gateway-builder")
        elif sec_name == "sgl-router-builder":
            stages.add("sgl-router-builder")
        elif sec_name == "rust-builder":
            stages.add("vllm-router-builder")
            stages.add("sglang-gateway-builder")
            stages.add("sgl-router-builder")
        else:
            stages.update(ALL_BUILDERS)

for b in ALL_BUILDERS:
    if b in stages:
        print(b)
PY
)"

if [[ -z "${PREFLIGHT_STAGE}" ]]; then
  printf 'FAIL: missing stage in %s\n' "${PREFLIGHT_REPORT}" >&2
  exit 1
fi

if [[ "${PREFLIGHT_STAGE}" == "tag_promotion" && -n "${CHANGED_STAGES}" ]]; then
  printf 'FAIL: Stage B tag_promotion must not modify pack/llm-router/Dockerfile or patches (changed stages: %s)\n' \
    "$(printf '%s' "${CHANGED_STAGES}" | tr '\n' ' ' | sed 's/ *$//')" >&2
  exit 1
fi

if [[ -n "${CHANGED_STAGES}" ]]; then
  read -r -a BUILD_CMD <<< "${AUTOSYNC_ROUTER_BUILD_RUNNER:-docker buildx build}"
  BUILD_LOG="$(mktemp "${TMPDIR:-/tmp}/autosync-router-build.XXXXXXXX")"
  _CLEANUP_DIRS+=("${BUILD_LOG}")
  while IFS= read -r target_stage; do
    [[ -n "${target_stage}" ]] || continue
    : > "${BUILD_LOG}"
    build_rc=0
    set +e
    "${BUILD_CMD[@]}" -f pack/llm-router/Dockerfile --platform linux/amd64 --target "${target_stage}" . >"${BUILD_LOG}" 2>&1
    build_rc=$?
    set -e
    cat "${BUILD_LOG}"
    if (( build_rc != 0 )); then
      err_tail="$(grep -E 'error:|\.go:[0-9]+|error\[E|FAIL' "${BUILD_LOG}" | tail -n 15 || true)"
      if [[ -z "${err_tail}" ]]; then
        err_tail="$(tail -n 15 "${BUILD_LOG}" || true)"
      fi
      first_err=""
      if [[ -n "${err_tail}" ]]; then
        printf '%s\n' "${err_tail}" >&2
        first_err="$(printf '%s\n' "${err_tail}" | sed -E 's/^#[0-9]+ [0-9.]+ //' | grep -m 1 -E '\S' || true)"
      fi
      if [[ -n "${first_err}" ]]; then
        printf 'FAIL: router builder target %s failed (%s)\n' "${target_stage}" "${first_err}" >&2
      else
        printf 'FAIL: router builder target %s failed\n' "${target_stage}" >&2
      fi
      exit 1
    fi
  done <<<"${CHANGED_STAGES}"
fi

# 6. Two-Stage Image Publication Gate and value.go <-> settings.md alignment.
MANIFESTS_JSON="$(autosync::resolve_staged_file "image-manifests.json" "${STAGE_DIR}")"
IMAGE_META_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-router-images.XXXXXXXX")"
_CLEANUP_DIRS+=("${IMAGE_META_FILE}")

python3 - \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${BASELINE_DIR}" \
  "${AUTOSYNC_CANDIDATE_ROOT}" \
  "${SCRIPT_DIR}" \
  > "${IMAGE_META_FILE}" <<'PY'
import re
import sys

base_vg_p, cand_vg_p, cand_sm_p, baseline_dir, cand_root, checks_dir = sys.argv[1:7]
sys.path.insert(0, checks_dir)
from setting_image_holds import (
    domain_holds,
    load_setting_image_holds,
    resolve_holds_file_for_validate,
    verify_router_holds,
)


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
    ("router", "ModelDeploymentRouterImage", "model-deployment-router-image", "ROUTER_IMG"),
    ("proxy", "ModelDeploymentRouterProxyImage", "model-deployment-router-proxy-image", "PROXY_IMG"),
    ("sidecar", "ModelDeploymentRoutingSidecarImage", "model-deployment-routing-sidecar-image", "SIDECAR_IMG"),
)

changed = False
cand_by_role = {}
base_by_role = {}
for role, var_name, key_name, out_name in pairs:
    cand_val = vg_image(cand_vg, var_name)
    base_val = vg_image(base_vg, var_name)
    doc_val = sm_default(cand_sm, key_name)
    if cand_val != doc_val:
        raise SystemExit(f"FAIL: value.go {var_name} ({cand_val}) disagrees with settings.md {key_name} Default ({doc_val})")
    if cand_val != base_val:
        changed = True
    cand_by_role[role] = cand_val
    base_by_role[role] = base_val
    print(f"{out_name}={cand_val}")

holds_path = resolve_holds_file_for_validate(baseline_dir, cand_root, "router")
holds_label = holds_path or ".auto-sync/setting-image-holds.json"
router_holds = domain_holds(load_setting_image_holds(holds_path), "router")
verify_router_holds(
    router_holds,
    cand_by_role["router"],
    cand_by_role["proxy"],
    cand_by_role["sidecar"],
    holds_label,
)
for role in ("router", "proxy", "sidecar"):
    if role in router_holds and cand_by_role[role] != base_by_role[role]:
        raise SystemExit(
            f"FAIL: router {role} image changed ({base_by_role[role]} -> {cand_by_role[role]}) while held by {holds_label}"
        )

print(f"IMAGES_CHANGED={'true' if changed else 'false'}")
print(f"HELD_ROUTER_KEYS={','.join(sorted(router_holds))}")
PY

IMAGES_CHANGED=""
HELD_ROUTER_KEYS=""
ROUTER_IMG=""
PROXY_IMG=""
SIDECAR_IMG=""
while IFS='=' read -r k v; do
  case "${k}" in
    IMAGES_CHANGED) IMAGES_CHANGED="${v}" ;;
    HELD_ROUTER_KEYS) HELD_ROUTER_KEYS="${v}" ;;
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

# 6b. Proposal schema and version parity (.auto-sync/proposals/router.json).
CAND_PROPOSAL="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/router.json"
python3 - "${CAND_PROPOSAL}" "${CAND_LLMD_VER}" "${CAND_VLLM_VER}" "${CAND_SGLANG_VER}" "${CAND_SGL_ROUTER_VER}" "${IMAGES_CHANGED}" "${HELD_ROUTER_KEYS}" <<'PY'
import json
import os
import sys

prop_p, llmd_v, vllm_v, sglang_v, sgl_router_v, images_changed_str, held_keys_csv = sys.argv[1:8]
held_keys = {k for k in held_keys_csv.split(",") if k}
if not os.path.isfile(prop_p):
    raise SystemExit(f"FAIL: missing proposal artifact: {prop_p}")
with open(prop_p, "r", encoding="utf-8") as f:
    prop = json.load(f)

if not isinstance(prop, dict) or prop.get("schema_version") != 1:
    raise SystemExit("FAIL: proposals/router.json must have schema_version == 1")
if not isinstance(prop.get("summary"), str) or not prop["summary"].strip():
    raise SystemExit("FAIL: proposals/router.json summary must be a non-empty string")
stage = prop.get("stage")
if stage not in ("aligned", "recipe_sync", "tag_promotion"):
    raise SystemExit(f"FAIL: proposals/router.json invalid stage: {stage!r}")
if not isinstance(prop.get("subscriptions"), list):
    raise SystemExit("FAIL: proposals/router.json subscriptions must be a JSON array")
versions = prop.get("versions")
if not isinstance(versions, dict):
    raise SystemExit("FAIL: proposals/router.json versions must be a JSON object")

allow_stage_b = (stage == "tag_promotion") or (images_changed_str == "true")
expected = {
    "llm-d-router": (llmd_v, not ({"router", "proxy", "sidecar"} <= held_keys)),
    "vllm-router": (vllm_v, "router" not in held_keys),
    "sglang-gateway": (sglang_v, "router" not in held_keys),
    "sgl-router": (sgl_router_v, "router" not in held_keys),
}
for uid, (exp_v, uid_allow_sb) in expected.items():
    got = versions.get(uid)
    if not isinstance(got, str) or not got.strip():
        raise SystemExit(f"FAIL: proposals/router.json versions.{uid} must be a non-empty string")
    if got == exp_v:
        continue
    if allow_stage_b and uid_allow_sb and got == f"{exp_v}+stage-b":
        continue
    raise SystemExit(f"FAIL: proposals/router.json versions.{uid} ({got!r}) != pinned ({exp_v!r})")
PY

# 6c. Validate Base Image build recommendations whenever pack/llm-router/** changed.
python3 -B "${SCRIPT_DIR}/base_image_builds.py" validate \
  "${BASELINE_DIR}" "${AUTOSYNC_CANDIDATE_ROOT}" "${CAND_PROPOSAL}" --domain router

# 7. Go settings/controller render tests and docs lint when Stage B files changed.
PROMOTED_FILES_CHANGED="false"
for rel in \
  "pkg/worker/settings/value.go" \
  "pkg/worker/settings/value_test.go" \
  "pkg/worker/controllers/worker/model_deployment_router_test.go" \
  "pkg/worker/controllers/worker/model_deployment_render_test.go" \
  "docs/reference/settings.md"; do
  if [[ -f "${BASELINE_DIR}/${rel}" && -f "${AUTOSYNC_CANDIDATE_ROOT}/${rel}" ]]; then
    if ! cmp -s "${BASELINE_DIR}/${rel}" "${AUTOSYNC_CANDIDATE_ROOT}/${rel}"; then
      PROMOTED_FILES_CHANGED="true"
      break
    fi
  fi
done

if [[ "${PROMOTED_FILES_CHANGED}" == "true" && "${AUTOSYNC_FAST_SELFTEST:-false}" != "true" ]]; then
  (
    set -e
    cd "${AUTOSYNC_CANDIDATE_ROOT}"
    autosync::go_test ./pkg/worker/settings/...
    if [[ -d "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/controllers/worker" ]]; then
      autosync::go_test ./pkg/worker/controllers/worker \
        -run '^(TestRenderModelDeploymentRouterObjects_ImageSources|TestRenderModelDeploymentRouterObjects_SerializedOutputIsPinnedToThePreSplitRender|TestRenderModelDeploymentPod_DecodeUsesRoutingSidecar|TestRenderModelDeploymentPod_DecodeUsesClassicSidecarBelowTheFloor)$'
    fi
  )
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint docs
  git -C "${AUTOSYNC_CANDIDATE_ROOT}" diff --exit-code
fi

autosync::write_check_out true "validate_router passed (images_changed=${IMAGES_CHANGED})"
printf 'OK: validate_router passed (images_changed=%s)\n' "${IMAGES_CHANGED}"
