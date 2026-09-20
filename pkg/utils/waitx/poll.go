// Package waitx wraps apimachinery's wait with helpers whose callback returns a plain error
// instead of the (done bool, err error) pair.
//
// The error means the opposite of what it means upstream. In wait.PollUntilContextCancel an
// error from the callback ENDS the wait; in every helper here an error means "not yet" and the
// helper calls again. Success is the nil return, and the nil return is what stops a retry.
//
// The names in this package deliberately do not mirror the upstream ones. They used to, and a
// caller reading a familiar name carried the upstream contract across with it: a device-plugin
// server logged a failed read, fell through to nil, and ended its retry loop reporting success
// having sent nothing. The names now say which of the two families a helper belongs to.
//
//   - Retry* stops as soon as the attempt returns nil. An error means try again.
//   - Repeat* never stops on nil. It runs on its interval until the context ends, whatever the
//     attempt returns, and hands the caller the last error it saw.
package waitx

import (
	"context"
	"errors"
	"time"

	"k8s.io/apimachinery/pkg/util/wait"
)

// AttemptFunc is one try at something that may not be ready yet.
//
// Returning nil means it is done. Returning an error means it is not, and the helper running it
// decides what happens next -- Retry* calls it again, Repeat* records the error and carries on.
// It is named an attempt rather than a condition because a condition returning an error reads as
// "evaluating the condition failed", which is not what an error here means.
type AttemptFunc = func(context.Context) error

// ErrStopRetrying is returned by an AttemptFunc that wants to give up rather than be tried again.
// The helper stops and returns the last real error the attempt produced.
//
// An attempt that gives up before it has ever returned a real error leaves nothing to report, so
// the helper returns nil, which its caller reads as success. Give up on the first attempt only
// where that reading is the intended one.
var ErrStopRetrying = errors.New("stop retrying")

// RetryUntilSuccess calls attempt on the given interval until it returns nil, and then stops.
//
// An error from attempt means "not ready", so it is called again; this is the inverse of
// wait.PollUntilContextCancel, whose condition ends the wait by returning an error. An attempt
// that only logs its failure and returns nil ends this helper reporting success.
//
// When the context ends first, the last error attempt returned is what comes back. An attempt can
// also give up by returning ErrStopRetrying.
func RetryUntilSuccess(
	ctx context.Context, interval time.Duration, immediate bool, attempt AttemptFunc,
) error {
	var lastErr error

	return wait.PollUntilContextCancel(ctx, interval, immediate, func(ctx context.Context) (bool, error) {
		err := attempt(ctx)

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
			// The attempt gave up rather than failed. Hand back what it failed with before.
			if errors.Is(err, ErrStopRetrying) {
				return true, lastErr
			}
			// Not ready. Remember why, and try again on the next interval.
			lastErr = err
			return false, nil // nolint:nilerr
		}

		// The attempt succeeded, so there is nothing left to retry.
		return true, nil
	})
}

// RetryUntilSuccessWithTimeout is RetryUntilSuccess bounded by its own deadline on top of the
// context's. It gives up when either ends, returning the last error attempt returned.
func RetryUntilSuccessWithTimeout(
	ctx context.Context, interval, timeout time.Duration, immediate bool, attempt AttemptFunc,
) error {
	deadlineCtx, deadlineCancel := context.WithTimeout(ctx, timeout)
	defer deadlineCancel()

	return RetryUntilSuccess(deadlineCtx, interval, immediate, attempt)
}

// RepeatUntilContextCancel calls attempt on the given interval until the context ends, whatever
// attempt returns.
//
// Unlike RetryUntilSuccess, a nil return does not stop it: this is the helper for work that has
// no completion, such as a monitor loop. The error an attempt returns is recorded rather than
// acted on, and the last one is what comes back when the context ends. An attempt can stop the
// loop early by returning ErrStopRetrying.
func RepeatUntilContextCancel(
	ctx context.Context, interval time.Duration, immediate bool, attempt AttemptFunc,
) error {
	var lastErr error

	return wait.PollUntilContextCancel(ctx, interval, immediate, func(ctx context.Context) (bool, error) {
		err := attempt(ctx)

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
			// The attempt asked to stop. Hand back what it failed with before.
			if errors.Is(err, ErrStopRetrying) {
				return true, lastErr
			}
			// Remember why this round failed. It does not end the loop.
			lastErr = err
		}

		// Success does not end this loop either. Wait out the interval and go again.
		return false, nil
	})
}
