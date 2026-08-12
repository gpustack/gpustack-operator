package amd

import (
	"fmt"
	"sort"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/mathx"
)

// Topology describes the one accelerator a mask is derived for, as the HSA agent-info API reports it.
//
// It carries no pointers and reads no device: every function below is closed-form integer
// arithmetic over these five fields, which is what makes the derivation testable against the
// conformance tables in
// .claude/skills/gpustack-operator-xbuild-and-verify/references/amd-cumask-conformance.md rather
// than against an accelerator. Reading the fields is a platform seam and lives elsewhere.
type Topology struct {
	// Name is the HSA agent name, the gfx string ("gfx1101", "gfx942", "gfx90a"). It selects the
	// arithmetic family, and an unrecognized one is refused rather than guessed at.
	Name string

	// CU is COMPUTE_UNIT_COUNT, the whole accelerator's compute units. Mask bit indices are CU indices.
	CU int

	// SE is NUM_SHADER_ENGINES. It is device-wide and already multiplied by the XCC count: an
	// accelerator reporting 32 with NUM_XCC=8 has four shader engines per XCC.
	SE int

	// SAPerSE is NUM_SHADER_ARRAYS_PER_SE. Recorded because the topology is read as a unit; no
	// branch of the derivation uses it.
	SAPerSE int

	// XCC is NUM_XCC. Zero means the field was not reported, which Validate refuses on the CDNA/GCN
	// branch rather than reading as one; the RDNA branch never reads it.
	XCC int
}

// Validate reports whether a mask can be derived for this accelerator at all.
//
// binding/hsa turns an agent-info field it cannot read into a zero rather than an error, so
// "absent" arrives here indistinguishable from "zero". Both must fail closed: a mask derived from a
// zero divides by it or confines nothing, and the platform reports neither — a rejected mask
// produces no error, no log line and no changed return code, so the cost is the loss of all
// isolation rather than a smaller slice.
func (t Topology) Validate() error {
	if t.CU <= 0 {
		return fmt.Errorf("cannot derive a CU mask for %q: it reports %d compute units", t.Name, t.CU)
	}
	cdna, known := t.family()
	if !known {
		return fmt.Errorf("cannot derive a CU mask for %q: unrecognised GPU architecture family, "+
			"only gfx9* (CDNA/GCN) and gfx10*/gfx11*/gfx12* (RDNA) are supported", t.Name)
	}
	if cdna {
		// The XCC count is the CDNA atom, and it is the one field whose absence is unsafe rather
		// than merely unknown: read as one, the atom becomes a single CU, the emitted window stops
		// covering every XCC, and the XCCs it misses run UNMASKED — measured, a one-bit mask left
		// 267 of 304 CUs reachable while reading as a plausible 3.7% of throughput. binding/hsa
		// cannot tell "this part has one XCC" from "this read failed", so neither can this, and the
		// safe reading of an ambiguous answer is to refuse. A genuinely single-XCC CDNA part
		// (gfx90a) reports 1 and is served normally.
		if t.XCC <= 0 {
			return fmt.Errorf("cannot derive a CU mask for %q: it reports no XCC count, and on this "+
				"architecture an unknown one cannot be assumed to be 1 — a mask sized for one XCC "+
				"leaves every other XCC unmasked", t.Name)
		}
	} else {
		// The RDNA atom is the WGP, a pair of CUs, and the mask is discarded whole when a pair is
		// split — so an odd CU count or an engine count no set of pairs can be spread over leaves
		// nothing safe to emit. XCC is not read on this branch at all.
		if t.SE <= 0 {
			return fmt.Errorf("cannot derive a CU mask for %q: it reports %d shader engines", t.Name, t.SE)
		}
		if t.CU%2 != 0 {
			return fmt.Errorf("cannot derive a CU mask for %q: %d compute units is odd, and RDNA "+
				"allocates in WGP pairs", t.Name, t.CU)
		}
		if t.SE > t.CU/2 {
			return fmt.Errorf("cannot derive a CU mask for %q: %d shader engines exceed its %d WGPs",
				t.Name, t.SE, t.CU/2)
		}
	}
	// Windows tile only when the quantum divides the accelerator: both a window's start and its
	// length are multiples of it, so a remainder would leave a tail that no start can address.
	if q := t.Quantum(); t.CU%q != 0 {
		return fmt.Errorf("cannot derive a CU mask for %q: its %d-CU quantum does not divide %d "+
			"compute units", t.Name, q, t.CU)
	}
	return nil
}

// IsCDNAFamily reports whether the accelerator takes the CDNA/GCN arithmetic.
//
// The branch is the ARCHITECTURE FAMILY, not the XCC count, and that distinction is the whole
// correctness of this file. gfx90a — MI210, and each GCD of an MI250X — is CDNA silicon that
// reports NUM_XCC = 1, so an "XCC > 1 means CDNA" test hands it to the RDNA branch and applies WGP
// pairing to a part that has no WGPs. csrc/amd/rocm-slicing-shim/build.sh compiles device code for
// gfx90a, so it is a part this product expects to meet.
//
// An unrecognized name reports false; Validate is what refuses it.
func (t Topology) IsCDNAFamily() bool {
	cdna, _ := t.family()
	return cdna
}

// Quantum is the smallest number of CUs a window may start at or span, on this accelerator.
//
// On RDNA it is one full round-robin round of WGP pairs (2*SE): the kernel hands mask bits to
// shader engines round-robin, and a start that is not on a round boundary splits a WGP pair, which
// makes ROCr discard the entire mask. On CDNA it is one "one CU in every XCC" atom (NUM_XCC),
// because bit i lands on XCC i mod X, so only a multiple of X preserves full XCC coverage at any
// offset.
//
// The result is meaningful only for a topology Validate accepts.
func (t Topology) Quantum() int {
	if t.IsCDNAFamily() {
		return t.xcc()
	}
	return 2 * t.SE
}

// MinPercent is the smallest percentage this accelerator can honor, i.e. the smallest integer whose
// window is one quantum rather than nothing. It is the actionable half of a refusal message.
//
// The result is meaningful only for a topology Validate accepts.
func (t Topology) MinPercent() int {
	units, atom := t.CU, t.xcc()
	if !t.IsCDNAFamily() {
		units, atom = t.CU/2, t.SE
	}
	// The alignment keeps a request whose rounded unit count reaches one atom, and RoundDiv rounds
	// half up, so the smallest such percentage is ceil((100*atom - 50) / units). It is always at
	// least 1, because the numerator exceeds the denominator whenever the accelerator has an atom
	// at all.
	return mathx.CeilDiv(100*atom-50, units)
}

// WindowCUs turns a requested percentage of the accelerator into a window length in CUs.
//
// The two arithmetics share nothing and neither degrades safely into the other: carrying RDNA's
// pairing rule onto CDNA does not fail, it doubles every slice; carrying CDNA's atom onto RDNA
// splits WGP pairs, and a split pair loses the mask entirely.
func WindowCUs(t Topology, pct int) (int, error) {
	if err := t.Validate(); err != nil {
		return 0, err
	}
	if pct <= 0 || pct > 100 {
		return 0, fmt.Errorf("cannot derive a CU mask for %q: %d%% is not a percentage of a card",
			t.Name, pct)
	}

	if t.IsCDNAFamily() {
		// Mask bits interleave across XCCs — bit i lands on XCC i mod X — so the atom is X CUs and
		// a bit count below one atom does NOT clamp down to a small slice: the XCCs it never
		// mentions run unmasked. Measured, "0:0" occupied 267 of 304 CUs while reading as a
		// plausible 3.7% of throughput. On a single-XCC part (gfx90a) X is 1 and this degenerates
		// to a contiguous window of any length — the least-assuming of the three rules.
		x := t.xcc()
		n := mathx.RoundDiv(t.CU*pct, 100) / x * x
		if n == 0 {
			return 0, fmt.Errorf("cannot derive a CU mask for %q: %d%% of %d CUs is below one "+
				"%d-CU atom on this card; its smallest slice is %d%%",
				t.Name, pct, t.CU, x, t.MinPercent())
		}
		return min(n, t.CU), nil
	}

	// RDNA allocates in WGP pairs, and the kernel spreads mask bits round-robin across shader
	// engines, so a WGP count that is not a multiple of SE yields no throughput for the remainder.
	// The C reference implementation (rocm_cumask_check.c, derive_and_reexec) clamps a sub-round
	// request UP to one round; this refuses instead, deliberately: a 1% request on a 60 CU / 3 SE
	// accelerator would otherwise take a 10% compute ceiling while Kueue charges 1%, and an accounting
	// mismatch that silently favors the tenant is not better than a refusal naming the number.
	wgps := t.CU / 2
	n := mathx.RoundDiv(wgps*pct, 100) / t.SE * t.SE
	if n == 0 {
		return 0, fmt.Errorf("cannot derive a CU mask for %q: %d%% of %d WGPs is below one "+
			"%d-WGP round on this card; its smallest slice is %d%%",
			t.Name, pct, wgps, t.SE, t.MinPercent())
	}
	return 2 * min(n, wgps), nil
}

// PackWindow chooses where a window of length CUs sits on an accelerator already carrying occupied.
//
// A mask is a position, not only a size: two containers handed the same window share it rather
// than the accelerator. So the lowest-indexed free quantised start wins, and only when the
// accelerator is full does the choice fall back to the quantised start overlapping the fewest
// covered CUs, ties broken by the lowest start.
//
// The occupancy is merged before it is measured. The caller unions reservation intervals onto
// annotation intervals, so a live allocation legitimately appears twice — harmless for a binary
// overlap test, a systematic bias for anything that counts. What merging cannot preserve is tenant
// multiplicity, so an already twice-shared window and a once-shared one look alike; that is
// accepted for a fallback that only runs on a full accelerator.
//
// The window is always within [0, CU): a length beyond the accelerator is clamped to it, since the
// caller has no error to return here and an out-of-range mask bit is dropped silently by ROCr.
func PackWindow(
	t Topology,
	length int,
	occupied []workercore.AcceleratorPlacement,
) workercore.AcceleratorPlacement {
	if length <= 0 || t.CU <= 0 {
		return workercore.AcceleratorPlacement{}
	}
	q := t.Quantum()
	if q <= 0 {
		q = 1
	}
	length = min(length, t.CU)

	covered := mergePlacements(occupied, t.CU)

	best, bestOverlap := 0, -1
	for start := 0; start+length <= t.CU; start += q {
		overlap := coveredCUs(covered, start, start+length)
		if overlap == 0 {
			return workercore.AcceleratorPlacement{
				Start:  int32(start),
				Length: int32(length),
			}
		}
		if bestOverlap < 0 || overlap < bestOverlap {
			best, bestOverlap = start, overlap
		}
	}
	return workercore.AcceleratorPlacement{
		Start:  int32(best),
		Length: int32(length),
	}
}

// Mask renders one accelerator's window as an HSA_CU_MASK segment, "<index>:<lo>-<hi>", where
// index is the accelerator's position in ROCR_VISIBLE_DEVICES and lo/hi are the window's first and
// last CU bits.
func Mask(index int, w workercore.AcceleratorPlacement) string {
	return fmt.Sprintf("%d:%d-%d", index, w.Start, w.Start+w.Length-1)
}

// family reports the arithmetic family of the gfx name, and whether it was recognized at all.
func (t Topology) family() (cdna, known bool) {
	switch {
	case strings.HasPrefix(t.Name, "gfx10"),
		strings.HasPrefix(t.Name, "gfx11"),
		strings.HasPrefix(t.Name, "gfx12"):
		return false, true
	case strings.HasPrefix(t.Name, "gfx9"):
		return true, true
	}
	return false, false
}

// xcc is the XCC count, floored at one so Quantum stays usable on a topology nobody validated —
// PackWindow guards against a zero quantum, and the RDNA branch never reads this. It is NOT the
// place that decides whether an unreported count is acceptable: Validate refuses it on the branch
// where it is load-bearing, and this floor must never become that decision by accident.
func (t Topology) xcc() int {
	return max(t.XCC, 1)
}

// mergePlacements returns the occupied intervals clamped to [0, cu) as a sorted, disjoint set.
func mergePlacements(
	in []workercore.AcceleratorPlacement,
	cu int,
) []workercore.AcceleratorPlacement {
	out := make([]workercore.AcceleratorPlacement, 0, len(in))
	for _, p := range in {
		start, end := max(int(p.Start), 0), min(int(p.Start)+int(p.Length), cu)
		if end <= start {
			continue
		}
		out = append(out, workercore.AcceleratorPlacement{
			Start:  int32(start),
			Length: int32(end - start),
		})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Start < out[j].Start })

	merged := out[:0]
	for _, p := range out {
		if n := len(merged); n > 0 {
			last := &merged[n-1]
			if p.Start <= last.Start+last.Length {
				last.Length = max(last.Length, p.Start+p.Length-last.Start)
				continue
			}
		}
		merged = append(merged, p)
	}
	return merged
}

// coveredCUs counts the CUs of [start, end) that the merged set covers.
func coveredCUs(merged []workercore.AcceleratorPlacement, start, end int) int {
	total := 0
	for _, p := range merged {
		lo, hi := max(int(p.Start), start), min(int(p.Start)+int(p.Length), end)
		if hi > lo {
			total += hi - lo
		}
	}
	return total
}
