package inject

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// TestEngines_AreAllKnown pins the enum against the table. An engine value the synthesis accepts but
// the table does not describe would reach SupportsTenant, get a false from a missing entry, and be
// refused for a reason nobody measured -- indistinguishable from a measured refusal.
func TestEngines_AreAllKnown(t *testing.T) {
	for _, engine := range Engines() {
		t.Run(string(engine), func(t *testing.T) {
			facts, ok := engineFactsFor(engine)
			require.True(t, ok, "engine %q is accepted but has no entry in the facts table", engine)
			assert.NotEmpty(t, facts.Version,
				"every entry records the version it was measured at, or a later reader cannot tell "+
					"whether the fact is still current")
			assert.NotEmpty(t, facts.Source,
				"every entry names the source line the fact was read from, so the discriminating "+
					"check can be re-run without finding the file again")
		})
	}
}

// TestSupportsTenant_PinsTheMeasuredAnswerPerEngine states each entry explicitly, so that changing one
// is a deliberate act with a failing test attached rather than an unnoticed edit.
//
// It did exactly that: an earlier revision asserted that EVERY engine truncates, and re-measuring
// SGLang at the version this project actually deploys turned it red. That is the test working. What
// it could not do was notice that the entry had been measured at the wrong ref in the first place -
// a table can only pin what someone read, not whether they read the right thing.
//
// The table below must contain BOTH answers. With one answer it cannot distinguish a per-engine
// measurement from a constant, which is the failure mode that let the wrong entry sit unchallenged.
func TestSupportsTenant_PinsTheMeasuredAnswerPerEngine(t *testing.T) {
	want := map[Engine]bool{
		EngineVLLM: false,
		// False because the release we pin carries no tenant at all - not because of which connector
		// this package renders. That distinction is the point: renderVLLM now writes
		// AscendStoreConnector for this engine, the engine's own store path, and this value did not
		// move. A previous note here predicted it would.
		EngineVLLMAscend: false,
		EngineSGLang:     true,
	}
	require.Len(t, want, len(Engines()), "every accepted engine needs a pinned answer")

	var sawTrue, sawFalse bool
	for _, engine := range Engines() {
		expected, ok := want[engine]
		require.True(t, ok, "engine %q has no pinned answer", engine)
		if expected {
			sawTrue = true
		} else {
			sawFalse = true
		}

		t.Run(string(engine), func(t *testing.T) {
			assert.Equal(t, expected, SupportsTenant(engine),
				"the measurement for %q changed; its source line and version must change with it", engine)
		})
	}
	assert.True(t, sawTrue && sawFalse,
		"both answers must appear, or this pins a constant rather than a per-engine measurement")
}

// TestSupportsTenant_UnknownEngine keeps an unrecognized value on the refusing side. A missing map
// entry already yields the zero value, so this asserts the safe direction is the one the zero value
// happens to land on -- a property worth pinning rather than inheriting.
func TestSupportsTenant_UnknownEngine(t *testing.T) {
	assert.False(t, SupportsTenant(Engine("mystery-engine")))
}

// TestParseEngine covers the only place an engine string enters the package.
func TestParseEngine(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		want  Engine
		valid bool
	}{
		{name: "vllm", input: "vllm", want: EngineVLLM, valid: true},
		{name: "vllm-ascend", input: "vllm-ascend", want: EngineVLLMAscend, valid: true},
		{name: "sglang", input: "sglang", want: EngineSGLang, valid: true},
		{name: "unknown value", input: "tensorrt", valid: false},
		{name: "empty", input: "", valid: false},
		{name: "case is significant", input: "vLLM", valid: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseEngine(tc.input)
			if !tc.valid {
				require.Error(t, err)
				assert.Equal(t, ReasonEngineUnknown, reasonOf(t, err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestParseRole covers the role annotation, whose empty value is legal and means "no role".
func TestParseRole(t *testing.T) {
	testCases := []struct {
		name  string
		input string
		want  Role
		valid bool
	}{
		{name: "prefill", input: "prefill", want: RolePrefill, valid: true},
		{name: "decode", input: "decode", want: RoleDecode, valid: true},
		{name: "unset means no role", input: "", want: RoleNone, valid: true},
		{name: "unknown value", input: "both", valid: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseRole(tc.input)
			if !tc.valid {
				require.Error(t, err)
				assert.Equal(t, ReasonRoleUnknown, reasonOf(t, err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

// TestRefusal_CarriesReasonNotProse pins what callers may depend on. Tests and the webhook branch on
// the reason; the message is for a human and may be reworded without breaking either.
func TestRefusal_CarriesReasonNotProse(t *testing.T) {
	err := newRefusal(ReasonEngineUnknown, "engine %q is not one this webhook accepts; it knows %s",
		"vllm-turbo", "vllm")

	var refusal *RefusalError
	require.ErrorAs(t, err, &refusal)
	assert.Equal(t, ReasonEngineUnknown, refusal.Reason)
	assert.Contains(t, refusal.Error(), "vllm-turbo", "the message names the subject it refused")
	assert.Contains(t, refusal.Error(), "vllm")
}

// reasonOf extracts the typed reason, failing the test when the error is not a RefusalError at all --
// an untyped error reaching a caller is itself the defect.
func reasonOf(t *testing.T, err error) Reason {
	t.Helper()

	var refusal *RefusalError
	require.ErrorAs(t, err, &refusal, "refusals must be typed so callers branch on the reason")
	return refusal.Reason
}
