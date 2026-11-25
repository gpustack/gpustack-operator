#!/usr/bin/env bash
# shellcheck disable=SC2016
# hack/check/skills-selftest.sh -- proves hack/check/skills.sh can fail, and fails for the stated
# reason.
#
# It builds a throwaway tree that passes, then breaks exactly one thing per case and asserts the
# checker both rejects the tree and names the defect. The passing baseline is a case of its own:
# without it, every rejection below is explained just as well by a checker that rejects everything.
#
# It also separates the two failure exits. A finding (1) is a statement about the tree; "cannot
# run" (2) is a statement about the checker, and reporting one as the other sends the reader
# looking for a defect that is not there.

set -o pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd -P)}"
CHECK="${ROOT_DIR}/hack/check/skills.sh"

[[ -f "${CHECK}" ]] || {
  echo "check/skills-selftest: missing ${CHECK}" >&2
  exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

failures=0

# fixture <dir>: a tree that must pass, so each case can break one thing in its own copy.
fixture() {
  local d="$1"
  mkdir -p "${d}/.agents/skills/gpustack-operator-code-review" \
    "${d}/.agents/skills/gpustack-operator-e2e/agents" \
    "${d}/.agents/skills/gpustack-operator-e2e/cases" \
    "${d}/.agents/skills/demo-skill" "${d}/.opencodereview" "${d}/.github"

  cat >"${d}/.agents/skills/demo-skill/SKILL.md" <<'EOF'
---
name: demo-skill
description: "A short description."
---

# demo
EOF

  cat >"${d}/.agents/skills/gpustack-operator-e2e/SKILL.md" <<'EOF'
---
name: gpustack-operator-e2e
description: "Run the operator E2E suite."
disable-model-invocation: true
---

# E2E

## Cases (locked titles)

| Case | Title |
|---|---|
| `cases/case-1.sh` | First case |
| `cases/case-2.sh` | Second case |
EOF

  cat >"${d}/.agents/skills/gpustack-operator-e2e/agents/openai.yaml" <<'EOF'
policy:
  allow_implicit_invocation: false
EOF

  printf '#!/usr/bin/env bash\nexit 0\n' >"${d}/.agents/skills/gpustack-operator-e2e/cases/case-1.sh"
  printf '#!/usr/bin/env bash\nexit 0\n' >"${d}/.agents/skills/gpustack-operator-e2e/cases/case-2.sh"
  chmod +x "${d}/.agents/skills/gpustack-operator-e2e/cases/case-1.sh" \
    "${d}/.agents/skills/gpustack-operator-e2e/cases/case-2.sh"

  cat >"${d}/.agents/skills/gpustack-operator-code-review/SKILL.md" <<'EOF'
---
name: gpustack-operator-code-review
description: "Review a diff."
---

## Out of scope

- `binding/`, `staging/`, `_test.go`.
EOF

  cat >"${d}/.opencodereview/rule.json" <<'EOF'
{
  "exclude": [
    "**/*_test.go",
    "binding/**",
    "staging/**"
  ],
  "rules": []
}
EOF

  # A verbatim mirror of the exclude array above, in the same order: that equality is the contract.
  cat >"${d}/.github/copilot-instructions.md" <<'EOF'
## Out of scope — do not review

- `**/*_test.go`
- `binding/**`
- `staging/**`
EOF

  # A real trigger row, so a case that freezes demo-skill tests only the thing it broke. The
  # AGENTS.md assertion wants the name beside an arrow, and gets its own cases below.
  cat >"${d}/AGENTS.md" <<'EOF'
- some change → `demo-skill`
- e2e change → `gpustack-operator-e2e`
EOF
}

# case <name> <expected-exit> <expected-substring-in-output> <mutation-command...>
case_() {
  local name="$1" want_rc="$2" want_msg="$3"
  shift 3
  local d="${WORK}/${name}"
  rm -rf "${d}"
  fixture "${d}"
  ( cd "${d}" && "$@" ) || {
    echo "check/skills-selftest: ${name}: could not apply its mutation" >&2
    failures=$((failures + 1))
    return
  }

  local out rc=0
  out="$(bash "${CHECK}" "${d}" 2>&1)" || rc=$?

  if [[ ${rc} -ne ${want_rc} ]]; then
    echo "check/skills-selftest: ${name}: exit ${rc}, expected ${want_rc}" >&2
    echo "${out}" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return
  fi
  if [[ -n "${want_msg}" ]] && ! grep -qF -- "${want_msg}" <<<"${out}"; then
    echo "check/skills-selftest: ${name}: exit ${rc} was right but the message did not mention '${want_msg}'" >&2
    echo "${out}" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return
  fi
}

# The positive baseline. An unbroken tree must pass, or nothing below distinguishes this checker
# from one that rejects every input.
case_ baseline 0 "" true

case_ long-description 1 "over the 247 cap" \
  bash -c 'printf "%s\n" "---" "name: demo-skill" "description: \"$(head -c 600 </dev/zero | tr "\0" "x")\"" "---" > .agents/skills/demo-skill/SKILL.md'

case_ name-mismatch 1 "but its directory is" \
  sed -i.bak 's/^name: demo-skill/name: other-name/' .agents/skills/demo-skill/SKILL.md

case_ unknown-key 1 "unsupported frontmatter key" \
  sed -i.bak 's/^description:/invented-key: 1\ndescription:/' .agents/skills/demo-skill/SKILL.md

case_ frozen-without-codex-policy 1 "agents/openai.yaml is missing" \
  sed -i.bak 's/^description:/disable-model-invocation: true\ndescription:/' .agents/skills/demo-skill/SKILL.md

case_ codex-policy-without-frontmatter 1 "Codex policy freezes this skill but the frontmatter does not" \
  bash -c 'mkdir -p .agents/skills/demo-skill/agents && printf "policy:\n  allow_implicit_invocation: false\n" > .agents/skills/demo-skill/agents/openai.yaml'

case_ non-boolean-freeze 1 "expected true or false" \
  sed -i.bak 's/^description:/disable-model-invocation: yes\ndescription:/' .agents/skills/demo-skill/SKILL.md

# Freezing removes the description from what a host loads, so AGENTS.md is the only thing left that
# can tell the model the skill exists. Correctly frozen on both hosts, and still invisible.
case_ frozen-missing-from-agents-md 1 "has no '<change> → demo-skill'" \
  bash -c 'sed -i.bak "s/^description:/disable-model-invocation: true\ndescription:/" .agents/skills/demo-skill/SKILL.md \
    && mkdir -p .agents/skills/demo-skill/agents \
    && printf "policy:\n  allow_implicit_invocation: false\n" > .agents/skills/demo-skill/agents/openai.yaml \
    && printf "No skills named here.\n" > AGENTS.md'

# Mentioning the skill is not the same as routing to it. This row names it while denying the
# trigger, which the presence-only version of this assertion accepted.
case_ agents-md-mention-without-trigger 1 "has no '<change> → demo-skill'" \
  bash -c 'sed -i.bak "s/^description:/disable-model-invocation: true\ndescription:/" .agents/skills/demo-skill/SKILL.md \
    && mkdir -p .agents/skills/demo-skill/agents \
    && printf "policy:\n  allow_implicit_invocation: false\n" > .agents/skills/demo-skill/agents/openai.yaml \
    && printf -- "- some change (No trigger: demo-skill)\n" > AGENTS.md'

# A commented-out policy is still matched by a substring search, and leaves the skill auto-invoked
# on Codex while every other signal reads as frozen.
case_ commented-out-codex-policy 1 "is not exactly" \
  bash -c 'sed -i.bak "s/^description:/disable-model-invocation: true\ndescription:/" .agents/skills/demo-skill/SKILL.md \
    && mkdir -p .agents/skills/demo-skill/agents \
    && printf "policy:\n#  allow_implicit_invocation: false\n" > .agents/skills/demo-skill/agents/openai.yaml'

# Reading to EOF and reading to the closing marker yield the same lines, so the close is asserted.
case_ unterminated-frontmatter 1 "frontmatter never closes" \
  bash -c 'mkdir -p .agents/skills/probe && printf -- "---\nname: probe\ndescription: \"probe\"\n" > .agents/skills/probe/SKILL.md'

# The copilot list is a verbatim mirror, so order is part of the contract, not only membership.
case_ copilot-mirror-reordered 1 "not the exclude list" \
  bash -c 'printf -- "## Out of scope — do not review\n\n- \`staging/**\`\n- \`binding/**\`\n- \`**/*_test.go\`\n" > .github/copilot-instructions.md'

# A bracket outside the quotes used to truncate the list and let everything after it go unchecked.
# That is the check failing to run, not a finding about the tree.
case_ malformed-exclude-line 2 "not a single quoted path" \
  bash -c 'printf "%s\n" "{" "  \"exclude\": [" "    \"binding/**\" ]bogus," "    \"staging/**\"" "  ]," "  \"rules\": []" "}" > .opencodereview/rule.json'

case_ exclude-missing-from-copilot 1 "not the exclude list" \
  sed -i.bak '/`staging\/\*\*`/d' .github/copilot-instructions.md

case_ exclude-missing-from-review-skill 1 "SKILL.md: does not name" \
  sed -i.bak 's/`staging\/`, //' .agents/skills/gpustack-operator-code-review/SKILL.md

# Two entries swapped, so the list is unsorted while every path in it is still named in the prose.
# The finding has to be the ordering and nothing else.
case_ unsorted-exclude-list 1 "is not sorted" \
  bash -c 'printf "%s\n" "{" "  \"exclude\": [" "    \"**/*_test.go\"," "    \"staging/**\"," "    \"binding/**\"" "  ]," "  \"rules\": []" "}" > .opencodereview/rule.json'

# The extractor losing its grip on rule.json must not read as "nothing is excluded".
case_ unreadable-rule-json 2 "no longer matches its shape" \
  sed -i.bak 's/"exclude"/"excluded_paths"/' .opencodereview/rule.json

# --- E2E case contiguity and SKILL.md index sync ---------------------------------------------

case_ e2e-case-gap 1 "missing case-2.sh" \
  bash -c 'mv .agents/skills/gpustack-operator-e2e/cases/case-2.sh .agents/skills/gpustack-operator-e2e/cases/case-3.sh \
    && sed -i.bak "s/case-2\.sh/case-3.sh/" .agents/skills/gpustack-operator-e2e/SKILL.md'

case_ e2e-case-nonzero-start 1 "starts at case-2.sh, expected case-1.sh" \
  bash -c 'rm .agents/skills/gpustack-operator-e2e/cases/case-1.sh \
    && sed -i.bak "/case-1\.sh/d" .agents/skills/gpustack-operator-e2e/SKILL.md'

case_ e2e-case-unindexed-script 1 "missing table row for" \
  bash -c 'printf "#!/usr/bin/env bash\nexit 0\n" > .agents/skills/gpustack-operator-e2e/cases/case-3.sh \
    && chmod +x .agents/skills/gpustack-operator-e2e/cases/case-3.sh'

case_ e2e-case-stale-index-row 1 "stale table row" \
  bash -c 'printf "| \`cases/case-3.sh\` | Stale case |\n" >> .agents/skills/gpustack-operator-e2e/SKILL.md'

case_ e2e-case-out-of-order-index 1 "out-of-order table row" \
  bash -c 'cat > .agents/skills/gpustack-operator-e2e/SKILL.md <<'\''EOF'\''
---
name: gpustack-operator-e2e
description: "Run the operator E2E suite."
disable-model-invocation: true
---

# E2E

## Cases (locked titles)

| Case | Title |
|---|---|
| `cases/case-2.sh` | Second case |
| `cases/case-1.sh` | First case |
EOF'

case_ e2e-case-bad-name 1 "invalid case filename" \
  bash -c 'printf "#!/usr/bin/env bash\nexit 0\n" > .agents/skills/gpustack-operator-e2e/cases/case-01.sh \
    && chmod +x .agents/skills/gpustack-operator-e2e/cases/case-01.sh'

case_ e2e-case-bad-name-alpha 1 "invalid case filename" \
  bash -c 'printf "#!/usr/bin/env bash\nexit 0\n" > .agents/skills/gpustack-operator-e2e/cases/case-foo.sh \
    && chmod +x .agents/skills/gpustack-operator-e2e/cases/case-foo.sh'

case_ e2e-case-duplicate-index-row 1 "duplicate table row" \
  bash -c 'printf "| \`cases/case-2.sh\` | Duplicate case |\n" >> .agents/skills/gpustack-operator-e2e/SKILL.md'

case_ e2e-case-not-executable 1 "is not executable" \
  chmod -x .agents/skills/gpustack-operator-e2e/cases/case-2.sh

case_ e2e-chaos-case-gap 1 "missing chaos-2.sh" \
  bash -c 'mkdir -p .agents/skills/gpustack-operator-e2e-chaos/agents .agents/skills/gpustack-operator-e2e-chaos/cases \
    && printf "policy:\n  allow_implicit_invocation: false\n" > .agents/skills/gpustack-operator-e2e-chaos/agents/openai.yaml \
    && printf -- "- chaos change → \`gpustack-operator-e2e-chaos\`\n" >> AGENTS.md \
    && cat > .agents/skills/gpustack-operator-e2e-chaos/SKILL.md <<'\''EOF'\''
---
name: gpustack-operator-e2e-chaos
description: "Run the operator chaos suite."
disable-model-invocation: true
---

# Chaos

## Cases

| Case | Title |
|---|---|
| `cases/chaos-1.sh` | First chaos |
| `cases/chaos-3.sh` | Third chaos |
EOF
    printf "#!/usr/bin/env bash\nexit 0\n" > .agents/skills/gpustack-operator-e2e-chaos/cases/chaos-1.sh
    printf "#!/usr/bin/env bash\nexit 0\n" > .agents/skills/gpustack-operator-e2e-chaos/cases/chaos-3.sh
    chmod +x .agents/skills/gpustack-operator-e2e-chaos/cases/chaos-1.sh .agents/skills/gpustack-operator-e2e-chaos/cases/chaos-3.sh'

if [[ ${failures} -gt 0 ]]; then
  echo "check/skills-selftest: ${failures} case(s) failed" >&2
  exit 1
fi
exit 0
