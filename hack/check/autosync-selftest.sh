#!/usr/bin/env bash
# hack/check/autosync-selftest.sh - fast static checks for .auto-sync/ and auto-sync.yml.
#
# Usage:
#   bash hack/check/autosync-selftest.sh [fast|all|common] [repo-root]
#   bash hack/check/autosync-selftest.sh [repo-root]
set -euo pipefail

usage() {
  cat >&2 <<'EOF'
Usage:
  bash hack/check/autosync-selftest.sh [fast|all|common] [repo-root]
  bash hack/check/autosync-selftest.sh [repo-root]
EOF
}

SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)"
DEFAULT_ROOT="$(cd "${SCRIPT_DIR}/../.." && pwd -P)"
ROOT_DIR="${DEFAULT_ROOT}"

if [[ $# -ge 1 ]]; then
  if [[ -d "${1}" ]]; then
    ROOT_DIR="$(cd "${1}" && pwd -P)"
  else
    case "${1}" in
      fast|all|common)
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
TMP_PYCACHE=""

cleanup_on_exit() {
  if [[ -n "${TMP_PYCACHE}" && -d "${TMP_PYCACHE}" ]]; then
    rm -rf "${TMP_PYCACHE}"
  fi
}
trap cleanup_on_exit EXIT

pass() {
  printf 'PASS  %s\n' "$1"
  PASSES=$((PASSES + 1))
}

fail() {
  printf 'FAIL  %s\n' "$1" >&2
  FAILS=$((FAILS + 1))
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
  if [[ -f "${lib_dir}/util.sh" && -f "${lib_dir}/log.sh" && -f "${lib_dir}/style.sh" ]]; then
    # shellcheck source=../lib/util.sh
    # shellcheck disable=SC1091
    source "${lib_dir}/util.sh"
    # shellcheck source=../lib/log.sh
    # shellcheck disable=SC1091
    source "${lib_dir}/log.sh"
    # shellcheck source=../lib/style.sh
    # shellcheck disable=SC1091
    source "${lib_dir}/style.sh"
    if gpustack::lint::shellcheck::validate >/dev/null 2>&1; then
      gpustack::lint::shellcheck::bin
      return 0
    fi
  fi

  local sys_sc
  sys_sc="$(command -v shellcheck 2>/dev/null || true)"
  if [[ -n "${sys_sc}" ]]; then
    printf '%s\n' "${sys_sc}"
    return 0
  fi

  echo "COULD-NOT-RUN: shellcheck could not be resolved or installed." >&2
  return 2
}

if [[ ! -d "${ROOT_DIR}/.auto-sync" ]]; then
  echo "FAIL: missing .auto-sync directory under ${ROOT_DIR}" >&2
  exit 1
fi

SHELLCHECK_CMD="$(resolve_shellcheck)" || exit 2

echo "== shell syntax and shellcheck =="
SH_FILES=("${ROOT_DIR}/hack/check/autosync-selftest.sh")
while IFS= read -r f; do
  [[ -n "${f}" ]] && SH_FILES+=("${f}")
done < <(find "${ROOT_DIR}/.auto-sync" -type f -name '*.sh' | sort)

for file in "${SH_FILES[@]}"; do
  rel="${file#"${ROOT_DIR}/"}"
  if bash -n "${file}"; then
    pass "bash -n ${rel}"
  else
    fail "bash -n ${rel}"
  fi
  if "${SHELLCHECK_CMD}" -x --severity=info "${file}"; then
    pass "shellcheck ${rel}"
  else
    fail "shellcheck ${rel}"
  fi
done

echo "== python syntax =="
PY_FILES=()
while IFS= read -r pf; do
  [[ -n "${pf}" ]] && PY_FILES+=("${pf}")
done < <(find "${ROOT_DIR}/.auto-sync" -type f -name '*.py' | sort)

TMP_PYCACHE="$(mktemp -d)"
for pf in "${PY_FILES[@]}"; do
  rel="${pf#"${ROOT_DIR}/"}"
  if PYTHONPYCACHEPREFIX="${TMP_PYCACHE}" python3 -m py_compile "${pf}"; then
    pass "py_compile ${rel}"
  else
    fail "py_compile ${rel}"
  fi
done

echo "== manifest, proposal, holds, and workflow validation =="
if python3 -B - "${ROOT_DIR}" <<'PY'
import glob
import importlib.util
import json
import os
import re
import shutil
import subprocess
import sys

root = sys.argv[1]
autosync_dir = os.path.join(root, ".auto-sync")

try:
    import yaml  # type: ignore
except ImportError:
    yaml = None


def _strip_yaml_scalar(val: str) -> object:
    val = val.strip()
    if not val:
        return ""
    if len(val) >= 2 and val[0] == val[-1] and val[0] in ("'", '"'):
        return val[1:-1]
    val = re.sub(r"\s+#.*$", "", val).strip()
    if val == "[]":
        return []
    if val.startswith("[") and val.endswith("]"):
        inner = val[1:-1].strip()
        return [_strip_yaml_scalar(x) for x in inner.split(",")] if inner else []
    if val in ("true", "True"):
        return True
    if val in ("false", "False"):
        return False
    return val


def _parse_yaml_stdlib(raw: str, path: str) -> dict:
    root_obj: dict = {}
    stack: list[tuple[int, object]] = [(-1, root_obj)]
    pending: tuple[int, dict, str] | None = None
    block_indent: int | None = None
    key_val_re = re.compile(r"^([^:\s][^:]*?):(?:\s+(.*)|)$")

    for lineno, line in enumerate(raw.splitlines(), start=1):
        if not line.strip():
            continue
        indent = len(line) - len(line.lstrip(" "))
        if "\t" in line[:indent]:
            raise ValueError(f"{path}:{lineno}: tab indentation is not allowed in YAML")
        stripped = line.strip()
        if block_indent is not None:
            if indent > block_indent:
                continue
            block_indent = None
        if stripped.startswith("#"):
            continue

        is_seq = stripped.startswith("- ") or stripped == "-"
        if pending is not None and (indent > pending[0] or (indent == pending[0] and is_seq)):
            p_indent, p_dict, p_key = pending
            new_container: object = [] if is_seq else {}
            p_dict[p_key] = new_container
            stack.append((p_indent, new_container))
            pending = None
        elif pending is not None:
            _, p_dict, p_key = pending
            p_dict[p_key] = None
            pending = None

        while len(stack) > 1 and indent <= stack[-1][0]:
            stack.pop()

        cur = stack[-1][1]
        if is_seq:
            if not isinstance(cur, list):
                raise ValueError(f"{path}:{lineno}: unexpected list item")
            rest = stripped[1:].strip()
            m = key_val_re.match(rest) if rest else None
            if not rest:
                item_dict: dict = {}
                cur.append(item_dict)
                stack.append((indent, item_dict))
            elif m:
                k = m.group(1).strip().strip("'\"")
                v = (m.group(2) or "").strip()
                item_dict = {}
                cur.append(item_dict)
                stack.append((indent, item_dict))
                if not v or v.startswith("#"):
                    pending = (indent + 2, item_dict, k)
                elif v[0] in ("|", ">") and len(v) <= 2:
                    item_dict[k] = ""
                    block_indent = indent + 2
                else:
                    item_dict[k] = _strip_yaml_scalar(v)
            else:
                cur.append(_strip_yaml_scalar(rest))
        else:
            if not isinstance(cur, dict):
                raise ValueError(f"{path}:{lineno}: expected mapping entry")
            m = key_val_re.match(stripped)
            if not m:
                raise ValueError(f"{path}:{lineno}: expected 'key: value'")
            k = m.group(1).strip().strip("'\"")
            v = (m.group(2) or "").strip()
            if not v or v.startswith("#"):
                pending = (indent, cur, k)
            elif v[0] in ("|", ">") and len(v) <= 2:
                cur[k] = ""
                block_indent = indent
            else:
                cur[k] = _strip_yaml_scalar(v)

    if pending is not None:
        _, p_dict, p_key = pending
        p_dict[p_key] = None
    return root_obj


def load_yaml(path: str):
    with open(path, "r", encoding="utf-8") as f:
        raw = f.read()
    if yaml is not None:
        return yaml.safe_load(raw)
    ruby_bin = shutil.which("ruby")
    if ruby_bin:
        res = subprocess.run(
            [ruby_bin, "-ryaml", "-rjson", "-e", "puts JSON.generate(YAML.safe_load(STDIN.read))"],
            input=raw,
            text=True,
            capture_output=True,
        )
        if res.returncode != 0:
            raise RuntimeError(res.stderr.strip() or f"ruby YAML parse failed for {path}")
        return json.loads(res.stdout)
    yq_bin = shutil.which("yq")
    if yq_bin:
        res = subprocess.run(
            [yq_bin, "-o=json", "."],
            input=raw,
            text=True,
            capture_output=True,
        )
        if res.returncode != 0:
            raise RuntimeError(res.stderr.strip() or f"yq YAML parse failed for {path}")
        return json.loads(res.stdout)
    return _parse_yaml_stdlib(raw, path)


def check_repo_file(rel_path: object, label: str) -> None:
    if not isinstance(rel_path, str) or not rel_path.strip():
        raise AssertionError(f"{label}: expected non-empty path string, got {rel_path!r}")
    if os.path.isabs(rel_path):
        raise AssertionError(f"{label}: path must be repo-relative, got {rel_path!r}")
    full = os.path.join(root, rel_path)
    if not os.path.isfile(full):
        raise AssertionError(f"{label}: referenced file does not exist: {rel_path}")


taskpacks = sorted(glob.glob(os.path.join(autosync_dir, "*-taskpack.yaml")))
if not taskpacks:
    raise AssertionError("no *-taskpack.yaml files found under .auto-sync")

for tp in taskpacks:
    rel_tp = os.path.relpath(tp, root)
    doc = load_yaml(tp)
    if not isinstance(doc, dict):
        raise AssertionError(f"{rel_tp}: expected YAML mapping")
    if doc.get("apiVersion") != "loopwright.dev/v1alpha1":
        raise AssertionError(f"{rel_tp}: apiVersion must be loopwright.dev/v1alpha1")
    if doc.get("kind") != "Taskpack":
        raise AssertionError(f"{rel_tp}: kind must be Taskpack")

    spec = doc.get("spec")
    if not isinstance(spec, dict):
        raise AssertionError(f"{rel_tp}: missing spec mapping")

    instructions = spec.get("instructions")
    if not isinstance(instructions, list) or not instructions:
        raise AssertionError(f"{rel_tp}: spec.instructions must be a non-empty list")
    for idx, instr in enumerate(instructions):
        check_repo_file(instr, f"{rel_tp} spec.instructions[{idx}]")

    proposal = spec.get("proposal")
    if not isinstance(proposal, dict):
        raise AssertionError(f"{rel_tp}: missing spec.proposal mapping")
    check_repo_file(proposal.get("path"), f"{rel_tp} spec.proposal.path")

    verification = spec.get("verification")
    if not isinstance(verification, dict):
        raise AssertionError(f"{rel_tp}: missing spec.verification mapping")
    local_checks = verification.get("localChecks")
    if not isinstance(local_checks, list):
        raise AssertionError(f"{rel_tp}: spec.verification.localChecks must be a list")
    for idx, chk in enumerate(local_checks):
        if not isinstance(chk, dict):
            raise AssertionError(f"{rel_tp}: localChecks[{idx}] must be a mapping")
        params = chk.get("parameters")
        if isinstance(params, dict) and "script" in params:
            check_repo_file(
                params.get("script"),
                f"{rel_tp} spec.verification.localChecks[{idx}].parameters.script",
            )
    print(f"PASS  taskpack {rel_tp}")

proposals = sorted(glob.glob(os.path.join(autosync_dir, "proposals", "*.json")))
if not proposals:
    raise AssertionError("no proposal JSON files found under .auto-sync/proposals")
for prop in proposals:
    rel_prop = os.path.relpath(prop, root)
    with open(prop, "r", encoding="utf-8") as f:
        pdata = json.load(f)
    if not isinstance(pdata, dict):
        raise AssertionError(f"{rel_prop}: expected JSON object")
    sv = pdata.get("schema_version")
    if isinstance(sv, bool) or sv != 1:
        raise AssertionError(f"{rel_prop}: schema_version must be 1, got {sv!r}")
    print(f"PASS  proposal {rel_prop}")

holds_mod_path = os.path.join(autosync_dir, "checks", "setting_image_holds.py")
holds_json_path = os.path.join(autosync_dir, "setting-image-holds.json")
spec_mod = importlib.util.spec_from_file_location("setting_image_holds", holds_mod_path)
if spec_mod is None or spec_mod.loader is None:
    raise AssertionError(f"unable to load {holds_mod_path}")
holds_mod = importlib.util.module_from_spec(spec_mod)
spec_mod.loader.exec_module(holds_mod)
holds_mod.load_setting_image_holds(holds_json_path)
print("PASS  setting-image-holds.json via setting_image_holds.py")

workflow_path = os.path.join(root, ".github", "workflows", "auto-sync.yml")
if not os.path.isfile(workflow_path):
    raise AssertionError("missing .github/workflows/auto-sync.yml")
wf_doc = load_yaml(workflow_path)
if not isinstance(wf_doc, dict):
    raise AssertionError(".github/workflows/auto-sync.yml: expected YAML mapping")
print("PASS  workflow .github/workflows/auto-sync.yml")
PY
then
  pass "manifest, proposal, holds, and workflow static checks"
else
  fail "manifest, proposal, holds, and workflow static checks"
fi

echo
echo "Summary: ${PASSES} passed, ${FAILS} failed"
[[ "${FAILS}" -eq 0 ]]
