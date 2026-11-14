package worker

import (
	"context"
	"errors"
	"math/big"
	"reflect"
	"strings"
	"testing"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
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
			wantShape := "aggregated/test/model/none/vllm/nvidia-h100/gpus=1,tp=1,ep=1,pp=1,dp=1,ldp=1,cp=1"
			if status.WorkloadID != wantShape {
				t.Fatalf("workload id = %q, want %q", status.WorkloadID, wantShape)
			}
		})
	}
}

func capacityPriorFixture(t *testing.T) (*workercore.ModelDeploymentScaler, *workercore.ModelDeployment, *core.ConfigMap, *workerapi.AcceleratorType) {
	t.Helper()
	imageDigest := "sha256:" + strings.Repeat("a", 64)
	engineImage := "vllm/vllm-openai@" + imageDigest
	modelName := "test/model"
	args := []string{"--revision=weights-rev", "--tokenizer=test/model", "--dtype=bfloat16", "--kv-cache-dtype=auto", "--no-enable-prefix-caching", "--cpu-offload-gb=0", "--swap-space=0"}
	// THE FIXTURE WRITES THE CATALOG IN THE FORMAT THE OPERATOR SHIPS, because a
	// fixture in another format proves nothing about the format the operator
	// reads.
	document := priors.NewCatalogDocument(priors.ProfileDocument{
		Mode:         string(priors.ModeAggregated),
		Model:        modelName,
		Quantization: "none",
		Engine:       "vllm",
		Hardware:     "nvidia-h100",
		Topology:     &priors.TopologyDocument{GPUsPerReplica: 1, TensorParallel: 1},
	})
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
			TTFTMilliseconds:      priors.LatencyQuantiles{"p90": ttft * 0.9, "p95": ttft},
			ITLMilliseconds:       priors.LatencyQuantiles{"p90": itl * 0.9, "p95": itl},
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
	md.Spec.Roles[0].AcceleratorType = "test-hardware"
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
	instance := &workerapi.AcceleratorType{ObjectMeta: meta.ObjectMeta{Name: "test-hardware"}, Status: workercore.AcceleratorTypeStatus{Detail: workercore.InstanceTypeDetail{Product: "H100", InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{Memory: "80Gi"}}}}
	return scaler, md, cm, instance
}

// capacityPriorCompleteFixture builds the fixture of a catalog that states the
// environment of the measurement.
//
// IT IS THE PLAIN FIXTURE, because the catalog the operator ships states no
// environment, and a catalog that states none constrains the deployment on
// none.
func capacityPriorCompleteFixture(t *testing.T) (*workercore.ModelDeploymentScaler, *workercore.ModelDeployment, *core.ConfigMap, *workerapi.AcceleratorType) {
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
		{name: "unknown role kind", field: "roleKind", want: "not supported"},
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
			case "roleKind":
				role.Kind = workercore.ModelDeploymentRoleKind("Custom")
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
	second.Topology = &priors.TopologyDocument{GPUsPerReplica: 2, TensorParallel: 1}
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
		{name: "hardware", kind: "*v1.AcceleratorType"},
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

func TestModelDeploymentScalerCapacityPriorSingleHalfRefusal(t *testing.T) {
	tests := []struct {
		name       string
		kind       workercore.ModelDeploymentRoleKind
		predictive bool
	}{
		{name: "prefill-only token refuses catalog lookup", kind: workercore.ModelDeploymentRoleKindPrefill},
		{name: "prefill-only predictive refuses catalog lookup", kind: workercore.ModelDeploymentRoleKindPrefill, predictive: true},
		{name: "decode-only token refuses catalog lookup", kind: workercore.ModelDeploymentRoleKindDecode},
		{name: "decode-only predictive refuses catalog lookup", kind: workercore.ModelDeploymentRoleKindDecode, predictive: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			scaler, md, cm, instance := capacityPriorCompleteFixture(t)
			md.Spec.Roles[0].Kind = tt.kind
			if tt.predictive {
				scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: *scaler.Spec.Algorithm.TokenThroughput,
				}
				scaler.Spec.Algorithm.TokenThroughput = nil
			}
			client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(cm, instance).Build()
			_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
			var refusal *priors.RefusalError
			if !errors.As(err, &refusal) || refusal.Reason != priors.RefusalCapacityPriorUnresolved ||
				!strings.Contains(err.Error(), "requires both Prefill and Decode") {
				t.Fatalf("err = %v, want RefusalCapacityPriorUnresolved requiring both halves", err)
			}
		})
	}
}

func capacityPriorDisaggregatedFixture(t *testing.T, includeEncode bool) (*workercore.ModelDeploymentScaler, *workercore.ModelDeployment, *core.ConfigMap, []*workerapi.AcceleratorType) {
	t.Helper()
	scaler, md, cm, instance := capacityPriorCompleteFixture(t)
	instance.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
	h200 := instance.DeepCopy()
	h200.Name = "test-h200"
	h200.Status.Detail.Product = "H200"
	h200.Status.Detail.Memory = "140Gi"

	args := append([]string{}, md.Spec.Roles[0].ExtraArgs...)
	args = append(args, "--tensor-parallel-size=2")

	roles := []workercore.ModelDeploymentRole{
		{
			Name:            "prefill",
			Kind:            workercore.ModelDeploymentRoleKindPrefill,
			Replicas:        1,
			Image:           md.Spec.Engine.Image,
			ExtraArgs:       slicesCloneStrings(args),
			AcceleratorType: instance.Name,
			Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("2")},
		},
		{
			Name:            "decode",
			Kind:            workercore.ModelDeploymentRoleKindDecode,
			Replicas:        3,
			Image:           md.Spec.Engine.Image,
			ExtraArgs:       append(slicesCloneStrings(md.Spec.Roles[0].ExtraArgs), "--tensor-parallel-size=4"),
			AcceleratorType: h200.Name,
			Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("4")},
		},
	}
	scalerRoles := []workercore.ModelDeploymentScalerRoleBounds{
		{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
		{Name: "decode", MinReplicas: 3, MaxReplicas: 9},
	}
	ratios := []workercore.ModelDeploymentScalerRoleRatio{
		{
			NumeratorRole:   "decode",
			DenominatorRole: "prefill",
			Min:             capacityPriorQuantity("3"),
			Max:             capacityPriorQuantity("3"),
		},
	}
	if includeEncode {
		roles = append([]workercore.ModelDeploymentRole{{
			Name:            "encode",
			Kind:            workercore.ModelDeploymentRoleKindEncode,
			Replicas:        1,
			Image:           md.Spec.Engine.Image,
			AcceleratorType: instance.Name,
			Resources:       &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("1")},
		}}, roles...)
		scalerRoles = append([]workercore.ModelDeploymentScalerRoleBounds{
			{Name: "encode", MinReplicas: 1, MaxReplicas: 3},
		}, scalerRoles...)
		ratios = []workercore.ModelDeploymentScalerRoleRatio{
			{
				NumeratorRole:   "encode",
				DenominatorRole: "prefill",
				Min:             capacityPriorQuantity("1"),
				Max:             capacityPriorQuantity("1"),
			},
			{
				NumeratorRole:   "decode",
				DenominatorRole: "prefill",
				Min:             capacityPriorQuantity("2"),
				Max:             capacityPriorQuantity("2"),
			},
		}
		roles[2].Replicas = 2
		scalerRoles[2].MinReplicas = 2
		scalerRoles[2].MaxReplicas = 6
	}

	md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	md.Spec.KVTransfer = &workercore.ModelDeploymentKVTransfer{Backend: workercore.ModelDeploymentKVTransferBackendNIXL}
	md.Spec.Roles = roles
	delete(md.Annotations, workercore.ModelDeploymentScalerHardwareIDAnnotationKey)

	scaler.Spec.Roles = scalerRoles
	scaler.Spec.Ratios = ratios

	serverDoc := capacityPriorReadCatalog(t, cm)
	prefillServer := serverDoc.Profiles[0]
	prefillServer.Mode = string(priors.ModeAggregated)
	prefillServer.Hardware = "nvidia-h100"
	prefillServer.Topology = &priors.TopologyDocument{GPUsPerReplica: 2, TensorParallel: 2}
	prefillServer.Points = nil
	// Under headroom=0.75, feasible boundary=1600 -> effective=1200 -> 600 tok/s/GPU.
	for i, tp := range []float64{1000, 1600, 1800} {
		ttft, itl := 200.0, 20.0
		if i == 2 {
			ttft, itl = 800, 80
		}
		prefillServer.Points = append(prefillServer.Points, priors.PointDocument{
			Concurrency:           i + 1,
			TotalTokensPerSecond:  tp,
			InputTokensPerSecond:  tp,
			OutputTokensPerSecond: tp,
			TTFTMilliseconds:      priors.LatencyQuantiles{"p95": ttft},
			ITLMilliseconds:       priors.LatencyQuantiles{"p95": itl},
		})
	}

	decodeServer := serverDoc.Profiles[0]
	decodeServer.Mode = string(priors.ModeAggregated)
	decodeServer.Hardware = "nvidia-h200"
	decodeServer.Topology = &priors.TopologyDocument{GPUsPerReplica: 4, TensorParallel: 4}
	decodeServer.Points = nil
	// Under headroom=0.75, feasible boundary=2000 -> effective=1500 -> 375 tok/s/GPU.
	for i, tp := range []float64{1200, 2000, 2400} {
		ttft, itl := 200.0, 20.0
		if i == 2 {
			ttft, itl = 800, 80
		}
		decodeServer.Points = append(decodeServer.Points, priors.PointDocument{
			Concurrency:           i + 1,
			TotalTokensPerSecond:  tp,
			InputTokensPerSecond:  tp,
			OutputTokensPerSecond: tp,
			TTFTMilliseconds:      priors.LatencyQuantiles{"p95": ttft},
			ITLMilliseconds:       priors.LatencyQuantiles{"p95": itl},
		})
	}

	doc := priors.NewCatalogDocument(prefillServer, decodeServer)
	capacityPriorWriteCatalog(t, cm, doc)
	return scaler, md, cm, []*workerapi.AcceleratorType{instance, h200}
}

func slicesCloneStrings(s []string) []string {
	out := make([]string, len(s))
	copy(out, s)
	return out
}

func TestSolveFixedWholeUnitVector(t *testing.T) {
	baseTarget := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{
				{Name: "encode", Kind: workercore.ModelDeploymentRoleKindEncode},
				{Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill},
				{Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode},
			},
		},
	}

	cases := []struct {
		name      string
		target    *workercore.ModelDeployment
		roles     []workercore.ModelDeploymentScalerRoleBounds
		ratios    []workercore.ModelDeploymentScalerRoleRatio
		wantUnit  map[string]int32
		wantMinK  int32
		wantMaxK  int32
		wantError string
	}{
		{
			name: "1P:3D",
			target: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{Roles: baseTarget.Spec.Roles[1:]},
			},
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
				{Name: "decode", MinReplicas: 3, MaxReplicas: 9},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("3"), Max: capacityPriorQuantity("3")},
			},
			wantUnit: map[string]int32{"prefill": 1, "decode": 3},
			wantMinK: 1,
			wantMaxK: 3,
		},
		{
			name: "0.5 equals 500m yields 2P:1D",
			target: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{Roles: baseTarget.Spec.Roles[1:]},
			},
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "prefill", MinReplicas: 2, MaxReplicas: 6},
				{Name: "decode", MinReplicas: 1, MaxReplicas: 4},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("0.5"), Max: capacityPriorQuantity("500m")},
			},
			wantUnit: map[string]int32{"prefill": 2, "decode": 1},
			wantMinK: 1,
			wantMaxK: 3,
		},
		{
			name:   "1E:1P:2D consistent cycle with reversed and duplicate edges",
			target: baseTarget,
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "encode", MinReplicas: 1, MaxReplicas: 5},
				{Name: "prefill", MinReplicas: 2, MaxReplicas: 5},
				{Name: "decode", MinReplicas: 4, MaxReplicas: 10},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "prefill", DenominatorRole: "encode", Min: capacityPriorQuantity("1"), Max: capacityPriorQuantity("1")},
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("2"), Max: capacityPriorQuantity("2")},
				{NumeratorRole: "encode", DenominatorRole: "decode", Min: capacityPriorQuantity("0.5"), Max: capacityPriorQuantity("500m")},
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("2"), Max: capacityPriorQuantity("2")},
			},
			wantUnit: map[string]int32{"encode": 1, "prefill": 1, "decode": 2},
			wantMinK: 2,
			wantMaxK: 5,
		},
		{
			name:   "contradictory cycle is refused",
			target: baseTarget,
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "encode", MinReplicas: 1, MaxReplicas: 5},
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 5},
				{Name: "decode", MinReplicas: 2, MaxReplicas: 10},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "prefill", DenominatorRole: "encode", Min: capacityPriorQuantity("1"), Max: capacityPriorQuantity("1")},
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("2"), Max: capacityPriorQuantity("2")},
				{NumeratorRole: "decode", DenominatorRole: "encode", Min: capacityPriorQuantity("3"), Max: capacityPriorQuantity("3")},
			},
			wantError: "inconsistent cycle",
		},
		{
			name:   "disconnected role is refused",
			target: baseTarget,
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "encode", MinReplicas: 1, MaxReplicas: 5},
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 5},
				{Name: "decode", MinReplicas: 2, MaxReplicas: 10},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("2"), Max: capacityPriorQuantity("2")},
			},
			wantError: "connect all",
		},
		{
			name: "loose ratio min != max is refused",
			target: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{Roles: baseTarget.Spec.Roles[1:]},
			},
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 5},
				{Name: "decode", MinReplicas: 1, MaxReplicas: 10},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("1"), Max: capacityPriorQuantity("3")},
			},
			wantError: "min == max",
		},
		{
			name: "empty integer multiplier interval is refused",
			target: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{Roles: baseTarget.Spec.Roles[1:]},
			},
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 1},
				{Name: "decode", MinReplicas: 4, MaxReplicas: 6},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("3"), Max: capacityPriorQuantity("3")},
			},
			wantError: "multiplier",
		},
		{
			name: "int32 overflow on canonical vector is refused",
			target: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{Roles: baseTarget.Spec.Roles[1:]},
			},
			roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "prefill", MinReplicas: 1, MaxReplicas: 1000000000},
				{Name: "decode", MinReplicas: 1, MaxReplicas: 1000000000},
			},
			ratios: []workercore.ModelDeploymentScalerRoleRatio{
				{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("3000000000"), Max: capacityPriorQuantity("3000000000")},
			},
			wantError: "int32",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			scaler := scalerTestScaler("test", "uid", workercore.ModelDeploymentScalerModeActive)
			scaler.Spec.Roles = tc.roles
			scaler.Spec.Ratios = tc.ratios
			unit, kMin, kMax, err := solveFixedWholeUnitVector(scaler, tc.target)
			if tc.wantError != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantError) {
					t.Fatalf("err = %v, want substring %q", err, tc.wantError)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !reflect.DeepEqual(unit, tc.wantUnit) || kMin != tc.wantMinK || kMax != tc.wantMaxK {
				t.Fatalf("got unit=%v k=[%d,%d], want unit=%v k=[%d,%d]", unit, kMin, kMax, tc.wantUnit, tc.wantMinK, tc.wantMaxK)
			}
		})
	}
}

func capacityPriorDisaggregatedProfile(
	modelName string, rP int, hwP string, tpP, rD int, hwD string, tpD int,
) priors.ProfileDocument {
	prof := priors.ProfileDocument{
		Mode:         string(priors.ModeDisaggregated),
		Model:        modelName,
		Quantization: "none",
		Engine:       "vllm",
		Prefill: &priors.StageDocument{
			Replicas: rP,
			Hardware: hwP,
			Topology: priors.TopologyDocument{GPUsPerReplica: tpP, TensorParallel: tpP},
		},
		Decode: &priors.StageDocument{
			Replicas: rD,
			Hardware: hwD,
			Topology: priors.TopologyDocument{GPUsPerReplica: tpD, TensorParallel: tpD},
		},
	}
	// Under headroom=0.75, feasible boundary=4000 -> effective=3000 per measured PD unit.
	for i, tp := range []float64{2000, 4000, 4800} {
		ttft, itl := 200.0, 20.0
		if i == 2 {
			ttft, itl = 800, 80
		}
		prof.Points = append(prof.Points, priors.PointDocument{
			Concurrency:           i + 1,
			TotalTokensPerSecond:  tp,
			InputTokensPerSecond:  tp,
			OutputTokensPerSecond: tp,
			TTFTMilliseconds:      priors.LatencyQuantiles{"p95": ttft},
			ITLMilliseconds:       priors.LatencyQuantiles{"p95": itl},
		})
	}
	return prof
}

func TestModelDeploymentScalerCapacityPriorDisaggregatedTiers(t *testing.T) {
	t.Run("manual bypass skips catalog reads on PD deployment", func(t *testing.T) {
		scaler, md, _, _ := capacityPriorDisaggregatedFixture(t, false)
		scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
		scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
			TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
				TTFTTargetMilliseconds:       capacityPriorQuantity("500"),
				ITLTargetMilliseconds:        capacityPriorQuantity("50"),
				PrefillTargetTokensPerSecond: capacityPriorQuantity("1000"),
				DecodeTargetTokensPerSecond:  capacityPriorQuantity("2000"),
			},
			PrefillTokenCapacityPerReplica: capacityPriorQuantity("1100"),
			DecodeTokenCapacityPerReplica:  capacityPriorQuantity("2100"),
		}
		scaler.Spec.Algorithm.TokenThroughput = nil
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), capacityPriorNoReads{t: t}, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregated || got.WholeUnit != nil {
			t.Fatalf("unexpected mode = %q wholeUnit = %#v", got.Mode, got.WholeUnit)
		}
	})

	t.Run("Priority 1 resolves exact mode disaggregated on pure PD 1P:2D", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		scaler.Spec.Ratios[0].Min = capacityPriorQuantity("2")
		scaler.Spec.Ratios[0].Max = capacityPriorQuantity("2")
		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 2, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeDisaggregated || got.WholeUnit == nil {
			t.Fatalf("unexpected Priority 1 result: mode=%q wholeUnit=%#v", got.Mode, got.WholeUnit)
		}
		// 1P*2 + 2D*4 = 10 GPUs per unit; pdScale=1, eff=3000 -> PerGPU=300, C_unit=3000.
		if got.WholeUnit.UnitGPUs != 10 || got.WholeUnit.UnitTargetTokensPerSecond != 3000 ||
			got.WholeUnit.PerGPUTokensPerSecond != 300 {
			t.Fatalf("unexpected WholeUnit = %#v", got.WholeUnit)
		}
		if len(got.Status) != 2 || got.Status[0].CompatibilityFingerprint != got.Status[1].CompatibilityFingerprint {
			t.Fatalf("expected 2 statuses sharing joint fingerprint, got %#v", got.Status)
		}
		if got.Status[0].GPUsPerReplica != 2 || got.Status[0].MeasuredReplicas != 1 ||
			got.Status[1].GPUsPerReplica != 4 || got.Status[1].MeasuredReplicas != 2 {
			t.Fatalf("unexpected stage geometry in status: prefill=%#v decode=%#v", got.Status[0], got.Status[1])
		}
		if got.Status[0].SeedTokensPerSecond.Cmp(*capacityPriorQuantity("4000")) != 0 ||
			got.Status[1].SeedTokensPerSecond.Cmp(*capacityPriorQuantity("4000")) != 0 {
			t.Fatalf("unexpected disaggregated seeds: %#v / %#v", got.Status[0].SeedTokensPerSecond, got.Status[1].SeedTokensPerSecond)
		}
	})

	t.Run("Priority 1 resolves EPD 1E:2P:4D against 1P:2D profile with pdScale 2", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, true)
		// Ratios: encode:prefill = 1:2, decode:prefill = 2:1 -> unit = {encode:1, prefill:2, decode:4}.
		scaler.Spec.Ratios = []workercore.ModelDeploymentScalerRoleRatio{
			{NumeratorRole: "encode", DenominatorRole: "prefill", Min: capacityPriorQuantity("0.5"), Max: capacityPriorQuantity("0.5")},
			{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("2"), Max: capacityPriorQuantity("2")},
		}
		scaler.Spec.Roles[0].MinReplicas = 1
		scaler.Spec.Roles[0].MaxReplicas = 4
		scaler.Spec.Roles[1].MinReplicas = 2
		scaler.Spec.Roles[1].MaxReplicas = 6
		scaler.Spec.Roles[2].MinReplicas = 4
		scaler.Spec.Roles[2].MaxReplicas = 12
		scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
		scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
			TokenThroughput: *scaler.Spec.Algorithm.TokenThroughput,
		}
		scaler.Spec.Algorithm.TokenThroughput = nil

		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 2, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeDisaggregated || got.WholeUnit == nil {
			t.Fatalf("expected Disaggregated mode, got %q", got.Mode)
		}
		// pdScale = gcd(2,4) = 2; cUnit = 2 * 3000 = 6000; seed = 2 * 4000 = 8000; UnitGPUs = 2*2 + 4*4 = 20.
		if got.WholeUnit.EncodeRole != "encode" || got.WholeUnit.UnitGPUs != 20 ||
			got.WholeUnit.UnitTargetTokensPerSecond != 6000 || got.WholeUnit.PerGPUTokensPerSecond != 300 {
			t.Fatalf("unexpected scaled EPD WholeUnit = %#v", got.WholeUnit)
		}
		if got.Status[0].MeasuredReplicas != 1 || got.Status[1].MeasuredReplicas != 2 {
			t.Fatalf("expected reduced measured replicas (1,2), got (%d,%d)", got.Status[0].MeasuredReplicas, got.Status[1].MeasuredReplicas)
		}
		if got.Status[0].SeedTokensPerSecond.Cmp(*capacityPriorQuantity("8000")) != 0 {
			t.Fatalf("expected scaled seed 8000, got %v", got.Status[0].SeedTokensPerSecond)
		}
		if got.Status[0].Fields[0].Field != "predictive.unitTokenCapacityPerReplica" ||
			got.Status[0].Fields[0].EffectiveTokensPerSecond.Cmp(*capacityPriorQuantity("6000")) != 0 {
			t.Fatalf("unexpected predictive unit fields: %#v", got.Status[0].Fields)
		}
	})

	t.Run("Priority 1 fails closed without falling back to mode aggregated", func(t *testing.T) {
		for _, mode := range []string{"ambiguous", "hardware-annotation", "slo"} {
			t.Run(mode, func(t *testing.T) {
				scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
				scaler.Spec.Ratios[0].Min = capacityPriorQuantity("3")
				scaler.Spec.Ratios[0].Max = capacityPriorQuantity("3")
				doc := capacityPriorReadCatalog(t, cm)
				doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
					md.Spec.Model.Name,
					1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
				))
				want := "satisfies the latency targets"
				switch mode {
				case "ambiguous":
					ambiguous := doc.Profiles[2]
					ambiguous.Quantization = "bf16"
					doc.Profiles = append(doc.Profiles, ambiguous)
					delete(md.Annotations, workercore.ModelDeploymentScalerPrecisionAnnotationKey)
					delete(md.Annotations, workercore.ModelDeploymentScalerModelIDAnnotationKey)
					want = "several catalog profiles match"
				case "hardware-annotation":
					md.Annotations[workercore.ModelDeploymentScalerDecodeHardwareIDAnnotationKey] = "nvidia-h100"
					want = "hardware-id does not name the selected hardware"
				case "slo":
					for i := range doc.Profiles[2].Points {
						doc.Profiles[2].Points[i].ITLMilliseconds["p95"] = 9999
					}
				}
				capacityPriorWriteCatalog(t, cm, doc)

				client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
				_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
				var refusal *priors.RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), want) {
					t.Fatalf("expected fail-closed refusal containing %q without fallback, got %v", want, err)
				}
			})
		}

		for _, tc := range []struct {
			name   string
			mutate func(md *workercore.ModelDeployment)
			want   string
		}{
			{
				name: "prefill launch arg prefixCaching mismatch",
				mutate: func(md *workercore.ModelDeployment) {
					for i := range md.Spec.Roles {
						if md.Spec.Roles[i].Name == "prefill" {
							md.Spec.Roles[i].ExtraArgs = append(md.Spec.Roles[i].ExtraArgs, "--enable-prefix-caching")
						}
					}
				},
				want: "servingConfig.prefixCaching",
			},
			{
				name: "decode router mismatch",
				mutate: func(md *workercore.ModelDeployment) {
					md.Spec.Router.Name = workercore.ModelDeploymentRouterVLLM
				},
				want: "servingConfig.router",
			},
			{
				name: "decode stage launch parameter conflict",
				mutate: func(md *workercore.ModelDeployment) {
					for i := range md.Spec.Roles {
						if md.Spec.Roles[i].Name == "decode" {
							md.Spec.Roles[i].ExtraArgs = append(md.Spec.Roles[i].ExtraArgs, "--max-model-len=8192")
						}
					}
				},
				want: "servingConfig.launchParameters",
			},
		} {
			t.Run(tc.name, func(t *testing.T) {
				scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
				scaler.Spec.Ratios[0].Min = capacityPriorQuantity("3")
				scaler.Spec.Ratios[0].Max = capacityPriorQuantity("3")
				disaggRaw, err := priors.MarshalCatalogDocument(priors.NewCatalogDocument(capacityPriorDisaggregatedProfile(
					md.Spec.Model.Name, 1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
				)))
				if err != nil {
					t.Fatal(err)
				}
				disaggCatalog, err := priors.ParseCatalog(disaggRaw)
				if err != nil {
					t.Fatal(err)
				}
				aggCatalog, err := priors.ParseCatalog([]byte(cm.Data[priors.DefaultCatalogConfigMapKey]))
				if err != nil {
					t.Fatal(err)
				}
				disaggProf := disaggCatalog.Profiles[0]
				disaggProf.ServingConfig = priors.ServingConfig{
					Router:              workercore.ModelDeploymentRouterLLMD,
					KVCacheDType:        "auto",
					Offload:             "none",
					PrefixCaching:       "disabled",
					SpeculativeDecoding: "none",
				}
				disaggProf.Decode.ServingConfig.LaunchParameters = []string{"--max-model-len=4096"}
				disaggCatalog.Profiles = append([]priors.Profile{disaggProf}, aggCatalog.Profiles...)

				tc.mutate(md)
				client := scalerTestClient(t, scaler, md, instances[0], instances[1])
				_, err = modelDeploymentScalerResolveDisaggregatedPriors(
					context.Background(), client, scaler, md, disaggCatalog, 0.95,
				)
				var refusal *priors.RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("expected fail-closed refusal containing %q without fallback, got %v", tc.want, err)
				}
			})
		}
	})

	t.Run("Priority 2 resolves 1P:3D when disaggregated profile is missing", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		doc := capacityPriorReadCatalog(t, cm)
		// Disaggregated profile has 1P:2D, while deployment is 1P:3D -> zero disaggregated matches -> Priority 2.
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 2, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregatedFallback || got.WholeUnit == nil {
			t.Fatalf("expected AggregatedFallback whole unit mode, got %q / %#v", got.Mode, got.WholeUnit)
		}
		// g_P=2, eff_P=1200 -> 600; g_D=4, eff_D=1500 -> 375.
		// c_gpu = 375, UnitGPUs = 1*2 + 3*4 = 14, C_unit = floor(14 * 375) = 5250.
		if got.WholeUnit.UnitGPUs != 14 || got.WholeUnit.MinUnits != 1 || got.WholeUnit.MaxUnits != 3 ||
			got.WholeUnit.PerGPUTokensPerSecond != 375 || got.WholeUnit.UnitTargetTokensPerSecond != 5250 {
			t.Fatalf("unexpected WholeUnit = %#v", got.WholeUnit)
		}
		if !strings.Contains(got.ConditionMessage, "GPU-time-balanced") {
			t.Fatalf("expected GPU-time-balanced fallback message, got %q", got.ConditionMessage)
		}
		if len(got.Status) != 2 || got.Status[0].CompatibilityFingerprint != got.Status[1].CompatibilityFingerprint {
			t.Fatalf("expected 2 statuses sharing joint compatibility fingerprint, got %#v", got.Status)
		}
		if got.Status[0].SeedTokensPerSecond.Cmp(*capacityPriorQuantity("1600")) != 0 ||
			got.Status[1].SeedTokensPerSecond.Cmp(*capacityPriorQuantity("2000")) != 0 {
			t.Fatalf("unexpected aggregated seeds: %#v / %#v", got.Status[0].SeedTokensPerSecond, got.Status[1].SeedTokensPerSecond)
		}
		for _, st := range got.Status {
			if len(st.Fields) != 1 || st.Fields[0].Field != "tokenThroughput.unitTargetTokensPerSecond" ||
				st.Fields[0].EffectiveTokensPerSecond.Cmp(*capacityPriorQuantity("5250")) != 0 {
				t.Fatalf("unexpected unit field status: %#v", st.Fields)
			}
		}
	})

	t.Run("Priority 2 resolves 1E:1P:2D Predictive when disaggregated profile is missing", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, true)
		scaler.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
		scaler.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
			TokenThroughput: *scaler.Spec.Algorithm.TokenThroughput,
		}
		scaler.Spec.Algorithm.TokenThroughput = nil

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregatedFallback || got.WholeUnit == nil {
			t.Fatalf("expected AggregatedFallback, got %q", got.Mode)
		}
		// UnitGPUs = 1*2 + 2*4 = 10, c_gpu = 375, C_unit = 3750.
		if got.WholeUnit.EncodeRole != "encode" || got.WholeUnit.UnitGPUs != 10 || got.WholeUnit.UnitTargetTokensPerSecond != 3750 {
			t.Fatalf("unexpected EPD WholeUnit = %#v", got.WholeUnit)
		}
		if got.Status[0].Fields[0].Field != "predictive.unitTokenCapacityPerReplica" ||
			got.Status[0].Fields[0].EffectiveTokensPerSecond.Cmp(*capacityPriorQuantity("3750")) != 0 {
			t.Fatalf("unexpected predictive unit status fields: %#v", got.Status[0].Fields)
		}
	})

	t.Run("Priority 2 resolves heterogeneous P/D quantizations via aggregated fallback", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		delete(md.Annotations, workercore.ModelDeploymentScalerPrecisionAnnotationKey)
		delete(md.Annotations, workercore.ModelDeploymentScalerModelIDAnnotationKey)
		md.Spec.Roles[0].ExtraArgs = append(md.Spec.Roles[0].ExtraArgs, "--quantization=fp8")
		md.Spec.Roles[1].ExtraArgs = append(md.Spec.Roles[1].ExtraArgs, "--quantization=fp4")
		scaler.Spec.Ratios[0].Min = capacityPriorQuantity("3")
		scaler.Spec.Ratios[0].Max = capacityPriorQuantity("3")

		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles[0].Quantization = "fp8"
		doc.Profiles[1].Quantization = "fp4"
		disagg := capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
		)
		disagg.Quantization = "fp8"
		doc.Profiles = append(doc.Profiles, disagg)
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregatedFallback || got.WholeUnit == nil {
			t.Fatalf("expected AggregatedFallback on conflicting P/D quantizations, got %q", got.Mode)
		}
		if got.WholeUnit.UnitTargetTokensPerSecond != 5250 {
			t.Fatalf("unexpected heterogenous fallback C_unit = %v, want 5250", got.WholeUnit.UnitTargetTokensPerSecond)
		}
	})

	t.Run("disaggregated whole-unit prerequisite refusals", func(t *testing.T) {
		cases := []struct {
			name string
			edit func(t *testing.T, scaler *workercore.ModelDeploymentScaler, md *workercore.ModelDeployment, cm *core.ConfigMap)
			want string
		}{
			{
				name: "custom catalog does not fall back to embedded",
				edit: func(_ *testing.T, scaler *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, _ *core.ConfigMap) {
					scaler.Spec.Algorithm.CapacityPrior.CatalogRef.Name = "missing-custom-cm"
				},
				want: "does not exist",
			},
			{
				name: "partial explicit override fails",
				edit: func(_ *testing.T, scaler *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, _ *core.ConfigMap) {
					scaler.Spec.Algorithm.TokenThroughput.PrefillTargetTokensPerSecond = capacityPriorQuantity("900")
				},
				want: "partial explicit",
			},
			{
				name: "missing ITL fails",
				edit: func(_ *testing.T, scaler *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, _ *core.ConfigMap) {
					scaler.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
				},
				want: "ITL",
			},
			{
				name: "loose ratio fails",
				edit: func(_ *testing.T, scaler *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, _ *core.ConfigMap) {
					scaler.Spec.Ratios[0].Max = capacityPriorQuantity("5")
				},
				want: "min == max",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
				tc.edit(t, scaler, md, cm)
				client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
				_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
				var refusal *priors.RefusalError
				if !errors.As(err, &refusal) || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("err = %v, want refusal containing %q", err, tc.want)
				}
			})
		}
	})
}

func TestModelDeploymentScalerCapacityPriorDisaggregatedFingerprint(t *testing.T) {
	scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, true)
	client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
	first, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		name    string
		changed bool
		mutate  func(nextScaler *workercore.ModelDeploymentScaler, nextMD *workercore.ModelDeployment, nextCM *core.ConfigMap)
	}{
		{
			name: "replica scaling retains joint fingerprint",
			mutate: func(_ *workercore.ModelDeploymentScaler, nextMD *workercore.ModelDeployment, _ *core.ConfigMap) {
				nextMD.Spec.Roles[0].Replicas = 2
				nextMD.Spec.Roles[1].Replicas = 2
				nextMD.Spec.Roles[2].Replicas = 4
			},
		},
		{
			name:    "ratio change invalidates joint fingerprint",
			changed: true,
			mutate: func(nextScaler *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, _ *core.ConfigMap) {
				nextScaler.Spec.Ratios[1].Min = capacityPriorQuantity("3")
				nextScaler.Spec.Ratios[1].Max = capacityPriorQuantity("3")
				nextScaler.Spec.Roles[2].MinReplicas = 3
			},
		},
		{
			name:    "encode config change invalidates joint fingerprint",
			changed: true,
			mutate: func(_ *workercore.ModelDeploymentScaler, nextMD *workercore.ModelDeployment, _ *core.ConfigMap) {
				nextMD.Spec.Roles[0].ExtraArgs = append(nextMD.Spec.Roles[0].ExtraArgs, "--max-num-seqs=16")
			},
		},
		{
			name:    "server seed change invalidates joint fingerprint",
			changed: true,
			mutate: func(_ *workercore.ModelDeploymentScaler, _ *workercore.ModelDeployment, nextCM *core.ConfigMap) {
				doc := capacityPriorReadCatalog(t, nextCM)
				doc.Profiles[0].Points[0].TotalTokensPerSecond = 1111
				doc.Profiles[0].Points[0].InputTokensPerSecond = 1111
				doc.Profiles[0].Points[0].OutputTokensPerSecond = 1111
				capacityPriorWriteCatalog(t, nextCM, doc)
			},
		},
		{
			name:    "mode switch from AggregatedFallback to Disaggregated invalidates joint fingerprint",
			changed: true,
			mutate: func(_ *workercore.ModelDeploymentScaler, nextMD *workercore.ModelDeployment, nextCM *core.ConfigMap) {
				doc := capacityPriorReadCatalog(t, nextCM)
				doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
					nextMD.Spec.Model.Name,
					1, "nvidia-h100", 2, 2, "nvidia-h200", 4,
				))
				capacityPriorWriteCatalog(t, nextCM, doc)
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			nextScaler, nextMD, nextCM := scaler.DeepCopy(), md.DeepCopy(), cm.DeepCopy()
			tc.mutate(nextScaler, nextMD, nextCM)
			nextClient := scalerTestClient(t, nextScaler, nextMD, nextCM, instances[0], instances[1])
			second, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), nextClient, nextScaler, nextMD)
			if err != nil {
				t.Fatal(err)
			}
			changed := first.WholeUnit.JointCompatibilityDigest != second.WholeUnit.JointCompatibilityDigest ||
				first.Status[0].CompatibilityFingerprint != second.Status[0].CompatibilityFingerprint ||
				first.Status[1].CompatibilityFingerprint != second.Status[1].CompatibilityFingerprint
			if changed != tc.changed {
				t.Fatalf("fingerprint changed = %v, want %v", changed, tc.changed)
			}
		})
	}
}

func TestModelDeploymentScalerCapacityPriorEmbeddedCatalog(t *testing.T) {
	b200_1x := &workerapi.AcceleratorType{
		ObjectMeta: meta.ObjectMeta{Name: "test-b200-1x"},
		Status: workercore.AcceleratorTypeStatus{
			Detail: workercore.InstanceTypeDetail{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Product:      "B200",
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
					Memory: "192Gi",
				},
			},
		},
	}
	b200_4x := b200_1x.DeepCopy()
	b200_4x.Name = "test-b200-4x"

	baseArgs := []string{"--quantization=fp4", "--kv-cache-dtype=auto", "--no-enable-prefix-caching", "--cpu-offload-gb=0", "--swap-space=0"}

	t.Run("embedded catalog resolves mode aggregated for Server", func(t *testing.T) {
		md := &workercore.ModelDeployment{
			ObjectMeta: meta.ObjectMeta{Name: "minimax-server", Namespace: "default"},
			Spec: workercore.ModelDeploymentSpec{
				Model:  workercore.ModelDeploymentModel{Name: "MiniMaxAI/MiniMax-M2.5"},
				Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:latest"},
				Roles: []workercore.ModelDeploymentRole{{
					Name: "server", Kind: workercore.ModelDeploymentRoleKindServer,
					AcceleratorType: b200_1x.Name, Replicas: 1,
					ExtraArgs: append(slicesCloneStrings(baseArgs), "--tensor-parallel-size=1", "--expert-parallel-size=1"),
					Resources: &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("1")},
				}},
			},
		}
		scaler := &workercore.ModelDeploymentScaler{
			ObjectMeta: meta.ObjectMeta{Name: "scaler-server", Namespace: "default"},
			Spec: workercore.ModelDeploymentScalerSpec{
				Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
					Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
					TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
						TTFTTargetMilliseconds: capacityPriorQuantity("1000"),
						ITLTargetMilliseconds:  capacityPriorQuantity("20"),
					},
					CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{},
				},
				Roles: []workercore.ModelDeploymentScalerRoleBounds{{Name: "server", MinReplicas: 1, MaxReplicas: 4}},
			},
		}
		client := scalerTestClient(t, scaler, md, b200_1x)
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregated || got.WholeUnit != nil {
			t.Fatalf("expected Aggregated mode, got %q", got.Mode)
		}
		if !strings.HasPrefix(got.Status[0].WorkloadID, "aggregated/") {
			t.Fatalf("expected aggregated workload ID, got %q", got.Status[0].WorkloadID)
		}
	})

	t.Run("embedded catalog resolves mode disaggregated for PD", func(t *testing.T) {
		md := &workercore.ModelDeployment{
			ObjectMeta: meta.ObjectMeta{Name: "minimax-pd", Namespace: "default"},
			Spec: workercore.ModelDeploymentSpec{
				Model:  workercore.ModelDeploymentModel{Name: "MiniMaxAI/MiniMax-M2.5"},
				Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM, Image: "vllm/vllm-openai:latest"},
				Roles: []workercore.ModelDeploymentRole{
					{
						Name: "prefill", Kind: workercore.ModelDeploymentRoleKindPrefill,
						AcceleratorType: b200_1x.Name, Replicas: 1,
						ExtraArgs: append(slicesCloneStrings(baseArgs), "--tensor-parallel-size=1", "--expert-parallel-size=1"),
						Resources: &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("1")},
					},
					{
						Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
						AcceleratorType: b200_4x.Name, Replicas: 1,
						ExtraArgs: append(slicesCloneStrings(baseArgs), "--tensor-parallel-size=4", "--expert-parallel-size=1"),
						Resources: &workercore.ModelDeploymentRoleResources{Accelerator: capacityPriorQuantity("4")},
					},
				},
			},
		}
		scaler := &workercore.ModelDeploymentScaler{
			ObjectMeta: meta.ObjectMeta{Name: "scaler-pd", Namespace: "default"},
			Spec: workercore.ModelDeploymentScalerSpec{
				Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
					Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
					TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
						TTFTTargetMilliseconds: capacityPriorQuantity("1000"),
						ITLTargetMilliseconds:  capacityPriorQuantity("20"),
					},
					CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{},
				},
				Roles: []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "prefill", MinReplicas: 1, MaxReplicas: 4},
					{Name: "decode", MinReplicas: 1, MaxReplicas: 4},
				},
				Ratios: []workercore.ModelDeploymentScalerRoleRatio{
					{NumeratorRole: "decode", DenominatorRole: "prefill", Min: capacityPriorQuantity("1"), Max: capacityPriorQuantity("1")},
				},
			},
		}
		client := scalerTestClient(t, scaler, md, b200_1x, b200_4x)
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatal(err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeDisaggregated || got.WholeUnit == nil {
			t.Fatalf("expected Disaggregated mode from embedded catalog, got %q / %#v", got.Mode, got.WholeUnit)
		}
		if !strings.HasPrefix(got.Status[0].WorkloadID, "disaggregated/") {
			t.Fatalf("expected disaggregated workload ID, got %q", got.Status[0].WorkloadID)
		}
		if got.WholeUnit.UnitGPUs != 5 {
			t.Fatalf("UnitGPUs = %d, want 5", got.WholeUnit.UnitGPUs)
		}
	})
}

func TestModelDeploymentScalerCapacityPriorExternalKVCacheRefusal(t *testing.T) {
	t.Run("single-role Server fails closed against YAML profile without external offload", func(t *testing.T) {
		scaler, md, cm, instance := capacityPriorCompleteFixture(t)
		md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "kv-pool"}}

		client := scalerTestClient(t, scaler, md, cm, instance)
		_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		var refusal *priors.RefusalError
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "servingConfig.offload") {
			t.Fatalf("err = %v, want RefusalError containing servingConfig.offload", err)
		}
	})

	t.Run("Priority 1 mode disaggregated fails closed without Priority 2 fallback against YAML profile", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "kv-pool"}}
		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		var refusal *priors.RefusalError
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "servingConfig.offload") {
			t.Fatalf("err = %v, want RefusalError containing servingConfig.offload", err)
		}

		// Verify Priority 1 fails closed even if disaggregated profile explicitly records offload=none while aggregated profiles are absent.
		raw := []byte(cm.Data[priors.DefaultCatalogConfigMapKey])
		parsedCatalog, parseErr := priors.ParseCatalog(raw)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		for i := range parsedCatalog.Profiles {
			if parsedCatalog.Profiles[i].Mode == priors.ModeDisaggregated {
				parsedCatalog.Profiles[i].ServingConfig.Offload = "none"
			}
		}
		_, err = modelDeploymentScalerResolveDisaggregatedPriors(
			context.Background(), client, scaler, md, parsedCatalog, 0.95,
		)
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "servingConfig.offload") {
			t.Fatalf("err = %v, want fail-closed RefusalError containing servingConfig.offload", err)
		}
	})

	t.Run("Priority 2 mode aggregated fallback fails closed against YAML profiles when spec.kvCache is set", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "kv-pool"}}

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		var refusal *priors.RefusalError
		if !errors.As(err, &refusal) || !strings.Contains(err.Error(), "servingConfig.offload") {
			t.Fatalf("err = %v, want RefusalError containing servingConfig.offload", err)
		}
	})

	t.Run("native kvTransfer without kvCache resolves unchanged", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		if md.Spec.KVTransfer == nil || md.Spec.KVCache != nil {
			t.Fatalf("expected KVTransfer!=nil and KVCache==nil in fixture")
		}
		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		if err != nil {
			t.Fatalf("unexpected error for native KVTransfer without KVCache: %v", err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregatedFallback {
			t.Fatalf("mode = %q, want AggregatedFallback", got.Mode)
		}
	})

	t.Run("explicit manual capacities with spec.kvCache bypass catalog unchanged", func(t *testing.T) {
		scaler, md, _, _ := capacityPriorDisaggregatedFixture(t, false)
		md.Spec.KVCache = &workercore.ModelDeploymentKVCache{PoolRef: core.LocalObjectReference{Name: "kv-pool"}}
		scaler.Spec.Algorithm.TokenThroughput.PrefillTargetTokensPerSecond = capacityPriorQuantity("1000")
		scaler.Spec.Algorithm.TokenThroughput.DecodeTargetTokensPerSecond = capacityPriorQuantity("2000")
		if ModelDeploymentScalerNeedsCapacityPrior(scaler, md) {
			t.Fatalf("expected ModelDeploymentScalerNeedsCapacityPrior == false with explicit capacities")
		}
		got, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), capacityPriorNoReads{t: t}, scaler, md)
		if err != nil {
			t.Fatalf("unexpected error with explicit capacities and spec.kvCache: %v", err)
		}
		if got.Mode != ModelDeploymentScalerPriorModeAggregated || got.WholeUnit != nil {
			t.Fatalf("unexpected mode=%q wholeUnit=%#v", got.Mode, got.WholeUnit)
		}
	})
}

func TestQuantityBoundedRatAndDisaggregatedIdentityGates(t *testing.T) {
	t.Run("bounded rational conversion rejects extreme exponents without mutating input Quantity", func(t *testing.T) {
		posScale := resource.MustParse("1e-10")
		posScale.AsDec().SetScale(1000)
		hugeBits := resource.MustParse("1e-10")
		hugeBits.AsDec().SetScale(0)
		hugeBits.AsDec().SetUnscaledBig(new(big.Int).Lsh(big.NewInt(1), 200))

		cases := []resource.Quantity{
			resource.MustParse("1e1000000000"),
			posScale,
			hugeBits,
			resource.MustParse("0"),
			resource.MustParse("-5"),
		}
		for i, q := range cases {
			before := q
			if _, err := quantityToPositiveRat(&q); err == nil {
				t.Fatalf("case %d (%#v) unexpectedly succeeded", i, q)
			}
			if !reflect.DeepEqual(q, before) {
				t.Fatalf("case %d mutated input Quantity: got %#v, want %#v", i, q, before)
			}
		}

		hugeQuantile := resource.MustParse("1e1000000000")
		beforeQuantile := hugeQuantile
		alg := &workercore.ModelDeploymentScalerAlgorithmSpec{
			Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
			TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
				LatencyQuantile: &hugeQuantile,
			},
		}
		if _, err := ModelDeploymentScalerLatencyQuantile(alg); err == nil {
			t.Fatal("expected huge LatencyQuantile to be rejected")
		}
		if hugeQuantile != beforeQuantile {
			t.Fatalf("ModelDeploymentScalerLatencyQuantile mutated input Quantity: got %#v, want %#v", hugeQuantile, beforeQuantile)
		}
	})

	t.Run("Priority 1 disaggregated refuses unrecognized decode image without falling back", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		for i := range md.Spec.Roles {
			if md.Spec.Roles[i].Kind == workercore.ModelDeploymentRoleKindDecode {
				md.Spec.Roles[i].Image = "registry.example/custom-runtime:v1"
			}
		}
		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)

		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		_, err := ResolveModelDeploymentScalerCapacityPriors(context.Background(), client, scaler, md)
		wantMsg := "the engine image of the role names no known engine family and does not match the selected profile image"
		if err == nil || !strings.Contains(err.Error(), wantMsg) {
			t.Fatalf("err = %v, want refusal mentioning %q", err, wantMsg)
		}
	})

	t.Run("disaggregated stage EngineEnv values are redacted and malformed stage entries are refused", func(t *testing.T) {
		profile := &priors.Profile{
			ServingConfig: priors.ServingConfig{EngineEnv: []string{"TOP_SECRET=top-val"}},
			Prefill:       &priors.DisaggregatedStage{ServingConfig: priors.ServingConfig{EngineEnv: []string{"PREFILL_SECRET=prefill-val"}}},
			Decode:        &priors.DisaggregatedStage{ServingConfig: priors.ServingConfig{EngineEnv: []string{"DECODE_SECRET=decode-val"}}},
		}
		values := modelDeploymentScalerEnvValues(&workercore.ModelDeploymentRole{}, profile)
		redacted := modelDeploymentScalerRedactEnvValues(
			"values top-val prefill-val decode-val must not leak",
			values,
		)
		for _, secret := range []string{"top-val", "prefill-val", "decode-val"} {
			if strings.Contains(redacted, secret) {
				t.Fatalf("redacted message %q leaked %q", redacted, secret)
			}
		}

		mergedMalformed := priors.MergeServingConfig(
			priors.ServingConfig{EngineEnv: []string{"BARE_ENTRY"}, EngineEnvComplete: true},
			priors.ServingConfig{EngineEnv: []string{"VALID=1"}},
		)
		if err := modelDeploymentScalerMatchEngineEnv(mergedMalformed.EngineEnv, []string{"VALID=1"}, true); err == nil ||
			!strings.Contains(err.Error(), "malformed engine environment entry") {
			t.Fatalf("err = %v, want malformed engine environment entry refusal", err)
		}
	})

	t.Run("disaggregated request refuses conflicting stage revisions, tokenizers, and builds", func(t *testing.T) {
		scaler, md, cm, instances := capacityPriorDisaggregatedFixture(t, false)
		doc := capacityPriorReadCatalog(t, cm)
		doc.Profiles = append(doc.Profiles, capacityPriorDisaggregatedProfile(
			md.Spec.Model.Name,
			1, "nvidia-h100", 2, 3, "nvidia-h200", 4,
		))
		capacityPriorWriteCatalog(t, cm, doc)
		parsed, err := priors.ParseCatalog([]byte(cm.Data[priors.DefaultCatalogConfigMapKey]))
		if err != nil {
			t.Fatal(err)
		}
		var disaggProfile *priors.Profile
		for i := range parsed.Profiles {
			if parsed.Profiles[i].Mode == priors.ModeDisaggregated {
				disaggProfile = &parsed.Profiles[i]
				break
			}
		}
		if disaggProfile == nil {
			t.Fatal("missing disaggregated profile")
		}
		delete(md.Annotations, workercore.ModelDeploymentScalerModelIDAnnotationKey)
		client := scalerTestClient(t, scaler, md, cm, instances[0], instances[1])
		prefillDecl, err := modelDeploymentScalerPriorDeclarationOf(context.Background(), client, md, &md.Spec.Roles[0])
		if err != nil {
			t.Fatal(err)
		}
		decodeDecl, err := modelDeploymentScalerPriorDeclarationOf(context.Background(), client, md, &md.Spec.Roles[1])
		if err != nil {
			t.Fatal(err)
		}

		cases := []struct {
			name   string
			mutate func(*priors.Profile, *modelDeploymentScalerPriorDeclaration, *modelDeploymentScalerPriorDeclaration)
			want   string
		}{
			{
				name: "conflicting model revisions",
				mutate: func(_ *priors.Profile, p, d *modelDeploymentScalerPriorDeclaration) {
					p.identity.Model.Revision = "rev-a"
					d.identity.Model.Revision = "rev-b"
				},
				want: "conflicting model revisions",
			},
			{
				name: "conflicting model tokenizers",
				mutate: func(prof *priors.Profile, p, d *modelDeploymentScalerPriorDeclaration) {
					prof.Identity.Model.Tokenizer = priors.UnspecifiedRevision
					p.identity.Model.Tokenizer = "org/tok-a"
					d.identity.Model.Tokenizer = "org/tok-b"
				},
				want: "conflicting model tokenizers",
			},
			{
				name: "conflicting engine builds",
				mutate: func(_ *priors.Profile, p, d *modelDeploymentScalerPriorDeclaration) {
					p.identity.Engine.Build = "build-1"
					d.identity.Engine.Build = "build-2"
				},
				want: "conflicting engine builds",
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				profCopy := *disaggProfile
				pCopy, dCopy := prefillDecl, decodeDecl
				tc.mutate(&profCopy, &pCopy, &dCopy)
				_, reqErr := modelDeploymentScalerDisaggregatedPriorRequestMatched(
					md, &md.Spec.Roles[0], &md.Spec.Roles[1], &profCopy, pCopy, dCopy, 1, 3,
				)
				if reqErr == nil || !strings.Contains(reqErr.Error(), tc.want) {
					t.Fatalf("err = %v, want %q", reqErr, tc.want)
				}
			})
		}
	})
}
