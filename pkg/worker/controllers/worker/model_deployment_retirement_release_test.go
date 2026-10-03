package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The cases here are the release predicate's own. They drive the state machine rather than the
// predicates directly, because the defect they cover was never a wrong predicate: it was that
// completion did not consult one. A case that called the predicate itself would pass whether or not
// the state machine ever asked.

// releaseNode is the one accelerator a release case reasons about, and the Devices object that
// publishes it.
const releaseNode = "node-a"

func releaseDevices(mutate ...func(*workercore.Devices)) *workercore.Devices {
	devs := &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: releaseNode, Namespace: "", UID: "devices-uid"},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID: "gpu-0", Manufacturer: "NVIDIA", Name: "H20",
				Accelerators: []workercore.Accelerator{{ID: "GPU-abc", Index: 0}},
			}},
		},
	}
	for _, m := range mutate {
		m(devs)
	}

	return devs
}

// releaseCardRecord is the annotation a real holder of one card carries, in the shape the allocator
// writes: a non-Option container record naming the card and its units.
func releaseCardRecord() map[string]deviceplugin.ContainerAllocation {
	return map[string]deviceplugin.ContainerAllocation{
		"vllm": {Devices: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID: "gpu-0", Manufacturer: "NVIDIA",
				Accelerators: []workercore.AcceleratorAllocation{{
					ID: "GPU-abc", Index: 0,
					Mode:      workercore.DeviceAllocationModeExclusive,
					Allocated: 1_600_000, Remaining: 0,
				}},
			}},
		}},
	}
}

func releaseHolder(t *testing.T, mutate ...func(*core.Pod)) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "qwen-server-one", Namespace: "team-a", UID: "member-1"},
		Spec: core.PodSpec{
			NodeName: releaseNode,
			Containers: []core.Container{{
				Name:  "vllm",
				Image: "vllm/vllm-openai:v0.25.1",
				// The holder asks for the card it is recorded as holding. A recorded claim with no
				// matching request would be a record this allocator never wrote.
				Resources: core.ResourceRequirements{
					Requests: core.ResourceList{
						nodefeature.GetAcceleratableResourceName(
							nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive): resource.MustParse("1"),
					},
				},
			}},
		},
		Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.0.8.1"},
	}
	for _, m := range mutate {
		m(pod)
	}
	raw, err := json.Marshal(releaseCardRecord())
	require.NoError(t, err, "the record a holder carries is marshalable")
	pod.Annotations = map[string]string{deviceplugin.AllocatedAcceleratorAnnoKey: string(raw)}

	return pod
}

func releaseWorkload(uid, ownerUID, ownerName string) *kueue.Workload {
	workload := &kueue.Workload{}
	workload.Name, workload.Namespace = "wl-qwen", "team-a"
	workload.UID = types.UID(uid)
	workload.OwnerReferences = []meta.OwnerReference{{
		APIVersion: "v1", Kind: "Pod", Name: ownerName, UID: types.UID(ownerUID),
	}}

	return workload
}

// releaseCase is one server state and the answer it must produce.
type releaseCase struct {
	name string
	// holder is shaped before the operation captures anything, for the states a fake client
	// cannot produce afterwards.
	holder []func(*core.Pod)
	// arrange is the server after the target's delete has landed.
	arrange func(*releaseScenario)
	// wantReleased is the answer the operation must reach.
	wantReleased bool
	wantReason   []string
}

type releaseScenario struct {
	t           *testing.T
	md          *workercore.ModelDeployment
	router      *core.Pod
	r           *ModelDeploymentReconciler
	cli         ctrlcli.Client
	devices     *workercore.Devices
	holder      *core.Pod
	workloadUID string
	started     time.Time
}

// newReleaseScenario drives a Router-backed operation to the point where its release must be
// observed: the target deleted, its evidence captured while the target still existed, and the
// ledger and Workload left in whatever state the case is about.
func newReleaseScenario(t *testing.T, holder ...func(*core.Pod)) *releaseScenario {
	t.Helper()

	md, router := retirementRouterFixture(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	held := releaseHolder(t, holder...)
	devices := releaseDevices()
	workload := releaseWorkload("wl-uid-1", "member-1", "qwen-server-one")

	cli := newModelDeploymentClient(md, newRenderInstanceType(), held, router, devices, workload)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*held})
	started := time.Now()
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			res.StartedAt = meta.NewTime(started)
			res.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(started)
			res.TargetWorkloadUID = "wl-uid-1"
		}))

	// The reservation is PERSISTED before anything is captured. The capture is read back against a
	// fresh server read of the operation, so an operation that exists only in this copy is not an
	// operation the observation can see.
	require.NoError(t, cli.Status().Update(context.Background(), md))
	key := ctrlcli.ObjectKeyFromObject(md)
	md = new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), key, md))

	scenario := &releaseScenario{
		t: t, md: md, r: r, router: router, cli: cli, devices: devices, holder: held,
		workloadUID: "wl-uid-1", started: started,
	}
	// The capture happens while the target is still there. That ordering is the contract, and a
	// scenario that captured afterwards would be testing a member the record can no longer describe.
	plan := &modelDeploymentRetirementPlan{
		Reservation: md.Status.Retirement, held: setsOf("member-1"),
	}
	plan.Hold = true
	record, err := r.captureModelDeploymentRetirementRelease(context.Background(), md, plan)
	require.NoError(t, err, "a real target on a real node with a real ledger can be captured")
	require.NoError(t, r.persistModelDeploymentRetirementRelease(context.Background(), md, record))
	require.Len(t, record.Nodes, 1, "the captured target's node and ledger are part of the record")
	require.NotEmpty(t, record.Nodes[0].Cards, "and the cards it held")

	return scenario
}

// replaceDevices puts a different Devices object under the same name on the server. A fake client
// cannot change an object's UID in place, so the server is rebuilt around the replacement: the
// observation has to notice a new identity, not a later state of the ledger it captured.
func (s *releaseScenario) replaceDevices(devs *workercore.Devices) {
	s.t.Helper()

	cli := newModelDeploymentClient(s.md, newRenderInstanceType(), s.holder, s.router, devs,
		releaseWorkload(s.workloadUID, "member-1", "qwen-server-one"))
	s.cli = cli
	s.r = retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*s.holder})
	s.devices = devs
}

func setsOf(uids ...string) sets.Set[types.UID] {
	out := sets.New[types.UID]()
	for _, uid := range uids {
		out.Insert(types.UID(uid))
	}

	return out
}

// settle advances the operation to Settling and reports what the completion step concluded.
// persistPhase writes the operation's current phase to the server. Authority is read from the
// server, so a fixture that advances a phase in memory and then reads a release has to persist the
// phase it is actually operating under rather than expect the observation to accept a stale one.
func (s *releaseScenario) persistPhase() {
	s.t.Helper()
	require.NoError(s.t, s.cli.Status().Update(context.Background(), s.md))
	key := ctrlcli.ObjectKeyFromObject(s.md)
	s.md = new(workercore.ModelDeployment)
	require.NoError(s.t, s.cli.Get(context.Background(), key, s.md))
}

func (s *releaseScenario) settle() (*modelDeploymentRetirementPlan, bool) {
	s.t.Helper()

	// A phase change is not a new operation, so the identity the record was bound to is carried
	// over. Restamping StartedAt here would be a different operation, and the record would be
	// refused for exactly the reason it should be. The retry token moves with it for the same
	// reason: a phase change is not a new attempt either.
	retry := s.md.Status.Retirement.LastConsumedRetryToken
	s.md = reserve(s.md, retirementReservation(
		workercore.ModelDeploymentRetirementStateSettling, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			res.StartedAt = meta.NewTime(s.started)
			res.Deadline = meta.NewTime(s.started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(time.Now())
			res.LastConsumedRetryToken = retry
			res.TargetWorkloadUID = s.workloadUID
		}))
	require.NoError(s.t, s.cli.Status().Update(context.Background(), s.md))
	key := ctrlcli.ObjectKeyFromObject(s.md)
	s.md = new(workercore.ModelDeployment)
	require.NoError(s.t, s.cli.Get(context.Background(), key, s.md))
	// The record written at capture time is the one the observation reads, and it is deliberately
	// not captured again here: a member that has been deleted cannot be described any more, and
	// re-capturing would let a later phase quietly rebuild the evidence an earlier one recorded.
	plan := &modelDeploymentRetirementPlan{
		Reservation: s.md.Status.Retirement, held: setsOf("member-1"),
	}
	plan.Hold = true
	s.r.advanceModelDeploymentRetirementToCompleted(context.Background(), s.md, plan)

	return plan, plan.Reservation.State == workercore.ModelDeploymentRetirementStateCompleted
}

func TestModelDeploymentRetirementRelease_Settling(t *testing.T) {
	testCases := []releaseCase{
		{
			name: "an observed release completes",
			arrange: func(s *releaseScenario) {
				// Target gone, Workload gone, the card free, nothing else on the node.
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				s.settleLedger()
			},
			wantReleased: true,
		},
		{
			name: "a captured Workload still present holds the operation",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
			},
			wantReason: []string{"workload"},
		},
		{
			name: "a Workload that still owns the target's member holds the operation",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				// A NEW Workload, with its own identity, claims the target's member.
				require.NoError(s.t, s.cli.Create(context.Background(),
					releaseWorkload("wl-uid-2", "member-1", "qwen-server-one")))
			},
			wantReason: []string{"owns the target's member"},
		},
		{
			// The held set skips a deleting Pod, so only a fresh read catches it. The timestamp is
			// immutable, so the Pod is born deleting rather than made deleting afterwards.
			name: "a target Pod still deleting holds the operation",
			holder: []func(*core.Pod){func(pod *core.Pod) {
				now := meta.Now()
				pod.DeletionTimestamp, pod.Finalizers = &now, []string{"gpustack.ai/test"}
			}},
			wantReason: []string{"still present"},
		},
		{
			name: "a replaced Devices object holds the operation",
			arrange: func(s *releaseScenario) {
				// Same name, different identity: a different ledger, not a later state of this one.
				// The swap comes first, because the record was captured against the original.
				replaced := s.devices.DeepCopy()
				replaced.UID = "devices-uid-2"
				s.replaceDevices(replaced)
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				s.settleLedger()
			},
			wantReason: []string{"no longer the one that was captured"},
		},
		{
			name: "a stale published ledger holds the operation",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				// The node's records are rebuilt from what is on it, so a stale PUBLISHED row is
				// what disagrees, and it is the disagreement that is the point.
				stale := s.rebuiltLedger()
				stale.Status.Groups[0].Accelerators[0].Mode = workercore.DeviceAllocationModeShared
				stale.Status.Groups[0].Accelerators[0].Allocated = 160_000
				s.publishLedger(stale)
			},
			wantReason: []string{"has not settled"},
		},
		{
			name: "a card legitimately reused by another live pod still completes",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				// A DIFFERENT pod now holds the same card. The target's claim is gone, which is
				// the question, and the card being busy is not a reason to refuse it.
				next := releaseHolder(s.t, func(pod *core.Pod) {
					pod.Name, pod.UID, pod.Namespace = "qwen-server-two", "member-2", "team-a"
				})
				require.NoError(s.t, s.cli.Create(context.Background(), next))
				s.settleLedger()
			},
			wantReleased: true,
		},
		{
			name: "a holder in another namespace is counted and can hold the operation",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				other := releaseHolder(s.t, func(pod *core.Pod) {
					pod.Name, pod.UID, pod.Namespace = "qwen-server-three", "member-3", "team-b"
				})
				require.NoError(s.t, s.cli.Create(context.Background(), other))
				// The published ledger says the card is free, and the node's records say it is not.
				s.publishLedger(freeLedger(s.rebuiltLedger()))
			},
			wantReason: []string{"has not settled"},
		},
		{
			name: "an unreadable allocation record on the node holds the operation",
			arrange: func(s *releaseScenario) {
				require.NoError(s.t, s.cli.Delete(context.Background(), s.holder))
				require.NoError(s.t, s.cli.Delete(context.Background(),
					releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
				broken := releaseHolder(s.t, func(pod *core.Pod) {
					pod.Name, pod.UID = "qwen-server-two", "member-2"
				})
				broken.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = "{not json"
				require.NoError(s.t, s.cli.Create(context.Background(), broken))
				// The ledger is left as it was, because the node's records cannot be read at all.
			},
			wantReason: []string{"cannot be read strictly"},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scenario := newReleaseScenario(t, tc.holder...)
			if tc.arrange != nil {
				tc.arrange(scenario)
			}

			plan, completed := scenario.settle()

			if tc.wantReleased {
				assert.True(t, completed, "the release was observed, so the operation completes")
				assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted,
					plan.Reservation.State)

				return
			}
			assert.False(t, completed, "the release was not observed, so the operation holds")
			assert.NotEqual(t, workercore.ModelDeploymentRetirementStateCompleted, plan.Reservation.State)
			for _, want := range tc.wantReason {
				assert.Contains(t, plan.Reservation.Reason, want)
			}
		})
	}
}

// settleLedger republishes the ledger the node's own records imply, so a case is about the
// disagreement it introduces rather than about an unrelated stale row.
func (s *releaseScenario) settleLedger() {
	s.t.Helper()
	s.publishLedger(s.rebuiltLedger())
}

// rebuiltLedger is the accounting the node's live records imply, without publishing it.
func (s *releaseScenario) rebuiltLedger() *workercore.Devices {
	s.t.Helper()

	live := new(core.PodList)
	require.NoError(s.t, s.cli.List(context.Background(), live, ctrlcli.MatchingFields{
		"spec.nodeName": releaseNode,
	}))
	rebuilt, _, err := deviceplugin.BuildDesiredStatusStrict(
		ctrllogDiscard(), s.devices, live)
	require.NoError(s.t, err, "the node's records are readable, or the case is not about the ledger")

	devs := new(workercore.Devices)
	require.NoError(s.t, s.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Name: s.devices.Name, Namespace: s.devices.Namespace}, devs),
		"the node's ledger is still on the server")
	devs.Status = rebuilt

	return devs
}

// publishLedger puts an accounting on the server as the device manager published it, which is what
// a later observation compares the node's own records against.
func (s *releaseScenario) publishLedger(devs *workercore.Devices) {
	s.t.Helper()
	require.NoError(s.t, s.cli.Status().Update(context.Background(), devs))
}

// freeLedger is a published ledger with a whole card free, which is what a released card looks like.
func freeLedger(devs *workercore.Devices) *workercore.Devices {
	published := devs.DeepCopy()
	published.Status.Groups[0].Accelerators[0] = workercore.AcceleratorAllocation{
		ID: "GPU-abc", Index: 0, Mode: workercore.DeviceAllocationModeNone,
		Allocated: 0, Remaining: 1_600_000,
	}

	return published
}

// TestModelDeploymentRetirementRelease_LateReleaseCompletes is the deadline half: a budget bounds
// waiting, and is not a license to declare a result or to keep one that has already happened.
func TestModelDeploymentRetirementRelease_LateReleaseCompletes(t *testing.T) {
	t.Run("release observed after the settle budget still completes", func(t *testing.T) {
		scenario := newReleaseScenario(t)
		require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
		require.NoError(t, scenario.cli.Delete(context.Background(),
			releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))
		scenario.settleLedger()

		// A budget that has already run out, and a release that has already been observed. The
		// record is the one captured with the target: an expired clock is not a license to rebuild
		// the evidence, and the target it describes is gone.
		expired := time.Now().Add(-modelDeploymentRetirementSettleBudget - time.Minute)
		scenario.md = reserve(scenario.md, retirementReservation(
			workercore.ModelDeploymentRetirementStateSettling, "server", 1, []string{"member-1"},
			func(res *workercore.ModelDeploymentRetirementStatus) {
				res.StartedAt = meta.NewTime(scenario.started)
				res.PhaseStartedAt = meta.NewTime(expired)
				res.Deadline = meta.NewTime(expired.Add(modelDeploymentRetirementOverallBudget))
				res.TargetWorkloadUID = scenario.workloadUID
			}))
		scenario.persistPhase()

		run := &modelDeploymentRetirementPlan{
			Reservation: scenario.md.Status.Retirement, held: setsOf("member-1"),
		}
		run.Hold = true
		scenario.r.advanceModelDeploymentRetirementToCompleted(context.Background(), scenario.md, run)

		assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted, run.Reservation.State,
			"an expired budget does not turn an observed release into an unobserved one")
	})

	t.Run("an unobserved release past the deadline keeps the operation settling", func(t *testing.T) {
		scenario := newReleaseScenario(t)
		// The Workload is never removed, so nothing is ever observed, and the budget is spent.
		expired := time.Now().Add(-modelDeploymentRetirementSettleBudget - time.Minute)
		scenario.md = reserve(scenario.md, retirementReservation(
			workercore.ModelDeploymentRetirementStateSettling, "server", 1, []string{"member-1"},
			func(res *workercore.ModelDeploymentRetirementStatus) {
				res.StartedAt = meta.NewTime(scenario.started)
				res.PhaseStartedAt = meta.NewTime(expired)
				res.Deadline = meta.NewTime(expired.Add(modelDeploymentRetirementOverallBudget))
				res.TargetWorkloadUID = scenario.workloadUID
			}))

		run := &modelDeploymentRetirementPlan{
			Reservation: scenario.md.Status.Retirement, held: setsOf("member-1"),
		}
		run.Hold = true
		scenario.r.advanceModelDeploymentRetirementToCompleted(context.Background(), scenario.md, run)

		assert.Equal(t, workercore.ModelDeploymentRetirementStateSettling, run.Reservation.State,
			"an exhausted budget holds the operation rather than completing or aborting it")
		assert.Contains(t, run.Reservation.Reason, "budget was exhausted")
		assert.False(t, run.Clear, "and nothing is cleared")
	})
}

// TestModelDeploymentRetirementRelease_EvidenceBinding pins that a record is usable only by the
// operation it was captured for. StartedAt alone cannot do this, because the stored time
// serializes to whole seconds and two retries a second apart share it.
func TestModelDeploymentRetirementRelease_EvidenceBinding(t *testing.T) {
	testCases := []struct {
		name    string
		mutate  func(*modelDeploymentRetirementRelease)
		wantErr string
	}{
		{
			name:    "another deployment's record is not ours",
			mutate:  func(r *modelDeploymentRetirementRelease) { r.ModelDeploymentUID = "someone-else" },
			wantErr: "another deployment",
		},
		{
			name:    "another generation's record is not ours",
			mutate:  func(r *modelDeploymentRetirementRelease) { r.ObservedGeneration = 99 },
			wantErr: "captured at generation",
		},
		{
			name: "a different target set is not ours",
			mutate: func(r *modelDeploymentRetirementRelease) {
				r.TargetMemberUIDs = []string{"member-9"}
			},
			wantErr: "the reservation targets",
		},
		{
			name:    "another retry's record is not ours even a second later",
			mutate:  func(r *modelDeploymentRetirementRelease) { r.LastConsumedRetryToken = "token-b" },
			wantErr: "different retry",
		},
		{
			name:    "another workload's record is not ours",
			mutate:  func(r *modelDeploymentRetirementRelease) { r.TargetWorkloadUID = "wl-other" },
			wantErr: "froze workload",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			scenario := newReleaseScenario(t)
			record := &modelDeploymentRetirementRelease{
				ModelDeploymentUID: scenario.md.UID,
				ObservedGeneration: scenario.md.Status.Retirement.ObservedGeneration,
				RoleName:           "server",
				ReplicaOrdinal:     1,
				TargetMemberUIDs:   []string{"member-1"},
				TargetWorkloadUID:  "wl-uid-1",
				StartedAt: scenario.md.Status.Retirement.StartedAt.UTC().
					Format(time.RFC3339),
				LastConsumedRetryToken: "token-a",
			}
			tc.mutate(record)

			plan := &modelDeploymentRetirementPlan{
				Reservation: scenario.md.Status.Retirement, held: setsOf("member-1"),
			}
			plan.Hold = true
			err := bindModelDeploymentRetirementRelease(record, scenario.md, plan)

			require.Error(t, err, "a record belonging to another operation is not evidence")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestModelDeploymentRetirementRelease_CaptureRequiresMembers is the capture precondition. A
// record that cannot say what the target held cannot be used to say it came back, so a member
// that is already gone is a refusal and never an empty capture.
func TestModelDeploymentRetirementRelease_CaptureRequiresMembers(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))

	plan := &modelDeploymentRetirementPlan{
		Reservation: scenario.md.Status.Retirement, held: setsOf("member-1"),
	}
	plan.Hold = true

	_, err := scenario.r.captureModelDeploymentRetirementRelease(context.Background(), scenario.md, plan)

	require.Error(t, err, "a member that is gone cannot have its allocation captured")
	assert.Contains(t, err.Error(), "already absent")
}

// TestModelDeploymentRetirementRelease_ObservationNeedsTheServer is the read-path precondition: a
// nil reader or a canceled caller holds rather than reporting a release nobody observed.
func TestModelDeploymentRetirementRelease_ObservationNeedsTheServer(t *testing.T) {
	scenario := newReleaseScenario(t)
	plan := &modelDeploymentRetirementPlan{
		Reservation: scenario.md.Status.Retirement, held: setsOf("member-1"),
	}
	plan.Hold = true

	withoutReader := &ModelDeploymentReconciler{Client: scenario.cli}
	reason, released := withoutReader.observeModelDeploymentRetirementReleased(
		context.Background(), scenario.md, plan)
	assert.False(t, released)
	assert.Contains(t, reason, "no API reader")

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	reason, released = scenario.r.observeModelDeploymentRetirementReleased(cancelled, scenario.md, plan)
	assert.False(t, released, "a cancelled observation is not an observation")
	assert.Contains(t, reason, "cancelled")
}

// ctrllogDiscard is the logger the strict rebuild is handed in a test, matching how the package's
// other deviceplugin-facing tests call it.
func ctrllogDiscard() logr.Logger { return logr.Discard() }

// TestModelDeploymentRetirementRelease_RouterBackedTwoToOneLifecycle is the positive the rest of
// this file refuses to be: a whole retirement, driven through the state machine, that reaches
// Completed and clears. Every other case here proves the release is not claimed too early, and a
// suite of refusals cannot tell a working predicate from one that refuses everything.
func TestModelDeploymentRetirementRelease_RouterBackedTwoToOneLifecycle(t *testing.T) {
	md, router := retirementRouterFixture(retirementDeployment())
	// Two replicas are declared and only one is served, which is the surplus an operator retires.
	// The target is the surplus member; the keeper is the one the Router keeps answering through.
	target := releaseHolder(t, func(pod *core.Pod) {
		pod.Name, pod.UID = "qwen-server-one", "member-1"
	})
	keeper := releaseHolder(t, func(pod *core.Pod) {
		pod.Name, pod.UID, pod.Status.PodIP = "qwen-server-zero", "keeper", "10.0.7.2"
	})
	devices := releaseDevices()
	workload := releaseWorkload("wl-uid-1", "member-1", "qwen-server-one")

	cli := newModelDeploymentClient(md, newRenderInstanceType(), target, keeper, router, devices, workload)
	md.Spec.Roles[0].Replicas = 2
	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			started := time.Now()
			res.StartedAt = meta.NewTime(started)
			res.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(started)
			res.TargetWorkloadUID = "wl-uid-1"
		}))
	require.NoError(t, cli.Status().Update(context.Background(), md))
	key := ctrlcli.ObjectKeyFromObject(md)
	md = new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), key, md))

	// The target's queues are idle on two consecutive complete reads, so the drain completes.
	reader := &scriptedDrainReader{answers: map[types.UID][]modelDeploymentDrainAnswer{
		"member-1": {idleDrain(), idleDrain()},
	}}
	r := retirementRouterReconciler(cli, reader, time.Now(), []core.Pod{*target, *keeper}, "qwen-server-zero")

	// Draining reads the queues, and the delete is authorized only after the evidence is captured.
	// carries the UID so a same-name replacement could not be taken in the target's place.
	plan := r.planModelDeploymentRetirement(context.Background(), md, []core.Pod{*target, *keeper})
	r.advanceModelDeploymentRetirementToDeleting(context.Background(), md, plan)
	require.True(t, plan.DeletesAllowed,
		"the release was captured and written, so the delete is authorized, reason=%s", plan.Reservation.Reason)
	require.Equal(t, workercore.ModelDeploymentRetirementStateDeleting, plan.Reservation.State)
	require.NoError(t, r.commitModelDeploymentRetirement(context.Background(), md, plan,
		[]core.Pod{*target, *keeper}))

	// The delete has landed, so the operation moves to Settling on fresh actual absence.
	md.Status.Retirement = plan.Reservation
	r.advanceModelDeploymentRetirementPastDeletion(context.Background(), md, plan)
	require.Equal(t, workercore.ModelDeploymentRetirementStateSettling, plan.Reservation.State)
	// Settling is persisted before completion, so the completion is authorized under the phase the
	// server actually holds rather than one that only exists in this object.
	md.Status.Retirement = plan.Reservation
	require.NoError(t, cli.Status().Update(context.Background(), md))
	mdKey := ctrlcli.ObjectKeyFromObject(md)
	md = new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), mdKey, md))

	// The ledger has now settled, and the release is complete.
	settlePublishedLedger(t, cli, devices)
	plan.Hold = true
	r.advanceModelDeploymentRetirementPastDeletion(context.Background(), md, plan)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted, plan.Reservation.State,
		"an observed release completes the operation, reason=%s", plan.Reservation.Reason)
	assert.True(t, plan.Clear, "a completed operation is cleared")

	// The keeper was never part of this operation, and saying so is the point of the 2 to 1 shape.
	stillThere := new(core.Pod)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: keeper.Namespace, Name: keeper.Name}, stillThere))
	assert.Equal(t, keeper.UID, stillThere.UID, "the member the Router kept serving is not deleted")
}

// settlePublishedLedger republishes the accounting the node's surviving records imply, which is the
// ledger a real device manager writes once the target's pod is gone.
func settlePublishedLedger(t *testing.T, cli ctrlcli.Client, devs *workercore.Devices) {
	t.Helper()

	live := new(core.PodList)
	require.NoError(t, cli.List(context.Background(), live, ctrlcli.MatchingFields{
		"spec.nodeName": releaseNode,
	}))
	rebuilt, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllogDiscard(), devs, live)
	require.NoError(t, err)

	published := new(workercore.Devices)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: devs.Name}, published))
	published.Status = rebuilt
	require.NoError(t, cli.Status().Update(context.Background(), published))
}

// TestModelDeploymentRetirementRelease_EmptyWorkloadUIDHolds is the invalid-evidence case: a
// Workload that owns a target member but carries no identity cannot be shown to be gone, so the
// operation holds rather than skipping it.
func TestModelDeploymentRetirementRelease_EmptyWorkloadUIDHolds(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))
	// A REPLACEMENT with the same name and no identity: the captured one is gone, and this one
	// cannot be shown to be anything but an object that still owns the target's member.
	unidentified := releaseWorkload("", "member-1", "qwen-server-one")
	require.NoError(t, scenario.cli.Create(context.Background(), unidentified))
	scenario.settleLedger()

	plan, completed := scenario.settle()

	assert.False(t, completed, "an unidentifiable owner cannot be shown to have released the quota")
	assert.Contains(t, plan.Reservation.Reason, "carries no identity")
}

// TestModelDeploymentRetirementRelease_SameSecondDifferentRetryHolds pins the retry token's job.
// Two operations a second apart share a serialized start time, so the token is the only thing that
// tells their evidence apart, and an old record must not answer for the new one.
func TestModelDeploymentRetirementRelease_SameSecondDifferentRetryHolds(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))
	scenario.settleLedger()

	// The start time is held identical on purpose. Only the retry moves.
	scenario.md.Status.Retirement.LastConsumedRetryToken = "second-retry"
	require.NoError(t, scenario.cli.Status().Update(context.Background(), scenario.md))
	fresh := new(workercore.ModelDeployment)
	require.NoError(t, scenario.cli.Get(context.Background(),
		ctrlcli.ObjectKeyFromObject(scenario.md), fresh))
	scenario.md = fresh

	plan, completed := scenario.settle()

	assert.False(t, completed, "a record captured under an earlier retry does not answer for this one")
	assert.Contains(t, plan.Reservation.Reason, "different retry")
}

// TestModelDeploymentRetirementRelease_CPUOnlyTargetCompletes is the applicability positive. A
// member that asks for no accelerator is recorded as asking for none, with no cards and no ledger
// entry, and its operation still completes. It is here because the GPU rule above refuses a
// requester with no record, and a refusal is not a policy if nothing else can ever pass.
func TestModelDeploymentRetirementRelease_CPUOnlyTargetCompletes(t *testing.T) {
	md, router := retirementRouterFixture(retirementDeployment())
	md.Spec.Roles[0].Replicas = 1
	// A CPU pod: it requests ordinary resources, carries no allocation record, and holds no card.
	holder := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "qwen-server-one", Namespace: "team-a", UID: "member-1"},
		Spec: core.PodSpec{
			NodeName:   releaseNode,
			Containers: []core.Container{{Name: "vllm", Image: "vllm/vllm-openai:v0.25.1"}},
		},
		Status: core.PodStatus{Phase: core.PodRunning, PodIP: "10.0.8.1"},
	}
	workload := releaseWorkload("wl-uid-1", "member-1", "qwen-server-one")
	cli := newModelDeploymentClient(md, newRenderInstanceType(), holder, router, releaseDevices(), workload)
	r := retirementRouterReconciler(cli, &scriptedDrainReader{}, time.Now(), []core.Pod{*holder})

	md = reserve(md, retirementReservation(
		workercore.ModelDeploymentRetirementStateDraining, "server", 1, []string{"member-1"},
		func(res *workercore.ModelDeploymentRetirementStatus) {
			started := time.Now()
			res.StartedAt = meta.NewTime(started)
			res.Deadline = meta.NewTime(started.Add(modelDeploymentRetirementOverallBudget))
			res.PhaseStartedAt = meta.NewTime(started)
			res.TargetWorkloadUID = "wl-uid-1"
		}))
	require.NoError(t, cli.Status().Update(context.Background(), md))
	key := ctrlcli.ObjectKeyFromObject(md)
	md = new(workercore.ModelDeployment)
	require.NoError(t, cli.Get(context.Background(), key, md))

	plan := &modelDeploymentRetirementPlan{Reservation: md.Status.Retirement, held: setsOf("member-1")}
	plan.Hold = true
	record, err := r.captureModelDeploymentRetirementRelease(context.Background(), md, plan)
	require.NoError(t, err, "a member that asks for no accelerator is captured, not refused")
	require.Len(t, record.Members, 1)
	assert.Equal(t, modelDeploymentRetirementClaimNone, record.Members[0].AcceleratorClaim,
		"the record states that this member asked for none, rather than leaving it to be inferred")
	assert.Empty(t, record.Members[0].Cards)
	assert.Empty(t, record.Nodes, "a member holding no card has no ledger to compare")
	require.NoError(t, r.persistModelDeploymentRetirementRelease(context.Background(), md, record))

	require.NoError(t, cli.Delete(context.Background(), holder))
	require.NoError(t, cli.Delete(context.Background(), workload))
	run := &modelDeploymentRetirementPlan{Reservation: md.Status.Retirement, held: setsOf("member-1")}
	run.Hold = true
	r.advanceModelDeploymentRetirementToCompleted(context.Background(), md, run)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted, run.Reservation.State,
		"a CPU target that is gone has released everything it held, reason=%s", run.Reservation.Reason)
}

// TestModelDeploymentRetirementRelease_InventoryChangeHolds is the Devices bookend: a ledger that
// lost the card is a changed input, not a released card.
func TestModelDeploymentRetirementRelease_InventoryChangeHolds(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))
	scenario.settleLedger()

	// The card is still in the ledger the target held, but it is no longer in the node's inventory.
	current := new(workercore.Devices)
	require.NoError(t, scenario.cli.Get(context.Background(),
		ctrlcli.ObjectKey{Name: releaseNode}, current))
	current.Spec.Groups[0].Accelerators = []workercore.Accelerator{{ID: "GPU-other", Index: 0}}
	require.NoError(t, scenario.cli.Update(context.Background(), current))

	plan, completed := scenario.settle()

	assert.False(t, completed, "a card that left the inventory cannot be read as released")
	assert.Contains(t, plan.Reservation.Reason, "left the node's inventory")
}

// TestModelDeploymentRetirementRelease_StaleLedgerThenConvergence is the convergence pair: a
// ledger that has not caught up holds, and the same operation completes once it does. Without the
// second half, a predicate that refuses forever would pass the first.
func TestModelDeploymentRetirementRelease_StaleLedgerThenConvergence(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))

	// The published ledger has not caught up with the node's records yet.
	scenario.publishLedger(staleLedger(scenario.rebuiltLedger()))

	plan, completed := scenario.settle()
	assert.False(t, completed, "a ledger that has not settled does not release a card")
	assert.Contains(t, plan.Reservation.Reason, "has not settled")

	// The device manager republishes, and the same operation is now free to finish.
	scenario.settleLedger()
	after, completed := scenario.settle()
	assert.True(t, completed,
		"a converged ledger releases the card, reason=%s", after.Reservation.Reason)
	assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted, after.Reservation.State)
}

// staleLedger is a published accounting that still shows the card carved.
func staleLedger(devs *workercore.Devices) *workercore.Devices {
	stale := devs.DeepCopy()
	stale.Status.Groups[0].Accelerators[0].Mode = workercore.DeviceAllocationModeShared
	stale.Status.Groups[0].Accelerators[0].Allocated = 160_000

	return stale
}

// TestModelDeploymentRetirementRelease_DeletingTerminalHolderIsCounted is the accounting subject
// that matters for a holder in its grace period: it still holds its card, so a ledger published
// before the device manager noticed it disagrees with the node's own records and the operation
// holds. A strict rebuild that quietly dropped terminating or terminal pods would read the card as
// free and complete on that stale ledger, which is the mistake this pins.
func TestModelDeploymentRetirementRelease_DeletingTerminalHolderIsCounted(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))

	// A DIFFERENT pod that is deleting and terminal, still carved for the same card. The claim is
	// partitioned, which is the claim the production rule keeps after a pod terminates: an exclusive
	// one is released on termination by design, so only a partition makes the counting visible.
	now := meta.Now()
	terminal := releaseHolder(t, func(pod *core.Pod) {
		pod.Name, pod.UID = "qwen-server-two", "member-2"
		pod.DeletionTimestamp, pod.Finalizers = &now, []string{"gpustack.ai/test"}
		pod.Status.Phase = core.PodSucceeded
	})
	terminal.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = partitionedCardRecord(t)
	require.NoError(t, scenario.cli.Create(context.Background(), terminal))

	// The ledger the device manager published BEFORE it saw that pod: the card reads free.
	scenario.publishLedger(freeLedger(scenario.rebuiltLedger()))

	plan, completed := scenario.settle()
	assert.False(t, completed, "a terminal holder still carved for the card is not a free card")
	assert.Contains(t, plan.Reservation.Reason, "has not settled")

	// Once the published ledger accounts for it, the same operation finishes.
	scenario.settleLedger()
	after, completed := scenario.settle()
	assert.True(t, completed, "the converged ledger completes, reason=%s", after.Reservation.Reason)
}

// partitionedCardRecord is a claim the allocator writes for a whole-card partition, which is the
// shape the production retention rule keeps after its pod reaches a terminal phase.
func partitionedCardRecord(t *testing.T) string {
	t.Helper()

	raw, err := json.Marshal(map[string]deviceplugin.ContainerAllocation{
		"vllm": {Devices: workercore.DevicesStatus{
			Groups: []workercore.DevicesAllocationGroup{{
				ID: "gpu-0", Manufacturer: "NVIDIA",
				Accelerators: []workercore.AcceleratorAllocation{{
					ID: "GPU-abc", Index: 0,
					Mode:                     workercore.DeviceAllocationModePartitioned,
					Allocated:                1_600_000,
					AllocatedPhysicalProfile: "full",
					AllocatedPhysicalPlacements: []workercore.AcceleratorPlacement{
						{Start: 0, Length: 1_600_000},
					},
				}},
			}},
		}},
	})
	require.NoError(t, err, "the partitioned record is marshalable")

	return string(raw)
}

// TestModelDeploymentRetirementRelease_RestartKeepsTheEvidence is the durability control. The
// reconciler that captured the evidence is thrown away and a new one is built from the same server;
// a record that only ever lived in one process would not answer for anything.
func TestModelDeploymentRetirementRelease_RestartKeepsTheEvidence(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))
	scenario.settleLedger()

	// A DIFFERENT reconciler, built from scratch against the same server. Nothing is carried over
	// from the writer: no in-memory record, no plan, no cached object.
	restarted := retirementRouterReconciler(scenario.cli, &scriptedDrainReader{}, time.Now(), nil)

	fresh := new(workercore.ModelDeployment)
	require.NoError(t, restarted.Client.Get(context.Background(),
		ctrlcli.ObjectKeyFromObject(scenario.md), fresh))
	fresh.Status.Retirement.State = workercore.ModelDeploymentRetirementStateSettling
	require.NoError(t, restarted.Client.Status().Update(context.Background(), fresh))
	fresh = new(workercore.ModelDeployment)
	require.NoError(t, restarted.Client.Get(context.Background(),
		ctrlcli.ObjectKeyFromObject(scenario.md), fresh))

	plan := &modelDeploymentRetirementPlan{Reservation: fresh.Status.Retirement, held: setsOf("member-1")}
	plan.Hold = true
	restarted.advanceModelDeploymentRetirementToCompleted(context.Background(), fresh, plan)

	assert.Equal(t, workercore.ModelDeploymentRetirementStateCompleted, plan.Reservation.State,
		"the record survived the writer, reason=%s", plan.Reservation.Reason)
}

// TestModelDeploymentRetirementRelease_VisibilityBorrowerChargesNothing follows the producer: a
// visibility borrower writes no record and is charged nothing, so its presence must not make the
// node's records disagree with a ledger that has not seen it.
func TestModelDeploymentRetirementRelease_VisibilityBorrowerChargesNothing(t *testing.T) {
	scenario := newReleaseScenario(t)
	require.NoError(t, scenario.cli.Delete(context.Background(), scenario.holder))
	require.NoError(t, scenario.cli.Delete(context.Background(),
		releaseWorkload(scenario.workloadUID, "member-1", "qwen-server-one")))

	// A borrower: an ordinary pod on the node with no allocation record and no accelerator request.
	borrower := releaseHolder(t, func(pod *core.Pod) {
		pod.Name, pod.UID = "qwen-server-two", "member-2"
		delete(pod.Annotations, deviceplugin.AllocatedAcceleratorAnnoKey)
		pod.Spec.Containers[0].Resources = core.ResourceRequirements{}
	})
	require.NoError(t, scenario.cli.Create(context.Background(), borrower))

	scenario.settleLedger()

	plan, completed := scenario.settle()
	assert.True(t, completed,
		"a borrower that writes no record is charged nothing, so the ledger still agrees, reason=%s",
		plan.Reservation.Reason)
}

// h411InterleavingReader applies a change to the server the first time the node's pod input is
// read, which is between the two bookends. A change that lands there is exactly the one a
// single-sided read cannot notice.
type h411InterleavingReader struct {
	ctrlcli.Reader
	apply func()
	fired bool
	// skip is how many pod lists pass before the change lands, so a case can put it between two
	// specific reads rather than always at the first one.
	skip int
}

func (r *h411InterleavingReader) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	err := r.Reader.List(ctx, list, opts...)
	if err != nil {
		return err
	}
	if _, ok := list.(*core.PodList); ok && !r.fired {
		if r.skip > 0 {
			r.skip--

			return nil
		}
		r.fired = true
		r.apply()
	}

	return nil
}

// TestModelDeploymentRetirementRelease_InterleavedOperationChangeHolds is the evidence control. A
// record or a reservation edited while the release is being read keeps whatever fields the reader
// was not looking at, so each case below moves one binding field or one recorded fact between the
// two bookends and must HOLD. The same operation untouched is the non-vacuous positive beside it.
func TestModelDeploymentRetirementRelease_InterleavedOperationChangeHolds(t *testing.T) {
	cases := []struct {
		name  string
		apply func(*testing.T, *releaseScenario)
		want  string
	}{
		{
			name: "the generation moves",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				current.Status.Retirement.ObservedGeneration = 99
				require.NoError(t, s.cli.Status().Update(context.Background(), current))
			},
			want: "generation",
		},
		{
			name: "the role changes",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				current.Status.Retirement.RoleName = "other-role"
				require.NoError(t, s.cli.Status().Update(context.Background(), current))
			},
			want: "role",
		},
		{
			name: "the ordinal changes",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				current.Status.Retirement.ReplicaOrdinal = 7
				require.NoError(t, s.cli.Status().Update(context.Background(), current))
			},
			want: "ordinal",
		},
		{
			name: "the operation is no longer live",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				current.Status.Retirement.State = workercore.ModelDeploymentRetirementStateAborted
				require.NoError(t, s.cli.Status().Update(context.Background(), current))
			},
			want: "this collection is authorized under",
		},
		{
			name: "the evidence on the server is edited",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				edited := new(modelDeploymentRetirementRelease)
				require.NoError(t, json.Unmarshal(
					[]byte(current.Annotations[modelDeploymentRetirementReleaseAnnotation]), edited))
				edited.Members[0].Namespace = "somewhere-else"
				raw, err := json.Marshal(edited)
				require.NoError(t, err)
				current.Annotations[modelDeploymentRetirementReleaseAnnotation] = string(raw)
				require.NoError(t, s.cli.Update(context.Background(), current))
			},
			want: "not the one this operation loaded",
		},
		{
			name: "the frozen workload moves",
			apply: func(t *testing.T, s *releaseScenario) {
				current := new(workercore.ModelDeployment)
				require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
				current.Status.Retirement.TargetWorkloadUID = "other-workload"
				require.NoError(t, s.cli.Status().Update(context.Background(), current))
			},
			want: "freezes workload",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newReleaseScenario(t)
			require.NoError(t, s.cli.Delete(context.Background(), s.holder))
			require.NoError(t, s.cli.Delete(context.Background(),
				releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
			s.settleLedger()

			reader := &h411InterleavingReader{Reader: s.cli}
			reader.apply = func() { tc.apply(t, s) }
			s.r.APIReader = reader

			plan, completed := s.settle()
			require.True(t, reader.fired, "the node input collection must be reached before the change lands")
			assert.False(t, completed, "a change between the bookends must HOLD, reason=%s", plan.Reservation.Reason)
			assert.Contains(t, plan.Reservation.Reason, tc.want)
		})
	}

	// The non-vacuous positive: the same harness, the same collection point, and no change.
	t.Run("the same read with nothing changed still completes", func(t *testing.T) {
		s := newReleaseScenario(t)
		require.NoError(t, s.cli.Delete(context.Background(), s.holder))
		require.NoError(t, s.cli.Delete(context.Background(),
			releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
		s.settleLedger()

		fired := false
		s.r.APIReader = &h411InterleavingReader{Reader: s.cli, apply: func() { fired = true }}

		plan, completed := s.settle()
		assert.True(t, fired, "the node input collection must be reached")
		assert.True(t, completed,
			"an operation that did not change completes, reason=%s", plan.Reservation.Reason)
	})
}

// TestModelDeploymentRetirementRelease_CapabilityChangeHolds keeps a card's identity and changes
// what it can offer, which is a different card to release into even though every identity field
// still matches.
func TestModelDeploymentRetirementRelease_CapabilityChangeHolds(t *testing.T) {
	s := newReleaseScenario(t)
	require.NoError(t, s.cli.Delete(context.Background(), s.holder))
	require.NoError(t, s.cli.Delete(context.Background(),
		releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
	s.settleLedger()

	current := new(workercore.Devices)
	require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKey{Name: releaseNode}, current))
	// The same card, the same group, the same index. A different physical-slice ceiling.
	current.Spec.Groups[0].Accelerators[0].Status.PhysicalSliced = workercore.AcceleratorPhysicalSliced{
		Count: 7,
		Profiles: []workercore.AcceleratorPhysicalSlicedProfile{{
			Name: "1g.5gb", MemoryMib: 5120, ComputeSlices: 1, MemorySlices: 1,
		}},
	}
	require.NoError(t, s.cli.Update(context.Background(), current))

	plan, completed := s.settle()

	assert.False(t, completed, "a capability change is a changed input, reason=%s", plan.Reservation.Reason)
	assert.Contains(t, plan.Reservation.Reason, "not the one the target was carved from")
}

// h411FailingReader fails every read, so the question is whether a read error holds the operation
// rather than being read as a released card.
type h411FailingReader struct {
	ctrlcli.Reader
}

func (h411FailingReader) Get(context.Context, ctrlcli.ObjectKey, ctrlcli.Object, ...ctrlcli.GetOption) error {
	return errors.New("the server refused the read")
}

// TestModelDeploymentRetirementRelease_ReadErrorHolds covers the error path a nil reader and a
// canceled context do not reach.
func TestModelDeploymentRetirementRelease_ReadErrorHolds(t *testing.T) {
	s := newReleaseScenario(t)
	s.r.APIReader = h411FailingReader{Reader: s.cli}

	plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1")}
	plan.Hold = true
	reason, released := s.r.observeModelDeploymentRetirementReleased(context.Background(), s.md, plan)

	assert.False(t, released, "a read that failed observed nothing")
	assert.Contains(t, reason, "refused the read")
}
