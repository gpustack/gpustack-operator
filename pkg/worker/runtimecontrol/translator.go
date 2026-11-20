package runtimecontrol

import (
	"fmt"
	"math/big"
	"slices"
	"sort"
	"strconv"
	"strings"

	"k8s.io/apimachinery/pkg/api/resource"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/elasticprofile"
)

// Canonical keys accepted in spec.roles[*].runtimeControl.parameters.
const (
	ParamSchedulingMaxActiveRequests         = "scheduling.maxActiveRequests"
	ParamSchedulingMaxBatchedTokens          = "scheduling.maxBatchedTokens"
	ParamSchedulingLongPrefillTokenThreshold = "scheduling.longPrefillTokenThreshold"
	ParamSchedulingPrefillMaxRequests        = "scheduling.prefillMaxRequests"
	ParamSchedulingScheduleConservativeness  = "scheduling.scheduleConservativeness"
	ParamSchedulingPrefillDecodeInterval     = "scheduling.prefillDecodeInterval"
	ParamSchedulingCacheAwareWindow          = "scheduling.cacheAwareWindow"
	ParamSchedulingCacheAwareThreshold       = "scheduling.cacheAwareThreshold"
	ParamSchedulingPPMaxMicroBatchSize       = "scheduling.ppMaxMicroBatchSize"
	ParamSchedulingMaxInflightKVLoads        = "scheduling.maxInflightKVLoads"
	ParamSchedulingMaxAsyncKVLoadsPerStep    = "scheduling.maxAsyncKVLoadsPerStep"

	ParamSpeculativeAcceptThresholdSingle = "speculative.acceptThresholdSingle"
	ParamSpeculativeAcceptThresholdAcc    = "speculative.acceptThresholdAcc"
	ParamSpeculativeDraftBudgetFraction   = "speculative.draftBudgetFraction"

	ParamElasticDataParallelWidth = "elastic.dataParallelWidth"
)

// ParameterKind identifies the scalar type of a canonical runtime-control parameter.
type ParameterKind string

const (
	// ParameterKindInt identifies a base-10 integer parameter.
	ParameterKindInt ParameterKind = "int"
	// ParameterKindDecimal identifies a decimal or Kubernetes Quantity parameter.
	ParameterKindDecimal ParameterKind = "decimal"
)

// ParameterSpec describes one canonical key in spec.roles[*].runtimeControl.parameters.
type ParameterSpec struct {
	Key                  string
	Kind                 ParameterKind
	Engines              []string
	EngineManufacturers  map[string][]string
	SentinelManaged      bool
	MinInt               *int64
	MaxInt               *int64
	MinDecimal           string
	MaxDecimal           string
	MinDecimalInclusive  bool
	CeilingFlagsByEngine map[string]string
	RequiresKVConnector  bool
	RequiresPP           bool
}

// ParameterErrorReason classifies a parameter validation failure for webhook mapping.
type ParameterErrorReason string

const (
	// ParameterErrorUnknown reports an unrecognized parameter key.
	ParameterErrorUnknown ParameterErrorReason = "Unknown"
	// ParameterErrorForbidden reports an unsupported engine, manufacturer, or prerequisite.
	ParameterErrorForbidden ParameterErrorReason = "Forbidden"
	// ParameterErrorInvalid reports a malformed value, out-of-range bound, or ceiling violation.
	ParameterErrorInvalid ParameterErrorReason = "Invalid"
)

// ParameterError describes one validation failure for a single parameter key.
type ParameterError struct {
	Key     string
	Value   string
	Reason  ParameterErrorReason
	Message string
}

// BootCeilings holds static boot-time limits and capabilities extracted from role.ExtraArgs.
type BootCeilings struct {
	MaxActiveRequests      *int32
	MaxBatchedTokens       *int32
	ElasticMaxDataParallel *int32
	PipelineParallelSize   *int32
	HasKVConnector         bool
}

// ValidationContext supplies engine, accelerator, and topology context for parameter validation.
type ValidationContext struct {
	Engine         string
	Manufacturer   string
	ExtraArgs      []string
	HasKVConnector bool
}

type parameterEntry struct {
	spec          ParameterSpec
	minDecimalRat *big.Rat
	maxDecimalRat *big.Rat
}

var (
	vllmManufacturers = []string{
		nodefeature.ManufacturerAscend,
		nodefeature.ManufacturerNVIDIA,
	}

	int64Zero       int64 = 0
	int64One        int64 = 1
	int64ElasticMin int64 = elasticprofile.WidthMin
	int64ElasticMax int64 = elasticprofile.WidthMax

	parameterRegistry     map[string]parameterEntry
	allParameterKeysCache []string
)

func init() {
	specs := []ParameterSpec{
		{
			Key:     ParamSchedulingMaxActiveRequests,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged: true,
			MinInt:          &int64One,
			CeilingFlagsByEngine: map[string]string{
				workercore.ModelDeploymentEngineVLLM:   "--max-num-seqs",
				workercore.ModelDeploymentEngineSGLang: "--max-running-requests",
			},
		},
		{
			Key:     ParamSchedulingMaxBatchedTokens,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineSGLang, workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged: true,
			MinInt:          &int64One,
			CeilingFlagsByEngine: map[string]string{
				workercore.ModelDeploymentEngineVLLM:   "--max-num-batched-tokens",
				workercore.ModelDeploymentEngineSGLang: "--chunked-prefill-size",
			},
		},
		{
			Key:     ParamSchedulingLongPrefillTokenThreshold,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged: true,
			MinInt:          &int64Zero,
		},
		{
			Key:             ParamSchedulingPrefillMaxRequests,
			Kind:            ParameterKindInt,
			Engines:         []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged: true,
			MinInt:          &int64One,
		},
		{
			Key:                 ParamSchedulingScheduleConservativeness,
			Kind:                ParameterKindDecimal,
			Engines:             []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged:     true,
			MinDecimal:          "0.1",
			MaxDecimal:          "2.0",
			MinDecimalInclusive: true,
		},
		{
			Key:             ParamSchedulingPrefillDecodeInterval,
			Kind:            ParameterKindInt,
			Engines:         []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged: true,
			MinInt:          &int64Zero,
		},
		{
			Key:     ParamSchedulingCacheAwareWindow,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged: true,
			MinInt:          &int64Zero,
		},
		{
			Key:     ParamSchedulingCacheAwareThreshold,
			Kind:    ParameterKindDecimal,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged:     true,
			MinDecimal:          "0.0",
			MaxDecimal:          "1.0",
			MinDecimalInclusive: true,
		},
		{
			Key:             ParamSchedulingPPMaxMicroBatchSize,
			Kind:            ParameterKindInt,
			Engines:         []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged: true,
			MinInt:          &int64One,
			RequiresPP:      true,
		},
		{
			Key:     ParamSchedulingMaxInflightKVLoads,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged:     true,
			MinInt:              &int64One,
			RequiresKVConnector: true,
		},
		{
			Key:     ParamSchedulingMaxAsyncKVLoadsPerStep,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged:     true,
			MinInt:              &int64One,
			RequiresKVConnector: true,
		},
		{
			Key:                 ParamSpeculativeAcceptThresholdSingle,
			Kind:                ParameterKindDecimal,
			Engines:             []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged:     true,
			MinDecimal:          "0.0",
			MaxDecimal:          "1.0",
			MinDecimalInclusive: true,
		},
		{
			Key:                 ParamSpeculativeAcceptThresholdAcc,
			Kind:                ParameterKindDecimal,
			Engines:             []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged:     true,
			MinDecimal:          "0.0",
			MaxDecimal:          "1.0",
			MinDecimalInclusive: true,
		},
		{
			Key:                 ParamSpeculativeDraftBudgetFraction,
			Kind:                ParameterKindDecimal,
			Engines:             []string{workercore.ModelDeploymentEngineSGLang},
			SentinelManaged:     true,
			MinDecimal:          "0.0",
			MaxDecimal:          "1.0",
			MinDecimalInclusive: false,
		},
		{
			Key:     ParamElasticDataParallelWidth,
			Kind:    ParameterKindInt,
			Engines: []string{workercore.ModelDeploymentEngineVLLM},
			EngineManufacturers: map[string][]string{
				workercore.ModelDeploymentEngineVLLM: vllmManufacturers,
			},
			SentinelManaged: false,
			MinInt:          &int64ElasticMin,
			MaxInt:          &int64ElasticMax,
			CeilingFlagsByEngine: map[string]string{
				workercore.ModelDeploymentEngineVLLM: "--elastic-ep-max-dp-size",
			},
		},
	}

	parameterRegistry = make(map[string]parameterEntry, len(specs))
	allParameterKeysCache = make([]string, 0, len(specs))
	for _, sp := range specs {
		entry := parameterEntry{spec: sp}
		if sp.Kind == ParameterKindDecimal {
			if sp.MinDecimal != "" {
				entry.minDecimalRat, _ = new(big.Rat).SetString(sp.MinDecimal)
			}
			if sp.MaxDecimal != "" {
				entry.maxDecimalRat, _ = new(big.Rat).SetString(sp.MaxDecimal)
			}
		}
		parameterRegistry[sp.Key] = entry
		allParameterKeysCache = append(allParameterKeysCache, sp.Key)
	}
	sort.Strings(allParameterKeysCache)
}

func (e ParameterError) Error() string {
	return fmt.Sprintf("runtimeControl.parameters[%q]: %s", e.Key, e.Message)
}

// LookupParameter returns the canonical ParameterSpec for key when registered.
func LookupParameter(key string) (ParameterSpec, bool) {
	entry, ok := parameterRegistry[key]
	if !ok {
		return ParameterSpec{}, false
	}
	return cloneParameterSpec(entry.spec), true
}

func cloneParameterSpec(src ParameterSpec) ParameterSpec {
	out := src
	if len(src.Engines) > 0 {
		out.Engines = append([]string(nil), src.Engines...)
	}
	if len(src.EngineManufacturers) > 0 {
		out.EngineManufacturers = make(map[string][]string, len(src.EngineManufacturers))
		for k, v := range src.EngineManufacturers {
			out.EngineManufacturers[k] = append([]string(nil), v...)
		}
	}
	if len(src.CeilingFlagsByEngine) > 0 {
		out.CeilingFlagsByEngine = make(map[string]string, len(src.CeilingFlagsByEngine))
		for k, v := range src.CeilingFlagsByEngine {
			out.CeilingFlagsByEngine[k] = v
		}
	}
	if src.MinInt != nil {
		v := *src.MinInt
		out.MinInt = &v
	}
	if src.MaxInt != nil {
		v := *src.MaxInt
		out.MaxInt = &v
	}
	return out
}

// AllParameterKeys returns a sorted slice of all canonical parameter keys.
func AllParameterKeys() []string {
	return append([]string(nil), allParameterKeysCache...)
}

// AllSentinelParameterKeys returns a sorted slice of sentinel-managed parameter keys
// supported by engine (or all sentinel-managed keys when engine is empty).
func AllSentinelParameterKeys(engine string) []string {
	out := make([]string, 0, len(allParameterKeysCache))
	for _, key := range allParameterKeysCache {
		spec := parameterRegistry[key].spec
		if !spec.SentinelManaged {
			continue
		}
		if engine != "" && !slices.Contains(spec.Engines, engine) {
			continue
		}
		out = append(out, key)
	}
	return out
}

// SentinelParameterKeys returns the sorted sentinel-managed keys present in params.
func SentinelParameterKeys(params map[string]string) []string {
	if len(params) == 0 {
		return nil
	}
	out := make([]string, 0, len(params))
	for key := range params {
		if entry, ok := parameterRegistry[key]; ok && entry.spec.SentinelManaged {
			out = append(out, key)
		}
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// ComputeResetFields returns the sorted sentinel-managed parameter keys that were
// previously applied on a Pod and are no longer present in desired.
func ComputeResetFields(desired map[string]string, previouslyApplied []string) []string {
	if len(previouslyApplied) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(previouslyApplied))
	out := make([]string, 0, len(previouslyApplied))
	for _, key := range previouslyApplied {
		entry, ok := parameterRegistry[key]
		if !ok || !entry.spec.SentinelManaged {
			continue
		}
		if _, wanted := desired[key]; wanted {
			continue
		}
		if _, dup := seen[key]; dup {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, key)
	}
	if len(out) == 0 {
		return nil
	}
	sort.Strings(out)
	return out
}

// RoleHasElasticEP reports whether role configures a non-empty elastic.dataParallelWidth parameter.
func RoleHasElasticEP(role *workercore.ModelDeploymentRole) bool {
	if role == nil || role.RuntimeControl == nil || len(role.RuntimeControl.Parameters) == 0 {
		return false
	}
	raw, exists := role.RuntimeControl.Parameters[ParamElasticDataParallelWidth]
	return exists && strings.TrimSpace(raw) != ""
}

// ParseElasticWidth parses and validates role.RuntimeControl.Parameters["elastic.dataParallelWidth"].
// When the key is absent, it returns (0, false, nil). When present, ok is true and width is validated
// against [elasticprofile.WidthMin, elasticprofile.WidthMax].
func ParseElasticWidth(role *workercore.ModelDeploymentRole) (int32, bool, error) {
	if role == nil || role.RuntimeControl == nil || len(role.RuntimeControl.Parameters) == 0 {
		return 0, false, nil
	}
	raw, exists := role.RuntimeControl.Parameters[ParamElasticDataParallelWidth]
	if !exists {
		return 0, false, nil
	}
	val, err := parseStrictInt32(raw)
	if err != nil {
		return 0, true, fmt.Errorf(
			"%w: %s %q must be an integer between %d and %d",
			ErrInvalidArgument, ParamElasticDataParallelWidth, raw,
			elasticprofile.WidthMin, elasticprofile.WidthMax,
		)
	}
	if val < elasticprofile.WidthMin || val > elasticprofile.WidthMax {
		return 0, true, fmt.Errorf(
			"%w: %s %d must be between %d and %d",
			ErrInvalidArgument, ParamElasticDataParallelWidth, val,
			elasticprofile.WidthMin, elasticprofile.WidthMax,
		)
	}
	return val, true, nil
}

// SetElasticWidth writes elastic.dataParallelWidth onto role.RuntimeControl.Parameters,
// initializing RuntimeControl and Parameters when nil.
func SetElasticWidth(role *workercore.ModelDeploymentRole, width int32) {
	if role == nil {
		return
	}
	if role.RuntimeControl == nil {
		role.RuntimeControl = &workercore.ModelDeploymentRoleRuntimeControl{}
	}
	if role.RuntimeControl.Parameters == nil {
		role.RuntimeControl.Parameters = make(map[string]string, 1)
	}
	role.RuntimeControl.Parameters[ParamElasticDataParallelWidth] = strconv.Itoa(int(width))
}

// ParseExtraArgsBootCeilings extracts declared boot ceilings and topology flags from role.ExtraArgs.
func ParseExtraArgsBootCeilings(engine string, extraArgs []string) BootCeilings {
	var out BootCeilings
	for i := 0; i < len(extraArgs); i++ {
		arg := extraArgs[i]
		if arg == "--" {
			break
		}
		rawName, val, hasVal := strings.Cut(arg, "=")
		name := normalizeExtraArgName(engine, rawName)

		switch engine {
		case workercore.ModelDeploymentEngineVLLM:
			switch {
			case name == "--max-num-seqs":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.MaxActiveRequests = &n
				}
			case name == "--max-num-batched-tokens":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.MaxBatchedTokens = &n
				}
			case name == "--elastic-ep-max-dp-size":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.ElasticMaxDataParallel = &n
				}
			case name == "--pipeline-parallel-size" || name == "-pp":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.PipelineParallelSize = &n
				}
			case name == "--kv-transfer-config" || strings.HasPrefix(name, "--kv-transfer-config."):
				out.HasKVConnector = true
				if !hasVal && i+1 < len(extraArgs) && !strings.HasPrefix(extraArgs[i+1], "-") {
					i++
				}
			}
		case workercore.ModelDeploymentEngineSGLang:
			switch name {
			case "--max-running-requests":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.MaxActiveRequests = &n
				}
			case "--chunked-prefill-size":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.MaxBatchedTokens = &n
				}
			case "--pp-size", "--pipeline-parallel-size":
				val, i = consumeFlagValue(extraArgs, i, val, hasVal)
				if n, ok := parsePositiveInt32(val); ok {
					out.PipelineParallelSize = &n
				}
			}
		}
	}
	return out
}

func normalizeExtraArgName(engine, rawName string) string {
	if engine != workercore.ModelDeploymentEngineVLLM || !strings.HasPrefix(rawName, "--") {
		return rawName
	}
	head, tail, dotted := strings.Cut(rawName, ".")
	head = strings.ReplaceAll(head, "_", "-")
	if !dotted {
		return head
	}
	return head + "." + tail
}

func consumeFlagValue(args []string, idx int, inlineVal string, hasVal bool) (string, int) {
	if hasVal {
		return inlineVal, idx
	}
	if idx+1 < len(args) {
		next := args[idx+1]
		if next != "--" && (!strings.HasPrefix(next, "-") || isNumericToken(next)) {
			return next, idx + 1
		}
	}
	return "", idx
}

func isNumericToken(s string) bool {
	_, err := strconv.ParseInt(strings.TrimSpace(s), 10, 64)
	return err == nil
}

func parsePositiveInt32(raw string) (int32, bool) {
	v, err := parseStrictInt32(raw)
	if err != nil || v <= 0 {
		return 0, false
	}
	return v, true
}

func parseStrictInt32(raw string) (int32, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return 0, fmt.Errorf("value must not be empty")
	}
	v, err := strconv.ParseInt(trimmed, 10, 32)
	if err != nil {
		return 0, fmt.Errorf("invalid integer %q", raw)
	}
	return int32(v), nil
}

func parseDecimalValue(raw string) (canonical string, rat *big.Rat, err error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "", nil, fmt.Errorf("value must not be empty")
	}
	if len(trimmed) > 32 {
		return "", nil, fmt.Errorf("decimal quantity %q exceeds maximum supported length", raw)
	}
	if idx := strings.IndexAny(trimmed, "eE"); idx >= 0 {
		expStr := trimmed[idx+1:]
		expVal, expErr := strconv.ParseInt(expStr, 10, 32)
		if expErr != nil || expVal < -maxCanonicalDecimalScaleAbs || expVal > maxCanonicalDecimalScaleAbs {
			return "", nil, fmt.Errorf("decimal quantity %q exceeds supported exponent range", raw)
		}
	}
	q, parseErr := resource.ParseQuantity(trimmed)
	if parseErr != nil {
		return "", nil, fmt.Errorf("invalid decimal quantity %q", raw)
	}
	if q.Format == resource.BinarySI {
		return "", nil, fmt.Errorf("binary suffix is not allowed in decimal quantity %q", raw)
	}
	dec := q.AsDec()
	unscaled := dec.UnscaledBig()
	scale := int(dec.Scale())
	if unscaled.Sign() != 0 &&
		(scale < -maxCanonicalDecimalScaleAbs ||
			scale > maxCanonicalDecimalScaleAbs ||
			unscaled.BitLen() > maxCanonicalUnscaledBits) {
		return "", nil, fmt.Errorf("decimal quantity %q exceeds supported precision or exponent range", raw)
	}
	canonical = CanonicalQuantityDecimal(&q)
	rat, ok := new(big.Rat).SetString(canonical)
	if !ok {
		return "", nil, fmt.Errorf("invalid decimal quantity %q", raw)
	}
	return canonical, rat, nil
}

// ValidateParameters validates spec.roles[*].runtimeControl.parameters against the canonical
// registry, engine/manufacturer support, value bounds, prerequisites, cross-parameter invariants,
// and declared extraArgs boot ceilings.
func ValidateParameters(ctx ValidationContext, params map[string]string) []ParameterError {
	if len(params) == 0 {
		return nil
	}

	ceilings := ParseExtraArgsBootCeilings(ctx.Engine, ctx.ExtraArgs)
	hasKVConnector := ctx.HasKVConnector || ceilings.HasKVConnector
	manufacturer := strings.ToLower(strings.TrimSpace(ctx.Manufacturer))

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var errs []ParameterError
	parsedInts := make(map[string]int32, len(params))

	for _, key := range keys {
		raw := params[key]
		entry, known := parameterRegistry[key]
		if !known {
			errs = append(errs, ParameterError{
				Key:     key,
				Value:   raw,
				Reason:  ParameterErrorUnknown,
				Message: fmt.Sprintf("unknown runtimeControl parameter %q", key),
			})
			continue
		}

		spec := entry.spec
		if !slices.Contains(spec.Engines, ctx.Engine) {
			errs = append(errs, ParameterError{
				Key:     key,
				Value:   raw,
				Reason:  ParameterErrorForbidden,
				Message: fmt.Sprintf("%s is not supported on engine %q (supported: %s)", key, ctx.Engine, strings.Join(spec.Engines, ", ")),
			})
			continue
		}

		if manufacturer != "" {
			if allowed := spec.EngineManufacturers[ctx.Engine]; len(allowed) > 0 && !slices.Contains(allowed, manufacturer) {
				errs = append(errs, ParameterError{
					Key:    key,
					Value:  raw,
					Reason: ParameterErrorForbidden,
					Message: fmt.Sprintf(
						"%s is not supported on %s with accelerator manufacturer %q (supported: %s)",
						key, ctx.Engine, manufacturer, strings.Join(allowed, ", "),
					),
				})
				continue
			}
		}

		switch spec.Kind {
		case ParameterKindInt:
			val, parseErr := parseStrictInt32(raw)
			if parseErr != nil {
				errs = append(errs, ParameterError{
					Key:     key,
					Value:   raw,
					Reason:  ParameterErrorInvalid,
					Message: fmt.Sprintf("must be a base-10 integer (%v)", parseErr),
				})
				continue
			}
			val64 := int64(val)
			if spec.MinInt != nil && val64 < *spec.MinInt {
				errs = append(errs, ParameterError{
					Key:     key,
					Value:   raw,
					Reason:  ParameterErrorInvalid,
					Message: formatIntBoundMessage(spec),
				})
				continue
			}
			if spec.MaxInt != nil && val64 > *spec.MaxInt {
				errs = append(errs, ParameterError{
					Key:     key,
					Value:   raw,
					Reason:  ParameterErrorInvalid,
					Message: formatIntBoundMessage(spec),
				})
				continue
			}
			parsedInts[key] = val

		case ParameterKindDecimal:
			_, rat, parseErr := parseDecimalValue(raw)
			if parseErr != nil {
				errs = append(errs, ParameterError{
					Key:     key,
					Value:   raw,
					Reason:  ParameterErrorInvalid,
					Message: fmt.Sprintf("must be a valid decimal quantity in %s", formatDecimalRange(spec)),
				})
				continue
			}
			if !decimalInBounds(rat, entry) {
				errs = append(errs, ParameterError{
					Key:     key,
					Value:   raw,
					Reason:  ParameterErrorInvalid,
					Message: fmt.Sprintf("must be in range %s", formatDecimalRange(spec)),
				})
				continue
			}
		}

		if spec.RequiresKVConnector && !hasKVConnector {
			errs = append(errs, ParameterError{
				Key:     key,
				Value:   raw,
				Reason:  ParameterErrorForbidden,
				Message: "requires an active KV connector (spec.kvCache, spec.kvTransfer, routed prefill/decode pair, or --kv-transfer-config)",
			})
		}

		if spec.RequiresPP && (ceilings.PipelineParallelSize == nil || *ceilings.PipelineParallelSize <= 1) {
			errs = append(errs, ParameterError{
				Key:     key,
				Value:   raw,
				Reason:  ParameterErrorInvalid,
				Message: "requires pipeline parallelism (--pp-size / --pipeline-parallel-size > 1)",
			})
		}

		if val, ok := parsedInts[key]; ok {
			if ceilingErr := checkDeclaredCeiling(ctx.Engine, spec, val, raw, ceilings); ceilingErr != nil {
				errs = append(errs, *ceilingErr)
			}
		}
	}

	// Cross-parameter invariants evaluated after individual values are parsed.
	if val, ok := parsedInts[ParamSchedulingLongPrefillTokenThreshold]; ok {
		if eff, hasEff := effectiveCeiling(parsedInts, ParamSchedulingMaxBatchedTokens, ceilings.MaxBatchedTokens); hasEff && val > eff {
			errs = append(errs, ParameterError{
				Key:     ParamSchedulingLongPrefillTokenThreshold,
				Value:   params[ParamSchedulingLongPrefillTokenThreshold],
				Reason:  ParameterErrorInvalid,
				Message: fmt.Sprintf("must not exceed effective maxBatchedTokens (%d)", eff),
			})
		}
	}

	if val, ok := parsedInts[ParamSchedulingPrefillMaxRequests]; ok {
		if eff, hasEff := effectiveCeiling(parsedInts, ParamSchedulingMaxActiveRequests, ceilings.MaxActiveRequests); hasEff && val > eff {
			errs = append(errs, ParameterError{
				Key:     ParamSchedulingPrefillMaxRequests,
				Value:   params[ParamSchedulingPrefillMaxRequests],
				Reason:  ParameterErrorInvalid,
				Message: fmt.Sprintf("must not exceed effective maxActiveRequests (%d)", eff),
			})
		}
	}

	if val, ok := parsedInts[ParamSchedulingPPMaxMicroBatchSize]; ok &&
		ceilings.PipelineParallelSize != nil && *ceilings.PipelineParallelSize > 1 {
		ppSize := *ceilings.PipelineParallelSize
		if eff, hasEff := effectiveCeiling(parsedInts, ParamSchedulingMaxActiveRequests, ceilings.MaxActiveRequests); hasEff {
			maxAllowed := eff / ppSize
			if val > maxAllowed {
				errs = append(errs, ParameterError{
					Key:    ParamSchedulingPPMaxMicroBatchSize,
					Value:  params[ParamSchedulingPPMaxMicroBatchSize],
					Reason: ParameterErrorInvalid,
					Message: fmt.Sprintf(
						"must not exceed effective maxActiveRequests / ppSize (%d / %d = %d)",
						eff, ppSize, maxAllowed,
					),
				})
			}
		}
	}

	return errs
}

func formatIntBoundMessage(spec ParameterSpec) string {
	switch {
	case spec.MinInt != nil && spec.MaxInt != nil:
		return fmt.Sprintf("must be between %d and %d", *spec.MinInt, *spec.MaxInt)
	case spec.MinInt != nil:
		return fmt.Sprintf("must be greater than or equal to %d", *spec.MinInt)
	case spec.MaxInt != nil:
		return fmt.Sprintf("must be less than or equal to %d", *spec.MaxInt)
	default:
		return "value out of range"
	}
}

func formatDecimalRange(spec ParameterSpec) string {
	left := "("
	if spec.MinDecimalInclusive {
		left = "["
	}
	return fmt.Sprintf("%s%s, %s]", left, spec.MinDecimal, spec.MaxDecimal)
}

func decimalInBounds(rat *big.Rat, entry parameterEntry) bool {
	if entry.minDecimalRat != nil {
		cmp := rat.Cmp(entry.minDecimalRat)
		if entry.spec.MinDecimalInclusive {
			if cmp < 0 {
				return false
			}
		} else if cmp <= 0 {
			return false
		}
	}
	if entry.maxDecimalRat != nil && rat.Cmp(entry.maxDecimalRat) > 0 {
		return false
	}
	return true
}

func checkDeclaredCeiling(
	engine string,
	spec ParameterSpec,
	val int32,
	raw string,
	ceilings BootCeilings,
) *ParameterError {
	var limit *int32
	switch spec.Key {
	case ParamSchedulingMaxActiveRequests:
		limit = ceilings.MaxActiveRequests
	case ParamSchedulingMaxBatchedTokens:
		limit = ceilings.MaxBatchedTokens
	case ParamElasticDataParallelWidth:
		limit = ceilings.ElasticMaxDataParallel
	default:
		return nil
	}
	if limit == nil || val <= *limit {
		return nil
	}
	flagName := spec.CeilingFlagsByEngine[engine]
	return &ParameterError{
		Key:     spec.Key,
		Value:   raw,
		Reason:  ParameterErrorInvalid,
		Message: fmt.Sprintf("must not exceed declared %s ceiling (%d)", flagName, *limit),
	}
}

func effectiveCeiling(parsedInts map[string]int32, paramKey string, bootCeiling *int32) (int32, bool) {
	if val, ok := parsedInts[paramKey]; ok && val > 0 {
		return val, true
	}
	if bootCeiling != nil && *bootCeiling > 0 {
		return *bootCeiling, true
	}
	return 0, false
}

// CanonicalizeParameters normalizes all known parameter values into deterministic canonical
// string representations (for example, "0.50" and "500m" both canonicalize to "0.5", and
// "+16" canonicalizes to "16"). Returns nil when params is empty.
func CanonicalizeParameters(params map[string]string) (map[string]string, error) {
	if len(params) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]string, len(params))
	for _, key := range keys {
		entry, ok := parameterRegistry[key]
		if !ok {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalidArgument, key)
		}
		raw := params[key]
		switch entry.spec.Kind {
		case ParameterKindInt:
			v, err := parseStrictInt32(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: parameter %q: %w", ErrInvalidArgument, key, err)
			}
			out[key] = strconv.FormatInt(int64(v), 10)
		case ParameterKindDecimal:
			canonical, _, err := parseDecimalValue(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: parameter %q: %w", ErrInvalidArgument, key, err)
			}
			out[key] = canonical
		}
	}
	return out, nil
}

// BuildSentinelParameters translates sentinel-managed parameters into typed JSON numbers
// (int64 for integer keys, float64 for decimal keys) suitable for POST /gpustack/v1/runtime_config.
// Non-sentinel keys such as elastic.dataParallelWidth are omitted.
func BuildSentinelParameters(params map[string]string) (map[string]any, error) {
	if len(params) == 0 {
		return nil, nil
	}

	keys := make([]string, 0, len(params))
	for k := range params {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	out := make(map[string]any, len(params))
	for _, key := range keys {
		entry, ok := parameterRegistry[key]
		if !ok {
			return nil, fmt.Errorf("%w: unknown parameter %q", ErrInvalidArgument, key)
		}
		if !entry.spec.SentinelManaged {
			continue
		}
		raw := params[key]
		switch entry.spec.Kind {
		case ParameterKindInt:
			v, err := parseStrictInt32(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: parameter %q: %w", ErrInvalidArgument, key, err)
			}
			out[key] = int64(v)
		case ParameterKindDecimal:
			canonical, _, err := parseDecimalValue(raw)
			if err != nil {
				return nil, fmt.Errorf("%w: parameter %q: %w", ErrInvalidArgument, key, err)
			}
			f, err := strconv.ParseFloat(canonical, 64)
			if err != nil {
				return nil, fmt.Errorf("%w: parameter %q: %w", ErrInvalidArgument, key, err)
			}
			out[key] = f
		}
	}
	if len(out) == 0 {
		return nil, nil
	}
	return out, nil
}
