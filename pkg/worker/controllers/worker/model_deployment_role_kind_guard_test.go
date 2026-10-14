package worker

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// crdSpecRoleKindValues returns the enum values the CRD schema lists for
// spec.roles[].kind, as plain strings, so the guard can compare them with the kinds it tests.
func crdSpecRoleKindValues(t *testing.T) []string {
	t.Helper()

	crd := workercore.GetCustomResourceDefinitions()["ModelDeployment"]
	require.NotNil(t, crd, "ModelDeployment is not registered")
	require.Len(t, crd.Spec.Versions, 1)

	schema := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	for _, part := range []string{"spec", "roles", "[]", "kind"} {
		if part == "[]" {
			require.NotNil(t, schema.Items)
			require.NotNil(t, schema.Items.Schema)
			schema = schema.Items.Schema
			continue
		}
		child, ok := schema.Properties[part]
		require.True(t, ok, "the schema has no %q", part)
		schema = &child
	}

	values := make([]string, 0, len(schema.Enum))
	for _, entry := range schema.Enum {
		raw := string(entry.Raw)
		// The schema stores enum entries as JSON raw strings, so the raw bytes carry the literal
		// double quotes the schema rendered. The guard compares the values with the typed kinds
		// it iterates over, which never carry quotes, so the quotes are stripped here.
		if len(raw) >= 2 && raw[0] == '"' && raw[len(raw)-1] == '"' {
			raw = raw[1 : len(raw)-1]
		}
		values = append(values, raw)
	}

	return values
}

// crdEnumAsKinds reads the spec.roles[].kind enum from the registered CRD and turns it into the
// typed values the guard iterates over. The zero value is added separately, because the schema
// would never name it (it is the default, not an enum entry) and a Go caller building a value in
// process hits it constantly.
func crdEnumAsKinds(t *testing.T) []workercore.ModelDeploymentRoleKind {
	t.Helper()

	values := crdSpecRoleKindValues(t)
	kinds := make([]workercore.ModelDeploymentRoleKind, 0, len(values)+1)
	kinds = append(kinds, "")
	for _, value := range values {
		kinds = append(kinds, workercore.ModelDeploymentRoleKind(value))
	}

	return kinds
}

// roleKindExpectations is the per-kind answer every kind-dependent function must give.
//
// THE TABLE IS THE SOURCE OF TRUTH: changing a function's classification for a kind is a
// deliberate act that updates this table, not a discovery that passes through it. Each row reads
// FAIL on the sub-test whose answer disagrees, and the same value in two places -- table and
// function -- is the contract.
type roleKindExpectations struct {
	Kind workercore.ModelDeploymentRoleKind

	// inject maps the API kind onto the renderer's role. The dedicated value, not RoleNone, is
	// the contract: a fallback would silently render the encoder as a shared-cache participant.
	InjectRole inject.Role
	InjectOK   bool

	// Upstream is the spelling the role-label filter and the per-role Service selector match on.
	Upstream   string
	UpstreamOK bool

	// PairPrefill records whether a role of this kind counts as the pair's prefill half.
	PairPrefill bool
	// PairDecode records whether a role of this kind counts as the pair's decode half.
	PairDecode bool

	// UsesKVTransfer records whether the role runs the engine-side KV leg under a routed pair.
	UsesKVTransfer bool
	// RoutesViaSidecar records whether the role fronts the routing proxy.
	RoutesViaSidecar bool
	// PublishesKVEvents records whether the role runs the event publisher. Decode consumes and
	// Encode produces embeddings; neither is a KV producer.
	PublishesKVEvents bool

	// EngineSupportVLLM records whether the vLLM renderer has a term for this kind. Encode answers
	// true there: the renderer takes an encoder-cache leg and reads the kind off it.
	EngineSupportVLLM bool
	// EngineSupportSGLang records the same answer for the SGLang renderer, kept on the same row
	// because the answer tracks the renderer not the engine. Encode answers true there too, though
	// that engine reaches the encoder stage by a different term: it splits an encoder off its own
	// server rather than through a connector.
	EngineSupportSGLang bool
}

// expectedRoleKinds is the table the guard asserts against. Each row is the deliberate answer
// for one kind; every kind-dependent function reads its answer from this row.
//
// ORDER: the zero value first, then the spec-declared values in CRD order. The test framework
// runs sub-tests in declaration order, and the order matters only for diagnostic readability.
var expectedRoleKinds = []roleKindExpectations{
	{
		Kind:       "",
		InjectRole: inject.RoleNone, InjectOK: true,
		Upstream: "", UpstreamOK: false,
		PairPrefill: false, PairDecode: false,
		UsesKVTransfer: false, RoutesViaSidecar: false, PublishesKVEvents: true,
		EngineSupportVLLM: true, EngineSupportSGLang: true,
	},
	{
		Kind:       workercore.ModelDeploymentRoleKindServer,
		InjectRole: inject.RoleNone, InjectOK: true,
		Upstream: "server", UpstreamOK: true,
		PairPrefill: false, PairDecode: false,
		UsesKVTransfer: false, RoutesViaSidecar: false, PublishesKVEvents: true,
		EngineSupportVLLM: true, EngineSupportSGLang: true,
	},
	{
		Kind:       workercore.ModelDeploymentRoleKindPrefill,
		InjectRole: inject.RolePrefill, InjectOK: true,
		Upstream: "prefill", UpstreamOK: true,
		PairPrefill: true, PairDecode: false,
		UsesKVTransfer: true, RoutesViaSidecar: false, PublishesKVEvents: true,
		EngineSupportVLLM: true, EngineSupportSGLang: true,
	},
	{
		Kind:       workercore.ModelDeploymentRoleKindDecode,
		InjectRole: inject.RoleDecode, InjectOK: true,
		Upstream: "decode", UpstreamOK: true,
		PairPrefill: false, PairDecode: true,
		UsesKVTransfer: true, RoutesViaSidecar: true, PublishesKVEvents: false,
		EngineSupportVLLM: true, EngineSupportSGLang: true,
	},
	{
		Kind:       workercore.ModelDeploymentRoleKindEncode,
		InjectRole: inject.RoleEncode, InjectOK: true,
		Upstream: "encode", UpstreamOK: true,
		PairPrefill: false, PairDecode: false,
		UsesKVTransfer: false, RoutesViaSidecar: false, PublishesKVEvents: false,
		// BOTH engines answer true for this kind, which is what the row now records: the encoder
		// stage reached each renderer and each gave it a term, though not the same one. SGLang
		// splits an encoder off its own server with --encoder-only and is handed the encoder's
		// addresses; the vLLM family describes the same leg with a connector configured through
		// --ec-transfer-config. An entry means the RENDERER has a term for the kind, and which term
		// is the renderer's business, not the guard's.
		EngineSupportVLLM: true, EngineSupportSGLang: true,
	},
}

// expectationFor finds the expectation row that names the kind, asserting it exists. A new kind
// added without a row fails here on the first sub-test rather than running with a zero value
// and silently passing.
func expectationFor(t *testing.T, kind workercore.ModelDeploymentRoleKind) roleKindExpectations {
	t.Helper()

	for _, row := range expectedRoleKinds {
		if row.Kind == kind {
			return row
		}
	}
	t.Fatalf("no expectation row for kind %q; add one to expectedRoleKinds", kind)

	return roleKindExpectations{}
}

// TestModelDeploymentRoleKindGuardEnumTracksCRD pins the guard's per-kind rows to the CRD
// schema. Drop "Encode" from the CRD and the tripwire fails: the guard sees a kind the table
// does not name, and it reports the dropped value by name.
func TestModelDeploymentRoleKindGuardEnumTracksCRD(t *testing.T) {
	crdValues := crdSpecRoleKindValues(t)
	require.Equal(t, []string{"Server", "Prefill", "Decode", "Encode"}, crdValues,
		"the spec kind enum carries the values the guard treats as exhaustive")
}

// TestModelDeploymentRoleKindGuard asserts every kind-dependent function classifies every kind
// deliberately. A kind that misses a site silently inherits the wrong default branch -- usually
// "treat as server" or "count as KV producer" -- and the silent result is what the guard exists
// to catch.
//
// THE PREDICATES BELOW read in three groups. The first three are the vocabulary pieces every
// other site reads through. The next two are the pair rules: which kind counts as a half and
// which kind is half of the pair at all. The last three are the side effects: KV event
// publishing, the engine support table, and the controller's per-role connector synthesis.
func TestModelDeploymentRoleKindGuard(t *testing.T) {
	kinds := crdEnumAsKinds(t)

	t.Run("inject_role_mapping", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				got, ok := modelDeploymentInjectRole(kind)
				assert.Equal(t, want.InjectOK, ok,
					"the inject mapping's ok return must match the expectation row")
				assert.Equal(t, want.InjectRole, got,
					"the inject mapping's role must match the expectation row")
			})
		}
	})

	t.Run("upstream_name_mapping", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				name, ok := ModelDeploymentRoleUpstreamKind(kind)
				assert.Equal(t, want.UpstreamOK, ok,
					"the upstream mapping's ok return must match the expectation row")
				assert.Equal(t, want.Upstream, name,
					"the upstream mapping's spelling must match the expectation row")
			})
		}
	})

	t.Run("effective_role_kind", func(t *testing.T) {
		for _, kind := range kinds {
			t.Run(string(kind), func(t *testing.T) {
				got := ModelDeploymentEffectiveRoleKind(&workercore.ModelDeploymentRole{Kind: kind})
				if kind == "" {
					assert.Equal(t, workercore.ModelDeploymentRoleKindServer, got,
						"an unset kind resolves to Server so every site reads a named value")
				} else {
					assert.Equal(t, kind, got,
						"a named kind round-trips through the resolver unchanged")
				}
			})
		}
	})

	t.Run("pair_rule", func(t *testing.T) {
		// A one-role deployment declares one half at most. The expectation row drives the answer:
		// only Prefill and Decode count as a half, and only their combination declares both.
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run("one_role/"+string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{{Name: "only", Kind: kind}},
					},
				}
				assert.False(t, ModelDeploymentDeclaresBothHalves(md),
					"a one-role deployment declares %q and never both halves", kind)
			})
			t.Run("with_prefill/"+string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{
							{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill},
							{Name: "x", Kind: kind},
						},
					},
				}
				got := ModelDeploymentDeclaresBothHalves(md)
				wantPair := want.PairDecode
				assert.Equal(t, wantPair, got,
					"the pair is complete iff %q beside Prefill supplies the Decode half", kind)
			})
			t.Run("with_decode/"+string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{
							{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode},
							{Name: "x", Kind: kind},
						},
					},
				}
				got := ModelDeploymentDeclaresBothHalves(md)
				wantPair := want.PairPrefill
				assert.Equal(t, wantPair, got,
					"the pair is complete iff %q beside Decode supplies the Prefill half", kind)
			})
			t.Run("with_pair/"+string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{
							{Name: "encode", Kind: workercore.ModelDeploymentRoleKindEncode},
							{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill},
							{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode},
							{Name: "x", Kind: kind},
						},
					},
				}
				got := ModelDeploymentDeclaresBothHalves(md)
				assert.True(t, got,
					"the pair is complete once Prefill and Decode are both present, regardless of %q", kind)
			})
		}
	})

	t.Run("uses_kv_transfer", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Router: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
						Roles: []workercore.ModelDeploymentRole{
							{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill},
							{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode},
							{Name: "x", Kind: kind},
						},
					},
				}
				got := modelDeploymentUsesKVTransfer(md, &md.Spec.Roles[2], nodefeature.ManufacturerNVIDIA)
				assert.Equal(t, want.UsesKVTransfer, got,
					"the transfer leg must match the expectation row")
			})
		}
	})

	t.Run("fronts_decode_with_sidecar", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Router: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
						Roles:  []workercore.ModelDeploymentRole{{Name: "x", Kind: kind}},
					},
				}
				got := modelDeploymentFrontsDecodeWithSidecar(md, &md.Spec.Roles[0])
				assert.Equal(t, want.RoutesViaSidecar, got,
					"the sidecar-fronting answer must match the expectation row")
			})
		}
	})

	t.Run("publishes_kv_events", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				md := &workercore.ModelDeployment{
					Spec: workercore.ModelDeploymentSpec{
						Router: &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD},
						Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
						Roles:  []workercore.ModelDeploymentRole{{Name: "x", Kind: kind}},
					},
				}
				got := modelDeploymentPublishesKVEvents(md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA)
				assert.Equal(t, want.PublishesKVEvents, got,
					"the KV event publisher answer must match the expectation row")
			})
		}
	})

	t.Run("engine_support", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run("vLLM/"+string(kind), func(t *testing.T) {
				got := ModelDeploymentSupportsRoleKind(workercore.ModelDeploymentEngineVLLM, kind)
				assert.Equal(t, want.EngineSupportVLLM, got,
					"vLLM's renderer answer must match the expectation row")
			})
			t.Run("SGLang/"+string(kind), func(t *testing.T) {
				got := ModelDeploymentSupportsRoleKind(workercore.ModelDeploymentEngineSGLang, kind)
				assert.Equal(t, want.EngineSupportSGLang, got,
					"SGLang's renderer answer must match the expectation row")
			})
		}
	})

	t.Run("controller_skips_kv_synthesis", func(t *testing.T) {
		for _, kind := range kinds {
			want := expectationFor(t, kind)
			t.Run(string(kind), func(t *testing.T) {
				md := newRenderDeployment(func(md *workercore.ModelDeployment) {
					md.Spec.Roles = []workercore.ModelDeploymentRole{{
						Name:         "x",
						Kind:         kind,
						Replicas:     1,
						InstanceType: "h20-8x",
						Image:        "vllm/vllm-openai:v0.25.1",
					}}
					md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
				})
				cli := newModelDeploymentClient(md, newRenderInstanceType(),
					newRenderBinding(), newRenderPool(), newRenderBackend())

				_, err := reconcileModelDeployment(t, cli)
				require.NoError(t, err)

				pods := replicaPods(t, cli)
				require.Len(t, pods, 1)

				hasTransferArg := false
				hasConfigEnv := false
				hasAnnotation := false
				for _, arg := range pods[0].Spec.Containers[0].Command {
					if arg == "--kv-transfer-config" {
						hasTransferArg = true
					}
				}
				for _, env := range pods[0].Spec.Containers[0].Env {
					if env.Name == "MOONCAKE_CONFIG_PATH" {
						hasConfigEnv = true
					}
				}
				_, hasAnnotation = pods[0].Annotations[inject.ClientConfigAnnotationKey]

				carries := hasTransferArg || hasConfigEnv || hasAnnotation

				// The expectation row drives the answer: a kind whose UsesKVTransfer, PublishesKVEvents
				// or RoutesViaSidecar is true must carry at least one piece of the connector; a kind
				// whose every side effect is false must carry none.
				needConnector := want.UsesKVTransfer || want.PublishesKVEvents || want.RoutesViaSidecar
				if needConnector {
					assert.True(t, carries,
						"kind %q must still receive at least one piece of the KV connector", kind)
				} else {
					assert.False(t, carries,
						"the KV connector must not render for kind %q", kind)
				}
			})
		}
	})
}

// TestModelDeploymentParallelismPair_DropsEncodeDegrees pins the parser-side rule that the
// parallel shape comes from the pair halves alone.
//
// A deployment that lists Encode first with explicit degree flags must NOT have those degrees
// land in pair.Prefill or pair.Decode: the parser walks roles in declaration order but only
// prefill/decode halves contribute to the pair, and Encode's degrees vanish into the same
// default of one that an undeclared role sees.
func TestModelDeploymentParallelismPair_DropsEncodeDegrees(t *testing.T) {
	md := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
			Roles: []workercore.ModelDeploymentRole{
				{
					Name: "encode",
					Kind: workercore.ModelDeploymentRoleKindEncode,
					ExtraArgs: []string{
						"--tensor-parallel-size", "4",
						"--data-parallel-size", "3",
					},
				},
				{
					Name:      "prefill",
					Kind:      workercore.ModelDeploymentRoleKindPrefill,
					ExtraArgs: []string{"--tensor-parallel-size", "2"},
				},
				{
					Name:      "decode",
					Kind:      workercore.ModelDeploymentRoleKindDecode,
					ExtraArgs: []string{"--tensor-parallel-size", "8"},
				},
			},
		},
	}

	pair, err := modelDeploymentDeclaredParallelismPair(md)
	require.NoError(t, err)

	assert.Equal(t, 2, pair.Prefill.TensorParallel,
		"Encode's tensor-parallel degree must not leak into the pair's prefill half")
	assert.Equal(t, 1, pair.Prefill.DataParallel,
		"Encode's data-parallel degree must not leak into the pair's prefill half")
	assert.Equal(t, 8, pair.Decode.TensorParallel,
		"Encode's tensor-parallel degree must not leak into the pair's decode half")
	assert.Equal(t, 1, pair.Decode.DataParallel,
		"Encode's data-parallel degree must not leak into the pair's decode half")
}

// TestEncodeProducesNoKVEvents is the controller-level proof: a deployment with one Encode role
// under the managed llm-d router on vLLM reconciles to a Pod that carries none of the KV event
// publisher's surface area.
//
// It is a separate test rather than a row in the guard because it exercises the per-role
// synthesis loop end-to-end and asserts on Pod bytes, not on a single function's return. The
// guard's `controller_skips_kv_synthesis` row asserts the same thing through the function table
// and exists as a fast tripwire; this one exists so a regression in the reconciler cannot hide
// behind a fixture change.
func TestEncodeProducesNoKVEvents(t *testing.T) {
	md := newRenderDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles = []workercore.ModelDeploymentRole{{
			Name:         "encoder",
			Kind:         workercore.ModelDeploymentRoleKindEncode,
			Replicas:     1,
			InstanceType: "h20-8x",
			Image:        "vllm/vllm-openai:v0.25.1",
		}}
		md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	pods := replicaPods(t, cli)
	require.Len(t, pods, 1, "an Encode role reconciles to one replica Pod")

	container := pods[0].Spec.Containers[0]
	for _, arg := range container.Command {
		assert.NotEqual(t, "--kv-transfer-config", arg,
			"the KV transfer flag must not render on an Encode Pod")
		assert.NotEqual(t, "--kv-events-config", arg,
			"the KV event publisher flag must not render on an Encode Pod")
	}
	for _, env := range container.Env {
		assert.NotEqual(t, "MOONCAKE_CONFIG_PATH", env.Name,
			"the KV config file must not mount on an Encode Pod")
	}
	_, hasAnnotation := pods[0].Annotations[inject.ClientConfigAnnotationKey]
	assert.False(t, hasAnnotation,
		"the KV client config annotation must not be projected on an Encode Pod")

	// Sanity: the role-kind label is what selectors match on, and Encode Pods carry the spelling
	// the upstream-kind mapping promises. Without this assertion the test would pass even if the
	// selector-spelling change silently regressed.
	assert.Equal(t, "encode", pods[0].Labels[modelDeploymentLabelKeyRoleKind],
		"the role-kind label must carry the spelling selectors match on")
}

// TestRoleKind_ExpectationTable_Exhaustive pins that every kind the CRD declares has an
// expectation row. A new kind added without a row fails here rather than at the first sub-test
// that calls expectationFor.
func TestRoleKind_ExpectationTable_Exhaustive(t *testing.T) {
	crdValues := crdSpecRoleKindValues(t)

	for _, value := range crdValues {
		kind := workercore.ModelDeploymentRoleKind(value)
		t.Run("row_for_"+string(kind), func(t *testing.T) {
			row := expectationFor(t, kind)
			assert.Equal(t, kind, row.Kind, "the row's kind must match what was looked up")
		})
	}
}
