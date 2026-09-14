#!/usr/bin/env bash
# check-api-descriptions.sh — Go mechanics in the API descriptions a cluster renders.
#
# Subject is the GENERATED artifacts, not the Go doc comments they come from. The defect being
# guarded is defined by who reads the sentence: `kubectl explain` and the OpenAPI document show
# these strings to whoever operates the cluster, and a sentence about pointers or Go structs
# reaches that reader instead of staying with the maintainer editing the struct. Checking the
# source would report a comment that never renders and miss one that does, so the artifacts are
# the subject and the source is where a finding gets fixed.
#
# WHAT IT REPORTS: a term from the vocabulary below, word-anchored, inside a Description this
# repository authors.
#
# THE VOCABULARY IS A VOCABULARY, and that is its limit rather than an oversight. It pins the
# forms of this defect that have already been seen; it cannot recognise a new one. A description
# added tomorrow reading "nil-able, because unset and empty differ" is the same defect in words no
# entry here matches, and this check will pass on it. So a green run means "no form we have met
# before", NEVER "no Go mechanics reach the operator" -- the class still rests on a human reading
# the description. Widening the terms until they cover the class is not available either: measured
# on this tree, bare `struct` also matches "instruction", "constructed" and "structural", and
# `is a STRUCT` matches "is a STRUCTURED FIELD", which is why every term is anchored below.
#
# OWNERSHIP IS READ FROM THE ARTIFACT, not from a list of sentences to ignore. The OpenAPI files
# carry it in the function name -- `schema_gpustack_*` is ours, `schema_k8sio_*`,
# `schema_pkg_apis_*` and `schema_apimachinery_*` are upstream's -- so upstream descriptions are
# out of SCOPE rather than exempted. The CRD file has no such boundary because upstream field docs
# are inlined into our own schemas; there they are recognised by the documentation link upstream
# puts in them, and that one IS an exemption.
#
# THE THREE FILES BELOW ARE NOT A SUBSET OF WHAT A CLUSTER RENDERS. They are that set, written out.
# A description not carried by one of them is not rendered by `kubectl explain`, so it is not read by
# an operator, so it is not an instance of this defect at all. The scope is the definition expressed
# as file names rather than a convenient place to stop looking, and what follows is the test of that
# equality -- a reader who doubts it can re-run these four rather than redo the survey.
#
#   1. A description written once in api/**/*.go reaches FIVE carriers: the source comment,
#      generated.proto, the CRD document, the OpenAPI definitions, and the apply-configuration
#      client. Searching the whole tree for files carrying generated descriptions returns exactly
#      the three below, plus a generator fixture under gen/ and the vendored trees under staging/.
#   2. A CRD field reaches two of the three. An aggregated-API field reaches the worker OpenAPI
#      alone. So coverage varies BY TYPE, and "three artifacts" means every type at least once
#      rather than every type three times.
#   3. The root OpenAPI is the SOLE carrier of the Setting family, whose four types appear in
#      neither of the others. It earns its place rather than duplicating one.
#   4. The two carriers left out are read by whoever writes Go, and the vocabulary cannot even be
#      applied to them: `struct` there is the language rather than a leak, measured at 90
#      occurrences under the apply-configuration directory and 21 in one API source file. Nor is
#      anything lost, since all five are verbatim copies of one source comment -- a leak cannot
#      reach generated.proto without existing at the source, and therefore in whichever of the
#      three below carries that type.
#
# WHAT THIS CHECK CANNOT NOTICE, stated with the action that would. It guards the three artifacts
# NAMED BELOW. A carrier that appears later is not in its field of view, and no amount of green here
# says otherwise: the per-file assertion further down catches a declared artifact silently dropping
# out of scope, which is the opposite failure. The measurement that finds a NEW carrier is the
# whole-tree enumeration in point 1 -- files containing a generated `Description: "` -- run on
# 2026-09-14, when it returned exactly these three. To re-confirm coverage, run that enumeration
# again; reading this check's exit code answers a different question. It is not done here on every
# run because scanning the tree is not what a per-commit gate should cost, and because the answer
# changes when a generator is added rather than when a description is edited.
#
# EVERY EXEMPTION MUST STILL MATCH SOMETHING. An exemption that has stopped matching is reported,
# so it expires by asking to be deleted rather than by quietly widening what passes. The same
# applies to the scope itself: a run that examined no description of ours at all is reported as a
# broken instrument, because an empty subject passes every check ever written.
#
# Exit codes: 0 nothing found, 1 a finding, 2 the check could not run.
#
# Usage: bash hack/check-api-descriptions.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"

# The artifacts a cluster serves. Two shapes: the OpenAPI definitions, which carry ownership in
# their function names, and the CRD document, which does not.
OPENAPI_FILES=(
  "api/worker/zz_generated.openapi.go"
  "api/zz_generated.openapi.go"
)
CRD_FILES=(
  "api/worker/v1alpha1/zz_generated.crds.go"
)

# The vocabulary. Each entry is matched with a letter boundary on both sides; see the note above
# for what happens without one. Lowercase here, compared case-insensitively.
VOCABULARY=(
  "pointer"
  "struct"
  "nil-able"
  "nilable"
  "compile-time"
  "import cycle"
)

# Descriptions that contain a vocabulary term and are still correct. Each entry is a substring
# unique to the description it pardons, so an edit to that sentence retires the pardon with it.
# A pardon matching nothing is a finding: see the note above.
EXEMPT_DESCRIPTIONS=(
  "it is the back pointer from a registered domain"
)

# The marker upstream leaves on its own field documentation when it is inlined into one of our
# CRDs. It is upstream's own convention rather than something this repository adds, which is why
# it can be trusted to identify upstream text.
UPSTREAM_DOC_MARKER="kubernetes.io/docs"

fail() { printf 'check-api-descriptions: %s\n' "$1" >&2; }

for rel in "${OPENAPI_FILES[@]}" "${CRD_FILES[@]}"; do
  if [ ! -f "$ROOT/$rel" ]; then
    fail "generated artifact not found: $rel"
    fail "this check reads what a cluster serves; with the artifact missing it has no subject"
    exit 2
  fi
done

# One awk pass per file. It tracks the most recent property key so a finding names the field an
# operator would have to look at, carries the owning definition so a reader can find it, and
# prints the matched term so the reason for the hit is visible without re-deriving it -- a whole
# Description line does not show which word fired.
scan_file() {
  local path="$1" rel="$2" owned_by_name="$3"
  awk -v REL="$rel" -v BYNAME="$owned_by_name" -v MARKER="$UPSTREAM_DOC_MARKER" \
      -v VOCAB="$(printf '%s\n' "${VOCABULARY[@]}" | paste -sd '|' -)" \
      -v EXEMPT="$(printf '%s\n' "${EXEMPT_DESCRIPTIONS[@]}" | paste -sd '\n' -)" '
    BEGIN {
      nv = split(VOCAB, terms, "|")
      ne = split(EXEMPT, pardons, "\n")
      ours = (BYNAME == "no")
    }
    # Ownership, where the artifact states it. Every definition boundary also clears the property
    # key: a type-level Description is preceded by the LAST property of the definition above, and
    # reporting that name sends the reader to a field the sentence has nothing to do with.
    /^func / {
      definition = $0
      sub(/^func /, "", definition)
      sub(/\(.*$/, "", definition)
      field = ""
      if (BYNAME == "yes") { ours = ($0 ~ /^func schema_gpustack/) }
    }
    # The property key a Description belongs to.
    /^[ \t]*"[a-zA-Z0-9_.-]+": \{/ {
      field = $0
      sub(/^[ \t]*"/, "", field)
      sub(/": \{.*$/, "", field)
    }
    !ours { next }
    !/Description:/ { next }
    # Upstream field documentation inlined into one of our own schemas.
    index($0, MARKER) { next }
    {
      lower = tolower($0)
      for (e = 1; e <= ne; e++) {
        if (pardons[e] != "" && index(lower, tolower(pardons[e]))) {
          print "PARDONED\t" pardons[e]
          next
        }
      }
      for (t = 1; t <= nv; t++) {
        term = terms[t]
        if (match(lower, "(^|[^a-z])" term "([^a-z]|$)")) {
          shown = (field == "" ? definition " (type-level description)" : definition "." field)
          print "FINDING\t" REL ":" NR "\t" shown "\t" term
        }
      }
      print "EXAMINED"
    }
  ' "$path"
}

findings=0
examined=0
examined_this=0
blind=0
declare -a pardons_seen=()

collect() {
  local path="$1" rel="$2" byname="$3" line kind
  examined_this=0
  while IFS= read -r line; do
    kind="${line%%$'\t'*}"
    case "$kind" in
      EXAMINED) examined=$((examined + 1)); examined_this=$((examined_this + 1)) ;;
      PARDONED) pardons_seen+=("${line#*$'\t'}") ;;
      FINDING)
        findings=$((findings + 1))
        printf '%s\n' "${line#FINDING$'\t'}" | awk -F'\t' \
          '{ printf "%s: %s carries \"%s\", which describes Go rather than the field\n", $1, $2, $3 }'
        ;;
    esac
  done < <(scan_file "$path" "$rel" "$byname")
}

# EVERY DECLARED ARTIFACT HAS TO CONTRIBUTE, and the assertion is per file rather than on the total.
# A total is a self-generated denominator: it is produced by the same scope it is meant to police, so
# it can only report the failure where EVERY artifact drops out at once. One artifact silently
# leaving the scope -- its generated function names renamed, say, while the other two keep matching
# -- leaves the total large and this check green, with a third of the subject no longer read. That
# is the one failure a gate must not have, because it does not look like a missing gate: it looks
# like a passing one. Naming the file is the point of the assertion; "some artifact contributed
# nothing" would leave whoever fixes it guessing which.
for rel in "${OPENAPI_FILES[@]}"; do
  collect "$ROOT/$rel" "$rel" "yes"
  if [ "$examined_this" -eq 0 ]; then
    fail "$rel contributed no description of ours; the ownership scope no longer selects anything there"
    blind=$((blind + 1))
  fi
done
for rel in "${CRD_FILES[@]}"; do
  collect "$ROOT/$rel" "$rel" "no"
  if [ "$examined_this" -eq 0 ]; then
    fail "$rel contributed no description of ours; the ownership scope no longer selects anything there"
    blind=$((blind + 1))
  fi
done

if [ "$blind" -gt 0 ]; then
  fail "either the generator changed its function names or the artifacts are not what they were"
  exit 2
fi

# A pardon that no longer matches has outlived the sentence it was written for. Reporting it is
# what keeps the list from silently growing into a second, unreviewed vocabulary.
stale=0
for pardon in "${EXEMPT_DESCRIPTIONS[@]}"; do
  seen=0
  for used in ${pardons_seen+"${pardons_seen[@]}"}; do
    [ "$used" = "$pardon" ] && seen=1 && break
  done
  if [ "$seen" -eq 0 ]; then
    printf 'EXEMPT_DESCRIPTIONS carries an entry that now matches nothing: %s\n' "$pardon"
    printf '  the description it pardoned has changed or gone; delete the entry\n'
    stale=$((stale + 1))
  fi
done

if [ "$findings" -gt 0 ] || [ "$stale" -gt 0 ]; then
  exit 1
fi
exit 0
