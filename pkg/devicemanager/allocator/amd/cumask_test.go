package amd

import (
	"strings"
	"testing"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
)

// The two cards the conformance fixture was measured on, in
// .claude/skills/gpustack-operator-xbuild-and-verify/references/amd-cumask-conformance.md.
var (
	// gfx1101: 60 CU / 30 WGP / 3 SE / 2 SA-per-SE / 1 XCC, RDNA.
	rdna = Topology{Name: "gfx1101", CU: 60, SE: 3, SAPerSE: 2, XCC: 1}
	// gfx942: 304 CU / 32 SE / 1 SA-per-SE / 8 XCC, CDNA.
	cdna = Topology{Name: "gfx942", CU: 304, SE: 32, SAPerSE: 1, XCC: 8}
	// gfx90a: MI210, CDNA silicon reporting a single XCC.
	cdnaSingleXCC = Topology{Name: "gfx90a", CU: 104, SE: 4, SAPerSE: 1, XCC: 1}
)

// derivedMask is the whole production path for a percentage on an empty card: the length, the
// placement and the rendered segment. The conformance tables' mask column is exactly this.
func derivedMask(t *testing.T, topo Topology, pct int) string {
	t.Helper()

	length, err := WindowCUs(topo, pct)
	if err != nil {
		t.Fatalf("WindowCUs(%s, %d%%): unexpected error: %v", topo.Name, pct, err)
	}
	return Mask(0, PackWindow(topo, length, nil))
}

// TestWindowCUs_ConformanceTableA reproduces the RDNA table row for row. The 25 % and 75 % rows are
// the ones that matter: the naive derivation emits 0:0-14 and 0:0-44, and both measured the whole
// card on hardware.
func TestWindowCUs_ConformanceTableA(t *testing.T) {
	testCases := []struct {
		name string
		pct  int
		want string
	}{
		{name: "10 percent, 3 WGPs", pct: 10, want: "0:0-5"},
		{name: "20 percent, 6 WGPs", pct: 20, want: "0:0-11"},
		{name: "25 percent, 8 WGPs aligned down to 6", pct: 25, want: "0:0-11"},
		{name: "50 percent, 15 WGPs", pct: 50, want: "0:0-29"},
		{name: "75 percent, 23 WGPs aligned down to 21", pct: 75, want: "0:0-41"},
		{name: "100 percent, 30 WGPs", pct: 100, want: "0:0-59"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivedMask(t, rdna, tc.pct); got != tc.want {
				t.Errorf("mask = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWindowCUs_ConformanceTableB reproduces the CDNA table row for row.
//
// The table's percentages are the exact CU fractions the probe was driven with; a request carries
// an integer percentage, so each fractional row is asserted through the smallest integer that lands
// on the same atom count — 2.63 % -> 3 %, 5.26 % -> 6 %, 10.5 % -> 11 %. The mask column, which is
// what the derivation must reproduce, is unchanged by that.
func TestWindowCUs_ConformanceTableB(t *testing.T) {
	testCases := []struct {
		name string
		pct  int
		want string
	}{
		{name: "2.63 percent row, one atom", pct: 3, want: "0:0-7"},
		{name: "5 percent, 15 CUs aligned down to 8", pct: 5, want: "0:0-7"},
		{name: "5.26 percent row, two atoms", pct: 6, want: "0:0-15"},
		{name: "10.5 percent row, four atoms", pct: 11, want: "0:0-31"},
		{name: "50 percent, 152 CUs", pct: 50, want: "0:0-151"},
		{name: "100 percent, 304 CUs", pct: 100, want: "0:0-303"},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if got := derivedMask(t, cdna, tc.pct); got != tc.want {
				t.Errorf("mask = %q, want %q", got, tc.want)
			}
		})
	}
}

// TestWindowCUs_RefusesSubQuantumRequest covers table B's 1 % row and RDNA's sub-round request. A
// sub-quantum mask fails open — it leaves the units it never mentions running unmasked — so it is
// refused rather than clamped, and the message names the minimum the card can honor.
func TestWindowCUs_RefusesSubQuantumRequest(t *testing.T) {
	testCases := []struct {
		name     string
		topology Topology
		pct      int
		wantMsg  string
	}{
		{
			name:     "table B 1 percent, below one 8-CU atom",
			topology: cdna,
			pct:      1,
			wantMsg:  "smallest slice is 3%",
		},
		{
			name:     "RDNA 1 percent, below one 3-WGP round",
			topology: rdna,
			pct:      1,
			wantMsg:  "smallest slice is 9%",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := WindowCUs(tc.topology, tc.pct)
			if err == nil {
				t.Fatalf("WindowCUs(%s, %d%%) = no error, want a refusal", tc.topology.Name, tc.pct)
			}
			if !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
		})
	}
}

// TestWindowCUs_SingleXCCCDNARoutesToCDNABranch pins the routing that an "NUM_XCC > 1 means CDNA"
// discriminator gets wrong: gfx90a is CDNA silicon reporting one XCC, and the RDNA branch would
// apply WGP pairing to a part that has no WGPs. Asserted through the branch's observable output —
// its quantum and an emitted mask — rather than through the predicate alone.
func TestWindowCUs_SingleXCCCDNARoutesToCDNABranch(t *testing.T) {
	// One CU in one XCC: the CDNA atom degenerates to a contiguous window of any length. The RDNA
	// branch would quantise to 2*SE = 8 CUs instead.
	if got, want := cdnaSingleXCC.Quantum(), 1; got != want {
		t.Errorf("Quantum() = %d, want %d", got, want)
	}
	// 33 % of 104 CUs is 34 CUs on the CDNA branch; the RDNA branch would round 52 WGPs to 17, align
	// down to 16 and emit 0:0-31.
	if got, want := derivedMask(t, cdnaSingleXCC, 33), "0:0-33"; got != want {
		t.Errorf("mask = %q, want %q", got, want)
	}
}

// TestValidate_DegenerateTopologies pins that every unusable topology refuses rather than dividing
// by a zero or panicking. binding/hsa turns an unreadable agent-info field into a zero rather than
// an error, so "absent" and "zero" are the same value here.
func TestValidate_DegenerateTopologies(t *testing.T) {
	testCases := []struct {
		name       string
		topology   Topology
		wantRefuse bool
		wantMsg    string
	}{
		{
			name:       "no compute units reported",
			topology:   Topology{Name: "gfx1101", CU: 0, SE: 3, SAPerSE: 2, XCC: 1},
			wantRefuse: true,
			wantMsg:    "reports 0 compute units",
		},
		{
			name:       "odd compute unit count cannot be paired into WGPs",
			topology:   Topology{Name: "gfx1101", CU: 61, SE: 3, SAPerSE: 2, XCC: 1},
			wantRefuse: true,
			wantMsg:    "is odd",
		},
		{
			name:       "no shader engines reported",
			topology:   Topology{Name: "gfx1101", CU: 60, SE: 0, SAPerSE: 2, XCC: 1},
			wantRefuse: true,
			wantMsg:    "reports 0 shader engines",
		},
		{
			name:       "more shader engines than WGPs",
			topology:   Topology{Name: "gfx1101", CU: 60, SE: 31, SAPerSE: 2, XCC: 1},
			wantRefuse: true,
			wantMsg:    "exceed its 30 WGPs",
		},
		{
			// The one field whose absence is unsafe rather than merely unknown: read as one, the
			// atom becomes a single CU and the window stops covering every XCC, which leaves the
			// XCCs it misses running unmasked — the silent fail-open this whole derivation exists
			// to avoid. binding/hsa cannot tell a failed read from a one-XCC part, so neither can
			// this, and an ambiguous answer is refused.
			name:       "an unreported XCC count is refused on the branch that reads it",
			topology:   Topology{Name: "gfx942", CU: 304, SE: 32, SAPerSE: 1, XCC: 0},
			wantRefuse: true,
			wantMsg:    "reports no XCC count",
		},
		{
			// The same zero on the other branch is not a hazard, because nothing there reads it.
			name:       "an unreported XCC count is ignored on RDNA, which never reads it",
			topology:   Topology{Name: "gfx1101", CU: 60, SE: 3, SAPerSE: 2, XCC: 0},
			wantRefuse: false,
		},
		{
			// A genuinely single-XCC CDNA part reports 1 and is served normally.
			name:       "a single-XCC CDNA part is served, not caught by that refusal",
			topology:   Topology{Name: "gfx90a", CU: 104, SE: 8, SAPerSE: 1, XCC: 1},
			wantRefuse: false,
		},
		{
			name:       "more XCCs than compute units",
			topology:   Topology{Name: "gfx942", CU: 4, SE: 32, SAPerSE: 1, XCC: 8},
			wantRefuse: true,
			wantMsg:    "does not divide 4 compute units",
		},
		{
			name:       "compute units not divisible by the quantum",
			topology:   Topology{Name: "gfx1101", CU: 62, SE: 3, SAPerSE: 2, XCC: 1},
			wantRefuse: true,
			wantMsg:    "does not divide 62 compute units",
		},
		{
			name:       "unrecognised architecture family",
			topology:   Topology{Name: "gfx803", CU: 64, SE: 4, SAPerSE: 1, XCC: 1},
			wantRefuse: true,
			wantMsg:    "unrecognised GPU architecture family",
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.topology.Validate()
			switch {
			case tc.wantRefuse && err == nil:
				t.Fatalf("Validate() = no error, want a refusal")
			case !tc.wantRefuse && err != nil:
				t.Fatalf("Validate() = %v, want no error", err)
			}
			if tc.wantMsg != "" && !strings.Contains(err.Error(), tc.wantMsg) {
				t.Errorf("error = %q, want it to contain %q", err.Error(), tc.wantMsg)
			}
			// A refused topology must also refuse a derivation, without panicking.
			if _, err = WindowCUs(tc.topology, 50); tc.wantRefuse != (err != nil) {
				t.Errorf("WindowCUs() error = %v, want a refusal: %t", err, tc.wantRefuse)
			}
		})
	}
}

// TestPackWindow pins where a window lands on a card that already carries some.
func TestPackWindow(t *testing.T) {
	testCases := []struct {
		name      string
		topology  Topology
		length    int
		occupied  []workercore.AcceleratorPhysicalPlacement
		wantStart int32
	}{
		{
			name:      "an empty card places at zero",
			topology:  rdna,
			length:    12,
			wantStart: 0,
		},
		{
			name:      "a card carrying one window places the next after it",
			topology:  rdna,
			length:    12,
			occupied:  []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 12}},
			wantStart: 12,
		},
		{
			name:     "a hole in the middle is reused ahead of the tail",
			topology: rdna,
			length:   6,
			occupied: []workercore.AcceleratorPhysicalPlacement{
				{Start: 0, Length: 6},
				{Start: 12, Length: 6},
			},
			wantStart: 6,
		},
		{
			name:      "a full card takes the least-overlapped start",
			topology:  rdna,
			length:    42,
			occupied:  []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 30}},
			wantStart: 18,
		},
		{
			// The occupancy union appends reservation intervals to annotation intervals, so a live
			// allocation normally appears twice. Unmerged, the duplicate would make start 0 look
			// three times as crowded as start 30 and move the window; merged, every start overlaps
			// the same 30 CUs and the tie goes to the lowest.
			name:     "the same allocation twice does not bias the least-overlap choice",
			topology: rdna,
			length:   30,
			occupied: []workercore.AcceleratorPhysicalPlacement{
				{Start: 0, Length: 60},
				{Start: 0, Length: 30},
				{Start: 0, Length: 30},
			},
			wantStart: 0,
		},
		{
			name:      "a CDNA window starts on an XCC atom boundary",
			topology:  cdna,
			length:    8,
			occupied:  []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 8}},
			wantStart: 8,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got := PackWindow(tc.topology, tc.length, tc.occupied)
			if got.Start != tc.wantStart {
				t.Errorf("Start = %d, want %d", got.Start, tc.wantStart)
			}
			if got.Length != int32(tc.length) {
				t.Errorf("Length = %d, want %d", got.Length, tc.length)
			}
		})
	}
}

// TestPackWindow_QuantisedOnBothAxes pins that every emitted window is a multiple of its branch's
// quantum on both axes and stays on the card: an unquantised start splits a WGP pair on RDNA and
// breaks XCC coverage on CDNA, and either loses the whole mask.
func TestPackWindow_QuantisedOnBothAxes(t *testing.T) {
	testCases := []struct {
		name     string
		topology Topology
		occupied []workercore.AcceleratorPhysicalPlacement
	}{
		{name: "RDNA, empty card", topology: rdna},
		{
			name:     "RDNA, partly occupied",
			topology: rdna,
			occupied: []workercore.AcceleratorPhysicalPlacement{{Start: 6, Length: 12}},
		},
		{
			name:     "RDNA, full card",
			topology: rdna,
			occupied: []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 60}},
		},
		{name: "CDNA, empty card", topology: cdna},
		{
			name:     "CDNA, full card",
			topology: cdna,
			occupied: []workercore.AcceleratorPhysicalPlacement{{Start: 0, Length: 304}},
		},
		{name: "single-XCC CDNA, empty card", topology: cdnaSingleXCC},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			q := int32(tc.topology.Quantum())
			for pct := 1; pct <= 100; pct++ {
				length, err := WindowCUs(tc.topology, pct)
				if err != nil {
					continue // Refused below one quantum; covered by its own case.
				}
				w := PackWindow(tc.topology, length, tc.occupied)
				if w.Start%q != 0 || w.Length%q != 0 {
					t.Errorf("%d%%: window %+v is not a multiple of the %d-CU quantum", pct, w, q)
				}
				if w.Start < 0 || int(w.Start+w.Length) > tc.topology.CU {
					t.Errorf("%d%%: window %+v runs off a %d-CU card", pct, w, tc.topology.CU)
				}
			}
		})
	}
}
