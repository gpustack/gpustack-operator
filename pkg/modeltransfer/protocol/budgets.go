package protocol

// Contract ceilings of the reader: transfer windows through 1 TiB per slot
// (dynamically sized to the source model's contiguous file span, with a
// 1 GiB default source fill window) and at most two operations in flight.
// The window ceiling is a misconfiguration guard, not a tuning limit: a
// reader may raise its window to any figure under it — 16, 64, 128 GiB —
// without a code change.
const (
	DefaultWindowBytes    int64 = 1 << 30
	MaxWindowBytes        int64 = 1 << 40
	MaxInflightOperations int   = 2
)

// ReadBudgets holds the three session budgets. WindowBytes bounds one
// transfer window, MaxInflight bounds concurrent operations, and
// OwnedOutputBytes bounds one owned output tensor. The owned-output budget
// and the window budget are separate: a tensor may exceed one window when
// it fits the owned-output budget.
type ReadBudgets struct {
	WindowBytes      int64 `json:"window_bytes"`
	MaxInflight      int   `json:"max_inflight"`
	OwnedOutputBytes int64 `json:"owned_output_bytes"`
}

// Validate checks the budgets against the contract ceilings.
func (b ReadBudgets) Validate() error {
	if b.WindowBytes < 1 || b.WindowBytes > MaxWindowBytes {
		return New(KindBudget, "window_bytes",
			"must be in [1, %d], got %d", MaxWindowBytes, b.WindowBytes)
	}
	if b.MaxInflight < 1 || b.MaxInflight > MaxInflightOperations {
		return New(KindBudget, "max_inflight",
			"must be in [1, %d], got %d", MaxInflightOperations, b.MaxInflight)
	}
	if b.OwnedOutputBytes < 1 {
		return New(KindBudget, "owned_output_bytes",
			"must be positive, got %d", b.OwnedOutputBytes)
	}
	return nil
}
