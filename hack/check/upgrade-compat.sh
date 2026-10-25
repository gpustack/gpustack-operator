#!/usr/bin/env bash
# hack/check/upgrade-compat.sh — evidence for the Go 1.27 / Kubernetes 1.36 upgrade.
#
# WHAT THIS IS. The upgrade moves a pinned set of modules across a major version
# boundary and then regenerates a lot of code. Both halves are invisible when
# they go wrong: a module graph that resolves to two Kubernetes minors looks
# like a working build, and a regenerated file that changed in a way nobody
# asked for looks like a green generation run. This tool answers three
# questions about a candidate change, and answers them from recorded facts
# rather than from a judgement made while looking at a diff.
#
#   modules   Which Kubernetes modules does the graph actually resolve to, are
#             they all on one minor, and does every staged module still carry
#             the version its pin claims?
#   snapshot  What does a set of paths look like right now, as relative paths,
#             file types and content hashes?
#   compare   How does the current state of those paths differ from a recorded
#             snapshot, as added, deleted and changed files?
#
# WHY THE THREE ARE SEPARATE. A snapshot of the whole tree says nothing about
# which files a regeneration is supposed to touch, so it is taken over a named
# set of paths. A module listing says nothing about whether the generated code
# changed, so it is a different mode. They are compared against each other by
# a human reading two reports, not by this tool, because "the module graph
# moved" and "the generated files moved" are separate facts with separate
# causes.
#
# WHAT IT DELIBERATELY DOES NOT DO. It does not decide whether a change is
# acceptable, and it does not hold a target Kubernetes minor of its own: it
# reads the minor the graph resolves to and checks that every Kubernetes
# module agrees with it. A graph on one minor is reported as such, and
# whether that minor is the one the project wants is a separate question this
# tool does not answer. It does not judge a version bump; it reports the
# resolved graph and lets the bump be judged.
#
# Usage:
#   bash hack/check/upgrade-compat.sh [--root DIR] modules
#   bash hack/check/upgrade-compat.sh [--root DIR] snapshot <output.json> <path>...
#   bash hack/check/upgrade-compat.sh [--root DIR] compare <snapshot.json> <path>...
#   bash hack/check/upgrade-compat.sh [--root DIR] self-test
set -euo pipefail

# The repository root the tool works against. It defaults to the checkout the
# script itself lives in, so the tool is runnable from anywhere without being
# told where it is, and it is overridable so the self-test can point it at a
# throwaway tree.
ROOT=""

# The resolved module graph, read by the helpers below once the modules mode
# has listed it.
MODULES_FILE=""

# The absolute path of this script, so the self-test can run the tool again
# against a throwaway tree.
SCRIPT_PATH="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/$(basename "${BASH_SOURCE[0]}")"

# The interpreter the bounded runs and the JSON readers use. It is checked
# before it is used, and the tool says so plainly when it is missing rather
# than running unbounded or reading a format it cannot read.
PYTHON3=""

# How long a single patch check may run before it is called inconclusive, and
# how long one self-test run of the modules mode may take.
PATCH_CHECK_TIMEOUT=15
MODULES_RUN_TIMEOUT=120

# The one directory this run writes into. Everything it creates lives under
# it, so the exit trap removes all of it. A list of directories would not:
# the helper that creates one runs in a command substitution, and a
# registration made there dies with it.
WORK_DIR="$(mktemp -d)"
cleanup() {
  rm -rf "${WORK_DIR}"
}
trap cleanup EXIT

make_workdir() {
  mktemp -d "${WORK_DIR}/work.XXXXXXXX"
}

# The helper the tool leans on. It does the three things the shell should not
# do by hand: read JSON, and run a command with a time limit on it.
HELPER="${WORK_DIR}/helper.py"
cat >"${HELPER}" <<'PYTHON'
import json
import subprocess
import sys


def bounded(argv):
    """Run a command with a time limit on it."""
    limit = float(argv[0])
    try:
        completed = subprocess.run(argv[1:], timeout=limit, stdin=subprocess.DEVNULL)
    except subprocess.TimeoutExpired:
        sys.exit(124)
    sys.exit(completed.returncode)


def modules(argv):
    """Read the module listing the go command printed and put it in records.

    The go command prints one JSON object per module, back to back, so the
    stream is read with the raw decoder rather than as one document.
    """
    decoder = json.JSONDecoder()
    text = sys.stdin.read()
    index = 0
    length = len(text)
    while index < length:
        while index < length and text[index] in " \t\r\n":
            index += 1
        if index >= length:
            break
        entry, index = decoder.raw_decode(text, index)
        replacement = entry.get("Replace") or {}
        print("|".join([
            entry.get("Path") or "",
            entry.get("Version") or "",
            replacement.get("Path") or "",
            replacement.get("Version") or "",
        ]))
    return 0


def snapshot(argv):
    """Read a snapshot file and put its records in the tool's record form.

    The path in a record is written the way the tool writes it, so a record
    read back here and a record written from the tree carry the same key.
    """
    try:
        with open(argv[0], encoding="utf-8") as handle:
            document = json.load(handle)
    except (OSError, ValueError) as error:
        sys.stderr.write("not a snapshot written by this tool: %s\n" % error)
        return 2
    if not isinstance(document, dict) or document.get("version") != 1:
        sys.stderr.write("not a snapshot written by this tool: no version marker\n")
        return 2
    entries = document.get("files")
    if not isinstance(entries, list):
        sys.stderr.write("not a snapshot written by this tool: no file list\n")
        return 2
    for entry in entries:
        if not isinstance(entry, dict):
            sys.stderr.write("malformed snapshot record: not an object\n")
            return 2
        path = entry.get("path")
        kind = entry.get("type")
        digest = entry.get("sha256")
        if not isinstance(path, str) or not isinstance(kind, str) or not isinstance(digest, str):
            sys.stderr.write("malformed snapshot record: a field is missing\n")
            return 2
        print("%s\t%s\t%s" % (json.dumps(path, ensure_ascii=False)[1:-1], kind, digest))
    return 0


def main(argv):
    if len(argv) < 2:
        sys.stderr.write("the helper needs a command\n")
        return 2
    command = argv[1]
    if command == "bounded":
        return bounded(argv[2:])
    if command == "modules":
        return modules(argv[2:])
    if command == "snapshot":
        return snapshot(argv[2:])
    sys.stderr.write("unknown helper command: %s\n" % command)
    return 2


sys.exit(main(sys.argv))
PYTHON

# The Kubernetes modules the specification names. Every one of them must be in
# the graph and every one of them must agree on the minor. The staged five are
# the ones the repository keeps under staging/; the rest come with the graph
# through other dependencies.
K8S_MODULES=(
  k8s.io/api
  k8s.io/apiextensions-apiserver
  k8s.io/apimachinery
  k8s.io/apiserver
  k8s.io/cli-runtime
  k8s.io/client-go
  k8s.io/code-generator
  k8s.io/component-base
  k8s.io/component-helpers
  k8s.io/kms
  k8s.io/kube-aggregator
  k8s.io/kubectl
  k8s.io/kubelet
)

# The modules the graph may carry without the specification naming them. The
# graph decides whether they are there, so the check does not ask for them,
# and a module that is there is still checked. The two lists differ in the
# version the module is checked against.
#
# k8s.io/csi-translation-lib is published in the Kubernetes release line.
# Upstream released v0.36.5 of it, and that release requires k8s.io/api and
# k8s.io/apimachinery v0.36.5, so a copy that sits on an older line is a
# mismatch and not a difference of cadence. When the module is present it
# joins the release cohort.
#
# k8s.io/gengo v1 is published on its own cadence and the repository is
# moving off it, so the graph may drop it. When it is still there its version
# is its own and is not compared with the release line.
OPTIONAL_RELEASE_MODULES=(
  k8s.io/csi-translation-lib
)

OPTIONAL_OWN_CADENCE_MODULES=(
  k8s.io/gengo
)

# The modules upstream versions independently. A rule that demanded these
# carry the Kubernetes minor would fail on every graph, because they have
# their own release cadence: kube-openapi and utils are versioned by
# pseudo-version, klog and logr by their own tags, and gogo/protobuf has not
# moved in years. Every one of them is checked for presence, and the ones the
# repository stages are also checked for a version stamp that agrees with their
# pin.
INDEPENDENT_MODULES=(
  github.com/gogo/protobuf
  github.com/go-logr/logr
  k8s.io/gengo/v2
  k8s.io/klog/v2
  k8s.io/kube-openapi
  k8s.io/utils
)

# Output helpers

log_info() { printf '%s\n' "$*"; }
log_error() { printf 'ERROR: %s\n' "$*" >&2; }

# The self-test's two reporters. They are plain functions rather than locals
# because bash cannot define a function with `local` in front of it, and
# because a function defined inside another sees that function's locals
# anyway: the counter the reporters increment is the one self_test declares.
self_test_pass() { printf 'PASS  %s\n' "$1"; }
self_test_fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# list_contains reports whether the needle is one of the items.
list_contains() {
  local needle="$1" item
  shift
  for item in ${1+"$@"}; do
    [[ "${item}" == "${needle}" ]] && return 0
  done
  return 1
}

# require_python finds the interpreter that reads the module graph and the
# snapshots as JSON and that bounds the patch checks. It is looked for before
# the first use rather than assumed, and the tool says so plainly when it is
# missing.
require_python() {
  local candidate
  if [[ -z "${PYTHON3}" ]]; then
    for candidate in python3 python; do
      if command -v "${candidate}" >/dev/null 2>&1; then
        PYTHON3="${candidate}"
        break
      fi
    done
  fi
  if [[ -z "${PYTHON3}" ]]; then
    log_error "no python3 on PATH, so the module graph and the snapshots cannot be read and a patch check cannot be bounded"
    return 1
  fi
  return 0
}

# require_tools checks what the modules mode needs: the interpreter and git,
# which runs the staged patch checks.
require_tools() {
  require_python || return 1
  if ! command -v git >/dev/null 2>&1; then
    log_error "no git on PATH, so the staged patches cannot be checked"
    return 1
  fi
  return 0
}

# run_bounded runs a command and gives up after a number of seconds. It
# returns 124 when the command was still running when the limit passed, and
# the command's own status otherwise. The command's output is left alone, so
# a caller can capture it and report what the command said.
#
# The limit is a Python 3 subprocess timeout. macOS ships no timeout(1), so a
# tool that assumed one would not run on the host it has to run on, and a
# hand-rolled job-control watchdog is a framework this tool should not carry.
# The child gets an empty standard input, so it can never wait for an answer,
# and the timeout kills it and reaps it, so no helper process is left behind.
run_bounded() {
  local limit="$1"
  shift
  "${PYTHON3}" "${HELPER}" bounded "${limit}" "$@"
}

# modules

# go_list_modules prints one line per module: path, version, and the path and
# version of the module that replaces it, separated by "|".
#
# The go command is the only thing that knows how the graph resolved: reading
# go.mod directly would miss the replacements, the excludes and the minimum
# version selection that decide what is actually built. The listing is read
# as the JSON the go command prints, which is what the specification names.
go_list_modules() {
  local listing
  if ! listing="$(cd "${ROOT}" && go list -m -json all 2>/dev/null)"; then
    log_error "go list -m -json all failed in ${ROOT}"
    return 1
  fi
  printf '%s' "${listing}" | "${PYTHON3}" "${HELPER}" modules
}

# module_version prints the version a module resolves to, or nothing when the
# module is not in the graph.
module_version() {
  local wanted="$1"
  awk -F'|' -v m="${wanted}" '$1 == m { print $2; exit }' "${MODULES_FILE}"
}

# module_replacement prints the path a module is replaced by, or nothing.
module_replacement() {
  local wanted="$1"
  awk -F'|' -v m="${wanted}" '$1 == m { print $3; exit }' "${MODULES_FILE}"
}

# k8s_minor prints the minor of a Kubernetes module version, or nothing when
# the version is not a vMAJOR.MINOR.PATCH. A pseudo-version has no minor to
# speak of, and a module whose version is "(devel)" is not a release.
k8s_minor() {
  local version="$1"
  [[ "${version}" =~ ^v([0-9]+)\.([0-9]+)\. ]] || return 0
  printf 'v%s.%s\n' "${BASH_REMATCH[1]}" "${BASH_REMATCH[2]}"
}

# staged_version prints the version a staged module copy claims, or nothing
# when there is no staged copy of that module.
staged_version() {
  local module="$1"
  local stamp="${ROOT}/staging/${module}/_VERSION_"
  [[ -f "${stamp}" ]] || return 0
  tr -d '[:space:]' <"${stamp}"
}

# staged_replacements prints the modules the graph replaces with a copy under
# staging, one per line: the module, its version, and the path of the copy
# relative to staging. The copies are found through the replacement paths the
# graph resolved, so a copy is found at whatever depth it sits and not only at
# a fixed one.
staged_replacements() {
  awk -F'|' '{
    path = $3
    sub(/^\.\//, "", path)
    if (path !~ /^staging\//) next
    sub(/^staging\//, "", path)
    print $1 "|" $2 "|" path
  }' "${MODULES_FILE}"
}

# staged_module_of prints the graph module whose replacement points at the
# given staged directory. A staged tree is reached through a `replace`
# directive, so this is how a pin and a staged copy are put back together.
staged_module_of() {
  local staged_rel="$1"
  awk -F'|' -v s="${staged_rel}" '{
    path = $3
    sub(/^\.\//, "", path)
    sub(/^staging\//, "", path)
    if (path == s) { print $1; exit }
  }' "${MODULES_FILE}"
}

# release_cohort prints the Kubernetes modules in the graph that upstream
# publishes in the same release line as the ones the specification names. A
# module belongs to that line when it sits under k8s.io and carries a v0
# release version, which is the shape upstream gives the modules it publishes
# with the Kubernetes release. The modules published on their own cadence are
# left out, because their version does not follow the release line.
release_cohort() {
  local module
  while IFS= read -r module; do
    [[ -n "${module}" ]] || continue
    list_contains "${module}" ${INDEPENDENT_MODULES[@]+"${INDEPENDENT_MODULES[@]}"} && continue
    list_contains "${module}" ${OPTIONAL_OWN_CADENCE_MODULES[@]+"${OPTIONAL_OWN_CADENCE_MODULES[@]}"} && continue
    printf '%s\n' "${module}"
  done < <(
    awk -F'|' '$1 ~ /^k8s\.io\// && $2 ~ /^v0\.[0-9]+\.[0-9]+$/ { print $1 }' "${MODULES_FILE}" |
      LC_ALL=C sort
  )
}

# path_relative_to prints the path of a target relative to a base directory.
# The base is always an ancestor of the target.
path_relative_to() {
  local target="$1" base="$2" out=""
  while [[ "${target}" != "${base}" ]]; do
    if [[ "${target}" == "/" ]]; then
      log_error "cannot express ${target} relative to ${base}"
      return 1
    fi
    out="$(basename "${target}")/${out}"
    target="$(dirname "${target}")"
  done
  printf '%s' "${out%/}"
}

# git_base prints the directory git resolves patch paths against: the
# repository root when the tree is in a repository, and the tool's root
# otherwise. A patch names its files relative to the copy it patches, so the
# check runs from the base with the copy's path as a directory prefix.
git_base() {
  local top
  if top="$(git -C "${ROOT}" rev-parse --show-toplevel 2>/dev/null)" && [[ -n "${top}" ]]; then
    printf '%s' "${top}"
    return
  fi
  printf '%s' "${ROOT}"
}

# patch_state prints the state of one staged patch: applies, applied,
# rejected or timeout. The two probes are exact, so a patch that no longer
# matches its staged copy is told apart from one that is already in it, and
# neither probe writes to the staged tree.
#
# The forward probe asks whether the patch would still apply to the staged
# copy. The reverse probe asks whether the staged copy already carries it.
# When neither can be answered, the finding claims only that the staged copy
# and the patch disagree, and not why. A probe that ran out of time says
# nothing about the patch and is never counted as applied.
patch_state() {
  local target_rel="$1" patch="$2" base="$3"
  local dir_arg status
  if ! dir_arg="$(path_relative_to "${ROOT}/${target_rel}" "${base}")"; then
    printf 'error:path\n'
    return 0
  fi
  status=0
  run_bounded "${PATCH_CHECK_TIMEOUT}" git -C "${base}" apply --check -p1 \
    --directory="${dir_arg}" "${patch}" >/dev/null 2>&1 || status=$?
  if [[ ${status} -eq 0 ]]; then
    printf 'applies\n'
    return 0
  fi
  if [[ ${status} -eq 124 ]]; then
    printf 'timeout\n'
    return 0
  fi
  if [[ ${status} -ne 1 ]]; then
    printf 'error:%s\n' "${status}"
    return 0
  fi
  status=0
  run_bounded "${PATCH_CHECK_TIMEOUT}" git -C "${base}" apply --check --reverse -p1 \
    --directory="${dir_arg}" "${patch}" >/dev/null 2>&1 || status=$?
  if [[ ${status} -eq 0 ]]; then
    printf 'applied\n'
    return 0
  fi
  if [[ ${status} -eq 124 ]]; then
    printf 'timeout\n'
    return 0
  fi
  if [[ ${status} -ne 1 ]]; then
    printf 'error:%s\n' "${status}"
    return 0
  fi
  printf 'rejected\n'
  return 0
}

# check_modules is the whole of the modules mode.
check_modules() {
  local fails=0
  local work
  work="$(make_workdir)"
  MODULES_FILE="${work}/modules.txt"
  require_tools
  go_list_modules >"${MODULES_FILE}"

  # Every required module must be present. A module that is absent from the
  # graph is not a version problem, it is a dependency that stopped being
  # required, and the two are reported differently because they are fixed
  # differently.
  local module
  for module in "${K8S_MODULES[@]}" "${INDEPENDENT_MODULES[@]}"; do
    if [[ -z "$(module_version "${module}")" ]]; then
      log_error "required module missing from the graph: ${module}"
      fails=$((fails + 1))
    fi
  done

  # Every Kubernetes module in the release cohort must be on the same minor.
  # This is the check that catches a half-finished bump: one module moved and
  # its neighbours did not, and the graph still resolves. The cohort is the
  # modules the specification names plus the ones the graph carries from the
  # same release line, so a module that only this check knows about is still
  # checked when it appears.
  local cohort=()
  for module in "${K8S_MODULES[@]}"; do
    cohort+=("${module}")
  done
  while IFS= read -r module; do
    [[ -n "${module}" ]] || continue
    list_contains "${module}" ${cohort[@]+"${cohort[@]}"} && continue
    cohort+=("${module}")
  done < <(release_cohort)

  # A module the graph may carry joins the cohort when it is there and when its
  # version has the shape of a release line version. A module of the release
  # line that sits on an older line is a mismatch the check has to name, so it
  # is compared like any other member of the cohort.
  for module in "${OPTIONAL_RELEASE_MODULES[@]}"; do
    local optional_version
    optional_version="$(module_version "${module}")"
    [[ -n "${optional_version}" ]] || continue
    [[ "${optional_version}" =~ ^v0\.[0-9]+\.[0-9]+$ ]] || continue
    list_contains "${module}" ${cohort[@]+"${cohort[@]}"} && continue
    cohort+=("${module}")
  done

  local minor="" minor_source=""
  for module in "${cohort[@]}"; do
    local version
    version="$(module_version "${module}")"
    local this_minor
    this_minor="$(k8s_minor "${version}")"
    [[ -n "${this_minor}" ]] || continue
    if [[ -z "${minor}" ]]; then
      minor="${this_minor}"
      minor_source="${module}"
    elif [[ "${this_minor}" != "${minor}" ]]; then
      log_error "Kubernetes minor versions disagree: ${module} is ${this_minor} but ${minor_source} is ${minor}"
      fails=$((fails + 1))
    fi
  done
  if [[ -n "${minor}" ]]; then
    log_info "kubernetes minor: ${minor}"
  fi

  # A staged module carries the version its pin claims. The staged tree is
  # what the build compiles, so a stamp that disagrees with the pin in the
  # graph means the tree and the pin have drifted apart and one of them is
  # wrong. The comparison is against the PIN, not against the replacement:
  # a staged copy has no version of its own, and the version the graph names
  # is the one the replacement is standing in for.
  #
  # The copies are found through the replacement paths the graph resolved, so
  # a copy is checked at whatever depth it sits, and a copy the graph
  # replaces but that carries no stamp is a finding rather than a skip.
  local graph_module graph_version staged_rel stamp_version
  while IFS='|' read -r graph_module graph_version staged_rel; do
    [[ -n "${graph_module}" ]] || continue
    if [[ ! -f "${ROOT}/staging/${staged_rel}/_VERSION_" ]]; then
      log_error "staged module ${staged_rel} has no version stamp but the graph replaces it"
      fails=$((fails + 1))
      continue
    fi
    stamp_version="$(staged_version "${staged_rel}")"
    if [[ -z "${stamp_version}" ]]; then
      log_error "staged module ${staged_rel} has an empty version stamp but the graph replaces it"
      fails=$((fails + 1))
      continue
    fi
    if [[ "${graph_version}" != "${stamp_version}" ]]; then
      log_error "staged module ${staged_rel} is stamped ${stamp_version} but the graph pins ${graph_module} at ${graph_version}"
      fails=$((fails + 1))
    fi
  done < <(staged_replacements)

  # The other direction: a staged copy that carries a stamp has to be the
  # replacement of a module in the graph. A stamp that no module claims means
  # the tree and the graph have drifted apart. The copies are found by their
  # stamp, so a copy is found at whatever depth it sits.
  local stamp
  while IFS= read -r stamp; do
    staged_rel="${stamp#"${ROOT}/staging/"}"
    staged_rel="${staged_rel%/_VERSION_}"
    stamp_version="$(staged_version "${staged_rel}")"
    if [[ -z "$(staged_module_of "${staged_rel}")" ]]; then
      local claim="${stamp_version:+ (${stamp_version})}"
      log_error "staged module ${staged_rel}${claim} is not the replacement of any module in the graph"
      fails=$((fails + 1))
    fi
  done < <(find "${ROOT}/staging" -type f -name '_VERSION_' 2>/dev/null | LC_ALL=C sort)

  # The staged patches are the other half of the staged tree. A patch that no
  # longer applies is a silent loss of behaviour, so each one is checked
  # against the staged copy it targets. The patch lives under hack/staging
  # and the copy it patches lives under staging, and the two paths have the
  # same shape below those two roots, so the copy is found by the path the
  # patch has, not by the name of the directory it sits in. A patch with no
  # staged copy is a finding: the patch would never be applied.
  local base
  base="$(git_base)"
  local patch
  while IFS= read -r patch; do
    [[ -n "${patch}" ]] || continue
    local patch_rel="${patch#"${ROOT}/"}"
    local patch_dir_rel="${patch_rel#"hack/staging/"}"
    patch_dir_rel="${patch_dir_rel%/*}"
    local target_rel="staging/${patch_dir_rel}"
    if [[ ! -d "${ROOT}/${target_rel}" ]]; then
      log_error "staged patch ${patch_rel} has no staged target directory ${target_rel}"
      fails=$((fails + 1))
      continue
    fi
    local state
    state="$(patch_state "${target_rel}" "${patch}" "${base}")"
    case "${state}" in
      applied)
        log_info "staged patch already applied: ${patch_rel}"
        ;;
      applies)
        log_error "staged patch is not applied to the staged copy: ${patch_rel}"
        fails=$((fails + 1))
        ;;
      rejected)
        log_error "staged patch does not apply: ${patch_rel}"
        fails=$((fails + 1))
        ;;
      timeout)
        log_error "staged patch check was inconclusive: ${patch_rel} (the check timed out)"
        fails=$((fails + 1))
        ;;
      *)
        log_error "staged patch check failed: ${patch_rel} (the check exited ${state#error:})"
        fails=$((fails + 1))
        ;;
    esac
  done < <(
    if [[ -d "${ROOT}/hack/staging" ]]; then
      find "${ROOT}/hack/staging" -type f -name '*.patch' | LC_ALL=C sort
    fi
  )

  # The report, so the graph is a recorded fact and not only a verdict. Only
  # the modules this check reasons about are listed: the whole graph is
  # several hundred entries and printing them buries the ones under review.
  log_info "resolved module graph (modules under review):"
  local reported=()
  for module in "${K8S_MODULES[@]}" "${INDEPENDENT_MODULES[@]}" \
    "${OPTIONAL_OWN_CADENCE_MODULES[@]}" "${OPTIONAL_RELEASE_MODULES[@]}" "${cohort[@]}"; do
    list_contains "${module}" ${reported[@]+"${reported[@]}"} && continue
    reported+=("${module}")
    local version replacement
    version="$(module_version "${module}")"
    [[ -n "${version}" ]] || continue
    replacement="$(module_replacement "${module}")"
    if [[ -n "${replacement}" ]]; then
      printf '  %s %s (replaced by %s)\n' "${module}" "${version}" "${replacement}"
    else
      printf '  %s %s\n' "${module}" "${version}"
    fi
  done

  if [[ ${fails} -gt 0 ]]; then
    printf 'module check failed with %s finding(s)\n' "${fails}" >&2
    return 1
  fi
  log_info "module check passed"
  return 0
}

# snapshot

# Paths that never belong in a snapshot. .git holds the history, not the
# working tree. The build caches hold whatever the last build left behind,
# and their contents change with every run, which would make every comparison
# report a change that nobody made. The snapshot file itself is excluded for
# the same reason it would otherwise be: it is written into the tree it
# describes.
is_excluded() {
  local rel="$1"
  case "${rel}" in
    .git | .git/*) return 0 ;;
    .cache | .cache/*) return 0 ;;
    .dist | .dist/*) return 0 ;;
    .sbin | .sbin/*) return 0 ;;
    .DS_Store) return 0 ;;
    */.DS_Store) return 0 ;;
    *) return 1 ;;
  esac
}

# json_escape prints the text of a JSON string, with the characters JSON
# cannot carry raw escaped. A control character that JSON still cannot carry
# is refused, because a snapshot that cannot describe a path faithfully is
# worse than one that says it cannot.
json_escape() {
  local s="$1"
  s="${s//\\/\\\\}"
  s="${s//\"/\\\"}"
  s="${s//$'\n'/\\n}"
  s="${s//$'\t'/\\t}"
  s="${s//$'\r'/\\r}"
  if [[ "${s}" == *[[:cntrl:]]* ]]; then
    log_error "a path carries a control character that a snapshot cannot record"
    return 1
  fi
  printf '%s' "${s}"
}

# file_kind prints the kind of a path: file, dir or link.
file_kind() {
  local path="$1"
  if [[ -L "${path}" ]]; then
    printf 'link'
  elif [[ -d "${path}" ]]; then
    printf 'dir'
  elif [[ -f "${path}" ]]; then
    printf 'file'
  else
    printf 'other'
  fi
}

# relpath prints a path relative to the repository root, using forward
# slashes so a snapshot taken on one platform compares against one taken on
# another.
relpath() {
  local path="$1"
  local abs
  abs="$(cd "$(dirname "${path}")" 2>/dev/null && printf '%s/%s' "$(pwd -P)" "$(basename "${path}")")" || {
    printf '%s' "${path}"
    return
  }
  if [[ "${abs}" == "${ROOT}" ]]; then
    printf '.'
    return
  fi
  local rel="${abs#"${ROOT}"/}"
  printf '%s' "${rel}"
}

# hash_stdin prints the SHA-256 of what it reads.
hash_stdin() {
  local out
  if command -v sha256sum >/dev/null 2>&1; then
    out="$(sha256sum | awk '{ print $1 }')" || out=""
  elif command -v shasum >/dev/null 2>&1; then
    out="$(shasum -a 256 | awk '{ print $1 }')" || out=""
  else
    log_error "no sha256sum or shasum on PATH"
    return 1
  fi
  if [[ -z "${out}" ]]; then
    log_error "cannot hash the content"
    return 1
  fi
  printf '%s' "${out}"
}

# content_hash prints the SHA-256 of what a path holds. A link is hashed by
# the path it points at, not by the content of that path: a link is a name,
# and hashing what it names would report the target's change as the link's
# change. A link that points at nothing still has a name, so a dangling link
# is recorded rather than given an empty hash.
content_hash() {
  local path="$1"
  if [[ -L "${path}" ]]; then
    local target
    target="$(readlink "${path}")" || {
      log_error "cannot read the target of the link ${path}"
      return 1
    }
    printf '%s' "${target}" | hash_stdin
    return
  fi
  if [[ -d "${path}" ]]; then
    printf 'dir'
    return
  fi
  local out
  if command -v sha256sum >/dev/null 2>&1; then
    out="$(sha256sum "${path}" 2>/dev/null | awk '{ print $1 }')" || out=""
  elif command -v shasum >/dev/null 2>&1; then
    out="$(shasum -a 256 "${path}" 2>/dev/null | awk '{ print $1 }')" || out=""
  else
    log_error "no sha256sum or shasum on PATH"
    return 1
  fi
  if [[ -z "${out}" ]]; then
    log_error "cannot hash ${path}"
    return 1
  fi
  printf '%s' "${out}"
}

# snapshot_records_for writes one record per file under the given paths into
# the given file. A record is the path, its type and its hash, separated by
# tabs. The path is written escaped, so a path that carries a space or a tab
# cannot break the record, and the same escaping is used wherever the record
# is written, so two records of the same path always read the same.
snapshot_records_for() {
  local out="$1"
  shift
  : >"${out}" || return 1
  local root
  for root in "$@"; do
    [[ -e "${root}" || -L "${root}" ]] || continue
    local found
    while IFS= read -r found; do
      local rel kind hash escaped
      rel="$(relpath "${found}")"
      is_excluded "${rel}" && continue
      kind="$(file_kind "${found}")"
      if ! hash="$(content_hash "${found}")"; then
        return 1
      fi
      if ! escaped="$(json_escape "${rel}")"; then
        return 1
      fi
      printf '%s\t%s\t%s\n' "${escaped}" "${kind}" "${hash}" >>"${out}" || return 1
    done < <(
      if [[ -f "${root}" || -L "${root}" ]]; then
        printf '%s\n' "${root}"
      else
        find "${root}" \( -type f -o -type l \) -not -path '*/.git/*' -not -name '.git' | LC_ALL=C sort
      fi
    )
  done
  return 0
}

# write_snapshot writes the snapshot file.
write_snapshot() {
  local out="$1"
  shift
  local abs_out
  abs_out="$(cd "$(dirname "${out}")" 2>/dev/null && printf '%s/%s' "$(pwd -P)" "$(basename "${out}")")" || abs_out="${out}"
  local out_rel out_escaped
  out_rel="$(relpath "${abs_out}")"
  out_escaped="$(json_escape "${out_rel}")"

  local records
  records="$(make_workdir)/records.txt"
  snapshot_records_for "${records}" "$@" || return 1

  {
    printf '{\n'
    printf '  "version": 1,\n'
    printf '  "root": "%s",\n' "$(json_escape "${ROOT}")"
    printf '  "files": [\n'
    local first=1
    local rel kind hash
    while IFS=$'\t' read -r rel kind hash; do
      [[ -n "${rel}" ]] || continue
      # The snapshot never describes itself: it is written after the walk, and
      # including it would make every snapshot differ from the one before it
      # for a reason that is the tool rather than the tree.
      if [[ "${rel}" == "${out_escaped}" ]]; then
        continue
      fi
      [[ ${first} -eq 0 ]] && printf ',\n'
      first=0
      printf '    {"path": "%s", "type": "%s", "sha256": "%s"}' \
        "${rel}" "${kind}" "${hash}"
    done <"${records}"
    [[ ${first} -eq 0 ]] && printf '\n'
    printf '  ]\n'
    printf '}\n'
  } >"${out}"
}

# compare

# snapshot_records prints the records of a snapshot file. The file is JSON
# this tool wrote, so it is read as JSON, and a file that is not one of ours
# is an error rather than a tree with no files in it.
snapshot_records() {
  local file="$1"
  [[ -f "${file}" ]] || {
    log_error "snapshot file not found: ${file}"
    return 1
  }
  local records
  if ! records="$("${PYTHON3}" "${HELPER}" snapshot "${file}" 2>&1)"; then
    log_error "${records}"
    return 1
  fi
  printf '%s\n' "${records}"
}

# compare_snapshot reports the difference between a recorded snapshot and the
# current state of the same paths. A file is added, deleted or changed by
# where it is, not by what its record says: a file whose content changed is
# changed and nothing else.
compare_snapshot() {
  local snapshot="$1"
  shift
  local work
  work="$(make_workdir)"

  snapshot_records "${snapshot}" >"${work}/old.txt" || return 1
  snapshot_records_for "${work}/new.txt" "$@" || return 1

  # The three reports are written before they are filled, so a run that finds
  # nothing still has a report to print.
  : >"${work}/added.txt"
  : >"${work}/deleted.txt"
  : >"${work}/changed.txt"

  local compare_rc=0
  awk -F'\t' \
    -v added_file="${work}/added.txt" \
    -v deleted_file="${work}/deleted.txt" \
    -v changed_file="${work}/changed.txt" \
    -v count_file="${work}/counts.txt" '
      NR == FNR {
        old_kind[$1] = $2
        old_hash[$1] = $3
        next
      }
      {
        path = $1
        seen[path] = 1
        if (!(path in old_kind)) {
          print "ADDED    " path > added_file
          added++
        } else if (old_kind[path] != $2 || old_hash[path] != $3) {
          print "CHANGED  " path > changed_file
          changed++
        }
      }
      END {
        for (path in old_kind) {
          if (!(path in seen)) {
            print "DELETED  " path > deleted_file
            deleted++
          }
        }
        printf "%d %d %d\n", added + 0, deleted + 0, changed + 0 > count_file
      }
    ' "${work}/old.txt" "${work}/new.txt" || compare_rc=$?
  if [[ ${compare_rc} -ne 0 ]]; then
    log_error "the comparison over ${snapshot} could not be completed"
    return 1
  fi

  local added deleted changed
  read -r added deleted changed <"${work}/counts.txt"
  cat "${work}/added.txt" "${work}/deleted.txt" "${work}/changed.txt"
  printf 'added=%s deleted=%s changed=%s\n' "${added}" "${deleted}" "${changed}"

  if [[ ${added} -gt 0 || ${deleted} -gt 0 || ${changed} -gt 0 ]]; then
    return 1
  fi
  return 0
}

# self-test

# modules_fixture writes a throwaway tree with a fake go that answers the
# module listing, so the modules mode can be exercised without a module
# graph. The listing is complete: a fixture that lacked a module would fail
# for that reason and would prove nothing about the finding under test.
modules_fixture() {
  local dir="$1"
  mkdir -p "${dir}/bin"
  local list="${dir}/modules.txt"
  cat >"${list}" <<'EOF'
k8s.io/api|v0.35.3|./staging/k8s.io/api|
k8s.io/apiextensions-apiserver|v0.35.3|./staging/k8s.io/apiextensions-apiserver|
k8s.io/apimachinery|v0.35.3|./staging/k8s.io/apimachinery|
k8s.io/apiserver|v0.35.3||
k8s.io/cli-runtime|v0.35.3||
k8s.io/client-go|v0.35.3||
k8s.io/code-generator|v0.35.3|./staging/k8s.io/code-generator|
k8s.io/component-base|v0.35.3||
k8s.io/component-helpers|v0.35.3||
k8s.io/cri-api|v0.35.1||
k8s.io/kms|v0.35.3||
k8s.io/kube-aggregator|v0.35.3|./staging/k8s.io/kube-aggregator|
k8s.io/kubectl|v0.35.3||
k8s.io/kubelet|v0.35.1||
k8s.io/metrics|v0.35.3||
k8s.io/klog/v2|v2.140.0|./staging/k8s.io/klog|
k8s.io/gengo|v0.0.0-20251215205346-5ee0d033ba5b||
k8s.io/gengo/v2|v2.0.0-20251215205346-5ee0d033ba5b||
k8s.io/kube-openapi|v0.0.0-20260127142750-a19766b6e2d4||
k8s.io/utils|v0.0.0-20260210185600-b8788abfbbc2||
github.com/gogo/protobuf|v1.3.2|./staging/github.com/gogo/protobuf|
github.com/go-logr/logr|v1.4.3|./staging/github.com/go-logr/logr|
EOF
  cat >"${dir}/bin/go" <<EOF
#!/usr/bin/env bash
# The self-test's fake go: it answers the module listing from the fixture
# and refuses every other command, so a check that reaches for the real
# toolchain cannot pass by accident.
if [[ "\${1:-}" != "list" ]]; then
  printf 'fake go: unexpected command: %s\n' "\$*" >&2
  exit 97
fi
while IFS='|' read -r path version replace replace_version; do
  [[ -n "\${path}" ]] || continue
  if [[ -n "\${replace}" ]]; then
    printf '{"Path":"%s","Version":"%s","Replace":{"Path":"%s","Version":"%s"}}\n' \\
      "\${path}" "\${version}" "\${replace}" "\${replace_version}"
  else
    printf '{"Path":"%s","Version":"%s"}\n' "\${path}" "\${version}"
  fi
done <"${list}"
EOF
  chmod +x "${dir}/bin/go"

  local staged stamp
  for staged in \
    "k8s.io/api|v0.35.3" \
    "k8s.io/apiextensions-apiserver|v0.35.3" \
    "k8s.io/apimachinery|v0.35.3" \
    "k8s.io/code-generator|v0.35.3" \
    "k8s.io/klog|v2.140.0" \
    "k8s.io/kube-aggregator|v0.35.3" \
    "github.com/go-logr/logr|v1.4.3" \
    "github.com/gogo/protobuf|v1.3.2"; do
    mkdir -p "${dir}/staging/${staged%%|*}"
    printf '%s' "${staged#*|}" >"${dir}/staging/${staged%%|*}/_VERSION_"
  done
  stamp="${dir}/staging/k8s.io/api"
  printf 'package api\n' >"${stamp}/api.go"
  stamp="${dir}/staging/github.com/go-logr/logr"
  printf 'package logr\n' >"${stamp}/logr.go"
}

# write_fixture_file writes the file a fixture patch is paired with. It
# carries the change the patch declares, so the patch is in the state the
# repository's staged trees are in.
write_fixture_file() {
  cat >"${1}" <<'EOF'
package fixture

// Extra.
// A note.
func Note() {}
EOF
}

# write_fixture_patch writes a patch in the shape the repository's own staged
# patches have: a git header, an index line, and a unified hunk. The file it
# names is written into the staged copy the patch belongs to.
write_fixture_patch() {
  local dir="$1" patch_dir="$2" name="$3" target_rel="$4"
  local target="${dir}/staging/${target_rel}"
  mkdir -p "${patch_dir}" "$(dirname "${target}")"
  write_fixture_file "${target}"
  {
    printf '%s\n' "diff --git a/${name} b/${name}"
    printf '%s\n' "index 1111111..2222222 100644"
    printf '%s\n' "--- a/${name}"
    printf '%s\n' "+++ b/${name}"
    printf '%s\n' "@@ -1,4 +1,5 @@"
    printf '%s\n' " package fixture"
    printf '%s\n' " "
    printf '%s\n' "+// Extra."
    printf '%s\n' " // A note."
    printf '%s\n' " func Note() {}"
  } >"${patch_dir}/001_enhanced.patch"
}

# write_deletion_patch writes a patch that only removes lines, in the shape
# the repository's own staged patches have. It is the shape a patch leaves
# behind once it has been applied: the lines it takes away are gone. The
# hunk and the blob names are the ones the staged k8s.io/klog patch carries
# for its go.sum, which is the file that patch empties.
write_deletion_patch() {
  local patch_dir="$1" name="$2"
  mkdir -p "${patch_dir}"
  {
    printf '%s\n' "diff --git a/${name} b/${name}"
    printf '%s\n' "index 0934eaa..e69de29 100644"
    printf '%s\n' "--- a/${name}"
    printf '%s\n' "+++ b/${name}"
    printf '%s\n' "@@ -1,2 +0,0 @@"
    printf '%s\n' "-github.com/go-logr/logr v1.4.1 h1:pKouT5E8xu9zeFC39JXRDukb6JFQPXM5p5I91188VAQ="
    printf '%s\n' "-github.com/go-logr/logr v1.4.1/go.mod h1:9T104GzyrTigFIr8wt5mBrctHMim0Nb2HLGrmQ40KvY="
  } >"${patch_dir}/001_enhanced.patch"
}

# fixture_move_line puts a fixture graph and the stamps of its staged copies
# on another release line. A case that mixes two lines needs the whole
# fixture on the line it claims, or the stamps disagree with the pins and the
# run reports a second finding. The minor is the number the line carries, and
# the patch number of every version is kept, so the graph still carries two
# versions of the same minor.
fixture_move_line() {
  local dir="$1" minor="$2"
  sed -i.bak "s#|v0\.[0-9][0-9]*\.\([0-9][0-9]*\)|#|v0.${minor}.\1|#g" "${dir}/modules.txt"
  rm -f "${dir}/modules.txt.bak"
  local stamp
  for stamp in "${dir}"/staging/k8s.io/*/_VERSION_; do
    [[ -f "${stamp}" ]] || continue
    sed -i.bak "s#^v0\.[0-9][0-9]*\.\([0-9][0-9]*\)\$#v0.${minor}.\1#" "${stamp}"
    rm -f "${stamp}.bak"
  done
}

# modules_case_setup applies the one change a modules case is about. Every
# other part of the fixture stays complete, so the only finding the run can
# report is the one under test.
modules_case_setup() {
  local id="$1" dir="$2"
  case "${id}" in
    org-paths)
      write_fixture_patch "${dir}" "${dir}/hack/staging/github.com/go-logr/logr" \
        "logr.go" "github.com/go-logr/logr/logr.go"
      ;;
    missing-target)
      # The patch is declared but the staged copy it patches is not there:
      # the patch would never be applied, so the check has to say so.
      mkdir -p "${dir}/hack/staging/k8s.io/absent"
      {
        printf '%s\n' "diff --git a/absent.go b/absent.go"
        printf '%s\n' "index 1111111..2222222 100644"
        printf '%s\n' "--- a/absent.go"
        printf '%s\n' "+++ b/absent.go"
        printf '%s\n' "@@ -1,4 +1,5 @@"
        printf '%s\n' " package fixture"
        printf '%s\n' " "
        printf '%s\n' "+// Extra."
        printf '%s\n' " // A note."
        printf '%s\n' " func Note() {}"
      } >"${dir}/hack/staging/k8s.io/absent/001_x.patch"
      ;;
    incompatible-patch)
      mkdir -p "${dir}/hack/staging/k8s.io/api"
      printf 'package fixture\n\n// Something else entirely.\nfunc Other() {}\n' \
        >"${dir}/staging/k8s.io/api/api.go"
      {
        printf '%s\n' "diff --git a/api.go b/api.go"
        printf '%s\n' "index 1111111..2222222 100644"
        printf '%s\n' "--- a/api.go"
        printf '%s\n' "+++ b/api.go"
        printf '%s\n' "@@ -1,4 +1,5 @@"
        printf '%s\n' " package fixture"
        printf '%s\n' " "
        printf '%s\n' "+// Extra."
        printf '%s\n' " // A note."
        printf '%s\n' " func Note() {}"
      } >"${dir}/hack/staging/k8s.io/api/001_x.patch"
      ;;
    already-applied)
      write_fixture_patch "${dir}" "${dir}/hack/staging/k8s.io/api" \
        "api.go" "k8s.io/api/api.go"
      ;;
    pin-vs-stamp)
      sed -i.bak 's#^k8s.io/api|v0.35.3#k8s.io/api|v0.35.1#' "${dir}/modules.txt"
      rm -f "${dir}/modules.txt.bak"
      ;;
    missing-module)
      sed -i.bak '/kubectl/d' "${dir}/modules.txt"
      rm -f "${dir}/modules.txt.bak"
      ;;
    minor-mismatch)
      sed -i.bak 's#^k8s.io/kubectl|v0.35.3#k8s.io/kubectl|v0.36.5#' "${dir}/modules.txt"
      rm -f "${dir}/modules.txt.bak"
      ;;
    csi-mismatch)
      # The graph carries the release line at v0.36 and one module of that
      # line that is still on an older one. The module is published in the
      # release line, so the disagreement is a finding and the only one.
      fixture_move_line "${dir}" "36"
      printf 'k8s.io/csi-translation-lib|v0.32.5||\n' >>"${dir}/modules.txt"
      ;;
    csi-absent)
      # The same graph without the module, so the pair shows that the finding
      # belongs to the module and not to the line.
      fixture_move_line "${dir}" "36"
      ;;
    gengo-v1-absent)
      # The graph after the move to gengo v2, where the old module is gone.
      sed -i.bak '/^k8s\.io\/gengo|/d' "${dir}/modules.txt"
      rm -f "${dir}/modules.txt.bak"
      ;;
    missing-gengo-v2)
      sed -i.bak '/^k8s\.io\/gengo\/v2|/d' "${dir}/modules.txt"
      rm -f "${dir}/modules.txt.bak"
      ;;
    depth3-logr-stamp)
      # The stamp of a staged copy that sits three levels below staging. The
      # copy is the one the graph replaces, so the stamp has to agree with
      # the pin whatever depth the copy sits at.
      printf 'v9.9.9' >"${dir}/staging/github.com/go-logr/logr/_VERSION_"
      ;;
    depth3-gogo-stamp)
      printf 'v9.9.9' >"${dir}/staging/github.com/gogo/protobuf/_VERSION_"
      ;;
    missing-stamp)
      # The copy is replaced but carries no stamp, so the version it stands
      # for cannot be told from any other.
      rm -f "${dir}/staging/github.com/go-logr/logr/_VERSION_"
      ;;
    empty-stamp)
      : >"${dir}/staging/github.com/go-logr/logr/_VERSION_"
      ;;
    depth3-valid)
      # The fixture as it is: the depth-3 copies carry stamps that agree with
      # their pins, so the run has to find nothing.
      ;;
    *)
      log_error "unknown modules case: ${id}"
      return 1
      ;;
  esac
}

# make_fixture writes a small tree for one self-test case.
make_fixture() {
  local dir="$1"
  mkdir -p "${dir}/sub"
  printf 'one\n' >"${dir}/keep.txt"
  printf 'two\n' >"${dir}/sub/nested.txt"
  printf 'three\n' >"${dir}/remove.txt"
}

# The self-test builds a small tree, breaks it in each of the ways the
# compare mode is supposed to detect, and asserts that compare names each
# one. A check that has only ever been seen to pass cannot be told apart from
# one whose comparison is broken, so the instrument is shown failing before
# it is trusted.
self_test() {
  # A function definition cannot carry `local`, so the two reporters are
  # plain functions that read the counter through the dynamic scope of the
  # shell that sourced this file.
  local fails=0

  local work
  work="$(make_workdir)"

  # Every case gets a tree of its own and a snapshot of its own, so a case
  # that changes a tree cannot change what another case sees. The root is
  # pointed at the tree of the case, because the paths in a snapshot are
  # relative to it.
  local tree snapshot
  local saved_root="${ROOT}"

  # fixture_case writes the tree of a case and the snapshot of that tree.
  fixture_case() {
    local name="$1"
    tree="${work}/${name}"
    snapshot="${work}/${name}.json"
    make_fixture "${tree}"
    # The physical path, for the same reason main resolves it: the paths
    # derived from the fixture come back through pwd -P and are spelled
    # /private/var.
    ROOT="$(cd "${tree}" && pwd -P)"
    write_snapshot "${snapshot}" "${tree}" >/dev/null
  }

  # 1. An unchanged tree compares equal.
  fixture_case same
  if compare_snapshot "${snapshot}" "${tree}" >"${work}/same.txt" 2>&1; then
    self_test_pass "compare reports no difference for an unchanged tree"
  else
    self_test_fail "compare reported a difference for an unchanged tree"
    sed 's/^/      /' "${work}/same.txt"
  fi

  # 2. An added file is reported as added.
  fixture_case added
  printf 'four\n' >"${tree}/added.txt"
  if compare_snapshot "${snapshot}" "${tree}" >"${work}/added.txt" 2>&1; then
    self_test_fail "compare did not report an added file"
  else
    if grep -q 'ADDED.*added\.txt' "${work}/added.txt"; then
      self_test_pass "compare reports an added file"
    else
      self_test_fail "compare did not name the added file"
      sed 's/^/      /' "${work}/added.txt"
    fi
  fi

  # 3. A deleted file is reported as deleted.
  fixture_case deleted
  rm -f "${tree}/remove.txt"
  if compare_snapshot "${snapshot}" "${tree}" >"${work}/deleted.txt" 2>&1; then
    self_test_fail "compare did not report a deleted file"
  else
    if grep -q 'DELETED.*remove\.txt' "${work}/deleted.txt"; then
      self_test_pass "compare reports a deleted file"
    else
      self_test_fail "compare did not name the deleted file"
      sed 's/^/      /' "${work}/deleted.txt"
    fi
  fi

  # 4. A changed file is reported as changed, and the change is detected by
  #    content rather than by timestamp.
  fixture_case changed
  printf 'changed\n' >"${tree}/keep.txt"
  touch -t 202001010000 "${tree}/keep.txt"
  if compare_snapshot "${snapshot}" "${tree}" >"${work}/changed.txt" 2>&1; then
    self_test_fail "compare did not report a changed file"
  else
    if grep -q 'CHANGED.*keep\.txt' "${work}/changed.txt"; then
      self_test_pass "compare reports a changed file"
    else
      self_test_fail "compare did not name the changed file"
      sed 's/^/      /' "${work}/changed.txt"
    fi
  fi

  # 5. The snapshot records a path, a type and a hash, and excludes the
  #    build caches and the snapshot file itself.
  fixture_case recorded
  if grep -q '"path": "keep.txt", "type": "file", "sha256": "[0-9a-f]\{64\}"' "${snapshot}"; then
    self_test_pass "snapshot records path, type and content hash"
  else
    self_test_fail "snapshot did not record path, type and content hash"
    sed 's/^/      /' "${snapshot}"
  fi
  if grep -q '"path": "sub/nested.txt"' "${snapshot}"; then
    self_test_pass "snapshot walks nested directories"
  else
    self_test_fail "snapshot did not walk a nested directory"
  fi
  mkdir -p "${tree}/.cache" "${tree}/.git"
  printf 'noise\n' >"${tree}/.cache/blob"
  printf 'noise\n' >"${tree}/.git/HEAD"
  local excluded_snapshot="${work}/excluded.json"
  write_snapshot "${excluded_snapshot}" "${tree}" >/dev/null
  if grep -qE '"path": "(\.git|\.cache)/' "${excluded_snapshot}"; then
    self_test_fail "snapshot recorded a path it is supposed to exclude"
  else
    self_test_pass "snapshot excludes .git and the build caches"
  fi
  if grep -q '"path": "excluded.json"' "${excluded_snapshot}"; then
    self_test_fail "snapshot recorded itself"
  else
    self_test_pass "snapshot excludes itself"
  fi

  # 6. A snapshot taken over a path that no longer exists is reported, not
  #    silently treated as an empty tree.
  fixture_case missing-snapshot
  if compare_snapshot "${work}/no-such-snapshot.json" "${tree}" >/dev/null 2>&1; then
    self_test_fail "compare accepted a missing snapshot file"
  else
    self_test_pass "compare rejects a missing snapshot file"
  fi

  # 7. A file that changed is changed and nothing else. The counts and the
  #    names are both checked, because a report that says a changed file was
  #    also added and deleted says three things where there is one.
  fixture_case exact
  printf 'seven\n' >"${tree}/added.txt"
  rm -f "${tree}/remove.txt"
  printf 'changed again\n' >"${tree}/keep.txt"
  compare_snapshot "${snapshot}" "${tree}" >"${work}/exact.txt" 2>&1 || true
  if grep -q '^added=1 deleted=1 changed=1$' "${work}/exact.txt"; then
    self_test_pass "compare counts an addition, a deletion and a change once each"
  else
    self_test_fail "compare did not count the changes once each"
    sed 's/^/      /' "${work}/exact.txt"
  fi
  if [[ "$(grep -c '^ADDED ' "${work}/exact.txt")" == "1" ]] &&
    [[ "$(grep -c '^DELETED ' "${work}/exact.txt")" == "1" ]] &&
    [[ "$(grep -c '^CHANGED ' "${work}/exact.txt")" == "1" ]]; then
    self_test_pass "compare names one addition, one deletion and one change"
  else
    self_test_fail "compare named the changes more than once"
    sed 's/^/      /' "${work}/exact.txt"
  fi
  if grep -q '^CHANGED  keep\.txt$' "${work}/exact.txt" &&
    ! grep -q '^ADDED  *keep\.txt$' "${work}/exact.txt" &&
    ! grep -q '^DELETED  *keep\.txt$' "${work}/exact.txt"; then
    self_test_pass "a changed file is reported as changed only"
  else
    self_test_fail "a changed file was reported as something else as well"
    sed 's/^/      /' "${work}/exact.txt"
  fi

  # 8. A path that carries a space is recorded and compared like any other.
  fixture_case spaced
  printf 'spaced\n' >"${tree}/a file with spaces.txt"
  local spaced_snapshot="${work}/spaced.json"
  write_snapshot "${spaced_snapshot}" "${tree}" >/dev/null
  if grep -q '"path": "a file with spaces.txt"' "${spaced_snapshot}"; then
    self_test_pass "snapshot records a path that carries a space"
  else
    self_test_fail "snapshot did not record a path that carries a space"
    sed 's/^/      /' "${spaced_snapshot}"
  fi
  printf 'spaced again\n' >"${tree}/a file with spaces.txt"
  compare_snapshot "${spaced_snapshot}" "${tree}" >"${work}/spaced.txt" 2>&1 || true
  if grep -q '^CHANGED  a file with spaces\.txt$' "${work}/spaced.txt"; then
    self_test_pass "compare names a changed path that carries a space"
  else
    self_test_fail "compare did not name a changed path that carries a space"
    sed 's/^/      /' "${work}/spaced.txt"
  fi

  # 9. A link is hashed by the path it points at, and a link that points at
  #    nothing is still recorded.
  fixture_case links
  ln -s "keep.txt" "${tree}/link.txt"
  ln -s "nowhere.txt" "${tree}/dangling.txt"
  local link_snapshot="${work}/link.json"
  write_snapshot "${link_snapshot}" "${tree}" >/dev/null
  if grep -q '"path": "link.txt", "type": "link", "sha256": "[0-9a-f]\{64\}"' "${link_snapshot}"; then
    self_test_pass "snapshot records a link by its type and its target"
  else
    self_test_fail "snapshot did not record a link by its type and its target"
    sed 's/^/      /' "${link_snapshot}"
  fi
  if grep -q '"path": "dangling.txt", "type": "link", "sha256": "[0-9a-f]\{64\}"' "${link_snapshot}"; then
    self_test_pass "snapshot records a link that points at nothing"
  else
    self_test_fail "snapshot did not record a link that points at nothing"
    sed 's/^/      /' "${link_snapshot}"
  fi

  # 10. A file that becomes a link is a change, not an addition and a
  #     deletion.
  fixture_case kind
  rm -f "${tree}/keep.txt"
  ln -s "sub/nested.txt" "${tree}/keep.txt"
  compare_snapshot "${snapshot}" "${tree}" >"${work}/type.txt" 2>&1 || true
  if grep -q '^CHANGED  keep\.txt$' "${work}/type.txt" &&
    grep -q ' changed=1$' "${work}/type.txt" &&
    ! grep -q '^ADDED  *keep\.txt$' "${work}/type.txt" &&
    ! grep -q '^DELETED  *keep\.txt$' "${work}/type.txt"; then
    self_test_pass "a file that becomes a link is a change"
  else
    self_test_fail "a file that became a link was not reported as a change"
    sed 's/^/      /' "${work}/type.txt"
  fi

  # 11. A snapshot that is not one of ours is refused rather than read as an
  #     empty tree.
  fixture_case malformed
  printf 'not a snapshot\n' >"${work}/malformed.json"
  if compare_snapshot "${work}/malformed.json" "${tree}" >"${work}/malformed.txt" 2>&1; then
    self_test_fail "compare accepted a malformed snapshot"
  else
    if grep -q 'not a snapshot written by this tool' "${work}/malformed.txt"; then
      self_test_pass "compare rejects a malformed snapshot"
    else
      self_test_fail "compare did not say why it rejected a malformed snapshot"
      sed 's/^/      /' "${work}/malformed.txt"
    fi
  fi

  ROOT="${saved_root}"

  # 12. The modules mode: one case per finding the check has to make. Each
  #     case runs the mode against a complete fixture with one thing broken,
  #     and asserts the exact finding and the exit status, so a fixture that
  #     failed for another reason cannot pass for the intended one.
  require_tools
  local cases=(
    "org-paths|org paths are kept|0|staged patch already applied: hack/staging/github.com/go-logr/logr/001_enhanced.patch"
    "missing-target|missing patch target|1|ERROR: staged patch hack/staging/k8s.io/absent/001_x.patch has no staged target directory staging/k8s.io/absent"
    "incompatible-patch|incompatible patch|1|ERROR: staged patch does not apply: hack/staging/k8s.io/api/001_x.patch"
    "already-applied|already applied patch shape|0|staged patch already applied: hack/staging/k8s.io/api/001_enhanced.patch"
    "pin-vs-stamp|declared pin against stamp|1|ERROR: staged module k8s.io/api is stamped v0.35.3 but the graph pins k8s.io/api at v0.35.1"
    "missing-module|missing required module|1|ERROR: required module missing from the graph: k8s.io/kubectl"
    "minor-mismatch|newly covered minor mismatch|1|ERROR: Kubernetes minor versions disagree: k8s.io/kubectl is v0.36 but k8s.io/api is v0.35"
    "csi-mismatch|csi translation lib on an older line|1|ERROR: Kubernetes minor versions disagree: k8s.io/csi-translation-lib is v0.32 but k8s.io/api is v0.36"
    "csi-absent|csi translation lib absent from the graph|0|module check passed"
    "gengo-v1-absent|gengo v1 dropped from the graph|0|module check passed"
    "missing-gengo-v2|missing required gengo v2|1|ERROR: required module missing from the graph: k8s.io/gengo/v2"
    "depth3-logr-stamp|drifted stamp on a depth-3 copy|1|ERROR: staged module github.com/go-logr/logr is stamped v9.9.9 but the graph pins github.com/go-logr/logr at v1.4.3"
    "depth3-gogo-stamp|drifted stamp on the other depth-3 copy|1|ERROR: staged module github.com/gogo/protobuf is stamped v9.9.9 but the graph pins github.com/gogo/protobuf at v1.3.2"
    "missing-stamp|replaced copy without a stamp|1|ERROR: staged module github.com/go-logr/logr has no version stamp but the graph replaces it"
    "empty-stamp|replaced copy with an empty stamp|1|ERROR: staged module github.com/go-logr/logr has an empty version stamp but the graph replaces it"
    "depth3-valid|valid stamps on the depth-3 copies|0|module check passed"
  )
  local case_line id description expected_status expected_line
  for case_line in "${cases[@]}"; do
    IFS='|' read -r id description expected_status expected_line <<<"${case_line}"
    local case_dir="${work}/modules-${id}"
    mkdir -p "${case_dir}"
    modules_fixture "${case_dir}"
    modules_case_setup "${id}" "${case_dir}" || {
      self_test_fail "modules case ${description}: the fixture could not be built"
      continue
    }
    local case_out="${work}/modules-${id}.txt"
    local case_rc=0
    run_bounded "${MODULES_RUN_TIMEOUT}" env "PATH=${case_dir}/bin:${PATH}" \
      "${SCRIPT_PATH}" --root "${case_dir}" modules >"${case_out}" 2>&1 || case_rc=$?
    if [[ ${case_rc} -eq 124 ]]; then
      self_test_fail "modules case ${description}: the run timed out"
      continue
    fi
    if [[ ${case_rc} -ne ${expected_status} ]]; then
      self_test_fail "modules case ${description}: exited ${case_rc}, expected ${expected_status}"
      sed 's/^/      /' "${case_out}"
      continue
    fi
    if ! grep -qF "${expected_line}" "${case_out}"; then
      self_test_fail "modules case ${description}: did not report the expected finding"
      sed 's/^/      /' "${case_out}"
      continue
    fi
    if [[ ${expected_status} -ne 0 ]]; then
      local finding_count
      finding_count="$(grep -c '^ERROR: ' "${case_out}" || true)"
      if [[ "${finding_count}" != "1" ]]; then
        self_test_fail "modules case ${description}: reported ${finding_count} findings, expected one"
        sed 's/^/      /' "${case_out}"
        continue
      fi
    fi
    self_test_pass "modules case ${description}"
  done

  # 13. A patch that only removes lines, against a staged copy whose file is
  #     empty because the patch was already applied to it. The staged copy of
  #     k8s.io/klog has this shape: its go.sum is zero bytes and the hunk the
  #     patch carries for that file only removes lines. The forward probe
  #     cannot apply, the reverse probe can, so the state is applied and not
  #     rejected. Neither probe writes to the staged copy.
  local empty_dir="${work}/modules-empty-file"
  mkdir -p "${empty_dir}"
  modules_fixture "${empty_dir}"
  mkdir -p "${empty_dir}/staging/k8s.io/klog"
  : >"${empty_dir}/staging/k8s.io/klog/go.sum"
  write_deletion_patch "${empty_dir}/hack/staging/k8s.io/klog" "go.sum"
  local empty_out="${work}/modules-empty-file.txt"
  local empty_rc=0
  run_bounded "${MODULES_RUN_TIMEOUT}" env "PATH=${empty_dir}/bin:${PATH}" \
    "${SCRIPT_PATH}" --root "${empty_dir}" modules >"${empty_out}" 2>&1 || empty_rc=$?
  if [[ ${empty_rc} -eq 0 ]] &&
    grep -qF "staged patch already applied: hack/staging/k8s.io/klog/001_enhanced.patch" "${empty_out}"; then
    self_test_pass "a deletion patch on an empty file is read as already applied"
  else
    self_test_fail "a deletion patch on an empty file was not read as already applied"
    sed 's/^/      /' "${empty_out}"
  fi
  if [[ -s "${empty_dir}/staging/k8s.io/klog/go.sum" ]]; then
    self_test_fail "the patch check wrote to the staged copy"
  else
    self_test_pass "the patch check left the staged copy untouched"
  fi

  if [[ ${fails} -gt 0 ]]; then
    log_error "self-test failed with ${fails} finding(s)"
    return 1
  fi
  log_info "self-test passed"
  return 0
}

# Dispatch

main() {
  local command=""

  # The root is an option rather than a positional argument because the
  # positional arguments of snapshot and compare are paths, and a path and a
  # root are not told apart by their shape.
  while [[ $# -gt 0 ]]; do
    case "$1" in
      --root)
        if [[ $# -lt 2 ]]; then
          log_error "--root needs a directory"
          return 2
        fi
        ROOT="$2"
        shift 2
        ;;
      --root=*)
        ROOT="${1#--root=}"
        shift
        ;;
      *)
        break
        ;;
    esac
  done

  if [[ -z "${ROOT}" ]]; then
    ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)"
  fi
  # The physical path. A root spelled /var/... and a path derived from it
  # spelled /private/var/... are one directory and two strings, and every
  # comparison between them fails in a way that reads as a content change.
  ROOT="$(cd "${ROOT}" && pwd -P)"

  command="${1:-}"
  shift || true

  case "${command}" in
    modules)
      check_modules
      ;;
    snapshot)
      if [[ $# -lt 2 ]]; then
        log_error "usage: upgrade-compat.sh snapshot <output.json> <path>..."
        return 2
      fi
      require_python || return 1
      local out="$1"
      shift
      write_snapshot "${out}" "$@"
      log_info "snapshot written to ${out}"
      ;;
    compare)
      if [[ $# -lt 2 ]]; then
        log_error "usage: upgrade-compat.sh compare <snapshot.json> <path>..."
        return 2
      fi
      require_python || return 1
      local snapshot="$1"
      shift
      compare_snapshot "${snapshot}" "$@"
      ;;
    self-test)
      require_python || return 1
      self_test
      ;;
    "" | -h | --help | help)
      sed -n '2,/^set -euo/p' "${BASH_SOURCE[0]}" | sed '$d'
      ;;
    *)
      log_error "unknown command: ${command}"
      return 2
      ;;
  esac
}

main "$@"
