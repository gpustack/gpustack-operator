#!/usr/bin/env bash
# check-review-config.sh - asserts that the two review-exclusion lists this repository keeps in
# separate files still say the same thing.
#
# Two AI reviewers run on a pull request and each reads its scope from its own file:
# `.opencodereview/rule.json` carries an `exclude` array, and `.github/copilot-instructions.md`
# carries an `## Out of scope` list. The second one states in its own words that it mirrors the
# first "verbatim and in the same order", and until now nothing held it to that. The docs gate
# cannot: check-docs.sh builds its page set from README.md, AGENTS.md, docs/** and
# .claude/skills/**, and `.github/` is in none of them.
#
# The failure this exists for is silent by construction. Two reviewers disagreeing about which
# files are in scope produces no error, no red check and no missing output -- it produces a review
# that covers less than a reader believes, and the belief comes from a sentence that was true when
# it was written. Nobody finds that by reading a diff, because each file is internally consistent.
#
# A THIRD LIST EXISTS AND THIS CHECK CANNOT REACH IT. Copilot's real exclusions are configured in
# the repository settings, not in any file here, so `.github/copilot-instructions.md` is a
# description of that configuration rather than the configuration itself. Editing either file below
# therefore leaves a third copy to update by hand. That is stated beside the list rather than
# checked, because there is nothing in the tree to compare against.
#
# Exit codes are three, not two:
#
#   0   the two lists agree
#   1   they disagree -- a finding about the files
#   2   this check could not run -- a finding about itself
#
# Folding 2 into 0 is the specific way a check like this dies: both lists are parsed out of text,
# and a parse that matches nothing yields two empty sets, which compare equal. It would go green on
# the day someone reformats either file, which is exactly the day it was needed. So an empty parse
# is an error about the instrument, never a verdict about the sources.

set -o errexit
set -o nounset
set -o pipefail

ROOT_DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)}"

RULE_JSON="${ROOT_DIR}/.opencodereview/rule.json"
COPILOT_MD="${ROOT_DIR}/.github/copilot-instructions.md"

for f in "${RULE_JSON}" "${COPILOT_MD}"; do
  if [[ ! -f "${f}" ]]; then
    echo "check-review-config: cannot run: ${f} does not exist" >&2
    exit 2
  fi
done

# The `exclude` array, one entry per line, in file order. Bounded by the array's own brackets so a
# later array in the same file cannot leak in.
json_list=$(awk '
  /"exclude"[[:space:]]*:[[:space:]]*\[/ { inside = 1; next }
  inside && /^[[:space:]]*\]/            { inside = 0; next }
  inside && match($0, /"[^"]+"/)         { print substr($0, RSTART + 1, RLENGTH - 2) }
' "${RULE_JSON}")

# The `## Out of scope` list, one entry per line, in file order. Bounded by the next `##` so a later
# section's bullets cannot leak in, and restricted to backticked bullets so prose in the section is
# not read as an entry.
md_list=$(awk '
  /^## Out of scope/       { inside = 1; next }
  inside && /^## /         { inside = 0 }
  inside && /^- `/ && match($0, /`[^`]+`/) { print substr($0, RSTART + 1, RLENGTH - 2) }
' "${COPILOT_MD}")

json_count=$(printf '%s' "${json_list}" | grep -c . || true)
md_count=$(printf '%s' "${md_list}" | grep -c . || true)

# The instrument checks itself before it reports on anything. Either list coming back empty means
# the shape it is parsed out of moved, not that the repository excludes nothing.
if [[ "${json_count}" -eq 0 ]]; then
  echo "check-review-config: cannot run: found no entries in the exclude array of ${RULE_JSON#"${ROOT_DIR}/"}" >&2
  echo "  the array is read between \"exclude\": [ and the matching ], one quoted entry per line" >&2
  exit 2
fi
if [[ "${md_count}" -eq 0 ]]; then
  echo "check-review-config: cannot run: found no entries under '## Out of scope' in ${COPILOT_MD#"${ROOT_DIR}/"}" >&2
  echo "  the list is read between that heading and the next '## ', as lines beginning '- \`'" >&2
  exit 2
fi

if [[ "${json_list}" == "${md_list}" ]]; then
  exit 0
fi

echo "check-review-config: the two review-exclusion lists disagree" >&2
echo "  ${RULE_JSON#"${ROOT_DIR}/"} (exclude array): ${json_count} entries" >&2
echo "  ${COPILOT_MD#"${ROOT_DIR}/"} (## Out of scope): ${md_count} entries" >&2
echo "" >&2
echo "  < is the JSON, > is the Markdown:" >&2
diff <(printf '%s\n' "${json_list}") <(printf '%s\n' "${md_list}") | sed 's/^/  /' >&2 || true
echo "" >&2
echo "  Order counts as well as membership: the Markdown says it mirrors the array verbatim and in" >&2
echo "  the same order, and a reader compares them by eye. Fix whichever one is wrong, then update" >&2
echo "  Copilot's content-exclusion policy in the repository settings to match -- that third copy" >&2
echo "  is not in the tree and nothing here can check it." >&2
exit 1
