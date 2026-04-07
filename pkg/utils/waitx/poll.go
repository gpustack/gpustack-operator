package waitx

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

type ConditionWithContextFunc = func(context.Context) error

// ErrCanceled is a sentinel error used to indicate that the polling should be canceled by internal logic,
// and the last error should be returned to the caller.
var ErrCanceled = errors.New("polling canceled")

// PollUntilContextCancel is similar to wait.PollUntilContextCancel,
// but it stops until no error returned from ConditionWithContextFunc,
// or the given context is canceled.
//
// When cancellation happens,
// PollUntilContextCancel returns the last error returned from ConditionWithContextFunc.
func PollUntilContextCancel(ctx context.Context, interval time.Duration, immediate bool, condition ConditionWithContextFunc) error {
	var lastErr error

	return wait.PollUntilContextCancel(ctx, interval, immediate, func(ctx context.Context) (bool, error) {
		err := condition(ctx)

		switch cerr := ctx.Err(); {
		case cerr != nil && err != nil:
			// Cancel by outside context, return the real cause.
			if errors.Is(err, cerr) && lastErr != nil {
				return false, lastErr
			}
			return false, err
		case cerr != nil:
			// Return the cancellation error here to make the external behavior consistent.
			return false, cerr
		case err != nil:
			// Cancel by internal logic, return the last error.
			if errors.Is(err, ErrCanceled) {
				return true, lastErr
			}
			// Record the last error.
			lastErr = err
			return false, nil // nolint:nilerr
		}

		// No error, stop polling.
		return true, nil
	})
}

// PollUntilContextTimeout is similar to wait.PollUntilContextTimeout,
// but it stops until no error returned from ConditionWithContextFunc,
// or the given context is canceled or timeout.
//
// When cancellation happens,
// PollUntilContextTimeout returns the last error returned from ConditionWithContextFunc.
func PollUntilContextTimeout(ctx context.Context, interval, timeout time.Duration, immediate bool, condition ConditionWithContextFunc) error {
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, timeout)
	defer deadlineCancel()
	return PollUntilContextCancel(deadlineCtx, interval, immediate, condition)
}

// UntilContextCancel is similar to wait.UntilContextCancel,
// but it stops until the given context is canceled.
//
// When cancellation happens,
// UntilContextCancel returns the last error returned from ConditionWithContextFunc.
func UntilContextCancel(ctx context.Context, interval time.Duration, immediate bool, condition ConditionWithContextFunc) error {
	var lastErr error

	return wait.PollUntilContextCancel(ctx, interval, immediate, func(ctx context.Context) (bool, error) {
		err := condition(ctx)

		switch cerr := ctx.Err(); {
		case cerr != nil && err != nil:
			// Cancel by outside context, return the real cause.
			if errors.Is(err, cerr) && lastErr != nil {
				return false, lastErr
			}
			return false, err
		case cerr != nil:
			// Since we use a background context to check the connection,
			// we should return the cancellation error here to make the external behavior consistent.
			return false, cerr
		case err != nil:
			// Cancel by internal logic, return the last error.
			if errors.Is(err, ErrCanceled) {
				return true, lastErr
			}
			// Record the last error.
			lastErr = err
		}

		// No error, keep polling.
		return false, nil
	})
}
