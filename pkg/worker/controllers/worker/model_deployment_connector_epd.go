package worker

import (
	"fmt"
	"strings"

	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/strconvx"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// The connectors an admitted row can name. The names are the upstream ones, because they are what
// a user reads in an engine log when the leg misbehaves.
//
// ECExampleConnector passes the encoder's output through a directory both roles mount. It is the
// only encoder-cache connector the Ascend example runs, and the only one that needs no copy path of
// any kind.
const ModelDeploymentECConnectorExample = "ECExampleConnector"

// ModelDeploymentECConnectorCPU is the network connector the decode sidecar drives, pulling the
// embeddings out of the encoder's response over NIXL.
const ModelDeploymentECConnectorCPU = "ECCPUConnector"

// ModelDeploymentECCPUConnectorFirstVLLM is the vLLM release carrying the network encoder-cache
// connectors ECCPUConnector and ECMooncakeConnector.
// https://github.com/vllm-project/vllm/blob/v0.30.0/docs/features/ec_cpu_connector.md
const ModelDeploymentECCPUConnectorFirstVLLM = "0.30.0"

// ModelDeploymentECConnectorSGLangURLs names SGLang's address-based encoder leg, the one carrying
// the encoder's output to the prefiller through --encoder-urls and --encoder-bootstrap-port.
//
// IT IS NOT AN ENCODER-CACHE CONNECTOR: SGLang configures no connector, and the prefiller dials the
// encoder's address itself. The name is kept in the same column as the cache connectors because that
// column is the whole answer to how a row carries the encoder's output.
const ModelDeploymentECConnectorSGLangURLs = "encoder-urls"

// ModelDeploymentSGLangEncoderFirst is the lowest SGLang release carrying --encoder-only,
// --language-only, --encoder-urls and --encoder-bootstrap-port.
// https://github.com/sgl-project/sglang/blob/v0.5.18/docs/docs/advanced_features/epd_disaggregation.mdx
const ModelDeploymentSGLangEncoderFirst = "0.5.10"

// ModelDeploymentECExampleConnectorFirstAscend is the lowest vllm-ascend release this row runs on,
// and it is the lowest release holding BOTH fixes the shared leg needs rather than either one:
//
//   - PR 16682 (commit ed0138b1a) skips the language-model forward in the dummy runs, without
//     which `--mm-encoder-only` aborts the engine at startup. First in v0.28.0.rc1.
//   - PR 18291 (commit 23d65455b) pins the encoder cache load to the rank's own NPU, without
//     which a prefill with tensor parallelism above one loads on the wrong device. First in
//     v0.30.0rc1.
//
// v0.23.0, which the row carried before, has neither: an encoder there dies at startup, and a
// parallel prefill reads a peer's embeddings. v0.30.0rc2 holds the same code as v0.30.0rc1 and
// fixes only the Dockerfile, so the floor is the first of the two.
//
// THE SPELLING IS THE PROJECT'S, NOT SEMVER'S: vllm-ascend tags a candidate `v0.30.0rc1` here and
// `v0.29.0.rc1` on the previous line, and admission reads both.
const ModelDeploymentECExampleConnectorFirstAscend = "0.30.0rc1"

// ModelDeploymentEPDRow is one admitted combination: the router that pairs the roles, the engine
// that serves them, the accelerator platform they run on, and the connector that carries the
// encoder's output to the prefiller, with the lowest engine version that connector appears in.
//
// The four columns are the ones a user writes, and every one of them has refused a real
// combination: a router that cannot drive an encoder role, an engine with no encoder arguments, a
// platform whose connector does not exist, and a connector the platform cannot carry. A row is
// therefore the whole admission answer, and a combination missing from this table is refused rather
// than admitted and discovered at container start.
//
// IT LIVES HERE RATHER THAN BESIDE THE ADMISSION RULES THAT READ IT, because it has a second
// reader the rules do not: the render. The engine leg and the decode sidecar are both told which
// connector this deployment runs, and both must be told the row's answer rather than each deriving
// one from the platform -- two derivations over one platform are two answers to one question. The
// admission package already imports this one to read shared policy such as the effective role kind,
// so a row placed there would make this package import its own caller, which Go forbids.
type ModelDeploymentEPDRow struct {
	// Router is the router that pairs the three roles.
	Router string
	// Engine is the engine the roles run.
	Engine string
	// Platform is the accelerator manufacturer every role's instance type must report.
	Platform string
	// Connector is the encoder-cache connector this row renders.
	Connector string
	// EngineFloor is the lowest engine version whose connector this row names.
	//
	// NO LONGER READ BY ADMISSION. It was the floor the encoder version gate compared against, and
	// that gate is gone with spec.engine.version: the operator no longer knows which version an
	// image carries, because the user names the image rather than the version. The requirement it
	// states did not become false, it moved -- the connector needs that version of the engine
	// library inside the image, and the image is now the user's to choose and to check. It is kept
	// because it is a fact about the connector that the field's own constants still cite upstream
	// provenance for, and because a future field could read it; the requirement itself is recorded
	// in docs/modules/model-deployment/engine-versions.md, which is where a user can act on it.
	EngineFloor string
	// SharedVolume records that the encoder leg reaches the prefiller through a mounted directory
	// rather than over the network, which is what brings the claim rule to bear on this row.
	SharedVolume bool
}

// modelDeploymentEPDMatrix holds every admitted (router, engine, platform) combination.
//
// The Ascend and ROCm vLLM rows are the design baseline: implemented from the upstream code and
// still unverified on hardware. The NVIDIA vLLM rows (behind llm-d-router and vllm-router) and the
// NVIDIA SGLang rows (behind llm-d-router and sglang-gateway) are verified on hardware. The
// remaining AMD and Ascend rows are admitted on the strength of the operator-side rendering and
// upstream connector parity; their hardware verification is tracked in issue #763. vllm-router is
// not admitted on Ascend because vLLM-Ascend's Mooncake KV connector requires the nixlv2 handshake
// that only llm-d-router's decode proxy relays, and a combination missing from this table is
// refused rather than admitted.
var modelDeploymentEPDMatrix = []ModelDeploymentEPDRow{
	{
		// vLLM on Ascend, fronted by llm-d-router, with the encoder's output carried through a
		// directory the user attaches to both roles as a shared ReadWriteMany claim.
		//
		// ECExampleConnector is the connector the upstream Ascend example itself runs:
		// https://github.com/vllm-project/vllm-ascend/blob/v0.23.0/docs/source/user_guide/feature_guide/epd_disaggregation.md
		// It is also the only one that works there, because the network connectors have no NPU copy
		// path and Ascend's EC and KV legs use files, MemCache and Mooncake over the ascend protocol,
		// never UCX or NIXL. The plugin's own network connector, ECMemcacheConnector, arrived in
		// v0.30.0rc1 and is not a row here: the operator does not configure it, and ECMooncakeConnector
		// is still open upstream.
		//
		// THE FLOOR IS ModelDeploymentECExampleConnectorFirstAscend, which states the two upstream
		// fixes by number. The version on this row is a vllm-ascend release rather than an upstream
		// one: the runner the operator assembles for an Ascend pool installs that plugin, so the
		// number a user writes is the one it tags.
		Router:       workercore.ModelDeploymentRouterLLMD,
		Engine:       workercore.ModelDeploymentEngineVLLM,
		Platform:     nodefeature.ManufacturerAscend,
		Connector:    ModelDeploymentECConnectorExample,
		EngineFloor:  ModelDeploymentECExampleConnectorFirstAscend,
		SharedVolume: true,
	},
	{
		// vLLM on ROCm, fronted by llm-d-router, with the encoder's output pulled over NIXL by the
		// decode sidecar rather than through a mounted directory.
		//
		// ECCPUConnector is the connector this platform takes, and the floor is the release the
		// network connectors appear in. NIXL supports ROCm from v1.3.0
		// (https://github.com/ai-dynamo/nixl/releases/tag/v1.3.0), so the engine floor and the
		// transport floor do not bind differently here. No required ROCm test of NIXL's UCX backend
		// was found upstream, which is why the row is admitted and marked unverified.
		//
		// A shared claim buys nothing on this row, so the claim rule does not apply: the sidecar reads
		// the encoder's response and pulls the embeddings itself.
		Router:      workercore.ModelDeploymentRouterLLMD,
		Engine:      workercore.ModelDeploymentEngineVLLM,
		Platform:    nodefeature.ManufacturerAMD,
		Connector:   ModelDeploymentECConnectorCPU,
		EngineFloor: ModelDeploymentECCPUConnectorFirstVLLM,
	},
	{
		// vLLM on NVIDIA CUDA, fronted by llm-d-router, pulls the encoder's output over NIXL the
		// way the ROCm row does. Verified on 4 x NVIDIA L40S (driver 580.173.02, CUDA 13.0,
		// Qwen/Qwen2.5-VL-7B-Instruct, vLLM 0.30.0): the multimodal request was answered through
		// the router with the leg live on both halves.
		Router:      workercore.ModelDeploymentRouterLLMD,
		Engine:      workercore.ModelDeploymentEngineVLLM,
		Platform:    nodefeature.ManufacturerNVIDIA,
		Connector:   ModelDeploymentECConnectorCPU,
		EngineFloor: ModelDeploymentECCPUConnectorFirstVLLM,
	},
	// THE TWO VLLM-ROUTER ROWS PAIR THE SAME ECCPUConnector LEG WITH vllm-router'S OWN
	// THREE-STAGE ORCHESTRATION (--epd-config={"encoder_urls":[...]}). The router drives the
	// encode -> prefill -> decode sequence directly without a decode sidecar, stripping
	// ec_transfer_params before the decode call while the engine roles exchange embeddings over
	// NIXL (ECCPUConnector) and KV cache over MooncakeConnector. Ascend is absent because
	// vLLM-Ascend's P/D KV leg requires the nixlv2 handshake that only llm-d-router's decode
	// sidecar speaks.
	{
		Router:      workercore.ModelDeploymentRouterVLLM,
		Engine:      workercore.ModelDeploymentEngineVLLM,
		Platform:    nodefeature.ManufacturerAMD,
		Connector:   ModelDeploymentECConnectorCPU,
		EngineFloor: ModelDeploymentECCPUConnectorFirstVLLM,
	},
	{
		Router:      workercore.ModelDeploymentRouterVLLM,
		Engine:      workercore.ModelDeploymentEngineVLLM,
		Platform:    nodefeature.ManufacturerNVIDIA,
		Connector:   ModelDeploymentECConnectorCPU,
		EngineFloor: ModelDeploymentECCPUConnectorFirstVLLM,
	},
	// THE SIX SGLANG ROWS THAT FOLLOW SHARE ONE LEG AND ONE EVIDENCE LEVEL. SGLang carries the
	// encoder's output by address rather than through an encoder-cache connector: the encoder runs
	// --encoder-only, the prefiller runs --language-only and dials the encoder through
	// --encoder-urls. That is what the connector column records, and it is why no row configures a
	// connector, mounts no claim and brings the claim rule to bear on the pair.
	//
	// THE NVIDIA ROWS HAVE RUN END TO END; THE OTHER PLATFORMS HAVE NOT. SGLang on NVIDIA passed
	// behind both routers on L40S: a text prompt and an image prompt each returned 200 with generated
	// text, the encoder was reached from the prefiller, and the prefiller kept its widened probes
	// (readiness 6, liveness 12, timeout 5s) at zero restarts. Send those requests to the deployment's
	// published endpoint, which is the router Service on port 8081: the plain <md> Service selects the
	// prefiller role directly and carries no bootstrap, so a request sent there is refused by SGLang
	// with HTTP 400 before any decoder runs. That is a property of the front door, not a failure of
	// the row. The AMD ROCm and Ascend NPU rows are still unverified on hardware, tracked in issue
	// #763.
	{
		// SGLang on Ascend, fronted by llm-d-router.
		Router:      workercore.ModelDeploymentRouterLLMD,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerAscend,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
	{
		Router:      workercore.ModelDeploymentRouterLLMD,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerAMD,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
	{
		Router:      workercore.ModelDeploymentRouterLLMD,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerNVIDIA,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
	{
		// The same leg behind sglang-gateway, which routes the pair natively rather than through
		// llm-d-router's decider.
		Router:      workercore.ModelDeploymentRouterSGLang,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerAscend,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
	{
		Router:      workercore.ModelDeploymentRouterSGLang,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerAMD,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
	{
		Router:      workercore.ModelDeploymentRouterSGLang,
		Engine:      workercore.ModelDeploymentEngineSGLang,
		Platform:    nodefeature.ManufacturerNVIDIA,
		Connector:   ModelDeploymentECConnectorSGLangURLs,
		EngineFloor: ModelDeploymentSGLangEncoderFirst,
	},
}

// ModelDeploymentEPDConnector returns the encoder-cache connector this combination runs, or the
// empty string when the combination is not admitted.
//
// The empty string is the answer for a deployment that declares no Encode role AND for one whose
// combination has no row, and both render no encoder stage. Those are the same rendering outcome
// reached for two different reasons, and neither is a failure to report: an unadmitted combination
// never reaches the render, because admission refuses it first.
//
// This is the single source the render reads. It takes the combination rather than the deployment
// because that is the whole identity of a row, and it returns one string rather than the row so
// that a caller cannot re-derive the connector from the row's other columns beside this answer.
func ModelDeploymentEPDConnector(router, engine, platform string) string {
	row, admitted := ModelDeploymentEPDRowFor(router, engine, platform)
	if !admitted {
		return ""
	}

	return row.Connector
}

// modelDeploymentEPDCPUBytes is the buffer the network encoder-cache connector allocates per
// replica.
//
// IT IS THE UPSTREAM EXAMPLE'S NUMBER, not a measured one: vLLM v0.30.0
// `docs/features/ec_cpu_connector.md` ships `"ec_cpu_bytes": 1073741824` in its configuration table.
// The connector requires the field and has no default (read at `ec_connector/cpu/connector.py`),
// so the operator must state a value; it is a capacity decision this operator makes on the user's
// behalf until the API carries a field for it, and the one number a user is most likely to want is
// their own. A deployment needing a different buffer has no way to say so, which is the gap a
// spec field would close.
const modelDeploymentEPDCPUBytes = 1 << 30

// modelDeploymentEPDEncoderLeg resolves what ONE role of a deployment declaring an Encode role must
// be told about the encoder stage, and whether that role runs any part of it.
//
// THE ROLE DECIDES WHICH HALF, and the row decides the transport. An encoder is the producer and
// the prefiller is the consumer; every other role renders nothing from the leg, which is why the
// zero answer below covers decode and server roles rather than filtering them out at the call site.
//
// The half is NOT written into the returned leg: `inject` reads it off the role the caller already
// passes it, so stating it here would be a second statement of the same fact.
//
// IT IS EMPTY FOR A DEPLOYMENT DECLARING NO ENCODER, which is what keeps the encoder stage off a
// deployment that never asked for one. A prefill/decode pair is a pair, and a row that happens to
// exist for its engine and platform says nothing about whether this deployment runs an encoder; a
// prefiller given a connector here would be told to look for embeddings no process produces. The
// answer is the same zero as for a kind outside the leg, which is why the gate is on the
// deployment rather than on the role.
//
// It is also empty for an unadmitted combination, which keeps a render that ran ahead of a changed
// spec from configuring a leg nothing admitted. Admission refuses such a deployment, so a render
// that reaches this on one is looking at an object stored before the row was withdrawn; the empty
// leg renders it as it rendered before any of this existed, which is the safe direction.
func modelDeploymentEPDEncoderLeg(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, manufacturer string,
) inject.EncoderCache {
	kind := ModelDeploymentEffectiveRoleKind(role)
	if kind != workercore.ModelDeploymentRoleKindEncode &&
		kind != workercore.ModelDeploymentRoleKindPrefill {
		return inject.EncoderCache{}
	}

	if !modelDeploymentDeclaresEncoder(md) {
		return inject.EncoderCache{}
	}

	router := ""
	if md.Spec.Router != nil {
		router = md.Spec.Router.Name
	}
	row, admitted := ModelDeploymentEPDRowFor(router, md.Spec.Engine.Name, manufacturer)
	if !admitted {
		return inject.EncoderCache{}
	}

	switch row.Connector {
	case ModelDeploymentECConnectorCPU:
		return inject.EncoderCache{Connector: row.Connector, CPUBytes: modelDeploymentEPDCPUBytes}
	case ModelDeploymentECConnectorExample:
		// The shared-directory leg carries its embeddings through a directory both roles mount, and
		// the connector is told ONE path while two roles have to arrive at the same one. Admission
		// refuses a deployment whose two mounts disagree, so the encoder's own mount is read here and
		// the prefiller is given that same path rather than its own: were they ever to disagree, one
		// number keeps the two ends of the leg paired and the mismatch becomes visible at admission.
		return inject.EncoderCache{
			Connector:         row.Connector,
			SharedStoragePath: modelDeploymentEPDSharedPath(md),
		}
	default:
		// An address leg renders no encoder cache: SGLang takes the encoder's addresses through
		// EncoderURLs and configures no connector, so there is nothing for a cache request to carry.
		return inject.EncoderCache{}
	}
}

// modelDeploymentDeclaresEncoder reports whether the deployment runs an encoder stage at all.
//
// It reads the EFFECTIVE kind rather than the declared one, so a role whose kind defaults is
// resolved the same way everywhere else in this file. The answer is asked before anything about the
// encoder stage is derived, because a deployment without an encoder has no encoder stage to
// describe and the rest of the answers below are meaningless for it.
func modelDeploymentDeclaresEncoder(md *workercore.ModelDeployment) bool {
	for i := range md.Spec.Roles {
		if ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) ==
			workercore.ModelDeploymentRoleKindEncode {
			return true
		}
	}

	return false
}

// modelDeploymentCreateRoleOrder returns the roles in the order a pass issues their Pods: the
// declared order, except that an encoder goes last in a deployment declaring one.
//
// THE ORDER IS WHAT MAKES THE PREFERENCES BELOW MEAN WHAT THEY SAY. The prefiller and the decoder
// carry the heavier mutual weight because they exchange KV on every request, but a preference is
// scored against Pods that already exist. With the encoder listed first it is placed first, the
// prefiller then scores the encoder's node alone, fills it, and the decoder lands on another node.
// Issuing the encoder last lets the heavier pair settle together first. Roles other than the
// encoder keep their declared relative order, and a deployment without an encoder is unchanged.
func modelDeploymentCreateRoleOrder(md *workercore.ModelDeployment) []*workercore.ModelDeploymentRole {
	ordered := make([]*workercore.ModelDeploymentRole, 0, len(md.Spec.Roles))
	var encoders []*workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if ModelDeploymentEffectiveRoleKind(role) == workercore.ModelDeploymentRoleKindEncode {
			encoders = append(encoders, role)

			continue
		}
		ordered = append(ordered, role)
	}

	return append(ordered, encoders...)
}

// modelDeploymentEPDPodAffinity renders the soft co-location preference one replica of a deployment
// declaring an encoder carries, or nil for a deployment declaring none, which must render exactly
// as it did before the preference existed.
//
// THE WEIGHTS FOLLOW THE TRANSFER LEG. Prefill and decode exchange KV cache on every token, so
// their pair is the one co-location buys the most and carries the larger weight; the encoder's
// output reaches the prefiller once per request, so that pair carries the smaller one. Every term
// is a preference and none is a requirement: a cluster too tight to co-locate a pair still
// schedules it, merely spread out.
//
// The terms select the way the Services do -- one deployment instance, one role kind -- and are
// scored on the node hostname, the level the transfer leg actually measures.
func modelDeploymentEPDPodAffinity(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) *core.PodAffinity {
	if !modelDeploymentDeclaresEncoder(md) {
		return nil
	}

	peerTerm := func(target workercore.ModelDeploymentRoleKind, weight int32) core.WeightedPodAffinityTerm {
		kind, _ := ModelDeploymentRoleUpstreamKind(target)
		return core.WeightedPodAffinityTerm{
			Weight: weight,
			PodAffinityTerm: core.PodAffinityTerm{
				LabelSelector: &meta.LabelSelector{MatchLabels: map[string]string{
					modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
					modelDeploymentLabelKeyInstance: md.Name,
					modelDeploymentLabelKeyRoleKind: kind,
				}},
				TopologyKey: core.LabelHostname,
			},
		}
	}

	switch ModelDeploymentEffectiveRoleKind(role) {
	case workercore.ModelDeploymentRoleKindPrefill:
		return &core.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
				peerTerm(workercore.ModelDeploymentRoleKindDecode, 100),
				peerTerm(workercore.ModelDeploymentRoleKindEncode, 50),
			},
		}
	case workercore.ModelDeploymentRoleKindDecode:
		return &core.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
				peerTerm(workercore.ModelDeploymentRoleKindPrefill, 100),
			},
		}
	case workercore.ModelDeploymentRoleKindEncode:
		return &core.PodAffinity{
			PreferredDuringSchedulingIgnoredDuringExecution: []core.WeightedPodAffinityTerm{
				peerTerm(workercore.ModelDeploymentRoleKindPrefill, 50),
			},
		}
	default:
		return nil
	}
}

// modelDeploymentEPDConnector is the row's connector for this deployment as a whole, or empty when
// it runs no admitted encoder stage.
//
// IT IS DEPLOYMENT-WIDE WHERE THE LEG IS PER ROLE, because the two are configured onto different
// processes: the leg is handed to the container the encoder cache runs inside, and only the encoder
// and the prefiller carry one, while the sidecar flag is read by the decode sidecar, which is
// neither of those roles. Reading the flag off the role's own leg would ask the decoder a question
// whose answer is always the zero value, and render no flag on the one Pod that reads it.
//
// The renderer runs the sidecar on the decode Pod only, so the flag is set unconditionally here and
// only ever read where a sidecar is. Empty renders nothing, which is the answer for a deployment
// declaring no encoder and for one on no admitted row.
func modelDeploymentEPDConnector(
	md *workercore.ModelDeployment, manufacturer string,
) string {
	if !modelDeploymentDeclaresEncoder(md) {
		return ""
	}

	router := ""
	if md.Spec.Router != nil {
		router = md.Spec.Router.Name
	}

	return ModelDeploymentEPDConnector(router, md.Spec.Engine.Name, manufacturer)
}

// modelDeploymentEPDSplitsEncoderByAddress reports whether an engine reaches the encoder stage
// through addresses rather than through a connector.
//
// THE TWO TRANSPORTS ARE BOTH REAL AND THEY NEED DIFFERENT INPUTS. An engine given a connector is
// configured by naming it, and the row is what says which; an engine that splits its encoder off
// its own server is configured by being handed the addresses to dial, so it names no connector and
// therefore has no row to be found in. Gating that engine on a row would leave its encoder
// rendering as an undivided server that serves nothing the prefiller asks for.
//
// IT IS THE ENGINE'S OWN FAMILY, because that is where the difference lives: the same package
// renders the address transport for the SGLang family alone, and an engine outside both families
// names no encoder arguments at all.
func modelDeploymentEPDSplitsEncoderByAddress(engine string) bool {
	return engine == workercore.ModelDeploymentEngineSGLang
}

// modelDeploymentEPDReachesEncoderStage reports whether this deployment's engine can be told about
// an encoder stage at all, and is the question the renderer's own reachability asks.
//
// IT IS FALSE FOR AN ENCODER ROLE ON A COMBINITION WITH NO ADMITTED ROW AND NO ADDRESS TRANSPORT,
// and that is the same answer the deployment had before an encoder role could be configured: such a
// role renders undivided. It is reached by a reconcile of an object admission refuses, either
// because the combination is not admitted or because the object was stored before the row was
// withdrawn; rendering it as it rendered keeps an unrelated reconcile from failing over a field
// nothing consumes.
func modelDeploymentEPDReachesEncoderStage(
	md *workercore.ModelDeployment, manufacturer string,
) bool {
	if !modelDeploymentDeclaresEncoder(md) {
		return false
	}

	return modelDeploymentEPDConnector(md, manufacturer) != "" ||
		modelDeploymentEPDSplitsEncoderByAddress(md.Spec.Engine.Name)
}

// modelDeploymentEPDSharedPath is the directory the encoder and the prefiller both mount, or empty
// when neither role declares a claim.
//
// The ENCODER'S OWN mount is read, and the prefiller's is deliberately not consulted: the two are
// compared at admission, so they are the same directory by the time a render runs, and reading one
// of them keeps this from becoming a second place the pair could be spelled differently.
func modelDeploymentEPDSharedPath(md *workercore.ModelDeployment) string {
	if !modelDeploymentDeclaresEncoder(md) {
		return ""
	}

	for i := range md.Spec.Roles {
		if ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) !=
			workercore.ModelDeploymentRoleKindEncode {
			continue
		}
		for _, volume := range md.Spec.Roles[i].AdditionalVolumes {
			if volume.PersistentVolumeClaim != nil {
				return volume.MountPath
			}
		}
	}

	return ""
}

// modelDeploymentEPDEncoderURLs are the addresses of the encoder stage, in the form the engine that
// takes addresses rather than a connector is given them: ONE URL PER SERVICE, not per replica.
//
// It is deployment-wide rather than per role, because a deployment declaring an Encode role has one
// set of them. The roles that do not read the list render nothing from it, so passing it to all of
// them is what lets one value cross the renderer without a per-role filter that could disagree with
// which roles actually consume it.
//
// The host is the short Service name, which every role of a deployment resolves by its own search
// path because all of them share the deployment's namespace. Only the scheme is derived, the way
// modelDeploymentRoleEndpoint derives it, so an encoder serving TLS is dialed over https by the
// prefiller as it is by the router, and an encoder that does not is addressed exactly as before.
// It is empty for a deployment declaring no encoder, which renders nothing.
func modelDeploymentEPDEncoderURLs(md *workercore.ModelDeployment) []string {
	if !modelDeploymentDeclaresEncoder(md) {
		return nil
	}

	for i := range md.Spec.Roles {
		if ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) !=
			workercore.ModelDeploymentRoleKindEncode {
			continue
		}

		role := &md.Spec.Roles[i]
		scheme, _ := modelDeploymentEngineTransport(md.Spec.Engine.Name, ModelDeploymentRoleArgs(role))
		port := modelDeploymentServicePort(role)

		return []string{strings.ToLower(string(scheme)) + "://" + md.Name + "-" + role.Name + ":" +
			strconvx.Itoa(int(port.ContainerPort))}
	}

	return nil
}

// modelDeploymentECSidecarConnector translates an admitted row's connector into the spelling the
// decode sidecar dispatches on, and returns empty for anything else.
//
// THE TWO SPELLINGS ARE DELIBERATELY DIFFERENT and neither names the other: the row carries the
// engine-side connector name, because that is what the engine resolves and what a user reads in an
// engine log, while the sidecar carries its own two, because those are the values its flag admits.
// The mapping is a table rather than a derivation because the two sets need not grow together --
// an admitted connector with no sidecar support is refused at admission, and the reverse leaves the
// sidecar able to name a connector no engine here resolves.
//
// It is the ONLY place the two vocabularies meet, which is what lets both halves of the leg be
// configured from one row without either half re-deciding the transport.
func ModelDeploymentECSidecarConnector(connector string) string {
	switch connector {
	case ModelDeploymentECConnectorExample:
		return modelDeploymentECConnectorExample
	case ModelDeploymentECConnectorCPU:
		return modelDeploymentECConnectorNIXL
	default:
		return ""
	}
}

// ModelDeploymentEPDMatrixSummary renders every admitted row, for a refusal to name beside the
// combination it refused. A user told a combination is not admitted can otherwise only read the
// table, which is not something they can see.
//
// IT NAMES NO ENGINE VERSION. A row's EngineFloor is what the connector needs of the image, and
// that requirement is now carried by the documentation rather than by admission, so printing it
// here would tell a refused user that a version they can satisfy would admit the row. Nothing they
// write would.
func ModelDeploymentEPDMatrixSummary() string {
	rows := make([]string, 0, len(modelDeploymentEPDMatrix))
	for _, row := range modelDeploymentEPDMatrix {
		rows = append(rows, fmt.Sprintf("%s+%s+%s+%s", row.Router, row.Engine, row.Platform, row.Connector))
	}

	return strings.Join(rows, ", ")
}

// ModelDeploymentEPDRowFor returns the admitted row for this combination and whether one is
// admitted.
//
// IT RETURNS A COPY, NOT A POINTER INTO THE TABLE, because the table is deliberately unexported so
// that no caller can reach it -- and a pointer handed out through an exported function reaches it
// just as well. The exported surface now spans two packages, so a caller in the admission package
// could rewrite the admitted table from a request, and every later admission would answer from the
// object it had just been given. A value cannot be written through, so the table stays read-only
// whatever a caller does with what it is handed.
func ModelDeploymentEPDRowFor(router, engine, platform string) (ModelDeploymentEPDRow, bool) {
	for _, row := range modelDeploymentEPDMatrix {
		if row.Router == router && row.Engine == engine && row.Platform == platform {
			return row, true
		}
	}

	return ModelDeploymentEPDRow{}, false
}

// ModelDeploymentEPDMatrixNames reports whether any admitted row satisfies the predicate.
func ModelDeploymentEPDMatrixNames(match func(*ModelDeploymentEPDRow) bool) bool {
	for i := range modelDeploymentEPDMatrix {
		if match(&modelDeploymentEPDMatrix[i]) {
			return true
		}
	}

	return false
}

// ModelDeploymentEPDMatrixGap names the column a combination misses, so a refusal says which side is
// absent rather than only that the combination is. It returns the empty string when the combination
// is admitted.
func ModelDeploymentEPDMatrixGap(router, engine, platform string) string {
	if _, admitted := ModelDeploymentEPDRowFor(router, engine, platform); admitted {
		return ""
	}

	named := ModelDeploymentEPDMatrixNames

	switch {
	case router == "":
		return "no router is named, so nothing pairs the three roles"
	case !named(func(row *ModelDeploymentEPDRow) bool { return row.Router == router }):
		return fmt.Sprintf("router %q is not admitted for an encoder role", router)
	case !named(func(row *ModelDeploymentEPDRow) bool { return row.Router == router && row.Engine == engine }):
		return fmt.Sprintf("router %q is not admitted with engine %q", router, engine)
	default:
		return fmt.Sprintf(
			"platform %q is not admitted for encoder roles under router %q and engine %q",
			platform, router, engine)
	}
}
