// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package mtml

import (
	"go/ast"
	"go/parser"
	"go/token"
	"go/types"
	"os"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The table pins both halves of the predicate: every code that means the call could not be made,
// and the near-misses that must not be mistaken for one.
//
// Both driver-version codes count: too old and too new state the same fact about the
// library/driver pair, that no call of this kind can succeed until one side moves.
func TestReturn_IsAPIUnavailable(t *testing.T) {
	testCases := []struct {
		name string
		ret  Return
		want bool
	}{
		{"the library was not found", ERROR_LIBRARY_NOT_FOUND, true},
		{"the symbol is absent from the library", ERROR_FUNCTION_NOT_FOUND, true},
		{"no driver is loaded", ERROR_DRIVER_NOT_LOADED, true},
		{"the driver is too old for the library", ERROR_DRIVER_TOO_OLD, true},
		{"the driver is too new for the library", ERROR_DRIVER_TOO_NEW, true},

		{"success", SUCCESS, false},
		{"the device does not support the feature", ERROR_NOT_SUPPORTED, false},
		{"the driver malfunctioned", ERROR_DRIVER_FAILURE, false},
		{"permission is denied", ERROR_NO_PERMISSION, false},
		{"the queried object was not found", ERROR_NOT_FOUND, false},
		{"the call timed out", ERROR_TIMEOUT, false},
		{"the failure is unknown", ERROR_UNKNOWN, false},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equalf(t, tc.want, tc.ret.IsAPIUnavailable(),
				"Return(%d).IsAPIUnavailable()", tc.ret)
		})
	}
}

// No call site writes a field's address into an argument — (*MTML).Init states why one must not
// reach the library. What this pins is the written form: an address that is hoisted into a local
// first still gets through, which would take data flow to see.
//
// Read from the source rather than exercised, because the panic is out of reach here: without
// libmtml.so on the host every entry point returns early, and the symbols only resolve after that
// dlopen — so no machine running this suite can make the call at all.
func TestBindingCallsTakeNoFieldAddress(t *testing.T) {
	entries, err := os.ReadDir(".")
	require.NoError(t, err)

	fset := token.NewFileSet()

	var offenders []string
	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}

		f, err := parser.ParseFile(fset, name, nil, parser.SkipObjectResolution)
		require.NoErrorf(t, err, "parse %s", name)

		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			// The library's own wrappers, which are the calls that reach C. A generated wrapper
			// takes the pointer it is given; only these call sites choose what it points at.
			callee, ok := call.Fun.(*ast.Ident)
			if !ok || !strings.HasPrefix(callee.Name, "mtml") {
				return true
			}
			for _, arg := range call.Args {
				// The whole argument, not only its root: a conversion or a pair of parentheses
				// around the address hands the library the very same pointer, and a conversion is
				// the form the generated wrappers are themselves written in.
				ast.Inspect(arg, func(n ast.Node) bool {
					addr, ok := n.(*ast.UnaryExpr)
					if !ok || addr.Op != token.AND {
						return true
					}
					if _, ok := ast.Unparen(addr.X).(*ast.SelectorExpr); ok {
						offenders = append(offenders, fset.Position(addr.Pos()).String()+
							": "+callee.Name+"(… "+types.ExprString(addr)+" …)")
					}
					return true
				})
			}
			return true
		})
	}

	assert.Emptyf(t, offenders, "take the address of a local and assign the field once the call "+
		"returns, so nothing but that one word reaches the library:\n%s",
		strings.Join(offenders, "\n"))
}
