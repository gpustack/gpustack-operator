package contextx

import (
	"context"
	"time"

	"gpustack.ai/gpustack/pkg/utils/gox"
)

func Background(stop <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	gox.Go(func() {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		cancel()
	})
	return ctx
}

func TODO(stop <-chan struct{}) context.Context {
	ctx, cancel := context.WithCancel(context.TODO())
	gox.Go(func() {
		select {
		case <-stop:
		case <-ctx.Done():
		}
		cancel()
	})
	return ctx
}

func WithCancel(ctx context.Context, ctxs ...context.Context) (context.Context, context.CancelFunc) {
	if len(ctxs) == 0 {
		ctxs = []context.Context{ctx}
	} else {
		ctxs = append(ctxs, ctx)
	}

	sctx, scancel := context.WithCancel(ctx)

	for _, c := range ctxs {
		if c == nil {
			continue
		}

		select {
		case <-c.Done():
			scancel()
			return sctx, scancel
		default:
		}

		parent := c
		gox.Go(func() {
			select {
			case <-parent.Done():
				scancel()
			case <-sctx.Done():
			}
		})
	}

	return sctx, scancel
}

func WithCancelCause(ctx context.Context, ctxs ...context.Context) (context.Context, context.CancelCauseFunc) {
	if len(ctxs) == 0 {
		ctxs = []context.Context{ctx}
	} else {
		ctxs = append(ctxs, ctx)
	}

	sctx, scancel := context.WithCancelCause(ctx)

	for _, c := range ctxs {
		if c == nil {
			continue
		}

		select {
		case <-c.Done():
			scancel(context.Cause(c))
			return sctx, scancel
		default:
		}

		parent := c
		gox.Go(func() {
			select {
			case <-parent.Done():
				scancel(context.Cause(parent))
			case <-sctx.Done():
			}
		})
	}

	return sctx, scancel
}

func WithoutCancel(ctx context.Context, ctxs ...context.Context) context.Context {
	ctx, _ = WithCancel(ctx, ctxs...)
	return ctx
}

func WithDeadline(ctx context.Context, deadline time.Time, ctxs ...context.Context) (context.Context, context.CancelFunc) {
	sctx, scancel := context.WithDeadline(ctx, deadline)
	return WithoutCancel(sctx, ctxs...), scancel
}

func WithDeadlineCause(ctx context.Context, deadline time.Time, cause error, ctxs ...context.Context) (context.Context, context.CancelFunc) {
	sctx, scancel := context.WithDeadlineCause(ctx, deadline, cause)
	return WithoutCancel(sctx, ctxs...), scancel
}

func WithTimeout(ctx context.Context, timeout time.Duration, ctxs ...context.Context) (context.Context, context.CancelFunc) {
	sctx, scancel := context.WithTimeout(ctx, timeout)
	return WithoutCancel(sctx, ctxs...), scancel
}

func WithTimeoutCause(ctx context.Context, timeout time.Duration, cause error, ctxs ...context.Context) (context.Context, context.CancelFunc) {
	sctx, scancel := context.WithTimeoutCause(ctx, timeout, cause)
	return WithoutCancel(sctx, ctxs...), scancel
}
