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

// GroupWithContext returns a waiting group and a context derived by the given context.Context.
// Waiting group notifies closing when any task raises error,
// any submitting task should use the returning context to receive quiting.
func GroupWithContext(ctx context.Context) IWaitGroup {
	g := gp.NewGroupContext(ctx)
	lg := klog.Background().WithName("gopool")

	return contextWaitGroup{lg: lg, g: g}
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

// GroupWithContextIn is similar as GroupWithContext but doesn't return a derived context,
// all tasks can receive the derived context at submitting, a kind of more compact usage.
func GroupWithContextIn(ctx context.Context) IContextWaitGroup {
	var g embeddedContextWaitGroup
	g.g, g.c = GroupWithContext(ctx), ctx

	return g
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

// QueuingGroupWithContextIn is similar as QueuingGroupWithContext but doesn't return a derived context,
// all tasks can receive the derived context at submitting, a kind of more compact usage.
func QueuingGroupWithContextIn(ctx context.Context, queueSize int) IContextWaitGroup {
	if queueSize <= 0 {
		queueSize = 1
	}

	var g embeddedContextWaitGroup
	g.g, g.c = GroupWithContext(ctx), ctx

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
