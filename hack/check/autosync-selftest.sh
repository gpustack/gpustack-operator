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
    check_repo_file(proposal.get("path"), f"{rel_tp} spec.proposal.path")
    if proposal.get("path") not in writable:
        raise AssertionError(f"{rel_tp}: spec.proposal.path must appear as an exact file in spec.scope.writable")

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

echo "== router freeze, sgl-router staging, and Base Image build recommendations end-to-end =="
if python3 -B - "${ROOT_DIR}" <<'PY'
import json
import os
import re
import shutil
import subprocess
import sys
import tempfile

root = sys.argv[1]
sys.path.insert(0, os.path.join(root, ".auto-sync", "checks"))
from base_image_builds import recommend_from_diff, validate_proposal_builds


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


def git_init_commit(repo_dir: str, files: dict[str, str], tag: str, msg: str, env: dict) -> None:
    if not os.path.isdir(os.path.join(repo_dir, ".git")):
        os.makedirs(repo_dir, exist_ok=True)
        run(["git", "-C", repo_dir, "init", "-q", "-b", "main"], env=env)
        run(["git", "-C", repo_dir, "config", "user.name", "selftest"], env=env)
        run(["git", "-C", repo_dir, "config", "user.email", "selftest@gpustack.local"], env=env)
        run(["git", "-C", repo_dir, "config", "commit.gpgsign", "false"], env=env)
    for rel, content in files.items():
        fp = os.path.join(repo_dir, rel)
        os.makedirs(os.path.dirname(fp), exist_ok=True)
        with open(fp, "w", encoding="utf-8") as f:
            f.write(content)
    run(["git", "-C", repo_dir, "add", "-A"], env=env)
    run(["git", "-C", repo_dir, "commit", "-q", "--allow-empty", "-m", msg], env=env)
    run(["git", "-C", repo_dir, "tag", "-f", tag], env=env)


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
    # 1. End-to-end router preflight: separate staging dirs, frozen sglang-gateway, sgl-router drift + base_image_builds.
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
    git_init_commit(
        vllm_fix_repo,
        {"Cargo.toml": '[package]\nname = "vllm-router"\nedition = "2021"\n'},
        "v0.1.17",
        "v0.1.17",
        git_env,
    )
    vllm_epd_sha = run(["git", "-C", vllm_fix_repo, "rev-parse", "refs/tags/v0.1.17^{commit}"], env=git_env).stdout.strip()
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

    # Read current defaults dynamically from value.go so the test stays decoupled from setting tag bumps.
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

    manifests_path = os.path.join(fix_dir, "image-manifests.json")
    with open(manifests_path, "w", encoding="utf-8") as f:
        json.dump(
            {
                "schemaVersion": 1,
                "images": {
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
                },
            },
            f,
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
        ["bash", os.path.join(root, ".auto-sync/scripts/preflight-router.sh")],
        cwd=root,
        env=pf_env,
        check=False,
    )
    if pf_res.returncode != 0:
        raise AssertionError(
            f"preflight-router.sh failed (rc={pf_res.returncode}):\n{pf_res.stdout}\n{pf_res.stderr}"
        )

    # Separate staging directories for sglang-gateway and sgl-router.
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
    print("PASS  preflight-router.sh separate sglang-gateway/sgl-router staging, freeze skip, and base_image_builds")

    # 2. validate_router.sh positive + negative freeze & base_image_builds tests.
    def make_candidate_tree(dst: str) -> None:
        for rel in (
            "pack/llm-router",
            "pkg/worker/settings/value.go",
            "docs/reference/settings.md",
            ".auto-sync/checks",
            ".auto-sync/proposals/router.json",
            ".auto-sync/setting-image-holds.json",
        ):
            src = os.path.join(root, rel)
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
    make_candidate_tree(val_base)

    def run_validate_router(cand_dir: str):
        env = {
            **os.environ,
            "AUTOSYNC_STAGE_DIR": val_stage,
            "LOOPWRIGHT_BASE_DIR": val_base,
            "LOOPWRIGHT_CANDIDATE_DIR": cand_dir,
            "AUTOSYNC_FAST_SELFTEST": "true",
            "AUTOSYNC_ALLOW_MISSING_STAGED_REPO": "true",
            "AUTOSYNC_ROUTER_BUILD_RUNNER": "true",
        }
        return run(
            ["bash", os.path.join(root, ".auto-sync/checks/validate_router.sh")],
            cwd=cand_dir,
            env=env,
            check=False,
        )

    # 2a. Positive: sgl-router bump to router-v0.2.0 + valid base_image_builds passes (real git apply on sgl-router!).
    cand_ok = os.path.join(tmp, "cand-router-ok")
    make_candidate_tree(cand_ok)
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
    with open(prop_ok, "w", encoding="utf-8") as f:
        json.dump(pdoc, f, indent=2)
    res_ok = run_validate_router(cand_ok)
    if res_ok.returncode != 0:
        raise AssertionError(f"valid sgl-router candidate failed validate_router.sh:\n{res_ok.stdout}\n{res_ok.stderr}")
    print("PASS  validate_router.sh accepts sgl-router bump with real patch apply and base_image_builds")

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
    with open(prop_nb, "w", encoding="utf-8") as f:
        json.dump(pdoc_nb, f, indent=2)
    res_nb = run_validate_router(cand_no_builds)
    if res_nb.returncode == 0 or "missing required build recommendation" not in res_nb.stderr:
        raise AssertionError(
            f"expected validate_router.sh to reject empty base_image_builds; rc={res_nb.returncode}, stderr={res_nb.stderr!r}"
        )
    print("PASS  validate_router.sh rejects missing base_image_builds when pack/llm-router changes")

    # 3. Cross-domain base_image_builds tests for kv-transfer (mirrored-mooncake, model-transfer, mirrored-modelexpress-server).
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
    with open(kv_prop, "w", encoding="utf-8") as f:
        json.dump(kv_pdoc, f, indent=2)
    validate_proposal_builds(kv_base, kv_cand, kv_prop, "kv-transfer")

    # Negative: dropping one required target fails validation.
    kv_pdoc["base_image_builds"] = [b for b in kv_builds if b["target"] != "cann"]
    with open(kv_prop, "w", encoding="utf-8") as f:
        json.dump(kv_pdoc, f, indent=2)
    try:
        validate_proposal_builds(kv_base, kv_cand, kv_prop, "kv-transfer")
    except SystemExit as exc:
        if "missing required build recommendation" not in str(exc):
            raise AssertionError(f"unexpected error for missing cann target: {exc}")
    else:
        raise AssertionError("expected validate_proposal_builds to reject missing cann target")

    # Negative: non-empty base_image_builds when no pack/* files changed must fail.
    kv_pdoc["base_image_builds"] = kv_builds
    with open(kv_prop, "w", encoding="utf-8") as f:
        json.dump(kv_pdoc, f, indent=2)
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
  pass "router freeze, sgl-router staging, and Base Image build recommendations end-to-end"
else
  fail "router freeze, sgl-router staging, and Base Image build recommendations end-to-end"
fi

echo
echo "Summary: ${PASSES} passed, ${FAILS} failed"
[[ "${FAILS}" -eq 0 ]]
