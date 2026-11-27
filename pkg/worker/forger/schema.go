package forger

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"strings"
	"unicode/utf8"

	"gpustack.ai/gpustack/pkg/worker/kernelkey"
)

const (
	// ForgeRequestSchemaVersion identifies the forge-request.json input schema.
	ForgeRequestSchemaVersion = "gpustack-forge-request/v1"
	// FlashInferTraceSchemaVersion identifies the distilled FlashInfer Trace schema version.
	FlashInferTraceSchemaVersion = "flashinfer-trace/v1"
	// DispatchManifestSchemaVersion identifies the dispatch-manifest.json schema version.
	DispatchManifestSchemaVersion = "gpustack-forge-dispatch/v1"
	// VerificationReportSchemaVersion identifies the verification-report.json schema version.
	VerificationReportSchemaVersion = "gpustack-forge-verification/v1"

	// ForgeRequestFileName is the input request file mounted inside ForgeSandbox.
	ForgeRequestFileName = "forge-request.json"
	// DispatchManifestFileName is the output kernel index file at the root of forge-output.
	DispatchManifestFileName = "dispatch-manifest.json"
	// VerificationReportFileName is the verifier verdict file written to forge-verified.
	VerificationReportFileName = "verification-report.json"

	// StatusPassed marks a passing FlashInfer Trace evaluation or verification report.
	StatusPassed = "PASSED"
	// StatusFailed marks a failed FlashInfer Trace evaluation or verification report.
	StatusFailed = "FAILED"

	// DefaultMinSpeedupRatio is the default minimum speedup required by forge-verifier.
	DefaultMinSpeedupRatio = 1.05
	// MinAllowedSpeedupRatio is the lowest speedup ratio accepted by validation.
	MinAllowedSpeedupRatio = 1.01
	// DefaultAtol is the default maximum absolute numerical error tolerance.
	DefaultAtol = 1e-2
	// DefaultRtol is the default maximum relative numerical error tolerance.
	DefaultRtol = 1e-2

	// MaxRelativePathLength caps any relative file path inside forge output trees.
	MaxRelativePathLength = 256
	// MaxRelativePathDepth caps directory depth inside forge output trees.
	MaxRelativePathDepth = 16
)

var sha256DigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

// VerificationThresholds bounds numerical correctness and latency speedup checks.
type VerificationThresholds struct {
	MinSpeedupRatio float64 `json:"minSpeedupRatio"`
	Atol            float64 `json:"atol"`
	Rtol            float64 `json:"rtol"`
}

// DefaultVerificationThresholds returns the default numerical and speedup thresholds.
func DefaultVerificationThresholds() VerificationThresholds {
	return VerificationThresholds{
		MinSpeedupRatio: DefaultMinSpeedupRatio,
		Atol:            DefaultAtol,
		Rtol:            DefaultRtol,
	}
}

// ForgeRequest matches forge-request.json mounted into /var/run/gpustack/forge/input/.
type ForgeRequest struct {
	SchemaVersion           string             `json:"schemaVersion,omitempty"`
	Engine                  string             `json:"engine"`
	EngineVersion           string             `json:"engineVersion"`
	Manufacturer            string             `json:"manufacturer"`
	Architecture            string             `json:"architecture"`
	TargetKernelKeys        []string           `json:"targetKernelKeys"`
	BaselineLatenciesUs     map[string]float64 `json:"baselineLatenciesUs,omitempty"`
	PerKernelTimeoutSeconds int32              `json:"perKernelTimeoutSeconds,omitempty"`
	MinSpeedupRatio         float64            `json:"minSpeedupRatio"`
	Atol                    float64            `json:"atol"`
	Rtol                    float64            `json:"rtol"`
}

// AxisSpec describes one FlashInfer Trace Definition axis ("const" or "var").
type AxisSpec struct {
	Type  string `json:"type"`
	Value *int64 `json:"value,omitempty"`
}

// TensorSpec describes one input or output tensor in a FlashInfer Trace Definition.
type TensorSpec struct {
	Shape []string `json:"shape"`
	Dtype string   `json:"dtype"`
}

// Definition matches the FlashInfer Trace Schema Definition object.
type Definition struct {
	Name           string                `json:"name"`
	OpType         string                `json:"op_type"`
	LocalKernelKey string                `json:"local_kernel_key,omitempty"`
	Axes           map[string]AxisSpec   `json:"axes"`
	Inputs         map[string]TensorSpec `json:"inputs"`
	Outputs        map[string]TensorSpec `json:"outputs"`
	Reference      string                `json:"reference"`
	Description    string                `json:"description,omitempty"`
	Tags           []string              `json:"tags,omitempty"`
}

// WorkloadInput describes one synthetic input specification in a FlashInfer Trace Workload.
type WorkloadInput struct {
	Type  string `json:"type"`
	Value any    `json:"value,omitempty"`
}

// Workload matches the FlashInfer Trace Schema Workload object.
type Workload struct {
	UUID               string                   `json:"uuid"`
	Definition         string                   `json:"definition,omitempty"`
	LocalKernelKey     string                   `json:"local_kernel_key,omitempty"`
	Axes               map[string]int64         `json:"axes"`
	Inputs             map[string]WorkloadInput `json:"inputs"`
	ObservedDurationUs float64                  `json:"observed_duration_us,omitempty"`
	CallCount          int64                    `json:"call_count,omitempty"`
}

// SolutionSource names one source file belonging to a FlashInfer Trace Solution.
type SolutionSource struct {
	Path    string `json:"path"`
	Content string `json:"content,omitempty"`
}

// SolutionSpec describes implementation language, hardware targets, and entrypoint of a Solution.
type SolutionSpec struct {
	Language                string   `json:"language"`
	TargetHardware          []string `json:"target_hardware"`
	EntryPoint              string   `json:"entry_point"`
	Dependencies            []string `json:"dependencies,omitempty"`
	DestinationPassingStyle bool     `json:"destination_passing_style,omitempty"`
}

// Solution matches the FlashInfer Trace Schema Solution object.
type Solution struct {
	Name           string           `json:"name"`
	Definition     string           `json:"definition"`
	LocalKernelKey string           `json:"local_kernel_key,omitempty"`
	Author         string           `json:"author"`
	Spec           SolutionSpec     `json:"spec"`
	Sources        []SolutionSource `json:"sources"`
	Description    string           `json:"description,omitempty"`
}

// TraceCorrectness matches upstream FlashInfer Trace evaluation.correctness fields.
type TraceCorrectness struct {
	MaxAbsError      float64 `json:"max_abs_error"`
	MaxRelativeError float64 `json:"max_relative_error"`
}

// TracePerformance matches upstream FlashInfer Trace evaluation.performance fields.
type TracePerformance struct {
	LatencyMs          float64 `json:"latency_ms"`
	ReferenceLatencyMs float64 `json:"reference_latency_ms"`
	SpeedupFactor      float64 `json:"speedup_factor"`
}

// TraceEnvironment records optional hardware and library metadata in a Trace evaluation.
type TraceEnvironment struct {
	Hardware string            `json:"hardware,omitempty"`
	Libs     map[string]string `json:"libs,omitempty"`
}

// TraceEvaluation matches upstream FlashInfer Trace evaluation fields.
type TraceEvaluation struct {
	Status      string            `json:"status"`
	Correctness *TraceCorrectness `json:"correctness,omitempty"`
	Performance *TracePerformance `json:"performance,omitempty"`
	Environment *TraceEnvironment `json:"environment,omitempty"`
	Log         string            `json:"log,omitempty"`
	Timestamp   string            `json:"timestamp,omitempty"`
}

// Trace matches the FlashInfer Trace Schema Trace evaluation record.
type Trace struct {
	Definition string          `json:"definition"`
	Workload   Workload        `json:"workload"`
	Solution   string          `json:"solution,omitempty"`
	Evaluation TraceEvaluation `json:"evaluation"`
}

// DispatchCompatibility declares the target engine and hardware envelope of a dispatch manifest.
type DispatchCompatibility struct {
	Engine        string `json:"engine"`
	EngineVersion string `json:"engineVersion"`
	Manufacturer  string `json:"manufacturer"`
	Architecture  string `json:"architecture"`
}

// DispatchManifestEntry maps one canonical LocalKernelKey to its solution, library, and trace.
type DispatchManifestEntry struct {
	LocalKernelKey    string  `json:"localKernelKey"`
	Definition        string  `json:"definition"`
	SolutionPath      string  `json:"solutionPath"`
	SolutionSHA256    string  `json:"solutionSha256,omitempty"`
	LibraryPath       string  `json:"libraryPath"`
	LibrarySHA256     string  `json:"librarySha256,omitempty"`
	Entrypoint        string  `json:"entrypoint"`
	TracePath         string  `json:"tracePath"`
	TraceSHA256       string  `json:"traceSha256,omitempty"`
	BaselineLatencyUs float64 `json:"baselineLatencyUs"`
	ForgedLatencyUs   float64 `json:"forgedLatencyUs"`
	SpeedupRatio      float64 `json:"speedupRatio"`
}

// DispatchManifest matches dispatch-manifest.json at the root of forge-output.
type DispatchManifest struct {
	SchemaVersion string                  `json:"schemaVersion"`
	Compatibility DispatchCompatibility   `json:"compatibility"`
	Checksums     map[string]string       `json:"checksums,omitempty"`
	Entries       []DispatchManifestEntry `json:"entries"`
}

// VerificationEntryReport records the per-candidate verification verdict in verification-report.json.
type VerificationEntryReport struct {
	Index          int              `json:"index"`
	LocalKernelKey string           `json:"localKernelKey"`
	Definition     string           `json:"definition,omitempty"`
	Passed         bool             `json:"passed"`
	Reason         string           `json:"reason"`
	Evaluation     *TraceEvaluation `json:"evaluation,omitempty"`
}

// VerificationReport matches verification-report.json emitted by forge-verifier and consumed by
// model-manager ingestion.
type VerificationReport struct {
	SchemaVersion          string                    `json:"schemaVersion"`
	OverallStatus          string                    `json:"overallStatus"`
	DispatchManifestDigest string                    `json:"dispatchManifestDigest"`
	PassedCount            int                       `json:"passedCount"`
	FailedCount            int                       `json:"failedCount"`
	Thresholds             VerificationThresholds    `json:"thresholds"`
	Entries                []VerificationEntryReport `json:"entries"`
	Error                  string                    `json:"error,omitempty"`
}

// ValidateSHA256Digest checks that digest has the canonical "sha256:<64 lowercase hex>" format.
func ValidateSHA256Digest(digest string) error {
	if !sha256DigestPattern.MatchString(digest) {
		return fmt.Errorf("digest %q must match sha256:<64 lowercase hex>", digest)
	}
	return nil
}

// ValidateRelativePath verifies that rel is a clean, bounded relative slash path without traversal,
// backslashes, or control characters.
func ValidateRelativePath(rel string) error {
	if rel == "" || len(rel) > MaxRelativePathLength || !utf8.ValidString(rel) {
		return fmt.Errorf("invalid relative path %q", rel)
	}
	if strings.HasPrefix(rel, "/") {
		return fmt.Errorf("relative path %q must not be absolute", rel)
	}
	for _, r := range rel {
		if r < 0x20 || r == 0x7f || r == '\\' {
			return fmt.Errorf("relative path %q contains forbidden character", rel)
		}
	}
	depth := 0
	for seg := range strings.SplitSeq(rel, "/") {
		depth++
		if seg == "" || seg == "." || seg == ".." {
			return fmt.Errorf("relative path %q contains invalid segment %q", rel, seg)
		}
	}
	if depth > MaxRelativePathDepth {
		return fmt.Errorf("relative path %q exceeds max depth %d", rel, MaxRelativePathDepth)
	}
	return nil
}

// ValidateThresholds verifies that numerical and speedup thresholds are finite and within bounds.
func ValidateThresholds(t VerificationThresholds) error {
	if math.IsNaN(t.MinSpeedupRatio) || math.IsInf(t.MinSpeedupRatio, 0) || t.MinSpeedupRatio < MinAllowedSpeedupRatio {
		return fmt.Errorf("minSpeedupRatio %v must be finite and >= %v", t.MinSpeedupRatio, MinAllowedSpeedupRatio)
	}
	if math.IsNaN(t.Atol) || math.IsInf(t.Atol, 0) || t.Atol < 0 {
		return fmt.Errorf("atol %v must be finite and >= 0", t.Atol)
	}
	if math.IsNaN(t.Rtol) || math.IsInf(t.Rtol, 0) || t.Rtol < 0 {
		return fmt.Errorf("rtol %v must be finite and >= 0", t.Rtol)
	}
	return nil
}

// ValidateForgeRequest validates forge-request.json compatibility, target keys, and thresholds.
func ValidateForgeRequest(req *ForgeRequest) error {
	if req == nil {
		return errors.New("forge request must not be nil")
	}
	if req.SchemaVersion != "" && req.SchemaVersion != ForgeRequestSchemaVersion {
		return fmt.Errorf("unsupported forge request schemaVersion %q", req.SchemaVersion)
	}
	target := kernelkey.CompatibilityTarget{
		Engine:        req.Engine,
		EngineVersion: req.EngineVersion,
		Manufacturer:  req.Manufacturer,
		Arch:          req.Architecture,
	}
	if _, err := kernelkey.NormalizeEngine(target.Engine); err != nil {
		return fmt.Errorf("forge request engine: %w", err)
	}
	if _, err := kernelkey.NormalizeEngineVersion(target.EngineVersion); err != nil {
		return fmt.Errorf("forge request engineVersion: %w", err)
	}
	if _, err := kernelkey.NormalizeManufacturer(target.Manufacturer); err != nil {
		return fmt.Errorf("forge request manufacturer: %w", err)
	}
	if _, err := kernelkey.NormalizeArch(target.Manufacturer, target.Arch); err != nil {
		return fmt.Errorf("forge request architecture: %w", err)
	}
	if len(req.TargetKernelKeys) == 0 {
		return errors.New("forge request targetKernelKeys must not be empty")
	}
	seen := make(map[string]struct{}, len(req.TargetKernelKeys))
	for i, rawKey := range req.TargetKernelKeys {
		parsed, err := kernelkey.Parse(rawKey)
		if err != nil {
			return fmt.Errorf("targetKernelKeys[%d]: %w", i, err)
		}
		if excluded, reason := kernelkey.IsExcluded(parsed.OpFamily); excluded {
			return fmt.Errorf("targetKernelKeys[%d]: excluded operator (%s)", i, reason)
		}
		if err := kernelkey.MatchCompatibility(parsed, target); err != nil {
			return fmt.Errorf("targetKernelKeys[%d]: %w", i, err)
		}
		if _, dup := seen[rawKey]; dup {
			return fmt.Errorf("duplicate targetKernelKey %q", rawKey)
		}
		seen[rawKey] = struct{}{}
	}
	for key, latency := range req.BaselineLatenciesUs {
		if math.IsNaN(latency) || math.IsInf(latency, 0) || latency < 0 {
			return fmt.Errorf("baselineLatenciesUs[%q] must be finite and >= 0", key)
		}
	}
	if req.PerKernelTimeoutSeconds < 0 {
		return fmt.Errorf("perKernelTimeoutSeconds %d must not be negative", req.PerKernelTimeoutSeconds)
	}
	return ValidateThresholds(VerificationThresholds{
		MinSpeedupRatio: req.MinSpeedupRatio,
		Atol:            req.Atol,
		Rtol:            req.Rtol,
	})
}

// ValidateDefinition validates a FlashInfer Trace Definition against single-GPU non-GEMM rules.
func ValidateDefinition(def *Definition) error {
	if def == nil {
		return errors.New("definition must not be nil")
	}
	if strings.TrimSpace(def.Name) == "" {
		return errors.New("definition name must not be empty")
	}
	if excluded, reason := kernelkey.IsExcluded(def.Name); excluded {
		return fmt.Errorf("definition name %q is excluded (%s)", def.Name, reason)
	}
	if err := kernelkey.ValidateOpFamily(def.OpType); err != nil {
		return fmt.Errorf("definition op_type: %w", err)
	}
	if def.LocalKernelKey != "" {
		parsed, err := kernelkey.Parse(def.LocalKernelKey)
		if err != nil {
			return fmt.Errorf("definition local_kernel_key: %w", err)
		}
		if parsed.OpFamily != def.OpType {
			return fmt.Errorf("definition op_type %q does not match local_kernel_key opFamily %q",
				def.OpType, parsed.OpFamily)
		}
	}
	if len(def.Axes) == 0 {
		return errors.New("definition axes must not be empty")
	}
	for axisName, spec := range def.Axes {
		if strings.TrimSpace(axisName) == "" {
			return errors.New("definition contains an empty axis name")
		}
		switch spec.Type {
		case "const":
			if kernelkey.IsDynamicVarAxis(axisName) {
				return fmt.Errorf("dynamic var axis %q must not be declared const", axisName)
			}
			if spec.Value == nil || *spec.Value <= 0 {
				return fmt.Errorf("const axis %q must specify a positive value", axisName)
			}
		case "var":
			if spec.Value != nil {
				return fmt.Errorf("var axis %q must not specify a fixed value", axisName)
			}
		default:
			return fmt.Errorf("axis %q has invalid type %q: want const or var", axisName, spec.Type)
		}
	}
	if len(def.Inputs) == 0 {
		return errors.New("definition inputs must not be empty")
	}
	if err := validateTensorSpecs("inputs", def.Inputs, def.Axes); err != nil {
		return err
	}
	if len(def.Outputs) == 0 {
		return errors.New("definition outputs must not be empty")
	}
	if err := validateTensorSpecs("outputs", def.Outputs, def.Axes); err != nil {
		return err
	}
	if strings.TrimSpace(def.Reference) == "" {
		return errors.New("definition reference must not be empty")
	}
	return nil
}

func validateTensorSpecs(section string, specs map[string]TensorSpec, axes map[string]AxisSpec) error {
	for name, spec := range specs {
		if strings.TrimSpace(name) == "" {
			return fmt.Errorf("definition %s contains an empty tensor name", section)
		}
		if strings.TrimSpace(spec.Dtype) == "" {
			return fmt.Errorf("definition %s[%q] dtype must not be empty", section, name)
		}
		for _, dim := range spec.Shape {
			if _, ok := axes[dim]; !ok {
				return fmt.Errorf("definition %s[%q] references undefined axis %q", section, name, dim)
			}
		}
	}
	return nil
}

// ValidateWorkload validates a FlashInfer Trace Workload record.
func ValidateWorkload(wl *Workload) error {
	if wl == nil {
		return errors.New("workload must not be nil")
	}
	if strings.TrimSpace(wl.UUID) == "" {
		return errors.New("workload uuid must not be empty")
	}
	if wl.LocalKernelKey != "" {
		if err := kernelkey.ValidateKey(wl.LocalKernelKey); err != nil {
			return fmt.Errorf("workload local_kernel_key: %w", err)
		}
	}
	for axis, val := range wl.Axes {
		if strings.TrimSpace(axis) == "" {
			return errors.New("workload axes contains an empty axis name")
		}
		if val <= 0 {
			return fmt.Errorf("workload axis %q value %d must be positive", axis, val)
		}
	}
	if len(wl.Inputs) == 0 {
		return errors.New("workload inputs must not be empty")
	}
	for name, in := range wl.Inputs {
		if strings.TrimSpace(name) == "" {
			return errors.New("workload inputs contains an empty input name")
		}
		if strings.TrimSpace(in.Type) == "" {
			return fmt.Errorf("workload inputs[%q] type must not be empty", name)
		}
	}
	if math.IsNaN(wl.ObservedDurationUs) || math.IsInf(wl.ObservedDurationUs, 0) || wl.ObservedDurationUs < 0 {
		return fmt.Errorf("workload observed_duration_us %v must be finite and >= 0", wl.ObservedDurationUs)
	}
	if wl.CallCount < 0 {
		return fmt.Errorf("workload call_count %d must not be negative", wl.CallCount)
	}
	return nil
}

// ValidateSolution validates a FlashInfer Trace Solution object and rejects unsafe source paths.
func ValidateSolution(sol *Solution) error {
	if sol == nil {
		return errors.New("solution must not be nil")
	}
	if strings.TrimSpace(sol.Name) == "" {
		return errors.New("solution name must not be empty")
	}
	if strings.TrimSpace(sol.Definition) == "" {
		return errors.New("solution definition must not be empty")
	}
	if sol.LocalKernelKey != "" {
		if err := kernelkey.ValidateKey(sol.LocalKernelKey); err != nil {
			return fmt.Errorf("solution local_kernel_key: %w", err)
		}
	}
	if strings.TrimSpace(sol.Author) == "" {
		return errors.New("solution author must not be empty")
	}
	if strings.TrimSpace(sol.Spec.Language) == "" {
		return errors.New("solution spec.language must not be empty")
	}
	if strings.TrimSpace(sol.Spec.EntryPoint) == "" {
		return errors.New("solution spec.entry_point must not be empty")
	}
	if len(sol.Spec.TargetHardware) == 0 {
		return errors.New("solution spec.target_hardware must not be empty")
	}
	if len(sol.Sources) == 0 {
		return errors.New("solution sources must not be empty")
	}
	for i, src := range sol.Sources {
		if err := ValidateRelativePath(src.Path); err != nil {
			return fmt.Errorf("solution sources[%d].path: %w", i, err)
		}
	}
	return nil
}

// ValidateEvaluation checks upstream FlashInfer Trace evaluation status, numerical error bounds,
// and performance speedup against thresholds.
func ValidateEvaluation(eval *TraceEvaluation, thresholds VerificationThresholds) error {
	if eval == nil {
		return errors.New("evaluation must not be nil")
	}
	if err := ValidateThresholds(thresholds); err != nil {
		return err
	}
	if eval.Status != StatusPassed {
		return fmt.Errorf("evaluation status is %q, want %q", eval.Status, StatusPassed)
	}
	if eval.Correctness == nil {
		return errors.New("evaluation.correctness must not be nil")
	}
	maxAbs := eval.Correctness.MaxAbsError
	if math.IsNaN(maxAbs) || math.IsInf(maxAbs, 0) || maxAbs < 0 {
		return fmt.Errorf("evaluation.correctness.max_abs_error %v must be finite and >= 0", maxAbs)
	}
	if maxAbs > thresholds.Atol {
		return fmt.Errorf("evaluation.correctness.max_abs_error %v exceeds atol %v", maxAbs, thresholds.Atol)
	}
	maxRel := eval.Correctness.MaxRelativeError
	if math.IsNaN(maxRel) || math.IsInf(maxRel, 0) || maxRel < 0 {
		return fmt.Errorf("evaluation.correctness.max_relative_error %v must be finite and >= 0", maxRel)
	}
	if maxRel > thresholds.Rtol {
		return fmt.Errorf("evaluation.correctness.max_relative_error %v exceeds rtol %v", maxRel, thresholds.Rtol)
	}
	if eval.Performance == nil {
		return errors.New("evaluation.performance must not be nil")
	}
	speedup := eval.Performance.SpeedupFactor
	if math.IsNaN(speedup) || math.IsInf(speedup, 0) || speedup < thresholds.MinSpeedupRatio {
		return fmt.Errorf("evaluation.performance.speedup_factor %v is below minSpeedupRatio %v",
			speedup, thresholds.MinSpeedupRatio)
	}
	if math.IsNaN(eval.Performance.LatencyMs) || math.IsInf(eval.Performance.LatencyMs, 0) || eval.Performance.LatencyMs < 0 {
		return fmt.Errorf("evaluation.performance.latency_ms %v must be finite and >= 0", eval.Performance.LatencyMs)
	}
	if math.IsNaN(eval.Performance.ReferenceLatencyMs) || math.IsInf(eval.Performance.ReferenceLatencyMs, 0) || eval.Performance.ReferenceLatencyMs < 0 {
		return fmt.Errorf("evaluation.performance.reference_latency_ms %v must be finite and >= 0",
			eval.Performance.ReferenceLatencyMs)
	}
	return nil
}

// ValidateTrace validates a FlashInfer Trace record against verification thresholds.
func ValidateTrace(trace *Trace, thresholds VerificationThresholds) error {
	if trace == nil {
		return errors.New("trace must not be nil")
	}
	if strings.TrimSpace(trace.Definition) == "" {
		return errors.New("trace definition must not be empty")
	}
	if err := ValidateWorkload(&trace.Workload); err != nil {
		return fmt.Errorf("trace workload: %w", err)
	}
	return ValidateEvaluation(&trace.Evaluation, thresholds)
}

// ValidateDispatchManifest validates dispatch-manifest.json schema, compatibility, relative paths,
// kernel keys, checksums, and minimum speedup ratio.
func ValidateDispatchManifest(manifest *DispatchManifest, minSpeedupRatio float64) error {
	if manifest == nil {
		return errors.New("dispatch manifest must not be nil")
	}
	if manifest.SchemaVersion != DispatchManifestSchemaVersion {
		return fmt.Errorf("unsupported dispatch manifest schemaVersion %q", manifest.SchemaVersion)
	}
	target := kernelkey.CompatibilityTarget{
		Engine:        manifest.Compatibility.Engine,
		EngineVersion: manifest.Compatibility.EngineVersion,
		Manufacturer:  manifest.Compatibility.Manufacturer,
		Arch:          manifest.Compatibility.Architecture,
	}
	if _, err := kernelkey.NormalizeEngine(target.Engine); err != nil {
		return fmt.Errorf("compatibility.engine: %w", err)
	}
	if _, err := kernelkey.NormalizeEngineVersion(target.EngineVersion); err != nil {
		return fmt.Errorf("compatibility.engineVersion: %w", err)
	}
	if _, err := kernelkey.NormalizeManufacturer(target.Manufacturer); err != nil {
		return fmt.Errorf("compatibility.manufacturer: %w", err)
	}
	if _, err := kernelkey.NormalizeArch(target.Manufacturer, target.Arch); err != nil {
		return fmt.Errorf("compatibility.architecture: %w", err)
	}
	for relPath, digest := range manifest.Checksums {
		if err := ValidateRelativePath(relPath); err != nil {
			return fmt.Errorf("checksums key %q: %w", relPath, err)
		}
		if err := ValidateSHA256Digest(digest); err != nil {
			return fmt.Errorf("checksums[%q]: %w", relPath, err)
		}
	}
	if len(manifest.Entries) == 0 {
		return errors.New("dispatch manifest entries must not be empty")
	}
	seenKeys := make(map[string]struct{}, len(manifest.Entries))
	for i, entry := range manifest.Entries {
		if err := validateDispatchManifestEntry(i, entry, target, minSpeedupRatio); err != nil {
			return err
		}
		if _, dup := seenKeys[entry.LocalKernelKey]; dup {
			return fmt.Errorf("duplicate localKernelKey %q in dispatch manifest", entry.LocalKernelKey)
		}
		seenKeys[entry.LocalKernelKey] = struct{}{}
	}
	return nil
}

func validateDispatchManifestEntry(
	index int, entry DispatchManifestEntry, target kernelkey.CompatibilityTarget, minSpeedupRatio float64,
) error {
	parsed, err := kernelkey.Parse(entry.LocalKernelKey)
	if err != nil {
		return fmt.Errorf("entries[%d].localKernelKey: %w", index, err)
	}
	if excluded, reason := kernelkey.IsExcluded(parsed.OpFamily); excluded {
		return fmt.Errorf("entries[%d].localKernelKey: excluded operator (%s)", index, reason)
	}
	if err := kernelkey.MatchCompatibility(parsed, target); err != nil {
		return fmt.Errorf("entries[%d].localKernelKey: %w", index, err)
	}
	if strings.TrimSpace(entry.Definition) == "" {
		return fmt.Errorf("entries[%d].definition must not be empty", index)
	}
	if err := ValidateRelativePath(entry.SolutionPath); err != nil {
		return fmt.Errorf("entries[%d].solutionPath: %w", index, err)
	}
	if entry.SolutionSHA256 != "" {
		if err := ValidateSHA256Digest(entry.SolutionSHA256); err != nil {
			return fmt.Errorf("entries[%d].solutionSha256: %w", index, err)
		}
	}
	if err := ValidateRelativePath(entry.LibraryPath); err != nil {
		return fmt.Errorf("entries[%d].libraryPath: %w", index, err)
	}
	if entry.LibrarySHA256 != "" {
		if err := ValidateSHA256Digest(entry.LibrarySHA256); err != nil {
			return fmt.Errorf("entries[%d].librarySha256: %w", index, err)
		}
	}
	if strings.TrimSpace(entry.Entrypoint) == "" {
		return fmt.Errorf("entries[%d].entrypoint must not be empty", index)
	}
	if err := ValidateRelativePath(entry.TracePath); err != nil {
		return fmt.Errorf("entries[%d].tracePath: %w", index, err)
	}
	if entry.TraceSHA256 != "" {
		if err := ValidateSHA256Digest(entry.TraceSHA256); err != nil {
			return fmt.Errorf("entries[%d].traceSha256: %w", index, err)
		}
	}
	if math.IsNaN(entry.BaselineLatencyUs) || math.IsInf(entry.BaselineLatencyUs, 0) || entry.BaselineLatencyUs < 0 {
		return fmt.Errorf("entries[%d].baselineLatencyUs %v must be finite and >= 0", index, entry.BaselineLatencyUs)
	}
	if math.IsNaN(entry.ForgedLatencyUs) || math.IsInf(entry.ForgedLatencyUs, 0) || entry.ForgedLatencyUs < 0 {
		return fmt.Errorf("entries[%d].forgedLatencyUs %v must be finite and >= 0", index, entry.ForgedLatencyUs)
	}
	if math.IsNaN(entry.SpeedupRatio) || math.IsInf(entry.SpeedupRatio, 0) || entry.SpeedupRatio <= 0 {
		return fmt.Errorf("entries[%d].speedupRatio %v must be finite and > 0", index, entry.SpeedupRatio)
	}
	if minSpeedupRatio > 0 && entry.SpeedupRatio < minSpeedupRatio {
		return fmt.Errorf("entries[%d].speedupRatio %v is below minSpeedupRatio %v",
			index, entry.SpeedupRatio, minSpeedupRatio)
	}
	return nil
}

// ValidateVerificationReport validates verification-report.json before permitting ingestion.
func ValidateVerificationReport(report *VerificationReport, wantDispatchDigest string) error {
	if report == nil {
		return errors.New("verification report must not be nil")
	}
	if report.SchemaVersion != VerificationReportSchemaVersion {
		return fmt.Errorf("unsupported verification report schemaVersion %q", report.SchemaVersion)
	}
	if report.OverallStatus != StatusPassed {
		return fmt.Errorf("verification report overallStatus is %q, want %q", report.OverallStatus, StatusPassed)
	}
	if err := ValidateSHA256Digest(report.DispatchManifestDigest); err != nil {
		return fmt.Errorf("verification report dispatchManifestDigest: %w", err)
	}
	if wantDispatchDigest != "" && report.DispatchManifestDigest != wantDispatchDigest {
		return fmt.Errorf("verification report dispatchManifestDigest %q does not match expected %q",
			report.DispatchManifestDigest, wantDispatchDigest)
	}
	if report.PassedCount < 1 {
		return fmt.Errorf("verification report passedCount %d must be >= 1", report.PassedCount)
	}
	if report.FailedCount < 0 {
		return fmt.Errorf("verification report failedCount %d must not be negative", report.FailedCount)
	}
	if err := ValidateThresholds(report.Thresholds); err != nil {
		return fmt.Errorf("verification report thresholds: %w", err)
	}
	if len(report.Entries) == 0 {
		return errors.New("verification report entries must not be empty")
	}
	passedEntries := 0
	for i, entry := range report.Entries {
		if !entry.Passed {
			continue
		}
		if err := kernelkey.ValidateKey(entry.LocalKernelKey); err != nil {
			return fmt.Errorf("verification report entries[%d].localKernelKey: %w", i, err)
		}
		if entry.Evaluation != nil {
			if err := ValidateEvaluation(entry.Evaluation, report.Thresholds); err != nil {
				return fmt.Errorf("verification report entries[%d].evaluation: %w", i, err)
			}
		}
		passedEntries++
	}
	if passedEntries != report.PassedCount {
		return fmt.Errorf("verification report passedCount %d does not match %d passed entries",
			report.PassedCount, passedEntries)
	}
	return nil
}
