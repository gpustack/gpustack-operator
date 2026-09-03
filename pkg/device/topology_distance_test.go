package device

import "testing"

// topologyDistanceTable is the vocabulary written out longhand, in order, exactly as the GPUStack
// runtime component publishes it (https://github.com/gpustack/runtime), where it is sampled on nine
// real platforms.
//
// These literals ARE the contract. That component is Python and is not a dependency of this module,
// so nothing at build time can hold the two vocabularies together — this table is the only thing
// that does. A value edited on either side and not the other shows up here and nowhere else.
var topologyDistanceTable = []struct {
	Name     string
	Value    int32
	Distance TopologyDistance
}{
	{"SELF", 0, TopologyDistanceSelf},
	{"LINK", 5, TopologyDistanceLink},
	{"PIX", 10, TopologyDistancePIX},
	{"PXB", 20, TopologyDistancePXB},
	{"PHB", 30, TopologyDistancePHB},
	{"NODE", 40, TopologyDistanceNode},
	{"SYS", 50, TopologyDistanceSys},
	{"UNK", 100, TopologyDistanceUnknown},
}

func TestTopologyDistanceValues(t *testing.T) {
	for _, tc := range topologyDistanceTable {
		t.Run(tc.Name, func(t *testing.T) {
			if got := int32(tc.Distance); got != tc.Value {
				t.Errorf("%s = %d, want %d — the vocabulary must match the runtime component's table",
					tc.Name, got, tc.Value)
			}
		})
	}
}

func TestTopologyDistanceNames(t *testing.T) {
	for _, tc := range topologyDistanceTable {
		t.Run(tc.Name, func(t *testing.T) {
			if got := tc.Distance.String(); got != tc.Name {
				t.Errorf("String() = %q, want %q", got, tc.Name)
			}
		})
	}
}

// TestTopologyDistanceUnnamedValueIsUnknown pins that a value outside the vocabulary renders as
// UNK rather than as an empty string or a number. The rendering reaches a node label, and an empty
// label value is a different failure — it publishes a key whose value means nothing — from a label
// that honestly says the distance is unknown.
func TestTopologyDistanceUnnamedValueIsUnknown(t *testing.T) {
	testCases := []struct {
		name     string
		distance TopologyDistance
	}{
		{"above the vocabulary", TopologyDistance(101)},
		{"between two levels", TopologyDistance(11)},
		{"negative", TopologyDistance(-1)},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.distance.String(); got != "UNK" {
				t.Errorf("String() = %q, want %q", got, "UNK")
			}
		})
	}
}

// TestTopologyDistanceAscendsWithRoomToInsert pins the vocabulary's one structural contract: the
// levels increase with distance, AND consecutive levels leave a value free between them.
//
// The two halves are one contract, not two, and deliberately live in one assertion: requiring a gap
// of at least 2 already requires the sequence to ascend, so a separate ordering test would have no
// failure of its own — every input that breaks ordering breaks this too. Keeping them apart would
// have added a test that can never be the only one to fail, which is a test that checks nothing.
//
// Why the contract matters: ascending is what lets a consumer ask "at least this close" with one
// comparison, and the gap is why the values are 0/5/10/20/… rather than 0/1/2/3/… — a level
// discovered later can be numbered between two existing ones, so nothing that persisted a number
// needs migrating.
func TestTopologyDistanceAscendsWithRoomToInsert(t *testing.T) {
	for i := 1; i < len(topologyDistanceTable); i++ {
		prev, cur := topologyDistanceTable[i-1], topologyDistanceTable[i]
		t.Run(prev.Name+".."+cur.Name, func(t *testing.T) {
			if gap := cur.Distance - prev.Distance; gap < 2 {
				t.Errorf("gap from %s (%d) to %s (%d) is %d; the sequence must ascend and leave a value to insert between them",
					prev.Name, prev.Distance, cur.Name, cur.Distance, gap)
			}
		})
	}
}

// TestBusDistance pins what the bus coordinates can and cannot establish.
//
// Two of its cases are the criteria: nothing here may return LINK — that level is not derivable
// from a bus reading at all, and a chain that derived it would rate accelerators as peers on the
// strength of them sitting near each other — and nothing may return a distance where a coordinate
// was missing, because "we could not tell" and "they are far apart" send an operator to different
// hardware.
func TestBusDistance(t *testing.T) {
	accelerator := func(bus, root string, switches []string, numa string) Topology {
		return Topology{PciBusID: bus, PciRootID: root, PciSwitches: switches, NumaAffinity: numa}
	}
	iface := func(bus, root string, switches []string, numa string) Interface {
		return Interface{
			Name: "eth0", PciBusID: bus, PciRootID: root, PciSwitches: switches, NumaAffinity: numa,
		}
	}

	testCases := []struct {
		name string
		a    Topology
		i    Interface
		want TopologyDistance
	}{
		{
			name: "behind the same innermost bridge",
			a:    accelerator("0000:01:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			i:    iface("0000:02:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			want: TopologyDistancePIX,
		},
		{
			// The paths meet at the outermost bridge and diverge below it. PciRootID IS that
			// outermost bridge, which is why sharing it is a weaker claim than sharing the first.
			name: "inside one bridge subtree but not behind the same innermost bridge",
			a: accelerator("0000:03:00.0", "0000:00:02.0",
				[]string{"0000:01:01.0", "0000:00:02.0"}, "0"),
			i: iface("0000:04:00.0", "0000:00:02.0",
				[]string{"0000:01:02.0", "0000:00:02.0"}, "0"),
			want: TopologyDistancePXB,
		},
		{
			// Different bridge subtrees on one NUMA node. This is where PHB and NODE become
			// indistinguishable, and the further of the two is reported.
			name: "different bridge subtrees on the same NUMA node",
			a:    accelerator("0000:01:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			i:    iface("0000:81:00.0", "0000:80:01.0", []string{"0000:80:01.0"}, "0"),
			want: TopologyDistanceNode,
		},
		{
			name: "different NUMA nodes",
			a:    accelerator("0000:01:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			i:    iface("0000:81:00.0", "0000:80:01.0", []string{"0000:80:01.0"}, "1"),
			want: TopologyDistanceSys,
		},
		{
			// The case the whole interface-first enumeration exists for: a NIC that is not on the
			// PCI bus at all. It has no coordinates, so there is no distance to report.
			name: "an interface with no PCI coordinates",
			a:    accelerator("0000:01:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			i:    iface("", "", nil, "0"),
			want: TopologyDistanceUnknown,
		},
		{
			name: "an accelerator with no PCI coordinates",
			a:    accelerator("", "", nil, "0"),
			i:    iface("0000:02:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, "0"),
			want: TopologyDistanceUnknown,
		},
		{
			// Past the bridge subtree NUMA is the only coordinate left, so an unknown one leaves
			// the question unanswered rather than defaulting to the same node.
			name: "different subtrees with an unknown NUMA node",
			a:    accelerator("0000:01:00.0", "0000:00:01.0", []string{"0000:00:01.0"}, ""),
			i:    iface("0000:81:00.0", "0000:80:01.0", []string{"0000:80:01.0"}, "0"),
			want: TopologyDistanceUnknown,
		},
		{
			// Two devices hanging directly off the root complex have no bridge to share, so their
			// own addresses are their PciRootIDs and the answer falls through to NUMA.
			name: "no bridges at all",
			a:    accelerator("0000:00:1f.6", "0000:00:1f.6", nil, "0"),
			i:    iface("0000:00:19.0", "0000:00:19.0", nil, "0"),
			want: TopologyDistanceNode,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := BusDistance(tc.a, tc.i)
			if got != tc.want {
				t.Errorf("distance = %s (%d), want %s (%d)", got, got, tc.want, tc.want)
			}
			switch got {
			case TopologyDistanceLink:
				t.Error("the bus axis returned LINK, a level it cannot observe: an accelerator " +
					"interconnect is invisible in bus coordinates, and deriving it here would " +
					"rate two devices as peers for sitting near each other")
			case TopologyDistanceSelf:
				t.Error("the bus axis returned SELF for an accelerator and a NIC, which are " +
					"never the same device")
			case TopologyDistancePHB:
				t.Error("the bus axis returned PHB, which asserts a shared PCIe host bridge — " +
					"a component these coordinates do not carry")
			}
		})
	}
}
