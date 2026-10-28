package priors

import (
	"fmt"
	"math"
	"strings"
)

// ParseCatalog reads a catalog and checks it.
//
// IT IS STRICT, because a field the reader does not know is a field the
// reader cannot check, and it is BOUNDED, because a catalog read from a
// ConfigMap must not be able to grow the controller's memory without limit.
//
// IT READS YAML, because YAML is a superset of JSON and one reader then serves
// both formats. The document it reads is the flat catalog, and the catalog it
// returns is the one the resolver reads.
func ParseCatalog(data []byte) (*Catalog, error) {
	if len(data) > MaxCatalogBytes {
		return nil, fmt.Errorf("catalog is %d bytes, which is over the %d byte limit", len(data), MaxCatalogBytes)
	}

	document, err := ParseCatalogDocument(data)
	if err != nil {
		return nil, err
	}
	catalog, err := toCatalog(document)
	if err != nil {
		return nil, err
	}
	if err := Validate(*catalog); err != nil {
		return nil, err
	}
	return catalog, nil
}

// field is a named value a validation checks.
type field struct {
	name  string
	value string
}

// requireNonEmpty returns an error naming the first empty field.
//
// THE FIELDS ARE CHECKED IN THE ORDER THEY ARE GIVEN, so the error names the
// same field every run.
func requireNonEmpty(fields ...field) error {
	for _, entry := range fields {
		if strings.TrimSpace(entry.value) == "" {
			return fmt.Errorf("%s is empty", entry.name)
		}
	}
	return nil
}

// Validate checks a catalog.
//
// IT REJECTS DUPLICATE SHAPES, UNSUPPORTED SCHEMA VERSIONS AND INVALID
// MEASUREMENTS, because a catalog that carries any of those cannot decide which
// measurement is usable.
func Validate(catalog Catalog) error {
	if catalog.SchemaVersion != SchemaVersion {
		return fmt.Errorf("catalog schema version %q is not supported", catalog.SchemaVersion)
	}
	if len(catalog.Profiles) > MaxProfiles {
		return fmt.Errorf("catalog holds %d profiles, which is over the %d profile limit", len(catalog.Profiles), MaxProfiles)
	}

	sourceIDs := map[string]struct{}{}
	for _, source := range catalog.Sources {
		if _, seen := sourceIDs[source.ID]; seen {
			return fmt.Errorf("source %q appears more than once", source.ID)
		}
		sourceIDs[source.ID] = struct{}{}

		if err := requireNonEmpty(
			field{"source id", source.ID},
			field{"source url", source.URL},
			field{"source revision", source.Revision},
			field{"source license", source.License},
			field{"source adapter version", source.AdapterVersion},
		); err != nil {
			return err
		}
		switch source.Outcome {
		case SourceOutcomeAccepted:
			if source.RefusalError != nil {
				return fmt.Errorf("source %q was accepted but carries a refusal", source.ID)
			}
		case SourceOutcomeRefused:
			if source.RefusalError == nil {
				return fmt.Errorf("source %q was refused but carries no reason", source.ID)
			}
		default:
			return fmt.Errorf("source %q carries the unknown outcome %q", source.ID, source.Outcome)
		}
		for _, artifact := range source.Artifacts {
			if err := requireNonEmpty(
				field{"artifact path", artifact.Path},
				field{"artifact digest", artifact.SHA256},
			); err != nil {
				return fmt.Errorf("source %q: %w", source.ID, err)
			}
		}
	}

	shapes := map[string]int{}
	for index, profile := range catalog.Profiles {
		shape := profile.Shape()
		if first, seen := shapes[shape.String()]; seen {
			return fmt.Errorf(
				"profiles[%d] and profiles[%d] normalize to the same shape %s",
				first, index, shape,
			)
		}
		shapes[shape.String()] = index

		if profile.SourceID != "" {
			if _, known := sourceIDs[profile.SourceID]; !known {
				return fmt.Errorf("profile %s names the unknown source %q", shape, profile.SourceID)
			}
		}
		if err := validateProfile(profile); err != nil {
			return fmt.Errorf("profile %s: %w", shape, err)
		}
	}

	digest, err := CatalogDigest(catalog)
	if err != nil {
		return fmt.Errorf("compute catalog digest: %w", err)
	}
	if catalog.Digest != digest {
		return fmt.Errorf("catalog digest %q does not match the content digest %q", catalog.Digest, digest)
	}
	return nil
}

// validateProfile checks one profile.
//
// IT REQUIRES THE IDENTITY AND THE TOPOLOGY, because a profile that does not
// name the deployment it measured cannot decide which measurement is usable.
//
// IT DOES NOT REQUIRE THE SERVING CONFIGURATION, because the catalog is a
// measurement and not a configuration: a profile that states no serving
// setting constrains nothing, and a profile that states one is compared on
// that one alone.
func validateProfile(profile Profile) error {
	if _, known := requirementFor(profile.Role); !known {
		return fmt.Errorf("carries the unknown role %q", profile.Role)
	}
	if err := requireNonEmpty(
		field{"model name", profile.Identity.Model.Name},
		field{"model revision", profile.Identity.Model.Revision},
		field{"model tokenizer", profile.Identity.Model.Tokenizer},
		field{"model quantization", profile.Identity.Model.Quantization},
		field{"engine image", profile.Identity.Engine.Image},
		field{"hardware accelerator", profile.Identity.Hardware.Accelerator},
		field{"hardware form factor", profile.Identity.Hardware.FormFactor},
		field{"workload id", profile.Workload.ID},
		field{"workload revision", profile.Workload.Revision},
		field{"workload modality", profile.Workload.Modality},
	); err != nil {
		return err
	}
	if profile.Identity.Hardware.MemoryGB <= 0 {
		return fmt.Errorf("hardware memory %d is not positive", profile.Identity.Hardware.MemoryGB)
	}
	if profile.Topology.GPUsPerReplica <= 0 {
		return fmt.Errorf("gpus per replica %d is not positive", profile.Topology.GPUsPerReplica)
	}
	if profile.MeasuredReplicas <= 0 {
		return fmt.Errorf("measured replicas %d is not positive", profile.MeasuredReplicas)
	}
	// THE TENSOR, PIPELINE, DATA AND LOCAL DATA PARALLEL WIDTHS ARE REQUIRED,
	// because a scaling replica is defined by them and a zero width must not
	// match a deployment by both sides leaving it out.
	//
	// CONTEXT AND EXPERT PARALLEL MAY BE ZERO, because a measured system that
	// does not use them states so with a zero width.
	for name, width := range map[string]int{
		"tensor parallel":     profile.Topology.TensorParallel,
		"pipeline parallel":   profile.Topology.PipelineParallel,
		"data parallel":       profile.Topology.DataParallel,
		"local data parallel": profile.Topology.LocalDataParallel,
	} {
		if width <= 0 {
			return fmt.Errorf("%s width %d is not positive", name, width)
		}
	}
	for name, width := range map[string]int{
		"context parallel": profile.Topology.ContextParallel,
		"expert parallel":  profile.Topology.ExpertParallel,
	} {
		if width < 0 {
			return fmt.Errorf("%s width %d is negative", name, width)
		}
	}
	switch profile.Workload.CacheBehavior {
	case CacheBehaviorAllMisses, CacheBehaviorIncludesHits, CacheBehaviorUnknown:
	default:
		return fmt.Errorf("workload cache behavior %q is not known", profile.Workload.CacheBehavior)
	}
	if err := CheckEffectiveLaunchParameters(
		profile.ServingConfig.LaunchParameters,
		profile.ServingConfig.EffectiveLaunchParameters,
	); err != nil {
		return fmt.Errorf("the effective launch parameters do not describe the launch: %w", err)
	}
	switch profile.Workload.ArrivalModel {
	case ClientModelFixedConcurrency, ClientModelOpenLoop:
	default:
		return fmt.Errorf("workload arrival model %q is not known", profile.Workload.ArrivalModel)
	}
	if len(profile.Points) == 0 {
		return fmt.Errorf("holds no measured load point")
	}
	if len(profile.Points) > MaxPointsPerProfile {
		return fmt.Errorf("holds %d measured load points, which is over the %d point limit", len(profile.Points), MaxPointsPerProfile)
	}

	for _, point := range profile.Points {
		if err := validatePoint(point); err != nil {
			return err
		}
	}

	if incoherent := loadAxisIncoherence(profile); incoherent != "" {
		return fmt.Errorf("%s", incoherent)
	}

	// THE DIGEST IS NOT DERIVED, because the catalog is written by a person and a
	// reader has to be able to tell whether the content is the content that was
	// measured.
	digest, err := ProfileDigest(profile)
	if err != nil {
		return fmt.Errorf("compute profile digest: %w", err)
	}
	if profile.Digest != digest {
		return fmt.Errorf("digest %q does not match the content digest %q", profile.Digest, digest)
	}
	return nil
}

// validatePoint checks one measured load point.
func validatePoint(point MeasuredPoint) error {
	switch point.Load.Kind {
	case LoadKindConcurrency, LoadKindRequestRate:
	default:
		return fmt.Errorf("load kind %q is not known", point.Load.Kind)
	}
	switch point.Load.ClientModel {
	case ClientModelFixedConcurrency, ClientModelOpenLoop:
	default:
		return fmt.Errorf("load client model %q is not known", point.Load.ClientModel)
	}
	if err := requireNonEmpty(field{"load unit", point.Load.Unit}); err != nil {
		return err
	}
	if !isFinite(point.Load.Value) || point.Load.Value <= 0 {
		return fmt.Errorf("load value %v is not a finite positive number", point.Load.Value)
	}

	switch point.Throughput.Metric {
	case ThroughputMetricCacheMissedInputTokensPerSecond,
		ThroughputMetricOutputTokensPerSecond,
		ThroughputMetricCacheMissedTotalTokensPerSecond,
		ThroughputMetricInputTokensPerSecondIncludingCacheHits,
		ThroughputMetricTotalTokensPerSecond:
	default:
		return fmt.Errorf("throughput metric %q is not known", point.Throughput.Metric)
	}
	switch point.Throughput.TokenCountConvention {
	case TokenCountConventionCacheMissedInput,
		TokenCountConventionOutput,
		TokenCountConventionCacheMissedInputPlusOutput,
		TokenCountConventionInputIncludingCacheHits,
		TokenCountConventionInputPlusOutput:
	default:
		return fmt.Errorf("token count convention %q is not known", point.Throughput.TokenCountConvention)
	}
	switch point.Throughput.Denominator {
	case DenominatorPerGPU, DenominatorPerReplica, DenominatorWholeSystem:
	default:
		return fmt.Errorf("throughput denominator %q is not known", point.Throughput.Denominator)
	}
	if err := requireNonEmpty(
		field{"throughput unit", point.Throughput.Unit},
		field{"token count convention", string(point.Throughput.TokenCountConvention)},
	); err != nil {
		return err
	}
	if !isFinite(point.Throughput.Value) || point.Throughput.Value <= 0 {
		return fmt.Errorf("throughput value %v is not a finite positive number", point.Throughput.Value)
	}

	if len(point.Latencies) == 0 {
		return fmt.Errorf("holds no latency statistic")
	}
	for _, latency := range point.Latencies {
		switch latency.Metric {
		case LatencyMetricTTFT, LatencyMetricITL, LatencyMetricTPOT:
		default:
			return fmt.Errorf("latency metric %q is not known", latency.Metric)
		}
		if latency.Unit != UnitMilliseconds {
			return fmt.Errorf("latency unit %q is not %q", latency.Unit, UnitMilliseconds)
		}
		if !isFinite(latency.Quantile) || latency.Quantile <= 0 || latency.Quantile > 1 {
			return fmt.Errorf("latency quantile %v is not within (0,1]", latency.Quantile)
		}
		if !isFinite(latency.Value) || latency.Value < 0 {
			return fmt.Errorf("latency value %v is not a finite number", latency.Value)
		}
		if latency.SampleCount < 0 {
			return fmt.Errorf("latency sample count %d is negative", latency.SampleCount)
		}
	}

	if point.Requests.Successful < 0 || point.Requests.Failed < 0 {
		return fmt.Errorf("request counts are negative")
	}
	if !isFinite(point.Duration.WarmUpSeconds) || point.Duration.WarmUpSeconds < 0 {
		return fmt.Errorf("warm-up duration %v is not a finite number", point.Duration.WarmUpSeconds)
	}
	if !isFinite(point.Duration.MeasuredSeconds) || point.Duration.MeasuredSeconds < 0 {
		return fmt.Errorf("measured duration %v is not a finite number", point.Duration.MeasuredSeconds)
	}
	return nil
}

// isFinite says whether a number is neither infinite nor not a number.
func isFinite(value float64) bool {
	return !math.IsNaN(value) && !math.IsInf(value, 0)
}
