package worker

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// _ModelDeploymentDocsPath is the page that documents what the operator owns.
//
// Relative to this package, and checked as a whole line rather than by heading, so moving the table
// inside the page is free and moving the page is not.
const _ModelDeploymentDocsPath = "../../../../docs/reference/model-deployment.md"

// TestModelDeploymentOwnedKeysDocs pins the owned-key table on the reference page to the catalog the
// code actually enforces.
//
// THIS EXISTS BECAUSE THE CODE SIDE ALREADY HAD A GUARD AND THE DOCS SIDE HAD NONE, and the gap
// produced exactly the defect it predicts. `TestModelDeploymentOwnedAndDefaultedCannotDisagree`
// fails when the renderer emits a key the catalog does not own — it went red on its own the day
// `MOONCAKE_TENANT_ID` was added — so the CODE could not drift. Nothing tied the catalog to the
// PAGE, so the page went on omitting that same variable from the sglang row while the webhook
// refused it: the table a user consults to learn what is safe to set said it was safe to set the
// one key whose ownership is a security property. A reviewer found it; nothing failed.
//
// The direction is one-way on purpose. Every owned key must appear in the page's row for its engine,
// because a key missing from the page reads as a key the user may set. A name on the page that the
// code does not own is not asserted here: the page is allowed to describe a key it merely explains,
// and pinning that direction too would make prose edits fail a unit test.
func TestModelDeploymentOwnedKeysDocs(t *testing.T) {
	page, err := os.ReadFile(_ModelDeploymentDocsPath)
	require.NoErrorf(t, err, "the owned-key table lives in %s", _ModelDeploymentDocsPath)

	// The row for an engine is the table line that starts with it. Read as a line rather than
	// searched for across the page: `MOONCAKE_MASTER` appearing in some paragraph is not the same
	// fact as it appearing in sglang's row, and only the second tells a reader it is owned there.
	rows := make(map[string]string, len(modelDeploymentOwnedKeys))
	for _, line := range strings.Split(string(page), "\n") {
		for engine := range modelDeploymentOwnedKeys {
			if strings.HasPrefix(strings.TrimSpace(line), fmt.Sprintf("| `%s` |", engine)) {
				rows[engine] = line
			}
		}
	}

	for engine, owned := range modelDeploymentOwnedKeys {
		t.Run(engine, func(t *testing.T) {
			row, ok := rows[engine]
			require.Truef(t, ok, "%s documents no owned-key row for engine %q", _ModelDeploymentDocsPath, engine)

			for _, key := range append(append([]string{}, owned.Args...), owned.Env...) {
				assert.Containsf(t, row, "`"+key+"`",
					"%q is owned on %q and the page's row for it does not name the key: a key missing "+
						"from this table reads as one the user may set", key, engine)
			}
		})
	}
}

// _ModelDeploymentAPITypesPath is the file whose Conditions field comment lists the status axes.
//
// Relative to this package. That comment is copied into the CRD and the OpenAPI document by
// generation, so it is what `kubectl explain modeldeployment.status.conditions` prints.
const _ModelDeploymentAPITypesPath = "../../../../api/worker/v1alpha1/model_deployment.go"

// _ModelDeploymentConditionDecl matches one condition-type declaration belonging to this kind. The
// prefix is what excludes the other kinds' conditions, which live in this same package.
var _ModelDeploymentConditionDecl = regexp.MustCompile(
	`ModelDeploymentCondition[A-Za-z]+\s+kubeapistatus\.ConditionType\s*=\s*"([A-Za-z]+)"`)

// TestModelDeploymentConditionAxesAreListedOnTheField pins the axis list in the Conditions field
// comment to the condition types this package declares.
//
// THIS EXISTS BECAUSE THE LIST WENT STALE THE FIRST TIME AN AXIS WAS ADDED. RoleKindsReady landed
// with the reference page updated and the controller updated and this comment not, so the rendered
// schema named four axes while status carried five. Nothing failed: prose has no compiler, and the
// generated artifacts faithfully carried the stale sentence to every reader of `kubectl explain`.
//
// The set is DERIVED from the declarations rather than written out here. A list in this file would
// be a third copy of the same enumeration, stale in the same way and for the same reason, and it
// would stay silent about exactly the axis nobody remembered to add to it.
//
// One-way, like the owned-key test above: every declared axis must be named in the comment. A name
// in the comment that no constant declares is not asserted, so prose may still explain something.
func TestModelDeploymentConditionAxesAreListedOnTheField(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err)

	axes := make([]string, 0, 8)

	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}

		body, err := os.ReadFile(source)
		require.NoErrorf(t, err, "reading %s", source)

		for _, match := range _ModelDeploymentConditionDecl.FindAllStringSubmatch(string(body), -1) {
			axes = append(axes, match[1])
		}
	}

	// Without this the test passes by finding nothing: a declaration whose spelling drifts away from
	// the pattern would empty the set, and an assertion over an empty set says nothing at all.
	require.NotEmptyf(t, axes, "%s matched no condition declaration in this package, so this test "+
		"would assert nothing", _ModelDeploymentConditionDecl)

	types, err := os.ReadFile(_ModelDeploymentAPITypesPath)
	require.NoErrorf(t, err, "the axis list lives in %s", _ModelDeploymentAPITypesPath)

	comment := modelDeploymentConditionsFieldComment(string(types))
	require.NotEmptyf(t, comment, "%s carries no Conditions field comment to check",
		_ModelDeploymentAPITypesPath)

	for _, axis := range axes {
		assert.Containsf(t, comment, axis,
			"%q is a condition this deployment reports and the Conditions field comment does not "+
				"name it: an axis missing from that list is an axis `kubectl explain` denies exists",
			axis)
	}
}

// modelDeploymentConditionsFieldComment returns the comment block introducing the Conditions field,
// which is the run of comment lines starting at the one that opens it.
//
// Taken as a block rather than as the whole file so that naming an axis anywhere else — in another
// field's comment, or in a constant — does not satisfy the assertion above.
func modelDeploymentConditionsFieldComment(types string) string {
	const opener = "// Conditions is the finer view"

	lines := strings.Split(types, "\n")

	start := -1

	for i, line := range lines {
		if strings.HasPrefix(strings.TrimSpace(line), opener) {
			start = i
			break
		}
	}

	if start < 0 {
		return ""
	}

	block := make([]string, 0, 8)

	for _, line := range lines[start:] {
		if !strings.HasPrefix(strings.TrimSpace(line), "//") {
			break
		}

		block = append(block, line)
	}

	return strings.Join(block, "\n")
}
