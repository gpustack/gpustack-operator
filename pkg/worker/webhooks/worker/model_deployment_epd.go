package worker

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"golang.org/x/mod/semver"
	"k8s.io/apimachinery/pkg/util/validation/field"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// The encoder-cache connectors an admitted row can name. The names are the upstream ones, because
// they are what a user reads in an engine log when the leg misbehaves.
//
// ECExampleConnector passes the encoder's output through a directory both roles mount. It is the
// only connector the Ascend example runs, and the only one that needs no copy path of any kind.
const modelDeploymentECConnectorExample = "ECExampleConnector"

// modelDeploymentECConnectorCPU is the network connector the decode sidecar drives, pulling the
// embeddings out of the encoder's response over NIXL.
const modelDeploymentECConnectorCPU = "ECCPUConnector"

// modelDeploymentECCPUConnectorFirstVLLM is the vLLM release carrying the network encoder-cache
// connectors ECCPUConnector and ECMooncakeConnector.
// https://github.com/vllm-project/vllm/blob/v0.30.0/docs/features/ec_cpu_connector.md
const modelDeploymentECCPUConnectorFirstVLLM = "0.30.0"

// modelDeploymentEPDRow is one admitted combination: the router that pairs the roles, the engine
// that serves them, the accelerator platform they run on, and the connector that carries the
// encoder's output to the prefiller, with the lowest engine version that connector appears in.
//
// The four columns are the ones a user writes, and every one of them has refused a real
// combination: a router that cannot drive an encoder role, an engine with no encoder arguments, a
// platform whose connector does not exist, and a connector the platform cannot carry. A row is
// therefore the whole admission answer, and a combination missing from this table is refused rather
// than admitted and discovered at container start.
type modelDeploymentEPDRow struct {
	// router is the router that pairs the three roles.
	router string
	// engine is the engine the roles run.
	engine string
	// platform is the accelerator manufacturer every role's instance type must report.
	platform string
	// connector is the encoder-cache connector this row renders.
	connector string
	// engineFloor is the lowest engine version whose connector this row names.
	engineFloor string
	// sharedVolume records that the encoder leg reaches the prefiller through a mounted directory
	// rather than over the network, which is what brings the claim rule to bear on this row.
	sharedVolume bool
}

// modelDeploymentEPDMatrix holds every admitted combination.
//
// IT STARTS WITH NO NVIDIA ROW, and that is the state of the work rather than an oversight: an
// NVIDIA combination is admitted only after its proof of concept passes, and none has. The rows
// below are the two the design admits at once, implemented from the upstream code and marked
// unverified until their verification passes.
var modelDeploymentEPDMatrix = []modelDeploymentEPDRow{
	{
		// vLLM on Ascend, fronted by llm-d-router, with the encoder's output carried through a
		// directory the user attaches to both roles as a shared ReadWriteMany claim.
		//
		// ECExampleConnector is the connector the upstream Ascend example itself runs:
		// https://github.com/vllm-project/vllm-ascend/blob/v0.23.0/docs/source/user_guide/feature_guide/epd_disaggregation.md
		// It is also the only one that works there, because the network connectors have no NPU copy
		// path and Ascend's EC and KV legs use files, MemCache and Mooncake over the ascend protocol,
		// never UCX or NIXL.
		//
		// THE FLOOR IS THE ONE THAT UPSTREAM EXAMPLE RECORDS, reached through vllm-ascend's own
		// versioning: it cuts a release branch beside each vLLM release and asks for vLLM at the same
		// version as itself, so the v0.23.0 line is vLLM 0.23.0. That sits above each leg this row
		// needs on its own -- ECExampleConnector arrived in 0.13.0, and MooncakeConnectorV1 was
		// recorded at 0.19.1 -- and it is the floor the pair can actually run at, because neither leg
		// can move up without the plugin moving with it.
		router:       workercore.ModelDeploymentRouterLLMD,
		engine:       workercore.ModelDeploymentEngineVLLM,
		platform:     nodefeature.ManufacturerAscend,
		connector:    modelDeploymentECConnectorExample,
		engineFloor:  "0.23.0",
		sharedVolume: true,
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
		router:      workercore.ModelDeploymentRouterLLMD,
		engine:      workercore.ModelDeploymentEngineVLLM,
		platform:    nodefeature.ManufacturerAMD,
		connector:   modelDeploymentECConnectorCPU,
		engineFloor: modelDeploymentECCPUConnectorFirstVLLM,
	},
}

// modelDeploymentEPDMatrixSummary renders every admitted row, for a refusal to name beside the
// combination it refused. A user told a combination is not admitted can otherwise only read the
// table, which is not something they can see.
func modelDeploymentEPDMatrixSummary() string {
	rows := make([]string, 0, len(modelDeploymentEPDMatrix))
	for _, row := range modelDeploymentEPDMatrix {
		rows = append(rows, fmt.Sprintf(
			"%s+%s+%s+%s>=%s", row.router, row.engine, row.platform, row.connector, row.engineFloor))
	}

	return strings.Join(rows, ", ")
}

// modelDeploymentEPDRowFor returns the admitted row for this combination, or nil.
func modelDeploymentEPDRowFor(router, engine, platform string) *modelDeploymentEPDRow {
	for i := range modelDeploymentEPDMatrix {
		row := &modelDeploymentEPDMatrix[i]
		if row.router == router && row.engine == engine && row.platform == platform {
			return row
		}
	}

	return nil
}

// modelDeploymentEPDMatrixNames reports whether any admitted row satisfies the predicate.
func modelDeploymentEPDMatrixNames(match func(*modelDeploymentEPDRow) bool) bool {
	for i := range modelDeploymentEPDMatrix {
		if match(&modelDeploymentEPDMatrix[i]) {
			return true
		}
	}

	return false
}

// modelDeploymentEPDMatrixGap names the column a combination misses, so a refusal says which side is
// absent rather than only that the combination is. It returns the empty string when the combination
// is admitted.
func modelDeploymentEPDMatrixGap(router, engine, platform string) string {
	if modelDeploymentEPDRowFor(router, engine, platform) != nil {
		return ""
	}

	named := modelDeploymentEPDMatrixNames

	switch {
	case router == "":
		return "no router is named, so nothing pairs the three roles"
	case !named(func(row *modelDeploymentEPDRow) bool { return row.router == router }):
		return fmt.Sprintf("router %q is not admitted for an encoder role", router)
	case !named(func(row *modelDeploymentEPDRow) bool { return row.router == router && row.engine == engine }):
		return fmt.Sprintf("router %q is not admitted with engine %q", router, engine)
	default:
		return fmt.Sprintf(
			"platform %q is not admitted for encoder roles under router %q and engine %q",
			platform, router, engine)
	}
}

// modelDeploymentSemver gives the engine version the leading v that semver comparison requires and
// that the version strings in this API do not carry. A version already carrying one is left alone,
// so the same helper serves the v-prefixed strings the renderer's own tables use.
func modelDeploymentSemver(version string) string {
	if strings.HasPrefix(version, "v") {
		return version
	}

	return "v" + version
}

// modelDeploymentMeetsFloor reports whether version is at or above floor.
//
// The two are compared as semver, so a pre-release sorts below the release it leads to and an
// unreadable version sorts below every valid one. Both directions refuse rather than admit: a
// connector that exists only in 0.30.0 is absent from 0.30.0rc1, and it is absent from a string
// nothing could read.
func modelDeploymentMeetsFloor(version, floor string) bool {
	return semver.Compare(modelDeploymentSemver(version), modelDeploymentSemver(floor)) >= 0
}

// modelDeploymentDeclaresEncode reports whether the deployment declares an Encode role, which is
// what makes it an encoder/prefill/decode deployment and brings the rules below to bear. A
// deployment without one keeps every rule it had.
func modelDeploymentDeclaresEncode(md *workercore.ModelDeployment) bool {
	return modelDeploymentRoleIndexOfKind(md, workercore.ModelDeploymentRoleKindEncode) != -1
}

// modelDeploymentRoleIndexOfKind returns where the one role of this kind sits, or -1 when the
// deployment declares none. The index is what a refusal lands on, because it is the field the user
// wrote.
func modelDeploymentRoleIndexOfKind(
	md *workercore.ModelDeployment, kind workercore.ModelDeploymentRoleKind,
) int {
	for i := range md.Spec.Roles {
		if workerctrl.ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) == kind {
			return i
		}
	}

	return -1
}

// modelDeploymentRoleClaimMount returns the additionalVolumes entry that mounts a claim, and whether
// the role mounts one at all. One is enough to compare against: a role that mounts two claims
// reaches no single shared directory, and the rule that refuses it is the one that refuses the
// encoder's and the prefiller's mounts disagreeing.
func modelDeploymentRoleClaimMount(
	role *workercore.ModelDeploymentRole,
) (*workercore.ModelDeploymentAdditionalVolume, bool) {
	for i := range role.AdditionalVolumes {
		if role.AdditionalVolumes[i].PersistentVolumeClaim != nil {
			return &role.AdditionalVolumes[i], true
		}
	}

	return nil, false
}

// modelDeploymentRouterName returns the router the deployment names, or the empty string.
func modelDeploymentRouterName(md *workercore.ModelDeployment) string {
	if md.Spec.Router == nil {
		return ""
	}

	return md.Spec.Router.Name
}

// validateModelDeploymentEPD holds every rule that applies only to a deployment declaring an Encode
// role. A deployment without one returns nothing, which is what keeps the rendering of every other
// shape unchanged.
//
// IT RUNS ON EVERY CREATE AND EVERY UPDATE, with no held-access exemption for an object stored
// before these rules. The exemption other late rules here carry exists because an object was stored
// while the rule did not exist, and refusing its later edits would strand it. No such object exists
// here: an Encode role was refused at admission until this rule admitted one, so the first object
// to reach these rules is the first that can satisfy them. Re-checking on every update is also what
// stops an edit moving an admitted deployment off the matrix, since the platform, the engine version
// and the router are all re-read from the object as submitted.
func (r *ModelDeploymentWebhook) validateModelDeploymentEPD(
	ctx context.Context, md *workercore.ModelDeployment,
) (field.ErrorList, error) {
	if md.DeletionTimestamp != nil || !modelDeploymentDeclaresEncode(md) {
		return nil, nil
	}

	errs := validateModelDeploymentEPDShape(md)
	errs = append(errs, validateModelDeploymentEPDClaimSources(md)...)

	platform, platformErrs, err := r.modelDeploymentEPDPlatform(ctx, md)
	if err != nil {
		return nil, err
	}
	if len(platformErrs) > 0 {
		// The matrix is read by platform, so a platform that could not be settled leaves nothing to
		// look up and reporting a second refusal about it would only restate the first.
		return append(errs, platformErrs...), nil
	}

	router := modelDeploymentRouterName(md)
	if gap := modelDeploymentEPDMatrixGap(router, md.Spec.Engine.Name, platform); gap != "" {
		return append(errs, field.Invalid(field.NewPath("spec", "engine", "name"), md.Spec.Engine.Name, fmt.Sprintf(
			"%s; an encoder role is admitted only for a combination this operator has run, and the "+
				"admitted rows are %s", gap, modelDeploymentEPDMatrixSummary()))), nil
	}

	row := modelDeploymentEPDRowFor(router, md.Spec.Engine.Name, platform)
	errs = append(errs, validateModelDeploymentEPDEngineVersion(md, row)...)
	if row.sharedVolume {
		errs = append(errs, validateModelDeploymentEPDSharedVolume(md, row)...)
	}

	return errs, nil
}

// validateModelDeploymentEPDShape holds the rules about the three roles and the fields around them
// that need nothing but the object.
//
// AT MOST ONE ROLE OF EACH KIND is already refused above, by the rule that refuses a repeated
// non-Server kind, and so is Encode beside a Server role. What is left here is the pair the rule
// cannot see: an Encode role on its own, with nothing to hand its output to.
func validateModelDeploymentEPDShape(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	encodeIdx := modelDeploymentRoleIndexOfKind(md, workercore.ModelDeploymentRoleKindEncode)
	encodePath := rolesPath.Index(encodeIdx).Child("kind")

	// The encoder has a reader and nothing else. A deployment holding an Encode role and no
	// Prefill would have the encoder produce embeddings that no role consumes; one with no Decode
	// would have no consumer of the prefiller's output either, which is the prefill/decode half this
	// shape exists to extend.
	for _, kind := range []workercore.ModelDeploymentRoleKind{
		workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode,
	} {
		if modelDeploymentRoleIndexOfKind(md, kind) >= 0 {
			continue
		}

		errs = append(errs, field.Required(encodePath, fmt.Sprintf(
			"an %q role needs a %q role beside it, and none is declared: the encoder produces "+
				"embeddings that only a prefiller reads, and the prefiller's own output needs a %q "+
				"role to generate from",
			workercore.ModelDeploymentRoleKindEncode, kind, workercore.ModelDeploymentRoleKindDecode)))
	}

	if md.Spec.Router == nil {
		errs = append(errs, field.Required(field.NewPath("spec", "router"),
			"an Encode role requires a router: without one nothing pairs the roles, and the "+
				"operator does not render an unpaired shape"))
	}

	if md.Spec.KVCache != nil {
		errs = append(errs, field.Forbidden(field.NewPath("spec", "kvCache"), fmt.Sprintf(
			"spec.kvCache is refused on a deployment with an %q role: the field is deployment-wide "+
				"and would attach every role to one pool, but the encoder keeps no KV cache at all, "+
				"so it has nothing to place in that pool",
			workercore.ModelDeploymentRoleKindEncode)))
	}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Command) > 0 {
			errs = append(errs, field.Forbidden(rolesPath.Index(i).Child("command"), fmt.Sprintf(
				"role %q is one of three roles the operator pairs through a router, and a command "+
					"replaces the managed arguments and the routing sidecar: the roles would keep "+
					"their kinds and stop talking to each other", role.Name)))
		}
	}

	errs = append(errs, validateModelDeploymentEPDEncodeShape(md, encodeIdx)...)

	return errs
}

// validateModelDeploymentEPDEncodeShape holds the rules about the Encode role alone.
func validateModelDeploymentEPDEncodeShape(md *workercore.ModelDeployment, encodeIdx int) field.ErrorList {
	var errs field.ErrorList

	role := &md.Spec.Roles[encodeIdx]
	rolePath := field.NewPath("spec", "roles").Index(encodeIdx)

	if role.ReplicaSize != 1 {
		errs = append(errs, field.Invalid(rolePath.Child("replicaSize"), role.ReplicaSize,
			fmt.Sprintf(
				"role %q runs one instance per Pod, so its replica size is 1: the operator places "+
					"one encoder per member and a second instance would produce embeddings for a "+
					"transfer key the prefiller never asks for", role.Name)))
	}

	if role.ElasticEP != nil {
		errs = append(errs, field.Forbidden(rolePath.Child("elasticEp"),
			fmt.Sprintf(
				"role %q cannot carry the elastic-EP profile: that profile admits a single Server "+
					"role of one instance of one Pod, and scales the group holding it, while an "+
					"encoder is a fixed producer whose withdrawal would cut transfers already in "+
					"flight", role.Name)))
	}

	// External data parallelism moves the members that answer a request outside the engine process,
	// which the operator discovers from the Nodes after admission. The encoder has no such question:
	// it produces embeddings rather than answers, and its output is addressed by one replica.
	status := workerctrl.ReadModelDeploymentRoleParallelism(md.Spec.Engine.Name, role)
	if status.LoadBalance == workercore.ModelDeploymentLoadBalanceExternal ||
		status.LoadBalance == workercore.ModelDeploymentLoadBalanceHybrid {
		errs = append(errs, field.Forbidden(rolePath.Child("extraArgs"), fmt.Sprintf(
			"role %q declares a data parallelism the engine balances outside itself, and passes %v: "+
				"an encoder has one producer and one output key, so there is no per-member endpoint "+
				"to balance across", role.Name, role.ExtraArgs)))
	}

	return errs
}

// validateModelDeploymentEPDClaimSources refuses an entry that mounts a claim it cannot name, or
// that names a source beside it.
//
// The rule reads every entry that carries a persistentVolumeClaim and ignores every entry that does
// not. That is deliberate: the claim source arrived with the encoder work, so no object stored before
// it can hold one, and refusing a second source on an entry that predates it would change the
// behavior of a deployment with no encoder role at all.
func validateModelDeploymentEPDClaimSources(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		avsPath := rolesPath.Index(i).Child("additionalVolumes")

		for j := range role.AdditionalVolumes {
			av, avPath := &role.AdditionalVolumes[j], avsPath.Index(j)
			if av.PersistentVolumeClaim == nil {
				continue
			}

			if av.PersistentVolumeClaim.ClaimName == "" {
				errs = append(errs, field.Required(avPath.Child("persistentVolumeClaim", "claimName"),
					fmt.Sprintf(
						"the claim name must not be empty: it names a claim in namespace %q, and an "+
							"empty reference names none", md.Namespace)))
			}

			// One entry mounts one volume. Naming a second source leaves the renderer a choice this
			// API does not make, and the container sees whichever it picked.
			for _, other := range []struct {
				field string
				set   bool
			}{
				{"configMap", av.ConfigMap != nil},
				{"secret", av.Secret != nil},
				{"hostPath", av.HostPath != nil},
			} {
				if other.set {
					errs = append(errs, field.Forbidden(avPath.Child(other.field),
						fmt.Sprintf(
							"%q is set beside persistentVolumeClaim on the same entry: one entry "+
								"mounts one volume, and two sources leave the renderer to pick",
							other.field)))
				}
			}
		}
	}

	return errs
}

// modelDeploymentEPDPlatform returns the one accelerator platform every role runs on, and the
// refusals that stopped it from settling on one.
//
// THE PLATFORM IS READ FROM EACH ROLE'S INSTANCE TYPE rather than declared, because a role names a
// type and the type is what reports its manufacturer. A mixed answer is refused rather than resolved,
// since the connector and the transfer protocol both follow the accelerator and one deployment
// cannot run the same leg on two of them. An empty manufacturer is the not-yet-synced state, not a
// platform of its own, and is refused the same way.
func (r *ModelDeploymentWebhook) modelDeploymentEPDPlatform(
	ctx context.Context, md *workercore.ModelDeployment,
) (string, field.ErrorList, error) {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	seen := make(map[string]*worker.InstanceType, len(md.Spec.Roles))

	platform := ""
	mixed := make([]string, 0, 2)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.InstanceType == "" {
			continue
		}

		instType, ok := seen[role.InstanceType]
		if !ok {
			var err error
			if instType, err = r.getInstanceType(ctx, role.InstanceType, i); err != nil {
				var fieldErr *field.Error
				if !errors.As(err, &fieldErr) || fieldErr.Type == field.ErrorTypeInternal {
					return "", nil, err
				}
				errs = append(errs, fieldErr)

				continue
			}
			seen[role.InstanceType] = instType
		}

		manufacturer := instType.Status.Detail.Manufacturer
		if manufacturer == "" {
			errs = append(errs, field.Invalid(rolesPath.Index(i).Child("instanceType"), role.InstanceType,
				fmt.Sprintf(
					"instance type %q reports no accelerator platform yet: an encoder role is admitted "+
						"per platform, because the connector it needs follows the accelerator, so the "+
						"platform has to be known before this deployment can be judged", role.InstanceType)))

			continue
		}

		if platform == "" {
			platform = manufacturer

			continue
		}
		if platform != manufacturer && !slices.Contains(mixed, manufacturer) {
			mixed = append(mixed, manufacturer)
		}
	}

	if len(mixed) > 0 {
		return "", append(errs, field.Invalid(field.NewPath("spec", "roles"), md.Spec.Roles, fmt.Sprintf(
			"the roles of this deployment run on more than one accelerator platform: one reports %q "+
				"and another reports %q. The encoder leg follows the accelerator, so one deployment "+
				"cannot carry it across two",
			platform, strings.Join(mixed, ", ")))), nil
	}

	return platform, errs, nil
}

// validateModelDeploymentEPDEngineVersion refuses an engine version this row cannot run.
func validateModelDeploymentEPDEngineVersion(
	md *workercore.ModelDeployment, row *modelDeploymentEPDRow,
) field.ErrorList {
	versionPath := field.NewPath("spec", "engine", "version")

	if md.Spec.Engine.Version == "" {
		return field.ErrorList{field.Required(versionPath, fmt.Sprintf(
			"an encoder role needs the engine version to be set: %s first appears in vLLM %s, and a "+
				"version this object does not name cannot be compared against that floor",
			row.connector, row.engineFloor))}
	}

	if modelDeploymentMeetsFloor(md.Spec.Engine.Version, row.engineFloor) {
		return nil
	}

	return field.ErrorList{field.Invalid(versionPath, md.Spec.Engine.Version, fmt.Sprintf(
		"%s carries %s from vLLM %s, so this deployment needs that version or later; %s is below it",
		row.connector, row.platform, row.engineFloor, md.Spec.Engine.Version))}
}

// validateModelDeploymentEPDSharedVolume refuses an encoder leg that the two roles cannot both read.
//
// THE THREE FIELDS ARE COMPARED TOGETHER because the connector is told one directory and both roles
// have to arrive at the same one. The claim alone is not enough: two roles can mount one claim at
// two sub-paths and reach two directories, and the encoder's file lands in one of them.
//
// The encoder's own mount is refused read-only, because the encoder writes the embeddings it is
// asked for and a read-only mount makes that write fail at run time rather than at admission.
func validateModelDeploymentEPDSharedVolume(
	md *workercore.ModelDeployment, row *modelDeploymentEPDRow,
) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	encodeIdx := modelDeploymentRoleIndexOfKind(md, workercore.ModelDeploymentRoleKindEncode)
	prefillIdx := modelDeploymentRoleIndexOfKind(md, workercore.ModelDeploymentRoleKindPrefill)

	// The rule reads two roles, so it has nothing to compare when either is missing or when the
	// encoder is not the single instance this row describes. The shape rules above have already
	// refused both, and a second refusal here would name the same missing role twice.
	if prefillIdx < 0 || md.Spec.Roles[encodeIdx].ReplicaSize != 1 {
		return nil
	}

	encodePath := rolesPath.Index(encodeIdx).Child("additionalVolumes")
	encode := &md.Spec.Roles[encodeIdx]

	encodeMount, ok := modelDeploymentRoleClaimMount(encode)
	if !ok {
		return append(errs, field.Required(encodePath, fmt.Sprintf(
			"role %q is the encoder-cache producer on %s, and %s carries its output through a "+
				"directory rather than over the network: attach the shared claim the prefiller mounts "+
				"as an additionalVolumes entry naming a persistentVolumeClaim",
			encode.Name, row.platform, row.connector)))
	}

	if encodeMount.ReadOnly {
		errs = append(errs, field.Forbidden(encodePath.Child("readOnly"), fmt.Sprintf(
			"role %q writes the embeddings %s carries, so its mount must not be read-only: the "+
				"encoder's write would fail once the Pod is running, with nothing in the object to say "+
				"so", encode.Name, row.connector)))
	}

	prefillPath := rolesPath.Index(prefillIdx).Child("additionalVolumes")
	prefillMount, ok := modelDeploymentRoleClaimMount(&md.Spec.Roles[prefillIdx])
	if !ok {
		return append(errs, field.Required(prefillPath, fmt.Sprintf(
			"role %q reads the embeddings %s carries out of the directory role %q writes them to, "+
				"and mounts nothing: attach the same claim role %q mounts",
			md.Spec.Roles[prefillIdx].Name, row.connector, encode.Name, encode.Name)))
	}

	for _, mismatch := range []struct {
		name  string
		equal bool
	}{
		{"claimName", encodeMount.PersistentVolumeClaim.ClaimName == prefillMount.PersistentVolumeClaim.ClaimName},
		{"subPath", encodeMount.SubPath == prefillMount.SubPath},
		{"mountPath", encodeMount.MountPath == prefillMount.MountPath},
	} {
		if mismatch.equal {
			continue
		}

		errs = append(errs, field.Invalid(encodePath.Child("mountPath"), encodeMount.MountPath, fmt.Sprintf(
			"role %q and role %q must name the same %s for %s to carry the encoder's output between "+
				"them, and they do not",
			encode.Name, md.Spec.Roles[prefillIdx].Name, mismatch.name, row.connector)))
	}

	return errs
}
