package nodefeature

import (
	"errors"
	"fmt"
	"maps"
	"slices"
	"strings"

	"k8s.io/apimachinery/pkg/util/sets"
	"sigs.k8s.io/yaml"

	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/utils/strconvx"

	_ "embed"
)

//go:embed unit_resources_preset.yaml
var _UnitResourcesPresetYAML []byte

// _UnitResourcesPresetTable is the parsed and validated preset table. A malformed table is a
// programming error rather than a runtime condition, so loading it panics instead of degrading
// silently. The load happens at package initialisation, not at build time, which means the
// validation unit tests are what actually catch a bad edit before it ships.
var _UnitResourcesPresetTable = mustLoadUnitResourcesPresets(_UnitResourcesPresetYAML)

// _UnitResourcesPresetTierNames is the tier ladder, ordered smallest first. The order is the
// monotonicity contract: a positively identified product must never be sized at or below an
// unidentified one.
var _UnitResourcesPresetTierNames = []string{"fallback", "small", "medium", "large", "xlarge"}

// _UnitResourcesPresetManufacturers is the set of manufacturer keys the table may use. It is
// spelled out from the constants instead of read from the registry knowns.go builds, because
// package-level variables are initialized before any init function runs, so that registry is
// still empty when the table loads. A unit test asserts the two stay in agreement.
var _UnitResourcesPresetManufacturers = sets.New(
	ManufacturerAMD,
	ManufacturerAscend,
	ManufacturerCambricon,
	ManufacturerHygon,
	ManufacturerIluvatar,
	ManufacturerMetaX,
	ManufacturerMThreads,
	ManufacturerNVIDIA,
	ManufacturerTHead,
)

const (
	// _UnitResourcesPresetFallbackTierName is the tier an unmatched product resolves to.
	_UnitResourcesPresetFallbackTierName = "fallback"

	// _UnitResourcesPresetRAMSuffix is the only RAM unit the InstanceType admission webhook
	// accepts, case-sensitively.
	_UnitResourcesPresetRAMSuffix = "Gi"

	// _UnitResourcesPresetTokenSeparators are the characters device.NormalizeName preserves
	// between the tokens of a product slug.
	_UnitResourcesPresetTokenSeparators = "-_."
)

// _UnitResourcesPresetFallbackTier pins the value an unmatched product is stamped with: the
// value the operator stamped before presets existed. Pinning it as data rather than convention
// is what keeps a table edit from turning an unrecognized product into a behavior change.
var _UnitResourcesPresetFallbackTier = unitResourcesPresetTier{CPU: "4", RAM: "16Gi"}

// unitResourcesPresetTier is one rung of the ladder: the per-whole-card CPU and RAM a matched
// product is stamped with. Both are held as the strings the InstanceType carries, so the values
// are validated by the very rules the admission webhook later applies to them.
type unitResourcesPresetTier struct {
	CPU string `json:"cpu"`
	RAM string `json:"ram"`
}

// unitResourcesPresetEntry matches one accelerator family of one manufacturer. Prefix is a token
// sequence, unique within its manufacturer and never a token-prefix of a sibling, so a product
// key matches at most one entry. By refines the match for variants of the same silicon and is
// the only ordered part of the table.
type unitResourcesPresetEntry struct {
	Prefix string                       `json:"prefix"`
	Family string                       `json:"family"`
	Tier   string                       `json:"tier"`
	By     []unitResourcesPresetVariant `json:"by,omitempty"`
}

// unitResourcesPresetVariant refines its entry when Token appears as a whole token anywhere in
// the product key, typically a capacity discriminator such as "80gb".
type unitResourcesPresetVariant struct {
	Token  string `json:"token"`
	Family string `json:"family"`
	Tier   string `json:"tier"`
}

// unitResourcesPresets is the whole table: the tier ladder, the leading marketing token
// sequences to strip, and the prefix entries per manufacturer.
type unitResourcesPresets struct {
	Tiers    map[string]unitResourcesPresetTier    `json:"tiers"`
	Strip    []string                              `json:"strip"`
	Families map[string][]unitResourcesPresetEntry `json:"families"`
}

// PresetUnitResources returns the per-whole-card CPU and RAM to stamp on a derived acceleratable
// InstanceType for the given accelerator manufacturer and product. The product is normalized to
// a token key and matched against at most one prefix entry, whose ordered variant list selects
// the tier. A manufacturer the table does not cover, an unmatched product, and an empty or
// unrepresentable input all yield the fallback tier, which is what the operator stamped before
// presets existed. The returned values always satisfy the InstanceType admission webhook's
// unit-spec rules, and the lookup is pure: same inputs, same output, no I/O.
//
// Three normalization constraints are load-bearing and must not be "simplified" away.
// device.NormalizeName is called with maxLength 0, because a bounded budget truncates without
// preserving token boundaries and would silently drop a trailing capacity discriminator. Its
// manufacturer trim fires only when the buffer length equals the manufacturer length at that
// exact instant, so a multi-word brand ("Meta X", "Moore Threads") is never trimmed and has to
// be covered by a strip sequence instead. And that trim is not token-boundary aware, so a
// manufacturer name that merely leads the product is removed regardless ("AMD64 Accelerator"
// under "amd" becomes "64-accelerator").
func PresetUnitResources(manufacturer, product string) (cpu, ram string) {
	return _UnitResourcesPresetTable.lookup(manufacturer, product)
}

// lookup resolves one (manufacturer, product) pair. The product is read only when the
// manufacturer has entries, so a manufacturer the operator cannot detect costs no normalization
// at all — load-time validation keeps the covered set and the detectable set identical.
func (t *unitResourcesPresets) lookup(manufacturer, product string) (cpu, ram string) {
	fallback := t.Tiers[_UnitResourcesPresetFallbackTierName]

	entries := t.Families[manufacturer]
	if len(entries) == 0 {
		return fallback.CPU, fallback.RAM
	}

	key := device.NormalizeName(product, manufacturer, 0, false)
	tokens := t.stripLeadingMarketing(splitPresetTokens(key))
	if len(tokens) == 0 {
		return fallback.CPU, fallback.RAM
	}

	for _, e := range entries {
		if !hasTokenPrefix(tokens, splitPresetTokens(e.Prefix)) {
			continue
		}
		for _, v := range e.By {
			if slices.Contains(tokens, v.Token) {
				tier := t.Tiers[v.Tier]
				return tier.CPU, tier.RAM
			}
		}
		tier := t.Tiers[e.Tier]
		return tier.CPU, tier.RAM
	}

	return fallback.CPU, fallback.RAM
}

// stripLeadingMarketing removes the leading marketing token sequences, repeatedly until none
// matches. One pass is not enough: "AMD Radeon Instinct MI300X OAM" needs both "radeon" and
// "instinct" gone before an "mi300x" prefix can match. Strip sequences are validated non-empty,
// so the loop always terminates.
func (t *unitResourcesPresets) stripLeadingMarketing(tokens []string) []string {
	for stripped := true; stripped; {
		stripped = false
		for _, s := range t.Strip {
			seq := splitPresetTokens(s)
			if !hasTokenPrefix(tokens, seq) {
				continue
			}
			tokens = tokens[len(seq):]
			stripped = true
			break
		}
	}
	return tokens
}

// splitPresetTokens splits a normalized slug into its tokens. device.NormalizeName preserves
// "-", "_" and "." between tokens, so Hygon's "K100_AI" and "K100-AI" tokenize identically.
func splitPresetTokens(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return strings.ContainsRune(_UnitResourcesPresetTokenSeparators, r)
	})
}

// hasTokenPrefix reports whether prefix is a token-prefix of tokens. Matching whole tokens is
// what keeps "a10" from matching "a100" during a lookup, and what keeps the table from rejecting
// the legitimate sibling pairs "h20"/"h200" and "l4"/"l40"/"l40s" during validation.
func hasTokenPrefix(tokens, prefix []string) bool {
	return len(prefix) <= len(tokens) && slices.Equal(tokens[:len(prefix)], prefix)
}

// mustLoadUnitResourcesPresets loads the embedded table, panicking on a malformed one.
func mustLoadUnitResourcesPresets(data []byte) *unitResourcesPresets {
	t, err := loadUnitResourcesPresets(data)
	if err != nil {
		panic("loading unit-resources preset table: " + err.Error())
	}
	return t
}

// loadUnitResourcesPresets strictly decodes and validates a preset table. Strict decoding is
// what makes a misspelled field a hard error instead of a silent widening.
func loadUnitResourcesPresets(data []byte) (*unitResourcesPresets, error) {
	t := new(unitResourcesPresets)
	if err := yaml.UnmarshalStrict(data, t); err != nil {
		return nil, fmt.Errorf("decoding unit-resources presets: %w", err)
	}
	if err := t.validate(); err != nil {
		return nil, err
	}
	return t, nil
}

// validate rejects a table that could resolve a product to more than one tier, could depend on
// entry order, or could stamp a value the InstanceType admission webhook would reject.
func (t *unitResourcesPresets) validate() error {
	if err := t.validateTiers(); err != nil {
		return err
	}
	if err := t.validateStrip(); err != nil {
		return err
	}
	return t.validateFamilies()
}

// validateTiers enforces the exact tier set, the pinned fallback, the monotonic ladder, and
// webhook-acceptable values.
func (t *unitResourcesPresets) validateTiers() error {
	if len(t.Tiers) != len(_UnitResourcesPresetTierNames) {
		return fmt.Errorf("tiers: must define exactly %v, got %d", _UnitResourcesPresetTierNames, len(t.Tiers))
	}
	var prevCPU, prevRAM int32
	for i, name := range _UnitResourcesPresetTierNames {
		tier, ok := t.Tiers[name]
		if !ok {
			return fmt.Errorf("tiers: %q is missing", name)
		}
		if name == _UnitResourcesPresetFallbackTierName && tier != _UnitResourcesPresetFallbackTier {
			return fmt.Errorf("tiers[%s]: must stay %s CPU / %s, the value stamped before presets existed",
				name, _UnitResourcesPresetFallbackTier.CPU, _UnitResourcesPresetFallbackTier.RAM)
		}
		cpu, err := parsePresetUnitCPU(tier.CPU)
		if err != nil {
			return fmt.Errorf("tiers[%s].cpu: %w", name, err)
		}
		ram, err := parsePresetUnitRAM(tier.RAM)
		if err != nil {
			return fmt.Errorf("tiers[%s].ram: %w", name, err)
		}
		switch {
		case i == 1 && (cpu <= prevCPU || ram <= prevRAM):
			return fmt.Errorf("tiers[%s]: must be strictly larger than %q on both axes, so an identified "+
				"product is never sized at or below an unidentified one", name, _UnitResourcesPresetTierNames[i-1])
		case i > 1 && (cpu < prevCPU || ram < prevRAM):
			return fmt.Errorf("tiers[%s]: must not be smaller than %q on either axis",
				name, _UnitResourcesPresetTierNames[i-1])
		}
		prevCPU, prevRAM = cpu, ram
	}
	return nil
}

// validateStrip enforces well-formed, non-redundant marketing sequences. Two sequences where one
// token-prefixes the other make stripping depend on their order in the list — listed shorter-first
// the longer one never fires, listed longer-first both do — and a result that moves with
// declaration order is exactly what this table forbids.
func (t *unitResourcesPresets) validateStrip() error {
	seen := sets.New[string]()
	for _, s := range t.Strip {
		if err := validatePresetSequence(s); err != nil {
			return fmt.Errorf("strip[%q]: %w", s, err)
		}
		if seen.Has(s) {
			return fmt.Errorf("strip[%q]: is listed twice", s)
		}
		seen.Insert(s)
	}
	for i, outer := range t.Strip {
		for j, inner := range t.Strip {
			if i == j {
				continue
			}
			if hasTokenPrefix(splitPresetTokens(inner), splitPresetTokens(outer)) {
				return fmt.Errorf("strip[%q]: is a token-prefix of strip[%q], which makes stripping "+
					"depend on the order the two are listed in", outer, inner)
			}
		}
	}
	return nil
}

// validateFamilies enforces the per-manufacturer entry rules: a detectable manufacturer,
// prefixes that cannot both match one key, globally unique family names, well-formed variant
// tokens, and no entry made dead by the strip or the manufacturer trim.
func (t *unitResourcesPresets) validateFamilies() error {
	families := sets.New[string]()

	for _, manufacturer := range slices.Sorted(maps.Keys(t.Families)) {
		if !_UnitResourcesPresetManufacturers.Has(manufacturer) {
			return fmt.Errorf("families[%s]: is not a manufacturer the operator can detect", manufacturer)
		}
		entries := t.Families[manufacturer]
		for _, e := range entries {
			if err := t.validateFamilyEntry(manufacturer, e, families); err != nil {
				return err
			}
		}
		if err := validatePresetPrefixesDisjoint(manufacturer, entries); err != nil {
			return err
		}
	}
	return nil
}

// validateFamilyEntry checks one entry and records the family names it claims.
func (t *unitResourcesPresets) validateFamilyEntry(
	manufacturer string,
	e unitResourcesPresetEntry,
	families sets.Set[string],
) error {
	at := fmt.Sprintf("families[%s][%s]", manufacturer, e.Prefix)

	if err := validatePresetSequence(e.Prefix); err != nil {
		return fmt.Errorf("%s.prefix: %w", at, err)
	}
	prefix := splitPresetTokens(e.Prefix)
	if hasTokenPrefix(prefix, splitPresetTokens(manufacturer)) {
		return fmt.Errorf("%s.prefix: starts with its own manufacturer name, which normalization already "+
			"removes, so the entry can never match", at)
	}
	for _, s := range t.Strip {
		if hasTokenPrefix(prefix, splitPresetTokens(s)) {
			return fmt.Errorf("%s.prefix: starts with the stripped sequence %q, so the entry can never match", at, s)
		}
	}

	if err := t.claimPresetFamily(at, e.Family, e.Tier, families); err != nil {
		return err
	}

	tokens := sets.New[string]()
	for _, v := range e.By {
		if err := validatePresetToken(v.Token); err != nil {
			return fmt.Errorf("%s.by[%q].token: %w", at, v.Token, err)
		}
		if tokens.Has(v.Token) {
			return fmt.Errorf("%s.by[%q].token: is listed twice", at, v.Token)
		}
		if slices.Contains(prefix, v.Token) {
			return fmt.Errorf("%s.by[%q].token: is a token of the entry's own prefix, so it matches "+
				"unconditionally and makes the entry's own tier unreachable", at, v.Token)
		}
		tokens.Insert(v.Token)
		if err := t.claimPresetFamily(at+".by["+v.Token+"]", v.Family, v.Tier, families); err != nil {
			return err
		}
	}
	return nil
}

// claimPresetFamily records a family name as taken and checks the tier it names exists.
func (t *unitResourcesPresets) claimPresetFamily(at, family, tier string, families sets.Set[string]) error {
	if family == "" {
		return fmt.Errorf("%s.family: must not be empty", at)
	}
	if families.Has(family) {
		return fmt.Errorf("%s.family: %q is already claimed elsewhere in the table", at, family)
	}
	families.Insert(family)
	if _, ok := t.Tiers[tier]; !ok {
		return fmt.Errorf("%s.tier: %q is not a defined tier", at, tier)
	}
	return nil
}

// validatePresetPrefixesDisjoint rejects a manufacturer whose entries could both match one key.
// This is the structural half of the one-product-one-tier guarantee: it makes "at most one entry
// matches" a property of the table rather than of the resolver.
func validatePresetPrefixesDisjoint(manufacturer string, entries []unitResourcesPresetEntry) error {
	for i, outer := range entries {
		for j, inner := range entries {
			if i == j {
				continue
			}
			if !hasTokenPrefix(splitPresetTokens(inner.Prefix), splitPresetTokens(outer.Prefix)) {
				continue
			}
			if outer.Prefix == inner.Prefix {
				return fmt.Errorf("families[%s]: prefix %q is used twice", manufacturer, outer.Prefix)
			}
			return fmt.Errorf("families[%s]: prefix %q is a token-prefix of %q, so a key could match both",
				manufacturer, outer.Prefix, inner.Prefix)
		}
	}
	return nil
}

// validatePresetSequence checks a token sequence — a prefix or a strip entry. It must carry at
// least one token, since an empty sequence is a token-prefix of every key.
func validatePresetSequence(s string) error {
	for _, r := range s {
		if isPresetTokenRune(r) || strings.ContainsRune(_UnitResourcesPresetTokenSeparators, r) {
			continue
		}
		return fmt.Errorf("must be lowercase [a-z0-9] plus the token separators %q",
			_UnitResourcesPresetTokenSeparators)
	}
	if len(splitPresetTokens(s)) == 0 {
		return errors.New("must carry at least one token")
	}
	return nil
}

// validatePresetToken checks a single whole-token matcher, which carries no separator.
func validatePresetToken(s string) error {
	if s == "" {
		return errors.New("must not be empty")
	}
	for _, r := range s {
		if !isPresetTokenRune(r) {
			return errors.New("must be a single lowercase [a-z0-9] token")
		}
	}
	return nil
}

func isPresetTokenRune(r rune) bool {
	return (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
}

// parsePresetUnitCPU mirrors the unit-CPU rule validateInstanceTypeUnitSpec applies to an
// acceleratable InstanceType (pkg/worker/webhooks/worker/instance_type.go): a unitless positive
// integer within int32. The rule is restated here rather than shared because the webhook package
// imports this one; reusing the same primitive keeps the two domains identical, so no table can
// load a value the webhook would then reject and leave the pool with no InstanceType at all.
func parsePresetUnitCPU(v string) (int32, error) {
	n, err := strconvx.ParseInt[int32](v, 10, 32)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("%q must be a positive integer with no unit suffix", v)
	}
	return n, nil
}

// parsePresetUnitRAM mirrors the same webhook rule for RAM: a positive integer carrying a
// case-sensitive "Gi" suffix.
func parsePresetUnitRAM(v string) (int32, error) {
	b, ok := strings.CutSuffix(v, _UnitResourcesPresetRAMSuffix)
	if !ok {
		return 0, fmt.Errorf("%q must carry a case-sensitive %q suffix", v, _UnitResourcesPresetRAMSuffix)
	}
	return parsePresetUnitCPU(b)
}
