#!/usr/bin/env bash
# .auto-sync/checks/validate_kv_transfer.sh - command.v1 verification for kv-transfer-sync.
set -euo pipefail

SCRIPT_DIR="$(CDPATH="" cd -P -- "$(dirname -- "${BASH_SOURCE[0]}")" && pwd -P)"
# Inline bootstrap: Loopwright copies gate.script alone to workDir/gate-script.sh.
if [[ ! -f "${SCRIPT_DIR}/lib.sh" && -f "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks/lib.sh" ]]; then
  SCRIPT_DIR="$(CDPATH="" cd -P -- "${LOOPWRIGHT_CANDIDATE_DIR:-.}/.auto-sync/checks" && pwd -P)"
fi
# shellcheck source=./lib.sh
# shellcheck disable=SC1091
source "${SCRIPT_DIR}/lib.sh"

autosync::bootstrap_env "${AUTOSYNC_STAGE_DIR:-}" "${LOOPWRIGHT_CANDIDATE_DIR:-$(pwd -P)}"
cd "${AUTOSYNC_CANDIDATE_ROOT}"

_CLEANUP_PATHS=()
cleanup_on_exit() {
  local p
  for p in "${_CLEANUP_PATHS[@]+"${_CLEANUP_PATHS[@]}"}"; do
    [[ -n "${p}" && -e "${p}" ]] && rm -rf "${p}"
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
KV_SETTING_KEYS='kv-cache-backend-image|model-deployment-modelexpress-server-image'
assert_value_go_only_keys \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${KV_SETTING_KEYS}"

assert_settings_md_only_keys \
  "${BASELINE_DIR}/docs/reference/settings.md" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${KV_SETTING_KEYS}"

assert_values_yaml_only_model_transfer_images \
  "${BASELINE_DIR}/deploy/gpustack-operator/chart/values.yaml" \
  "${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/values.yaml"

read_dockerfile_arg_from_file() {
  local df_path="$1"
  local arg_name="$2"
  local required="${3:-true}"
  local args=()
  if [[ "${required}" != "true" ]]; then
    args+=("--optional")
  fi
  args+=("${df_path}" "${arg_name}" "${df_path}")
  python3 "${SCRIPT_DIR}/dockerfile_arg.py" "${args[@]}"
}

# 2. Five-block MOONCAKE_VERSION case guard in pack/mirrored-mooncake/Dockerfile.
MOONCAKE_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/mirrored-mooncake/Dockerfile"
if [[ ! -f "${MOONCAKE_DF}" ]]; then
  printf 'FAIL: missing %s\n' "${MOONCAKE_DF}" >&2
  exit 1
fi

python3 - "${MOONCAKE_DF}" "${AUTOSYNC_CANDIDATE_ROOT}" <<'PY'
import fnmatch
import os
import re
import sys

df_path, cand_root = sys.argv[1:3]
sys.path.insert(0, os.path.join(cand_root, ".auto-sync", "checks"))
from dockerfile_arg import read_dockerfile_arg

with open(df_path, "r", encoding="utf-8") as f:
    text = f.read()

pinned = read_dockerfile_arg(text, "MOONCAKE_VERSION", df_path)

lines = text.splitlines(keepends=True)
release_guards = []
context_blocks = []
i = 0
while i < len(lines):
    if re.match(r'^\s*case\s+"\$\{MOONCAKE_VERSION\}"\s+in\b', lines[i]):
        start_line = i + 1
        buf = [lines[i]]
        i += 1
        while i < len(lines) and not re.match(r'^\s*esac\b', lines[i]):
            buf.append(lines[i])
            i += 1
        if i >= len(lines):
            print(f"FAIL: unterminated case block at line {start_line}", file=sys.stderr)
            sys.exit(1)
        buf.append(lines[i])
        snippet = "".join(buf)
        if "exit 1" in snippet and "unrecognized release" in snippet:
            release_guards.append((start_line, snippet))
        if "context_manager.cpp" in snippet:
            context_blocks.append((start_line, snippet))
    i += 1

if len(release_guards) < 4:
    print(
        f"FAIL: expected at least 4 MOONCAKE_VERSION release-guard case blocks, found {len(release_guards)}",
        file=sys.stderr,
    )
    sys.exit(1)

if len(context_blocks) < 1:
    print(
        "FAIL: expected 5th MOONCAKE_VERSION context_manager.cpp case block in pack/mirrored-mooncake/Dockerfile",
        file=sys.stderr,
    )
    sys.exit(1)


def parse_case_arms(snippet: str, start_line: int) -> list[tuple[list[str], list[str]]]:
    arms: list[tuple[list[str], list[str]]] = []
    in_arm = False
    cur_pats: list[str] = []
    cur_body: list[str] = []
    for raw_line in snippet.splitlines()[1:-1]:
        st = raw_line.strip()
        if not st or st.startswith("#"):
            continue
        if not in_arm:
            if ")" not in st:
                raise SystemExit(
                    f"FAIL: unexpected shell syntax inside case block at line {start_line}: {st!r}"
                )
            pat_raw, rest = st.split(")", 1)
            if any(tok in pat_raw for tok in ("$(", "`", ";", "&")):
                raise SystemExit(
                    f"FAIL: unsafe shell construct in case arm at line {start_line}: {pat_raw!r}; "
                    "validate_kv_transfer.sh's static case-arm parser only supports simple shell globs "
                    "([A-Za-z0-9.*?_-]) for sandbox safety; write Dockerfile arms as simple |-separated globs "
                    "(or extend parse_case_arms)"
                )
            pats: list[str] = []
            for p in pat_raw.split("|"):
                p = p.strip()
                if (p.startswith('"') and p.endswith('"')) or (p.startswith("'") and p.endswith("'")):
                    p = p[1:-1]
                if not p or not re.fullmatch(r"[A-Za-z0-9.*?_-]+", p):
                    raise SystemExit(
                        f"FAIL: unsupported glob pattern {p!r} in case arm at line {start_line}; "
                        "validate_kv_transfer.sh's static case-arm parser only supports simple shell globs "
                        "([A-Za-z0-9.*?_-]) for sandbox safety; write Dockerfile arms as simple |-separated globs "
                        "(or extend parse_case_arms)"
                    )
                pats.append(p)
            cur_pats = pats
            cur_body = []
            rest = rest.strip()
            if rest.endswith(";;"):
                stmt = rest[:-2].strip()
                if stmt:
                    cur_body.append(stmt)
                arms.append((cur_pats, cur_body))
                in_arm = False
            else:
                if rest:
                    cur_body.append(rest)
                in_arm = True
        else:
            if st.endswith(";;"):
                stmt = st[:-2].strip()
                if stmt:
                    cur_body.append(stmt)
                arms.append((cur_pats, cur_body))
                in_arm = False
            else:
                cur_body.append(st)
    if in_arm:
        raise SystemExit(f"FAIL: unterminated case arm (;;) at line {start_line}")
    return arms


for start_line, snippet in release_guards:
    arms = parse_case_arms(snippet, start_line)
    matched_arm = None
    for pats, body in arms:
        if any(fnmatch.fnmatchcase(pinned, pat) for pat in pats):
            matched_arm = (pats, body)
            break
    if matched_arm is None:
        print(
            f'FAIL: MOONCAKE_VERSION="{pinned}" did not match any arm in release-guard case block at line {start_line}',
            file=sys.stderr,
        )
        sys.exit(1)
    m_pats, m_body = matched_arm
    if m_pats == ["*"] or any(re.search(r"\bexit\s+[1-9]\d*\b", ln) for ln in m_body):
        print(
            f'FAIL: MOONCAKE_VERSION="{pinned}" rejected by release-guard case block at line {start_line}',
            file=sys.stderr,
        )
        sys.exit(1)

for start_line, snippet in context_blocks:
    arms = parse_case_arms(snippet, start_line)
    explicit_patterns = [pat for pats, _ in arms for pat in pats if pat != "*"]
    if not explicit_patterns:
        print(
            f"FAIL: context_manager.cpp case block at line {start_line} has no explicit version arms",
            file=sys.stderr,
        )
        sys.exit(1)
    if not any(fnmatch.fnmatchcase(pinned, pat) for pat in explicit_patterns):
        print(
            f'FAIL: MOONCAKE_VERSION="{pinned}" is not handled by an explicit arm in context_manager.cpp case block at line {start_line}',
            file=sys.stderr,
        )
        sys.exit(1)
PY

# 3. Byte-level HIXL parity with gpustack/runner & patch dry-run against staged hixl.
KV_REPORT="${STAGE_DIR}/kv-transfer-preflight-report.json"
if [[ ! -f "${KV_REPORT}" ]]; then
  printf 'FAIL: missing preflight report: %s\n' "${KV_REPORT}" >&2
  exit 1
fi

if ! REPORT_META="$(python3 - "${KV_REPORT}" <<'PY'
import json
import sys

path = sys.argv[1]
with open(path, "r", encoding="utf-8") as f:
    rep = json.load(f)
if not isinstance(rep, dict):
    raise SystemExit(f"FAIL: {path} must be a JSON object")
val = rep.get("enforce_runner_hixl_parity", True)
if not isinstance(val, bool):
    raise SystemExit(f"FAIL: {path} enforce_runner_hixl_parity must be a boolean")
stage = rep.get("stage")
if not isinstance(stage, str) or stage not in ("recipe_sync", "tag_promotion", "no_change"):
    raise SystemExit(
        f"FAIL: {path} stage must be one of recipe_sync, tag_promotion, no_change (got {stage!r})"
    )
raw_files = rep.get("stage_a_recipe_files")
if (
    not isinstance(raw_files, list)
    or not raw_files
    or not all(
        isinstance(p, str)
        and (sp := p.strip())
        and sp.startswith("pack/")
        and not sp.startswith("/")
        and ".." not in sp.split("/")
        for p in raw_files
    )
):
    raise SystemExit(
        f"FAIL: {path} stage_a_recipe_files must be a non-empty list of pack/-scoped relative paths"
    )
recipe_files = [p.strip() for p in raw_files]
print(f"{'true' if val else 'false'}|{stage}")
for rf in recipe_files:
    print(rf)
PY
)"; then
  printf 'FAIL: unable to parse %s\n' "${KV_REPORT}" >&2
  exit 1
fi

REPORT_HEADER="${REPORT_META%%$'\n'*}"
IFS='|' read -r ENFORCE_RUNNER_PARITY PREFLIGHT_STAGE <<<"${REPORT_HEADER}"
if [[ -z "${PREFLIGHT_STAGE}" ]]; then
  printf 'FAIL: empty stage in %s\n' "${KV_REPORT}" >&2
  exit 1
fi
STAGE_A_RECIPE_FILES_LIST="${REPORT_META#*$'\n'}"

if [[ "${PREFLIGHT_STAGE}" == "tag_promotion" ]]; then
  while IFS= read -r recipe_rel; do
    [[ -n "${recipe_rel}" ]] || continue
    base_f="${BASELINE_DIR}/${recipe_rel}"
    cand_f="${AUTOSYNC_CANDIDATE_ROOT}/${recipe_rel}"
    if [[ -f "${base_f}" && ! -f "${cand_f}" ]]; then
      printf 'FAIL: Stage B tag_promotion must not delete Stage A recipe file %s\n' "${recipe_rel}" >&2
      exit 1
    fi
    if [[ -f "${base_f}" && -f "${cand_f}" ]]; then
      if ! cmp -s "${base_f}" "${cand_f}"; then
        printf 'FAIL: Stage B tag_promotion must not modify Stage A recipe file %s\n' "${recipe_rel}" >&2
        exit 1
      fi
    fi
  done <<<"${STAGE_A_RECIPE_FILES_LIST}"
fi

MT_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/model-transfer/Dockerfile"
HIXL_PATCH="${AUTOSYNC_CANDIDATE_ROOT}/pack/model-transfer/patches/hixl/001_channel_generation.patch"
CAND_HIXL_COMMIT="$(read_dockerfile_arg_from_file "${MT_DF}" "HIXL_COMMIT")"

# Stage B tag_promotion leaves pack/model-transfer/** untouched, so deferred gpustack/runner drift waits for Stage A.
if [[ "${ENFORCE_RUNNER_PARITY}" == "true" && "${PREFLIGHT_STAGE}" != "tag_promotion" && -d "${STAGE_DIR}/gpustack-runner" ]]; then
  RUNNER_CANN_DF="${STAGE_DIR}/gpustack-runner/pack/cann/Dockerfile.vllm"
  RUNNER_PATCH="${STAGE_DIR}/gpustack-runner/pack/cann/patches/hixl/001_channel_generation.patch"
  if [[ -f "${RUNNER_CANN_DF}" ]]; then
    RUNNER_HIXL_COMMIT="$(read_dockerfile_arg_from_file "${RUNNER_CANN_DF}" "VLLM_HIXL_COMMIT" "false")"
    if [[ -n "${RUNNER_HIXL_COMMIT}" && "${CAND_HIXL_COMMIT}" != "${RUNNER_HIXL_COMMIT}" ]]; then
      printf 'FAIL: HIXL_COMMIT (%s) != gpustack/runner VLLM_HIXL_COMMIT (%s)\n' \
        "${CAND_HIXL_COMMIT}" "${RUNNER_HIXL_COMMIT}" >&2
      exit 1
    fi
  fi
  if [[ -f "${RUNNER_PATCH}" ]]; then
    if ! cmp -s "${HIXL_PATCH}" "${RUNNER_PATCH}"; then
      printf 'FAIL: %s is not byte-identical to gpustack/runner %s\n' \
        "pack/model-transfer/patches/hixl/001_channel_generation.patch" \
        "pack/cann/patches/hixl/001_channel_generation.patch" >&2
      exit 1
    fi
  fi
fi

if [[ -d "${STAGE_DIR}/hixl" ]]; then
  HIXL_TMP="$(mktemp -d "${TMPDIR:-/tmp}/autosync-hixl-check.XXXXXXXX")"
  _CLEANUP_PATHS+=("${HIXL_TMP}")
  if ! cp -R "${STAGE_DIR}/hixl/." "${HIXL_TMP}/"; then
    rm -rf "${HIXL_TMP}"
    printf 'FAIL: unable to copy staged hixl directory\n' >&2
    exit 1
  fi
  if [[ -d "${HIXL_TMP}/.git" ]]; then
    if ! git -C "${HIXL_TMP}" cat-file -e "${CAND_HIXL_COMMIT}^{commit}" 2>/dev/null; then
      rm -rf "${HIXL_TMP}"
      printf 'FAIL: HIXL_COMMIT %s not present in staged hixl tree\n' "${CAND_HIXL_COMMIT}" >&2
      exit 1
    fi
    checkout_err=""
    if ! checkout_err="$(
      GIT_LFS_SKIP_SMUDGE=1 git -C "${HIXL_TMP}" \
        -c filter.lfs.smudge= \
        -c filter.lfs.process= \
        -c filter.lfs.clean= \
        -c filter.lfs.required=false \
        checkout -f -q --detach "${CAND_HIXL_COMMIT}" 2>&1
    )"; then
      rm -rf "${HIXL_TMP}"
      printf 'FAIL: unable to checkout HIXL_COMMIT %s in staged hixl tree: %s\n' \
        "${CAND_HIXL_COMMIT}" "${checkout_err}" >&2
      exit 1
    fi
  fi
  if ! (cd "${HIXL_TMP}" && patch --dry-run --batch --fuzz=0 -p1 < "${HIXL_PATCH}" >/dev/null); then
    rm -rf "${HIXL_TMP}"
    printf 'FAIL: HIXL patch --dry-run failed against %s at %s\n' "${STAGE_DIR}/hixl" "${CAND_HIXL_COMMIT}" >&2
    exit 1
  fi
  rm -rf "${HIXL_TMP}"
fi

# 3b. NIXL_VERSION PyPI wheel parity and NIXL_ROCM_VERSION <-> pyproject.toml alignment.
BASE_MT_DF="${BASELINE_DIR}/pack/model-transfer/Dockerfile"
if [[ ! -f "${BASE_MT_DF}" || ! -f "${MT_DF}" ]]; then
  printf 'FAIL: missing baseline or candidate pack/model-transfer/Dockerfile\n' >&2
  exit 1
fi

python3 - "${BASE_MT_DF}" "${MT_DF}" "${STAGE_DIR}" "${AUTOSYNC_CANDIDATE_ROOT}" <<'PY'
import json
import os
import re
import subprocess
import sys

base_df_p, cand_df_p, stage_dir, cand_root = sys.argv[1:5]
sys.path.insert(0, os.path.join(cand_root, ".auto-sync", "checks"))
from dockerfile_arg import read_dockerfile_arg


def read_text(p: str) -> str:
    with open(p, "r", encoding="utf-8") as f:
        return f.read()


base_df = read_text(base_df_p)
cand_df = read_text(cand_df_p)

base_nixl_ver = read_dockerfile_arg(base_df, "NIXL_VERSION", base_df_p)
cand_nixl_ver = read_dockerfile_arg(cand_df, "NIXL_VERSION", cand_df_p)
base_nixl_rocm_ref = read_dockerfile_arg(base_df, "NIXL_ROCM_REF", base_df_p)
cand_nixl_rocm_ref = read_dockerfile_arg(cand_df, "NIXL_ROCM_REF", cand_df_p)
cand_nixl_rocm_ver = read_dockerfile_arg(cand_df, "NIXL_ROCM_VERSION", cand_df_p)
base_ucx_rocm_ref = read_dockerfile_arg(base_df, "NIXL_ROCM_UCX_REF", base_df_p)
cand_ucx_rocm_ref = read_dockerfile_arg(cand_df, "NIXL_ROCM_UCX_REF", cand_df_p)

SEMVER_TAG_RE = re.compile(r"^v[0-9]+\.[0-9]+\.[0-9]+$")
if not SEMVER_TAG_RE.fullmatch(cand_nixl_rocm_ref):
    raise SystemExit(
        f"FAIL: NIXL_ROCM_REF must be a stable SemVer release tag vX.Y.Z, got {cand_nixl_rocm_ref!r}"
    )
if not SEMVER_TAG_RE.fullmatch(cand_ucx_rocm_ref):
    raise SystemExit(
        f"FAIL: NIXL_ROCM_UCX_REF must be a stable SemVer release tag vX.Y.Z, got {cand_ucx_rocm_ref!r}"
    )
if cand_nixl_rocm_ver != cand_nixl_ver:
    raise SystemExit(
        f"FAIL: NIXL_ROCM_VERSION ({cand_nixl_rocm_ver!r}) must match CUDA NIXL_VERSION ({cand_nixl_ver!r})"
    )
if cand_nixl_rocm_ref != f"v{cand_nixl_rocm_ver}":
    raise SystemExit(
        f"FAIL: NIXL_ROCM_REF ({cand_nixl_rocm_ref!r}) must equal 'v' + NIXL_ROCM_VERSION ({f'v{cand_nixl_rocm_ver}'!r})"
    )

report_path = os.path.join(stage_dir, "kv-transfer-preflight-report.json")
verified_nixl: set[str] = set()
verified_ucx: set[str] = set()
if os.path.isfile(report_path):
    with open(report_path, "r", encoding="utf-8") as f:
        rep = json.load(f)
    nixl_sec = rep.get("nixl_pypi")
    if isinstance(nixl_sec, dict):
        sel = nixl_sec.get("selected_version")
        if isinstance(sel, str) and sel.strip():
            verified_nixl.add(sel.strip())
        for rv in nixl_sec.get("recent_versions") or []:
            if isinstance(rv, str) and rv.strip():
                verified_nixl.add(rv.strip())
    elif isinstance(nixl_sec, str) and nixl_sec.strip():
        verified_nixl.add(nixl_sec.strip())
    for rv in rep.get("nixl_pypi_recent") or []:
        if isinstance(rv, str) and rv.strip():
            verified_nixl.add(rv.strip())
    ucx_sec = rep.get("ucx_rocm")
    if isinstance(ucx_sec, dict):
        ucx_target = ucx_sec.get("target_ref")
        if isinstance(ucx_target, str) and ucx_target.strip():
            verified_ucx.add(ucx_target.strip())
        for rv in ucx_sec.get("recent_versions") or []:
            if isinstance(rv, str) and rv.strip():
                verified_ucx.add(rv.strip())
    elif isinstance(ucx_sec, str) and ucx_sec.strip():
        verified_ucx.add(ucx_sec.strip())
    for rv in rep.get("ucx_rocm_recent") or []:
        if isinstance(rv, str) and rv.strip():
            verified_ucx.add(rv.strip())

if cand_nixl_ver != base_nixl_ver:
    if not verified_nixl:
        raise SystemExit(
            f"FAIL: NIXL_VERSION changed ({base_nixl_ver} -> {cand_nixl_ver}) without wheel-verified nixl_pypi entries in kv-transfer-preflight-report.json"
        )
    if cand_nixl_ver not in verified_nixl:
        raise SystemExit(
            f"FAIL: NIXL_VERSION={cand_nixl_ver!r} is not in wheel-verified PyPI set {sorted(verified_nixl)}"
        )

if cand_ucx_rocm_ref != base_ucx_rocm_ref:
    if not verified_ucx:
        raise SystemExit(
            f"FAIL: NIXL_ROCM_UCX_REF changed ({base_ucx_rocm_ref} -> {cand_ucx_rocm_ref}) without verified ucx_rocm entries in kv-transfer-preflight-report.json"
        )
    if cand_ucx_rocm_ref not in verified_ucx:
        raise SystemExit(
            f"FAIL: NIXL_ROCM_UCX_REF={cand_ucx_rocm_ref!r} is not in verified UCX release set {sorted(verified_ucx)}"
        )

nixl_repo = ""
for cand_dir in (os.path.join(stage_dir, "nixl-rocm"), os.path.join(stage_dir, "nixl")):
    if os.path.isdir(os.path.join(cand_dir, ".git")):
        nixl_repo = cand_dir
        break

pyproject_raw = ""
if nixl_repo:
    obj_spec = f"refs/tags/{cand_nixl_rocm_ref}:pyproject.toml"
    has_obj = subprocess.run(
        ["git", "-C", nixl_repo, "cat-file", "-e", obj_spec],
        stdout=subprocess.DEVNULL,
        stderr=subprocess.DEVNULL,
    ).returncode == 0
    if not has_obj:
        raise SystemExit(
            f"FAIL: NIXL_ROCM_REF {cand_nixl_rocm_ref} (refs/tags/{cand_nixl_rocm_ref}:pyproject.toml) not found in staged nixl repository {nixl_repo}"
        )
    pyproject_raw = subprocess.check_output(
        ["git", "-C", nixl_repo, "show", obj_spec],
        text=True,
    )
elif os.path.isfile(os.path.join(stage_dir, "nixl-rocm", "pyproject.toml")):
    pyproject_raw = read_text(os.path.join(stage_dir, "nixl-rocm", "pyproject.toml"))
elif cand_nixl_rocm_ref != base_nixl_rocm_ref:
    raise SystemExit(
        f"FAIL: NIXL_ROCM_REF changed ({base_nixl_rocm_ref} -> {cand_nixl_rocm_ref}) "
        f"without staged nixl-rocm repository or pyproject.toml in {stage_dir}"
    )

if pyproject_raw:
    proj_ver = ""
    try:
        import tomllib
        doc = tomllib.loads(pyproject_raw)
        proj_ver = str((doc.get("project") or {}).get("version") or "").strip()
    except ImportError:
        in_proj = False
        for raw_ln in pyproject_raw.splitlines():
            st = raw_ln.strip()
            if st.startswith("["):
                in_proj = st == "[project]"
                continue
            if in_proj:
                m_v = re.match(r'^version\s*=\s*["\']([^"\']+)["\']', st)
                if m_v:
                    proj_ver = m_v.group(1).strip()
                    break
    if proj_ver != cand_nixl_rocm_ver:
        raise SystemExit(
            f"FAIL: NIXL_ROCM_VERSION ({cand_nixl_rocm_ver!r}) != pyproject.toml [project].version ({proj_ver!r}) at {cand_nixl_rocm_ref}"
        )
PY

# 4. ModelExpress SemVer tag, Cargo.toml version, and --locked builder verification.
BASE_MX_DF="${BASELINE_DIR}/pack/mirrored-modelexpress-server/Dockerfile"
CAND_MX_DF="${AUTOSYNC_CANDIDATE_ROOT}/pack/mirrored-modelexpress-server/Dockerfile"
if [[ -f "${CAND_MX_DF}" ]]; then
  BASE_MX_VERSION=""
  if [[ -f "${BASE_MX_DF}" ]]; then
    BASE_MX_VERSION="$(read_dockerfile_arg_from_file "${BASE_MX_DF}" "MODELEXPRESS_VERSION" "false")"
  fi
  CAND_MX_VERSION="$(read_dockerfile_arg_from_file "${CAND_MX_DF}" "MODELEXPRESS_VERSION")"
  if [[ ! "${CAND_MX_VERSION}" =~ ^v[0-9]+\.[0-9]+\.[0-9]+$ ]]; then
    printf 'FAIL: MODELEXPRESS_VERSION must be a SemVer tag vX.Y.Z, got %q\n' "${CAND_MX_VERSION}" >&2
    exit 1
  fi

  python3 - "${STAGE_DIR}" "${BASE_MX_VERSION}" "${CAND_MX_VERSION}" <<'PY'
import os
import re
import subprocess
import sys

stage_dir, base_ver, cand_ver = sys.argv[1:4]
mx_stage = os.path.join(stage_dir, "modelexpress")


def parse_ws_version(cargo_txt: str) -> str:
    try:
        import tomllib
        doc = tomllib.loads(cargo_txt)
        ws = str(((doc.get("workspace") or {}).get("package") or {}).get("version") or "").strip()
        if ws:
            return ws
        return str((doc.get("package") or {}).get("version") or "").strip()
    except ImportError:
        section = ""
        ws = ""
        pkg = ""
        for raw_ln in cargo_txt.splitlines():
            st = raw_ln.strip()
            if not st or st.startswith("#"):
                continue
            if st.startswith("["):
                section = st
                continue
            m_v = re.match(r'^version\s*=\s*["\']([^"\']+)["\']', st)
            if m_v:
                if section == "[workspace.package]":
                    ws = m_v.group(1).strip()
                elif section == "[package]":
                    pkg = m_v.group(1).strip()
        return ws or pkg


if not os.path.isdir(mx_stage):
    raise SystemExit(f"FAIL: staged modelexpress directory {mx_stage} not found")

cargo_txt = ""
if os.path.isdir(os.path.join(mx_stage, ".git")):
    obj_spec = f"refs/tags/{cand_ver}:Cargo.toml"
    has_obj = (
        subprocess.run(
            ["git", "-C", mx_stage, "cat-file", "-e", obj_spec],
            stdout=subprocess.DEVNULL,
            stderr=subprocess.DEVNULL,
        ).returncode
        == 0
    )
    if not has_obj:
        raise SystemExit(
            f"FAIL: MODELEXPRESS_VERSION {cand_ver} (Cargo.toml) not found in staged modelexpress repository {mx_stage}"
        )
    cargo_txt = subprocess.check_output(["git", "-C", mx_stage, "show", obj_spec], text=True)
elif os.path.isfile(os.path.join(mx_stage, "Cargo.toml")):
    with open(os.path.join(mx_stage, "Cargo.toml"), "r", encoding="utf-8") as f:
        cargo_txt = f.read()
else:
    raise SystemExit(
        f"FAIL: staged modelexpress directory {mx_stage} is missing Cargo.toml and refs/tags/{cand_ver}:Cargo.toml"
    )

ws_ver = parse_ws_version(cargo_txt)
expected = cand_ver.lstrip("v")
if not ws_ver or ws_ver != expected:
    raise SystemExit(
        f"FAIL: MODELEXPRESS_VERSION {cand_ver} ({expected!r}) != staged Cargo.toml [workspace.package].version ({ws_ver!r})"
    )
PY

  if [[ -f "${BASE_MX_DF}" && "${BASE_MX_VERSION}" != "${CAND_MX_VERSION}" ]]; then
    BUILD_CMD="${AUTOSYNC_DOCKER_BUILD_CMD:-docker buildx build -f pack/mirrored-modelexpress-server/Dockerfile --target builder --platform linux/amd64 pack/mirrored-modelexpress-server}"
    if ! bash -c "${BUILD_CMD}"; then
      printf 'FAIL: ModelExpress builder verification failed\n' >&2
      exit 1
    fi
  fi
fi

# 5. Two-Stage Image Publication Gate and value.go <-> settings.md alignment.
MANIFESTS_JSON="${STAGE_DIR}/image-manifests.json"
IMAGE_META_FILE="$(mktemp "${TMPDIR:-/tmp}/autosync-kv-images.XXXXXXXX")"
_CLEANUP_PATHS+=("${IMAGE_META_FILE}")
python3 - \
  "${BASELINE_DIR}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/settings/value.go" \
  "${AUTOSYNC_CANDIDATE_ROOT}/docs/reference/settings.md" \
  "${BASELINE_DIR}/deploy/gpustack-operator/chart/values.yaml" \
  "${AUTOSYNC_CANDIDATE_ROOT}/deploy/gpustack-operator/chart/values.yaml" \
  "${BASELINE_DIR}" \
  "${AUTOSYNC_CANDIDATE_ROOT}" \
  "${SCRIPT_DIR}" \
  > "${IMAGE_META_FILE}" <<'PY'
import os
import re
import sys

(
    base_vg_p,
    cand_vg_p,
    cand_sm_p,
    base_vy_p,
    cand_vy_p,
    baseline_dir,
    cand_root,
    checks_dir,
) = sys.argv[1:9]
sys.path.insert(0, checks_dir)
from setting_image_holds import (
    domain_holds,
    load_setting_image_holds,
    resolve_holds_file_for_validate,
    verify_kv_transfer_holds,
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


def vy_mt_images(txt: str) -> dict[str, str]:
    out = {}
    in_mt = False
    in_images = False
    for raw in txt.splitlines():
        if raw.strip() == "" or raw.lstrip().startswith("#"):
            continue
        if not raw.startswith(" "):
            in_mt = raw == "modelTransfer:" or raw.startswith("modelTransfer: ")
            in_images = False
        elif in_mt and re.match(r"^  [A-Za-z0-9_-]+:", raw):
            in_images = raw == "  images:" or raw.startswith("  images: ")
        elif in_mt and in_images:
            m = re.match(r'^    (nvidia|amd|ascend):\s*(?:"([^"]+)"|\'([^\']+)\'|([^\s#]+))', raw)
            if m:
                out[m.group(1)] = (m.group(2) or m.group(3) or m.group(4)).strip()
    return out


base_vg = read(base_vg_p)
cand_vg = read(cand_vg_p)
cand_sm = read(cand_sm_p)
base_vy = read(base_vy_p)
cand_vy = read(cand_vy_p)

kv_img = vg_image(cand_vg, "KVCacheBackendImage")
mx_img = vg_image(cand_vg, "ModelDeploymentModelExpressServerImage")
if kv_img != sm_default(cand_sm, "kv-cache-backend-image"):
    raise SystemExit("FAIL: value.go KVCacheBackendImage disagrees with settings.md kv-cache-backend-image Default")
if mx_img != sm_default(cand_sm, "model-deployment-modelexpress-server-image"):
    raise SystemExit("FAIL: value.go ModelDeploymentModelExpressServerImage disagrees with settings.md model-deployment-modelexpress-server-image Default")

cand_rm_p = os.path.join(os.path.dirname(os.path.dirname(cand_sm_p)), "modules", "model-delivery", "remote.md")
if not os.path.isfile(cand_rm_p):
    raise SystemExit("FAIL: missing docs/modules/model-delivery/remote.md")
rm_txt = read(cand_rm_p)
m_rm = re.search(
    r'name:\s*model-deployment-modelexpress-server-image\b.*?\n\s*value:\s*([^\s#]+)',
    rm_txt,
    re.DOTALL,
)
if not m_rm:
    raise SystemExit(
        "FAIL: missing model-deployment-modelexpress-server-image value in docs/modules/model-delivery/remote.md"
    )
if m_rm.group(1).strip() != mx_img:
    raise SystemExit(
        f"FAIL: value.go ModelDeploymentModelExpressServerImage ({mx_img}) disagrees with "
        f"docs/modules/model-delivery/remote.md ({m_rm.group(1).strip()})"
    )

base_kv = vg_image(base_vg, "KVCacheBackendImage")
base_mx = vg_image(base_vg, "ModelDeploymentModelExpressServerImage")

base_mt = vy_mt_images(base_vy)
cand_mt = vy_mt_images(cand_vy)
for k in ("nvidia", "amd", "ascend"):
    if k not in cand_mt:
        raise SystemExit(f"FAIL: missing modelTransfer.images.{k} in values.yaml")

images_changed = (
    kv_img != base_kv
    or mx_img != base_mx
    or cand_mt != base_mt
)

holds_path = resolve_holds_file_for_validate(baseline_dir, cand_root, "kv-transfer")
holds_label = holds_path or ".auto-sync/setting-image-holds.json"
kv_holds = domain_holds(load_setting_image_holds(holds_path), "kv-transfer")
verify_kv_transfer_holds(kv_holds, kv_img, mx_img, cand_mt, holds_label)

if "mooncake" in kv_holds and kv_img != base_kv:
    raise SystemExit(
        f"FAIL: KVCacheBackendImage changed ({base_kv} -> {kv_img}) while mooncake is held by {holds_label}"
    )
if "modelexpress" in kv_holds and mx_img != base_mx:
    raise SystemExit(
        f"FAIL: ModelDeploymentModelExpressServerImage changed ({base_mx} -> {mx_img}) while modelexpress is held by {holds_label}"
    )
for k in ("nvidia", "amd", "ascend"):
    if ("model-transfer" in kv_holds or f"model-transfer.{k}" in kv_holds) and cand_mt[k] != base_mt[k]:
        raise SystemExit(
            f"FAIL: modelTransfer.images.{k} changed ({base_mt[k]} -> {cand_mt[k]}) while held by {holds_label}"
        )

print(f"IMAGES_CHANGED={'true' if images_changed else 'false'}")
print(f"HELD_KV_KEYS={','.join(sorted(kv_holds))}")
print(f"KV_IMG={kv_img}")
print(f"MX_IMG={mx_img}")
print(f"MT_NVIDIA={cand_mt['nvidia']}")
print(f"MT_AMD={cand_mt['amd']}")
print(f"MT_ASCEND={cand_mt['ascend']}")
PY

IMAGES_CHANGED=""
HELD_KV_KEYS=""
KV_IMG=""
MX_IMG=""
MT_NVIDIA=""
MT_AMD=""
MT_ASCEND=""
while IFS='=' read -r k v; do
  case "${k}" in
    IMAGES_CHANGED) IMAGES_CHANGED="${v}" ;;
    HELD_KV_KEYS) HELD_KV_KEYS="${v}" ;;
    KV_IMG) KV_IMG="${v}" ;;
    MX_IMG) MX_IMG="${v}" ;;
    MT_NVIDIA) MT_NVIDIA="${v}" ;;
    MT_AMD) MT_AMD="${v}" ;;
    MT_ASCEND) MT_ASCEND="${v}" ;;
  esac
done < "${IMAGE_META_FILE}"
rm -f "${IMAGE_META_FILE}"

assert_rocm_amd64_published() {
  local manifests_json="$1"
  local image_ref="$2"
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
with open(manifests_path, "r", encoding="utf-8") as f:
    data = json.load(f)
images = data.get("images", {})
entry = None
for key in (image_ref, f"docker.io/{image_ref}"):
    if key in images:
        entry = images[key]
        break
if not isinstance(entry, dict):
    print(f"FAIL: image not found in manifests report: {image_ref}", file=sys.stderr)
    sys.exit(1)
amd64 = str(entry.get("amd64Digest") or "")
if not DIGEST_RE.fullmatch(amd64):
    print(f"FAIL: rocm image {image_ref} missing valid amd64Digest", file=sys.stderr)
    sys.exit(1)
arm64 = str(entry.get("arm64Digest") or "")
if arm64:
    if not DIGEST_RE.fullmatch(arm64) or arm64 == amd64 or entry.get("published") is not True:
        print(f"FAIL: image {image_ref} has invalid multi-arch manifest entry", file=sys.stderr)
        sys.exit(1)
elif "amd64Published" in entry:
    if entry.get("amd64Published") is not True:
        print(f"FAIL: rocm image {image_ref} is not published", file=sys.stderr)
        sys.exit(1)
elif entry.get("published") is False:
    print(f"FAIL: rocm image {image_ref} is marked published=false", file=sys.stderr)
    sys.exit(1)
PY
}

if [[ "${IMAGES_CHANGED}" == "true" || -f "${MANIFESTS_JSON}" ]]; then
  autosync::assert_image_published "${MANIFESTS_JSON}" "${KV_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MX_IMG}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_NVIDIA}"
  autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_ASCEND}"
  if [[ "${MT_AMD}" == *-rocm* ]]; then
    assert_rocm_amd64_published "${MANIFESTS_JSON}" "${MT_AMD}"
  else
    autosync::assert_image_published "${MANIFESTS_JSON}" "${MT_AMD}"
  fi
fi

# 6. Proposal schema and version parity (.auto-sync/proposals/kv-transfer.json).
CAND_PROPOSAL="${AUTOSYNC_CANDIDATE_ROOT}/.auto-sync/proposals/kv-transfer.json"
python3 - "${CAND_PROPOSAL}" "${MOONCAKE_DF}" "${MT_DF}" "${CAND_MX_DF}" "${IMAGES_CHANGED}" "${AUTOSYNC_CANDIDATE_ROOT}" "${HELD_KV_KEYS}" <<'PY'
import json
import os
import re
import sys

prop_p, mc_df_p, mt_df_p, mx_df_p, images_changed_str, cand_root, held_keys_csv = sys.argv[1:8]
held_keys = {k for k in held_keys_csv.split(",") if k}
sys.path.insert(0, os.path.join(cand_root, ".auto-sync", "checks"))
from dockerfile_arg import read_dockerfile_arg
if not os.path.isfile(prop_p):
    raise SystemExit(f"FAIL: missing proposal artifact: {prop_p}")
with open(prop_p, "r", encoding="utf-8") as f:
    prop = json.load(f)

if not isinstance(prop, dict) or prop.get("schema_version") != 1:
    raise SystemExit("FAIL: proposals/kv-transfer.json must have schema_version == 1")
if not isinstance(prop.get("summary"), str) or not prop["summary"].strip():
    raise SystemExit("FAIL: proposals/kv-transfer.json summary must be a non-empty string")
stage = prop.get("stage")
if stage not in ("aligned", "recipe_sync", "tag_promotion"):
    raise SystemExit(f"FAIL: proposals/kv-transfer.json invalid stage: {stage!r}")
if not isinstance(prop.get("subscriptions"), list):
    raise SystemExit("FAIL: proposals/kv-transfer.json subscriptions must be a JSON array")
versions = prop.get("versions")
if not isinstance(versions, dict):
    raise SystemExit("FAIL: proposals/kv-transfer.json versions must be a JSON object")

required_ids = ("mooncake", "nixl-pypi", "nixl-rocm", "ucx-rocm", "hixl", "modelexpress", "gpustack-runner")
for uid in required_ids:
    val = versions.get(uid)
    if not isinstance(val, str) or not val.strip():
        raise SystemExit(f"FAIL: proposals/kv-transfer.json versions.{uid} must be a non-empty string")


def read_arg(path: str, arg_name: str) -> str:
    with open(path, "r", encoding="utf-8") as f:
        return read_dockerfile_arg(f.read(), arg_name, path)


allow_stage_b = (stage == "tag_promotion") or (images_changed_str == "true")


def check_pin(uid: str, expected: str, allow_sb: bool = False) -> None:
    got = versions[uid]
    if got == expected:
        return
    if allow_sb and allow_stage_b and got == f"{expected}+stage-b":
        return
    raise SystemExit(f"FAIL: proposals/kv-transfer.json versions.{uid} ({got!r}) != pinned ({expected!r})")


check_pin("mooncake", read_arg(mc_df_p, "MOONCAKE_VERSION"), allow_sb=("mooncake" not in held_keys))
check_pin("nixl-pypi", read_arg(mt_df_p, "NIXL_VERSION"), allow_sb=True)
check_pin("nixl-rocm", read_arg(mt_df_p, "NIXL_ROCM_REF"))
check_pin("ucx-rocm", read_arg(mt_df_p, "NIXL_ROCM_UCX_REF"))
for rocm_uid in ("nixl-rocm", "ucx-rocm"):
    if not re.fullmatch(r"v\d+\.\d+\.\d+", str(versions.get(rocm_uid) or "")):
        raise SystemExit(
            f"FAIL: proposals/kv-transfer.json versions.{rocm_uid} ({versions.get(rocm_uid)!r}) "
            "must be a stable SemVer tag vX.Y.Z"
        )
check_pin("hixl", read_arg(mt_df_p, "HIXL_COMMIT"))
check_pin("modelexpress", read_arg(mx_df_p, "MODELEXPRESS_VERSION"), allow_sb=("modelexpress" not in held_keys))
if not re.fullmatch(r"v\d+\.\d+\.\d+(?:\+stage-b)?", str(versions.get("modelexpress") or "")):
    raise SystemExit(
        f"FAIL: proposals/kv-transfer.json versions.modelexpress ({versions.get('modelexpress')!r}) "
        "must be a SemVer tag vX.Y.Z (optionally with +stage-b)"
    )

runner_v = str(versions.get("gpustack-runner") or "")
all_mt_held = ("model-transfer" in held_keys) or all(
    f"model-transfer.{v}" in held_keys for v in ("nvidia", "amd", "ascend")
)
if all_mt_held and runner_v == "aligned+stage-b":
    raise SystemExit(
        "FAIL: proposals/kv-transfer.json versions.gpustack-runner cannot be 'aligned+stage-b' while all model-transfer images are held"
    )
if runner_v not in ("aligned", "aligned+stage-b") and not re.fullmatch(r"[0-9a-f]{40}", runner_v):
    raise SystemExit(
        f"FAIL: proposals/kv-transfer.json versions.gpustack-runner ({runner_v!r}) "
        "must be 'aligned', 'aligned+stage-b', or a 40-hex commit SHA"
    )
PY

# 7. Chart / settings / docs regeneration and test gate when Stage B files changed.
PROMOTED_FILES_CHANGED="false"
for rel in \
  "pkg/worker/settings/value.go" \
  "pkg/worker/settings/value_test.go" \
  "pkg/worker/controllers/worker/model_deployment_transfer_test.go" \
  "docs/reference/settings.md" \
  "docs/modules/model-delivery/remote.md" \
  "deploy/gpustack-operator/chart/values.yaml" \
  "deploy/gpustack-operator/chart/values.schema.json" \
  "deploy/gpustack-operator/chart/README.md.gotmpl" \
  "deploy/gpustack-operator/chart/README.md"; do
  if [[ -f "${BASELINE_DIR}/${rel}" && -f "${AUTOSYNC_CANDIDATE_ROOT}/${rel}" ]]; then
    if ! cmp -s "${BASELINE_DIR}/${rel}" "${AUTOSYNC_CANDIDATE_ROOT}/${rel}"; then
      PROMOTED_FILES_CHANGED="true"
      break
    fi
  fi
done

if [[ "${PROMOTED_FILES_CHANGED}" == "true" && "${AUTOSYNC_FAST_SELFTEST:-false}" != "true" ]]; then
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" generate chart
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint chart
  make -C "${AUTOSYNC_CANDIDATE_ROOT}" lint docs
  (
    set -e
    cd "${AUTOSYNC_CANDIDATE_ROOT}"
    go test ./pkg/worker/settings/...
    if [[ -d "${AUTOSYNC_CANDIDATE_ROOT}/pkg/worker/controllers/worker" ]]; then
      go test ./pkg/worker/controllers/worker -run '^TestRenderModelDeploymentModelExpressObjects$'
    fi
  )
  git -C "${AUTOSYNC_CANDIDATE_ROOT}" diff --exit-code
fi
