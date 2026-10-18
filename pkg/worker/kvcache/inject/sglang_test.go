package inject

import (
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	utilvalidation "k8s.io/apimachinery/pkg/util/validation"
)

// encoderURLs is a deployment's encoder addresses as the caller resolves them: one per replica
// Service, in the engine's own spelling. Carried as data rather than written inline so the rows
// below that add and remove addresses differ by that and by nothing else.
var encoderURLs = []string{
	"http://qwen-epd-encode:8000",
	"http://qwen-epd-encode-2:8000",
}

// TestRender_SGLangEncoderLeg covers the third stage of an encoder-prefill-decode deployment, one row
// per role, asserting the whole Result rather than the calls that built it.
//
// wantArgs is the complete argument list on purpose. The leg's three flags are the difference
// between a request that reaches an encoder and one that is answered whole, and a renderer that
// dropped one would still start a container and still serve.
func TestRender_SGLangEncoderLeg(t *testing.T) {
	testCases := []struct {
		name              string
		role              Role
		disaggregated     bool
		kvTransfer        bool
		encoderURLs       []string
		connection        Connection
		wantArgs          []string
		wantDefaultedArgs [][]string
		wantPorts         []core.ContainerPort
		wantAnnotations   map[string]string
	}{
		{
			// The encoder holds no cache, so it carries no store configuration either. Everything a
			// prefiller needs is absent here, and its absence is the acceptance.
			name: "the encoder renders one switch and no cache",
			role: RoleEncode,
			wantArgs: []string{
				"--encoder-only",
			},
		},
		{
			name:        "the language-only half carries every address under one flag",
			role:        RolePrefill,
			encoderURLs: encoderURLs,
			connection:  testConnection(),
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--language-only",
				"--encoder-bootstrap-port", "8997",
				// One flag, then the addresses: the engine's parser takes every entry after this
				// one as another address, which is why nothing else follows it.
				"--encoder-urls",
				"http://qwen-epd-encode:8000",
				"http://qwen-epd-encode-2:8000",
			},
			wantDefaultedArgs: [][]string{{"--enable-hierarchical-cache"}},
			wantPorts: []core.ContainerPort{
				{Name: "enc-bootstrap", Protocol: core.ProtocolTCP, ContainerPort: 8997},
			},
		},
		{
			// Both halves at once is the shape the guide's own three-stage example launches.
			name:          "the language-only half keeps its split beside the encoder leg",
			role:          RolePrefill,
			disaggregated: true,
			kvTransfer:    true,
			encoderURLs:   encoderURLs,
			connection:    testConnection(),
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--disaggregation-mode", "prefill",
				"--disaggregation-transfer-backend", "mooncake_tcp",
				"--disaggregation-bootstrap-port", "8998",
				"--language-only",
				"--encoder-bootstrap-port", "8997",
				"--encoder-urls",
				"http://qwen-epd-encode:8000",
				"http://qwen-epd-encode-2:8000",
			},
			wantDefaultedArgs: [][]string{{"--enable-hierarchical-cache"}},
			wantPorts: []core.ContainerPort{
				{Name: "bootstrap", Protocol: core.ProtocolTCP, ContainerPort: 8998},
				{Name: "enc-bootstrap", Protocol: core.ProtocolTCP, ContainerPort: 8997},
			},
			wantAnnotations: map[string]string{"sglang.ai/bootstrap-port": "8998"},
		},
		{
			// The decoder reads the embeddings out of the cache the prefiller built. The guide gives
			// it no encoder flag, and neither does this renderer.
			name:          "the decoder carries no encoder flag",
			role:          RoleDecode,
			disaggregated: true,
			kvTransfer:    true,
			encoderURLs:   encoderURLs,
			connection:    testConnection(),
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--disaggregation-mode", "decode",
				"--disaggregation-transfer-backend", "mooncake_tcp",
			},
			wantDefaultedArgs: [][]string{
				{"--disaggregation-decode-retraction-backup", "cpu_tensor"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{
				Engine: EngineSGLang, Role: tc.role, Disaggregated: tc.disaggregated,
				KVTransfer: tc.kvTransfer, Connection: tc.connection, EncoderURLs: tc.encoderURLs,
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantArgs, result.Args)
			assert.Equal(t, tc.wantDefaultedArgs, result.DefaultedArgs)
			assert.Equal(t, tc.wantPorts, result.Ports)
			assert.Equal(t, tc.wantAnnotations, result.PodAnnotations)
		})
	}
}

// TestRender_SGLangEncoderRendersNothingElse pins the encoder's whole output, because "renders no
// generation-only flag" is only checkable against the complete Result.
func TestRender_SGLangEncoderRendersNothingElse(t *testing.T) {
	result, err := Render(Input{Engine: EngineSGLang, Role: RoleEncode})
	require.NoError(t, err)

	assert.Empty(t, result.Env, "an encoder holds no cache, so it takes no store environment")
	assert.Empty(t, result.DefaultedArgs, "no cache means nothing for the hierarchical cache to hang off")
	assert.Empty(t, result.Ports, "the encoder serves on the port the caller already renders")
	assert.Empty(t, result.PodAnnotations, "no bootstrap registry to publish")
	assert.False(t, result.KVTransfer, "the encoder is in no KV split")
	assert.False(t, result.TenantInjected, "no store, so no reuse domain is written")
}

// TestRender_SGLangEncoderRefusesACache covers the pair the encoder cannot be.
//
// The store client this renderer writes hangs a hierarchical cache off the engine it configures,
// and the encoder builds no language model to cache anything for, so accepting the pair would leave
// a container that starts normally, carries a full Mooncake configuration and moves no blocks.
func TestRender_SGLangEncoderRefusesACache(t *testing.T) {
	testCases := []struct {
		name  string
		input Input
	}{
		{
			name:  "a shared store",
			input: Input{Engine: EngineSGLang, Role: RoleEncode, Connection: testConnection()},
		},
		{
			name:  "a point-to-point transfer",
			input: Input{Engine: EngineSGLang, Role: RoleEncode, KVTransfer: true},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.input)
			require.Error(t, err, "an encoder must not be configured with a cache it cannot use")

			assert.Equal(t, ReasonRoleUnsupported, reasonOf(t, err))
			assert.Contains(t, err.Error(), "encode",
				"the message must name the role it declined")
		})
	}
}

// TestRender_SGLangEncoderRoleSkipsTheStoreFunnel covers the two clauses in Render that refuse an
// input holding no cache of any kind.
//
// The encoder role is exempt from both, because the leg it renders connects to no cache: there is
// no store, no transfer and no event stream on that Pod. The rows that keep other roles refused are
// the point of the test -- an exemption on the role that quietly became an exemption on everything
// would leave every other store-less input rendering nothing and looking configured.
func TestRender_SGLangEncoderRoleSkipsTheStoreFunnel(t *testing.T) {
	testCases := []struct {
		name    string
		input   Input
		wantErr string
	}{
		{
			// Reaches its renderer: with the clause exempt, an encoder with no store renders
			// --encoder-only; without it, the input never leaves the funnel.
			name:  "the encoder role reaches its renderer with no store",
			input: Input{Engine: EngineSGLang, Role: RoleEncode},
		},
		{
			name:    "a role with no store and no other capability is still refused",
			input:   Input{Engine: EngineSGLang, Role: RoleNone},
			wantErr: "no shared store, point-to-point transfer, or KV event publisher was requested",
		},
		{
			name:    "a prefill half with no store and no transfer is still refused",
			input:   Input{Engine: EngineSGLang, Role: RolePrefill},
			wantErr: "no shared store, point-to-point transfer, or KV event publisher was requested",
		},
		{
			name:    "a decode half with no store and no transfer is still refused",
			input:   Input{Engine: EngineSGLang, Role: RoleDecode},
			wantErr: "no shared store, point-to-point transfer, or KV event publisher was requested",
		},
		{
			// The transfer-only leg is this engine's own exemption from the second clause and it
			// predates the encoder: a prefill half may run with no store, and the encoder role must
			// not have narrowed that.
			name:  "a transfer-only prefill half still passes the second clause",
			input: Input{Engine: EngineSGLang, Role: RolePrefill, KVTransfer: true},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Render(tc.input)

			if tc.wantErr == "" {
				require.NoError(t, err)
				return
			}
			require.Error(t, err)
			assert.Contains(t, err.Error(), tc.wantErr)
		})
	}
}

// TestSGLangEncoderConstantsAreTheUpstreamSpellings pins the flag names and the port number byte for
// byte, because a wrong spelling is not an error the engine reports: SGLang's own parser accepts any
// prefix of a registered flag, and the tests elsewhere in this package pin their variables the same
// way for the same reason.
func TestSGLangEncoderConstantsAreTheUpstreamSpellings(t *testing.T) {
	assert.Equal(t, "--encoder-only", sglangEncoderOnlyArg)
	assert.Equal(t, "--language-only", sglangLanguageOnlyArg)
	assert.Equal(t, "--encoder-urls", sglangEncoderURLsArg)
	assert.Equal(t, "--encoder-bootstrap-port", sglangEncoderBootstrapPortArg)
	assert.EqualValues(t, 8997, SGLangEncoderBootstrapPort,
		"upstream defaults encoder_bootstrap_port to 8997")
}

// TestSGLangEncoderPortsAreDistinct covers the port contract, which is the part of this leg another
// component has to agree with.
//
// The three numbers a reader has to keep apart are the HTTP port every role already serves on, the
// prefill's disaggregation bootstrap port and the prefill's encoder bootstrap port. The first is
// rendered by the caller as --port and is not this package's to declare; the two this package does
// declare must not collide with each other, because a Pod has one socket per number.
func TestSGLangEncoderPortsAreDistinct(t *testing.T) {
	assert.NotEqual(t, SGLangBootstrapPort, SGLangEncoderBootstrapPort,
		"the two bootstrap ports are different servers in different processes")

	result, err := Render(Input{
		Engine: EngineSGLang, Role: RolePrefill, Disaggregated: true, KVTransfer: true,
		Connection: testConnection(), EncoderURLs: encoderURLs,
	})
	require.NoError(t, err)

	seen := map[int32]string{}
	for _, port := range result.Ports {
		name, held := seen[port.ContainerPort]
		assert.False(t, held,
			"port %d is already declared as %q; two purposes cannot share one socket",
			port.ContainerPort, name)
		seen[port.ContainerPort] = port.Name

		assert.Empty(t, utilvalidation.IsValidPortName(port.Name),
			"port name %q must be a valid IANA service name, at most fifteen characters", port.Name)
	}
	assert.Contains(t, seen, SGLangEncoderBootstrapPort, "the pair's encoder bootstrap port")
	assert.Contains(t, seen, SGLangBootstrapPort, "the pair's disaggregation bootstrap port")
}

// TestSGLangEncoderOwnedArgsCoverWhatTheLegWrites is the half of the owned-key contract the injector
// can prove alone: every argument the encoder leg ADDS is one a caller must refuse a user value for.
// The caller holds the other half, the refusal itself, and the two are held together by the list this
// test reads, so a flag added to the leg without being owned fails here rather than shipping as a key
// a workload can answer for.
//
// The comparison is against the same render with no addresses declared, so the store and split flags
// the caller already owns are not counted here: the delta is the leg, and nothing else.
func TestSGLangEncoderOwnedArgsCoverWhatTheLegWrites(t *testing.T) {
	owned := SGLangEncoderOwnedArgs()

	// The encoder renders nothing but the leg, so its whole argument list is the delta.
	encoder, err := Render(Input{Engine: EngineSGLang, Role: RoleEncode})
	require.NoError(t, err)
	for _, flag := range flagsIn(encoder.Args) {
		assert.Contains(t, owned, flag,
			"the encoder renders %q and no other flag, so a caller must refuse a user value for it", flag)
	}

	prefill := Input{
		Engine: EngineSGLang, Role: RolePrefill, Disaggregated: true, KVTransfer: true,
		Connection: testConnection(),
	}
	without, err := Render(prefill)
	require.NoError(t, err)
	prefill.EncoderURLs = encoderURLs
	with, err := Render(prefill)
	require.NoError(t, err)

	added := flagsAddedBy(without.Args, with.Args)
	require.NotEmpty(t, added, "the leg rendered nothing, so this test would pass vacuously")
	for _, flag := range added {
		assert.Contains(t, owned, flag,
			"the encoder leg adds %q, so a caller must refuse a user value for it", flag)
	}
}

// flagsIn returns the arguments of a rendered command line, dropping the values that follow them.
// A list flag's values are not flags, and the value of any flag is the caller's to resolve.
func flagsIn(args []string) []string {
	out := []string{}
	for _, arg := range args {
		if strings.HasPrefix(arg, "--") {
			out = append(out, arg)
		}
	}
	return out
}

// flagsAddedBy returns the flags of the second command line that the first does not already carry,
// which is how the encoder leg is told apart from the store and split arguments rendered either way.
func flagsAddedBy(before, after []string) []string {
	already := flagsIn(before)
	out := []string{}
	for _, flag := range flagsIn(after) {
		if !slices.Contains(already, flag) {
			out = append(out, flag)
		}
	}
	return out
}

// TestSGLangEncoderOwnedArgsAreTheFlagsThemselves keeps the list from growing into something that
// looks like a registry: a caller refuses a command-line entry by resolving it to one of these
// names, so an entry that is not a registered flag of this leg would refuse Pods over nothing.
func TestSGLangEncoderOwnedArgsAreTheFlagsThemselves(t *testing.T) {
	want := []string{
		"--encoder-only", "--language-only", "--encoder-urls", "--encoder-bootstrap-port",
	}

	assert.Equal(t, want, SGLangEncoderOwnedArgs())
}

// TestRender_SGLangWithoutAnEncoderIsUnchanged covers the deployments that declared no encoder, by
// rendering each of their roles with no addresses and pinning the complete Result.
//
// The leg is gated on the addresses the caller resolved, so an empty list has to produce exactly the
// bytes it produced before this leg existed. Asserting the whole Result rather than the presence of
// a flag is what catches a render that appended something new to every prefill half.
func TestRender_SGLangWithoutAnEncoderIsUnchanged(t *testing.T) {
	testCases := []struct {
		name              string
		role              Role
		disaggregated     bool
		kvTransfer        bool
		connection        Connection
		wantArgs          []string
		wantDefaultedArgs [][]string
		wantPorts         []core.ContainerPort
		wantAnnotations   map[string]string
	}{
		{
			name:       "a shared store with no split",
			role:       RoleNone,
			connection: testConnection(),
			wantArgs:   []string{"--hicache-storage-backend", "mooncake"},
			wantDefaultedArgs: [][]string{
				{"--enable-hierarchical-cache"},
			},
		},
		{
			name:          "a managed prefill half",
			role:          RolePrefill,
			disaggregated: true,
			kvTransfer:    true,
			connection:    testConnection(),
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--disaggregation-mode", "prefill",
				"--disaggregation-transfer-backend", "mooncake_tcp",
				"--disaggregation-bootstrap-port", "8998",
			},
			wantDefaultedArgs: [][]string{{"--enable-hierarchical-cache"}},
			wantPorts: []core.ContainerPort{
				{Name: "bootstrap", Protocol: core.ProtocolTCP, ContainerPort: 8998},
			},
			wantAnnotations: map[string]string{"sglang.ai/bootstrap-port": "8998"},
		},
		{
			name:          "a managed decode half",
			role:          RoleDecode,
			disaggregated: true,
			kvTransfer:    true,
			connection:    testConnection(),
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
				"--disaggregation-mode", "decode",
				"--disaggregation-transfer-backend", "mooncake_tcp",
			},
			wantDefaultedArgs: [][]string{
				{"--disaggregation-decode-retraction-backup", "cpu_tensor"},
			},
		},
		{
			name:       "a lone prefill half runs undivided",
			role:       RolePrefill,
			connection: testConnection(),
			wantArgs:   []string{"--hicache-storage-backend", "mooncake"},
			wantDefaultedArgs: [][]string{
				{"--enable-hierarchical-cache"},
			},
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			result, err := Render(Input{
				Engine: EngineSGLang, Role: tc.role, Disaggregated: tc.disaggregated,
				KVTransfer: tc.kvTransfer, Connection: tc.connection,
			})
			require.NoError(t, err)

			assert.Equal(t, tc.wantArgs, result.Args)
			assert.Equal(t, tc.wantDefaultedArgs, result.DefaultedArgs)
			assert.Equal(t, tc.wantPorts, result.Ports)
			assert.Equal(t, tc.wantAnnotations, result.PodAnnotations)

			for _, arg := range result.Args {
				assert.NotContains(t, SGLangEncoderOwnedArgs(), arg,
					"a deployment that declared no encoder must render no encoder flag")
			}
		})
	}
}

// TestRender_SGLangEncoderBootstrapPortIsRenderedOnTheContainer keeps the port the process listens on
// and the number written into the command line from drifting apart, which is the failure the
// disaggregation bootstrap port already documents on the other leg.
func TestRender_SGLangEncoderBootstrapPortIsRenderedOnTheContainer(t *testing.T) {
	result, err := Render(Input{
		Engine: EngineSGLang, Role: RolePrefill, Connection: testConnection(),
		EncoderURLs: encoderURLs,
	})
	require.NoError(t, err)

	want := strconv.Itoa(int(SGLangEncoderBootstrapPort))
	flagAt := indexOfArg(result.Args, sglangEncoderBootstrapPortArg)
	require.NotEqual(t, -1, flagAt, "the language-only half must name the bootstrap port it serves")
	assert.Equal(t, want, result.Args[flagAt+1],
		"the rendered value and the declared port must be one number")
}

// indexOfArg returns where an argument first appears, or -1. A test helper rather than a loop in each
// test, because several rows assert on a flag and its value as a pair.
func indexOfArg(args []string, flag string) int {
	for i, arg := range args {
		if arg == flag {
			return i
		}
	}
	return -1
}
