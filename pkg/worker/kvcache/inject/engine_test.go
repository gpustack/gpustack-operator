package inject

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// TestEngines_HaveMeasuredTransportConstraint is the transport table's counterpart to the pin above.
//
// An engine with no entry is LET THROUGH by checkTransport, so a missing row is not a compile error
// and not a refusal -- it is silently the permissive answer. This is what makes the omission visible.
//
// The version and source line keep an empty Required distinguishable from a row nobody measured.
func TestEngines_HaveMeasuredTransportConstraint(t *testing.T) {
	var sawConstrained, sawUnconstrained bool

	for _, engine := range Engines() {
		facts, ok := engineTransportConstraint[engine]
		t.Run(string(engine), func(t *testing.T) {
			require.True(t, ok, "engine %q is rendered but its transport requirement is unmeasured, "+
				"which checkTransport reads as accepting everything", engine)
			assert.NotEmpty(t, facts.Version,
				"the release the answer was read from, or a later reader cannot tell whether it is "+
					"still current")
			assert.NotEmpty(t, facts.Source,
				"the source line the answer was read at -- required even when Required is empty, "+
					"since an unsourced empty is how 'refuses nothing' becomes 'nobody looked'")

			// The two spellings travel together or not at all. A row carrying only the artifact one
			// prints a blank where the remediation names a value to set, and a blank in that sentence
			// reads as an instruction rather than as a missing field.
			assert.Equal(t, facts.Required == "", facts.RequiredAPIValue == "",
				"a required transport must be recorded in both spellings: artifact %q, API %q",
				facts.Required, facts.RequiredAPIValue)
		})

		if facts.Required == "" {
			sawUnconstrained = true
		} else {
			sawConstrained = true
		}
	}

	assert.True(t, sawConstrained && sawUnconstrained,
		"both answers must appear, or this table pins a constant rather than a per-engine measurement")
}

// TestCheckTransport covers the rule in both directions, and the positive rows are not padding.
//
// With only the refusals, a checkTransport that returned an error unconditionally would pass every
// case: the negative rows cannot tell a measured refusal from a broken one. The engine and the
// transport are varied SEPARATELY for the same reason -- vllm-ascend is admitted on one transport and
// refused on three, and tcp is admitted on two engines and refused on one, so neither column can be
// the whole answer.
func TestCheckTransport(t *testing.T) {
	testCases := []struct {
		name     string
		engine   Engine
		protocol string
		accepted bool
	}{
		// THE CASE #172 IS ABOUT, and the condition is not that anybody chose tcp: Auto is the
		// schema's default and the backend resolves it to this value, so a pool left alone lands here.
		{name: "vllm-ascend refuses the transport a default pool offers", engine: EngineVLLMAscend, protocol: "tcp"},
		{name: "vllm-ascend refuses rdma", engine: EngineVLLMAscend, protocol: "rdma"},
		{name: "vllm-ascend refuses hip", engine: EngineVLLMAscend, protocol: "hip"},
		// The positive baseline for the constrained engine. Without it a refusal keyed on the engine
		// alone -- ignoring the transport entirely -- would satisfy every row above.
		{name: "vllm-ascend accepts ascend", engine: EngineVLLMAscend, protocol: "ascend", accepted: true},
		// The positive baseline for the default pool: the ordinary deployment must stay admitted, and
		// this is the row that fails if the rule is ever keyed on the transport alone.
		{name: "vllm accepts the transport a default pool offers", engine: EngineVLLM, protocol: "tcp", accepted: true},
		{name: "vllm accepts ascend", engine: EngineVLLM, protocol: "ascend", accepted: true},
		{name: "sglang accepts the transport a default pool offers", engine: EngineSGLang, protocol: "tcp", accepted: true},
		// SGLang compares protocol to "rdma" and carries on either way, so this row is the one that
		// would break if a re-check ever mistook that comparison for a requirement.
		{name: "sglang accepts rdma", engine: EngineSGLang, protocol: "rdma", accepted: true},
		// An unmeasured engine claims less. Render refuses it earlier as an unknown engine, so
		// this pins that the permissive direction is the one the missing entry lands on.
		{name: "an unmeasured engine is not refused here", engine: Engine("mystery-engine"), protocol: "tcp", accepted: true},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			err := checkTransport(tc.engine, tc.protocol)
			if tc.accepted {
				assert.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Equal(t, ReasonTransportUnsupported, reasonOf(t, err))
		})
	}
}

// TestCheckTransport_MessageNamesThePair pins the half a reason code cannot carry.
//
// Neither half of the pair is wrong on its own -- the transport is a legal enum value and the engine
// is a legal engine -- so a message naming only one of them sends its reader to change the wrong
// object. It also has to name the value that WOULD work, since "unsupported" alone leaves a user
// guessing between four transports.
//
// EACH SPELLING IS PINNED IN ITS OWN SENTENCE, not merely present somewhere in the message. The
// remediation assertion carries the field name with the value, so it fails if the two ever drift
// apart; a check that only asked whether the message contains either spelling would go green on the
// message that names the artifact spelling in the sentence telling an operator what to set -- which
// is what this message did until a review caught it. The value it named there is one the enum
// rejects, so following the error produced a schema rejection: a refusal whose remediation cannot be
// carried out is a worse outcome than a refusal with no remediation at all, because it reads as
// actionable.
func TestCheckTransport_MessageNamesThePair(t *testing.T) {
	err := checkTransport(EngineVLLMAscend, "tcp")
	require.Error(t, err)
	message := err.Error()

	assert.Contains(t, message, string(EngineVLLMAscend), "the engine half of the pair")

	// The pair is reported in the ARTIFACT's spelling, because that is the value the container was
	// handed. Both halves in one substring, so the sentence cannot lose one of them.
	assert.Contains(t, message, `accepts only the "ascend" transport and this pool offers "tcp"`,
		"the pair is reported in the spelling the container actually saw")

	// The remediation is in the API's spelling, because it names a field the schema validates. The
	// field name is part of the assertion: that is what makes this positional rather than a presence
	// check that any mention of the value would satisfy.
	assert.Contains(t, message, `spec.transport.protocol to "CANN"`,
		"the remediation must name a value the enum accepts, next to the field it goes in")
	assert.NotContains(t, message, `spec.transport.protocol to "ascend"`,
		"the artifact spelling in that sentence is a schema rejection waiting to happen")

	assert.Contains(t, message, "KVCacheBackend",
		"the transport is the backend's, and a reader looking for it on the Binding finds nothing")
	assert.Contains(t, message, "v0.19.1rc1",
		"the version behind the answer, so the refusal says why rather than only what")
}

// TestMatchTransport pins the pool-aware half: which of a pool's offers an engine is handed, and
// when a pool is refused outright.
//
// The accepted rows are the control that gives the refusal its teeth. A MatchTransport that
// refused every pool would satisfy the refusal row alone, and one that always returned the first
// offer would pass every row but the second — the constraint is what skips an offer, and only
// that row can see it.
func TestMatchTransport(t *testing.T) {
	testCases := []struct {
		name    string
		engine  Engine
		offers  []string
		want    string
		wantErr bool
	}{
		// Repeated offers are one effective transport and remain admissible.
		{
			name:   "an unconstrained engine accepts one effective transport",
			engine: EngineVLLM, offers: []string{"rdma", "rdma"}, want: "rdma",
		},
		{
			name:   "an unconstrained engine refuses mixed offers",
			engine: EngineVLLM, offers: []string{"rdma", "tcp"}, wantErr: true,
		},
		{
			name:   "a constrained engine takes the first offer it accepts",
			engine: EngineVLLMAscend, offers: []string{"tcp", "ascend"}, want: "ascend",
		},
		{
			name:   "a constrained engine refuses a pool no group serves",
			engine: EngineVLLMAscend, offers: []string{"tcp", "rdma"}, wantErr: true,
		},
		// The same permissive direction as the singular check: refusing on a fact nobody read
		// would turn an unmeasured engine into a broken one.
		{
			name:   "an unmeasured engine is not refused",
			engine: Engine("mystery-engine"), offers: []string{"tcp"}, want: "tcp",
		},
		// The no-store shape is Render's ReasonConnectionIncomplete case, not a transport answer:
		// refusing it here would borrow a refusal that belongs to another reason.
		{
			name:   "no offers is not a transport refusal",
			engine: EngineVLLMAscend, offers: nil, want: "",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := MatchTransport(tc.engine, tc.offers)
			if tc.wantErr {
				require.Error(t, err)
				assert.Equal(t, ReasonTransportUnsupported, reasonOf(t, err))
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}

func TestMatchTransport_MixedOffersMessage(t *testing.T) {
	_, err := MatchTransport(EngineVLLM, []string{"rdma", "", "tcp", "rdma"})
	require.Error(t, err)
	message := err.Error()
	assert.Contains(t, message, `["rdma" "tcp"]`)
	assert.Contains(t, message, "declares no required transport")
	assert.Contains(t, message, "only one transport")
	assert.Contains(t, message, "NotSupportedTransport")
	assert.Contains(t, message, "spec.transport.protocol")
	assert.Contains(t, message, "member group's transport.protocol")
}

// TestMatchTransport_MessageNamesEveryOffer is the plural counterpart of the singular message pin:
// the refusal has to list what the pool's groups DO offer, because "unsupported" alone leaves a
// user guessing between a backend field and a group field.
func TestMatchTransport_MessageNamesEveryOffer(t *testing.T) {
	_, err := MatchTransport(EngineVLLMAscend, []string{"tcp", "rdma"})
	require.Error(t, err)
	message := err.Error()

	assert.Contains(t, message, string(EngineVLLMAscend), "the engine half of the pair")
	assert.Contains(t, message, `accepts only the "ascend" transport and no group in this pool offers it`,
		"the constraint, in the artifact's spelling")
	assert.Contains(t, message, `["tcp" "rdma"]`,
		"every group's offer, so neither serving it is visible without opening the object")
	assert.Contains(t, message, `spec.transport.protocol to "CANN"`,
		"the backend remediation, in the spelling the schema accepts")
	assert.Contains(t, message, `transport.protocol`,
		"the group field is named too: one group on the right transport is enough")
	assert.NotContains(t, message, `to "ascend"`,
		"the artifact spelling in a remediation sentence is a schema rejection waiting to happen")
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
		// Renderable but NOT nameable: the operator derives it from the pool's accelerator, and
		// ModelDeployment.spec.engine already refuses the same value for the same reason.
		{name: "vllm-ascend", input: "vllm-ascend", valid: false},
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

// TestParseEngine_AscendIsRefusedByName pins the half of the refusal that a bare error cannot carry.
//
// The value is renderable -- the operator derives it -- so refusing it without naming what to use
// instead sends a user looking for a third annotation, which is the dead end this alignment exists
// to close. Asserting only "it errors" would pass on a message that says nothing.
func TestParseEngine_AscendIsRefusedByName(t *testing.T) {
	_, err := ParseEngine(string(EngineVLLMAscend))
	require.Error(t, err)
	assert.Contains(t, err.Error(), string(EngineVLLM),
		"the refusal must name the engine to set instead")
	assert.NotContains(t, err.Error(), "set one of",
		"and it must not fall through to the generic list, which would offer no reason")
}

// TestSelectableEngines_MatchesTheModelDeploymentEnum states the invariant this split exists for:
// what a user may name here is what the other API's enum publishes. The two surfaces described one
// concept with different value sets, and one of them had already ruled the mixed value wrong.
func TestSelectableEngines_MatchesTheModelDeploymentEnum(t *testing.T) {
	assert.Equal(t, []Engine{EngineVLLM, EngineSGLang}, SelectableEngines(),
		"ModelDeployment.spec.engine publishes exactly vllm and sglang")
	assert.Contains(t, Engines(), EngineVLLMAscend,
		"while the renderer keeps it, because the operator still derives and renders it")
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

// TestEngineTransportConstraint_SpellingsAgreeWithTheBackend is the cross-package pin, and it exists
// because the drift it catches is SILENT.
//
// The two spellings in one entry are joined by nothing but the fact that somebody read them off two
// files. If mooncake's mapping is ever edited, the recorded API value stops producing the recorded
// artifact value -- and the only symptom is a refusal message telling an operator to set a field to a
// value that no longer maps, which no test asserting either spelling ALONE can see. One assertion has
// to hold both ends.
//
// It reads the table rather than a hand-picked engine, so a row added without its API spelling
// checked is covered on arrival. The negative row is what gives it teeth: without it, a
// MemberProtocol that returned the required transport for every input would pass.
func TestEngineTransportConstraint_SpellingsAgreeWithTheBackend(t *testing.T) {
	backendWith := func(protocol string) *workercore.KVCacheBackend {
		return &workercore.KVCacheBackend{
			Spec: workercore.KVCacheBackendSpec{
				Transport: workercore.KVCacheBackendTransport{Protocol: protocol},
			},
		}
	}

	var checked int
	for _, engine := range Engines() {
		facts := engineTransportConstraint[engine]
		if facts.Required == "" {
			continue
		}
		checked++

		t.Run(string(engine), func(t *testing.T) {
			assert.Equal(t, facts.Required, mooncake.MemberProtocol(backendWith(facts.RequiredAPIValue)),
				"the API spelling this entry tells an operator to set must render the artifact "+
					"spelling this entry compares against")

			// The schema's own default must NOT satisfy the requirement, or the row describes a
			// constraint that every pool already meets and the refusal could never fire.
			assert.NotEqual(t, facts.Required, mooncake.MemberProtocol(backendWith(mooncake.MemberProtocolAuto)),
				"a requirement the default transport already satisfies would make this table inert")
		})
	}

	require.Positive(t, checked,
		"no engine constrains a transport, so this pinned nothing; the table has lost its one "+
			"constrained row")
}

// testConnectionFor is testConnection with a transport the engine's store backend accepts.
//
// A fixture pairing an engine with a transport it refuses is not a weaker case, it is an IMPOSSIBLE
// configuration: Render declines the pair, so a case built on one measures that refusal instead of
// whatever it meant to measure. Every case that varies the engine takes its connection from here.
//
// The transport is read out of the table rather than written as a literal, which is deliberate and is
// safe for one reason worth stating: no case using this helper asserts anything ABOUT the transport
// rule. They assert vehicles, connector names, roles and tenants, and they need a connection that does
// not itself refuse. The rule is pinned against literals in TestCheckTransport, where deriving it from
// the table would make the check circular.
func testConnectionFor(engine Engine) Connection {
	conn := testConnection()
	if required := engineTransportConstraint[engine].Required; required != "" {
		conn.Protocol = required
	}
	return conn
}

// reasonOf extracts the typed reason, failing the test when the error is not a RefusalError at all --
// an untyped error reaching a caller is itself the defect.
func reasonOf(t *testing.T, err error) Reason {
	t.Helper()

	var refusal *RefusalError
	require.ErrorAs(t, err, &refusal, "refusals must be typed so callers branch on the reason")
	return refusal.Reason
}
