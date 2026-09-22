package router

import (
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func vllmInput(roles ...Role) Input {
	return Input{
		Roles:          roles,
		Namespace:      "team-a",
		EndpointLabels: map[string]string{"app.kubernetes.io/instance": "qwen", "member-index": "0"},
		ListenPort:     8081,
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

	for _, arg := range []string{"--port=8081", "--prometheus-port=9090", "--service-discovery-port=8000"} {
		assert.Contains(t, output.Arguments, arg)
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
