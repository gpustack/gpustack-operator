#!/usr/bin/env bash
# hack/check/helm-chart-test-selftest.sh - proves the chart test mounts the kubeconfig the caller
# chose, refuses the ones it cannot use, and mounts the one it chose unchanged.
#
# The chart test runs chart-testing in a container, which reads the kubeconfig the host mounts at
# /root/.kube/config. Three things about that mount are a contract, and none of them shows up in a
# built chart or in a passing install:
#
#   SELECTION   the file mounted is the one the caller names in KUBECONFIG, or the default path the
#               caller passes in when the caller names none. A helper that always mounts the default
#               installs onto the cluster the caller did not choose.
#   VALIDATION  a path list, a path that does not exist, and a path that names a directory are all
#               refused by name. Docker creates a directory at a missing bind-mount source, so a
#               missing path otherwise reaches the container as a directory.
#   BINDING     the mount is read-only, it lands at /root/.kube/config, and the path reaches Docker
#               as an absolute host path. A relative source in --volume is read as a named volume.
#
# The file is mounted as it stands: the caller's bytes and the caller's current-context are what the
# container sees, and nothing here rewrites them.
#
# The bind argument is read back from the command the real gpustack::helm::test builds, through a
# docker stub that records its arguments. Asserting a string the helper assembles would only assert
# the helper against itself; asserting the argument that reaches docker is what the contract is about.
#
# Each contract is shown to bite: a helper that breaks it is written over a copy of the tree, and
# the case that covers the contract has to report the break.
#
# NEVER point ROOT_DIR at the real checkout: hack/lib/ is copied into a throwaway tree, and every
# fixture lives inside that tree. The default path the resolver falls back to is passed in as an
# argument, so the caller's own HOME and the caller's own kubeconfig are never read, and the
# environment of this file is never changed to make a case work.
#
# Usage: bash hack/check/helm-chart-test-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)"

# REQUIRED: the physical path. hack/lib/init.sh derives ROOT_DIR with `pwd -P`, and the resolver
# answers an absolute path, so a fixture under /var, a symlink to /private/var, and the path the
# resolver prints would be two spellings of one file and would not compare equal as strings.
fixture_root="$(cd "$(mktemp -d)" && pwd -P)"
trap 'rm -rf "${fixture_root}"' EXIT

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

# ---------------------------------------------------------------------------- the tree under test

# The tree under test is a copy, so a case that writes anything cannot reach the real checkout.
tree_under_test="${fixture_root}/tree"
mkdir -p "${tree_under_test}/hack"
cp -R "$ROOT/hack/lib" "${tree_under_test}/hack/lib"

# ---------------------------------------------------------------------------- the fixtures

# The fixture kubeconfig carries a current-context, because the context is one of the things the
# contract says reaches the container unchanged.
fixture_context="isolated-fixture"

function write_kubeconfig() {
  local path="$1"
  mkdir -p "$(dirname "${path}")"
  {
    printf 'apiVersion: v1\n'
    printf 'kind: Config\n'
    printf 'current-context: %s\n' "${fixture_context}"
    printf 'clusters:\n'
    printf '  - name: %s\n' "${fixture_context}"
    printf '    cluster:\n'
    printf '      server: https://127.0.0.1:6443\n'
  } > "${path}"
}

default_kubeconfig_path="${fixture_root}/default.yaml"
named_kubeconfig_path="${fixture_root}/named.yaml"
spaced_kubeconfig_path="${fixture_root}/a dir/named.yaml"
relative_kubeconfig_path="${fixture_root}/relative.yaml"
absent_kubeconfig_path="${fixture_root}/absent.yaml"
directory_kubeconfig_path="${fixture_root}/a-directory"
kubeconfig_path_list="${fixture_root}/list-a.yaml:${fixture_root}/list-b.yaml"

write_kubeconfig "${default_kubeconfig_path}"
write_kubeconfig "${named_kubeconfig_path}"
write_kubeconfig "${spaced_kubeconfig_path}"
write_kubeconfig "${relative_kubeconfig_path}"
write_kubeconfig "${fixture_root}/list-a.yaml"
write_kubeconfig "${fixture_root}/list-b.yaml"
mkdir -p "${directory_kubeconfig_path}"

# A chart for the real chart test to install, so the invocation under test is the real one.
fixture_chart_path="${fixture_root}/chart"
mkdir -p "${fixture_chart_path}"
cat >"${fixture_chart_path}/Chart.yaml" <<'EOF'
apiVersion: v2
name: t7-smoke
version: 0.1.0
EOF

# A docker that records the arguments it was handed and does nothing else. It stands in for the
# container so the command the chart test builds can be read back.
stub_bin_path="${fixture_root}/bin"
mkdir -p "${stub_bin_path}"
cat >"${stub_bin_path}/docker" <<'STUB'
#!/usr/bin/env bash
printf '%s\n' "$@" >"${DOCKER_ARGS_FILE}"
exit 0
STUB
chmod +x "${stub_bin_path}/docker"

# ---------------------------------------------------------------------------- the runner

# run_in_tree <tree> <kubeconfig> <default path> <cwd> <function> [arguments]
#
# Runs one function of the tree under test, in that tree, under the given environment. The
# environment it sets is the kubeconfig the case names, and nothing else: the caller's own HOME is
# left exactly as it was found.
function run_in_tree() {
  local tree="$1" kubeconfig="$2" default_kubeconfig_path="$3" cwd="$4" fn="$5"
  shift 5
  (
    cd "${cwd}" || exit 1
    if [ -n "${kubeconfig}" ]; then
      export KUBECONFIG="${kubeconfig}"
    else
      unset KUBECONFIG
    fi
    # shellcheck disable=SC1091
    source "${tree}/hack/lib/init.sh"
    # The resolver reports a refusal by returning non-zero. init.sh installs errexit and an ERR trap
    # that turns any failing command into a panic, so both are stepped over here: the refusal is the
    # answer this file is asking for, not a breakdown.
    set +o errexit
    trap - ERR
    "${fn}" "${default_kubeconfig_path}" "$@"
  )
}

# The cases. Each one states what the resolver must do with one input, and reports its own label, so
# a case that breaks can be named rather than counted.
#
#      <label>|<kubeconfig>|<default path>|<cwd>|<want>
#
# A want of "path:<expected>" is the path the resolver must print. A want of "refuse:<words>" is a
# refusal whose message carries those words, and the words are part of the case: a path list and a
# path that does not exist are both refused, and only the message tells the two apart. A helper that
# dropped the list rule would still refuse a list, by the existence rule, and this case is what
# notices that the caller was told the wrong thing.
function cases() {
  printf '%s\n' \
    "default-selected||${default_kubeconfig_path}|${fixture_root}|path:${default_kubeconfig_path}" \
    "named-file|${named_kubeconfig_path}|${default_kubeconfig_path}|${fixture_root}|path:${named_kubeconfig_path}" \
    "relative|relative.yaml|${default_kubeconfig_path}|${fixture_root}|path:${relative_kubeconfig_path}" \
    "spaced-path|${spaced_kubeconfig_path}|${default_kubeconfig_path}|${fixture_root}|path:${spaced_kubeconfig_path}" \
    "missing-default||${absent_kubeconfig_path}|${fixture_root}|refuse:does not exist" \
    "directory-default||${directory_kubeconfig_path}|${fixture_root}|refuse:not a regular file" \
    "missing-named|${absent_kubeconfig_path}|${default_kubeconfig_path}|${fixture_root}|refuse:does not exist" \
    "directory-named|${directory_kubeconfig_path}|${default_kubeconfig_path}|${fixture_root}|refuse:not a regular file" \
    "path-list|${kubeconfig_path_list}|${default_kubeconfig_path}|${fixture_root}|refuse:more than one file"
}

# run_cases <tree>: reports one line per case, "<label>: ok" or "<label>: not-ok (<reason>)".
function run_cases() {
  local tree="$1"
  local label kubeconfig default_path cwd want outcome rc stdout errfile

  while IFS='|' read -r label kubeconfig default_path cwd want; do
    [ -n "${label}" ] || continue
    errfile="${fixture_root}/stderr-${label}"
    rc=0
    outcome="$(run_in_tree "${tree}" "${kubeconfig}" "${default_path}" "${cwd}" \
      gpustack::helm::test::kubeconfig 2>"${errfile}")" || rc=$?
    stdout="${outcome}"

    if [[ "${want}" == "refuse:"* ]]; then
      local expected_words="${want#refuse:}"
      if [[ "${rc}" -eq 0 ]]; then
        printf '%s: not-ok (the resolver accepted it)\n' "${label}"
        continue
      fi
      if ! grep -qF "${expected_words}" "${errfile}" 2>/dev/null; then
        printf '%s: not-ok (the refusal did not say %s)\n' "${label}" "${expected_words}"
        continue
      fi
      printf '%s: ok\n' "${label}"
      continue
    fi

    if [[ "${rc}" -ne 0 ]]; then
      printf '%s: not-ok (the resolver refused it)\n' "${label}"
      continue
    fi
    if [[ "${stdout}" != "${want#path:}" ]]; then
      printf '%s: not-ok (%s is not %s)\n' "${label}" "${stdout}" "${want#path:}"
      continue
    fi
    printf '%s: ok\n' "${label}"
  done < <(cases)
}

# run_chart_test <tree> <kubeconfig>: runs the real gpustack::helm::test against the stub docker
# and echoes the volume argument that reached docker.
function run_chart_test() {
  local tree="$1" kubeconfig="$2"
  rm -f "${fixture_root}/docker-args.txt"
  (
    cd "${fixture_root}" || exit 1
    export PATH="${stub_bin_path}:${PATH}"
    export DOCKER_ARGS_FILE="${fixture_root}/docker-args.txt"
    if [ -n "${kubeconfig}" ]; then
      export KUBECONFIG="${kubeconfig}"
    else
      unset KUBECONFIG
    fi
    # shellcheck disable=SC1091
    source "${tree}/hack/lib/init.sh"
    set +o errexit
    trap - ERR
    gpustack::helm::test "${fixture_chart_path}"
  ) >/dev/null 2>&1
  # Every volume argument the command built, one per line.
  awk 'previous == "--volume" { print; next } { previous = $0 }' \
    "${fixture_root}/docker-args.txt" 2>/dev/null
}

# ---------------------------------------------------------------------------- the contracts

echo "== the chart test mounts the kubeconfig the caller chose =="
report="$(run_cases "${tree_under_test}")"
broken="$(printf '%s\n' "${report}" | grep "not-ok" || true)"
if [ -n "${broken}" ]; then
  while IFS= read -r line; do fail "${line}"; done <<<"${broken}"
else
  pass "every selection and validation case holds"
fi

echo
echo "== the chart test mounts the kubeconfig read-only and unchanged =="
volume_arguments="$(run_chart_test "${tree_under_test}" "${named_kubeconfig_path}")"
if printf '%s\n' "${volume_arguments}" | grep -qxF "${named_kubeconfig_path}:/root/.kube/config:ro"; then
  pass "the command mounts the caller's absolute path, read-only, at /root/.kube/config"
else
  fail "the command mounts no read-only kubeconfig at /root/.kube/config; it mounts: ${volume_arguments}"
fi

resolved="$(run_in_tree "${tree_under_test}" "${named_kubeconfig_path}" "${default_kubeconfig_path}" \
  "${fixture_root}" gpustack::helm::test::kubeconfig 2>/dev/null)" || resolved=""
if [ "${resolved}" = "${named_kubeconfig_path}" ]; then
  pass "the mounted file is the caller's own file"
else
  fail "the mounted file is ${resolved}, not the caller's own ${named_kubeconfig_path}"
fi

if grep -qF "current-context: ${fixture_context}" "${resolved}" 2>/dev/null; then
  pass "the caller's current-context reaches the container"
else
  fail "the caller's current-context did not reach the container"
fi

cp "${named_kubeconfig_path}" "${fixture_root}/named.expected"
run_cases "${tree_under_test}" >/dev/null
if cmp -s "${fixture_root}/named.expected" "${named_kubeconfig_path}"; then
  pass "the caller's kubeconfig is not rewritten"
else
  fail "the caller's kubeconfig was rewritten"
fi

# ---------------------------------------------------------------------------- the contracts bite

# mutate <name>: a copy of the tree, returned on stdout.
function mutate() {
  local name="$1"
  local tree="${fixture_root}/mutant-${name}"
  rm -rf "${tree}"
  mkdir -p "${tree}/hack"
  cp -R "$ROOT/hack/lib" "${tree}/hack/lib"
  echo "${tree}"
}

# mutate_body <name> <body>: a copy of the tree whose resolver is the body instead of the real one.
# The body is appended to helm.sh, so the later definition wins and everything else stays real.
function mutate_body() {
  local name="$1" body="$2"
  local tree
  tree="$(mutate "${name}")"
  printf '\n%s\n' "${body}" >> "${tree}/hack/lib/helm.sh"
  echo "${tree}"
}

# drop_block <tree> <awk-pattern>: removes the if-block that opens at the first line matching the
# pattern, up to and including its "fi". The pattern is an awk regular expression and it has to
# match the line that opens the block, because dropping from anywhere else would leave the block
# without its "fi" and the resolver would not load at all, which the cases would report as a
# breakdown rather than as the rule that went missing.
function drop_block() {
  local tree="$1" pattern="$2"
  awk -v pat="${pattern}" '
    !dropping && $0 ~ pat { dropping = 1; next }
    dropping && $0 ~ /^[[:space:]]*fi[[:space:]]*$/ { dropping = 0; next }
    !dropping { print }
  ' "${tree}/hack/lib/helm.sh" > "${tree}/hack/lib/helm.sh.new"
  mv "${tree}/hack/lib/helm.sh.new" "${tree}/hack/lib/helm.sh"
}

# expect_detected <label> <mutant> <case>: the mutant has to make that case report a break.
function expect_detected() {
  local label="$1" mutant="$2" expected_case="$3"
  local report
  report="$(run_cases "${mutant}")"
  if printf '%s\n' "${report}" | grep -q "^${expected_case}: not-ok"; then
    pass "${label}: a helper that breaks this is caught"
  else
    fail "${label}: a helper that breaks this was not caught"
  fi
}

echo
echo "== a helper that breaks the contract is caught =="

mutant="$(mutate no-list-rule)"
drop_block "${mutant}" '== \*:\*'
expect_detected "a resolver that does not refuse a path list" "${mutant}" "path-list"

mutant="$(mutate no-existence-rule)"
drop_block "${mutant}" '! -e '
expect_detected "a resolver that accepts a path that does not exist" "${mutant}" "missing-default"

mutant="$(mutate no-regular-file-rule)"
drop_block "${mutant}" '! -f '
expect_detected "a resolver that accepts a directory" "${mutant}" "directory-default"

mutant="$(mutate_body no-absolutization '
gpustack::helm::test::kubeconfig() {
  local default_kubeconfig_path="$1"
  local kubeconfig="${KUBECONFIG:-}"
  if [[ -z "${kubeconfig}" ]]; then
    kubeconfig="${default_kubeconfig_path}"
  fi
  if [[ "${kubeconfig}" == *:* ]]; then
    gpustack::log::error "KUBECONFIG names more than one file: ${kubeconfig}"
    return 1
  fi
  if [[ ! -f "${kubeconfig}" ]]; then
    gpustack::log::error "the kubeconfig ${kubeconfig} is not a regular file"
    return 1
  fi
  echo -n "${kubeconfig}"
}')"
expect_detected "a resolver that leaves the path relative" "${mutant}" "relative"

mutant="$(mutate_body rewrites-the-file '
gpustack::helm::test::kubeconfig() {
  local default_kubeconfig_path="$1"
  local kubeconfig="${KUBECONFIG:-}"
  if [[ -z "${kubeconfig}" ]]; then
    kubeconfig="${default_kubeconfig_path}"
  fi
  if [[ ! -f "${kubeconfig}" ]]; then
    gpustack::log::error "the kubeconfig ${kubeconfig} is not a regular file"
    return 1
  fi
  local rewritten="${kubeconfig}.rewritten"
  sed "s/^current-context: .*/current-context: rewritten/" "${kubeconfig}" > "${rewritten}"
  echo -n "${rewritten}"
}')"
expect_detected "a resolver that rewrites the file" "${mutant}" "named-file"

echo
if [ "$fails" -gt 0 ]; then
  echo "SELFTEST FAILED: $fails case(s)"
  exit 1
fi
# What a green run establishes: the chart test mounts the kubeconfig the caller named, refuses the
# ones it cannot use, mounts that file read-only at /root/.kube/config by an absolute host path, and
# leaves the caller's bytes and current-context alone. Each of those was shown to bite on a helper
# that breaks it, and the bind argument was read back from the command the real chart test builds.
#
# What it does NOT establish: that the chart test installs against the right cluster. The endpoint a
# container reaches is a property of the network the container runs in, not of this resolver, and the
# resolver deliberately refuses to rewrite the kubeconfig rather than guess an address.
echo "SELFTEST PASSED"
