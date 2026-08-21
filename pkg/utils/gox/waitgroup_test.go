package gox

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// groupTimeout bounds every wait below, so a group that cannot end fails the test instead of stalling
// the suite.
const groupTimeout = 10 * time.Second

// groupSettle is how long a case gives something it expects NOT to happen.
const groupSettle = 100 * time.Millisecond

var errGroupTask = errors.New("task failed")

// TestGroupWithContextIn_FailingTaskEndsItsSiblings pins the context these tasks are handed. It is the
// group's own, not the caller's, and that is load-bearing: Wait does not return until every task has,
// so a task watching a context nobody cancels holds a sibling's error back for as long as the caller
// lives — the failure is never reported and the caller never learns.
func TestGroupWithContextIn_FailingTaskEndsItsSiblings(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	t.Cleanup(cancelCaller)

	var siblingReturned atomic.Bool
	started := make(chan struct{})
	fail := make(chan struct{})

	gp := GroupWithContextIn(caller)
	gp.Go(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		siblingReturned.Store(true)

		return ctx.Err()
	})
	gp.Go(func(ctx context.Context) error {
		select {
		case <-fail:
			return errGroupTask
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	// Failed only once the sibling is running: the pool skips a task still queued when the group is
	// canceled, and a sibling that never ran proves nothing about what a failure does to one.
	<-started
	close(fail)

	errCh := make(chan error, 1)
	go func() {
		errCh <- gp.Wait()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, errGroupTask) {
			t.Fatalf("Wait must report the task's own failure, got %v", err)
		}
	case <-time.After(groupTimeout):
		t.Fatal("Wait did not return after a task failed")
	}
	if !siblingReturned.Load() {
		t.Fatal("the sibling must have returned before Wait did")
	}
	if caller.Err() != nil {
		t.Fatal("the caller's context must not be canceled by a task's failure")
	}
}

// TestGroupWithContextIn_CompletedTaskLeavesItsSiblingsRunning is the other half of the rule, and the
// reason a task's success cannot end the group: tasks that hand off to each other — one producing into
// a channel, one consuming until it closes — are a group where the producer finishing is normal and
// canceling the consumer there would break a correct caller.
func TestGroupWithContextIn_CompletedTaskLeavesItsSiblingsRunning(t *testing.T) {
	handoff := make(chan int, 1)

	gp := GroupWithContextIn(context.Background())
	gp.Go(func(context.Context) error {
		handoff <- 1
		close(handoff)

		return nil
	})
	gp.Go(func(ctx context.Context) error {
		for {
			select {
			case _, ok := <-handoff:
				if !ok {
					return nil
				}
			case <-ctx.Done():
				return ctx.Err()
			}
		}
	})

	errCh := make(chan error, 1)
	go func() {
		errCh <- gp.Wait()
	}()
	select {
	case err := <-errCh:
		if err != nil {
			t.Fatalf("a producer finishing must not cancel its consumer, got %v", err)
		}
	case <-time.After(groupTimeout):
		t.Fatal("Wait did not return")
	}
}

// TestQueuingGroupWithContextIn_FailingTaskEndsItsSiblings is the same rule in the queuing form,
// which needs its own case because that form reads the group's context twice: its tasks watch it, and
// its Go consults it before taking a slot. So the context it hands out decides not only when a queued
// sibling returns but whether the group accepts any more work once its run is over — on the caller's
// context it would keep admitting tasks into a group that had already failed.
func TestQueuingGroupWithContextIn_FailingTaskEndsItsSiblings(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())
	t.Cleanup(cancelCaller)

	var siblingReturned atomic.Bool
	started := make(chan struct{})
	fail := make(chan struct{})

	// Two slots, so both tasks are in flight rather than one waiting for the other to free one.
	gp := QueuingGroupWithContextIn(caller, 2)
	gp.Go(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()
		siblingReturned.Store(true)

		return ctx.Err()
	})
	gp.Go(func(ctx context.Context) error {
		select {
		case <-fail:
			return errGroupTask
		case <-ctx.Done():
			return ctx.Err()
		}
	})

	<-started
	close(fail)

	errCh := make(chan error, 1)
	go func() {
		errCh <- gp.Wait()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, errGroupTask) {
			t.Fatalf("Wait must report the task's own failure, got %v", err)
		}
	case <-time.After(groupTimeout):
		t.Fatal("Wait did not return after a task failed")
	}
	if !siblingReturned.Load() {
		t.Fatal("the queued sibling must have returned before Wait did")
	}

	// The admission gate, with the caller still very much alive.
	late := make(chan struct{})
	gp.Go(func(context.Context) error {
		close(late)

		return nil
	})
	select {
	case <-late:
		t.Fatal("a group whose run is over must not admit another task")
	case <-time.After(groupSettle):
	}
	if caller.Err() != nil {
		t.Fatal("the caller's context must not be canceled by a task's failure")
	}
}

// TestGroupWithContextIn_CallerCancellationReachesTasks keeps the caller's own cancellation working
// through the derived context: it is the caller that ends the group at shutdown, and a task that
// stopped watching for that would outlive the process's own teardown.
func TestGroupWithContextIn_CallerCancellationReachesTasks(t *testing.T) {
	caller, cancelCaller := context.WithCancel(context.Background())

	started := make(chan struct{})
	gp := GroupWithContextIn(caller)
	gp.Go(func(ctx context.Context) error {
		close(started)
		<-ctx.Done()

		return ctx.Err()
	})

	<-started
	cancelCaller()

	errCh := make(chan error, 1)
	go func() {
		errCh <- gp.Wait()
	}()
	select {
	case err := <-errCh:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("the caller giving up must reach the task, got %v", err)
		}
	case <-time.After(groupTimeout):
		t.Fatal("Wait did not return after the caller was canceled")
	}
}

// TestGroup_PanickingTaskIsReported pins that a task that panicked is reported as the panic it was.
// Each flavor of group carries its own guard, and until the recover helper behind them worked, a
// panic went past both: the plain group's task was left to the pool, which recovers it and drops the
// result nobody reads, so Wait returned nothing at all.
func TestGroup_PanickingTaskIsReported(t *testing.T) {
	testCases := []struct {
		name string
		// newGroup builds the flavor of group under test.
		newGroup func() IWaitGroup
	}{
		{
			name:     "a group of its own",
			newGroup: Group,
		},
		{
			name:     "a group sharing a context",
			newGroup: func() IWaitGroup { return GroupWithContext(context.Background()) },
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			gp := tc.newGroup()
			gp.Go(func() error {
				panic(errGroupTask)
			})

			errCh := make(chan error, 1)
			go func() {
				errCh <- gp.Wait()
			}()
			select {
			case err := <-errCh:
				if !errors.Is(err, errGroupTask) {
					t.Fatalf("Wait must report the panic the task died of, got %v", err)
				}
			case <-time.After(groupTimeout):
				t.Fatal("Wait did not return after a task panicked")
			}
		})
	}
}
