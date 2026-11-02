package v1

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apiextensions "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions"
	apiextensionsv1 "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/v1"
	apiextensionsvalidation "k8s.io/apiextensions-apiserver/pkg/apis/apiextensions/validation"
	apischema "k8s.io/apiextensions-apiserver/pkg/apiserver/schema"
	celvalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/cel"
	pruning "k8s.io/apiextensions-apiserver/pkg/apiserver/schema/pruning"
	apiservalidation "k8s.io/apiextensions-apiserver/pkg/apiserver/validation"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	jsonserializer "k8s.io/apimachinery/pkg/runtime/serializer/json"
	"k8s.io/apimachinery/pkg/runtime/serializer/protobuf"
	"k8s.io/apimachinery/pkg/util/validation/field"
	celconfig "k8s.io/apiserver/pkg/apis/cel"
	k8sapplymeta "k8s.io/client-go/applyconfigurations/meta/v1"
	smdfield "sigs.k8s.io/structured-merge-diff/v6/fieldpath"
	smdmerge "sigs.k8s.io/structured-merge-diff/v6/merge"
	smdtyped "sigs.k8s.io/structured-merge-diff/v6/typed"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	applyv1alpha1 "gpustack.ai/gpustack/pkg/kubeclients/applyconfiguration/worker/v1alpha1"
)

// These tests cover the wire contract for the selector and the two atomic HCA lists.

type rdmaSelState struct {
	selectorSet    bool
	includePresent bool
	names          []string
	exclude        []string
}

func rdmaSelStateOfPublic(p *DevicesRDMASelectionPolicy) rdmaSelState {
	return rdmaSelStateFrom(p.Spec.NodeSelector, p.Spec.Include, p.Spec.Exclude)
}

func rdmaSelStateOfInternal(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
	return rdmaSelStateFrom(p.Spec.NodeSelector, p.Spec.Include, p.Spec.Exclude)
}

func rdmaSelStateFrom(
	selector *meta.LabelSelector,
	include []string,
	exclude []string,
) rdmaSelState {
	return rdmaSelState{selectorSet: selector != nil, includePresent: len(include) > 0, names: include, exclude: exclude}
}

func (s rdmaSelState) assertEqual(t *testing.T, want rdmaSelState, label string) {
	t.Helper()
	assert.Equal(t, want.selectorSet, s.selectorSet, "%s: nodeSelector presence", label)
	assert.Equal(t, want.includePresent, s.includePresent, "%s: include presence is the restriction switch", label)
	assert.Equal(t, rdmaSelStringList(want.names), rdmaSelStringList(s.names), "%s: include names (nil equals empty)", label)
	assert.Equal(t, rdmaSelStringList(want.exclude), rdmaSelStringList(s.exclude), "%s: exclude (nil equals empty)", label)
}

// rdmaSelStringList normalizes nil and empty slices for wire comparisons.
func rdmaSelStringList(s []string) []string {
	if len(s) == 0 {
		return []string{}
	}
	return s
}

type rdmaWireCase struct {
	name string
	spec string
	want rdmaSelState
	// wantCRDOk is the structural schema's verdict; wantCELOk is the schema's
	// x-kubernetes-validations verdict on top of it. The CEL rules only ever add
	// rejections, so a case the structural schema refuses can still be CEL-accepted:
	// an empty list already failed minItems, and the list rules judge only the entries.
	wantCRDOk bool
	wantCELOk bool
	crdNote   string
}

func rdmaWireCases() []rdmaWireCase {
	return []rdmaWireCase{
		{
			name:      "selector-missing-is-invalid-state",
			spec:      `{}`,
			want:      rdmaSelState{selectorSet: false},
			wantCRDOk: false,
			wantCELOk: true,
		},
		{
			name:      "selector-null-is-invalid-state",
			spec:      `{"nodeSelector":null}`,
			want:      rdmaSelState{selectorSet: false},
			wantCRDOk: true,
			wantCELOk: true,
			crdNote:   "the generated schema keeps pointers nullable, so the schema accepts null; the webhook layer must reject a nil selector",
		},
		{
			name:      "selector-empty-matches-every-node",
			spec:      `{"nodeSelector":{}}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name:      "include-absent-is-unrestricted",
			spec:      `{"nodeSelector":{}}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name:      "include-null-is-unrestricted",
			spec:      `{"nodeSelector":{},"include":null}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name:      "include-empty-rejected-by-schema",
			spec:      `{"nodeSelector":{},"include":[]}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: false,
			// An empty list fails minItems before any rule runs, and the list rules
			// judge only the entries, so CEL never sees a violation here.
			wantCELOk: true,
		},
		{
			name:      "include-set-selects-named",
			spec:      `{"nodeSelector":{},"include":["mlx5_4","mlx5_5"]}`,
			want:      rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_4", "mlx5_5"}},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name:      "exclude-null-excludes-none",
			spec:      `{"nodeSelector":{},"exclude":null}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name:      "exclude-empty-rejected-by-schema",
			spec:      `{"nodeSelector":{},"exclude":[]}`,
			want:      rdmaSelState{selectorSet: true},
			wantCRDOk: false,
			wantCELOk: true,
		},
		{
			name:      "exclude-set-excludes-named",
			spec:      `{"nodeSelector":{},"exclude":["mlx5_5"]}`,
			want:      rdmaSelState{selectorSet: true, exclude: []string{"mlx5_5"}},
			wantCRDOk: true,
			wantCELOk: true,
		},
		{
			name: "include-and-exclude-mutually-exclusive-rejected",
			spec: `{"nodeSelector":{},"include":["mlx5_4"],"exclude":["mlx5_5"]}`,
			want: rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_4"}, exclude: []string{"mlx5_5"}},
			// The structural schema alone carries no exclusivity, which is why the
			// webhook and the resolver each re-check it; the schema-level rule is
			// what now refuses the shape at admission.
			wantCRDOk: true,
			wantCELOk: false,
		},
		{
			name:      "include-empty-item-rejected-by-rule",
			spec:      `{"nodeSelector":{},"include":[""]}`,
			want:      rdmaSelState{selectorSet: true, includePresent: true, names: []string{""}},
			wantCRDOk: true,
			wantCELOk: false,
		},
		{
			name:      "exclude-empty-item-rejected-by-rule",
			spec:      `{"nodeSelector":{},"exclude":[""]}`,
			want:      rdmaSelState{selectorSet: true, exclude: []string{""}},
			wantCRDOk: true,
			wantCELOk: false,
		},
		{
			// The rule bounds each name because the registration cost budget prices an
			// unbounded string at its worst case; the webhook's uniqueness check needs
			// no such budget and stays there.
			name:      "include-with-oversized-name-rejected-by-rule",
			spec:      fmt.Sprintf(`{"nodeSelector":{},"include":[%q]}`, strings.Repeat("m", 65)),
			want:      rdmaSelState{selectorSet: true, includePresent: true, names: []string{strings.Repeat("m", 65)}},
			wantCRDOk: true,
			wantCELOk: false,
		},
	}
}

func rdmaWireBody(version, spec string) string {
	return fmt.Sprintf(`{"apiVersion":"worker.gpustack.ai/%s","kind":"DevicesRDMASelectionPolicy",`+
		`"metadata":{"name":"probe"},"spec":%s}`, version, spec)
}

type rdmaWireCodecs struct {
	jsonEnc   runtime.Encoder
	jsonDec   runtime.Decoder
	strictDec runtime.Decoder
	pb        *protobuf.Serializer
}

func rdmaWireCodecsFor(t *testing.T) rdmaWireCodecs {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, workercore.AddToScheme(scheme))
	require.NoError(t, AddToScheme(scheme))
	return rdmaWireCodecs{
		jsonEnc: jsonserializer.NewSerializerWithOptions(jsonserializer.DefaultMetaFactory, scheme, scheme, jsonserializer.SerializerOptions{}),
		jsonDec: serializer.NewCodecFactory(scheme).UniversalDeserializer(),
		strictDec: jsonserializer.NewSerializerWithOptions(jsonserializer.DefaultMetaFactory, scheme, scheme,
			jsonserializer.SerializerOptions{Strict: true}),
		pb: protobuf.NewSerializer(scheme, scheme),
	}
}

// TestRDMASelectionPolicyWireJSON pins the typed decode state of every JSON shape on the
// public and internal types, under the strict and non-strict serializers. Typed decode
// itself rejects nothing; semantic rejection belongs to the webhook and the resolver.
func TestRDMASelectionPolicyWireJSON(t *testing.T) {
	cc := rdmaWireCodecsFor(t)

	targets := []struct {
		label   string
		version string
		into    func() runtime.Object
		stateOf func(runtime.Object) rdmaSelState
	}{
		{
			label:   "public-v1",
			version: "v1",
			into:    func() runtime.Object { return &DevicesRDMASelectionPolicy{} },
			stateOf: func(o runtime.Object) rdmaSelState { return rdmaSelStateOfPublic(o.(*DevicesRDMASelectionPolicy)) },
		},
		{
			label:   "internal-v1alpha1",
			version: "v1alpha1",
			into:    func() runtime.Object { return &workercore.DevicesRDMASelectionPolicy{} },
			stateOf: func(o runtime.Object) rdmaSelState {
				return rdmaSelStateOfInternal(o.(*workercore.DevicesRDMASelectionPolicy))
			},
		},
	}
	decoders := []struct {
		label string
		dec   runtime.Decoder
	}{
		{label: "non-strict", dec: cc.jsonDec},
		{label: "strict", dec: cc.strictDec},
	}

	for _, c := range rdmaWireCases() {
		for _, target := range targets {
			for _, dec := range decoders {
				t.Run(c.name+"/"+target.label+"/"+dec.label, func(t *testing.T) {
					body := rdmaWireBody(target.version, c.spec)
					require.True(t, json.Valid([]byte(body)),
						"test setup: fixture must be well-formed JSON before any serializer assertion")
					obj := target.into()
					require.NoError(t, runtime.DecodeInto(dec.dec, []byte(body), obj),
						"typed decode must accept well-formed documents; rejection is semantic")
					target.stateOf(obj).assertEqual(t, c.want, "decoded")
				})
			}
		}
	}
}

// TestRDMASelectionPolicyWireTypedForward shows what typed JSON forwarding sends to storage.
// It does not exercise a running public API server.
func TestRDMASelectionPolicyWireTypedForward(t *testing.T) {
	cc := rdmaWireCodecsFor(t)
	for _, name := range []string{"include", "exclude"} {
		t.Run(name, func(t *testing.T) {
			body := rdmaWireBody("v1", fmt.Sprintf(`{"nodeSelector":{},%q:[]}`, name))
			public := &DevicesRDMASelectionPolicy{}
			require.NoError(t, runtime.DecodeInto(cc.jsonDec, []byte(body), public))
			internal := (*workercore.DevicesRDMASelectionPolicy)(public)
			if name == "include" {
				require.NotNil(t, internal.Spec.Include)
			} else {
				require.NotNil(t, internal.Spec.Exclude)
			}
			encoded, err := runtime.Encode(cc.jsonEnc, internal)
			require.NoError(t, err)
			var forwarded map[string]any
			require.NoError(t, json.Unmarshal(encoded, &forwarded))
			spec, ok := forwarded["spec"].(map[string]any)
			require.True(t, ok)
			assert.NotContains(t, spec, name, "omitempty removes the empty list before forwarding")
		})
	}
}

// TestRDMASelectionPolicyWireProtobuf pins the semantic state after a protobuf round trip.
// Protobuf has no presence for repeated fields, so nil and empty lists collapse.
func TestRDMASelectionPolicyWireProtobuf(t *testing.T) {
	cc := rdmaWireCodecsFor(t)

	type pbCase struct {
		name      string
		build     func() *workercore.DevicesRDMASelectionPolicy
		wantState func(*workercore.DevicesRDMASelectionPolicy) rdmaSelState
	}
	cases := []pbCase{
		{
			name: "include-absent-stays-absent",
			build: func() *workercore.DevicesRDMASelectionPolicy {
				return &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{},
				}}
			},
			wantState: func(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
				return rdmaSelState{selectorSet: true}
			},
		},
		{
			name: "selector-empty-stays-present",
			build: func() *workercore.DevicesRDMASelectionPolicy {
				return &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{},
				}}
			},
			wantState: func(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
				return rdmaSelState{selectorSet: true}
			},
		},
		{
			name: "include-empty-collapses-to-nil",
			build: func() *workercore.DevicesRDMASelectionPolicy {
				return &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{},
					Include:      []string{},
				}}
			},
			wantState: func(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
				return rdmaSelState{selectorSet: true}
			},
		},
		{
			name: "include-with-names-survives",
			build: func() *workercore.DevicesRDMASelectionPolicy {
				return &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{},
					Include:      []string{"mlx5_4"},
				}}
			},
			wantState: func(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
				return rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_4"}}
			},
		},
		{
			name: "exclude-empty-collapses-to-none",
			build: func() *workercore.DevicesRDMASelectionPolicy {
				return &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
					NodeSelector: &meta.LabelSelector{},
					Exclude:      []string{},
				}}
			},
			wantState: func(p *workercore.DevicesRDMASelectionPolicy) rdmaSelState {
				return rdmaSelState{selectorSet: true}
			},
		},
	}

	for _, c := range cases {
		for _, target := range []struct {
			label string
			from  func() runtime.Object
			into  func() *workercore.DevicesRDMASelectionPolicy
			state func(*workercore.DevicesRDMASelectionPolicy) rdmaSelState
		}{
			{
				label: "internal-v1alpha1",
				from:  func() runtime.Object { return c.build() },
				into:  func() *workercore.DevicesRDMASelectionPolicy { return &workercore.DevicesRDMASelectionPolicy{} },
				state: rdmaSelStateOfInternal,
			},
			{
				label: "public-v1",
				from: func() runtime.Object {
					src := c.build()
					pub := &DevicesRDMASelectionPolicy{Spec: src.Spec}
					return pub
				},
				into:  func() *workercore.DevicesRDMASelectionPolicy { return &workercore.DevicesRDMASelectionPolicy{} },
				state: rdmaSelStateOfInternal,
			},
		} {
			t.Run(c.name+"/"+target.label, func(t *testing.T) {
				data, err := runtime.Encode(cc.pb, target.from())
				require.NoError(t, err)
				out := target.into()
				require.NoError(t, runtime.DecodeInto(cc.pb, data, out))
				target.state(out).assertEqual(t, c.wantState(out), "round-tripped")
			})
		}
	}
}

// TestRDMASelectionPolicyWireConversion pins the generated conversion pair used by the
// storage proxy, in both directions, over the full state matrix.
func TestRDMASelectionPolicyWireConversion(t *testing.T) {
	cases := []rdmaSelState{
		{selectorSet: false},
		{selectorSet: true},
		{selectorSet: true, names: []string{}},
		{selectorSet: true, includePresent: true, names: []string{"mlx5_4"}},
		{selectorSet: true, exclude: []string{}},
		{selectorSet: true, exclude: []string{"mlx5_5"}},
		{selectorSet: true, includePresent: true, names: []string{"mlx5_4", "mlx5_5"}, exclude: []string{"mlx5_5"}},
	}

	for i, want := range cases {
		t.Run(fmt.Sprintf("case-%d", i), func(t *testing.T) {
			internal := &workercore.DevicesRDMASelectionPolicy{Spec: workercore.DevicesRDMASelectionPolicySpec{
				NodeSelector: func() *meta.LabelSelector {
					if !want.selectorSet {
						return nil
					}
					return &meta.LabelSelector{}
				}(),
				Include: want.names,
				Exclude: want.exclude,
			}}

			t.Run("internal-to-public", func(t *testing.T) {
				public := &DevicesRDMASelectionPolicy{}
				require.NoError(t, Convert_v1alpha1_DevicesRDMASelectionPolicy_To_v1_DevicesRDMASelectionPolicy(internal, public, nil))
				rdmaSelStateOfPublic(public).assertEqual(t, want, "converted")
			})
			t.Run("public-back-to-internal", func(t *testing.T) {
				public := &DevicesRDMASelectionPolicy{}
				require.NoError(t, Convert_v1alpha1_DevicesRDMASelectionPolicy_To_v1_DevicesRDMASelectionPolicy(internal, public, nil))
				back := &workercore.DevicesRDMASelectionPolicy{}
				require.NoError(t, Convert_v1_DevicesRDMASelectionPolicy_To_v1alpha1_DevicesRDMASelectionPolicy(public, back, nil))
				rdmaSelStateOfInternal(back).assertEqual(t, want, "converted back")
			})
		})
	}
}

// TestRDMASelectionPolicyWireGeneratedCRD runs the real structural validator, the real CEL
// validator, and the pruning of the CRD API server against the generated schema. The webhook
// rejects a nil selector, and the schema's x-kubernetes-validations carry the include/exclude
// exclusivity and the non-empty, unique HCA-name rules.
func TestRDMASelectionPolicyWireGeneratedCRD(t *testing.T) {
	crd := workercore.GetCustomResourceDefinitions()["DevicesRDMASelectionPolicy"]
	require.NotNil(t, crd, "generated CRD for the policy must exist")
	schemaV1 := crd.Spec.Versions[0].Schema.OpenAPIV3Schema
	require.NotNil(t, schemaV1)

	var internalProps apiextensions.JSONSchemaProps
	require.NoError(t, apiextensionsv1.Convert_v1_JSONSchemaProps_To_apiextensions_JSONSchemaProps(schemaV1, &internalProps, nil))
	structural, err := apischema.NewStructural(&internalProps)
	require.NoError(t, err)
	validator, _, err := apiservalidation.NewSchemaValidator(&internalProps)
	require.NoError(t, err)
	celValidator := celvalidation.NewValidator(structural, true, celconfig.PerCallLimit)
	require.NotNil(t, celValidator,
		"the generated schema must carry x-kubernetes-validations for the CEL validator to compile")

	// The generated CRD must also survive Kubernetes CRD registration validation, which is
	// where the CEL cost budget lives: a rule the budget prices over its limit is refused at
	// apiserver startup, which would keep every CRD of this operator from installing.
	apiextensionsv1.SetDefaults_CustomResourceDefinition(crd)
	var internalCRD apiextensions.CustomResourceDefinition
	require.NoError(t, apiextensionsv1.Convert_v1_CustomResourceDefinition_To_apiextensions_CustomResourceDefinition(crd, &internalCRD, nil))
	internalCRD.Status.StoredVersions = []string{internalCRD.Spec.Versions[0].Name}
	regErrs := apiextensionsvalidation.ValidateCustomResourceDefinition(context.Background(), &internalCRD)
	require.Empty(t, regErrs, "generated CRD must pass Kubernetes CRD registration validation including CEL cost budget")

	for _, c := range rdmaWireCases() {
		t.Run(c.name, func(t *testing.T) {
			var spec map[string]any
			require.True(t, json.Valid([]byte(c.spec)), "test setup: fixture must be well-formed JSON")
			require.NoError(t, json.Unmarshal([]byte(c.spec), &spec))

			obj := map[string]any{
				"apiVersion": "worker.gpustack.ai/v1alpha1",
				"kind":       "DevicesRDMASelectionPolicy",
				"metadata":   map[string]any{"name": "probe"},
				"spec":       spec,
			}
			errs := apiservalidation.ValidateCustomResource(field.NewPath("test"), obj, validator)
			if c.crdNote != "" {
				t.Logf("schema behavior note: %s", c.crdNote)
			}
			if c.wantCRDOk {
				assert.Empty(t, errs, "schema must accept %q", c.name)
			} else {
				assert.NotEmpty(t, errs, "schema must reject %q", c.name)
			}

			celErrs, _ := celValidator.Validate(context.Background(), field.NewPath("test"), structural, obj, nil, celconfig.PerCallLimit)
			if c.wantCELOk {
				assert.Empty(t, celErrs, "the schema's rules must accept %q", c.name)
			} else {
				assert.NotEmpty(t, celErrs, "the schema's rules must reject %q", c.name)
			}

			if c.name == "include-empty-rejected-by-schema" {
				pruning.Prune(obj, structural, true)
				encoded, merr := json.Marshal(obj)
				require.NoError(t, merr)
				t.Logf("pruned stored form: %s", encoded)
				assert.Contains(t, string(encoded), `"include":[]`,
					"pruning must keep the explicit empty list")
			}
		})
	}
}

func rdmaWireStoredJSON() []byte {
	return []byte(`{"apiVersion":"worker.gpustack.ai/v1alpha1","kind":"DevicesRDMASelectionPolicy",` +
		`"metadata":{"name":"probe"},"spec":{"nodeSelector":{},` +
		`"include":["mlx5_4","mlx5_5"],"exclude":["mlx5_9"]}}`)
}

// TestRDMASelectionPolicyWireJSONMergePatch pins RFC 7386 semantics against the stored
// form: null removes a member and an array replaces the old list.
func TestRDMASelectionPolicyWireJSONMergePatch(t *testing.T) {
	cc := rdmaWireCodecsFor(t)

	cases := []struct {
		name  string
		patch string
		want  rdmaSelState
	}{
		{
			name:  "include-null-removes-the-restriction",
			patch: `{"spec":{"include":null}}`,
			want:  rdmaSelState{selectorSet: true, exclude: []string{"mlx5_9"}},
		},
		{
			name:  "include-list-replaces-existing-list",
			patch: `{"spec":{"include":["mlx5_0"]}}`,
			want:  rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_0"}, exclude: []string{"mlx5_9"}},
		},
		{
			name:  "exclude-null-removes-exclusions",
			patch: `{"spec":{"exclude":null}}`,
			want:  rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_4", "mlx5_5"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, json.Valid([]byte(c.patch)), "test setup: patch fixture must be well-formed JSON")
			merged, err := jsonpatch.MergePatch(rdmaWireStoredJSON(), []byte(c.patch))
			require.NoError(t, err)
			require.True(t, json.Valid(merged), "test setup: merged result must be well-formed JSON")

			out := &workercore.DevicesRDMASelectionPolicy{}
			require.NoError(t, runtime.DecodeInto(cc.jsonDec, merged, out))
			rdmaSelStateOfInternal(out).assertEqual(t, c.want, "merged")
		})
	}
}

// TestRDMASelectionPolicyWireJSONPatch pins RFC 6902 operations that replace or remove
// the include restriction.
func TestRDMASelectionPolicyWireJSONPatch(t *testing.T) {
	cc := rdmaWireCodecsFor(t)

	cases := []struct {
		name string
		ops  string
		want rdmaSelState
	}{
		{
			name: "remove-include-unrestricts",
			ops:  `[{"op":"remove","path":"/spec/include"}]`,
			want: rdmaSelState{selectorSet: true, exclude: []string{"mlx5_9"}},
		},
		{
			name: "replace-include-list",
			ops:  `[{"op":"replace","path":"/spec/include","value":["mlx5_0"]}]`,
			want: rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_0"}, exclude: []string{"mlx5_9"}},
		},
		{
			name: "remove-exclude",
			ops:  `[{"op":"remove","path":"/spec/exclude"}]`,
			want: rdmaSelState{selectorSet: true, includePresent: true, names: []string{"mlx5_4", "mlx5_5"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			require.True(t, json.Valid([]byte(c.ops)), "test setup: patch fixture must be well-formed JSON")
			patch, err := jsonpatch.DecodePatch([]byte(c.ops))
			require.NoError(t, err)
			patched, err := patch.Apply(rdmaWireStoredJSON())
			require.NoError(t, err)
			require.True(t, json.Valid(patched), "test setup: patched result must be well-formed JSON")

			out := &workercore.DevicesRDMASelectionPolicy{}
			require.NoError(t, runtime.DecodeInto(cc.jsonDec, patched, out))
			rdmaSelStateOfInternal(out).assertEqual(t, c.want, "patched")
		})
	}
}

// The server-side apply tests below use the structured-merge-diff engine with a schema
// mirroring this CRD's structural facts: atomic string lists.

const rdmaSelApplySchema = `
types:
- name: policy
  map:
    fields:
    - name: apiVersion
      type: {scalar: string}
    - name: kind
      type: {scalar: string}
    - name: metadata
      type: {namedType: __untyped_atomic_}
    - name: spec
      type: {namedType: policy.spec}
- name: policy.spec
  map:
    fields:
    - name: nodeSelector
      type: {namedType: __untyped_atomic_}
    - name: include
      type:
        list:
          elementType: {scalar: string}
          elementRelationship: atomic
    - name: exclude
      type:
        list:
          elementType: {scalar: string}
          elementRelationship: atomic
- name: __untyped_atomic_
  scalar: untyped
  list:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
  map:
    elementType:
      namedType: __untyped_atomic_
    elementRelationship: atomic
`

const rdmaSelApplyVersion = smdfield.APIVersion("worker.gpustack.ai/v1alpha1")

// rdmaSelApplyEnv is a minimal in-process server-side apply session: one live object plus
// its managed fields, updated through the real structured-merge-diff Updater.
type rdmaSelApplyEnv struct {
	t        *testing.T
	parser   *smdtyped.Parser
	updater  *smdmerge.Updater
	live     *smdtyped.TypedValue
	managers smdfield.ManagedFields
}

// rdmaSelApplyConverter is the single-version identity converter the merge engine
// requires; no version conversion happens in this session.
type rdmaSelApplyConverter struct{}

func (rdmaSelApplyConverter) Convert(object *smdtyped.TypedValue, _ smdfield.APIVersion) (*smdtyped.TypedValue, error) {
	return object, nil
}

func (rdmaSelApplyConverter) IsMissingVersionError(_ error) bool { return false }

func newRdmaSelApplyEnv(t *testing.T) *rdmaSelApplyEnv {
	t.Helper()
	parser, err := smdtyped.NewParser(rdmaSelApplySchema)
	require.NoError(t, err)
	live, err := parser.Type("policy").FromUnstructured(map[string]any{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind":       "DevicesRDMASelectionPolicy",
		"metadata":   map[string]any{"name": "probe"},
	})
	require.NoError(t, err)
	builder := smdmerge.UpdaterBuilder{
		Converter:         rdmaSelApplyConverter{},
		ReturnInputOnNoop: true,
	}
	return &rdmaSelApplyEnv{
		t:        t,
		parser:   parser,
		updater:  builder.BuildUpdater(),
		live:     live,
		managers: smdfield.ManagedFields{},
	}
}

func (e *rdmaSelApplyEnv) objectTV(spec map[string]any) *smdtyped.TypedValue {
	e.t.Helper()
	tv, err := e.parser.Type("policy").FromUnstructured(map[string]any{
		"apiVersion": "worker.gpustack.ai/v1alpha1",
		"kind":       "DevicesRDMASelectionPolicy",
		"metadata":   map[string]any{"name": "probe"},
		"spec":       spec,
	})
	require.NoError(e.t, err)
	return tv
}

func (e *rdmaSelApplyEnv) Apply(manager string, spec map[string]any) map[string]any {
	e.t.Helper()
	return e.apply(manager, spec, false)
}

// ApplyErr applies without force and returns the engine error, for conflict assertions.
func (e *rdmaSelApplyEnv) ApplyErr(manager string, spec map[string]any) error {
	e.t.Helper()
	_, err := e.applyResult(manager, spec, false)
	return err
}

// ApplyForced applies with force, taking over foreign-owned fields on conflict.
func (e *rdmaSelApplyEnv) ApplyForced(manager string, spec map[string]any) map[string]any {
	e.t.Helper()
	return e.apply(manager, spec, true)
}

func (e *rdmaSelApplyEnv) apply(manager string, spec map[string]any, force bool) map[string]any {
	e.t.Helper()
	live, err := e.applyResult(manager, spec, force)
	require.NoError(e.t, err, "apply by %q", manager)
	return e.content(live)
}

func (e *rdmaSelApplyEnv) applyResult(manager string, spec map[string]any, force bool) (*smdtyped.TypedValue, error) {
	e.t.Helper()
	config := e.objectTV(spec)
	live, managers, err := e.updater.Apply(e.live, config, rdmaSelApplyVersion, e.managers, manager, force)
	if err != nil {
		return nil, err
	}
	e.managers = managers
	e.setLive(live)
	return e.live, nil
}

func (e *rdmaSelApplyEnv) Update(manager string, spec map[string]any) error {
	e.t.Helper()
	config := e.objectTV(spec)
	live, managers, err := e.updater.Update(e.live, config, rdmaSelApplyVersion, e.managers, manager)
	if err != nil {
		return err
	}
	e.managers = managers
	e.setLive(live)
	return nil
}

// setLive renormalizes the merged result through plain Go maps, mirroring the real
// serving path where every request decodes the live object from storage afresh instead
// of reusing the merge engine's internal value wrappers.
func (e *rdmaSelApplyEnv) setLive(live *smdtyped.TypedValue) {
	e.t.Helper()
	if live == nil {
		return
	}
	normalized, err := e.parser.Type("policy").FromUnstructured(e.content(live))
	require.NoError(e.t, err)
	e.live = normalized
}

func (e *rdmaSelApplyEnv) content(tv *smdtyped.TypedValue) map[string]any {
	e.t.Helper()
	content, ok := tv.AsValue().Unstructured().(map[string]any)
	require.True(e.t, ok, "applied result must be an object")
	return content
}

func (e *rdmaSelApplyEnv) specOf(content map[string]any) map[string]any {
	e.t.Helper()
	spec, ok := content["spec"].(map[string]any)
	require.True(e.t, ok, "result must carry spec")
	return spec
}

func (e *rdmaSelApplyEnv) namesOf(spec map[string]any) ([]string, bool) {
	e.t.Helper()
	raw, ok := spec["include"]
	if !ok {
		return nil, false
	}
	items, ok := raw.([]any)
	require.True(e.t, ok, "names must be a list")
	names := make([]string, 0, len(items))
	for _, item := range items {
		name, ok := item.(string)
		require.True(e.t, ok, "names entries must be strings")
		names = append(names, name)
	}
	return names, true
}

// TestRDMASelectionPolicyWireApplyOwnership checks atomic list ownership locally.
// The real public API field manager is not exercised here.
func TestRDMASelectionPolicyWireApplyOwnership(t *testing.T) {
	fullSpec := func(names ...string) map[string]any {
		return map[string]any{
			"nodeSelector": map[string]any{},
			"include":      rdmaSelIfaceNames(names...),
		}
	}

	t.Run("generated-builder-omits-empty-lists", func(t *testing.T) {
		builder := applyv1alpha1.DevicesRDMASelectionPolicySpec().
			WithNodeSelector(k8sapplymeta.LabelSelector()).
			WithInclude().WithExclude()
		encoded, err := json.Marshal(builder)
		require.NoError(t, err)
		var spec map[string]any
		require.NoError(t, json.Unmarshal(encoded, &spec))
		assert.NotContains(t, spec, "include")
		assert.NotContains(t, spec, "exclude")
	})

	t.Run("generated-builder-includes-names", func(t *testing.T) {
		builder := applyv1alpha1.DevicesRDMASelectionPolicySpec().
			WithNodeSelector(k8sapplymeta.LabelSelector()).
			WithInclude("mlx5_4", "mlx5_5").WithExclude("mlx5_9")
		encoded, err := json.Marshal(builder)
		require.NoError(t, err)
		var spec map[string]any
		require.NoError(t, json.Unmarshal(encoded, &spec))
		env := newRdmaSelApplyEnv(t)
		applied := env.specOf(env.Apply("admin", spec))
		names, present := env.namesOf(applied)
		assert.True(t, present)
		assert.Equal(t, []string{"mlx5_4", "mlx5_5"}, names)
		assert.Equal(t, []any{"mlx5_9"}, applied["exclude"])
	})

	t.Run("other-manager-conflicts-until-forced", func(t *testing.T) {
		env := newRdmaSelApplyEnv(t)
		env.Apply("admin", fullSpec("mlx5_4"))
		assert.Error(t, env.ApplyErr("operator", fullSpec("mlx5_5")))
		spec := env.specOf(env.ApplyForced("operator", fullSpec("mlx5_5")))
		names, present := env.namesOf(spec)
		assert.True(t, present)
		assert.Equal(t, []string{"mlx5_5"}, names)
	})

	t.Run("same-manager-reapply-replaces-list", func(t *testing.T) {
		env := newRdmaSelApplyEnv(t)
		env.Apply("admin", fullSpec("mlx5_4"))
		spec := env.specOf(env.Apply("admin", fullSpec("mlx5_5")))
		names, _ := env.namesOf(spec)
		assert.Equal(t, []string{"mlx5_5"}, names)
	})

	t.Run("same-manager-omission-removes-list", func(t *testing.T) {
		env := newRdmaSelApplyEnv(t)
		env.Apply("admin", fullSpec("mlx5_4"))
		spec := env.specOf(env.Apply("admin", map[string]any{"nodeSelector": map[string]any{}}))
		_, present := env.namesOf(spec)
		assert.False(t, present)
	})

	t.Run("plain-update-takes-over-owned-list", func(t *testing.T) {
		env := newRdmaSelApplyEnv(t)
		env.Apply("admin", fullSpec("mlx5_4"))
		require.NoError(t, env.Update("writer", fullSpec("mlx5_4", "mlx5_5")))
		names, _ := env.namesOf(env.specOf(env.content(env.live)))
		assert.Equal(t, []string{"mlx5_4", "mlx5_5"}, names)
		if admin, ok := env.managers["admin"]; ok {
			assert.NotContains(t, admin.Set().String(), ".spec.include")
		}
	})
}

// rdmaSelIfaceNames builds the name list as plain interface values, the only list form
// the structured-merge-diff unstructured decoder accepts.
func rdmaSelIfaceNames(names ...string) []any {
	out := make([]any, 0, len(names))
	for _, name := range names {
		out = append(out, name)
	}
	return out
}
