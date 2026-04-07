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
func RecoverWithStackScanner(cb RecoverStackScannerFunc) {
	recoverWithStackScanner(true, cb)
}

// RecoverWithGoroutineStackScanner recovers from a panic and provides a stack scanner
// that contains the stack trace of the goroutine where the panic occurred.
func RecoverWithGoroutineStackScanner(cb RecoverStackScannerFunc) {
	recoverWithStackScanner(false, cb)
}

func recoverWithStackScanner(all bool, cb RecoverStackScannerFunc) {
	if r := recover(); r != nil {
		b := make([]byte, 64<<10)
		n := runtime.Stack(b, all)
		s := bufio.NewScanner(bytes.NewReader(b[:n]))
		var e error
		if err, ok := r.(error); ok {
			e = err
		} else {
			e = fmt.Errorf("panic: %v", r)
		}
		cb(_Scanner{s}, e)
	}
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
