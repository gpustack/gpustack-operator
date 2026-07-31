package nodefeature

import (
	"fmt"
	"os"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// _UnitResourcesPresetDocsPath is the reference page every preset must appear on, so that a tier
// assignment always carries the public configuration it was taken from.
const _UnitResourcesPresetDocsPath = "../../docs/reference/instance-type-unit-resources.md"

// TestUnitResourcesPresetDocs keeps the shipped table and its reference page in sync. Provenance
// lives only on the page — the table carries matching facts and nothing else — so an entry missing
// from the page is an entry whose values nobody can audit.
//
// It matches a whole table row rather than the family name alone, for two reasons. A bare substring
// search passes on the wrong row whenever one family name prefixes another ("ascend-910b" sits
// inside "ascend-910b2"), which silently exempts a tenth of the table. And matching the row pins the
// resolved CPU and RAM too, so re-tiering an entry without touching the page fails here rather than
// leaving an operator reading a number the operator no longer stamps.
func TestUnitResourcesPresetDocs(t *testing.T) {
	docs, err := os.ReadFile(_UnitResourcesPresetDocsPath)
	require.NoError(t, err)
	page := string(docs)

	documents := func(t *testing.T, manufacturer, family, tier string) {
		t.Helper()
		values, ok := _UnitResourcesPresetTable.Tiers[tier]
		require.Truef(t, ok, "tier %q of %s is not defined", tier, family)
		row := fmt.Sprintf("| `%s` | `%s` | `%s` | `%s` |", family, tier, values.CPU, values.RAM)
		assert.Containsf(t, page, row, "%s: %s is not documented as %q in %s",
			manufacturer, family, row, _UnitResourcesPresetDocsPath)
	}

	for manufacturer, entries := range _UnitResourcesPresetTable.Families {
		for _, e := range entries {
			documents(t, manufacturer, e.Family, e.Tier)
			for _, v := range e.By {
				documents(t, manufacturer, v.Family, v.Tier)
			}
		}
	}

	for tier, values := range _UnitResourcesPresetTable.Tiers {
		assert.Containsf(t, page, fmt.Sprintf("| `%s` |", tier),
			"tier %q is not documented in %s", tier, _UnitResourcesPresetDocsPath)
		assert.Containsf(t, page, fmt.Sprintf("| `%s` | `%s` |", values.CPU, values.RAM),
			"the %s tier's values are not documented in %s", tier, _UnitResourcesPresetDocsPath)
	}
}
