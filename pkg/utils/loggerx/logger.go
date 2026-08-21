package loggerx

import (
	"bufio"
	"bytes"
	"fmt"
	"runtime"
	"strings"
)

type (
	Scanner interface {
		Scan() bool
		Text() string
	}
	// RecoverStackScannerFunc is a callback function type that takes a bufio.Scanner and an error as parameters.
	// The bufio.Scanner contains the stack trace of a panic, and the error represents the panic error.
	RecoverStackScannerFunc = func(s Scanner, e error)
)

// RecoverWithStackScanner recovers from a panic and provides a stack scanner
// that contains the full stack trace of the panic.
//
// It has to be deferred directly — `defer loggerx.RecoverWithStackScanner(cb)` — because that is what
// lets it recover at all: a recover only yields the panic value when the function calling it is the
// one that was deferred. Wrapping this call in another function silently disarms it.
func RecoverWithStackScanner(cb RecoverStackScannerFunc) {
	if r := recover(); r != nil {
		reportRecovered(true, r, cb)
	}
}

// RecoverWithGoroutineStackScanner recovers from a panic and provides a stack scanner
// that contains the stack trace of the goroutine where the panic occurred.
//
// It has to be deferred directly, for the reason RecoverWithStackScanner gives.
func RecoverWithGoroutineStackScanner(cb RecoverStackScannerFunc) {
	if r := recover(); r != nil {
		reportRecovered(false, r, cb)
	}
}

// maxStackBytes bounds the buffer the stack is read into. Growing until the trace fits is what makes
// the trace whole, and a ceiling is what keeps a process that is already unwinding a panic from
// asking for an unbounded allocation to describe it.
const maxStackBytes = 8 << 20

// reportRecovered hands a recovered panic value to cb as an error and a scanner over the stack the
// panic is being unwound from — every goroutine's when all is true, the panicking one's otherwise.
//
// The buffer starts at the size one goroutine's trace needs and doubles until the trace fits: every
// goroutine of a device manager passes that comfortably, and runtime.Stack fills the buffer it is
// given and says nothing about what it left out, so a fixed buffer silently cuts the trace mid-frame.
func reportRecovered(all bool, r any, cb RecoverStackScannerFunc) {
	var (
		b []byte
		n int
	)
	for size := 64 << 10; ; size *= 2 {
		b = make([]byte, size)
		n = runtime.Stack(b, all)
		if n < size || size >= maxStackBytes {
			break
		}
	}
	s := bufio.NewScanner(bytes.NewReader(b[:n]))
	var e error
	if err, ok := r.(error); ok {
		e = err
	} else {
		e = fmt.Errorf("panic: %v", r)
	}
	cb(_Scanner{s}, e)
}

type _Scanner struct {
	s *bufio.Scanner
}

func (s _Scanner) Scan() bool {
	return s.s.Scan()
}

func (s _Scanner) Text() string {
	str := s.s.Text()
	if !strings.Contains(str, "\t") {
		return str
	}

	var strBuilder strings.Builder
	for _, r := range str {
		if r == '\t' {
			strBuilder.WriteString("    ")
		} else {
			strBuilder.WriteRune(r)
		}
	}
	return strBuilder.String()
}
