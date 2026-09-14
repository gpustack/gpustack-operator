#!/usr/bin/env bash
# check-api-descriptions-selftest.sh — proves check-api-descriptions.sh can fail, and proves the
# four places it must stay quiet.
#
# The check is a vocabulary matched against generated text, and both halves of that can go wrong
# without a symptom. A term that matches nothing reports a clean tree; a term matched as a bare
# substring reports half the file and gets switched off within a day. Measured on this repository
# before the anchors went in: `struct` matched "instruction", "constructed" and "structural", and
# `is a STRUCT` matched "is a STRUCTURED FIELD". Each of those is pinned below as MUST NOT REPORT,
# next to the ownership scope and the pardon list, which are pinned the same way.
#
# The cases that assert silence are the more important half. A check nobody has seen fail and a
# check that cannot fail read identically from the outside, and so do a scope that excludes
# upstream and a scope that excludes everything.
#
# Usage: bash hack/check-api-descriptions-selftest.sh [repo-root]
set -euo pipefail

ROOT="${1:-}"
[ -n "$ROOT" ] || ROOT="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
CHECK="$ROOT/hack/check-api-descriptions.sh"

MINI="$(mktemp -d)"
trap 'rm -rf "$MINI"' EXIT
TREE="$MINI/tree"

fails=0
pass() { printf 'PASS  %s\n' "$1"; }
fail() { printf 'FAIL  %s\n' "$1"; fails=$((fails + 1)); }

WORKER_OPENAPI="api/worker/zz_generated.openapi.go"
ROOT_OPENAPI="api/zz_generated.openapi.go"
CRDS="api/worker/v1alpha1/zz_generated.crds.go"

# A tree the check is clean on. It carries one definition of ours, one upstream definition, the
# pardoned description, and every near-miss that a bare substring would have reported. The pardon
# has to be present in every case that expects success, because a pardon matching nothing is
# itself a finding.
build() {
  rm -rf "${TREE:?}"
  mkdir -p "$TREE/api/worker/v1alpha1"

  cat > "$TREE/$WORKER_OPENAPI" <<'EOF'
package worker

func schema_gpustack_api_worker_v1alpha1_Thing(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{
			SchemaProps: spec.SchemaProps{
				Description: "Thing is one thing.",
				Properties: map[string]spec.Schema{
					"placements": {
						SchemaProps: spec.SchemaProps{
							Description: "Placements is the legal placement set, constructed by the manufacturer at detect time.",
						},
					},
					"cache": {
						SchemaProps: spec.SchemaProps{
							Description: "L1I is the L1 instruction cache size in bytes.",
						},
					},
					"transport": {
						SchemaProps: spec.SchemaProps{
							Description: "Transport uses structural-schema defaulting, which does not descend into an absent object.",
						},
					},
					"resources": {
						SchemaProps: spec.SchemaProps{
							Description: "Resources is a STRUCTURED FIELD, because admission and scheduling both read it.",
						},
					},
					"owner": {
						SchemaProps: spec.SchemaProps{
							Description: "Owner is a typed referenced object located in the same namespace.",
						},
					},
					"boundTo": {
						SchemaProps: spec.SchemaProps{
							Description: "BoundTo carries no kind, because it is not a usedBy: it is the back pointer from a registered domain to the object that holds it.",
						},
					},
				},
			},
		},
	}
}

func schema_k8sio_api_core_v1_ReplicationControllerSpec(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{
			SchemaProps: spec.SchemaProps{
				Properties: map[string]spec.Schema{
					"replicas": {
						SchemaProps: spec.SchemaProps{
							Description: "Replicas is the number of desired replicas. This is a pointer to distinguish between explicit zero and unspecified.",
						},
					},
				},
			},
		},
	}
}
EOF

  cat > "$TREE/$ROOT_OPENAPI" <<'EOF'
package api

func schema_gpustackai_api_Other(ref common.ReferenceCallback) common.OpenAPIDefinition {
	return common.OpenAPIDefinition{
		Schema: spec.Schema{
			SchemaProps: spec.SchemaProps{
				Description: "Other is another thing.",
			},
		},
	}
}
EOF

  cat > "$TREE/$CRDS" <<'EOF'
package v1alpha1

func crd_gpustack_api_worker_v1alpha1_Thing() *v1.CustomResourceDefinition {
	return &v1.CustomResourceDefinition{
		Spec: v1.CustomResourceDefinitionSpec{
			Properties: map[string]v1.JSONSchemaProps{
				"hostPath": {
					Description: "type for HostPath Volume\nDefaults to \"\"\nThis is a pointer. More info: https://kubernetes.io/docs/concepts/storage/volumes",
				},
				"note": {
					Description: "Note is an ordinary sentence with no machinery in it.",
				},
			},
		},
	}
}
EOF
}

run() { bash "$CHECK" "$TREE" >"$MINI/out" 2>"$MINI/err"; }
rc_of() { local rc=0; run || rc=$?; printf '%s' "$rc"; }

# 1. The tree above is clean. Everything in it that a wider or scope-free check would have
#    reported is in it on purpose.
build
rc="$(rc_of)"
if [ "$rc" = "0" ]; then
  pass "clean tree: near-misses, upstream text and the pardoned description all stay quiet"
else
  fail "clean tree reported something (exit $rc)"
  sed 's/^/      /' "$MINI/out" "$MINI/err"
fi

# 2. It can fail, and it names the field rather than only the file.
build
sed -i.bak 's|Note is an ordinary sentence with no machinery in it.|Note is a POINTER, so unset and empty are different.|' "$TREE/$CRDS"
rc="$(rc_of)"
if [ "$rc" = "1" ] && grep -q 'note' "$MINI/out" && grep -q 'pointer' "$MINI/out"; then
  pass "plants a vocabulary term: reports it, and names the field"
else
  fail "planted term not reported as a finding naming its field (exit $rc)"
  sed 's/^/      /' "$MINI/out"
fi

# 3. A type-level description is named as one. Reporting the last property of the definition
#    above sends the reader to a field the sentence has nothing to do with.
build
sed -i.bak 's|Thing is one thing.|Thing is a STRUCT rather than a bool, so an absent key differs from a false one.|' "$TREE/$WORKER_OPENAPI"
rc="$(rc_of)"
if [ "$rc" = "1" ] && grep -q 'type-level description' "$MINI/out"; then
  pass "a type-level description is reported as one, not as the property above it"
else
  fail "type-level description misattributed or not reported (exit $rc)"
  sed 's/^/      /' "$MINI/out"
fi

# 4. MUST NOT REPORT: the same sentence inside an upstream definition. Upstream descriptions are
#    out of scope rather than exempted, because this repository cannot fix them.
build
sed -i.bak 's|This is a pointer to distinguish between explicit zero and unspecified.|This is a POINTER and a STRUCT and nil-able.|' "$TREE/$WORKER_OPENAPI"
rc="$(rc_of)"
if [ "$rc" = "0" ]; then
  pass "upstream definitions stay out of scope even carrying every term at once"
else
  fail "reported a description owned by upstream (exit $rc)"
  sed 's/^/      /' "$MINI/out"
fi

# 5. MUST NOT REPORT: upstream field docs inlined into one of our CRDs, recognised by upstream's
#    own documentation link. Case 1 already runs with that description carrying "pointer" and
#    stays quiet, which on its own would also be explained by the file being skipped entirely or
#    by the term never matching. So this case removes ONLY the marker and asserts the same
#    sentence now fires: what is being tested is the marker, and nothing else changes.
build
sed -i.bak 's| More info: https://kubernetes.io/docs/concepts/storage/volumes||' "$TREE/$CRDS"
rc="$(rc_of)"
if [ "$rc" = "1" ] && grep -q 'hostPath' "$MINI/out"; then
  pass "the upstream marker is what silences inlined docs: removing it makes the same sentence fire"
else
  fail "removing the upstream marker did not make the inlined description fire (exit $rc)"
  sed 's/^/      /' "$MINI/out"
fi

# 6. A pardon that no longer matches is reported, so the list expires by asking to be deleted
#    rather than by quietly widening what passes.
build
sed -i.bak 's|it is the back pointer from a registered domain to the object that holds it|it is the reverse link from a registered domain to the object that holds it|' "$TREE/$WORKER_OPENAPI"
rc="$(rc_of)"
if [ "$rc" = "1" ] && grep -q 'matches nothing' "$MINI/out"; then
  pass "a pardon that stopped matching is reported instead of silently kept"
else
  fail "stale pardon not reported (exit $rc)"
  sed 's/^/      /' "$MINI/out"
fi

# 7. An empty subject is a broken instrument, not a clean tree. This is the failure that would
#    otherwise make every case above pass for the wrong reason.
build
for f in "$WORKER_OPENAPI" "$ROOT_OPENAPI" "$CRDS"; do
  grep -v 'Description:' "$TREE/$f" > "$TREE/$f.stripped" && mv "$TREE/$f.stripped" "$TREE/$f"
done
rc="$(rc_of)"
if [ "$rc" = "2" ]; then
  pass "a scope that selected no description reports a broken instrument, not success"
else
  fail "empty subject did not report as unable to run (exit $rc)"
  sed 's/^/      /' "$MINI/out" "$MINI/err"
fi

# 8. A missing artifact is also unable-to-run. The check reads what a cluster serves; with the
#    artifact gone it has no subject, and saying "clean" there is the same lie as case 7.
build
rm -f "$TREE/$CRDS"
rc="$(rc_of)"
if [ "$rc" = "2" ]; then
  pass "a missing generated artifact reports as unable to run"
else
  fail "missing artifact did not report as unable to run (exit $rc)"
  sed 's/^/      /' "$MINI/out" "$MINI/err"
fi

# 9. One declared artifact silently leaving the scope, while the others keep matching. This is the
#    failure a total cannot see: the other two files keep the count large, so an assertion on the
#    sum stays green with a third of the subject unread. Measured against the previous version of
#    the check on this same input: it exited 0 and never named the file.
build
sed -i.bak 's|^func schema_gpustackai_|func schema_RENAMEDgpustackai_|' "$TREE/$ROOT_OPENAPI"
rc="$(rc_of)"
if [ "$rc" = "2" ] && grep -q "$ROOT_OPENAPI" "$MINI/err"; then
  pass "one artifact dropping out of scope is reported, and the file is named"
else
  fail "an artifact contributing nothing went unreported or unnamed (exit $rc)"
  sed 's/^/      /' "$MINI/out" "$MINI/err"
fi

if [ "$fails" -gt 0 ]; then
  printf '\n%d self-test case(s) failed; the description check is not trustworthy\n' "$fails" >&2
  exit 1
fi
printf '\nall self-test cases passed\n'
exit 0
