package priors

import (
	"math"
	"strings"
	"testing"
)

// flatCatalogHeader starts the flat catalog document.
//
// IT NAMES THE SCHEMA THE READER EXPECTS, because a document that states no
// schema is a document the reader refuses.
const flatCatalogHeader = "apiVersion: worker.gpustack.ai/v1alpha1\nkind: ModelDeploymentScalerCapacityPriorCatalog\nprofiles:\n"

// flatProfileDocument is one measured profile in the flat catalog format.
//
// IT STATES THE FIELDS THE READER REQUIRES, because a document that leaves one
// out is a document the reader refuses.
const flatProfileDocument = `  - role: Server
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
          p95: 274.2075
        itlMilliseconds:
          p95: 9.8355
`

// flatProfileWithoutPointsDocument is one profile that measured nothing.
const flatProfileWithoutPointsDocument = `  - role: Server
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    topology:
      gpusPerReplica: 8
      tensorParallel: 8
      expertParallel: 1
    points: []
`

// flatProfileWithAnUnknownFieldDocument is one profile that carries a field
// the reader does not know.
const flatProfileWithAnUnknownFieldDocument = `  - role: Server
    model: deepseek-ai/DeepSeek-R1-0528
    quantization: fp8
    engine: vllm
    hardware: amd-mi325x
    unknownField: 1
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
          p95: 274.2075
        itlMilliseconds:
          p95: 9.8355
`

// TestParseCatalogRefusesBadCatalogs covers what the reader rejects.
//
// EACH CASE ASSERTS THE VERDICT, because a catalog that is accepted when it
// should not be would let an unmeasured value into a deployment.
func TestParseCatalogRefusesBadCatalogs(t *testing.T) {
	cases := []struct {
		name     string
		document string
		reason   string
	}{
		{
			name:     "an unsupported apiVersion",
			document: "apiVersion: worker.gpustack.ai/v2\nkind: ModelDeploymentScalerCapacityPriorCatalog\nprofiles: []\n",
			reason:   "apiVersion",
		},
		{
			name:     "an unsupported kind",
			document: "apiVersion: worker.gpustack.ai/v1alpha1\nkind: WrongKind\nprofiles: []\n",
			reason:   "kind",
		},
		{
			name:     "a profile with no measured point",
			document: flatCatalogHeader + flatProfileWithoutPointsDocument,
			reason:   "holds no measured load point",
		},
		{
			name:     "an unknown field inside a profile",
			document: flatCatalogHeader + flatProfileWithAnUnknownFieldDocument,
			reason:   "unknown field",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(testCase.document))
			if err == nil {
				t.Fatal("expected the catalog to be refused")
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Fatalf("the refusal %q does not say %q", err, testCase.reason)
			}
		})
	}
}

// TestParseCatalogReadsTheFlatProfile asserts the reader accepts the flat
// profile it has to accept.
//
// IT MATTERS BECAUSE A GATE THAT REFUSES EVERY CATALOG PROTECTS NOTHING, and
// the refusals beside it only mean something when the document they name is
// read.
func TestParseCatalogReadsTheFlatProfile(t *testing.T) {
	catalog, err := ParseCatalog([]byte(flatCatalogHeader + flatProfileDocument))
	if err != nil {
		t.Fatalf("parse the catalog: %v", err)
	}
	if len(catalog.Profiles) != 1 {
		t.Fatalf("the catalog holds %d profiles, want 1", len(catalog.Profiles))
	}
}

// TestParseCatalogRefusesTwoProfilesOfTheSameShape asserts the reader refuses
// a catalog that holds two profiles of one shape.
//
// IT MATTERS BECAUSE A CATALOG THAT HOLDS TWO MEASUREMENTS OF ONE SHAPE CANNOT
// SAY WHICH ONE APPLIES, and the refusal has to name both profiles so a reader
// can find them.
func TestParseCatalogRefusesTwoProfilesOfTheSameShape(t *testing.T) {
	document := flatCatalogHeader + flatProfileDocument + flatProfileDocument

	_, err := ParseCatalog([]byte(document))
	if err == nil {
		t.Fatal("expected the catalog to be refused")
	}
	if !strings.Contains(err.Error(), "profiles[0]") || !strings.Contains(err.Error(), "profiles[1]") {
		t.Fatalf("the refusal does not name both profiles: %v", err)
	}
	if !strings.Contains(err.Error(), "Server/deepseek-ai/DeepSeek-R1-0528/fp8/vllm/amd-mi325x/gpus=8,tp=8") {
		t.Fatalf("the refusal does not name the shape: %v", err)
	}
}

// TestValidateRefusesBadCatalogs covers what the catalog check rejects.
//
// EACH CASE ASSERTS THE VERDICT AND THE REASON, because a catalog that is
// accepted when it should not be would let an unmeasured value into a
// deployment.
func TestValidateRefusesBadCatalogs(t *testing.T) {
	unknownSource := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	unknownSource.SourceID = "source-that-does-not-exist"
	unknownSource = redigest(t, unknownSource)

	emptyIdentity := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	emptyIdentity.Identity.Model.Revision = ""
	emptyIdentity = redigest(t, emptyIdentity)

	wrongDigest := testCatalog(t, testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	}))
	wrongDigest.Digest = "sha256:0000000000000000000000000000000000000000000000000000000000000000"

	cases := []struct {
		name    string
		catalog *Catalog
		reason  string
	}{
		{
			name:    "a digest that does not match the content",
			catalog: wrongDigest,
			reason:  "does not match the content digest",
		},
		{
			name:    "a profile that names an unknown source",
			catalog: testCatalog(t, unknownSource),
			reason:  "names the unknown source",
		},
		{
			name:    "a profile with an empty identity field",
			catalog: testCatalog(t, emptyIdentity),
			reason:  "model revision is empty",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			err := Validate(*testCase.catalog)
			if err == nil {
				t.Fatal("expected the catalog to be refused")
			}
			if !strings.Contains(err.Error(), testCase.reason) {
				t.Fatalf("the refusal %q does not say %q", err, testCase.reason)
			}
		})
	}
}

// TestValidateRefusesANonFiniteMeasurement asserts a measurement that is not
// a finite number is refused.
//
// IT IS CHECKED ON THE CATALOG, because a value that is not finite cannot be
// carried by a measurement that a resolver would trust.
func TestValidateRefusesANonFiniteMeasurement(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	profile.Points[0].Throughput.Value = math.Inf(1)

	catalog := Catalog{
		SchemaVersion: SchemaVersion,
		Sources:       []Source{testSource()},
		Profiles:      []Profile{profile},
	}
	err := Validate(catalog)
	if err == nil {
		t.Fatal("expected the catalog to be refused")
	}
	if !strings.Contains(err.Error(), "throughput value") {
		t.Fatalf("the refusal %q does not name the measurement", err)
	}
}

// TestValidateRefusesANotANumberLatency asserts a latency that is not a
// number is refused.
func TestValidateRefusesANotANumberLatency(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	profile.Points[0].Latencies[0].Value = math.NaN()

	catalog := Catalog{
		SchemaVersion: SchemaVersion,
		Sources:       []Source{testSource()},
		Profiles:      []Profile{profile},
	}
	err := Validate(catalog)
	if err == nil {
		t.Fatal("expected the catalog to be refused")
	}
	if !strings.Contains(err.Error(), "latency value") {
		t.Fatalf("the refusal %q does not name the latency", err)
	}
}

// TestParseCatalogRefusesAnOversizedCatalog asserts the size bound holds.
func TestParseCatalogRefusesAnOversizedCatalog(t *testing.T) {
	data := make([]byte, MaxCatalogBytes+1)
	for i := range data {
		data[i] = ' '
	}

	if _, err := ParseCatalog(data); err == nil {
		t.Fatal("expected the catalog to be refused")
	}
}

// TestParseCatalogRefusesTrailingContent asserts a catalog file that carries
// content the reader cannot parse is refused.
func TestParseCatalogRefusesTrailingContent(t *testing.T) {
	document := flatCatalogHeader + flatProfileDocument + "this is not part of the document\n"

	if _, err := ParseCatalog([]byte(document)); err == nil {
		t.Fatal("expected the catalog to be refused")
	}
}

// TestParseCatalogIsStrict asserts the reader refuses content it cannot check.
//
// IT IS CHECKED ON THE RAW DOCUMENT, because a reader that accepted what it
// does not understand would let an unchecked field into a catalog the resolver
// trusts.
func TestParseCatalogIsStrict(t *testing.T) {
	cases := []struct {
		name     string
		document string
	}{
		{
			name:     "an unknown field inside a profile",
			document: flatCatalogHeader + flatProfileWithAnUnknownFieldDocument,
		},
		{
			name:     "a truncated catalog",
			document: "apiVersion: worker.gpustack.ai/v1alpha1\nkind: ModelDeploymentScalerCapacityPriorCatalog\nprofiles:\n  - role: Server\n    model: \"deepseek-ai/DeepSeek-R1-0528\n",
		},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			_, err := ParseCatalog([]byte(testCase.document))
			if err == nil {
				t.Fatal("expected the catalog to be refused")
			}
			if !strings.Contains(err.Error(), "decode catalog") {
				t.Fatalf("the refusal %q does not come from the reader", err)
			}
		})
	}
}

// TestCompareShapesOrdersTheSixElements asserts the order two shapes are
// written in.
//
// IT MATTERS BECAUSE A CATALOG THAT IS WRITTEN IN THE ORDER THE PROFILES WERE
// MEASURED CHANGES ITS BYTES EVERY TIME A MEASUREMENT IS ADDED, and a reader
// that diffs the file cannot tell a new measurement from a reordered one.
func TestCompareShapesOrdersTheSixElements(t *testing.T) {
	base := Shape{
		Role:         RoleKindServer,
		Model:        "org/model",
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

	smallerRole := base
	smallerRole.Role = RoleKindDecode

	smallerModel := base
	smallerModel.Model = "org/a-model"

	smallerQuantization := base
	smallerQuantization.Quantization = "fp4"

	smallerEngine := base
	smallerEngine.Engine = "sglang"

	smallerHardware := base
	smallerHardware.Hardware = "amd-mi300x"

	fewerGPUs := base
	fewerGPUs.Topology.GPUsPerReplica = 4

	smallerTensorParallel := base
	smallerTensorParallel.Topology.TensorParallel = 4

	cases := []struct {
		name  string
		left  Shape
		right Shape
		order string
	}{
		{name: "the same shape", left: base, right: base, order: "equal"},
		{name: "a smaller role comes first", left: smallerRole, right: base, order: "less"},
		{name: "a smaller model comes first", left: smallerModel, right: base, order: "less"},
		{name: "a smaller recipe comes first", left: smallerQuantization, right: base, order: "less"},
		{name: "a smaller engine comes first", left: smallerEngine, right: base, order: "less"},
		{name: "a smaller accelerator comes first", left: smallerHardware, right: base, order: "less"},
		{name: "fewer GPUs per replica come first", left: fewerGPUs, right: base, order: "less"},
		{name: "a smaller tensor parallel width comes first", left: smallerTensorParallel, right: base, order: "less"},
		{name: "the order follows the other shape", left: base, right: smallerModel, order: "greater"},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			got := CompareShapes(testCase.left, testCase.right)
			switch testCase.order {
			case "equal":
				if got != 0 {
					t.Fatalf("the shapes compare to %d, want 0", got)
				}
			case "less":
				if got >= 0 {
					t.Fatalf("the shapes compare to %d, want less than 0", got)
				}
			case "greater":
				if got <= 0 {
					t.Fatalf("the shapes compare to %d, want greater than 0", got)
				}
			default:
				t.Fatalf("the case names the unknown order %q", testCase.order)
			}
		})
	}
}

// TestProfileShapeIsStable asserts the shape of a profile depends on the
// identity and not on the measurements.
//
// IT MATTERS BECAUSE A PROFILE KEEPS ITS SHAPE WHEN NEW MEASUREMENTS ARE ADDED
// TO IT, and a shape that moved would make the profile a second profile.
func TestProfileShapeIsStable(t *testing.T) {
	first := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	second := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
		serverPoint(16, 1300, 9000, 20),
	})

	if first.Shape().String() != second.Shape().String() {
		t.Fatalf("the shapes %q and %q differ for the same identity", first.Shape(), second.Shape())
	}
	if first.Digest == second.Digest {
		t.Fatal("the digests match for different measurements")
	}
}

// TestProfileShapeChangesWithTheIdentity asserts a different identity derives
// a different shape.
//
// IT MATTERS BECAUSE THE SHAPE IS WHAT A PROFILE IS KNOWN BY, and two profiles
// of one identity are one profile.
func TestProfileShapeChangesWithTheIdentity(t *testing.T) {
	first := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	other := first
	other.Identity.Model.Quantization = "fp8"
	other = redigest(t, other)

	if first.Shape().String() == other.Shape().String() {
		t.Fatalf("the shape %q is the same for two identities", first.Shape())
	}
	if first.Shape().Quantization == other.Shape().Quantization {
		t.Fatalf("the shapes %q and %q carry the same recipe", first.Shape(), other.Shape())
	}
}

// TestDigestIsStable asserts the content digest does not depend on the order
// the fields are written in.
func TestDigestIsStable(t *testing.T) {
	first, err := Digest(map[string]any{"a": 1, "b": "two"})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	second, err := Digest(map[string]any{"b": "two", "a": 1})
	if err != nil {
		t.Fatalf("digest: %v", err)
	}
	if first != second {
		t.Fatalf("the digests %q and %q differ for the same content", first, second)
	}
}

// TestEmbeddedCatalogIsValid asserts the catalog shipped with the operator
// can be read.
//
// IT MAY HOLD NO PROFILES, because a profile only enters the catalog when a
// source measured the evidence the resolver needs.
func TestEmbeddedCatalogIsValid(t *testing.T) {
	catalog, err := LoadEmbedded()
	if err != nil {
		t.Fatalf("load embedded catalog: %v", err)
	}
	if catalog.SchemaVersion != SchemaVersion {
		t.Fatalf("the embedded catalog carries schema version %q", catalog.SchemaVersion)
	}
	for _, profile := range catalog.Profiles {
		if profile.Shape().Model == "" {
			t.Fatal("the embedded catalog holds a profile whose shape names no model")
		}
	}
}

// TestValidateRefusesASourceWithoutItsReason asserts a source that was
// refused is recorded with its reason, so an empty catalog still says why.
func TestValidateRefusesASourceWithoutItsReason(t *testing.T) {
	catalog := testCatalog(t, testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	}))
	catalog.Sources[0].Outcome = SourceOutcomeRefused
	catalog.Sources[0].RefusalError = nil
	reseal(t, catalog)

	err := Validate(*catalog)
	if err == nil {
		t.Fatal("expected the catalog to be refused")
	}
	if !strings.Contains(err.Error(), "carries no reason") {
		t.Fatalf("the refusal %q does not name the reason", err)
	}
}

// TestValidateRefusesAMixedLoadAxis asserts a profile whose load points do
// not vary one comparable load model is refused, because a concurrency count
// and a request rate are not comparable numbers.
func TestValidateRefusesAMixedLoadAxis(t *testing.T) {
	mixedKind := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		ratePoint(10, 1200, 500, 20),
	})

	mixedClient := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	mixedClient.Points[1].Load.ClientModel = ClientModelOpenLoop
	mixedClient = redigest(t, mixedClient)

	mixedUnit := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	mixedUnit.Points[1].Load.Unit = "queries/second"
	mixedUnit = redigest(t, mixedUnit)

	workloadMismatch := testProfile(t, RoleKindServer, []MeasuredPoint{
		ratePoint(5, 1000, 500, 20),
		ratePoint(10, 1200, 500, 20),
	})

	cases := []struct {
		name    string
		profile Profile
	}{
		{name: "mixed load kinds", profile: mixedKind},
		{name: "mixed client models", profile: mixedClient},
		{name: "mixed load units", profile: mixedUnit},
		{name: "a load model the workload does not declare", profile: workloadMismatch},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := testCatalog(t, testCase.profile)
			if err := Validate(*catalog); err == nil {
				t.Fatal("expected the catalog to be refused")
			}
		})
	}
}

// TestValidateRefusesAnUnknownWorkloadEnum asserts the declared workload
// enums are checked, because a value no reader knows cannot be honored.
func TestValidateRefusesAnUnknownWorkloadEnum(t *testing.T) {
	unknownCache := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	unknownCache.Workload.CacheBehavior = CacheBehavior("Sometimes")
	unknownCache = redigest(t, unknownCache)

	unknownArrival := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	unknownArrival.Workload.ArrivalModel = ClientModel("Sometimes")
	unknownArrival = redigest(t, unknownArrival)

	cases := []struct {
		name    string
		profile Profile
	}{
		{name: "an unknown cache behavior", profile: unknownCache},
		{name: "an unknown arrival model", profile: unknownArrival},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			catalog := testCatalog(t, testCase.profile)
			if err := Validate(*catalog); err == nil {
				t.Fatal("expected the catalog to be refused")
			}
		})
	}
}

// TestValidateRefusesAMissingRequiredTopologyWidth asserts the tensor,
// pipeline, data and local data parallel widths are positive, because a
// width the source did not state must not match a deployment by being zero
// on both sides.
func TestValidateRefusesAMissingRequiredTopologyWidth(t *testing.T) {
	cases := []struct {
		name   string
		widths func(profile *Profile)
	}{
		{name: "tensor parallel", widths: func(profile *Profile) { profile.Topology.TensorParallel = 0 }},
		{name: "pipeline parallel", widths: func(profile *Profile) { profile.Topology.PipelineParallel = 0 }},
		{name: "data parallel", widths: func(profile *Profile) { profile.Topology.DataParallel = 0 }},
		{name: "local data parallel", widths: func(profile *Profile) { profile.Topology.LocalDataParallel = 0 }},
	}

	for _, testCase := range cases {
		t.Run(testCase.name, func(t *testing.T) {
			profile := testProfile(t, RoleKindServer, []MeasuredPoint{
				serverPoint(4, 1000, 500, 20),
				serverPoint(8, 1200, 5000, 20),
			})
			testCase.widths(&profile)
			profile = redigest(t, profile)

			catalog := testCatalog(t, profile)
			if err := Validate(*catalog); err == nil {
				t.Fatal("expected the catalog to be refused")
			}
		})
	}
}

// TestValidateAcceptsAnExplicitlyUnusedParallelWidth asserts context and
// expert parallel may be zero, because a system that does not use them states
// so with a zero width and those two fields are documented that way.
func TestValidateAcceptsAnExplicitlyUnusedParallelWidth(t *testing.T) {
	profile := testProfile(t, RoleKindServer, []MeasuredPoint{
		serverPoint(4, 1000, 500, 20),
		serverPoint(8, 1200, 5000, 20),
	})
	profile.Topology.ContextParallel = 0
	profile.Topology.ExpertParallel = 0
	profile = redigest(t, profile)

	catalog := testCatalog(t, profile)
	if err := Validate(*catalog); err != nil {
		t.Fatalf("validate: %v", err)
	}
}
