package modelkernelkey

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"regexp"
	"slices"
	"strings"
)

const signatureHashHexLen = 16

var signatureHash16Pattern = regexp.MustCompile(`^[a-f0-9]{16}$`)

// BuildSpec carries raw inputs used to construct and canonicalize a LocalKernelKey.
// VarAxes is accepted for caller convenience and is always excluded from hashing.
type BuildSpec struct {
	Engine        string
	EngineVersion string
	Manufacturer  string
	Arch          string
	OpFamily      string
	ConstAxes     map[string]int64
	VarAxes       map[string]int64
	InputDtypes   []string
	OutputDtypes  []string
	Constants     map[string]any
}

// LocalKernelKey identifies a compiled single-GPU operator specialization in
// canonical form: <engine>/<engineVersion>/<manufacturer>/<arch>/<opFamily>/<signatureHash16>.
type LocalKernelKey struct {
	Engine          string           `json:"engine"`
	EngineVersion   string           `json:"engineVersion"`
	Manufacturer    string           `json:"manufacturer"`
	Arch            string           `json:"arch"`
	OpFamily        string           `json:"opFamily"`
	ConstAxes       map[string]int64 `json:"constAxes,omitempty"`
	InputDtypes     []string         `json:"inputDtypes,omitempty"`
	OutputDtypes    []string         `json:"outputDtypes,omitempty"`
	Constants       map[string]any   `json:"constants,omitempty"`
	SignatureHash16 string           `json:"signatureHash16"`
}

// canonicalSignaturePayload uses alphabetically ordered fields matching sorted-key
// compact JSON for SHA-256 signature calculation.
type canonicalSignaturePayload struct {
	ConstAxes    map[string]int64 `json:"constAxes"`
	Constants    map[string]any   `json:"constants"`
	InputDtypes  []string         `json:"inputDtypes"`
	OutputDtypes []string         `json:"outputDtypes"`
}

// Build canonicalizes spec, filters dynamic var axes out of ConstAxes, computes
// SignatureHash16, validates the result, and returns a canonical LocalKernelKey.
func Build(spec BuildSpec) (LocalKernelKey, error) {
	engine, err := NormalizeEngine(spec.Engine)
	if err != nil {
		return LocalKernelKey{}, err
	}
	engineVersion, err := NormalizeEngineVersion(spec.EngineVersion)
	if err != nil {
		return LocalKernelKey{}, err
	}
	manufacturer, err := NormalizeManufacturer(spec.Manufacturer)
	if err != nil {
		return LocalKernelKey{}, err
	}
	arch, err := NormalizeArch(manufacturer, spec.Arch)
	if err != nil {
		return LocalKernelKey{}, err
	}
	opFamily := strings.ToLower(strings.TrimSpace(spec.OpFamily))
	if err := ValidateOpFamily(opFamily); err != nil {
		return LocalKernelKey{}, err
	}

	constAxes, err := FilterConstAxes(spec.ConstAxes)
	if err != nil {
		return LocalKernelKey{}, err
	}
	inputDtypes, err := canonicalizeDtypes("inputDtypes", spec.InputDtypes)
	if err != nil {
		return LocalKernelKey{}, err
	}
	outputDtypes, err := canonicalizeDtypes("outputDtypes", spec.OutputDtypes)
	if err != nil {
		return LocalKernelKey{}, err
	}
	constants, err := canonicalizeConstants(spec.Constants)
	if err != nil {
		return LocalKernelKey{}, err
	}

	hash16, err := ComputeSignatureHash(constAxes, inputDtypes, outputDtypes, constants)
	if err != nil {
		return LocalKernelKey{}, err
	}

	key := LocalKernelKey{
		Engine:          engine,
		EngineVersion:   engineVersion,
		Manufacturer:    manufacturer,
		Arch:            arch,
		OpFamily:        opFamily,
		ConstAxes:       constAxes,
		InputDtypes:     inputDtypes,
		OutputDtypes:    outputDtypes,
		Constants:       constants,
		SignatureHash16: hash16,
	}
	if err := key.Validate(); err != nil {
		return LocalKernelKey{}, err
	}
	return key, nil
}

// Parse parses a canonical 6-segment LocalKernelKey string:
// <engine>/<engineVersion>/<manufacturer>/<arch>/<opFamily>/<signatureHash16>.
//
// Parse requires every segment to already be in canonical normalized form so
// Format(Parse(raw)) == raw holds on every accepted input.
func Parse(raw string) (LocalKernelKey, error) {
	if raw == "" {
		return LocalKernelKey{}, fmt.Errorf("local kernel key must not be empty")
	}
	parts := strings.Split(raw, "/")
	if len(parts) != 6 {
		return LocalKernelKey{}, fmt.Errorf(
			"local kernel key %q must have 6 slash-separated segments", raw,
		)
	}

	key := LocalKernelKey{
		Engine:          parts[0],
		EngineVersion:   parts[1],
		Manufacturer:    parts[2],
		Arch:            parts[3],
		OpFamily:        parts[4],
		SignatureHash16: parts[5],
	}
	if err := key.Validate(); err != nil {
		return LocalKernelKey{}, err
	}
	return key, nil
}

// ValidateKey parses and validates a canonical LocalKernelKey string.
func ValidateKey(raw string) error {
	_, err := Parse(raw)
	return err
}

// Format validates key and returns its canonical slash-separated representation.
func Format(key LocalKernelKey) (string, error) {
	return key.Format()
}

// Format validates k and formats it as
// <engine>/<engineVersion>/<manufacturer>/<arch>/<opFamily>/<signatureHash16>.
func (k LocalKernelKey) Format() (string, error) {
	if err := k.Validate(); err != nil {
		return "", err
	}
	return k.formatUnchecked(), nil
}

// String implements fmt.Stringer. It returns the canonical formatted key or empty
// string if the required segments are not populated.
func (k LocalKernelKey) String() string {
	if k.Engine == "" || k.EngineVersion == "" || k.Manufacturer == "" ||
		k.Arch == "" || k.OpFamily == "" || k.SignatureHash16 == "" {
		return ""
	}
	return k.formatUnchecked()
}

func (k LocalKernelKey) formatUnchecked() string {
	return strings.Join([]string{
		k.Engine,
		k.EngineVersion,
		k.Manufacturer,
		k.Arch,
		k.OpFamily,
		k.SignatureHash16,
	}, "/")
}

// Validate verifies that k carries canonical normalized segments, an allowed
// single-GPU OpFamily, valid 16-char lowercase hex SignatureHash16, no dynamic
// var axes inside ConstAxes, and (when signature inputs are present) a matching
// SignatureHash16.
func (k LocalKernelKey) Validate() error {
	normEngine, err := NormalizeEngine(k.Engine)
	if err != nil {
		return err
	}
	if k.Engine != normEngine {
		return fmt.Errorf("engine %q must be canonical lowercase %q", k.Engine, normEngine)
	}

	normVer, err := NormalizeEngineVersion(k.EngineVersion)
	if err != nil {
		return err
	}
	if k.EngineVersion != normVer {
		return fmt.Errorf("engineVersion %q must be normalized %q", k.EngineVersion, normVer)
	}

	normManu, err := NormalizeManufacturer(k.Manufacturer)
	if err != nil {
		return err
	}
	if k.Manufacturer != normManu {
		return fmt.Errorf("manufacturer %q must be canonical lowercase %q", k.Manufacturer, normManu)
	}

	normArch, err := NormalizeArch(k.Manufacturer, k.Arch)
	if err != nil {
		return err
	}
	if k.Arch != normArch {
		return fmt.Errorf("arch %q must be canonical %q", k.Arch, normArch)
	}

	if err := ValidateOpFamily(k.OpFamily); err != nil {
		return err
	}

	if !signatureHash16Pattern.MatchString(k.SignatureHash16) {
		return fmt.Errorf("signatureHash16 %q must be 16 lowercase hex characters", k.SignatureHash16)
	}

	for axis := range k.ConstAxes {
		if strings.TrimSpace(axis) == "" {
			return fmt.Errorf("constAxes contains an empty axis name")
		}
		if IsDynamicVarAxis(axis) {
			return fmt.Errorf("constAxes must not include dynamic var axis %q", axis)
		}
	}

	hasSignatureInputs := len(k.ConstAxes) > 0 ||
		len(k.InputDtypes) > 0 ||
		len(k.OutputDtypes) > 0 ||
		len(k.Constants) > 0
	if hasSignatureInputs {
		expectedHash, err := k.ComputeSignatureHash()
		if err != nil {
			return fmt.Errorf("compute signatureHash16: %w", err)
		}
		if k.SignatureHash16 != expectedHash {
			return fmt.Errorf("signatureHash16 %q does not match canonical payload hash %q",
				k.SignatureHash16, expectedHash)
		}
	}

	return nil
}

// ComputeSignatureHash computes the 16-hex SHA-256 signature hash over k's
// ConstAxes, InputDtypes, OutputDtypes, and Constants.
func (k LocalKernelKey) ComputeSignatureHash() (string, error) {
	return ComputeSignatureHash(k.ConstAxes, k.InputDtypes, k.OutputDtypes, k.Constants)
}

// MatchesTarget checks whether k is compatible with target.
func (k LocalKernelKey) MatchesTarget(target CompatibilityTarget) error {
	return MatchCompatibility(k, target)
}

// ComputeSignatureHash returns the first 16 lowercase hex characters of SHA-256
// over canonical sorted-key compact JSON of {constAxes, inputDtypes, outputDtypes, constants}.
// Dynamic var axes in constAxes are stripped and nil collections normalize to empty.
func ComputeSignatureHash(
	constAxes map[string]int64,
	inputDtypes []string,
	outputDtypes []string,
	constants map[string]any,
) (string, error) {
	filteredAxes, err := FilterConstAxes(constAxes)
	if err != nil {
		return "", err
	}
	inDtypes, err := canonicalizeDtypes("inputDtypes", inputDtypes)
	if err != nil {
		return "", err
	}
	outDtypes, err := canonicalizeDtypes("outputDtypes", outputDtypes)
	if err != nil {
		return "", err
	}
	normConstants, err := canonicalizeConstants(constants)
	if err != nil {
		return "", err
	}

	payload := canonicalSignaturePayload{
		ConstAxes:    filteredAxes,
		Constants:    normConstants,
		InputDtypes:  inDtypes,
		OutputDtypes: outDtypes,
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(payload); err != nil {
		return "", fmt.Errorf("marshal canonical signature payload: %w", err)
	}
	compact := bytes.TrimSuffix(buf.Bytes(), []byte("\n"))

	sum := sha256.Sum256(compact)
	return hex.EncodeToString(sum[:])[:signatureHashHexLen], nil
}

func canonicalizeDtypes(fieldName string, dtypes []string) ([]string, error) {
	out := make([]string, 0, len(dtypes))
	for _, dt := range dtypes {
		norm := strings.ToLower(strings.TrimSpace(dt))
		norm = strings.TrimPrefix(norm, "torch.")
		if norm == "" {
			return nil, fmt.Errorf("%s contains an empty dtype", fieldName)
		}
		out = append(out, norm)
	}
	return out, nil
}

func canonicalizeConstants(constants map[string]any) (map[string]any, error) {
	out := make(map[string]any, len(constants))
	for k, v := range constants {
		trimmed := strings.TrimSpace(k)
		if trimmed == "" {
			return nil, fmt.Errorf("constants contains an empty key")
		}
		out[trimmed] = cloneConstantValue(v)
	}
	return out, nil
}

func cloneConstantValue(v any) any {
	switch tv := v.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(tv))
		for k, val := range tv {
			cloned[k] = cloneConstantValue(val)
		}
		return cloned
	case []any:
		cloned := make([]any, len(tv))
		for i, val := range tv {
			cloned[i] = cloneConstantValue(val)
		}
		return cloned
	case []string:
		return slices.Clone(tv)
	case map[string]string:
		return maps.Clone(tv)
	default:
		return v
	}
}
