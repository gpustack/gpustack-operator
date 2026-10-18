package worker

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// The table is shared policy: admission refuses a combination it lacks and the render reads the
// same row to learn which connector to configure. These cases pin the table's own shape, so a row
// cannot be added or edited without something here saying so -- and the refusal message a user
// reads is built from the same rows.
// TestModelDeploymentEPDEncoderLeg_AnAddressLegConfiguresNoCache reads the leg the render hands each
// role, which is where the SGLang rows and the vLLM rows part company.
//
// THE SGLANG ROWS ARE ADMITTED, so the leg is resolved on them rather than skipped, and the answer has
// to be the zero value: SGLang takes the encoder's addresses through EncoderURLs and resolves no
// encoder-cache connector, so a cache request here would name a connector the engine cannot resolve.
//
// THE VLLM ROW BESIDE THEM IS THE CONTRAST. An assertion of emptiness alone cannot tell a correct zero
// from a leg that never ran on any row, so the same shape on an admitted vLLM row is asserted to
// resolve to a cache. Without it this case would still pass on a build that returned nothing at all.
func TestModelDeploymentEPDEncoderLeg_AnAddressLegConfiguresNoCache(t *testing.T) {
	legKinds := []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindEncode,
		workercore.ModelDeploymentRoleKindPrefill,
	}

	for _, router := range []string{
		workercore.ModelDeploymentRouterLLMD,
		workercore.ModelDeploymentRouterSGLang,
	} {
		for _, platform := range []string{
			nodefeature.ManufacturerNVIDIA,
			nodefeature.ManufacturerAMD,
			nodefeature.ManufacturerAscend,
		} {
			md := epdDeployment(t, legKinds)
			md.Spec.Router = &workercore.ModelDeploymentRouter{Name: router}
			md.Spec.Engine = workercore.ModelDeploymentEngine{
				Name:  workercore.ModelDeploymentEngineSGLang,
				Image: "lmsysorg/sglang:v0.5.18",
			}

			for i := range md.Spec.Roles {
				assert.Equal(t, inject.EncoderCache{},
					modelDeploymentEPDEncoderLeg(md, &md.Spec.Roles[i], platform),
					"%s+SGLang on %s configures no encoder cache on role %q",
					router, platform, md.Spec.Roles[i].Name)
			}
		}
	}

	vllm := epdDeployment(t, legKinds)
	vllm.Spec.Router = &workercore.ModelDeploymentRouter{Name: workercore.ModelDeploymentRouterLLMD}
	vllm.Spec.Engine = workercore.ModelDeploymentEngine{
		Name:  workercore.ModelDeploymentEngineVLLM,
		Image: "vllm/vllm-openai:v0.30.0",
	}

	for i := range vllm.Spec.Roles {
		leg := modelDeploymentEPDEncoderLeg(vllm, &vllm.Spec.Roles[i], nodefeature.ManufacturerNVIDIA)
		assert.Equal(t, ModelDeploymentECConnectorCPU, leg.Connector,
			"the admitted vLLM row resolves to an encoder cache on role %q, so the zeros above are "+
				"the SGLang rows' answer rather than a leg that never ran",
			vllm.Spec.Roles[i].Name)
	}
}

// TestModelDeploymentEPDMatrix pins the table's own shape, so a row cannot be added or edited without
// something here saying so. The rows are the whole admission answer, and the message a refusal
// carries is built from them.
func TestModelDeploymentEPDMatrix(t *testing.T) {
	t.Run("every row names a router, an engine and a platform the API has", func(t *testing.T) {
		routers := map[string]bool{
			workercore.ModelDeploymentRouterLLMD:   true,
			workercore.ModelDeploymentRouterVLLM:   true,
			workercore.ModelDeploymentRouterSGLang: true,
		}
		engines := map[string]bool{
			workercore.ModelDeploymentEngineVLLM:   true,
			workercore.ModelDeploymentEngineSGLang: true,
		}

		for _, row := range modelDeploymentEPDMatrix {
			assert.True(t, routers[row.Router], "router %q", row.Router)
			assert.True(t, engines[row.Engine], "engine %q", row.Engine)
			assert.NotEmpty(t, row.Platform, "row %+v names no platform", row)
			assert.NotEmpty(t, row.Connector, "row %+v names no connector", row)
			assert.NotEmpty(t, row.EngineFloor, "row %+v names no floor", row)
		}
	})

	t.Run("no row names the same combination twice", func(t *testing.T) {
		seen := map[string]bool{}
		for _, row := range modelDeploymentEPDMatrix {
			key := strings.Join([]string{row.Router, row.Engine, row.Platform}, "+")
			assert.False(t, seen[key], "%s is admitted twice", key)
			seen[key] = true
		}
	})

	// THE REMAINING CANDIDATE PAIRS ARE REFUSED. vllm-router fronts vLLM alone, so pairing it with
	// SGLang has no row on any platform, and on Ascend vLLM-Ascend's P/D KV leg requires
	// llm-d-router's decode sidecar handshake; sglang-gateway drives the encoder leg on SGLang
	// alone, so pairing it with vLLM is refused everywhere rather than on one platform.
	t.Run("unadmitted router or engine combinations are refused", func(t *testing.T) {
		for _, combination := range []struct {
			router, engine string
		}{
			{workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentEngineSGLang},
			{workercore.ModelDeploymentRouterSGLang, workercore.ModelDeploymentEngineVLLM},
		} {
			for _, platform := range []string{
				nodefeature.ManufacturerNVIDIA,
				nodefeature.ManufacturerAMD,
				nodefeature.ManufacturerAscend,
			} {
				_, admitted := ModelDeploymentEPDRowFor(combination.router, combination.engine, platform)
				assert.False(t, admitted, "%s+%s is admitted on %s",
					combination.router, combination.engine, platform)
			}
		}

		_, admitted := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterVLLM, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		assert.False(t, admitted, "vllm-router+vLLM is not admitted on ascend")
	})

	// THE FLOORS ARE SPELLED THE WAY EACH PROJECT PUBLISHES A TAG, which is what admission compares
	// against. The values are pinned here because a floor is the one column no user can work
	// around: a deployment naming exactly the floor has to be admitted, and a floor moved in the
	// wrong direction refuses work that runs or admits a pair that does not.
	t.Run("each floor is the release its connector arrives in", func(t *testing.T) {
		assert.Equal(t, "0.30.0rc1", ModelDeploymentECExampleConnectorFirstAscend,
			"the Ascend floor is the first vllm-ascend release holding both encoder fixes, and it "+
				"is spelled the way that project tags a candidate")
		assert.Equal(t, "0.30.0", ModelDeploymentECCPUConnectorFirstVLLM,
			"the network floor is the vLLM release the NIXL connectors ship in")
		assert.Equal(t, "0.5.10", ModelDeploymentSGLangEncoderFirst,
			"the SGLang floor is the first release carrying --encoder-only, --language-only, "+
				"--encoder-urls and --encoder-bootstrap-port")
	})

	t.Run("the admitted rows are the ones the design names", func(t *testing.T) {
		ascend, admitted := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, admitted, "the Ascend row is admitted")
		assert.Equal(t, ModelDeploymentECConnectorExample, ascend.Connector)
		assert.True(t, ascend.SharedVolume, "the Ascend row carries the encoder leg through a claim")
		assert.Equal(t, ModelDeploymentECExampleConnectorFirstAscend, ascend.EngineFloor,
			"the Ascend row carries the floor its own platform's releases are compared against")

		for _, router := range []string{
			workercore.ModelDeploymentRouterLLMD,
			workercore.ModelDeploymentRouterVLLM,
		} {
			for _, platform := range []string{
				nodefeature.ManufacturerAMD,
				nodefeature.ManufacturerNVIDIA,
			} {
				row, admitted := ModelDeploymentEPDRowFor(
					router, workercore.ModelDeploymentEngineVLLM, platform)
				require.True(t, admitted, "the %s+%s row on %s is admitted",
					router, workercore.ModelDeploymentEngineVLLM, platform)
				assert.Equal(t, ModelDeploymentECConnectorCPU, row.Connector)
				assert.False(t, row.SharedVolume,
					"the %s+%s row on %s carries the encoder leg over the network",
					router, workercore.ModelDeploymentEngineVLLM, platform)
				assert.Equal(t, ModelDeploymentECCPUConnectorFirstVLLM, row.EngineFloor)
			}
		}

		// SGLANG IS ADMITTED BEHIND BOTH ROUTERS ON ALL THREE PLATFORMS, and every one of those
		// rows reads the same: the address leg, no claim, and the SGLang floor. The loop is the
		// assertion, because a row admitted on two platforms and forgotten on the third would
		// otherwise pass with the two platforms checked.
		for _, router := range []string{
			workercore.ModelDeploymentRouterLLMD,
			workercore.ModelDeploymentRouterSGLang,
		} {
			for _, platform := range []string{
				nodefeature.ManufacturerNVIDIA,
				nodefeature.ManufacturerAMD,
				nodefeature.ManufacturerAscend,
			} {
				row, admitted := ModelDeploymentEPDRowFor(
					router, workercore.ModelDeploymentEngineSGLang, platform)
				require.True(t, admitted, "the %s+%s row on %s is admitted",
					router, workercore.ModelDeploymentEngineSGLang, platform)
				assert.Equal(t, ModelDeploymentECConnectorSGLangURLs, row.Connector,
					"the SGLang leg is named by address and configures no encoder-cache connector")
				assert.False(t, row.SharedVolume,
					"the SGLang leg mounts no claim, so the claim rule does not apply to it")
				assert.Equal(t, ModelDeploymentSGLangEncoderFirst, row.EngineFloor)
			}
		}

		assert.Empty(t, ModelDeploymentECSidecarConnector(ModelDeploymentECConnectorSGLangURLs),
			"the decode sidecar dispatches on encoder-cache connectors only, so an address leg "+
				"leaves it with nothing to name and renders no --ec-connector")
	})

	// THE RETURNED ROW IS A COPY, so a caller holding one cannot reach the table behind it. The
	// table is unexported precisely so that no package can rewrite what is admitted, and a pointer
	// would have undone that for every package that could name this function.
	t.Run("the returned row is not the table", func(t *testing.T) {
		admitted, found := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, found)

		admitted.Connector = "SomethingElseEntirely"
		again, found := ModelDeploymentEPDRowFor(
			workercore.ModelDeploymentRouterLLMD, workercore.ModelDeploymentEngineVLLM,
			nodefeature.ManufacturerAscend)
		require.True(t, found)
		assert.Equal(t, ModelDeploymentECConnectorExample, again.Connector,
			"writing to a returned row must not reach the admitted table")
	})
}

// TestModelDeploymentEPDEncoderURLs_TakeTheSchemeFromTheEncoderArguments pins the one property the
// address has: the host is the short Service name, and only the scheme follows the encoder's own
// arguments. A listener serving TLS is dialed over https and any other over http, so a prefiller
// never dials an address the encoder refuses, and a non-TLS encoder renders what it always did.
func TestModelDeploymentEPDEncoderURLs_TakeTheSchemeFromTheEncoderArguments(t *testing.T) {
	testCases := []struct {
		name        string
		encoderArgs []string
		want        string
	}{
		{
			name: "no TLS flag is http",
			want: "http://qwen-encode:9100",
		},
		{
			name:        "a certificate is https",
			encoderArgs: []string{"--ssl-certfile", "/etc/tls/tls.crt"},
			want:        "https://qwen-encode:9100",
		},
		{
			name:        "a key alone is https",
			encoderArgs: []string{"--ssl-keyfile", "/etc/tls/tls.key"},
			want:        "https://qwen-encode:9100",
		},
		{
			name:        "a CA bundle alone leaves http",
			encoderArgs: []string{"--ssl-ca-certs", "/etc/tls/ca.crt"},
			want:        "http://qwen-encode:9100",
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			md := epdDeployment(t, []workercore.ModelDeploymentRoleKind{
				workercore.ModelDeploymentRoleKindEncode,
				workercore.ModelDeploymentRoleKindPrefill,
			})
			for i := range md.Spec.Roles {
				if md.Spec.Roles[i].Kind == workercore.ModelDeploymentRoleKindEncode {
					md.Spec.Roles[i].ExtraArgs = append([]string{"--port", "9100"}, tc.encoderArgs...)
				}
			}

			assert.Equal(t, []string{tc.want}, modelDeploymentEPDEncoderURLs(md))
		})
	}
}
