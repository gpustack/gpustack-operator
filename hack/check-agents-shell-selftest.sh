#!/usr/bin/env bash
# check-agents-shell-selftest.sh — proves the .agents shell gate can fail, on each shape it
# claims to catch, and stays quiet on the shifts and untouched files it promises not to report.
#
# A gate that has only ever been seen to pass cannot be told apart from one that cannot fail,
# and this one's whole design is a comparison against a base: the cases below therefore commit
# a base first and dirty the tree after, which is the state the comparison is about.
#
# Usage: bash hack/check-agents-shell-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
ROOT="$(cd "$ROOT" && pwd)"
CHECK="$ROOT/hack/check-agents-shell.sh"

if [ ! -f "$CHECK" ]; then
  echo "FAIL: $CHECK does not exist, so the gate was not exercised." >&2
  exit 2
fi
if ! command -v shellcheck >/dev/null 2>&1; then
  echo "FAIL: no shellcheck on PATH, so the self-test cannot exercise the gate." >&2
  exit 2
fi

MINI="$(mktemp -d)"
trap 'rm -rf "${MINI}"' EXIT

fails=0
cases=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# expect <want: red|green|could-not-run> <label> [KEY=VAL...] -- <checker args...>
expect() {
  local want="$1" label="$2"
  shift 2
  local envs=()
  while [ "$1" != "--" ]; do
    envs+=("$1")
    shift
  done
  shift
  cases=$((cases + 1))
  local rc=0
  if [ "${#envs[@]}" -gt 0 ]; then
    env "${envs[@]}" bash "$CHECK" "$@" >"${MINI}/out" 2>&1 || rc=$?
  else
    bash "$CHECK" "$@" >"${MINI}/out" 2>&1 || rc=$?
  fi
  case "${want}:${rc}" in
    red:1) pass "${label}" ;;
    green:0) pass "${label}" ;;
    could-not-run:2) pass "${label}" ;;
    red:*) fail "${label}: want exit 1, got ${rc}; output: $(cat "${MINI}/out")" ;;
    green:*) fail "${label}: want exit 0, got ${rc}; output: $(cat "${MINI}/out")" ;;
    could-not-run:*) fail "${label}: want exit 2, got ${rc}; output: $(cat "${MINI}/out")" ;;
  esac
}

# A base with one pre-existing finding (the unquoted expansion) so the comparison cases below
# have something on the base side to stay quiet about.
mkdir -p "${MINI}/repo/.agents/skills/demo/cases" "${MINI}/repo/.agents/hooks"
cd "${MINI}/repo"
git init -q
git -c user.email=check@example.invalid -c user.name=check -c commit.gpgsign=false commit -q --allow-empty -m "root" >/dev/null

cat >.agents/skills/demo/cases/base-with-findings.sh <<'BASE'
#!/usr/bin/env bash
set -uo pipefail
echo $PRE_EXISTING_UNQUOTED
BASE_FINDING_ONLY=1
echo ok
BASE
cat >.agents/skills/demo/cases/clean.sh <<'CLEAN'
#!/usr/bin/env bash
set -uo pipefail
echo ok
CLEAN
cat >.agents/hooks/other.sh <<'HOOK'
#!/usr/bin/env bash
set -uo pipefail
echo ok
HOOK
git add -A
git -c user.email=check@example.invalid -c user.name=check -c commit.gpgsign=false commit -qm "base" >/dev/null

echo "== the gate on each shape it must catch =="
printf 'echo $NEW_UNQUOTED\n' >>.agents/skills/demo/cases/clean.sh
expect red "a new unquoted expansion in a changed file" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/clean.sh

printf 'if true; then echo broken\n' >>.agents/skills/demo/cases/clean.sh
expect red "a syntax error" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/clean.sh

echo "== a duplicated offending line =="
printf 'echo $PRE_EXISTING_UNQUOTED\n' >>.agents/skills/demo/cases/base-with-findings.sh
expect red "the same finding duplicated is one more, not noise the base excuses" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/base-with-findings.sh

echo "== the gate on each shape it must not report =="
{ head -1 .agents/skills/demo/cases/base-with-findings.sh; printf '# a line inserted above the findings\n'; tail -n +2 .agents/skills/demo/cases/base-with-findings.sh; } >/tmp/c68st-shift
mv /tmp/c68st-shift .agents/skills/demo/cases/base-with-findings.sh
expect green "a line inserted above a base finding shifts its line, not its key" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/base-with-findings.sh

printf '# harmless comment\n' >>.agents/skills/demo/cases/clean.sh
expect green "a harmless edit to a file whose neighbour carries base findings" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/clean.sh

git rm -q .agents/skills/demo/cases/clean.sh
expect green "a deleted file has nothing to check" -- "${MINI}/repo"
# After git rm the path is out of the index, so it is restored from HEAD rather than the index.
git checkout -q HEAD -- .agents/skills/demo/cases/clean.sh

printf '# shellcheck disable=SC2086  # the split is the point of the exercise\necho $DELIBERATELY_UNQUOTED\n' >>.agents/skills/demo/cases/clean.sh
expect green "the documented inline disable, with its reason, silences the finding" -- "${MINI}/repo"
git checkout -q -- .agents/skills/demo/cases/clean.sh

expect green "a clean tree checks nothing" -- "${MINI}/repo"

echo "== --base mode, the shape CI runs =="
printf 'echo $COMMITTED_NEW_UNQUOTED\n' >>.agents/skills/demo/cases/clean.sh
git add -A
git -c user.email=check@example.invalid -c user.name=check -c commit.gpgsign=false commit -qm "adds a finding" >/dev/null
expect red "a committed new finding against an older --base" -- --base HEAD~1 "${MINI}/repo"

echo "== the instrument itself =="
# A PATH holding only what the checker itself needs, shellcheck deliberately absent. It cannot be
# a bare /usr/bin:/bin: GitHub runners preinstall shellcheck there, and on such a host the case
# below would exercise nothing while reading as a pass — the exact vacuous shape it exists to bar.
nosc="${MINI}/nosc"
mkdir -p "${nosc}"
for tool in git bash mktemp sed cut tr cat awk; do
  ln -s "$(command -v "${tool}")" "${nosc}/${tool}"
done
expect could-not-run "no shellcheck on PATH is reported as not-run, not as a pass" PATH="${nosc}" -- "${MINI}/repo"

if [ "${fails}" -gt 0 ]; then
  echo >&2
  echo "SELFTEST FAILED: ${fails} of ${cases} case(s) did not answer as the gate's contract states." >&2
  exit 1
fi
echo "SELFTEST PASSED: the gate was seen to fire on each shape it claims to catch (${cases} case(s))."
