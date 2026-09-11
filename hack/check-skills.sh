#!/usr/bin/env bash
# check-skills.sh -- the contract every in-project skill under .agents/skills/ must satisfy.
#
# Two things are asserted here because nothing else asserts them and both fail silently.
#
# FRONTMATTER. A skill's name and description are the only part a host loads before the skill is
# invoked, so they are what a session pays for on every turn. A description that grows past the
# host's listing budget is truncated with no error and the tail simply never reaches the model --
# that already happened here and cost half of one skill's discovery text. An unknown key is
# rejected rather than ignored: the hosts disagree about which keys they read, and a key only one
# of them honours is a rule that fires on one machine and not another.
#
# INVOCATION POLICY. `disable-model-invocation: true` makes a skill explicit-only in Claude and
# Kimi. Codex does not read that key -- its standard validator rejects it -- and takes the same
# policy from agents/openai.yaml instead. The two must agree, or the skill is frozen on two hosts
# and auto-invoked on the third. Freezing also removes the skill's description from what the host
# loads, so AGENTS.md carries the trigger instead: a frozen skill absent from AGENTS.md is one the
# model can no longer learn exists, and nothing about that state looks different from working.
#
# EXCLUSION LISTS. Three reviewers read three different files, and only .opencodereview/rule.json
# is machine-parsed; the other two are prose a human wrote. Every path rule.json excludes must be
# named in both, so a path added to one does not quietly stay reviewable in the others. The check
# is single-directional -- rule.json owns the list, the prose must mention it -- because the prose
# legitimately says more than the list does.
#
# That list is also required to stay sorted. Order carries no meaning in "exclude" -- a path is
# dropped if it matches any entry -- so sorting costs nothing and is what makes an addition, and a
# duplicate, visible in a diff. This applies to "exclude" alone: entries in "rules" are evaluated
# in declaration order and the first matching path wins, so sorting that array would silently
# repoint files at a different prompt.
#
# EXIT: 0 nothing found, 1 findings reported, 2 the check could not run (its own diagnostic).

set -o pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd -P)}"
SKILLS_DIR="${ROOT_DIR}/.agents/skills"
RULE_JSON="${ROOT_DIR}/.opencodereview/rule.json"
COPILOT_MD="${ROOT_DIR}/.github/copilot-instructions.md"
REVIEW_MD="${SKILLS_DIR}/gpustack-operator-code-review/SKILL.md"
AGENTS_MD="${ROOT_DIR}/AGENTS.md"

# A description longer than this is the shape that got truncated. The longest one in the tree is a
# third of it, so the cap leaves room to write a real trigger sentence and still stops the essay.
DESC_MAX=500

ALLOWED_KEYS="name description allowed-tools disable-model-invocation license compatibility metadata"

findings=0
report() {
  printf 'check-skills: %s\n' "$1" >&2
  findings=$((findings + 1))
}
cannot_run() {
  printf 'check-skills: cannot run: %s\n' "$1" >&2
  exit 2
}

[[ -d "${SKILLS_DIR}" ]] || cannot_run "no skills directory at ${SKILLS_DIR}"

# --- frontmatter and invocation policy -------------------------------------------------------

skill_count=0
for skill_md in "${SKILLS_DIR}"/*/SKILL.md; do
  [[ -f "${skill_md}" ]] || continue
  skill_count=$((skill_count + 1))
  dir="$(dirname "${skill_md}")"
  want_name="$(basename "${dir}")"
  rel=".agents/skills/${want_name}/SKILL.md"

  if [[ "$(head -n 1 "${skill_md}")" != "---" ]]; then
    report "${rel}: no frontmatter (line 1 is not '---')"
    continue
  fi

  # The frontmatter block, and only it: everything between the first '---' and the next one. A
  # value may wrap, so a key is a line whose first token ends in ':' at column 0 -- a wrapped
  # continuation is indented and is not mistaken for one.
  fm="$(awk 'NR==1{next} /^---$/{exit} {print}' "${skill_md}")"
  if [[ -z "${fm}" ]]; then
    report "${rel}: frontmatter is empty or never closes"
    continue
  fi

  keys="$(printf '%s\n' "${fm}" | awk -F: '/^[A-Za-z][A-Za-z0-9_-]*:/ {print $1}')"
  for key in ${keys}; do
    case " ${ALLOWED_KEYS} " in
    *" ${key} "*) ;;
    *) report "${rel}: unsupported frontmatter key '${key}' (allowed: ${ALLOWED_KEYS})" ;;
    esac
  done

  name="$(printf '%s\n' "${fm}" | awk -F': *' '/^name:/ {print $2; exit}' | tr -d '"'"'")"
  if [[ "${name}" != "${want_name}" ]]; then
    report "${rel}: name is '${name}' but its directory is '${want_name}'"
  fi
  if ! [[ "${name}" =~ ^[a-z0-9]+(-[a-z0-9]+)*$ ]]; then
    report "${rel}: name '${name}' is not lowercase kebab-case"
  fi

  desc="$(printf '%s\n' "${fm}" | awk '/^description:/{sub(/^description: */,""); print; exit}')"
  if [[ -z "${desc}" ]]; then
    report "${rel}: no description"
  elif [[ ${#desc} -gt ${DESC_MAX} ]]; then
    report "${rel}: description is ${#desc} characters, over the ${DESC_MAX} cap -- a host truncates the listing silently"
  fi

  frozen="$(printf '%s\n' "${fm}" | awk -F': *' '/^disable-model-invocation:/ {print $2; exit}')"
  openai_yaml="${dir}/agents/openai.yaml"
  case "${frozen}" in
  "" | false)
    if [[ -f "${openai_yaml}" ]] && grep -q 'allow_implicit_invocation: *false' "${openai_yaml}"; then
      report "${rel}: Codex policy freezes this skill but the frontmatter does not -- it would be explicit-only on Codex and auto-invoked on Claude and Kimi"
    fi
    ;;
  true)
    if [[ ! -f "${openai_yaml}" ]]; then
      report "${rel}: disable-model-invocation is true but agents/openai.yaml is missing -- Codex does not read that key and would still auto-invoke"
    elif ! grep -q 'allow_implicit_invocation: *false' "${openai_yaml}"; then
      report ".agents/skills/${want_name}/agents/openai.yaml: does not set allow_implicit_invocation: false"
    fi
    if [[ -f "${AGENTS_MD}" ]] && ! grep -qF -- "${want_name}" "${AGENTS_MD}"; then
      report "AGENTS.md: names no trigger for '${want_name}', whose description a frozen skill no longer gets loaded -- nothing would tell the model it exists"
    fi
    ;;
  *)
    report "${rel}: disable-model-invocation is '${frozen}', expected true or false"
    ;;
  esac
done

[[ ${skill_count} -gt 0 ]] || cannot_run "no SKILL.md found under ${SKILLS_DIR}"

# --- exclusion lists --------------------------------------------------------------------------

for f in "${RULE_JSON}" "${COPILOT_MD}" "${REVIEW_MD}" "${AGENTS_MD}"; do
  [[ -f "${f}" ]] || cannot_run "missing ${f}"
done

# The "exclude" array of rule.json, one quoted path per line. An empty read means the extractor
# stopped matching the file's shape, which would make every assertion below vacuously true -- so
# it is an error, not a pass.
excludes="$(awk '
  /"exclude"[[:space:]]*:[[:space:]]*\[/ { inside = 1; next }
  inside && /\]/                        { exit }
  inside                                { gsub(/[",]/, ""); gsub(/^[[:space:]]+|[[:space:]]+$/, ""); if ($0 != "") print }
' "${RULE_JSON}")"

if [[ -z "${excludes}" ]]; then
  cannot_run "read no exclude entries from ${RULE_JSON}; the extractor no longer matches its shape"
fi

if [[ "${excludes}" != "$(LC_ALL=C sort <<<"${excludes}")" ]]; then
  report ".opencodereview/rule.json: the exclude list is not sorted (LC_ALL=C)"
fi

# Reduce a glob to the literal a human would write in prose: drop a trailing /** or /**/*.ext, and
# drop a leading **/ so "**/*_test.go" is looked for as "_test.go".
while IFS= read -r glob; do
  needle="${glob}"
  needle="${needle%/\*\*}"
  needle="${needle%/\*\*/\*.go}"
  needle="${needle#\*\*/}"
  needle="${needle#\*}"
  needle="${needle%\*}"
  [[ -n "${needle}" ]] || continue
  grep -qF -- "${needle}" "${COPILOT_MD}" ||
    report ".github/copilot-instructions.md: does not name '${needle}', excluded by .opencodereview/rule.json"
  grep -qF -- "${needle}" "${REVIEW_MD}" ||
    report ".agents/skills/gpustack-operator-code-review/SKILL.md: does not name '${needle}', excluded by .opencodereview/rule.json"
done <<<"${excludes}"

[[ ${findings} -eq 0 ]] || exit 1
exit 0
