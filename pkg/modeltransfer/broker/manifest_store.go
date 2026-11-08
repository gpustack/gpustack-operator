package broker

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

const manifestsDir = "manifests"

func (l *ledger) manifestDirectory(digest string) (string, error) {
	if len(digest) != 64 || strings.Trim(digest, "0123456789abcdef") != "" {
		return "", fmt.Errorf("invalid manifest digest")
	}
	return filepath.Join(l.root, manifestsDir, digest), nil
}

// storeManifest publishes immutable metadata before the generation can
// become durable. It runs without the broker mutex — the parse reads every
// page and a fresh publication writes and syncs them — and mutates only
// the caller's record copy, which the admission then publishes under the
// mutex. A failed publication exposes no work order or native owner.
func (b *Broker) storeManifest(ctx context.Context, rec *GenerationRecord) error {
	if rec.ManifestPagesPath == "" {
		return nil
	}
	manifest, err := b.parseDerivedManifest(ctx, rec)
	if err != nil {
		return err
	}
	destination, err := b.ledger.manifestDirectory(rec.ManifestSHA256)
	if err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	if err := os.MkdirAll(parent, 0o700); err != nil {
		return err
	}
	if _, err := os.Stat(destination); err == nil {
		if _, err := protocol.LoadManifest(ctx, filepath.Join(destination, "manifest.json"), rec.ManifestSHA256); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	} else {
		stage, err := os.MkdirTemp(parent, ".manifest-")
		if err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(stage) }()
		pages := filepath.Join(stage, "pages")
		if err := os.Mkdir(pages, 0o700); err != nil {
			return err
		}
		reader := b.pageReaderFor(rec.ManifestPagesPath)
		for _, page := range manifest.TensorPages {
			raw, err := reader(ctx, page)
			if err != nil {
				return err
			}
			sum := sha256.Sum256(raw)
			if len(raw) != page.Size || hex.EncodeToString(sum[:]) != page.SHA256 {
				return fmt.Errorf("page changed during publication")
			}
			if err := writeSynced(filepath.Join(pages, page.SHA256+".json"), raw); err != nil {
				return err
			}
		}
		if err := writeSynced(filepath.Join(stage, "manifest.json"), []byte(rec.ManifestJSON)); err != nil {
			return err
		}
		if err := os.Rename(stage, destination); err != nil {
			// Two admissions of one manifest publish the same immutable
			// directory; whichever rename lands, an identical verified copy
			// serves equally well and the staged copy is discarded.
			if _, loadErr := protocol.LoadManifest(ctx, filepath.Join(destination, "manifest.json"), rec.ManifestSHA256); loadErr != nil {
				return err
			}
		} else {
			directory, err := os.Open(parent)
			if err != nil {
				return err
			}
			defer directory.Close()
			if err := directory.Sync(); err != nil {
				return err
			}
		}
	}
	rec.ManifestPagesPath = filepath.Join(destination, "pages")
	rec.ManifestStored = true
	return nil
}

func (l *ledger) restoreManifest(rec *GenerationRecord) error {
	if !rec.ManifestStored || rec.State == StateReleased {
		return nil
	}
	directory, err := l.manifestDirectory(rec.ManifestSHA256)
	if err != nil {
		return err
	}
	file, err := os.Open(filepath.Join(directory, "manifest.json"))
	if err != nil {
		return err
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, protocol.MaxManifestBytes+1))
	if err != nil {
		return err
	}
	if len(raw) > protocol.MaxManifestBytes {
		return fmt.Errorf("stored manifest exceeds bound")
	}
	sum := sha256.Sum256(raw)
	if hex.EncodeToString(sum[:]) != rec.ManifestSHA256 {
		return fmt.Errorf("stored manifest digest mismatch")
	}
	rec.ManifestJSON = string(raw)
	rec.ManifestPagesPath = filepath.Join(directory, "pages")
	return nil
}

// sweepManifestStoresLocked retains every nonreleased generation, including
// unknown ownership. Only unreferenced metadata and abandoned staging leave.
func (b *Broker) sweepManifestStoresLocked() error {
	keep := make(map[string]bool)
	for generation, rec := range b.records {
		if rec.State != StateReleased && rec.ManifestStored {
			keep[rec.ManifestSHA256] = true
		} else if rec.State == StateReleased {
			delete(b.manifestCache, generation)
		}
	}
	root := filepath.Join(b.ledger.root, manifestsDir)
	entries, err := os.ReadDir(root)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if keep[entry.Name()] {
			continue
		}
		if !strings.HasPrefix(entry.Name(), ".manifest-") && (len(entry.Name()) != 64 || strings.Trim(entry.Name(), "0123456789abcdef") != "") {
			continue
		}
		if err := os.RemoveAll(filepath.Join(root, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}
