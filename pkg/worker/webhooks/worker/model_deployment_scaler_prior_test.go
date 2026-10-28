package worker

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	workerapi "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/scaling/priors"
)

type modelDeploymentScalerPriorFixture struct {
	scaler       *workercore.ModelDeploymentScaler
	target       *workercore.ModelDeployment
	catalog      *core.ConfigMap
	instance     *workerapi.InstanceType
	wideInstance *workerapi.InstanceType
	args         []string
	document     priors.CatalogDocument
}

func newModelDeploymentScalerPriorFixture(t *testing.T) modelDeploymentScalerPriorFixture {
	t.Helper()

	imageDigest := "sha256:" + strings.Repeat("a", 64)
	engineImage := "vllm/vllm-openai@" + imageDigest
	modelName := "Qwen/Qwen2.5-72B-Instruct"
	args := []string{
		"--revision=weights-rev-1",
		"--tokenizer=Qwen/Qwen2.5-72B-Instruct",
		"--dtype=bfloat16",
		"--kv-cache-dtype=auto",
		"--no-enable-prefix-caching",
		"--cpu-offload-gb=0",
		"--swap-space=0",
	}

	// THE FIXTURE WRITES THE CATALOG IN THE FORMAT THE OPERATOR READS, because a
	// fixture in another format proves nothing about the format the operator
	// ships.
	buildProfile := func(role priors.RoleKind, gpusPerReplica int, quantization string, baseThroughput float64) priors.ProfileDocument {
		document := priors.ProfileDocument{
			Role:         string(role),
			Model:        modelName,
			Quantization: quantization,
			Engine:       "vllm",
			Hardware:     "nvidia-h100",
			Topology:     priors.TopologyDocument{GPUsPerReplica: gpusPerReplica},
		}
		for i, mult := range []float64{1.0, 2.0, 2.1} {
			ttft, itl := float64(100*(i+1)), float64(10*(i+1))
			if i == 2 {
				ttft, itl = 800, 80
			}
			rate := baseThroughput * mult
			point := priors.PointDocument{
				Concurrency:           i + 1,
				TotalTokensPerSecond:  rate,
				InputTokensPerSecond:  rate,
				OutputTokensPerSecond: rate,
			}
			if role != priors.RoleKindDecode {
				point.TTFTMilliseconds = priors.LatencyQuantiles{"p95": ttft}
			}
			if role != priors.RoleKindPrefill {
				point.ITLMilliseconds = priors.LatencyQuantiles{"p95": itl}
			}
			document.Points = append(document.Points, point)
		}
		return document
	}

	document := priors.NewCatalogDocument(
		buildProfile(priors.RoleKindServer, 1, "none", 1000),
		buildProfile(priors.RoleKindPrefill, 1, "none", 1200),
		buildProfile(priors.RoleKindPrefill, 2, "none", 800),
		buildProfile(priors.RoleKindDecode, 1, "none", 600),
	)
	rawCatalog, err := priors.MarshalCatalogDocument(document)
	require.NoError(t, err)

	// THE PROFILES THE CASES USE ARE THE ONES THE READER SEES, because a
	// fixture that describes a profile the reader never builds proves nothing.
	parsed, err := priors.ParseCatalog(rawCatalog)
	require.NoError(t, err)
	require.Len(t, parsed.Profiles, 4)
	serverProfile := priorProfile(t, parsed, priors.RoleKindServer, 1)
	// THE ROLES SHARE ONE MODEL AND ONE HARDWARE, because the deployment states
	// its model and its hardware once and every role of it reads that one.
	for _, profile := range []priors.Profile{
		priorProfile(t, parsed, priors.RoleKindPrefill, 1),
		priorProfile(t, parsed, priors.RoleKindPrefill, 2),
		priorProfile(t, parsed, priors.RoleKindDecode, 1),
	} {
		require.Equal(t, serverProfile.Identity.Model, profile.Identity.Model)
		require.Equal(t, serverProfile.Identity.Hardware, profile.Identity.Hardware)
	}

	modelDigest, err := priors.Digest(serverProfile.Identity.Model)
	require.NoError(t, err)
	hardwareDigest, err := priors.Digest(serverProfile.Identity.Hardware)
	require.NoError(t, err)

	target := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "team-a",
			Name:      "qwen-72b",
			UID:       "target-uid",
			Annotations: map[string]string{
				workercore.ModelDeploymentScalerModelIDAnnotationKey:    "model:" + modelDigest,
				workercore.ModelDeploymentScalerHardwareIDAnnotationKey: "hardware:" + hardwareDigest,
				workercore.ModelDeploymentScalerPrecisionAnnotationKey:  "none",
			},
		},
		Spec: workercore.ModelDeploymentSpec{
			Model: workercore.ModelDeploymentModel{Name: modelName},
			Engine: workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineVLLM,
				Image: engineImage,
			},
			Roles: []workercore.ModelDeploymentRole{{
				Name:         "server",
				Kind:         workercore.ModelDeploymentRoleKindServer,
				Replicas:     1,
				InstanceType: "h100-1x",
				ExtraArgs:    args,
				Resources: &workercore.ModelDeploymentRoleResources{
					Accelerator: ptr.To(resource.MustParse("1")),
				},
			}},
		},
	}

	scaler := &workercore.ModelDeploymentScaler{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-scaler"},
		Spec: workercore.ModelDeploymentScalerSpec{
			TargetRef: workercore.ModelDeploymentScalerTargetReference{Name: "qwen-72b"},
			Roles: []workercore.ModelDeploymentScalerRoleBounds{
				{Name: "server", MinReplicas: 1, MaxReplicas: 4},
			},
			Algorithm: workercore.ModelDeploymentScalerAlgorithmSpec{
				Type: workercore.ModelDeploymentScalerAlgorithmTypeTokenThroughput,
				TokenThroughput: &workercore.ModelDeploymentScalerTokenThroughputSpec{
					TTFTTargetMilliseconds: ptr.To(resource.MustParse("500")),
					ITLTargetMilliseconds:  ptr.To(resource.MustParse("50")),
				},
				CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{
						Name: "test-priors",
					},
				},
			},
		},
	}

	cm := &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "test-priors"},
		Data:       map[string]string{priors.DefaultCatalogConfigMapKey: string(rawCatalog)},
	}
	instance := &workerapi.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "h100-1x"},
		Spec:       workercore.InstanceTypeSpec{Acceleratable: true},
		Status: workercore.InstanceTypeStatus{
			Detail: workercore.InstanceTypeDetail{
				Product: "H100",
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
					Memory: "80Gi",
				},
			},
		},
	}
	wideInstance := &workerapi.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: "h100-2x"},
		Spec:       workercore.InstanceTypeSpec{Acceleratable: true},
		Status: workercore.InstanceTypeStatus{
			Detail: workercore.InstanceTypeDetail{
				Product: "H100",
				InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
					Memory: "80Gi",
				},
			},
		},
	}

	return modelDeploymentScalerPriorFixture{
		scaler:       scaler,
		target:       target,
		catalog:      cm,
		instance:     instance,
		wideInstance: wideInstance,
		args:         args,
		document:     document,
	}
}

// priorProfile returns the profile of one role and replica width.
//
// IT LOOKS THE PROFILE UP BY ITS SHAPE, because a profile is known by the shape
// it measured and the shape is what a catalog is selected by.
func priorProfile(t *testing.T, catalog *priors.Catalog, role priors.RoleKind, gpusPerReplica int) priors.Profile {
	t.Helper()
	for _, profile := range catalog.Profiles {
		if profile.Role == role && profile.Topology.GPUsPerReplica == gpusPerReplica {
			return profile
		}
	}
	t.Fatalf("the catalog holds no %s profile with %d GPUs per replica", role, gpusPerReplica)
	return priors.Profile{}
}

func (fx modelDeploymentScalerPriorFixture) scalerWith(mutate ...func(*workercore.ModelDeploymentScaler)) *workercore.ModelDeploymentScaler {
	s := fx.scaler.DeepCopy()
	for _, m := range mutate {
		m(s)
	}
	return s
}

func (fx modelDeploymentScalerPriorFixture) targetWith(mutate ...func(*workercore.ModelDeployment)) *workercore.ModelDeployment {
	d := fx.target.DeepCopy()
	for _, m := range mutate {
		m(d)
	}
	return d
}

// catalogWith returns the catalog ConfigMap of the given written profiles.
//
// IT WRITES THE CATALOG THE WAY THE OPERATOR READS IT, because a catalog in
// another format proves nothing about the format the operator ships.
func (fx modelDeploymentScalerPriorFixture) catalogWith(t *testing.T, profiles ...priors.ProfileDocument) *core.ConfigMap {
	t.Helper()
	raw, err := priors.MarshalCatalogDocument(priors.NewCatalogDocument(profiles...))
	require.NoError(t, err)
	return &core.ConfigMap{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "test-priors"},
		Data:       map[string]string{priors.DefaultCatalogConfigMapKey: string(raw)},
	}
}

// catalogUnderKey returns the catalog ConfigMap with the catalog under one key.
func (fx modelDeploymentScalerPriorFixture) catalogUnderKey(t *testing.T, key string) *core.ConfigMap {
	t.Helper()
	catalog := fx.catalogWith(t, fx.document.Profiles...)
	catalog.Data = map[string]string{key: catalog.Data[priors.DefaultCatalogConfigMapKey]}
	return catalog
}

// catalogWithoutRole returns the catalog ConfigMap without the profile of one role.
//
// IT DROPS EVERY PROFILE OF THE ROLE, because a catalog that keeps one profile
// of the role still matches the role and proves nothing.
func (fx modelDeploymentScalerPriorFixture) catalogWithoutRole(t *testing.T, role priors.RoleKind) *core.ConfigMap {
	t.Helper()
	var kept []priors.ProfileDocument
	for _, profile := range fx.document.Profiles {
		if profile.Role != string(role) {
			kept = append(kept, profile)
		}
	}
	return fx.catalogWith(t, kept...)
}

// catalogWithTwoRecipes returns the catalog ConfigMap with a second recipe of one role.
//
// IT GIVES THE SECOND PROFILE ANOTHER QUANTIZATION, because a catalog reader
// refuses two profiles of one shape, and a deployment that states no recipe
// cannot choose between two recipes of one shape.
func (fx modelDeploymentScalerPriorFixture) catalogWithTwoRecipes(t *testing.T, role priors.RoleKind) *core.ConfigMap {
	t.Helper()
	profiles := append([]priors.ProfileDocument(nil), fx.document.Profiles...)
	for _, profile := range profiles {
		if profile.Role != string(role) {
			continue
		}
		other := profile
		other.Quantization = "fp8"
		profiles = append(profiles, other)
		break
	}
	return fx.catalogWith(t, profiles...)
}

func (fx modelDeploymentScalerPriorFixture) allObjects() []ctrlcli.Object {
	return []ctrlcli.Object{
		fx.target.DeepCopy(),
		fx.catalog.DeepCopy(),
		fx.instance.DeepCopy(),
		fx.wideInstance.DeepCopy(),
	}
}

func (fx modelDeploymentScalerPriorFixture) targetOnlyObjects() []ctrlcli.Object {
	return []ctrlcli.Object{fx.target.DeepCopy()}
}

func (fx modelDeploymentScalerPriorFixture) targetAndInstanceObjects() []ctrlcli.Object {
	return []ctrlcli.Object{fx.target.DeepCopy(), fx.instance.DeepCopy(), fx.wideInstance.DeepCopy()}
}

func (fx modelDeploymentScalerPriorFixture) objectsWithTarget(mutate ...func(*workercore.ModelDeployment)) []ctrlcli.Object {
	return []ctrlcli.Object{
		fx.targetWith(mutate...),
		fx.catalog.DeepCopy(),
		fx.instance.DeepCopy(),
		fx.wideInstance.DeepCopy(),
	}
}

// objectsWithCatalog returns the objects of a fixture whose catalog is another one.
func (fx modelDeploymentScalerPriorFixture) objectsWithCatalog(catalog *core.ConfigMap) []ctrlcli.Object {
	return fx.objectsWithCatalogAndTarget(catalog)
}

// objectsWithCatalogAndTarget returns the objects of a fixture whose catalog and
// target are other ones.
func (fx modelDeploymentScalerPriorFixture) objectsWithCatalogAndTarget(
	catalog *core.ConfigMap, mutate ...func(*workercore.ModelDeployment),
) []ctrlcli.Object {
	return []ctrlcli.Object{
		fx.targetWith(mutate...),
		catalog.DeepCopy(),
		fx.instance.DeepCopy(),
		fx.wideInstance.DeepCopy(),
	}
}

// modelDeploymentScalerPriorRole builds a role of the given replica width.
//
// IT NAMES THE INSTANCE TYPE AFTER THE WIDTH, because the role states its
// accelerators and the instance type it runs on has to exist for the reader.
func modelDeploymentScalerPriorRole(
	name string, kind workercore.ModelDeploymentRoleKind, extraArgs []string, gpusPerReplica int64,
) workercore.ModelDeploymentRole {
	return workercore.ModelDeploymentRole{
		Name:         name,
		Kind:         kind,
		Replicas:     1,
		InstanceType: fmt.Sprintf("h100-%dx", gpusPerReplica),
		ExtraArgs:    append([]string(nil), extraArgs...),
		Resources: &workercore.ModelDeploymentRoleResources{
			Accelerator: ptr.To(resource.MustParse(strconv.FormatInt(gpusPerReplica, 10))),
		},
	}
}

func newModelDeploymentScalerPriorWebhook(
	t *testing.T,
	objects []ctrlcli.Object,
	forbidNonTargetReads bool,
	getErr error,
	configMapGetErr error,
) *ModelDeploymentScalerWebhook {
	t.Helper()

	builder := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme)
	if len(objects) > 0 {
		builder = builder.WithObjects(objects...)
	}
	if forbidNonTargetReads || getErr != nil || configMapGetErr != nil {
		builder = builder.WithInterceptorFuncs(interceptor.Funcs{
			Get: func(ctx context.Context, client ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				if getErr != nil {
					return getErr
				}
				if configMapGetErr != nil {
					if _, isCM := obj.(*core.ConfigMap); isCM {
						return configMapGetErr
					}
				}
				if forbidNonTargetReads {
					if _, isTarget := obj.(*workercore.ModelDeployment); !isTarget {
						t.Fatalf("unexpected non-target API read for %T (%s)", obj, key)
					}
				}
				return client.Get(ctx, key, obj, opts...)
			},
		})
	}
	return &ModelDeploymentScalerWebhook{APIReader: builder.Build()}
}

func TestModelDeploymentScalerDefault_CapacityPrior(t *testing.T) {
	cases := []struct {
		name                       string
		objects                    []ctrlcli.Object
		forbidNonTargetReads       bool
		getErr                     error
		scaler                     *workercore.ModelDeploymentScaler
		wantErr                    string
		wantNilCapacityPrior       bool
		wantCatalogKey             string
		wantHeadroom               *resource.Quantity
		checkTokenThroughputSLOs   bool
		wantTokenThroughputTTFT    *resource.Quantity
		wantTokenThroughputITL     *resource.Quantity
		wantTokenThroughputWindows bool
		checkPredictiveSLOs        bool
		wantPredictiveTTFT         *resource.Quantity
		wantPredictiveITL          *resource.Quantity
	}{
		{
			name:                     "nil capacityPrior preserves legacy SLO defaults and nil block",
			scaler:                   modelDeploymentScaler(),
			wantNilCapacityPrior:     true,
			checkTokenThroughputSLOs: true,
			wantTokenThroughputTTFT:  ptr.To(resource.MustParse("500")),
			wantTokenThroughputITL:   ptr.To(resource.MustParse("50")),
		},
		{
			name: "a capacityPrior with a missing capacity suppresses the legacy SLO defaults and defaults headroom and catalog key",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.ServerTargetTokensPerSecond = nil
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "priors-cm"},
				}
			}),
			wantCatalogKey:           "priors.yaml",
			wantHeadroom:             ptr.To(resource.MustParse("0.75")),
			checkTokenThroughputSLOs: true,
		},
		{
			name: "a capacityPrior with missing capacity suppresses default SLOs and populates catalog defaults",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "priors-cm"},
				}
			}),
			wantCatalogKey:             "priors.yaml",
			wantHeadroom:               ptr.To(resource.MustParse("0.75")),
			checkTokenThroughputSLOs:   true,
			wantTokenThroughputWindows: true,
		},
		{
			name: "a capacityPrior with all serving capacities explicit keeps default SLOs",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond:  ptr.To(resource.MustParse("1000")),
					PrefillTargetTokensPerSecond: ptr.To(resource.MustParse("2000")),
					DecodeTargetTokensPerSecond:  ptr.To(resource.MustParse("500")),
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{}
			}),
			checkTokenThroughputSLOs: true,
			wantTokenThroughputTTFT:  ptr.To(resource.MustParse("500")),
			wantTokenThroughputITL:   ptr.To(resource.MustParse("50")),
		},
		{
			name:                 "server-only target with a capacity prior and manual server tokenThroughput capacity keeps legacy 500/50 SLO defaults without external catalog reads",
			objects:              []ctrlcli.Object{modelDeploymentScalerTarget()},
			forbidNonTargetReads: true,
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "missing-catalog"},
				}
			}),
			checkTokenThroughputSLOs: true,
			wantTokenThroughputTTFT:  ptr.To(resource.MustParse("500")),
			wantTokenThroughputITL:   ptr.To(resource.MustParse("50")),
		},
		{
			name:                 "server-only target with a capacity prior and manual server predictive capacities keeps legacy 500/50 SLO defaults without external catalog reads",
			objects:              []ctrlcli.Object{modelDeploymentScalerTarget()},
			forbidNonTargetReads: true,
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("1000")),
					TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
						ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
					},
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "missing-catalog"},
				}
			}),
			checkPredictiveSLOs: true,
			wantPredictiveTTFT:  ptr.To(resource.MustParse("500")),
			wantPredictiveITL:   ptr.To(resource.MustParse("50")),
		},
		{
			name: "a predictive capacityPrior with partial capacity suppresses default SLOs",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("1000")),
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{}
			}),
			checkPredictiveSLOs: true,
		},
		{
			name: "catalogRef with explicit whitespace key is preserved by Default so validation can refuse it",
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{
						Name: "priors-cm",
						Key:  "   ",
					},
				}
			}),
			wantCatalogKey: "   ",
		},
		{
			name:   "default propagates non-NotFound target read errors when catalog needs target inspection",
			getErr: errors.New("apiserver target read failure"),
			scaler: modelDeploymentScaler(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{}
			}),
			wantErr: "apiserver target read failure",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := newModelDeploymentScalerPriorWebhook(t, tc.objects, tc.forbidNonTargetReads, tc.getErr, nil)
			scaler := tc.scaler.DeepCopy()
			err := webhook.Default(context.Background(), scaler)
			if tc.wantErr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			if tc.wantNilCapacityPrior {
				assert.Nil(t, scaler.Spec.Algorithm.CapacityPrior)
			}
			if tc.wantCatalogKey != "" {
				require.NotNil(t, scaler.Spec.Algorithm.CapacityPrior)
				require.NotNil(t, scaler.Spec.Algorithm.CapacityPrior.CatalogRef)
				assert.Equal(t, tc.wantCatalogKey, scaler.Spec.Algorithm.CapacityPrior.CatalogRef.Key)
			}
			if tc.wantHeadroom != nil {
				require.NotNil(t, scaler.Spec.Algorithm.CapacityPrior)
				require.NotNil(t, scaler.Spec.Algorithm.CapacityPrior.Headroom)
				assert.Zero(t, scaler.Spec.Algorithm.CapacityPrior.Headroom.Cmp(*tc.wantHeadroom))
			}
			if tc.checkTokenThroughputSLOs {
				require.NotNil(t, scaler.Spec.Algorithm.TokenThroughput)
				if tc.wantTokenThroughputTTFT == nil {
					assert.Nil(t, scaler.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds)
				} else {
					require.NotNil(t, scaler.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds)
					assert.Equal(t, tc.wantTokenThroughputTTFT.String(), scaler.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds.String())
				}
				if tc.wantTokenThroughputITL == nil {
					assert.Nil(t, scaler.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds)
				} else {
					require.NotNil(t, scaler.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds)
					assert.Equal(t, tc.wantTokenThroughputITL.String(), scaler.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds.String())
				}
			}
			if tc.wantTokenThroughputWindows {
				require.NotNil(t, scaler.Spec.Algorithm.TokenThroughput)
				assert.Equal(t, int32(180), scaler.Spec.Algorithm.TokenThroughput.StableWindowSeconds)
				assert.Equal(t, int32(60), scaler.Spec.Algorithm.TokenThroughput.PanicWindowSeconds)
				require.NotNil(t, scaler.Spec.Algorithm.TokenThroughput.PanicThreshold)
				assert.Zero(t, scaler.Spec.Algorithm.TokenThroughput.PanicThreshold.Cmp(resource.MustParse("2.0")))
			}
			if tc.checkPredictiveSLOs {
				require.NotNil(t, scaler.Spec.Algorithm.Predictive)
				if tc.wantPredictiveTTFT == nil {
					assert.Nil(t, scaler.Spec.Algorithm.Predictive.TokenThroughput.TTFTTargetMilliseconds)
				} else {
					require.NotNil(t, scaler.Spec.Algorithm.Predictive.TokenThroughput.TTFTTargetMilliseconds)
					assert.Equal(t, tc.wantPredictiveTTFT.String(), scaler.Spec.Algorithm.Predictive.TokenThroughput.TTFTTargetMilliseconds.String())
				}
				if tc.wantPredictiveITL == nil {
					assert.Nil(t, scaler.Spec.Algorithm.Predictive.TokenThroughput.ITLTargetMilliseconds)
				} else {
					require.NotNil(t, scaler.Spec.Algorithm.Predictive.TokenThroughput.ITLTargetMilliseconds)
					assert.Equal(t, tc.wantPredictiveITL.String(), scaler.Spec.Algorithm.Predictive.TokenThroughput.ITLTargetMilliseconds.String())
				}
			}
		})
	}
}

func TestModelDeploymentScalerValidate_CapacityPrior(t *testing.T) {
	fx := newModelDeploymentScalerPriorFixture(t)

	cases := []struct {
		name                 string
		objects              []ctrlcli.Object
		oldScaler            *workercore.ModelDeploymentScaler
		scaler               *workercore.ModelDeploymentScaler
		isUpdate             bool
		forbidNonTargetReads bool
		configMapGetErr      error
		wantErr              string
		wantWarning          string
	}{
		{
			name:    "a scaler without a capacityPrior requires manual capacity as before",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior = nil
			}),
			wantErr: "this role is a Server role, and the token throughput algorithm needs the target of that kind",
		},
		{
			name:    "a capacityPrior on proportional algorithm is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm = workercore.ModelDeploymentScalerAlgorithmSpec{
					Type: workercore.ModelDeploymentScalerAlgorithmTypeProportional,
					Proportional: &workercore.ModelDeploymentScalerProportionalSpec{
						Metric: workercore.ModelDeploymentScalerProportionalMetricQueueDepth,
						RoleTargets: []workercore.ModelDeploymentScalerRoleTarget{
							{Name: "server", Target: resource.MustParse("8")},
						},
					},
					CapacityPrior: &workercore.ModelDeploymentScalerCapacityPriorSpec{},
				}
			}),
			wantErr: "spec.algorithm.capacityPrior",
		},
		{
			name:    "catalogRef with empty name is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.CatalogRef.Name = "   "
			}),
			wantErr: "spec.algorithm.capacityPrior.catalogRef.name",
		},
		{
			name:    "catalogRef with invalid DNS name is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.CatalogRef.Name = "Invalid_Name"
			}),
			wantErr: "spec.algorithm.capacityPrior.catalogRef.name",
		},
		{
			name:    "catalogRef with whitespace key is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.CatalogRef.Key = "   "
			}),
			wantErr: "spec.algorithm.capacityPrior.catalogRef.key",
		},
		{
			name:    "catalogRef with invalid ConfigMap key is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.CatalogRef.Key = "bad/key"
			}),
			wantErr: "spec.algorithm.capacityPrior.catalogRef.key",
		},
		{
			name:    "zero headroom is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.Headroom = ptr.To(resource.MustParse("0"))
			}),
			wantErr: "spec.algorithm.capacityPrior.headroom",
		},
		{
			name:    "negative headroom is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.Headroom = ptr.To(resource.MustParse("-0.5"))
			}),
			wantErr: "spec.algorithm.capacityPrior.headroom",
		},
		{
			name:    "headroom above one is refused",
			objects: fx.targetOnlyObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.Headroom = ptr.To(resource.MustParse("1.05"))
			}),
			wantErr: "spec.algorithm.capacityPrior.headroom",
		},
		{
			name:    "explicit zero manual capacity is refused even when a capacity prior is requested",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.ServerTargetTokensPerSecond = ptr.To(resource.MustParse("0"))
			}),
			wantErr: "spec.algorithm.tokenThroughput.serverTargetTokensPerSecond",
		},
		{
			name:    "explicit negative predictive capacity is refused even when a capacity prior is requested",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("-10")),
					TokenThroughput:               *s.Spec.Algorithm.TokenThroughput,
				}
				s.Spec.Algorithm.TokenThroughput = nil
			}),
			wantErr: "spec.algorithm.predictive.serverTokenCapacityPerReplica",
		},
		{
			name:    "missing ttftTargetMilliseconds for server role needing catalog is refused",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = nil
			}),
			wantErr: "spec.algorithm.tokenThroughput.ttftTargetMilliseconds",
		},
		{
			name:    "missing itlTargetMilliseconds for server role needing catalog is refused",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
			}),
			wantErr: "spec.algorithm.tokenThroughput.itlTargetMilliseconds",
		},
		{
			name: "missing ttftTargetMilliseconds for prefill role needing catalog is refused",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0] = modelDeploymentScalerPriorRole("prefill", workercore.ModelDeploymentRoleKindPrefill, fx.args, 1)
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{{Name: "prefill", MinReplicas: 1, MaxReplicas: 4}}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = nil
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = ptr.To(resource.MustParse("50"))
			}),
			wantErr: "spec.algorithm.tokenThroughput.ttftTargetMilliseconds",
		},
		{
			name: "prefill role needing catalog does not require itlTargetMilliseconds",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0] = modelDeploymentScalerPriorRole("prefill", workercore.ModelDeploymentRoleKindPrefill, fx.args, 1)
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{{Name: "prefill", MinReplicas: 1, MaxReplicas: 4}}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = ptr.To(resource.MustParse("500"))
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
			}),
		},
		{
			name: "missing itlTargetMilliseconds for decode role needing catalog is refused",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0] = modelDeploymentScalerPriorRole("decode", workercore.ModelDeploymentRoleKindDecode, fx.args, 1)
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{{Name: "decode", MinReplicas: 1, MaxReplicas: 4}}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = ptr.To(resource.MustParse("500"))
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
			}),
			wantErr: "spec.algorithm.tokenThroughput.itlTargetMilliseconds",
		},
		{
			name: "decode role needing catalog does not require ttftTargetMilliseconds",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0] = modelDeploymentScalerPriorRole("decode", workercore.ModelDeploymentRoleKindDecode, fx.args, 1)
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{{Name: "decode", MinReplicas: 1, MaxReplicas: 4}}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = nil
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = ptr.To(resource.MustParse("50"))
			}),
		},
		{
			name: "all manual capacities with a capacity prior bypass external reads and missing catalog",
			objects: []ctrlcli.Object{fx.targetWith(func(d *workercore.ModelDeployment) {
				d.Annotations = nil
			})},
			forbidNonTargetReads: true,
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.TokenThroughput = &workercore.ModelDeploymentScalerTokenThroughputSpec{
					ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "missing-catalog"},
				}
			}),
		},
		{
			name:    "predictive with explicit forecast and missing fallback resolves fallback from catalog",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("900")),
					TokenThroughput:               *s.Spec.Algorithm.TokenThroughput,
				}
				s.Spec.Algorithm.TokenThroughput = nil
			}),
		},
		{
			name:    "predictive with missing forecast and explicit fallback resolves forecast from catalog",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
						ServerTargetTokensPerSecond: ptr.To(resource.MustParse("900")),
						TTFTTargetMilliseconds:      ptr.To(resource.MustParse("500")),
						ITLTargetMilliseconds:       ptr.To(resource.MustParse("50")),
					},
				}
				s.Spec.Algorithm.TokenThroughput = nil
			}),
		},
		{
			name:    "predictive with both forecast and fallback missing resolves both from catalog",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					TokenThroughput: *s.Spec.Algorithm.TokenThroughput,
				}
				s.Spec.Algorithm.TokenThroughput = nil
			}),
		},
		{
			name: "repeated prefill roles of distinct replica widths resolve from the profile of each width",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles = []workercore.ModelDeploymentRole{
					modelDeploymentScalerPriorRole("prefill-a", workercore.ModelDeploymentRoleKindPrefill, fx.args, 1),
					modelDeploymentScalerPriorRole("prefill-b", workercore.ModelDeploymentRoleKindPrefill, fx.args, 2),
				}
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "prefill-a", MinReplicas: 1, MaxReplicas: 4},
					{Name: "prefill-b", MinReplicas: 1, MaxReplicas: 4},
				}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = ptr.To(resource.MustParse("500"))
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
			}),
		},
		{
			name: "repeated prefill roles of one replica width resolve from the one profile of that width",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles = []workercore.ModelDeploymentRole{
					modelDeploymentScalerPriorRole("prefill-a", workercore.ModelDeploymentRoleKindPrefill, fx.args, 1),
					modelDeploymentScalerPriorRole("prefill-b", workercore.ModelDeploymentRoleKindPrefill, fx.args, 1),
				}
			}),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Roles = []workercore.ModelDeploymentScalerRoleBounds{
					{Name: "prefill-a", MinReplicas: 1, MaxReplicas: 4},
					{Name: "prefill-b", MinReplicas: 1, MaxReplicas: 4},
				}
				s.Spec.Algorithm.TokenThroughput.TTFTTargetMilliseconds = ptr.To(resource.MustParse("500"))
				s.Spec.Algorithm.TokenThroughput.ITLTargetMilliseconds = nil
			}),
		},
		{
			name: "all manual server predictive capacities with a capacity prior bypass external reads and missing catalog",
			objects: []ctrlcli.Object{fx.targetWith(func(d *workercore.ModelDeployment) {
				d.Annotations = nil
			})},
			forbidNonTargetReads: true,
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.Type = workercore.ModelDeploymentScalerAlgorithmTypePredictive
				s.Spec.Algorithm.TokenThroughput = nil
				s.Spec.Algorithm.Predictive = &workercore.ModelDeploymentScalerPredictiveSpec{
					ServerTokenCapacityPerReplica: ptr.To(resource.MustParse("1000")),
					TokenThroughput: workercore.ModelDeploymentScalerTokenThroughputSpec{
						ServerTargetTokensPerSecond: ptr.To(resource.MustParse("1000")),
					},
				}
				s.Spec.Algorithm.CapacityPrior = &workercore.ModelDeploymentScalerCapacityPriorSpec{
					CatalogRef: &workercore.ModelDeploymentScalerCatalogReference{Name: "missing-catalog"},
				}
			}),
		},
		{
			name:        "missing target on create admits with warning",
			scaler:      fx.scalerWith(),
			wantWarning: "qwen-72b",
		},
		{
			name:    "existing target on create with valid catalog and deployment identity succeeds",
			objects: fx.allObjects(),
			scaler:  fx.scalerWith(),
		},
		{
			name:    "existing target on create with missing catalog ConfigMap is refused",
			objects: fx.targetAndInstanceObjects(),
			scaler:  fx.scalerWith(),
			wantErr: "test-priors",
		},
		{
			name:    "a role whose shape matches no catalog profile is refused",
			objects: fx.objectsWithCatalog(fx.catalogWithoutRole(t, priors.RoleKindServer)),
			scaler:  fx.scalerWith(),
			wantErr: "no catalog profile matches the deployment shape",
		},
		{
			name: "a deployment that states no recipe and a catalog that holds two recipes is refused",
			objects: fx.objectsWithCatalogAndTarget(
				fx.catalogWithTwoRecipes(t, priors.RoleKindServer),
				func(d *workercore.ModelDeployment) {
					d.Annotations = nil
				},
			),
			scaler:  fx.scalerWith(),
			wantErr: "several catalog profiles match the deployment shape",
		},
		{
			name: "existing target on create with model identity mismatch is refused",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Annotations[workercore.ModelDeploymentScalerModelIDAnnotationKey] = "model:wrong"
			}),
			scaler:  fx.scalerWith(),
			wantErr: "model-id",
		},
		{
			name: "existing target on create with an unpinned image tag of the same engine family is accepted",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Engine.Image = "vllm/vllm-openai:v0.6.0"
			}),
			scaler: fx.scalerWith(),
		},
		{
			name: "existing target on create with an image of another engine family is refused",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Engine.Image = "lmsysorg/sglang:v0.5.22"
			}),
			scaler:  fx.scalerWith(),
			wantErr: "engine image",
		},
		{
			name: "existing target on create with unsupported data parallel topology is refused",
			objects: fx.objectsWithTarget(func(d *workercore.ModelDeployment) {
				d.Spec.Roles[0].ExtraArgs = append([]string{"--data-parallel-size=2"}, d.Spec.Roles[0].ExtraArgs...)
			}),
			scaler:  fx.scalerWith(),
			wantErr: "parallelism",
		},
		{
			name:    "the catalog is read under the priors.yaml key",
			objects: fx.allObjects(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.CatalogRef.Key = priors.DefaultCatalogConfigMapKey
			}),
		},
		{
			name:    "the legacy priors.json key is still accepted",
			objects: fx.objectsWithCatalog(fx.catalogUnderKey(t, priors.LegacyCatalogConfigMapKey)),
			scaler:  fx.scalerWith(),
		},
		{
			name:    "a catalog ConfigMap that holds neither key is refused",
			objects: fx.objectsWithCatalog(fx.catalogUnderKey(t, "other.yaml")),
			scaler:  fx.scalerWith(),
			wantErr: `lacks key "priors.yaml"`,
		},
		{
			name:      "update when catalog ConfigMap is missing admits with warning so repair or shadow mode is possible",
			isUpdate:  true,
			objects:   fx.targetAndInstanceObjects(),
			oldScaler: fx.scalerWith(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Mode = workercore.ModelDeploymentScalerModeShadow
			}),
			wantWarning: "test-priors",
		},
		{
			name:      "update with structural error still refuses even when catalog is missing",
			isUpdate:  true,
			objects:   fx.targetAndInstanceObjects(),
			oldScaler: fx.scalerWith(),
			scaler: fx.scalerWith(func(s *workercore.ModelDeploymentScaler) {
				s.Spec.Algorithm.CapacityPrior.Headroom = ptr.To(resource.MustParse("1.5"))
			}),
			wantErr: "spec.algorithm.capacityPrior.headroom",
		},
		{
			name:            "create propagates non-NotFound API reader errors",
			objects:         fx.allObjects(),
			configMapGetErr: errors.New("apiserver etcd timeout"),
			scaler:          fx.scalerWith(),
			wantErr:         "apiserver etcd timeout",
		},
		{
			name:            "update propagates non-NotFound API reader errors",
			isUpdate:        true,
			objects:         fx.allObjects(),
			configMapGetErr: errors.New("apiserver etcd timeout"),
			oldScaler:       fx.scalerWith(),
			scaler:          fx.scalerWith(),
			wantErr:         "apiserver etcd timeout",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			webhook := newModelDeploymentScalerPriorWebhook(t, tc.objects, tc.forbidNonTargetReads, nil, tc.configMapGetErr)
			newScaler := tc.scaler.DeepCopy()
			before := newScaler.DeepCopy()

			var (
				warnings []string
				err      error
			)
			if tc.isUpdate {
				warnings, err = webhook.ValidateUpdate(context.Background(), tc.oldScaler.DeepCopy(), newScaler)
			} else {
				warnings, err = webhook.ValidateCreate(context.Background(), newScaler)
			}

			assert.Equal(t, before.Spec, newScaler.Spec, "validation must not mutate persisted spec")
			if tc.wantErr == "" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tc.wantErr)
			}
			if tc.wantWarning != "" {
				require.NotEmpty(t, warnings)
				assert.Contains(t, strings.Join(warnings, "; "), tc.wantWarning)
			}
		})
	}
}
