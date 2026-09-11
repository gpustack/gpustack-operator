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
# Kimi. Codex ignores that key: a skill carrying only it still appears in the catalog, with no
# diagnostic on stderr, so nothing signals that the freeze did not take. Codex takes the policy
# from agents/openai.yaml instead -- a skill carrying only that file is absent from the catalog and
# still runs under `$name`. Both files are therefore required, and both are asserted here, or the
# skill is frozen on two hosts and auto-invoked on the third. Freezing also removes the skill's
# description from what a host loads, so AGENTS.md carries the trigger instead: a frozen skill with
# no trigger row is one the model can no longer learn exists, and nothing about that looks broken.
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

# Every host truncates the catalog listing, and each picks its own limit: measured at 1536 characters
# on Claude Code 2.1.261, 1024 on codex-cli 0.154.0, and 247 on Kimi Code 0.42.0. The cap is the
# tightest of the three, because a description that fits only the widest host is silently cut on the
# other two -- that already happened here and cost one skill half its discovery text. The longest
# description in the tree sits at 240 under this cap.
DESC_MAX=247

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
  #
  # Reading to end-of-file and reading to the closing '---' produce the same lines, so the close is
  # asserted separately. Without it a file that never closes its frontmatter passes: every key is
  # found, and nothing reports that the body was parsed as metadata.
  if ! awk 'NR>1 && /^---$/{found=1; exit} END{exit !found}' "${skill_md}"; then
    report "${rel}: frontmatter never closes -- no second '---'"
    continue
  fi
  fm="$(awk 'NR==1{next} /^---$/{exit} {print}' "${skill_md}")"
  if [[ -z "${fm}" ]]; then
    report "${rel}: frontmatter is empty"
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

  # Strip the surrounding YAML quotes before measuring: the cap is on the value a host renders, and
  # counting the delimiters would reject a description two characters shorter than the real limit.
  desc="$(printf '%s\n' "${fm}" | awk '/^description:/{sub(/^description: */,""); print; exit}')"
  desc="${desc%\"}"
  desc="${desc#\"}"
  if [[ -z "${desc}" ]]; then
    report "${rel}: no description"
  elif [[ ${#desc} -gt ${DESC_MAX} ]]; then
    report "${rel}: description is ${#desc} characters, over the ${DESC_MAX} cap -- a host truncates the listing silently"
  fi

  frozen="$(printf '%s\n' "${fm}" | awk -F': *' '/^disable-model-invocation:/ {print $2; exit}')"
  openai_yaml="${dir}/agents/openai.yaml"

  # policy_ok <file>: the file is exactly the two-line Codex policy and nothing else. A substring
  # search would accept the same text commented out, or nested under an unrelated key, and either
  # of those leaves the skill auto-invoked on Codex while reading as frozen here.
  policy_ok() {
    [[ "$(awk 'NF' "$1")" == "policy:
  allow_implicit_invocation: false" ]]
  }

  case "${frozen}" in
  "" | false)
    if [[ -f "${openai_yaml}" ]] && policy_ok "${openai_yaml}"; then
      report "${rel}: Codex policy freezes this skill but the frontmatter does not -- it would be explicit-only on Codex and auto-invoked on Claude and Kimi"
    fi
    ;;
  true)
    if [[ ! -f "${openai_yaml}" ]]; then
      report "${rel}: disable-model-invocation is true but agents/openai.yaml is missing -- Codex does not read that key and would still auto-invoke"
    elif ! policy_ok "${openai_yaml}"; then
      report ".agents/skills/${want_name}/agents/openai.yaml: is not exactly 'policy:' / '  allow_implicit_invocation: false'"
    fi
    # The name has to sit in an actual trigger row. Searching the whole file accepts a sentence that
    # mentions the skill while saying nothing about when to offer it -- including one that denies it.
    if [[ -f "${AGENTS_MD}" ]] && ! grep -F -- "${want_name}" "${AGENTS_MD}" | grep -q '→'; then
      report "AGENTS.md: has no '<change> → ${want_name}' row; a frozen skill's description is never loaded, so nothing else would tell the model it exists"
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
# One quoted path per line, and the array must close on a line of its own. The previous version
# stopped at the first line containing ']' without asking whether the bracket was inside a quoted
# glob: a single malformed entry such as "**/*_test.go]" truncated the list silently, and every
# assertion below then passed over the handful of entries that survived. So a line that is not a
# quoted entry is a refusal to run, not a short list.
excludes="$(awk '
  /"exclude"[[:space:]]*:[[:space:]]*\[/                      { inside = 1; next }
  inside && /^[[:space:]]*\][[:space:]]*,?[[:space:]]*$/      { print "__CLOSED__"; exit }
  inside && /^[[:space:]]*"[^"]*"[[:space:]]*,?[[:space:]]*$/ {
      gsub(/^[[:space:]]*"|"[[:space:]]*,?[[:space:]]*$/, ""); print; next
  }
  inside                                                      { print "__MALFORMED__"; exit }
' "${RULE_JSON}")"

case "${excludes}" in
*__MALFORMED__*) cannot_run "${RULE_JSON}: the exclude array holds a line that is not a single quoted path" ;;
*__CLOSED__*) excludes="${excludes%__CLOSED__}"; excludes="${excludes%$'\n'}" ;;
*) cannot_run "${RULE_JSON}: the exclude array never closes; the extractor no longer matches its shape" ;;
esac

if [[ -z "${excludes}" ]]; then
  cannot_run "read no exclude entries from ${RULE_JSON}"
fi

if [[ "${excludes}" != "$(LC_ALL=C sort <<<"${excludes}")" ]]; then
  report ".opencodereview/rule.json: the exclude list is not sorted (LC_ALL=C)"
fi

# Reduce a glob to the literal a human would write in prose, KEEPING the trailing slash of a
# directory. Dropping it is what made this assertion vacuous: "gen/**" reduced to "gen", which
# occurs inside "**/generated.*", so the rule passed with every literal mention of gen/ deleted
# from both files. "gen/" occurs in neither.
while IFS= read -r glob; do
  needle="${glob}"
  needle="${needle%\*\*/\*.go}"
  needle="${needle%\*\*}"
  needle="${needle#\*\*/}"
  needle="${needle#\*}"
  needle="${needle%\*}"
  [[ -n "${needle}" ]] || continue
  grep -qF -- "${needle}" "${REVIEW_MD}" ||
    report ".agents/skills/gpustack-operator-code-review/SKILL.md: does not name '${needle}', excluded by .opencodereview/rule.json"
done <<<"${excludes}"

# copilot-instructions.md states the list verbatim rather than in prose, so it gets the stronger
# assertion: the two lists must be equal, in order. "Is this path mentioned somewhere in the file"
# is not enough there -- four of these globs reduce to a literal (`_test.go`, `generated.`) that the
# document's other sections already contain, so their mention would be satisfied by a sentence that
# has nothing to do with the exclusion list.
mirrored="$(awk '
  /^## Out of scope/      { inside = 1; next }
  inside && /^## /        { exit }
  inside && /^- `.*`$/    { gsub(/^- `|`$/, ""); print }
' "${COPILOT_MD}")"

if [[ "${mirrored}" != "${excludes}" ]]; then
  report ".github/copilot-instructions.md: its Out-of-scope list is not the exclude list of .opencodereview/rule.json, verbatim and in the same order"
  diff <(printf '%s\n' "${excludes}") <(printf '%s\n' "${mirrored}") |
    sed 's/^/check-skills:   /' >&2
fi

[[ ${findings} -eq 0 ]] || exit 1
exit 0
