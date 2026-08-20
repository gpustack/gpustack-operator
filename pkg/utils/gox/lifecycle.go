package gox

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"

	"go.uber.org/multierr"
)

// Lifecycle is the running-until-stopped half of a component: the context every task its Start
// launched runs under, and the cancellation its Stop performs on it.
//
// A component whose only ending is its caller's does not need one. GroupWithContextIn already ends
// its tasks when the caller gives up, and when one of them fails. This is for the ones that are also
// stopped by somebody other than their caller — which is the thing no context derived from a caller
// can express: the cancel belongs to the component, its Stop is what holds it, and being stopped is
// deliberately not an outcome to report. A supervisor that treated a stop it asked for as a failure
// would act on it.
//
// The zero value is ready to use. It must not be copied once started, and it does not run again once
// stopped.
type Lifecycle struct {
	// mu guards the running state below: Start writes it as it begins and clears it on the way out,
	// while Stop reaches it from another goroutine.
	mu   sync.Mutex
	stop context.CancelFunc
	// done is closed once Start has returned, which is what Stop waits on: returning before then
	// would report as stopped tasks that are still running, and whatever they hold is exactly what a
	// replacement is about to take over.
	done chan struct{}
	// stopped keeps a Stop that arrived before its run from being lost. A component handed to a pool
	// can reach its Start well after it was discarded, and a run that began then is one nothing holds
	// the cancel of — the leak this type exists to prevent, reproduced by the teardown meant to fix
	// it.
	stopped bool
}

// Start runs every task under one context derived from ctx, and blocks until all of them have
// returned.
//
// A task that fails ends the rest: the group underneath cancels the context they share as soon as
// one of them reports an error. That is also what lets the failure be reported at all — nothing can
// be reported until every task has returned, and a task watching nothing but its context would
// otherwise hold a sibling's failure back for as long as the run lives. A task that simply finishes
// is left to have finished, and its siblings keep running: a component is entitled to a task that is
// done rather than broken, and one that degrades instead of failing is the usual shape of it.
//
// It returns the tasks' failures, the caller's error when only that happened, and nil when the run
// ended because Stop was called. An error that is a cancellation is not reported: it is either this
// Lifecycle's own teardown reflected back, or the run already ending on a sibling's failure, and
// whichever it is has a report of its own. A task with a genuine failure to report must therefore not
// wrap context.Canceled in it. A task that panics is reported as an error carrying the panic and its
// stack, alongside whatever its siblings reported.
//
// On a Lifecycle that has been stopped it starts nothing and returns nil. On one that is already
// running it starts nothing and returns once that run is over, so a caller that retries cannot end up
// with two sets of tasks under one Stop.
func (l *Lifecycle) Start(ctx context.Context, tasks ...func(context.Context) error) error {
	caller := ctx
	ctx, stop := context.WithCancel(caller)
	defer stop()

	done, state := l.begin(stop)
	if state == runStateStopped {
		return nil
	}
	if state == runStateRunning {
		// This call started nothing, so it has nothing of its own to wait for — but returning while
		// the run it collided with is still going would report as over a component that is not.
		// Waiting on the caller instead outlasts that run, and on a context nobody cancels never ends
		// at all.
		select {
		case <-done:
		case <-caller.Done():
		}

		return caller.Err()
	}
	defer l.end(done)

	var (
		mu       sync.Mutex
		failures error
	)

	gp := GroupWithContextIn(ctx)
	for i := range tasks {
		task := tasks[i]
		gp.Go(func(ctx context.Context) error {
			err := runTask(ctx, task)
			if err != nil && !errors.Is(err, context.Canceled) {
				mu.Lock()
				failures = multierr.Append(failures, err)
				mu.Unlock()
			}

			return err
		})
	}

	// The group's own error says that the run ended, not why — a cancellation masks whatever the
	// tasks reported, and a task the pool never got to run reports nothing at all. So the tasks' own
	// outcomes above are the report, and this is waited on for the guarantee that every one of them
	// has returned. It is still read as a backstop, for a group error no task accounted for.
	if err := gp.Wait(); failures == nil && err != nil && !errors.Is(err, context.Canceled) {
		failures = err
	}

	mu.Lock()
	defer mu.Unlock()

	// A failure outranks the caller's own cancellation. A caller shutting down already knows that it
	// is; the failure is the thing only this return can tell it, and a task that fails as the process
	// goes down is exactly the one worth hearing about.
	if failures != nil {
		return failures
	}

	return caller.Err()
}

// Stop cancels the context every task of the current run is under, and does not return until Start
// has: past it, nothing that Start started is still running. It also stops a run that has not begun
// yet, and the Lifecycle does not run again afterwards.
//
// Stopping one twice costs nothing. Stopping one that is not running is not free, though: being
// stopped is remembered, so a Start dequeued from a pool afterwards launches nothing and reports
// nothing — which is the point, since that Start is one nobody holds the cancel of.
//
// It must not be called from inside a task: it waits for the run to end, and the run does not end
// until that task has returned.
func (l *Lifecycle) Stop() {
	// The cancel is read together with the channel that closes on its run's end, so what is waited
	// on below is the run this Stop canceled rather than that run's already-closed successor.
	l.mu.Lock()
	l.stopped = true
	stop, done := l.stop, l.done
	l.mu.Unlock()

	if stop == nil {
		return
	}

	stop()
	<-done
}

// runTask runs one task and turns a panic into the error it never got to return.
//
// Recovering here rather than leaving it to the group is what makes a panic reportable at all: the
// group keeps only the first error its tasks produce, so a panic is lost whenever a sibling failed
// first, and the run is then reported as that sibling's failure alone.
func runTask(ctx context.Context, task func(context.Context) error) (err error) {
	defer func() {
		r := recover()
		if r == nil {
			return
		}

		// A panic value that is already an error is wrapped rather than formatted, so a caller can
		// still match on what was panicked with.
		if e, ok := r.(error); ok {
			err = fmt.Errorf("task panicked: %w\n%s", e, debug.Stack())

			return
		}

		err = fmt.Errorf("task panicked: %v\n%s", r, debug.Stack())
	}()

	return task(ctx)
}

// runState is what a starting run found on the Lifecycle it belongs to.
type runState uint8

const (
	// runStateReady means the run was recorded and may proceed.
	runStateReady runState = iota
	// runStateRunning means another run holds this Lifecycle.
	runStateRunning
	// runStateStopped means this Lifecycle was stopped, so there is nothing left to run.
	runStateStopped
)

// begin records the cancel of a starting run and hands back the channel that closes on its end.
// Deciding this under the same lock Stop takes is what keeps a stop from being lost: outside it, a
// run can begin between a Stop finding nothing to cancel and that Stop returning.
func (l *Lifecycle) begin(stop context.CancelFunc) (chan struct{}, runState) {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.stopped {
		return nil, runStateStopped
	}
	if l.stop != nil {
		// The running run's own channel, so a call that starts nothing can still wait for the run it
		// collided with rather than for its caller.
		return l.done, runStateRunning
	}

	done := make(chan struct{})
	l.stop, l.done = stop, done

	return done, runStateReady
}

// end retires the finished run and releases every Stop waiting on it.
func (l *Lifecycle) end(done chan struct{}) {
	l.mu.Lock()
	l.stop, l.done = nil, nil
	l.mu.Unlock()

	close(done)
}
