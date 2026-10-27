package priors

import (
	"testing"
)

// flatCatalog is a catalog in the format the operator ships.
const flatCatalog = `schemaVersion: gpustack.ai/v1
profiles:
  - role: Server
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points:
      - concurrency: 4
        totalTokensPerSecond: 3237.76
        inputTokensPerSecond: 2877.58
        outputTokensPerSecond: 360.17
        ttftMilliseconds:
          p50: 274.2075
          p90: 382.8469
          p99: 834.4765
        itlMilliseconds:
          p50: 9.8355
          p90: 9.9316
          p99: 11.0441
      - concurrency: 8
        totalTokensPerSecond: 5316.61
        inputTokensPerSecond: 4718.93
        outputTokensPerSecond: 597.68
        ttftMilliseconds:
          p50: 282.8146
          p90: 582.4434
          p99: 1597.9275
        itlMilliseconds:
          p50: 11.317
          p90: 11.5236
          p99: 80.7111
`

// TestParseCatalogReadsTheFlatCatalog asserts the catalog the operator ships
// is read into the profile the resolver reads.
//
// IT MATTERS BECAUSE THE FILE IS THE CATALOG, and a reader that reads it into
// something else resolves against something the file does not say.
func TestParseCatalogReadsTheFlatCatalog(t *testing.T) {
	catalog, err := ParseCatalog([]byte(flatCatalog))
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(catalog.Profiles))
	}
	profile := catalog.Profiles[0]

	want := Shape{
		Role:         RoleKindServer,
		Model:        "deepseek-ai/DeepSeek-R1-0528",
		Quantization: "fp8",
		Engine:       "vllm",
		Hardware:     "amd-mi325x",
		Topology: Topology{
			GPUsPerReplica:    8,
			TensorParallel:    8,
			PipelineParallel:  1,
			DataParallel:      1,
			LocalDataParallel: 1,
			ContextParallel:   1,
			ExpertParallel:    1,
		},
	}
	if profile.Shape() != want {
		t.Fatalf("the profile has the shape %q, want %q", profile.Shape(), want)
	}
	if profile.Workload.ID != profile.Shape().String() {
		t.Fatalf("the profile names the workload %q", profile.Workload.ID)
	}
	if profile.Role != RoleKindServer {
		t.Fatalf("the profile describes the %s role", profile.Role)
	}
	if profile.Identity.Model.Name != "deepseek-ai/DeepSeek-R1-0528" {
		t.Fatalf("the profile measures the model %q", profile.Identity.Model.Name)
	}
	if profile.Identity.Model.Quantization != "fp8" {
		t.Fatalf("the profile measures the recipe %q", profile.Identity.Model.Quantization)
	}
	if profile.Identity.Engine.Image != "vllm" {
		t.Fatalf("the profile measured the engine %q", profile.Identity.Engine.Image)
	}
	if profile.Identity.Hardware.Accelerator != "amd-mi325x" {
		t.Fatalf("the profile measured the accelerator %q", profile.Identity.Hardware.Accelerator)
	}
	if profile.Identity.Hardware.MemoryGB != 256 {
		t.Fatalf("the profile measured %d gibibytes", profile.Identity.Hardware.MemoryGB)
	}
	if profile.Topology.GPUsPerReplica != 8 ||
		profile.Topology.TensorParallel != 8 ||
		profile.Topology.ExpertParallel != 1 {
		t.Fatalf("the profile measured the topology %+v", profile.Topology)
	}
	if len(profile.Points) != 2 {
		t.Fatalf("the profile holds %d points, want 2", len(profile.Points))
	}
	if profile.Points[0].Throughput.Value != 3237.76 {
		t.Fatalf("the first point sustained %v tokens per second", profile.Points[0].Throughput.Value)
	}
	if profile.Points[0].Throughput.Metric != ThroughputMetricCacheMissedTotalTokensPerSecond {
		t.Fatalf("the first point counts %q", profile.Points[0].Throughput.Metric)
	}
}

// TestParseCatalogReadsTheJSONCatalog asserts a catalog written as JSON is
// read as well.
//
// IT MATTERS BECAUSE THE READER TAKES ITS BYTES FROM A CONFIGMAP, and a
// ConfigMap holds whatever the operator wrote into it.
func TestParseCatalogReadsTheJSONCatalog(t *testing.T) {
	const jsonCatalog = `{"schemaVersion":"gpustack.ai/v1","profiles":[` +
		`{"role":"Server","model":"org/model",` +
		`"quantization":"fp8","engine":"vllm","hardware":"amd-mi325x",` +
		`"topology":{"gpusPerReplica":8,"tensorParallel":8,"expertParallel":1},"points":[` +
		`{"concurrency":4,"totalTokensPerSecond":100,"inputTokensPerSecond":90,` +
		`"outputTokensPerSecond":10,"ttftMilliseconds":{"p95":100},"itlMilliseconds":{"p95":10}},` +
		`{"concurrency":8,"totalTokensPerSecond":200,"inputTokensPerSecond":180,` +
		`"outputTokensPerSecond":20,"ttftMilliseconds":{"p95":200},"itlMilliseconds":{"p95":20}}]}]}`

	catalog, err := ParseCatalog([]byte(jsonCatalog))
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(catalog.Profiles))
	}
	if catalog.Profiles[0].Identity.Model.Name != "org/model" {
		t.Fatalf("the profile measures the model %q", catalog.Profiles[0].Identity.Model.Name)
	}
	if catalog.Profiles[0].Topology.GPUsPerReplica != 8 ||
		catalog.Profiles[0].Topology.TensorParallel != 8 ||
		catalog.Profiles[0].Topology.ExpertParallel != 1 {
		t.Fatalf("the profile measured the topology %+v", catalog.Profiles[0].Topology)
	}
}

// TestParseCatalogRefusesAnUnknownField asserts the reader refuses a field it
// does not know.
//
// IT MATTERS BECAUSE A FIELD THE READER IGNORES IS A FIELD THE READER DID NOT
// CHECK, and a catalog nobody checked is a catalog nobody can trust.
func TestParseCatalogRefusesAnUnknownField(t *testing.T) {
	document := "schemaVersion: gpustack.ai/v1\nunknownField: 1\nprofiles: []\n"
	if _, err := ParseCatalog([]byte(document)); err == nil {
		t.Fatal("the catalog with an unknown field was read")
	}
}

// TestMarshalCatalogDocumentIsStable asserts a catalog written twice is the
// same bytes.
//
// IT MATTERS BECAUSE A CATALOG THAT CHANGES ITS BYTES ON EVERY RUN IS A FILE NO
// ONE CAN DIFF.
func TestMarshalCatalogDocumentIsStable(t *testing.T) {
	document, err := ParseCatalogDocument([]byte(flatCatalog))
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	first, err := MarshalCatalogDocument(document)
	if err != nil {
		t.Fatalf("write the catalog: %v", err)
	}
	second, err := MarshalCatalogDocument(document)
	if err != nil {
		t.Fatalf("write the catalog: %v", err)
	}
	if string(first) != string(second) {
		t.Fatal("two writes of one catalog differ")
	}
	if _, err := ParseCatalog(first); err != nil {
		t.Fatalf("the written catalog does not parse: %v", err)
	}
}

// TestMarshalCatalogDocumentSortsTheProfiles asserts the written catalog is in
// a stable order.
//
// IT MATTERS BECAUSE A CATALOG THAT IS WRITTEN IN THE ORDER THE MEASUREMENTS
// ARRIVED CHANGES ITS BYTES EVERY TIME A MEASUREMENT IS ADDED.
func TestMarshalCatalogDocumentSortsTheProfiles(t *testing.T) {
	document := CatalogDocument{
		SchemaVersion: SchemaVersion,
		Profiles: []ProfileDocument{
			{Role: "Server", Model: "org/b-model", Points: []PointDocument{{Concurrency: 8}, {Concurrency: 4}}},
			{Role: "Server", Model: "org/a-model"},
		},
	}
	encoded, err := MarshalCatalogDocument(document)
	if err != nil {
		t.Fatalf("write the catalog: %v", err)
	}
	written := string(encoded)
	first := indexOf(written, "org/a-model")
	second := indexOf(written, "org/b-model")
	if first < 0 || second < 0 || first > second {
		t.Fatalf("the profiles are not written in order:\n%s", written)
	}
	if indexOf(written, "concurrency: 4") > indexOf(written, "concurrency: 8") {
		t.Fatalf("the points are not written in order:\n%s", written)
	}
}

// indexOf returns where a substring appears, or -1.
func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// TestEmbeddedCatalogIsRead asserts the catalog the operator ships is read.
//
// IT MATTERS BECAUSE A CATALOG THAT DOES NOT PARSE REFUSES EVERY RESOLUTION, and
// a refusal nobody can explain is a refusal nobody can fix.
func TestEmbeddedCatalogIsRead(t *testing.T) {
	catalog, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("read the embedded catalog: %v", err)
	}
	if len(catalog.Profiles) == 0 {
		t.Fatal("the embedded catalog holds no profile")
	}
	for _, profile := range catalog.Profiles {
		if profile.Shape().Model == "" {
			t.Fatal("the embedded catalog holds a profile with no model")
		}
		if profile.Workload.ID != profile.Shape().String() {
			t.Fatalf("the embedded profile names the workload %q", profile.Workload.ID)
		}
		if refusal := CheckEligibility(profile); refusal != nil {
			t.Fatalf("the embedded profile %q cannot carry a capacity: %v", profile.Shape(), refusal)
		}
	}
}

// TestFlatCatalogCapsConservativelyAtHighestConcurrency asserts that when all
// measured points in a flat catalog profile satisfy a loose latency target,
// resolution caps conservatively at the highest measured concurrency point
// rather than extrapolating or refusing.
func TestFlatCatalogCapsConservativelyAtHighestConcurrency(t *testing.T) {
	catalog, err := ParseCatalog([]byte(flatCatalog))
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	profile := catalog.Profiles[0]
	resolution, err := catalog.Resolve(Request{
		Role:          profile.Role,
		Identity:      profile.Identity,
		ServingConfig: profile.ServingConfig,
		Topology:      profile.Topology,
		Constraints: Constraints{
			TTFTTargetMilliseconds: 5000,
			ITLTargetMilliseconds:  200,
			Headroom:               0.75,
			LatencyQuantile:        0.95,
		},
	})
	if err != nil {
		t.Fatalf("resolve under loose latency targets: %v", err)
	}
	if resolution.SeedTokensPerSecond != 5316.61 {
		t.Fatalf("seed tokens/s = %v, want 5316.61", resolution.SeedTokensPerSecond)
	}
	if resolution.Basis.Boundary != BoundaryExplicitSaturation {
		t.Fatalf("boundary = %q, want %q", resolution.Basis.Boundary, BoundaryExplicitSaturation)
	}
}
