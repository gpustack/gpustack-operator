package gox

import (
	"context"
	"sync"

	pond "github.com/alitto/pond/v2"
	"go.uber.org/multierr"
	klog "k8s.io/klog/v2"

	"gpustack.ai/gpustack/pkg/utils/loggerx"
)

type IWaitGroup interface {
	Wait() error
	Go(func() error)
}

type IContextWaitGroup interface {
	Wait() error
	Go(func(context.Context) error)
}

// Group returns a waiting group,
// which closes at all tasks finishing and aggregates errors from tasks.
func Group() IWaitGroup {
	lg := klog.Background().WithName("gopool")
	return &waitGroup{lg: lg}
}

type waitGroup struct {
	lg  klog.Logger
	g   sync.WaitGroup
	m   sync.Mutex
	err error
}

// Wait blocks until all tasks completed and aggregates errors from tasks.
func (g *waitGroup) Wait() error {
	g.g.Wait()
	return g.err
}

// Go submits a task as goroutine.
func (g *waitGroup) Go(f func() error) {
	if f == nil {
		return
	}

	wf := func() (err error) {
		defer loggerx.RecoverWithGoroutineStackScanner(func(s loggerx.Scanner, e error) {
			g.lg.Error(e, "caught panic in task")
			for s.Scan() {
				g.lg.Error(nil, s.Text())
			}
			err = e
		})

		return f()
	}

	g.g.Add(1)
	Go(func() {
		defer g.g.Done()

		err := wf()
		if err != nil {
			g.m.Lock()
			g.err = multierr.Append(g.err, err)
			g.m.Unlock()
		}
	})
}

// QueuingGroup returns a waiting group with a queue size,
// which closes at all tasks finishing and aggregates errors from tasks.
func QueuingGroup(queueSize int) IWaitGroup {
	if queueSize <= 0 {
		queueSize = 1
	}
	return &queuingWaitGroup{wg: Group(), cc: make(chan struct{}, queueSize)}
}

type queuingWaitGroup struct {
	wg IWaitGroup
	cc chan struct{}
}

// Wait blocks until all tasks completed and aggregates errors from tasks.
func (g *queuingWaitGroup) Wait() error {
	return g.wg.Wait()
}

// Go submits a task as goroutine.
func (g *queuingWaitGroup) Go(f func() error) {
	if f == nil {
		return
	}

	g.cc <- struct{}{}
	g.wg.Go(func() error {
		defer func() {
			<-g.cc
		}()

		return f()
	})
}

// GroupWithContext returns a waiting group whose tasks share one context derived from the given one.
// That context is canceled when the given one is, and as soon as any task in the group returns an
// error. Tasks submitted here are not handed it — GroupWithContextIn is the form that does.
func GroupWithContext(ctx context.Context) IWaitGroup {
	return newContextWaitGroup(ctx)
}

// newContextWaitGroup returns the group concretely, so a caller inside this package can reach the
// derived context that the IWaitGroup interface does not carry.
func newContextWaitGroup(ctx context.Context) contextWaitGroup {
	return contextWaitGroup{
		lg: klog.Background().WithName("gopool"),
		g:  gp.NewGroupContext(ctx),
	}
}

type contextWaitGroup struct {
	lg klog.Logger
	g  pond.TaskGroup
}

// Wait blocks until either all tasks completed or
// one of them returned a non-nil error or the context associated to this group
// was canceled.
func (g contextWaitGroup) Wait() error {
	return g.g.Wait()
}

// context returns the context this group's tasks are meant to run under.
func (g contextWaitGroup) context() context.Context {
	return g.g.Context()
}

// Go submits a task as goroutine.
func (g contextWaitGroup) Go(f func() error) {
	if f == nil {
		return
	}

	wf := func() (err error) {
		defer loggerx.RecoverWithGoroutineStackScanner(func(s loggerx.Scanner, e error) {
			g.lg.Error(e, "caught panic in task")
			for s.Scan() {
				g.lg.Error(nil, s.Text())
			}
			err = e
		})

		return f()
	}

	g.g.SubmitErr(wf)
}

// QueuingGroupWithContext returns a waiting group with a queue size and a context derived by the given context.Context.
// Waiting group notifies closing when any task raises error,
// any submitting task should use the returning context to receive quiting.
func QueuingGroupWithContext(ctx context.Context, queueSize int) IWaitGroup {
	if queueSize <= 0 {
		queueSize = 1
	}
	g := GroupWithContext(ctx)
	return &queuingContextWaitGroup{g: g, cc: make(chan struct{}, queueSize)}
}

type queuingContextWaitGroup struct {
	g  IWaitGroup
	cc chan struct{}
}

// Wait blocks until either all tasks completed or
// one of them returned a non-nil error or the context associated to this group
// was canceled.
func (g *queuingContextWaitGroup) Wait() error {
	return g.g.Wait()
}

// Go submits a task as goroutine.
func (g *queuingContextWaitGroup) Go(f func() error) {
	if f == nil {
		return
	}

	g.g.Go(func() error {
		defer func() {
			<-g.cc
		}()

		return f()
	})
}

// GroupWithContextIn is similar as GroupWithContext but hands each task the group's own derived
// context at submitting, a kind of more compact usage.
//
// That context is deliberately not the caller's. It ends when the caller's does, and also as soon as
// any task in the group returns an error — so a task watching nothing but its context still returns
// when a sibling fails. Which is what lets the group report that failure at all: Wait does not return
// until every task has, so one task waiting on a context nobody cancels holds a sibling's error back
// for as long as the caller lives. A task that simply returns nil cancels nothing, so a group of
// tasks that hand off to each other still runs to completion.
func GroupWithContextIn(ctx context.Context) IContextWaitGroup {
	g := newContextWaitGroup(ctx)

	return embeddedContextWaitGroup{g: g, c: g.context()}
}

type embeddedContextWaitGroup struct {
	g IWaitGroup
	c context.Context
}

// Wait blocks until either all tasks completed or
// one of them returned a non-nil error or the context associated to this group
// was canceled.
func (g embeddedContextWaitGroup) Wait() error {
	return g.g.Wait()
}

// Go submits a task as goroutine.
func (g embeddedContextWaitGroup) Go(f func(context.Context) error) {
	if f == nil {
		return
	}

	g.g.Go(func() error {
		return f(g.c)
	})
}

// QueuingGroupWithContextIn is similar as QueuingGroupWithContext but hands each task the group's own
// derived context at submitting, a kind of more compact usage. That context follows the same rule as
// GroupWithContextIn's.
func QueuingGroupWithContextIn(ctx context.Context, queueSize int) IContextWaitGroup {
	if queueSize <= 0 {
		queueSize = 1
	}

	cwg := newContextWaitGroup(ctx)
	g := embeddedContextWaitGroup{g: cwg, c: cwg.context()}

	return queuingEmbeddedContextWaitGroup{g: g, cc: make(chan struct{}, queueSize)}
}

type queuingEmbeddedContextWaitGroup struct {
	g  embeddedContextWaitGroup
	cc chan struct{}
}

// Wait blocks until either all tasks completed or
// one of them returned a non-nil error or the context associated to this group
// was canceled.
func (g queuingEmbeddedContextWaitGroup) Wait() error {
	return g.g.Wait()
}

// Go submits a task as goroutine.
func (g queuingEmbeddedContextWaitGroup) Go(f func(context.Context) error) {
	if f == nil {
		return
	}

	select {
	case <-g.g.c.Done():
		return
	case g.cc <- struct{}{}:
	}

	g.g.Go(func(ctx context.Context) error {
		defer func() {
			<-g.cc
		}()

		return f(ctx)
	})
}
