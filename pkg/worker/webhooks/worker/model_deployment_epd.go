package worker

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"golang.org/x/mod/semver"
	"k8s.io/apimachinery/pkg/util/validation/field"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
)

// modelDeploymentCandidate matches the release-candidate tail vllm-ascend publishes, the `rcN` of
// v0.30.0rc1. The dot ahead of the tail in v0.29.0.rc1 belongs to the release and is trimmed off
// before this runs, so a tail this matches never carries one.
var modelDeploymentCandidate = regexp.MustCompile(`^rc(0|[1-9][0-9]*)$`)

// modelDeploymentPost matches the `postN` tail of v0.23.0.post1, on the same terms.
//
// A NUMBER WITH A LEADING ZERO IS NOT MATCHED, which is what makes a recognized tail a tail semver
// can read: it rejects a numeric identifier that carries one. A version written that way is left
// alone and stays unreadable, which refuses it.
var modelDeploymentPost = regexp.MustCompile(`^post(0|[1-9][0-9]*)$`)

// modelDeploymentSemver rewrites a version into the spelling semver comparison reads, and leaves a
// version it cannot read alone so that one still sorts below every valid version.
//
// VLLM-ASCEND DOES NOT SPELL A CANDIDATE THE WAY SEMVER REQUIRES. Its tags are v0.30.0rc1 on the
// current line and v0.29.0.rc1 on the one before it, and semver calls both invalid. An invalid
// version sorts below every valid one, so a floor written in either spelling would refuse the very
// tag it names. A tag the project publishes is a version a user may legitimately write here, so the
// tails are read rather than refused. The leading v is added on the way out for the same reason:
// it is what these version strings do not carry and what the comparison requires.
//
// THE NUMBER AFTER rc IS SEPARATED BY A DOT because semver orders an alphanumeric pre-release
// identifier lexically, which would place rc10 below rc9.
//
// A POST-RELEASE IS NEWER THAN THE RELEASE IT FOLLOWS, and semver has no slot above a release: the
// next patch release is the only one. Mapping the tail there keeps both orders the project
// publishes, a post above the release it follows and below the patch that follows that. The post
// number rides along as a second identifier, so two post-releases of one release keep their own
// order too.
//
// A TAIL THIS DOES NOT RECOGNIZE IS LEFT ALONE, so it stays unreadable and every floor refuses it.
// That covers a number with a leading zero, which semver rejects, and a release whose patch is too
// large to count, which no next patch can be built from.
func modelDeploymentSemver(version string) string {
	core := strings.TrimPrefix(version, "v")

	// The release part is the numeric core, and everything after it is a tail this function either
	// reads or leaves for the comparison to refuse. The dot separating the two belongs to neither,
	// so the release ends on its last segment rather than on a separator.
	tail := strings.IndexFunc(core, func(r rune) bool { return (r < '0' || r > '9') && r != '.' })
	if tail < 0 {
		return "v" + core
	}
	release, suffix := strings.TrimSuffix(core[:tail], "."), core[tail:]

	if candidate := modelDeploymentCandidate.FindStringSubmatch(suffix); candidate != nil {
		return "v" + release + "-rc." + candidate[1]
	}

	// The pre-release `-0.N` below the next patch is what puts a post above the release it follows
	// and below the patch that follows that, and orders two posts of one release against each other.
	if post := modelDeploymentPost.FindStringSubmatch(suffix); post != nil {
		if next, ok := modelDeploymentNextPatch(release); ok {
			return "v" + next + "-0." + post[1]
		}
	}

	return "v" + core
}

// modelDeploymentNextPatch returns release with its last segment raised by one, and whether that
// segment was a number this could read.
func modelDeploymentNextPatch(release string) (string, bool) {
	dot := strings.LastIndexByte(release, '.')
	if dot < 0 {
		return "", false
	}

	patch, err := strconv.Atoi(release[dot+1:])
	if err != nil {
		return "", false
	}

	return release[:dot+1] + strconv.Itoa(patch+1), true
}

// modelDeploymentMeetsFloor reports whether version is at or above floor.
//
// The two are compared as semver after modelDeploymentSemver has read the spellings a project
// publishes, so a candidate sorts below the release it leads to and a post-release above the one it
// follows. An unreadable version still sorts below every valid one. Both directions refuse rather
// than admit: a connector that exists only in a release is absent from a candidate of it, and it is
// absent from a string nothing could read.
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
	if gap := workerctrl.ModelDeploymentEPDMatrixGap(router, md.Spec.Engine.Name, platform); gap != "" {
		return append(errs, field.Invalid(field.NewPath("spec", "engine", "name"), md.Spec.Engine.Name, fmt.Sprintf(
			"%s; an encoder role is admitted only for a combination in the admitted matrix, and the "+
				"admitted rows are %s", gap, workerctrl.ModelDeploymentEPDMatrixSummary()))), nil
	}

	// The gap check above has already refused every combination this cannot answer for, so the row
	// is here. The second return is stated rather than dropped, because discarding it would leave a
	// future edit moving the gap check without noticing it had stopped guaranteeing the row.
	row, admitted := workerctrl.ModelDeploymentEPDRowFor(router, md.Spec.Engine.Name, platform)
	if !admitted {
		return append(errs, field.Invalid(field.NewPath("spec", "engine", "name"),
			md.Spec.Engine.Name, "no admitted row for the combination the gap check reported one "+
				"for; this is the operator's own tables disagreeing, not a field to correct")), nil
	}

	errs = append(errs, validateModelDeploymentEPDEngineVersion(md, row)...)
	if row.SharedVolume {
		errs = append(errs, validateModelDeploymentEPDSharedVolume(md, row)...)
	}
	errs = append(errs, validateModelDeploymentEPDSideChannelPort(md, row)...)

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
			"an %q role needs a %q role beside it, and none is declared: %s",
			workercore.ModelDeploymentRoleKindEncode, kind, modelDeploymentEPDSiblingReason(kind))))
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

// modelDeploymentEPDSiblingReason states why a role kind has to be declared beside the Encode role.
// Each kind carries its own sentence, because one sentence covering both explains the prefiller in
// the breath of a missing decoder and leaves the reader working out which half is being reported.
func modelDeploymentEPDSiblingReason(kind workercore.ModelDeploymentRoleKind) string {
	if kind == workercore.ModelDeploymentRoleKindPrefill {
		return "the encoder produces embeddings that only a prefiller reads"
	}

	return fmt.Sprintf("the prefiller's output needs a %q role to generate from",
		workercore.ModelDeploymentRoleKindDecode)
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

// validateModelDeploymentEPDSideChannelPort refuses an encoder whose own HTTP port is the port its
// side channel needs.
//
// THE COLLISION IS BETWEEN TWO WRITERS IN ONE CONTAINER. An NIXL producer binds a ZMQ ROUTER socket
// on the side-channel port in the same container that serves HTTP, so an encoder serving there puts
// the engine's own server on the socket the consumers dial for embeddings. One of the two loses the
// bind, and which one is not decided here -- the symptoms land as a leg that never starts or a
// consumer that connects to an HTTP server, with nothing in either log naming the collision.
//
// BOTH SPELLINGS ARE REFUSED, because a role can name its serving port two ways and the render
// honors either one. A declared `ports` entry is what the operator renders into the engine's
// --port, and it takes precedence: a role declaring a port is held to the first of them
// (`modelDeploymentTargetPort`), so a role may pass --port AND declare one. Reading only the flag
// would leave the declared spelling unguarded, and the refusal would name a number the render does
// not use.
//
// ONLY THE NETWORK ROW CAN COLLIDE, because the side channel is what the network connector renders.
// The shared-directory row carries its embeddings through a mounted path and binds no second socket,
// so its encoder is free to serve on that number. This rule therefore stays beside the matrix rather
// than joining `modelDeploymentRouterReservedPorts`, which is vendor-blind by construction and would
// have to over-claim the shared row.
//
// A ROLE NAMING NEITHER SERVES ON THE DEFAULT, which is not this number, so there is nothing to
// refuse.
func validateModelDeploymentEPDSideChannelPort(
	md *workercore.ModelDeployment, row workerctrl.ModelDeploymentEPDRow,
) field.ErrorList {
	if row.Connector != workerctrl.ModelDeploymentECConnectorCPU {
		return nil
	}

	encodeIdx := modelDeploymentRoleIndexOfKind(md, workercore.ModelDeploymentRoleKindEncode)
	if encodeIdx < 0 {
		return nil
	}

	role := &md.Spec.Roles[encodeIdx]
	rolesPath := field.NewPath("spec", "roles").Index(encodeIdx)
	refusal := func(path *field.Path, value any) field.ErrorList {
		return field.ErrorList{field.Invalid(path, value, fmt.Sprintf(
			"role %q serves HTTP on port %d, which is the port %s binds its side channel on: the "+
				"engine would serve on the socket every consumer dials for embeddings, and one of "+
				"the two fails to bind",
			role.Name, inject.VLLMECSideChannelPort, row.Connector))}
	}

	// The declared set is read first because it is the one the render prefers: where a role
	// declares a port, the flag is held to it rather than consulted.
	var errs field.ErrorList
	for _, port := range role.Ports {
		if port.Port == inject.VLLMECSideChannelPort {
			errs = append(errs, refusal(rolesPath.Child("ports"), port.Port)...)
		}
	}

	if listen, ok := workerctrl.ModelDeploymentRoleListenPort(md.Spec.Engine.Name, role); ok &&
		listen == inject.VLLMECSideChannelPort {
		errs = append(errs, refusal(rolesPath.Child("extraArgs"),
			fmt.Sprintf("--port=%d", listen))...)
	}

	return errs
}

// modelDeploymentEngineProject names the project a row's floor is a release of, which both refusals
// below state beside the number. On SGLang the engine is SGLang itself; on Ascend with vLLM the
// engine is the vllm-ascend plugin and the version is one of its tags; on every other admitted
// platform it is an upstream vLLM release. Naming the wrong project would send a user after tags that
// do not carry the connector.
//
// THE ENGINE IS ANSWERED FIRST, because an Ascend SGLang row exists and its floor is an SGLang
// release: reading the platform first would send that user after vllm-ascend tags.
func modelDeploymentEngineProject(row workerctrl.ModelDeploymentEPDRow) string {
	if row.Engine == workercore.ModelDeploymentEngineSGLang {
		return workercore.ModelDeploymentEngineSGLang
	}

	if row.Platform == nodefeature.ManufacturerAscend {
		return string(inject.EngineVLLMAscend)
	}

	return "vLLM"
}

// validateModelDeploymentEPDEngineVersion refuses an engine version this row cannot run.
func validateModelDeploymentEPDEngineVersion(
	md *workercore.ModelDeployment, row workerctrl.ModelDeploymentEPDRow,
) field.ErrorList {
	versionPath := field.NewPath("spec", "engine", "version")
	project := modelDeploymentEngineProject(row)

	if md.Spec.Engine.Version == "" {
		return field.ErrorList{field.Required(versionPath, fmt.Sprintf(
			"an encoder role needs the engine version to be set: %s first appears in %s %s, and a "+
				"version this object does not name cannot be compared against that floor",
			row.Connector, project, row.EngineFloor))}
	}

	if modelDeploymentMeetsFloor(md.Spec.Engine.Version, row.EngineFloor) {
		return nil
	}

	return field.ErrorList{field.Invalid(versionPath, md.Spec.Engine.Version, fmt.Sprintf(
		"%s carries %s from %s %s, so this deployment needs that version or later; %s is below it",
		row.Connector, row.Platform, project, row.EngineFloor, md.Spec.Engine.Version))}
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
	md *workercore.ModelDeployment, row workerctrl.ModelDeploymentEPDRow,
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
			encode.Name, row.Platform, row.Connector)))
	}

	if encodeMount.ReadOnly {
		errs = append(errs, field.Forbidden(encodePath.Child("readOnly"), fmt.Sprintf(
			"role %q writes the embeddings %s carries, so its mount must not be read-only: the "+
				"encoder's write would fail once the Pod is running, with nothing in the object to say "+
				"so", encode.Name, row.Connector)))
	}

	prefillPath := rolesPath.Index(prefillIdx).Child("additionalVolumes")
	prefillMount, ok := modelDeploymentRoleClaimMount(&md.Spec.Roles[prefillIdx])
	if !ok {
		return append(errs, field.Required(prefillPath, fmt.Sprintf(
			"role %q reads the embeddings %s carries out of the directory role %q writes them to, "+
				"and mounts nothing: attach the same claim role %q mounts",
			md.Spec.Roles[prefillIdx].Name, row.Connector, encode.Name, encode.Name)))
	}

	for _, mismatch := range []struct {
		// name is the field the two roles have to spell the same way.
		name string
		// value is what the encoder's own entry carries there.
		value string
		// equal reports whether the two entries agree on it.
		equal bool
	}{
		{
			"claimName", encodeMount.PersistentVolumeClaim.ClaimName,
			encodeMount.PersistentVolumeClaim.ClaimName == prefillMount.PersistentVolumeClaim.ClaimName,
		},
		{"subPath", encodeMount.SubPath, encodeMount.SubPath == prefillMount.SubPath},
		{"mountPath", encodeMount.MountPath, encodeMount.MountPath == prefillMount.MountPath},
	} {
		if mismatch.equal {
			continue
		}

		errs = append(errs, field.Invalid(
			modelDeploymentEPDVolumeMismatchField(encodePath, mismatch.name), mismatch.value, fmt.Sprintf(
				"role %q and role %q must name the same %s for %s to carry the encoder's output between "+
					"them, and they do not",
				encode.Name, md.Spec.Roles[prefillIdx].Name, mismatch.name, row.Connector)))
	}

	return errs
}

// modelDeploymentEPDVolumeMismatchField returns the field a shared-mount mismatch is reported on.
//
// THE FIELD THE USER MUST FIX IS NOT THE SAME FOR THE THREE MISMATCHES, so each one is looked up
// rather than named: the claim name lives under the source that carries it and the two mount fields
// sit beside it, exactly as the claim-source rule above spells them. Landing all three on one field
// sends an edit to a field that is already correct, and leaves the field that is wrong unmentioned.
func modelDeploymentEPDVolumeMismatchField(volumesPath *field.Path, name string) *field.Path {
	if name == "claimName" {
		return volumesPath.Child("persistentVolumeClaim", "claimName")
	}

	return volumesPath.Child(name)
}
