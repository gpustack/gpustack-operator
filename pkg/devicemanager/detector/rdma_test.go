package detector

import (
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"gpustack.ai/gpustack/pkg/device"
)

// rdmaNetdevPlant plants one physical NIC the interface pass would also see.
type rdmaNetdevPlant struct {
	name string
	bdf  string
}

// rdmaPCIPlant plants one PCI device with no interface on it.
type rdmaPCIPlant struct {
	bdf   string
	attrs map[string]string
}

// rdmaVFPlant plants one virtual function under its physical function.
type rdmaVFPlant struct {
	pf    string
	bdf   string
	index int
}

// rdmaPortPlant describes one port: text attributes, bounded entry tables, and the netdev
// associations the kernel writes as the entries' text.
type rdmaPortPlant struct {
	number int
	attrs  map[string]string
	// dirAttrs are port-relative paths planted as directories, so reading them fails.
	dirAttrs []string
	gids     []string
	pkeys    []string
	ndevs    []string
}

// rdmaDevicePlant describes one class/infiniband entry. An empty bdf hangs it under
// devices/virtual, the software-RDMA shape whose records carry no PCI fields.
type rdmaDevicePlant struct {
	name         string
	bdf          string
	driver       string
	nodeGuid     string
	sysImageGuid string
	// dirAttrs are device-relative paths planted as directories, so reading them fails.
	dirAttrs []string
	ports    []rdmaPortPlant
}

// rdmaFixturePlant is everything one case plants before enumerating. Files and symlinks reach the
// shapes the typed plants cannot express: a directory that is a regular file, a link that escapes
// the root, a link that dangles.
type rdmaFixturePlant struct {
	netdevs  []rdmaNetdevPlant
	pciDevs  []rdmaPCIPlant
	vfs      []rdmaVFPlant
	devices  []rdmaDevicePlant
	files    map[string]string
	symlinks map[string]string
}

func (p rdmaFixturePlant) build(t *testing.T, f *sysfsFixture) {
	t.Helper()
	for _, n := range p.netdevs {
		f.physicalNIC(n.name, n.bdf)
	}
	for _, d := range p.pciDevs {
		f.addPCIDevice(d.bdf, "pci0000:00", nil, d.attrs)
	}
	for _, v := range p.vfs {
		f.addVF(v.pf, v.bdf, v.index)
	}
	for rel, content := range p.files {
		f.write(rel, content)
	}
	for rel, target := range p.symlinks {
		f.symlink(target, rel)
	}
	for _, d := range p.devices {
		p.buildDevice(t, f, d)
	}
}

func (p rdmaFixturePlant) buildDevice(t *testing.T, f *sysfsFixture, d rdmaDevicePlant) {
	t.Helper()

	rel := f.addClassRDMA(d.name, d.bdf, d.driver)
	for _, name := range d.dirAttrs {
		f.mkdir(filepath.Join(rel, name))
	}
	if d.nodeGuid != "" {
		f.write(filepath.Join(rel, "node_guid"), d.nodeGuid)
	}
	if d.sysImageGuid != "" {
		f.write(filepath.Join(rel, "sys_image_guid"), d.sysImageGuid)
	}
	for _, port := range d.ports {
		p.buildPort(t, f, rel, port)
	}
}

func (p rdmaFixturePlant) buildPort(t *testing.T, f *sysfsFixture, rdmaRel string, port rdmaPortPlant) {
	t.Helper()

	portRel := filepath.Join(rdmaRel, "ports", strconv.Itoa(port.number))
	f.mkdir(portRel)
	for k, v := range port.attrs {
		f.write(filepath.Join(portRel, k), v)
	}
	for _, name := range port.dirAttrs {
		f.mkdir(filepath.Join(portRel, name))
	}
	for i, g := range port.gids {
		f.write(filepath.Join(portRel, "gids", strconv.Itoa(i)), g)
	}
	for i, k := range port.pkeys {
		f.write(filepath.Join(portRel, "pkeys", strconv.Itoa(i)), k)
	}
	for i, nd := range port.ndevs {
		// The kernel writes the netdev's name as the entry's text, not as a link.
		f.write(filepath.Join(portRel, "gid_attrs", "ndevs", strconv.Itoa(i)), nd)
	}
}

// rdmaGIDs returns n distinct GID values.
func rdmaGIDs(n int) []string {
	gids := make([]string, n)
	for i := range gids {
		gids[i] = fmt.Sprintf("fe80::%x", i)
	}
	return gids
}

// addClassRDMA plants one class/infiniband entry. With a bdf the entry hangs under that PCI
// device — the layout the walk resolves the parent coordinates from — and optionally binds a
// driver to the parent; with none it hangs under devices/virtual, the software-RDMA shape whose
// records carry no PCI fields.
func (f *sysfsFixture) addClassRDMA(name, bdf, driver string) string {
	f.t.Helper()

	var rdmaRel string
	if bdf != "" {
		devRel, ok := f.devPaths[bdf]
		if !ok {
			f.t.Fatalf("addClassRDMA(%s): PCI device %s was never added", name, bdf)
		}
		rdmaRel = filepath.Join(devRel, "infiniband", name)
		f.mkdir(rdmaRel)
		f.symlink(devRel, filepath.Join(rdmaRel, "device"))
		if driver != "" {
			f.mkdir(filepath.Join("bus", "pci", "drivers", driver))
			f.symlink(filepath.Join("bus", "pci", "drivers", driver), filepath.Join(devRel, "driver"))
		}
	} else {
		rdmaRel = filepath.Join("devices", "virtual", "infiniband", name)
		f.mkdir(rdmaRel)
	}
	f.symlink(rdmaRel, filepath.Join("class", "infiniband", name))
	return rdmaRel
}

// TestEnumerateRDMADevicesShape covers the record each kind of verbs device produces. Each case
// plants a fixture declaratively and states the whole expected inventory; the shared runner
// enumerates once and compares, and any device without declared notes must carry none.
func TestEnumerateRDMADevicesShape(t *testing.T) {
	boundGIDs := rdmaGIDs(256)
	allGIDs := rdmaGIDs(300)
	sparseGIDs := make([]string, 300)
	sparseWant := make([]string, 0, 200)
	for i := range sparseGIDs {
		if i%3 == 2 {
			continue
		}
		sparseGIDs[i] = allGIDs[i]
	}
	for i := 0; i < 256; i++ {
		if i%3 != 2 {
			sparseWant = append(sparseWant, allGIDs[i])
		}
	}
	pairedNDevs := make([]string, 300)
	for i := range pairedNDevs {
		pairedNDevs[i] = fmt.Sprintf("eth%d", i%2)
	}

	testCases := []struct {
		name  string
		plant rdmaFixturePlant
		want  []device.RDMADevice
		// notes maps a device name to substrings its Message must carry; every other
		// device's Message must be empty.
		notes map[string][]string
	}{
		{
			name: "PCI verbs device with RoCE port and netdev association",
			plant: rdmaFixturePlant{
				netdevs: []rdmaNetdevPlant{{name: "eth0", bdf: "0000:01:00.0"}},
				devices: []rdmaDevicePlant{{
					name: "mlx5_0", bdf: "0000:01:00.0", driver: "mlx5_core",
					nodeGuid:     "0c:c9:a0:00:00:00:00:00",
					sysImageGuid: "0c:c9:a0:00:00:00:00:01",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs: map[string]string{
							"link_layer": "Ethernet",
							"state":      "4: ACTIVE",
							"phys_state": "5: LinkUp",
							"rate":       "200 Gb/sec (2X, NRZ)",
						},
						gids:  []string{"fe80::1"},
						pkeys: []string{"0xffff"},
						ndevs: []string{"eth0"},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:           "mlx5_0",
				AllocationUnit: "0000:01:00.0",
				Owner:          device.RDMADeviceOwnerGPUStack,
				Driver:         "mlx5_core",
				PciBusID:       "0000:01:00.0",
				NodeGuid:       "0c:c9:a0:00:00:00:00:00",
				SysImageGuid:   "0c:c9:a0:00:00:00:00:01",
				NumaAffinity:   "0",
				CpuAffinity:    "0-15",
				Ports: []device.RDMAPort{{
					Number:        1,
					NetDevs:       []string{"eth0"},
					LinkLayer:     "Ethernet",
					State:         "ACTIVE",
					PhysicalState: "LinkUp",
					Rate:          "200 Gb/sec (2X, NRZ)",
					GIDs:          []string{"fe80::1"},
					PKeys:         []string{"0xffff"},
				}},
			}},
		},
		{
			name: "native InfiniBand without IPoIB has no netdev association",
			plant: rdmaFixturePlant{
				pciDevs: []rdmaPCIPlant{{bdf: "0000:02:00.0", attrs: map[string]string{"numa_node": "1"}}},
				devices: []rdmaDevicePlant{{
					name: "hns_0", bdf: "0000:02:00.0", driver: "hns_roce_hw_v2",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs: map[string]string{
							"link_layer": "InfiniBand",
							"state":      "4: ACTIVE",
							"lid":        "0x1a",
							"sm_lid":     "0x2b",
						},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:           "hns_0",
				AllocationUnit: "0000:02:00.0",
				Owner:          device.RDMADeviceOwnerGPUStack,
				Driver:         "hns_roce_hw_v2",
				PciBusID:       "0000:02:00.0",
				NumaAffinity:   "1",
				Ports: []device.RDMAPort{{
					Number:    1,
					LinkLayer: "InfiniBand",
					State:     "ACTIVE",
					LID:       "0x1a",
					SMLID:     "0x2b",
				}},
			}},
		},
		{
			name: "RoCE port without IB fields reports them absent",
			plant: rdmaFixturePlant{
				pciDevs: []rdmaPCIPlant{{bdf: "0000:03:00.0"}},
				devices: []rdmaDevicePlant{{
					name: "roce_0", bdf: "0000:03:00.0", driver: "irdma",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs:  map[string]string{"link_layer": "Ethernet"},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:           "roce_0",
				AllocationUnit: "0000:03:00.0",
				Owner:          device.RDMADeviceOwnerGPUStack,
				Driver:         "irdma",
				PciBusID:       "0000:03:00.0",
				Ports: []device.RDMAPort{{
					Number:    1,
					LinkLayer: "Ethernet",
				}},
			}},
		},
		{
			name: "two ports with aliased and duplicated associations",
			plant: rdmaFixturePlant{
				netdevs: []rdmaNetdevPlant{
					{name: "bond0", bdf: "0000:04:00.0"},
					{name: "eth9", bdf: "0000:04:00.1"},
				},
				devices: []rdmaDevicePlant{{
					name: "mlx5_1", bdf: "0000:04:00.0", driver: "mlx5_core",
					ports: []rdmaPortPlant{
						{
							number: 1,
							attrs:  map[string]string{"link_layer": "Ethernet"},
							ndevs:  []string{"bond0", "eth9", "bond0"},
						},
						{
							number: 2,
							attrs:  map[string]string{"link_layer": "Ethernet"},
							ndevs:  []string{"eth9"},
						},
					},
				}},
			},
			want: []device.RDMADevice{{
				Name:           "mlx5_1",
				AllocationUnit: "0000:04:00.0",
				Owner:          device.RDMADeviceOwnerGPUStack,
				Driver:         "mlx5_core",
				PciBusID:       "0000:04:00.0",
				NumaAffinity:   "0",
				CpuAffinity:    "0-15",
				Ports: []device.RDMAPort{
					{Number: 1, NetDevs: []string{"bond0", "eth9"}, LinkLayer: "Ethernet"},
					{Number: 2, NetDevs: []string{"eth9"}, LinkLayer: "Ethernet"},
				},
			}},
		},
		{
			name: "distinct VFs of one physical function",
			plant: rdmaFixturePlant{
				pciDevs: []rdmaPCIPlant{
					{bdf: "0000:05:00.0"}, {bdf: "0000:05:00.2"}, {bdf: "0000:05:00.3"},
				},
				vfs: []rdmaVFPlant{
					{pf: "0000:05:00.0", bdf: "0000:05:00.2", index: 0},
					{pf: "0000:05:00.0", bdf: "0000:05:00.3", index: 1},
				},
				devices: []rdmaDevicePlant{
					{name: "mlx5_2", bdf: "0000:05:00.0", driver: "mlx5_core"},
					{name: "mlx5_6", bdf: "0000:05:00.2", driver: "mlx5_core"},
					{name: "mlx5_7", bdf: "0000:05:00.3", driver: "mlx5_core"},
				},
			},
			want: []device.RDMADevice{
				{
					Name:           "mlx5_2",
					AllocationUnit: "0000:05:00.0",
					Owner:          device.RDMADeviceOwnerGPUStack,
					Driver:         "mlx5_core",
					PciBusID:       "0000:05:00.0",
				},
				{
					Name:            "mlx5_6",
					AllocationUnit:  "0000:05:00.2",
					Owner:           device.RDMADeviceOwnerGPUStack,
					Driver:          "mlx5_core",
					PciBusID:        "0000:05:00.2",
					ParentPciBusID:  "0000:05:00.0",
					VirtualFunction: true,
				},
				{
					Name:            "mlx5_7",
					AllocationUnit:  "0000:05:00.3",
					Owner:           device.RDMADeviceOwnerGPUStack,
					Driver:          "mlx5_core",
					PciBusID:        "0000:05:00.3",
					ParentPciBusID:  "0000:05:00.0",
					VirtualFunction: true,
				},
			},
		},
		{
			name: "two verbs devices sharing one parent stay distinct",
			plant: rdmaFixturePlant{
				pciDevs: []rdmaPCIPlant{{bdf: "0000:06:00.0"}},
				devices: []rdmaDevicePlant{
					{name: "mlx5_8", bdf: "0000:06:00.0", driver: "mlx5_core"},
					{name: "mlx5_9", bdf: "0000:06:00.0"},
				},
			},
			want: []device.RDMADevice{
				{
					Name:           "mlx5_8",
					AllocationUnit: "0000:06:00.0",
					Owner:          device.RDMADeviceOwnerGPUStack,
					Driver:         "mlx5_core",
					PciBusID:       "0000:06:00.0",
				},
				{
					// The driver hangs off the shared parent, so both records read it.
					Name:           "mlx5_9",
					AllocationUnit: "0000:06:00.0",
					Owner:          device.RDMADeviceOwnerGPUStack,
					Driver:         "mlx5_core",
					PciBusID:       "0000:06:00.0",
				},
			},
		},
		{
			name: "software RDMA without a PCI parent is a record, not a gap",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "rxe0",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs:  map[string]string{"link_layer": "Ethernet", "state": "4: ACTIVE"},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "rxe0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{Number: 1, LinkLayer: "Ethernet", State: "ACTIVE"}},
			}},
		},
		{
			name: "EFA ownership judged on driver evidence, never the name",
			plant: rdmaFixturePlant{
				pciDevs: []rdmaPCIPlant{{bdf: "0000:00:ff.0"}},
				devices: []rdmaDevicePlant{
					{name: "efa_0", bdf: "0000:00:ff.0", driver: "efa"},
					{name: "efa_named_0"},
				},
			},
			want: []device.RDMADevice{
				{
					Name:           "efa_0",
					AllocationUnit: "0000:00:ff.0",
					Owner:          device.RDMADeviceOwnerExternal,
					Driver:         "efa",
					PciBusID:       "0000:00:ff.0",
				},
				{Name: "efa_named_0", Owner: device.RDMADeviceOwnerGPUStack},
			},
		},
		{
			name: "oversized GID table truncates with a diagnostic",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "big_0",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs:  map[string]string{"link_layer": "Ethernet"},
						gids:   rdmaGIDs(rdmaPortAttrMaxEntries + 4),
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "big_0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{Number: 1, LinkLayer: "Ethernet", GIDs: boundGIDs}},
			}},
			notes: map[string][]string{
				"big_0": {"port 1 gids truncated at 256 entries"},
			},
		},
		{
			name: "GID bound counts attempted entries, not collected values",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "gap_0",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs:  map[string]string{"link_layer": "Ethernet"},
						gids:   sparseGIDs,
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "gap_0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{Number: 1, LinkLayer: "Ethernet", GIDs: sparseWant}},
			}},
			notes: map[string][]string{
				"gap_0": {"port 1 gids truncated at 256 entries"},
			},
		},
		{
			name: "netdev bound counts attempted entries before deduplication",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "nd_0",
					ports: []rdmaPortPlant{{
						number: 1,
						attrs:  map[string]string{"link_layer": "Ethernet"},
						ndevs:  pairedNDevs,
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "nd_0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{
					Number:    1,
					LinkLayer: "Ethernet",
					NetDevs:   []string{"eth0", "eth1"},
				}},
			}},
			notes: map[string][]string{
				"nd_0": {"port 1 netdevs truncated at 256 entries"},
			},
		},
		{
			name: "unreadable netdev entry is a diagnostic",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "ndx_0",
					ports: []rdmaPortPlant{{
						number:   1,
						attrs:    map[string]string{"link_layer": "Ethernet"},
						dirAttrs: []string{filepath.Join("gid_attrs", "ndevs", "1")},
						ndevs:    []string{"eth0"},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "ndx_0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{
					Number:    1,
					LinkLayer: "Ethernet",
					NetDevs:   []string{"eth0"},
				}},
			}},
			notes: map[string][]string{
				"ndx_0": {"port 1 netdev 1"},
			},
		},
		{
			name: "unreadable port attribute is a diagnostic, not a silent unknown",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name: "attrx_0",
					ports: []rdmaPortPlant{{
						number:   1,
						attrs:    map[string]string{"link_layer": "Ethernet"},
						dirAttrs: []string{"lid", "state"},
					}},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "attrx_0",
				Owner: device.RDMADeviceOwnerGPUStack,
				Ports: []device.RDMAPort{{Number: 1, LinkLayer: "Ethernet"}},
			}},
			notes: map[string][]string{
				"attrx_0": {"port 1 lid", "port 1 state"},
			},
		},
		{
			name: "unreadable device attribute is a diagnostic",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{
					name:     "guidx_0",
					dirAttrs: []string{"node_guid"},
				}},
			},
			want: []device.RDMADevice{{
				Name:  "guidx_0",
				Owner: device.RDMADeviceOwnerGPUStack,
			}},
			notes: map[string][]string{
				"guidx_0": {"node_guid"},
			},
		},
		{
			name:  "class absent enumerates as an empty inventory",
			plant: rdmaFixturePlant{},
			want:  []device.RDMADevice{},
		},
		{
			name: "non-directory class entries are skipped",
			plant: rdmaFixturePlant{
				files:   map[string]string{"class/infiniband/stray": "not a device"},
				devices: []rdmaDevicePlant{{name: "s_0"}},
			},
			want: []device.RDMADevice{{Name: "s_0", Owner: device.RDMADeviceOwnerGPUStack}},
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newSysfsFixture(t)
			tc.plant.build(t, f)
			devices, err := enumerateRDMAInterfaces(f.root)
			require.NoError(t, err)

			// Message content is checked through the declared notes — exact error text belongs
			// to the platform — so it is set aside before the inventories are compared and
			// every device without declared notes must have carried none.
			messages := make(map[string]string, len(devices))
			for i := range devices {
				messages[devices[i].Name] = devices[i].Message
				devices[i].Message = ""
			}
			assert.Equal(t, tc.want, devices)
			for name, notes := range tc.notes {
				msg, ok := messages[name]
				require.True(t, ok, "device %s was not enumerated", name)
				for _, note := range notes {
					assert.Contains(t, msg, note)
				}
			}
			for name, msg := range messages {
				if _, declared := tc.notes[name]; !declared {
					assert.Empty(t, msg, "%s: unexpected diagnostics", name)
				}
			}
		})
	}
}

// TestEnumerateRDMADevicesFailures pins the reads that must END the pass rather than shorten the
// list. An unreadable class is the load-bearing one: only its true absence enumerates as empty,
// and any other stat failure ends the pass, because an empty inventory claims hardware that is
// not there.
func TestEnumerateRDMADevicesFailures(t *testing.T) {
	testCases := []struct {
		name    string
		plant   rdmaFixturePlant
		wantErr string
	}{
		{
			name: "a class that cannot be statted is an error, not an empty inventory",
			plant: rdmaFixturePlant{
				files: map[string]string{"class": "not a directory"},
			},
			wantErr: "class/infiniband",
		},
		{
			name: "unlistable class directory",
			plant: rdmaFixturePlant{
				files: map[string]string{"class/infiniband": "not a directory"},
			},
			wantErr: "list class/infiniband",
		},
		{
			name: "dangling class symlink",
			plant: rdmaFixturePlant{
				symlinks: map[string]string{"class/infiniband": "nowhere"},
			},
			wantErr: "list class/infiniband",
		},
		{
			name: "class entry escaping the root",
			plant: rdmaFixturePlant{
				symlinks: map[string]string{"class/infiniband/escaped": "/etc"},
			},
			wantErr: "outside",
		},
		{
			name: "unlistable ports directory",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{name: "bad_0"}},
				files: map[string]string{
					filepath.Join("devices", "virtual", "infiniband", "bad_0", "ports"): "not a directory",
				},
			},
			wantErr: "list ports of bad_0",
		},
		{
			name: "dangling ports symlink",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{name: "bad_1"}},
				symlinks: map[string]string{
					filepath.Join("devices", "virtual", "infiniband", "bad_1", "ports"): "nowhere",
				},
			},
			wantErr: "list ports of bad_1",
		},
		{
			// The entry was listed, so it existed when the pass ran; only its target is gone. That
			// is not the race a vanished name is — the name is still there and the device may have
			// moved — so the pass ends instead of publishing the remaining entries as the whole
			// inventory.
			name: "a listed class entry whose target is gone is an error, not a silent skip",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{name: "ok_0"}},
				symlinks: map[string]string{
					filepath.Join("class", "infiniband", "dang_0"): "nowhere",
				},
			},
			wantErr: "dang_0",
		},
		{
			// Same boundary one level down: a port the listing saw whose link now dangles must not
			// come back as a portless device reporting success.
			name: "a listed port entry whose target is gone is an error, not a portless success",
			plant: rdmaFixturePlant{
				devices: []rdmaDevicePlant{{name: "dport_0"}},
				symlinks: map[string]string{
					filepath.Join("devices", "virtual", "infiniband", "dport_0", "ports", "1"): "nowhere",
				},
			},
			wantErr: "port 1 of dport_0",
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			f := newSysfsFixture(t)
			tc.plant.build(t, f)
			_, err := enumerateRDMAInterfaces(f.root)
			require.Error(t, err, "a failed necessary read must end the pass with an error")
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestRDMAEntryDirBoundaries pins the answers the necessary-entry check gives, through the one
// helper both the class and the port level use: a name that is gone is the ordinary race and is
// skipped, a name still present whose target cannot be resolved ends the pass, a plain file is
// not a device, and a directory is one. The stat that fails for neither absence nor resolution
// is planted through a plain file where a parent directory belongs, which makes the Lstat fail
// with ENOTDIR — deterministic and free of permission-bit sensitivity.
func TestRDMAEntryDirBoundaries(t *testing.T) {
	f := newSysfsFixture(t)
	f.mkdir(filepath.Join("class", "infiniband", "real_0"))
	f.write(filepath.Join("class", "infiniband", "stray"), "not a device")
	f.symlink("nowhere", filepath.Join("class", "infiniband", "dang_0"))
	f.symlink("/etc", filepath.Join("class", "infiniband", "escaped"))
	f.write(filepath.Join("class", "infiniband", "blocker"), "not a directory")
	underBlocker := filepath.Join("class", "infiniband", "blocker", "ports", "1")

	s, err := newSysfsTree(f.root)
	require.NoError(t, err)

	testCases := []struct {
		name    string
		rel     string
		wantDir bool
		wantErr string
	}{
		{
			name: "a name that is truly gone is skipped, not failed",
			rel:  filepath.Join("class", "infiniband", "vanished_0"),
		},
		{
			name:    "a directory is enumerated",
			rel:     filepath.Join("class", "infiniband", "real_0"),
			wantDir: true,
		},
		{
			name: "a plain file is skipped, not failed",
			rel:  filepath.Join("class", "infiniband", "stray"),
		},
		{
			name:    "a still-listed dangling link is a failed necessary read",
			rel:     filepath.Join("class", "infiniband", "dang_0"),
			wantErr: "unreadable",
		},
		{
			name:    "a still-listed escaping link is a failed necessary read",
			rel:     filepath.Join("class", "infiniband", "escaped"),
			wantErr: "outside",
		},
		{
			name:    "a stat failing for another reason is a failed necessary read",
			rel:     underBlocker,
			wantErr: "not a directory",
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			dir, err := rdmaEntryDir(s, tc.rel)
			if tc.wantErr != "" {
				require.Error(t, err, "a failed necessary read must end the pass")
				assert.Contains(t, err.Error(), tc.wantErr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.wantDir, dir)
		})
	}
}

// TestReadRDMAParentPhysfnStatFailureIsDiagnosed pins the physfn boundary: a stat that fails for
// any reason other than true absence leaves the relation unknown WITH a diagnostic, never as a
// silent "no virtual-function relation" negative fact. The parent walk is called directly with
// the path a successful device-link resolution would have produced; the fixture plants a regular
// file where the PCI device directory belongs, which makes every child stat fail with ENOTDIR —
// deterministic and free of permission-bit sensitivity.
func TestReadRDMAParentPhysfnStatFailureIsDiagnosed(t *testing.T) {
	f := newSysfsFixture(t)
	f.write(filepath.Join("devices", "pci0000:00", "0000:08:00.0"), "not a device directory")

	s, err := newSysfsTree(f.root)
	require.NoError(t, err)
	resolved := filepath.Join(s.realRoot, "devices", "pci0000:00", "0000:08:00.0")
	dev := &device.RDMADevice{Name: "vfcheck_0"}
	var notes []string
	readRDMAParent(s, resolved, dev, &notes)

	assert.False(t, dev.VirtualFunction, "the relation is unknown, never claimed either way")
	assert.Equal(t, "0000:08:00.0", dev.PciBusID,
		"the coordinate already in hand is kept, not withdrawn with the unreadable facts")
	diagnosed := false
	for _, note := range notes {
		diagnosed = diagnosed || strings.Contains(note, "physfn")
	}
	assert.True(t, diagnosed, "the physfn stat failure must be named in the diagnostics: %v", notes)
}

// TestRDMAInventoryChanged pins the monitor trigger: an RDMA-class change takes a detect round on
// its own, a stable failure never spins, and recovery from incompleteness is the first successful
// read — with the revision, not any timestamp or diagnostic, deciding after that. A
// diagnostic-only change publishes through the monitor's periodic full detect pass, which is the
// bounded refresh this table deliberately does not duplicate.
func TestRDMAInventoryChanged(t *testing.T) {
	// complete builds the stored state a successful refreshRDMAInterfaces produces: the devices travel
	// with the revision digested over them, so the comparison always sees both.
	complete := func(stored []device.RDMADevice) *device.RDMASnapshot {
		return &device.RDMASnapshot{
			Complete: true,
			Revision: device.RDMASnapshotRevision(stored),
			Devices:  stored,
		}
	}
	incomplete := func(revision string) *device.RDMASnapshot {
		return &device.RDMASnapshot{Complete: false, Revision: revision}
	}
	devices := []device.RDMADevice{{Name: "mlx5_0"}}

	testCases := []struct {
		name    string
		current *device.RDMASnapshot
		devices []device.RDMADevice
		err     error
		want    bool
	}{
		{name: "no observation yet, successful read", current: nil, devices: devices, want: true},
		{name: "no observation yet, failed read", current: nil, err: assert.AnError, want: false},
		{
			name:    "unchanged revision",
			current: complete(devices),
			devices: devices,
			want:    false,
		},
		{
			name:    "changed revision",
			current: complete([]device.RDMADevice{{Name: "mlx5_1"}}),
			devices: devices,
			want:    true,
		},
		{
			name:    "recovery from incomplete",
			current: incomplete("other"),
			devices: devices,
			want:    true,
		},
		{
			name:    "failure over complete marks incompleteness once",
			current: complete(devices),
			err:     assert.AnError,
			want:    true,
		},
		{
			name:    "failure over incomplete does not spin",
			current: incomplete("rev"),
			err:     assert.AnError,
			want:    false,
		},
		{
			name:    "empty successful inventory over a populated record",
			current: complete(devices),
			devices: []device.RDMADevice{},
			want:    true,
		},
	}
	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, rdmaInterfacesChanged(tc.current, tc.devices, tc.err))
		})
	}
}

// TestRefreshRDMAAndReadIsolation pins the local snapshot's state machine: initial unknown stays
// unknown on a first failure, a failure retains the last successful observation behind an
// incomplete mark, a success replaces it whole with a fresh time, and neither the caller's input
// nor a returned value reaches the stored state without a copy.
func TestRefreshRDMAAndReadIsolation(t *testing.T) {
	now := time.Now()
	later := now.Add(time.Minute)
	// devices is populated through the nested shapes a port carries, so every state below is
	// pinned against content a shallow list-length check would miss.
	devices := []device.RDMADevice{{
		Name:           "mlx5_0",
		AllocationUnit: "0000:01:00.0",
		Owner:          device.RDMADeviceOwnerGPUStack,
		Driver:         "mlx5_core",
		PciBusID:       "0000:01:00.0",
		NumaAffinity:   "0",
		CpuAffinity:    "0-15",
		Ports: []device.RDMAPort{{
			Number:    1,
			NetDevs:   []string{"eth0", "eth1"},
			LinkLayer: "Ethernet",
			State:     "ACTIVE",
			GIDs:      []string{"fe80::1", "fe80::2"},
			PKeys:     []string{"0xffff", "0x0001"},
		}, {
			Number:  2,
			NetDevs: []string{"eth9"},
		}},
	}}

	t.Run("initial failure stays unknown", func(t *testing.T) {
		d := &Detector{}
		assert.Nil(t, d.refreshRDMAInterfaces(nil, assert.AnError, now))
		assert.Nil(t, d.RDMAInterfaceSnapshot())
	})

	t.Run("initial success publishes complete", func(t *testing.T) {
		d := &Detector{}
		got := d.refreshRDMAInterfaces(devices, nil, now)
		require.NotNil(t, got)
		assert.True(t, got.Complete)
		assert.Equal(t, device.RDMASnapshotRevision(devices), got.Revision)
		assert.NotEmpty(t, got.Revision)
		assert.Equal(t, now, got.LastSuccessfulTime)
		assert.Empty(t, got.Message)
	})

	t.Run("failure retains and marks incomplete", func(t *testing.T) {
		d := &Detector{}
		d.refreshRDMAInterfaces(devices, nil, now)
		got := d.refreshRDMAInterfaces(nil, assert.AnError, later)
		require.NotNil(t, got)
		assert.False(t, got.Complete)
		assert.Equal(t, devices, got.Devices,
			"the retained devices travel through the failure whole, nested ports included")
		assert.Equal(t, device.RDMASnapshotRevision(devices), got.Revision,
			"the retained revision travels with the retained devices")
		assert.Equal(t, now, got.LastSuccessfulTime,
			"a failed pass does not move the successful time")
		assert.NotEmpty(t, got.Message)
	})

	t.Run("successful empty replaces the retained inventory", func(t *testing.T) {
		d := &Detector{}
		d.refreshRDMAInterfaces(devices, nil, now)
		d.refreshRDMAInterfaces(nil, assert.AnError, now)
		got := d.refreshRDMAInterfaces([]device.RDMADevice{}, nil, later)
		require.NotNil(t, got)
		assert.True(t, got.Complete)
		assert.NotNil(t, got.Devices, "an empty inventory is a list, not an absent one")
		assert.Empty(t, got.Devices, "the successful pass replaces the retained devices")
		assert.Equal(t, device.RDMASnapshotRevision([]device.RDMADevice{}), got.Revision)
		assert.NotEmpty(t, got.Revision, "the empty inventory still digests to a nonempty revision")
		assert.Equal(t, later, got.LastSuccessfulTime)
		assert.Empty(t, got.Message)
	})

	t.Run("recovery replaces whole with a fresh time", func(t *testing.T) {
		d := &Detector{}
		d.refreshRDMAInterfaces(devices, nil, now)
		d.refreshRDMAInterfaces(nil, assert.AnError, now)
		fresh := []device.RDMADevice{{Name: "mlx5_1"}}
		got := d.refreshRDMAInterfaces(fresh, nil, later)
		require.NotNil(t, got)
		assert.True(t, got.Complete)
		assert.Empty(t, got.Message)
		assert.Equal(t, device.RDMASnapshotRevision(fresh), got.Revision)
		assert.Equal(t, later, got.LastSuccessfulTime)
	})

	t.Run("stored state does not alias the caller's input", func(t *testing.T) {
		d := &Detector{}
		input := []device.RDMADevice{{
			Name:  "mlx5_0",
			Ports: []device.RDMAPort{{Number: 1, NetDevs: []string{"eth0"}}},
		}}
		d.refreshRDMAInterfaces(input, nil, now)
		input[0].Name = "mutated"
		input[0].Ports[0].NetDevs[0] = "mutated"

		stored := d.RDMAInterfaceSnapshot()
		require.NotNil(t, stored)
		assert.Equal(t, "mlx5_0", stored.Devices[0].Name)
		assert.Equal(t, []string{"eth0"}, stored.Devices[0].Ports[0].NetDevs)
	})

	t.Run("the returned snapshot is not the stored one", func(t *testing.T) {
		d := &Detector{}
		got := d.refreshRDMAInterfaces(devices, nil, now)
		require.NotNil(t, got)
		got.Devices[0].Name = "mutated"
		got.Complete = false

		stored := d.RDMAInterfaceSnapshot()
		require.NotNil(t, stored)
		assert.Equal(t, "mlx5_0", stored.Devices[0].Name)
		assert.True(t, stored.Complete)
	})

	t.Run("reads are copy-isolated", func(t *testing.T) {
		d := &Detector{}
		d.refreshRDMAInterfaces(devices, nil, now)

		first := d.RDMAInterfaceSnapshot()
		require.NotNil(t, first)
		second := d.RDMAInterfaceSnapshot()
		require.NotNil(t, second)
		assert.NotSame(t, first, second, "each reader gets its own copy")

		first.Devices[0].Name = "mutated"
		first.Devices[0].Ports[0].NetDevs[0] = "mutated"
		first.Devices[0].Ports[0].GIDs[0] = "mutated"
		first.Complete = false
		stored := d.RDMAInterfaceSnapshot()
		assert.Equal(t, "mlx5_0", stored.Devices[0].Name)
		assert.Equal(t, "eth0", stored.Devices[0].Ports[0].NetDevs[0])
		assert.Equal(t, "fe80::1", stored.Devices[0].Ports[0].GIDs[0])
		assert.True(t, stored.Complete)
	})
}

// TestRefreshRDMAStateSequences folds declarative observation sequences through one runner and
// pins the whole final snapshot each sequence must leave behind: which devices, ports and
// revision a failure retains, what a successful empty refresh replaces them with, and when the
// state stays unknown. rdmaInterfacesChanged's trigger table sits beside this one; the two
// together are what a reader between passes can rely on.
func TestRefreshRDMAStateSequences(t *testing.T) {
	// The sequence stamps are fixed literals, so no case turns on the clock it ran at.
	at := func(seconds int) time.Time {
		return time.Date(2026, 10, 30, 12, 0, seconds, 0, time.UTC)
	}
	errFirst := errors.New("rdma read failed")
	errLater := errors.New("rdma read failed again later")

	// nested builds one populated observation under the given name, through the shapes a port
	// carries — the content a retention case must carry through a failure field by field.
	nested := func(name string) []device.RDMADevice {
		return []device.RDMADevice{{
			Name:           name,
			AllocationUnit: "0000:09:00.0",
			Owner:          device.RDMADeviceOwnerGPUStack,
			Driver:         "mlx5_core",
			PciBusID:       "0000:09:00.0",
			NumaAffinity:   "1",
			CpuAffinity:    "8-15",
			Ports: []device.RDMAPort{{
				Number:    1,
				NetDevs:   []string{"eth0", "eth1"},
				LinkLayer: "Ethernet",
				State:     "ACTIVE",
				Rate:      "200 Gb/sec (2X, NRZ)",
				GIDs:      []string{"fe80::1", "fe80::2"},
				PKeys:     []string{"0xffff", "0x0001"},
			}},
		}}
	}

	type observation struct {
		devices []device.RDMADevice
		err     error
		at      time.Time
	}

	testCases := []struct {
		name string
		// observations is folded in order through refreshRDMAInterfaces on one Detector.
		observations []observation
		// want is the snapshot expected after the last observation; nil expects no snapshot.
		want *device.RDMASnapshot
	}{
		{
			name: "a first failure keeps the state unknown",
			observations: []observation{
				{err: errFirst, at: at(1)},
			},
			want: nil,
		},
		{
			name: "a failure retains the last successful devices field by field",
			observations: []observation{
				{devices: nested("mlx5_0"), at: at(1)},
				{err: errFirst, at: at(2)},
			},
			want: &device.RDMASnapshot{
				Devices:            nested("mlx5_0"),
				Complete:           false,
				Revision:           device.RDMASnapshotRevision(nested("mlx5_0")),
				LastSuccessfulTime: at(1),
				Message:            errFirst.Error(),
			},
		},
		{
			name: "a stable failure keeps the retained record under the latest message",
			observations: []observation{
				{devices: nested("mlx5_0"), at: at(1)},
				{err: errFirst, at: at(2)},
				{err: errLater, at: at(3)},
			},
			want: &device.RDMASnapshot{
				Devices:            nested("mlx5_0"),
				Complete:           false,
				Revision:           device.RDMASnapshotRevision(nested("mlx5_0")),
				LastSuccessfulTime: at(1),
				Message:            errLater.Error(),
			},
		},
		{
			name: "a successful empty refresh replaces the retained inventory",
			observations: []observation{
				{devices: nested("mlx5_0"), at: at(1)},
				{err: errFirst, at: at(2)},
				{devices: []device.RDMADevice{}, at: at(4)},
			},
			want: &device.RDMASnapshot{
				Devices:            []device.RDMADevice{},
				Complete:           true,
				Revision:           device.RDMASnapshotRevision([]device.RDMADevice{}),
				LastSuccessfulTime: at(4),
				Message:            "",
			},
		},
		{
			name: "a recovery replaces the observation whole",
			observations: []observation{
				{devices: nested("mlx5_0"), at: at(1)},
				{err: errFirst, at: at(2)},
				{devices: nested("mlx5_9"), at: at(5)},
			},
			want: &device.RDMASnapshot{
				Devices:            nested("mlx5_9"),
				Complete:           true,
				Revision:           device.RDMASnapshotRevision(nested("mlx5_9")),
				LastSuccessfulTime: at(5),
				Message:            "",
			},
		},
		{
			name: "an empty success replaces a populated observation without a failure between",
			observations: []observation{
				{devices: nested("mlx5_0"), at: at(1)},
				{devices: []device.RDMADevice{}, at: at(3)},
			},
			want: &device.RDMASnapshot{
				Devices:            []device.RDMADevice{},
				Complete:           true,
				Revision:           device.RDMASnapshotRevision([]device.RDMADevice{}),
				LastSuccessfulTime: at(3),
				Message:            "",
			},
		},
	}

	for _, tc := range testCases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			d := &Detector{}
			for _, o := range tc.observations {
				d.refreshRDMAInterfaces(o.devices, o.err, o.at)
			}

			got := d.RDMAInterfaceSnapshot()
			if tc.want == nil {
				assert.Nil(t, got, "nothing was retained, so nothing is published")
				return
			}
			require.NotNil(t, got)
			assert.Equal(t, tc.want.Devices, got.Devices, "the device set, ports and entries included")
			assert.Equal(t, tc.want.Complete, got.Complete)
			assert.Equal(t, tc.want.Revision, got.Revision)
			assert.Equal(t, tc.want.LastSuccessfulTime, got.LastSuccessfulTime)
			assert.Equal(t, tc.want.Message, got.Message)
			if got.Complete {
				assert.NotEmpty(t, got.Revision,
					"every successful enumeration digests, the empty inventory included")
			}
		})
	}
}

// TestRDMASnapshotConcurrentAccess runs refreshes and reads against one Detector under -race: the
// atomic snapshot handoff is the only synchronization the paths share, so any state reaching a
// reader or writer without a copy shows up here.
func TestRDMASnapshotConcurrentAccess(t *testing.T) {
	d := &Detector{}
	now := time.Now()

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(fails bool) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if fails && i%2 == 0 {
					d.refreshRDMAInterfaces(nil, assert.AnError, now)
					continue
				}
				d.refreshRDMAInterfaces([]device.RDMADevice{{
					Name:  "mlx5_0",
					Ports: []device.RDMAPort{{Number: uint32(i + 1)}},
				}}, nil, now)
			}
		}(w == 1)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				if snapshot := d.RDMAInterfaceSnapshot(); snapshot != nil && len(snapshot.Devices) > 0 {
					_ = snapshot.Devices[0].Name
				}
			}
		}()
	}
	wg.Wait()
}
