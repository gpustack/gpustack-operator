package gox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// lifecycleTimeout bounds every wait on a Lifecycle, so a regression that brings back the hang fails
// the test instead of stalling the suite.
const lifecycleTimeout = 10 * time.Second

// lifecycleSettle is how long a case gives something it expects NOT to happen. Long enough that a run
// which ends the moment a task does — the regression these cases guard — is caught every time.
const lifecycleSettle = 100 * time.Millisecond

var (
	errLifecycleTask  = errors.New("task failed")
	errLifecyclePanic = errors.New("task panicked")
)

// lifecycleTasks builds the tasks a case hands a Lifecycle and records what they did: entered counts
// the ones that got to run, returned the ones that got out. A case asserts on both, because "Stop
// returned" is only worth anything if the tasks returned first.
//
// The task shapes below are the ones the components using a Lifecycle actually supply: one that runs
// until its context ends, one that fails, one that finishes its work successfully, one that panics,
// and one that ends only when the case says so.
type lifecycleTasks struct {
	entered  atomic.Int32
	returned atomic.Int32
	// fail, finish and hold are closed to make the corresponding task end, so a case decides when
	// that happens rather than racing the other tasks into the pool. fail serves both the task that
	// returns an error and the one that panics, which is what lets one case end a run by both at once.
	fail   chan struct{}
	finish chan struct{}
	hold   chan struct{}
}

func newLifecycleTasks() *lifecycleTasks {
	return &lifecycleTasks{
		fail:   make(chan struct{}),
		finish: make(chan struct{}),
		hold:   make(chan struct{}),
	}
}

// blocking is what most of the tasks a component starts look like: it runs until its context ends,
// and reports that context's error the way a resource server reports its caller's.
func (r *lifecycleTasks) blocking() func(context.Context) error {
	return func(ctx context.Context) error {
		r.entered.Add(1)
		defer r.returned.Add(1)

		<-ctx.Done()

		return ctx.Err()
	}
}

// finishing runs until the case lets it finish successfully, or until its context ends. It stands for
// the task that has nothing to do and says so — a metrics poller on a node it cannot identify, a
// producer that has produced everything — which is a component degrading, not failing.
func (r *lifecycleTasks) finishing() func(context.Context) error {
	return func(ctx context.Context) error {
		r.entered.Add(1)
		defer r.returned.Add(1)

		select {
		case <-r.finish:
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// failing runs until the case makes it fail, or until its context ends.
func (r *lifecycleTasks) failing() func(context.Context) error {
	return func(ctx context.Context) error {
		r.entered.Add(1)
		defer r.returned.Add(1)

		select {
		case <-r.fail:
			return errLifecycleTask
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// panicking panics instead of returning, when the case says so. The task returns nothing then, so
// whether the run reports anything at all depends on where the panic is recovered.
//
// It waits for the cue rather than panicking on entry for the same reason failing does: the pool skips
// a task still queued when the group is canceled, so a run cut short on its first task leaves the
// others never having run.
func (r *lifecycleTasks) panicking() func(context.Context) error {
	return func(ctx context.Context) error {
		r.entered.Add(1)
		defer r.returned.Add(1)

		select {
		case <-r.fail:
			panic(errLifecyclePanic)
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// held ends when the case releases it and for no other reason, not even its context. It keeps a run
// from ending while the case arranges what has to be true by the time it does.
func (r *lifecycleTasks) held() func(context.Context) error {
	return func(context.Context) error {
		r.entered.Add(1)
		defer r.returned.Add(1)

		<-r.hold

		return nil
	}
}

// TestLifecycle pins what a component's Start and Stop mean to each other: past Stop every task Start
// launched has returned, being stopped is not an outcome to report, and anything that went wrong is.
func TestLifecycle(t *testing.T) {
	testCases := []struct {
		name string
		// blocking is how many context-watching tasks the case runs; the flags below each add one
		// task of the corresponding shape.
		blocking  int
		failing   bool
		panicking bool
		held      bool
		// act drives the case once every task is running, and is where a case asserts what must hold
		// at the moment it acts — that Stop did not return before its tasks did. Nil when the tasks
		// alone decide the outcome.
		act func(t *testing.T, l *Lifecycle, r *lifecycleTasks, cancelCaller context.CancelFunc)
		// wantErrs is every outcome the run must report. More than one because a run can end with
		// several tasks having something to say, and dropping any of them is the defect these cases
		// exist to catch; empty means the run reports nothing.
		wantErrs []error
	}{
		{
			name:     "stop ends a task that only watches its context",
			blocking: 1,
			act: func(t *testing.T, l *Lifecycle, r *lifecycleTasks, _ context.CancelFunc) {
				l.Stop()
				assert.Equal(t, int32(1), r.returned.Load(),
					"Stop must not return before the tasks it cancelled have")
			},
			wantErrs: nil,
		},
		{
			name:     "being stopped is not an outcome to report",
			blocking: 3,
			act: func(t *testing.T, l *Lifecycle, r *lifecycleTasks, _ context.CancelFunc) {
				l.Stop()
				assert.Equal(t, int32(3), r.returned.Load(),
					"Stop must not return before the tasks it cancelled have")
			},
			wantErrs: nil,
		},
		{
			name:     "the caller giving up is reported",
			blocking: 2,
			act: func(_ *testing.T, _ *Lifecycle, _ *lifecycleTasks, cancelCaller context.CancelFunc) {
				cancelCaller()
			},
			wantErrs: []error{context.Canceled},
		},
		{
			name:     "a failing task ends its siblings and is reported",
			blocking: 2,
			failing:  true,
			act: func(_ *testing.T, _ *Lifecycle, r *lifecycleTasks, _ context.CancelFunc) {
				close(r.fail)
			},
			wantErrs: []error{errLifecycleTask},
		},
		{
			// The task returns nothing itself, so this outcome exists only because the panic is
			// recovered where a failure is recorded.
			name:      "a panicking task is reported",
			blocking:  1,
			panicking: true,
			act: func(_ *testing.T, _ *Lifecycle, r *lifecycleTasks, _ context.CancelFunc) {
				close(r.fail)
			},
			wantErrs: []error{errLifecyclePanic},
		},
		{
			// Both, and this is the case that decides where the panic is recovered: the group
			// underneath keeps only the first error its tasks produce, so a panic left for it to
			// report is lost the moment a sibling fails too — in either order.
			name:      "a panicking task and a failing one are both reported",
			failing:   true,
			panicking: true,
			act: func(_ *testing.T, _ *Lifecycle, r *lifecycleTasks, _ context.CancelFunc) {
				close(r.fail)
			},
			wantErrs: []error{errLifecycleTask, errLifecyclePanic},
		},
		{
			// A caller that gives up mid-failure already knows it is shutting down. The failure is
			// the thing only this return can tell it, so it is not the one that gets dropped.
			name:    "a failing task outranks the caller giving up",
			failing: true,
			held:    true,
			act: func(t *testing.T, _ *Lifecycle, r *lifecycleTasks, cancelCaller context.CancelFunc) {
				close(r.fail)
				require.Eventually(t, func() bool {
					return r.returned.Load() == 1
				}, lifecycleTimeout, 10*time.Millisecond, "the failing task did not return")

				// Ordered deliberately: the run cannot end until the held task is released, so by
				// then both a failure and a canceled caller are waiting to be reported.
				cancelCaller()
				close(r.hold)
			},
			wantErrs: []error{errLifecycleTask},
		},
		{
			name:     "stopping twice is a no-op the second time",
			blocking: 1,
			act: func(_ *testing.T, l *Lifecycle, _ *lifecycleTasks, _ context.CancelFunc) {
				l.Stop()
				l.Stop()
			},
			wantErrs: nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var l Lifecycle
			r := newLifecycleTasks()

			tasks := make([]func(context.Context) error, 0, tc.blocking+3)
			for range tc.blocking {
				tasks = append(tasks, r.blocking())
			}
			if tc.failing {
				tasks = append(tasks, r.failing())
			}
			if tc.panicking {
				tasks = append(tasks, r.panicking())
			}
			if tc.held {
				tasks = append(tasks, r.held())
			}

			caller, cancelCaller := context.WithCancel(context.Background())
			t.Cleanup(cancelCaller)

			errCh := make(chan error, 1)
			go func() {
				errCh <- l.Start(caller, tasks...)
			}()

			// Acted on only once every task is running: a Lifecycle acted on before it began is its
			// own case, and the pool skips a task queued behind a cancellation.
			require.Eventually(t, func() bool {
				return r.entered.Load() == int32(len(tasks))
			}, lifecycleTimeout, 10*time.Millisecond, "tasks did not start")

			if tc.act != nil {
				tc.act(t, &l, r, cancelCaller)
			}

			select {
			case err := <-errCh:
				if len(tc.wantErrs) == 0 {
					assert.NoError(t, err)
				}
				for _, want := range tc.wantErrs {
					assert.ErrorIs(t, err, want)
				}
			case <-time.After(lifecycleTimeout):
				t.Fatal("Start did not return")
			}
			assert.Equal(t, int32(len(tasks)), r.returned.Load(),
				"every task Start launched must have returned")
		})
	}
}

// TestLifecycle_FinishedTaskLeavesItsSiblingsRunning is the line between a task that is done and a
// component that is over. A metrics poller handed a node it cannot name serves nothing and says so,
// and a run that ended there would take the device plugin down over a missing environment variable.
// Only a task that FAILED ends its siblings.
func TestLifecycle_FinishedTaskLeavesItsSiblingsRunning(t *testing.T) {
	var l Lifecycle
	r := newLifecycleTasks()

	caller, cancelCaller := context.WithCancel(context.Background())
	t.Cleanup(cancelCaller)

	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Start(caller, r.blocking(), r.blocking(), r.finishing())
	}()
	require.Eventually(t, func() bool {
		return r.entered.Load() == 3
	}, lifecycleTimeout, 10*time.Millisecond, "tasks did not start")

	close(r.finish)
	require.Eventually(t, func() bool {
		return r.returned.Load() == 1
	}, lifecycleTimeout, 10*time.Millisecond, "the finishing task did not return")

	select {
	case err := <-errCh:
		t.Fatalf("a task finishing must not end the run, got %v", err)
	case <-time.After(lifecycleSettle):
	}
	assert.Equal(t, int32(1), r.returned.Load(), "the siblings of a finished task must still be running")

	l.Stop()

	select {
	case err := <-errCh:
		assert.NoError(t, err)
	case <-time.After(lifecycleTimeout):
		t.Fatal("Start did not return after Stop")
	}
	assert.Equal(t, int32(3), r.returned.Load())
}

// TestLifecycle_SecondStartWaitsForTheRunItCollidedWith covers a Start that arrives while a run is
// already going. It must launch nothing — one Stop cannot own two sets of tasks — and it must return
// when that run is over rather than when its own caller is: a call parked on the caller outlasts the
// run it is waiting for, and on a context nobody cancels never returns at all.
//
// Both callers here are context.Background() on purpose: that is the shape in which waiting on the
// caller is not merely late but permanent.
func TestLifecycle_SecondStartWaitsForTheRunItCollidedWith(t *testing.T) {
	var l Lifecycle
	first, second := newLifecycleTasks(), newLifecycleTasks()

	firstCh := make(chan error, 1)
	go func() {
		firstCh <- l.Start(context.Background(), first.blocking())
	}()
	require.Eventually(t, func() bool {
		return first.entered.Load() == 1
	}, lifecycleTimeout, 10*time.Millisecond, "the first run did not start")

	secondCh := make(chan error, 1)
	go func() {
		secondCh <- l.Start(context.Background(), second.blocking())
	}()

	select {
	case err := <-secondCh:
		t.Fatalf("the second Start returned while the run it collided with was still going, got %v", err)
	case <-time.After(lifecycleSettle):
	}
	assert.Equal(t, int32(0), second.entered.Load(), "the second Start must launch nothing")

	l.Stop()

	require.NoError(t, <-firstCh)
	select {
	case err := <-secondCh:
		assert.NoError(t, err, "a Start that launched nothing has nothing to report")
	case <-time.After(lifecycleTimeout):
		t.Fatal("the second Start did not return once the run it collided with was over")
	}
	assert.Equal(t, int32(0), second.entered.Load())
}

// TestLifecycle_StopBeforeItsRunBegins covers the component discarded before it ever ran. Its Start
// is submitted to a pool, so it can reach a Lifecycle well after the Stop that retired it — and a run
// that began then is a run nobody holds the cancel of, which is the leak this type exists to prevent.
func TestLifecycle_StopBeforeItsRunBegins(t *testing.T) {
	var l Lifecycle
	r := newLifecycleTasks()

	l.Stop()

	// Bounded rather than called here: a Lifecycle that lost the Stop starts the task, and nothing
	// is left to cancel it — the regression would hang this test rather than fail it.
	errCh := make(chan error, 1)
	go func() {
		errCh <- l.Start(context.Background(), r.blocking())
	}()

	select {
	case err := <-errCh:
		require.NoError(t, err, "a stopped Lifecycle must report nothing")
	case <-time.After(lifecycleTimeout):
		t.Fatal("Start did not return on a stopped Lifecycle")
	}
	assert.Equal(t, int32(0), r.entered.Load(), "a stopped Lifecycle must start nothing")
}

// TestLifecycle_StopRacingItsStart is the same handover without the ordering. Whichever of the two
// wins, the invariant is the one a replacement component depends on: past both calls, no task of that
// run is still holding what the replacement is about to take over.
func TestLifecycle_StopRacingItsStart(t *testing.T) {
	for range 100 {
		var l Lifecycle
		r := newLifecycleTasks()

		errCh := make(chan error, 1)
		go func() {
			errCh <- l.Start(context.Background(), r.blocking())
		}()

		l.Stop()

		select {
		case err := <-errCh:
			require.NoError(t, err)
		case <-time.After(lifecycleTimeout):
			t.Fatal("Start did not return after a Stop that raced it")
		}
		require.Equal(t, r.entered.Load(), r.returned.Load(),
			"a task that started must have returned by the time both calls have")
	}
}
