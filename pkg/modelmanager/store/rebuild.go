package store

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// Mount is one line of /proc/self/mountinfo, reduced to what the rebuild reads.
type Mount struct {
	// Device is the "major:minor" of the filesystem the mount comes from.
	Device string
	// Root is the path, inside that filesystem, of the directory mounted at MountPoint: for a bind
	// mount, its source. It is how a target is traced back to a digest without the ledger.
	Root string
	// MountPoint is where the mount is visible.
	MountPoint string
}

// ParseMountInfo reads mountinfo lines.
func ParseMountInfo(r io.Reader) ([]Mount, error) {
	var mounts []Mount
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		if len(f) < 5 {
			return nil, fmt.Errorf("short mountinfo line %q", sc.Text())
		}
		mounts = append(mounts, Mount{Device: f[2], Root: unescapeMountInfo(f[3]), MountPoint: unescapeMountInfo(f[4])})
	}

	return mounts, sc.Err()
}

// unescapeMountInfo undoes the octal escapes mountinfo writes for space, tab, newline and backslash.
func unescapeMountInfo(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+4 <= len(s) {
			if v, err := strconv.ParseUint(s[i+1:i+4], 8, 8); err == nil {
				b.WriteByte(byte(v))
				i += 3
				continue
			}
		}
		b.WriteByte(s[i])
	}

	return b.String()
}

var (
	// publishedRootPattern is a bind mount's source inside the cache filesystem: the root's own
	// path within that filesystem is a prefix only when the cache shares it with others.
	publishedRootPattern = regexp.MustCompile(`/` + publishedDir + `/([0-9a-f]{64})/` + treeDir + `$`)
	// csiTargetPattern is kubelet's target of a CSI volume,
	// <kubelet>/pods/<pod UID>/volumes/kubernetes.io~csi/<volume>/mount.
	csiTargetPattern = regexp.MustCompile(`/pods/([^/]+)/volumes/kubernetes\.io~csi/([^/]+)/mount$`)
)

// Rebuilt is the reference table reconstructed at start.
type Rebuilt struct {
	// Kept are in the ledger and mounted from the digest the ledger names.
	Kept []Ref
	// Corrected are in the ledger but mounted from another digest; the mount wins, since it is
	// what the consumer sees.
	Corrected []Ref
	// Adopted are mounted without a ledger record.
	Adopted []Ref
	// Dropped are in the ledger but not mounted.
	Dropped []Ref
}

// References is every reference the rebuild keeps, by target path.
func (r Rebuilt) References() []Ref {
	refs := make([]Ref, 0, len(r.Kept)+len(r.Corrected)+len(r.Adopted))
	refs = append(append(append(refs, r.Kept...), r.Corrected...), r.Adopted...)
	sort.Slice(refs, func(i, j int) bool { return refs[i].TargetPath < refs[j].TargetPath })

	return refs
}

// Rebuild reconstructs the references from the mounts and the ledger, and rewrites the ledger to
// match. device is the cache filesystem's "major:minor", so a bind mount of an unrelated directory
// that happens to end in published/<hex>/tree is not taken for one of the cache's.
func (s *Store) Rebuild(mounts []Mount, device string) (Rebuilt, error) {
	mounted := map[string]string{}
	for _, m := range mounts {
		if m.Device != device || !csiTargetPattern.MatchString(m.MountPoint) {
			continue
		}
		if g := publishedRootPattern.FindStringSubmatch(m.Root); g != nil {
			mounted[m.MountPoint] = g[1]
		}
	}

	ledger, err := s.Refs()
	if err != nil {
		return Rebuilt{}, err
	}
	var out Rebuilt
	seen := map[string]bool{}
	for _, r := range ledger {
		seen[r.TargetPath] = true
		hex, ok := mounted[r.TargetPath]
		switch {
		case !ok:
			out.Dropped = append(out.Dropped, r)
			if err := s.RemoveRef(r.VolumeID); err != nil {
				return out, err
			}
		case hex != r.Hex:
			r.Hex = hex
			out.Corrected = append(out.Corrected, r)
			if err := s.WriteRef(r); err != nil {
				return out, err
			}
		default:
			out.Kept = append(out.Kept, r)
		}
	}

	targets := make([]string, 0, len(mounted))
	for t := range mounted {
		if !seen[t] {
			targets = append(targets, t)
		}
	}
	sort.Strings(targets)
	for _, t := range targets {
		g := csiTargetPattern.FindStringSubmatch(t)
		r := Ref{TargetPath: t, Hex: mounted[t], PodUID: g[1], VolumeID: EphemeralVolumeID(g[1], g[2])}
		out.Adopted = append(out.Adopted, r)
		if err := s.WriteRef(r); err != nil {
			return out, err
		}
	}

	return out, nil
}
