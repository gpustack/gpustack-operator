package device

// IsLogicallySliceable reports whether the card's reported capability admits logical
// (software) slicing — it is not in a physical partitioning mode and offers at least one
// soft slice. A card reporting neither capability returns false.
//
// Detectors report the two capabilities exclusively per card, so the partition test never
// changes today's answer. It is here so this predicate enforces the contract it states
// rather than inheriting it from every detector: a card that did report both would serve
// the partition family alone, never both, which is the over-advertisement the split exists
// to remove.
func IsLogicallySliceable(status AcceleratorStatus) bool {
	return status.LogicalSliced.Count > 0 && !IsPartitioned(status)
}

// IsPartitioned reports whether the card's reported capability is in a physical (hardware)
// partitioning mode, i.e. it has at least one hardware partition profile. A card reporting
// neither capability returns false.
func IsPartitioned(status AcceleratorStatus) bool {
	return status.PhysicalSliced.Count > 0
}

// IsWholeCardCapable reports whether the card can serve an exclusive or shared whole-card
// claim — it is not physically partitioned. A card reporting neither capability returns
// true, since an unpartitioned card is always available as a whole card regardless of
// whether it also offers logical slicing.
func IsWholeCardCapable(status AcceleratorStatus) bool {
	return !IsPartitioned(status)
}
