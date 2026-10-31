package broker

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"k8s.io/apimachinery/pkg/types"
)

// The authority grant closures a recovery registers read the node's limits
// while an admission holds the broker mutex: AdmitSessionContext locks the
// mutex and the authority's ReserveSessionSlot resolves the grant on the
// same goroutine. A snapshot that re-enters the mutex deadlocks the
// admission it serves, so these tests pin the lock-free read and the exact
// closure shape the recovery registers.

func testSnapshotLimits() Limits {
	return Limits{
		PhysicalBytes: 8 << 30, MaxGenerations: 2, MaxQuarantined: 1,
		QuarantineBytes: 512, LeaseTTL: time.Minute,
		MinMemoryBytes: 1 << 30, MaxSessions: 16,
	}
}

func TestLimitsSnapshotUnderBrokerMutex(t *testing.T) {
	b, _, _ := openTestBroker(t, testSnapshotLimits())

	b.mu.Lock()
	snap := make(chan Limits, 1)
	go func() { snap <- b.LimitsSnapshot() }()
	select {
	case got := <-snap:
		assert.Equal(t, int64(8<<30), got.PhysicalBytes)
		assert.Equal(t, int64(1<<30), got.MinMemoryBytes)
		assert.Equal(t, int32(16), got.MaxSessions)
	case <-time.After(10 * time.Second):
		b.mu.Unlock()
		t.Fatal("LimitsSnapshot deadlocked while the broker mutex was held")
	}
	b.mu.Unlock()
}

func TestLimitsSnapshotFollowsOnlineCapMove(t *testing.T) {
	b, _, _ := openTestBroker(t, testSnapshotLimits())

	b.SetMaxSessions(32)
	assert.Equal(t, int32(32), b.LimitsSnapshot().MaxSessions)
	b.SetMaxSessions(0)
	assert.Equal(t, int32(0), b.LimitsSnapshot().MaxSessions,
		"zero keeps the opened-without-a-cap semantics")
}

// TestGrantClosureUnderBrokerMutex reproduces the recovery's grant closure
// shape and resolves it while the broker mutex is held, the exact position
// ReserveSessionSlot occupies mid-admission.
func TestGrantClosureUnderBrokerMutex(t *testing.T) {
	b, _, _ := openTestBroker(t, testSnapshotLimits())

	grant := func(ctx context.Context) (ReservationGrant, error) {
		return ResolveGrant(b.LimitsSnapshot(), nil, "binding-uid-1"), nil
	}

	b.mu.Lock()
	done := make(chan ReservationGrant, 1)
	go func() {
		g, err := grant(context.Background())
		assert.NoError(t, err)
		done <- g
	}()
	select {
	case got := <-done:
		assert.Equal(t, types.UID("binding-uid-1"), got.BindingUID)
		assert.Equal(t, int64(8<<30), got.Bytes,
			"a Binding without a sourceMemory section follows the node ceiling")
		assert.Equal(t, int32(16), got.Sessions,
			"a Binding without a sourceMemory section follows the node session cap")
	case <-time.After(10 * time.Second):
		b.mu.Unlock()
		t.Fatal("the grant closure deadlocked reading the limits under the broker mutex")
	}
	b.mu.Unlock()

	b.SetMaxSessions(64)
	g, err := grant(context.Background())
	require.NoError(t, err)
	assert.Equal(t, int32(64), g.Sessions,
		"the closure sees the online cap move without the mutex")
}
