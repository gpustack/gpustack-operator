package waitx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// scriptedAttempt returns an AttemptFunc handing back results in order, nil once they run out, and
// a counter of how many times it was called. The count is what these tests assert on: whether a
// return value continued or ended a loop is only visible in whether the attempt ran again.
func scriptedAttempt(results ...error) (AttemptFunc, *int) {
	var calls int

	return func(context.Context) error {
		calls++
		if calls <= len(results) {
			return results[calls-1]
		}

		return nil
	}, &calls
}

// TestRetryUntilSuccess is the executable form of this helper's contract, which is the inverse of
// the apimachinery function it used to be named after: there a condition's error ends the wait,
// here it means "not ready" and the attempt runs again. Reading the code has not been enough --
// nine device-plugin servers shipped a loop that logged a failure and returned nil, which ended
// the retry reporting success.
func TestRetryUntilSuccess(t *testing.T) {
	sentinel := errors.New("not ready")

	cases := []struct {
		name      string
		results   []error
		wantCalls int
		wantErr   error
	}{
		{
			name:      "an attempt that succeeds at once is not run again",
			wantCalls: 1,
		},
		{
			name:      "an error means not ready, so the attempt runs again until it succeeds",
			results:   []error{sentinel, sentinel},
			wantCalls: 3,
		},
		{
			name:      "giving up reports what the attempt last failed with, not the giving up",
			results:   []error{sentinel, ErrStopRetrying},
			wantCalls: 2,
			wantErr:   sentinel,
		},
		{
			// The sharp edge the doc comment warns about, pinned so it cannot change silently.
			name:      "giving up before anything failed reports success, because nothing failed",
			results:   []error{ErrStopRetrying},
			wantCalls: 1,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			attempt, calls := scriptedAttempt(c.results...)

			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()

			err := RetryUntilSuccess(ctx, time.Millisecond, true, attempt)

			if c.wantErr != nil {
				require.ErrorIs(t, err, c.wantErr)
			} else {
				require.NoError(t, err)
			}
			assert.Equal(t, c.wantCalls, *calls, "the attempt ran a different number of times")
		})
	}
}

// TestRetryUntilSuccess_ContextEndFirst covers the exit this helper takes when the attempt never
// succeeds: the caller gets the reason the attempt kept failing, not the bare cancellation.
func TestRetryUntilSuccess_ContextEndFirst(t *testing.T) {
	sentinel := errors.New("still not ready")

	ctx, cancel := context.WithCancel(context.Background())
	var calls int
	err := RetryUntilSuccess(ctx, time.Millisecond, true, func(context.Context) error {
		calls++
		if calls == 3 {
			cancel()
		}

		return sentinel
	})
	defer cancel()

	require.ErrorIs(t, err, sentinel, "the caller was not told why the attempt kept failing")
	assert.Equal(t, 3, calls)
}

// TestRepeatUntilContextCancel is the other half of the pair, and the reason the two carry
// different verbs: a nil return ends a Retry and does not end a Repeat. A Repeat named like a
// Retry is the mistake this separation exists to prevent.
func TestRepeatUntilContextCancel(t *testing.T) {
	t.Run("success does not end the loop", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		var calls int
		err := RepeatUntilContextCancel(ctx, time.Millisecond, true, func(context.Context) error {
			calls++
			if calls == 3 {
				cancel()
			}

			return nil
		})

		require.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 3, calls, "a nil return ended the loop, which is Retry's behavior not Repeat's")
	})

	t.Run("giving up ends the loop and reports the last failure", func(t *testing.T) {
		sentinel := errors.New("a round failed")

		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()

		attempt, calls := scriptedAttempt(sentinel, ErrStopRetrying)
		err := RepeatUntilContextCancel(ctx, time.Millisecond, true, attempt)

		require.ErrorIs(t, err, sentinel)
		assert.Equal(t, 2, *calls)
	})
}
