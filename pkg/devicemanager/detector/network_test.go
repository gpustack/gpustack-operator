package detector

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// sysfsFixture builds a throwaway sysfs tree so the NIC pass can be exercised without a Linux host.
//
// The pass reads nothing but files, so a directory is a faithful stand-in for the parts of /sys it
// touches — and it has to be, because the seam that picks the real root lives in a _linux.go file
// that does not compile on the development platform. A fixture is the only place this logic can be
// checked before it reaches a node.
//
// The layout mirrors the real one rather than a convenient flattening of it, because the pass
// distinguishes a virtual interface from a physical one BY that layout: /sys/class/net/<name> is a
// symlink, and where it lands (devices/virtual/… versus devices/pci0000:00/…) is the answer. A
// fixture that made those directories real would let a broken classifier pass.
type sysfsFixture struct {
	t        *testing.T
	root     string
	devPaths map[string]string // bdf -> the device's path under devices/
}

func newSysfsFixture(t *testing.T) *sysfsFixture {
	t.Helper()
	return &sysfsFixture{t: t, root: t.TempDir(), devPaths: map[string]string{}}
}

func (f *sysfsFixture) write(rel, content string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if err := os.WriteFile(p, []byte(content), 0o644); err != nil {
		f.t.Fatalf("write %s: %v", p, err)
	}
}

func (f *sysfsFixture) mkdir(rel string) {
	f.t.Helper()
	if err := os.MkdirAll(filepath.Join(f.root, rel), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", rel, err)
	}
}

// symlink points rel at target. target is interpreted relative to the fixture root unless absolute,
// which is how an escaping symlink is planted.
func (f *sysfsFixture) symlink(target, rel string) {
	f.t.Helper()
	p := filepath.Join(f.root, rel)
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		f.t.Fatalf("mkdir %s: %v", filepath.Dir(p), err)
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(f.root, target)
	}
	if err := os.Symlink(target, p); err != nil {
		f.t.Fatalf("symlink %s -> %s: %v", p, target, err)
	}
}

// addPCIDevice creates a PCI device under a bridge chain, mirroring the real layout where
// /sys/bus/pci/devices/<bdf> is a symlink into /sys/devices/pciDDDD:BB/<bridge>/…/<bdf>.
func (f *sysfsFixture) addPCIDevice(bdf, rootComplex string, bridges []string, attrs map[string]string) {
	f.t.Helper()
	segments := append([]string{"devices", rootComplex}, bridges...)
	devRel := filepath.Join(append(segments, bdf)...)
	f.mkdir(devRel)
	for k, v := range attrs {
		f.write(filepath.Join(devRel, k), v)
	}
	f.devPaths[bdf] = devRel
	f.symlink(devRel, filepath.Join("bus/pci/devices", bdf))
}

// addNetDevice creates a network interface under its PCI device, the way the kernel does: the
// interface directory lives at <pci device>/net/<name>, class/net/<name> points at it, and the
// interface's own device attribute points back up at the PCI device.
func (f *sysfsFixture) addNetDevice(name, bdf string, attrs map[string]string) {
	f.t.Helper()
	devRel, ok := f.devPaths[bdf]
	if !ok {
		f.t.Fatalf("addNetDevice(%s): PCI device %s was never added", name, bdf)
	}
	ifaceRel := filepath.Join(devRel, "net", name)
	f.mkdir(ifaceRel)
	for k, v := range attrs {
		f.write(filepath.Join(ifaceRel, k), v)
	}
	f.symlink(devRel, filepath.Join(ifaceRel, "device"))
	f.symlink(ifaceRel, filepath.Join("class/net", name))
}

// addVirtualNetDevice creates an interface with no device behind it — loopback, a bridge, a veth.
// The kernel puts these under devices/virtual, which is how they are recognized.
func (f *sysfsFixture) addVirtualNetDevice(name string, attrs map[string]string) {
	f.t.Helper()
	ifaceRel := filepath.Join("devices/virtual/net", name)
	f.mkdir(ifaceRel)
	if attrs == nil {
		attrs = map[string]string{"mtu": "1500", "operstate": "up"}
	}
	for k, v := range attrs {
		f.write(filepath.Join(ifaceRel, k), v)
	}
	f.symlink(ifaceRel, filepath.Join("class/net", name))
}

// addPlatformNetDevice creates a real interface that is NOT on the PCI bus — the case a PCI-rooted
// walk cannot see at all, and the reason this pass enumerates interfaces first.
func (f *sysfsFixture) addPlatformNetDevice(name, devName string, attrs map[string]string) {
	f.t.Helper()
	devRel := filepath.Join("devices/platform", devName)
	ifaceRel := filepath.Join(devRel, "net", name)
	f.mkdir(ifaceRel)
	for k, v := range attrs {
		f.write(filepath.Join(ifaceRel, k), v)
	}
	f.symlink(devRel, filepath.Join(ifaceRel, "device"))
	f.symlink(ifaceRel, filepath.Join("class/net", name))
}

// addRDMA binds an RDMA device to a PCI device, in the layout where it hangs under the device.
func (f *sysfsFixture) addRDMA(bdf, rdmaName string, portAttrs map[string]string) {
	f.t.Helper()
	devRel, ok := f.devPaths[bdf]
	if !ok {
		f.t.Fatalf("addRDMA: PCI device %s was never added", bdf)
	}
	base := filepath.Join(devRel, "infiniband", rdmaName)
	f.mkdir(base)
	for k, v := range portAttrs {
		f.write(filepath.Join(base, "ports/1", k), v)
	}
}

// addVF nests a virtual function under its physical function, both directions as the kernel does.
func (f *sysfsFixture) addVF(pfBDF, vfBDF string, index int) {
	f.t.Helper()
	pfRel, vfRel := f.devPaths[pfBDF], f.devPaths[vfBDF]
	if pfRel == "" || vfRel == "" {
		f.t.Fatalf("addVF: %s or %s was never added", pfBDF, vfBDF)
	}
	f.symlink(vfRel, filepath.Join(pfRel, "virtfn"+strconv.Itoa(index)))
	f.symlink(pfRel, filepath.Join(vfRel, "physfn"))
}

// physicalNIC is the common case: a PCI-backed, up interface behind one upstream bridge.
func (f *sysfsFixture) physicalNIC(name, bdf string) {
	f.t.Helper()
	f.addPCIDevice(bdf, "pci0000:00", []string{"0000:00:01.0"}, map[string]string{
		"numa_node":     "0",
		"vendor":        "0x15b3",
		"device":        "0x1017",
		"local_cpulist": "0-15",
	})
	f.addNetDevice(name, bdf, map[string]string{"mtu": "9000", "operstate": "up", "carrier": "1"})
}

func findInterface(ifaces []workercore.DeviceInterface, name string) *workercore.DeviceInterface {
	for i := range ifaces {
		if ifaces[i].Name == name {
			return &ifaces[i]
		}
	}
	return nil
}

// TestEnumerateInterfacesShape covers the record each kind of interface produces, including the two
// kinds that are easiest to lose: one that is not on the PCI bus at all, and one whose RDMA tree
// cannot be read. Both must appear, because an interface missing from the inventory is
// indistinguishable from hardware that is not there.
func TestEnumerateInterfacesShape(t *testing.T) {
	testCases := []struct {
		name   string
		build  func(f *sysfsFixture)
		assert func(t *testing.T, ifaces []workercore.DeviceInterface)
	}{
		{
			name: "PCI-backed RDMA interface",
			build: func(f *sysfsFixture) {
				f.physicalNIC("eth0", "0000:01:00.0")
				f.addRDMA("0000:01:00.0", "mlx5_0", nil)
			},
			assert: func(t *testing.T, ifaces []workercore.DeviceInterface) {
				got := findInterface(ifaces, "eth0")
				if got == nil {
					t.Fatalf("eth0 missing from %d interfaces", len(ifaces))
				}
				// PciRootID is the OUTERMOST BRIDGE's address, not the root complex's name.
				// That is what binding.PCIDevice.Root computes and what all nine
				// manufacturers already write into DeviceTopology.PciRootID, so the
				// interface side must compute it identically — a differently-derived value
				// here would never compare equal to an accelerator's, and the proximity it
				// exists to answer would silently always be "unrelated".
				if got.PciBusID != "0000:01:00.0" || got.PciRootID != "0000:00:01.0" {
					t.Errorf("bus coordinates = %q/%q, want 0000:01:00.0/0000:00:01.0",
						got.PciBusID, got.PciRootID)
				}
				if len(got.PciSwitches) != 1 || got.PciSwitches[0] != "0000:00:01.0" {
					t.Errorf("PciSwitches = %v, want the one upstream bridge", got.PciSwitches)
				}
				if !got.RDMA || got.RDMADevice != "mlx5_0" {
					t.Errorf("rdma = %v/%q, want true/mlx5_0", got.RDMA, got.RDMADevice)
				}
				if got.MTU != 9000 || !got.Up || got.Virtual {
					t.Errorf("mtu/up/virtual = %d/%v/%v", got.MTU, got.Up, got.Virtual)
				}
				if got.NumaAffinity != "0" || got.CpuAffinity != "0-15" {
					t.Errorf("affinity = %q/%q", got.NumaAffinity, got.CpuAffinity)
				}
				if got.PciVendor != "0x15b3" || got.PciDevice != "0x1017" {
					t.Errorf("pci ids = %q/%q, want raw hex", got.PciVendor, got.PciDevice)
				}
				if got.Bus != "pci" {
					t.Errorf("bus = %q, want pci", got.Bus)
				}
			},
		},
		{
			// The second branch of the coordinate algorithm: a device attached straight to the
			// root complex has no bridge above it, so binding.PCIDevice.Root falls back to the
			// device's OWN address. Reproducing that fallback is what keeps the two sides
			// comparable; deriving "the root complex" instead would look more correct and
			// compare equal to nothing.
			name: "PCI device attached directly to the root complex",
			build: func(f *sysfsFixture) {
				f.addPCIDevice("0000:05:00.0", "pci0000:00", nil, map[string]string{
					"numa_node": "1", "vendor": "0x8086", "device": "0x1572",
				})
				f.addNetDevice("eth9", "0000:05:00.0", map[string]string{"operstate": "down"})
			},
			assert: func(t *testing.T, ifaces []workercore.DeviceInterface) {
				got := findInterface(ifaces, "eth9")
				if got == nil {
					t.Fatalf("eth9 missing")
				}
				if len(got.PciSwitches) != 0 {
					t.Errorf("PciSwitches = %v, want empty — there is no bridge above it",
						got.PciSwitches)
				}
				if got.PciRootID != "0000:05:00.0" {
					t.Errorf("PciRootID = %q, want the device's own address", got.PciRootID)
				}
				if got.Up {
					t.Error("operstate down must not report up")
				}
				if got.MTU != 0 {
					t.Errorf("MTU = %d, want 0 — the attribute is absent, and absent is not a value",
						got.MTU)
				}
			},
		},
		{
			// P5: an interface on a non-PCI interconnect is invisible to a PCI-rooted walk. It
			// must be a KIND of interface here, not a hole, and it must not be mistaken for a
			// virtual one just because it has no PCI coordinates.
			name: "real interface that is not on the PCI bus",
			build: func(f *sysfsFixture) {
				f.addPlatformNetDevice("hccn0", "hisi-roce.0",
					map[string]string{"mtu": "1500", "operstate": "up"})
			},
			assert: func(t *testing.T, ifaces []workercore.DeviceInterface) {
				got := findInterface(ifaces, "hccn0")
				if got == nil {
					t.Fatalf("hccn0 missing — an interface with no PCI device was dropped")
				}
				if got.PciBusID != "" || got.PciRootID != "" || len(got.PciSwitches) != 0 {
					t.Errorf("PCI fields must be absent, got %q/%q/%v",
						got.PciBusID, got.PciRootID, got.PciSwitches)
				}
				if got.Virtual {
					t.Error("a real non-PCI interface must not be reported as virtual")
				}
				if got.Bus != "platform" {
					t.Errorf("bus = %q, want platform — the absence must read as a kind", got.Bus)
				}
			},
		},
		{
			// An RDMA tree that cannot be read yields rdma:false plus a reason naming what was
			// tried — never a crash, and never a silent claim of no RDMA.
			name: "unreadable RDMA tree",
			build: func(f *sysfsFixture) {
				f.physicalNIC("eth1", "0000:02:00.0")
				f.symlink("/nonexistent-escape-target",
					filepath.Join(f.devPaths["0000:02:00.0"], "infiniband"))
			},
			assert: func(t *testing.T, ifaces []workercore.DeviceInterface) {
				got := findInterface(ifaces, "eth1")
				if got == nil {
					t.Fatalf("eth1 missing")
				}
				if got.RDMA {
					t.Error("rdma must be false when the tree cannot be read")
				}
				if got.Link == nil {
					t.Fatal("an unreadable RDMA tree must carry a reason, not nothing")
				}
				if got.Link.State != workercore.DeviceInterfaceLinkStateUnverified {
					t.Errorf("state = %q, want unverified — a missing file must never reach failed",
						got.Link.State)
				}
				if got.Link.Reason == "" {
					t.Error("the reason must name the layouts that were tried")
				}
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSysfsFixture(t)
			tc.build(f)
			ifaces, err := enumerateInterfaces(f.root)
			if err != nil {
				t.Fatalf("enumerateInterfaces: %v", err)
			}
			tc.assert(t, ifaces)
		})
	}
}

// vfCount is twelve rather than the eight a NIC usually exposes, and the number is load-bearing.
//
// The VFs are discovered by reading virtfn0…virtfnN, and a directory read returns those in STRING
// order: virtfn0, virtfn1, virtfn10, virtfn11, virtfn2, … With eight VFs that string order happens
// to equal bus-id order, so the sort under test would be dead code and removing it would not fail
// anything. Crossing ten makes the two orders disagree, which is what gives the assertion below
// something to catch.
const vfCount = 12

// TestEnumerateInterfacesNestsVirtualFunctions pins P9: a PF with N VFs is one entry with N
// nested, never N+1 top-level entries — and that the nested list is ordered by bus id.
func TestEnumerateInterfacesNestsVirtualFunctions(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.write(filepath.Join(f.devPaths["0000:01:00.0"], "sriov_numvfs"), strconv.Itoa(vfCount))
	for i := 0; i < vfCount; i++ {
		vf := fmt.Sprintf("0000:01:%02d.1", i+1)
		f.addPCIDevice(vf, "pci0000:00", []string{"0000:00:01.0"}, map[string]string{"numa_node": "0"})
		f.addNetDevice("eth0v"+strconv.Itoa(i), vf, map[string]string{"mtu": "9000", "operstate": "up"})
		f.addVF("0000:01:00.0", vf, i)
	}

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}

	if len(ifaces) != 1 {
		names := make([]string, 0, len(ifaces))
		for _, iface := range ifaces {
			names = append(names, iface.Name)
		}
		t.Fatalf("got %d top-level interfaces (%s), want 1 — VFs must not appear as siblings",
			len(ifaces), strings.Join(names, ","))
	}
	pf := ifaces[0]
	if !pf.SRIOV {
		t.Error("sriov must be true for a physical function")
	}
	if len(pf.VirtualFunctions) != vfCount {
		t.Fatalf("got %d nested VFs, want %d", len(pf.VirtualFunctions), vfCount)
	}
	for i := 1; i < len(pf.VirtualFunctions); i++ {
		if pf.VirtualFunctions[i-1].PciBusID >= pf.VirtualFunctions[i].PciBusID {
			t.Errorf("VFs are not ordered by bus id: %q before %q",
				pf.VirtualFunctions[i-1].PciBusID, pf.VirtualFunctions[i].PciBusID)
		}
	}
}

// TestEnumerateInterfacesSRIOVIsSeparateFromVFCount pins that "a PF with zero VFs configured" and
// "not a PF at all" stay different facts (P10).
func TestEnumerateInterfacesSRIOVIsSeparateFromVFCount(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.write(filepath.Join(f.devPaths["0000:01:00.0"], "sriov_numvfs"), "0")
	f.physicalNIC("eth1", "0000:02:00.0")

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}
	pf, plain := findInterface(ifaces, "eth0"), findInterface(ifaces, "eth1")
	if pf == nil || plain == nil {
		t.Fatal("both interfaces must be reported")
	}
	if !pf.SRIOV {
		t.Error("a PF with zero VFs configured is still a PF")
	}
	if plain.SRIOV {
		t.Error("an interface that is not a PF must not report sriov")
	}
	if len(pf.VirtualFunctions) != 0 || len(plain.VirtualFunctions) != 0 {
		t.Error("neither interface has virtual functions")
	}
}

// TestEnumerateInterfacesMarksVirtual pins that a virtual interface is recorded and marked, never
// dropped: a node whose only interface is a bridge must read as one virtual interface.
func TestEnumerateInterfacesMarksVirtual(t *testing.T) {
	f := newSysfsFixture(t)
	f.addVirtualNetDevice("lo", map[string]string{"mtu": "65536", "operstate": "unknown"})
	f.addVirtualNetDevice("br0", map[string]string{"mtu": "1500", "operstate": "up"})

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}
	if len(ifaces) != 2 {
		t.Fatalf("got %d interfaces, want 2 — a virtual interface must not be dropped", len(ifaces))
	}
	for _, iface := range ifaces {
		if !iface.Virtual {
			t.Errorf("%s must be marked virtual", iface.Name)
		}
		if iface.Bus != "virtual" {
			t.Errorf("%s bus = %q, want virtual", iface.Name, iface.Bus)
		}
	}
}

// TestEnumerateInterfacesSkipsTheBondingControlFile pins that the net class directory's one
// non-interface entry is not published as an interface.
//
// With the bonding driver loaded the directory holds `bonding_masters`, a regular file that is the
// driver's control surface rather than a device. It was measured as the only non-symlink entry
// there on two hosts, and enumerating it produced a record carrying a name and nothing else.
func TestEnumerateInterfacesSkipsTheBondingControlFile(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.addVirtualNetDevice("bond0", nil)
	// A regular file, exactly as the kernel presents it — not a symlink to a device directory.
	f.write(filepath.Join("class/net", "bonding_masters"), "bond0\n")

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}
	for _, iface := range ifaces {
		if iface.Name == "bonding_masters" {
			t.Fatalf("bonding_masters was published as an interface (%+v); it is the bonding "+
				"driver's control file, and an entry with a name and no bus reads as an "+
				"interface whose bus could not be determined", iface)
		}
	}
	if len(ifaces) != 2 {
		names := make([]string, 0, len(ifaces))
		for _, iface := range ifaces {
			names = append(names, iface.Name)
		}
		t.Fatalf("got %d interfaces (%s), want 2 — the real ones must still be reported",
			len(ifaces), strings.Join(names, ","))
	}
}

// TestEnumerateInterfacesSkipsAnEntryThatVanished pins the one carve-out in the rule that an
// unclassifiable net class entry ends the pass: an interface removed between the directory listing
// and the classification leaves a dangling symlink, and that is an ordinary race rather than an
// unreadable tree. Failing the pass on it would let any interface teardown suppress the whole
// inventory for a cycle, which is the cost of reading a race as a fault.
func TestEnumerateInterfacesSkipsAnEntryThatVanished(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.symlink(filepath.Join(f.root, "devices", "gone"), filepath.Join("class/net", "eth1"))

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v — an entry that vanished mid-enumeration is a race, "+
			"not an unreadable tree", err)
	}
	if len(ifaces) != 1 || ifaces[0].Name != "eth0" {
		t.Fatalf("got %+v, want just eth0 — the vanished entry is skipped and the real one kept",
			ifaces)
	}
}

// TestEnumerateInterfacesFailedReadIsNotEmpty pins that a failure to enumerate is reported as an
// error, never as an empty list. An empty list is indistinguishable from a node that was never
// profiled, so returning (nil, nil) here is the failure mode that passes by omission.
func TestEnumerateInterfacesFailedReadIsNotEmpty(t *testing.T) {
	testCases := []struct {
		name  string
		setup func(t *testing.T) string
	}{
		{
			name:  "root does not exist",
			setup: func(t *testing.T) string { return filepath.Join(t.TempDir(), "absent") },
		},
		{
			name: "the net class directory is missing",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "bus/pci/devices"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				return root
			},
		},
		{
			name: "the net class directory is a file",
			setup: func(t *testing.T) string {
				root := t.TempDir()
				if err := os.MkdirAll(filepath.Join(root, "class"), 0o755); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(filepath.Join(root, "class/net"), []byte("x"), 0o644); err != nil {
					t.Fatalf("write: %v", err)
				}
				return root
			},
		},
		{
			// A PARTIAL absence is the same failure mode: this PF answers `sriov_numvfs` and then
			// refuses to be listed, so the record would read "a PF with zero VFs configured" —
			// the fact this inventory keeps distinct from "not a PF" — off a directory that failed.
			name: "a physical function's virtual functions cannot be listed",
			setup: func(t *testing.T) string {
				f := newSysfsFixture(t)
				f.physicalNIC("eth0", "0000:01:00.0")
				pfRel := f.devPaths["0000:01:00.0"]
				f.write(filepath.Join(pfRel, "sriov_numvfs"), "4")

				// Executable but not readable: a file inside can still be opened by name, so the
				// flag above still answers, while the directory refuses to enumerate.
				pfDir := filepath.Join(f.root, pfRel)
				if err := os.Chmod(pfDir, 0o111); err != nil {
					t.Fatalf("chmod %s: %v", pfDir, err)
				}
				t.Cleanup(func() { _ = os.Chmod(pfDir, 0o755) })
				// The trigger has to actually fire. A process with CAP_DAC_OVERRIDE ignores the
				// mode bits, and a case whose trigger never fires reads exactly like one that
				// passed — so say so instead of counting it.
				if _, err := os.ReadDir(pfDir); err == nil {
					t.Skip("this process can list a mode-0111 directory, " +
						"so an unlistable one cannot be staged here")
				}
				return f.root
			},
		},
		{
			// A non-virtual interface whose `device` link does not resolve. The degraded record
			// would be `rdma: false` with no verdict — indistinguishable from a plain Ethernet
			// NIC — so a node whose only RDMA interface hit this would lose `rdma.capable` because
			// a symlink could not be read.
			name: "a physical interface's device link does not resolve",
			setup: func(t *testing.T) string {
				f := newSysfsFixture(t)
				f.physicalNIC("eth0", "0000:01:00.0")
				// Replace the interface's own `device` link with a dangling one, which is what a
				// transiently unreadable sysfs looks like from here.
				p := filepath.Join(f.root, f.devPaths["0000:01:00.0"], "net", "eth0", "device")
				if err := os.Remove(p); err != nil {
					t.Fatalf("remove %s: %v", p, err)
				}
				if err := os.Symlink(filepath.Join(f.root, "devices", "gone"), p); err != nil {
					t.Fatalf("symlink: %v", err)
				}
				return f.root
			},
		},
		{
			// The net class entry itself cannot be classified. The skip that keeps `bonding_masters`
			// out is written on "proven not to be a directory", so an entry that could not be read
			// must not take the same exit: doing so drops a real interface from the list and
			// publishes the partial absence F3 refuses.
			name: "a net class entry resolves outside the sysfs root",
			setup: func(t *testing.T) string {
				f := newSysfsFixture(t)
				f.physicalNIC("eth0", "0000:01:00.0")
				f.symlink(t.TempDir(), filepath.Join("class/net", "eth1"))
				return f.root
			},
		},
		{
			// The same contract one level down, and staged without relying on mode bits: skipping
			// past an unreadable virtfn would shorten a list whose length is itself a fact.
			name: "a virtual function resolves outside the sysfs root",
			setup: func(t *testing.T) string {
				f := newSysfsFixture(t)
				f.physicalNIC("eth0", "0000:01:00.0")
				pfRel := f.devPaths["0000:01:00.0"]
				f.write(filepath.Join(pfRel, "sriov_numvfs"), "1")
				f.symlink(t.TempDir(), filepath.Join(pfRel, "virtfn0"))
				return f.root
			},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			ifaces, err := enumerateInterfaces(tc.setup(t))
			if err == nil {
				t.Fatalf("got (%d interfaces, nil error), want an error — "+
					"a failed enumeration must not serialize as an empty inventory", len(ifaces))
			}
		})
	}
}

// TestReadSysfsAttrDiscipline pins the limits every sysfs read goes through. Following symlinks is
// unavoidable here, so checking where they landed is the only thing standing between this pass and
// reading a file outside the tree it was pointed at.
func TestReadSysfsAttrDiscipline(t *testing.T) {
	f := newSysfsFixture(t)
	outside := filepath.Join(t.TempDir(), "secret")
	if err := os.WriteFile(outside, []byte("not for us"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	f.mkdir("class/net/eth0")
	f.symlink(outside, "class/net/eth0/escaped")
	f.write("class/net/eth0/huge", strings.Repeat("x", 1<<20))
	f.write("class/net/eth0/normal", "9000\n")

	testCases := []struct {
		name    string
		rel     string
		want    string
		wantErr bool
	}{
		{"a normal attribute is read and trimmed", "class/net/eth0/normal", "9000", false},
		{"a symlink escaping the root is refused", "class/net/eth0/escaped", "", true},
		{"an oversized file is refused", "class/net/eth0/huge", "", true},
		{"a missing attribute is refused", "class/net/eth0/absent", "", true},
	}
	tree, err := newSysfsTree(f.root)
	if err != nil {
		t.Fatalf("newSysfsTree: %v", err)
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tree.attr(tc.rel)
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr = %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Errorf("got %q, want %q", got, tc.want)
			}
		})
	}
}

// TestEnumerateInterfacesIsDeterministic pins P8. An unsorted list makes the align function see a
// change on every pass, so the detector writes to the API forever with no wrong data anywhere to
// notice it by — the one failure mode no functional test would catch.
//
// What this does NOT cover, measured rather than assumed: deleting the top-level sort does not
// fail this test. The interfaces are discovered by reading a directory, which already returns them
// in name order, so the sort is a deliberate belt to the directory read's braces — not a line any
// fixture can hold to account. The assertion is on the CONTRACT (the published list is ordered),
// which is the right thing to assert; it just should not be mistaken for coverage of that line. The
// nested-VF ordering is different: see vfCount for why that one is genuinely exercised.
func TestEnumerateInterfacesIsDeterministic(t *testing.T) {
	f := newSysfsFixture(t)
	for _, n := range []struct{ name, bdf string }{
		{"eth2", "0000:03:00.0"}, {"eth0", "0000:01:00.0"}, {"eth1", "0000:02:00.0"},
	} {
		f.physicalNIC(n.name, n.bdf)
	}
	f.addVirtualNetDevice("br0", nil)

	first, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("first pass: %v", err)
	}
	second, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("second pass: %v", err)
	}

	if len(first) != 4 {
		t.Fatalf("got %d interfaces, want 4", len(first))
	}
	for i := 1; i < len(first); i++ {
		if first[i-1].Name >= first[i].Name {
			t.Errorf("not sorted by name: %q before %q", first[i-1].Name, first[i].Name)
		}
	}
	for i := range first {
		if first[i].Name != second[i].Name {
			t.Fatalf("two passes over unchanged hardware differ at %d: %q vs %q",
				i, first[i].Name, second[i].Name)
		}
	}
}

// TestResolveRDMAReverseLayoutFailureIsUnverified pins the rule this feature applies everywhere
// else, on the one path that did not: a tree that exists and cannot be read is UNVERIFIED, never
// "this interface has no RDMA device".
//
// Publishing `rdma: false` there is the same unsupported claim as publishing an empty inventory
// after a failed enumeration. It was reachable through the reverse `/sys/class/infiniband` layout,
// where every failure was discarded and the interface came back looking like plain Ethernet.
func TestResolveRDMAReverseLayoutFailureIsUnverified(t *testing.T) {
	testCases := []struct {
		name  string
		build func(f *sysfsFixture)
	}{
		{
			// The class directory itself is there and unreadable.
			name: "the RDMA class directory cannot be read",
			build: func(f *sysfsFixture) {
				f.symlink("/nonexistent-escape-target", sysfsInfinibandDir)
			},
		},
		{
			// The class directory lists an entry whose `device` link cannot be resolved. That
			// candidate might have been the match, so no answer was established.
			name: "a candidate's device link cannot be resolved",
			build: func(f *sysfsFixture) {
				f.mkdir(filepath.Join(sysfsInfinibandDir, "mlx5_0"))
				f.symlink("/nonexistent-escape-target",
					filepath.Join(sysfsInfinibandDir, "mlx5_0", "device"))
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			f := newSysfsFixture(t)
			f.physicalNIC("eth0", "0000:02:00.0")
			tc.build(f)

			ifaces, err := enumerateInterfaces(f.root)
			if err != nil {
				t.Fatalf("enumerateInterfaces: %v", err)
			}
			got := findInterface(ifaces, "eth0")
			if got == nil {
				t.Fatal("eth0 missing from the inventory")
			}
			if got.RDMA {
				t.Error("rdma must not be true: no RDMA device was resolved")
			}
			if got.Link == nil {
				t.Fatal("an unreadable RDMA tree must leave a verdict; a nil link here reads as " +
					"'this interface has no RDMA device', which is not what was established")
			}
			if got.Link.State != workercore.DeviceInterfaceLinkStateUnverified {
				t.Errorf("state = %q, want %q", got.Link.State,
					workercore.DeviceInterfaceLinkStateUnverified)
			}
		})
	}
}

// TestEnumerateInterfacesChecksVirtualFunctionLinks pins that a VF's RDMA gets a verdict.
//
// A VF is removed from the top-level list, so a nil verdict here is a bound RDMA device with no
// verdict anywhere in the record — and on a setup that exposes RDMA only through its virtual
// functions, that is every RDMA device the node has.
func TestEnumerateInterfacesChecksVirtualFunctionLinks(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:01:00.0")
	f.write(filepath.Join(f.devPaths["0000:01:00.0"], "sriov_numvfs"), "2")

	// One VF's link is up, the other's is down, so the two states cannot both come from a default.
	f.addPCIDevice("0000:01:00.1", "pci0000:00", []string{"0000:00:01.0"}, map[string]string{"numa_node": "0"})
	f.addNetDevice("eth0v0", "0000:01:00.1", map[string]string{"mtu": "9000"})
	f.addVF("0000:01:00.0", "0000:01:00.1", 0)
	f.addRDMA("0000:01:00.1", "mlx5_1", map[string]string{"state": "4: ACTIVE", "phys_state": "5: LinkUp"})

	f.addPCIDevice("0000:01:00.2", "pci0000:00", []string{"0000:00:01.0"}, map[string]string{"numa_node": "0"})
	f.addNetDevice("eth0v1", "0000:01:00.2", map[string]string{"mtu": "9000"})
	f.addVF("0000:01:00.0", "0000:01:00.2", 1)
	f.addRDMA("0000:01:00.2", "mlx5_2", map[string]string{"state": "1: DOWN", "phys_state": "3: Disabled"})

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}
	pf := findInterface(ifaces, "eth0")
	if pf == nil {
		t.Fatal("eth0 missing from the inventory")
	}
	if len(pf.VirtualFunctions) != 2 {
		t.Fatalf("virtual functions = %d, want 2", len(pf.VirtualFunctions))
	}

	want := map[string]workercore.DeviceInterfaceLinkState{
		"0000:01:00.1": workercore.DeviceInterfaceLinkStateOK,
		"0000:01:00.2": workercore.DeviceInterfaceLinkStateFailed,
	}
	for i := range pf.VirtualFunctions {
		vf := &pf.VirtualFunctions[i]
		if !vf.RDMA {
			t.Errorf("vf %s: rdma = false, want true", vf.PciBusID)
			continue
		}
		if vf.Link == nil {
			t.Errorf("vf %s: a bound RDMA device with no link verdict", vf.PciBusID)
			continue
		}
		if vf.Link.State != want[vf.PciBusID] {
			t.Errorf("vf %s: state = %q, want %q", vf.PciBusID, vf.Link.State, want[vf.PciBusID])
		}
	}
}

// TestRDMAClassIndexIsReadOncePerTree pins the cost half of the reverse layout, which no value
// assertion can reach: the class directory is listed once per pass, not once per interface.
//
// resolveRDMA runs for every interface AND every virtual function, and the shape this replaced
// listed `class/infiniband` and resolved every entry under it on each of those calls — quadratic in
// the number of RDMA virtual functions, on every pass, on a layout the code supports. Nothing about
// the published inventory changes, so the only observable is how many times the directory is read.
//
// It is measured by DELETING the directory between two calls. A second read would find it absent;
// a memo still answers from the first. The third assertion is the one that matters most: a NEW tree
// over the same root reports it absent, which is what separates a per-pass memo from a cache. An
// index that outlived a pass would answer with hardware that has since gone away.
func TestRDMAClassIndexIsReadOncePerTree(t *testing.T) {
	f := newSysfsFixture(t)
	f.physicalNIC("eth0", "0000:02:00.0")
	// The reverse layout: the RDMA device lives under the class directory and points back.
	f.mkdir(filepath.Join(sysfsInfinibandDir, "mlx5_0"))
	f.symlink(f.devPaths["0000:02:00.0"], filepath.Join(sysfsInfinibandDir, "mlx5_0", "device"))

	tree, err := newSysfsTree(f.root)
	if err != nil {
		t.Fatalf("newSysfsTree: %v", err)
	}

	first := tree.rdmaClassIndex()
	if !first.listed || len(first.byDevice) != 1 {
		t.Fatalf("first index = %+v, want it listed with one entry", first)
	}

	if err := os.RemoveAll(filepath.Join(f.root, sysfsInfinibandDir)); err != nil {
		t.Fatalf("remove the class directory: %v", err)
	}

	second := tree.rdmaClassIndex()
	if !second.listed || len(second.byDevice) != len(first.byDevice) {
		t.Errorf("second index = %+v, want the first one's answer; the directory was read again, "+
			"so every interface and virtual function pays for its own listing", second)
	}

	fresh, err := newSysfsTree(f.root)
	if err != nil {
		t.Fatalf("newSysfsTree: %v", err)
	}
	if idx := fresh.rdmaClassIndex(); idx.listed {
		t.Error("a new tree answered from a previous tree's reading: the memo is scoped to one " +
			"pass precisely so it cannot report hardware that has gone away")
	}
}

// TestBusIsTheDeviceSubsystemNotThePathRoot pins that a NIC bridged off PCI is not reported as a
// PCI device.
//
// The path is where a device SITS: a USB NIC resolves through
// `devices/pci0000:00/<bridge>/usb1/1-1/1-1:1.0/net/<name>`, so classifying from the first path
// segment called it PCI. The consequence was not cosmetic — the basename of its device directory
// ("1-1:1.0") was published as `pciBusId`, a bridge walk ran on a device with no PCI address, and
// the coordinates that came out feed the RDMA distance label.
func TestBusIsTheDeviceSubsystemNotThePathRoot(t *testing.T) {
	f := newSysfsFixture(t)

	// A real PCI NIC, with the subsystem link a kernel would give it.
	f.physicalNIC("eth0", "0000:02:00.0")
	f.mkdir("bus/pci")
	f.symlink("bus/pci", filepath.Join(f.devPaths["0000:02:00.0"], "subsystem"))

	// A USB NIC: its device directory sits under a PCI host controller, and its own subsystem is
	// usb. `1-1:1.0` is a USB interface address, not a BDF.
	usbRel := filepath.Join(sysfsDevicesDir, "pci0000:00", "0000:00:14.0", "usb1", "1-1", "1-1:1.0")
	f.mkdir(usbRel)
	f.mkdir("bus/usb")
	f.symlink("bus/usb", filepath.Join(usbRel, "subsystem"))
	ifaceRel := filepath.Join(usbRel, "net", "eth1")
	f.write(filepath.Join(ifaceRel, "mtu"), "1500")
	f.write(filepath.Join(ifaceRel, "operstate"), "up")
	f.symlink(usbRel, filepath.Join(ifaceRel, "device"))
	f.symlink(ifaceRel, filepath.Join("class/net", "eth1"))

	ifaces, err := enumerateInterfaces(f.root)
	if err != nil {
		t.Fatalf("enumerateInterfaces: %v", err)
	}

	pci := findInterface(ifaces, "eth0")
	if pci == nil {
		t.Fatal("eth0 missing from the inventory")
	}
	if pci.Bus != "pci" || pci.PciBusID != "0000:02:00.0" {
		t.Errorf("eth0 bus=%q pciBusId=%q, want pci and its BDF; the subsystem answer must not "+
			"take PCI coordinates away from a device that has them", pci.Bus, pci.PciBusID)
	}

	usb := findInterface(ifaces, "eth1")
	if usb == nil {
		t.Fatal("eth1 missing from the inventory: a USB NIC is still an interface")
	}
	if usb.Bus != "usb" {
		t.Errorf("eth1 bus = %q, want %q", usb.Bus, "usb")
	}
	// The load-bearing half: no fabricated PCI coordinates. `pciBusId` holding a USB interface
	// address is what makes a distance computed from it meaningless rather than merely unknown.
	if usb.PciBusID != "" || usb.PciRootID != "" || len(usb.PciSwitches) != 0 {
		t.Errorf("eth1 carries PCI coordinates it has none of: pciBusId=%q pciRootId=%q switches=%v",
			usb.PciBusID, usb.PciRootID, usb.PciSwitches)
	}
}
