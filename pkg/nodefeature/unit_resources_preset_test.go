package nodefeature

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/util/sets"
)

// presetLookupCases is the corpus driven through both the shipped table and an entry-shuffled
// copy of it, so every expectation below doubles as an order-independence assertion.
var presetLookupCases = []struct {
	name         string
	manufacturer string
	product      string
	cpu          string
	ram          string
}{
	// Tiers reachable from the shipped table.
	{"flagship", "nvidia", "NVIDIA H100 80GB HBM3", "12", "192Gi"},
	{"flagship, label-sanitized twin", "nvidia", "NVIDIA-H100-80GB-HBM3", "12", "192Gi"},
	{"flagship, pcie variant", "nvidia", "NVIDIA H100 PCIe", "12", "192Gi"},
	{"flagship, nvl variant", "nvidia", "NVIDIA H100 NVL", "12", "192Gi"},
	{"flagship, bare chip name", "nvidia", "H100", "12", "192Gi"},
	{"entry-level", "nvidia", "Tesla T4", "8", "32Gi"},
	{"entry-level, label-sanitized twin", "nvidia", "Tesla-T4", "8", "32Gi"},
	{"unified memory overrides the vram band", "nvidia", "NVIDIA GB10", "8", "32Gi"},

	// The ordered `by` walk and its whole-token matching.
	{"sku split, 80gb wins", "nvidia", "NVIDIA A100 80GB PCIe", "12", "128Gi"},
	{"sku split, 40gb", "nvidia", "NVIDIA A100-SXM4-40GB", "8", "64Gi"},
	{"sku split, entry catch-all", "nvidia", "NVIDIA A100", "8", "64Gi"},
	{"a whole token, not a substring", "nvidia", "NVIDIA A100 140GB", "8", "64Gi"},

	// Negative matching: a prefix must not swallow a sibling that shares a substring.
	{"a10 is not a100", "nvidia", "NVIDIA A10", "4", "16Gi"},
	{"310p is not 910b", "ascend", "310P3", "4", "16Gi"},

	// Ascend's detector emits the chip name bare as often as prefixed.
	{"bare chip name", "ascend", "910B2", "12", "128Gi"},
	{"prefixed chip name", "ascend", "Ascend910B2", "12", "128Gi"},

	// Everything the table does not positively recognize keeps today's value.
	{"unknown product", "nvidia", "NVIDIA RTX 5070", "4", "16Gi"},
	{"uncovered manufacturer", "amd", "MI300X", "4", "16Gi"},
	{"undetectable manufacturer", "kunlun", "P800", "4", "16Gi"},
	{"empty product", "nvidia", "", "4", "16Gi"},
	{"product is the manufacturer alone", "nvidia", "NVIDIA", "4", "16Gi"},
	{"product is a lone stripped token", "nvidia", "Tesla", "4", "16Gi"},
	{"product normalizes to an empty key", "nvidia", "!!!", "4", "16Gi"},
}

func TestPresetUnitResources(t *testing.T) {
	for _, c := range presetLookupCases {
		t.Run(c.name, func(t *testing.T) {
			cpu, ram := PresetUnitResources(c.manufacturer, c.product)
			assert.Equal(t, c.cpu, cpu, "unit CPU")
			assert.Equal(t, c.ram, ram, "unit RAM")
		})
	}
}

// TestPresetUnitResourcesIgnoresEntryOrder drives the whole corpus against a table whose entries
// are reversed. Only the entry lists are permuted: a `by` list's order is deliberately semantic,
// and TestPresetUnitResourcesByOrderIsSemantic is what pins that half.
func TestPresetUnitResourcesIgnoresEntryOrder(t *testing.T) {
	shuffled := &unitResourcesPresets{
		Tiers:    _UnitResourcesPresetTable.Tiers,
		Strip:    slices.Clone(_UnitResourcesPresetTable.Strip),
		Families: make(map[string][]unitResourcesPresetEntry, len(_UnitResourcesPresetTable.Families)),
	}
	slices.Reverse(shuffled.Strip)
	for manufacturer, entries := range _UnitResourcesPresetTable.Families {
		reversed := slices.Clone(entries)
		slices.Reverse(reversed)
		shuffled.Families[manufacturer] = reversed
	}
	require.NoError(t, shuffled.validate(), "the permuted table must still be valid")

	for _, c := range presetLookupCases {
		t.Run(c.name, func(t *testing.T) {
			cpu, ram := shuffled.lookup(c.manufacturer, c.product)
			assert.Equal(t, c.cpu, cpu, "unit CPU")
			assert.Equal(t, c.ram, ram, "unit RAM")
		})
	}
}

// TestPresetUnitResourcesByOrderIsSemantic pins the one part of the table whose order does
// decide the outcome, so that reordering a `by` list is a visible behavior change rather than a
// silent one. Entry order cannot matter — at most one entry ever matches a key — but two `by`
// tokens of the same entry can both appear in one key, and the first listed wins. No product
// string a detector emits carries two capacity tokens, so the ambiguity is synthetic; the point
// of pinning it is that the declared order is the tie-break, rather than the tie-break being
// undefined.
func TestPresetUnitResourcesByOrderIsSemantic(t *testing.T) {
	const product = "NVIDIA A100 80GB 40GB"

	cases := []struct {
		name string
		by   string
		cpu  string
		ram  string
	}{
		{
			name: "the first listed token wins",
			by: "        - {token: 80gb, family: a100-80gb, tier: large}\n" +
				"        - {token: 40gb, family: a100-40gb, tier: medium}\n",
			cpu: "12",
			ram: "128Gi",
		},
		{
			name: "reversing the list reverses the outcome",
			by: "        - {token: 40gb, family: a100-40gb, tier: medium}\n" +
				"        - {token: 80gb, family: a100-80gb, tier: large}\n",
			cpu: "8",
			ram: "64Gi",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			fixture := presetFixture{
				families: "families:\n  nvidia:\n    - prefix: a100\n      family: a100\n" +
					"      tier: medium\n      by:\n" + c.by,
			}
			table, err := loadUnitResourcesPresets([]byte(fixture.yaml()))
			require.NoError(t, err)

			cpu, ram := table.lookup("nvidia", product)
			assert.Equal(t, c.cpu, cpu, "unit CPU")
			assert.Equal(t, c.ram, ram, "unit RAM")
		})
	}

	t.Run("the shipped table declares the larger capacity first", func(t *testing.T) {
		cpu, ram := PresetUnitResources("nvidia", product)
		assert.Equal(t, "12", cpu, "unit CPU")
		assert.Equal(t, "128Gi", ram, "unit RAM")
	})
}

// TestPresetUnitResourcesMatchesAtMostOneEntry is the property half of the one-product-one-tier
// guarantee: for keys built out of the table's own prefixes and plausible suffixes, no
// manufacturer ever has two entries matching the same key. It counts entries only — the variant
// list inside an entry resolves by declared order, which TestPresetUnitResourcesByOrderIsSemantic
// pins instead.
func TestPresetUnitResourcesMatchesAtMostOneEntry(t *testing.T) {
	suffixes := []string{"", "-x", "-80gb", "0", "-1-2", "-sxm4-40gb", "_ai"}

	for manufacturer, entries := range _UnitResourcesPresetTable.Families {
		for _, e := range entries {
			for _, suffix := range suffixes {
				key := e.Prefix + suffix
				var matched []string
				for _, candidate := range entries {
					if hasTokenPrefix(splitPresetTokens(key), splitPresetTokens(candidate.Prefix)) {
						matched = append(matched, candidate.Prefix)
					}
				}
				assert.LessOrEqualf(t, len(matched), 1,
					"key %q of %s matched %v", key, manufacturer, matched)
			}
		}
	}
}

// TestShippedUnitResourcesPresetTable pins the properties of the shipped table that are policy
// rather than structure, so a data edit that breaks them fails here rather than in the field.
func TestShippedUnitResourcesPresetTable(t *testing.T) {
	t.Run("every tier stays within the flat-CPU ceiling", func(t *testing.T) {
		// Above 48 GiB the ladder deliberately stops growing CPU, so that N single-card Pods
		// still fit a real N-card node when general-resource overcommit is turned off.
		for name, tier := range _UnitResourcesPresetTable.Tiers {
			cpu, err := parsePresetUnitCPU(tier.CPU)
			require.NoErrorf(t, err, "tier %s", name)
			assert.LessOrEqualf(t, cpu, int32(12), "tier %s CPU", name)
		}
	})

	t.Run("the fallback is today's value", func(t *testing.T) {
		cpu, ram := PresetUnitResources("nvidia", "a product no table will ever carry")
		assert.Equal(t, "4", cpu)
		assert.Equal(t, "16Gi", ram)
	})

	t.Run("the accepted manufacturer keys are the detectable ones", func(t *testing.T) {
		// The validator cannot read the registry knowns.go builds, since that init runs after
		// the table loads. This is the guard against the two drifting apart.
		assert.ElementsMatch(t,
			GetKnownAcceleratableManufacturers(),
			sets.List(_UnitResourcesPresetManufacturers))
	})
}

// presetFixture composes a preset table out of its three blocks, so a case states only the block
// it is exercising.
type presetFixture struct {
	tiers    string
	strip    string
	families string
}

const (
	validPresetTiers = `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "12", ram: 192Gi}
`
	validPresetStrip = `strip: [tesla]
`
	validPresetFamilies = `families:
  nvidia:
    - {prefix: h100, family: h100, tier: xlarge}
`
)

func (f presetFixture) yaml() string {
	tiers, strip, families := f.tiers, f.strip, f.families
	if tiers == "" {
		tiers = validPresetTiers
	}
	if strip == "" {
		strip = validPresetStrip
	}
	if families == "" {
		families = validPresetFamilies
	}
	return tiers + strip + families
}

// TestLoadUnitResourcesPresetsRejects drives one purpose-built bad table per validation rule.
func TestLoadUnitResourcesPresetsRejects(t *testing.T) {
	cases := []struct {
		name    string
		rule    string
		fixture presetFixture
		errMsg  string
	}{
		{
			name: "an undefined tier name",
			rule: "V1",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: h100, family: h100, tier: huge}\n",
			},
			errMsg: `"huge" is not a defined tier`,
		},
		{
			name: "an extra tier",
			rule: "V1",
			fixture: presetFixture{
				tiers: validPresetTiers + `  huge: {cpu: "16", ram: 256Gi}` + "\n",
			},
			errMsg: "must define exactly",
		},
		{
			name: "a missing tier",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
`,
			},
			errMsg: "must define exactly",
		},
		{
			name: "a mutated fallback, which would break the zero-regression promise",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "8", ram: 32Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "12", ram: 192Gi}
`,
			},
			errMsg: "must stay 4 CPU / 16Gi",
		},
		{
			// The count check passes, so this is caught by name rather than by arity, and the
			// error has to name the missing key instead of blaming the fallback's value.
			name: "the right number of tiers, but one of them renamed",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  baseline: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "12", ram: 192Gi}
`,
			},
			errMsg: `"fallback" is missing`,
		},
		{
			name: "a small tier that is not strictly above the fallback",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "4", ram: 16Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "12", ram: 192Gi}
`,
			},
			errMsg: "must be strictly larger",
		},
		{
			name: "a non-monotonic ladder",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "8", ram: 32Gi}
  xlarge: {cpu: "12", ram: 192Gi}
`,
			},
			errMsg: "must not be smaller",
		},
		{
			name: "a CPU that parses as int64 but overflows int32, which the webhook would reject",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "3000000000", ram: 192Gi}
`,
			},
			errMsg: "must be a positive integer with no unit suffix",
		},
		{
			name: "a RAM suffix in the wrong case, which the webhook would reject",
			rule: "V1",
			fixture: presetFixture{
				tiers: `tiers:
  fallback: {cpu: "4", ram: 16Gi}
  small: {cpu: "8", ram: 32Gi}
  medium: {cpu: "8", ram: 64Gi}
  large: {cpu: "12", ram: 128Gi}
  xlarge: {cpu: "12", ram: 192gi}
`,
			},
			errMsg: `must carry a case-sensitive "Gi" suffix`,
		},
		{
			name: "a misspelled manufacturer",
			rule: "V2",
			fixture: presetFixture{
				families: "families:\n  nvdiia:\n    - {prefix: h100, family: h100, tier: xlarge}\n",
			},
			errMsg: "is not a manufacturer the operator can detect",
		},
		{
			name: "a repeated prefix",
			rule: "V3",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - {prefix: h100, family: h100, tier: xlarge}
    - {prefix: h100, family: h100-again, tier: large}
`,
			},
			errMsg: `prefix "h100" is used twice`,
		},
		{
			name: "a prefix that token-prefixes a sibling",
			rule: "V3",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - {prefix: a100, family: a100, tier: medium}
    - {prefix: a100-sxm, family: a100-sxm, tier: large}
`,
			},
			errMsg: `is a token-prefix of "a100-sxm"`,
		},
		{
			name: "a family name claimed twice",
			rule: "V4",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - {prefix: h100, family: hopper, tier: xlarge}
    - {prefix: h200, family: hopper, tier: xlarge}
`,
			},
			errMsg: "is already claimed elsewhere",
		},
		{
			name: "a family name a variant re-claims",
			rule: "V4",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - prefix: a100
      family: a100
      tier: medium
      by:
        - {token: 80gb, family: a100, tier: large}
`,
			},
			errMsg: "is already claimed elsewhere",
		},
		{
			name: "an empty variant token",
			rule: "V5",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - prefix: a100
      family: a100
      tier: medium
      by:
        - {token: "", family: a100-blank, tier: large}
`,
			},
			errMsg: "must not be empty",
		},
		{
			name: "a repeated variant token",
			rule: "V5",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - prefix: a100
      family: a100
      tier: medium
      by:
        - {token: 80gb, family: a100-80gb, tier: large}
        - {token: 80gb, family: a100-80gb-again, tier: xlarge}
`,
			},
			errMsg: "is listed twice",
		},
		{
			name: "a variant token equal to a token of its own prefix",
			rule: "V5",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - prefix: a100-sxm
      family: a100-sxm
      tier: medium
      by:
        - {token: sxm, family: a100-sxm-variant, tier: large}
`,
			},
			errMsg: "makes the entry's own tier unreachable",
		},
		{
			name: "an empty prefix, which would swallow the whole manufacturer",
			rule: "V6",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: \"\", family: anything, tier: xlarge}\n",
			},
			errMsg: "must carry at least one token",
		},
		{
			name: "a prefix carrying only separators",
			rule: "V6",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: \"-\", family: anything, tier: xlarge}\n",
			},
			errMsg: "must carry at least one token",
		},
		{
			name: "an uppercase prefix",
			rule: "V6",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: H100, family: h100, tier: xlarge}\n",
			},
			errMsg: "must be lowercase",
		},
		{
			name: "a variant token carrying a separator",
			rule: "V6",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - prefix: a100
      family: a100
      tier: medium
      by:
        - {token: 80-gb, family: a100-80gb, tier: large}
`,
			},
			errMsg: "must be a single lowercase",
		},
		{
			name:    "an empty strip sequence",
			rule:    "V7",
			fixture: presetFixture{strip: "strip: [\"\"]\n"},
			errMsg:  "must carry at least one token",
		},
		{
			name:    "a repeated strip sequence",
			rule:    "V7",
			fixture: presetFixture{strip: "strip: [tesla, tesla]\n"},
			errMsg:  "is listed twice",
		},
		{
			name:    "a strip sequence that token-prefixes another",
			rule:    "V7",
			fixture: presetFixture{strip: "strip: [radeon, radeon-instinct]\n"},
			errMsg:  "depend on the order the two are listed in",
		},
		{
			name: "a prefix shadowed by the strip",
			rule: "V8",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: tesla-t4, family: t4, tier: small}\n",
			},
			errMsg: "starts with the stripped sequence",
		},
		{
			name: "a prefix shadowed by its own manufacturer name",
			rule: "V8",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: nvidia-h100, family: h100, tier: xlarge}\n",
			},
			errMsg: "starts with its own manufacturer name",
		},
		{
			name: "an unknown field",
			rule: "strict decode",
			fixture: presetFixture{
				families: "families:\n  nvidia:\n    - {prefix: h100, family: h100, tier: xlarge, require: [x]}\n",
			},
			errMsg: "decoding unit-resources presets",
		},
		{
			name: "a duplicate mapping key",
			rule: "strict decode",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - {prefix: h100, family: h100, tier: xlarge}
  nvidia:
    - {prefix: a100, family: a100, tier: medium}
`,
			},
			errMsg: "decoding unit-resources presets",
		},
	}

	for _, c := range cases {
		t.Run(c.rule+": "+c.name, func(t *testing.T) {
			_, err := loadUnitResourcesPresets([]byte(c.fixture.yaml()))
			require.Error(t, err)
			assert.ErrorContains(t, err, c.errMsg)
		})
	}
}

// TestLoadUnitResourcesPresetsAccepts pins the tables the rules must NOT reject. A validation
// implemented on plain string prefixes instead of tokens would fail every case here, silently
// cutting a manufacturer's coverage down to whichever sibling was written first.
func TestLoadUnitResourcesPresetsAccepts(t *testing.T) {
	cases := []struct {
		name    string
		fixture presetFixture
	}{
		{
			name: "siblings whose names share a string prefix",
			fixture: presetFixture{
				families: `families:
  nvidia:
    - {prefix: h20, family: h20, tier: medium}
    - {prefix: h200, family: h200, tier: xlarge}
    - {prefix: a10, family: a10, tier: small}
    - {prefix: a100, family: a100, tier: medium}
    - {prefix: l4, family: l4, tier: medium}
    - {prefix: l40, family: l40, tier: medium}
    - {prefix: l40s, family: l40s, tier: large}
`,
			},
		},
		{
			name: "one prefix per manufacturer across several manufacturers",
			fixture: presetFixture{
				strip: "strip: [radeon, instinct, meta-x, moore-threads]\n",
				families: `families:
  nvidia:
    - {prefix: h100, family: h100, tier: xlarge}
  amd:
    - {prefix: mi300x, family: mi300x, tier: xlarge}
  metax:
    - {prefix: c500, family: c500, tier: large}
`,
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := loadUnitResourcesPresets([]byte(c.fixture.yaml()))
			require.NoError(t, err)
		})
	}
}

// TestPresetUnitResourcesNormalization pins the normalization behaviors the table's coverage
// depends on. Each drives a purpose-built table, because the shipped one covers only the seed
// families.
func TestPresetUnitResourcesNormalization(t *testing.T) {
	cases := []struct {
		name         string
		fixture      presetFixture
		manufacturer string
		product      string
		cpu          string
		ram          string
	}{
		{
			name: "two marketing sequences are stripped in successive passes",
			fixture: presetFixture{
				strip:    "strip: [radeon, instinct]\n",
				families: "families:\n  amd:\n    - {prefix: mi300x, family: mi300x, tier: xlarge}\n",
			},
			manufacturer: "amd",
			product:      "AMD Radeon Instinct MI300X OAM",
			cpu:          "12",
			ram:          "192Gi",
		},
		{
			name: "a multi-word brand survives the manufacturer trim and needs a strip sequence",
			fixture: presetFixture{
				strip:    "strip: [meta-x]\n",
				families: "families:\n  metax:\n    - {prefix: c500, family: c500, tier: large}\n",
			},
			manufacturer: "metax",
			product:      "Meta X C500",
			cpu:          "12",
			ram:          "128Gi",
		},
		{
			name: "an underscore separates tokens exactly like a hyphen",
			fixture: presetFixture{
				families: "families:\n  hygon:\n    - {prefix: k100, family: k100, tier: large}\n",
			},
			manufacturer: "hygon",
			product:      "K100_AI",
			cpu:          "12",
			ram:          "128Gi",
		},
		{
			name: "the hyphenated twin of the same product resolves identically",
			fixture: presetFixture{
				families: "families:\n  hygon:\n    - {prefix: k100, family: k100, tier: large}\n",
			},
			manufacturer: "hygon",
			product:      "K100-AI",
			cpu:          "12",
			ram:          "128Gi",
		},
		{
			name: "the manufacturer trim is not token-boundary aware",
			fixture: presetFixture{
				// "AMD64 Accelerator" normalizes to "64-accelerator": the trim fires the instant
				// the buffer spells the manufacturer, mid-token. The table must not assume
				// otherwise.
				families: "families:\n  amd:\n    - {prefix: \"64\", family: amd64, tier: small}\n",
			},
			manufacturer: "amd",
			product:      "AMD64 Accelerator",
			cpu:          "8",
			ram:          "32Gi",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			table, err := loadUnitResourcesPresets([]byte(c.fixture.yaml()))
			require.NoError(t, err)

			cpu, ram := table.lookup(c.manufacturer, c.product)
			assert.Equal(t, c.cpu, cpu, "unit CPU")
			assert.Equal(t, c.ram, ram, "unit RAM")
		})
	}
}
