package hygon

import (
	"bufio"
	"fmt"
	"io/fs"
	"math"
	"math/bits"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"gpustack.ai/gpustack/pkg/utils/osx"
)

// Bounds of the DTK hy-virtual scheme: vdev ids run below 200 and at most 20 pipe ids are
// allocatable per accelerator (the per-accelerator ≤ 4-slice bound is enforced upstream by the
// sliced token pool). Only pipe ids are a pool -- a vdev id is the ordinal of the record's own
// file, see allocateVdev. An accelerator's compute units are addressed as a 128-bit mask (two
// cu_mask words).
const (
	maxVdevID  = 200
	maxPipeID  = 20
	cuWordBits = 64
)

// allocMu serializes the whole scan -> validate -> allocate -> write cycle. The
// device-plugin responder runs outside the reconciler's allocateMutex and kubelet does
// not reliably serialize Allocate (concurrent Kueue batches), so without this two Hygon
// allocations could read the same on-disk slot set and pack overlapping CU masks / reuse
// a vdev id.
var allocMu sync.Mutex

// cuMask is an accelerator's 128-bit compute-unit bitmask, split into the two 64-bit words the
// vdev.conf carries as `cu_mask`. Bit c marks compute unit c: word lo covers CU 0..63,
// word hi covers CU 64..127.
type cuMask struct {
	lo, hi uint64
}

func (m cuMask) has(cu int) bool {
	if cu < cuWordBits {
		return m.lo&(uint64(1)<<uint(cu)) != 0
	}
	return m.hi&(uint64(1)<<uint(cu-cuWordBits)) != 0
}

func (m *cuMask) set(cu int) {
	if cu < cuWordBits {
		m.lo |= uint64(1) << uint(cu)
		return
	}
	m.hi |= uint64(1) << uint(cu-cuWordBits)
}

func (m cuMask) or(o cuMask) cuMask {
	return cuMask{lo: m.lo | o.lo, hi: m.hi | o.hi}
}

func (m cuMask) count() int {
	return bits.OnesCount64(m.lo) + bits.OnesCount64(m.hi)
}

// fullMask is the mask of every compute unit an accelerator of the given CU count owns; a
// used mask must be a subset of it (a bit beyond the accelerator's CUs is a corrupt record).
func fullMask(cores int) cuMask {
	var m cuMask
	for cu := 0; cu < cores; cu++ {
		m.set(cu)
	}
	return m
}

// sliceCUCount converts a compute percent to a CU count against the accelerator's total CU
// count: ceil so a positive percent never rounds down to zero, capped at the accelerator
// total.
func sliceCUCount(coresPct int, cores uint32) int {
	return min(int(math.Ceil(float64(coresPct)*float64(cores)/100)), int(cores))
}

// packCUMask picks the cuCount lowest free compute units in [0, cores) not already set in
// used, returning the new (disjoint) mask. It fails closed: a used mask referencing a CU
// beyond the accelerator, or fewer than cuCount free CUs, is an error rather than a silent
// overlap or short mask.
func packCUMask(used cuMask, cuCount, cores int) (cuMask, error) {
	if cores > 2*cuWordBits {
		return cuMask{}, fmt.Errorf("card compute unit count %d exceeds the 128-bit cu_mask width", cores)
	}
	full := fullMask(cores)
	if used.lo&^full.lo != 0 || used.hi&^full.hi != 0 {
		return cuMask{}, fmt.Errorf("used cu_mask references a compute unit beyond the card's %d", cores)
	}
	var m cuMask
	picked := 0
	for cu := 0; cu < cores && picked < cuCount; cu++ {
		if used.has(cu) {
			continue
		}
		m.set(cu)
		picked++
	}
	if picked < cuCount {
		return cuMask{}, fmt.Errorf("insufficient free compute units: need %d, %d of %d free", cuCount, cores-used.count(), cores)
	}
	return m, nil
}

// vdevConf is one parsed vdev.conf record: an accelerator's slice reservation the DTK/hyhal
// runtime reads and the on-disk scanner treats as occupied.
type vdevConf struct {
	pciBusID string
	mask     cuMask
	cuCount  int
	memMib   int64
	deviceID int
	vdevID   int
	pipeID   int
}

// render serializes the record in the exact schema the DTK/hyhal runtime expects.
func (c vdevConf) render() string {
	var b strings.Builder
	_, _ = fmt.Fprintf(&b, "PciBusId: %s\n", c.pciBusID)
	_, _ = fmt.Fprintf(&b, "cu_mask: 0x%016x\n", c.mask.lo)
	_, _ = fmt.Fprintf(&b, "cu_mask: 0x%016x\n", c.mask.hi)
	_, _ = fmt.Fprintf(&b, "cu_count: %d\n", c.cuCount)
	_, _ = fmt.Fprintf(&b, "mem: %d MiB\n", c.memMib)
	_, _ = fmt.Fprintf(&b, "device_id: %d\n", c.deviceID)
	_, _ = fmt.Fprintf(&b, "vdev_id: %d\n", c.vdevID)
	_, _ = fmt.Fprintf(&b, "pipe_id: %d\n", c.pipeID)
	b.WriteString("enable: 1\n")
	return b.String()
}

// parseVdevConf reads a vdev.conf fail-closed: a missing / duplicate / malformed /
// out-of-range field is an error, so the scanner never silently frees a live slice's
// vdev_id / pipe_id / CU mask by skipping a record it could not read.
func parseVdevConf(path string) (vdevConf, error) {
	f, err := os.Open(path)
	if err != nil {
		return vdevConf{}, err
	}
	defer func() { _ = f.Close() }()

	var (
		c     vdevConf
		masks []uint64
		seen  = map[string]bool{}
	)
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		if strings.TrimSpace(line) == "" {
			continue
		}
		key, val, found := strings.Cut(line, ":")
		if !found {
			return vdevConf{}, fmt.Errorf("vdev.conf %q: malformed line %q", path, line)
		}
		key, val = strings.TrimSpace(key), strings.TrimSpace(val)
		if key != "cu_mask" && seen[key] {
			return vdevConf{}, fmt.Errorf("vdev.conf %q: duplicate key %q", path, key)
		}
		seen[key] = true

		switch key {
		case "PciBusId":
			c.pciBusID = val
		case "cu_mask":
			m, err := strconv.ParseUint(strings.TrimPrefix(val, "0x"), 16, 64)
			if err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad cu_mask %q: %w", path, val, err)
			}
			masks = append(masks, m)
		case "cu_count":
			if c.cuCount, err = strconv.Atoi(val); err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad cu_count %q: %w", path, val, err)
			}
		case "mem":
			num, _, _ := strings.Cut(val, " ")
			if c.memMib, err = strconv.ParseInt(num, 10, 64); err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad mem %q: %w", path, val, err)
			}
		case "device_id":
			if c.deviceID, err = strconv.Atoi(val); err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad device_id %q: %w", path, val, err)
			}
		case "vdev_id":
			if c.vdevID, err = strconv.Atoi(val); err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad vdev_id %q: %w", path, val, err)
			}
		case "pipe_id":
			if c.pipeID, err = strconv.Atoi(val); err != nil {
				return vdevConf{}, fmt.Errorf("vdev.conf %q: bad pipe_id %q: %w", path, val, err)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return vdevConf{}, fmt.Errorf("vdev.conf %q: %w", path, err)
	}

	// Only the slot-derivation fields are required: PciBusId, both cu_mask words, cu_count,
	// vdev_id, pipe_id. mem and device_id are informational — render always emits them, but a
	// scan reconstructing the used vdev/pipe/CU sets does not read them — so they are not
	// enforced here.
	if c.pciBusID == "" || len(masks) != 2 || !seen["cu_count"] || !seen["vdev_id"] || !seen["pipe_id"] {
		return vdevConf{}, fmt.Errorf("vdev.conf %q: incomplete record", path)
	}
	if c.vdevID < 0 || c.vdevID >= maxVdevID || c.pipeID < 0 || c.pipeID >= maxPipeID {
		return vdevConf{}, fmt.Errorf("vdev.conf %q: vdev_id %d / pipe_id %d out of range", path, c.vdevID, c.pipeID)
	}
	c.mask = cuMask{lo: masks[0], hi: masks[1]}
	return c, nil
}

// scanVdevConfs parses every per-pod vdev.conf under the pods dir
// (<podsDir>/<podUID>/c-<container>/etc/vdev/docker/vdev*.conf) except selfPath. It is
// fail-closed: an unreadable directory or a corrupt record errors the whole scan rather
// than being skipped — either would silently free a live slice's slot and let a later
// allocation double-book it. filepath.Glob is deliberately avoided because it swallows
// directory read errors (reporting only ErrBadPattern), which would drop a live conf.
func scanVdevConfs(podsDir, selfPath string) ([]vdevConf, error) {
	confs := make([]vdevConf, 0)
	err := filepath.WalkDir(podsDir, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			// A missing pods dir means no slices yet; any other read error fails closed.
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if d.IsDir() || path == selfPath {
			return nil
		}
		name := d.Name()
		if !strings.HasPrefix(name, "vdev") || !strings.HasSuffix(name, ".conf") {
			return nil
		}
		c, perr := parseVdevConf(path)
		if perr != nil {
			return perr
		}
		confs = append(confs, c)
		return nil
	})
	if err != nil {
		return nil, err
	}
	return confs, nil
}

// allocateVdev derives an accelerator's vdev.conf slot from the on-disk scan and publishes it
// atomically at selfPath. It is idempotent — re-allocating the same selfPath for the same
// accelerator reuses the existing record — and level-based: a dead pod's reclaimed work dir frees
// its slot on the next scan, so no in-memory counter or Release callback is needed. The
// whole scan -> validate -> allocate -> write runs under allocMu because concurrent
// Allocates are not serialized upstream.
func allocateVdev(podsDir, selfPath, pciBusID string, cores uint32, coresPct int, memMib int64, deviceID int) (vdevConf, error) {
	allocMu.Lock()
	defer allocMu.Unlock()

	// Idempotent reuse of an existing self-config for the same accelerator. The vdev id is checked
	// too, so a record written before vdev ids were tied to the file name -- which the runtime
	// rejects, see below -- is replaced rather than reused.
	if c, err := parseVdevConf(selfPath); err == nil && c.pciBusID == pciBusID && c.vdevID == deviceID {
		return c, nil
	}

	confs, err := scanVdevConfs(podsDir, selfPath)
	if err != nil {
		return vdevConf{}, err
	}

	// Aggregate the used slots: CU bits and pipe ids are per-accelerator, so only a record naming
	// the same card contributes. Vdev ids are not pooled at all -- see below.
	var usedCU cuMask
	usedPipe := make(map[int]bool)
	for i := range confs {
		if confs[i].pciBusID == pciBusID {
			usedCU = usedCU.or(confs[i].mask)
			usedPipe[confs[i].pipeID] = true
		}
	}

	n := sliceCUCount(coresPct, cores)
	if n <= 0 {
		return vdevConf{}, fmt.Errorf("card %s: sliced request resolves to zero compute units (cores-percent=%d, card cores=%d)", pciBusID, coresPct, cores)
	}
	mask, err := packCUMask(usedCU, n, int(cores))
	if err != nil {
		return vdevConf{}, err
	}
	// The vdev id is the record's own ordinal, not a slot drawn from a pool. The DTK/hyhal runtime
	// checks it against the ordinal in the file name it read the record from -- vdev<N>.conf must
	// carry vdev_id N -- and a mismatch is not a degraded slice: the container is left with no
	// accelerator at all ("No HIP GPUs are available"), measured on an 8-DCU host. A node-wide pool
	// would hand the second pod on a node an id its own file name contradicts, because every pod
	// numbers its own confs from zero.
	//
	// Node-wide uniqueness is not required to make up for that: two containers each holding vdev_id
	// 0 on one card run side by side, the runtime telling them apart by container id and numbering
	// its own instances (0x<gpu_id>@0 and @1 under the kfd vgpu sysfs). Pipe ids are the ones that
	// must not collide on a card, and they are still drawn from the scan above.
	vdevID := deviceID
	if vdevID < 0 || vdevID >= maxVdevID {
		return vdevConf{}, fmt.Errorf("card %s: device ordinal %d is not a usable vdev_id", pciBusID, vdevID)
	}
	pipeID, err := lowestFreeSlot(usedPipe, maxPipeID)
	if err != nil {
		return vdevConf{}, fmt.Errorf("pipe_id pool exhausted for card %s: %w", pciBusID, err)
	}

	c := vdevConf{
		pciBusID: pciBusID,
		mask:     mask,
		cuCount:  mask.count(),
		memMib:   memMib,
		deviceID: deviceID,
		vdevID:   vdevID,
		pipeID:   pipeID,
	}
	if err := writeVdevConf(selfPath, c); err != nil {
		return vdevConf{}, err
	}
	return c, nil
}

// lowestFreeSlot returns the lowest index in [0, limit) not present in used, or an error
// when the pool is exhausted.
func lowestFreeSlot(used map[int]bool, limit int) (int, error) {
	for i := 0; i < limit; i++ {
		if !used[i] {
			return i, nil
		}
	}
	return 0, fmt.Errorf("no free slot below %d", limit)
}

// writeVdevConf publishes a vdev.conf durably: a concurrent scanner never reads a partial
// record, and a record that has been written survives an unclean shutdown.
func writeVdevConf(path string, c vdevConf) error {
	dir := filepath.Dir(path)
	if err := osx.MkdirAll(dir, 0o777); err != nil {
		return fmt.Errorf("create vdev.conf dir %q: %w", dir, err)
	}
	data := []byte(c.render())
	if err := osx.DurableWrite(path, data, 0o644); err != nil {
		return fmt.Errorf("write vdev.conf %q: %w", path, err)
	}
	return nil
}
