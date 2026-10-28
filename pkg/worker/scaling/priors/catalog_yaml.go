package priors

import (
	"bytes"
	"fmt"
	"math/big"
	"sort"
	"strconv"
	"strings"

	yamlv3 "gopkg.in/yaml.v3"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/yaml"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
type CatalogDocument = workercore.ModelDeploymentScalerCapacityPriorCatalog

// ProfileDocument is one profile as it is written.
type ProfileDocument = workercore.ModelDeploymentScalerCapacityPriorProfile

// TopologyDocument is the shape of one scaling replica as it is written.
//
// IT NAMES THE WIDTHS THE MEASUREMENT STATES, because a width a source did not
// state is a width of one and a reader has to know which widths were measured.
type TopologyDocument = workercore.ModelDeploymentScalerCapacityPriorTopology

// PointDocument is one measured load point as it is written.
//
// IT NAMES THE THREE RATES THE MEASUREMENT PRODUCED, because a reader asks
// about the prompt tokens, the generated tokens and the total before it asks
// about any one of them.
type PointDocument = workercore.ModelDeploymentScalerCapacityPriorPoint

// LatencyQuantiles holds the measured latency quantiles of one point, keyed by
// percentile names such as p25, p50, p70, p90, p95, or p99.
//
// A QUANTILE THE SOURCE DID NOT PUBLISH IS ABSENT, because a zero latency is a
// measurement and a missing one is not. The reader decides what a missing
// quantile means.
type LatencyQuantiles = map[string]float64

// NewCatalogDocument returns a catalog document with the canonical TypeMeta
// header and the given profiles.
func NewCatalogDocument(profiles ...ProfileDocument) CatalogDocument {
	return CatalogDocument{
		TypeMeta: meta.TypeMeta{
			APIVersion: CatalogAPIVersion,
			Kind:       CatalogKind,
		},
		Profiles: profiles,
	}
}

// catalogDocumentWire orders the top-level fields of a written catalog so
// apiVersion and kind always precede profiles when encoded by yaml.v3.
type catalogDocumentWire struct {
	APIVersion string            `yaml:"apiVersion"`
	Kind       string            `yaml:"kind"`
	Profiles   []ProfileDocument `yaml:"profiles"`
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
	if document.APIVersion == "" {
		document.APIVersion = CatalogAPIVersion
	}
	if document.Kind == "" {
		document.Kind = CatalogKind
	}
	sortCatalogDocument(&document)
	wire := catalogDocumentWire{
		APIVersion: document.APIVersion,
		Kind:       document.Kind,
		Profiles:   document.Profiles,
	}
	if wire.Profiles == nil {
		wire.Profiles = []ProfileDocument{}
	}
	var node yamlv3.Node
	if err := node.Encode(wire); err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	sortQuantileMappingNodes(&node)
	var buffer bytes.Buffer
	encoder := yamlv3.NewEncoder(&buffer)
	encoder.SetIndent(2)
	if err := encoder.Encode(&node); err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	if err := encoder.Close(); err != nil {
		return nil, fmt.Errorf("encode catalog: %w", err)
	}
	return buffer.Bytes(), nil
}

// sortCatalogDocument puts the profiles and their points in a stable order.
//
// IT SORTS BY SHAPE AND BY LOAD, because a catalog that is written in the order
// the profiles were measured changes its bytes every time a measurement is
// added.
func sortCatalogDocument(d *CatalogDocument) {
	sort.SliceStable(d.Profiles, func(i, j int) bool {
		return CompareShapes(DocumentShape(d.Profiles[i]), DocumentShape(d.Profiles[j])) < 0
	})
	for i := range d.Profiles {
		points := d.Profiles[i].Points
		sort.SliceStable(points, func(a, b int) bool {
			return points[a].Concurrency < points[b].Concurrency
		})
	}
}

// sortQuantileMappingNodes orders latency quantile keys in ascending numeric
// percentile order inside ttftMilliseconds and itlMilliseconds mappings.
func sortQuantileMappingNodes(node *yamlv3.Node) {
	if node == nil {
		return
	}
	if node.Kind == yamlv3.MappingNode {
		for i := 0; i+1 < len(node.Content); i += 2 {
			key := node.Content[i].Value
			val := node.Content[i+1]
			if (key == "ttftMilliseconds" || key == "itlMilliseconds") && val.Kind == yamlv3.MappingNode {
				sortQuantilePairs(val)
			} else {
				sortQuantileMappingNodes(val)
			}
		}
		return
	}
	for _, child := range node.Content {
		sortQuantileMappingNodes(child)
	}
}

func sortQuantilePairs(mapping *yamlv3.Node) {
	type pair struct {
		key *yamlv3.Node
		val *yamlv3.Node
	}
	pairs := make([]pair, 0, len(mapping.Content)/2)
	for i := 0; i+1 < len(mapping.Content); i += 2 {
		pairs = append(pairs, pair{key: mapping.Content[i], val: mapping.Content[i+1]})
	}
	sort.SliceStable(pairs, func(i, j int) bool {
		return compareQuantileKeys(pairs[i].key.Value, pairs[j].key.Value) < 0
	})
	for i, entry := range pairs {
		mapping.Content[2*i] = entry.key
		mapping.Content[2*i+1] = entry.val
	}
}

func compareQuantileKeys(left, right string) int {
	leftLevel, leftErr := parseQuantileKey(left)
	rightLevel, rightErr := parseQuantileKey(right)
	if leftErr == nil && rightErr == nil && leftLevel != rightLevel {
		if leftLevel < rightLevel {
			return -1
		}
		return 1
	}
	return strings.Compare(left, right)
}

// toCatalog turns a document into the catalog the resolver reads.
func toCatalog(d CatalogDocument) (*Catalog, error) {
	if d.APIVersion != CatalogAPIVersion {
		return nil, fmt.Errorf("catalog apiVersion %q is not supported, want %q", d.APIVersion, CatalogAPIVersion)
	}
	if d.Kind != CatalogKind {
		return nil, fmt.Errorf("catalog kind %q is not supported, want %q", d.Kind, CatalogKind)
	}
	catalog := Catalog{SchemaVersion: d.APIVersion}
	for _, profile := range d.Profiles {
		converted, err := toProfile(profile)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", DocumentShape(profile), err)
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
func toProfile(d ProfileDocument) (Profile, error) {
	role := RoleKind(d.Role)
	if _, known := requirementFor(role); !known {
		return Profile{}, fmt.Errorf("carries the unknown role %q", d.Role)
	}
	shape := DocumentShape(d)
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
		Topology:         NormalizeTopologyDocument(d.Topology),
		MeasuredReplicas: 1,
		Workload: Workload{
			// THE WORKLOAD IS NAMED AFTER THE SHAPE, because the document names
			// no workload of its own and a measurement is identified by what it
			// measured.
			ID: shape.String(),
			// THE DOCUMENT PINS NO WEIGHT COMMIT AND NO WORKLOAD REVISION,
			// because a public benchmark publishes a model name and a sweep.
			// FLAT CATALOG DOCUMENTS ONLY CARRY PROFILES QUALIFIED UNDER
			// ALL-MISS PROMPT ACCOUNTING (inputTokensPerSecond and
			// totalTokensPerSecond count cache-missed prompt tokens).
			Revision:      UnspecifiedRevision,
			Modality:      "text",
			CacheBehavior: CacheBehaviorAllMisses,
			ArrivalModel:  ClientModelFixedConcurrency,
		},
	}
	for _, point := range d.Points {
		converted, err := toPoint(point, role)
		if err != nil {
			return Profile{}, err
		}
		profile.Points = append(profile.Points, converted)
	}
	if len(profile.Points) >= 2 {
		highest := 0
		for i := 1; i < len(profile.Points); i++ {
			if profile.Points[i].Load.Value > profile.Points[highest].Load.Value {
				highest = i
			}
		}
		// THE HIGHEST CONCURRENCY POINT OF A MULTI-POINT SWEEP CAPS THE
		// FEASIBLE CURVE WHEN EVERY MEASURED POINT MEETS THE LATENCY TARGET.
		profile.Points[highest].Saturation = &Saturation{
			Measured: true,
			Note:     "highest concurrency point of the measured sweep",
		}
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
func toPoint(d PointDocument, role RoleKind) (MeasuredPoint, error) {
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
	ttftReadings, err := latencyReadings(LatencyMetricTTFT, d.TTFTMilliseconds)
	if err != nil {
		return MeasuredPoint{}, err
	}
	itlReadings, err := latencyReadings(LatencyMetricITL, d.ITLMilliseconds)
	if err != nil {
		return MeasuredPoint{}, err
	}
	point.Latencies = append(point.Latencies, ttftReadings...)
	point.Latencies = append(point.Latencies, itlReadings...)
	return point, nil
}

// latencyReadings turns the written quantiles of one metric into latency
// readings ordered by ascending quantile.
func latencyReadings(metric LatencyMetric, quantiles LatencyQuantiles) ([]Latency, error) {
	if len(quantiles) == 0 {
		return nil, nil
	}
	readings := make([]Latency, 0, len(quantiles))
	for key, value := range quantiles {
		level, err := parseQuantileKey(key)
		if err != nil {
			return nil, fmt.Errorf("%s latency quantile %q: %w", metric, key, err)
		}
		readings = append(readings, Latency{
			Metric:   metric,
			Quantile: level,
			Value:    value,
			Unit:     UnitMilliseconds,
		})
	}
	sort.Slice(readings, func(i, j int) bool {
		return readings[i].Quantile < readings[j].Quantile
	})
	for i := 1; i < len(readings); i++ {
		if readings[i].Quantile == readings[i-1].Quantile {
			return nil, fmt.Errorf("%s latency quantiles resolve to the same quantile %v", metric, readings[i].Quantile)
		}
	}
	return readings, nil
}

// parseQuantileKey turns a quantile key such as p25, p50, p70, p90, p95, p99,
// or p99.9 into its quantile level in (0, 1].
func parseQuantileKey(key string) (float64, error) {
	if !strings.HasPrefix(key, "p") {
		return 0, fmt.Errorf("quantile key %q must start with \"p\"", key)
	}
	rest := strings.TrimPrefix(key, "p")
	if rest == "" {
		return 0, fmt.Errorf("quantile key %q names no percentile", key)
	}
	percentile, err := strconv.ParseFloat(rest, 64)
	if err != nil || !isFinite(percentile) || percentile <= 0 || percentile > 100 {
		return 0, fmt.Errorf("quantile key %q must name a percentile in (0, 100]", key)
	}
	if canonical := strconv.FormatFloat(percentile, 'f', -1, 64); canonical != rest {
		return 0, fmt.Errorf("quantile key %q is not in canonical form %q", key, "p"+canonical)
	}
	rat, ok := new(big.Rat).SetString(rest)
	if !ok {
		return 0, fmt.Errorf("quantile key %q is not a decimal percentile", key)
	}
	rat.Mul(rat, big.NewRat(1, 100))
	level, _ := rat.Float64()
	return level, nil
}
