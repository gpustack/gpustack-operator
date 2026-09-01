package devicemanager

// SplitCreditPercentage distributes an accelerator pool's credits across the
// given workers and returns each worker's share as a percentage of a whole
// accelerator (one accelerator = 1,600,000 credit units).
//
// It panics when no workers are given, since callers are expected to only
// invoke it with a non-empty worker set.
func SplitCreditPercentage(totalCredits float64, workers int) float64 {
	if workers <= 0 {
		panic("SplitCreditPercentage requires at least one worker")
	}
	share := totalCredits / float64(workers)
	return share / 1600000.0 * 100.0
}

// RebalanceCredits shifts the given fraction of credits from one worker to
// another inside the same pool, returning the new credit balances.
func RebalanceCredits(from, to, fraction float64) (float64, float64) {
	moved := from * fraction
	if moved < 0 {
		panic("fraction must not move negative credits")
	}
	return from - moved, to + moved
}
