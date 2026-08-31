// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dmi

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wrapper list in dmi_mig_wrapper_api.def is what the C wrapper expands into function pointers,
// wrappers and dlsym calls, and a slip in it is invisible to every other check here: the macro
// expands to C that compiles, cgo generates Go that compiles, and the wrong argument reaches the
// driver.
//
// This API is unusually exposed to that. Three of its calls take two same-typed out-parameters in a
// row -- nvmlGetSystemMigMode and nvmlDeviceGetMigMode take (current_mode, pending_mode), and
// nvmlDeviceGetGpuInstanceProfileInfo's neighbors are all `unsigned int` -- so swapping a pair
// changes nothing a compiler or a type-sequence comparison can see. That is why the checks below
// compare parameter NAMES as well as types.
//
// The files read here are the copies hack/generate.sh places in this package, which are the ones
// actually compiled.

// Entry points bound by DMI_MIG_API_LIST that the vendor header does not declare. A reason is
// required for each, so binding a symbol the header never mentions cannot happen silently.
var undeclaredBoundEntryPoints = map[string]string{
	"nvmlDeviceGetUtilizationRates": "exported by libhydmi_mig.so but declared nowhere in the vendor " +
		"header; dmi_mig_wrapper.h asserts NVML's signature for it, validated on hardware against a " +
		"MIG device handle",
}

var (
	// nvmlReturn_t nvmlName(params);  -- params wrap across lines, so the class is negated rather
	// than dotted.
	headerDeclPattern = regexp.MustCompile(`nvmlReturn_t\s+(nvml\w+)\s*\(([^)]*)\)\s*;`)

	// X(nvmlReturn_t, name, (params), (call args))
	defEntryPattern = regexp.MustCompile(`X\(\s*nvmlReturn_t\s*,\s*(nvml\w*)\s*,\s*\(([^)]*)\)\s*,\s*\(([^)]*)\)\s*\)`)

	blockComment = regexp.MustCompile(`(?s)/\*.*?\*/`)
	lineComment  = regexp.MustCompile(`//[^\n]*`)
)

// defEntry is one X(...) line: the parameters the generated wrapper declares, and the arguments it
// forwards to the vendor symbol.
type defEntry struct {
	params   []string
	callArgs []string
}

// canonicalParam rewrites one parameter declaration so that spelling differences which mean the same
// thing compare equal, while the parameter name is kept -- it is the only thing that distinguishes
// two same-typed parameters from each other. `const` is dropped: the header and the list may
// disagree about it without disagreeing about the ABI.
func canonicalParam(decl string) string {
	decl = strings.ReplaceAll(decl, "const ", " ")

	stars := strings.Count(decl, "*")
	fields := strings.Fields(strings.ReplaceAll(decl, "*", " "))
	if stars == 0 || len(fields) < 2 {
		return strings.Join(fields, " ")
	}

	typeFields, name := fields[:len(fields)-1], fields[len(fields)-1]
	return strings.Join(typeFields, " ") + " " + strings.Repeat("*", stars) + " " + name
}

// canonicalParams splits a parameter list and canonicalizes each entry.
func canonicalParams(list string) []string {
	var params []string
	for _, decl := range strings.Split(list, ",") {
		if canonical := canonicalParam(decl); canonical != "" {
			params = append(params, canonical)
		}
	}
	return params
}

// paramNames returns the declared name of each canonicalized parameter, in order.
func paramNames(params []string) []string {
	names := make([]string, 0, len(params))
	for _, param := range params {
		fields := strings.Fields(param)
		names = append(names, fields[len(fields)-1])
	}
	return names
}

// readHeaderDecls parses the vendored header into entry point name -> canonicalized parameters.
// Comments are stripped first: the doc block above every declaration names nvmlReturn_t repeatedly
// in its @return lines, and those are not declarations.
func readHeaderDecls(t *testing.T) map[string][]string {
	t.Helper()

	content, err := os.ReadFile("dmi_mig.h")
	require.NoError(t, err, "reading the vendored header")

	body := lineComment.ReplaceAllString(blockComment.ReplaceAllString(string(content), ""), "")

	decls := make(map[string][]string)
	for _, match := range headerDeclPattern.FindAllStringSubmatch(body, -1) {
		decls[match[1]] = canonicalParams(match[2])
	}
	require.NotEmpty(t, decls, "no nvml* declaration parsed out of the header")

	return decls
}

// readDefEntries parses the wrapper list into entry point name -> declaration.
func readDefEntries(t *testing.T) map[string]defEntry {
	t.Helper()

	content, err := os.ReadFile("dmi_mig_wrapper_api.def")
	require.NoError(t, err, "reading the wrapper list")

	entries := make(map[string]defEntry)
	for _, match := range defEntryPattern.FindAllStringSubmatch(string(content), -1) {
		var callArgs []string
		for _, arg := range strings.Split(match[3], ",") {
			if arg = strings.TrimSpace(arg); arg != "" {
				callArgs = append(callArgs, arg)
			}
		}
		entries[match[1]] = defEntry{params: canonicalParams(match[2]), callArgs: callArgs}
	}
	require.NotEmpty(t, entries, "no entry parsed out of the wrapper list")

	return entries
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	return keys
}

// Three contracts between the header and the wrapper list, each able to fail on its own: an entry
// point the header declares and the list has not bound, an entry in the list the header does not
// declare, and a count that drifted on both sides at once.
func TestWrapperDefCoversHeader(t *testing.T) {
	decls := readHeaderDecls(t)
	entries := readDefEntries(t)

	t.Run("every declared entry point is bound", func(t *testing.T) {
		for _, name := range sortedKeys(decls) {
			assert.Contains(t, entries, name,
				"%s is declared by the vendored header but DMI_MIG_API_LIST does not bind it, so every "+
					"call to it would report FUNCTION_NOT_FOUND", name)
		}
	})

	t.Run("a bound entry point the header does not declare carries a reason", func(t *testing.T) {
		for _, name := range sortedKeys(entries) {
			if _, declared := decls[name]; declared {
				continue
			}
			reason := undeclaredBoundEntryPoints[name]
			assert.NotEmpty(t, reason,
				"%s is bound by DMI_MIG_API_LIST but the vendored header does not declare it; its "+
					"signature is therefore asserted rather than inherited, which needs a recorded reason",
				name)
		}
	})

	t.Run("the bound count is the declared count plus the documented additions", func(t *testing.T) {
		assert.Equal(t, len(decls)+len(undeclaredBoundEntryPoints), len(entries))
	})
}

// The wrapper's own parameters must be the header's, name included. Types alone are not enough: this
// API takes two `unsigned int *` in a row in three places, so only the names tell them apart.
func TestWrapperDefParametersMatchHeader(t *testing.T) {
	decls := readHeaderDecls(t)
	entries := readDefEntries(t)

	for _, name := range sortedKeys(decls) {
		t.Run(name, func(t *testing.T) {
			entry, bound := entries[name]
			require.True(t, bound, "%s is declared by the header but not bound", name)

			assert.Equal(t, decls[name], entry.params,
				"%s: the wrapper list's parameters differ from the vendored header's", name)
		})
	}
}

// A wrapper whose declaration matches the header can still forward its arguments in the wrong order:
// the call-argument list is written separately from the parameter list, and two same-typed
// out-parameters swapped there compile and run, returning the pending mode as the current one.
func TestWrapperDefCallArgsForwardParametersInOrder(t *testing.T) {
	entries := readDefEntries(t)

	for _, name := range sortedKeys(entries) {
		t.Run(name, func(t *testing.T) {
			entry := entries[name]
			assert.Equal(t, paramNames(entry.params), entry.callArgs,
				"%s: the wrapper forwards its arguments in an order its own parameter list does not use",
				name)
		})
	}
}

// The whole reason this package exists as a wrapper rather than a straight generation off the vendor
// header: the library exports NVML's own symbol names, and binding/nvml emits those same names. A
// package that reached one of them directly would resolve against whichever library reached the
// process's global scope first, and the misroute is silent — the return enum is NVML's verbatim, so
// a wrong-library answer looks like a right one.
//
// Regenerating without the wrapper produces exactly that: c-for-go emits `C.nvmlDeviceGetCount` in
// place of `C.w_nvmlDeviceGetCount`, everything still compiles, and nothing else here notices. So
// every cgo call this package makes must carry the w_ prefix.
func TestPackageNeverCallsAVendorSymbolDirectly(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	require.NoError(t, err, "listing the package's Go sources")
	require.NotEmpty(t, sources, "no Go source found to check")

	// A CALL to C.nvml..., with no w_ in front of it -- the leading word boundary excludes the
	// wrapped C.w_nvml... form, which Go's regexp cannot express as a negative lookbehind. The
	// vendor's TYPES are reached by their own names and must be: they are typedefs the wrapper
	// header includes, they occupy no symbol in the shared object, and so they cannot misroute. A
	// conversion to one reads as a call, which is why the _t suffix is excluded here.
	bareCall := regexp.MustCompile(`\bC\.(nvml\w*)\s*\(`)

	var found []string
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}

		content, rerr := os.ReadFile(source)
		require.NoError(t, rerr, "reading %s", source)

		for _, match := range bareCall.FindAllStringSubmatch(string(content), -1) {
			if strings.HasSuffix(match[1], "_t") {
				continue
			}
			found = append(found, source+": C."+match[1])
		}
	}
	sort.Strings(found)

	assert.Empty(t, found,
		"a cgo call reaches a vendor symbol by its own name, which collides with binding/nvml;"+
			" it must go through the w_ wrapper")
}
