package artifact

import (
	"cmp"
	"context"
	"fmt"
	"os"
	"slices"

	"gpustack.ai/gpustack/pkg/modelartifact"
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/protocol"
)

func verifyPagedTree(
	ctx context.Context, st *store.Store, artifactUID string, sourceGeneration int64,
	contentDigest string, canonical modelartifact.Manifest, pageDirectory string,
) (*View, error) {
	if len(canonical.Entries) > protocol.MaxManifestFiles {
		return nil, fmt.Errorf("%w: file count exceeds the manifest bound", ErrBoundsExceeded)
	}
	treeDir := st.PublishedTree(store.HexOf(contentDigest))
	root, err := os.OpenRoot(treeDir)
	if err != nil {
		return nil, fmt.Errorf("%w: %w", ErrNotPublished, err)
	}
	defer root.Close()
	manifest := &protocol.Manifest{
		ContractVersion: protocol.ContractVersion, ArtifactUID: artifactUID,
		SourceGeneration: sourceGeneration, Tensors: []protocol.TensorDescriptor{},
	}
	for _, entry := range canonical.Entries {
		manifest.Files = append(manifest.Files, protocol.FileDescriptor{Path: entry.Path, Size: entry.Size})
	}
	var verified *verifiedFiles
	pages, err := protocol.WriteManifestPages(ctx, pageDirectory, manifest.Files, func(yield func(protocol.TensorDescriptor, error) bool) {
		var verifyErr error
		stopped := false
		verified, verifyErr = verifyFilesWithConsumer(ctx, canonical.Entries,
			func(ctx context.Context, entry modelartifact.ManifestEntry, registeredBytes int64) (*verifiedFile, error) {
				return verifyOne(ctx, root, entry, registeredBytes)
			},
			func(file *verifiedFile) error {
				slices.SortFunc(file.tensors, func(a, b protocol.TensorDescriptor) int {
					if n := cmp.Compare(a.DataBegin, b.DataBegin); n != 0 {
						return n
					}
					if n := cmp.Compare(a.DataEnd, b.DataEnd); n != 0 {
						return n
					}
					return cmp.Compare(a.Name, b.Name)
				})
				for _, tensor := range file.tensors {
					if !yield(tensor, nil) {
						stopped = true
						return context.Canceled
					}
				}
				return nil
			})
		if verifyErr != nil && !stopped {
			yield(protocol.TensorDescriptor{}, verifyErr)
		}
	})
	if err != nil {
		return nil, err
	}
	manifest.TensorPages = pages
	if err := manifest.BindPages(ctx, protocol.LocalPageReader(pageDirectory)); err != nil {
		return nil, err
	}
	raw, digest, err := encodeManifest(manifest)
	if err != nil {
		return nil, err
	}
	manifest.ManifestSHA256 = digest
	return &View{
		Manifest: manifest, Raw: raw, SHA256: digest, ContentDigest: contentDigest,
		TreeDir: treeDir, RegisteredBytes: verified.registeredBytes, Registrations: verified.registrations,
	}, nil
}
