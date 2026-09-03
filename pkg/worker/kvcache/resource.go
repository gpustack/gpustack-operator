// Package kvcache holds what is true of a KVCacheBackend whatever implementation backs it.
//
// It is deliberately small, and that is the point of the split rather than an accident of it: almost
// everything a kv cache backend does — the process it runs, the flags it takes, the routes it serves,
// the metrics it publishes — is a fact about one implementation. Those live in the sub-package for
// that implementation, today only mooncake. What stays here is what a second implementation would
// answer identically: how an object rendered for a backend is labeled, and how the image a backend
// declares resolves to a pull policy.
//
// Nothing here dispatches on `spec.type`, and no interface abstracts over implementations. There is
// exactly one, the API admits exactly one value, and an abstraction drawn against a single case would
// be drawn in the wrong place. The package boundary is where the second one gets added; it is not a
// seam already built for it.
package kvcache

const (
	// ResourceType is what every object rendered for a backend is noted as, and ResourceNoteBackend
	// is the note carrying which backend it belongs to.
	//
	// They are exported because the reconciler's watches read them: a Deployment changing anywhere
	// in the cluster is filtered down to this operator's own by the type, and then mapped back to
	// the backend to re-enqueue by the note. Without that pair, the watch would either wake on
	// every Deployment in the cluster or need a label selector that duplicates what the note
	// already says.
	//
	// They sit HERE rather than beside the renderers that stamp them because they say nothing about
	// which implementation rendered the object — a watch filters by them before it knows or cares.
	// Under an implementation package, the backend reconciler would have to import that one
	// implementation to answer a question that has nothing to do with it.
	ResourceType        = "kvcachebackends"
	ResourceNoteBackend = "backend"
)
