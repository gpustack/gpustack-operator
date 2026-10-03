package worker

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

type retirementGPUProtocolFixture struct {
	client       ctrlcli.Client
	kept, target *core.Pod
	workload     *kueue.Workload
}

func newRetirementGPUProtocolFixture(t *testing.T) *retirementGPUProtocolFixture {
	t.Helper()
	md, router := retirementRouterFixture(retirementDeployment())
	md.Generation = 1
	quantity := resource.MustParse("1")
	md.Spec.Roles[0].Resources = &workercore.ModelDeploymentRoleResources{Accelerator: &quantity}
	kept := realSurplusReplicaAt(t, md, "qwen-server-zero", 0, "")
	target := realSurplusReplicaAt(t, md, "qwen-server-one", 1, "")
	md.Spec.Roles[0].Replicas = 1
	require.Equal(t, realRenderHashOf(t, md, 0), kept.Annotations[modelDeploymentPodSpecHashAnnotation])
	objects := make([]ctrlcli.Object, 0, 9)
	objects = append(objects, md, newRenderInstanceType(), router)
	for i, pod := range []*core.Pod{kept, target} {
		pod.UID = types.UID([]string{"member-0", "member-1"}[i])
		pod.Spec.NodeName = []string{"node-keeper", "node-target"}[i]
		pod.Status.Phase = core.PodRunning
		pod.Status.PodIP = []string{"10.0.4.2", "10.0.4.1"}[i]
		devs := releaseDevices()
		devs.Name = pod.Spec.NodeName
		devs.UID = types.UID("devices-" + pod.Spec.NodeName)
		devs.Spec.Groups[0].Accelerators[0].ID = "GPU-" + pod.Spec.NodeName
		allocation := releaseCardRecord()["vllm"]
		allocation.Devices.Groups[0].Accelerators[0].ID = devs.Spec.Groups[0].Accelerators[0].ID
		claim, err := json.Marshal(map[string]deviceplugin.ContainerAllocation{modelDeploymentMainContainerName: allocation})
		require.NoError(t, err)
		pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = string(claim)
		require.True(t, podRequestsAccelerator(pod), "the actual renderer must request a GPU")
		status, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), devs, &core.PodList{Items: []core.Pod{*pod}})
		require.NoError(t, err, "the rendered container claim must be valid")
		devs.Status = status
		wl := releaseWorkload("workload-"+string(pod.UID), string(pod.UID), pod.Name)
		wl.Name = "workload-" + pod.Name
		objects = append(objects, pod, devs, wl)
	}
	cli := newModelDeploymentClient(objects...)
	wl := new(kueue.Workload)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Namespace: target.Namespace, Name: "workload-" + target.Name}, wl))
	return &retirementGPUProtocolFixture{client: cli, kept: kept, target: target, workload: wl}
}

func (f *retirementGPUProtocolFixture) reconciler() *ModelDeploymentReconciler {
	return retirementRouterReconciler(f.client, &scriptedDrainReader{
		answers: map[types.UID][]modelDeploymentDrainAnswer{f.target.UID: {idleDrain(), idleDrain()}},
	}, time.Now(), []core.Pod{*f.kept, *f.target})
}

func (f *retirementGPUProtocolFixture) advanceToDraining(t *testing.T) {
	t.Helper()
	r := f.reconciler()
	for _, state := range []workercore.ModelDeploymentRetirementState{
		workercore.ModelDeploymentRetirementStateAdmitted,
		workercore.ModelDeploymentRetirementStateDisqualified,
		workercore.ModelDeploymentRetirementStateWithdrawing,
		workercore.ModelDeploymentRetirementStateDraining,
	} {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err)
		md := getModelDeployment(t, f.client)
		require.NotNil(t, md.Status.Retirement)
		require.Equal(t, state, md.Status.Retirement.State, md.Status.Retirement.Reason)
		require.Equal(t, []string{"0", "1"}, replicaOrdinals(t, f.client))
		require.Equal(t, []string{string(f.target.UID)}, md.Status.Retirement.TargetMemberUIDs)
	}
}

func TestRetirementGPUProtocolPersistsEveryPhaseAndWaitsForLedger(t *testing.T) {
	f := newRetirementGPUProtocolFixture(t)
	require.Nil(t, getModelDeployment(t, f.client).Status.Retirement)
	f.advanceToDraining(t)
	r := f.reconciler()
	for _, state := range []workercore.ModelDeploymentRetirementState{
		workercore.ModelDeploymentRetirementStateDeleting,
		workercore.ModelDeploymentRetirementStateSettling,
		workercore.ModelDeploymentRetirementStateSettling,
	} {
		_, err := reconcileModelDeploymentWith(t, r)
		require.NoError(t, err)
		md := getModelDeployment(t, f.client)
		require.NotNil(t, md.Status.Retirement)
		require.Equal(t, state, md.Status.Retirement.State, md.Status.Retirement.Reason)
		require.Equal(t, []string{"0"}, replicaOrdinals(t, f.client))
		require.True(t, apierrors.IsNotFound(f.client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod))))
		require.True(t, apierrors.IsNotFound(f.client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload))))
		record := new(modelDeploymentRetirementRelease)
		require.NoError(t, json.Unmarshal([]byte(md.Annotations[modelDeploymentRetirementReleaseAnnotation]), record))
		require.Len(t, record.Members, 1)
		require.Len(t, record.Nodes, 1)
		require.Equal(t, f.target.UID, record.Members[0].PodUID)
		require.Equal(t, modelDeploymentRetirementClaimCard, record.Members[0].AcceleratorClaim)
		require.NotEmpty(t, record.Members[0].Cards)
		require.NotEmpty(t, record.Nodes[0].Inventory)
		require.Equal(t, string(f.workload.UID), record.TargetWorkloadUID)
	}
	devs := new(workercore.Devices)
	require.NoError(t, f.client.Get(context.Background(), ctrlcli.ObjectKey{Name: f.target.Spec.NodeName}, devs))
	status, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), devs, &core.PodList{})
	require.NoError(t, err)
	devs.Status = status
	require.NoError(t, f.client.Status().Update(context.Background(), devs))
	_, err = reconcileModelDeploymentWith(t, r)
	require.NoError(t, err)
	require.Nil(t, getModelDeployment(t, f.client).Status.Retirement, "strict ledger convergence must clear the durable operation")
	require.Equal(t, []string{"0"}, replicaOrdinals(t, f.client))
	kept := new(core.Pod)
	require.NoError(t, f.client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.kept), kept))
	require.Equal(t, f.kept.UID, kept.UID)
}

func TestRetirementAnnotationConflictCannotPersistEvidenceOrDelete(t *testing.T) {
	for _, tc := range []struct {
		name     string
		conflict bool
	}{
		{name: "stored resource version moves before patch", conflict: true},
		{name: "matching resource version"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetirementGPUProtocolFixture(t)
			f.advanceToDraining(t)
			base := f.client
			before := getModelDeployment(t, base)
			require.Empty(t, before.Annotations[modelDeploymentRetirementReleaseAnnotation])
			reached, refused := false, false
			f.client = ctrlinterceptor.NewClient(base.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
				Patch: func(ctx context.Context, client ctrlcli.WithWatch, obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.PatchOption) error {
					md, ok := obj.(*workercore.ModelDeployment)
					if !ok || md.Annotations[modelDeploymentRetirementReleaseAnnotation] == "" {
						return client.Patch(ctx, obj, patch, opts...)
					}
					reached = true
					wire, err := patch.Data(obj)
					require.NoError(t, err)
					var emitted struct {
						Metadata struct {
							ResourceVersion string `json:"resourceVersion"`
						} `json:"metadata"`
					}
					require.NoError(t, json.Unmarshal(wire, &emitted))
					require.Equal(t, before.ResourceVersion, emitted.Metadata.ResourceVersion)
					if tc.conflict {
						moved := getModelDeployment(t, base)
						if moved.Annotations == nil {
							moved.Annotations = map[string]string{}
						}
						moved.Annotations["unrelated"] = "preserved concurrent change"
						require.NoError(t, client.Update(ctx, moved))
						require.NotEqual(t, before.ResourceVersion, moved.ResourceVersion)
					}
					err = client.Patch(ctx, obj, patch, opts...)
					refused = apierrors.IsConflict(err)
					return err
				},
			})
			_, err := reconcileModelDeploymentWith(t, f.reconciler())
			require.NoError(t, err, "a refused evidence write holds the operation")
			require.True(t, reached, "the actual annotation CAS must be reached")
			require.Equal(t, tc.conflict, refused)
			after := getModelDeployment(t, base)
			if tc.conflict {
				require.Equal(t, before.Annotations[modelDeploymentRetirementReleaseAnnotation], after.Annotations[modelDeploymentRetirementReleaseAnnotation])
				require.Equal(t, "preserved concurrent change", after.Annotations["unrelated"])
				require.Equal(t, workercore.ModelDeploymentRetirementStateDraining, after.Status.Retirement.State)
				require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod)))
				require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload)))
			} else {
				require.NotEmpty(t, after.Annotations[modelDeploymentRetirementReleaseAnnotation])
				require.Equal(t, workercore.ModelDeploymentRetirementStateDeleting, after.Status.Retirement.State)
				require.True(t, apierrors.IsNotFound(base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod))))
				require.True(t, apierrors.IsNotFound(base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload))))
			}
		})
	}
}

func TestRetirementEvidenceSurvivesFailedDeletingStatusWrite(t *testing.T) {
	f := newRetirementGPUProtocolFixture(t)
	f.advanceToDraining(t)
	base := f.client
	reached := false
	failure := errors.New("deleting status write interrupted")
	// The retirement writer patches rather than updates, so the interceptor watches the patch. The
	// subject is the same write: the one that would move the operation to Deleting.
	f.client = ctrlinterceptor.NewClient(base.(ctrlcli.WithWatch), ctrlinterceptor.Funcs{
		SubResourcePatch: func(ctx context.Context, client ctrlcli.Client, name string, obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption) error {
			if _, ok := obj.(*workercore.ModelDeployment); !ok || name != "status" {
				return client.SubResource(name).Patch(ctx, obj, patch, opts...)
			}
			data, err := patch.Data(obj)
			require.NoError(t, err)
			if !bytes.Contains(data, []byte(`"state":"Deleting"`)) {
				return client.SubResource(name).Patch(ctx, obj, patch, opts...)
			}
			reached = true
			stored := getModelDeployment(t, base)
			require.Equal(t, workercore.ModelDeploymentRetirementStateDraining, stored.Status.Retirement.State)
			require.NotEmpty(t, stored.Annotations[modelDeploymentRetirementReleaseAnnotation], "annotation CAS must precede the actual failed state write")
			return failure
		},
		SubResourceUpdate: func(ctx context.Context, client ctrlcli.Client, name string, obj ctrlcli.Object, opts ...ctrlcli.SubResourceUpdateOption) error {
			md, ok := obj.(*workercore.ModelDeployment)
			if ok && name == "status" && md.Status.Retirement != nil && md.Status.Retirement.State == workercore.ModelDeploymentRetirementStateDeleting {
				reached = true
				stored := getModelDeployment(t, base)
				require.Equal(t, workercore.ModelDeploymentRetirementStateDraining, stored.Status.Retirement.State)
				require.NotEmpty(t, stored.Annotations[modelDeploymentRetirementReleaseAnnotation], "annotation CAS must precede the actual failed state write")
				return failure
			}
			return client.SubResource(name).Update(ctx, obj, opts...)
		},
	})
	_, err := reconcileModelDeploymentWith(t, f.reconciler())
	require.ErrorIs(t, err, failure)
	require.True(t, reached)
	require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod)))
	require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload)))
	f.client = base
	_, err = reconcileModelDeploymentWith(t, f.reconciler())
	require.NoError(t, err)
	require.Equal(t, workercore.ModelDeploymentRetirementStateDeleting, getModelDeployment(t, base).Status.Retirement.State)
	require.True(t, apierrors.IsNotFound(base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod))))
	require.True(t, apierrors.IsNotFound(base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload))))
}

// TestDrainingHoldsOnAnAnswerItCannotCountAndProgressesOnAnHonestOne is the lifecycle subject: a
// read the protocol cannot trust must leave the replica and its captured objects exactly where they
// were, and the same operation must then move on once a genuine finite zero arrives.
//
// The reader is scripted rather than stubbed, so the answers are the seam's own type carrying the
// evidence the protocol is supposed to judge. Each refused answer is a complete, self-consistent
// Idle claim resting on a value that is not a count, which is the shape a miscounting or
// adversarial reader produces. Nothing is deleted while one of them is in force.
func TestDrainingHoldsOnAnAnswerItCannotCountAndProgressesOnAnHonestOne(t *testing.T) {
	// EVERY EXPECTED GAUGE IS PRESENT IN BOTH ANSWERS. A claim carrying only the offending gauge
	// would be refused earlier and for a different reason, by the completeness check, and the case
	// would pass without the rule under test ever looking at the value. The only difference between
	// the two shapes is the number.
	refused := func(name string, overrides map[string]float64) modelDeploymentDrainAnswer {
		t.Helper()
		values := map[string]float64{}
		for _, metric := range modelDeploymentInFlightMetrics[workercore.ModelDeploymentEngineVLLM] {
			values[metric] = 0
		}
		for metric, value := range overrides {
			values[metric] = value
		}

		return modelDeploymentDrainAnswer{
			State: modelDeploymentDrainIdle, Complete: true, Series: values,
			Reason: name,
		}
	}
	complete := func(overrides map[string]float64) modelDeploymentDrainAnswer {
		values := map[string]float64{}
		for _, metric := range modelDeploymentInFlightMetrics[workercore.ModelDeploymentEngineVLLM] {
			values[metric] = 0
		}
		for metric, value := range overrides {
			values[metric] = value
		}

		return modelDeploymentDrainAnswer{
			State: modelDeploymentDrainIdle, Complete: true, Series: values,
		}
	}

	for _, tc := range []struct {
		name string
		// mention is the refusal the operation's own reason must carry. Asserting the phase alone
		// would not discriminate: this replica is also held at Draining for other reasons, so a
		// case that only checked the phase would pass whether or not the rule judged the evidence.
		mention string
		first   modelDeploymentDrainAnswer
	}{
		{
			name:    "a NaN inside a complete idle claim",
			mention: "unusable activity",
			first:   refused("a NaN", map[string]float64{"vllm:num_requests_running": math.NaN()}),
		},
		{
			name:    "an infinity inside a complete idle claim",
			mention: "unusable activity",
			first:   refused("an infinity", map[string]float64{"vllm:num_requests_waiting": math.Inf(1)}),
		},
		{
			name:    "a negative count inside a complete idle claim",
			mention: "unusable activity",
			first:   refused("a negative", map[string]float64{"vllm:num_requests_running": -1.0}),
		},
		{
			name:    "a positive count inside a complete idle claim",
			mention: "still holds activity",
			first:   refused("a positive", map[string]float64{"vllm:num_requests_running": 2.0}),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newRetirementGPUProtocolFixture(t)
			f.advanceToDraining(t)
			base := f.client
			reader := &scriptedDrainReader{
				answers: map[types.UID][]modelDeploymentDrainAnswer{
					// The protocol reads twice before it will believe a member, so the refusal is
					// delivered on both reads. A protocol that believed the first would pass the
					// first read and this case would not be the one it claims to be.
					f.target.UID: {tc.first, tc.first},
				},
			}
			f.client = base
			r := retirementRouterReconciler(base, reader, time.Now(), []core.Pod{*f.kept, *f.target})
			r.Client, r.APIReader = base, base

			_, err := reconcileModelDeploymentWith(t, r)
			require.NoError(t, err, "an untrustworthy read holds the operation rather than failing the pass")

			md := getModelDeployment(t, base)
			require.NotNil(t, md.Status.Retirement)
			assert.Equal(t, workercore.ModelDeploymentRetirementStateDraining, md.Status.Retirement.State,
				"the operation does not advance on an answer it cannot count")
			assert.Contains(t, md.Status.Retirement.Reason, tc.mention,
				"and the reason says the evidence was refused, which is what this case is about")
			require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.target), new(core.Pod)),
				"the target survives an answer that cannot be counted")
			require.NoError(t, base.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.workload), new(kueue.Workload)),
				"and so does the Workload it belongs to")
			assert.Equal(t, []string{"0", "1"}, replicaOrdinals(t, base),
				"no replica is removed on an untrustworthy read")

			// THE SAME OPERATION PROGRESSES once the answers are honest. This is what makes the hold
			// above a hold rather than a permanent refusal: the read is what was wrong, not the
			// member and not the operation.
			honest := retirementRouterReconciler(base,
				&scriptedDrainReader{answers: map[types.UID][]modelDeploymentDrainAnswer{
					f.target.UID: {complete(nil), complete(nil)},
				}}, time.Now(), []core.Pod{*f.kept, *f.target})
			honest.Client, honest.APIReader = base, base
			_, err = reconcileModelDeploymentWith(t, honest)
			require.NoError(t, err)

			assert.Equal(t, workercore.ModelDeploymentRetirementStateDeleting,
				getModelDeployment(t, base).Status.Retirement.State,
				"an honest finite zero carries the operation forward")
		})
	}
}
