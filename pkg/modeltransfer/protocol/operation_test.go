package protocol

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func pendingOperation() *Operation {
	return &Operation{
		ID: "op-1", Kind: "read_tensor", Tensor: "w", State: OpPending,
		Offset: 0, Length: 48,
	}
}

func TestOperationStates(t *testing.T) {
	t.Run("new operation is pending", func(t *testing.T) {
		op := pendingOperation()
		assert.Equal(t, OpPending, op.State)
		assert.False(t, op.State.Terminal())
	})

	t.Run("pending completes", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkSucceeded())
		assert.Equal(t, OpSucceeded, op.State)
		assert.True(t, op.State.Terminal())
		assert.True(t, op.CanRelease())
	})

	t.Run("pending fails with reason", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkFailed("source-unavailable", "file gone"))
		assert.Equal(t, OpFailed, op.State)
		assert.Equal(t, "source-unavailable", op.ErrorCode)
		assert.True(t, op.CanRelease())
	})

	t.Run("double mark of same state is a no-op", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkSucceeded())
		require.NoError(t, op.MarkSucceeded())
		assert.Equal(t, OpSucceeded, op.State)
	})

	t.Run("conflicting terminal mark rejected", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkSucceeded())
		err := op.MarkFailed("x", "late failure")
		require.Error(t, err)
		assert.Equal(t, KindOperationState, KindOf(err))
	})
}

func TestOperationCancel(t *testing.T) {
	t.Run("cancel from pending", func(t *testing.T) {
		op := pendingOperation()
		op.MarkCancelled()
		assert.Equal(t, OpCancelled, op.State)
		assert.True(t, op.CanRelease())
	})

	t.Run("cancel is idempotent", func(t *testing.T) {
		op := pendingOperation()
		op.MarkCancelled()
		op.MarkCancelled()
		assert.Equal(t, OpCancelled, op.State)
	})

	t.Run("cancel after completion is a no-op", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkSucceeded())
		op.MarkCancelled()
		assert.Equal(t, OpSucceeded, op.State)
	})

	t.Run("cancel on unknown retains", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("deadline"))
		op.MarkCancelled()
		assert.Equal(t, OpUnknown, op.State)
		assert.True(t, op.Retained)
		assert.False(t, op.CanRelease())
	})
}

func TestOperationUnknown(t *testing.T) {
	t.Run("unknown from pending retains resources", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("backend state not terminal"))
		assert.Equal(t, OpUnknown, op.State)
		assert.True(t, op.Retained)
		assert.False(t, op.State.Terminal())
		assert.False(t, op.CanRelease())
	})

	t.Run("unknown resolves to succeeded", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("deadline"))
		require.NoError(t, op.ResolveUnknown(OpSucceeded, "", ""))
		assert.Equal(t, OpSucceeded, op.State)
		assert.False(t, op.Retained)
		assert.True(t, op.CanRelease())
	})

	t.Run("unknown resolves to failed", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("deadline"))
		require.NoError(t, op.ResolveUnknown(OpFailed, "source-unavailable", "confirmed lost"))
		assert.Equal(t, OpFailed, op.State)
		assert.Equal(t, "source-unavailable", op.ErrorCode)
		assert.True(t, op.CanRelease())
	})

	t.Run("resolve rejects non terminal target", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("deadline"))
		err := op.ResolveUnknown(OpPending, "", "")
		require.Error(t, err)
		assert.Equal(t, KindOperationState, KindOf(err))
	})

	t.Run("resolve rejects cancelled target and keeps retention", func(t *testing.T) {
		op := pendingOperation()
		require.NoError(t, op.MarkUnknown("deadline"))
		err := op.ResolveUnknown(OpCancelled, "", "")
		require.Error(t, err)
		assert.Equal(t, KindOperationState, KindOf(err))
		assert.Equal(t, OpUnknown, op.State)
		assert.True(t, op.Retained)
		assert.False(t, op.CanRelease())
	})

	t.Run("resolve on pending rejected", func(t *testing.T) {
		op := pendingOperation()
		err := op.ResolveUnknown(OpSucceeded, "", "")
		require.Error(t, err)
		assert.Equal(t, KindOperationState, KindOf(err))
	})
}

func TestReadBudgets(t *testing.T) {
	valid := ReadBudgets{WindowBytes: 65536, MaxInflight: 2, OwnedOutputBytes: 1 << 20}
	require.NoError(t, valid.Validate())

	cases := []struct {
		name   string
		mutate func(*ReadBudgets)
	}{
		{"window above ceiling", func(b *ReadBudgets) { b.WindowBytes = MaxWindowBytes + 1 }},
		{"zero window", func(b *ReadBudgets) { b.WindowBytes = 0 }},
		{"inflight above ceiling", func(b *ReadBudgets) { b.MaxInflight = 3 }},
		{"zero inflight", func(b *ReadBudgets) { b.MaxInflight = 0 }},
		{"zero owned budget", func(b *ReadBudgets) { b.OwnedOutputBytes = 0 }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			budgets := valid
			tc.mutate(&budgets)
			err := budgets.Validate()
			require.Error(t, err)
			assert.Equal(t, KindBudget, KindOf(err))
		})
	}

	t.Run("owned output budget independent of window", func(t *testing.T) {
		budgets := ReadBudgets{WindowBytes: 1024, MaxInflight: 2, OwnedOutputBytes: 1 << 20}
		require.NoError(t, budgets.Validate())
	})

	t.Run("contract ceilings", func(t *testing.T) {
		assert.EqualValues(t, 1<<30, DefaultWindowBytes)
		assert.EqualValues(t, 1<<40, MaxWindowBytes)
		assert.Equal(t, 2, MaxInflightOperations)
	})

	t.Run("a larger configured window is legal", func(t *testing.T) {
		for _, window := range []int64{8 << 30, 16 << 30, 64 << 30, 128 << 30} {
			budgets := ReadBudgets{WindowBytes: window, MaxInflight: 2, OwnedOutputBytes: window}
			require.NoError(t, budgets.Validate(), "window %d is under the ceiling", window)
		}
	})
}
