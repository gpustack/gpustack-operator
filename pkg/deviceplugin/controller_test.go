package deviceplugin

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/serializer"
	"k8s.io/apimachinery/pkg/types"
	k8stesting "k8s.io/client-go/testing"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlintercept "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	apiv1 "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/json"
)

// pl builds one placement interval [start, start+length).
func pl(start, length int32) workercore.AcceleratorPlacement {
	return workercore.AcceleratorPlacement{Start: start, Length: length}
}

// a100Placements is the A100 capability inventory for the four worked-example profiles, each
// carrying its full empty-accelerator legal placement set (the detect-time cache the reconciler
// subtracts occupied from).
func a100Placements() []workercore.AcceleratorPhysicalSlicedProfile {
	return []workercore.AcceleratorPhysicalSlicedProfile{
		{
			Name: "1g.5gb", MemoryMib: 5120, ComputeSlices: 1, MemorySlices: 1, Count: 7,
			Placements: []workercore.AcceleratorPlacement{pl(0, 1), pl(1, 1), pl(2, 1), pl(3, 1), pl(4, 1), pl(5, 1), pl(6, 1)},
		},
		{
			Name: "1g.10gb", MemoryMib: 10240, ComputeSlices: 1, MemorySlices: 2, Count: 4,
			Placements: []workercore.AcceleratorPlacement{pl(0, 2), pl(2, 2), pl(4, 2), pl(6, 2)},
		},
		{
			Name: "2g.10gb", MemoryMib: 10240, ComputeSlices: 2, MemorySlices: 2, Count: 3,
			Placements: []workercore.AcceleratorPlacement{pl(0, 2), pl(2, 2), pl(4, 2)},
		},
		{
			Name: "3g.20gb", MemoryMib: 20480, ComputeSlices: 3, MemorySlices: 4, Count: 2,
			Placements: []workercore.AcceleratorPlacement{pl(0, 4), pl(4, 4)},
		},
	}
}

// physicalAndLogicalDevices returns a node's Devices with one MIG-enabled accelerator (mig-0, a
// non-empty PhysicalSliced.Profiles carrying cached Placements) and one logical-slice accelerator
// (logical-0), so a reconcile exercises the ledger fold on the MIG accelerator and its absence on
// the logical accelerator.
func physicalAndLogicalDevices(nodeName string) *workercore.Devices {
	return &workercore.Devices{
		ObjectMeta: meta.ObjectMeta{Name: nodeName},
		Spec: workercore.DevicesSpec{
			Groups: []workercore.DevicesGroup{{
				ID:           "grp-0",
				Manufacturer: nodefeature.ManufacturerNVIDIA,
				Accelerators: []workercore.Accelerator{
					{
						ID:    "mig-0",
						Index: 0,
						Status: workercore.AcceleratorStatus{PhysicalSliced: workercore.AcceleratorPhysicalSliced{
							Profiles: a100Placements(),
							Count:    7,
						}},
					},
					{
						ID:     "logical-0",
						Index:  1,
						Status: workercore.AcceleratorStatus{LogicalSliced: workercore.AcceleratorLogicalSliced{Count: 128}},
					},
				},
			}},
		},
	}
}

// allocationAnnotation renders the per-container allocation records the way the device plugin
// persists them.
func allocationAnnotation(t *testing.T, allocations PodAllocations) map[string]string {
	t.Helper()
	annoBytes, err := json.Marshal(allocations)
	require.NoError(t, err)
	return map[string]string{AllocatedAcceleratorAnnoKey: string(annoBytes)}
}

// physicalSliceAllocation records one MIG instance of profile on (group, device) at the given
// placements — the upward record the reconciler unions into the accelerator's occupied set.
func physicalSliceAllocation(group, device, profile string, placements ...workercore.AcceleratorPlacement) workercore.DevicesStatus {
	return workercore.DevicesStatus{Groups: []workercore.DevicesAllocationGroup{{
		ID:           group,
		Manufacturer: nodefeature.ManufacturerNVIDIA,
		Accelerators: []workercore.AcceleratorAllocation{{
			ID:                          device,
			Mode:                        workercore.DeviceAllocationModeSliced,
			Allocated:                   nodefeature.ResourceMaxUnits / 8,
			AllocatedPhysicalProfile:    profile,
			AllocatedPhysicalPlacements: placements,
		}},
	}}}
}

// physicalSlicePod builds a Pod whose single workload container holds one MIG instance.
func physicalSlicePod(t *testing.T, name, group, device, profile, node string, placements ...workercore.AcceleratorPlacement) *core.Pod {
	t.Helper()
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      name,
			Namespace: "default",
			UID:       types.UID(name),
			Annotations: allocationAnnotation(t, PodAllocations{
				"main": {Devices: physicalSliceAllocation(group, device, profile, placements...)},
			}),
		},
		Spec: core.PodSpec{NodeName: node},
	}
}

// TestDevicesReconciler_PatchAllocatingPod_PerContainer verifies the durable allocation record is
// keyed by container. A single slot lets a second container's Allocate erase the first from the
// ledger the reconciler rebuilds, so an accelerator still running a container would read free after
// a device-manager restart; and a repeated Allocate for one container must overwrite its own entry
// rather than charge its accelerator twice.
func TestDevicesReconciler_PatchAllocatingPod_PerContainer(t *testing.T) {
	const nodeName = "node-acc"
	ctx := context.Background()
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{Name: "p", Namespace: "default", UID: "uid-p"},
		Spec:       core.PodSpec{NodeName: nodeName},
	}
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(pod).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		Build()
	rec := &DevicesReconciler{NodeName: nodeName, Client: cli}

	reread := func() *core.Pod {
		got := new(core.Pod)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
		return got
	}
	chargedCards := func(p *core.Pod) []string {
		status, err := allocatedStatusOf(p)
		require.NoError(t, err)
		var cards []string
		for i := range status.Groups {
			for j := range status.Groups[i].Accelerators {
				cards = append(cards, status.Groups[i].Accelerators[j].ID)
			}
		}
		return cards
	}

	require.NoError(t, rec.patchAllocatingPod(ctx, pod,
		"init", physicalSliceAllocation("grp-0", "dev-0", "1g.10gb", pl(0, 2)), []string{"grp-0:dev-0:0000"}))
	require.NoError(t, rec.patchAllocatingPod(ctx, reread(),
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(0, 2)), []string{"grp-0:dev-1:0000"}))

	got := reread()
	allocations, err := AllocatedAcceleratorsOf(got)
	require.NoError(t, err)
	require.Len(t, allocations, 2, "both containers must be recorded")
	assert.Equal(t, []string{"grp-0:dev-0:0000"}, allocations["init"].DeviceIDs,
		"the device IDs kubelet offered are recorded with the allocation")
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(got), "both cards stay charged")

	// Re-recording one container replaces its own entry instead of adding a second charge.
	require.NoError(t, rec.patchAllocatingPod(ctx, got,
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(2, 2)), []string{"grp-0:dev-1:0001"}))
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(reread()),
		"a repeated Allocate for one container is idempotent, not additive")

	// A stale informer copy must not erase a sibling's claim: the patch replaces the annotation's
	// whole value, so the in-process reservations for the pod's other containers are overlaid.
	rec.reserveDevices("uid-p", "init", physicalSliceAllocation("grp-0", "dev-0", "1g.10gb", pl(0, 2)), nil)
	require.NoError(t, rec.patchAllocatingPod(ctx, pod, // the pre-patch copy, as the informer would hand it back
		"main", physicalSliceAllocation("grp-0", "dev-1", "1g.10gb", pl(2, 2)), []string{"grp-0:dev-1:0001"}))
	assert.Equal(t, []string{"dev-0", "dev-1"}, chargedCards(reread()),
		"a sibling container's claim survives a patch built from a stale pod copy")
}

// TestDevicesReconciler_UnpatchAllocatingPod verifies the inverse of patchAllocatingPod: it takes one
// container's entry back out and keeps every sibling's, whichever source records them. A sibling can be
// known only to the informer copy or only to this process's reservations, and the patch replaces the
// annotation's whole value, so a rebuild that reads one source alone erases the other's claim.
//
// It also pins the two conclusions the entry itself decides: a claim the container already held is
// restored rather than removed — the patch being undone overwrote it, so removing it would report an
// accelerator free while its instance is still carved — and an annotation left with nothing to record
// goes away entirely, because the Pod-delete prune is gated on the key's presence.
func TestDevicesReconciler_UnpatchAllocatingPod(t *testing.T) {
	const nodeName = "node-unpatch"

	held := func(device string) ContainerAllocation {
		return ContainerAllocation{
			Devices:   physicalSliceAllocation("grp-0", device, "1g.10gb", pl(0, 2)),
			DeviceIDs: []string{"grp-0:" + device + ":0000"},
		}
	}
	prior := held("dev-prior")

	cases := []struct {
		name string
		// recorded is the pod's annotation as the unpatch reads it.
		recorded PodAllocations
		// rawRecorded overrides recorded with a literal annotation value.
		rawRecorded string
		// reserved is this process's reservation table for the pod, by container.
		reserved PodAllocations
		// prior is what the container's entry held before the patch being undone, nil when nothing.
		prior          *ContainerAllocation
		wantErr        bool
		wantAbsent     bool
		wantContainers []string
	}{
		{
			name:       "the last entry goes, and the annotation with it",
			recorded:   PodAllocations{"main": held("dev-0")},
			wantAbsent: true,
		},
		{
			name:           "a sibling recorded in the annotation survives",
			recorded:       PodAllocations{"main": held("dev-0"), "sidecar": held("dev-1")},
			wantContainers: []string{"sidecar"},
		},
		{
			name:           "a sibling known only to this process survives",
			recorded:       PodAllocations{"main": held("dev-0")},
			reserved:       PodAllocations{"sidecar": held("dev-1")},
			wantContainers: []string{"sidecar"},
		},
		{
			name:           "a replayed claim is restored, not removed",
			recorded:       PodAllocations{"main": held("dev-0")},
			prior:          &prior,
			wantContainers: []string{"main"},
		},
		{
			name:        "an unreadable record is refused, not written over",
			rawRecorded: "{not json",
			wantErr:     true,
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			annotations := allocationAnnotation(t, c.recorded)
			if c.rawRecorded != "" {
				annotations = map[string]string{AllocatedAcceleratorAnnoKey: c.rawRecorded}
			}
			pod := &core.Pod{
				ObjectMeta: meta.ObjectMeta{
					Name: "p", Namespace: "default", UID: "uid-p",
					Annotations: annotations,
				},
				Spec: core.PodSpec{NodeName: nodeName},
			}
			cli := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithObjects(pod).
				Build()
			rec := &DevicesReconciler{NodeName: nodeName, Client: cli}
			for name, reserved := range c.reserved {
				rec.reserveDevices(pod.UID, name, reserved.Devices, reserved.DeviceIDs)
			}

			err := rec.unpatchAllocatingPod(ctx, pod, "main", c.prior)
			got := new(core.Pod)
			require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKeyFromObject(pod), got))
			if c.wantErr {
				require.Error(t, err, "a record this process cannot read must be refused")
				assert.Equal(t, c.rawRecorded, got.Annotations[AllocatedAcceleratorAnnoKey],
					"and left exactly as it was, since rewriting it would drop claims nobody can enumerate")
				return
			}
			require.NoError(t, err)
			if c.wantAbsent {
				assert.NotContains(t, got.Annotations, AllocatedAcceleratorAnnoKey,
					"an annotation with nothing left to record must be removed, not left behind empty")
				return
			}

			allocations, err := AllocatedAcceleratorsOf(got)
			require.NoError(t, err)
			names := make([]string, 0, len(allocations))
			for name := range allocations {
				names = append(names, name)
			}
			assert.ElementsMatch(t, c.wantContainers, names)
			if c.prior != nil {
				assert.Equal(t, *c.prior, allocations["main"],
					"the claim the container already held must come back unchanged, device IDs included")
			}
			for _, name := range c.wantContainers {
				if name == "main" {
					continue
				}
				assert.NotEmpty(t, allocations[name].DeviceIDs,
					"a sibling's device IDs travel with its allocation, whichever source it came from")
			}
		})
	}
}

// pendingReleaseKeys lists the compensating writes still waiting to be retried, as
// "<pod uid>/<container>".
func pendingReleaseKeys(r *DevicesReconciler) []string {
	r.pendingReleasesMutex.RLock()
	defer r.pendingReleasesMutex.RUnlock()
	keys := make([]string, 0, len(r.pendingReleases))
	for k := range r.pendingReleases {
		keys = append(keys, string(k.PodUID)+"/"+k.Container)
	}
	return keys
}

// TestDevicesReconciler_PendingRelease verifies a compensating write that could not land is retried
// until it does. The reservation half of a compensation is memory and cannot fail; this half is I/O
// and can, and an entry left behind keeps charging its accelerator to a container kubelet never
// started — the very leak the compensation exists to close, with nothing else retrying it.
//
// It also pins what the retry must NOT do: take back a claim the container legitimately acquired after
// the failure, or apply a stale entry to a pod that merely reuses the name.
func TestDevicesReconciler_PendingRelease(t *testing.T) {
	const nodeName = "node-pending"
	const container = "main"

	claim := ContainerAllocation{
		Devices:   physicalSliceAllocation("grp-0", "mig-0", "3g.20gb", pl(0, 4)),
		DeviceIDs: []string{"grp-0:mig-0:0000"},
	}
	other := ContainerAllocation{
		Devices:   physicalSliceAllocation("grp-0", "mig-0", "1g.10gb", pl(4, 2)),
		DeviceIDs: []string{"grp-0:mig-0:0001"},
	}

	type fixture struct {
		rec     *DevicesReconciler
		cli     ctrlcli.WithWatch
		pod     *core.Pod
		failing bool
		patches int
	}
	// newFixture builds a reconciler over one pod whose patches fail while failing is set, counting
	// every patch it issues.
	newFixture := func(t *testing.T, podUID types.UID, annotations map[string]string) *fixture {
		t.Helper()
		f := new(fixture)
		f.pod = &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name: "p", Namespace: "default", UID: podUID, Annotations: annotations,
			},
			Spec: core.PodSpec{NodeName: nodeName},
		}
		f.cli = ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithObjects(physicalAndLogicalDevices(nodeName), f.pod).
			WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
				return []string{obj.(*core.Pod).Spec.NodeName}
			}).
			WithInterceptorFuncs(ctrlintercept.Funcs{
				SubResourceUpdate: func(
					context.Context, ctrlcli.Client, string, ctrlcli.Object, ...ctrlcli.SubResourceUpdateOption,
				) error {
					return nil
				},
				Patch: func(
					ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object,
					patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
				) error {
					f.patches++
					if f.failing {
						return errors.New("the api server is unreachable")
					}
					return cli.Patch(ctx, obj, patch, opts...)
				},
			}).
			Build()
		f.rec = &DevicesReconciler{NodeName: nodeName, Client: f.cli}
		return f
	}
	reconcile := func(t *testing.T, f *fixture) ctrl.Result {
		t.Helper()
		res, err := f.rec.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
		require.NoError(t, err)
		return res
	}
	recordedFor := func(t *testing.T, f *fixture) PodAllocations {
		t.Helper()
		got := new(core.Pod)
		require.NoError(t, f.cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(f.pod), got))
		allocations, err := AllocatedAcceleratorsOf(got)
		require.NoError(t, err)
		return allocations
	}

	t.Run("a write that could not land is retried until it does", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{container: claim}))
		f.failing = true
		require.Error(t, f.rec.unpatchAllocatingPod(context.Background(), f.pod, container, nil))
		require.Equal(t, []string{"uid-p/" + container}, pendingReleaseKeys(f.rec))

		res := reconcile(t, f)
		assert.Positive(t, res.RequeueAfter,
			"nothing else brings this reconciler back, so a pending write must ask for another pass")
		assert.NotEmpty(t, pendingReleaseKeys(f.rec), "and it stays pending while the write keeps failing")

		f.failing = false
		res = reconcile(t, f)
		assert.Zero(t, res.RequeueAfter, "with nothing left pending there is nothing to come back for")
		assert.Empty(t, pendingReleaseKeys(f.rec))
		assert.NotContains(t, recordedFor(t, f), container,
			"the entry the refused allocation left behind is finally off the pod")
	})

	t.Run("the retry restores what the entry held, not just removes it", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{container: other}))
		f.failing = true
		require.Error(t, f.rec.unpatchAllocatingPod(context.Background(), f.pod, container, &claim))

		f.failing = false
		reconcile(t, f)
		assert.Equal(t, claim, recordedFor(t, f)[container],
			"the claim the container already held travels with the pending write")
	})

	t.Run("a write whose pod is gone is dropped without another patch", func(t *testing.T) {
		f := newFixture(t, "uid-p", nil)
		f.rec.recordPendingRelease("uid-gone", container, nil)

		before := f.patches
		reconcile(t, f)
		assert.Equal(t, before, f.patches, "a pod that is no longer on this node is not patched")
		assert.Empty(t, pendingReleaseKeys(f.rec))
	})

	t.Run("only a claim that landed invalidates it, never a reservation", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{container: claim}))
		f.rec.recordPendingRelease("uid-p", container, nil)

		f.rec.reserveDevices("uid-p", container, claim.Devices, claim.DeviceIDs)
		require.NotEmpty(t, pendingReleaseKeys(f.rec),
			"a reservation is not a record anything outside this process reads; if the patch that "+
				"follows it fails, this give-back is still the only thing that frees the accelerator")

		require.NoError(t, f.rec.patchAllocatingPod(
			context.Background(), f.pod, container, claim.Devices, claim.DeviceIDs,
		))
		assert.Empty(t, pendingReleaseKeys(f.rec),
			"a claim that landed supersedes the give-back that would have taken it back")
	})

	t.Run("a replacement whose own write fails keeps the give-back", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{container: claim}))
		f.rec.recordPendingRelease("uid-p", container, nil)

		// The kubelet retries the same container: it reserves, then its durable patch fails too. The
		// reservation is rolled back by the caller, and the annotation still carries the refused
		// attempt's entry — so the give-back has to survive both failures.
		f.rec.reserveDevices("uid-p", container, other.Devices, other.DeviceIDs)
		f.failing = true
		require.Error(t, f.rec.patchAllocatingPod(
			context.Background(), f.pod, container, other.Devices, other.DeviceIDs,
		))
		f.rec.releaseReservation("uid-p", container)
		require.NotEmpty(t, pendingReleaseKeys(f.rec), "nothing else is left to free that accelerator")

		f.failing = false
		reconcile(t, f)
		assert.NotContains(t, recordedFor(t, f), container, "and the retry finally gives it back")
	})

	t.Run("a pod reusing the name under a new uid is left alone", func(t *testing.T) {
		f := newFixture(t, "uid-new", allocationAnnotation(t, PodAllocations{container: claim}))
		f.rec.recordPendingRelease("uid-old", container, nil)

		reconcile(t, f)
		assert.Contains(t, recordedFor(t, f), container,
			"the new pod's own claim must survive an entry left over from the previous one")
		assert.Empty(t, pendingReleaseKeys(f.rec), "and that entry goes with the pod it belonged to")
	})

	t.Run("recording a write wakes the reconciler", func(t *testing.T) {
		f := newFixture(t, "uid-p", nil)
		f.rec.pendingReleaseEvents = make(chan ctrlevent.GenericEvent, 1)

		f.rec.recordPendingRelease("uid-p", container, nil)
		select {
		case <-f.rec.pendingReleaseEvents:
		default:
			t.Fatal("recording a compensating write must enqueue this node: the annotation change that " +
				"would otherwise wake the reconciler is the write that just failed")
		}

		// A wake-up already queued is the same wake-up, so further records coalesce into it rather
		// than blocking the Allocate that is recording them.
		f.rec.recordPendingRelease("uid-p", "init", nil)
		f.rec.recordPendingRelease("uid-p", "sidecar", nil)
		assert.Len(t, pendingReleaseKeys(f.rec), 3)
	})

	t.Run("a failed retry does not wake the reconciler again", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{container: claim}))
		f.rec.pendingReleaseEvents = make(chan ctrlevent.GenericEvent, 1)
		f.failing = true

		require.Error(t, f.rec.unpatchAllocatingPod(context.Background(), f.pod, container, nil))
		<-f.rec.pendingReleaseEvents // the first record wakes the reconciler; consume it

		res := reconcile(t, f)
		require.NotEmpty(t, pendingReleaseKeys(f.rec), "the write still has not landed")
		assert.Positive(t, res.RequeueAfter, "the requeue paces the next attempt")
		select {
		case <-f.rec.pendingReleaseEvents:
			t.Fatal("a failed retry must not send another wake-up: it would bring the next attempt " +
				"straight back with no delay, and the retry would spin")
		default:
		}
	})

	t.Run("two writes for one pod land one pass at a time", func(t *testing.T) {
		f := newFixture(t, "uid-p", allocationAnnotation(t, PodAllocations{"init": claim, container: other}))
		f.rec.recordPendingRelease("uid-p", "init", nil)
		f.rec.recordPendingRelease("uid-p", container, nil)

		reconcile(t, f)
		assert.Len(t, recordedFor(t, f), 1,
			"one write per pass: a second one in the same pass rebuilds from a pod copy the first did "+
				"not change, and puts the entry it just removed back")
		require.Len(t, pendingReleaseKeys(f.rec), 1, "the other one stays pending")

		reconcile(t, f)
		assert.Empty(t, recordedFor(t, f), "and the next pass takes it")
		assert.Empty(t, pendingReleaseKeys(f.rec))
	})

	t.Run("an unreadable record enqueues nothing", func(t *testing.T) {
		f := newFixture(t, "uid-p", map[string]string{AllocatedAcceleratorAnnoKey: "{not json"})
		require.Error(t, f.rec.unpatchAllocatingPod(context.Background(), f.pod, container, nil))
		assert.Empty(t, pendingReleaseKeys(f.rec),
			"a record this process cannot parse will not parse on a retry either")
	})
}

// TestDevicesReconciler_TerminatingPodStillCharges verifies a terminating Pod keeps charging its
// accelerator. The reclaimer destroys an instance when the Pod object is gone, not when its
// containers exit, so dropping it at the deletion timestamp would advertise a slot the hardware
// still holds.
func TestDevicesReconciler_TerminatingPodStillCharges(t *testing.T) {
	const nodeName = "node-term"
	capture := &statusCapture{}
	pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
	deleting := meta.NewTime(pod.CreationTimestamp.Add(time.Second))
	pod.DeletionTimestamp = &deleting
	pod.Finalizers = []string{"gpustack.ai/test-hold"} // the fake client rejects a deleted object without one
	rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

	reconcileNode(t, rec, nodeName)

	mig := acceleratorByID(t, capture.last, "mig-0")
	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles,
		"a terminating Pod's instance still occupies its card")
	assert.Equal(t, int32(1), remainingByProfile(t, capture.last, "mig-0", "3g.20gb"),
		"the free count must not rebound before the Pod object is gone")
}

// TestDevicesReconciler_TerminatedContainerStillCharges verifies a container that has already
// exited keeps charging its accelerator while its Pod lives. The reclaimer and kubelet both scope a
// device to the Pod's life, so filtering by container liveness would report an accelerator free
// while its instance still occupies memory slices — room the placement would then refuse. The
// assertion is on the resulting per-profile capacity, because the ledger fixture alone passes
// either way.
func TestDevicesReconciler_TerminatedContainerStillCharges(t *testing.T) {
	const nodeName = "node-exited"
	capture := &statusCapture{}
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name: "p-init", Namespace: "default", UID: "uid-init",
			Annotations: allocationAnnotation(t, PodAllocations{
				"init": {Devices: physicalSliceAllocation("grp-0", "mig-0", "3g.20gb", pl(0, 4))},
			}),
		},
		Spec: core.PodSpec{NodeName: nodeName},
		Status: core.PodStatus{
			Phase: core.PodRunning,
			InitContainerStatuses: []core.ContainerStatus{{
				Name:  "init",
				State: core.ContainerState{Terminated: &core.ContainerStateTerminated{ExitCode: 0}},
			}},
		},
	}
	rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

	reconcileNode(t, rec, nodeName)

	assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}},
		acceleratorByID(t, capture.last, "mig-0").AllocatedProfiles,
		"an exited container's instance still occupies its card")
	assert.Equal(t, int32(1), remainingByProfile(t, capture.last, "mig-0", "3g.20gb"),
		"the free count must not rebound while the Pod lives")
}

// statusCapture records the last ledger a reconcile publishes, captured at the status write
// rather than read back, so the assertion is on exactly what the reconciler wrote.
type statusCapture struct{ last *workercore.DevicesStatus }

func acceleratorByID(t *testing.T, status *workercore.DevicesStatus, id string) workercore.AcceleratorAllocation {
	t.Helper()
	require.NotNil(t, status, "no status was written")
	for i := range status.Groups {
		for j := range status.Groups[i].Accelerators {
			if status.Groups[i].Accelerators[j].ID == id {
				return status.Groups[i].Accelerators[j]
			}
		}
	}
	t.Fatalf("accelerator %q not found in reconciled status", id)
	return workercore.AcceleratorAllocation{}
}

// remainingByProfile returns the reconciled RemainingProfiles count for an accelerator's profile, 0
// if absent.
func remainingByProfile(t *testing.T, status *workercore.DevicesStatus, id, profile string) int32 {
	t.Helper()
	acc := acceleratorByID(t, status, id)
	for _, pc := range acc.RemainingProfiles {
		if pc.Name == profile {
			return pc.Count
		}
	}
	return 0
}

func newReconciler(t *testing.T, nodeName string, capture *statusCapture, objs ...ctrlcli.Object) *DevicesReconciler {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			SubResourcePatch: func(
				_ context.Context, _ ctrlcli.Client, _ string, obj ctrlcli.Object,
				_ ctrlcli.Patch, _ ...ctrlcli.SubResourcePatchOption,
			) error {
				published := obj.(*workercore.Devices).Status.DeepCopy()
				capture.last = &workercore.DevicesStatus{Groups: published.Groups}
				return nil
			},
		}).
		Build()
	return &DevicesReconciler{NodeName: nodeName, Client: cli}
}

func reconcileNode(t *testing.T, rec *DevicesReconciler, nodeName string) {
	t.Helper()
	_, err := rec.Reconcile(context.Background(), ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
	require.NoError(t, err)
}

// TestDevicesReconciler_PhysicalLedgerFold verifies the placement-aware MIG ledger is derived by
// pure annotation-merge (no NVML): an empty MIG accelerator reports its full free ceilings and a
// logical accelerator none; annotated placements fold to the worked-example Allocated/Free; two
// same-profile Pods at different slots reconstruct the real occupied set; the ledger is recomputed
// (not stomped) on a second reconcile; and missing occupancy only overstates Free, never
// understates it.
func TestDevicesReconciler_PhysicalLedgerFold(t *testing.T) {
	const nodeName = "node-mig"

	t.Run("empty MIG card reports full free, logical card empty", func(t *testing.T) {
		capture := &statusCapture{}
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName))

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Nil(t, mig.AllocatedProfiles, "empty MIG card has no allocated instances")
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 4},
			{Name: "1g.5gb", Count: 7},
			{Name: "2g.10gb", Count: 3},
			{Name: "3g.20gb", Count: 2},
		}, mig.RemainingProfiles, "empty MIG card reports full per-profile ceilings")

		logical := acceleratorByID(t, capture.last, "logical-0")
		assert.Nil(t, logical.AllocatedProfiles, "logical card carries no allocated ledger")
		assert.Nil(t, logical.RemainingProfiles, "logical card carries no free ledger")
	})

	t.Run("one 3g.20gb@slot0 folds to the worked-example ledger", func(t *testing.T) {
		capture := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles)
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 2},
			{Name: "1g.5gb", Count: 3},
			{Name: "2g.10gb", Count: 1},
			{Name: "3g.20gb", Count: 1},
		}, mig.RemainingProfiles, "matches the spec worked example")
	})

	t.Run("two same-profile pods at different slots reconstruct occupied", func(t *testing.T) {
		capture := &statusCapture{}
		p1 := physicalSlicePod(t, "p1", "grp-0", "mig-0", "1g.10gb", nodeName, pl(0, 2))
		p2 := physicalSlicePod(t, "p2", "grp-0", "mig-0", "1g.10gb", nodeName, pl(2, 2))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), p1, p2)

		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "1g.10gb", Count: 2}}, mig.AllocatedProfiles,
			"two instances of the same profile counted")
		assert.Equal(t, []workercore.AcceleratorProfileCount{
			{Name: "1g.10gb", Count: 2},
			{Name: "1g.5gb", Count: 3},
			{Name: "2g.10gb", Count: 1},
			{Name: "3g.20gb", Count: 1},
		}, mig.RemainingProfiles, "free reflects both occupied slots [0,2)+[2,4), not the empty ceiling")
	})

	t.Run("ledger recomputed (not stomped) on a second reconcile", func(t *testing.T) {
		capture := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec := newReconciler(t, nodeName, capture, physicalAndLogicalDevices(nodeName), pod)

		reconcileNode(t, rec, nodeName)
		reconcileNode(t, rec, nodeName)

		mig := acceleratorByID(t, capture.last, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}}, mig.AllocatedProfiles,
			"recomputed inside the wholesale build on the second pass, not lost")
		assert.NotEmpty(t, mig.RemainingProfiles)
	})

	t.Run("missing occupancy overstates free, never understates", func(t *testing.T) {
		// With the instance's annotation recorded, 3g.20gb free = 1. With it missing (a
		// crash-orphan the reconciler cannot see), free rises to the empty ceiling 2 — an
		// overstate. Free for the placements the reconciler does see is never inflated.
		recorded := &statusCapture{}
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		reconcileNode(t, newReconciler(t, nodeName, recorded, physicalAndLogicalDevices(nodeName), pod), nodeName)
		recordedFree := remainingByProfile(t, recorded.last, "mig-0", "3g.20gb")

		missing := &statusCapture{}
		reconcileNode(t, newReconciler(t, nodeName, missing, physicalAndLogicalDevices(nodeName)), nodeName)
		missingFree := remainingByProfile(t, missing.last, "mig-0", "3g.20gb")

		assert.GreaterOrEqual(t, missingFree, recordedFree, "missing occupancy only inflates free")
		assert.Equal(t, int32(1), recordedFree, "recorded instance leaves one 3g.20gb slot free")
		assert.Equal(t, int32(2), missingFree, "unrecorded orphan overstates free to the empty ceiling")
	})
}

// TestDevicesReconciler_ReconcileNotifier_Unsubscribes keeps the broadcast set from growing once per
// stream. kubelet opens a fresh ListAndWatch on every registration, and re-registers on every one of
// its own restarts, so a subscription that outlived its reader would be walked by every later
// broadcast — some of them synchronously under the node allocate mutex.
func TestDevicesReconciler_ReconcileNotifier_Unsubscribes(t *testing.T) {
	r := &DevicesReconciler{}

	_, releaseSliced := r.getReconcileNotifier(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeSliced,
	)
	_, releaseShared := r.getReconcileNotifier(
		nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeShared,
	)
	require.Len(t, r.notifiers, 2)

	releaseSliced()
	require.Len(t, r.notifiers, 1, "releasing one subscription must leave the other")
	assert.Equal(t, workercore.DeviceAllocationModeShared, r.notifiers[0].AllocationMode)

	// Releasing twice is what a retried teardown does; it must not take somebody else's
	// subscription with it.
	releaseSliced()
	require.Len(t, r.notifiers, 1)

	releaseShared()
	assert.Empty(t, r.notifiers)
}

// foreignSelection builds interface-policy state a concurrent writer owns on the same status:
// a selection reference, exclusions and the resolved condition. No production writer publishes
// it yet; the tests below stand in for whoever does.
func foreignSelection() (workercore.DevicesInterfacesStatus, []apiv1.Condition) {
	transition := meta.NewTime(time.Date(2026, 10, 30, 8, 0, 0, 0, time.UTC))
	return workercore.DevicesInterfacesStatus{
		RDMASelectionPolicy: &workercore.DevicesRDMASelectionPolicyStatus{
			PolicyRef: &workercore.DevicesRDMASelectionPolicyReference{
				Name: "edge-select", UID: "uid-policy", Generation: 2,
			},
			Excluded: []workercore.DevicesRDMAExclusion{{Name: "mlx5_5", Reason: "external"}},
		},
	}, []apiv1.Condition{
		{
			Type:               "RDMASelectionResolved",
			Status:             meta.ConditionTrue,
			LastTransitionTime: transition,
			Reason:             "PolicyResolved",
			Message:            "selection resolved from policy edge-select",
		},
	}
}

// ledgerWriteSpy counts status writes on every subresource path and keeps the last patch payload
// and its transport type. failWrite fails status writes only; raceForeign runs a concurrent
// writer's update just before the reconciler's write lands, over the underlying client so the spy
// does not count it.
type ledgerWriteSpy struct {
	writes        int
	patches       int
	lastPatch     []byte
	lastPatchType types.PatchType
	failWrite     bool
	raceForeign   func(context.Context, ctrlcli.Client)
}

// newLedgerReconciler builds a reconciler over a store with the Devices status subresource
// enabled, so both writers' writes land. The tracker is a plain object tracker: the default one
// records managed fields over the typed object, and the Devices spec carries a uint64 that walk
// cannot represent. Resource versions, status semantics and patch application are kept.
func newLedgerReconciler(
	t *testing.T, node string, objs ...ctrlcli.Object,
) (*DevicesReconciler, *ledgerWriteSpy) {
	t.Helper()
	spy := new(ledgerWriteSpy)
	tracker := k8stesting.NewObjectTracker(
		scheme.Scheme, serializer.NewCodecFactory(scheme.Scheme).UniversalDecoder(),
	)
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjectTracker(tracker).
		WithStatusSubresource(&workercore.Devices{}).
		WithObjects(objs...).
		WithIndex(&core.Pod{}, IndexingPodsByNodeName, func(obj ctrlcli.Object) []string {
			return []string{obj.(*core.Pod).Spec.NodeName}
		}).
		WithInterceptorFuncs(ctrlintercept.Funcs{
			SubResourceUpdate: func(
				ctx context.Context, cli ctrlcli.Client, sub string, obj ctrlcli.Object,
				opts ...ctrlcli.SubResourceUpdateOption,
			) error {
				spy.writes++
				if spy.failWrite {
					return errors.New("the api server is unreachable")
				}
				if spy.raceForeign != nil {
					spy.raceForeign(ctx, cli)
				}
				return cli.SubResource(sub).Update(ctx, obj, opts...)
			},
			SubResourcePatch: func(
				ctx context.Context, cli ctrlcli.Client, sub string, obj ctrlcli.Object,
				patch ctrlcli.Patch, opts ...ctrlcli.SubResourcePatchOption,
			) error {
				spy.writes++
				spy.patches++
				data, err := patch.Data(obj)
				if err != nil {
					return err
				}
				spy.lastPatch = data
				spy.lastPatchType = patch.Type()
				if spy.failWrite {
					return errors.New("the api server is unreachable")
				}
				if spy.raceForeign != nil {
					spy.raceForeign(ctx, cli)
				}
				return cli.SubResource(sub).Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
	return &DevicesReconciler{NodeName: node, Client: cli}, spy
}

// storedDevices reads the node's Devices back from the store the writers share.
func storedDevices(t *testing.T, rec *DevicesReconciler) *workercore.Devices {
	t.Helper()
	got := new(workercore.Devices)
	require.NoError(t, rec.Client.Get(
		context.Background(), ctrlcli.ObjectKey{Name: rec.NodeName}, got,
	))
	return got
}

// agreeingLedgerOf rebuilds the ledger a reconcile would publish for the given pods, so a fixture
// can seed the store with an already-agreeing accelerator state.
func agreeingLedgerOf(t *testing.T, devs *workercore.Devices, pods ...*core.Pod) workercore.DevicesStatus {
	t.Helper()
	items := make([]core.Pod, 0, len(pods))
	for i := range pods {
		items = append(items, *pods[i])
	}
	ledger, _ := BuildDesiredStatus(logr.Discard(), devs, &core.PodList{Items: items})
	return ledger
}

// assertSelectionSurvived compares the foreign state semantically: the references, exclusions and
// condition content must be exactly what the writer wrote, and the condition instants must be the
// same moments, wherever the codec renders them.
func assertSelectionSurvived(
	t *testing.T,
	wantIfaces workercore.DevicesInterfacesStatus, wantConditions []apiv1.Condition,
	got *workercore.DevicesStatus,
) {
	t.Helper()
	require.NotNil(t, got.Interfaces, "the selection state must survive")
	assert.True(t, kubemeta.DeepEqual(wantIfaces, *got.Interfaces),
		"the selection references and exclusions changed")
	require.Len(t, got.Conditions, len(wantConditions), "the root conditions must survive")
	for i := range wantConditions {
		want, have := wantConditions[i], got.Conditions[i]
		assert.Equal(t, want.Type, have.Type)
		assert.Equal(t, want.Status, have.Status)
		assert.Equal(t, want.Reason, have.Reason)
		assert.Equal(t, want.Message, have.Message)
		assert.True(t, want.LastTransitionTime.Time.Equal(have.LastTransitionTime.Time),
			"condition %s reports a different instant", want.Type)
	}
}

// TestDevicesReconciler_StatusWriteOwnsGroupsOnly verifies the accelerator reconcile publishes
// status.groups and nothing else: a groups change keeps the foreign selection and conditions, the
// merge patch itself carries the read's resource version and only the rebuilt groups, an emptied
// ledger removes groups while the foreign state survives, a foreign-only difference and a
// nil-beside-empty ledger write nothing, and a write that lost a race comes back as a conflict a
// fresh pass completes.
func TestDevicesReconciler_StatusWriteOwnsGroupsOnly(t *testing.T) {
	const nodeName = "node-ledger-owner"

	seedForeign := func(devs *workercore.Devices) {
		ifaces, conditions := foreignSelection()
		devs.Status.Interfaces = &ifaces
		devs.Status.Conditions = conditions
	}

	// assertOwnedPatch opens the captured write and checks the transport contract it must keep:
	// exactly one merge patch, conditioned on the resource version the ledger was read at, whose
	// body is metadata plus status and whose status touches groups alone.
	assertOwnedPatch := func(t *testing.T, spy *ledgerWriteSpy, rvAtRead string) map[string]json.RawMessage {
		t.Helper()
		assert.Equal(t, 1, spy.patches, "one ledger change is one status patch")
		assert.Equal(t, types.MergePatchType, spy.lastPatchType,
			"the ledger is published as a merge patch, so it cannot carry foreign fields")
		patchFields := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(spy.lastPatch, &patchFields))
		require.Len(t, patchFields, 2,
			"the patch carries its precondition and its status fields, and nothing else")
		require.Contains(t, patchFields, "metadata")
		require.Contains(t, patchFields, "status")
		patchMeta := map[string]any{}
		require.NoError(t, json.Unmarshal(patchFields["metadata"], &patchMeta))
		assert.Equal(t, rvAtRead, patchMeta["resourceVersion"],
			"the precondition is the resource version the ledger was read at")
		patchStatus := map[string]json.RawMessage{}
		require.NoError(t, json.Unmarshal(patchFields["status"], &patchStatus))
		require.Len(t, patchStatus, 1, "groups is the only status field this writer owns")
		require.Contains(t, patchStatus, "groups")
		return patchStatus
	}

	t.Run("a groups change keeps the foreign selection and conditions", func(t *testing.T) {
		devs := physicalAndLogicalDevices(nodeName)
		seedForeign(devs)
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		want := agreeingLedgerOf(t, devs, pod)
		rec, spy := newLedgerReconciler(t, nodeName, devs, pod)
		rvAtRead := storedDevices(t, rec).ResourceVersion

		reconcileNode(t, rec, nodeName)

		patchStatus := assertOwnedPatch(t, spy, rvAtRead)
		var published []workercore.DevicesAllocationGroup
		require.NoError(t, json.Unmarshal(patchStatus["groups"], &published))
		assert.True(t, kubemeta.DeepEqual(want.Groups, published),
			"the patch publishes the full rebuilt ledger, not a partial one")

		got := storedDevices(t, rec)
		mig := acceleratorByID(t, &got.Status, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}},
			mig.AllocatedProfiles, "the rebuild still publishes the accelerator ledger")
		ifaces, conditions := foreignSelection()
		assertSelectionSurvived(t, ifaces, conditions, &got.Status)
	})

	t.Run("a foreign-only difference writes nothing", func(t *testing.T) {
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		devs := physicalAndLogicalDevices(nodeName)
		devs.Status = agreeingLedgerOf(t, devs, pod)
		ifaces, conditions := foreignSelection()
		// The same instants the writer reported, rendered in another zone.
		shifted := make([]apiv1.Condition, len(conditions))
		for i := range conditions {
			shifted[i] = conditions[i]
			shifted[i].LastTransitionTime = meta.NewTime(
				conditions[i].LastTransitionTime.In(time.FixedZone("UTC+8", 8*3600)),
			)
		}
		devs.Status.Interfaces = &ifaces
		devs.Status.Conditions = shifted
		rec, spy := newLedgerReconciler(t, nodeName, devs, pod)

		reconcileNode(t, rec, nodeName)

		assert.Zero(t, spy.writes,
			"an agreeing ledger writes nothing, whatever the foreign fields say")
		got := storedDevices(t, rec)
		assertSelectionSurvived(t, ifaces, shifted, &got.Status)
	})

	t.Run("a nil and an empty ledger are the same no-op", func(t *testing.T) {
		devs := &workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: nodeName}}
		seedForeign(devs)
		rec, spy := newLedgerReconciler(t, nodeName, devs)

		reconcileNode(t, rec, nodeName)

		assert.Zero(t, spy.writes,
			"an empty ledger beside a nil one is not a change worth a write")
		got := storedDevices(t, rec)
		ifaces, conditions := foreignSelection()
		assertSelectionSurvived(t, ifaces, conditions, &got.Status)
	})

	t.Run("an emptied ledger removes groups and keeps the foreign status", func(t *testing.T) {
		seed := physicalAndLogicalDevices(nodeName)
		devs := seed.DeepCopy()
		devs.Status = agreeingLedgerOf(t, seed)
		ifaces, conditions := foreignSelection()
		devs.Status.Interfaces = &ifaces
		devs.Status.Conditions = conditions
		devs.Spec.Groups = nil
		rec, spy := newLedgerReconciler(t, nodeName, devs)
		rvAtRead := storedDevices(t, rec).ResourceVersion

		reconcileNode(t, rec, nodeName)

		patchStatus := assertOwnedPatch(t, spy, rvAtRead)
		cleared := string(patchStatus["groups"])
		assert.True(t, cleared == "null" || cleared == "[]",
			"an emptied ledger still sends groups and clears it, but got %s", cleared)

		got := storedDevices(t, rec)
		assert.Empty(t, got.Status.Groups, "the retired accelerators leave no ledger behind")
		assertSelectionSurvived(t, ifaces, conditions, &got.Status)
	})

	t.Run("a lost race is a conflict a fresh pass completes", func(t *testing.T) {
		devs := physicalAndLogicalDevices(nodeName)
		devs.Status = agreeingLedgerOf(t, devs)
		seedForeign(devs)
		pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
		rec, spy := newLedgerReconciler(t, nodeName, devs, pod)

		// The interface policy writer lands its own update between this pass's read and its
		// write: the selection reference moves and the resolved condition reports it.
		spy.raceForeign = func(_ context.Context, cli ctrlcli.Client) {
			live := new(workercore.Devices)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: nodeName}, live))
			live.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation = 2
			for i := range live.Status.Conditions {
				if live.Status.Conditions[i].Type == "RDMASelectionResolved" {
					live.Status.Conditions[i].Message = "selection resolved from policy edge-select at generation 2"
				}
			}
			require.NoError(t, cli.Status().Update(context.Background(), live))
		}

		_, err := rec.Reconcile(context.Background(),
			ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
		require.Error(t, err, "a write that raced a foreign update must come back as an error")
		assert.True(t, apierrors.IsConflict(err),
			"the error is the conflict itself, not a publish that guessed its way past one")

		got := storedDevices(t, rec)
		mig := acceleratorByID(t, &got.Status, "mig-0")
		assert.Nil(t, mig.AllocatedProfiles, "the stale pass publishes nothing")
		require.NotNil(t, got.Status.Interfaces)
		assert.Equal(t, int64(2), got.Status.Interfaces.RDMASelectionPolicy.PolicyRef.Generation,
			"the foreign write that won the race keeps its value")
		afterRace := got.Status.DeepCopy()

		spy.raceForeign = nil
		reconcileNode(t, rec, nodeName)

		got = storedDevices(t, rec)
		mig = acceleratorByID(t, &got.Status, "mig-0")
		assert.Equal(t, []workercore.AcceleratorProfileCount{{Name: "3g.20gb", Count: 1}},
			mig.AllocatedProfiles, "the fresh pass completes the ledger update")
		require.NotNil(t, got.Status.Interfaces)
		assert.True(t, kubemeta.DeepEqual(afterRace.Interfaces, got.Status.Interfaces) &&
			kubemeta.DeepEqual(afterRace.Conditions, got.Status.Conditions),
			"completing the ledger keeps the foreign state the race wrote")
	})
}

// TestDevicesReconciler_StatusWriteFailureStillPaysReleases verifies a failed status write does
// not starve the compensations: the pending give-back still lands in the same pass, the
// reservation and visibility sweeps still retire the entries whose pod is gone while keeping the
// live ones, the failure is still returned, and nothing is published over the failed write.
func TestDevicesReconciler_StatusWriteFailureStillPaysReleases(t *testing.T) {
	const nodeName = "node-status-fail"
	devs := physicalAndLogicalDevices(nodeName)
	devs.Status = agreeingLedgerOf(t, devs)
	pod := physicalSlicePod(t, "p-3g", "grp-0", "mig-0", "3g.20gb", nodeName, pl(0, 4))
	vanished := types.UID("pod-vanished")
	rec, spy := newLedgerReconciler(t, nodeName, devs, pod)
	// Both in-process sweep tables hold one live entry and one whose pod is long gone, so either
	// way the prune decides is observable after the pass.
	rec.reserveDevices(pod.UID, "main",
		physicalSliceAllocation("grp-0", "mig-0", "3g.20gb", pl(0, 4)), []string{"grp-0:mig-0:0000"})
	rec.reserveDevices(vanished, "main",
		physicalSliceAllocation("grp-0", "mig-0", "1g.10gb", pl(4, 2)), []string{"grp-0:mig-0:0001"})
	rec.grantVisibility(pod.UID, "main")
	rec.grantVisibility(vanished, "main")
	rec.recordPendingRelease(pod.UID, "main", nil)
	require.Equal(t, []string{string(pod.UID) + "/main"}, pendingReleaseKeys(rec))
	spy.failWrite = true

	_, err := rec.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: nodeName}})
	require.ErrorContains(t, err, "api server",
		"the write failure is still reported, so the ledger is retried from a fresh read")
	assert.Empty(t, pendingReleaseKeys(rec),
		"the compensating write must not wait on the publish")

	_, liveReserved := rec.reservedDevices(pod.UID, "main")
	assert.True(t, liveReserved,
		"the failed publish does not touch the live container's reservation")
	_, goneReserved := rec.reservedDevices(vanished, "main")
	assert.False(t, goneReserved,
		"the dead entry still leaves the reservation table in the same failed pass")
	assert.True(t, rec.visibilityGranted(pod.UID, "main"),
		"the live container's visibility grant survives the failed publish")
	assert.False(t, rec.visibilityGranted(vanished, "main"),
		"the dead grant still leaves the visibility table in the same failed pass")

	recorded := new(core.Pod)
	require.NoError(t, rec.Client.Get(context.Background(), ctrlcli.ObjectKeyFromObject(pod), recorded))
	allocations, readErr := AllocatedAcceleratorsOf(recorded)
	require.NoError(t, readErr)
	assert.NotContains(t, allocations, "main",
		"the give-back landed during the very pass whose publish failed")

	got := storedDevices(t, rec)
	mig := acceleratorByID(t, &got.Status, "mig-0")
	assert.Zero(t, mig.Allocated, "nothing was published over the failed write")
}
