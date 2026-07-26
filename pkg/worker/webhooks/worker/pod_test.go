package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueuectrlconst "sigs.k8s.io/kueue/pkg/controller/constants"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

const testLocalQueueName = "test-lq"

func newPodWebhook(objs ...ctrlcli.Object) *PodWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &PodWebhook{Client: cli, APIReader: cli}
}

// slicedPod builds a queue-routed Pod whose single container requests the given
// (parsed) resources in both Requests and Limits.
func slicedPod(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      "p",
			Labels:    map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
			},
		},
	}
}

// instanceTypeWithEntrance builds the operator-owned InstanceType that fronts
// testLocalQueueName, carrying the observed per-card VRAM (Status.Detail.Memory) the webhook
// reverse-looks-up by the queue-entrance label the Default webhook stamps. An empty memory models
// the not-yet-ready state (Detail not computed).
func instanceTypeWithEntrance(memory string) *workercore.InstanceType {
	it := &workercore.InstanceType{
		ObjectMeta: meta.ObjectMeta{
			Name:   "it-" + testLocalQueueName,
			Labels: map[string]string{QueueEntranceLabelKey: testLocalQueueName},
		},
	}
	it.Status.Detail.Memory = memory
	return it
}

// instanceTypeWithPhysicalProfiles builds the fronting InstanceType a partition request reads: a
// computed Detail (a non-empty Manufacturer marks it ready) carrying the per-card VRAM and the
// pool's aggregated partition profile inventory (name → per-instance MemoryMib + pool ceiling Count).
func instanceTypeWithPhysicalProfiles(
	memory string, profiles ...workercore.AcceleratorSlicedPhysicalDetailProfile,
) *workercore.InstanceType {
	it := instanceTypeWithEntrance(memory)
	it.Status.Detail.Manufacturer = nodefeature.ManufacturerNVIDIA
	it.Status.Detail.SlicedDetail.Physical.Profiles = profiles
	return it
}

// physicalProfiles is the canonical H100-80GB inventory the partition tests fold against.
func physicalProfiles() []workercore.AcceleratorSlicedPhysicalDetailProfile {
	return []workercore.AcceleratorSlicedPhysicalDetailProfile{
		{Name: "1g.10gb", Count: 7, MemoryMib: 10240},
		{Name: "2g.20gb", Count: 3, MemoryMib: 20480},
	}
}

// partitionKey returns the per-profile physical-partition key for NVIDIA
// (e.g. "nvidia.com/gpu.partitioned.mig-1g.10gb").
func partitionKey(profile string) core.ResourceName {
	return nodefeature.GetAcceleratablePartitionedProfileResourceName(nodefeature.ManufacturerNVIDIA, profile)
}

func TestPodWebhook_Default(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)

	cases := []struct {
		name      string
		requests  map[core.ResourceName]string
		itMemory  string // fronting InstanceType Status.Detail.Memory; "" → no InstanceType object unless itPresent
		itPresent bool   // add a fronting InstanceType even when itMemory is empty (models Detail not ready)
		wantUnits int64  // expected .sliced.units after Default; 0 → unset
		wantCores int64  // expected .sliced.cores-percentage after Default; 0 → unchecked
		wantErr   bool
	}{
		{
			name:      "memory-percentage folds to units, cores defaults to 100",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20"},
			wantUnits: 320000, // 20 × M/100
			wantCores: 100,
		},
		{
			name:      "memory-mib folds to units using the InstanceType VRAM",
			requests:  map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			itMemory:  "40960Mi", // 40Gi card; 4Gi req = 10% = 160000 units
			wantUnits: 160000,
			wantCores: 100,
		},
		{
			name:      "explicit cores-percentage is preserved",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.coresPct: "50"},
			wantUnits: 320000,
			wantCores: 50,
		},
		{
			name:      "client-supplied units is recomputed from memory (not trusted)",
			requests:  map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.units: "999"},
			wantUnits: 320000, // 20 × M/100 overwrites the forged 999
			wantCores: 100,
		},
		{
			name:      "no memory leaves units unset",
			requests:  map[core.ResourceName]string{names.card: "1"},
			wantUnits: 0,
			wantCores: 100,
		},
		{
			name:     "memory-mib without a resolvable VRAM is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			wantErr:  true,
		},
		{
			name:      "memory-mib rejected as retryable when Detail not ready",
			requests:  map[core.ResourceName]string{names.card: "1", names.memMib: "4096"},
			itPresent: true, // InstanceType exists but Status.Detail.Memory is empty
			wantErr:   true,
		},
		{
			name:     "memory-percentage over 100 is rejected (no overflow)",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "200"},
			wantErr:  true,
		},
		{
			name:     "memory-mib over the per-card VRAM is rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "81920"},
			itMemory: "40960Mi", // 40Gi card; a 80Gi request cannot fit one card
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.itMemory != "" || c.itPresent {
				objs = append(objs, instanceTypeWithEntrance(c.itMemory))
			}
			w := newPodWebhook(objs...)
			pod := slicedPod(c.requests)

			err := w.Default(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			ctr := &pod.Spec.Containers[0]
			if c.wantUnits == 0 {
				_, ok := ctr.Resources.Requests[names.units]
				assert.False(t, ok, "units should be unset")
			} else {
				reqUnits := ctr.Resources.Requests[names.units]
				limUnits := ctr.Resources.Limits[names.units]
				assert.Equal(t, c.wantUnits, reqUnits.Value(), "units request")
				assert.Equal(t, c.wantUnits, limUnits.Value(), "units limit")
			}
			if c.wantCores != 0 {
				reqCores := ctr.Resources.Requests[names.coresPct]
				assert.Equal(t, c.wantCores, reqCores.Value(), "cores-percentage")
			}
		})
	}
}

func TestPodWebhook_ValidateCreate(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	nvidiaShared := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeShared)

	cases := []struct {
		name     string
		requests map[core.ResourceName]string
		wantErr  bool
	}{
		{
			name:     "memory-percentage with compute headroom allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.coresPct: "100"},
		},
		{
			name:     "memory-percentage without cores allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20"},
		},
		{
			name:     "memory-mib allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memMib: "4096", names.coresPct: "100"},
		},
		{
			name:     "only .sliced rejected",
			requests: map[core.ResourceName]string{names.card: "1"},
			wantErr:  true,
		},
		{
			name:     ".sliced plus units but no memory rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.units: "320000", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "units-only request without the .sliced card rejected",
			requests: map[core.ResourceName]string{names.units: "320000"},
			wantErr:  true,
		},
		{
			name:     "memory-percentage over 100 rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "200", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "both memory keys rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "20", names.memMib: "4096", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "cores smaller than memory allowed",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "50", names.coresPct: "20"},
		},
		{
			name:     "non-positive memory-percentage rejected",
			requests: map[core.ResourceName]string{names.card: "1", names.memPct: "0", names.coresPct: "100"},
			wantErr:  true,
		},
		{
			name:     "sliced sub-resource without the .sliced card rejected",
			requests: map[core.ResourceName]string{names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "exclusive only allowed",
			requests: map[core.ResourceName]string{nvidiaBase: "1"},
		},
		{
			name:     "exclusive and sliced together rejected",
			requests: map[core.ResourceName]string{nvidiaBase: "1", names.card: "1", names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "shared and sliced together rejected",
			requests: map[core.ResourceName]string{nvidiaShared: "1", names.card: "1", names.memPct: "20"},
			wantErr:  true,
		},
		{
			name:     "exclusive and shared together rejected",
			requests: map[core.ResourceName]string{nvidiaBase: "1", nvidiaShared: "1"},
			wantErr:  true,
		},
		{
			// Rule 2: multi-card logical slicing is deferred, so the card key is capped at 1
			// at admission rather than failing inside Allocate after scheduling.
			name:     "two logically sliced cards rejected",
			requests: map[core.ResourceName]string{names.card: "2", names.memPct: "20"},
			wantErr:  true,
		},
		{
			// A fractional quantity must not round up to a passing 1.
			name:     "a fractional sliced card count rejected",
			requests: map[core.ResourceName]string{names.card: "1m", names.memPct: "20"},
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook()
			pod := slicedPod(c.requests)

			_, err := w.ValidateCreate(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestPodWebhook_DefaultPartitioned(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	pnames := partitionResourceNamesForBase(nvidiaBase)
	profileKey := partitionKey("1g.10gb")
	unknownKey := partitionKey("9g.99gb")

	cases := []struct {
		name      string
		requests  map[core.ResourceName]string
		it        *workercore.InstanceType
		wantUnits int64
		wantErr   bool
	}{
		{
			// 10Gi / 80Gi x D = D/8 = 200000, identical to a logical .sliced.memory-mib: 10Gi request.
			name:      "profile folds units from its VRAM",
			requests:  map[core.ResourceName]string{pnames.card: "1", profileKey: "1"},
			it:        instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...),
			wantUnits: 200000,
		},
		{
			name:     "unknown profile rejected",
			requests: map[core.ResourceName]string{pnames.card: "1", unknownKey: "1"},
			it:       instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...),
			wantErr:  true,
		},
		{
			name:     "detail not ready rejected (retryable)",
			requests: map[core.ResourceName]string{pnames.card: "1", profileKey: "1"},
			it:       instanceTypeWithEntrance("81920Mi"), // Manufacturer empty -> not ready
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook(c.it)
			pod := slicedPod(c.requests)

			err := w.Default(context.Background(), pod)
			if c.wantErr {
				assert.Error(t, err)
				return
			}
			assert.NoError(t, err)

			ctr := &pod.Spec.Containers[0]
			reqUnits := ctr.Resources.Requests[pnames.units]
			limUnits := ctr.Resources.Limits[pnames.units]
			assert.Equal(t, c.wantUnits, reqUnits.Value(), "partitioned units request")
			assert.Equal(t, c.wantUnits, limUnits.Value(), "partitioned units limit")
			// A partition request takes none of the logical budget keys.
			_, hasCores := ctr.Resources.Requests[names.coresPct]
			assert.False(t, hasCores, "a partition request must not default cores-percentage")
			_, hasLogicalUnits := ctr.Resources.Requests[names.units]
			assert.False(t, hasLogicalUnits, "a partition request must not fold the logical units key")
		})
	}
}

// TestPodWebhook_PartitionedUnitsParity guards the non-conflict property: a partition profile
// request and a logical .sliced.memory-mib request of the profile's VRAM fold to the identical
// units value, so the two families charge the same credits for the same VRAM.
func TestPodWebhook_PartitionedUnitsParity(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	pnames := partitionResourceNamesForBase(nvidiaBase)

	partitionPod := slicedPod(map[core.ResourceName]string{pnames.card: "1", partitionKey("1g.10gb"): "1"})
	logicalPod := slicedPod(map[core.ResourceName]string{names.card: "1", names.memMib: "10240"}) // 10Gi
	w := newPodWebhook(instanceTypeWithPhysicalProfiles("81920Mi", physicalProfiles()...))

	assert.NoError(t, w.Default(context.Background(), partitionPod))
	assert.NoError(t, w.Default(context.Background(), logicalPod))

	partitionUnits := partitionPod.Spec.Containers[0].Resources.Requests[pnames.units]
	logicalUnits := logicalPod.Spec.Containers[0].Resources.Requests[names.units]
	assert.Equal(t, logicalUnits.Value(), partitionUnits.Value(),
		"a partition and a same-VRAM logical slice fold identically")
}

func TestPodWebhook_ValidateCreatePartitioned(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	pnames := partitionResourceNamesForBase(nvidiaBase)
	profileKey := partitionKey("1g.10gb")
	profileKey2 := partitionKey("2g.20gb")

	cases := []struct {
		name    string
		pod     *core.Pod
		wantErr bool
	}{
		{
			name: "one card, one profile is valid",
			pod:  slicedPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "1"}),
		},
		{
			// Rule 3.
			name:    "two partitioned cards rejected",
			pod:     slicedPod(map[core.ResourceName]string{pnames.card: "2", profileKey: "1"}),
			wantErr: true,
		},
		{
			// Rule 5.
			name:    "a per-profile value of 2 rejected",
			pod:     slicedPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "2"}),
			wantErr: true,
		},
		{
			name:    "a profile key without the card key rejected",
			pod:     slicedPod(map[core.ResourceName]string{profileKey: "1"}),
			wantErr: true,
		},
		{
			// Rule 4 at container scope: a card key with no profile has no hardware shape.
			name:    "the card key alone rejected",
			pod:     slicedPod(map[core.ResourceName]string{pnames.card: "1"}),
			wantErr: true,
		},
		{
			// Rule 4.
			name:    "two profiles on one container rejected",
			pod:     slicedPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "1", profileKey2: "1"}),
			wantErr: true,
		},
		{
			// Rule 1: the partition and logical families are mutually exclusive Pod-wide, which
			// subsumes the per-container branch this replaces.
			name:    "a partition plus a logical memory key rejected",
			pod:     slicedPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "1", names.memMib: "4096"}),
			wantErr: true,
		},
		{
			// Rules 4 and 6.
			name:    "two containers naming different profiles rejected",
			pod:     twoProfilePod(pnames.card, profileKey, profileKey2),
			wantErr: true,
		},
		{
			name: "a partition on an init container alone is valid",
			pod:  initContainerPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "1"}),
		},
		{
			// Rule 5 on the init path too: init containers are validated, not skipped.
			name:    "a per-profile value of 2 on an init container rejected",
			pod:     initContainerPod(map[core.ResourceName]string{pnames.card: "1", profileKey: "2"}),
			wantErr: true,
		},
		{
			// Rules 1 (two families) and 1 again (two groups).
			name:    "an init-container partition plus an app-container exclusive rejected",
			pod:     initPartitionPlusAppExclusivePod(pnames.card, profileKey, nvidiaBase),
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newPodWebhook()
			_, err := w.ValidateCreate(context.Background(), c.pod)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestPodWebhook_CardCountRejectionsAreDistinct pins that the three "exactly 1" rules read as
// three different problems: a logical slice count names the deferral, a partition card count
// names the one-Pod-per-instance shape, and a per-profile count names the instance. A shared
// message would send an operator looking at the wrong key.
func TestPodWebhook_CardCountRejectionsAreDistinct(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	pnames := partitionResourceNamesForBase(nvidiaBase)
	profileKey := partitionKey("1g.10gb")
	w := newPodWebhook()

	messageFor := func(requests map[core.ResourceName]string) string {
		_, err := w.ValidateCreate(context.Background(), slicedPod(requests))
		if !assert.Error(t, err) {
			return ""
		}
		return err.Error()
	}

	sliced := messageFor(map[core.ResourceName]string{names.card: "2", names.memPct: "20"})
	partitioned := messageFor(map[core.ResourceName]string{pnames.card: "2", profileKey: "1"})
	profile := messageFor(map[core.ResourceName]string{pnames.card: "1", profileKey: "2"})

	assert.Contains(t, sliced, "multi-card logical slicing is not supported yet")
	assert.Contains(t, partitioned, "one Pod per instance")
	assert.Contains(t, profile, "exactly 1 instance")
	assert.NotEqual(t, sliced, partitioned)
	assert.NotEqual(t, partitioned, profile)
}

// TestPodWebhook_ClaimsConfinedToOneContainerGroup pins rule 1's group half. Two claims in
// different lifetime groups coexist rather than succeed one another — the kubelet keeps a
// finished init container's devices in its Pod record for the Pod's whole life — while the
// scheduler charges the Pod only max(sum init, sum app) per key. The node would then
// over-advertise by exactly one slot and the next tenant would fail terminally.
func TestPodWebhook_ClaimsConfinedToOneContainerGroup(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)

	claim := core.ResourceList{nvidiaBase: resource.MustParse("1")}
	both := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "init", Resources: core.ResourceRequirements{Requests: claim, Limits: claim.DeepCopy()}},
			},
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: claim.DeepCopy(), Limits: claim.DeepCopy()}},
			},
		},
	}

	w := newPodWebhook()
	_, err := w.ValidateCreate(context.Background(), both)
	assert.Error(t, err, "the same family claimed in both groups must be rejected")
	assert.Contains(t, err.Error(), "spec.initContainers",
		"the rejection must name the group that has to give up its request")

	// The same claim in one group alone is fine.
	initOnly := both.DeepCopy()
	initOnly.Spec.Containers[0].Resources = core.ResourceRequirements{}
	_, err = w.ValidateCreate(context.Background(), initOnly)
	assert.NoError(t, err)
}

// TestPodWebhook_RestartableInitContainerMayNotClaim pins rule 7: a native sidecar starts during
// the init phase and keeps running, so it overlaps every later init container as well as every
// app container and can belong to neither group.
func TestPodWebhook_RestartableInitContainerMayNotClaim(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	visibility := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)
	always := core.ContainerRestartPolicyAlways

	newPod := func(rl core.ResourceList) *core.Pod {
		return &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: "default", Name: "p",
				Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
			},
			Spec: core.PodSpec{
				InitContainers: []core.Container{{
					Name: "sidecar", RestartPolicy: &always,
					Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()},
				}},
				Containers: []core.Container{{Name: "main"}},
			},
		}
	}

	w := newPodWebhook()
	_, err := w.ValidateCreate(context.Background(), newPod(core.ResourceList{nvidiaBase: resource.MustParse("1")}))
	assert.Error(t, err, "a restartable init container may not request an accelerator")

	// The visibility resource is deliberately outside the accelerator families, so the SSH
	// sidecar shape stays admissible wherever it runs.
	_, err = w.ValidateCreate(context.Background(), newPod(core.ResourceList{visibility: resource.MustParse("1")}))
	assert.NoError(t, err, "the visibility resource is not an accelerator family")
}

// TestPodWebhook_AtMostOneSlicingContainer pins rule 6: two containers of the claiming group each
// requesting a slicing family is rejected, while two exclusive containers stay allowed.
func TestPodWebhook_AtMostOneSlicingContainer(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)

	twoContainers := func(a, b core.ResourceList) *core.Pod {
		return &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Namespace: "default", Name: "p",
				Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
			},
			Spec: core.PodSpec{Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: a, Limits: a.DeepCopy()}},
				{Name: "aux", Resources: core.ResourceRequirements{Requests: b, Limits: b.DeepCopy()}},
			}},
		}
	}
	slice := core.ResourceList{names.card: resource.MustParse("1"), names.memPct: resource.MustParse("20")}
	exclusive := core.ResourceList{nvidiaBase: resource.MustParse("1")}

	w := newPodWebhook()
	_, err := w.ValidateCreate(context.Background(), twoContainers(slice, slice.DeepCopy()))
	assert.Error(t, err, "two containers may not both request a slicing family")

	_, err = w.ValidateCreate(context.Background(), twoContainers(exclusive, exclusive.DeepCopy()))
	assert.NoError(t, err, "two whole-card containers stay allowed")
}

// TestPodWebhook_FoldsWhicheverGroupClaims pins that the units fold follows the claim rather than
// the container field: a logical slice requested on an init container is folded, which the
// app-container-only fold this replaces silently skipped.
func TestPodWebhook_FoldsWhicheverGroupClaims(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)

	pod := initContainerPod(map[core.ResourceName]string{names.card: "1", names.memPct: "20"})
	w := newPodWebhook()
	assert.NoError(t, w.Default(context.Background(), pod))

	init := &pod.Spec.InitContainers[0]
	units := init.Resources.Requests[names.units]
	cores := init.Resources.Requests[names.coresPct]
	assert.Equal(t, int64(320000), units.Value(), "units folded on the claiming init container")
	assert.Equal(t, int64(100), cores.Value(), "cores defaulted on the claiming init container")
}

// TestPodWebhook_PartitionProfileMissingMemoryIsRetryable pins that a partition profile present in
// the fronting InstanceType's inventory but with MemoryMib not yet populated (partial detail during
// detection / rollout skew) yields a retryable not-ready error from Default, not a permanent
// zero-units rejection.
func TestPodWebhook_PartitionProfileMissingMemoryIsRetryable(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	pnames := partitionResourceNamesForBase(nvidiaBase)

	// The profile is offered but its per-instance MemoryMib is still 0 (detail not fully computed).
	it := instanceTypeWithPhysicalProfiles("81920Mi",
		workercore.AcceleratorSlicedPhysicalDetailProfile{Name: "1g.10gb", Count: 7, MemoryMib: 0})
	w := newPodWebhook(it)

	pod := slicedPod(map[core.ResourceName]string{pnames.card: "1", partitionKey("1g.10gb"): "1"})
	err := w.Default(context.Background(), pod)
	assert.Error(t, err)
	assert.Contains(t, err.Error(), "not ready",
		"a partition profile with MemoryMib==0 must be a retryable not-ready rejection, not a permanent zero-units error")
}

// twoProfilePod builds a Pod whose two app containers each name a distinct partition profile — the
// unattributable multi-profile shape ValidateCreate rejects.
func twoProfilePod(card, profileA, profileB core.ResourceName) *core.Pod {
	rlA := core.ResourceList{card: resource.MustParse("1"), profileA: resource.MustParse("1")}
	rlB := core.ResourceList{card: resource.MustParse("1"), profileB: resource.MustParse("1")}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rlA, Limits: rlA.DeepCopy()}},
				{Name: "aux", Resources: core.ResourceRequirements{Requests: rlB, Limits: rlB.DeepCopy()}},
			},
		},
	}
}

// initContainerPod builds a queue-routed Pod whose accelerator request lives on a plain (not
// restartable) init container, so the webhook must validate and fold init containers too.
func initContainerPod(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "init", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
			},
			Containers: []core.Container{{Name: "main"}},
		},
	}
}

// initPartitionPlusAppExclusivePod builds a Pod with a partition request on an init container and a
// whole-card exclusive request on an app container — two families across two container groups.
func initPartitionPlusAppExclusivePod(card, profileKey, exclusiveKey core.ResourceName) *core.Pod {
	initRL := core.ResourceList{card: resource.MustParse("1"), profileKey: resource.MustParse("1")}
	appRL := core.ResourceList{exclusiveKey: resource.MustParse("1")}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default", Name: "p",
			Labels: map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			InitContainers: []core.Container{
				{Name: "init", Resources: core.ResourceRequirements{Requests: initRL, Limits: initRL.DeepCopy()}},
			},
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: appRL, Limits: appRL.DeepCopy()}},
			},
		},
	}
}

// slicedPodWithSidecar builds the two-container shape the InstanceReconciler emits
// for an SSH-enabled sliced Instance once the accelerator is colocated on `main`:
// the sliced request lives on `main`, and the `sshd` sidecar carries no accelerator
// resource. The webhook iterates all containers, so it must still default and admit
// the sliced request wherever it lands.
func slicedPodWithSidecar(requests map[core.ResourceName]string) *core.Pod {
	rl := core.ResourceList{}
	for n, v := range requests {
		rl[n] = resource.MustParse(v)
	}
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: "default",
			Name:      "p",
			Labels:    map[string]string{kueuectrlconst.QueueLabel: testLocalQueueName},
		},
		Spec: core.PodSpec{
			Containers: []core.Container{
				{Name: "main", Resources: core.ResourceRequirements{Requests: rl, Limits: rl.DeepCopy()}},
				{Name: "sshd"},
			},
		},
	}
}

// TestPodWebhook_SlicedRequestOnMainWithSidecar guards that after the accelerator is
// colocated on `main`, the Pod webhook still folds .sliced.units on `main` and admits
// the main(sliced) + sshd(no accelerator) Pod — the sidecar contributes no accelerator
// mode, so podAcceleratorModes stays at one mode.
func TestPodWebhook_SlicedRequestOnMainWithSidecar(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)

	pod := slicedPodWithSidecar(map[core.ResourceName]string{names.card: "1", names.memPct: "60"})
	w := newPodWebhook()

	// Default folds units onto main (the request holder) and leaves sshd untouched.
	assert.NoError(t, w.Default(context.Background(), pod))
	main, sshd := &pod.Spec.Containers[0], &pod.Spec.Containers[1]
	units := main.Resources.Requests[names.units]
	assert.Equal(t, int64(960000), units.Value(), "units folded on main (60 × M/100)")
	assert.Empty(t, sshd.Resources.Requests, "sshd sidecar carries no resources")

	// ValidateCreate admits: only one accelerator mode (sliced) across the Pod.
	_, err := w.ValidateCreate(context.Background(), pod)
	assert.NoError(t, err)
}

// TestPodWebhook_VisibilityResourceIsNotAMode guards that the SSH sidecar's device-only
// visibility resource is outside the known-acceleratable families, so a Pod that requests a
// real sliced accelerator on `main` and the visibility resource on `sshd` still reads as a
// single accelerator mode and admits (a known-family name here would make
// podAcceleratorModes see two modes and reject the Pod).
func TestPodWebhook_VisibilityResourceIsNotAMode(t *testing.T) {
	nvidiaBase := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeExclusive)
	names := logicalResourceNamesForBase(nvidiaBase)
	visibility := nodefeature.GetAcceleratableResourceName(nodefeature.ManufacturerNVIDIA, workercore.DeviceAllocationModeVisibility)

	pod := slicedPodWithSidecar(map[core.ResourceName]string{names.card: "1", names.memPct: "60"})
	// sshd requests the visibility resource with the same quantity as main's card count.
	cards := resource.MustParse("1")
	pod.Spec.Containers[1].Resources = core.ResourceRequirements{
		Requests: core.ResourceList{visibility: cards},
		Limits:   core.ResourceList{visibility: cards.DeepCopy()},
	}
	w := newPodWebhook()

	_, err := w.ValidateCreate(context.Background(), pod)
	assert.NoError(t, err, "main(sliced) + sshd(visibility) is a single accelerator mode")
}
