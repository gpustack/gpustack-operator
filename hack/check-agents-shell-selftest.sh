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
# Resolve the pinned shellcheck exactly as the gate does, then drive every fixture with it through
# SHELLCHECK_BIN. Demanding one on PATH instead broke the image build: its container ships no
# shellcheck, `make ci` runs `make lint`, and this self-test failed before its first case — which
# is also why the guard cannot be "try PATH, else skip": a gate that steps aside where shellcheck
# is absent is exactly the vacuous pass this file exists to bar. A pin that cannot be resolved at
# all is still a loud failure of the instrument, reported as one.
SELFTEST_LIB="$(cd "$(dirname "${BASH_SOURCE[0]}")/lib" && pwd)"
ROOT_DIR="$(cd "${SELFTEST_LIB}/../.." && pwd)"
# shellcheck source=lib/util.sh
# shellcheck source=lib/log.sh
# shellcheck source=lib/style.sh
# shellcheck disable=SC1091
source "${SELFTEST_LIB}/util.sh"
# shellcheck disable=SC1091
source "${SELFTEST_LIB}/log.sh"
# shellcheck disable=SC1091
source "${SELFTEST_LIB}/style.sh"
if ! gpustack::lint::shellcheck::validate; then
  echo "FAIL: the pinned shellcheck cannot be resolved, so the self-test cannot exercise the gate." >&2
  exit 2
fi
SHELLCHECK_BIN="$(gpustack::lint::shellcheck::bin)"
export SHELLCHECK_BIN

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
{ head -1 .agents/skills/demo/cases/base-with-findings.sh; printf '# a line inserted above the findings\n'; tail -n +2 .agents/skills/demo/cases/base-with-findings.sh; } >"${MINI}/shift.tmp"
mv "${MINI}/shift.tmp" .agents/skills/demo/cases/base-with-findings.sh
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

echo "== the pin on PATH with an empty .sbin =="
# The shape bin() exists to survive: the pinned version resolves through PATH while the tree's
# .sbin is empty. bin()'s answer is consumed as a SHELLCHECK_BIN override — exactly what this
# self-test's own driver does with it — and the checker's "[ -x ${SHELLCHECK_BIN} ]" is answered
# against the working directory, where a bare "shellcheck" names nothing. So the case must feed
# the checker the VALUE bin() answers, not let it self-resolve: under a bin() that answers the
# bare name the override fails -x and the case goes red, which is the discrimination its comment
# claims. The pinned binary is PLACED on a stub PATH rather than hoped for on the real one, and
# the tree under test is a fresh copy whose .sbin is empty by construction.
pinpath="${MINI}/pinbin"
mkdir -p "${pinpath}"
ln -s "${SHELLCHECK_BIN}" "${pinpath}/shellcheck"
pinrepo="${MINI}/pinrepo"
mkdir -p "${pinrepo}/.agents/skills/demo/cases"
cp -R "$ROOT/hack" "${pinrepo}/hack"
git -C "${pinrepo}" init -q
printf '#!/usr/bin/env bash\necho ok\n' >"${pinrepo}/.agents/skills/demo/cases/case.sh"
git -C "${pinrepo}" add -A
git -C "${pinrepo}" -c user.email=check@example.invalid -c user.name=check \
  -c commit.gpgsign=false commit -qm "base" >/dev/null
printf '# harmless edit\n' >>"${pinrepo}/.agents/skills/demo/cases/case.sh"
# What bin() answers in the copy, asked there under the stub PATH, so the answer reflects the
# empty-.sbin shape rather than this tree's warm one.
pinbin="$(cd "${pinrepo}" && env PATH="${pinpath}:/usr/bin:/bin" bash -c '
  ROOT_DIR="$(pwd)"
  # shellcheck source=hack/lib/util.sh
  # shellcheck source=hack/lib/log.sh
  # shellcheck source=hack/lib/style.sh
  # shellcheck disable=SC1091
  source hack/lib/util.sh
  # shellcheck disable=SC1091
  source hack/lib/log.sh
  # shellcheck disable=SC1091
  source hack/lib/style.sh
  gpustack::lint::shellcheck::bin
')"
expect green "a pinned shellcheck on PATH with an empty .sbin resolves absolutely and passes" \
  SHELLCHECK_BIN="${pinbin}" PATH="${pinpath}:/usr/bin:/bin" -- "${pinrepo}"

echo "== the instrument itself =="
# A forced instrument that does not exist is a loud not-run. The pinned resolution the checker
# otherwise performs is exercised by every case above (they all resolve it the same way make lint
# does), so what this case pins is the loudness: exit 2, never a silent pass and never a quiet
# fall-back to whatever else is installed.
expect could-not-run "an instrument that cannot be resolved is reported as not-run, not as a pass" \
  SHELLCHECK_BIN="${MINI}/no-such-shellcheck" -- "${MINI}/repo"

if [ "${fails}" -gt 0 ]; then
  echo >&2
  echo "SELFTEST FAILED: ${fails} of ${cases} case(s) did not answer as the gate's contract states." >&2
  exit 1
fi
echo "SELFTEST PASSED: the gate was seen to fire on each shape it claims to catch (${cases} case(s))."
