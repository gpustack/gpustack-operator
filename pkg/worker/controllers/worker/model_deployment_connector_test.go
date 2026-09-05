package worker

import (
	"encoding/json"
	"slices"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// connectorInput is one pool-and-domain fixture every case renders from.
//
// It carries a NON-EMPTY Domain on purpose: a case asserting which engines receive a tenant is only
// worth anything if a domain was available to render.
func connectorInput(engine, manufacturer string) ModelDeploymentConnectorInput {
	return ModelDeploymentConnectorInput{
		Engine:              engine,
		Manufacturer:        manufacturer,
		Domain:              "team-a-shared",
		MasterServerAddress: "shared-kv-master.gpustack-system.svc:50051",
		Protocol:            "tcp",
	}
}

// clientConfigOf decodes the client JSON out of the annotation that carries it.
//
// The configuration is no longer a map on the render: it is a JSON document in a Pod annotation,
// projected into the container by downwardAPI. Decoding rather than matching the string is what the
// renderer asks for -- JSON defines no key order, so comparing text would pin something that is not
// part of the contract.
//
// Requiring the annotation to be there first is deliberate. Every assertion built on this value
// would pass vacuously against a nil map, so a render that produced no file at all would read as a
// render whose file lacks whichever key the case names.
func clientConfigOf(t *testing.T, got ModelDeploymentConnectorRender) map[string]any {
	t.Helper()

	raw, ok := got.PodAnnotations[inject.ClientConfigAnnotationKey]
	require.True(t, ok, "the render carries no client-config annotation, so there is no file to read")

	var cfg map[string]any
	require.NoError(t, json.Unmarshal([]byte(raw), &cfg))

	return cfg
}

func TestSynthesizeModelDeploymentConnector(t *testing.T) {
	// The two vLLM-family readers load configuration from a file, because their loader uses the
	// environment only to locate the path. BOTH NOW GET THE SAME SEVEN KEYS, including `mode`, which
	// only vLLM's reader has -- and rendering it for vLLM-Ascend is safe for a reason worth stating
	// rather than assuming: vLLM does not pass `mode` to store.setup() either, it only validates the
	// pair against global_segment_size and logs it. The value that drives both engines is the size,
	// and both read that. A key one reader ignores is harmless exactly when the reader that HAS it
	// does not act on it.
	//
	// The size values are float64 because that is what decoding a JSON number into `any` yields, not
	// because anything renders a float. The contract these assertions hold is "a JSON number, never
	// a string" -- and they still hold it: a rendered `"0"` would decode to a string and fail here.
	wantFileConfig := map[string]any{
		"master_server_address": "shared-kv-master.gpustack-system.svc:50051",
		"metadata_server":       "P2PHANDSHAKE",
		"protocol":              "tcp",
		"device_name":           "",
		"global_segment_size":   float64(0),
		"local_buffer_size":     float64(134217728),
		"mode":                  "standalone-store",
	}

	testCases := []struct {
		name             string
		engine           string
		manufacturer     string
		wantArgs         []string
		wantEnv          []core.EnvVar
		wantDefaultedEnv []core.EnvVar
		wantConfig       map[string]any
	}{
		{
			name:         "vllm_golden",
			engine:       workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantFileConfig,
		},
		{
			// SAME ENGINE VALUE AS THE CASE ABOVE, different hardware. That is the whole point of
			// narrowing the enum: the connector name changed and nothing about the engine did.
			name:         "vllm_on_ascend_golden",
			engine:       workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerAscend,
			wantArgs: []string{
				"--kv-transfer-config",
				`{"kv_connector":"AscendStoreConnector","kv_role":"kv_both"}`,
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_CONFIG_PATH", Value: "/etc/gpustack/kvcache/mooncake.json"},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       wantFileConfig,
		},
		{
			// SGLang takes the environment, and every part of this case is load-bearing: no
			// extra-config argument and no config-path variable, because either one diverts the
			// engine onto a loader whose per-key fallbacks are compile-time literals; and no
			// file, because a mounted file nothing reads claims a wiring that is not happening.
			// The one value that cannot be known at admission time arrives as a fieldRef, which is
			// the whole reason this engine does not get a file.
			//
			// MOONCAKE_TENANT_ID IS NEW HERE, and it is a fix rather than an addition. This engine
			// DOES forward a tenant at the version this project ships: its loader reads that
			// variable and passes the value on as a keyword argument, so the domain reaches the
			// store. The renderer decides this per engine from a table carrying the version and
			// source line each answer was measured at -- the two vLLM entries forward none and get
			// none. Nothing in this file restates that answer; the fixture supplies a domain and
			// this case pins what came back for THIS engine.
			name:         "sglang_golden",
			engine:       workercore.ModelDeploymentEngineSGLang,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			wantArgs: []string{
				"--hicache-storage-backend", "mooncake",
			},
			wantEnv: []core.EnvVar{
				{Name: "MOONCAKE_TENANT_ID", Value: "team-a-shared"},
				{Name: "MOONCAKE_MASTER", Value: "shared-kv-master.gpustack-system.svc:50051"},
				{Name: "MOONCAKE_TE_META_DATA_SERVER", Value: "P2PHANDSHAKE"},
				{Name: "MOONCAKE_PROTOCOL", Value: "tcp"},
				{Name: "MOONCAKE_DEVICE", Value: ""},
				{Name: "MOONCAKE_GLOBAL_SEGMENT_SIZE", Value: "0"},
				{
					Name: "MOONCAKE_LOCAL_HOSTNAME",
					ValueFrom: &core.EnvVarSource{
						FieldRef: &core.ObjectFieldSelector{FieldPath: "status.podIP"},
					},
				},
			},
			wantDefaultedEnv: []core.EnvVar{{Name: "MC_TE_METRIC", Value: "1"}},
			wantConfig:       nil,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(connectorInput(tc.engine, tc.manufacturer))
			require.NoError(t, err)

			assert.Equal(t, tc.wantArgs, got.Args)
			assert.Equal(t, tc.wantEnv, got.Env)
			assert.Equal(t, tc.wantDefaultedEnv, got.DefaultedEnv)

			// An engine on the environment carrier renders no file, and the three pieces that
			// deliver one go together: the annotation holding the document, the projection reading
			// that annotation, and the mount. Asserting all three empty is what distinguishes "no
			// file, by design" from "the file went missing".
			if tc.wantConfig == nil {
				assert.Empty(t, got.Volumes, "the environment carrier mounts nothing")
				assert.Empty(t, got.VolumeMounts, "the environment carrier mounts nothing")
				assert.NotContains(t, got.PodAnnotations, inject.ClientConfigAnnotationKey,
					"an annotation holding a document nothing projects would be dead weight on every replica")

				return
			}

			// Exact equality, not containment: "exactly the keys this engine's reader reads" is a
			// claim about what is absent as much as about what is present.
			assert.Equal(t, tc.wantConfig, clientConfigOf(t, got))
			assert.NotEmpty(t, got.Volumes, "the file carrier needs the projection that reads the annotation")
			assert.NotEmpty(t, got.VolumeMounts, "a projected volume nothing mounts reaches no container")
		})
	}
}

// connectorInputForKind is the fixture above with a role kind, for the cases that vary only in it.
func connectorInputForKind(
	engine, manufacturer string, kind workercore.ModelDeploymentRoleKind,
) ModelDeploymentConnectorInput {
	in := connectorInput(engine, manufacturer)
	in.Kind = kind

	return in
}

// TestSynthesizeModelDeploymentConnector_RoleDiscriminator is the golden per (engine, kind).
//
// It is what makes G1 falsifiable at all. If a prefill role and a decode role rendered the same
// configuration they would not be prefill and decode -- they would be replicas of one role that
// happen to share a Workload, and the atomic-admission demonstration would pass identically for a
// deployment that has nothing to do with P/D.
//
// The two vLLM-family rows are both here because they share this renderer and NOT a connector
// registry: a single expected connector name would pass while one of the two engines could not start.
func TestSynthesizeModelDeploymentConnector_RoleDiscriminator(t *testing.T) {
	testCases := []struct {
		name         string
		engine       string
		manufacturer string
		kind         workercore.ModelDeploymentRoleKind
		wantArg      string
	}{
		{
			name: "vllm_server", engine: workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			kind:         workercore.ModelDeploymentRoleKindServer,
			wantArg:      `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`,
		},
		{
			name: "vllm_prefill", engine: workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			kind:         workercore.ModelDeploymentRoleKindPrefill,
			wantArg:      `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_producer"}`,
		},
		{
			name: "vllm_decode", engine: workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerNVIDIA,
			kind:         workercore.ModelDeploymentRoleKindDecode,
			wantArg:      `{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_consumer"}`,
		},
		{
			name: "vllm_ascend_prefill", engine: workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerAscend,
			kind:         workercore.ModelDeploymentRoleKindPrefill,
			wantArg:      `{"kv_connector":"AscendStoreConnector","kv_role":"kv_producer"}`,
		},
		{
			name: "vllm_ascend_decode", engine: workercore.ModelDeploymentEngineVLLM,
			manufacturer: nodefeature.ManufacturerAscend,
			kind:         workercore.ModelDeploymentRoleKindDecode,
			wantArg:      `{"kv_connector":"AscendStoreConnector","kv_role":"kv_consumer"}`,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(
				connectorInputForKind(tc.engine, tc.manufacturer, tc.kind))
			require.NoError(t, err)

			require.Len(t, got.Args, 2)
			assert.Equal(t, "--kv-transfer-config", got.Args[0])
			assert.JSONEq(t, tc.wantArg, got.Args[1])
		})
	}
}

// TestSynthesizeModelDeploymentConnector_ServerIsTheSingleRoleRender is the regression guard that
// this spec does not change what a deployment written before it renders.
//
// An unset kind and an explicit server must be the SAME render, byte for byte, and both must be the
// render the single-role golden case above pins. Two ways to say "no split" that produced two
// configurations would roll every existing deployment the moment the field was defaulted.
func TestSynthesizeModelDeploymentConnector_ServerIsTheSingleRoleRender(t *testing.T) {
	unset, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	server, err := SynthesizeModelDeploymentConnector(connectorInputForKind(
		workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA,
		workercore.ModelDeploymentRoleKindServer))
	require.NoError(t, err)

	assert.Equal(t, unset, server, "an unset kind and an explicit server are one shape")
	assert.Equal(t, []string{
		"--kv-transfer-config",
		`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_both"}`,
	}, server.Args, "and it is what the single-role golden case pins")
}

// TestSynthesizeModelDeploymentConnector_DiscriminatorTravelsInAnOwnedKey is the owned-key half.
//
// The discriminator is not a key of its own -- it is a field inside the vLLM transfer-config
// document. That means the key a user would have to supply to overwrite it is already owned, so the
// webhook already refuses it, and the refusal and the render read the same table.
//
// The owned name is DERIVED from the render rather than restated: the case finds which argument
// changed between two kinds and asks whether THAT one is owned. Restating "--kv-transfer-config"
// would keep passing if the discriminator ever moved to an argument nobody owns, which is exactly the
// change this exists to catch.
func TestSynthesizeModelDeploymentConnector_DiscriminatorTravelsInAnOwnedKey(t *testing.T) {
	server, err := SynthesizeModelDeploymentConnector(connectorInputForKind(
		workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA,
		workercore.ModelDeploymentRoleKindServer))
	require.NoError(t, err)
	prefill, err := SynthesizeModelDeploymentConnector(connectorInputForKind(
		workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA,
		workercore.ModelDeploymentRoleKindPrefill))
	require.NoError(t, err)

	require.Equal(t, len(server.Args), len(prefill.Args),
		"the kind changes a value, not the shape of the command line")
	assert.Equal(t, server.Env, prefill.Env, "and it changes nothing in the environment")

	var carriers []string
	for i := range prefill.Args {
		if prefill.Args[i] != server.Args[i] {
			// The changed entry is a value; the key it belongs to is the flag before it.
			carriers = append(carriers, ModelDeploymentArgName(prefill.Args[i-1]))
		}
	}

	require.Len(t, carriers, 1, "exactly one argument carries the discriminator")
	assert.True(t, ModelDeploymentOwnsArg(workercore.ModelDeploymentEngineVLLM, carriers[0]),
		"%s carries the role discriminator and must be owned, or a user could supply a second one "+
			"and no rule would say which won", carriers[0])
}

// TestSynthesizeModelDeploymentConnector_RoleTheEngineCannotBeTold covers the two ways a kind fails
// to render, and neither may fall back to a plain server.
//
// A decoder configured as a server serves whole requests and looks healthy, which is the silent
// wrong result the whole kind field exists to prevent.
func TestSynthesizeModelDeploymentConnector_RoleTheEngineCannotBeTold(t *testing.T) {
	_, err := SynthesizeModelDeploymentConnector(connectorInputForKind(
		workercore.ModelDeploymentEngineSGLang, nodefeature.ManufacturerNVIDIA,
		workercore.ModelDeploymentRoleKindPrefill))
	require.Error(t, err, "sglang has no prefill/decode equivalent, so it is refused rather than rendered")

	_, err = SynthesizeModelDeploymentConnector(connectorInputForKind(
		workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA, "router"))
	require.ErrorContains(t, err, "router",
		"a kind with no mapping is refused naming it, never rendered as a plain server")
}

// TestModelDeploymentConnector_ReachesThePodPerRole closes the gap the cases above leave open.
//
// Every one of them calls the synthesis directly, so all of them would still pass if the reconciler
// stopped handing it the role's kind: the parameter would never enter the system under test, and
// each role's Pod would carry the plain-server configuration while the tests stayed green. This one
// runs the reconciler and reads the rendered Pods.
//
// The pool and the backend are in the fixture beside the Binding for the same reason: a converge
// fixture carrying only the Binding resolves NO connection, and every claim about a connector then
// holds vacuously.
func TestModelDeploymentConnector_ReachesThePodPerRole(t *testing.T) {
	md := twoRoleDeployment(func(md *workercore.ModelDeployment) {
		md.Spec.Roles[0].Kind = workercore.ModelDeploymentRoleKindPrefill
		md.Spec.Roles[1].Kind = workercore.ModelDeploymentRoleKindDecode
	})
	cli := newModelDeploymentClient(md, newRenderInstanceType(),
		newRenderBinding(), newRenderPool(), newRenderBackend())

	_, err := reconcileModelDeployment(t, cli)
	require.NoError(t, err)

	byRole := map[string]string{}
	pods := replicaPods(t, cli)
	for i := range pods {
		pod := &pods[i]
		// The operator owns the whole argv, so the synthesized arguments are folded into Command;
		// the container carries no Args at all.
		argv := pod.Spec.Containers[0].Command
		at := slices.Index(argv, "--kv-transfer-config")
		require.GreaterOrEqual(t, at, 0, "%s carries no transfer configuration at all", pod.Name)
		byRole[modelDeploymentPodRole(pod)] = argv[at+1]
	}

	require.Len(t, byRole, 2)
	assert.JSONEq(t,
		`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_producer"}`, byRole["prefill"])
	assert.JSONEq(t,
		`{"kv_connector":"MooncakeStoreConnector","kv_role":"kv_consumer"}`, byRole["decode"])
}

// TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier states the four properties that
// make the environment carrier correct, each of which a plausible-looking edit would break.
//
// It exists as its own test rather than as more rows in the golden case because the golden case
// asserts the whole rendering by equality, and an equality assertion tells whoever broke it WHAT
// changed but not WHY it mattered. These carry the why.
func TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier(t *testing.T) {
	got, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineSGLang, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	// The engine resolves its config source in the order extra-config, then config path, then
	// environment. Either of the first two wins over the environment, so rendering one does not
	// add a fallback, it REPLACES this configuration with one whose missing keys become literals.
	assert.NotContains(t, got.Args, "--hicache-storage-backend-extra-config",
		"its loader is key-for-key identical to the file loader and sits at higher precedence")
	for _, e := range got.Env {
		assert.NotEqual(t, "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH", e.Name,
			"setting it selects the file loader, whose fallback for local_hostname is the literal localhost")
	}

	// The identity has to be evaluated by kubelet at container start, because no Pod IP exists when
	// the object is admitted. A literal here is the defect being avoided, INCLUDING a literal that
	// looks correct, so the assertion is on the fieldRef and on the absence of a value.
	var hostname *core.EnvVar
	for i := range got.Env {
		if got.Env[i].Name == "MOONCAKE_LOCAL_HOSTNAME" {
			hostname = &got.Env[i]
		}
	}
	require.NotNil(t, hostname, "SGLang reads this key and its own fallback is the literal localhost")
	assert.Empty(t, hostname.Value, "a literal identity registers every replica as the same peer")
	require.NotNil(t, hostname.ValueFrom)
	require.NotNil(t, hostname.ValueFrom.FieldRef)
	assert.Equal(t, "status.podIP", hostname.ValueFrom.FieldRef.FieldPath)

	// A pure client contributes no storage segment. The value must be an explicit zero: this
	// engine's own fallback is the string "4gb", so an absent key is a 4 GiB in-process member.
	// The store accepts zero for exactly this purpose -- its setup_internal skips mounting a
	// segment and its validator requires zero or at least MIN_SEGMENT_SIZE.
	assert.Contains(t, got.Env, core.EnvVar{Name: "MOONCAKE_GLOBAL_SEGMENT_SIZE", Value: "0"})

	// NO FILE IS RENDERED FOR THIS ENGINE, and all three carriers of one are asserted absent rather
	// than just the document: an annotation with no projection is dead weight on every replica, and
	// a projection with no annotation mounts an empty file. Checking one of the three would leave
	// the other two free to appear.
	assert.NotContains(t, got.PodAnnotations, inject.ClientConfigAnnotationKey)
	assert.Empty(t, got.Volumes)
	assert.Empty(t, got.VolumeMounts)
}

// TestSynthesizeModelDeploymentConnector_KeysNeverRendered states, per key, WHY the operator leaves
// it out. Exact map equality above already fails if any of them appears; these cases exist so that
// whoever deletes one has to read the reason first.
//
// BOTH REMAINING REASONS ARE DECISIONS. Three others used to pin a defect instead, and the
// distinction was marked inline precisely so that this could happen: a green case is not by itself
// an endorsement of the behavior it asserts, so each one carries its reason rather than just its
// key, and the three whose reason stopped holding were deleted rather than left green.
func TestSynthesizeModelDeploymentConnector_KeysNeverRendered(t *testing.T) {
	testCases := []struct {
		name string
		key  string
		why  string
	}{
		{
			name: "no_tenant_id",
			key:  "tenant_id",
			why: "no supported engine passes a tenant to the cache client: it is setup()'s 11th " +
				"parameter and every engine calls setup() positionally with seven or eight " +
				"arguments, so rendering the key would document a wiring that is not happening",
		},
		{
			name: "no_local_hostname",
			key:  "local_hostname",
			why: "these two readers derive it from their own process, so a deployment-wide file " +
				"holding a value that differs per replica would be wrong for all but one. " +
				"THIS IS TRUE OF THESE TWO ONLY -- SGLang reads the key from the file and falls " +
				"back to a literal, which is why it takes the environment instead",
		},
		// THREE MORE CASES USED TO SIT HERE, pinning the absence of `global_segment_size`,
		// `local_buffer_size` and `mode` as a KNOWN DEFECT. They are gone because the defect is
		// fixed: those keys declare a role rather than sizing a contribution, and the coherent
		// group is now rendered. What replaced them is a POSITIVE assertion in
		// TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient -- the direction
		// that fails when a key goes missing, rather than the direction that passes.
	}

	// SGLANG IS DELIBERATELY NOT IN THIS LIST, and leaving it in would be worse than useless. It
	// renders no ClientConfig at all, so every NotContains below would pass against a nil map --
	// vacuously, proving nothing, while reading as three more engines' worth of coverage. Its own
	// keys are asserted positively in TestSynthesizeModelDeploymentConnector_SGLangEnvironmentCarrier,
	// where an absent key fails instead of passing.
	// Both file carriers, which is now one engine on two backends rather than two engines.
	carriers := []struct{ engine, manufacturer string }{
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA},
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerAscend},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			for _, c := range carriers {
				got, err := SynthesizeModelDeploymentConnector(connectorInput(c.engine, c.manufacturer))
				// A nil map would make every assertion below vacuous, so the carrier itself is
				// checked first: this test only means anything where a file is rendered.
				require.NoError(t, err)
				require.NotEmpty(t, clientConfigOf(t, got), "%s on %s renders a file carrier", c.engine, c.manufacturer)
				assert.NotContains(t, clientConfigOf(t, got), tc.key, "%s on %s: %s", c.engine, c.manufacturer, tc.why)
			}
		})
	}
}

// TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient asserts the group that makes
// a replica a client of the pool rather than a member of it.
//
// It is stated positively and per key, because the failure it guards is a MISSING key: every
// engine's config class defaults `global_segment_size` to 4 GiB, so an absent key does not fall
// back to "no contribution", it selects the wrong role and does so without any error. An assertion
// that a key is absent cannot catch that; only one that it is present with the right value can.
//
// The values are asserted as JSON numbers. All three engines run these keys through a parser that
// would also accept "0", but the type the config classes declare is int, and matching the
// declaration means the rendering does not depend on that parser staying where it is.
func TestSynthesizeModelDeploymentConnector_FileCarrierDeclaresPureClient(t *testing.T) {
	// BOTH ROWS NOW EXPECT THE SAME FILE, and the pair is kept rather than collapsed because "the
	// same engine on two accelerators renders one document" is the property worth pinning. A single
	// row would assert it for whichever hardware it named.
	//
	// The previous second row expected NO `mode` on Ascend, whose reader has no such field. It is
	// rendered for both now: vLLM does not pass `mode` to store.setup() either, only validating it
	// against global_segment_size and logging it, so the key drives nothing on the reader that HAS
	// it -- which is the condition under which a key the other reader ignores is harmless.
	testCases := []struct {
		name         string
		manufacturer string
	}{
		{
			name:         "vllm_on_nvidia_declares_standalone_store",
			manufacturer: nodefeature.ManufacturerNVIDIA,
		},
		{
			name:         "vllm_on_ascend_declares_the_same_store",
			manufacturer: nodefeature.ManufacturerAscend,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := SynthesizeModelDeploymentConnector(
				connectorInput(workercore.ModelDeploymentEngineVLLM, tc.manufacturer))
			require.NoError(t, err)

			cfg := clientConfigOf(t, got)

			// float64 is what decoding a JSON number into `any` yields; the contract held here is
			// "a number, never a string", and a rendered "0" would decode to a string and fail.
			assert.Equal(t, float64(0), cfg["global_segment_size"],
				"a positive value, including the 4 GiB default, makes this replica a store member")
			assert.Equal(t, float64(128*1024*1024), cfg["local_buffer_size"],
				"the documented client staging size; a non-positive value is rejected outright")

			// The mode is one half of a cross-field rule vLLM's own __post_init__ enforces in both
			// directions: embedded rejects a zero segment, standalone-store rejects a non-zero one.
			// Rendering the segment without the mode raises at startup, which is why the two are
			// asserted together and never one at a time.
			assert.Equal(t, "standalone-store", cfg["mode"])
		})
	}
}

// TestSynthesizeModelDeploymentConnector_DeviceSpellingIsNotConfigurable replaces a case that
// asserted a caller-supplied RDMA device reached the file under the JSON spelling.
//
// THE CAPABILITY IT COVERED WAS THE DEFECT. A device is named per host -- mlx5_0 on one, erdma_0 on
// the next -- so no single name is correct for every host one pool spans, and the deleted case
// passed `mlx5_0` as though it were. The filter is empty on every path including RDMA, meaning "use
// every device found", and it is a constant in the renderer rather than an input here.
//
// The spelling half is still worth pinning: the same fact has three names across three surfaces, and
// only the file's applies to a rendered document.
func TestSynthesizeModelDeploymentConnector_DeviceSpellingIsNotConfigurable(t *testing.T) {
	got, err := SynthesizeModelDeploymentConnector(
		connectorInput(workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA))
	require.NoError(t, err)

	cfg := clientConfigOf(t, got)
	assert.Equal(t, "", cfg["device_name"],
		"empty is the only value correct for every host in one pool")
	assert.NotContains(t, cfg, "rdma_devices", "that is setup()'s positional spelling, not the file's")
	assert.NotContains(t, cfg, "MOONCAKE_DEVICE", "that is Mooncake's own environment spelling, not the file's")
}

func TestSynthesizeModelDeploymentConnector_UnsupportedEngine(t *testing.T) {
	_, err := SynthesizeModelDeploymentConnector(connectorInput("tensorrt-llm", nodefeature.ManufacturerNVIDIA))
	require.Error(t, err)
}

func TestModelDeploymentArgName(t *testing.T) {
	testCases := []struct {
		name  string
		given string
		want  string
	}{
		{name: "bare_flag", given: "--kv-transfer-config", want: "--kv-transfer-config"},
		{name: "flag_with_inline_value", given: `--kv-transfer-config={"a":1}`, want: "--kv-transfer-config"},
		{name: "value_only", given: "32768", want: "32768"},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ModelDeploymentArgName(tc.given))
		})
	}
}

func TestModelDeploymentOwnership(t *testing.T) {
	testCases := []struct {
		name     string
		engine   string
		arg      string
		env      string
		wantsArg bool
		wantsEnv bool
	}{
		{
			name:     "vllm_owns_its_transfer_config",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--kv-transfer-config",
			wantsArg: true,
		},
		{
			name:     "ownership_ignores_an_inline_value",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      `--kv-transfer-config={"kv_connector":"Other"}`,
			wantsArg: true,
		},
		{
			name:     "vllm_does_not_own_an_ordinary_argument",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--max-model-len=32768",
			wantsArg: false,
		},
		{
			name:     "sglang_owns_its_hicache_arguments",
			engine:   workercore.ModelDeploymentEngineSGLang,
			arg:      "--hicache-storage-backend-extra-config",
			wantsArg: true,
		},
		{
			// Ownership is per (engine, key). SGLang's key is an ordinary user argument on vLLM,
			// and refusing it there would refuse something harmless.
			name:     "vllm_does_not_own_sglangs_key",
			engine:   workercore.ModelDeploymentEngineVLLM,
			arg:      "--hicache-storage-backend-extra-config",
			wantsArg: false,
		},
		{
			name:     "sglang_does_not_own_vllms_key",
			engine:   workercore.ModelDeploymentEngineSGLang,
			arg:      "--kv-transfer-config",
			wantsArg: false,
		},
		{
			name:     "vllm_owns_its_config_path",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MOONCAKE_CONFIG_PATH",
			wantsEnv: true,
		},
		{
			// Owned even though the operator never sets it: setting it would divert the engine onto
			// the file loader, whose per-key fallbacks are literals. Ownership here is for what a
			// user entry would DESTROY, not for what it would duplicate.
			name:     "sglang_owns_the_config_path_it_deliberately_leaves_unset",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "SGLANG_HICACHE_MOONCAKE_CONFIG_PATH",
			wantsEnv: true,
		},
		{
			name:     "sglang_owns_its_identity_variable",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_LOCAL_HOSTNAME",
			wantsEnv: true,
		},
		{
			name:     "sglang_owns_its_segment_size",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_GLOBAL_SEGMENT_SIZE",
			wantsEnv: true,
		},
		{
			// Per (engine, key) again, in the direction that is easy to get wrong: vLLM loads no
			// value from the environment, so these names carry nothing there and refusing them
			// would refuse something harmless.
			name:     "vllm_does_not_own_sglangs_environment",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MOONCAKE_GLOBAL_SEGMENT_SIZE",
			wantsEnv: false,
		},
		{
			// SGLang owns six MOONCAKE_-prefixed names and NOT this one, which is the case that
			// fails if ownership is ever reduced to a prefix test. MOONCAKE_CONFIG_PATH is the
			// vLLM family's config path; this engine does not read it.
			name:     "sglang_does_not_own_the_mooncake_config_path",
			engine:   workercore.ModelDeploymentEngineSGLang,
			env:      "MOONCAKE_CONFIG_PATH",
			wantsEnv: false,
		},
		{
			// A defaulted key is deliberately not owned: duplication is harmless because last-wins
			// is well defined, so a user turning metrics off gets their value rather than a refusal.
			name:     "the_metrics_switch_is_not_owned",
			engine:   workercore.ModelDeploymentEngineVLLM,
			env:      "MC_TE_METRIC",
			wantsEnv: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			if tc.arg != "" {
				assert.Equal(t, tc.wantsArg, ModelDeploymentOwnsArg(tc.engine, tc.arg))
			}
			if tc.env != "" {
				assert.Equal(t, tc.wantsEnv, ModelDeploymentOwnsEnv(tc.engine, tc.env))
			}
		})
	}
}

// TestModelDeploymentOwnedAndDefaultedCannotDisagree is the invariant that keeps the refusal and the
// render from drifting apart. It reads the renderer's actual output rather than the table, so a key
// added to one and forgotten in the other fails here instead of in production.
func TestModelDeploymentOwnedAndDefaultedCannotDisagree(t *testing.T) {
	// Every (engine, backend) the renderer can be called with, not every engine. The Ascend row is
	// the load-bearing one: the ownership table is keyed on the ENGINE alone, so this is what
	// proves that claim - if any key the Ascend backend renders were not in vLLM's own entry, this
	// fails, and the table would have to be re-keyed.
	carriers := []struct{ engine, manufacturer string }{
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerNVIDIA},
		{workercore.ModelDeploymentEngineVLLM, nodefeature.ManufacturerAscend},
		{workercore.ModelDeploymentEngineSGLang, nodefeature.ManufacturerNVIDIA},
	}

	for _, c := range carriers {
		t.Run(c.engine+"_on_"+c.manufacturer, func(t *testing.T) {
			engine := c.engine
			got, err := SynthesizeModelDeploymentConnector(connectorInput(engine, c.manufacturer))
			require.NoError(t, err)

			for _, arg := range got.Args {
				name := ModelDeploymentArgName(arg)
				if !isFlag(name) {
					continue // a flag's value, not a key ownership applies to
				}
				assert.True(t, ModelDeploymentOwnsArg(engine, name),
					"the renderer emits %q but the table does not own it", name)
			}

			for _, env := range got.Env {
				assert.True(t, ModelDeploymentOwnsEnv(engine, env.Name),
					"the renderer emits %q as owned but the table does not own it", env.Name)
				assert.False(t, ModelDeploymentDefaultsEnv(env.Name),
					"%q is both owned and defaulted, so a user supplying it is both refused and honoured", env.Name)
			}

			for _, env := range got.DefaultedEnv {
				assert.True(t, ModelDeploymentDefaultsEnv(env.Name),
					"the renderer defaults %q but the table does not list it", env.Name)
				assert.False(t, ModelDeploymentOwnsEnv(engine, env.Name),
					"%q is both defaulted and owned", env.Name)
			}
		})
	}
}

func isFlag(s string) bool {
	return len(s) > 2 && s[0] == '-' && s[1] == '-'
}

func TestModelDeploymentEngineCommand(t *testing.T) {
	testCases := []struct {
		name    string
		engine  string
		model   string
		want    []string
		wantErr bool
	}{
		{
			// The model is POSITIONAL on vLLM's serve subcommand, not a flag.
			name:   "vllm_serves_the_model_positionally",
			engine: workercore.ModelDeploymentEngineVLLM,
			model:  "Qwen/Qwen2.5-72B-Instruct",
			want:   []string{"vllm", "serve", "Qwen/Qwen2.5-72B-Instruct"},
		},
		{
			// The former enum value, kept as a case so that it stays refused. It adds no execution
			// path the unsupported_engine case below does not already cover -- what it records is
			// the DECISION: the runner's service dimension has no such value, every Ascend image is
			// service=vllm, and putting it back would hang the connector on the engine again.
			// An Ascend pool gets its command from the vllm row above, because the entry point does
			// not vary with the backend even though the connector does.
			name:    "the_former_vllm_ascend_value_is_not_an_engine",
			engine:  "vllm-ascend",
			model:   "Qwen/Qwen2.5-72B-Instruct",
			wantErr: true,
		},
		{
			name:   "sglang_launches_a_module_with_model_path",
			engine: workercore.ModelDeploymentEngineSGLang,
			model:  "Qwen/Qwen2.5-72B-Instruct",
			want: []string{
				"python3", "-m", "sglang.launch_server",
				"--model-path", "Qwen/Qwen2.5-72B-Instruct",
			},
		},
		{
			name:    "unsupported_engine",
			engine:  "tensorrt-llm",
			model:   "Qwen/Qwen2.5-72B-Instruct",
			wantErr: true,
		},
		{
			// An empty model would render an argv the engine refuses at startup, which is a
			// failure one layer away from its cause.
			name:    "empty_model",
			engine:  workercore.ModelDeploymentEngineVLLM,
			model:   "",
			wantErr: true,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ModelDeploymentEngineCommand(tc.engine, tc.model)
			if tc.wantErr {
				require.Error(t, err)

				return
			}
			require.NoError(t, err)
			assert.Equal(t, tc.want, got)
		})
	}
}
