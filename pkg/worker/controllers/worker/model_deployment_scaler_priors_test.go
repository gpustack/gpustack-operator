package worker

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

type capacityPriorNoReads struct {
	ctrlcli.Reader
	t *testing.T
}

func (r capacityPriorNoReads) Get(context.Context, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
	r.t.Fatal("manual capacities must not read external objects")
	return nil
}

func TestModelDeploymentScalerCapacityPriorManualFields(t *testing.T) {
	tests := []struct {
		name       string
		kind       workercore.ModelDeploymentRoleKind
		predictive bool
	}{
		{name: "server token", kind: workercore.ModelDeploymentRoleKindServer},
		{name: "prefill token", kind: workercore.ModelDeploymentRoleKindPrefill},
		{name: "decode token", kind: workercore.ModelDeploymentRoleKindDecode},
		{name: "server predictive", kind: workercore.ModelDeploymentRoleKindServer, predictive: true},
		{name: "prefill predictive", kind: workercore.ModelDeploymentRoleKindPrefill, predictive: true},
		{name: "decode predictive", kind: workercore.ModelDeploymentRoleKindDecode, predictive: true},
		{name: "encode remains ratio coupled", kind: workercore.ModelDeploymentRoleKindEncode, predictive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{
				{Name: "one", Kind: tt.kind}, {Name: "two", Kind: tt.kind},
			}}}
			token := workercore.ModelDeploymentScalerTokenThroughputSpec{
				ServerTargetTokensPerSecond:  capacityPriorQuantity("101"),
				PrefillTargetTokensPerSecond: capacityPriorQuantity("202"),
				DecodeTargetTokensPerSecond:  capacityPriorQuantity("303"),
			}
			scaler := &workercore.ModelDeploymentScaler{}
			scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
				Type:            workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &token,
				CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "missing"},
				},
			}
			if tt.predictive {
				scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				scaler.Spec.Algorithm.TokenThroughput = nil
				scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput:                token,
					ServerTokenCapacityPerReplica:  capacityPriorQuantity("404"),
					PrefillTokenCapacityPerReplica: capacityPriorQuantity("505"),
					DecodeTokenCapacityPerReplica:  capacityPriorQuantity("606"),
				}
			}
			before := scaler.DeepCopy()
			if ModelDeploymentScalerNeedsCapacityPrior(scaler, md) {
				t.Fatal("all relevant capacity fields are explicit")
			}
			resolved, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), capacityPriorNoReads{t: t}, scaler, md)
			if err != nil {
				t.Fatal(err)
			}
			if !kubemeta.DeepEqual(before.Spec, scaler.Spec) {
				t.Fatal("resolution changed the persisted specification")
			}
			for _, role := range md.Spec.Roles {
				if !kubemeta.DeepEqual(resolved.Algorithms[role.Name], scaler.Spec.Algorithm) {
					t.Fatalf("role %s lost explicit algorithm fields", role.Name)
				}
			}
			one := resolved.Algorithms["one"]
			if tt.predictive {
				one.Predictive.ServerTokenCapacityPerReplica.Set(999)
			} else {
				one.TokenThroughput.ServerTargetTokensPerSecond.Set(999)
			}
			if !kubemeta.DeepEqual(resolved.Algorithms["two"], scaler.Spec.Algorithm) || !kubemeta.DeepEqual(before.Spec, scaler.Spec) {
				t.Fatal("role algorithms share mutable capacities")
			}
			for _, status := range resolved.Status {
				for _, field := range status.Fields {
					if field.Source != workercore.ModelDeploymentScalerCapacityFieldSourceExplicit {
						t.Fatalf("manual field source = %s", field.Source)
					}
				}
			}
		})
	}
}

func capacityPriorQuantity(value string) *resource.Quantity {
	q := resource.MustParse(value)
	return &q
}

func TestModelDeploymentScalerCapacityPriorPartialFields(t *testing.T) {
	tests := []struct {
		name         string
		predictive   bool
		primary      string
		fallback     string
		wantPrimary  string
		wantFallback string
	}{
		{name: "token missing", wantFallback: "1500"},
		{name: "predictive both missing", predictive: true, wantPrimary: "1500", wantFallback: "1500"},
		{name: "predictive explicit primary", predictive: true, primary: "123", wantPrimary: "123", wantFallback: "1500"},
		{name: "predictive explicit fallback", predictive: true, fallback: "456", wantPrimary: "1500", wantFallback: "456"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			if tt.predictive {
				scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{TokenThroughput: *scaler.Spec.Algorithm.TokenThroughput}
				scaler.Spec.Algorithm.TokenThroughput = nil
				if tt.primary != "" {
					scaler.Spec.Algorithm.Predictive.ServerTokenCapacityPerReplica = capacityPriorQuantity(tt.primary)
				}
				if tt.fallback != "" {
					scaler.Spec.Algorithm.Predictive.TokenThroughput.ServerTargetTokensPerSecond = capacityPriorQuantity(tt.fallback)
				}
			}
			before := scaler.DeepCopy()
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			if err != nil {
				t.Fatal(err)
			}
			effective := got.Algorithms["server"]
			token := effective.TokenThroughput
			if tt.predictive {
				token = &effective.Predictive.TokenThroughput
				if effective.Predictive.ServerTokenCapacityPerReplica.Cmp(*capacityPriorQuantity(tt.wantPrimary)) != 0 {
					t.Fatalf("primary = %v, want %s", effective.Predictive.ServerTokenCapacityPerReplica, tt.wantPrimary)
				}
			}
			if token.ServerTargetTokensPerSecond.Cmp(*capacityPriorQuantity(tt.wantFallback)) != 0 {
				t.Fatalf("fallback = %v, want %s", token.ServerTargetTokensPerSecond, tt.wantFallback)
			}
			if !kubemeta.DeepEqual(before.Spec, scaler.Spec) {
				t.Fatal("resolved capacities leaked into spec")
			}
			status := got.Status[0]
			if status.CatalogDigest == "" || status.CompatibilityFingerprint == "" || status.SourceRevision != "" ||
				status.WorkloadRevision != "unspecified" || status.GPUsPerReplica != 1 || status.MeasuredReplicas != 1 || len(status.LatencyBounds) != 2 {
				t.Fatalf("missing capacity evidence: %#v", status)
			}
			// THE STATUS NAMES THE SHAPE THE CAPACITY CAME FROM, because a
			// profile is known by its shape and the status is what a reader
			// gets to see.
			wantShape := "Server/test/model/none/vllm/nvidia-h100/gpus=1,tp=1,ep=1,pp=1,dp=1,ldp=1,cp=1"
			if status.WorkloadID != wantShape {
				t.Fatalf("workload id = %q, want %q", status.WorkloadID, wantShape)
			}
		})
	}
}

func capacityPriorFixture(t *testing.T) (*workercore.ModelDeploymentScaler, *workercore.ModelDeployment, *core.ConfigMap, *workerapi.InstanceType) {
	t.Helper()
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	engineImage := "vllm/vllm-openai@" + imageDigest
	modelName := "test/model"
	args := []string{"--revision=weights-rev", "--tokenizer=test/model", "--dtype=bfloat16", "--kv-cache-dtype=auto", "--no-enable-prefix-caching", "--cpu-offload-gb=0", "--swap-space=0"}
	// THE FIXTURE WRITES THE CATALOG IN THE FORMAT THE OPERATOR SHIPS, because a
	// fixture in another format proves nothing about the format the operator
	// reads.
	document := priors.CatalogDocument{
		SchemaVersion: priors.SchemaVersion,
		Profiles: []priors.ProfileDocument{{
			Role:         string(priors.RoleKindServer),
			Model:        modelName,
			Quantization: "none",
			Engine:       "vllm",
			Hardware:     "nvidia-h100",
			Topology:     priors.TopologyDocument{GPUsPerReplica: 1, TensorParallel: 1},
		}},
	}
	for i, throughput := range []float64{1000, 2000, 2100} {
		ttft, itl := float64(100*(i+1)), float64(10*(i+1))
		if i == 2 {
			ttft, itl = 800, 80
		}
		document.Profiles[0].Points = append(document.Profiles[0].Points, priors.PointDocument{
			Concurrency:           i + 1,
			TotalTokensPerSecond:  throughput,
			InputTokensPerSecond:  throughput,
			OutputTokensPerSecond: throughput,
			TTFTMilliseconds:      priors.LatencyQuantiles{P90: ptr.To(ttft * 0.9), P95: ptr.To(ttft)},
			ITLMilliseconds:       priors.LatencyQuantiles{P90: ptr.To(itl * 0.9), P95: ptr.To(itl)},
		})
	}
	data, err := priors.MarshalCatalogDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := priors.ParseCatalog(data)
	if err != nil {
		t.Fatalf("invalid synthetic fixture: %v", err)
	}
	profile := catalog.Profiles[0]
	md := scalerTestDeployment("target", "target-uid", map[string]int32{"server": 1})
	md.Spec.Engine = workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: engineImage}
	md.Spec.Model.Name = modelName
	md.Spec.Roles[0].ExtraArgs = args
	md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("1")}
	md.Spec.Roles[0].InstanceType = "test-hardware"
	md.Annotations = map[string]string{
		workercore.ModelDeploymentScalerModelIDAnnotationKey:    "model:" + capacityPriorDigestResult(t, func() (string, error) { return priors.Digest(profile.Identity.Model) }),
		workercore.ModelDeploymentScalerHardwareIDAnnotationKey: "hardware:" + capacityPriorDigestResult(t, func() (string, error) { return priors.Digest(profile.Identity.Hardware) }),
		workercore.ModelDeploymentScalerPrecisionAnnotationKey:  "none",
	}
	scaler := scalerTestScaler("test", "scaler-uid", workercore.ModelDeploymentScalerModeActive)
	scaler.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
		Type:            workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
		TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{TTFTTargetMilliseconds: capacityPriorQuantity("500"), ITLTargetMilliseconds: capacityPriorQuantity("50")},
		CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{
			CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "test-priors"},
		},
	}
	cm := &core.ConfigMap{ObjectMeta: meta.ObjectMeta{Name: "test-priors", Namespace: scaler.Namespace}, Data: map[string]string{priors.DefaultCatalogConfigMapKey: string(data)}}
	instance := &workerapi.InstanceType{ObjectMeta: meta.ObjectMeta{Name: "test-hardware"}, Spec: workercore.InstanceTypeSpec{Acceleratable: true}, Status: workercore.InstanceTypeStatus{Detail: workercore.InstanceTypeDetail{Product: "H100", InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{Memory: "80Gi"}}}}
	return scaler, md, cm, instance
}

// capacityPriorCompleteFixture builds the fixture of a catalog that states the
// environment of the measurement.
//
// IT IS THE PLAIN FIXTURE, because the catalog the operator ships states no
// environment, and a catalog that states none constrains the deployment on
// none.
func capacityPriorCompleteFixture(t *testing.T) (*workercore.ModelDeploymentScaler, *workercore.ModelDeployment, *core.ConfigMap, *workerapi.InstanceType) {
	t.Helper()
	return capacityPriorFixture(t)
}

func capacityPriorDigestResult(t *testing.T, fn func() (string, error)) string {
	t.Helper()
	value, err := fn()
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func TestModelDeploymentScalerCapacityPriorRefusals(t *testing.T) {
	tests := []struct{ name, field, value, want string }{
		{name: "wrong model", field: "model", value: "another/model", want: "no catalog profile matches the deployment shape"},
		{name: "another engine family", field: "image", value: "lmsysorg/sglang:v0.5.22", want: "does not match the deployment engine"},
		{name: "wrong tokenizer", field: "arg", value: "--tokenizer=other", want: "tokenizer conflicts"},
		{name: "wrong quantization", field: "arg", value: "--quantization=fp8", want: "no catalog profile matches the deployment shape"},
		{name: "custom command", field: "command", want: "managed single-serving-Pod"},
		{name: "multiple pods", field: "size", want: "managed single-serving-Pod"},
		{name: "data parallel", field: "arg", value: "--data-parallel-size=2", want: "metric denominator"},
		{name: "external data parallel", field: "arg", value: "--data-parallel-external-lb", want: "metric denominator"},
		{name: "elastic parallel", field: "arg", value: "--elastic-ep-max-dp-size=4", want: "metric denominator"},
		{name: "malformed parallel", field: "arg", value: "--tensor-parallel-size", want: "unreadable parallelism"},
		{name: "a data parallel environment variable", field: "env", want: "metric denominator"},
		{name: "router synthesis", field: "router", want: "generated cache"},
		{name: "cache synthesis", field: "cache", want: "generated cache"},
		{name: "speculative decoding", field: "arg", value: "--speculative-config={}", want: "cannot establish settings"},
		{name: "malformed dtype", field: "arg", value: "--dtype", want: "requires a value"},
		{name: "wrong observed hardware", field: "hardware", value: "other-product", want: "no catalog profile matches the deployment shape"},
		{name: "wrong observed memory", field: "memory", value: "40Gi", want: "memory conflicts"},
		{name: "absent artifact", field: "artifact", value: "missing", want: "does not exist"},
		{name: "absent catalog", field: "catalog", value: "absent", want: "does not exist"},
		{name: "invalid catalog", field: "catalog", value: "invalid", want: "is invalid"},
		{name: "absent key", field: "catalog", value: "key", want: "lacks key"},
		{name: "another shape in the catalog", field: "shape", want: "no catalog profile matches the deployment shape"},
		{name: "missing TTFT", field: "slo", value: "ttft", want: "TTFT"},
		{name: "missing ITL", field: "slo", value: "itl", want: "ITL"},
		{name: "explicit zero", field: "capacity", value: "0", want: "must be finite and positive"},
		{name: "explicit negative", field: "capacity", value: "-1", want: "must be finite and positive"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			role := &md.Spec.Roles[0]
			switch tt.field {
			case "model":
				md.Spec.Model.Name = tt.value
			case "image":
				role.Image = tt.value
			case "arg":
				role.ExtraArgs = append(role.ExtraArgs, tt.value)
			case "command":
				role.Command = []string{"custom"}
			case "size":
				role.ReplicaSize = 2
			case "env":
				role.Env = []workercore.ModelDeploymentEnvVar{{Name: "VLLM_DP_SIZE", Value: "2"}}
			case "router":
				md.Spec.Router = &workercore.ModelDeploymentRouter{Name: "llm-d-router"}
			case "cache":
				md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "cache"}}
			case "hardware":
				instance.Status.Detail.Product = tt.value
			case "memory":
				instance.Status.Detail.Memory = tt.value
			case "artifact":
				md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: tt.value}
			case "catalog":
				switch tt.value {
				case "absent":
					scaler.Spec.Algorithm.CapacityPrior.CatalogRef.Name = "absent"
				case "invalid":
					cm.Data[priors.DefaultCatalogConfigMapKey] = "{"
				case "key":
					// THE CATALOGMAP HOLDS NEITHER NAME, because a reader that
					// falls back to the other name finds the catalog the operator
					// shipped.
					scaler.Spec.Algorithm.CapacityPrior.CatalogRef.Key = "missing"
					delete(cm.Data, priors.DefaultCatalogConfigMapKey)
				}
			case "shape":
				// THE CATALOG MEASURES ANOTHER MODEL, because a profile is
				// selected by the shape it measured and a catalog that holds
				// no profile of the deployment's shape resolves nothing.
				document := capacityPriorReadCatalog(t, cm)
				document.Profiles[0].Model = "another/model"
				capacityPriorWriteCatalog(t, cm, document)
			case "slo":
				if tt.value == "ttft" {
					scaler.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = nil
				} else {
					scaler.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
				}
			case "capacity":
				scaler.Spec.Algorithm.TokenThroughput.ServerTargetTokensPerSecond = capacityPriorQuantity(tt.value)
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			var refusal *priors.RefusalError
			if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("error = %v, want typed refusal containing %q", err, tt.want)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorFingerprint(t *testing.T) {
	scaler, md, cm, instance := capacityPriorCompleteFixture(t)
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
	first, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		name, field string
		changed     bool
	}{
		{name: "replica changes retain learning", field: "replicas"},
		{name: "target replacement resets learning", field: "uid", changed: true},
		{name: "SLO changes reset learning", field: "slo", changed: true},
		{name: "headroom changes reset learning", field: "headroom", changed: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			nextMD, nextScaler := md.DeepCopy(), scaler.DeepCopy()
			switch tt.field {
			case "replicas":
				nextMD.Spec.Roles[0].Replicas++
			case "uid":
				nextMD.UID = "another-target"
			case "slo":
				nextScaler.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = capacityPriorQuantity("600")
			case "headroom":
				nextScaler.Spec.Algorithm.CapacityPrior.Headroom = capacityPriorQuantity("0.8")
			}
			second, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, nextScaler, nextMD)
			if err != nil {
				t.Fatal(err)
			}
			changed := first.Status[0].CompatibilityFingerprint != second.Status[0].CompatibilityFingerprint
			if changed != tt.changed {
				t.Fatalf("fingerprint changed = %v, want %v", changed, tt.changed)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorTwoServerRoles(t *testing.T) {
	scaler, md, cm, instance := capacityPriorCompleteFixture(t)
	document := capacityPriorReadCatalog(t, cm)
	second := document.Profiles[0]
	// THE SECOND ROLE MEASURES A WIDER REPLICA, because a profile is selected
	// by the shape it measured and two roles of one shape would resolve to one
	// measurement.
	second.Topology = priors.TopologyDocument{GPUsPerReplica: 2, TensorParallel: 1}
	// THE POINTS ARE COPIED, because a slice a profile shares with another
	// profile is a slice both profiles change.
	second.Points = make([]priors.PointDocument, 0, len(document.Profiles[0].Points))
	for _, point := range document.Profiles[0].Points {
		point.TotalTokensPerSecond /= 2
		point.InputTokensPerSecond /= 2
		point.OutputTokensPerSecond /= 2
		second.Points = append(second.Points, point)
	}
	document.Profiles = append(document.Profiles, second)
	capacityPriorWriteCatalog(t, cm, document)
	role := md.Spec.Roles[0].DeepCopy()
	role.Name = "second"
	role.Resources = &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("2")}
	md.Spec.Roles = append(md.Spec.Roles, *role)
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
	resolved, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
	if err != nil {
		t.Fatal(err)
	}
	for role, want := range map[string]string{"server": "1500", "second": "750"} {
		effective := resolved.Algorithms[role].TokenThroughput.ServerTargetTokensPerSecond
		if effective.Cmp(*capacityPriorQuantity(want)) != 0 {
			t.Fatalf("role %s capacity = %v, want %s", role, effective, want)
		}
	}
}

// capacityPriorWriteCatalog writes the catalog document into the ConfigMap.
//
// IT CHECKS THE DOCUMENT BEFORE IT WRITES IT, because a catalog the reader
// refuses refuses every resolution.
func capacityPriorWriteCatalog(t *testing.T, cm *core.ConfigMap, document priors.CatalogDocument) {
	t.Helper()
	data, err := priors.MarshalCatalogDocument(document)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := priors.ParseCatalog(data); err != nil {
		t.Fatalf("invalid synthetic fixture: %v", err)
	}
	cm.Data[priors.DefaultCatalogConfigMapKey] = string(data)
}

// capacityPriorReadCatalog reads the catalog document of the ConfigMap.
func capacityPriorReadCatalog(t *testing.T, cm *core.ConfigMap) priors.CatalogDocument {
	t.Helper()
	document, err := priors.ParseCatalogDocument([]byte(cm.Data[priors.DefaultCatalogConfigMapKey]))
	if err != nil {
		t.Fatal(err)
	}
	return document
}

func TestModelDeploymentScalerCapacityPriorNativeIdentityFlags(t *testing.T) {
	tests := []struct {
		name, engine, prefix string
		args                 []string
		wantConflict         string
	}{
		{name: "sglang tokenizer path agrees", engine: workercore.ModelDeploymentEngineSGLang, prefix: "disabled", args: []string{"--tokenizer-path=test/model", "--disable-radix-cache"}},
		{name: "sglang tokenizer path conflicts", engine: workercore.ModelDeploymentEngineSGLang, prefix: "disabled", args: []string{"--tokenizer-path=another/tokenizer", "--disable-radix-cache"}, wantConflict: "tokenizer conflicts"},
		{name: "attached quantization agrees", engine: workercore.ModelDeploymentEngineVLLM, prefix: "disabled", args: []string{"-qnone", "--no-enable-prefix-caching"}},
		{name: "attached quantization conflicts", engine: workercore.ModelDeploymentEngineVLLM, prefix: "disabled", args: []string{"-qfp8", "--no-enable-prefix-caching"}, wantConflict: "no catalog profile matches the deployment shape"},
		{name: "last prefix enable wins", engine: workercore.ModelDeploymentEngineVLLM, prefix: "enabled", args: []string{"--no-enable-prefix-caching", "--enable-prefix-caching"}},
		{name: "last prefix disable wins", engine: workercore.ModelDeploymentEngineVLLM, prefix: "disabled", args: []string{"--enable-prefix-caching", "--no-enable-prefix-caching"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			md.Spec.Engine.Name = tt.engine
			if tt.engine == workercore.ModelDeploymentEngineSGLang {
				// THE CATALOG AND THE DEPLOYMENT RUN THE SAME ENGINE, because
				// the catalog states the engine family and the deployment names
				// the engine it runs.
				md.Spec.Engine.Image = "lmsysorg/sglang:v0.5.22"
				document := capacityPriorReadCatalog(t, cm)
				document.Profiles[0].Engine = "sglang"
				capacityPriorWriteCatalog(t, cm, document)
			}
			args := []string{"--revision=weights-rev", "--dtype=bfloat16", "--kv-cache-dtype=auto", "--cpu-offload-gb=0", "--swap-space=0"}
			args = append(args, tt.args...)
			md.Spec.Roles[0].ExtraArgs = args
			client := scalerTestClient(t, scaler, md, cm, instance)
			resolved, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			if tt.wantConflict != "" {
				var refusal *priors.RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tt.wantConflict) {
					t.Fatalf("identity conflict = %v, want %s", err, tt.wantConflict)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := resolved.Algorithms["server"].TokenThroughput.ServerTargetTokensPerSecond; got.Cmp(*capacityPriorQuantity("1500")) != 0 {
				t.Fatalf("effective capacity = %v, want 1500", got)
			}
		})
	}
}

type capacityPriorErrorReader struct {
	ctrlcli.Reader
	kind string
	err  error
}

func (r capacityPriorErrorReader) Get(ctx context.Context, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
	if reflect.TypeOf(obj).String() == r.kind {
		return r.err
	}
	return r.Reader.Get(ctx, key, obj, opts...)
}

func TestModelDeploymentScalerCapacityPriorReadErrors(t *testing.T) {
	tests := []struct{ name, kind string }{
		{name: "catalog", kind: "*v1.ConfigMap"},
		{name: "hardware", kind: "*v1.InstanceType"},
		{name: "artifact", kind: "*v1alpha1.ModelArtifact"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			if tt.name == "artifact" {
				md.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			forbidden := apierrors.NewForbidden(schema.GroupResource{Resource: tt.name}, "test", errors.New("denied"))
			_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), capacityPriorErrorReader{Reader: client, kind: tt.kind, err: forbidden}, scaler, md)
			if !apierrors.IsForbidden(err) || !errors.Is(err, forbidden) {
				t.Fatalf("API error changed: %v", err)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorTokenizerIdentity(t *testing.T) {
	tests := []struct {
		name, tokenizer, revision, identity string
		wantError                           bool
	}{
		{name: "explicit repository", tokenizer: "test/tokenizer", identity: "test/tokenizer"},
		{name: "explicit repository and revision", tokenizer: "test/tokenizer", revision: "abc", identity: "test/tokenizer@abc"},
		{name: "default repository with revision", revision: "abc", identity: "test/model@abc"},
		{name: "revision mismatch", tokenizer: "test/tokenizer", revision: "abc", identity: "test/tokenizer@def", wantError: true},
		{name: "repository mismatch", tokenizer: "other/tokenizer", revision: "abc", identity: "test/tokenizer@abc", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{Model: workercore.ModelDeploymentModel{Name: "test/model"}}}
			flags := map[string]string{}
			if tt.tokenizer != "" {
				flags["--tokenizer"] = tt.tokenizer
			}
			if tt.revision != "" {
				flags["--tokenizer-revision"] = tt.revision
			}
			identity := priors.ModelIdentity{Name: "test/model", Revision: "weights", Tokenizer: tt.identity, Quantization: "none"}
			// THE DECLARATION IS READ FROM THE DEPLOYMENT, because the profile
			// is selected by what the deployment states.
			model, err := modelDeploymentScalerPriorModelIdentity(
				context.Background(), capacityPriorNoReads{t: t}, md, "server", flags,
			)
			if err != nil {
				t.Fatalf("read the model identity: %v", err)
			}
			declaration := modelDeploymentScalerPriorDeclaration{
				identity: priors.Identity{Model: model},
				flags:    flags,
			}
			err = modelDeploymentScalerPriorModel(&workercore.ModelDeploymentRole{Name: "server"}, identity, declaration)
			if (err != nil) != tt.wantError {
				t.Fatalf("identity error = %v, wantError %v", err, tt.wantError)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorQuantityPrecision(t *testing.T) {
	tests := []struct {
		name       string
		multiplier float64
		want       string
	}{
		{name: "sub milli capacity remains positive", multiplier: 1e-7, want: "0.00015"},
		{name: "large capacity avoids milli integer overflow", multiplier: 1e14, want: "150000000000000000"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			document := capacityPriorReadCatalog(t, cm)
			for i := range document.Profiles[0].Points {
				document.Profiles[0].Points[i].TotalTokensPerSecond *= tt.multiplier
				document.Profiles[0].Points[i].InputTokensPerSecond *= tt.multiplier
				document.Profiles[0].Points[i].OutputTokensPerSecond *= tt.multiplier
			}
			capacityPriorWriteCatalog(t, cm, document)
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			result, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			if err != nil {
				t.Fatal(err)
			}
			actual := result.Algorithms["server"].TokenThroughput.ServerTargetTokensPerSecond
			if actual.Cmp(*capacityPriorQuantity(tt.want)) != 0 {
				t.Fatalf("capacity = %v, want %s", actual, tt.want)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorTopology(t *testing.T) {
	tests := []struct {
		name, engine string
		args         []string
		gpus         string
		wantTP       int
		wantError    bool
	}{
		{name: "vllm TP inside one pod", engine: workercore.ModelDeploymentEngineVLLM, args: []string{"--tensor-parallel-size=2"}, gpus: "2", wantTP: 2},
		{name: "vllm underscore matches engine", engine: workercore.ModelDeploymentEngineVLLM, args: []string{"--tensor_parallel_size", "2"}, gpus: "2", wantTP: 2},
		{name: "sglang TP inside one pod", engine: workercore.ModelDeploymentEngineSGLang, args: []string{"--tp-size=2"}, gpus: "2", wantTP: 2},
		{name: "external local DP sentinel", engine: workercore.ModelDeploymentEngineVLLM, args: []string{"--data-parallel-size-local=0"}, gpus: "1", wantError: true},
		{name: "fractional accelerator", engine: workercore.ModelDeploymentEngineVLLM, gpus: "0.5", wantError: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			role := &workercore.ModelDeploymentRole{Name: "server", ExtraArgs: tt.args, Resources: &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity(tt.gpus)}}
			topology, err := modelDeploymentScalerPriorTopology(tt.engine, role)
			if (err != nil) != tt.wantError {
				t.Fatalf("topology error = %v, wantError %v", err, tt.wantError)
			}
			if err == nil && topology.TensorParallel != tt.wantTP {
				t.Fatalf("TP = %d, want %d", topology.TensorParallel, tt.wantTP)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorRoleSpecificFields(t *testing.T) {
	tests := []struct {
		name       string
		kind       workercore.ModelDeploymentRoleKind
		predictive bool
	}{
		{name: "prefill token", kind: workercore.ModelDeploymentRoleKindPrefill},
		{name: "prefill predictive", kind: workercore.ModelDeploymentRoleKindPrefill, predictive: true},
		{name: "decode token", kind: workercore.ModelDeploymentRoleKindDecode},
		{name: "decode predictive", kind: workercore.ModelDeploymentRoleKindDecode, predictive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			document := capacityPriorReadCatalog(t, cm)
			profile := &document.Profiles[0]
			md.Spec.Roles[0].Kind = tt.kind
			token := scaler.Spec.Algorithm.TokenThroughput
			// THE ROLE OF THE PROFILE PICKS THE RATE THE READER TAKES FROM THE
			// DOCUMENT, because the document holds the three rates the
			// measurement produced.
			if tt.kind == workercore.ModelDeploymentRoleKindPrefill {
				profile.Role = string(priors.RoleKindPrefill)
				token.ITLTargetMilliseconds = nil
			} else {
				profile.Role = string(priors.RoleKindDecode)
				token.TTFTTargetMilliseconds = nil
			}
			capacityPriorWriteCatalog(t, cm, document)
			if tt.predictive {
				scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{TokenThroughput: *token}
				scaler.Spec.Algorithm.TokenThroughput = nil
				if tt.kind == workercore.ModelDeploymentRoleKindPrefill {
					scaler.Spec.Algorithm.Predictive.PrefillTokenCapacityPerReplica = capacityPriorQuantity("123")
				} else {
					scaler.Spec.Algorithm.Predictive.DecodeTokenCapacityPerReplica = capacityPriorQuantity("123")
				}
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			resolved, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			if err != nil {
				t.Fatal(err)
			}
			effective := resolved.Algorithms["server"]
			token = effective.TokenThroughput
			if tt.predictive {
				token = &effective.Predictive.TokenThroughput
			}
			fallback := token.DecodeTargetTokensPerSecond
			if tt.kind == workercore.ModelDeploymentRoleKindPrefill {
				fallback = token.PrefillTargetTokensPerSecond
			}
			if fallback == nil || fallback.Cmp(*capacityPriorQuantity("1500")) != 0 {
				t.Fatalf("role fallback = %v", fallback)
			}
			if tt.predictive {
				primary := effective.Predictive.DecodeTokenCapacityPerReplica
				if tt.kind == workercore.ModelDeploymentRoleKindPrefill {
					primary = effective.Predictive.PrefillTokenCapacityPerReplica
				}
				if primary.Cmp(*capacityPriorQuantity("123")) != 0 {
					t.Fatalf("explicit primary = %v", primary)
				}
			}
			if len(resolved.Status[0].LatencyBounds) != 1 {
				t.Fatalf("role latency evidence = %#v", resolved.Status[0].LatencyBounds)
			}
		})
	}
}
