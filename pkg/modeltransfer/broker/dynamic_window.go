package broker

import (
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// MinSourceBufferBytes is the smallest source buffer this node still
// serves: below it the dynamic window refuses and a client falls back to
// its local or peer-to-peer path.
const MinSourceBufferBytes int64 = 1 << 30

// MemoryAlignmentBytes is the 2 MiB boundary every storage and transfer
// memory figure aligns to across platforms.
const MemoryAlignmentBytes int64 = 2 << 20

// AlignDown2M rounds a positive byte count down to the 2 MiB boundary.
func AlignDown2M(n int64) int64 {
	if n <= 0 {
		return 0
	}
	return (n / MemoryAlignmentBytes) * MemoryAlignmentBytes
}

// DefaultMemInfoPath is the meminfo-format file the broker reads its
// node's memory figures from.
const DefaultMemInfoPath = "/proc/meminfo"

// hasQuarterHeadroom reports whether avail clears window with a quarter of
// the window to spare. The comparison is shaped so no intermediate
// overflows: the difference is only taken once avail is known to exceed
// the window.
func hasQuarterHeadroom(avail, window int64) bool {
	if avail <= window {
		return false
	}
	return avail-window > window/4
}

// ComputeDynamicSourceBuffer resolves the transfer buffer ceiling this
// node can offer right now out of its available RAM, so a loaded node
// shrinks what it promises instead of overcommitting host memory it does
// not have.
//
// The ceiling starts at maxMemoryBytes, floored at minMemoryBytes, which
// is itself floored at 1 GiB. More than a quarter of the ceiling free
// above it keeps the ceiling. Otherwise it shrinks by a quarter per step
// until either the headroom recovers or the available RAM at least clears
// the step plain; the floor caps the shrink. Every ceiling and shrink step
// aligns down to 2 MiB. When even the floor does not fit, the window is
// refused: the caller then declines to build an RDMA transfer, and the
// client falls back to its local or peer-to-peer path.
func ComputeDynamicSourceBuffer(availRAM, maxMemoryBytes, minMemoryBytes int64) (int64, bool) {
	minMemoryBytes = AlignDown2M(minMemoryBytes)
	if minMemoryBytes < MinSourceBufferBytes {
		minMemoryBytes = MinSourceBufferBytes
	}
	maxMemoryBytes = AlignDown2M(maxMemoryBytes)
	if maxMemoryBytes < minMemoryBytes {
		maxMemoryBytes = minMemoryBytes
	}
	// A ceiling past a quarter of the address space can never be backed by
	// real RAM, and its own quarter-headroom arithmetic would overflow.
	maxCap := AlignDown2M(math.MaxInt64 / 4)
	if maxMemoryBytes > maxCap {
		maxMemoryBytes = maxCap
	}
	if availRAM <= minMemoryBytes {
		return 0, false
	}
	if hasQuarterHeadroom(availRAM, maxMemoryBytes) {
		return maxMemoryBytes, true
	}
	newMax := maxMemoryBytes
	for newMax > minMemoryBytes {
		newMax = max(AlignDown2M(newMax*3/4), minMemoryBytes)
		if hasQuarterHeadroom(availRAM, newMax) || availRAM > newMax {
			break
		}
	}
	if availRAM <= newMax {
		return 0, false
	}
	return newMax, true
}

// memInfoFieldBytes reads one kB figure out of a meminfo-format file and
// returns it in bytes. It is the one parser and one error path for every
// meminfo reader: an unreadable file, an absent field, a malformed line,
// and an out-of-range figure each answer an explicit error that names its
// cause, so a caller can tell "the node's memory figures are unavailable"
// from "the figures say zero". The byte answer is always zero on an error,
// so every caller keeps its fail-closed bound.
func memInfoFieldBytes(path, field string) (int64, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(raw), "\n") {
		value, ok := strings.CutPrefix(line, field)
		if !ok {
			continue
		}
		fields := strings.Fields(value)
		if len(fields) != 2 || fields[1] != "kB" {
			return 0, fmt.Errorf("the %s line in %s is malformed: %q", field, path, line)
		}
		kb, parseErr := strconv.ParseInt(fields[0], 10, 64)
		if parseErr != nil {
			return 0, fmt.Errorf("the %s line in %s is malformed: %w", field, path, parseErr)
		}
		if kb < 0 || kb > math.MaxInt64/1024 {
			return 0, fmt.Errorf("the %s figure in %s is outside the byte range: %d kB", field, path, kb)
		}
		return kb * 1024, nil
	}
	return 0, fmt.Errorf("%s does not name %q", path, field)
}

// MemInfoTotalBytes reads the total RAM figure, in bytes, out of a
// meminfo-format file; zero when the file or the figure is unusable, so
// the caller falls back to its own floor. The error answer stays internal
// to this package on purpose: this signature is the established default's
// fail-closed contract, and callers that need the cause read the parser.
func MemInfoTotalBytes(path string) int64 {
	total, _ := memInfoFieldBytes(path, "MemTotal:")
	return total
}

// memInfoAvailableBytes reads the node's available RAM, in bytes, with the
// cause of any failure: a container without the file, a misdirected path,
// and a malformed figure answer distinct errors instead of one silent zero
// that a real memory-exhaustion answer cannot be told apart from.
func memInfoAvailableBytes(path string) (int64, error) {
	return memInfoFieldBytes(path, "MemAvailable:")
}
