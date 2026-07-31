package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemname"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

func newInstanceWebhook(objs ...ctrlcli.Object) *InstanceWebhook {
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		Build()
	return &InstanceWebhook{Client: cli, APIReader: cli}
}

// sliceableDetail is the observed accelerator Detail that marks a fixture InstanceType as
// slice-ready: a manufacturer (so Status.Detail.AcceleratorReady is true) plus a non-zero logical
// slice count (so Status.Detail.IsLogicallySliceable is true). The webhook now reads sliceability and
// readiness from Status.Detail, so a slice-path fixture must set it.
var sliceableDetail = workercore.InstanceTypeDetail{
	Manufacturer: "nvidia",
	InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
		SlicedDetail: workercore.AcceleratorSlicedDetail{
			Logical: workercore.AcceleratorSlicedLogicalDetail{Count: 128},
		},
	},
}

// partitionedDetail is the observed accelerator Detail of a pool whose cards are all in a
// hardware partitioning mode: a manufacturer (so Status.Detail.AcceleratorReady is true), a
// per-card VRAM the partition sizing anchors on, no logical slice count (so
// Status.Detail.IsLogicallySliceable is false — a partitioned card serves no logical slice), and the
// profile inventory a partition request is validated against.
var partitionedDetail = workercore.InstanceTypeDetail{
	Manufacturer: "nvidia",
	InstanceTypeAcceleratorDetail: workercore.InstanceTypeAcceleratorDetail{
		Memory: "81920Mi", // 80Gi per card
		SlicedDetail: workercore.AcceleratorSlicedDetail{
			Physical: workercore.AcceleratorSlicedPhysicalDetail{
				Count: 2,
				Profiles: []workercore.AcceleratorSlicedPhysicalDetailProfile{
					{Name: "2g.20gb", Count: 4, MemoryMib: 20480},
					{Name: "3g.40gb", Count: 2, MemoryMib: 40960},
				},
			},
		},
	},
}

// partitionedInstanceType builds an all-partitioned pool: every card is in a partitioning mode,
// so the exclusive / shared / logical-slice views report a zero capacity (they are computed from
// unpartitioned cards only) while the partition view reports the instances the pool can host.
func partitionedInstanceType(name string) *worker.InstanceType {
	return &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{
			Detail:                 partitionedDetail,
			AcceleratorPartitioned: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("2"), Capacity: resource.MustParse("4")},
		},
	}
}

// webhookInstance builds a valid Instance (with a volume so non-type validation
// passes) referencing the given InstanceType.
func webhookInstance(name, instType string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: "default", Name: name},
		Spec: workercore.InstanceSpec{
			Type:             instType,
			InstanceTemplate: workercore.InstanceTemplate{Image: "img"},
			Volume: workercore.InstanceVolume{
				Ephemeral: &workercore.InstanceEphemeralVolume{Capacity: resource.MustParse("10Gi")},
			},
		},
	}
}

func TestInstanceWebhook_ValidateCreate(t *testing.T) {
	cases := []struct {
		name string

		instType string
		stop     bool

		wantErr bool
	}{
		{
			name:     "stopped allows missing type",
			instType: "missing",
			stop:     true,
		},
		{
			name:     "running requires type",
			instType: "missing",
			wantErr:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", c.instType)
			if c.stop {
				inst.Spec.Stop = true
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// pinnedType builds the InstanceType a pinned Instance references. The pin is not validated against
// it — only the node's existence is checked — but a running Instance still requires the type to
// exist.
func pinnedType(name string) *worker.InstanceType {
	return &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: workercore.InstanceTypeSpec{
			OS:   "linux",
			Arch: "amd64",
		},
	}
}

// TestInstanceWebhook_ValidateCreate_NodePin pins the whole of the node pin's admission contract:
// creation rejects a node that does not exist, so a typo is caught rather than leaving the Instance
// Pending forever, and nothing else about the node is checked. In particular a node outside the
// referenced InstanceType's pool is accepted by design — forbidding it would block pinning a
// card-less Instance (a model download, say) onto a specific accelerated node.
func TestInstanceWebhook_ValidateCreate_NodePin(t *testing.T) {
	const typeName = "generic-type"

	cases := []struct {
		name string

		nodeName   string
		nodeLabels map[string]string
		nodeAbsent bool
		stop       bool

		wantErr bool
	}{
		{
			name: "no pin",
		},
		{
			name:     "pin to an existing node",
			nodeName: "node-1",
		},
		{
			name:       "pin to a node that does not exist",
			nodeName:   "node-1",
			nodeAbsent: true,
			wantErr:    true,
		},
		{
			name:       "pin to an unmanaged node is accepted",
			nodeName:   "node-1",
			nodeLabels: map[string]string{},
		},
		{
			name:     "pin to a node outside the pool is accepted",
			nodeName: "node-1",
			nodeLabels: map[string]string{
				systemname.ManagedLabelKey: "true",
				core.LabelArchStable:       "arm64",
			},
		},
		{
			name:       "stopped instance still rejects a node that does not exist",
			nodeName:   "node-1",
			nodeAbsent: true,
			stop:       true,
			wantErr:    true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			objs := []ctrlcli.Object{pinnedType(typeName)}
			if c.nodeName != "" && !c.nodeAbsent {
				objs = append(objs, &core.Node{
					ObjectMeta: meta.ObjectMeta{Name: c.nodeName, Labels: c.nodeLabels},
				})
			}
			w := newInstanceWebhook(objs...)

			inst := webhookInstance("a", typeName)
			inst.Spec.NodeName = c.nodeName
			inst.Spec.Stop = c.stop

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateUpdate_NodePin pins the node pin's mutability contract — frozen while
// the Instance runs, free while it is stopped — and that no update path re-checks the node. A pin
// whose node has since gone away still starts: the Pod then stays Pending with the scheduler's own
// reason instead of the start being blocked.
func TestInstanceWebhook_ValidateUpdate_NodePin(t *testing.T) {
	const typeName = "generic-type"

	cases := []struct {
		name string

		oldNodeName string
		newNodeName string
		oldStop     bool
		newStop     bool
		nodeAbsent  bool

		wantErr bool
	}{
		{
			name:        "running instance cannot change the pin",
			oldNodeName: "node-1",
			newNodeName: "node-2",
			wantErr:     true,
		},
		{
			name:        "running instance may keep the pin",
			oldNodeName: "node-1",
			newNodeName: "node-1",
		},
		{
			name:        "stopped instance may change the pin",
			oldNodeName: "node-1",
			newNodeName: "node-2",
			oldStop:     true,
			newStop:     true,
		},
		{
			name:        "start accepts a pin",
			oldNodeName: "node-2",
			newNodeName: "node-2",
			oldStop:     true,
		},
		{
			name:        "start accepts a pin whose node is gone",
			oldNodeName: "node-2",
			newNodeName: "node-2",
			oldStop:     true,
			nodeAbsent:  true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			objs := []ctrlcli.Object{pinnedType(typeName)}
			if !c.nodeAbsent {
				objs = append(objs, &core.Node{
					ObjectMeta: meta.ObjectMeta{Name: c.newNodeName},
				})
			}
			w := newInstanceWebhook(objs...)

			instOld := webhookInstance("a", typeName)
			instOld.Spec.NodeName = c.oldNodeName
			instOld.Spec.Stop = c.oldStop
			if c.oldStop {
				instOld.Status.Phase = workerctrl.InstancePhaseStopped
			}
			inst := instOld.DeepCopy()
			inst.Spec.NodeName = c.newNodeName
			inst.Spec.Stop = c.newStop

			_, err := w.ValidateUpdate(context.Background(), instOld, inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_AdditionalVolumes pins what an additional volume entry must
// look like for the backing Pod to be constructible: an absolute mount path that neither repeats
// another entry's nor shadows the workspace, exactly one source, a relative sub path that cannot
// escape the volume, and a reference that names something. The referenced object itself is not
// looked up, matching spec.volume.persistent.
func TestInstanceWebhook_ValidateCreate_AdditionalVolumes(t *testing.T) {
	const typeName = "generic-type"

	ref := func(name string) *core.LocalObjectReference {
		return &core.LocalObjectReference{Name: name}
	}

	cases := []struct {
		name string

		volumes []workercore.InstanceAdditionalVolume

		wantErr bool
	}{
		{
			name: "no additional volumes",
		},
		{
			name: "a persistent source",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("dataset")},
			},
		},
		{
			name: "a config map source, read-only by sub path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/etc/app", ReadOnly: true, SubPath: "conf", ConfigMap: ref("app-config")},
			},
		},
		{
			name: "a secret source",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/var/run/creds", Secret: ref("app-creds")},
			},
		},
		{
			name: "a host path source",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/models", HostPath: &core.HostPathVolumeSource{Path: "/mnt/models"}},
			},
		},
		{
			name: "several entries at distinct paths",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("dataset")},
				{MountPath: "/models", Persistent: ref("models")},
			},
		},
		{
			name: "a missing mount path",
			volumes: []workercore.InstanceAdditionalVolume{
				{Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "a relative mount path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "data", Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "a duplicated mount path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("dataset")},
				{MountPath: "/data", Persistent: ref("models")},
			},
			wantErr: true,
		},
		{
			name: "a mount path shadowing the workspace",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/workspace", Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "no source",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data"},
			},
			wantErr: true,
		},
		{
			name: "two sources",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("dataset"), ConfigMap: ref("app-config")},
			},
			wantErr: true,
		},
		{
			name: "an absolute sub path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", SubPath: "/conf", Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "a sub path escaping the volume",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", SubPath: "conf/../../etc", Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "an unnamed reference",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("")},
			},
			wantErr: true,
		},
		{
			name: "an empty host path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/models", HostPath: &core.HostPathVolumeSource{}},
			},
			wantErr: true,
		},
		{
			name: "a non-canonical mount path shadowing the workspace",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/workspace/.", Persistent: ref("dataset")},
			},
			wantErr: true,
		},
		{
			name: "mount paths that differ textually but resolve to one place",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/data", Persistent: ref("dataset")},
				{MountPath: "/tmp/../data", Persistent: ref("models")},
			},
			wantErr: true,
		},
		{
			name: "a host path stepping back out of its parent",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/etc", HostPath: &core.HostPathVolumeSource{Path: "/tmp/../etc"}},
			},
			wantErr: true,
		},
		{
			name: "a relative host path",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/models", HostPath: &core.HostPathVolumeSource{Path: "models"}},
			},
			wantErr: true,
		},
		{
			name: "an unsupported host path type",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/models", HostPath: &core.HostPathVolumeSource{
					Path: "/mnt/models",
					Type: ptr.To[core.HostPathType]("Bogus"),
				}},
			},
			wantErr: true,
		},
		{
			name: "a supported host path type",
			volumes: []workercore.InstanceAdditionalVolume{
				{MountPath: "/host/models", HostPath: &core.HostPathVolumeSource{
					Path: "/mnt/models",
					Type: ptr.To(core.HostPathDirectoryOrCreate),
				}},
			},
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(pinnedType(typeName))

			inst := webhookInstance("a", typeName)
			inst.Spec.VolumeMount = "/workspace"
			inst.Spec.AdditionalVolumes = c.volumes

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateUpdate_AdditionalVolumes pins that the list is frozen while the
// Instance runs and free while it is stopped, and that starting re-checks it — an entry edited into
// an unbuildable shape while stopped would otherwise yield a Pod the API server refuses on every
// reconcile, unlike the node pin, whose worst case is merely staying Pending.
func TestInstanceWebhook_ValidateUpdate_AdditionalVolumes(t *testing.T) {
	const typeName = "generic-type"

	valid := []workercore.InstanceAdditionalVolume{
		{MountPath: "/data", Persistent: &core.LocalObjectReference{Name: "dataset"}},
	}
	invalid := []workercore.InstanceAdditionalVolume{
		{MountPath: "/data"},
	}

	cases := []struct {
		name string

		oldVolumes []workercore.InstanceAdditionalVolume
		newVolumes []workercore.InstanceAdditionalVolume
		oldStop    bool
		newStop    bool

		wantErr bool
	}{
		{
			name:       "running instance cannot change the list",
			newVolumes: valid,
			wantErr:    true,
		},
		{
			name:       "running instance may keep the list",
			oldVolumes: valid,
			newVolumes: valid,
		},
		{
			name:       "stopped instance may change the list",
			newVolumes: valid,
			oldStop:    true,
			newStop:    true,
		},
		{
			name:       "start accepts a valid list",
			oldVolumes: valid,
			newVolumes: valid,
			oldStop:    true,
		},
		{
			name:       "start rejects a list edited while stopped into an unbuildable shape",
			oldVolumes: invalid,
			newVolumes: invalid,
			oldStop:    true,
			wantErr:    true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(pinnedType(typeName))

			instOld := webhookInstance("a", typeName)
			instOld.Spec.VolumeMount = "/workspace"
			instOld.Spec.AdditionalVolumes = c.oldVolumes
			instOld.Spec.Stop = c.oldStop
			if c.oldStop {
				instOld.Status.Phase = workerctrl.InstancePhaseStopped
			}
			inst := instOld.DeepCopy()
			inst.Spec.AdditionalVolumes = c.newVolumes
			inst.Spec.Stop = c.newStop

			_, err := w.ValidateUpdate(context.Background(), instOld, inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_SlicedPercentages pins the sliced request
// contract: the memory/compute percentages must be in [0,100] and are independent
// of each other.
func TestInstanceWebhook_ValidateCreate_SlicedPercentages(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name             string
		memPct, coresPct int32
		wantErr          bool
	}{
		{name: "equal budgets allowed", memPct: 20, coresPct: 20},
		{name: "compute larger than memory allowed", memPct: 20, coresPct: 50},
		{name: "whole-card slice allowed", memPct: 100, coresPct: 100},
		{name: "no slice allowed", memPct: 0, coresPct: 0},
		{name: "compute smaller than memory allowed", memPct: 50, coresPct: 20},
		{name: "memory above 100 rejected", memPct: 101, coresPct: 101, wantErr: true},
		{name: "compute above 100 rejected", memPct: 20, coresPct: 101, wantErr: true},
		{name: "negative memory rejected", memPct: -1, wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_ResourceCaps pins that a Create is rejected when
// the request exceeds the InstanceType's per-unit RAM (unitRAM × count) or its
// LocalStorage, and accepted within those caps. The count is the accelerator request
// for an acceleratable type, else the CPU request.
func TestInstanceWebhook_ValidateCreate_ResourceCaps(t *testing.T) {
	const typeName = "gpustack-generic-linux-amd64"

	cases := []struct {
		name          string
		acceleratable bool
		unitRAM       string
		localCap      string
		count         int64
		ram           string
		localStorage  string
		wantErr       bool
	}{
		{name: "non-accel RAM within cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "4Gi", localStorage: "32Gi"},
		{name: "non-accel RAM over cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "5Gi", localStorage: "32Gi", wantErr: true},
		{name: "non-accel local storage over cap", unitRAM: "2Gi", localCap: "64Gi", count: 2, ram: "4Gi", localStorage: "100Gi", wantErr: true},
		{name: "accel within caps", acceleratable: true, unitRAM: "4Gi", localCap: "128Gi", count: 2, ram: "8Gi", localStorage: "64Gi"},
		{name: "accel RAM over cap", acceleratable: true, unitRAM: "4Gi", localCap: "128Gi", count: 2, ram: "16Gi", localStorage: "64Gi", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: c.acceleratable,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: c.unitRAM},
					LocalStorage:  c.localCap,
				},
				Status: workercore.InstanceTypeStatus{
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("100")},
					CPU:         workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("100")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			ram := resource.MustParse(c.ram)
			local := resource.MustParse(c.localStorage)
			cnt := resource.NewQuantity(c.count, resource.DecimalSI)
			res := &workercore.InstanceResources{RAM: ram, LocalStorage: local}
			if c.acceleratable {
				res.Accelerator = cnt
			} else {
				res.CPU = *cnt
			}
			inst.Spec.Resources = res

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_AcceleratedCPU pins the accelerated-CPU contract. A real
// accelerated InstanceType reports the three-view, not the CPU view, so its Status.CPU is zero —
// capping the CPU request against Status.CPU.OnceMaxRequest would reject every accelerated Instance
// (the real-cluster regression). Instead the CPU is bounded by unitResources.cpu × accelerator
// count: accepted at the cap (what defaulting sets), rejected above it.
func TestInstanceWebhook_ValidateCreate_AcceleratedCPU(t *testing.T) {
	const typeName = "gpustack-nvidia-a10g-linux-amd64"

	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			// A real accelerated type reports the three-view; Status.CPU stays zero.
			Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
		},
	}
	w := newInstanceWebhook(instType)

	cases := []struct {
		name    string
		cpu     string // request on a single-accelerator Instance; cap is unitCPU(2) × 1
		wantErr bool
	}{
		{name: "cpu at unitCPU x count accepted", cpu: "2"},
		{name: "cpu above unitCPU x count rejected", cpu: "3", wantErr: true},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			inst := webhookInstance("a", typeName)
			inst.Spec.Resources = &workercore.InstanceResources{
				CPU:          resource.MustParse(c.cpu),
				RAM:          resource.MustParse("8Gi"),
				LocalStorage: resource.MustParse("20Gi"),
				Accelerator:  resource.NewQuantity(1, resource.DecimalSI),
			}
			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_CPURejectionMessage pins what a rejected CPU request tells the
// administrator. A zero maximum is not a small limit: a drained pool keeps its ClusterQueue
// admitting, so it reports a healthy phase with a capacity of zero, and "exceeds the maximum" then
// describes a limit that was never the problem. Each state names itself instead.
func TestInstanceWebhook_ValidateCreate_CPURejectionMessage(t *testing.T) {
	const typeName = "gpustack-generic-linux-amd64"

	cases := []struct {
		name     string
		capacity string // Status.CPU.Capacity
		onceMax  string // Status.CPU.OnceMaxRequest
		cpu      string // the Instance's CPU request
		wantMsg  string // "" means the request must be accepted
	}{
		{
			name: "drained pool names the absent capacity", capacity: "0", onceMax: "0", cpu: "1",
			wantMsg: "has no CPU capacity: no managed node currently backs it",
		},
		{
			name: "saturated pool names the exhausted capacity", capacity: "48", onceMax: "0", cpu: "1",
			wantMsg: "has no CPU available: its capacity 48 is fully requested",
		},
		{
			name: "over the maximum carries the maximum", capacity: "48", onceMax: "16", cpu: "32",
			wantMsg: "exceeds the maximum CPU request 16 of instance type " + typeName,
		},
		{
			name: "within the maximum accepted", capacity: "48", onceMax: "16", cpu: "16",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
					LocalStorage:  "64Gi",
				},
				Status: workercore.InstanceTypeStatus{
					CPU: workercore.InstanceTypeResource{
						Capacity:       resource.MustParse(c.capacity),
						OnceMaxRequest: resource.MustParse(c.onceMax),
					},
				},
			}
			inst := webhookInstance("a", typeName)
			inst.Spec.Resources = &workercore.InstanceResources{
				CPU:          resource.MustParse(c.cpu),
				RAM:          resource.MustParse("2Gi"),
				LocalStorage: resource.MustParse("10Gi"),
			}

			_, err := newInstanceWebhook(instType).ValidateCreate(context.Background(), inst)
			if c.wantMsg == "" {
				assert.NoError(t, err)
				return
			}
			assert.ErrorContains(t, err, c.wantMsg)
		})
	}
}

func TestInstanceWebhook_ValidateUpdate(t *testing.T) {
	cases := []struct {
		name string

		oldType, newType string
		oldStop, newStop bool
		phase            string // applied to both old and new status
		registerType     string // "" → no InstanceType registered

		wantErr bool
	}{
		{
			name:    "stopped allows type change",
			oldType: "old", newType: "new",
			oldStop: true, newStop: true,
			phase: workerctrl.InstancePhaseStopped,
		},
		{
			name:    "running forbids type change",
			oldType: "old", newType: "new",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
		{
			name:    "start stopped requires existing type",
			oldType: "gone", newType: "gone",
			oldStop: true, newStop: false,
			phase:   workerctrl.InstancePhaseStopped,
			wantErr: true,
		},
		{
			name:    "start stopped with existing type allowed",
			oldType: "live", newType: "live",
			oldStop: true, newStop: false,
			phase:        workerctrl.InstancePhaseStopped,
			registerType: "live",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.registerType != "" {
				objs = append(objs, &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: c.registerType}})
			}
			w := newInstanceWebhook(objs...)

			old := webhookInstance("a", c.oldType)
			old.Spec.Stop = c.oldStop
			old.Status.Phase = c.phase
			neu := webhookInstance("a", c.newType)
			neu.Spec.Stop = c.newStop
			neu.Status.Phase = c.phase

			_, err := w.ValidateUpdate(context.Background(), old, neu)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateUpdate_StartRevalidatesResources pins that starting a stopped
// Instance re-validates the resources that will take effect with the SAME checks as create — not
// just the upper caps. A stopped Instance's resources are mutable (the immutability guard is
// skipped while stopped), so without this a request create would reject (CPU over the non-accel
// cap, a negative quantity, an out-of-range slice percentage) could be slipped in while stopped
// and then started.
func TestInstanceWebhook_ValidateUpdate_StartRevalidatesResources(t *testing.T) {
	const genType = "gpustack-generic-linux-amd64"
	const sliceType = "gpustack-nvidia-a10g-linux-amd64"

	generic := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: genType},
		Spec: workercore.InstanceTypeSpec{
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			CPU: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("48")},
		},
	}
	sliceable := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: sliceType},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "2", RAM: "8Gi"},
			LocalStorage:  "64Gi",
		},
		Status: workercore.InstanceTypeStatus{
			Detail:      sliceableDetail,
			Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("1")},
		},
	}

	cases := []struct {
		name     string
		instType string
		res      *workercore.InstanceResources
		wantErr  bool
	}{
		{
			name:     "start within caps allowed",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("1"), RAM: resource.MustParse("2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
		},
		{
			name:     "start with non-accel cpu over cap rejected",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("999"), RAM: resource.MustParse("2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
			wantErr: true,
		},
		{
			name:     "start with negative ram rejected",
			instType: genType,
			res: &workercore.InstanceResources{
				CPU: resource.MustParse("1"), RAM: resource.MustParse("-2Gi"), LocalStorage: resource.MustParse("10Gi"),
			},
			wantErr: true,
		},
		{
			name:     "start with out-of-range slice percentage rejected",
			instType: sliceType,
			res: &workercore.InstanceResources{
				Accelerator:                       resource.NewQuantity(1, resource.DecimalSI),
				AcceleratorSlicedMemoryPercentage: 200,
				AcceleratorSlicedCoresPercentage:  200,
			},
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(generic, sliceable)

			old := webhookInstance("a", c.instType)
			old.Spec.Stop = true
			old.Status.Phase = workerctrl.InstancePhaseStopped
			neu := webhookInstance("a", c.instType)
			neu.Spec.Stop = false
			neu.Status.Phase = workerctrl.InstancePhaseStopped
			neu.Spec.Resources = c.res

			_, err := w.ValidateUpdate(context.Background(), old, neu)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

func TestInstanceWebhook_Default(t *testing.T) {
	cases := []struct {
		name string

		stop  bool
		phase string

		wantErr bool
	}{
		{
			// Fresh (Phase "", not stopped) → the type must exist.
			name:    "fresh requires type",
			wantErr: true,
		},
		{
			name: "stopped skips type",
			stop: true,
		},
		{
			name:    "running update skips type",
			phase:   workerctrl.InstancePhaseReady,
			wantErr: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook()
			inst := webhookInstance("a", "missing")
			if c.stop {
				inst.Spec.Stop = true
			}
			inst.Status.Phase = c.phase

			err := w.Default(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_Default_SlicedPercentages verifies the webhook copies a
// lone memory/compute percentage to the other so they default to an equal share.
func TestInstanceWebhook_Default_SlicedPercentages(t *testing.T) {
	// Default reads a setting through the loopback client; point it at an empty
	// fake cluster (configured once) so the setting falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-8s"

	cases := []struct {
		name                     string
		memPct, coresPct         int32
		wantMemPct, wantCoresPct int32
	}{
		{name: "memory copied to cores", memPct: 20, coresPct: 0, wantMemPct: 20, wantCoresPct: 20},
		{name: "cores copied to memory", memPct: 0, coresPct: 30, wantMemPct: 30, wantCoresPct: 30},
		{name: "both set left unchanged", memPct: 20, coresPct: 40, wantMemPct: 20, wantCoresPct: 40},
		{name: "both zero left unchanged", memPct: 0, coresPct: 0, wantMemPct: 0, wantCoresPct: 0},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: "1", RAM: "2Gi"},
				},
				Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			acc := resource.MustParse("1")
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			err := w.Default(context.Background(), inst)
			assert.NoError(t, err)
			assert.Equal(t, c.wantMemPct, inst.Spec.Resources.AcceleratorSlicedMemoryPercentage, "memory percentage")
			assert.Equal(t, c.wantCoresPct, inst.Spec.Resources.AcceleratorSlicedCoresPercentage, "cores percentage")
		})
	}
}

// TestInstanceWebhook_Default_SlicedUnitScaling pins that on a sliceable InstanceType the
// defaulted CPU/RAM are both sized by the memory slice percentage of ONE card's unit resources
// (the fraction of the card actually reserved; the compute percentage throttles GPU cores only,
// not host resources), flooring fractions and never dropping below one, while a zero (no-slice)
// percentage takes the whole card's unit.
func TestInstanceWebhook_Default_SlicedUnitScaling(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default (which recomputes CPU/RAM regardless).
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	const unitCPU = "16"
	const unitRAM = "40Gi"

	cases := []struct {
		name             string
		memPct, coresPct int32
		wantCPU          int64 // cores
		wantRAMGi        int64
	}{
		{name: "half slice", memPct: 50, coresPct: 50, wantCPU: 8, wantRAMGi: 20},
		{name: "quarter slice", memPct: 25, coresPct: 25, wantCPU: 4, wantRAMGi: 10},
		{name: "cores rounds down", memPct: 20, coresPct: 20, wantCPU: 3, wantRAMGi: 8},
		{name: "compute share ignored for host cpu", memPct: 20, coresPct: 50, wantCPU: 3, wantRAMGi: 8},
		{name: "tiny slice floors cpu to one", memPct: 5, coresPct: 5, wantCPU: 1, wantRAMGi: 2},
		{name: "no slice takes the whole card", memPct: 0, coresPct: 0, wantCPU: 16, wantRAMGi: 40},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
					UnitResources: workercore.InstanceTypeUnitResources{CPU: unitCPU, RAM: unitRAM},
				},
				Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			inst.Spec.Resources = &workercore.InstanceResources{
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			err := w.Default(context.Background(), inst)
			assert.NoError(t, err)
			assert.Equal(t, int64(1), inst.Spec.Resources.Accelerator.Value(), "accelerator defaults to 1")
			assert.Equal(t, c.wantCPU, inst.Spec.Resources.CPU.Value(), "cpu cores")
			assert.Equal(t, c.wantRAMGi<<30, inst.Spec.Resources.RAM.Value(), "ram bytes")
		})
	}
}

// TestInstanceWebhook_Default_SlicedZeroAccelerator pins that a sliced request (a non-zero slice
// percentage) whose accelerator count is explicitly zero is defaulted to one card, so it is not
// rejected by validation for not being exactly 1 — a slice is a fraction of ONE card.
func TestInstanceWebhook_Default_SlicedZeroAccelerator(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
	}
	w := newInstanceWebhook(instType)

	inst := webhookInstance("a", typeName)
	zero := resource.MustParse("0")
	inst.Spec.Resources = &workercore.InstanceResources{
		Accelerator:                       &zero,
		AcceleratorSlicedMemoryPercentage: 50,
	}

	err := w.Default(context.Background(), inst)
	assert.NoError(t, err)
	assert.Equal(t, int64(1), inst.Spec.Resources.Accelerator.Value(), "explicit zero accelerator defaults to 1")
}

// TestInstanceWebhook_ValidateCreate_SlicedAccelerator pins that a sliced request (a non-zero
// slice percentage) on a sliceable InstanceType must be a single card: the accelerator count
// must be exactly 1 (the slice is expressed through the memory/compute percentages, not the
// card count). Whole-card (zero-percentage) requests are covered by
// TestInstanceWebhook_ValidateCreate_WholeCardOnLogicallySliceable.
func TestInstanceWebhook_ValidateCreate_SlicedAccelerator(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name    string
		acc     string // "" → accelerator left unset
		wantErr bool
	}{
		{name: "one accepted", acc: "1"},
		{name: "two rejected", acc: "2", wantErr: true},
		{name: "zero rejected", acc: "0", wantErr: true},
		{name: "fractional rejected", acc: "1m", wantErr: true}, // Value() rounds "1m" up to 1
		{name: "unset rejected", acc: "", wantErr: true},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			res := &workercore.InstanceResources{
				AcceleratorSlicedMemoryPercentage: 50,
				AcceleratorSlicedCoresPercentage:  50,
			}
			if c.acc != "" {
				q := resource.MustParse(c.acc)
				res.Accelerator = &q
			}
			inst.Spec.Resources = res

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_ValidateCreate_WholeCardOnLogicallySliceable pins that a whole-card request (both
// slice percentages zero) on a sliceable InstanceType is treated as an exclusive request: it
// may span multiple cards up to the InstanceType's whole-card OnceMaxRequest, unlike a sliced
// request which is pinned to one card.
func TestInstanceWebhook_ValidateCreate_WholeCardOnLogicallySliceable(t *testing.T) {
	const typeName = "sliced-8s"

	cases := []struct {
		name    string
		acc     string
		wantErr bool
	}{
		{name: "single card accepted", acc: "1"},
		{name: "multi card accepted", acc: "3"},
		{name: "at max accepted", acc: "4"},
		{name: "above max rejected", acc: "5", wantErr: true},
		{name: "negative rejected", acc: "-1", wantErr: true},
		{name: "fractional rejected", acc: "1m", wantErr: true}, // extended resources must be whole cards
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := &worker.InstanceType{
				ObjectMeta: meta.ObjectMeta{Name: typeName},
				Spec: workercore.InstanceTypeSpec{
					Acceleratable: true,
				},
				Status: workercore.InstanceTypeStatus{
					Detail:      sliceableDetail,
					Accelerator: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4")},
				},
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			q := resource.MustParse(c.acc)
			// No slice percentages → a whole-card (exclusive) request.
			inst.Spec.Resources = &workercore.InstanceResources{Accelerator: &q}

			_, err := w.ValidateCreate(context.Background(), inst)
			if c.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
		})
	}
}

// TestInstanceWebhook_Default_WholeCardScaling pins that a whole-card request (zero slice
// percentages) on a sliceable InstanceType scales the unit CPU/RAM by the accelerator count,
// like a non-sliceable type — not by a single card's slice fraction.
func TestInstanceWebhook_Default_WholeCardScaling(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default (which recomputes CPU/RAM regardless).
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-a10g"
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{Detail: sliceableDetail},
	}
	w := newInstanceWebhook(instType)

	inst := webhookInstance("a", typeName)
	acc := resource.MustParse("3")
	inst.Spec.Resources = &workercore.InstanceResources{Accelerator: &acc}

	err := w.Default(context.Background(), inst)
	assert.NoError(t, err)
	assert.Equal(t, int64(48), inst.Spec.Resources.CPU.Value(), "cpu scales by card count")
	assert.Equal(t, int64(120)<<30, inst.Spec.Resources.RAM.Value(), "ram scales by card count")
}

// TestInstanceWebhook_SlicedRequestNotReadyRejected pins the R3-High fail-safe: a slice request (a
// non-zero slice percentage) on an accelerated InstanceType whose Status.Detail is not yet computed
// is rejected as retryable, never silently sized or validated as a whole-card request. Both the
// mutating Default (which would otherwise fall through to whole-card CPU/RAM scaling) and the
// validating path must reject it.
func TestInstanceWebhook_SlicedRequestNotReadyRejected(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty fake
	// cluster so it falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "sliced-not-ready"
	// Accelerated, but Status.Detail is empty — the reconciler has not computed it yet.
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
	}
	w := newInstanceWebhook(instType)

	newSliceInstance := func() *workercore.Instance {
		inst := webhookInstance("a", typeName)
		acc := resource.MustParse("1")
		inst.Spec.Resources = &workercore.InstanceResources{
			Accelerator:                       &acc,
			AcceleratorSlicedMemoryPercentage: 50,
		}
		return inst
	}

	// Default must reject with a transient (retryable) error, not fall through to whole-card sizing.
	derr := w.Default(context.Background(), newSliceInstance())
	require.Error(t, derr, "Default must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(derr),
		"Default rejection is a transient (retryable) error, not a permanent Invalid")

	// ValidateCreate must reject with a transient error, not treat it as a whole-card request.
	_, cerr := w.ValidateCreate(context.Background(), newSliceInstance())
	require.Error(t, cerr, "ValidateCreate must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(cerr),
		"ValidateCreate rejection is a transient (retryable) error, not a permanent Invalid")

	// ValidateUpdate on the start (resume) path likewise rejects with a transient error.
	old := newSliceInstance()
	old.Spec.Stop = true
	old.Status.Phase = workerctrl.InstancePhaseStopped
	neu := newSliceInstance()
	neu.Spec.Stop = false
	neu.Status.Phase = workerctrl.InstancePhaseStopped
	_, uerr := w.ValidateUpdate(context.Background(), old, neu)
	require.Error(t, uerr, "ValidateUpdate start must reject a slice request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(uerr),
		"ValidateUpdate start rejection is a transient (retryable) error, not a permanent Invalid")

	// A negative slice percentage is still a slice request: the readiness gate keys on a
	// non-zero (not merely positive) percentage, so it is rejected as retryable rather than
	// falling through to whole-card sizing while Detail is not ready.
	negInst := webhookInstance("a", typeName)
	negAcc := resource.MustParse("1")
	negInst.Spec.Resources = &workercore.InstanceResources{
		Accelerator:                       &negAcc,
		AcceleratorSlicedMemoryPercentage: -1,
	}
	nerr := w.Default(context.Background(), negInst)
	require.Error(t, nerr, "Default must reject a negative slice percentage while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(nerr),
		"negative-percentage rejection is a transient (retryable) error, not a whole-card fallthrough")
}

// TestInstanceWebhook_ValidateCreate_PartitionedProfile pins the Instance-side hardware
// partition request: the profile must be one the fronting pool offers, it is mutually exclusive
// with the two logical slice percentages, and — like a logical slice — it is always exactly one
// card. A manufacturer with no hardware partitioning offers no profile at all, so any profile
// against it is rejected rather than shaped into an empty resource key.
func TestInstanceWebhook_ValidateCreate_PartitionedProfile(t *testing.T) {
	const typeName = "partitioned-h100"

	cases := []struct {
		name             string
		manufacturer     string // "" → the fixture's own (nvidia)
		logicalOnly      bool   // swap the pool for one whose cards only slice logically
		profile          string
		acc              string // "" → accelerator left unset
		memPct, coresPct int32

		wantErr     bool
		wantMessage string // substring the rejection must carry
	}{
		{name: "offered profile accepted", profile: "3g.40gb", acc: "1"},
		{name: "other offered profile accepted", profile: "2g.20gb", acc: "1"},
		{
			// A pool that CAN partition but not into this profile: the offered set is the useful
			// answer, and it is what distinguishes this from the capability rejection below.
			name: "unknown profile rejected with the offered set", profile: "7g.80gb", acc: "1",
			wantErr: true, wantMessage: "2g.20gb 3g.40gb",
		},
		{
			// A pool that cannot partition at all: without the capability guard this fell through
			// to "offered: []", which reads as a mistyped profile rather than a pool that does
			// not partition.
			name:        "partition request against an all-logical pool names the missing capability",
			logicalOnly: true, profile: "3g.40gb", acc: "1",
			wantErr: true, wantMessage: "does not offer hardware partitioning",
		},
		{
			name: "profile with memory percentage rejected", profile: "3g.40gb", acc: "1", memPct: 50,
			wantErr: true, wantMessage: "mutually exclusive",
		},
		{
			name: "profile with cores percentage rejected", profile: "3g.40gb", acc: "1", coresPct: 50,
			wantErr: true, wantMessage: "mutually exclusive",
		},
		{
			name: "two cards rejected", profile: "3g.40gb", acc: "2",
			wantErr: true, wantMessage: "exactly 1",
		},
		{
			name: "fractional card rejected", profile: "3g.40gb", acc: "1m",
			wantErr: true, wantMessage: "exactly 1",
		},
		{
			name:         "manufacturer without hardware partitioning rejected",
			manufacturer: "ascend", profile: "3g.40gb", acc: "1",
			wantErr: true, wantMessage: "does not support hardware partitioning",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := partitionedInstanceType(typeName)
			if c.manufacturer != "" {
				instType.Status.Detail.Manufacturer = c.manufacturer
			}
			if c.logicalOnly {
				instType.Status.Detail = sliceableDetail
				instType.Status.AcceleratorPartitioned = workercore.InstanceTypeResource{}
			}
			w := newInstanceWebhook(instType)

			inst := webhookInstance("a", typeName)
			res := &workercore.InstanceResources{
				AcceleratorPartitionedProfile:     c.profile,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}
			if c.acc != "" {
				q := resource.MustParse(c.acc)
				res.Accelerator = &q
			}
			inst.Spec.Resources = res

			_, err := w.ValidateCreate(context.Background(), inst)
			if !c.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantMessage)
		})
	}
}

// TestInstanceWebhook_ValidateCreate_PoolCannotServe pins that a request a pool structurally
// cannot serve is rejected at admission rather than admitted into a permanent Pending. An
// all-partitioned pool has no unpartitioned card, so it serves neither a whole-card (exclusive)
// nor a logical-slice request; the same requests are accepted on a logically sliceable pool.
func TestInstanceWebhook_ValidateCreate_PoolCannotServe(t *testing.T) {
	const partType = "partitioned-h100"
	const sliceType = "sliced-a10g"

	sliceable := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: sliceType},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
		Status: workercore.InstanceTypeStatus{
			Detail:            sliceableDetail,
			Accelerator:       workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("4"), Capacity: resource.MustParse("4")},
			AcceleratorSliced: workercore.InstanceTypeResource{OnceMaxRequest: resource.MustParse("100"), Capacity: resource.MustParse("400")},
		},
	}

	cases := []struct {
		name             string
		instType         string
		cards            string
		memPct, coresPct int32

		wantErr     bool
		wantMessage string
	}{
		{name: "exclusive on a logically sliceable pool accepted", instType: sliceType},
		{name: "logical slice on a logically sliceable pool accepted", instType: sliceType, memPct: 50, coresPct: 50},
		{
			// Its whole-card OnceMaxRequest is zero — the view counts free unpartitioned cards
			// and it has none — so the generic cap check rejects the claim.
			name: "exclusive on an all-partitioned pool rejected", instType: partType,
			wantErr: true, wantMessage: "exceeds the maximum accelerator request",
		},
		{
			name: "logical slice on an all-partitioned pool rejected", instType: partType, memPct: 50, coresPct: 50,
			wantErr: true, wantMessage: "does not offer logical slicing",
		},
		{
			// A zero-card request asks for no accelerator at all and the controller emits no
			// extended resource for it, so every pool serves it — the structural rejection
			// applies to a positive claim only.
			name: "a zero-card request on an all-partitioned pool accepted", instType: partType, cards: "0",
		},
		{
			// The pool check must not mask a malformed request: a negative claim reads as
			// negative, whichever pool it lands on.
			name: "a negative request on an all-partitioned pool reads as negative", instType: partType, cards: "-1",
			wantErr: true, wantMessage: "cannot be negative",
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(sliceable, partitionedInstanceType(partType))

			inst := webhookInstance("a", c.instType)
			cards := c.cards
			if cards == "" {
				cards = "1"
			}
			acc := resource.MustParse(cards)
			inst.Spec.Resources = &workercore.InstanceResources{
				Accelerator:                       &acc,
				AcceleratorSlicedMemoryPercentage: c.memPct,
				AcceleratorSlicedCoresPercentage:  c.coresPct,
			}

			_, err := w.ValidateCreate(context.Background(), inst)
			if !c.wantErr {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.wantMessage)
		})
	}
}

// TestInstanceWebhook_Default_PartitionedProfile pins the mutating side of a partition request:
// the card count is pinned to one (a partition is one instance on one card) and the host CPU/RAM
// are sized by the profile's share of a card's VRAM — the same VRAM-anchored fraction the logical
// slice path uses — instead of a whole card's unit resources.
func TestInstanceWebhook_Default_PartitionedProfile(t *testing.T) {
	// Default reads the overcommit setting through the loopback client; point it at an empty
	// fake cluster so it falls back to its default.
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "partitioned-h100"

	cases := []struct {
		name      string
		profile   string
		acc       string // "" → accelerator left unset
		wantCPU   int64  // cores, from unitCPU 16
		wantRAMGi int64  // from unitRAM 40Gi
	}{
		{name: "half card profile", profile: "3g.40gb", acc: "1", wantCPU: 8, wantRAMGi: 20},
		{name: "quarter card profile", profile: "2g.20gb", acc: "1", wantCPU: 4, wantRAMGi: 10},
		{name: "absent card count defaults to one", profile: "3g.40gb", wantCPU: 8, wantRAMGi: 20},
		{name: "explicit zero card count defaults to one", profile: "3g.40gb", acc: "0", wantCPU: 8, wantRAMGi: 20},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			w := newInstanceWebhook(partitionedInstanceType(typeName))

			inst := webhookInstance("a", typeName)
			res := &workercore.InstanceResources{AcceleratorPartitionedProfile: c.profile}
			if c.acc != "" {
				q := resource.MustParse(c.acc)
				res.Accelerator = &q
			}
			inst.Spec.Resources = res

			err := w.Default(context.Background(), inst)
			require.NoError(t, err)
			assert.Equal(t, int64(1), inst.Spec.Resources.Accelerator.Value(), "a partition is one card")
			assert.Equal(t, c.wantCPU, inst.Spec.Resources.CPU.Value(), "cpu cores")
			assert.Equal(t, c.wantRAMGi<<30, inst.Spec.Resources.RAM.Value(), "ram bytes")
		})
	}
}

// TestInstanceWebhook_PartitionRequestNotReadyRejected pins that a partition request against an
// InstanceType whose accelerator Detail is not computed yet is rejected as retryable — the profile
// inventory it is validated against lives in that Detail, so an empty one must not read as "the
// pool does not offer this profile", which would be a permanent rejection of a valid request.
func TestInstanceWebhook_PartitionRequestNotReadyRejected(t *testing.T) {
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "partitioned-not-ready"
	instType := &worker.InstanceType{
		ObjectMeta: meta.ObjectMeta{Name: typeName},
		Spec: workercore.InstanceTypeSpec{
			Acceleratable: true,
			UnitResources: workercore.InstanceTypeUnitResources{CPU: "16", RAM: "40Gi"},
		},
	}
	w := newInstanceWebhook(instType)

	newPartitionInstance := func() *workercore.Instance {
		inst := webhookInstance("a", typeName)
		acc := resource.MustParse("1")
		inst.Spec.Resources = &workercore.InstanceResources{
			Accelerator:                   &acc,
			AcceleratorPartitionedProfile: "3g.40gb",
		}
		return inst
	}

	derr := w.Default(context.Background(), newPartitionInstance())
	require.Error(t, derr, "Default must reject a partition request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(derr),
		"Default rejection is a transient (retryable) error, not a permanent Invalid")

	_, cerr := w.ValidateCreate(context.Background(), newPartitionInstance())
	require.Error(t, cerr, "ValidateCreate must reject a partition request while Detail is not ready")
	assert.True(t, kerrors.IsInternalError(cerr),
		"ValidateCreate rejection is a transient (retryable) error, not a permanent Invalid")
}

// TestInstanceWebhook_Default_PartitionProfileNotSizeable pins the gap between "the Detail is not
// computed" and "the Detail is computed but cannot size THIS profile". A profile can appear in the
// inventory before its per-instance memory is populated (partial detail during detection, or a
// device-manager rollout skew) — the Pod webhook already treats that as retryable. Default must do
// the same rather than fall back to whole-card sizing: the resources it writes stick, because
// Default does not run again once they are set, so the Instance would be permanently sized for a
// whole card while its Pod is rejected forever.
//
// The last case is the guard on the other side: a profile the pool genuinely does not offer stays a
// PERMANENT rejection from validation, and must not be turned into a retry loop.
func TestInstanceWebhook_Default_PartitionProfileNotSizeable(t *testing.T) {
	system.LoopbackCtrlClient.Configure(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

	const typeName = "partitioned-partial-detail"

	cases := []struct {
		name string
		// mutate makes the pool's observed Detail partial in one specific way.
		mutate  func(*worker.InstanceType)
		profile string

		wantDefaultRetryable bool // Default rejects as transient
		wantCreateInvalid    bool // Default passes, ValidateCreate rejects as permanent
	}{
		{
			name:    "offered profile with no memory detail is retryable",
			profile: "3g.40gb",
			mutate: func(it *worker.InstanceType) {
				profs := it.Status.Detail.SlicedDetail.Physical.Profiles
				for i := range profs {
					if profs[i].Name == "3g.40gb" {
						profs[i].MemoryMib = 0
					}
				}
			},
			wantDefaultRetryable: true,
		},
		{
			name:    "offered profile with no per-card VRAM is retryable",
			profile: "3g.40gb",
			mutate: func(it *worker.InstanceType) {
				it.Status.Detail.Memory = ""
			},
			wantDefaultRetryable: true,
		},
		{
			name:              "unoffered profile stays a permanent rejection",
			profile:           "9g.90gb",
			mutate:            func(*worker.InstanceType) {},
			wantCreateInvalid: true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			instType := partitionedInstanceType(typeName)
			c.mutate(instType)
			w := newInstanceWebhook(instType)

			newInst := func() *workercore.Instance {
				inst := webhookInstance("a", typeName)
				acc := resource.MustParse("1")
				inst.Spec.Resources = &workercore.InstanceResources{
					Accelerator:                   &acc,
					AcceleratorPartitionedProfile: c.profile,
				}
				return inst
			}

			inst := newInst()
			derr := w.Default(context.Background(), inst)
			if c.wantDefaultRetryable {
				require.Error(t, derr, "Default must reject a profile it cannot size")
				assert.True(t, kerrors.IsInternalError(derr),
					"the rejection is transient (retryable), not a permanent Invalid")
				assert.True(t, inst.Spec.Resources.CPU.IsZero(),
					"no whole-card sizing may be written when the profile cannot be sized")
				assert.True(t, inst.Spec.Resources.RAM.IsZero(),
					"no whole-card sizing may be written when the profile cannot be sized")
				return
			}

			require.NoError(t, derr, "Default must not turn a permanent condition into a retry")
			_, cerr := w.ValidateCreate(context.Background(), newInst())
			if c.wantCreateInvalid {
				require.Error(t, cerr, "an unoffered profile must be rejected")
				assert.False(t, kerrors.IsInternalError(cerr),
					"an unoffered profile is a permanent rejection naming the offered set")
			}
		})
	}
}
