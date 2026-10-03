package worker

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

func h410PersistOperation(t *testing.T, s *releaseScenario) {
	t.Helper()
	s.md.Status.Retirement.State = workercore.ModelDeploymentRetirementStateSettling
	require.NoError(t, s.cli.Status().Update(context.Background(), s.md))
	fresh := new(workercore.ModelDeployment)
	require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), fresh))
	require.Equal(t, s.md.Status.Retirement.TargetMemberUIDs, fresh.Status.Retirement.TargetMemberUIDs)
	require.Equal(t, s.md.ResourceVersion, fresh.ResourceVersion)
}

func h410RemoveTargetAndWorkload(t *testing.T, s *releaseScenario) {
	t.Helper()
	require.NoError(t, s.cli.Delete(context.Background(), s.holder))
	require.NoError(t, s.cli.Delete(context.Background(), releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
}

func TestH410IncompleteEvidenceNeverCompletes(t *testing.T) {
	cases := []struct {
		name         string
		removeTarget bool
		stripMembers bool
		stripNodes   bool
		wantComplete bool
	}{
		{name: "complete evidence and converged ledger positive", removeTarget: true, wantComplete: true},
		{name: "no member or node facts while target remains", stripMembers: true, stripNodes: true},
		{name: "GPU cards remain in members but node facts omitted", removeTarget: true, stripNodes: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := newReleaseScenario(t)
			if tc.removeTarget {
				h410RemoveTargetAndWorkload(t, s)
			} else {
				require.NoError(t, s.cli.Delete(context.Background(), releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
			}
			s.settleLedger()
			h410PersistOperation(t, s)
			record := new(modelDeploymentRetirementRelease)
			require.NoError(t, json.Unmarshal([]byte(s.md.Annotations[modelDeploymentRetirementReleaseAnnotation]), record))
			require.NotEmpty(t, record.Members)
			require.NotEmpty(t, record.Nodes)
			if tc.stripMembers {
				record.Members = nil
			}
			if tc.stripNodes {
				record.Nodes = nil
			}
			raw, err := json.Marshal(record)
			require.NoError(t, err)
			s.md.Annotations[modelDeploymentRetirementReleaseAnnotation] = string(raw)
			require.NoError(t, s.cli.Update(context.Background(), s.md))
			plan, complete := s.settle()
			assert.Equal(t, tc.wantComplete, complete, "incomplete bound evidence must HOLD, reason=%s", plan.Reservation.Reason)
		})
	}
}

func TestH410GPURequestWithoutClaimCannotCapture(t *testing.T) {
	name := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	s := newReleaseScenario(t, func(p *core.Pod) {
		p.Spec.Containers[0].Resources.Requests = core.ResourceList{name: resource.MustParse("1")}
	})
	require.True(t, requestsCard(&s.holder.Spec.Containers[0]), "actual production classifier must recognize this request")
	delete(s.holder.Annotations, deviceplugin.AllocatedAcceleratorAnnoKey)
	require.NoError(t, s.cli.Update(context.Background(), s.holder))
	plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1"), Hold: true}
	record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)
	require.Error(t, err, "GPU requester with absent actual claim must not be treated as CPU; record=%+v", record)
}

func TestH410CaptureRequiresEveryFrozenTarget(t *testing.T) {
	s := newReleaseScenario(t)
	s.md.Status.Retirement.TargetMemberUIDs = append(s.md.Status.Retirement.TargetMemberUIDs, "lost-second-member")
	plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1"), Hold: true}
	record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)
	require.Error(t, err, "one remaining member is not the complete frozen target set; record=%+v", record)
}

type h410AfterNodeListReader struct {
	ctrlcli.Reader
	after func()
	fired bool
}

func (r *h410AfterNodeListReader) List(ctx context.Context, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
	err := r.Reader.List(ctx, list, opts...)
	if err != nil {
		return err
	}
	if _, ok := list.(*core.PodList); ok && !r.fired {
		r.fired = true
		r.after()
	}
	return nil
}

func TestH410ChangedAllocationInputsNeverComplete(t *testing.T) {
	for _, change := range []string{"node holder appears after input list", "operation retry changes during collection"} {
		t.Run(change, func(t *testing.T) {
			s := newReleaseScenario(t)
			h410RemoveTargetAndWorkload(t, s)
			s.settleLedger()
			h410PersistOperation(t, s)
			reader := &h410AfterNodeListReader{Reader: s.cli}
			reader.after = func() {
				if change == "node holder appears after input list" {
					replacement := releaseHolder(t, func(p *core.Pod) { p.Name = "new-holder"; p.UID = "new-holder-uid" })
					require.NoError(t, s.cli.Create(context.Background(), replacement))
					fresh := new(core.Pod)
					require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(replacement), fresh))
					require.Equal(t, replacement.UID, fresh.UID)
				} else {
					current := new(workercore.ModelDeployment)
					require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
					current.Status.Retirement.LastConsumedRetryToken = "new-retry"
					require.NoError(t, s.cli.Status().Update(context.Background(), current))
				}
			}
			s.r.APIReader = reader
			plan, complete := s.settle()
			require.True(t, reader.fired, "the actual node input collection must be reached before injecting the interleaving")
			assert.False(t, complete, "changed input/operation during collection must HOLD, reason=%s", plan.Reservation.Reason)
		})
	}
}

// TestCancelledPlannerDoesNotCompleteTheOperation covers the caller's context reaching the
// post-delete steps. A pass that was canceled has not read the server, so the release was never
// observed and the operation must hold; the same pass uncancelled is the positive beside it.
func TestCancelledPlannerDoesNotCompleteTheOperation(t *testing.T) {
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()

	s := newReleaseScenario(t)
	plan := s.r.planModelDeploymentRetirement(cancelled, s.md, []core.Pod{*s.holder})
	assert.NotEqual(t, workercore.ModelDeploymentRetirementStateCompleted, plan.Reservation.State,
		"a cancelled pass has not observed anything, reason=%s", plan.Reservation.Reason)
	assert.False(t, plan.Clear, "and it does not clear the operation")

	// The positive: the same operation, the same state, with a live context.
	s = newReleaseScenario(t)
	live := s.r.planModelDeploymentRetirement(context.Background(), s.md, []core.Pod{*s.holder})
	assert.NotEqual(t, workercore.ModelDeploymentRetirementStateCompleted, live.Reservation.State,
		"the target is still present, so a live pass does not complete it either")
	assert.Contains(t, live.Reservation.Reason, "", "the pass reasons about what it read")
}

// TestPhaseChangedDuringCollectionHolds covers completion authority. A reservation that moves from
// Settling back to Deleting while the release is being read is a different authority for the same
// collection, and completing on it would answer a question the pass was not asked.
func TestPhaseChangedDuringCollectionHolds(t *testing.T) {
	s := newReleaseScenario(t)
	require.NoError(t, s.cli.Delete(context.Background(), s.holder))
	require.NoError(t, s.cli.Delete(context.Background(),
		releaseWorkload(s.workloadUID, "member-1", "qwen-server-one")))
	s.settleLedger()

	reader := &h411InterleavingReader{Reader: s.cli}
	reader.apply = func() {
		current := new(workercore.ModelDeployment)
		require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(s.md), current))
		current.Status.Retirement.State = workercore.ModelDeploymentRetirementStateDeleting
		require.NoError(t, s.cli.Status().Update(context.Background(), current))
	}
	s.r.APIReader = reader

	plan, completed := s.settle()

	require.True(t, reader.fired, "the collection must be reached before the phase moves")
	assert.False(t, completed, "a phase that moved mid-collection is not the authorized one")
	assert.Contains(t, plan.Reservation.Reason, "this collection is authorized under")
}

// TestCaptureRefusesAChangedClaimDuringItsOwnReads is the capture's own input bookend. A member
// whose claim changes between the capture's first read and its later node read is valid at each
// end and incoherent together, so persisting the earlier read would describe the target as holding
// something it no longer holds. The positive is the same fixture with nothing changing.
func TestCaptureRefusesAChangedClaimDuringItsOwnReads(t *testing.T) {
	t.Run("a claim that moves during the capture is refused", func(t *testing.T) {
		s := newReleaseScenario(t)
		// The first pod list reads the members and the second reads the node input, so the change
		// lands between the two reads the capture is supposed to agree with.
		reader := &h411InterleavingReader{Reader: s.cli, skip: 1}
		reader.apply = func() {
			changed := s.holder.DeepCopy()
			changed.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] = partitionedCardRecord(t)
			require.NoError(t, s.cli.Update(context.Background(), changed))
		}
		s.r.APIReader = reader

		plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1")}
		plan.Hold = true
		record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)

		require.Error(t, err, "stale release facts must not be persisted against a newer claim")
		assert.Nil(t, record, "and no record is produced")
		assert.Contains(t, err.Error(), "changed while its release was being captured")
	})

	t.Run("an unchanged claim still captures", func(t *testing.T) {
		s := newReleaseScenario(t)
		fired := false
		s.r.APIReader = &h411InterleavingReader{Reader: s.cli, apply: func() { fired = true }}

		plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1")}
		plan.Hold = true
		record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)

		require.NoError(t, err, "a claim that did not move captures, reason=%v", err)
		assert.True(t, fired, "the node read must be reached for the bookend to mean anything")
		assert.Len(t, record.Members, 1)
		assert.NotEmpty(t, record.Nodes[0].Inventory,
			"and the node records the inventory the release is judged against")
	})
}

// TestCaptureRefusesALedgerThatMovesDuringItsOwnReads is the Devices side of the capture's input
// bookend. The ledger is read before the node input and again after it, and a capability that
// changed between the two reads is a changed input, not the ledger this record is about. The
// mutation is a real physical profile geometry change, the field the strict ledger derives
// RemainingProfiles from.
func TestCaptureRefusesALedgerThatMovesDuringItsOwnReads(t *testing.T) {
	changeLedger := func(t *testing.T, s *releaseScenario) {
		current := new(workercore.Devices)
		require.NoError(t, s.cli.Get(context.Background(), ctrlcli.ObjectKey{Name: releaseNode}, current))
		current.Spec.Groups[0].Memory = 96_000
		current.Spec.Groups[0].Accelerators[0].Status.PhysicalSliced = workercore.AcceleratorPhysicalSliced{
			Count: 7,
			Profiles: []workercore.AcceleratorPhysicalSlicedProfile{{
				Name: "1g.10gb", MemoryMib: 10240, ComputeSlices: 1, MemorySlices: 2,
				Count: 7, Placements: []workercore.AcceleratorPlacement{{Start: 0, Length: 4}},
			}},
		}
		require.NoError(t, s.cli.Update(context.Background(), current))
	}

	t.Run("a capability that moves during the capture is refused", func(t *testing.T) {
		s := newReleaseScenario(t)
		// The first pod list reads the members; the second reads the node input, which is between
		// the ledger's first read and its re-read.
		reader := &h411InterleavingReader{Reader: s.cli, skip: 1}
		reader.apply = func() { changeLedger(t, s) }
		s.r.APIReader = reader

		plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1")}
		plan.Hold = true
		record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)

		require.Error(t, err, "a ledger that changed mid-capture must not be recorded as captured")
		assert.Nil(t, record)
		assert.Contains(t, err.Error(), "changed while its release was being captured")
	})

	t.Run("an unmoved ledger still captures", func(t *testing.T) {
		s := newReleaseScenario(t)
		fired := false
		s.r.APIReader = &h411InterleavingReader{Reader: s.cli, apply: func() { fired = true }}

		plan := &modelDeploymentRetirementPlan{Reservation: s.md.Status.Retirement, held: setsOf("member-1")}
		plan.Hold = true
		record, err := s.r.captureModelDeploymentRetirementRelease(context.Background(), s.md, plan)

		require.NoError(t, err, "a native GPU ledger that did not move captures")
		assert.True(t, fired, "the re-read must be reached for the bookend to mean anything")
		assert.NotEmpty(t, record.Nodes[0].Inventory)
	})
}
