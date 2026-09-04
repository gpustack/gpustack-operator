package nodefeature

import (
	"testing"

	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
)

func linkState(state workercore.DeviceInterfaceLinkState) *workercore.DeviceInterfaceLink {
	return &workercore.DeviceInterfaceLink{State: state}
}

func rdmaNIC(
	name, bus, root, numa string, switches []string, link *workercore.DeviceInterfaceLink,
) device.Interface {
	return device.Interface{
		Name: name, Bus: "pci", PciBusID: bus, PciRootID: root, PciSwitches: switches,
		NumaAffinity: numa, RDMA: true, RDMADevice: "mlx5_0", Link: link,
	}
}

// plainNIC is a PCI-backed interface with NO RDMA device of its own.
func plainNIC(name, bus, root, numa string, switches []string) device.Interface {
	return device.Interface{
		Name: name, Bus: "pci", PciBusID: bus, PciRootID: root, PciSwitches: switches,
		NumaAffinity: numa,
	}
}

// vfRDMA is a virtual function carrying a bound RDMA device and a link verdict.
//
// It has its own NUMA affinity, because the API type carries one and the sysfs reader fills it from
// the VF's own device directory. An earlier revision of this helper asserted the opposite — "a VF
// shares its physical function's, which is why the API type does not repeat them" — and the label
// reduction believed it, publishing the parent's socket for endpoints that never contributed to it.
// Its PCI path is genuinely the parent's, which is a different claim and still true.
func vfRDMA(
	bus, numa string, link *workercore.DeviceInterfaceLink,
) workercore.DeviceInterfaceVirtualFunction {
	return workercore.DeviceInterfaceVirtualFunction{
		PciBusID: bus, NumaAffinity: numa, RDMA: true, RDMADevice: "mlx5_1", Link: link,
	}
}

func withVFs(
	iface device.Interface, vfs ...workercore.DeviceInterfaceVirtualFunction,
) device.Interface {
	iface.SRIOV = true
	iface.VirtualFunctions = vfs
	return iface
}

func acceleratorAt(bus, root, numa string, switches []string) device.DevicesGroupList {
	return device.DevicesGroupList{{
		Manufacturer: ManufacturerNVIDIA,
		Accelerators: []device.Accelerator{{
			ID: "GPU-0",
			Topology: device.Topology{
				PciBusID: bus, PciRootID: root, PciSwitches: switches, NumaAffinity: numa,
			},
		}},
	}}
}

// TestConstructRDMANodeLabels pins the gate and the two informational values.
//
// The criterion is which link state withholds the capable key. `failed` must, because that is the
// whole of F5's gate; `unverified` must NOT, because withholding on an unreadable file turns "we
// could not ask" into "this node has no RDMA" and removes a working node from scheduling.
func TestConstructRDMANodeLabels(t *testing.T) {
	testCases := []struct {
		name       string
		groups     device.DevicesGroupList
		interfaces []device.Interface
		want       map[string]string
	}{
		{
			name:   "a verified link one bridge from the accelerator",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// THE gate. Every other fact about this node is unchanged from the case above.
			name:   "a failed link withholds every key",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateFailed)),
			},
			want: map[string]string{},
		},
		{
			// The converse of the gate, and equally a criterion. The name says which cell: a BOUND
			// device whose link could not be verified. The unbound one is two cases below, and it
			// is the record the detector actually writes for a tree it could not read.
			name:   "an unverified link on a bound device still emits the keys",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateUnverified)),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// The record the detector writes when the RDMA tree is PRESENT and could not be read:
			// an `unverified` verdict with `rdma` left false, because it will not claim a device it
			// could not read. Testing `rdma` before the verdict made this the one shape that turned
			// "could not ask" into "no RDMA here" and withheld the key — the exact failure the case
			// above exists to prevent, reached from the other side. The case above cannot catch it:
			// its interface is bound, so the flag carries it regardless of the verdict.
			name:   "an unverified verdict with no bound device still emits the keys",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				func() device.Interface {
					iface := plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0",
						[]string{"0000:00:01.0"})
					iface.Link = linkState(workercore.DeviceInterfaceLinkStateUnverified)
					return iface
				}(),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// The same record one level down, because readVirtualFunctions reaches it through its
			// own branch: a VF whose RDMA tree is present and unreadable.
			name:   "an unverified verdict on a virtual function still emits the keys",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				withVFs(
					plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
					workercore.DeviceInterfaceVirtualFunction{
						PciBusID: "0000:02:00.1",
						Link:     linkState(workercore.DeviceInterfaceLinkStateUnverified),
					},
				),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// The negative control that stops "any link record counts" from being the fix: a plain
			// NIC with NO verdict at all is not an RDMA endpoint, and a node of them carries no key.
			name:   "a plain NIC with no verdict emits nothing",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			},
			want: map[string]string{},
		},
		{
			// One broken and one working link is a node that can still serve RDMA, and the values
			// must describe the working one — a minimum taken over the broken interface too would
			// advertise a proximity to a link that does not carry traffic.
			name:   "a broken link beside a working one describes the working one",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateFailed)),
				rdmaNIC("eth1", "0000:81:00.0", "0000:80:01.0", "1", []string{"0000:80:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "1",
				NodeRDMADistanceLabelKey: "SYS",
			},
		},
		{
			// SR-IOV: the physical function has no RDMA of its own, and the usable device is on a
			// virtual function nested under it. Reading only the top level would report this node
			// as having no RDMA at all, which on a VF-only setup is every device it has.
			name:   "a virtual function's RDMA makes the node capable",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				withVFs(
					plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
					vfRDMA("0000:02:00.1", "", linkState(workercore.DeviceInterfaceLinkStateOK)),
				),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// The converse, so the VF traversal cannot become an unconditional yes: a VF whose
			// only RDMA device reports failed leaves the node with nothing usable.
			name:   "a virtual function whose link failed leaves the node with nothing",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				withVFs(
					plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
					vfRDMA("0000:02:00.1", "", linkState(workercore.DeviceInterfaceLinkStateFailed)),
				),
			},
			want: map[string]string{},
		},
		{
			// The VF's own NUMA affinity is what gets published, not its parent's. The reduction
			// used to run over the PARENT interface whenever any of its functions was usable, so a
			// node whose RDMA lives on socket 1 advertised socket 0 because that is where the plain
			// NIC hosting the functions sits. The PCI path is still the parent's, which is why the
			// distance below is unchanged -- a VF sits behind the same bridges.
			name:   "a virtual function publishes its own NUMA node, not its parent's",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				withVFs(
					plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
					vfRDMA("0000:02:00.1", "1", linkState(workercore.DeviceInterfaceLinkStateOK)),
				),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "1",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// And the control for the rule that keeps the two cases above consistent: a blank VF
			// affinity is the kernel declining to answer, not a different answer, so the parent's
			// value stays rather than being replaced by nothing.
			name:   "a virtual function with no NUMA affinity keeps its parent's",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				withVFs(
					plainNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
					vfRDMA("0000:02:00.1", "", linkState(workercore.DeviceInterfaceLinkStateOK)),
				),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			name:   "RDMA on both sockets carries both NUMA nodes, ordered",
			groups: acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{
				rdmaNIC("eth1", "0000:81:00.0", "0000:80:01.0", "1", []string{"0000:80:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey:  "true",
				NodeRDMANumaLabelKey:     "0_1",
				NodeRDMADistanceLabelKey: "PIX",
			},
		},
		{
			// A distance is a statement about a pair, so a node with RDMA and nothing to pair it
			// with reports no distance rather than an unknown one.
			name:   "RDMA with no accelerator reports no distance",
			groups: nil,
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
			},
			want: map[string]string{
				NodeRDMACapableLabelKey: "true",
				NodeRDMANumaLabelKey:    "0",
			},
		},
		{
			// An interface with no NUMA affinity must not contribute one. The kernel denied it, and
			// normalising it to node 0 would publish an affinity nobody read.
			name:   "an interface with no NUMA affinity contributes no NUMA value",
			groups: nil,
			interfaces: []device.Interface{
				rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "", []string{"0000:00:01.0"},
					linkState(workercore.DeviceInterfaceLinkStateOK)),
			},
			want: map[string]string{NodeRDMACapableLabelKey: "true"},
		},
		{
			name:       "no RDMA anywhere emits nothing",
			groups:     acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
			interfaces: []device.Interface{{Name: "eth0", Bus: "pci"}},
			want:       map[string]string{},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := ConstructRDMANodeLabels(tc.groups, tc.interfaces)

			if len(got) != len(tc.want) {
				t.Fatalf("labels = %v, want %v", got, tc.want)
			}
			for k, v := range tc.want {
				if got[k] != v {
					t.Errorf("label %s = %q, want %q", k, got[k], v)
				}
			}
		})
	}
}

// TestRDMALabelBudget is the arithmetic F7 turns into an acceptance criterion.
//
// A ResourceFlavor's nodeLabels selector is capped at eight entries. The pool discriminators claim
// up to five, the flavor's own accelerator-count key one more, and this feature spends one on the
// capable key. Adding a sixth pool discriminator, or a second selector key here, would push a fully
// specified accelerated pool over the cap — and the failure would arrive as a flavor the API server
// rejects, on a cluster, not here.
//
// So the count is asserted rather than described, and the pool side is COUNTED rather than assumed:
// a test naming the number five would keep passing after someone added a sixth key.
func TestRDMALabelBudget(t *testing.T) {
	const kueueNodeLabelsMax = 8

	// The widest a pool discriminator set can get: accelerated, CPU-aware, both groups real, and
	// os/arch both known.
	pool := PoolScheduleLabels(true, true, "amd-25-1", "nvidia-h100", "linux", "amd64")

	// The flavor additionally pins the accelerator count, which is not part of the pool set.
	const flavorOwnKeys = 1

	// Exactly one key of this feature's set may enter a selector.
	selectable := []string{NodeRDMACapableLabelKey}

	total := len(pool) + flavorOwnKeys + len(selectable)
	if total > kueueNodeLabelsMax {
		t.Fatalf("a fully specified accelerated pool would pin %d node labels (%d pool + %d "+
			"flavor + %d rdma), over Kueue's cap of %d; the flavor would be rejected by the API "+
			"server on a cluster rather than here",
			total, len(pool), flavorOwnKeys, len(selectable), kueueNodeLabelsMax)
	}

	// And the two informational keys must NOT be selectable, because there is no budget for them.
	for _, k := range []string{NodeRDMADistanceLabelKey, NodeRDMANumaLabelKey} {
		for _, s := range selectable {
			if k == s {
				t.Errorf("%s is treated as a selector key; the budget affords one, and an "+
					"equality selector cannot compare an ordered value anyway", k)
			}
		}
	}
}

// TestRDMALabelsAreNotPoolDiscriminators pins that these keys stay out of the pool identity.
//
// PoolFlavorSelector decides which labels identify a pool. An RDMA key leaking into it would split
// every pool by whether its nodes happen to have a working link, so a link going down would move
// nodes between pools rather than merely stop a flavor selecting them.
func TestRDMALabelsAreNotPoolDiscriminators(t *testing.T) {
	labels := PoolScheduleLabels(true, true, "amd-25-1", "nvidia-h100", "linux", "amd64")
	labels[NodeRDMACapableLabelKey] = "true"
	labels[NodeRDMADistanceLabelKey] = "PIX"
	labels[NodeRDMANumaLabelKey] = "0"

	selector := PoolFlavorSelector(labels)
	for k := range selector {
		if k == NodeRDMACapableLabelKey || k == NodeRDMADistanceLabelKey ||
			k == NodeRDMANumaLabelKey {
			t.Errorf("%s reached the pool selector; a link going down would then move nodes "+
				"between pools instead of stopping a flavor from selecting them", k)
		}
	}
	// The selector still works, so this is not passing by returning nothing.
	if selector[core.LabelOSStable] != "linux" {
		t.Fatalf("selector = %v, want it to still carry the pool discriminators", selector)
	}
}

// TestRDMALabelValuesAreValidBeforeSanitizing pins the general rule the NUMA separator taught.
//
// Every label value in this package goes through SanitizeLabelValue, and for a SCALAR that is a
// safety net. For a COMPOSITE value it is a hazard: the sanitizer drops characters it does not
// allow without complaining, so a comma-joined set of NUMA nodes publishes as "01" — a value that
// looks valid and means a different node. The rule is that the value must already be valid, which
// makes the sanitizer a no-op, and this asserts exactly that rather than asserting one separator.
func TestRDMALabelValuesAreValidBeforeSanitizing(t *testing.T) {
	labels := ConstructRDMANodeLabels(
		acceleratorAt("0000:01:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"}),
		[]device.Interface{
			rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "0", []string{"0000:00:01.0"},
				linkState(workercore.DeviceInterfaceLinkStateOK)),
			rdmaNIC("eth1", "0000:81:00.0", "0000:80:01.0", "1", []string{"0000:80:01.0"},
				linkState(workercore.DeviceInterfaceLinkStateOK)),
			rdmaNIC("eth2", "0000:82:00.0", "0000:80:02.0", "12", []string{"0000:80:02.0"},
				linkState(workercore.DeviceInterfaceLinkStateOK)),
		})

	if len(labels) == 0 {
		t.Fatal("the fixture produced no labels, so this asserts nothing")
	}
	for k, v := range labels {
		if sanitized := kubemeta.SanitizeLabelValue(v); sanitized != v {
			t.Errorf("label %s = %q, which the sanitizer rewrites to %q; a composite value has "+
				"to be valid BEFORE sanitizing, or its structure is silently destroyed",
				k, v, sanitized)
		}
	}
	// And the multi-node set really is multi-node, so the assertion above is not passing on a
	// single-valued fixture.
	if got := labels[NodeRDMANumaLabelKey]; got != "0_1_12" {
		t.Errorf("numa = %q, want three nodes joined", got)
	}
}

// TestRDMANumaNodesIsValidBeforeSanitizing asserts the joined value at the point it is decided.
//
// Asserting it on the returned label cannot work, and that is the finding this test exists for: an
// interface with no NUMA affinity that reached the set would produce a LEADING separator, and the
// sanitizer every label value passes through strips exactly that — so the published label is
// identical either way, and the guard that keeps the empty member out has no observable effect
// downstream of it.
//
// Removing the guard on that basis would leave the value's structure to the sanitizer, which is the
// one thing the sibling test above establishes must not be relied on. So the contract is asserted
// where it holds: the joined value must already be a valid label value, with no leading, trailing
// or doubled separator, before anything sanitizes it.
func TestRDMANumaNodesIsValidBeforeSanitizing(t *testing.T) {
	capable := []device.Interface{
		rdmaNIC("eth0", "0000:02:00.0", "0000:00:01.0", "", nil, nil),
		rdmaNIC("eth1", "0000:81:00.0", "0000:80:01.0", "1", nil, nil),
		rdmaNIC("eth2", "0000:82:00.0", "0000:80:02.0", "0", nil, nil),
		rdmaNIC("eth3", "0000:83:00.0", "0000:80:03.0", "", nil, nil),
	}

	got := rdmaNumaNodes(capable)
	if got != "0"+rdmaNumaSeparator+"1" {
		t.Errorf("numa nodes = %q, want the two nodes that were read; an interface whose NUMA "+
			"affinity the kernel did not give must not become a member", got)
	}
	if sanitized := kubemeta.SanitizeLabelValue(got); sanitized != got {
		t.Errorf("numa nodes = %q, which the sanitizer rewrites to %q; the value has to be a "+
			"valid label value before it is sanitized, not after", got, sanitized)
	}
}
