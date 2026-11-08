package metadata

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"

	"gpustack.ai/gpustack/pkg/modelmanager/store"
)

// EnsureView builds (or joins) the metadata view of hex's published tree
// under viewRoot, content-addressed by the view's own record.
//
// A view that already stands under viewRoot and verifies against the
// requested digest and required files answers directly: a destination node
// that holds no full checkpoint — nothing published for hex — still serves
// its pre-staged view, and never needs the marker a local build requires.
// Only a view that does not exist yet is built, from the node's verified
// published tree, with the manifest read from beside the marker when the
// sealed tree does not carry it. No download is ever started: a
// metadata-only request materializes nothing but the view.
func EnsureView(st *store.Store, viewRoot, hex, digest string,
	required []string, selfVolumeID ...string,
) (string, error) {
	return EnsureViewContext(context.Background(), st, viewRoot, hex, digest, required, selfVolumeID...)
}

// EnsureViewContext is EnsureView with a caller-supplied context for lock
// acquisition and unreferenced view retirement.
func EnsureViewContext(ctx context.Context, st *store.Store, viewRoot, hex, digest string,
	required []string, selfVolumeID ...string,
) (string, error) {
	if required == nil {
		required = DefaultRequiredFiles
	}
	if err := ValidateRequired(required); err != nil {
		return "", err
	}
	if err := os.MkdirAll(viewRoot, 0o755); err != nil {
		return "", err
	}

	// An already-verified view of this artifact answers at once, whether
	// or not this node holds the full checkpoint it was derived from.
	if dir, ok := findView(viewRoot, digest, required); ok {
		return dir, nil
	}

	marker, err := st.ReadMarker(hex)
	if err != nil {
		if !errors.Is(err, fs.ErrNotExist) {
			return "", fmt.Errorf("read published tree marker for %s: %w", digest, err)
		}
		return "", fmt.Errorf(
			"no verified published tree for %s and no pre-staged metadata view: "+
				"materialize the full checkpoint, pre-stage the view, or use remote delivery",
			digest,
		)
	}
	if marker.Digest != digest {
		return "", fmt.Errorf("%w: the published tree verifies against %s, not %s",
			ErrStaleDigest, marker.Digest, digest)
	}

	// Build into a private staging directory of the view root, owned by a
	// lock the builder holds from before the directory exists until after
	// promotion: concurrent builds of the same view both succeed, a view
	// directory is either absent or whole, and recovery can prove a
	// staging abandoned instead of guessing.
	stage, err := NewStaging(viewRoot)
	if err != nil {
		return "", err
	}
	defer stage.Release()
	view, err := buildContext(ctx, st.PublishedTree(hex), stage.Dir(), digest, required,
		st.PublishedManifestPath(hex))
	if err != nil {
		return "", err
	}
	final := filepath.Join(viewRoot, view.ID())
	if err := publishStoreView(ctx, st, stage.Dir(), final, hex, digest, required, selfVolumeID...); err != nil {
		return "", err
	}
	return final, nil
}

// findView returns the first verified view under root that answers the
// request, scanning in name order so the choice is deterministic.
func findView(root, digest string, required []string) (string, bool) {
	entries, err := os.ReadDir(root)
	if err != nil {
		return "", false
	}
	for _, entry := range entries {
		if !entry.IsDir() || !isHex64(entry.Name()) {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		view, err := Load(dir)
		if err != nil || !viewSatisfies(view, digest, required) {
			continue
		}
		view, err = verifyTransferView(dir)
		if err != nil || !viewSatisfies(view, digest, required) {
			continue
		}
		return dir, true
	}
	return "", false
}

// isHex64 reports whether s is 64 lowercase hexadecimal characters, the
// shape of a content-addressed view directory.
func isHex64(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') {
			return false
		}
	}
	return true
}
