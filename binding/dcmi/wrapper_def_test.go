// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package dcmi

import (
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The wrapper list in dcmi_wrapper_api.def is transcribed by hand from the vendor headers, and a
// transcription slip there is invisible to every other check in this package: the macro expands to
// C that compiles, cgo generates Go that compiles, and the wrong argument reaches the driver.
//
// dcmiv2_get_device_list is the sharp edge. Its (device_list, device_num) pair is the reverse of
// V1's dcmi_get_card_list (card_num, card_list), and both parameters are `int *`, so a swap changes
// nothing a compiler can see -- and nothing a type-sequence comparison can see either, which is why
// the checks below compare parameter *names* as well as types.
//
// Two distinct mistakes are possible and both are covered:
//
//   - the declaration disagrees with the header, so the wrapper takes its arguments in an order the
//     driver does not use;
//   - the declaration matches the header but the call-argument list reorders them, so the wrapper
//     silently swaps two same-typed arguments on their way through.
//
// The files read here are the copies hack/generate.sh places in this package, which are the ones
// actually compiled.

// Entry points dcmi_interface_api_v2.h declares that DCMI_V2_API_LIST deliberately does not bind.
// A reason is required for each, so dropping an entry point from the list without recording why
// fails TestWrapperDefCoversV2Header rather than passing quietly.
var v2UnboundEntryPoints = map[string]string{
	"dcmiv2_init": "dlsym'd directly in dcmi_wrapper.c, which owns the V1-then-V2 init sequence",
	"dcmiv2_subscribe_fault_event": "the public header declares two parameters while the vendor's own " +
		"function pointer takes three, so the two sources disagree about its binary ABI",
}

var (
	// DCMIDLLEXPORT int dcmiv2_name(params);  -- params may wrap across lines.
	v2HeaderDeclPattern = regexp.MustCompile(`DCMIDLLEXPORT\s+int\s+(dcmiv2_\w+)\s*\(([^)]*)\)\s*;`)

	// X(int, name, (params), (call args))
	defEntryPattern = regexp.MustCompile(`X\(\s*int\s*,\s*(dcmi\w*)\s*,\s*\(([^)]*)\)\s*,\s*\(([^)]*)\)\s*\)`)
)

// defEntry is one X(...) line of a wrapper list: the parameters the generated wrapper declares, and
// the arguments it forwards to the vendor symbol.
type defEntry struct {
	params   []string
	callArgs []string
}

// canonicalParam rewrites one parameter declaration so that spelling differences which mean the same
// thing compare equal, while the parameter name is kept -- it is the only thing that distinguishes
// two same-typed parameters from each other.
func canonicalParam(decl string) string {
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

// readV2HeaderDecls parses the V2 header into entry point name -> canonicalized parameters.
func readV2HeaderDecls(t *testing.T) map[string][]string {
	t.Helper()

	content, err := os.ReadFile("dcmi_interface_api_v2.h")
	require.NoError(t, err, "reading the vendored V2 header")

	decls := make(map[string][]string)
	for _, match := range v2HeaderDeclPattern.FindAllStringSubmatch(string(content), -1) {
		decls[match[1]] = canonicalParams(match[2])
	}
	require.NotEmpty(t, decls, "no dcmiv2_* declaration parsed out of the V2 header")

	return decls
}

// readDefEntries parses the wrapper list into entry point name -> declaration, keeping only the
// names the prefix selects so the V1 and V2 lists can be examined separately.
func readDefEntries(t *testing.T, prefix string) map[string]defEntry {
	t.Helper()

	content, err := os.ReadFile("dcmi_wrapper_api.def")
	require.NoError(t, err, "reading the wrapper list")

	entries := make(map[string]defEntry)
	for _, match := range defEntryPattern.FindAllStringSubmatch(string(content), -1) {
		// "dcmiv2_" does not start with "dcmi_" -- the fifth character is 'v', not '_' -- so this one
		// test separates the two lists with no further filtering.
		name := match[1]
		if !strings.HasPrefix(name, prefix) {
			continue
		}

		var callArgs []string
		for _, arg := range strings.Split(match[3], ",") {
			if arg = strings.TrimSpace(arg); arg != "" {
				callArgs = append(callArgs, arg)
			}
		}
		entries[name] = defEntry{params: canonicalParams(match[2]), callArgs: callArgs}
	}
	require.NotEmpty(t, entries, "no %s* entry parsed out of the wrapper list", prefix)

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
// point the header gained and the list has not, an entry in the list the header does not declare,
// and a count that drifted on both sides at once.
func TestWrapperDefCoversV2Header(t *testing.T) {
	decls := readV2HeaderDecls(t)
	entries := readDefEntries(t, "dcmiv2_")

	t.Run("every declared entry point is bound or documented as unbound", func(t *testing.T) {
		for _, name := range sortedKeys(decls) {
			_, bound := entries[name]
			reason, unbound := v2UnboundEntryPoints[name]

			assert.NotEqual(t, bound, unbound,
				"%s must be either bound by DCMI_V2_API_LIST or listed in v2UnboundEntryPoints, "+
					"never both and never neither", name)
			if unbound {
				assert.NotEmpty(t, reason, "%s is listed as unbound with no reason", name)
			}
		}
	})

	t.Run("no bound entry point is absent from the header", func(t *testing.T) {
		for _, name := range sortedKeys(entries) {
			assert.Contains(t, decls, name,
				"%s is bound by DCMI_V2_API_LIST but the vendored V2 header does not declare it", name)
		}
	})

	t.Run("the bound count is the declared count minus the documented exclusions", func(t *testing.T) {
		assert.Equal(t, len(decls)-len(v2UnboundEntryPoints), len(entries))
	})
}

// The wrapper's own parameters must be the header's, name included. Types alone are not enough:
// dcmiv2_get_device_list declares two `int *` in a row, so only the names tell them apart.
func TestWrapperDefParametersMatchV2Header(t *testing.T) {
	decls := readV2HeaderDecls(t)
	entries := readDefEntries(t, "dcmiv2_")

	for _, name := range sortedKeys(decls) {
		if _, unbound := v2UnboundEntryPoints[name]; unbound {
			continue
		}

		t.Run(name, func(t *testing.T) {
			entry, bound := entries[name]
			require.True(t, bound, "%s is declared by the header but not bound", name)

			assert.Equal(t, decls[name], entry.params,
				"%s: the wrapper list's parameters differ from the vendored header's", name)
		})
	}
}

// A wrapper forwards its own parameters positionally, so the call-argument list has to be the
// declared names in declared order. Reordering two same-typed arguments here compiles cleanly and
// swaps them on every call -- the failure this whole file exists for. Both API generations are
// checked: V1's dcmi_get_card_list carries the identical two-`int *` hazard.
func TestWrapperDefCallArgsFollowDeclarationOrder(t *testing.T) {
	for _, prefix := range []string{"dcmi_", "dcmiv2_"} {
		entries := readDefEntries(t, prefix)

		for _, name := range sortedKeys(entries) {
			entry := entries[name]

			t.Run(name, func(t *testing.T) {
				assert.Equal(t, paramNames(entry.params), entry.callArgs,
					"%s: the call arguments are not the declared parameters in declared order", name)
			})
		}
	}
}

// Proof that the comparison above has teeth, without mutating the real files: the swap that no
// compiler and no type-only comparison can see must be visible to this one.
func TestWrapperDefParameterComparisonCatchesASwap(t *testing.T) {
	const headerParams = "int *device_list, int *device_num, int list_len"

	testCases := []struct {
		name      string
		defParams string
		wantEqual bool
	}{
		{
			name:      "transcribed as declared",
			defParams: "int *device_list, int *device_num, int list_len",
			wantEqual: true,
		},
		{
			name:      "spelling differences only",
			defParams: "int*   device_list,int * device_num,  int list_len",
			wantEqual: true,
		},
		{
			name:      "the two same-typed pointers swapped",
			defParams: "int *device_num, int *device_list, int list_len",
			wantEqual: false,
		},
		{
			name:      "transcribed in V1's dcmi_get_card_list order",
			defParams: "int *device_num, int *device_list, int list_len",
			wantEqual: false,
		},
		{
			name:      "a parameter dropped",
			defParams: "int *device_list, int *device_num",
			wantEqual: false,
		},
		{
			name:      "a pointer flattened to a value",
			defParams: "int *device_list, int device_num, int list_len",
			wantEqual: false,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			equal := assert.ObjectsAreEqual(canonicalParams(headerParams), canonicalParams(tc.defParams))
			assert.Equalf(t, tc.wantEqual, equal, "comparing %q against the header's %q",
				tc.defParams, headerParams)
		})
	}
}
