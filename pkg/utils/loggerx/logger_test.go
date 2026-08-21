package loggerx

import (
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

var errPanicking = errors.New("panicking pass")

// TestRecoverWithStackScanner pins that the exported helpers recover, which is the whole of what
// their callers ask of them: every call site is a `defer loggerx.RecoverWith...` guarding a pass that
// must not take the process down with it.
//
// The panic is raised one call below the guard and observed by an outer recover of the shape the Go
// spec blesses, so the case can tell "the callback ran" from "the panic went past it" instead of
// inferring one from the other. The helper is deferred through the table's function value, which is
// the exported function itself — the same frame a call site defers.
func TestRecoverWithStackScanner(t *testing.T) {
	testCases := []struct {
		name string
		// recoverWith is the exported helper under test, deferred by the loop below.
		recoverWith func(RecoverStackScannerFunc)
		// panicValue is what the guarded pass panics with.
		panicValue any
		// expectedError is the error the callback must receive, when the panic value is already one.
		expectedError error
		// expectedMessage is the message the callback's error must carry, when it is not.
		expectedMessage string
	}{
		{
			name:          "the goroutine scanner reports a panicking pass",
			recoverWith:   RecoverWithGoroutineStackScanner,
			panicValue:    errPanicking,
			expectedError: errPanicking,
		},
		{
			name:          "the full scanner reports a panicking pass",
			recoverWith:   RecoverWithStackScanner,
			panicValue:    errPanicking,
			expectedError: errPanicking,
		},
		{
			name:            "a panic value that is not an error is wrapped",
			recoverWith:     RecoverWithGoroutineStackScanner,
			panicValue:      "runtime error: invalid memory address",
			expectedMessage: "panic: runtime error: invalid memory address",
		},
		{
			name:            "a panic value that is not even a string is wrapped by its printed form",
			recoverWith:     RecoverWithStackScanner,
			panicValue:      42,
			expectedMessage: "panic: 42",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var (
				fired bool
				got   error
				stack []string
			)

			escaped := func() (escaped any) {
				defer func() { escaped = recover() }()
				func() {
					defer tc.recoverWith(func(s Scanner, e error) {
						fired, got = true, e
						for s.Scan() {
							stack = append(stack, s.Text())
						}
					})
					panic(tc.panicValue)
				}()
				return nil
			}()

			assert.Nil(t, escaped, "the panic must not travel past the guard")
			require.True(t, fired, "the callback must run")
			if tc.expectedError != nil {
				assert.ErrorIs(t, got, tc.expectedError, "an error panic value is passed through as it is")
			} else {
				assert.EqualError(t, got, tc.expectedMessage)
			}
			assert.NotEmpty(t, stack, "the callback must be handed the stack of the pass that panicked")
			assert.True(t, strings.Contains(strings.Join(stack, "\n"), "TestRecoverWithStackScanner"),
				"the stack must be the panicking goroutine's own, %q", stack)
		})
	}
}

// TestRecoverWithStackScannerWholeStack pins that the trace handed to the callback is the whole one.
// runtime.Stack fills the buffer it is given and says nothing about what it left out, so a fixed buffer
// cuts the dump mid-frame — and the guard that asks for every goroutine's stack runs inside a device
// manager, whose own is far past 64KiB. Measured on hardware before this was fixed: the last line of a
// detector's dump was `github.com/alitto/pond/v2.invok`.
func TestRecoverWithStackScannerWholeStack(t *testing.T) {
	const parked = 2000

	release := make(chan struct{})
	var running sync.WaitGroup
	running.Add(parked)
	for range parked {
		go func() {
			running.Done()
			<-release
		}()
	}
	running.Wait()
	t.Cleanup(func() { close(release) })

	var goroutines int
	func() {
		defer RecoverWithStackScanner(func(s Scanner, _ error) {
			for s.Scan() {
				if strings.HasPrefix(s.Text(), "goroutine ") {
					goroutines++
				}
			}
		})
		panic(errPanicking)
	}()

	assert.GreaterOrEqual(t, goroutines, parked,
		"the trace must carry every goroutine, not as many as fit a fixed buffer")
}

// TestRecoverWithStackScannerWithoutPanic pins that the guard costs a pass that returned normally
// nothing: it must not invent an error for it, which would turn every guarded call into a failure.
func TestRecoverWithStackScannerWithoutPanic(t *testing.T) {
	var fired bool

	func() {
		defer RecoverWithGoroutineStackScanner(func(Scanner, error) { fired = true })
	}()

	assert.False(t, fired, "the callback must not run for a pass that did not panic")
}
