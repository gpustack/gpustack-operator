package store

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	klog "k8s.io/klog/v2"
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
	// metadataViewRootPattern is a bind mount of a metadata view: the view directory under the
	// cache root's model-transfer/views/, named by the view's content address. A view mount never implies
	// a published tree; its digest is recovered from the view's own record.
	metadataViewRootPattern = regexp.MustCompile(`/` + viewsSubpath + `/([0-9a-f]{64})$`)
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

// References is every reference the rebuild keeps, by target path and volume ID.
func (r Rebuilt) References() []Ref {
	refs := make([]Ref, 0, len(r.Kept)+len(r.Corrected)+len(r.Adopted))
	refs = append(append(append(refs, r.Kept...), r.Corrected...), r.Adopted...)
	sort.Slice(refs, func(i, j int) bool {
		if refs[i].TargetPath != refs[j].TargetPath {
			return refs[i].TargetPath < refs[j].TargetPath
		}
		return refs[i].VolumeID < refs[j].VolumeID
	})

	return refs
}

// isModelTransferSourceRef reports whether r is a durable source-pool reference claimed
// by the model-transfer broker rather than a CSI bind mount.
func isModelTransferSourceRef(r Ref) bool {
	return r.TargetPath == "" && strings.HasPrefix(r.VolumeID, "model-transfer.")
}

// Rebuild reconstructs the references from the mounts and the ledger, and rewrites the ledger to
// match. device is the cache filesystem's "major:minor", so a bind mount of an unrelated directory
// that happens to end in published/<hex>/tree is not taken for one of the cache's. Durable
// source-pool references (model-transfer.<generation>) are owned by the source broker and kept across
// CSI mount rebuilds. Metadata view mounts are kept too: their digest comes from the view's own
// record, and keeping them never marks the full checkpoint published.
// Cancellation stops waiting for the claim locks before any ledger rewrite.
// Once the locks are held, per-reference writes and removals finish or return an I/O error;
// the rewrite is not one atomic transaction.
func (s *Store) Rebuild(ctx context.Context, mounts []Mount, device string) (Rebuilt, error) {
	mounted := map[string]string{}
	mountedViews := map[string]string{}
	for _, m := range mounts {
		if m.Device != device || !csiTargetPattern.MatchString(m.MountPoint) {
			continue
		}
		if g := publishedRootPattern.FindStringSubmatch(m.Root); g != nil {
			mounted[m.MountPoint] = g[1]
			continue
		}
		if g := metadataViewRootPattern.FindStringSubmatch(m.Root); g != nil {
			mountedViews[m.MountPoint] = g[1]
		}
	}

	if err := s.lockClaimMu(ctx); err != nil {
		return Rebuilt{}, err
	}
	defer s.unlockClaimMu()
	unlock, err := s.lockClaims(ctx)
	if err != nil {
		return Rebuilt{}, err
	}
	defer unlock()

	ledger, err := s.Refs()
	if err != nil {
		return Rebuilt{}, err
	}
	var out Rebuilt
	seen := map[string]bool{}
	for _, r := range ledger {
		if isModelTransferSourceRef(r) {
			out.Kept = append(out.Kept, r)
			continue
		}
		seen[r.TargetPath] = true
		viewID, isView := mountedViews[r.TargetPath]
		var hex string
		var ok bool
		if isView {
			// The view's own record names the digest the mount serves; an
			// unreadable record leaves the ledger's word standing, since the
			// mount is what the consumer sees either way. If neither names a
			// digest, recovery aborts rather than dropping a live mount's ref.
			hex = s.viewDigest(viewID, r.Hex)
			if hex == "" {
				return out, fmt.Errorf("mounted metadata view %s at %s has no recoverable artifact digest", viewID, r.TargetPath)
			}
			ok = true
		} else {
			hex, ok = mounted[r.TargetPath]
		}
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

	// A mounted view the ledger lost is adopted with the digest its own
	// record names. An unreadable record aborts recovery so collection cannot
	// start without a reference for a live mount.
	viewTargets := make([]string, 0, len(mountedViews))
	for t := range mountedViews {
		if !seen[t] {
			viewTargets = append(viewTargets, t)
		}
	}
	sort.Strings(viewTargets)
	for _, t := range viewTargets {
		hex := s.viewDigest(mountedViews[t], "")
		if hex == "" {
			return out, fmt.Errorf("mounted metadata view %s at %s has no recoverable artifact digest", mountedViews[t], t)
		}
		g := csiTargetPattern.FindStringSubmatch(t)
		r := Ref{TargetPath: t, Hex: hex, PodUID: g[1], VolumeID: EphemeralVolumeID(g[1], g[2])}
		out.Adopted = append(out.Adopted, r)
		if err := s.WriteRef(r); err != nil {
			return out, err
		}
	}

	return out, nil
}

// viewDigest reads a view directory's record under this store's model-transfer/views
// root and returns the artifact digest's hexadecimal part. fallback is the
// ledger's word, returned when the record cannot be read.
func (s *Store) viewDigest(viewID, fallback string) string {
	raw, err := os.ReadFile(filepath.Join(s.ViewsRoot(), viewID, "view.json"))
	if err != nil {
		klog.ErrorS(err, "read mounted metadata view", "view", viewID)
		return fallback
	}
	var view struct {
		ArtifactDigest string `json:"artifactDigest"`
	}
	if err := json.Unmarshal(raw, &view); err != nil {
		klog.ErrorS(err, "decode mounted metadata view", "view", viewID)
		return fallback
	}
	hex := HexOf(view.ArtifactDigest)
	if hex == "" {
		klog.ErrorS(nil, "mounted metadata view has an invalid artifact digest", "view", viewID)
		return fallback
	}
	return hex
}
