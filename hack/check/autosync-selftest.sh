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

echo "== TaskpackSuite, Taskpack 2.0 manifests, digests, memory, proposals, holds, and workflow validation =="
if python3 -B - "${ROOT_DIR}" <<'PY'
import glob
import hashlib
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
    if re.fullmatch(r"-?\d+", val):
        return int(val)
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


def check_repo_file(rel_path: object, label: str) -> str:
    if not isinstance(rel_path, str) or not rel_path.strip():
        raise AssertionError(f"{label}: expected non-empty path string, got {rel_path!r}")
    if os.path.isabs(rel_path):
        raise AssertionError(f"{label}: path must be repo-relative, got {rel_path!r}")
    full = os.path.join(root, rel_path)
    if not os.path.isfile(full):
        raise AssertionError(f"{label}: referenced file does not exist: {rel_path}")
    return full


def check_scope_glob(pattern: object, label: str) -> None:
    if not isinstance(pattern, str) or not pattern.strip():
        raise AssertionError(f"{label}: expected non-empty glob string, got {pattern!r}")
    if os.path.isabs(pattern) or pattern.startswith("./") or pattern.endswith("/"):
        raise AssertionError(f"{label}: invalid scope pattern {pattern!r}")
    if pattern.endswith("/**"):
        base_rel = pattern[:-3]
        if "**" in base_rel:
            raise AssertionError(f"{label}: '**' is only allowed as trailing '/**' in {pattern!r}")
        if "*" not in base_rel and "?" not in base_rel:
            full = os.path.join(root, base_rel)
            if os.path.isfile(full):
                raise AssertionError(f"{label}: exact file {base_rel!r} must not end with '/**'")
            if not os.path.isdir(full):
                raise AssertionError(f"{label}: directory {base_rel!r} does not exist for {pattern!r}")
    else:
        if "**" in pattern:
            raise AssertionError(f"{label}: invalid '**' placement in {pattern!r}")
        if "*" not in pattern and "?" not in pattern:
            full = os.path.join(root, pattern)
            if os.path.isdir(full):
                raise AssertionError(
                    f"{label}: directory {pattern!r} MUST use explicit '{pattern}/**' under Taskpack 2.0"
                )
            if not os.path.isfile(full):
                raise AssertionError(f"{label}: exact file {pattern!r} does not exist")


def sha256_tree(dir_path: str) -> str:
    # Matches Loopwright's computeDirectoryTreeDigest in packages/taskpack/src/digests.ts.
    rows: list[tuple[str, int, str]] = []
    for cur_root, dirs, files in os.walk(dir_path):
        dirs.sort()
        for fn in sorted(files):
            full = os.path.join(cur_root, fn)
            rel = os.path.relpath(full, dir_path).replace(os.sep, "/")
            data = open(full, "rb").read()
            fhash = hashlib.sha256(data).hexdigest()
            rows.append((rel, len(data), fhash))
    rows.sort(key=lambda item: item[0])
    h = hashlib.sha256()
    h.update(b"loopwright.dev/digests/tree/v1\n")
    for rel, size, fhash in rows:
        h.update(f"{rel}\0{size}\0{fhash}\n".encode("utf-8"))
    return h.hexdigest()


def canonical_json(value: object) -> str:
    # Matches @loopwright/memory canonical() in packages/memory/src/schema.ts.
    if value is None or isinstance(value, (bool, int, float, str)):
        return json.dumps(value, ensure_ascii=False, separators=(",", ":"))
    if isinstance(value, list):
        return "[" + ",".join(canonical_json(item) for item in value) + "]"
    if isinstance(value, dict):
        return (
            "{"
            + ",".join(
                json.dumps(k, ensure_ascii=False, separators=(",", ":")) + ":" + canonical_json(value[k])
                for k in sorted(value.keys())
            )
            + "}"
        )
    raise TypeError(f"unsupported JSON type: {type(value)!r}")


FORBIDDEN_ENV_RE = re.compile(
    r"(?:^|_)(?:TOKEN|SECRET|PASSWORD|PASSWD|API_KEY|APIKEY|PRIVATE_KEY|ACCESS_KEY|CREDENTIAL)(?:$|_)"
)
FORBIDDEN_ENV_PREFIXES = ("LOOPWRIGHT_", "GITHUB_APP_", "INPUT_", "ACTIONS_", "RUNNER_", "GIT_")
EXPECTED_DOMAINS = ["slicing", "kv-transfer", "router", "priors", "charts", "k8s-deps"]
MAX_LOOPWRIGHT_PATTERNS = 128
GITHUB_BUILT_IN_PROTECTED = {
    ".git",
    ".github",
    ".loopwright",
    ".agents",
    ".claude",
    ".codex",
    "AGENTS.md",
    "CLAUDE.md",
}

taskpacks = sorted(glob.glob(os.path.join(autosync_dir, "*-taskpack.yaml")))
if len(taskpacks) != 6:
    raise AssertionError(f"expected 6 *-taskpack.yaml files under .auto-sync, found {len(taskpacks)}")

seen_lockfiles: set[str] = set()
manifest_by_rel: dict[str, dict] = {}

for tp in taskpacks:
    rel_tp = os.path.relpath(tp, root).replace(os.sep, "/")
    doc = load_yaml(tp)
    if not isinstance(doc, dict):
        raise AssertionError(f"{rel_tp}: expected YAML mapping")
    if doc.get("apiVersion") != "loopwright.dev/v1alpha1":
        raise AssertionError(f"{rel_tp}: apiVersion must be loopwright.dev/v1alpha1")
    if doc.get("kind") != "Taskpack":
        raise AssertionError(f"{rel_tp}: kind must be Taskpack")

    meta = doc.get("metadata") or {}
    tp_name = meta.get("name")
    if not isinstance(tp_name, str) or not tp_name:
        raise AssertionError(f"{rel_tp}: missing metadata.name")

    spec = doc.get("spec")
    if not isinstance(spec, dict):
        raise AssertionError(f"{rel_tp}: missing spec mapping")

    execution = spec.get("execution")
    if not isinstance(execution, dict) or execution.get("mode") != "strict":
        raise AssertionError(f"{rel_tp}: spec.execution.mode must be 'strict'")
    digests_cfg = execution.get("digests")
    if not isinstance(digests_cfg, dict) or digests_cfg.get("pinDigests") is not True:
        raise AssertionError(f"{rel_tp}: spec.execution.digests.pinDigests must be true")
    lockfile_rel = digests_cfg.get("lockfile")
    lockfile_full = check_repo_file(lockfile_rel, f"{rel_tp} spec.execution.digests.lockfile")
    if lockfile_rel in seen_lockfiles:
        raise AssertionError(f"{rel_tp}: duplicate digest lockfile {lockfile_rel!r} across taskpacks")
    seen_lockfiles.add(lockfile_rel)

    lock_doc = json.load(open(lockfile_full, "r", encoding="utf-8"))
    if lock_doc.get("taskpack") != tp_name:
        raise AssertionError(
            f"{lockfile_rel}: taskpack {lock_doc.get('taskpack')!r} != manifest {tp_name!r}"
        )
    for entry in lock_doc.get("entries") or []:
        epath = entry.get("path", "")
        ekind = entry.get("kind", "")
        ewant = entry.get("sha256", "")
        efull = os.path.join(root, epath)
        if ekind == "directory":
            egot = sha256_tree(efull)
        else:
            egot = hashlib.sha256(open(efull, "rb").read()).hexdigest()
        if egot != ewant:
            raise AssertionError(
                f"{lockfile_rel}: digest drift for {epath} ({ekind}): lockfile={ewant[:12]} != actual={egot[:12]}"
            )

    preflight = spec.get("preflight")
    if not isinstance(preflight, dict):
        raise AssertionError(f"{rel_tp}: missing spec.preflight mapping")
    pf_script = check_repo_file(preflight.get("script"), f"{rel_tp} spec.preflight.script")
    if not os.access(pf_script, os.X_OK):
        raise AssertionError(f"{rel_tp}: preflight script {preflight.get('script')} must be executable")
    for env_name in preflight.get("env") or []:
        if (
            not isinstance(env_name, str)
            or env_name in ("GITHUB_TOKEN", "GH_TOKEN")
            or env_name.startswith(FORBIDDEN_ENV_PREFIXES)
            or FORBIDDEN_ENV_RE.search(env_name)
        ):
            raise AssertionError(f"{rel_tp}: forbidden preflight env variable {env_name!r}")

    scope = spec.get("scope")
    if not isinstance(scope, dict):
        raise AssertionError(f"{rel_tp}: missing spec.scope mapping")
    writable = scope.get("writable")
    protected = scope.get("protected")
    if not isinstance(writable, list) or not writable:
        raise AssertionError(f"{rel_tp}: spec.scope.writable must be non-empty")
    if not isinstance(protected, list):
        raise AssertionError(f"{rel_tp}: spec.scope.protected must be a list")
    for idx, pat in enumerate(writable):
        check_scope_glob(pat, f"{rel_tp} spec.scope.writable[{idx}]")
    for idx, pat in enumerate(protected):
        check_scope_glob(pat, f"{rel_tp} spec.scope.protected[{idx}]")

    instructions = spec.get("instructions")
    if not isinstance(instructions, list) or not instructions:
        raise AssertionError(f"{rel_tp}: spec.instructions must be a non-empty list")
    for idx, instr in enumerate(instructions):
        instr_path = instr.get("path") if isinstance(instr, dict) else instr
        check_repo_file(instr_path, f"{rel_tp} spec.instructions[{idx}]")

    proposal = spec.get("proposal")
    if not isinstance(proposal, dict):
        raise AssertionError(f"{rel_tp}: missing spec.proposal mapping")
    prop_full = check_repo_file(proposal.get("path"), f"{rel_tp} spec.proposal.path")
    if proposal.get("path") not in writable:
        raise AssertionError(f"{rel_tp}: spec.proposal.path must appear as an exact file in spec.scope.writable")
    req_fields = proposal.get("requiredFields")
    if isinstance(req_fields, list):
        with open(prop_full, "r", encoding="utf-8") as pf_handle:
            prop_obj = json.load(pf_handle)
        missing_fields = [rf for rf in req_fields if rf not in prop_obj]
        if missing_fields:
            raise AssertionError(
                f"{rel_tp}: proposal {proposal.get('path')} missing requiredFields {missing_fields!r}"
            )

    baseline_align = spec.get("baselineAlignment")
    if not isinstance(baseline_align, dict) or baseline_align.get("policy") not in ("extract-context", "fail-fast"):
        raise AssertionError(
            f"{rel_tp}: spec.baselineAlignment.policy must be 'extract-context' or 'fail-fast', got {baseline_align!r}"
        )
    if "maxConflictFiles" in baseline_align:
        mcf = baseline_align["maxConflictFiles"]
        if isinstance(mcf, bool) or not isinstance(mcf, int) or not (1 <= mcf <= 64):
            raise AssertionError(f"{rel_tp}: spec.baselineAlignment.maxConflictFiles out of range: {mcf!r}")
    if "maxConflictBytes" in baseline_align:
        mcb = baseline_align["maxConflictBytes"]
        if isinstance(mcb, bool) or not isinstance(mcb, int) or not (1024 <= mcb <= 262144):
            raise AssertionError(f"{rel_tp}: spec.baselineAlignment.maxConflictBytes out of range: {mcb!r}")

    verification = spec.get("verification")
    if not isinstance(verification, dict):
        raise AssertionError(f"{rel_tp}: missing spec.verification mapping")
    local_checks = verification.get("localChecks")
    if not isinstance(local_checks, list):
        raise AssertionError(f"{rel_tp}: spec.verification.localChecks must be a list")
    has_cmd_v1 = False
    for idx, chk in enumerate(local_checks):
        if not isinstance(chk, dict):
            raise AssertionError(f"{rel_tp}: localChecks[{idx}] must be a mapping")
        params = chk.get("parameters")
        if chk.get("implementation") == "command.v1":
            has_cmd_v1 = True
            if not isinstance(params, dict):
                raise AssertionError(f"{rel_tp}: command.v1 missing parameters")
            check_repo_file(
                params.get("script"),
                f"{rel_tp} spec.verification.localChecks[{idx}].parameters.script",
            )
            toolchains = params.get("toolchainPaths")
            if toolchains is not None:
                if not isinstance(toolchains, list) or not all(
                    isinstance(p, str) and p.strip() for p in toolchains
                ):
                    raise AssertionError(
                        f"{rel_tp}: command.v1 toolchainPaths must be a list of non-empty strings"
                    )
                if "/usr/local/bin" in toolchains:
                    raise AssertionError(
                        f"{rel_tp}: command.v1 toolchainPaths must not include /usr/local/bin"
                    )
    if not has_cmd_v1:
        raise AssertionError(f"{rel_tp}: expected at least one command.v1 check")

    output_cfg = spec.get("output")
    if not isinstance(output_cfg, dict) or set(output_cfg.get("formats") or []) < {"json", "github-step-summary"}:
        raise AssertionError(f"{rel_tp}: spec.output.formats must include json and github-step-summary")

    manifest_by_rel[rel_tp] = doc
    print(f"PASS  taskpack {rel_tp}")

suite_rel = ".auto-sync/suite.yaml"
suite_path = check_repo_file(suite_rel, suite_rel)
suite_doc = load_yaml(suite_path)
if not isinstance(suite_doc, dict):
    raise AssertionError(f"{suite_rel}: expected YAML mapping")
if suite_doc.get("apiVersion") != "loopwright.dev/v1alpha1":
    raise AssertionError(f"{suite_rel}: apiVersion must be loopwright.dev/v1alpha1")
if suite_doc.get("kind") != "TaskpackSuite":
    raise AssertionError(f"{suite_rel}: kind must be TaskpackSuite")
suite_meta = suite_doc.get("metadata") or {}
if not isinstance(suite_meta.get("name"), str) or not suite_meta["name"].strip():
    raise AssertionError(f"{suite_rel}: missing metadata.name")
suite_spec = suite_doc.get("spec")
if not isinstance(suite_spec, dict):
    raise AssertionError(f"{suite_rel}: missing spec mapping")
suite_domains = suite_spec.get("domains")
if not isinstance(suite_domains, list) or len(suite_domains) != 6:
    raise AssertionError(f"{suite_rel}: expected 6 spec.domains entries, got {suite_domains!r}")

suite_domain_ids: list[str] = []
suite_schedules: set[str] = set()
head_prefixes: list[tuple[str, str]] = []
for idx, d_entry in enumerate(suite_domains):
    if not isinstance(d_entry, dict):
        raise AssertionError(f"{suite_rel}: spec.domains[{idx}] must be a mapping")
    did = d_entry.get("id")
    dtp = d_entry.get("taskpack")
    if not isinstance(did, str) or not did:
        raise AssertionError(f"{suite_rel}: spec.domains[{idx}] missing id")
    if did in suite_domain_ids:
        raise AssertionError(f"{suite_rel}: duplicate domain id {did!r}")
    suite_domain_ids.append(did)
    expected_tp_rel = f".auto-sync/{did}-taskpack.yaml"
    if dtp != expected_tp_rel or dtp not in manifest_by_rel:
        raise AssertionError(
            f"{suite_rel}: domain {did!r} taskpack must be {expected_tp_rel!r}, got {dtp!r}"
        )
    if d_entry.get("enabled", True) is not True:
        raise AssertionError(f"{suite_rel}: domain {did!r} must be enabled")
    scheds = d_entry.get("schedules")
    if not isinstance(scheds, list) or not scheds or not all(isinstance(s, str) and s.strip() for s in scheds):
        raise AssertionError(f"{suite_rel}: domain {did!r} must declare non-empty schedules")
    suite_schedules.update(scheds)
    pub = (manifest_by_rel[dtp].get("spec") or {}).get("publication") or {}
    hbp = str(pub.get("headBranchPrefix") or "")
    if not hbp:
        raise AssertionError(f"{dtp}: missing spec.publication.headBranchPrefix")
    for prev_id, prev_hbp in head_prefixes:
        if hbp.startswith(prev_hbp) or prev_hbp.startswith(hbp):
            raise AssertionError(
                f"{suite_rel}: colliding headBranchPrefix between {prev_id!r} ({prev_hbp!r}) and {did!r} ({hbp!r})"
            )
    head_prefixes.append((did, hbp))

if set(suite_domain_ids) != set(EXPECTED_DOMAINS):
    raise AssertionError(
        f"{suite_rel}: domain set {suite_domain_ids!r} != expected {EXPECTED_DOMAINS!r}"
    )

# Verify Loopwright suite-wide and per-domain protected-path union stays well within 128 patterns.
suite_union: set[str] = set(GITHUB_BUILT_IN_PROTECTED) | {
    suite_rel,
    ".auto-sync/policies",
    ".auto-sync/checks",
    ".auto-sync/scripts",
    ".auto-sync/memory",
    ".auto-sync/taskpack.yaml",
    ".auto-sync/taskpack.yml",
    ".auto-sync/taskpack.json",
    ".auto-sync/digests.lock.json",
}
for entry_name in sorted(os.listdir(autosync_dir)):
    rel_entry = f".auto-sync/{entry_name}"
    if rel_entry == ".auto-sync/proposals":
        for prop_name in sorted(os.listdir(os.path.join(autosync_dir, "proposals"))):
            suite_union.add(f".auto-sync/proposals/{prop_name}")
    else:
        suite_union.add(rel_entry)
for rel_tp, mdoc in manifest_by_rel.items():
    suite_union.add(rel_tp)
    mspec = mdoc["spec"]
    suite_union.add(mspec["execution"]["digests"]["lockfile"])
    suite_union.add(mspec["preflight"]["script"])
    suite_union.add(mspec["memory"]["directory"])
    for instr in mspec["instructions"]:
        suite_union.add(instr["path"] if isinstance(instr, dict) else instr)
    for chk in mspec["verification"]["localChecks"]:
        if chk.get("implementation") == "command.v1":
            suite_union.add(chk["parameters"]["script"])

if len(suite_union) > MAX_LOOPWRIGHT_PATTERNS:
    raise AssertionError(
        f"{suite_rel}: suiteProtectedPaths union ({len(suite_union)}) exceeds {MAX_LOOPWRIGHT_PATTERNS}"
    )
for rel_tp, mdoc in manifest_by_rel.items():
    mspec = mdoc["spec"]
    prop_path = mspec["proposal"]["path"]
    dom_protected = set(mspec["scope"]["protected"]) | (
        (suite_union - GITHUB_BUILT_IN_PROTECTED) - {prop_path}
    )
    if len(dom_protected) > MAX_LOOPWRIGHT_PATTERNS:
        raise AssertionError(
            f"{rel_tp}: merged suite protectedPaths ({len(dom_protected)}) exceeds {MAX_LOOPWRIGHT_PATTERNS}"
        )
print(f"PASS  suite {suite_rel} (protected union={len(suite_union)}/{MAX_LOOPWRIGHT_PATTERNS})")

proposals = sorted(glob.glob(os.path.join(autosync_dir, "proposals", "*.json")))
if len(proposals) != 6:
    raise AssertionError(f"expected 6 proposal JSON files under .auto-sync/proposals, found {len(proposals)}")
for prop in proposals:
    rel_prop = os.path.relpath(prop, root).replace(os.sep, "/")
    with open(prop, "r", encoding="utf-8") as f:
        pdata = json.load(f)
    if not isinstance(pdata, dict):
        raise AssertionError(f"{rel_prop}: expected JSON object")
    sv = pdata.get("schema_version")
    if isinstance(sv, bool) or sv != 1:
        raise AssertionError(f"{rel_prop}: schema_version must be 1, got {sv!r}")
    print(f"PASS  proposal {rel_prop}")

memory_indices = sorted(glob.glob(os.path.join(autosync_dir, "memory", "*", "index.json")))
for midx in memory_indices:
    rel_midx = os.path.relpath(midx, root).replace(os.sep, "/")
    raw_bytes = open(midx, "rb").read()
    if not raw_bytes.endswith(b"\n") or raw_bytes.endswith(b"\n\n"):
        raise AssertionError(f"{rel_midx}: must end with a single trailing LF")
    mdoc = json.loads(raw_bytes.decode("utf-8"))
    if not isinstance(mdoc, dict) or mdoc.get("version") != 1:
        raise AssertionError(f"{rel_midx}: expected version=1 memory index object")
    expected_tp_dir = os.path.basename(os.path.dirname(midx))
    if mdoc.get("taskpack") != expected_tp_dir:
        raise AssertionError(f"{rel_midx}: taskpack {mdoc.get('taskpack')!r} != {expected_tp_dir!r}")
    digest_base = {
        "version": mdoc["version"],
        "taskpack": mdoc["taskpack"],
        "repository": mdoc["repository"],
        "updatedAtMs": mdoc["updatedAtMs"],
        "entries": mdoc["entries"],
        "retired": mdoc["retired"],
    }
    want_digest = hashlib.sha256(
        b"loopwright.dev/memory/memory-store-index/v1alpha1\0"
        + canonical_json(digest_base).encode("utf-8")
    ).hexdigest()
    got_digest = (mdoc.get("storeDigest") or {}).get("hex")
    if got_digest != want_digest:
        raise AssertionError(
            f"{rel_midx}: storeDigest mismatch: got {got_digest!r}, expected {want_digest!r}"
        )
    print(f"PASS  memory {rel_midx}")

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

wf_on = wf_doc.get("on") if "on" in wf_doc else wf_doc.get(True)
if not isinstance(wf_on, dict):
    raise AssertionError("auto-sync.yml: missing top-level 'on' trigger block")
wf_crons = {
    str(item.get("cron") or "").strip()
    for item in (wf_on.get("schedule") or [])
    if isinstance(item, dict) and str(item.get("cron") or "").strip()
}
if wf_crons != suite_schedules:
    raise AssertionError(
        f"auto-sync.yml: on.schedule crons {sorted(wf_crons)!r} != suite.yaml schedules {sorted(suite_schedules)!r}"
    )

jobs = wf_doc.get("jobs") or {}
resolve_job = jobs.get("resolve-domains") or {}
resolve_steps = resolve_job.get("steps") or []
resolve_lw_steps = [
    s for s in resolve_steps if isinstance(s, dict) and str(s.get("uses") or "").startswith("gpustack/loopwright@")
]
if len(resolve_lw_steps) != 1:
    raise AssertionError("auto-sync.yml: resolve-domains must contain exactly one gpustack/loopwright step")
resolve_lw_uses = str(resolve_lw_steps[0].get("uses") or "")
resolve_lw_ref = resolve_lw_uses.split("@", 1)[1].strip()
if not re.fullmatch(r"[0-9a-f]{40}", resolve_lw_ref):
    raise AssertionError(
        f"auto-sync.yml: resolve-domains gpustack/loopwright must be pinned to a 40-char SHA, got {resolve_lw_uses!r}"
    )
resolve_with = resolve_lw_steps[0].get("with") or {}
if resolve_with.get("suite-path") != suite_rel:
    raise AssertionError(
        f"auto-sync.yml: resolve-domains must set suite-path: {suite_rel}, got {resolve_with.get('suite-path')!r}"
    )
if str(resolve_with.get("resolve-suite-only")).lower() != "true":
    raise AssertionError("auto-sync.yml: resolve-domains must set resolve-suite-only: 'true'")

auto_sync_job = jobs.get("auto-sync") or {}
strategy_matrix = str((auto_sync_job.get("strategy") or {}).get("matrix") or "")
if "needs.resolve-domains.outputs.matrix" not in strategy_matrix:
    raise AssertionError("auto-sync.yml: auto-sync strategy.matrix must consume needs.resolve-domains.outputs.matrix")
concurrency_group = str((auto_sync_job.get("concurrency") or {}).get("group") or "")
if "matrix.domainId" not in concurrency_group:
    raise AssertionError("auto-sync.yml: auto-sync concurrency.group must be keyed by matrix.domainId")

sync_steps = auto_sync_job.get("steps") or []
lw_steps = [s for s in sync_steps if isinstance(s, dict) and str(s.get("uses") or "").startswith("gpustack/loopwright@")]
if len(lw_steps) != 1:
    raise AssertionError(".github/workflows/auto-sync.yml: expected exactly one gpustack/loopwright step in auto-sync")
lw_uses = str(lw_steps[0].get("uses") or "")
lw_ref = lw_uses.split("@", 1)[1].strip()
if not re.fullmatch(r"[0-9a-f]{40}", lw_ref):
    raise AssertionError(f"auto-sync.yml: gpustack/loopwright must be pinned to a 40-char commit SHA, got {lw_uses!r}")
if lw_ref != resolve_lw_ref:
    raise AssertionError(
        f"auto-sync.yml: resolve-domains ({resolve_lw_ref}) and auto-sync ({lw_ref}) must pin identical Loopwright SHAs"
    )
fetch_pr_steps = [s for s in sync_steps if isinstance(s, dict) and s.get("name") == "Fetch PR head"]
expected_pr_expr = str((fetch_pr_steps[0].get("env") or {}).get("EXPECTED_PR_HEAD_SHA") or "") if len(fetch_pr_steps) == 1 else ""
if len(fetch_pr_steps) != 1 or (
    "needs.resolve-domains.outputs.pr_head_sha" not in expected_pr_expr
    and "matrix.headSha" not in expected_pr_expr
):
    raise AssertionError("auto-sync.yml: Fetch PR head step must verify EXPECTED_PR_HEAD_SHA from suite resolution")
lw_with = lw_steps[0].get("with") or {}
if lw_with.get("suite-path") != suite_rel:
    raise AssertionError(f"auto-sync.yml: Loopwright step must set suite-path: {suite_rel}, got {lw_with.get('suite-path')!r}")
if "matrix.domainId" not in str(lw_with.get("domain") or ""):
    raise AssertionError(f"auto-sync.yml: Loopwright step must set domain: ${{{{ matrix.domainId }}}}, got {lw_with.get('domain')!r}")
if lw_with.get("mode") != "auto":
    raise AssertionError(f"auto-sync.yml: Loopwright step must set mode: auto, got {lw_with.get('mode')!r}")
if str(lw_with.get("allow-privileged-execution")).lower() != "true":
    raise AssertionError("auto-sync.yml: Loopwright step must set allow-privileged-execution: 'true'")
if lw_with.get("output") != "json,github-step-summary":
    raise AssertionError("auto-sync.yml: Loopwright step must set output: json,github-step-summary")
for legacy_key in ("upstream-records", "source-revision"):
    if legacy_key in lw_with:
        raise AssertionError(f"auto-sync.yml: legacy input {legacy_key!r} must be removed under Taskpack 2.0")
print("PASS  workflow .github/workflows/auto-sync.yml")
PY
then
  pass "TaskpackSuite, Taskpack 2.0 manifests, digests, memory, proposals, holds, and workflow static checks"
else
  fail "TaskpackSuite, Taskpack 2.0 manifests, digests, memory, proposals, holds, and workflow static checks"
fi

echo "== router freeze, commit graduation, sync_images, sgl-router staging, and Base Image build recommendations end-to-end =="
if python3 -B - "${ROOT_DIR}" <<'PY'
import json
import os
import re
import shutil
import stat
import subprocess
import sys
import tarfile
import tempfile

root = sys.argv[1]
sys.path.insert(0, os.path.join(root, ".auto-sync", "checks"))
from base_image_builds import recommend_from_diff, validate_proposal_builds
from dockerfile_arg import read_dockerfile_arg


def run(cmd, *, cwd=None, env=None, check=True):
    return subprocess.run(
        cmd,
        cwd=cwd,
        env=env,
        text=True,
        capture_output=True,
        check=check,
    )


def read_json(path: str):
    with open(path, "r", encoding="utf-8") as f:
        return json.load(f)


def write_json(path: str, data) -> None:
    os.makedirs(os.path.dirname(path), exist_ok=True)
    with open(path, "w", encoding="utf-8") as f:
        json.dump(data, f, indent=2)
        f.write("\n")


def git_init_repo(repo_dir: str, env: dict) -> None:
    os.makedirs(repo_dir, exist_ok=True)
    run(["git", "-C", repo_dir, "init", "-q", "-b", "main"], env=env)
    run(["git", "-C", repo_dir, "config", "user.name", "selftest"], env=env)
    run(["git", "-C", repo_dir, "config", "user.email", "selftest@gpustack.local"], env=env)
    run(["git", "-C", repo_dir, "config", "commit.gpgsign", "false"], env=env)


def git_init_commit(repo_dir: str, files: dict[str, str], tag: str | None, msg: str, env: dict) -> str:
    if not os.path.isdir(os.path.join(repo_dir, ".git")):
        git_init_repo(repo_dir, env)
    for rel, content in files.items():
        fp = os.path.join(repo_dir, rel)
        os.makedirs(os.path.dirname(fp), exist_ok=True)
        with open(fp, "w", encoding="utf-8") as f:
            f.write(content)
    run(["git", "-C", repo_dir, "add", "-A"], env=env)
    run(["git", "-C", repo_dir, "commit", "-q", "--allow-empty", "-m", msg], env=env)
    sha = run(["git", "-C", repo_dir, "rev-parse", "HEAD"], env=env).stdout.strip()
    if tag:
        run(["git", "-C", repo_dir, "tag", "-f", tag], env=env)
    return sha


def replace_df_arg(txt: str, arg_name: str, value: str) -> str:
    rendered = '""' if value == "" else value
    out, n = re.subn(
        rf"^(ARG {re.escape(arg_name)}=).*$",
        rf"\g<1>{rendered}",
        txt,
        count=1,
        flags=re.MULTILINE,
    )
    if n != 1:
        raise AssertionError(f"could not replace ARG {arg_name} in Dockerfile")
    return out


def make_isolated_repo(
    dst: str,
    env: dict,
    *,
    llmd_commit: str = "",
    vllm_commit: str = "",
    sgl_router_commit: str = "",
    sidecar_tag: str | None = None,
    holds_doc: dict | None = None,
    llmd_patches: dict[str, str] | None = None,
) -> None:
    """Create an isolated git repository copy decoupled from live *_COMMIT SHAs."""
    for rel in (
        ".auto-sync",
        "pack/llm-router",
        "pkg/worker/settings/value.go",
        "docs/reference/settings.md",
    ):
        src = os.path.join(root, rel)
        out = os.path.join(dst, rel)
        os.makedirs(os.path.dirname(out), exist_ok=True)
        if os.path.isdir(src):
            shutil.copytree(src, out)
        else:
            shutil.copy2(src, out)

    df_path = os.path.join(dst, "pack/llm-router/Dockerfile")
    with open(df_path, "r", encoding="utf-8") as f:
        df_txt = f.read()
    df_txt = replace_df_arg(df_txt, "LLMD_ROUTER_COMMIT", llmd_commit)
    df_txt = replace_df_arg(df_txt, "VLLM_ROUTER_COMMIT", vllm_commit)
    df_txt = replace_df_arg(df_txt, "SGL_ROUTER_COMMIT", sgl_router_commit)
    with open(df_path, "w", encoding="utf-8") as f:
        f.write(df_txt)

    if sidecar_tag is not None:
        new_sidecar = f"gpustack/mirrored-llm-d-router-disagg-sidecar:{sidecar_tag}"
        vg_path = os.path.join(dst, "pkg/worker/settings/value.go")
        with open(vg_path, "r", encoding="utf-8") as f:
            vg_s = f.read()
        vg_s, n1 = re.subn(
            r'(ModelDeploymentRoutingSidecarImage\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\(")([^"]+)("\))',
            rf"\g<1>{new_sidecar}\g<3>",
            vg_s,
            count=1,
            flags=re.DOTALL,
        )
        assert n1 == 1, "failed to rewrite ModelDeploymentRoutingSidecarImage in isolated value.go"
        with open(vg_path, "w", encoding="utf-8") as f:
            f.write(vg_s)

        sm_path = os.path.join(dst, "docs/reference/settings.md")
        with open(sm_path, "r", encoding="utf-8") as f:
            sm_s = f.read()
        sm_s, n2 = re.subn(
            r"^(\| `model-deployment-routing-sidecar-image`<br/>.*?<br/>Default: `)([^`]+)(`)",
            rf"\g<1>{new_sidecar}\g<3>",
            sm_s,
            count=1,
            flags=re.MULTILINE,
        )
        assert n2 == 1, "failed to rewrite model-deployment-routing-sidecar-image in isolated settings.md"
        with open(sm_path, "w", encoding="utf-8") as f:
            f.write(sm_s)

    if holds_doc is not None:
        write_json(os.path.join(dst, ".auto-sync/setting-image-holds.json"), holds_doc)

    if llmd_patches is not None:
        llmd_pdir = os.path.join(dst, "pack/llm-router/patches/llm-d")
        shutil.rmtree(llmd_pdir, ignore_errors=True)
        os.makedirs(llmd_pdir, exist_ok=True)
        for pname, pbody in llmd_patches.items():
            with open(os.path.join(llmd_pdir, pname), "w", encoding="utf-8") as f:
                f.write(pbody)

    # Align isolated proposal versions with the isolated Dockerfile pins.
    prop_path = os.path.join(dst, ".auto-sync/proposals/router.json")
    pdoc = read_json(prop_path)
    pdoc["stage"] = "aligned"
    pdoc["versions"] = {
        "llm-d-router": f"v1.0.0+{llmd_commit[:7]}" if llmd_commit else "v1.0.0",
        "vllm-router": f"v0.1.17+{vllm_commit[:7]}" if vllm_commit else "v0.1.17",
        "sglang-gateway": "gateway-v0.3.1",
        "sgl-router": f"router-v0.1.0+{sgl_router_commit[:7]}" if sgl_router_commit else "router-v0.1.0",
    }
    pdoc["base_image_builds"] = []
    pdoc["sync_images"] = []
    write_json(prop_path, pdoc)

    git_init_repo(dst, env)
    run(["git", "-C", dst, "add", "-A"], env=env)
    run(["git", "-C", dst, "commit", "-q", "-m", "isolated fixture baseline"], env=env)


def make_build_recorder(script_path: str, log_path: str) -> str:
    with open(script_path, "w", encoding="utf-8") as f:
        f.write(
            "#!/usr/bin/env bash\n"
            "set -euo pipefail\n"
            "target=''\n"
            "while (( $# > 0 )); do\n"
            "  if [[ \"$1\" == '--target' && $# -ge 2 ]]; then\n"
            "    target=\"$2\"\n"
            "    shift 2\n"
            "  else\n"
            "    shift\n"
            "  fi\n"
            "done\n"
            f"printf '%s\\n' \"${{target}}\" >> \"{log_path}\"\n"
        )
    os.chmod(script_path, stat.S_IRWXU)
    return script_path


git_env = {
    **os.environ,
    "LC_ALL": "C",
    "GIT_AUTHOR_NAME": "selftest",
    "GIT_AUTHOR_EMAIL": "selftest@gpustack.local",
    "GIT_COMMITTER_NAME": "selftest",
    "GIT_COMMITTER_EMAIL": "selftest@gpustack.local",
}

# Exact minimal context lines matching pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch
SGL_ROUTER_LIB_RS = """// SPDX-FileCopyrightText: Copyright (c) 2026 The SGLang Authors
// SPDX-License-Identifier: Apache-2.0

//! sgl-router: slim KV-aware OpenAI-compatible router for SGLang workers.

pub const VERSION: &str = env!("CARGO_PKG_VERSION");

pub mod buckets_reorg;
pub mod config;
pub mod discovery;
pub mod health;
pub(crate) mod off_worker;
pub mod policies;
pub mod policies_reorg;
"""

SGL_ROUTER_APP_RS = """fn is_infra_path(path: &str) -> bool {
    matches!(
        path,
        "/healthz"
            | "/readyz"
            | "/health"
            | "/liveness"
            | "/readiness"
            | "/health_generate"
            | "/metrics"
    )
}

pub fn build_router_with_body_limit(ctx: Arc<AppContext>, max_body_bytes: usize) -> Router {
    let router = Router::new()
        .route("/healthz", get(crate::server::routes::health::healthz))
        .route("/readyz", get(crate::server::routes::health::readyz))
        // Aliases of the probes above: the engine's `/health`, and the
        // `/liveness` and `/readiness` paths deployments poll.
        .route("/health", get(crate::server::routes::health::healthz))
        .route("/liveness", get(crate::server::routes::health::healthz))
        .route("/readiness", get(crate::server::routes::health::readyz))
        .route("/metrics", get(crate::server::routes::metrics::metrics))
        .route(
            "/v1/models",
            get(crate::server::routes::models::list_models),
        );
    router
}
"""

with tempfile.TemporaryDirectory(prefix="autosync-selftest-e2e.") as tmp:
    # 0. Unit checks for dockerfile_arg.py allow_empty and base_image_builds.py empty *_COMMIT handling.
    sample_df = (
        'ARG LLMD_ROUTER_VERSION=v1.0.0\n'
        'ARG LLMD_ROUTER_COMMIT=""\n'
        'ARG VLLM_ROUTER_COMMIT=\n'
        'ARG SGL_ROUTER_COMMIT=  # cleared\n'
        'ARG EMPTY_SINGLE=\'\'\n'
        'ARG BAD_MULTI="" extra\n'
    )
    assert read_dockerfile_arg(sample_df, "LLMD_ROUTER_VERSION") == "v1.0.0"
    assert read_dockerfile_arg(sample_df, "LLMD_ROUTER_COMMIT", allow_empty=True) == ""
    assert read_dockerfile_arg(sample_df, "VLLM_ROUTER_COMMIT", allow_empty=True) == ""
    assert read_dockerfile_arg(sample_df, "SGL_ROUTER_COMMIT", allow_empty=True) == ""
    assert read_dockerfile_arg(sample_df, "EMPTY_SINGLE", allow_empty=True) == ""
    for strict_bad in ("LLMD_ROUTER_COMMIT", "VLLM_ROUTER_COMMIT", "SGL_ROUTER_COMMIT"):
        try:
            read_dockerfile_arg(sample_df, strict_bad)
        except SystemExit:
            pass
        else:
            raise AssertionError(f"strict read_dockerfile_arg must reject empty {strict_bad}")
    try:
        read_dockerfile_arg(sample_df, "BAD_MULTI", allow_empty=True)
    except SystemExit:
        pass
    else:
        raise AssertionError("read_dockerfile_arg(allow_empty=True) must still reject multi-token ARG")

    df_cli_path = os.path.join(tmp, "sample.Dockerfile")
    with open(df_cli_path, "w", encoding="utf-8") as f:
        f.write(sample_df)
    cli_empty = run(
        ["python3", "-B", os.path.join(root, ".auto-sync/checks/dockerfile_arg.py"), "--allow-empty", df_cli_path, "LLMD_ROUTER_COMMIT"],
        check=False,
    )
    assert cli_empty.returncode == 0 and cli_empty.stdout.strip() == "", f"unexpected CLI --allow-empty output: {cli_empty}"
    cli_strict = run(
        ["python3", "-B", os.path.join(root, ".auto-sync/checks/dockerfile_arg.py"), df_cli_path, "LLMD_ROUTER_COMMIT"],
        check=False,
    )
    assert cli_strict.returncode != 0, "CLI without --allow-empty must reject empty ARG"
    print("PASS  dockerfile_arg.py --allow-empty and strict non-empty parsing")

    # Read current defaults dynamically from value.go so fixture manifests stay decoupled from tag bumps.
    with open(os.path.join(root, "pkg/worker/settings/value.go"), "r", encoding="utf-8") as f:
        vg_txt = f.read()

    def cur_vg_img(var_name: str) -> str:
        m = re.search(
            rf'{re.escape(var_name)}\s*=\s*settings\.NewEditable\([^)]*?setting\.InitializeFromEnv\("([^"]+)"\)',
            vg_txt,
            re.DOTALL,
        )
        assert m, f"missing {var_name} in value.go"
        return m.group(1).strip()

    cur_router_img = cur_vg_img("ModelDeploymentRouterImage")
    cur_proxy_img = cur_vg_img("ModelDeploymentRouterProxyImage")
    cur_sidecar_img = cur_vg_img("ModelDeploymentRoutingSidecarImage")

    def base_manifest_images(extra: dict | None = None) -> dict:
        imgs = {
            cur_router_img: {
                "published": True,
                "amd64Digest": "sha256:" + "1" * 64,
                "arm64Digest": "sha256:" + "2" * 64,
            },
            cur_proxy_img: {
                "published": True,
                "amd64Digest": "sha256:" + "3" * 64,
                "arm64Digest": "sha256:" + "4" * 64,
            },
            cur_sidecar_img: {
                "published": True,
                "amd64Digest": "sha256:" + "5" * 64,
                "arm64Digest": "sha256:" + "6" * 64,
            },
        }
        if extra:
            imgs.update(extra)
        return {"schemaVersion": 1, "images": imgs}

    # 1. Isolated tag-pinned fixture (empty *_COMMIT=""): separate staging, freeze guard, sgl-router drift, stage isolation.
    iso_tag_repo = os.path.join(tmp, "iso-tag-repo")
    make_isolated_repo(iso_tag_repo, git_env, sidecar_tag="v1.0.0")

    fix_dir = os.path.join(tmp, "router-fixture")
    repos_dir = os.path.join(fix_dir, "repos")
    git_init_commit(
        os.path.join(repos_dir, "llm-d-router"),
        {"go.mod": "module example.com/llmd\n\ngo 1.27.0\n"},
        "v1.0.0",
        "v1.0.0",
        git_env,
    )
    vllm_fix_repo = os.path.join(repos_dir, "vllm-router")
    vllm_epd_sha = git_init_commit(
        vllm_fix_repo,
        {"Cargo.toml": '[package]\nname = "vllm-router"\nedition = "2021"\n'},
        "v0.1.17",
        "v0.1.17",
        git_env,
    )
    # Shared sglang fixture repo carrying gateway-v0.3.1, newer gateway-v0.4.0 (must be ignored!),
    # and router-v0.1.0 + router-v0.2.0 (sgl-router + rust subpaths).
    sgl_repo = os.path.join(repos_dir, "sglang")
    git_init_commit(
        sgl_repo,
        {
            "sgl-model-gateway/Cargo.toml": '[package]\nname = "sgl-model-gateway"\nedition = "2021"\n',
            "sgl-router/Cargo.toml": '[package]\nname = "sgl-router"\nedition = "2021"\n',
            "sgl-router/src/lib.rs": SGL_ROUTER_LIB_RS,
            "sgl-router/src/server/app.rs": SGL_ROUTER_APP_RS,
            "rust/Cargo.toml": '[workspace]\nmembers = []\n',
        },
        "gateway-v0.3.1",
        "gateway-v0.3.1",
        git_env,
    )
    run(["git", "-C", sgl_repo, "tag", "-f", "router-v0.1.0"], env=git_env)
    git_init_commit(
        sgl_repo,
        {"sgl-model-gateway/README.md": "deleted later\n"},
        "gateway-v0.4.0",
        "gateway-v0.4.0 (must not unfreeze)",
        git_env,
    )
    git_init_commit(
        sgl_repo,
        {
            "sgl-router/Cargo.toml": '[package]\nname = "sgl-router"\nversion = "0.2.0"\nedition = "2021"\n',
            "rust/Cargo.toml": '[workspace]\nmembers = []\n# router-v0.2.0\n',
        },
        "router-v0.2.0",
        "router-v0.2.0",
        git_env,
    )

    sidecar_v100 = "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.0"
    manifests_path = os.path.join(fix_dir, "image-manifests.json")
    write_json(
        manifests_path,
        base_manifest_images({
            sidecar_v100: {
                "published": True,
                "amd64Digest": "sha256:" + "7" * 64,
                "arm64Digest": "sha256:" + "8" * 64,
            }
        }),
    )

    stage_dir = os.path.join(tmp, "stage-router")
    staging_dir = os.path.join(tmp, "preflight-staging")
    preflight_out = os.path.join(tmp, "preflight-out.json")
    pf_env = {
        **os.environ,
        "AUTOSYNC_STAGE_DIR": stage_dir,
        "AUTOSYNC_ROUTER_FIXTURE_DIR": fix_dir,
        "AUTOSYNC_ROUTER_EPD_COMMIT": vllm_epd_sha,
        "AUTOSYNC_SKIP_GO_MOD_DOWNLOAD": "true",
        "AUTOSYNC_SKIP_LFS_FETCH": "true",
        "AUTOSYNC_SKIP_GO_TEST": "true",
        "LOOPWRIGHT_STAGING_DIR": staging_dir,
        "LOOPWRIGHT_PREFLIGHT_OUT": preflight_out,
    }
    pf_res = run(
        ["bash", os.path.join(iso_tag_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_tag_repo,
        env=pf_env,
        check=False,
    )
    if pf_res.returncode != 0:
        raise AssertionError(
            f"preflight-router.sh failed (rc={pf_res.returncode}):\n{pf_res.stdout}\n{pf_res.stderr}"
        )

    gw_stage = os.path.join(stage_dir, "sglang-gateway")
    sr_stage = os.path.join(stage_dir, "sgl-router")
    if not os.path.isdir(os.path.join(gw_stage, ".git")):
        raise AssertionError(f"expected separate staged repo at {gw_stage}")
    if not os.path.isdir(os.path.join(sr_stage, ".git")):
        raise AssertionError(f"expected separate staged repo at {sr_stage}")
    if os.path.exists(os.path.join(stage_dir, "sglang")):
        raise AssertionError("legacy single-repo stage dir $STAGE_DIR/sglang must not be created")

    rep = read_json(os.path.join(staging_dir, "router-preflight-report.json"))
    if rep.get("stage") != "recipe_sync":
        raise AssertionError(f"expected stage=recipe_sync, got {rep.get('stage')!r}")
    if rep["target_tags"]["sglang-gateway"] != "gateway-v0.3.1":
        raise AssertionError(
            f"frozen sglang-gateway must stay gateway-v0.3.1, got {rep['target_tags']['sglang-gateway']!r}"
        )
    if rep["stage_a_drifts"]["sglang-gateway"] is not False:
        raise AssertionError("frozen sglang-gateway must report stage_a_drifts=False")
    if rep["target_tags"]["sgl-router"] != "router-v0.2.0" or rep["stage_a_drifts"]["sgl-router"] is not True:
        raise AssertionError(f"expected sgl-router drift to router-v0.2.0, got {rep['target_tags']!r}")
    if rep.get("sync_images") != []:
        raise AssertionError(f"expected empty sync_images when sidecar already at v1.0.0, got {rep.get('sync_images')!r}")

    pf_builds = rep.get("base_image_builds")
    if not isinstance(pf_builds, list) or len(pf_builds) != 1:
        raise AssertionError(f"expected 1 base_image_builds entry in router report, got {pf_builds!r}")
    b0 = pf_builds[0]
    if (
        b0.get("repository") != "gpustack/llm-router"
        or b0.get("target") != ""
        or not b0.get("recommended_tag")
        or 'gh workflow run "Base Image" -f repository=llm-router -f tag=' not in b0.get("command", "")
    ):
        raise AssertionError(f"unexpected router base_image_builds entry: {b0!r}")

    pf_summary = read_json(os.path.join(staging_dir, "router-preflight-summary.json"))
    if (pf_summary.get("highlights") or {}).get("base_image_builds") != pf_builds:
        raise AssertionError("router-preflight-summary.json highlights.base_image_builds mismatch")
    if (pf_summary.get("highlights") or {}).get("sync_images") != []:
        raise AssertionError("router-preflight-summary.json highlights.sync_images mismatch")
    print("PASS  preflight-router.sh separate sglang-gateway/sgl-router staging, empty *_COMMIT, freeze skip, and base_image_builds")

    # 1b. Negative: upstream override for frozen sglang-gateway must fail loudly.
    fix_dir_bad = os.path.join(tmp, "router-fixture-bad-override")
    shutil.copytree(fix_dir, fix_dir_bad)
    write_json(os.path.join(fix_dir_bad, "upstream.json"), {"sglang-gateway": "gateway-v0.4.0"})
    pf_bad = run(
        ["bash", os.path.join(iso_tag_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_tag_repo,
        env={**pf_env, "AUTOSYNC_ROUTER_FIXTURE_DIR": fix_dir_bad},
        check=False,
    )
    if pf_bad.returncode == 0 or "sglang-gateway is frozen" not in pf_bad.stderr:
        raise AssertionError(
            f"expected preflight-router.sh to reject sglang-gateway upstream override; rc={pf_bad.returncode}, stderr={pf_bad.stderr!r}"
        )
    print("PASS  preflight-router.sh rejects frozen sglang-gateway upstream override")

    # 2. validate_router.sh positive + negative freeze, stage-diff isolation, and base_image_builds tests.
    def copy_candidate_tree(src_repo: str, dst: str) -> None:
        for rel in (
            "pack/llm-router",
            "pkg/worker/settings/value.go",
            "docs/reference/settings.md",
            ".auto-sync/checks",
            ".auto-sync/proposals/router.json",
            ".auto-sync/setting-image-holds.json",
        ):
            src = os.path.join(src_repo, rel)
            out = os.path.join(dst, rel)
            os.makedirs(os.path.dirname(out), exist_ok=True)
            if os.path.isdir(src):
                shutil.copytree(src, out)
            else:
                shutil.copy2(src, out)

    val_stage = os.path.join(tmp, "val-stage")
    os.makedirs(val_stage, exist_ok=True)
    shutil.copytree(sr_stage, os.path.join(val_stage, "sgl-router"))
    shutil.copy2(os.path.join(staging_dir, "router-preflight-report.json"), os.path.join(val_stage, "router-preflight-report.json"))
    shutil.copy2(os.path.join(staging_dir, "image-manifests.json"), os.path.join(val_stage, "image-manifests.json"))
    val_base = os.path.join(val_stage, "baseline")
    copy_candidate_tree(iso_tag_repo, val_base)

    build_log_tag = os.path.join(tmp, "build-targets-tag.log")
    build_rec_tag = make_build_recorder(os.path.join(tmp, "record-build-tag.sh"), build_log_tag)

    def run_validate_router(cand_dir: str, *, stage_p: str = val_stage, base_p: str = val_base, runner: str = "true"):
        env = {
            **os.environ,
            "AUTOSYNC_STAGE_DIR": stage_p,
            "LOOPWRIGHT_BASE_DIR": base_p,
            "LOOPWRIGHT_CANDIDATE_DIR": cand_dir,
            "AUTOSYNC_FAST_SELFTEST": "true",
            "AUTOSYNC_ALLOW_MISSING_STAGED_REPO": "true",
            "AUTOSYNC_ROUTER_BUILD_RUNNER": runner,
        }
        return run(
            ["bash", os.path.join(root, ".auto-sync/checks/validate_router.sh")],
            cwd=cand_dir,
            env=env,
            check=False,
        )

    # 2a. Positive: sgl-router bump to router-v0.2.0 + valid base_image_builds passes and builds ONLY sgl-router-builder.
    cand_ok = os.path.join(tmp, "cand-router-ok")
    copy_candidate_tree(iso_tag_repo, cand_ok)
    df_ok = os.path.join(cand_ok, "pack/llm-router/Dockerfile")
    with open(df_ok, "r", encoding="utf-8") as f:
        df_txt = f.read()
    with open(df_ok, "w", encoding="utf-8") as f:
        f.write(df_txt.replace("ARG SGL_ROUTER_VERSION=router-v0.1.0", "ARG SGL_ROUTER_VERSION=router-v0.2.0", 1))
    prop_ok = os.path.join(cand_ok, ".auto-sync/proposals/router.json")
    pdoc = read_json(prop_ok)
    pdoc["stage"] = "recipe_sync"
    pdoc["versions"]["sgl-router"] = "router-v0.2.0"
    pdoc["base_image_builds"] = pf_builds
    pdoc["sync_images"] = []
    write_json(prop_ok, pdoc)
    res_ok = run_validate_router(cand_ok, runner=build_rec_tag)
    if res_ok.returncode != 0:
        raise AssertionError(f"valid sgl-router candidate failed validate_router.sh:\n{res_ok.stdout}\n{res_ok.stderr}")
    built_targets = [ln.strip() for ln in open(build_log_tag, "r", encoding="utf-8").read().splitlines() if ln.strip()]
    if built_targets != ["sgl-router-builder"]:
        raise AssertionError(f"expected only ['sgl-router-builder'] built, got {built_targets!r}")
    print("PASS  validate_router.sh accepts sgl-router bump with stage-diff isolation (sgl-router-builder only)")

    # 2b. Negative: modifying SGLANG_GATEWAY_VERSION must fail validate_router.sh.
    cand_unfreeze = os.path.join(tmp, "cand-router-unfreeze")
    shutil.copytree(cand_ok, cand_unfreeze)
    df_unf = os.path.join(cand_unfreeze, "pack/llm-router/Dockerfile")
    with open(df_unf, "r", encoding="utf-8") as f:
        t = f.read()
    with open(df_unf, "w", encoding="utf-8") as f:
        f.write(t.replace("ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1", "ARG SGLANG_GATEWAY_VERSION=gateway-v0.4.0", 1))
    res_unf = run_validate_router(cand_unfreeze)
    if res_unf.returncode == 0 or "SGLANG_GATEWAY_VERSION is frozen" not in res_unf.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject SGLANG_GATEWAY_VERSION change; rc={res_unf.returncode}, stderr={res_unf.stderr!r}"
        )
    print("PASS  validate_router.sh rejects SGLANG_GATEWAY_VERSION modification")

    # 2c. Negative: modifying pack/llm-router/patches/sglang-gateway/* must fail validate_router.sh.
    cand_gw_patch = os.path.join(tmp, "cand-router-gw-patch")
    shutil.copytree(cand_ok, cand_gw_patch)
    with open(
        os.path.join(cand_gw_patch, "pack/llm-router/patches/sglang-gateway/0002-observer-read-only-endpoint-view.patch"),
        "a",
        encoding="utf-8",
    ) as f:
        f.write("\n# unauthorized edit\n")
    res_gwp = run_validate_router(cand_gw_patch)
    if res_gwp.returncode == 0 or "pack/llm-router/patches/sglang-gateway/ is frozen" not in res_gwp.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject sglang-gateway patch edit; rc={res_gwp.returncode}, stderr={res_gwp.stderr!r}"
        )
    print("PASS  validate_router.sh rejects pack/llm-router/patches/sglang-gateway/ modification")

    # 2d. Negative: missing/empty base_image_builds on modified pack/llm-router/Dockerfile must fail.
    cand_no_builds = os.path.join(tmp, "cand-router-no-builds")
    shutil.copytree(cand_ok, cand_no_builds)
    prop_nb = os.path.join(cand_no_builds, ".auto-sync/proposals/router.json")
    pdoc_nb = read_json(prop_nb)
    pdoc_nb["base_image_builds"] = []
    write_json(prop_nb, pdoc_nb)
    res_nb = run_validate_router(cand_no_builds)
    if res_nb.returncode == 0 or "missing required build recommendation" not in res_nb.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject empty base_image_builds; rc={res_nb.returncode}, stderr={res_nb.stderr!r}"
        )
    print("PASS  validate_router.sh rejects missing base_image_builds when pack/llm-router changes")

    # 2e. Negative: cross-domain extraneous base_image_builds entry on router must fail validate_router.sh.
    cand_extra_builds = os.path.join(tmp, "cand-router-extra-builds")
    shutil.copytree(cand_ok, cand_extra_builds)
    prop_eb = os.path.join(cand_extra_builds, ".auto-sync/proposals/router.json")
    pdoc_eb = read_json(prop_eb)
    pdoc_eb["base_image_builds"] = pf_builds + [
        {
            "repository": "gpustack/mirrored-mooncake",
            "target": "",
            "recommended_tag": "0.3.15-cpu",
            "reason": "extraneous cross-domain entry",
            "command": 'gh workflow run "Base Image" -f repository=mirrored-mooncake -f tag=0.3.15-cpu',
        }
    ]
    write_json(prop_eb, pdoc_eb)
    res_eb = run_validate_router(cand_extra_builds)
    if res_eb.returncode == 0 or "unexpected build recommendation" not in res_eb.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject cross-domain base_image_builds; rc={res_eb.returncode}, stderr={res_eb.stderr!r}"
        )
    print("PASS  validate_router.sh rejects cross-domain extraneous base_image_builds")

    # 2f. Negative: AUTOSYNC_ALLOW_MISSING_STAGED_REPO fallback rejects sgl-router patch missing lib.rs/app.rs hooks.
    val_stage_nostage = os.path.join(tmp, "val-stage-nostage")
    os.makedirs(val_stage_nostage, exist_ok=True)
    shutil.copy2(os.path.join(staging_dir, "router-preflight-report.json"), os.path.join(val_stage_nostage, "router-preflight-report.json"))
    shutil.copy2(os.path.join(staging_dir, "image-manifests.json"), os.path.join(val_stage_nostage, "image-manifests.json"))
    cand_bad_fallback = os.path.join(tmp, "cand-router-bad-fallback")
    shutil.copytree(cand_ok, cand_bad_fallback)
    with open(
        os.path.join(cand_bad_fallback, "pack/llm-router/patches/sgl-router/0001-observer-endpoints.patch"),
        "w",
        encoding="utf-8",
    ) as f:
        f.write("+pub struct ObserverRouter {}\n")
    res_bf = run_validate_router(cand_bad_fallback, stage_p=val_stage_nostage, base_p=val_base)
    if res_bf.returncode == 0 or "missing observer module/endpoint hooks" not in res_bf.stderr:
        raise AssertionError(
            f"expected validate_router.sh fallback to reject truncated sgl-router patch; rc={res_bf.returncode}, stderr={res_bf.stderr!r}"
        )
    print("PASS  validate_router.sh missing-stage fallback rejects sgl-router patch without lib.rs/app.rs hooks")

    # 2g. Negative: adding SGLANG_GATEWAY_COMMIT must fail validate_router.sh.
    cand_gw_commit = os.path.join(tmp, "cand-router-gw-commit")
    shutil.copytree(cand_ok, cand_gw_commit)
    df_gwc = os.path.join(cand_gw_commit, "pack/llm-router/Dockerfile")
    with open(df_gwc, "r", encoding="utf-8") as f:
        t = f.read()
    with open(df_gwc, "w", encoding="utf-8") as f:
        f.write(
            t.replace(
                "ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1",
                "ARG SGLANG_GATEWAY_VERSION=gateway-v0.3.1\nARG SGLANG_GATEWAY_COMMIT=deadbeef",
                1,
            )
        )
    res_gwc = run_validate_router(cand_gw_commit)
    if res_gwc.returncode == 0 or "SGLANG_GATEWAY_COMMIT is forbidden" not in res_gwc.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject SGLANG_GATEWAY_COMMIT; rc={res_gwc.returncode}, stderr={res_gwc.stderr!r}"
        )
    print("PASS  validate_router.sh rejects SGLANG_GATEWAY_COMMIT")

    # 2h. Negative: introducing a new *_COMMIT when baseline *_COMMIT="" must fail closed.
    cand_new_commit = os.path.join(tmp, "cand-router-new-commit")
    shutil.copytree(cand_ok, cand_new_commit)
    df_nc = os.path.join(cand_new_commit, "pack/llm-router/Dockerfile")
    with open(df_nc, "r", encoding="utf-8") as f:
        t = f.read()
    t = replace_df_arg(t, "LLMD_ROUTER_COMMIT", "1234567890abcdef")
    with open(df_nc, "w", encoding="utf-8") as f:
        f.write(t)
    res_nc = run_validate_router(cand_new_commit)
    if res_nc.returncode == 0 or "introduced a new pinned commit" not in res_nc.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject new *_COMMIT from tag-pinned baseline; rc={res_nc.returncode}, stderr={res_nc.stderr!r}"
        )
    print("PASS  validate_router.sh rejects candidate-introduced *_COMMIT from tag-pinned baseline")

    # 3. Ancestry-gated commit graduation: non-ancestor hold, anti-downgrade rejection, ancestor graduation + patch rebase, and sync_images.
    grad_fix = os.path.join(tmp, "grad-fixture")
    grad_repos = os.path.join(grad_fix, "repos")
    llmd_grad_repo = os.path.join(grad_repos, "llm-d-router")
    # v1.0.0 baseline on main
    git_init_commit(
        llmd_grad_repo,
        {
            "go.mod": "module example.com/llmd\n\ngo 1.27.0\n",
            "pkg/epp/observer_target.go": "package epp\n\nfunc Version() string {\n\treturn \"v1.0.0\"\n}\n",
        },
        "v1.0.0",
        "v1.0.0",
        git_env,
    )
    # Branch release-1.0 off v1.0.0 and cut patch tag v1.0.1 (NOT containing main's pinned commit!)
    run(["git", "-C", llmd_grad_repo, "checkout", "-q", "-b", "release-1.0", "v1.0.0"], env=git_env)
    v101_sha = git_init_commit(
        llmd_grad_repo,
        {"pkg/epp/release_fix.go": "package epp\n\nconst patchFix = true\n"},
        "v1.0.1",
        "v1.0.1 release-branch patch",
        git_env,
    )
    # Switch back to main and create post-v1.0.0 PINNED_COMMIT with distinct context (so patch ONLY applies on PINNED_COMMIT, not v1.0.0!)
    run(["git", "-C", llmd_grad_repo, "checkout", "-q", "main"], env=git_env)
    pinned_llmd_sha = git_init_commit(
        llmd_grad_repo,
        {
            "pkg/epp/observer_target.go": (
                "package epp\n\n"
                "// post-v1.0.0 main refactor\n"
                "func Version() string {\n"
                "\treturn \"main-feature\"\n"
                "}\n\n"
                "func Tail() int {\n"
                "\treturn 1\n"
                "}\n"
            )
        },
        None,
        "post-v1.0.0 main commit",
        git_env,
    )
    # Build a real patch against pinned_llmd_sha (fails on v1.0.0, applies cleanly on pinned_llmd_sha).
    llmd_test_patch = (
        "Add observer marker on post-v1.0.0 main.\n\n"
        f"AGAINST: llm-d/llm-d-router commit {pinned_llmd_sha} (post-v1.0.0 main).\n\n"
        "diff --git a/pkg/epp/observer_target.go b/pkg/epp/observer_target.go\n"
        "--- a/pkg/epp/observer_target.go\n"
        "+++ b/pkg/epp/observer_target.go\n"
        "@@ -2,5 +2,6 @@ package epp\n"
        " \n"
        " // post-v1.0.0 main refactor\n"
        " func Version() string {\n"
        "+\t// observer hook\n"
        " \treturn \"main-feature\"\n"
        " }\n"
    )

    # Copy vllm-router and sglang fixture repos (keep sgl-router at router-v0.1.0 so only llm-d-router varies).
    shutil.copytree(vllm_fix_repo, os.path.join(grad_repos, "vllm-router"))
    sgl_grad_repo = os.path.join(grad_repos, "sglang")
    git_init_commit(
        sgl_grad_repo,
        {
            "sgl-model-gateway/Cargo.toml": '[package]\nname = "sgl-model-gateway"\nedition = "2021"\n',
            "sgl-router/Cargo.toml": '[package]\nname = "sgl-router"\nedition = "2021"\n',
            "sgl-router/src/lib.rs": SGL_ROUTER_LIB_RS,
            "sgl-router/src/server/app.rs": SGL_ROUTER_APP_RS,
            "rust/Cargo.toml": '[workspace]\nmembers = []\n',
        },
        "gateway-v0.3.1",
        "gateway-v0.3.1",
        git_env,
    )
    run(["git", "-C", sgl_grad_repo, "tag", "-f", "router-v0.1.0"], env=git_env)

    iso_commit_repo = os.path.join(tmp, "iso-commit-repo")
    make_isolated_repo(
        iso_commit_repo,
        git_env,
        llmd_commit=pinned_llmd_sha,
        sidecar_tag="v1.0.0",
        llmd_patches={"0001-observer-test.patch": llmd_test_patch},
    )

    # 3a. Non-ancestor newer patch tag (v1.0.1) holds llm-d-router without downgrading, AND emits sync_images for unmirrored v1.0.1 sidecar.
    grad_manifests_missing = os.path.join(grad_fix, "image-manifests.json")
    write_json(
        grad_manifests_missing,
        base_manifest_images({
            sidecar_v100: {
                "published": True,
                "amd64Digest": "sha256:" + "7" * 64,
                "arm64Digest": "sha256:" + "8" * 64,
            }
        }),
    )
    hold_stage = os.path.join(tmp, "stage-hold")
    hold_staging = os.path.join(tmp, "staging-hold")
    hold_out = os.path.join(tmp, "out-hold.json")
    hold_res = run(
        ["bash", os.path.join(iso_commit_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_commit_repo,
        env={
            **pf_env,
            "AUTOSYNC_STAGE_DIR": hold_stage,
            "AUTOSYNC_ROUTER_FIXTURE_DIR": grad_fix,
            "LOOPWRIGHT_STAGING_DIR": hold_staging,
            "LOOPWRIGHT_PREFLIGHT_OUT": hold_out,
        },
        check=False,
    )
    if hold_res.returncode != 0:
        raise AssertionError(f"preflight-router.sh (non-ancestor hold) failed:\n{hold_res.stdout}\n{hold_res.stderr}")

    hold_rep = read_json(os.path.join(hold_staging, "router-preflight-report.json"))
    hold_sum = read_json(os.path.join(hold_staging, "router-preflight-summary.json"))
    hold_recs = {r["id"]: r for r in read_json(os.path.join(hold_stage, "upstream-records.json"))}
    llmd_grad_info = (hold_rep.get("graduation") or {}).get("llm-d-router") or {}
    if (
        hold_rep.get("stage") != "no_change"
        or hold_rep["stage_a_drifts"]["llm-d-router"] is not False
        or llmd_grad_info.get("graduation_status") != "awaiting_tag_containing_commit"
        or llmd_grad_info.get("is_ancestor") is not False
        or llmd_grad_info.get("pinned_commit") != pinned_llmd_sha
        or llmd_grad_info.get("latest_tag") != "v1.0.1"
    ):
        raise AssertionError(f"expected non-ancestor hold for llm-d-router, got report={hold_rep!r}")
    if (
        hold_recs["llm-d-router"].get("status") != "held_back"
        or hold_recs["llm-d-router"].get("version") != f"v1.0.0+{pinned_llmd_sha[:7]}"
    ):
        raise AssertionError(f"expected held_back upstream record with <ver>+<short7>, got {hold_recs['llm-d-router']!r}")
    if (hold_sum.get("highlights") or {}).get("graduation") != hold_rep.get("graduation"):
        raise AssertionError("router-preflight-summary.json missing highlights.graduation")
    expected_v101_cmd = 'gh workflow run "Sync Image" -f images="ghcr.io/llm-d/llm-d-router-disagg-sidecar:v1.0.1"'
    hold_sync = hold_rep.get("sync_images")
    if (
        not isinstance(hold_sync, list)
        or len(hold_sync) != 1
        or hold_sync[0].get("source") != "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v1.0.1"
        or hold_sync[0].get("destination") != "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.0.1"
        or hold_sync[0].get("command") != expected_v101_cmd
        or (hold_sum.get("highlights") or {}).get("sync_images") != hold_sync
    ):
        raise AssertionError(f"expected sync_images for unmirrored v1.0.1 sidecar, got {hold_sync!r}")
    print("PASS  non-ancestor patch tag v1.0.1 holds commit-pinned llm-d-router without downgrading")

    # 3b. Negative: manual candidate clearing or modifying *_COMMIT onto non-ancestor tag fails closed in validate_router.sh.
    hold_val_stage = os.path.join(tmp, "val-stage-hold")
    os.makedirs(hold_val_stage, exist_ok=True)
    shutil.copytree(os.path.join(hold_stage, "llm-d-router"), os.path.join(hold_val_stage, "llm-d-router"))
    shutil.copy2(os.path.join(hold_staging, "router-preflight-report.json"), os.path.join(hold_val_stage, "router-preflight-report.json"))
    shutil.copy2(os.path.join(hold_staging, "image-manifests.json"), os.path.join(hold_val_stage, "image-manifests.json"))
    hold_val_base = os.path.join(hold_val_stage, "baseline")
    copy_candidate_tree(iso_commit_repo, hold_val_base)

    # Positive check: unchanged commit-pinned candidate passes validate_router.sh (with real patch apply on pinned_llmd_sha!).
    cand_held_ok = os.path.join(tmp, "cand-held-ok")
    copy_candidate_tree(iso_commit_repo, cand_held_ok)
    p_held = read_json(os.path.join(cand_held_ok, ".auto-sync/proposals/router.json"))
    p_held["sync_images"] = hold_sync
    write_json(os.path.join(cand_held_ok, ".auto-sync/proposals/router.json"), p_held)
    res_held_ok = run_validate_router(cand_held_ok, stage_p=hold_val_stage, base_p=hold_val_base)
    if res_held_ok.returncode != 0:
        raise AssertionError(f"commit-pinned candidate failed validate_router.sh:\n{res_held_ok.stdout}\n{res_held_ok.stderr}")

    # Negative 1: manually clearing LLMD_ROUTER_COMMIT="" onto non-ancestor v1.0.1 must fail closed.
    cand_downgrade = os.path.join(tmp, "cand-downgrade")
    shutil.copytree(cand_held_ok, cand_downgrade)
    df_dg = os.path.join(cand_downgrade, "pack/llm-router/Dockerfile")
    with open(df_dg, "r", encoding="utf-8") as f:
        t = f.read()
    t = replace_df_arg(t, "LLMD_ROUTER_VERSION", "v1.0.1")
    t = replace_df_arg(t, "LLMD_ROUTER_COMMIT", "")
    with open(df_dg, "w", encoding="utf-8") as f:
        f.write(t)
    res_dg = run_validate_router(cand_downgrade, stage_p=hold_val_stage, base_p=hold_val_base)
    if res_dg.returncode == 0 or "anti-downgrade gate" not in res_dg.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject non-ancestor commit clear; rc={res_dg.returncode}, stderr={res_dg.stderr!r}"
        )

    # Negative 2: manually changing LLMD_ROUTER_COMMIT to another SHA must fail closed.
    cand_swap_sha = os.path.join(tmp, "cand-swap-sha")
    shutil.copytree(cand_held_ok, cand_swap_sha)
    df_sw = os.path.join(cand_swap_sha, "pack/llm-router/Dockerfile")
    with open(df_sw, "r", encoding="utf-8") as f:
        t = f.read()
    t = replace_df_arg(t, "LLMD_ROUTER_COMMIT", v101_sha)
    with open(df_sw, "w", encoding="utf-8") as f:
        f.write(t)
    res_sw = run_validate_router(cand_swap_sha, stage_p=hold_val_stage, base_p=hold_val_base)
    if res_sw.returncode == 0 or "modified pinned commit" not in res_sw.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject commit SHA modification; rc={res_sw.returncode}, stderr={res_sw.stderr!r}"
        )

    # Negative 3: mutating *_VERSION while keeping non-empty *_COMMIT unchanged must fail closed.
    cand_mut_ver = os.path.join(tmp, "cand-mut-ver-same-commit")
    shutil.copytree(cand_held_ok, cand_mut_ver)
    df_mv = os.path.join(cand_mut_ver, "pack/llm-router/Dockerfile")
    with open(df_mv, "r", encoding="utf-8") as f:
        t = f.read()
    t = replace_df_arg(t, "LLMD_ROUTER_VERSION", "v1.0.1")
    with open(df_mv, "w", encoding="utf-8") as f:
        f.write(t)
    res_mv = run_validate_router(cand_mut_ver, stage_p=hold_val_stage, base_p=hold_val_base)
    if res_mv.returncode == 0 or "while pinned commit" not in res_mv.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject *_VERSION mutation with unchanged *_COMMIT; rc={res_mv.returncode}, stderr={res_mv.stderr!r}"
        )

    # Negative 4: forged preflight report claiming is_ancestor=true against real non-ancestor staged repo must fail closed.
    forged_val_stage = os.path.join(tmp, "val-stage-forged-report")
    shutil.copytree(hold_val_stage, forged_val_stage)
    forged_rep_path = os.path.join(forged_val_stage, "router-preflight-report.json")
    forged_rep = read_json(forged_rep_path)
    forged_rep["graduation"]["llm-d-router"]["is_ancestor"] = True
    forged_rep["graduation"]["llm-d-router"]["graduation_status"] = "ready_to_graduate"
    forged_rep["graduation"]["llm-d-router"]["latest_tag"] = "v1.0.1"
    write_json(forged_rep_path, forged_rep)
    res_forged = run_validate_router(cand_downgrade, stage_p=forged_val_stage, base_p=hold_val_base)
    if res_forged.returncode == 0 or "git merge-base --is-ancestor" not in res_forged.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject forged report via real git merge-base check; rc={res_forged.returncode}, stderr={res_forged.stderr!r}"
        )

    # Negative 5: missing sync_images when preflight emitted one must fail closed.
    cand_missing_sync = os.path.join(tmp, "cand-missing-sync")
    shutil.copytree(cand_held_ok, cand_missing_sync)
    p_ms = read_json(os.path.join(cand_missing_sync, ".auto-sync/proposals/router.json"))
    p_ms["sync_images"] = []
    write_json(os.path.join(cand_missing_sync, ".auto-sync/proposals/router.json"), p_ms)
    res_ms = run_validate_router(cand_missing_sync, stage_p=hold_val_stage, base_p=hold_val_base)
    if res_ms.returncode == 0 or "sync_images" not in res_ms.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject missing sync_images; rc={res_ms.returncode}, stderr={res_ms.stderr!r}"
        )
    print("PASS  validate_router.sh enforces anti-downgrade commit gate, forged-report check, and sync_images parity")

    # 3c. Positive: ancestor release tag v1.1.0 (containing pinned_llmd_sha) graduates cleanly,
    # pre-stages LLMD_ROUTER_VERSION=v1.1.0 + LLMD_ROUTER_COMMIT="", rebases patch from pinned_llmd_sha,
    # builds ONLY epp-builder, and emits sync_images for unmirrored v1.1.0 sidecar.
    run(["git", "-C", llmd_grad_repo, "checkout", "-q", "main"], env=git_env)
    git_init_commit(
        llmd_grad_repo,
        {
            "pkg/epp/observer_target.go": (
                "package epp\n\n"
                "// post-v1.0.0 main refactor\n"
                "func Version() string {\n"
                "\treturn \"main-feature\"\n"
                "}\n\n"
                "func Tail() int {\n"
                "\treturn 2 // bumped in v1.1.0\n"
                "}\n"
            ),
            "pkg/epp/v110_new.go": "package epp\n\nconst releaseV110 = true\n",
        },
        "v1.1.0",
        "v1.1.0 release containing pinned commit",
        git_env,
    )

    grad_stage = os.path.join(tmp, "stage-grad")
    grad_staging = os.path.join(tmp, "staging-grad")
    grad_out = os.path.join(tmp, "out-grad.json")
    grad_res = run(
        ["bash", os.path.join(iso_commit_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_commit_repo,
        env={
            **pf_env,
            "AUTOSYNC_STAGE_DIR": grad_stage,
            "AUTOSYNC_ROUTER_FIXTURE_DIR": grad_fix,
            "LOOPWRIGHT_STAGING_DIR": grad_staging,
            "LOOPWRIGHT_PREFLIGHT_OUT": grad_out,
        },
        check=False,
    )
    if grad_res.returncode != 0:
        raise AssertionError(f"preflight-router.sh (ancestor graduation) failed:\n{grad_res.stdout}\n{grad_res.stderr}")

    grad_rep = read_json(os.path.join(grad_staging, "router-preflight-report.json"))
    grad_sum = read_json(os.path.join(grad_staging, "router-preflight-summary.json"))
    grad_recs = {r["id"]: r for r in read_json(os.path.join(grad_stage, "upstream-records.json"))}
    g_llmd = (grad_rep.get("graduation") or {}).get("llm-d-router") or {}
    if (
        grad_rep.get("stage") != "recipe_sync"
        or grad_rep["stage_a_drifts"]["llm-d-router"] is not True
        or g_llmd.get("graduation_status") != "ready_to_graduate"
        or g_llmd.get("is_ancestor") is not True
        or g_llmd.get("latest_tag") != "v1.1.0"
        or (grad_rep.get("rebase") or {}).get("llm-d-router", {}).get("auto_rebased") is not True
    ):
        raise AssertionError(f"expected ready_to_graduate + auto_rebased for llm-d-router, got {grad_rep!r}")
    if (
        grad_recs["llm-d-router"].get("status") != "available"
        or grad_recs["llm-d-router"].get("version") != "v1.1.0"
        or grad_recs["llm-d-router"].get("currentVersion") != f"v1.0.0+{pinned_llmd_sha[:7]}"
    ):
        raise AssertionError(f"unexpected graduated upstream record: {grad_recs['llm-d-router']!r}")

    expected_v110_cmd = 'gh workflow run "Sync Image" -f images="ghcr.io/llm-d/llm-d-router-disagg-sidecar:v1.1.0"'
    grad_sync = grad_rep.get("sync_images")
    if (
        not isinstance(grad_sync, list)
        or len(grad_sync) != 1
        or grad_sync[0].get("source") != "ghcr.io/llm-d/llm-d-router-disagg-sidecar:v1.1.0"
        or grad_sync[0].get("destination") != "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.1.0"
        or grad_sync[0].get("command") != expected_v110_cmd
        or (grad_sum.get("highlights") or {}).get("sync_images") != grad_sync
    ):
        raise AssertionError(f"expected sync_images for v1.1.0 sidecar, got {grad_sync!r}")

    overlay_tgz = os.path.join(grad_staging, "candidate-overlay.tgz")
    if not os.path.isfile(overlay_tgz):
        raise AssertionError("expected candidate-overlay.tgz for graduated llm-d-router")

    # Apply pre-staged overlay into candidate and validate with stage-diff build recorder.
    grad_val_stage = os.path.join(tmp, "val-stage-grad")
    os.makedirs(grad_val_stage, exist_ok=True)
    shutil.copytree(os.path.join(grad_stage, "llm-d-router"), os.path.join(grad_val_stage, "llm-d-router"))
    shutil.copy2(os.path.join(grad_staging, "router-preflight-report.json"), os.path.join(grad_val_stage, "router-preflight-report.json"))
    shutil.copy2(os.path.join(grad_staging, "image-manifests.json"), os.path.join(grad_val_stage, "image-manifests.json"))
    grad_val_base = os.path.join(grad_val_stage, "baseline")
    copy_candidate_tree(iso_commit_repo, grad_val_base)

    cand_grad = os.path.join(tmp, "cand-grad-ok")
    copy_candidate_tree(iso_commit_repo, cand_grad)
    with tarfile.open(overlay_tgz, "r:gz") as tf:
        tf.extractall(cand_grad)

    grad_df_txt = open(os.path.join(cand_grad, "pack/llm-router/Dockerfile"), "r", encoding="utf-8").read()
    assert read_dockerfile_arg(grad_df_txt, "LLMD_ROUTER_VERSION") == "v1.1.0"
    assert read_dockerfile_arg(grad_df_txt, "LLMD_ROUTER_COMMIT", allow_empty=True) == ""
    grad_patch_txt = open(os.path.join(cand_grad, "pack/llm-router/patches/llm-d/0001-observer-test.patch"), "r", encoding="utf-8").read()
    assert "AGAINST: llm-d/llm-d-router v1.1.0." in grad_patch_txt, f"expected AGAINST updated to v1.1.0: {grad_patch_txt[:200]}"

    # Check base_image_builds diff handles cleared LLMD_ROUTER_COMMIT="" cleanly without reporting "removed".
    grad_diff_builds = recommend_from_diff(grad_val_base, cand_grad, domain="router")
    assert len(grad_diff_builds) == 1 and 'LLMD_ROUTER_COMMIT=""' in grad_diff_builds[0]["reason"], (
        f"unexpected base_image_builds reason on commit graduation: {grad_diff_builds!r}"
    )

    p_grad = read_json(os.path.join(cand_grad, ".auto-sync/proposals/router.json"))
    p_grad["stage"] = "recipe_sync"
    p_grad["versions"]["llm-d-router"] = "v1.1.0"
    p_grad["base_image_builds"] = grad_rep["base_image_builds"]
    p_grad["sync_images"] = grad_sync
    write_json(os.path.join(cand_grad, ".auto-sync/proposals/router.json"), p_grad)

    build_log_grad = os.path.join(tmp, "build-targets-grad.log")
    build_rec_grad = make_build_recorder(os.path.join(tmp, "record-build-grad.sh"), build_log_grad)
    res_grad_val = run_validate_router(cand_grad, stage_p=grad_val_stage, base_p=grad_val_base, runner=build_rec_grad)
    if res_grad_val.returncode != 0:
        raise AssertionError(
            f"graduated llm-d-router candidate failed validate_router.sh:\n{res_grad_val.stdout}\n{res_grad_val.stderr}"
        )
    grad_built = [ln.strip() for ln in open(build_log_grad, "r", encoding="utf-8").read().splitlines() if ln.strip()]
    if grad_built != ["epp-builder"]:
        raise AssertionError(f"expected only ['epp-builder'] built on graduation, got {grad_built!r}")
    print("PASS  ancestor release tag v1.1.0 graduates llm-d-router, rebases from PINNED_COMMIT, and builds epp-builder only")

    # 3d. sync_images matrix: single-arch emits alert; published multi-arch emits []; held sidecar emits [].
    sidecar_v110 = "gpustack/mirrored-llm-d-router-disagg-sidecar:v1.1.0"
    # Single-arch only (missing arm64Digest) -> must still emit sync_images.
    man_single_arch = os.path.join(tmp, "manifests-single-arch.json")
    write_json(
        man_single_arch,
        base_manifest_images({
            sidecar_v100: {
                "published": True,
                "amd64Digest": "sha256:" + "7" * 64,
                "arm64Digest": "sha256:" + "8" * 64,
            },
            sidecar_v110: {
                "published": True,
                "amd64Digest": "sha256:" + "9" * 64,
                "arm64Digest": "",
            },
        }),
    )
    sa_stage = os.path.join(tmp, "stage-sync-single")
    sa_staging = os.path.join(tmp, "staging-sync-single")
    sa_res = run(
        ["bash", os.path.join(iso_commit_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_commit_repo,
        env={
            **pf_env,
            "AUTOSYNC_STAGE_DIR": sa_stage,
            "AUTOSYNC_ROUTER_FIXTURE_DIR": grad_fix,
            "AUTOSYNC_MOCK_MANIFESTS_JSON": man_single_arch,
            "LOOPWRIGHT_STAGING_DIR": sa_staging,
            "LOOPWRIGHT_PREFLIGHT_OUT": os.path.join(tmp, "out-sync-single.json"),
        },
        check=False,
    )
    assert sa_res.returncode == 0, f"preflight single-arch failed: {sa_res.stderr}"
    sa_rep = read_json(os.path.join(sa_staging, "router-preflight-report.json"))
    assert len(sa_rep.get("sync_images") or []) == 1 and sa_rep["sync_images"][0]["command"] == expected_v110_cmd, (
        f"single-arch sidecar must still emit sync_images, got {sa_rep.get('sync_images')!r}"
    )

    # Published multi-arch -> must emit sync_images: [].
    man_multi_arch = os.path.join(tmp, "manifests-multi-arch.json")
    write_json(
        man_multi_arch,
        base_manifest_images({
            sidecar_v100: {
                "published": True,
                "amd64Digest": "sha256:" + "7" * 64,
                "arm64Digest": "sha256:" + "8" * 64,
            },
            sidecar_v110: {
                "published": True,
                "amd64Digest": "sha256:" + "a" * 64,
                "arm64Digest": "sha256:" + "b" * 64,
            },
        }),
    )
    ma_stage = os.path.join(tmp, "stage-sync-multi")
    ma_staging = os.path.join(tmp, "staging-sync-multi")
    ma_res = run(
        ["bash", os.path.join(iso_commit_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_commit_repo,
        env={
            **pf_env,
            "AUTOSYNC_STAGE_DIR": ma_stage,
            "AUTOSYNC_ROUTER_FIXTURE_DIR": grad_fix,
            "AUTOSYNC_MOCK_MANIFESTS_JSON": man_multi_arch,
            "LOOPWRIGHT_STAGING_DIR": ma_staging,
            "LOOPWRIGHT_PREFLIGHT_OUT": os.path.join(tmp, "out-sync-multi.json"),
        },
        check=False,
    )
    assert ma_res.returncode == 0, f"preflight multi-arch failed: {ma_res.stderr}"
    ma_rep = read_json(os.path.join(ma_staging, "router-preflight-report.json"))
    assert ma_rep.get("sync_images") == [], f"multi-arch published sidecar must emit [], got {ma_rep.get('sync_images')!r}"

    # Held sidecar in setting-image-holds.json -> must emit sync_images: [].
    held_sidecar_holds = os.path.join(tmp, "held-sidecar-holds.json")
    write_json(
        held_sidecar_holds,
        {
            "schema_version": 1,
            "holds": {
                "sidecar": {
                    "tag": "v1.0.0",
                    "reason": "test hold on sidecar",
                }
            },
        },
    )
    hs_stage = os.path.join(tmp, "stage-sync-held")
    hs_staging = os.path.join(tmp, "staging-sync-held")
    hs_res = run(
        ["bash", os.path.join(iso_commit_repo, ".auto-sync/scripts/preflight-router.sh")],
        cwd=iso_commit_repo,
        env={
            **pf_env,
            "AUTOSYNC_STAGE_DIR": hs_stage,
            "AUTOSYNC_ROUTER_FIXTURE_DIR": grad_fix,
            "AUTOSYNC_SETTING_IMAGE_HOLDS_FILE": held_sidecar_holds,
            "LOOPWRIGHT_STAGING_DIR": hs_staging,
            "LOOPWRIGHT_PREFLIGHT_OUT": os.path.join(tmp, "out-sync-held.json"),
        },
        check=False,
    )
    assert hs_res.returncode == 0, f"preflight held-sidecar failed: {hs_res.stderr}"
    hs_rep = read_json(os.path.join(hs_staging, "router-preflight-report.json"))
    assert hs_rep.get("sync_images") == [], f"held sidecar must emit [], got {hs_rep.get('sync_images')!r}"
    print("PASS  sync_images emits on missing/single-arch sidecar and suppresses on multi-arch, held, or ineligible")

    # 4. Cross-domain base_image_builds tests for kv-transfer (mirrored-mooncake, model-transfer, mirrored-modelexpress-server).
    kv_base = os.path.join(tmp, "kv-base")
    kv_cand = os.path.join(tmp, "kv-cand")
    for rel in (
        "pack/mirrored-mooncake/Dockerfile",
        "pack/model-transfer/Dockerfile",
        "pack/model-transfer/patches/hixl/001_channel_generation.patch",
        "pack/mirrored-modelexpress-server/Dockerfile",
        "pkg/worker/settings/value.go",
        "deploy/gpustack-operator/chart/values.yaml",
        ".auto-sync/proposals/kv-transfer.json",
    ):
        for dst_root in (kv_base, kv_cand):
            out = os.path.join(dst_root, rel)
            os.makedirs(os.path.dirname(out), exist_ok=True)
            shutil.copy2(os.path.join(root, rel), out)

    # Modify mooncake, model-transfer (rocm UCX + cann HIXL patch), and modelexpress.
    mc_df = os.path.join(kv_cand, "pack/mirrored-mooncake/Dockerfile")
    with open(mc_df, "r", encoding="utf-8") as f:
        t = f.read()
    with open(mc_df, "w", encoding="utf-8") as f:
        f.write(re.sub(r'^ARG MOONCAKE_VERSION="[^"]+"', 'ARG MOONCAKE_VERSION="0.3.15"', t, count=1, flags=re.MULTILINE))

    mt_df = os.path.join(kv_cand, "pack/model-transfer/Dockerfile")
    with open(mt_df, "r", encoding="utf-8") as f:
        t = f.read()
    with open(mt_df, "w", encoding="utf-8") as f:
        f.write(re.sub(r'^ARG NIXL_ROCM_UCX_REF="[^"]+"', 'ARG NIXL_ROCM_UCX_REF="v1.24.0"', t, count=1, flags=re.MULTILINE))

    with open(os.path.join(kv_cand, "pack/model-transfer/patches/hixl/001_channel_generation.patch"), "a", encoding="utf-8") as f:
        f.write("\n# bump\n")

    mx_df = os.path.join(kv_cand, "pack/mirrored-modelexpress-server/Dockerfile")
    with open(mx_df, "r", encoding="utf-8") as f:
        t = f.read()
    with open(mx_df, "w", encoding="utf-8") as f:
        f.write(re.sub(r'^ARG MODELEXPRESS_VERSION=\S+', 'ARG MODELEXPRESS_VERSION=v0.9.0', t, count=1, flags=re.MULTILINE))

    cli_res = run(
        [
            "bash",
            os.path.join(root, ".auto-sync/scripts/base-image-recommendations.sh"),
            "diff",
            kv_base,
            kv_cand,
            "--domain",
            "kv-transfer",
        ],
        cwd=root,
    )
    kv_builds = json.loads(cli_res.stdout)
    by_key = {(b["repository"], b["target"]): b for b in kv_builds}
    want_keys = {
        ("gpustack/mirrored-mooncake", ""),
        ("gpustack/model-transfer", "rocm"),
        ("gpustack/model-transfer", "cann"),
        ("gpustack/mirrored-modelexpress-server", ""),
    }
    if set(by_key) != want_keys:
        raise AssertionError(f"expected kv-transfer base_image_builds keys {sorted(want_keys)}, got {sorted(by_key)}")
    if by_key[("gpustack/mirrored-mooncake", "")]["recommended_tag"] != "0.3.15-cpu":
        raise AssertionError(f"unexpected mooncake tag: {by_key[('gpustack/mirrored-mooncake', '')]!r}")
    if by_key[("gpustack/mirrored-modelexpress-server", "")]["recommended_tag"] != "v0.9.0":
        raise AssertionError(f"unexpected modelexpress tag: {by_key[('gpustack/mirrored-modelexpress-server', '')]!r}")
    if "-f target=rocm" not in by_key[("gpustack/model-transfer", "rocm")]["command"]:
        raise AssertionError(f"missing target=rocm in command: {by_key[('gpustack/model-transfer', 'rocm')]!r}")
    if "-f target=cann" not in by_key[("gpustack/model-transfer", "cann")]["command"]:
        raise AssertionError(f"missing target=cann in command: {by_key[('gpustack/model-transfer', 'cann')]!r}")

    kv_prop = os.path.join(kv_cand, ".auto-sync/proposals/kv-transfer.json")
    kv_pdoc = read_json(kv_prop)
    kv_pdoc["base_image_builds"] = kv_builds
    write_json(kv_prop, kv_pdoc)
    validate_proposal_builds(kv_base, kv_cand, kv_prop, "kv-transfer")

    # Negative: dropping one required target fails validation.
    kv_pdoc["base_image_builds"] = [b for b in kv_builds if b["target"] != "cann"]
    write_json(kv_prop, kv_pdoc)
    try:
        validate_proposal_builds(kv_base, kv_cand, kv_prop, "kv-transfer")
    except SystemExit as exc:
        if "missing required build recommendation" not in str(exc):
            raise AssertionError(f"unexpected error for missing cann target: {exc}")
    else:
        raise AssertionError("expected validate_proposal_builds to reject missing cann target")

    # Negative: extraneous cross-domain entry on kv-transfer fails validation.
    kv_pdoc["base_image_builds"] = kv_builds + pf_builds
    write_json(kv_prop, kv_pdoc)
    try:
        validate_proposal_builds(kv_base, kv_cand, kv_prop, "kv-transfer")
    except SystemExit as exc:
        if "unexpected build recommendation" not in str(exc):
            raise AssertionError(f"unexpected error for extraneous router build in kv-transfer: {exc}")
    else:
        raise AssertionError("expected validate_proposal_builds to reject cross-domain router entry in kv-transfer")

    # Negative: non-empty base_image_builds when no pack/* files changed must fail.
    kv_pdoc["base_image_builds"] = kv_builds
    write_json(kv_prop, kv_pdoc)
    try:
        validate_proposal_builds(kv_base, kv_base, kv_prop, "kv-transfer")
    except SystemExit as exc:
        if "must be empty when no mapped pack/* files changed" not in str(exc):
            raise AssertionError(f"unexpected error for stale base_image_builds: {exc}")
    else:
        raise AssertionError("expected validate_proposal_builds to reject stale base_image_builds")

    # Sanity: recommend_from_diff on identical dirs returns [].
    if recommend_from_diff(kv_base, kv_base) != []:
        raise AssertionError("expected empty recommend_from_diff on unchanged dirs")
    print("PASS  base_image_builds positive and negative validation across router and kv-transfer domains")
PY
then
  pass "router freeze, commit graduation, sync_images, sgl-router staging, and Base Image build recommendations end-to-end"
else
  fail "router freeze, commit graduation, sync_images, sgl-router staging, and Base Image build recommendations end-to-end"
fi

echo
echo "Summary: ${PASSES} passed, ${FAILS} failed"
[[ "${FAILS}" -eq 0 ]]
