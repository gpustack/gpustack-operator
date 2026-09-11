#!/usr/bin/env bash
# check-skills-selftest.sh -- proves check-skills.sh can fail, and fails for the stated reason.
#
# It builds a throwaway tree that passes, then breaks exactly one thing per case and asserts the
# checker both rejects the tree and names the defect. The passing baseline is a case of its own:
# without it, every rejection below is explained just as well by a checker that rejects everything.
#
# It also separates the two failure exits. A finding (1) is a statement about the tree; "cannot
# run" (2) is a statement about the checker, and reporting one as the other sends the reader
# looking for a defect that is not there.

set -o pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}"
CHECK="${ROOT_DIR}/hack/check-skills.sh"

[[ -f "${CHECK}" ]] || {
  echo "check-skills-selftest: missing ${CHECK}" >&2
  exit 1
}

WORK="$(mktemp -d)"
trap 'rm -rf "${WORK}"' EXIT

failures=0

# fixture <dir>: a tree that must pass, so each case can break one thing in its own copy.
fixture() {
  local d="$1"
  mkdir -p "${d}/.agents/skills/gpustack-operator-code-review" \
    "${d}/.agents/skills/demo-skill" "${d}/.opencodereview" "${d}/.github"

  cat >"${d}/.agents/skills/demo-skill/SKILL.md" <<'EOF'
---
name: demo-skill
description: "A short description."
---

# demo
EOF

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

  cat >"${d}/.github/copilot-instructions.md" <<'EOF'
Out of scope: `binding/`, `staging/`, and every `_test.go`.
EOF

  # Named here so a case that freezes demo-skill tests only the thing it broke. The AGENTS.md
  # assertion fires on frozen skills alone, and gets its own case below.
  cat >"${d}/AGENTS.md" <<'EOF'
Invoke by name: `demo-skill`.
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
    echo "check-skills-selftest: ${name}: could not apply its mutation" >&2
    failures=$((failures + 1))
    return
  }

  local out rc=0
  out="$(bash "${CHECK}" "${d}" 2>&1)" || rc=$?

  if [[ ${rc} -ne ${want_rc} ]]; then
    echo "check-skills-selftest: ${name}: exit ${rc}, expected ${want_rc}" >&2
    echo "${out}" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return
  fi
  if [[ -n "${want_msg}" ]] && ! grep -qF -- "${want_msg}" <<<"${out}"; then
    echo "check-skills-selftest: ${name}: exit ${rc} was right but the message did not mention '${want_msg}'" >&2
    echo "${out}" | sed 's/^/    /' >&2
    failures=$((failures + 1))
    return
  fi
}

# The positive baseline. An unbroken tree must pass, or nothing below distinguishes this checker
# from one that rejects every input.
case_ baseline 0 "" true

case_ long-description 1 "over the 500 cap" \
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
case_ frozen-missing-from-agents-md 1 "names no trigger for" \
  bash -c 'sed -i.bak "s/^description:/disable-model-invocation: true\ndescription:/" .agents/skills/demo-skill/SKILL.md \
    && mkdir -p .agents/skills/demo-skill/agents \
    && printf "policy:\n  allow_implicit_invocation: false\n" > .agents/skills/demo-skill/agents/openai.yaml \
    && printf "No skills named here.\n" > AGENTS.md'

case_ exclude-missing-from-copilot 1 "copilot-instructions.md: does not name" \
  sed -i.bak 's/`staging\/`, //' .github/copilot-instructions.md

case_ exclude-missing-from-review-skill 1 "SKILL.md: does not name" \
  sed -i.bak 's/`staging\/`, //' .agents/skills/gpustack-operator-code-review/SKILL.md

# Two entries swapped, so the list is unsorted while every path in it is still named in the prose.
# The finding has to be the ordering and nothing else.
case_ unsorted-exclude-list 1 "is not sorted" \
  bash -c 'printf "%s\n" "{" "  \"exclude\": [" "    \"**/*_test.go\"," "    \"staging/**\"," "    \"binding/**\"" "  ]," "  \"rules\": []" "}" > .opencodereview/rule.json'

# The extractor losing its grip on rule.json must not read as "nothing is excluded".
case_ unreadable-rule-json 2 "no longer matches its shape" \
  sed -i.bak 's/"exclude"/"excluded_paths"/' .opencodereview/rule.json

if [[ ${failures} -gt 0 ]]; then
  echo "check-skills-selftest: ${failures} case(s) failed" >&2
  exit 1
fi
exit 0
