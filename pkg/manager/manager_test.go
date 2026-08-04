package manager

import (
	"context"
	"errors"
	"strings"
	"testing"

	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcache "sigs.k8s.io/controller-runtime/pkg/cache"
)

// _FakeCache reports a fixed cache-sync answer. WaitForReady asks nothing else of the cache, so the
// embedded nil interface is never reached.
type _FakeCache struct {
	ctrlcache.Cache

	synced bool
}

func (c _FakeCache) WaitForCacheSync(context.Context) bool { return c.synced }

// _FakeCtrlManager serves that cache. Same reasoning for the embedded nil interface: WaitForReady
// only ever calls GetCache.
type _FakeCtrlManager struct {
	ctrl.Manager

	cache ctrlcache.Cache
}

func (m _FakeCtrlManager) GetCache() ctrlcache.Cache { return m.cache }

func TestManager_WaitForReady(t *testing.T) {
	// The worker registers this as a liveness check, so its answer decides whether a process whose
	// controllers have stopped is restarted or left running while reconciling nothing.
	testCases := []struct {
		name           string
		started        bool
		stopped        bool
		cacheSynced    bool
		cancelContext  bool
		wantErr        error
		wantErrMessage string
	}{
		{
			name:        "a started manager with a synced cache is ready",
			started:     true,
			cacheSynced: true,
		},
		{
			name:           "a stopped manager is not ready",
			started:        true,
			stopped:        true,
			cacheSynced:    true,
			wantErrMessage: "stopped",
		},
		{
			name:           "a started manager whose cache is not synced is not ready",
			started:        true,
			wantErrMessage: "not synced",
		},
		{
			name:          "a manager that has not started yet fails with the caller's context",
			cancelContext: true,
			wantErr:       context.Canceled,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			m := &Manager{
				CtrlManager: CtrlManager{
					Manager: _FakeCtrlManager{cache: _FakeCache{synced: tc.cacheSynced}},
				},
				sentinel: _CtrlManagerSentinel{done: make(chan struct{})},
			}
			m.stopped.Store(tc.stopped)
			if tc.started {
				// The sentinel is closed when the manager starts and stays closed from then on,
				// which is why the wait alone cannot tell a started manager from a stopped one.
				close(m.sentinel.done)
			}

			ctx := context.Background()
			if tc.cancelContext {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}

			err := m.WaitForReady(ctx)

			if tc.wantErr == nil && tc.wantErrMessage == "" {
				if err != nil {
					t.Fatalf("WaitForReady() error = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("WaitForReady() = nil, want an error")
			}
			if tc.wantErr != nil && !errors.Is(err, tc.wantErr) {
				t.Errorf("WaitForReady() error = %v, want %v", err, tc.wantErr)
			}
			if tc.wantErrMessage != "" && !strings.Contains(err.Error(), tc.wantErrMessage) {
				t.Errorf("WaitForReady() error = %q, want it to mention %q", err, tc.wantErrMessage)
			}
		})
	}
}
