package priors

import (
	"bytes"
	"fmt"
	"sort"

	yamlv3 "gopkg.in/yaml.v3"
	"sigs.k8s.io/yaml"
)

// CatalogDocument is the catalog as it is written.
//
// IT IS A FLAT DOCUMENT, because a catalog is read by a person before it is read
// by a machine, and a person reads a profile faster when the profile names the
// model, the engine and the hardware on one line each.
//
// IT CARRIES NO DIGEST AND NO PROVENANCE, because a digest of a document that
// holds one is a digest of a digest, and a provenance a reader cannot check is a
// provenance the reader has to trust. The digest of the catalog is computed at
// load, and the source of a measurement is the name of the file it was read
// from.
type CatalogDocument struct {
	// SchemaVersion is the schema the document was written in.
	SchemaVersion string `json:"schemaVersion" yaml:"schemaVersion"`

	// Profiles holds the measured profiles.
	Profiles []ProfileDocument `json:"profiles" yaml:"profiles"`
}

// ProfileDocument is one profile as it is written.
type ProfileDocument struct {
	// Role is the serving role the measurements describe.
	Role string `json:"role" yaml:"role"`

	// Model is the served model.
	Model string `json:"model" yaml:"model"`

	// Quantization is the quantization recipe.
	Quantization string `json:"quantization" yaml:"quantization"`

	// Engine is the serving engine family.
	Engine string `json:"engine" yaml:"engine"`

	// Hardware is the accelerator family.
	Hardware string `json:"hardware" yaml:"hardware"`

	// Topology is the shape of one scaling replica.
	Topology TopologyDocument `json:"topology" yaml:"topology"`

	// Points are the measured load points.
	Points []PointDocument `json:"points" yaml:"points"`
}

// TopologyDocument is the shape of one scaling replica as it is written.
//
// IT NAMES THE WIDTHS THE MEASUREMENT STATES, because a width a source did not
// state is a width of one and a reader has to know which widths were measured.
type TopologyDocument struct {
	// GPUsPerReplica is how many GPUs one scaling replica uses.
	GPUsPerReplica int `json:"gpusPerReplica,omitempty" yaml:"gpusPerReplica,omitempty"`

	// TensorParallel is the tensor parallel width.
	TensorParallel int `json:"tensorParallel,omitempty" yaml:"tensorParallel,omitempty"`

	// PipelineParallel is the pipeline parallel width.
	PipelineParallel int `json:"pipelineParallel,omitempty" yaml:"pipelineParallel,omitempty"`

	// DataParallel is the data parallel width.
	DataParallel int `json:"dataParallel,omitempty" yaml:"dataParallel,omitempty"`

	// LocalDataParallel is the local data parallel width.
	LocalDataParallel int `json:"localDataParallel,omitempty" yaml:"localDataParallel,omitempty"`

	// ContextParallel is the context parallel width.
	ContextParallel int `json:"contextParallel,omitempty" yaml:"contextParallel,omitempty"`

	// ExpertParallel is the expert parallel width.
	ExpertParallel int `json:"expertParallel,omitempty" yaml:"expertParallel,omitempty"`
}

// Normalized returns the topology in its canonical form.
//
// IT REPLACES AN UNSTATED WIDTH WITH ONE, because a width a source did not
// state is the width of one and a width of zero names no deployment.
func (d TopologyDocument) Normalized() Topology {
	topology := Topology(d)
	return topology.Normalized()
}

// PointDocument is one measured load point as it is written.
//
// IT NAMES THE THREE RATES THE MEASUREMENT PRODUCED, because a reader asks
// about the prompt tokens, the generated tokens and the total before it asks
// about any one of them.
type PointDocument struct {
	// Concurrency is how many requests ran at the same time.
	Concurrency int `json:"concurrency" yaml:"concurrency"`

	// TotalTokensPerSecond is the prompt and generated token rate.
	TotalTokensPerSecond float64 `json:"totalTokensPerSecond" yaml:"totalTokensPerSecond"`

	// InputTokensPerSecond is the prompt token rate.
	InputTokensPerSecond float64 `json:"inputTokensPerSecond" yaml:"inputTokensPerSecond"`

	// OutputTokensPerSecond is the generated token rate.
	OutputTokensPerSecond float64 `json:"outputTokensPerSecond" yaml:"outputTokensPerSecond"`

	// TTFTMilliseconds is the measured time to first token.
	TTFTMilliseconds LatencyQuantiles `json:"ttftMilliseconds" yaml:"ttftMilliseconds"`

	// ITLMilliseconds is the measured inter-token latency.
	ITLMilliseconds LatencyQuantiles `json:"itlMilliseconds" yaml:"itlMilliseconds"`
}

// LatencyQuantiles holds the measured latency quantiles of one point.
//
// A QUANTILE THE SOURCE DID NOT PUBLISH IS ABSENT, because a zero latency is a
// measurement and a missing one is not. The reader decides what a missing
// quantile means.
type LatencyQuantiles struct {
	// P50 is the median.
	P50 *float64 `json:"p50,omitempty" yaml:"p50,omitempty"`

	// P90 is the ninetieth percentile.
	P90 *float64 `json:"p90,omitempty" yaml:"p90,omitempty"`

	// P95 is the ninety-fifth percentile.
	P95 *float64 `json:"p95,omitempty" yaml:"p95,omitempty"`

	// P99 is the ninety-ninth percentile.
	P99 *float64 `json:"p99,omitempty" yaml:"p99,omitempty"`
}

// ParseCatalogDocument reads a catalog document.
//
// IT IS STRICT, because a field the reader does not know is a field the reader
// cannot check, and it reads YAML, because YAML is a superset of JSON and one
// reader then serves both formats.
func ParseCatalogDocument(data []byte) (CatalogDocument, error) {
	var document CatalogDocument
	if err := yaml.UnmarshalStrict(data, &document); err != nil {
		return CatalogDocument{}, fmt.Errorf("decode catalog: %w", err)
	}
	return document, nil
}

// MarshalCatalogDocument writes a catalog document.
//
// IT WRITES CANONICAL YAML, because the file is read by people and by machines
// and a file that changes only when its content changes is a file two people
// can diff.
//
// IT KEEPS THE ORDER THE FIELDS ARE DECLARED IN, because a reader reads a
// profile from its name to its measurements and not from the alphabet.
func MarshalCatalogDocument(document CatalogDocument) ([]byte, error) {
	document.sort()
	var buffer bytes.Buffer
	encoder := yamlv3.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(document); err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	return buffer.Bytes(), nil
}

// sort puts the profiles and their points in a stable order.
//
// IT SORTS BY SHAPE AND BY LOAD, because a catalog that is written in the order
// the profiles were measured changes its bytes every time a measurement is
// added.
func (d *CatalogDocument) sort() {
	sort.SliceStable(d.Profiles, func(i, j int) bool {
		return CompareShapes(d.Profiles[i].Shape(), d.Profiles[j].Shape()) < 0
	})
	for i := range d.Profiles {
		points := d.Profiles[i].Points
		sort.SliceStable(points, func(a, b int) bool {
			return points[a].Concurrency < points[b].Concurrency
		})
	}
}

// toCatalog turns a document into the catalog the resolver reads.
func (d CatalogDocument) toCatalog() (*Catalog, error) {
	catalog := Catalog{SchemaVersion: d.SchemaVersion}
	for _, profile := range d.Profiles {
		converted, err := profile.toProfile()
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", profile.Shape(), err)
		}
		catalog.Profiles = append(catalog.Profiles, converted)
	}
	digest, err := CatalogDigest(catalog)
	if err != nil {
		return nil, fmt.Errorf("compute catalog digest: %w", err)
	}
	catalog.Digest = digest
	return &catalog, nil
}

// toProfile turns one written profile into the profile the resolver reads.
//
// IT FILLS THE FIELDS THE DOCUMENT DOES NOT STATE, because the document is a
// measurement and not a configuration: a measurement says how fast the replica
// was, not which serving settings it ran under.
func (d ProfileDocument) toProfile() (Profile, error) {
	role := RoleKind(d.Role)
	if _, known := requirementFor(role); !known {
		return Profile{}, fmt.Errorf("carries the unknown role %q", d.Role)
	}
	shape := d.Shape()
	profile := Profile{
		Role: role,
		Identity: Identity{
			Model: ModelIdentity{
				Name:         CanonicalModelName(d.Model),
				Revision:     UnspecifiedRevision,
				Tokenizer:    CanonicalModelName(d.Model),
				Quantization: CanonicalQuantization(d.Quantization),
			},
			Engine: EngineIdentity{
				// THE DOCUMENT NAMES THE ENGINE FAMILY AND NOT THE IMAGE, because
				// the image carries a tag and a digest that say nothing about
				// the engine behind them.
				Image: d.Engine,
			},
			Hardware: NormalizeHardwareIdentity(HardwareIdentity{Accelerator: d.Hardware}),
		},
		ServingConfig:    ServingConfig{},
		Topology:         d.Topology.Normalized(),
		MeasuredReplicas: 1,
		Workload: Workload{
			// THE WORKLOAD IS NAMED AFTER THE SHAPE, because the document names
			// no workload of its own and a measurement is identified by what it
			// measured.
			ID: shape.String(),
			// THE DOCUMENT PINS NO WEIGHT COMMIT AND NO WORKLOAD REVISION,
			// because a public benchmark publishes a model name and a sweep.
			Revision:      UnspecifiedRevision,
			Modality:      "text",
			CacheBehavior: CacheBehaviorAllMisses,
			ArrivalModel:  ClientModelFixedConcurrency,
		},
	}
	for _, point := range d.Points {
		profile.Points = append(profile.Points, point.toPoint(role))
	}
	if len(profile.Points) >= 2 {
		highest := 0
		for i := 1; i < len(profile.Points); i++ {
			if profile.Points[i].Load.Value > profile.Points[highest].Load.Value {
				highest = i
			}
		}
		profile.Points[highest].Saturation = &Saturation{Measured: true}
	}
	digest, err := ProfileDigest(profile)
	if err != nil {
		return Profile{}, fmt.Errorf("compute profile digest: %w", err)
	}
	profile.Digest = digest
	return profile, nil
}

// toPoint turns one written load point into the point the resolver reads.
//
// IT PICKS THE RATE THE ROLE NEEDS, because the document holds the three rates
// the measurement produced and the resolver asks for one of them.
func (d PointDocument) toPoint(role RoleKind) MeasuredPoint {
	requirement, _ := requirementFor(role)
	rate := d.TotalTokensPerSecond
	switch requirement.metric {
	case ThroughputMetricCacheMissedInputTokensPerSecond:
		rate = d.InputTokensPerSecond
	case ThroughputMetricOutputTokensPerSecond:
		rate = d.OutputTokensPerSecond
	}
	point := MeasuredPoint{
		Load: LoadLevel{
			Kind:        LoadKindConcurrency,
			Value:       float64(d.Concurrency),
			ClientModel: ClientModelFixedConcurrency,
			Unit:        "requests",
		},
		Throughput: Throughput{
			Metric:               requirement.metric,
			Value:                rate,
			Unit:                 UnitTokensPerSecond,
			Denominator:          DenominatorPerReplica,
			TokenCountConvention: requirement.convention,
		},
	}
	point.Latencies = append(point.Latencies, latencyReadings(LatencyMetricTTFT, d.TTFTMilliseconds)...)
	point.Latencies = append(point.Latencies, latencyReadings(LatencyMetricITL, d.ITLMilliseconds)...)
	return point
}

// latencyReadings turns the written quantiles of one metric into latency
// readings.
//
// IT KEEPS THE ORDER OF THE QUANTILES, because a reader reads the readings in
// the order they are written.
func latencyReadings(metric LatencyMetric, quantiles LatencyQuantiles) []Latency {
	readings := make([]Latency, 0, 4)
	for _, quantile := range []struct {
		value float64
		level float64
	}{
		{value: quantileValue(quantiles.P50), level: 0.50},
		{value: quantileValue(quantiles.P90), level: 0.90},
		{value: quantileValue(quantiles.P95), level: 0.95},
		{value: quantileValue(quantiles.P99), level: 0.99},
	} {
		if quantile.value == 0 {
			continue
		}
		readings = append(readings, Latency{
			Metric:   metric,
			Quantile: quantile.level,
			Value:    quantile.value,
			Unit:     UnitMilliseconds,
		})
	}
	return readings
}

// quantileValue returns the value of a written quantile, or zero when the
// source did not publish it.
func quantileValue(value *float64) float64 {
	if value == nil {
		return 0
	}
	return *value
}
