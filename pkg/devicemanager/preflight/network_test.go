package preflight

import (
	"bytes"
	"errors"
	"strings"
	"testing"
	"time"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/device"
)

func rdmaIface(name, rdmaDevice string, link *workercore.DeviceInterfaceLink) workercore.DeviceInterface {
	return workercore.DeviceInterface{
		Name: name, Bus: "pci", RDMA: true, RDMADevice: rdmaDevice, Link: link,
	}
}

// pfWithVFs builds a physical function carrying no RDMA of its own, which is the SR-IOV shape: the
// PF is a plain NIC and the RDMA devices handed to containers are its virtual functions.
func pfWithVFs(
	name string, vfs ...workercore.DeviceInterfaceVirtualFunction,
) workercore.DeviceInterface {
	return workercore.DeviceInterface{Name: name, Bus: "pci", VirtualFunctions: vfs}
}

func rdmaVF(
	busID, rdmaDevice string, link *workercore.DeviceInterfaceLink,
) workercore.DeviceInterfaceVirtualFunction {
	return workercore.DeviceInterfaceVirtualFunction{
		Name: "", PciBusID: busID, RDMA: true, RDMADevice: rdmaDevice, Link: link,
	}
}

// TestNetworkReport pins what the preflight says about this node's RDMA links.
//
// The rule it exists for is the preflight spec's own: an empty result reads as a node that passed.
// Every way of having nothing to report — the enumeration failed, there are interfaces but none is
// RDMA-capable — has to say so in words rather than produce no rows.
func TestNetworkReport(t *testing.T) {
	now := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)

	testCases := []struct {
		name       string
		interfaces []workercore.DeviceInterface
		err        error
		wantChecks []NetworkCheck
		// wantNote is a substring; empty means the report must carry no note.
		wantNote string
	}{
		{
			name: "one row per RDMA-capable interface, carrying the link verdict",
			interfaces: []workercore.DeviceInterface{
				rdmaIface("eth0", "mlx5_0", &workercore.DeviceInterfaceLink{
					State: workercore.DeviceInterfaceLinkStateOK,
				}),
				rdmaIface("eth1", "mlx5_1", &workercore.DeviceInterfaceLink{
					State:  workercore.DeviceInterfaceLinkStateFailed,
					Reason: `port 1: state="1: DOWN" phys_state="3: Disabled"`,
				}),
			},
			wantChecks: []NetworkCheck{
				{
					Interface: "eth0", RDMADevice: "mlx5_0",
					State: workercore.DeviceInterfaceLinkStateOK,
					Depth: device.PreflightDepthDeclared,
				},
				{
					Interface: "eth1", RDMADevice: "mlx5_1",
					State:  workercore.DeviceInterfaceLinkStateFailed,
					Depth:  device.PreflightDepthDeclared,
					Reason: `port 1: state="1: DOWN" phys_state="3: Disabled"`,
				},
			},
		},
		{
			// An interface with no RDMA device gets no row. `rdma: false` already carries that
			// fact, and a verdict here would be a verdict on a check that was never in question.
			name: "a NIC with no RDMA device produces no row, and the absence is explained",
			interfaces: []workercore.DeviceInterface{
				{Name: "eth0", Bus: "pci"},
				{Name: "lo", Bus: "virtual", Virtual: true},
			},
			wantChecks: nil,
			wantNote:   "none of the 2 interfaces",
		},
		{
			// A failed enumeration is not a node without RDMA. Reporting no rows and no note here
			// would be the exact shape the preflight spec forbids.
			name:       "a failed enumeration says so instead of reporting nothing",
			err:        errors.New("the network interface inventory is only available on linux"),
			wantChecks: nil,
			wantNote:   "only available on linux",
		},
		{
			// The one case where `rdma: false` does NOT mean the question was settled: the RDMA
			// tree is present and could not be read, so the detector records a verdict without
			// claiming a device. Dropping the row on `!RDMA` alone discarded exactly this
			// diagnostic, on the node where it is hardest to obtain any other way -- the chain
			// that would publish the node's record is not up yet, which is why preflight exists.
			name: "an interface whose RDMA tree could not be read still gets a row",
			interfaces: []workercore.DeviceInterface{{
				Name: "eth0", Bus: "pci",
				Link: &workercore.DeviceInterfaceLink{
					State:  workercore.DeviceInterfaceLinkStateUnverified,
					Reason: "the RDMA tree is present but could not be read; tried a, b",
				},
			}},
			wantChecks: []NetworkCheck{{
				Interface: "eth0",
				State:     workercore.DeviceInterfaceLinkStateUnverified,
				Depth:     device.PreflightDepthDeclared,
				Reason:    "the RDMA tree is present but could not be read; tried a, b",
			}},
		},
		{
			// The contract is over the parameter, not over what today's producer happens to emit:
			// an RDMA-bound interface with no link record must not be dropped from the document.
			name:       "an RDMA interface with no link record is unverified, not omitted",
			interfaces: []workercore.DeviceInterface{rdmaIface("eth0", "mlx5_0", nil)},
			wantChecks: []NetworkCheck{{
				Interface: "eth0", RDMADevice: "mlx5_0",
				State:  workercore.DeviceInterfaceLinkStateUnverified,
				Depth:  device.PreflightDepthDeclared,
				Reason: "the inventory carries no link state for this interface",
			}},
		},
		{
			// The SR-IOV node, and the case whose absence made this section contradict the node's
			// own labels: every RDMA device here is a virtual function, the PF carries none, and a
			// pass that read only the top level reported that no RDMA device is bound while
			// ConstructRDMANodeLabels -- reading the same inventory -- published `rdma.capable`.
			name: "a node whose only RDMA is on a virtual function still gets a row",
			interfaces: []workercore.DeviceInterface{
				pfWithVFs("eth0", rdmaVF("0000:41:00.2", "mlx5_2",
					&workercore.DeviceInterfaceLink{State: workercore.DeviceInterfaceLinkStateOK})),
			},
			wantChecks: []NetworkCheck{{
				Interface: "eth0/0000:41:00.2", RDMADevice: "mlx5_2",
				State: workercore.DeviceInterfaceLinkStateOK,
				Depth: device.PreflightDepthDeclared,
			}},
		},
		{
			// A PF that is itself RDMA-bound and also hands out functions: both are rows, and the
			// PF's comes first so the list reads as the inventory is shaped.
			name: "a physical function and its virtual function are separate rows",
			interfaces: []workercore.DeviceInterface{
				func() workercore.DeviceInterface {
					iface := rdmaIface("eth0", "mlx5_0", &workercore.DeviceInterfaceLink{
						State: workercore.DeviceInterfaceLinkStateOK,
					})
					iface.VirtualFunctions = []workercore.DeviceInterfaceVirtualFunction{
						rdmaVF("0000:41:00.2", "mlx5_2", &workercore.DeviceInterfaceLink{
							State:  workercore.DeviceInterfaceLinkStateFailed,
							Reason: `port 1: state="1: DOWN" phys_state="3: Disabled"`,
						}),
					}
					return iface
				}(),
			},
			wantChecks: []NetworkCheck{
				{
					Interface: "eth0", RDMADevice: "mlx5_0",
					State: workercore.DeviceInterfaceLinkStateOK,
					Depth: device.PreflightDepthDeclared,
				},
				{
					Interface: "eth0/0000:41:00.2", RDMADevice: "mlx5_2",
					State:  workercore.DeviceInterfaceLinkStateFailed,
					Depth:  device.PreflightDepthDeclared,
					Reason: `port 1: state="1: DOWN" phys_state="3: Disabled"`,
				},
			},
		},
		{
			// The negative control for the two cases above. A virtual function is not a row because
			// it is a virtual function — it is a row on the same terms as a NIC, and an SR-IOV NIC
			// handing out plain ethernet functions has nothing for this section to say. Without
			// this case, "emit a row for every VF" passes the two above and floods the document on
			// every SR-IOV node in the fleet.
			name: "virtual functions with no RDMA produce no rows",
			interfaces: []workercore.DeviceInterface{
				pfWithVFs("eth0",
					workercore.DeviceInterfaceVirtualFunction{PciBusID: "0000:41:00.2"},
					workercore.DeviceInterfaceVirtualFunction{PciBusID: "0000:41:00.3"}),
			},
			wantChecks: nil,
			wantNote:   "nor any of their 2 virtual functions",
		},
		{
			// The unreadable-tree diagnostic at the VF level: a verdict with no RDMA device claimed.
			// The top level has this case already, and a VF reaching it through a different branch
			// of the same reader is what makes it worth its own row rather than an assumption.
			name: "a virtual function whose RDMA tree could not be read still gets a row",
			interfaces: []workercore.DeviceInterface{
				pfWithVFs("eth0", workercore.DeviceInterfaceVirtualFunction{
					PciBusID: "0000:41:00.2",
					Link: &workercore.DeviceInterfaceLink{
						State:  workercore.DeviceInterfaceLinkStateUnverified,
						Reason: "the RDMA tree is present but could not be read; tried a, b",
					},
				}),
			},
			wantChecks: []NetworkCheck{{
				Interface: "eth0/0000:41:00.2",
				State:     workercore.DeviceInterfaceLinkStateUnverified,
				Depth:     device.PreflightDepthDeclared,
				Reason:    "the RDMA tree is present but could not be read; tried a, b",
			}},
		},
		{
			// And the no-verdict case at the VF level, for the same reason the top-level one exists:
			// the contract is over the parameter, not over what today's producer happens to emit.
			name: "an RDMA virtual function with no link record is unverified, not omitted",
			interfaces: []workercore.DeviceInterface{
				pfWithVFs("eth0", rdmaVF("0000:41:00.2", "mlx5_2", nil)),
			},
			wantChecks: []NetworkCheck{{
				Interface: "eth0/0000:41:00.2", RDMADevice: "mlx5_2",
				State:  workercore.DeviceInterfaceLinkStateUnverified,
				Depth:  device.PreflightDepthDeclared,
				Reason: "the inventory carries no link state for this interface",
			}},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := networkReport(tc.interfaces, tc.err, now)

			if !got.Timestamp.Equal(now) {
				t.Errorf("timestamp = %v, want %v; a preflight reads mutable host state, so the "+
					"reading is only worth what its time claims", got.Timestamp, now)
			}
			if len(got.Checks) != len(tc.wantChecks) {
				t.Fatalf("checks = %+v, want %+v", got.Checks, tc.wantChecks)
			}
			for i := range tc.wantChecks {
				if got.Checks[i] != tc.wantChecks[i] {
					t.Errorf("check %d = %+v, want %+v", i, got.Checks[i], tc.wantChecks[i])
				}
			}
			switch {
			case tc.wantNote == "" && got.Note != "":
				t.Errorf("note = %q, want none; rows are their own account", got.Note)
			case tc.wantNote != "" && !strings.Contains(got.Note, tc.wantNote):
				t.Errorf("note = %q, want it to carry %q", got.Note, tc.wantNote)
			}
		})
	}
}

// TestReportDoesNotFailOnABrokenLink pins that a link row is diagnostic, not a verdict.
//
// The error Report returns is what a script turns into an exit code, and what it answers is whether
// this node can serve the allocation modes its allocators offer. A down RDMA link stops none of
// them — it withholds a node label, which changes what a flavor selects, not what an allocator can
// hand out. Were a link row wired into that error, every script gating an install on `preflight`
// would start refusing nodes that allocate perfectly well.
func TestReportDoesNotFailOnABrokenLink(t *testing.T) {
	network := networkReport([]workercore.DeviceInterface{
		rdmaIface("eth0", "mlx5_0", &workercore.DeviceInterfaceLink{
			State:  workercore.DeviceInterfaceLinkStateFailed,
			Reason: `port 1: state="1: DOWN" phys_state="3: Disabled"`,
		}),
	}, nil, time.Now())

	var buf bytes.Buffer
	err := Report(&buf, device.PreflightGroupList{{
		Manufacturer: "nvidia",
		Detection:    device.PreflightDetection{State: device.PreflightStateOK, Accelerators: 1},
	}}, network)
	if err != nil {
		t.Fatalf("a broken RDMA link failed the pass: %v", err)
	}

	// And it is in the document, which is what makes the nil error a decision rather than an
	// omission: a report that dropped the row would pass this too.
	if out := buf.String(); !strings.Contains(out, "mlx5_0") ||
		!strings.Contains(out, string(workercore.DeviceInterfaceLinkStateFailed)) {
		t.Errorf("the document does not carry the failed link:\n%s", out)
	}
}
