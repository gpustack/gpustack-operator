package device

// IsLogicallySliceable reports whether the accelerator's reported capability admits logical
// (software) slicing — it is not in a physical partitioning mode and offers at least one
// logical slice. An accelerator reporting neither capability returns false.
//
// Detectors report the two capabilities exclusively per accelerator, so the partition test
// never changes today's answer. It is here so this predicate enforces the contract it states
// rather than inheriting it from every detector: an accelerator that did report both would
// serve the partition family alone, never both, which is the over-advertisement the split
// exists to remove.
func IsLogicallySliceable(status AcceleratorStatus) bool {
	return status.LogicalSliced.Count > 0 && !IsPartitioned(status)
}

// IsPartitioned reports whether the accelerator's reported capability is in a physical
// (hardware) partitioning mode, i.e. it has at least one hardware partition profile. An
// accelerator reporting neither capability returns false.
func IsPartitioned(status AcceleratorStatus) bool {
	return status.PhysicalSliced.Count > 0
}

// IsWholeAcceleratorCapable reports whether the accelerator can serve an exclusive or shared
// whole-accelerator claim — it is not physically partitioned. An accelerator reporting neither
// capability returns true, since an unpartitioned accelerator is always available as a whole
// regardless of whether it also offers logical slicing.
func IsWholeAcceleratorCapable(status AcceleratorStatus) bool {
	return !IsPartitioned(status)
}
