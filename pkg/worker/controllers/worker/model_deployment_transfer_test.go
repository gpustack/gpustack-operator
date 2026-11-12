package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	appsv1 "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueuepodconst "sigs.k8s.io/kueue/pkg/controller/jobs/pod/constants"

	gpustack "gpustack.ai/gpustack/api/v1"
	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/modelstore"
	"gpustack.ai/gpustack/pkg/modeltransfer/authorization"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

var modelTransferTestDigest = "sha256:" + strings.Repeat("a", 64)

func modelTransferRemoteRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery:              workercore.ModelDeploymentModelDeliveryRemote,
		ModelTransferFallback: workercore.ModelDeploymentModelDeliveryNode,
		ModelTransferPolicy:   settings.ModelArtifactRemoteDeliveryPrefer,
		ArtifactName:          "weights",
		ArtifactUID:           "artifact-uid-1",
		ManifestDigest:        modelTransferTestDigest,
		Repository:            "org/model",
		Revision:              "main",
	}
}

func modelTransferPolicyStub(t *testing.T, policy string) {
	t.Helper()
	orig := modelDeploymentModelTransferPolicy
	t.Cleanup(func() { modelDeploymentModelTransferPolicy = orig })
	modelDeploymentModelTransferPolicy = func(context.Context) string { return policy }
}

func modelTransferTestEnv(t *testing.T, objs ...ctrlcli.Object) ctrlcli.WithWatch {
	t.Helper()
	return ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithIndex(&workercore.ModelPrefetch{}, modelPrefetchArtifactIndex, modelPrefetchArtifactIndexValues).WithStatusSubresource(&workercore.ModelDeployment{}).WithObjects(objs...).Build()
}

func TestModelDeploymentModelTransferPolicyResolution(t *testing.T) {
	hub := func() *workercore.ModelArtifact {
		return &workercore.ModelArtifact{
			ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "weights", UID: "artifact-uid-1"},
			Spec: workercore.ModelArtifactSpec{
				Source: workercore.ModelArtifactSource{
					HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"},
				},
			},
			Status: workercore.ModelArtifactStatus{
				Resolved: &workercore.ModelArtifactResolved{
					ManifestDigest: modelTransferTestDigest, Revision: "main", SizeBytes: 5 << 30,
				},
			},
		}
	}
	cases := []struct {
		name        string
		policy      string
		sourceReady bool
		csiDriver   bool
		base        workercore.ModelDeploymentModelDelivery
		want        workercore.ModelDeploymentModelDelivery
		wantBlocked bool
		wantSource  bool
	}{
		{name: "Disabled keeps the Node delivery", policy: "Disabled", base: "Node", want: "Node"},
		{name: "Disabled keeps the Engine delivery", policy: "Disabled", base: "Engine", want: "Engine"},
		{
			name: "Prefer with a prepared source goes remote", policy: "Prefer", sourceReady: true,
			csiDriver: true, base: "Node", want: "Remote", wantSource: true,
		},
		{
			name: "Prefer without a prepared source keeps the fallback", policy: "Prefer",
			csiDriver: true, base: "Engine", want: "Engine",
		},
		{
			name: "Require goes remote without a prepared source", policy: "Require",
			csiDriver: true, base: "Node", want: "Remote",
		},
		{
			name: "Require without the CSIDriver blocks", policy: "Require",
			base: "Node", want: "Node", wantBlocked: true,
		},
		{
			name: "Prefer without the CSIDriver keeps the fallback", policy: "Prefer",
			base: "Node", want: "Node",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, c.policy)
			ma := hub()
			var cli ctrlcli.Client
			if c.sourceReady {
				sourceReconciler, sourceClient, pf, _, authority, report := testSourceAuthorityFixture(t)
				cli = sourceClient
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: "team-a", Name: "model"}, ma))
				ma.Status.Resolved.ManifestDigest = modelTransferTestDigest
				require.NoError(t, cli.Update(context.Background(), ma))
				var doc map[string]any
				require.NoError(t, json.Unmarshal([]byte(authority.Data["reservations.json"]), &doc))
				doc["entries"].([]any)[0].(map[string]any)["digest"] = modelTransferTestDigest
				raw, err := json.Marshal(doc)
				require.NoError(t, err)
				authority.Data["reservations.json"] = string(raw)
				require.NoError(t, cli.Update(context.Background(), authority))
				report.Status.ModelTransfer.Generations[0].Digest = modelTransferTestDigest
				expiry := meta.NewTime(time.Now().Add(time.Minute))
				report.Status.ModelTransfer.Generations[0].LeaseExpiresAt = &expiry
				require.NoError(t, cli.Update(context.Background(), report))
				reconcileModelPrefetch(t, sourceReconciler, pf.Namespace, pf.Name)
			} else {
				cli = modelTransferTestEnv(t, ma)
			}
			if c.csiDriver {
				require.NoError(t, cli.Create(context.Background(), &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}}))
			}
			artifact := ma.DeepCopy()
			w := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
				Delivery: c.base, ManifestDigest: "", Repository: "org/model",
			}}

			applyModelDeploymentModelTransferPolicy(context.Background(), cli, w, artifact, nil)

			require.NotNil(t, w.Render)
			assert.Equal(t, c.want, w.Render.Delivery)
			assert.Equal(t, c.wantBlocked, w.Blocked)
			if w.Render.Delivery == workercore.ModelDeploymentModelDeliveryRemote {
				assert.Equal(t, c.wantSource, w.Render.SourceReady)
				assert.Equal(t, modelTransferTestDigest, w.Render.ManifestDigest,
					"the remote render carries the digest whatever the base delivery was")
				assert.Equal(t, c.policy, w.Render.ModelTransferPolicy)
				if c.policy == settings.ModelArtifactRemoteDeliveryPrefer {
					assert.NotEqual(t, workercore.ModelDeploymentModelDelivery(""), w.Render.ModelTransferFallback,
						"Prefer declares the fallback it is allowed to take")
					return
				}
				assert.Empty(t, w.Render.ModelTransferFallback,
					"Require names no fallback: its failures are the visible answer")
			}
		})
	}
}

func TestModelDeploymentModelTransferSourceLossHoldsTheRemoteDelivery(t *testing.T) {
	// A Prefer deployment whose source drained KEEPS the remote delivery
	// while its own status names a remote replica: flipping it back would
	// change the desired spec and roll exactly the replicas that loaded.
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	ma := &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "weights", UID: "artifact-uid-1"},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"},
			},
		},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{ManifestDigest: modelTransferTestDigest},
		},
	}
	cli := modelTransferTestEnv(t, ma, &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}})
	w := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryNode, ManifestDigest: "",
	}}
	carried := &workercore.ModelDeploymentModelRemoteStatus{
		Policy: workercore.ModelDeploymentRemotePolicyPrefer,
		Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
			Role: "s0",
			Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
				Ordinal: 0, Delivery: workercore.ModelDeploymentModelDeliveryRemote,
				Load: workercore.ModelDeploymentRemoteLoadStateLoaded,
			}},
		}},
	}

	applyModelDeploymentModelTransferPolicy(context.Background(), cli, w, ma,
		&modelArtifactRemoteState{Carried: carried})

	require.NotNil(t, w.Render)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryRemote, w.Render.Delivery,
		"a source loss never rolls a loaded replica onto a disk delivery")
	assert.False(t, w.Render.SourceReady, "the drain is still observed truthfully")

	// And with no remote replica in the carried status, Prefer falls back.
	w2 := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryNode, ManifestDigest: "",
	}}
	applyModelDeploymentModelTransferPolicy(context.Background(), cli, w2, ma.DeepCopy(), nil)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, w2.Render.Delivery)

	// And with no carried status but a live replica still serving, Prefer keeps the
	// remote delivery: the Pod is the truth the status cannot contradict.
	w3 := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryNode, ManifestDigest: "",
	}}
	applyModelDeploymentModelTransferPolicy(context.Background(), cli, w3, ma.DeepCopy(),
		&modelArtifactRemoteState{LiveRemote: sets.New[string]("server/0")})
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryRemote, w3.Render.Delivery,
		"a live remote replica holds the delivery even when the status names none")
}

func TestModelDeploymentModelTransferVerifiedNodeOnlyNeverWeakens(t *testing.T) {
	anchor := modelTransferRemoteRender()
	anchor.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryEngine
	anchor.VerifiedNodeOnly = true

	got := anchor.modelTransferFallbackRender()
	require.NotNil(t, got)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, got.Delivery,
		"an expected-digest artifact falls back to the node, never to an unverified engine download")

	unverified := modelTransferRemoteRender()
	unverified.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryEngine
	got = unverified.modelTransferFallbackRender()
	require.NotNil(t, got)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryEngine, got.Delivery)
}

func TestModelDeploymentModelTransferEligibility(t *testing.T) {
	original := modelDeploymentModelTransferQualified
	modelTransferWithdrawQualification()
	t.Cleanup(func() { modelDeploymentModelTransferQualified = original })

	managed := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
		Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
	}}
	takeOverRole := &workercore.ModelDeploymentRole{Command: []string{"run", "me"}}

	// Nothing has been accepted, so a supported combination is unqualified and an
	// unsupported one is ineligible. They are different answers and a reader has
	// to be able to tell them apart.
	assert.Equal(t, modelTransferTupleUnqualified, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{}, nodefeature.ManufacturerNVIDIA,
	),
		"a managed vLLM role on NVIDIA names a backend that no acceptance has qualified")
	assert.Equal(t, modelTransferTupleUnqualified, modelDeploymentModelTransferRoleQualification(
		&workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineSGLang},
		}}, &workercore.ModelDeploymentRole{}, nodefeature.ManufacturerNVIDIA,
	),
		"a managed SGLang role on NVIDIA names a backend that no acceptance has qualified")
	assert.Equal(t, modelTransferRoleIneligible, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{}, "amd",
	),
		"an unqualified manufacturer does not qualify")
	assert.Equal(t, modelTransferRoleIneligible, modelDeploymentModelTransferRoleQualification(
		managed, takeOverRole, nodefeature.ManufacturerNVIDIA,
	),
		"a role that took its command over cannot be wrapped")
	assert.Equal(t, modelTransferRoleIneligible, modelDeploymentModelTransferRoleQualification(
		&workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: "mindie"},
		}}, &workercore.ModelDeploymentRole{}, nodefeature.ManufacturerNVIDIA,
	),
		"an engine without an adapter does not qualify")

	// A scoped fixture qualifies the exact tuple and nothing else: the same
	// backend on another hardware is another tuple with its own acceptance.
	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	assert.Equal(t, modelTransferTupleQualified, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{}, nodefeature.ManufacturerNVIDIA,
	))
	assert.Equal(t, modelTransferTupleQualified, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{ExtraArgs: []string{"--load-format", "model-transfer"}},
		nodefeature.ManufacturerNVIDIA,
	), "an explicit model-transfer load format remains eligible")
	assert.Equal(t, modelTransferTupleQualified, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{ExtraArgs: []string{"--load-format=model-transfer"}},
		nodefeature.ManufacturerNVIDIA,
	), "an explicit --load-format=model-transfer remains eligible")
	for _, extraArgs := range [][]string{
		{"--load-format", "safetensors"},
		{"--load-format=runai_streamer"},
		{"--load-format"},
		{"--load-format", "model-transfer", "--load-format", "model-transfer"},
	} {
		assert.Equal(t, modelTransferRoleIneligible, modelDeploymentModelTransferRoleQualification(
			managed, &workercore.ModelDeploymentRole{ExtraArgs: extraArgs}, nodefeature.ManufacturerNVIDIA,
		), "conflicting or duplicate --load-format in ExtraArgs (%v) is ineligible for remote delivery", extraArgs)
	}
	assert.Equal(t, modelTransferTupleUnqualified, modelDeploymentModelTransferRoleQualification(
		managed, &workercore.ModelDeploymentRole{}, nodefeature.ManufacturerAscend,
	),
		"a qualified tuple qualifies itself, never the backend it names")

	// The candidate table is what the combination would use, and it is
	// unchanged by the gate.
	assert.Equal(t, "nixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineVLLM,
	))
	assert.Equal(t, "nixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineSGLang,
	))
	assert.Equal(t, "hixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerAscend, workercore.ModelDeploymentEngineVLLM,
	),
		"vLLM on an Ascend pool is the vllm-ascend qualification, and HIXL is its backend")
	assert.Equal(t, "hixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerAscend, workercore.ModelDeploymentEngineSGLang,
	),
		"SGLang on an Ascend pool pairs with HIXL")
	assert.Empty(t, modelDeploymentModelTransferCandidateBackend("amd", workercore.ModelDeploymentEngineVLLM),
		"an unqualified manufacturer names no backend")
}

func TestModelDeploymentModelTransferForRole(t *testing.T) {
	md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
		Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
	}}
	takeOver := &workercore.ModelDeploymentRole{Command: []string{"run", "me"}}

	t.Run("a qualified role keeps the remote render", func(t *testing.T) {
		modelTransferQualifiedTuple(t, modelTransferTuple{
			Manufacturer: nodefeature.ManufacturerNVIDIA,
			Engine:       workercore.ModelDeploymentEngineVLLM,
			Backend:      "nixl",
		})
		got, reason, _ := modelTransferRemoteRender().modelTransferForRole(md, &workercore.ModelDeploymentRole{},
			nodefeature.ManufacturerNVIDIA, false)
		require.Empty(t, reason)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryRemote, got.Delivery)
	})

	t.Run("an ineligible role falls back under Prefer", func(t *testing.T) {
		got, reason, _ := modelTransferRemoteRender().modelTransferForRole(md, takeOver, nodefeature.ManufacturerNVIDIA, false)
		require.Empty(t, reason)
		require.NotNil(t, got)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, got.Delivery)
	})

	t.Run("an unqualified pool falls back under Prefer", func(t *testing.T) {
		got, reason, _ := modelTransferRemoteRender().modelTransferForRole(md, &workercore.ModelDeploymentRole{},
			"amd", false)
		require.Empty(t, reason)
		require.NotNil(t, got)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, got.Delivery,
			"a pool without a qualified backend is a Prefer fallback, never a guess")
	})

	t.Run("an ineligible role is refused under Require", func(t *testing.T) {
		render := modelTransferRemoteRender()
		render.ModelTransferPolicy = settings.ModelArtifactRemoteDeliveryRequire
		render.ModelTransferFallback = ""
		_, reason, message := render.modelTransferForRole(md, takeOver, nodefeature.ManufacturerNVIDIA, false)
		assert.Equal(t, modelDeploymentModelTransferReasonRoleIneligible, reason)
		assert.Contains(t, message, "never falls back")
	})

	t.Run("a swept ordinal carries its override per replica", func(t *testing.T) {
		md := mdWithModelTransferStatus()
		modelDeploymentModelTransferBumpAttempt(md, "s0", 1,
			workercore.ModelDeploymentModelDeliveryNode)
		override := modelDeploymentModelTransferReplicaOverride(md, "s0", 1)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, override,
			"the swept ordinal renders its fallback")
		healthy := modelDeploymentModelTransferReplicaOverride(md, "s0", 0)
		assert.Empty(t, healthy,
			"a serving remote ordinal on the same role keeps its remote delivery")
	})
}

func mdWithModelTransferStatus() *workercore.ModelDeployment {
	return &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
		},
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{},
			// The carry-forward reads the declared replica counts from the role statuses the
			// observation pass derived from the spec, which is where the reconciler holds them.
			Roles: []workercore.ModelDeploymentRoleStatus{
				{Name: "s0", Desired: 2},
			},
		},
	}
}

func TestModelDeploymentModelTransferRemotePodRendering(t *testing.T) {
	render := modelTransferRemoteRender()

	volumes, mounts := render.volumes(false)
	require.Len(t, volumes, 3)
	require.Len(t, mounts, 3)

	csi := volumes[0].CSI
	require.NotNil(t, csi, "the first volume is the plugin's metadata view")
	assert.Equal(t, modelstore.DriverName, csi.Driver)
	assert.True(t, *csi.ReadOnly)
	assert.Equal(t, "weights", csi.VolumeAttributes[modelArtifactVolumeAttrArtifact])
	assert.Equal(t, "artifact-uid-1", csi.VolumeAttributes[modelArtifactVolumeAttrArtifactUID])
	assert.Equal(t, modelTransferTestDigest, csi.VolumeAttributes[modelArtifactVolumeAttrDigest])
	assert.Equal(t, modelDeploymentModelTransferDeliveryMetadata,
		csi.VolumeAttributes[modelDeploymentModelTransferVolumeAttrDelivery],
		"the view attribute is what keeps the plugin from materializing weights")

	projection := volumes[1].Projected
	require.NotNil(t, projection, "the second volume is the projected broker token")
	require.Len(t, projection.Sources, 1)
	token := projection.Sources[0].ServiceAccountToken
	require.NotNil(t, token)
	assert.Equal(t, authorization.Audience, token.Audience,
		"the token is minted for the audience the source broker reviews")
	assert.Equal(t, int64(3600), *token.ExpirationSeconds)
	assert.Equal(t, "token", token.Path)

	hostPath := volumes[2].HostPath
	require.NotNil(t, hostPath, "the third volume is the host-staged wheel directory")
	assert.Equal(t, modelDeploymentModelTransferWheelHostPath, hostPath.Path)

	byName := map[string]core.VolumeMount{}
	for i := range mounts {
		byName[mounts[i].Name] = mounts[i]
	}
	assert.True(t, byName[modelDeploymentModelVolumeName].ReadOnly)
	assert.Equal(t, ModelDeploymentModelMountPath, byName[modelDeploymentModelVolumeName].MountPath)
	assert.True(t, byName[modelDeploymentModelTransferTokenVolumeName].ReadOnly)
	assert.True(t, byName[modelDeploymentModelTransferWheelVolumeName].ReadOnly)

	container := &core.Container{
		Name:    modelDeploymentMainContainerName,
		Command: []string{"vllm", "serve", "org/model"},
	}
	render.applyModelTransfer(container, workercore.ModelDeploymentEngineVLLM)

	require.Len(t, container.Command, 7)
	assert.Equal(t, "sh", container.Command[0])
	assert.Equal(t, "-c", container.Command[1])
	bootstrap := container.Command[2]
	assert.Contains(t, bootstrap, `importlib.util.find_spec("gpustack_model_transfer")`,
		"the probe checks importability before installing")
	assert.Contains(t, bootstrap, "--no-deps --no-index --find-links "+modelDeploymentModelTransferWheelContainerPath)
	assert.Contains(t, bootstrap, "exec \"$@\"", "the original argv is exec'd, signals and all")
	assert.Equal(t, []string{"vllm", "serve", "org/model"}, container.Command[4:],
		"the engine argv rides behind the bootstrap unchanged")

	found := false
	for i := range container.Args {
		if container.Args[i] == "--load-format" {
			require.Greater(t, len(container.Args), i+1)
			assert.Equal(t, modelDeploymentModelTransferLoadFormat, container.Args[i+1])
			found = true
		}
	}
	assert.True(t, found, "the engine is told to load through the model-transfer adapter")

	// Calling applyModelTransfer when --load-format model-transfer is already
	// present in Args does not append a duplicate flag.
	preconfigured := &core.Container{
		Name:    modelDeploymentMainContainerName,
		Command: []string{"vllm", "serve", "org/model"},
		Args:    []string{"--load-format", modelDeploymentModelTransferLoadFormat},
	}
	render.applyModelTransfer(preconfigured, workercore.ModelDeploymentEngineVLLM)
	assert.Equal(t, []string{"--load-format", modelDeploymentModelTransferLoadFormat}, preconfigured.Args,
		"an existing --load-format model-transfer is not duplicated")

	env := map[string]string{}
	for _, e := range container.Env {
		env[e.Name] = e.Value
	}
	assert.Equal(t, "weights", env[modelDeploymentModelTransferEnvArtifact])
	assert.Equal(t, modelTransferTestDigest, env[modelDeploymentModelTransferEnvManifestDigest])
	assert.Equal(t, modelDeploymentModelTransferTokenFile, env[modelDeploymentModelTransferEnvToken])
	assert.Equal(t, modelDeploymentModelTransferLoadFormat, env[modelDeploymentModelTransferEnvLoadFormat])

	other := &core.Container{Name: modelDeploymentMainContainerName, Command: []string{"vllm", "serve", "m"}}
	nodeRender := &ModelDeploymentArtifactRender{Delivery: workercore.ModelDeploymentModelDeliveryNode}
	nodeRender.applyModelTransfer(other, workercore.ModelDeploymentEngineVLLM)
	assert.Equal(t, []string{"vllm", "serve", "m"}, other.Command,
		"a non-remote delivery is decorated by nothing")
}

func modelTransferPod(role string, ordinal int, delivery workercore.ModelDeploymentModelDelivery,
	phase core.PodPhase, ready bool,
) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: role + "-" + strconv.Itoa(ordinal),
			Labels: map[string]string{
				modelDeploymentLabelKeyComponent:   role,
				modelDeploymentReplicaOrdinalLabel: strconv.Itoa(ordinal),
			},
			Annotations: map[string]string{
				modelDeploymentModelTransferDeliveryAnnotation: string(delivery),
			},
		},
		Spec: core.PodSpec{Containers: []core.Container{{Name: modelDeploymentMainContainerName}}},
		Status: core.PodStatus{
			Phase: phase,
			ContainerStatuses: []core.ContainerStatus{{
				Name:  modelDeploymentMainContainerName,
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
				Ready: ready,
			}},
		},
	}
	if phase == core.PodFailed {
		pod.Status.ContainerStatuses[0].State = core.ContainerState{
			Terminated: &core.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
		}
	}

	return pod
}

func TestModelDeploymentModelTransferStatusObservation(t *testing.T) {
	holder := mdWithModelTransferStatus()
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}

	// Replica 0 loads remotely; replica 1 sits waiting for a source.
	pods := []core.Pod{
		*modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, false),
		*modelTransferPod("s0", 1, workercore.ModelDeploymentModelDeliveryRemote, core.PodPending, false),
	}
	modelDeploymentModelTransferObserveStatus(holder, pods, weights, nil, false)

	remote := holder.Status.Model.Remote
	require.NotNil(t, remote)
	assert.Equal(t, workercore.ModelDeploymentRemotePolicyPrefer, remote.Policy)
	require.Len(t, remote.Roles, 1)
	require.Len(t, remote.Roles[0].Replicas, 2)
	assert.Equal(t, workercore.ModelDeploymentRemoteLoadStateLoading, remote.Roles[0].Replicas[0].Load)
	assert.Equal(t, workercore.ModelDeploymentRemoteLoadStateAwaitingSource,
		remote.Roles[0].Replicas[1].Load,
		"the prerequisite that blocks a replica is its own observation, not a loading synonym")
	assert.NotEqual(t, workercore.ModelDeploymentModelDelivery(""), remote.Roles[0].Replicas[1].Delivery)

	// Replica 0 reaches Loaded and keeps it when the source later drains:
	// the next pass observes the same Pod with no source anywhere.
	holder2 := mdWithModelTransferStatus()
	holder2.Status.Model = holder.Status.Model.DeepCopy()
	pods2 := []core.Pod{
		*modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, true),
	}
	modelDeploymentModelTransferObserveStatus(holder2, pods2, weights, holder.Status.Model.Remote, false)
	loaded := holder2.Status.Model.Remote.Roles[0].Replicas[0]
	assert.Equal(t, workercore.ModelDeploymentRemoteLoadStateLoaded, loaded.Load,
		"an already loaded replica keeps its observation after the source drains")

	// A replica that fell back carries the fallback delivery beside the
	// remote one, which is the mixed outcome the aggregate string cannot
	// summarize.
	holder3 := mdWithModelTransferStatus()
	holder3.Status.Model = holder.Status.Model.DeepCopy()
	modelDeploymentModelTransferBumpAttempt(holder3, "s0", 1,
		workercore.ModelDeploymentModelDeliveryNode)
	pods3 := []core.Pod{
		*modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, true),
		*modelTransferPod("s0", 1, workercore.ModelDeploymentModelDeliveryNode, core.PodPending, false),
	}
	modelDeploymentModelTransferObserveStatus(holder3, pods3, weights, holder2.Status.Model.Remote, true)
	entries := holder3.Status.Model.Remote.Roles[0].Replicas
	require.Len(t, entries, 2)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryRemote, entries[0].Delivery)
	assert.Equal(t, workercore.ModelDeploymentRemoteLoadStateLoaded, entries[0].Load)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, entries[1].Delivery,
		"the fallback replica reads its own outcome, and the view says mixed")
}

func TestModelDeploymentModelTransferSweepFallback(t *testing.T) {
	build := func(policy string, pods ...*core.Pod) (*workercore.ModelDeployment, ctrlcli.Client, []core.Pod) {
		md := mdWithModelTransferStatus()
		md.Name, md.Namespace = "model", "team-a"
		render := modelTransferRemoteRender()
		render.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryNode
		objs := []ctrlcli.Object{md}
		if policy == settings.ModelArtifactRemoteDeliveryRequire {
			objs = append(objs, &storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}})
		}
		for i := range pods {
			objs = append(objs, pods[i])
		}
		cli := modelTransferTestEnv(t, objs...)

		live := &core.PodList{}
		require.NoError(t, cli.List(context.Background(), live))

		return md, cli, live.Items
	}

	t.Run("a failed remote replica is recreated on the declared fallback", func(t *testing.T) {
		md, cli, pods := build(settings.ModelArtifactRemoteDeliveryPrefer,
			modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false),
			modelTransferPod("s0", 1, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, true),
		)
		swept, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods,
			settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
		require.NoError(t, err)
		assert.Len(t, swept, 1, "the failed replica's pod is returned for the Workload cleanup")
		remaining := &core.PodList{}
		require.NoError(t, cli.List(context.Background(), remaining))
		require.Len(t, remaining.Items, 1, "the failed pod is deleted; the serving one stands")
		assert.Equal(t, "s0-1", remaining.Items[0].Name,
			"an already-serving replica is kept untouched")
		override := modelDeploymentModelTransferReplicaOverride(md, "s0", 0)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, override)
	})

	t.Run("Require never sweeps", func(t *testing.T) {
		md, cli, pods := build(settings.ModelArtifactRemoteDeliveryRequire,
			modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false),
		)
		swept, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods,
			settings.ModelArtifactRemoteDeliveryRequire, workercore.ModelDeploymentModelDeliveryNode)
		require.NoError(t, err)
		assert.Empty(t, swept)
		remaining := &core.PodList{}
		require.NoError(t, cli.List(context.Background(), remaining))
		assert.Len(t, remaining.Items, 1, "a Require failure is the visible answer, not a fallback")
	})

	t.Run("a fallback pod is never a candidate", func(t *testing.T) {
		md, cli, pods := build(settings.ModelArtifactRemoteDeliveryPrefer,
			modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryNode, core.PodFailed, false),
		)
		swept, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods,
			settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
		require.NoError(t, err)
		assert.Empty(t, swept, "a pod already on its fallback is nobody's remote failure")
	})

	t.Run("the attempt count is bounded and the overrun is visible", func(t *testing.T) {
		md, cli, _ := build(settings.ModelArtifactRemoteDeliveryPrefer)
		failedCounter := 0
		for range modelDeploymentModelTransferAttemptsBound {
			if md.Status.Model.Remote != nil {
				modelDeploymentModelTransferReplicaEntry(md.Status.Model.Remote, "s0", 0).Delivery = workercore.ModelDeploymentModelDeliveryRemote
			}
			// Each pass finds the replacement pod still failing remotely: one
			// attempt per pass, the pod deleted, the next pass judging its
			// replacement.
			failed := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false)
			failed.Name = failed.Name + "-" + fmt.Sprint(failedCounter)
			failedCounter++
			require.NoError(t, cli.Create(context.Background(), failed))
			pods := []core.Pod{*failed}
			_, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods,
				settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
			require.NoError(t, err)
		}
		entry := md.Status.Model.Remote.Roles[0].Replicas[0]
		assert.Equal(t, modelDeploymentModelTransferAttemptsBound, entry.FallbackAttempts)
		assert.Equal(t, modelDeploymentModelTransferReasonLoadFailed, entry.Reason)

		modelDeploymentModelTransferReplicaEntry(md.Status.Model.Remote, "s0", 0).Delivery = workercore.ModelDeploymentModelDeliveryRemote
		failedAgain := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false)
		failedAgain.Name += "-overrun"
		require.NoError(t, cli.Create(context.Background(), failedAgain))
		pods := []core.Pod{*failedAgain}
		swept, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods,
			settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
		require.NoError(t, err)
		assert.Len(t, swept, 1,
			"the overrun still lands the replica on its fallback instead of looping remote")
		entry = md.Status.Model.Remote.Roles[0].Replicas[0]
		assert.Equal(t, modelDeploymentModelTransferReasonAttemptsSpent, entry.Reason,
			"the spent bound is visible, and the replica still lands on its fallback")
		override := modelDeploymentModelTransferReplicaOverride(md, "s0", 0)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, override)
	})
}

func TestModelDeploymentCompileCacheRemoteIdentity(t *testing.T) {
	digestRender := &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryRemote, ManifestDigest: modelTransferTestDigest,
	}
	nodeRender := &ModelDeploymentArtifactRender{
		Delivery: workercore.ModelDeploymentModelDeliveryNode, ManifestDigest: modelTransferTestDigest,
	}
	md := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{Model: workercore.ModelDeploymentModel{Name: "qwen"}},
	}

	remote, err := modelDeploymentCompileCacheRenderIdentity(ModelDeploymentCompileCacheInput{
		Deployment: md, Artifact: digestRender,
	})
	require.NoError(t, err)
	node, err := modelDeploymentCompileCacheRenderIdentity(ModelDeploymentCompileCacheInput{
		Deployment: md, Artifact: nodeRender,
	})
	require.NoError(t, err)
	assert.Equal(t, node, remote,
		"one set of weights is one compiled identity whichever delivery mounted it")
}

// TestModelDeploymentModelTransferStatusCarriesAttemptsAcrossReplacement pins the finding-1 contract: the
// attempts a replica spent survive the pod that spent them, and a replacement fallback pod
// re-observes with its history and its fallback reason intact.
func TestModelDeploymentModelTransferStatusCarriesAttemptsAcrossReplacement(t *testing.T) {
	holder := mdWithModelTransferStatus()
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}

	// The replica fails remotely and is swept onto Node.
	failed := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false)
	modelDeploymentModelTransferObserveStatus(holder, []core.Pod{*failed}, weights, nil, true)
	modelDeploymentModelTransferBumpAttempt(holder, "s0", 0,
		workercore.ModelDeploymentModelDeliveryNode)
	before := holder.Status.Model.Remote.Roles[0].Replicas[0]
	require.Equal(t, int32(1), before.FallbackAttempts)

	// The replacement renders the fallback and comes up Pending: the new
	// observation must not reset the count or lose the reason.
	carried := holder.Status.Model.Remote
	replacement := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryNode, core.PodPending, false)
	modelDeploymentModelTransferObserveStatus(holder, []core.Pod{*replacement}, weights, carried, true)

	entry := holder.Status.Model.Remote.Roles[0].Replicas[0]
	assert.Equal(t, int32(1), entry.FallbackAttempts,
		"the attempt count is the replica's history, not the pod's")
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, entry.Delivery)
	assert.Equal(t, modelDeploymentModelTransferReasonLoadFailed, entry.Reason,
		"the fallback reason survives the pod that was replaced")
}

// TestModelDeploymentModelTransferStatusRefusesStaleCarriedFallbacks pins the carry-forward
// boundary: a fallback entry is carried only for a replica the deployment still declares, so a
// scale-down past an ordinal, or a removed or renamed role, cannot keep its entry forever, while a
// replica that is still declared keeps its entry across the pod gap between its sweep and its
// recreation.
func TestModelDeploymentModelTransferStatusRefusesStaleCarriedFallbacks(t *testing.T) {
	weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
	live := []core.Pod{
		*modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, true),
	}

	t.Run("a still-declared replica keeps its fallback across the pod gap", func(t *testing.T) {
		holder := mdWithModelTransferStatus()
		carried := &workercore.ModelDeploymentModelRemoteStatus{
			Policy: workercore.ModelDeploymentRemotePolicyPrefer,
			Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
				Role: "s0",
				Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
					Ordinal: 1, Delivery: workercore.ModelDeploymentModelDeliveryNode,
					Load:             workercore.ModelDeploymentRemoteLoadStateLoaded,
					FallbackAttempts: 2,
				}},
			}},
		}
		modelDeploymentModelTransferObserveStatus(holder, live, weights, carried, true)

		roles := holder.Status.Model.Remote.Roles
		require.Len(t, roles, 1)
		require.Len(t, roles[0].Replicas, 2)
		assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, roles[0].Replicas[1].Delivery,
			"the replica between its sweep and its recreation keeps its fallback entry")
		assert.Equal(t, int32(2), roles[0].Replicas[1].FallbackAttempts)
		assert.Equal(t, workercore.ModelDeploymentRemoteLoadStateLoaded, roles[0].Replicas[1].Load)
	})

	t.Run("an ordinal past the declared replica count is refused", func(t *testing.T) {
		holder := mdWithModelTransferStatus()
		carried := &workercore.ModelDeploymentModelRemoteStatus{
			Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
				Role: "s0",
				Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
					Ordinal: 3, Delivery: workercore.ModelDeploymentModelDeliveryNode,
					Load:             workercore.ModelDeploymentRemoteLoadStateLoaded,
					FallbackAttempts: 1,
				}},
			}},
		}
		modelDeploymentModelTransferObserveStatus(holder, live, weights, carried, true)

		roles := holder.Status.Model.Remote.Roles
		require.Len(t, roles, 1)
		assert.Len(t, roles[0].Replicas, 1,
			"the scaled-away ordinal's stale entry is refused, not carried forever")
		assert.Equal(t, int32(0), roles[0].Replicas[0].Ordinal)
	})

	t.Run("a removed or renamed role is refused", func(t *testing.T) {
		holder := mdWithModelTransferStatus()
		carried := &workercore.ModelDeploymentModelRemoteStatus{
			Roles: []workercore.ModelDeploymentRemoteRoleStatus{
				{
					Role: "s0",
					Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
						Ordinal: 1, Delivery: workercore.ModelDeploymentModelDeliveryNode,
						FallbackAttempts: 1,
					}},
				},
				{
					Role: "former",
					Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
						Ordinal: 0, Delivery: workercore.ModelDeploymentModelDeliveryNode,
						FallbackAttempts: 5,
					}},
				},
			},
		}
		modelDeploymentModelTransferObserveStatus(holder, live, weights, carried, true)

		roles := holder.Status.Model.Remote.Roles
		require.Len(t, roles, 1, "the removed role's stale fallback is refused")
		assert.Equal(t, "s0", roles[0].Role)
		require.Len(t, roles[0].Replicas, 2)
		assert.Equal(t, int32(1), roles[0].Replicas[1].Ordinal)
	})
}

// TestModelDeploymentModelTransferSweepMultiMemberGroup pins the group semantics: a
// multi-member replica is swept once, every member of it goes, and a replica that
// may have loaded is never a candidate.
func TestModelDeploymentModelTransferSweepMultiMemberGroup(t *testing.T) {
	cases := []struct {
		name string
		// first and second are what the two members' loads did, in the order
		// the sweep reads them.
		first  modelTransferMemberLoad
		second modelTransferMemberLoad
		// swept is how many members the sweep takes.
		swept int
	}{
		{
			name:   "every member failed",
			first:  modelTransferMemberFailed,
			second: modelTransferMemberFailed,
			swept:  2,
		},
		{
			name:   "the first member serves",
			first:  modelTransferMemberServing,
			second: modelTransferMemberFailed,
		},
		{
			name:   "the second member serves",
			first:  modelTransferMemberFailed,
			second: modelTransferMemberServing,
		},
		{
			name:   "the second member's load is under way",
			first:  modelTransferMemberFailed,
			second: modelTransferMemberUnknown,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			md := mdWithModelTransferStatus()
			md.Name, md.Namespace = "model", "team-a"
			first := modelTransferMemberPod("s0", 2, "s0-2-member-0", c.first)
			second := modelTransferMemberPod("s0", 2, "s0-2-member-1", c.second)
			cli := modelTransferTestEnv(t, md, first, second)
			pods := &core.PodList{}
			require.NoError(t, cli.List(context.Background(), pods))

			swept, err := modelDeploymentModelTransferSweep(context.Background(), cli, md, pods.Items,
				settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
			require.NoError(t, err)
			assert.Len(t, swept, c.swept,
				"a replica falls back only when every member of it failed its load")

			remaining := &core.PodList{}
			require.NoError(t, cli.List(context.Background(), remaining))
			assert.Len(t, remaining.Items, 2-c.swept, "every member the sweep took is gone")
			if c.swept == 0 {
				return
			}

			entry := modelDeploymentModelTransferReplicaEntry(md.Status.Model.Remote, "s0", 2)
			assert.Equal(t, int32(1), entry.FallbackAttempts,
				"the group is one attempt, not one per member")
		})
	}
}

// TestModelDeploymentModelTransferBindingHashStability pins the finding-2 contract: source readiness and
// the pool's transfer backend are hardware observations, and two renders differing only in them
// are one Pod spec.
func TestModelDeploymentModelTransferBindingHashStability(t *testing.T) {
	build := func(sourceReady bool, backend string) ModelDeploymentRenderInput {
		return ModelDeploymentRenderInput{
			Deployment: &workercore.ModelDeployment{
				Spec: workercore.ModelDeploymentSpec{
					Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
					Model:  workercore.ModelDeploymentModel{Name: "qwen"},
				},
			},
			Role: &workercore.ModelDeploymentRole{Name: "s0"},
			Artifact: &ModelDeploymentArtifactRender{
				Delivery:             workercore.ModelDeploymentModelDeliveryRemote,
				ManifestDigest:       modelTransferTestDigest,
				SourceReady:          sourceReady,
				ModelTransferBackend: backend,
			},
		}
	}

	a := modelDeploymentBindingHash(build(true, "nixl"), workercore.ModelDeploymentRoleResources{})
	b := modelDeploymentBindingHash(build(false, "hixl"), workercore.ModelDeploymentRoleResources{})
	assert.Equal(t, a, b,
		"a source drain or a backend name must never read as a Pod spec change")
}

// TestModelDeploymentModelTransferBackendEnv pins the finding-3 contract: the qualified backend reaches
// the adapter through the environment, per role.
func TestModelDeploymentModelTransferBackendEnv(t *testing.T) {
	render := modelTransferRemoteRender()
	render.ModelTransferBackend = "hixl"
	container := &core.Container{
		Name: modelDeploymentMainContainerName, Command: []string{"vllm", "serve", "m"},
	}
	render.applyModelTransfer(container, workercore.ModelDeploymentEngineVLLM)

	found := false
	for _, e := range container.Env {
		if e.Name == modelDeploymentModelTransferEnvBackend {
			assert.Equal(t, "hixl", e.Value)
			found = true
		}
	}
	assert.True(t, found, "the adapter learns its backend through the environment")

	plain := modelTransferRemoteRender()
	plainContainer := &core.Container{
		Name: modelDeploymentMainContainerName, Command: []string{"vllm", "serve", "m"},
	}
	plain.applyModelTransfer(plainContainer, workercore.ModelDeploymentEngineVLLM)
	for _, e := range plainContainer.Env {
		assert.NotEqual(t, modelDeploymentModelTransferEnvBackend, e.Name,
			"no qualified backend means no backend claim")
	}
}

// modelTransferQualifiedTuple installs a scoped qualification fixture for the
// duration of a test, so the remote render can be exercised without a
// production bypass. Production answers every tuple as unqualified.
func modelTransferQualifiedTuple(t *testing.T, tuple modelTransferTuple) {
	t.Helper()

	original := modelDeploymentModelTransferQualified
	modelDeploymentModelTransferQualified = func(got modelTransferTuple) bool {
		return got == tuple
	}
	t.Cleanup(func() { modelDeploymentModelTransferQualified = original })
}

// modelTransferWithdrawQualification takes the qualification away again, so the rest of
// a test runs against a tuple no acceptance has qualified.
func modelTransferWithdrawQualification() {
	modelDeploymentModelTransferQualified = modelDeploymentModelTransferNoTupleQualified
}

// TestModelDeploymentModelTransferQualificationGate pins the gate: a candidate
// backend is not a qualification, and a qualified tuple qualifies itself and
// nothing else.
func TestModelDeploymentModelTransferQualificationGate(t *testing.T) {
	original := modelDeploymentModelTransferQualified
	modelTransferWithdrawQualification()
	t.Cleanup(func() { modelDeploymentModelTransferQualified = original })

	assert.Equal(t, "nixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineVLLM,
	))
	assert.Equal(t, "hixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerAscend, workercore.ModelDeploymentEngineVLLM,
	))
	assert.Equal(t, "nixl", modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineSGLang,
	))
	assert.Empty(t, modelDeploymentModelTransferCandidateBackend(
		nodefeature.ManufacturerNVIDIA, "mindie",
	))

	assert.Empty(t, modelDeploymentModelTransferQualifiedBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineVLLM,
	), "no tuple has passed an acceptance, so no pool may use a backend")
	assert.Empty(t, modelDeploymentModelTransferQualifiedBackend(
		nodefeature.ManufacturerAscend, workercore.ModelDeploymentEngineVLLM,
	))

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	assert.Equal(t, "nixl", modelDeploymentModelTransferQualifiedBackend(
		nodefeature.ManufacturerNVIDIA, workercore.ModelDeploymentEngineVLLM,
	))
	assert.Empty(t, modelDeploymentModelTransferQualifiedBackend(
		nodefeature.ManufacturerAscend, workercore.ModelDeploymentEngineVLLM,
	), "a qualified tuple qualifies itself, never the backend it names")
}

// TestModelDeploymentModelTransferRequireRefusesAnUnqualifiedTuple pins the
// refusal: Require never falls back, so an unqualified tuple is a visible
// answer on every nominal backend, and it is not the ineligible role's answer.
func TestModelDeploymentModelTransferRequireRefusesAnUnqualifiedTuple(t *testing.T) {
	original := modelDeploymentModelTransferQualified
	modelTransferWithdrawQualification()
	t.Cleanup(func() { modelDeploymentModelTransferQualified = original })

	md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
		Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
	}}
	render := modelTransferRemoteRender()
	render.ModelTransferPolicy = settings.ModelArtifactRemoteDeliveryRequire
	render.ModelTransferFallback = ""

	for _, manufacturer := range []string{
		nodefeature.ManufacturerNVIDIA, nodefeature.ManufacturerAscend,
	} {
		got, reason, message := render.modelTransferForRole(md, &workercore.ModelDeploymentRole{}, manufacturer, false)
		assert.Nil(t, got, "Require never falls back, so no render comes back")
		assert.Equal(t, modelDeploymentModelTransferReasonUnqualifiedBackend, reason)
		assert.Contains(t, message, "no source-safety acceptance",
			"the refusal says what is missing")
		assert.NotContains(t, message, "T19")
		assert.NotContains(t, message, "T20")
	}

	// An ineligible role keeps its own reason: the two answers are
	// different and a reader has to be able to tell them apart.
	takeOver := &workercore.ModelDeploymentRole{Command: []string{"run", "me"}}
	_, reason, message := render.modelTransferForRole(md, takeOver, nodefeature.ManufacturerNVIDIA, false)
	assert.Equal(t, modelDeploymentModelTransferReasonRoleIneligible, reason)
	assert.Contains(t, message, "never falls back")
}

// TestModelDeploymentModelTransferPreferFallsBackForAnUnqualifiedTuple pins
// Prefer's answer: the declared fallback, identity-preserving, and no backend
// claim on the render that lands.
func TestModelDeploymentModelTransferPreferFallsBackForAnUnqualifiedTuple(t *testing.T) {
	md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{
		Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
	}}

	got, reason, _ := modelTransferRemoteRender().modelTransferForRole(md, &workercore.ModelDeploymentRole{},
		nodefeature.ManufacturerAscend, false)
	require.Empty(t, reason)
	require.NotNil(t, got)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, got.Delivery,
		"an unqualified tuple takes Prefer's declared fallback")
	assert.Empty(t, got.ModelTransferBackend, "the fallback carries no backend claim")

	verified := modelTransferRemoteRender()
	verified.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryEngine
	verified.VerifiedNodeOnly = true
	got, reason, _ = verified.modelTransferForRole(md, &workercore.ModelDeploymentRole{},
		nodefeature.ManufacturerAscend, false)
	require.Empty(t, reason)
	require.NotNil(t, got)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, got.Delivery,
		"a verified-node-only artifact falls back to the node, never to an engine download")
}

// TestModelDeploymentModelTransferUnqualifiedTupleHoldsALoadedRemoteReplica
// pins the conflict between the fallback and a loaded remote replica: the
// fallback changes the desired delivery, which would roll the replica that
// already loaded, so the delivery is held instead.
func TestModelDeploymentModelTransferUnqualifiedTupleHoldsALoadedRemoteReplica(t *testing.T) {
	md := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
		},
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{
				Remote: &workercore.ModelDeploymentModelRemoteStatus{
					Policy: workercore.ModelDeploymentRemotePolicyPrefer,
					Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
						Role: "s0",
						Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
							Ordinal: 0, Delivery: workercore.ModelDeploymentModelDeliveryRemote,
							Load: workercore.ModelDeploymentRemoteLoadStateLoaded,
						}},
					}},
				},
			},
		},
	}

	got, reason, message := modelTransferRemoteRender().modelTransferForRole(md, &workercore.ModelDeploymentRole{Name: "s0"},
		nodefeature.ManufacturerAscend, false)
	assert.Nil(t, got, "a fallback here would roll the replica that loaded")
	assert.Equal(t, modelDeploymentModelTransferReasonUnqualifiedBackend, reason)
	assert.Contains(t, message, "roll",
		"the reason says why the fallback was not taken")
}

// modelTransferRolloutSetup reaches the admitted steady state a real cluster has: every
// replica holds a Kueue Workload that Kueue admitted, so the rollout is free
// to turn a replica over. The joint-admission barrier is not consulted here:
// this deployment spreads over one role, and the barrier opens for such a
// deployment on its own.
func modelTransferRolloutSetup(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	assertReplicaUIDs(t, cli)
	armReplicaFinalizers(t, cli)
	// One Workload per replica, not per Pod: the members of a multi-member
	// replica share one Kueue group.
	composed := sets.New[string]()
	for _, pod := range replicaPods(t, cli) {
		key := modelDeploymentModelTransferReplicaKeyOf(&pod)
		if composed.Has(key) {
			continue
		}
		composed.Insert(key)
		composeWorkloadFor(t, cli, pod)
	}
	for _, wl := range rolloutWorkloads(t, cli) {
		admitted := wl.DeepCopy()
		admitted.Status.Conditions = append(admitted.Status.Conditions, meta.Condition{
			Type: kueue.WorkloadAdmitted, Status: meta.ConditionTrue, Reason: "Test",
			LastTransitionTime: meta.Now(),
		})
		require.NoError(t, cli.Update(context.Background(), admitted))
	}
}

// modelTransferSetReplicasLoaded marks the deployment's replica Pods' main container
// ready, which is how a replica that loaded the weights looks. The gate reads
// the Pods for a replica that still serves, so a fixture that only sets the
// Pod's own ready condition does not look loaded.
func modelTransferSetReplicasLoaded(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for _, pod := range replicaPods(t, cli) {
		pod.Status.ContainerStatuses = []core.ContainerStatus{
			{Name: modelDeploymentMainContainerName, Ready: true},
		}
		require.NoError(t, cli.Status().Update(context.Background(), &pod))
	}
}

// modelTransferSetReplicasNotReady marks the deployment's replica Pods' main container
// running but not ready, which is how a replica that loaded and is waiting on
// something else looks. Readiness is not a load record: the Pod cannot tell a
// replica that loaded from one that never did.
func modelTransferSetReplicasNotReady(t *testing.T, cli ctrlcli.Client) {
	t.Helper()

	for _, pod := range replicaPods(t, cli) {
		pod.Status.ContainerStatuses = []core.ContainerStatus{{
			Name:  modelDeploymentMainContainerName,
			Ready: false,
			State: core.ContainerState{Running: &core.ContainerStateRunning{}},
		}}
		require.NoError(t, cli.Status().Update(context.Background(), &pod))
	}
}

// modelTransferMemberPod builds a replica member whose load did what the state says.
func modelTransferMemberPod(role string, ordinal int, name string, load modelTransferMemberLoad) *core.Pod {
	pod := modelTransferPod(role, ordinal, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, false)
	switch load {
	case modelTransferMemberServing:
		pod.Status.ContainerStatuses[0].Ready = true
	case modelTransferMemberFailed:
		pod.Status.Phase = core.PodFailed
		pod.Status.ContainerStatuses[0].State = core.ContainerState{
			Terminated: &core.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
		}
	}
	pod.Name = name

	return pod
}

// modelTransferMemberLoad is what a replica member's load did.
type modelTransferMemberLoad int

const (
	// modelTransferMemberServing is a member that loaded and serves: its main container
	// is Ready.
	modelTransferMemberServing modelTransferMemberLoad = iota
	// modelTransferMemberUnknown is a member whose load history is unknown: it is
	// running and not Ready, so it may already have loaded.
	modelTransferMemberUnknown
	// modelTransferMemberFailed is a member whose load failed: its main container
	// exited nonzero.
	modelTransferMemberFailed
)

// modelTransferSetMemberLoads marks the deployment's replica Pods' main container by
// member index, so a case can say what each member's load did.
func modelTransferSetMemberLoads(t *testing.T, cli ctrlcli.Client, loads map[int]modelTransferMemberLoad) {
	t.Helper()

	for _, pod := range replicaPods(t, cli) {
		switch loads[modelDeploymentPodMemberIndex(&pod)] {
		case modelTransferMemberServing:
			pod.Status.Phase = core.PodRunning
			pod.Status.ContainerStatuses = []core.ContainerStatus{
				{Name: modelDeploymentMainContainerName, Ready: true},
			}
		case modelTransferMemberFailed:
			pod.Status.Phase = core.PodFailed
			pod.Status.ContainerStatuses = []core.ContainerStatus{{
				Name: modelDeploymentMainContainerName,
				State: core.ContainerState{
					Terminated: &core.ContainerStateTerminated{ExitCode: 1, Reason: "Error"},
				},
			}}
		default:
			pod.Status.Phase = core.PodRunning
			pod.Status.ContainerStatuses = []core.ContainerStatus{{
				Name:  modelDeploymentMainContainerName,
				State: core.ContainerState{Running: &core.ContainerStateRunning{}},
			}}
		}
		require.NoError(t, cli.Status().Update(context.Background(), &pod))
	}
}

// modelTransferMember returns the deployment's replica Pod whose member index is the one
// named.
func modelTransferMember(t *testing.T, cli ctrlcli.Client, member int) core.Pod {
	t.Helper()

	for _, pod := range replicaPods(t, cli) {
		if modelDeploymentPodMemberIndex(&pod) == member {
			return pod
		}
	}

	t.Fatalf("the replica has no member %d", member)

	return core.Pod{}
}

// modelTransferStaleRemoteView empties the deployment's own remote view, or names the
// replica on the delivery it is leaving, the way a report that lags the Pods it
// describes would.
func modelTransferStaleRemoteView(t *testing.T, cli ctrlcli.Client, stale bool) {
	t.Helper()

	held := getModelDeployment(t, cli)
	require.NotNil(t, held.Status.Model)
	require.NotNil(t, held.Status.Model.Remote,
		"the pass that rendered the replica named it, so the view can go stale from there")
	if stale {
		for i := range held.Status.Model.Remote.Roles {
			for j := range held.Status.Model.Remote.Roles[i].Replicas {
				held.Status.Model.Remote.Roles[i].Replicas[j].Delivery = workercore.ModelDeploymentModelDeliveryNode
			}
		}
	} else {
		held.Status.Model.Remote = nil
	}
	require.NoError(t, cli.Status().Update(context.Background(), held))
}

// modelTransferSourceReadyStub makes the source observation answer as a prepared source does,
// and returns the switch that drains it again. A case needs the source prepared
// while the replica renders, and gone before the pass that must not roll it.
//
// The source is not what stops a rollout: an unprepared source leaves the weights
// unblocked and the delivery decided, so the rollout acts on whatever the
// delivery says. A case that must show a rollout not rolling therefore has to
// give the deployment a reason to keep its delivery, not a reason to wait.
func modelTransferSourceReadyStub(t *testing.T, ready bool) func(bool) {
	t.Helper()

	original := modelDeploymentModelTransferSourceReady
	t.Cleanup(func() { modelDeploymentModelTransferSourceReady = original })
	current := ready
	modelDeploymentModelTransferSourceReady = func(
		context.Context, ctrlcli.Reader, *workercore.ModelArtifact,
	) (bool, time.Time, error) {
		return current, time.Time{}, nil
	}

	return func(next bool) { current = next }
}

// modelTransferFixtureArtifact is the hub artifact the transfer cases deliver.
func modelTransferFixtureArtifact() *workercore.ModelArtifact {
	return &workercore.ModelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "weights", UID: "artifact-uid-1"},
		Spec: workercore.ModelArtifactSpec{
			Source: workercore.ModelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{Repository: "org/model"},
			},
		},
		Status: workercore.ModelArtifactStatus{
			Resolved: &workercore.ModelArtifactResolved{
				ManifestDigest: modelTransferTestDigest, Revision: "main", SizeBytes: 5 << 30,
			},
			Conditions: []gpustack.Condition{{
				Type:   string(ModelArtifactConditionResolved),
				Status: meta.ConditionTrue,
			}},
		},
	}
}

// modelTransferFixtureClient is the fixture the transfer reconcile cases run on: the
// deployment, its instance type and binding, the hub artifact, the node
// plugin's CSIDriver, and Kueue's own admission check. The instance type
// carries the given manufacturer, so a case exercises the pool it names
// rather than the default one.
func modelTransferFixtureClient(
	t *testing.T, md *workercore.ModelDeployment, manufacturer string,
) ctrlcli.Client {
	t.Helper()

	return newModelDeploymentClient(md,
		newRenderAcceleratorType(func(it *worker.AcceleratorType) {
			it.Status.Detail.Manufacturer = manufacturer
		}),
		newRenderBinding(),
		modelTransferFixtureArtifact(),
		&storage.CSIDriver{ObjectMeta: meta.ObjectMeta{Name: modelstore.DriverName}},
		jointCheckObject())
}

// TestModelDeploymentModelTransferUnqualifiedTupleStartsNoRemotePod pins the
// refusal where it is first observed: a deployment whose tuple no acceptance has
// qualified starts NO remote Pod at all, and says why in the weights condition.
//
// Each case runs on the pool it names, and the refusal names the backend that
// pool would have used, so a case that silently ran on the other manufacturer's
// pool fails instead of passing twice.
func TestModelDeploymentModelTransferUnqualifiedTupleStartsNoRemotePod(t *testing.T) {
	original := modelDeploymentModelTransferQualified
	modelTransferWithdrawQualification()
	t.Cleanup(func() { modelDeploymentModelTransferQualified = original })

	cases := []struct {
		manufacturer string
		backend      string
	}{
		{manufacturer: nodefeature.ManufacturerNVIDIA, backend: "nixl"},
		{manufacturer: nodefeature.ManufacturerAscend, backend: "hixl"},
	}
	for _, c := range cases {
		t.Run(c.manufacturer, func(t *testing.T) {
			modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryRequire)
			modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				m.Spec.KVCache = nil
			})
			cli := modelTransferFixtureClient(t, md, c.manufacturer)

			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			assert.Empty(t, modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryRemote),
				"an unqualified tuple starts no remote replica")
			held := getModelDeployment(t, cli)
			require.NotNil(t, held.Status.Conditions)
			assert.Equal(t, modelDeploymentModelTransferReasonUnqualifiedBackend,
				ModelDeploymentConditionWeightsReady.GetReason(held),
				"the refusal is the visible answer")
			message := ModelDeploymentConditionWeightsReady.GetMessage(held)
			assert.Contains(t, message, "no source-safety acceptance",
				"the refusal says what is missing")
			assert.Contains(t, message, fmt.Sprintf("on a %s pool", c.manufacturer),
				"the refusal names the pool it refused")
			assert.Contains(t, message, fmt.Sprintf("%s transfer backend", c.backend),
				"the refusal names the backend this pool would have used")
		})
	}
}

// TestModelDeploymentModelTransferUnqualifiedTupleKeepsALiveRemotePodWithoutStatus
// covers the case the carried status cannot answer: a replica still renders the
// remote delivery, but the deployment's own remote view names no replica. The
// view is a report of an earlier pass, and a report can lag the Pods it
// describes. The Pod is the truth there, so the refusal must hold on it alone.
func TestModelDeploymentModelTransferUnqualifiedTupleKeepsALiveRemotePodWithoutStatus(t *testing.T) {
	cases := []struct {
		name string
		// stale writes the remote view the way a lagging report would: it
		// names the replica, but on the delivery the replica is leaving.
		stale bool
	}{
		{name: "the remote view names no replica"},
		{name: "the remote view names the replica on the fallback", stale: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
			modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				m.Spec.KVCache = nil
			})
			cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

			modelTransferQualifiedTuple(t, modelTransferTuple{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Backend:      "nixl",
			})
			modelTransferRolloutSetup(t, cli)
			setEndpointEligibilityPodsReady(t, cli, true)
			modelTransferSetReplicasLoaded(t, cli)
			remotePods := modelTransferRemotePods(t, cli)
			require.NotEmpty(t, remotePods, "the qualified tuple rendered a remote replica")

			// The deployment's own remote view is emptied or made stale, the
			// way a report that lags the Pods it describes would be.
			modelTransferStaleRemoteView(t, cli, c.stale)

			// The qualification is withdrawn. The replica still renders the
			// remote delivery, and nothing may turn it over.
			modelTransferWithdrawQualification()
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			after := modelTransferRemotePods(t, cli)
			require.Len(t, after, len(remotePods),
				"the live remote replica is still there")
			for _, pod := range after {
				assert.Nil(t, pod.DeletionTimestamp,
					"the live remote replica is not condemned: the Pod is the truth about it, "+
						"not the deployment's own view of it")
			}
		})
	}
}

// TestModelDeploymentModelTransferSourceLossKeepsALiveRemotePodWithoutStatus covers
// the path that decides the delivery before the gate is consulted: a Prefer
// deployment whose source drained keeps the remote delivery while a replica still
// serves it, even when the deployment's own remote view names no replica.
//
// The delivery is what a rollout reads. A remote delivery that becomes the base
// delivery is a different desired spec, and a different spec replaces the replica.
// The drain is not a reason to replace a replica that serves, so the drain must not
// reach the replica through the delivery.
func TestModelDeploymentModelTransferSourceLossKeepsALiveRemotePodWithoutStatus(t *testing.T) {
	cases := []struct {
		name  string
		stale bool
	}{
		{name: "the remote view names no replica"},
		{name: "the remote view names the replica on the fallback", stale: true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
			drainSource := modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				m.Spec.KVCache = nil
			})
			cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

			modelTransferQualifiedTuple(t, modelTransferTuple{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Backend:      "nixl",
			})
			modelTransferRolloutSetup(t, cli)
			setEndpointEligibilityPodsReady(t, cli, true)
			modelTransferSetReplicasLoaded(t, cli)
			remotePods := modelTransferRemotePods(t, cli)
			require.NotEmpty(t, remotePods, "the qualified tuple rendered a remote replica")

			modelTransferStaleRemoteView(t, cli, c.stale)

			// The source drains. The replica still serves the remote delivery,
			// and the deployment's own view names none.
			drainSource(false)
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			after := modelTransferRemotePods(t, cli)
			require.Len(t, after, len(remotePods),
				"the live remote replica is still there")
			for _, pod := range after {
				assert.Nil(t, pod.DeletionTimestamp,
					"the live remote replica is not condemned: the delivery stays remote, so the "+
						"desired spec does not change and the rollout has no reason to replace it")
			}
		})
	}
}

// TestModelDeploymentModelTransferSourceLossReplacesAReplicaOnTheBaseDelivery is the
// control for the hold above: the same fixture with the remote delivery out of the
// picture. The desired delivery is the base delivery, the desired spec changes, and
// the rollout replaces the replica. The hold above is therefore the delivery's doing
// and not a fixture that could never replace anything.
func TestModelDeploymentModelTransferSourceLossReplacesAReplicaOnTheBaseDelivery(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	drainSource := modelTransferSourceReadyStub(t, true)
	md := newRenderDeployment(func(m *workercore.ModelDeployment) {
		m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
		m.Spec.KVCache = nil
	})
	cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	modelTransferRolloutSetup(t, cli)
	setEndpointEligibilityPodsReady(t, cli, true)
	modelTransferSetReplicasLoaded(t, cli)
	remotePods := modelTransferRemotePods(t, cli)
	require.NotEmpty(t, remotePods, "the qualified tuple rendered a remote replica")

	modelTransferStaleRemoteView(t, cli, false)

	// The remote delivery leaves the picture: the policy no longer engages it,
	// so the desired delivery is the base delivery.
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryDisabled)
	drainSource(false)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	after := modelTransferRemotePods(t, cli)
	require.Len(t, after, len(remotePods),
		"the replicas are still listed while they terminate")
	condemned := 0
	for _, pod := range after {
		if pod.DeletionTimestamp != nil {
			condemned++
		}
	}
	assert.Positive(t, condemned,
		"the rollout replaces the replica: the base delivery is a different desired spec, and the "+
			"rollout turns one replica of the role over per pass")
}

// TestModelDeploymentModelTransferUnqualifiedTupleHoldsANotReadyRemoteReplica pins
// the hold on a replica whose readiness says nothing about its load history. A
// replica that loaded and is waiting on something else is not Ready, and a replica
// that never loaded is not Ready either, so the Pod cannot tell the two apart and
// the hold must not read readiness as a load record.
func TestModelDeploymentModelTransferUnqualifiedTupleHoldsANotReadyRemoteReplica(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	modelTransferSourceReadyStub(t, true)
	md := newRenderDeployment(func(m *workercore.ModelDeployment) {
		m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
		m.Spec.KVCache = nil
	})
	cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	modelTransferRolloutSetup(t, cli)
	setEndpointEligibilityPodsReady(t, cli, true)
	modelTransferSetReplicasNotReady(t, cli)
	remotePods := modelTransferRemotePods(t, cli)
	require.NotEmpty(t, remotePods, "the qualified tuple rendered a remote replica")

	modelTransferStaleRemoteView(t, cli, false)

	// The qualification is withdrawn: the fallback would replace the replica,
	// and the load history of a replica that is not Ready is unknown.
	modelTransferWithdrawQualification()
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	after := modelTransferRemotePods(t, cli)
	require.Len(t, after, len(remotePods),
		"the remote replica is still there")
	for _, pod := range after {
		assert.Nil(t, pod.DeletionTimestamp,
			"a replica that is not Ready is still a replica that may have loaded: the hold does "+
				"not read readiness as a load record")
	}
}

// TestModelDeploymentModelTransferMultiMemberReplicaKeepsTheServingMember covers the
// multi-member replica: one member serves and another failed its load. The serving
// member protects the replica, so the sweep leaves the replica alone, the gate holds
// the remote delivery, and the rollout turns nothing over.
//
// The order the members arrive in is the case. The sweep reads the members in the
// order they come and lets the last one decide, so the same replica was swept or
// spared depending on which member the list happened to name first.
func TestModelDeploymentModelTransferMultiMemberReplicaKeepsTheServingMember(t *testing.T) {
	cases := []struct {
		name string
		// serving is the member index that serves. The other member failed its
		// load, and the members are listed in index order, so the index says
		// which one the sweep reads first.
		serving int
	}{
		{name: "the serving member comes first", serving: 0},
		{name: "the failed member comes first", serving: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
			modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				m.Spec.KVCache = nil
				m.Spec.Roles[0].Replicas = 1
				m.Spec.Roles[0].ReplicaSize = 2
			})
			cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

			modelTransferQualifiedTuple(t, modelTransferTuple{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Backend:      "nixl",
			})
			modelTransferRolloutSetup(t, cli)
			setEndpointEligibilityPodsReady(t, cli, true)

			members := replicaPods(t, cli)
			require.Len(t, members, 2, "the replica is made of two members")
			require.Equal(t, 0, modelDeploymentPodMemberIndex(&members[0]),
				"the members are listed in member order, so the case names the order the sweep reads")

			modelTransferSetMemberLoads(t, cli, map[int]modelTransferMemberLoad{
				c.serving:     modelTransferMemberServing,
				1 - c.serving: modelTransferMemberFailed,
			})

			// The deployment's own remote view names no replica, so the Pods are
			// the only record of what the replica serves.
			modelTransferStaleRemoteView(t, cli, false)

			// The qualification is withdrawn: the fallback would take the
			// delivery away from the member that serves.
			modelTransferWithdrawQualification()
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			serving := modelTransferMember(t, cli, c.serving)
			assert.Nil(t, serving.DeletionTimestamp,
				"the member that serves is not condemned: the replica has loaded, and the sweep never "+
					"touches a replica a member serves")
			failed := modelTransferMember(t, cli, 1-c.serving)
			assert.NotNil(t, failed.DeletionTimestamp,
				"the failed member leaves on its own terms: it is a terminal replica, and a terminal "+
					"replica is a departure this operator did not initiate")
			assert.Empty(t, modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryNode),
				"no replica takes the local delivery: the gate holds the remote delivery while a member "+
					"serves")
			assert.Empty(t, modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryEngine),
				"no replica takes the local delivery: the gate holds the remote delivery while a member "+
					"serves")
		})
	}
}

// TestModelDeploymentModelTransferMultiMemberReplicaHoldsTheUnknownMember covers the
// member whose load history is unknown: it is running and not Ready, so it may
// already have loaded. A replica with such a member and a member that failed is
// not a replica whose load failed, so the sweep leaves it alone and the member
// that may have loaded keeps its place.
//
// The order the members arrive in is the case, as it is for the serving member
// above: the sweep reads the members in the order they come.
func TestModelDeploymentModelTransferMultiMemberReplicaHoldsTheUnknownMember(t *testing.T) {
	cases := []struct {
		name string
		// unknown is the member index whose load history is unknown. The other
		// member failed its load, and the members are listed in index order, so
		// the index says which one the sweep reads first.
		unknown int
	}{
		{name: "the unknown member comes first", unknown: 0},
		{name: "the failed member comes first", unknown: 1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
			modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				m.Spec.KVCache = nil
				m.Spec.Roles[0].Replicas = 1
				m.Spec.Roles[0].ReplicaSize = 2
			})
			cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

			modelTransferQualifiedTuple(t, modelTransferTuple{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Backend:      "nixl",
			})
			modelTransferRolloutSetup(t, cli)
			setEndpointEligibilityPodsReady(t, cli, true)

			members := replicaPods(t, cli)
			require.Len(t, members, 2, "the replica is made of two members")
			require.Equal(t, 0, modelDeploymentPodMemberIndex(&members[0]),
				"the members are listed in member order, so the case names the order the sweep reads")

			modelTransferSetMemberLoads(t, cli, map[int]modelTransferMemberLoad{
				c.unknown:     modelTransferMemberUnknown,
				1 - c.unknown: modelTransferMemberFailed,
			})

			// The deployment's own remote view names no replica, so the Pods are
			// the only record of what the replica serves.
			modelTransferStaleRemoteView(t, cli, false)

			// The qualification is withdrawn: the fallback would take the
			// delivery away from a member that may have loaded.
			modelTransferWithdrawQualification()
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			unknown := modelTransferMember(t, cli, c.unknown)
			assert.Nil(t, unknown.DeletionTimestamp,
				"the member whose load history is unknown is not condemned: readiness is no proof "+
					"that it never loaded, so the sweep does not touch it")
			assert.Empty(t, modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryNode),
				"no replica takes the local delivery: the gate holds the remote delivery while a member "+
					"may have loaded")
			assert.Empty(t, modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryEngine),
				"no replica takes the local delivery: the gate holds the remote delivery while a member "+
					"may have loaded")
		})
	}
}

// TestModelDeploymentModelTransferMultiMemberFailedReplicaFallsBack covers the other
// side of the boundary the case above draws: a replica whose every member failed
// its load is a replica whose load failed, so it falls back to the declared local
// delivery.
func TestModelDeploymentModelTransferMultiMemberFailedReplicaFallsBack(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	modelTransferSourceReadyStub(t, true)
	md := newRenderDeployment(func(m *workercore.ModelDeployment) {
		m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
		m.Spec.KVCache = nil
		m.Spec.Roles[0].Replicas = 1
		m.Spec.Roles[0].ReplicaSize = 2
	})
	cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	modelTransferRolloutSetup(t, cli)
	setEndpointEligibilityPodsReady(t, cli, true)

	modelTransferSetMemberLoads(t, cli, map[int]modelTransferMemberLoad{
		0: modelTransferMemberFailed,
		1: modelTransferMemberFailed,
	})

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	for member := range 2 {
		failed := modelTransferMember(t, cli, member)
		assert.NotNil(t, failed.DeletionTimestamp,
			"every member of a replica whose load failed goes, so the replica recreates on the fallback")
	}
	held := getModelDeployment(t, cli)
	require.NotNil(t, held.Status.Model)
	require.NotNil(t, held.Status.Model.Remote)
	entry := modelDeploymentModelTransferReplicaEntry(held.Status.Model.Remote, "server", 0)
	assert.NotEqual(t, workercore.ModelDeploymentModelDeliveryRemote, entry.Delivery,
		"the replica lands on the declared local delivery")
	assert.Equal(t, int32(1), entry.FallbackAttempts,
		"the group is one attempt, not one per member")
}

// TestModelDeploymentModelTransferMultiMemberUnknownMemberRollsOnTheBaseDelivery is
// the control for the hold above: the same fixture with the remote delivery out of
// the picture. The desired delivery is the base delivery, so the rollout turns the
// replica over, and the hold above is the delivery's doing and not a fixture that
// could never turn anything over.
func TestModelDeploymentModelTransferMultiMemberUnknownMemberRollsOnTheBaseDelivery(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	modelTransferSourceReadyStub(t, true)
	md := newRenderDeployment(func(m *workercore.ModelDeployment) {
		m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
		m.Spec.KVCache = nil
		m.Spec.Roles[0].Replicas = 1
		m.Spec.Roles[0].ReplicaSize = 2
	})
	cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	modelTransferRolloutSetup(t, cli)
	setEndpointEligibilityPodsReady(t, cli, true)

	modelTransferSetMemberLoads(t, cli, map[int]modelTransferMemberLoad{
		0: modelTransferMemberUnknown,
		1: modelTransferMemberFailed,
	})
	modelTransferStaleRemoteView(t, cli, false)

	// The remote delivery leaves the picture: the policy no longer engages it,
	// so the desired delivery is the base delivery.
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryDisabled)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	unknown := modelTransferMember(t, cli, 0)
	assert.NotNil(t, unknown.DeletionTimestamp,
		"the rollout turns the replica over: the base delivery is a different desired spec")
}

// TestModelDeploymentModelTransferMultiMemberReplicaRollsOnTheBaseDelivery is the
// control for the hold above: the same fixture, the same missing remote view and
// the same short replica, with the remote delivery out of the picture. The desired
// delivery is the base delivery, so the rollout turns the replica over. The only
// difference is the delivery, so the serving member's condemnation above is the
// delivery's doing and not a fixture that could never turn anything over.
func TestModelDeploymentModelTransferMultiMemberReplicaRollsOnTheBaseDelivery(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	modelTransferSourceReadyStub(t, true)
	md := newRenderDeployment(func(m *workercore.ModelDeployment) {
		m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
		m.Spec.KVCache = nil
		m.Spec.Roles[0].Replicas = 1
		m.Spec.Roles[0].ReplicaSize = 2
	})
	cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

	modelTransferQualifiedTuple(t, modelTransferTuple{
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Backend:      "nixl",
	})
	modelTransferRolloutSetup(t, cli)
	setEndpointEligibilityPodsReady(t, cli, true)

	modelTransferSetMemberLoads(t, cli, map[int]modelTransferMemberLoad{
		0: modelTransferMemberServing,
		1: modelTransferMemberFailed,
	})
	modelTransferStaleRemoteView(t, cli, false)

	// The remote delivery leaves the picture: the policy no longer engages it,
	// so the desired delivery is the base delivery.
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryDisabled)
	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	serving := modelTransferMember(t, cli, 0)
	assert.NotNil(t, serving.DeletionTimestamp,
		"the rollout turns the replica over: the base delivery is a different desired spec")
}

// TestModelDeploymentModelTransferUnqualifiedTupleHoldsALiveRemoteReplica pins
// the hold on the Pods alone: a replica that still serves the remote delivery
// holds the delivery even when the deployment's own remote view names nothing.
func TestModelDeploymentModelTransferUnqualifiedTupleHoldsALiveRemoteReplica(t *testing.T) {
	md := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
		},
	}

	got, reason, message := modelTransferRemoteRender().modelTransferForRole(md, &workercore.ModelDeploymentRole{},
		nodefeature.ManufacturerAscend, true)
	assert.Nil(t, got, "a fallback here would roll the replica that still serves")
	assert.Equal(t, modelDeploymentModelTransferReasonUnqualifiedBackend, reason)
	assert.Contains(t, message, "roll",
		"the reason says why the fallback was not taken")
}

// TestModelDeploymentModelTransferUnqualifiedTupleKeepsServingReplicas
// reconciles a deployment twice: once while its tuple is qualified, so a
// remote replica exists, and once with the qualification withdrawn. The second
// pass must leave the replica that loaded alone, and it is the Pod that says
// so, not the status.
func TestModelDeploymentModelTransferUnqualifiedTupleKeepsServingReplicas(t *testing.T) {
	cases := []struct {
		name   string
		policy string
		// carried seeds a deployment that already names a remote replica, so
		// Prefer keeps the remote delivery before the first pass renders it.
		carried bool
	}{
		{name: "Require refuses and holds the remote replica", policy: settings.ModelArtifactRemoteDeliveryRequire},
		{
			name:   "Prefer holds the remote replica instead of rolling it",
			policy: settings.ModelArtifactRemoteDeliveryPrefer, carried: true,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			modelTransferPolicyStub(t, c.policy)
			modelTransferSourceReadyStub(t, true)
			md := newRenderDeployment(func(m *workercore.ModelDeployment) {
				m.Spec.Model.ArtifactRef = &core.LocalObjectReference{Name: "weights"}
				// No KVCache pool: a deployment that names one holds its replicas while the
				// connector is unresolved, and that hold would keep the replica alive for a
				// reason of its own, which is not what this case is about.
				m.Spec.KVCache = nil
				if c.carried {
					m.Status.Model = &workercore.ModelDeploymentModelStatus{
						Remote: &workercore.ModelDeploymentModelRemoteStatus{
							Policy: workercore.ModelDeploymentRemotePolicyPrefer,
							Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
								Role: "server",
								Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
									Ordinal: 0, Delivery: workercore.ModelDeploymentModelDeliveryRemote,
									Load: workercore.ModelDeploymentRemoteLoadStateLoaded,
								}},
							}},
						},
					}
				}
			})
			cli := modelTransferFixtureClient(t, md, nodefeature.ManufacturerNVIDIA)

			// The first pass renders the remote delivery, because the tuple is
			// qualified for the duration of this test, and the fixture then
			// reaches the admitted steady state a real cluster has: a rollout
			// only turns a replica over when its Workload holds an admission,
			// so without that the assertion below would be satisfied by the
			// fixture's own hold rather than by the refusal.
			modelTransferQualifiedTuple(t, modelTransferTuple{
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Engine:       workercore.ModelDeploymentEngineVLLM,
				Backend:      "nixl",
			})
			modelTransferRolloutSetup(t, cli)
			setEndpointEligibilityPodsReady(t, cli, true)
			remotePods := modelTransferRemotePods(t, cli)
			require.NotEmpty(t, remotePods, "the qualified tuple rendered a remote replica")

			// The qualification is withdrawn: the tuple is unqualified from
			// here on, and nothing may roll the replica that loaded.
			modelTransferWithdrawQualification()
			_, err := reconcileModelDeployment(t, cli)
			require.NoError(t, err)

			after := modelTransferRemotePods(t, cli)
			require.Len(t, after, len(remotePods),
				"the replica that loaded is not deleted by the refusal")
			for _, pod := range after {
				assert.Nil(t, pod.DeletionTimestamp,
					"the replica that loaded is not condemned: a rollback to the fallback changes the "+
						"desired delivery, and the rollout would turn this replica over")
				assert.Equal(t, string(workercore.ModelDeploymentModelDeliveryRemote),
					pod.Annotations[modelDeploymentModelTransferDeliveryAnnotation],
					"the surviving replica keeps the remote delivery")
			}
			fallbacks := modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryNode)
			assert.Empty(t, fallbacks,
				"the refusal takes no fallback: a fallback replica would carry the delivery away from the one that loaded")
			fallbacks = modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryEngine)
			assert.Empty(t, fallbacks,
				"the refusal takes no fallback: a fallback replica would carry the delivery away from the one that loaded")
			held := getModelDeployment(t, cli)
			require.NotNil(t, held.Status.Conditions)
			assert.Equal(t, modelDeploymentModelTransferReasonUnqualifiedBackend,
				ModelDeploymentConditionWeightsReady.GetReason(held),
				"the refusal is the visible answer")
		})
	}
}

// modelTransferRemotePods lists the deployment's Pods that render the remote delivery.
func modelTransferRemotePods(t *testing.T, cli ctrlcli.Client) []core.Pod {
	t.Helper()

	return modelTransferPodsWithDelivery(t, cli, workercore.ModelDeploymentModelDeliveryRemote)
}

// modelTransferPodsWithDelivery lists the deployment's Pods that render a delivery.
func modelTransferPodsWithDelivery(
	t *testing.T, cli ctrlcli.Client, delivery workercore.ModelDeploymentModelDelivery,
) []core.Pod {
	t.Helper()

	list := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), list, ctrlcli.InNamespace("team-a")))
	pods := make([]core.Pod, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].Annotations[modelDeploymentModelTransferDeliveryAnnotation] == string(delivery) {
			pods = append(pods, list.Items[i])
		}
	}

	return pods
}

// TestModelDeploymentModelTransferWatchFeedsOnSourceHealth pins the finding-4 contract: a model-transfer report
// change is a deployment-relevant event, and the mapper wakes the deployments the digest names —
// remote and Prefer alike, beside the node-delivered ones.
func TestModelDeploymentModelTransferWatchFeedsOnSourceHealth(t *testing.T) {
	before := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: modelTransferTestDigest, State: workercore.NodeModelStoreModelStateReady,
			}},
		},
	}
	after := before.DeepCopy()
	after.Status.ModelTransfer = &workercore.NodeModelStoreModelTransferStatus{
		Generations: []workercore.NodeModelStoreModelTransferGeneration{{
			Generation: "7", Digest: modelTransferTestDigest,
			State: workercore.NodeModelStoreModelTransferGenerationStateServing,
		}},
	}
	assert.True(t, nodeModelStoreModelsChanged().Update(ctrlevent.UpdateEvent{
		ObjectOld: before, ObjectNew: after,
	}), "a generation arriving wakes the deployments that wait on it")

	remoteMD := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "remote"},
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{
				ManifestDigest: modelTransferTestDigest,
				Delivery:       workercore.ModelDeploymentModelDeliveryRemote,
			},
		},
	}
	cli := modelTransferTestEnv(t, remoteMD)
	r := &ModelDeploymentReconciler{Client: cli}
	reqs := r.mapModelDeploymentNodeModelStore(context.Background(), after)
	require.Len(t, reqs, 1)
	assert.Equal(t, "remote", reqs[0].Name,
		"the model-transfer report's digest reaches the deployment loading it remotely")

	// An unrelated digest wakes nobody: a store that names only a digest
	// no deployment loads enqueues nothing.
	other := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-2"},
		Status: workercore.NodeModelStoreStatus{
			ModelTransfer: &workercore.NodeModelStoreModelTransferStatus{
				Generations: []workercore.NodeModelStoreModelTransferGeneration{{
					Generation: "9", Digest: "sha256:" + strings.Repeat("e", 64),
					State: workercore.NodeModelStoreModelTransferGenerationStateServing,
				}},
			},
		},
	}
	assert.Empty(t, r.mapModelDeploymentNodeModelStore(context.Background(), other),
		"a digest no deployment names enqueues nothing")
}

func TestModelDeploymentModelTransferWeightsReadiness(t *testing.T) {
	remote := workercore.ModelDeploymentModelDeliveryRemote
	ready := modelTransferPod("s0", 0, remote, core.PodRunning, true)
	missingDelivery := ready.DeepCopy()
	missingDelivery.Annotations = nil
	departing := ready.DeepCopy()
	now := meta.Now()
	departing.DeletionTimestamp = &now
	cases := []struct {
		name     string
		replicas int32
		source   bool
		pods     []core.Pod
		reason   string
	}{
		{name: "loaded survives source loss", replicas: 1, pods: []core.Pod{*ready}, reason: modelWeightsReasonLoaded},
		{name: "missing desired replica", replicas: 1, source: true, reason: modelWeightsReasonLoading},
		{name: "departing replica", replicas: 1, source: true, pods: []core.Pod{*departing}, reason: modelWeightsReasonLoading},
		{name: "partial desired population", replicas: 2, source: true, pods: []core.Pod{*ready}, reason: modelWeightsReasonLoading},
		{name: "no desired replicas", reason: modelWeightsReasonLoaded},
		{name: "no delivery evidence", replicas: 1, source: true, pods: []core.Pod{*missingDelivery}, reason: modelWeightsReasonLoading},
		{name: "waiting for source", replicas: 1, reason: modelWeightsReasonAwaitingSource},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			holder := mdWithModelTransferStatus()
			holder.Spec.Roles = []workercore.ModelDeploymentRole{{Name: "s0", Replicas: tc.replicas}}
			render := modelTransferRemoteRender()
			render.SourceReady = tc.source
			modelDeploymentModelTransferWeightsVoice(holder, tc.pods, &modelArtifactWeights{Render: render})
			assert.Equal(t, tc.reason, ModelDeploymentConditionWeightsReady.GetReason(holder))
		})
	}
}

func TestModelDeploymentModelTransferGroupObservationIsOrderIndependent(t *testing.T) {
	cases := []struct {
		name     string
		phase    core.PodPhase
		ready    bool
		missing  bool
		delivery workercore.ModelDeploymentModelDelivery
		load     workercore.ModelDeploymentRemoteLoadState
	}{
		{name: "failed member", phase: core.PodFailed, delivery: workercore.ModelDeploymentModelDeliveryRemote, load: workercore.ModelDeploymentRemoteLoadStateUnknown},
		{name: "loading member", phase: core.PodRunning, delivery: workercore.ModelDeploymentModelDeliveryRemote, load: workercore.ModelDeploymentRemoteLoadStateLoading},
		{name: "all loaded", phase: core.PodRunning, ready: true, delivery: workercore.ModelDeploymentModelDeliveryRemote, load: workercore.ModelDeploymentRemoteLoadStateLoaded},
		{name: "missing member", missing: true, load: workercore.ModelDeploymentRemoteLoadStateAwaitingMetadata},
		{name: "inconsistent delivery", phase: core.PodRunning, ready: true, delivery: workercore.ModelDeploymentModelDeliveryNode, load: workercore.ModelDeploymentRemoteLoadStateUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			leader := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodRunning, true)
			member := modelTransferPod("s0", 0, tc.delivery, tc.phase, tc.ready)
			for i, pod := range []*core.Pod{leader, member} {
				pod.Name += "-" + strconv.Itoa(i)
				pod.Labels[modelDeploymentMemberIndexLabel] = strconv.Itoa(i)
				pod.Annotations[kueuepodconst.GroupTotalCountAnnotation] = "2"
			}
			pods := []core.Pod{*leader}
			if !tc.missing {
				pods = append(pods, *member)
			}
			holder := mdWithModelTransferStatus()
			holder.Spec.Roles = []workercore.ModelDeploymentRole{{Name: "s0", Replicas: 1, ReplicaSize: 2}}
			weights := &modelArtifactWeights{Render: modelTransferRemoteRender()}
			weights.Render.SourceReady = true
			modelDeploymentModelTransferObserveStatus(holder, pods, weights, nil, true)
			first := holder.Status.Model.Remote.DeepCopy()
			require.Len(t, first.Roles, 1)
			require.Len(t, first.Roles[0].Replicas, 1)
			assert.Equal(t, tc.load, first.Roles[0].Replicas[0].Load)
			slices.Reverse(pods)
			modelDeploymentModelTransferObserveStatus(holder, pods, weights, nil, true)
			assert.Equal(t, first, holder.Status.Model.Remote)
			modelDeploymentModelTransferWeightsVoice(holder, pods, weights)
			want := modelWeightsReasonLoading
			if tc.load == workercore.ModelDeploymentRemoteLoadStateLoaded {
				want = modelWeightsReasonLoaded
			}
			assert.Equal(t, want, ModelDeploymentConditionWeightsReady.GetReason(holder))
		})
	}
}

func TestModelTransferFallbackDecisionPrecedesDeletion(t *testing.T) {
	for _, tc := range []struct {
		name       string
		failStatus bool
	}{
		{name: "status failure preserves pods", failStatus: true},
		{name: "deletion retry preserves attempt"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := context.Background()
			md := mdWithModelTransferStatus()
			md.Name, md.Namespace = "model", "team-a"
			pod := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false)
			base := modelTransferTestEnv(t, md, pod)
			require.NoError(t, base.Get(ctx, ctrlcli.ObjectKeyFromObject(md), md))
			deleteCalls := 0
			cli := ctrlinterceptor.NewClient(base, ctrlinterceptor.Funcs{
				SubResourceUpdate: func(ctx context.Context, c ctrlcli.Client, sub string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
					if tc.failStatus {
						return fmt.Errorf("injected status failure")
					}
					return c.SubResource(sub).Update(ctx, obj, opts...)
				},
				Delete: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
					deleteCalls++
					saved := new(workercore.ModelDeployment)
					require.NoError(t, c.Get(ctx, ctrlcli.ObjectKeyFromObject(md), saved))
					require.Equal(t, workercore.ModelDeploymentModelDeliveryNode, modelDeploymentModelTransferReplicaOverride(saved, "s0", 0))
					return fmt.Errorf("injected deletion failure")
				},
			})
			_, err := modelDeploymentModelTransferSweep(ctx, cli, md, []core.Pod{*pod}, settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
			require.Error(t, err)
			require.NoError(t, base.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), new(core.Pod)))
			saved := new(workercore.ModelDeployment)
			require.NoError(t, base.Get(ctx, ctrlcli.ObjectKeyFromObject(md), saved))
			if tc.failStatus {
				require.Zero(t, deleteCalls)
				require.Nil(t, saved.Status.Model.Remote)
				return
			}
			require.Equal(t, int32(1), saved.Status.Model.Remote.Roles[0].Replicas[0].FallbackAttempts)
			_, err = modelDeploymentModelTransferSweep(ctx, base, saved, []core.Pod{*pod}, settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
			require.NoError(t, err)
			require.NoError(t, base.Get(ctx, ctrlcli.ObjectKeyFromObject(md), saved))
			require.Equal(t, int32(1), saved.Status.Model.Remote.Roles[0].Replicas[0].FallbackAttempts)
		})
	}
}

func TestModelTransferSweepRecoversTerminatingFallbackMembers(t *testing.T) {
	ctx := context.Background()
	md := mdWithModelTransferStatus()
	md.Name, md.Namespace = "model", "team-a"
	modelDeploymentModelTransferBumpAttempt(md, "s0", 0, workercore.ModelDeploymentModelDeliveryNode)
	pod := modelTransferPod("s0", 0, workercore.ModelDeploymentModelDeliveryRemote, core.PodFailed, false)
	pod.DeletionTimestamp = &meta.Time{Time: time.Now()}
	pod.Finalizers = []string{"kueue.x-k8s.io/managed"}
	cli := modelTransferTestEnv(t, md, pod)
	for range 2 {
		swept, err := modelDeploymentModelTransferSweep(ctx, cli, md, []core.Pod{*pod},
			settings.ModelArtifactRemoteDeliveryPrefer, workercore.ModelDeploymentModelDeliveryNode)
		require.NoError(t, err)
		require.Len(t, swept, 1, "the caller must retry deleting the held Kueue Workload")
		assert.Equal(t, int32(1), md.Status.Model.Remote.Roles[0].Replicas[0].FallbackAttempts)
	}
}

func TestModelTransferPolicyPropagatesCSIDriverReadFailure(t *testing.T) {
	for _, policy := range []string{settings.ModelArtifactRemoteDeliveryPrefer, settings.ModelArtifactRemoteDeliveryRequire} {
		t.Run(policy, func(t *testing.T) {
			modelTransferPolicyStub(t, policy)
			cli := ctrlinterceptor.NewClient(modelTransferTestEnv(t), ctrlinterceptor.Funcs{
				Get: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
					return fmt.Errorf("injected API failure")
				},
			})
			weights := &modelArtifactWeights{Render: &ModelDeploymentArtifactRender{Delivery: workercore.ModelDeploymentModelDeliveryEngine}}
			artifact := &workercore.ModelArtifact{Status: workercore.ModelArtifactStatus{Resolved: &workercore.ModelArtifactResolved{ManifestDigest: modelTransferTestDigest}}}
			err := applyModelDeploymentModelTransferPolicy(context.Background(), cli, weights, artifact, nil)
			require.ErrorContains(t, err, "injected API failure")
			assert.False(t, weights.Blocked)
			assert.Equal(t, workercore.ModelDeploymentModelDeliveryEngine, weights.Render.Delivery)
		})
	}
}

func TestModelTransferFallbackIgnoresSiblingRemoteRole(t *testing.T) {
	md := mdWithModelTransferStatus()
	modelDeploymentModelTransferBumpAttempt(md, "sibling", 0, workercore.ModelDeploymentModelDeliveryRemote)
	role := &workercore.ModelDeploymentRole{Name: "new-role"}
	render := modelTransferRemoteRender()
	effective, reason, _ := render.modelTransferForRole(md, role, nodefeature.ManufacturerAscend, false)
	require.Empty(t, reason)
	require.NotNil(t, effective)
	assert.Equal(t, workercore.ModelDeploymentModelDeliveryNode, effective.Delivery)
	assert.False(t, modelDeploymentModelTransferRoleLive(sets.New("sibling/0"), role.Name))
	assert.True(t, modelDeploymentModelTransferRoleLive(sets.New("new-role/0"), role.Name))
}

func TestModelTransferHistoryFollowsArtifactIdentity(t *testing.T) {
	for _, tc := range []struct {
		name     string
		artifact string
		revision string
		digest   string
		keep     bool
	}{
		{name: "same", artifact: "weights", revision: "rev", digest: modelTransferTestDigest, keep: true},
		{name: "different artifact", artifact: "other", revision: "rev", digest: modelTransferTestDigest},
		{name: "different revision", artifact: "weights", revision: "new", digest: modelTransferTestDigest},
		{name: "different digest", artifact: "weights", revision: "rev", digest: "sha256:" + strings.Repeat("b", 64)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := mdWithModelTransferStatus()
			md.Status.Model.Artifact, md.Status.Model.Revision, md.Status.Model.ManifestDigest = "weights", "rev", modelTransferTestDigest
			modelDeploymentModelTransferBumpAttempt(md, "s0", 0, workercore.ModelDeploymentModelDeliveryNode)
			weights := &modelArtifactWeights{Blocked: true, Render: modelTransferRemoteRender(), Status: &workercore.ModelDeploymentModelStatus{
				Artifact: tc.artifact, Revision: tc.revision, ManifestDigest: tc.digest,
			}}
			observeModelDeploymentWeights(md, nil, weights, nil)
			assert.Equal(t, tc.keep, md.Status.Model.Remote != nil)
		})
	}
}

func TestModelTransferInterfaceFollowsResolvedDelivery(t *testing.T) {
	for _, tc := range []struct {
		name     string
		delivery workercore.ModelDeploymentModelDelivery
		want     string
	}{
		{name: "remote", delivery: workercore.ModelDeploymentModelDeliveryRemote, want: "rdma"},
		{name: "node fallback", delivery: workercore.ModelDeploymentModelDeliveryNode},
		{name: "engine fallback", delivery: workercore.ModelDeploymentModelDeliveryEngine},
	} {
		t.Run(tc.name, func(t *testing.T) {
			md := mdWithModelTransferStatus()
			role := &workercore.ModelDeploymentRole{Name: "server"}
			remoteDelivery := tc.delivery == workercore.ModelDeploymentModelDeliveryRemote
			actual := ModelDeploymentDirectInterfaceProtocol(md, role, nodefeature.ManufacturerNVIDIA,
				remoteDelivery, false)
			assert.Equal(t, tc.want, actual)
		})
	}
}

func TestModelTransferQualifiedRolesOwnTheirRender(t *testing.T) {
	modelTransferQualifiedTuple(t, modelTransferTuple{Manufacturer: nodefeature.ManufacturerNVIDIA, Engine: workercore.ModelDeploymentEngineVLLM, Backend: "nixl"})
	md := &workercore.ModelDeployment{Spec: workercore.ModelDeploymentSpec{Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM}}}
	base := modelTransferRemoteRender()
	first, reason, _ := base.modelTransferForRole(md, &workercore.ModelDeploymentRole{Name: "first"}, nodefeature.ManufacturerNVIDIA, false)
	require.Empty(t, reason)
	first.ModelTransferBackend = "nixl"
	second, reason, _ := base.modelTransferForRole(md, &workercore.ModelDeploymentRole{Name: "second"}, nodefeature.ManufacturerNVIDIA, false)
	require.Empty(t, reason)
	second.ModelTransferBackend = "other-role-backend"
	require.Equal(t, "nixl", first.ModelTransferBackend)
	require.Empty(t, base.ModelTransferBackend)
}

func TestModelTransferIneligibleRoleHoldsALoadedRemoteReplica(t *testing.T) {
	md := &workercore.ModelDeployment{
		Spec: workercore.ModelDeploymentSpec{
			Engine: workercore.ModelDeploymentEngine{Name: workercore.ModelDeploymentEngineVLLM},
		},
		Status: workercore.ModelDeploymentStatus{
			Model: &workercore.ModelDeploymentModelStatus{
				Remote: &workercore.ModelDeploymentModelRemoteStatus{
					Policy: workercore.ModelDeploymentRemotePolicyPrefer,
					Roles: []workercore.ModelDeploymentRemoteRoleStatus{{
						Role: "s0",
						Replicas: []workercore.ModelDeploymentRemoteReplicaStatus{{
							Ordinal: 0, Delivery: workercore.ModelDeploymentModelDeliveryRemote,
							Load: workercore.ModelDeploymentRemoteLoadStateLoaded,
						}},
					}},
				},
			},
		},
	}

	got, reason, message := modelTransferRemoteRender().modelTransferForRole(
		md, &workercore.ModelDeploymentRole{Name: "s0", ExtraArgs: []string{"--load-format=safetensors"}},
		nodefeature.ManufacturerNVIDIA, false,
	)
	assert.Nil(t, got, "an ineligible fallback must not roll a replica that already loaded remotely")
	assert.Equal(t, modelDeploymentModelTransferReasonRoleIneligible, reason)
	assert.Contains(t, message, "roll")
}

// ---------------------------------------------------------------------------
// The ModelExpress delivery: recognition, the sidecar objects, and the staging.
// ---------------------------------------------------------------------------

// modelExpressHubRender is a hub artifact render for a ModelExpress role: the weights'
// hub identity is what a published source identity names.
func modelExpressHubRender() *ModelDeploymentArtifactRender {
	return &ModelDeploymentArtifactRender{
		Delivery:       workercore.ModelDeploymentModelDeliveryNode,
		Hub:            modelDeploymentHubHuggingFace,
		ArtifactName:   "weights",
		ArtifactUID:    "artifact-uid-1",
		ManifestDigest: modelTransferTestDigest,
	}
}

func TestModelDeploymentModelExpressLoadFormatRecognition(t *testing.T) {
	cases := []struct {
		name   string
		engine string
		argv   []string
		want   string
	}{
		{name: "none", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "serve", "m"}, want: ""},
		{name: "two token form", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-format", "modelexpress"}, want: "modelexpress"},
		{name: "equals form", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-format=mx"}, want: "mx"},
		{name: "underscore spelling on vLLM", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load_format", "Modelexpress "}, want: "modelexpress"},
		{name: "underscore spelling is not a flag on SGLang", engine: workercore.ModelDeploymentEngineSGLang, argv: []string{"python", "--load_format", "modelexpress"}, want: ""},
		{name: "unique prefix abbreviation", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-form=mx"}, want: "mx"},
		{name: "double dash ends option scanning", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--", "--load-format", "modelexpress"}, want: ""},
		{name: "the last flag wins", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-format", "auto", "--load-format", "mx"}, want: "mx"},
		{name: "a dangling flag names nothing", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-format"}, want: ""},
		{name: "a value with dashes is not an alias", engine: workercore.ModelDeploymentEngineVLLM, argv: []string{"vllm", "--load-format=modelexpressx"}, want: "modelexpressx"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			assert.Equal(t, c.want, extractLoadFormatFromArgs(c.engine, c.argv))
		})
	}

	for _, tc := range []struct {
		name string
		role workercore.ModelDeploymentRole
		want bool
	}{
		{
			name: "managed extra args select modelexpress",
			role: workercore.ModelDeploymentRole{ExtraArgs: []string{"--load-format", "modelexpress"}},
			want: true,
		},
		{
			name: "managed extra args select the shorthand",
			role: workercore.ModelDeploymentRole{ExtraArgs: []string{"--load-format=mx"}},
			want: true,
		},
		{
			name: "a take-over command selects modelexpress",
			role: workercore.ModelDeploymentRole{Command: []string{"python", "-m", "x", "--load-format=Modelexpress"}},
			want: true,
		},
		{
			name: "auto is not model express",
			role: workercore.ModelDeploymentRole{ExtraArgs: []string{"--load-format=auto"}},
			want: false,
		},
		{
			name: "no format at all",
			role: workercore.ModelDeploymentRole{ExtraArgs: []string{"--served-model-name", "qwen"}},
			want: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			role := tc.role
			assert.Equal(t, tc.want, modelDeploymentRoleUsesModelExpress(workercore.ModelDeploymentEngineVLLM, &role))
		})
	}
	assert.False(t, modelDeploymentRoleUsesModelExpress(workercore.ModelDeploymentEngineVLLM, nil), "no role is no claim")
}

func TestModelDeploymentModelExpressResourceNameAndAddress(t *testing.T) {
	short := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
	}
	assert.Equal(t, "qwen-modelexpress", modelDeploymentModelExpressResourceName(short))
	assert.Equal(t, "qwen-modelexpress.team-a.svc:8001", modelDeploymentModelExpressServerAddress(short))

	long := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Name: strings.Repeat("q", 80), Namespace: "team-a"},
	}
	name := modelDeploymentModelExpressResourceName(long)
	assert.LessOrEqual(t, len(name), 63, "the name must fit a DNS label")
	assert.True(t, strings.HasPrefix(name, strings.Repeat("q", 80)[:20]))
	assert.NotContains(t, name, "modelexpress", "a truncated name settles by digest, not by a cut suffix")
	assert.Equal(t, name, modelDeploymentModelExpressResourceName(long), "the digest settles one name")
}

func TestRenderModelDeploymentModelExpressObjects(t *testing.T) {
	modelexpressRole := func() workercore.ModelDeploymentRole {
		return workercore.ModelDeploymentRole{
			Name: "server", ExtraArgs: []string{"--load-format", "modelexpress"},
		}
	}
	cases := []struct {
		name    string
		md      *workercore.ModelDeployment
		wantAny bool
	}{
		{
			name: "a role selecting modelexpress renders the pair",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
				Spec:       workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{modelexpressRole()}},
			},
			wantAny: true,
		},
		{
			name: "the mx shorthand renders the pair",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
				Spec: workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{{
					Name: "server", ExtraArgs: []string{"--load-format=mx"},
				}}},
			},
			wantAny: true,
		},
		{
			name: "a plain role renders nothing",
			md: &workercore.ModelDeployment{
				ObjectMeta: meta.ObjectMeta{Name: "qwen", Namespace: "team-a"},
				Spec: workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{{
					Name: "server", ExtraArgs: []string{"--load-format=auto"},
				}}},
			},
		},
		{
			name: "a deleted deployment renders nothing",
			md: func() *workercore.ModelDeployment {
				now := meta.Now()
				return &workercore.ModelDeployment{
					ObjectMeta: meta.ObjectMeta{
						Name: "qwen", Namespace: "team-a", DeletionTimestamp: &now,
					},
					Spec: workercore.ModelDeploymentSpec{Roles: []workercore.ModelDeploymentRole{modelexpressRole()}},
				}
			}(),
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			deploy, service := renderModelDeploymentModelExpressObjects(context.Background(), c.md)
			if !c.wantAny {
				assert.Nil(t, deploy)
				assert.Nil(t, service)
				return
			}
			require.NotNil(t, deploy)
			require.NotNil(t, service)

			require.Len(t, deploy.Spec.Template.Spec.Containers, 1)
			server := deploy.Spec.Template.Spec.Containers[0]
			assert.Equal(t, appsv1.RecreateDeploymentStrategyType, deploy.Spec.Strategy.Type,
				"two servers at once would split the replicas' registrations")
			assert.Equal(t, int32(1), *deploy.Spec.Replicas)
			assert.Equal(t, []string{"modelexpress-server"}, server.Command)
			assert.Equal(t, "gpustack/mirrored-modelexpress-server:v0.7.0",
				strings.TrimSpace(server.Image))
			env := map[string]string{}
			for _, e := range server.Env {
				env[e.Name] = e.Value
			}
			assert.Equal(t, "memory", env["MX_METADATA_BACKEND"])
			assert.Equal(t, "8001", env["MODEL_EXPRESS_SERVER_PORT"])
			require.Len(t, server.Ports, 1)
			assert.Equal(t, int32(8001), server.Ports[0].ContainerPort)
			require.NotNil(t, server.ReadinessProbe)
			require.NotNil(t, server.ReadinessProbe.TCPSocket)
			assert.Equal(t, intstr.FromInt32(8001), server.ReadinessProbe.TCPSocket.Port)
			require.NotNil(t, server.LivenessProbe)
			require.NotNil(t, server.LivenessProbe.TCPSocket)
			assert.Equal(t, intstr.FromInt32(8001), server.LivenessProbe.TCPSocket.Port)

			assert.Equal(t, core.ServiceTypeClusterIP, service.Spec.Type)
			require.Len(t, service.Spec.Ports, 1)
			assert.Equal(t, "grpc", service.Spec.Ports[0].Name)
			assert.Equal(t, int32(8001), service.Spec.Ports[0].Port)
			assert.Equal(t, intstr.FromInt32(8001), service.Spec.Ports[0].TargetPort)
			assert.Equal(t, core.ProtocolTCP, service.Spec.Ports[0].Protocol)
		})
	}
}

func TestModelDeploymentModelTransferRoleKeepsModelexpressFormat(t *testing.T) {
	// The three delivered formats are the operator's own vocabulary: a role selecting one
	// of them is not in conflict with the operator, and a role selecting anything else is.
	vllm := workercore.ModelDeploymentEngineVLLM
	for _, format := range []string{"model-transfer", "modelexpress", "mx", "MX", " Modelexpress "} {
		assert.False(t, modelDeploymentRoleHasConflictingLoadFormat(vllm, []string{"--load-format", format}),
			"%q is a delivered format", format)
		assert.False(t, modelDeploymentRoleHasConflictingLoadFormat(vllm, []string{"--load-format=" + format}),
			"%q is a delivered format", format)
	}
	for _, format := range []string{"auto", "safetensors", ""} {
		assert.True(t, modelDeploymentRoleHasConflictingLoadFormat(vllm, []string{"--load-format", format}),
			"%q is not a delivered format", format)
	}
	assert.True(t, modelDeploymentRoleHasConflictingLoadFormat(
		vllm, []string{"--load-format", "--enforce-eager"},
	), "a flag followed by another option has no consumable value and is a conflict")
	assert.True(t, modelDeploymentRoleHasConflictingLoadFormat(
		vllm, []string{"--load-format", "modelexpress", "--load-format", "mx"},
	),
		"two selections are one conflict whatever the values")

	// A remote delivery defers to the role's own selection instead of appending its own.
	render := modelTransferRemoteRender()
	container := &core.Container{
		Name:    modelDeploymentMainContainerName,
		Command: []string{"vllm", "serve", "m", "--load-format", "modelexpress"},
	}
	render.applyModelTransfer(container, workercore.ModelDeploymentEngineVLLM)
	joined := strings.Join(append(slices.Clone(container.Command), container.Args...), " ")
	assert.Contains(t, joined, "--load-format modelexpress")
	assert.NotContains(t, joined, "--load-format model-transfer",
		"the operator never appends a second selection beside the role's own")
	assert.Equal(t, 1, strings.Count(joined, "--load-format"),
		"the effective selection stays the role's own")
}

// modelExpressPodFixture renders one managed vLLM role selecting modelexpress against a
// hub artifact, under the given delivery and policy, and hands back the pod.
func modelExpressPodFixture(t *testing.T, delivery workercore.ModelDeploymentModelDelivery,
	policy string,
) *core.Pod {
	t.Helper()
	modelTransferPolicyStub(t, policy)
	md := newRenderDeployment()
	md.Spec.Roles[0].ExtraArgs = []string{"--load-format", "modelexpress"}
	render := modelExpressHubRender()
	render.Delivery = delivery
	if delivery == workercore.ModelDeploymentModelDeliveryRemote {
		render.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryNode
		render.ModelTransferPolicy = policy
	}
	render.ModelTransferConfigMap = "model-transfer-endpoints"
	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Artifact:        render,
	})
	require.NoError(t, err)

	return pod
}

func modelExpressEnvOf(pod *core.Pod) map[string]string {
	env := map[string]string{}
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Value != "" {
			env[e.Name] = e.Value
		}
	}

	return env
}

// assertModelDeploymentPodValid is what the API server would refuse a Pod over, checked
// before any field-level expectation: every mount names a declared volume, no volume is
// declared twice, and within one container no mount path repeats or lands inside another.
// Two containers sharing one mount path is legal, so the path checks stay per container.
func assertModelDeploymentPodValid(t *testing.T, pod *core.Pod) {
	t.Helper()
	declared := sets.New[string]()
	for i, volume := range pod.Spec.Volumes {
		require.NotEmpty(t, volume.Name, "volume %d names nothing", i)
		require.False(t, declared.Has(volume.Name),
			"volume %q is declared twice", volume.Name)
		declared.Insert(volume.Name)
	}
	for _, c := range append(slices.Clone(pod.Spec.InitContainers), pod.Spec.Containers...) {
		mounts := make([]core.VolumeMount, 0, len(c.VolumeMounts))
		paths := sets.New[string]()
		for _, mount := range c.VolumeMounts {
			require.True(t, declared.Has(mount.Name),
				"container %q mounts undeclared volume %q", c.Name, mount.Name)
			require.False(t, paths.Has(mount.MountPath),
				"container %q mounts path %q twice", c.Name, mount.MountPath)
			paths.Insert(mount.MountPath)
			mounts = append(mounts, mount)
		}
		// Nesting is checked over every unordered pair in both directions, so it
		// holds whatever order the mounts are declared in.
		for i, outer := range mounts {
			for _, inner := range mounts[i+1:] {
				require.False(t, strings.HasPrefix(inner.MountPath, outer.MountPath+"/"),
					"container %q mounts %q nested inside %q",
					c.Name, inner.MountPath, outer.MountPath)
				require.False(t, strings.HasPrefix(outer.MountPath, inner.MountPath+"/"),
					"container %q mounts %q nested inside %q",
					c.Name, outer.MountPath, inner.MountPath)
			}
		}
	}
}

// TestModelDeploymentModelExpressPodPerDelivery renders a --load-format modelexpress
// role under BOTH deliveries and asserts the pod is valid and complete: one command
// wrap, the client env under either delivery, and the weights volume that answers to
// the delivery.
func TestModelDeploymentModelExpressPodPerDelivery(t *testing.T) {
	cases := []struct {
		name     string
		delivery workercore.ModelDeploymentModelDelivery
		policy   string
	}{
		{
			name:     "Node delivery carries the seed replica",
			delivery: workercore.ModelDeploymentModelDeliveryNode,
			policy:   settings.ModelArtifactRemoteDeliveryPrefer,
		},
		{
			name:     "Remote delivery carries the metadata view",
			delivery: workercore.ModelDeploymentModelDeliveryRemote,
			policy:   settings.ModelArtifactRemoteDeliveryRequire,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			pod := modelExpressPodFixture(t, c.delivery, c.policy)
			assertModelDeploymentPodValid(t, pod)

			ctr := pod.Spec.Containers[0]
			// ONE command wrap under either delivery: the bootstrap installs the
			// host-staged wheel once, and a doubly nested sh -c would run it twice.
			require.GreaterOrEqual(t, len(ctr.Command), 4)
			assert.Equal(t, "sh", ctr.Command[0])
			assert.Equal(t, "-c", ctr.Command[1])
			assert.Equal(t, modelDeploymentModelTransferBootstrapScript, ctr.Command[2])
			assert.Equal(t, "sh", ctr.Command[3])
			require.Greater(t, len(ctr.Command), 4)
			assert.NotEqual(t, "sh", ctr.Command[4], "the engine argv is not wrapped again")
			assert.Contains(t, strings.Join(ctr.Command, " "), "--load-format modelexpress",
				"the role's own selection survives the staging")

			// The client env is complete under either delivery: a Node-rendered seed
			// replica reaches a source that becomes ready later through the same env a
			// remote replica reads.
			env := modelExpressEnvOf(pod)
			assert.Equal(t, "qwen-modelexpress.team-a.svc:8001",
				env[modelDeploymentModelExpressEnvServerAddress])
			assert.Equal(t, "1", env[modelDeploymentModelExpressEnvArtifactTransfer])
			assert.Equal(t, "gpustack", env[modelDeploymentModelExpressEnvLoadPlugin])
			assert.Equal(t, "server-cache", env[modelDeploymentModelExpressEnvLoadPluginBefore])
			assert.Equal(t, c.policy, env[modelDeploymentModelTransferEnvDeliveryPolicy])
			assert.Equal(t, "nixl", env[modelDeploymentModelTransferEnvBackend],
				"the client dials the pool's backend under either delivery")
			assert.Equal(t, modelTransferClientMount+"/endpoints.json",
				env["GPUSTACK_MODEL_TRANSFER_CONFIG"])
			assert.Equal(t, modelDeploymentModelTransferDefaultTimeoutSeconds,
				env[modelDeploymentModelTransferEnvTimeout])
			assert.Equal(t, modelDeploymentModelExpressLoadFormat,
				env[modelDeploymentModelTransferEnvLoadFormat],
				"the load-format env names the adapter this role reads")
			assert.Len(t, env[modelDeploymentModelExpressEnvCompileDigest], 16,
				"the compile-config digest covers the whole cache identity")
			assert.Contains(t, env[modelDeploymentModelExpressEnvArtifactReadyURL],
				"http://127.0.0.1:", "the ready hook grades the replica's own engine over plain HTTP")
			for _, identity := range []string{
				"KUBERNETES_POD_UID", "KUBERNETES_POD_NAME", "KUBERNETES_POD_NAMESPACE", "KUBERNETES_POD_IP",
			} {
				found := false
				for _, e := range ctr.Env {
					if e.Name == identity {
						require.NotNil(t, e.ValueFrom)
						require.NotNil(t, e.ValueFrom.FieldRef)
						found = true
					}
				}
				assert.True(t, found, "the pod identity env %q is rendered", identity)
			}
			workerHost := false
			for _, e := range ctr.Env {
				if e.Name == modelDeploymentModelExpressEnvWorkerHost {
					require.NotNil(t, e.ValueFrom)
					require.NotNil(t, e.ValueFrom.FieldRef)
					assert.Equal(t, "status.podIP", e.ValueFrom.FieldRef.FieldPath)
					workerHost = true
				}
			}
			assert.True(t, workerHost, "the worker host is the pod's own IP")

			// The weights volume answers to the delivery; the client volumes are the
			// same trio under either delivery.
			weightsVolume := core.Volume{}
			for _, v := range pod.Spec.Volumes {
				if v.Name == modelDeploymentModelVolumeName {
					weightsVolume = v
				}
			}
			require.NotNil(t, weightsVolume.CSI)
			if c.delivery == workercore.ModelDeploymentModelDeliveryRemote {
				assert.Equal(t, modelDeploymentModelTransferDeliveryMetadata,
					weightsVolume.CSI.VolumeAttributes[modelDeploymentModelTransferVolumeAttrDelivery],
					"the remote view is metadata-only")
			} else {
				assert.Empty(t,
					weightsVolume.CSI.VolumeAttributes[modelDeploymentModelTransferVolumeAttrDelivery],
					"the node view materializes the tree the seed loads from")
			}
			declared := sets.New[string]()
			for _, v := range pod.Spec.Volumes {
				declared.Insert(v.Name)
			}
			for _, wanted := range []string{
				modelDeploymentModelTransferTokenVolumeName,
				modelDeploymentModelTransferWheelVolumeName,
				"gpustack-model-transfer-config",
			} {
				assert.True(t, declared.Has(wanted), "the client volume %q is mounted", wanted)
			}

			// The security context carries the pair the transfer needs.
			require.NotNil(t, ctr.SecurityContext)
			require.NotNil(t, ctr.SecurityContext.Capabilities)
			assert.Contains(t, ctr.SecurityContext.Capabilities.Add, core.Capability("IPC_LOCK"))
			assert.Contains(t, ctr.SecurityContext.Capabilities.Add, core.Capability("SYS_RESOURCE"))
		})
	}

	// A pre-existing GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT in role.env under Remote delivery
	// must never leave a second trailing model-transfer entry after ModelExpress staging
	// settles the load format to modelexpress.
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryRequire)
	md := newRenderDeployment()
	md.Spec.Roles[0].ExtraArgs = []string{"--load-format", "modelexpress"}
	md.Spec.Roles[0].Env = []workercore.ModelDeploymentEnvVar{
		{Name: modelDeploymentModelTransferEnvLoadFormat, Value: modelDeploymentModelTransferLoadFormat},
	}
	render := modelExpressHubRender()
	render.Delivery = workercore.ModelDeploymentModelDeliveryRemote
	render.ModelTransferFallback = workercore.ModelDeploymentModelDeliveryNode
	render.ModelTransferPolicy = settings.ModelArtifactRemoteDeliveryRequire
	render.ModelTransferConfigMap = "model-transfer-endpoints"
	pod, err := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
		Deployment:      md,
		Role:            &md.Spec.Roles[0],
		AcceleratorType: newRenderAcceleratorType(),
		Artifact:        render,
	})
	require.NoError(t, err)
	var loadFormatValues []string
	for _, e := range pod.Spec.Containers[0].Env {
		if e.Name == modelDeploymentModelTransferEnvLoadFormat {
			loadFormatValues = append(loadFormatValues, e.Value)
		}
	}
	assert.Equal(t, []string{modelDeploymentModelExpressLoadFormat}, loadFormatValues,
		"exactly one GPUSTACK_MODEL_TRANSFER_LOAD_FORMAT entry survives and names modelexpress")

	// A Node/Engine fallback ModelExpress replica also defaults GPUSTACK_MODEL_TRANSFER_NIXL_BACKEND
	// to LIBFABRIC when ModelTransferEFA is true (and UCX otherwise).
	for _, efa := range []bool{false, true} {
		fallback := modelExpressHubRender()
		fallback.Delivery = workercore.ModelDeploymentModelDeliveryNode
		fallback.ModelTransferEFA = efa
		fallback.ModelTransferConfigMap = "model-transfer-endpoints"
		fbPod, fbErr := renderModelDeploymentPodTemplate(context.Background(), ModelDeploymentRenderInput{
			Deployment:      md,
			Role:            &md.Spec.Roles[0],
			AcceleratorType: newRenderAcceleratorType(),
			Artifact:        fallback,
		})
		require.NoError(t, fbErr)
		wantPlugin := "UCX"
		if efa {
			wantPlugin = "LIBFABRIC"
		}
		assert.Equal(t, wantPlugin, modelExpressEnvOf(fbPod)[modelDeploymentModelTransferEnvNIXLBackend])
	}
}

func TestModelDeploymentModelExpressEndpointsConfigMapAlwaysReconciled(t *testing.T) {
	modelTransferPolicyStub(t, settings.ModelArtifactRemoteDeliveryPrefer)
	md := newRenderDeployment()
	md.Spec.Roles[0].ExtraArgs = []string{"--load-format", "modelexpress"}
	cli := modelTransferTestEnv(t)
	r := &ModelDeploymentReconciler{Client: cli}

	// A Node delivery with a ModelExpress role still reconciles the discovery view.
	weights := &modelArtifactWeights{Render: modelExpressHubRender()}
	require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), md, weights))
	require.NotEmpty(t, weights.Render.ModelTransferConfigMap,
		"the staging mounts the view the reconciler wrote")
	cm := new(core.ConfigMap)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: md.Namespace, Name: weights.Render.ModelTransferConfigMap}, cm))
	var discovery struct {
		Endpoints []modelTransferEndpoint `json:"endpoints"`
	}
	require.NoError(t, json.Unmarshal([]byte(cm.Data["endpoints.json"]), &discovery))
	assert.Empty(t, discovery.Endpoints, "no ready source is an honest empty view")

	// A plain deployment under a Node delivery reconciles nothing.
	plain := newRenderDeployment()
	plainWeights := &modelArtifactWeights{Render: modelExpressHubRender()}
	require.NoError(t, r.reconcileModelTransferClientConfig(context.Background(), plain, plainWeights))
	assert.Empty(t, plainWeights.Render.ModelTransferConfigMap,
		"no ModelExpress role means no discovery view to mount")
}

// TestApplyModelTransfer_DefaultsTheNIXLTransportPlugin pins the plugin default the adapter
// reads: UCX is the plugin everything else speaks, LIBFABRIC the one an EFA device is reachable
// through, and a role env entry naming the plugin outranks the operator's answer -- which is why
// the default is appended only where the container carries none.
func TestApplyModelTransfer_DefaultsTheNIXLTransportPlugin(t *testing.T) {
	container := func(env ...core.EnvVar) *core.Container {
		return &core.Container{
			Name:    modelDeploymentMainContainerName,
			Command: []string{"vllm", "serve", "org/model"},
			Env:     env,
		}
	}
	envValue := func(c *core.Container, name string) string {
		for i := range c.Env {
			if c.Env[i].Name == name {
				return c.Env[i].Value
			}
		}
		return ""
	}

	t.Run("UCX is the default without an EFA fabric", func(t *testing.T) {
		render := modelTransferRemoteRender()
		render.ModelTransferBackend = "nixl"
		c := container()
		render.applyModelTransfer(c, workercore.ModelDeploymentEngineVLLM)
		assert.Equal(t, "UCX", envValue(c, modelDeploymentModelTransferEnvNIXLBackend))
	})

	t.Run("LIBFABRIC follows the EFA report", func(t *testing.T) {
		render := modelTransferRemoteRender()
		render.ModelTransferBackend = "nixl"
		render.ModelTransferEFA = true
		c := container()
		render.applyModelTransfer(c, workercore.ModelDeploymentEngineVLLM)
		assert.Equal(t, "LIBFABRIC", envValue(c, modelDeploymentModelTransferEnvNIXLBackend))
	})

	t.Run("a role env entry outranks the default", func(t *testing.T) {
		render := modelTransferRemoteRender()
		render.ModelTransferBackend = "nixl"
		render.ModelTransferEFA = true
		c := container(core.EnvVar{
			Name: modelDeploymentModelTransferEnvNIXLBackend, Value: "UCX",
		})
		render.applyModelTransfer(c, workercore.ModelDeploymentEngineVLLM)
		assert.Equal(t, "UCX", envValue(c, modelDeploymentModelTransferEnvNIXLBackend),
			"the plugin is defaulted, never owned: the workload may answer for its image")
	})

	t.Run("the hixl backend carries no plugin selection", func(t *testing.T) {
		render := modelTransferRemoteRender()
		render.ModelTransferBackend = "hixl"
		render.ModelTransferEFA = true
		c := container()
		render.applyModelTransfer(c, workercore.ModelDeploymentEngineVLLM)
		assert.Empty(t, envValue(c, modelDeploymentModelTransferEnvNIXLBackend),
			"the Ascend data plane picks its transport on the engine side, not here")
	})
}

// TestModelDeploymentDirectInterfaceProtocol_SGLangAndRemoteEFA covers the two legs the fabric
// answer widened for: an SGLang prefill/decode pair resolves the direct protocol the way a
// vLLM pair does when both halves request interfaces, and NIXL remote weight delivery resolves
// EFA when awsEFAEnabled is true over the RDMA default.
func TestModelDeploymentDirectInterfaceProtocol_SGLangAndRemoteEFA(t *testing.T) {
	pair := func(engine string) *workercore.ModelDeployment {
		return newRenderDeployment(func(md *workercore.ModelDeployment) {
			md.Spec.Engine.Name = engine
			md.Spec.KVCache = nil
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
			md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
			md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{
				Interface: ptr.To(resource.MustParse("1")),
			}
			md.Spec.Roles = append(md.Spec.Roles, workercore.ModelDeploymentRole{
				Name: "decode", Kind: workercore.ModelDeploymentRoleKindDecode,
				Resources: &workercore.ModelDeploymentRoleResources{
					Interface: ptr.To(resource.MustParse("1")),
				},
			})
		})
	}

	t.Run("an SGLang pair resolves rdma when both halves request interfaces", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineSGLang)
		assert.Equal(t, "rdma", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, false, false,
		))
	})

	t.Run("an SGLang pair resolves efa when awsEFAEnabled is true", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineSGLang)
		assert.Equal(t, "efa", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, false, true,
		))
	})

	t.Run("an SGLang multi-member Server role resolves nothing", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineSGLang)
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindServer
		md.Spec.Roles[0].ReplicaSize = 2
		md.Spec.Roles = md.Spec.Roles[:1]
		assert.Equal(t, "", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, false, false,
		),
			"the collective shape is vLLM's alone; SGLang Server roles are not a granted leg")
	})

	t.Run("remote weight delivery prefers efa when awsEFAEnabled is true", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineVLLM)
		assert.Equal(t, "efa", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, true, true,
		),
			"the weight source resolves to EFA when awsEFAEnabled is true")
	})

	t.Run("remote weight delivery keeps rdma without awsEFAEnabled", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineVLLM)
		assert.Equal(t, "rdma", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, true, false,
		))
	})

	t.Run("remote weight delivery on SGLang resolves efa when awsEFAEnabled is true", func(t *testing.T) {
		md := pair(workercore.ModelDeploymentEngineSGLang)
		assert.Equal(t, "efa", ModelDeploymentDirectInterfaceProtocol(
			md, &md.Spec.Roles[0], nodefeature.ManufacturerNVIDIA, true, true,
		))
	})
}
