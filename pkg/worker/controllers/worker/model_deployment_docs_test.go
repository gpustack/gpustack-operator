package worker

import (
	"fmt"
	"os"
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
