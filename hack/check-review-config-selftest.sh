#!/usr/bin/env bash
# check-review-config-selftest.sh - proves check-review-config.sh rejects the disagreements it
# exists for, and reports its own failure as its own failure rather than as agreement.
#
# The checked script compares two lists that are parsed out of text. That gives it two ways to be
# useless and only one of them looks like a bug: it can miss a real disagreement, or -- the one this
# file spends most of its cases on -- it can parse nothing from either side and call two empty sets
# equal. The second is the dangerous one, because it turns green precisely when a file is reformatted
# or a heading is renamed, which is when the two lists are most likely to drift apart unnoticed.
#
# So the cases below are not "does it pass on a good tree". They are: does it go red on each shape of
# disagreement, and does it exit 2 rather than 0 on each shape of broken instrument.
#
# Fixtures are built in a throwaway directory. NEVER point this at the real checkout: the whole
# point is to hand the script trees that are wrong on purpose.

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"
CHECK="${ROOT_DIR}/hack/check-review-config.sh"

if [[ ! -x "${CHECK}" && ! -f "${CHECK}" ]]; then
  echo "check-review-config-selftest: cannot run: ${CHECK} does not exist" >&2
  exit 2
fi

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

failures=0
cases=0

# A rule.json carrying the given exclude entries, plus a second array so the parser is made to stop
# at the right bracket rather than running to the end of the file.
make_json() {
  local dir="$1"; shift
  mkdir -p "${dir}/.opencodereview"
  {
    printf '{\n  "include": [\n    "docs/**/*.md"\n  ],\n  "exclude": [\n'
    local first=1
    for entry in "$@"; do
      [[ ${first} -eq 1 ]] || printf ',\n'
      first=0
      printf '    "%s"' "${entry}"
    done
    printf '\n  ],\n  "rules": [\n    {\n      "path": "api/**/*.go",\n      "rule": "unused"\n    }\n  ]\n}\n'
  } >"${dir}/.opencodereview/rule.json"
}

# A copilot-instructions.md carrying the given out-of-scope entries, plus a following section so the
# parser is made to stop at the next heading rather than swallowing the rest of the file.
make_md() {
  local dir="$1"; shift
  mkdir -p "${dir}/.github"
  {
    printf '# Copilot Code Review\n\n## Out of scope — do not review\n\n'
    printf 'Mirrors the `exclude` list in `.opencodereview/rule.json`, verbatim and in the same order.\n\n'
    for entry in "$@"; do
      printf -- '- `%s`\n' "${entry}"
    done
    printf '\n## Go conventions\n\n- `this bullet belongs to another section`\n'
  } >"${dir}/.github/copilot-instructions.md"
}

run_case() {
  local name="$1" dir="$2" want="$3"
  cases=$((cases + 1))
  local got=0
  bash "${CHECK}" "${dir}" >/dev/null 2>&1 || got=$?
  if [[ "${got}" -eq "${want}" ]]; then
    echo "PASS  ${name} (exit ${got})"
  else
    echo "FAIL  ${name}: expected exit ${want}, got ${got}"
    failures=$((failures + 1))
  fi
}

echo "=== positive baseline: two lists that agree ==="
d="${WORK}/agree"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go" "hack/**" "staging/**"
make_md "${d}" "**/*_test.go" "hack/**" "staging/**"
run_case "identical lists pass" "${d}" 0
echo

echo "=== membership: an entry the Markdown does not carry ==="
d="${WORK}/missing-md"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go" "hack/**" "staging/**"
make_md "${d}" "**/*_test.go" "staging/**"
run_case "an entry dropped from the Markdown is caught" "${d}" 1
echo

echo "=== membership: an entry the JSON does not carry ==="
d="${WORK}/missing-json"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go" "staging/**"
make_md "${d}" "**/*_test.go" "hack/**" "staging/**"
run_case "an entry dropped from the JSON is caught" "${d}" 1
echo

echo "=== order: the same entries in a different order ==="
d="${WORK}/reordered"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go" "hack/**" "staging/**"
make_md "${d}" "hack/**" "**/*_test.go" "staging/**"
run_case "a reordered list is caught, not just a different set" "${d}" 1
echo

# The four cases below are the reason this file exists. Each one leaves the script unable to read
# one side, and each must exit 2. An exit 0 here would mean the check passes by comparing nothing.
echo "=== instrument: the exclude array cannot be parsed ==="
d="${WORK}/json-reshaped"; mkdir -p "${d}" "${d}/.opencodereview"
make_md "${d}" "**/*_test.go" "hack/**"
printf '{"include":[],"exclude":["**/*_test.go","hack/**"],"rules":[]}\n' >"${d}/.opencodereview/rule.json"
run_case "a single-line exclude array is reported as unreadable, not as empty" "${d}" 2
echo

echo "=== instrument: the Out of scope heading was renamed ==="
d="${WORK}/md-renamed"; mkdir -p "${d}" "${d}/.github"
make_json "${d}" "**/*_test.go" "hack/**"
printf '# Copilot\n\n## Excluded paths\n\n- `**/*_test.go`\n- `hack/**`\n' >"${d}/.github/copilot-instructions.md"
run_case "a renamed heading is reported as unreadable, not as empty" "${d}" 2
echo

echo "=== instrument: both sides unreadable, which is the pair that compares equal ==="
d="${WORK}/both-broken"; mkdir -p "${d}/.opencodereview" "${d}/.github"
printf '{"exclude":["a","b"]}\n' >"${d}/.opencodereview/rule.json"
printf '# Copilot\n\n## Excluded paths\n\n- `a`\n- `b`\n' >"${d}/.github/copilot-instructions.md"
run_case "two empty parses do not pass as agreement" "${d}" 2
echo

echo "=== instrument: a file is missing outright ==="
d="${WORK}/no-md"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go"
run_case "a missing file is reported as unreadable" "${d}" 2
echo

echo "=== scope: bullets after the next heading are not read ==="
d="${WORK}/next-section"; mkdir -p "${d}"
make_json "${d}" "**/*_test.go" "hack/**"
make_md "${d}" "**/*_test.go" "hack/**"
run_case "the following section's bullet is excluded from the comparison" "${d}" 0
echo

echo "${cases} cases, ${failures} failed"
[[ "${failures}" -eq 0 ]]
