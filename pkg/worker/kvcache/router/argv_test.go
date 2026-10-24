package router

import (
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func vllmInput(roles ...Role) Input {
	return Input{
		Roles:          roles,
		Namespace:      "team-a",
		EndpointLabels: map[string]string{"app.kubernetes.io/instance": "qwen", "member-index": "0"},
		ListenPort:     8000,
		MetricsPort:    9090,
		DiscoveryPort:  8000,
	}
}

func serverRole() Role {
	return Role{Kind: "server", RoleLabelKey: "role-kind", Selector: map[string]string{
		"app.kubernetes.io/instance": "qwen", "component": "server", "member-index": "0",
	}}
}

func prefillRole() Role {
	return Role{Kind: "prefill", RoleLabelKey: "role-kind", Selector: map[string]string{
		"app.kubernetes.io/instance": "qwen", "component": "prefill", "member-index": "0",
	}}
}

func decodeRole() Role {
	return Role{Kind: "decode", RoleLabelKey: "role-kind", Selector: map[string]string{
		"app.kubernetes.io/instance": "qwen", "component": "decode", "member-index": "0",
	}}
}

// encodeRole is the encoder, and it is the one role whose selector names Pods that never answer.
// Its entries are deliberately the kind this file can tell apart at a glance, because every case
// below is decided on whether "component=encode" reaches a selector.
func encodeRole() Role {
	return Role{Kind: "encode", RoleLabelKey: "role-kind", Selector: map[string]string{
		"app.kubernetes.io/instance": "qwen", "component": "encode", "member-index": "0",
	}}
}

// TestRenderVLLM_StatesTheDefaultsThatFailSilently is the case this renderer mostly exists for.
//
// Each of these four flags overrides an upstream default, and each failure is silent in a different
// place: the two bind addresses leave the process reachable only inside its own network namespace,
// so a Service in front of it resolves to nothing; the connector leaves the router routing
// correctly and transferring nothing; and an absent discovery namespace makes it watch the whole
// cluster, which the namespaced Role this operator renders does not permit, so it lists nothing.
//
// THE VALUES ARE WRITTEN OUT rather than built from the package's own constants, because asserting
// against the constant passes whatever the constant holds -- and what is being pinned here is the
// agreement with a program this repository does not build.
func TestRenderVLLM_StatesTheDefaultsThatFailSilently(t *testing.T) {
	output, err := Render(VLLM, vllmInput(serverRole()))
	require.NoError(t, err)

	for _, arg := range []string{
		"--host=0.0.0.0",
		"--prometheus-host=0.0.0.0",
		"--kv-connector=mooncake",
		"--service-discovery-namespace=team-a",
	} {
		assert.Contains(t, output.Arguments, arg)
	}
	assert.Empty(t, output.Document, "this router reads no configuration file")
}

func TestRenderVLLM_NamesItsOwnPortsAndTheOneItDials(t *testing.T) {
	output, err := Render(VLLM, vllmInput(serverRole()))
	require.NoError(t, err)

	for _, arg := range []string{"--port=8000", "--prometheus-port=9090", "--service-discovery-port=8000"} {
		assert.Contains(t, output.Arguments, arg)
	}
}

// TestRenderSGLang_RendersDPAwareOnlyForTheExternalDPShape pins the flag's shape gate.
//
// --dp-aware is what makes the gateway register one worker per dp member instead of one per
// group, and the shape that needs it is the External DP shape: one server role at an attention
// width of two or more. A width under two has no subordinate member to register, the vLLM router has no
// such flag at all, and the disaggregated shape is P/D, whose halves are named by their own
// selectors -- so each of the other three cases renders without it. D15.
func TestRenderSGLang_RendersDPAwareOnlyForTheExternalDPShape(t *testing.T) {
	testCases := []struct {
		name                      string
		router                    string
		attentionDataParallelSize int32
		roles                     []Role
		wantDPAware               bool
	}{
		{
			name:                      "the external dp shape at width two carries the flag",
			router:                    SGLang,
			attentionDataParallelSize: 2,
			roles:                     []Role{serverRole()},
			wantDPAware:               true,
		},
		{
			name:                      "a width of one has no subordinate member to register",
			router:                    SGLang,
			attentionDataParallelSize: 1,
			roles:                     []Role{serverRole()},
			wantDPAware:               false,
		},
		{
			name:                      "the vllm router has no such flag",
			router:                    VLLM,
			attentionDataParallelSize: 2,
			roles:                     []Role{serverRole()},
			wantDPAware:               false,
		},
		{
			name:                      "the disaggregated shape names its halves by their own selectors",
			router:                    SGLang,
			attentionDataParallelSize: 2,
			roles:                     []Role{prefillRole(), decodeRole()},
			wantDPAware:               false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			input := Input{
				Roles:                     tc.roles,
				Namespace:                 "team-a",
				EndpointLabels:            map[string]string{"app.kubernetes.io/instance": "qwen"},
				ListenPort:                8000,
				MetricsPort:               9090,
				DiscoveryPort:             8000,
				AttentionDataParallelSize: tc.attentionDataParallelSize,
			}
			output, err := Render(tc.router, input)
			require.NoError(t, err)

			if tc.wantDPAware {
				assert.Contains(t, output.Arguments, "--dp-aware",
					"the external dp shape registers one worker per member")
			} else {
				assert.NotContains(t, output.Arguments, "--dp-aware",
					"no other shape carries the flag")
			}
		})
	}
}

// TestRenderVLLM_SelectorIsOneArgumentPerLabel pins the shape the flag actually takes.
//
// A comma-joined expression would be accepted and then read as ONE entry whose key carries the rest
// of the string, because the parser splits on the first "=" and keeps the remainder as the value.
// Nothing would report it: the router would simply match no Pod. The entries are also sorted, so
// two renders of one input produce one command line rather than a rolling Deployment.
func TestRenderVLLM_SelectorIsOneArgumentPerLabel(t *testing.T) {
	output, err := Render(VLLM, vllmInput(serverRole()))
	require.NoError(t, err)

	at := indexOf(t, output.Arguments, "--selector")
	assert.Equal(t, []string{"app.kubernetes.io/instance=qwen", "member-index=0"},
		output.Arguments[at+1:], "one entry per label, sorted, and nothing after them")
	for _, arg := range output.Arguments {
		assert.NotContains(t, arg, ",", "a comma-joined selector is read as one malformed entry")
	}
}

// TestRenderVLLM_DisaggregationRendersBothHalvesAndNoDeploymentSelector pins the split shape.
//
// The two per-role selectors are a UNION -- a Pod is taken when it matches either -- while the
// deployment-wide one is a single set. Rendering both would put the deployment selector and the
// role selectors in front of the same router, which upstream reads in different modes, so the
// undivided one is what must NOT appear here.
func TestRenderVLLM_DisaggregationRendersBothHalvesAndNoDeploymentSelector(t *testing.T) {
	output, err := Render(VLLM, vllmInput(prefillRole(), decodeRole()))
	require.NoError(t, err)

	assert.Contains(t, output.Arguments, "--vllm-pd-disaggregation")
	assert.NotContains(t, output.Arguments, "--selector",
		"the deployment-wide selector is the undivided shape's, and the two are read in different modes")

	prefillAt := indexOf(t, output.Arguments, "--prefill-selector")
	decodeAt := indexOf(t, output.Arguments, "--decode-selector")
	require.Less(t, prefillAt, decodeAt)
	assert.Equal(t,
		[]string{"app.kubernetes.io/instance=qwen", "component=prefill", "member-index=0"},
		output.Arguments[prefillAt+1:decodeAt])
	assert.Equal(t,
		[]string{"app.kubernetes.io/instance=qwen", "component=decode", "member-index=0"},
		output.Arguments[decodeAt+1:])
}

// TestRenderVLLM_UndividedShapeRendersNoDisaggregation is the mirror, and it is what keeps the
// branch above from being taken whenever any role exists: a deployment of servers alone has no half
// to pair, and the mode upstream enters would then demand selectors it was never given.
func TestRenderVLLM_UndividedShapeRendersNoDisaggregation(t *testing.T) {
	output, err := Render(VLLM, vllmInput(serverRole()))
	require.NoError(t, err)

	assert.NotContains(t, output.Arguments, "--vllm-pd-disaggregation")
	assert.NotContains(t, output.Arguments, "--prefill-selector")
	assert.NotContains(t, output.Arguments, "--decode-selector")
	assert.Contains(t, output.Arguments, "--selector")
}

// TestRenderVLLM_OneHalfIsNotASplit is the case the row above cannot see.
//
// A deployment declaring a prefiller and no decoder has no pair to route between, and both halves
// are what upstream demands once the mode is on: entering it with one selector missing is a router
// told to disaggregate against a set that can only ever serve one side. A renderer testing whether
// EITHER half exists passes every other assertion here, because every other input has both or
// neither -- which is why this row names one.
func TestRenderVLLM_OneHalfIsNotASplit(t *testing.T) {
	for _, only := range []Role{prefillRole(), decodeRole()} {
		t.Run(only.Kind, func(t *testing.T) {
			output, err := Render(VLLM, vllmInput(only))
			require.NoError(t, err)

			assert.NotContains(t, output.Arguments, "--vllm-pd-disaggregation",
				"one half is not a split")
			assert.NotContains(t, output.Arguments, "--prefill-selector")
			assert.NotContains(t, output.Arguments, "--decode-selector")
			assert.Contains(t, output.Arguments, "--selector",
				"it is routed as the undivided shape, naming every Pod that answers")
		})
	}
}

// TestRenderVLLM_RefusesAnInputItCannotDiscoverBy covers the three values whose absence would render
// a router that starts, reports healthy, and reaches nothing.
func TestRenderVLLM_RefusesAnInputItCannotDiscoverBy(t *testing.T) {
	cases := []struct {
		name  string
		input func() Input
		want  string
	}{
		{
			name: "no namespace",
			input: func() Input {
				in := vllmInput(serverRole())
				in.Namespace = ""

				return in
			},
			want: "render vllm-router: no discovery namespace",
		},
		{
			name: "no discovery port",
			input: func() Input {
				in := vllmInput(serverRole())
				in.DiscoveryPort = 0

				return in
			},
			want: "render vllm-router: no discovery port",
		},
		{
			name: "no endpoint labels",
			input: func() Input {
				in := vllmInput(serverRole())
				in.EndpointLabels = nil

				return in
			},
			want: "render vllm-router: no endpoint labels to discover by",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(VLLM, c.input())
			require.EqualError(t, err, c.want)
		})
	}
}

func indexOf(t *testing.T, args []string, want string) int {
	t.Helper()

	for i, arg := range args {
		if arg == want {
			return i
		}
	}
	require.Failf(t, "argument not rendered", "%q is not among %v", want, args)

	return -1
}

// TestRenderArgv_TheGatewayIsTheVLLMRouterMinusItsOwnTwoFlags states this renderer's whole reason
// for being shared, as a difference rather than as a second expected command line.
//
// A case listing the gateway's flags in full would pass just as well if the two renderers had
// drifted into two unrelated surfaces -- it asserts what the gateway renders, never that it renders
// the same thing. Comparing the two outputs is what makes the sharing the thing under test: only
// the disaggregation switch's spelling and the vLLM router's own connector may differ, and anything
// else appearing on one side alone fails here.
func TestRenderArgv_TheGatewayIsTheVLLMRouterMinusItsOwnTwoFlags(t *testing.T) {
	in := vllmInput(prefillRole(), decodeRole())

	vllm, err := Render(VLLM, in)
	require.NoError(t, err)
	gateway, err := Render(SGLang, in)
	require.NoError(t, err)

	assert.Equal(t, []string{"--kv-connector=mooncake", "--vllm-pd-disaggregation"},
		missingFrom(vllm.Arguments, gateway.Arguments),
		"the vLLM router names a transfer connector the gateway has no flag for, and spells the "+
			"disaggregation switch with its engine's name")
	assert.Equal(t, []string{"--pd-disaggregation"},
		missingFrom(gateway.Arguments, vllm.Arguments),
		"the gateway differs by that spelling and by nothing else")
}

// TestRenderArgv_TheGatewayNamesNoModel pins the omission, which is a decision rather than a gap.
//
// Turning on service discovery turns on the gateway's inference-gateway mode by itself, and that
// mode routes by the id each worker reports for its own. The flag that would carry a name here
// loads a TOKENIZER instead, so rendering it would make the router Pod depend on reaching a model
// repository at startup for a name it does not route by.
func TestRenderArgv_TheGatewayNamesNoModel(t *testing.T) {
	in := vllmInput(serverRole())
	in.ModelName = "Qwen/Qwen2.5-72B-Instruct"

	gateway, err := Render(SGLang, in)
	require.NoError(t, err)

	for _, arg := range gateway.Arguments {
		assert.NotContains(t, arg, "--model-path")
		assert.NotContains(t, arg, "--tokenizer-path")
		assert.NotContains(t, arg, in.ModelName)
	}
}

// missingFrom returns the entries of left that right does not carry, sorted.
func missingFrom(left, right []string) []string {
	held := make(map[string]struct{}, len(right))
	for _, arg := range right {
		held[arg] = struct{}{}
	}

	var out []string
	for _, arg := range left {
		if _, ok := held[arg]; !ok {
			out = append(out, arg)
		}
	}
	slices.Sort(out)

	return out
}

// prefillEntries and decodeEntries are the two halves a split deployment hands the router, written
// out so a case compares against the agreement with the upstream parser rather than with whatever
// the renderer happens to produce.
var (
	prefillEntries = []string{"app.kubernetes.io/instance=qwen", "component=prefill", "member-index=0"}
	decodeEntries  = []string{"app.kubernetes.io/instance=qwen", "component=decode", "member-index=0"}
)

// TestRenderArgv_WithoutAnEncoderTheArgvIsTheGoldenOne is the freeze the whole change rests on.
//
// A deployment with no Encode role must render the command line it rendered before this renderer knew
// the encoder existed, byte for byte. It is written as one whole command line per router rather than
// as a set of flag checks, because the flags that would be ADDED for an encoder are exactly the kind
// that pass a membership assertion when they are malformed -- one appended with a different spelling,
// or a value split across two arguments, contains nothing this file already looks for.
func TestRenderArgv_WithoutAnEncoderTheArgvIsTheGoldenOne(t *testing.T) {
	cases := []struct {
		name    string
		router  string
		command []string
		args    []string
	}{
		{
			name:    "vllm-router",
			router:  VLLM,
			command: []string{"vllm-router"},
			args: []string{
				"--host=0.0.0.0",
				"--port=8000",
				"--prometheus-host=0.0.0.0",
				"--prometheus-port=9090",
				"--kv-connector=mooncake",
				"--service-discovery",
				"--service-discovery-namespace=team-a",
				"--service-discovery-port=8000",
				"--vllm-pd-disaggregation",
				"--prefill-selector",
				"app.kubernetes.io/instance=qwen",
				"component=prefill",
				"member-index=0",
				"--decode-selector",
				"app.kubernetes.io/instance=qwen",
				"component=decode",
				"member-index=0",
			},
		},
		{
			name:    "sglang-gateway",
			router:  SGLang,
			command: []string{"sgl-model-gateway"},
			args: []string{
				"--host=0.0.0.0",
				"--port=8000",
				"--prometheus-host=0.0.0.0",
				"--prometheus-port=9090",
				"--service-discovery",
				"--service-discovery-namespace=team-a",
				"--service-discovery-port=8000",
				"--pd-disaggregation",
				"--prefill-selector",
				"app.kubernetes.io/instance=qwen",
				"component=prefill",
				"member-index=0",
				"--decode-selector",
				"app.kubernetes.io/instance=qwen",
				"component=decode",
				"member-index=0",
			},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			output, err := Render(c.router, vllmInput(prefillRole(), decodeRole()))
			require.NoError(t, err)

			assert.Equal(t, c.command, output.Command)
			assert.Equal(t, c.args, output.Arguments)
		})
	}
}

// TestRenderArgv_AnEncoderReachesNoSelector covers the shape both routers are given, in every
// position the role can be declared in.
//
// AN ENCODER ANSWERS NOTHING. It consumes media and returns embeddings that only a prefiller reads,
// so a router told an encoder is a backend routes requests to a process that cannot generate from
// them. The position matters because the two per-role selectors are a UNION rather than a
// conjunction, and because a deployment-wide selector would take whichever role came first -- so an
// implementation that picked the first role rather than the role of each kind would pass every
// single-role case in this file.
//
// THE INPUT CARRIES THE ENCODER'S ENDPOINT LIST in every row, because the vLLM router refuses a
// declared encoder without one. The gateway takes the same list and renders nothing from it.
func TestRenderArgv_AnEncoderReachesNoSelector(t *testing.T) {
	orders := []struct {
		name  string
		roles []Role
	}{
		{name: "encoder first", roles: []Role{encodeRole(), prefillRole(), decodeRole()}},
		{name: "encoder middle", roles: []Role{prefillRole(), encodeRole(), decodeRole()}},
		{name: "encoder last", roles: []Role{prefillRole(), decodeRole(), encodeRole()}},
	}
	for _, router := range []string{VLLM, SGLang} {
		for _, order := range orders {
			t.Run(router+"/"+order.name, func(t *testing.T) {
				in := vllmInput(order.roles...)
				in.EncoderURLs = []string{"http://qwen-encode.team-a.svc:8000"}
				output, err := Render(router, in)
				require.NoError(t, err)

				entries := selectorEntries(output.Arguments)
				for flag, got := range entries {
					assert.NotContains(t, got, "component=encode",
						"%s would make the router treat the encoder as an endpoint", flag)
				}

				assert.NotContains(t, entries, "--selector",
					"the deployment-wide selector is the undivided shape's, and it names every Pod")
				assert.Equal(t, prefillEntries, entries["--prefill-selector"])
				assert.Equal(t, decodeEntries, entries["--decode-selector"])
			})
		}
	}
}

// TestRenderArgv_TwoEncoderReplicasReachNoArgument is the replica count against the selector.
//
// Replicas of a role share that role's labels, so a scale from one encoder to two changes nothing
// this renderer is handed and must change nothing it emits. The case states that in both directions:
// the encoder's own selector is rendered nowhere, AND a term naming the second replica reaches no
// argument either -- so a deployment-wide selector that widened to cover the replica set would be
// caught here rather than at run time, where it would offer the router an encoder as a backend.
func TestRenderArgv_TwoEncoderReplicasReachNoArgument(t *testing.T) {
	in := vllmInput(prefillRole(), decodeRole(), encodeRole())
	in.EncoderURLs = []string{"http://qwen-encode.team-a.svc:8000"}
	in.EndpointLabels = map[string]string{
		"app.kubernetes.io/instance": "qwen", "member-index": "1",
	}

	output, err := Render(VLLM, in)
	require.NoError(t, err)

	for flag, got := range selectorEntries(output.Arguments) {
		assert.NotContains(t, got, "component=encode", "%s would make the encoder a backend", flag)
		assert.NotContains(t, got, "member-index=1", "%s would follow one replica the others omit", flag)
	}
}

// TestRenderArgv_AnEncoderWithACustomPortIsStillInNoSelector holds the port out of the decision.
//
// The port a role serves on is not carried on its role: one Input states one discovery port, and the
// controller refuses a deployment whose roles disagree on it. So a deployment that moves every role
// to another port changes one argument and no selector -- which is what makes this the row that would
// fail if the encoder were being matched by something other than its role kind.
func TestRenderArgv_AnEncoderWithACustomPortIsStillInNoSelector(t *testing.T) {
	in := vllmInput(encodeRole(), prefillRole(), decodeRole())
	in.DiscoveryPort = 9123

	output, err := Render(SGLang, in)
	require.NoError(t, err)

	assert.Contains(t, output.Arguments, "--service-discovery-port=9123")
	for flag, got := range selectorEntries(output.Arguments) {
		assert.NotContains(t, got, "component=encode", "%s would make the encoder a backend", flag)
	}
}

// TestRenderArgv_AnUnpairedEncoderIsRefused covers the one shape where a selector has no kind to
// match on.
//
// The split shape names each half by its own role, so an encoder beside a prefiller and a decoder
// is simply absent from both. The undivided shape has one selector naming every Pod of the
// deployment, and in it the encoder cannot be left out -- it is in the set. Rendering it anyway would
// be a router offered an encoder as a backend, so the renderer refuses instead: the pair an encoder
// needs is checked where it can be checked, and a caller that means the undivided shape has said by
// accident that it holds an encoder.
func TestRenderArgv_AnUnpairedEncoderIsRefused(t *testing.T) {
	cases := []struct {
		name    string
		router  string
		roles   []Role
		wantErr string
	}{
		{
			name:   "encoder beside a server",
			router: VLLM,
			roles:  []Role{encodeRole(), serverRole()},
			wantErr: "render vllm-router: a deployment-wide selector would offer the encode role as a backend, " +
				"so a deployment holding one needs a prefill and a decode role beside it",
		},
		{
			name:   "encoder beside a server on the gateway",
			router: SGLang,
			roles:  []Role{encodeRole(), serverRole()},
			wantErr: "render sglang-gateway: a deployment-wide selector would offer the encode role as a backend, " +
				"so a deployment holding one needs a prefill and a decode role beside it",
		},
		{
			name:   "encoder with one half only",
			router: VLLM,
			roles:  []Role{encodeRole(), prefillRole()},
			wantErr: "render vllm-router: a deployment-wide selector would offer the encode role as a backend, " +
				"so a deployment holding one needs a prefill and a decode role beside it",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, err := Render(c.router, vllmInput(c.roles...))
			require.EqualError(t, err, c.wantErr)
		})
	}
}

// TestRenderArgv_APairedEncoderStillRendersTheUndividedShape is the control for the row above: the
// refusal is about the encoder, not about every deployment holding three roles.
func TestRenderArgv_APairedEncoderStillRendersTheUndividedShape(t *testing.T) {
	output, err := Render(VLLM, vllmInput(prefillRole(), decodeRole()))
	require.NoError(t, err)

	assert.NotContains(t, output.Arguments, "--selector")
	assert.Contains(t, output.Arguments, "--prefill-selector")
}

// TestRenderVLLM_TheEncoderStageIsCarriedAsOneStaticList pins the one flag an encoder adds.
//
// THE DOCUMENT IS WRITTEN OUT IN FULL rather than built from the package's constants, for the same
// reason the golden command line above is: what is being pinned is the agreement with a program
// this repository does not build, whose parser refuses unknown keys outright. Placement is part of
// the pin -- the document rides ahead of the two selector lists, which read every token up to the
// next one starting with a dash -- and so are the two absences: the document names no prefiller it
// cannot know, and the command line turns on no gateway mode upstream refuses beside the encoder
// stage.
func TestRenderVLLM_TheEncoderStageIsCarriedAsOneStaticList(t *testing.T) {
	cases := []struct {
		name  string
		roles []Role
		urls  []string
		want  string
	}{
		{
			name:  "one encoder service",
			roles: []Role{encodeRole(), prefillRole(), decodeRole()},
			urls:  []string{"http://qwen-encode.team-a.svc:8000"},
			want:  `--epd-config={"encoder_urls":["http://qwen-encode.team-a.svc:8000"]}`,
		},
		{
			name:  "several encoder services",
			roles: []Role{encodeRole(), prefillRole(), decodeRole()},
			urls: []string{
				"http://qwen-encode.team-a.svc:8000", "https://qwen-encode.team-a.svc:9100",
			},
			want: `--epd-config={"encoder_urls":["http://qwen-encode.team-a.svc:8000",` +
				`"https://qwen-encode.team-a.svc:9100"]}`,
		},
		{
			name:  "a declared encoder with an empty selector still carries its list",
			roles: []Role{{Kind: "encode", RoleLabelKey: "role-kind"}, prefillRole(), decodeRole()},
			urls:  []string{"http://qwen-encode.team-a.svc:8000"},
			want:  `--epd-config={"encoder_urls":["http://qwen-encode.team-a.svc:8000"]}`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := vllmInput(c.roles...)
			in.EncoderURLs = c.urls
			output, err := Render(VLLM, in)
			require.NoError(t, err)

			epdAt := indexOf(t, output.Arguments, c.want)
			assert.Greater(t, epdAt, indexOf(t, output.Arguments, "--vllm-pd-disaggregation"),
				"the document rides behind the disaggregation switch")
			assert.Less(t, epdAt, indexOf(t, output.Arguments, "--prefill-selector"),
				"the document is pinned ahead of the list flags that follow it")
			for _, arg := range output.Arguments {
				assert.NotContains(t, arg, "consumer_zmq_addrs",
					"selector discovery cannot name prefillers, so the document must claim none")
				assert.NotContains(t, arg, "--enable-igw",
					"upstream refuses the encoder stage beside inference-gateway mode")
			}
		})
	}
}

// TestRenderVLLM_TheEncoderStageIsAllOrNothing refuses the halves apart.
//
// A DECLARED ENCODER WITHOUT ENDPOINTS would start a router that refuses itself at startup --
// upstream validates an empty encoder_urls and exits -- so the renderer refuses first, where the
// message names the deployment. ENDPOINTS WITHOUT A DECLARED ROLE would be dropped in silence if
// ignored, and silence is the one answer this package never gives. An endpoint without its scheme
// would survive this render and fail the router's own URL check instead.
func TestRenderVLLM_TheEncoderStageIsAllOrNothing(t *testing.T) {
	cases := []struct {
		name    string
		roles   []Role
		urls    []string
		wantErr string
	}{
		{
			name:    "a declared encoder without endpoints",
			roles:   []Role{encodeRole(), prefillRole(), decodeRole()},
			wantErr: "render vllm-router: the encode role and its encoder endpoints must be declared together",
		},
		{
			name:    "endpoints without a declared encoder",
			roles:   []Role{prefillRole(), decodeRole()},
			urls:    []string{"http://qwen-encode.team-a.svc:8000"},
			wantErr: "render vllm-router: the encode role and its encoder endpoints must be declared together",
		},
		{
			name:    "an endpoint without a scheme",
			roles:   []Role{encodeRole(), prefillRole(), decodeRole()},
			urls:    []string{"qwen-encode.team-a.svc:8000"},
			wantErr: `render vllm-router: encoder endpoint "qwen-encode.team-a.svc:8000" is not an http(s) URL`,
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := vllmInput(c.roles...)
			in.EncoderURLs = c.urls
			_, err := Render(VLLM, in)
			require.EqualError(t, err, c.wantErr)
		})
	}
}

// TestRenderArgv_TheGatewayTakesNoEncoderStage states the asymmetry between the two routers.
//
// The gateway's prefiller dials the encoder itself, by --encoder-urls on the engine's own command
// line, so the gateway process never learns the endpoints. The strongest statement of that is
// equality: its command line with an encoder declared and its endpoints handed over is the command
// line it renders without either.
func TestRenderArgv_TheGatewayTakesNoEncoderStage(t *testing.T) {
	withEncoder := vllmInput(encodeRole(), prefillRole(), decodeRole())
	withEncoder.EncoderURLs = []string{"http://qwen-encode.team-a.svc:8000"}

	gatewayWith, err := Render(SGLang, withEncoder)
	require.NoError(t, err)
	gatewayWithout, err := Render(SGLang, vllmInput(prefillRole(), decodeRole()))
	require.NoError(t, err)

	assert.Equal(t, gatewayWithout.Arguments, gatewayWith.Arguments,
		"the gateway's command line is decided by the halves it routes, never by the encoder stage")
	for _, arg := range gatewayWith.Arguments {
		assert.NotContains(t, arg, "epd-config")
	}
}

// TestRenderArgv_NoEncoderRoleNoEncoderStage is the omission on the shape every existing
// deployment has.
//
// The golden freeze pins both routers' complete command lines without an encoder, which is the
// byte-identical guarantee; this states the same thing as a membership the new flag must keep
// failing on its own, so a future edit that renders the stage unconditionally cannot hide behind
// a frozen list nobody re-reads.
func TestRenderArgv_NoEncoderRoleNoEncoderStage(t *testing.T) {
	for _, name := range []string{VLLM, SGLang} {
		t.Run(name, func(t *testing.T) {
			output, err := Render(name, vllmInput(prefillRole(), decodeRole()))
			require.NoError(t, err)

			for _, arg := range output.Arguments {
				assert.NotContains(t, arg, "epd-config",
					"no encoder is declared, so there is nothing to configure")
			}
		})
	}
}

// TestRenderArgv_AnEncoderWithAnEmptySelectorIsStillDeclared closes the edge declaresRole
// documents: a role declared with an empty selector is present all the same.
//
// The undivided shape refuses an encoder whose selector names Pods; this is the same refusal for
// one whose selector names nothing, which is what separates declared-but-unselected -- still an
// encoder in the answering set -- from not declared at all.
func TestRenderArgv_AnEncoderWithAnEmptySelectorIsStillDeclared(t *testing.T) {
	declared := Role{Kind: "encode", RoleLabelKey: "role-kind"}

	_, err := Render(VLLM, vllmInput(declared, serverRole()))
	require.EqualError(t, err,
		"render vllm-router: a deployment-wide selector would offer the encode role as a backend, "+
			"so a deployment holding one needs a prefill and a decode role beside it")
}

// selectorEntries returns the label entries every selector flag on a rendered command line carries,
// keyed by that flag.
//
// A selector flag is a LIST, and a list consumes every following value until the next one starting
// with a dash, which is why the selectors are rendered last. Reading the line that way is the same
// way the upstream parser reads it, so a flag rendered in the wrong place is reported here rather
// than swallowed into the selector above it.
func selectorEntries(args []string) map[string][]string {
	entries := map[string][]string{}
	var current string
	for _, arg := range args {
		switch {
		case strings.HasPrefix(arg, "-"):
			current = ""
			for _, flag := range []string{"--selector", "--prefill-selector", "--decode-selector"} {
				if arg == flag {
					current = flag
					entries[flag] = nil
				}
			}
		case current != "":
			entries[current] = append(entries[current], arg)
		}
	}

	return entries
}
