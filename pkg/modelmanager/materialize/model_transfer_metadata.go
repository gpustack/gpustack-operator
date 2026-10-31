package materialize

import (
	"gpustack.ai/gpustack/pkg/modelmanager/store"
	"gpustack.ai/gpustack/pkg/modeltransfer/metadata"
)

// EnsureMetadata builds the metadata-only view of hex's published tree under
// viewRoot and returns its directory.
//
// It is the materializer's model-transfer half: it never downloads a checkpoint, never
// publishes a tree, and holds the published tree it reads from unchanged.
// A metadata-only request for a digest the node does not hold verified is an
// actionable refusal, not a complete download in disguise.
func EnsureMetadata(st *store.Store, viewRoot, hex, digest string,
	required []string,
) (string, error) {
	return metadata.EnsureView(st, viewRoot, hex, digest, required)
}
