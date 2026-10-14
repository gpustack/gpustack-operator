package worker

import (
	"fmt"
	"strings"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// The encoder-cache connectors an admitted row can name. The names are the upstream ones, because
// they are what a user reads in an engine log when the leg misbehaves.
//
// ECExampleConnector passes the encoder's output through a directory both roles mount. It is the
// only connector the Ascend example runs, and the only one that needs no copy path of any kind.
const ModelDeploymentECConnectorExample = "ECExampleConnector"

// ModelDeploymentECConnectorCPU is the network connector the decode sidecar drives, pulling the
// embeddings out of the encoder's response over NIXL.
const ModelDeploymentECConnectorCPU = "ECCPUConnector"

// ModelDeploymentECCPUConnectorFirstVLLM is the vLLM release carrying the network encoder-cache
// connectors ECCPUConnector and ECMooncakeConnector.
// https://github.com/vllm-project/vllm/blob/v0.30.0/docs/features/ec_cpu_connector.md
const ModelDeploymentECCPUConnectorFirstVLLM = "0.30.0"

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
	EngineFloor string
	// SharedVolume records that the encoder leg reaches the prefiller through a mounted directory
	// rather than over the network, which is what brings the claim rule to bear on this row.
	SharedVolume bool
}

// modelDeploymentEPDMatrix holds every admitted combination.
//
// IT STARTS WITH NO NVIDIA ROW, and that is the state of the work rather than an oversight: an
// NVIDIA combination is admitted only after its proof of concept passes, and none has. The rows
// below are the two the design admits at once, implemented from the upstream code and marked
// unverified until their verification passes.
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

	if row.Connector == ModelDeploymentECConnectorCPU {
		return inject.EncoderCache{Connector: row.Connector, CPUBytes: modelDeploymentEPDCPUBytes}
	}

	// The shared-directory leg carries its embeddings through a directory both roles mount, and
	// the connector is told ONE path while two roles have to arrive at the same one. Admission
	// refuses a deployment whose two mounts disagree, so the encoder's own mount is read here and
	// the prefiller is given that same path rather than its own: were they ever to disagree, one
	// number keeps the two ends of the leg paired and the mismatch becomes visible at admission.
	return inject.EncoderCache{
		Connector:         row.Connector,
		SharedStoragePath: modelDeploymentEPDSharedPath(md),
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
// The address is the per-role Service, named deployment-role, on the port the encoder's container
// serves: the Service is what a consumer inside the cluster can resolve, and the encoder serves no
// other address. It is empty for a deployment declaring no encoder, which renders nothing.
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
		return []string{fmt.Sprintf("http://%s-%s:%d", md.Name, role.Name,
			modelDeploymentServicePort(role).ContainerPort)}
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
func ModelDeploymentEPDMatrixSummary() string {
	rows := make([]string, 0, len(modelDeploymentEPDMatrix))
	for _, row := range modelDeploymentEPDMatrix {
		rows = append(rows, fmt.Sprintf(
			"%s+%s+%s+%s>=%s", row.Router, row.Engine, row.Platform, row.Connector, row.EngineFloor))
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
