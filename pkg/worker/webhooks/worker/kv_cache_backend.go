package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	conregname "github.com/google/go-containerregistry/pkg/name"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/utils/ptr"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// MaxLeaderReplicas is the most leader processes a backend runs, and it is exported because the
// SCHEMA carries the same ceiling and a test holds the two equal.
//
// REQUIRED: raise both together. They catch different absences -- this one explains, the schema's
// still holds when the webhook is not installed -- and raising one alone yields the worst pairing:
// admission accepts what the schema then rejects, reporting an error against a field nobody got
// wrong. The test asserts EQUALITY rather than the value, so it survives the ceiling moving.
//
// The value is a guard against a misreading, not a property of the election: a Lease admits any
// number of candidates. Exactly one leader serves and the rest are standbys holding no data, so
// raising this buys spare processes and no capacity -- and the reading it is here to catch is the
// one where somebody sets it high expecting throughput.
const MaxLeaderReplicas = 5

// KVCacheBackendWebhook validates a v1alpha1.KVCacheBackend.
//
// It is validating only. Every default this API has — the backend type, the leader's replica count
// and allocation strategy, the transport protocol — is a CRD schema default, and every enum is a
// CRD schema enum. Neither needs a webhook, and for the enums a webhook could not help even if one
// were written: structural schema validation runs in rest.BeforeCreate and the validating admission
// chain runs after it, so a value outside an enum is refused before this handler is reached.
//
// What is left here is what a schema cannot express: a choice between two branches, a cross-object
// read, a collision between a derived flag and an escape hatch, and immutability.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="kvcachebackends",scope="Cluster"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type KVCacheBackendWebhook struct{}

func (r *KVCacheBackendWebhook) SetupWebhook(_ context.Context, _ webhook.SetupOptions) (runtime.Object, error) {
	// This handler holds no client. Every rule below is answered from the object itself or from the
	// settings cache, and a field nothing reads is one a later reader has to prove is unused.
	return &workercore.KVCacheBackend{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*KVCacheBackendWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*KVCacheBackendWebhook)(nil)
)

// ReceiveDeletionUpdate keeps this webhook validating updates to a backend that is being deleted. Its
// finalizer holds while status.usedBy is non-empty, so the branch choice and the disk tier stay
// frozen for as long as anything still consumes the backend rather than only while it is live.
//
// FORBIDDEN: reading the fallback-image setting on an update that leaves spec.image where it was.
// That setting is the only thing this handler reads beyond the two objects it is handed, and
// validateKVCacheBackendSpec gates it on the image having moved for the reason the guard exists: the
// reconciler removing this object's finalizer is an update, and refusing it would strand the object
// undeletable after teardown had already removed every workload it ran.
func (r *KVCacheBackendWebhook) ReceiveDeletionUpdate() {}

func (r *KVCacheBackendWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	kvcb := obj.(*workercore.KVCacheBackend)

	if errs := validateKVCacheBackendSpec(ctx, kvcb, nil, true); len(errs) > 0 {
		return nil, kerrors.NewInvalid(kvcb.GroupVersionKind().GroupKind(), kvcb.Name, errs)
	}

	return nil, nil
}

func (r *KVCacheBackendWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldKvcb, newKvcb := oldObj.(*workercore.KVCacheBackend), newObj.(*workercore.KVCacheBackend)

	errs := validateKVCacheBackendSpec(ctx, newKvcb, oldKvcb, oldKvcb.Spec.Image != newKvcb.Spec.Image)
	errs = append(errs, validateKVCacheBackendImmutable(oldKvcb, newKvcb)...)
	errs = append(errs, validateKVCacheBackendMultiTenancyWithdrawal(oldKvcb, newKvcb)...)
	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(newKvcb.GroupVersionKind().GroupKind(), newKvcb.Name, errs)
	}

	return nil, nil
}

func (r *KVCacheBackendWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Deletion is refused by the reconciler's finalizer while status.usedBy is non-empty, not
	// here: this handler sees only the object, while the decision needs the consumers it holds.
	return nil, nil
}

// validateKVCacheBackendSpec holds every rule that applies to a spec whether it arrived by create
// or by update.
//
// old is the object as it was, and nil on create. It exists for the same reason checkFallback does:
// a rule that re-judges something an update did not touch can strand an already-admitted object,
// and not every update is the user's.
//
// checkFallback asks whether this call must also prove the cluster-wide fallback image is still
// there. It is false for an update that leaves spec.image where it was.
//
// The fallback comes from an EDITABLE setting, so a rule that re-read it on every update would make
// an already-admitted object stop being updatable the moment an admin clears that setting — and not
// every update is the user's. The reconciler removing this object's finalizer is one, so refusing it
// would strand the object undeletable forever, after teardown had already removed every workload it
// ran. Whether the object NAMES a usable image is still checked on every call; only the question
// about external state is scoped to the updates that could have changed the answer.
func validateKVCacheBackendSpec(
	ctx context.Context, kvcb, old *workercore.KVCacheBackend, checkFallback bool,
) field.ErrorList {
	specPath := field.NewPath("spec")

	errs := validateKVCacheBackendName(kvcb)
	errs = append(errs, validateKVCacheBackendImage(ctx, kvcb, checkFallback, specPath.Child("image"))...)
	errs = append(errs, validateKVCacheBackendPullSecrets(
		kvcb.Spec.ImagePullSecrets, specPath.Child("imagePullSecrets"))...)
	var oldSpec *workercore.KVCacheBackendSpec
	if old != nil {
		oldSpec = &old.Spec
	}
	errs = append(errs, validateKVCacheBackendConnection(
		&kvcb.Spec, oldSpec, specPath.Child("connection"))...)

	return errs
}

// validateKVCacheBackendPullSecrets refuses a secret reference no Secret could ever satisfy.
//
// LocalObjectReference's name is optional for backwards compatibility, so the generated schema takes
// an entry of `{}` — and Pod validation takes it too, checking only that the name carries no
// surrounding whitespace. The reference is copied into both rendered Pod specs and fails at image
// pull, which surfaces as ImagePullBackOff on a node rather than as anything on this object.
//
// The shape is the one validateInstanceVolume already uses on its own references, and for the same
// reason: a name no Secret could carry can never resolve, unlike one that merely does not exist yet.
func validateKVCacheBackendPullSecrets(
	refs []core.LocalObjectReference, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for i := range refs {
		namePath := fldPath.Index(i).Child("name")
		if refs[i].Name == "" {
			errs = append(errs, field.Required(namePath,
				"name of the referenced secret must be specified"))
			continue
		}
		if msgs := validation.IsDNS1123Subdomain(refs[i].Name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(namePath, refs[i].Name, strings.Join(msgs, "; ")))
		}
	}

	return errs
}

// validateKVCacheBackendName refuses a name whose rendered objects could not carry it.
//
// A KVCacheBackend's own name is a DNS SUBDOMAIN — up to 253 characters, and dots are legal. What is
// rendered from it is stricter: the leader's Service name has to be a DNS-1035 LABEL, which allows
// no dots and caps at 63 including the "-leader" suffix, and the name also travels into
// app.kubernetes.io/instance, a label value capped at 63. A name that clears its own rule and fails
// the children's is admitted, and then fails inside every reconcile with nothing to show for it but
// a create error in a log.
//
// It checks the names the renderers actually produce rather than restating their limits here, so a
// change to a suffix cannot drift away from the rule that guards it. Only a managed backend is
// checked: an external one renders nothing, so nothing is derived from its name.
func validateKVCacheBackendName(kvcb *workercore.KVCacheBackend) field.ErrorList {
	managed := kvcb.Spec.Connection.Managed
	if managed == nil {
		return nil
	}

	rendered := []string{mooncake.LeaderObjectName(kvcb)}
	for i := range managed.Members {
		rendered = append(rendered, mooncake.MemberObjectName(kvcb, i))
	}

	for _, name := range rendered {
		msgs := validation.IsDNS1035Label(name)
		if len(msgs) == 0 {
			continue
		}
		// One error, not one per rendered object: they all fail for the same reason and the fix is
		// the same rename.
		return field.ErrorList{field.Invalid(field.NewPath("metadata", "name"), kvcb.Name,
			fmt.Sprintf("this backend renders an object named %q, which is not a valid name for "+
				"one: %s", name, strings.Join(msgs, "; ")))}
	}

	return nil
}

// validateKVCacheBackendImage refuses a backend that names no image anywhere. The field is optional
// because the cluster-wide setting is the better place to pin a verified version; it is not optional
// for both to be empty, because then nothing decides what runs.
//
// Only a MANAGED backend is asked, for the same reason the name rule is: an external one runs
// somebody else's deployment, so the reconciler resolves no image for it and there is nothing this
// could be refusing on behalf of.
//
// The setting now ships a default, so the refusal below does not fire on a stock install. It is not
// dead: the setting allows blank, and a cluster that clears it is asking for exactly this refusal --
// every backend must then name its own image. Do not remove this on the grounds that the default
// makes it unreachable; the default is what an administrator is allowed to take away.
func validateKVCacheBackendImage(
	ctx context.Context, kvcb *workercore.KVCacheBackend, checkFallback bool, fldPath *field.Path,
) field.ErrorList {
	if kvcb.Spec.Connection.Managed == nil {
		return nil
	}

	switch image := strings.TrimSpace(kvcb.Spec.Image); {
	case image != "":
		return checkImageReference(image, fldPath)

	case kvcb.Spec.Image != "":
		// Set to nothing but blanks. Refused rather than ignored: ignoring it would fall through to
		// the setting and run an image this object does not name, and naming one is the whole point
		// of the field — silently declining the override is the one answer nobody asked for.
		return field.ErrorList{field.Invalid(fldPath, kvcb.Spec.Image,
			"must not be blank: an image is either named here or left out entirely so the "+
				`"kv-cache-backend-image" setting decides`)}
	}

	if !checkFallback {
		// This update did not move spec.image, so it cannot have changed whether a fallback is
		// needed. Re-asking would put an editable setting in the path of every update to an object
		// that was admitted long before it was cleared.
		return nil
	}

	// Only read the setting when the object does not carry an image, so a backend that names its
	// own is admitted whatever state the setting is in.
	//
	// ShouldValue, not Value: Value reports "the setting was never written" as an ERROR while still
	// returning the default, so treating its error as a failure would refuse the common case with a
	// message about an unreadable setting instead of the one an operator can act on. Both of its
	// failure modes converge here anyway — without a non-blank image the object cannot be admitted,
	// and the fix is the same sentence either way. A transport failure that clears later simply lets
	// the same object through on the next attempt, which is the level-based behavior we want.
	if strings.TrimSpace(settings.KVCacheBackendImage.ShouldValue(ctx)) == "" {
		return field.ErrorList{field.Required(fldPath, fmt.Sprintf(
			"an image must be named either here or in the %q setting, and neither carries one",
			"kv-cache-backend-image"))}
	}

	return nil
}

// validateKVCacheBackendConnection enforces the branch choice and everything inside the branch that
// was taken.
func validateKVCacheBackendConnection(
	spec, oldSpec *workercore.KVCacheBackendSpec, fldPath *field.Path,
) field.ErrorList {
	managed, external := spec.Connection.Managed, spec.Connection.External

	switch {
	case managed == nil && external == nil:
		return field.ErrorList{field.Required(fldPath,
			`one of "managed" or "external" must be set: a backend with neither describes nothing`)}
	case managed != nil && external != nil:
		return field.ErrorList{field.Forbidden(fldPath,
			`only one of "managed" or "external" may be set: a backend with both describes two`)}
	case external != nil:
		return validateKVCacheBackendExternal(external, fldPath.Child("external"))
	default:
		var oldManaged *workercore.KVCacheBackendManaged
		if oldSpec != nil {
			oldManaged = oldSpec.Connection.Managed
		}
		return validateKVCacheBackendManaged(managed, oldManaged, fldPath.Child("managed"))
	}
}

// validateKVCacheBackendExternal requires both endpoint roles. The schema keys the list by name and
// so already refuses a duplicate role; what it cannot say is that a missing role leaves a reader
// with nothing to point at.
func validateKVCacheBackendExternal(
	external *workercore.KVCacheBackendExternal, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	endpointsPath := fldPath.Child("endpoints")

	// Each address is checked as well as counted. The schema types it as a bounded string, which
	// cannot say host:port — so a blank one is structurally valid, is published into status
	// unchanged, and reaches an engine as an address it cannot dial. A backend whose Admin entry
	// answers would even read Ready while doing it.
	for i := range external.Endpoints {
		endpoint := &external.Endpoints[i]
		if err := checkHostPort(endpoint.Address); err != nil {
			errs = append(errs, field.Invalid(endpointsPath.Index(i).Child("address"),
				endpoint.Address, fmt.Sprintf(
					"must be host:port, and this operator dials it as given: %v", err)))
		}
	}

	for _, role := range []string{
		workercore.KVCacheBackendEndpointNameClient,
		workercore.KVCacheBackendEndpointNameAdmin,
	} {
		if slices.ContainsFunc(external.Endpoints, func(e workercore.KVCacheBackendEndpoint) bool {
			return e.Name == role
		}) {
			continue
		}
		errs = append(errs, field.Required(endpointsPath, fmt.Sprintf(
			"an entry named %q is required: this operator reads the %q address and publishes the %q one, "+
				"so an external backend naming only one leaves either the scrape or every engine "+
				"with no address",
			role, workercore.KVCacheBackendEndpointNameAdmin,
			workercore.KVCacheBackendEndpointNameClient)))
	}

	return errs
}

// checkImageReference refuses a string a container runtime could not resolve.
//
// Kubernetes takes any non-empty image on a Pod, so "not a valid image" is admitted here, rendered
// verbatim, and fails in the kubelet as ImagePullBackOff with reason InvalidImageName — a fault
// reported per Pod, on a node, long after the object that caused it was accepted. The same parser
// the container-image Settings use answers the question at admission instead.
func checkImageReference(image string, fldPath *field.Path) field.ErrorList {
	if _, err := conregname.ParseReference(image); err != nil {
		return field.ErrorList{field.Invalid(fldPath, image,
			fmt.Sprintf("is not a container image reference: %v", err))}
	}
	return nil
}

// checkHostPort reports why an address is not one this operator could dial.
//
// It is deliberately shallow: it establishes that a host and a port are both present, that the port
// is a port, and that the whole thing survives being turned into the URL the admin client builds.
// Whether the host resolves and whether anything answers are questions for the reconciler, which
// reports them as conditions — admission refuses only what could never work.
func checkHostPort(address string) error {
	// Refused, not trimmed. Nothing here can WRITE to the object — a validating webhook returns a
	// verdict — so normalising for the check would have admitted the untrimmed value and left it in
	// the spec, from where it is copied verbatim into status and handed to the admin client.
	// Measured: http.NewRequest on "http:// leader.example:9003 /health" fails with an invalid
	// port, on every observation, for the life of the object.
	if address != strings.TrimSpace(address) {
		return errors.New("must not begin or end with a space: the address is stored and dialed " +
			"exactly as written here")
	}

	host, port, err := net.SplitHostPort(address)
	switch {
	case err != nil:
		return err
	case host == "":
		return errors.New("no host")
	}

	number, err := strconv.Atoi(port)
	if err != nil {
		return fmt.Errorf("port %q is not a number", port)
	}
	if number < 1 || number > 65535 {
		return fmt.Errorf("port %d is outside 1-65535", number)
	}

	// SplitHostPort is not enough on its own. It splits on the last colon and asks nothing about
	// what it split, so "bad host:9003" parses cleanly and then fails where it is actually used —
	// url.Parse rejects the space in the host. Admission has to ask the question the caller will
	// ask, or it admits an address guaranteed to fail on every scrape.
	parsed, err := url.Parse("http://" + address + "/")
	if err != nil {
		return fmt.Errorf("cannot be used as an address: %w", err)
	}

	// And parsing is not enough either, because url.Parse REINTERPRETS rather than refuses. Every
	// one of these was measured to reach a running client as something other than what was
	// written: "bad/path:9003" dials host "bad" on port 80 with the rest as a path, "a?b:9003" and
	// "a#b:9003" the same with a query and a fragment, and "user@host:9003" hands the leading
	// segment over as credentials and dials the remainder.
	//
	// One comparison covers all four, and userinfo needs no check of its own: a parsed authority
	// never carries it, so an address that has any differs from the authority by exactly that much.
	if parsed.Host != address {
		return fmt.Errorf("is read as host %q rather than as the address itself", parsed.Host)
	}

	return nil
}

// validateKVCacheBackendManaged enforces the two scope limits and the escape-hatch rules.
//
// oldManaged is nil on create. It is used for one thing: skipping the escape-hatch rules over a
// list no update touched. See unchangedPassthrough.
func validateKVCacheBackendManaged(
	managed, oldManaged *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	if replicas := managed.Leader.Replicas; replicas != nil {
		if *replicas > MaxLeaderReplicas {
			errs = append(errs, field.Invalid(fldPath.Child("leader", "replicas"), *replicas,
				fmt.Sprintf("at most %d is supported: only one leader serves at a time and the rest "+
					"are standbys, so more of them adds spare processes rather than capacity",
					MaxLeaderReplicas)))
		}

		// The pairing rule, and it names the field that is MISSING rather than the one that is set.
		// Several leaders with nothing electing between them is not a degraded configuration: each
		// one serves, and the members register with whichever they were told about.
		if *replicas > 1 && managed.Leader.HighAvailability == nil {
			errs = append(errs, field.Invalid(fldPath.Child("leader", "replicas"), *replicas,
				"more than one leader requires leader.highAvailability, which elects one of them "+
					"through a Kubernetes Lease; without it every replica would serve"))
		}
	}

	// A snapshot is refused at any replica count, because restoring one can make the cache serve
	// WRONG DATA rather than miss. The snapshot records where each key sits in member memory, and
	// nothing checks that memory still holds that key when the index is read back: a forced remove
	// (the path an engine's cache reset takes) frees it for the next write, and a standby loads the
	// snapshot once at its own start, so by the time it takes over another leader may have reused it.
	//
	// An update is judged only when it moves the snapshot or the replica count. An object admitted
	// before this rule keeps running as it was rendered, and it still has to take an unrelated edit
	// and the reconciler's removal of its finalizer -- see unchangedPassthrough for that failure.
	if snapshot := mooncake.LeaderSnapshot(managed.Leader); snapshot != nil {
		moved := oldManaged == nil ||
			!equality.Semantic.DeepEqual(mooncake.LeaderSnapshot(oldManaged.Leader), snapshot) ||
			!ptr.Equal(oldManaged.Leader.Replicas, managed.Leader.Replicas)
		if moved {
			errs = append(errs, field.Forbidden(fldPath.Child("leader", "highAvailability", "snapshot"),
				"snapshots are not supported: a leader restoring one can serve another key's bytes "+
					"instead of a miss, because the member memory the snapshot points at may have been "+
					"reused since it was taken -- after a forced remove such as an engine's cache reset, "+
					"or before a standby that loaded it at its own start takes over"))
		}
	}

	// REQUIRED: an update that switches high availability on or off re-runs these rules even over a
	// list it did not touch, and the exemption below is what makes that necessary. Turning the field
	// on is what turns `enable_ha`, `ha_backend_type`, `ha_backend_connstring` and `cluster_id` into
	// DERIVED flags; an object admitted before they were derived may carry one, and the renderer
	// appends the escape hatch AFTER the derived flags, so `-enable_ha=false` left in the list would
	// win over the `-enable_ha=true` the election needs -- several unelected masters, admitted by a
	// rule that only ever looked at whether the list moved.
	// THE SNAPSHOT DECLARATION IS COUPLED THE SAME WAY, and for the same reason one step down: the
	// snapshot keys are reserved unconditionally, but an object admitted BEFORE they were reserved
	// can carry one, and it carries it inside an already-present high-availability block. Comparing
	// only whether that block appeared or vanished leaves such an update reading as unchanged, the
	// rules are skipped, and the stale `-enable_snapshot_restore=false` the hatch appends after the
	// derived flags wins over the declaration that was just added.
	var oldLeaderExtraArgs []string
	haUnchanged := true
	if oldManaged != nil {
		oldLeaderExtraArgs = oldManaged.Leader.ExtraArgs
		haUnchanged = (oldManaged.Leader.HighAvailability == nil) ==
			(managed.Leader.HighAvailability == nil) &&
			leaderSnapshotDeclared(oldManaged.Leader.HighAvailability) ==
				leaderSnapshotDeclared(managed.Leader.HighAvailability)
	}
	if !haUnchanged ||
		!unchangedPassthrough(oldManaged != nil, oldLeaderExtraArgs, managed.Leader.ExtraArgs) {
		errs = append(errs, validateExtraArgs(managed.Leader.ExtraArgs,
			mooncake.LeaderExtraArgsRules, fldPath.Child("leader", "extraArgs"))...)
	}

	// The leader's environment hatch is the same rules the member side of this validator enforces,
	// against the leader's own derived names, and it is COUPLED TO THE SAME DECLARATIONS as the
	// argument hatch above rather than to the passthrough alone.
	//
	// The reserved list being unconditional is what makes the coupling necessary, not what makes it
	// unnecessary. What a declaration moves is which of those names the renderer EMITS: the snapshot
	// path variable is emitted only under a snapshot declaration, and the Pod IP only under high
	// availability. A list carrying one of those names from before it was reserved is exempted by
	// the passthrough comparison, and the renderer appends the hatch AFTER the derived variables --
	// so the update that turns the declaration on is the moment the stale value starts overriding
	// the mount path the claim arrives at, and it is the one update this rule must not skip.
	var oldLeaderExtraEnv []workercore.InstanceEnvVar
	if oldManaged != nil {
		oldLeaderExtraEnv = oldManaged.Leader.ExtraEnv
	}
	if !haUnchanged ||
		!unchangedPassthrough(oldManaged != nil, oldLeaderExtraEnv, managed.Leader.ExtraEnv) {
		errs = append(errs, validateExtraEnvs(managed.Leader.ExtraEnv,
			mooncake.LeaderDerivedEnvs, fldPath.Child("leader", "extraEnv"))...)
	}

	for i := range managed.Members {
		var oldMember *workercore.KVCacheBackendMember
		if oldManaged != nil && i < len(oldManaged.Members) {
			oldMember = &oldManaged.Members[i]
		}
		errs = append(errs, validateKVCacheBackendMember(&managed.Members[i], oldMember,
			fldPath.Child("members").Index(i))...)
	}

	errs = append(errs, validateKVCacheBackendLocalDiskUniqueness(managed, fldPath)...)
	errs = append(errs, validateKVCacheBackendSnapshot(managed, fldPath)...)
	errs = append(errs, validateKVCacheBackendScaleIn(managed, fldPath.Child("scaleIn"))...)

	return errs
}

// validateKVCacheBackendSnapshot refuses a snapshot that names no storage to keep itself on.
//
// The schema already requires the field and bounds its length. What it cannot say is that the value
// has to be a name Kubernetes can resolve to a claim: an unresolvable one renders a volume the
// kubelet refuses, so every leader replica stays pending with the reason on a Pod rather than on the
// object somebody edited.
//
// It deliberately does NOT check the claim's ACCESS MODES, which is the requirement that actually
// decides whether the feature works. That needs a read this handler does not have, against an object
// that legitimately does not exist yet when the backend is created — claims are usually applied
// alongside it. The reconciler asks once it can see the claim and publishes the answer as a
// condition, so the check is late rather than absent.
func validateKVCacheBackendSnapshot(
	managed *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	snapshot := mooncake.LeaderSnapshot(managed.Leader)
	if snapshot == nil {
		return nil
	}

	claimPath := fldPath.Child("leader", "highAvailability", "snapshot",
		"persistentVolumeClaimName")

	// Reached when the schema is not the one enforcing this — a cluster whose CRD predates the
	// field's minLength, or an object arriving through a path that skipped structural validation.
	// The message names what the storage is FOR, because a reader who left it blank is usually one
	// who took the snapshot block for a switch.
	if snapshot.PersistentVolumeClaimName == "" {
		return field.ErrorList{field.Required(claimPath,
			"a snapshot has to name the claim it is kept on: the replica that serves writes the "+
				"snapshot and a standby reads it back, so it cannot live inside either pod")}
	}

	if msgs := validation.IsDNS1123Subdomain(snapshot.PersistentVolumeClaimName); len(msgs) > 0 {
		return field.ErrorList{field.Invalid(claimPath, snapshot.PersistentVolumeClaimName,
			strings.Join(msgs, "; "))}
	}

	return nil
}

// validateKVCacheBackendLocalDiskUniqueness enforces that only one member group declares a disk
// tier.
//
// The reason is the capacity contract rather than any one renderer's reach, and it is the SAME
// sentence that bounds the list itself to one entry: status.capacity is a single pair of figures
// for the whole backend and cannot attribute a tier's bytes to one disk, so two tiers would
// describe neither. Two gates carry it, one per level — the list's own entry bound, and this rule
// across groups — and that is why they move together with that status shape or not at all.
//
// The rule lives in the webhook rather than in the schema because it is a rule about a PAIR of
// positions: a schema can say a value is wrong, only a webhook can say that two groups may not
// each carry one.
func validateKVCacheBackendLocalDiskUniqueness(
	managed *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	var withDisk []int
	for i := range managed.Members {
		if len(managed.Members[i].LocalDisks) > 0 {
			withDisk = append(withDisk, i)
		}
	}

	// One disk tier per backend, and the reason is the capacity contract rather than the
	// rendering: status.capacity is a single pair of figures for the whole backend, and it reports
	// the disk tier by ADDING the leader's file family to its memory one. Two tiers would land in
	// that one family with no way to tell them apart, so the object would describe neither.
	//
	// It ALSO happens to be the only thing refusing two groups that name the same host directory,
	// which would run two stores over one tier wherever their selectors meet. That is not this
	// rule's reason, so relaxing it for a capacity reason would let the other case through
	// silently — the case pinning it is named for the collision rather than for the attribution.
	if len(withDisk) > 1 {
		return field.ErrorList{field.Forbidden(
			fldPath.Child("members").Index(withDisk[1]).Child("localDisks"),
			fmt.Sprintf("only one member group may declare localDisks, and groups %v do: "+
				"the leader reports every disk tier through one pair of gauges, so status.capacity "+
				"could not say which figure belonged to which group", withDisk))}
	}

	return nil
}

// validateKVCacheBackendScaleIn bounds the grace a departing member waits for.
//
// Both bounds refuse a value that would make the shutdown hook fail rather than wait, and a failing
// preStop is recorded as an event and otherwise ignored — so the hook would look configured while
// draining nothing.
func validateKVCacheBackendScaleIn(
	managed *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	scaleIn := managed.ScaleIn
	if scaleIn == nil {
		return nil
	}

	var errs field.ErrorList
	gracePath := fldPath.Child("gracePeriodSeconds")

	// The schema bounds this too, and the duplication is on purpose: the schema's message names a
	// number, this one names the reason. A reader who hits the schema's bound first has still been
	// stopped from shipping a hook that fails on every shutdown.
	//
	// LIMITED: no grace is both above the ceiling and negative, so only one bound can fire and
	// either could return early unobserved. They accumulate so that moving a bound later — a floor
	// above zero, a ceiling that follows the endpoint's — reports both violations rather than the
	// first alone. No test covers that: the aggregation case in this package's tests passes either
	// way, so this note is what has to stop the next edit.
	if scaleIn.GracePeriodSeconds > mooncake.MemberMaxGracePeriodSeconds {
		errs = append(errs, field.Invalid(gracePath, scaleIn.GracePeriodSeconds, fmt.Sprintf(
			"must not exceed %d: the member's own endpoint refuses a larger grace with a 400, so "+
				"this would render a shutdown hook that fails every time it runs",
			mooncake.MemberMaxGracePeriodSeconds)))
	}
	if scaleIn.GracePeriodSeconds < 0 {
		errs = append(errs, field.Invalid(gracePath, scaleIn.GracePeriodSeconds,
			"must not be negative"))
	}

	// A grace on a backend with no disk tier is not refused. It is inert — there is nothing to
	// deregister and no hook is rendered — but refusing it would make declaring a scale-in policy
	// depend on the order in which two independent fields are edited, and an operator adding the
	// tier next would have to remove and re-add the grace around it.

	return errs
}

const quantityTooLarge = "must not exceed 9223372036854775807 (2^63-1) bytes: the renderer reads " +
	"this as a signed 64-bit count, and a larger one does not survive the conversion"

// memberBucketSize is the renderer's bucket size as a Quantity, so the three bounds below compare
// against the figure that is actually sent rather than against a copy of it, and print it the way
// the field they guard is written.
var memberBucketSize = *resource.NewQuantity(mooncake.MemberBucketSizeLimit, resource.BinarySI)

// validateKVCacheBackendMember holds the per-group rules a schema cannot carry: a device
// resource named on a group with no device to charge, and two quantities whose schema type is a
// string.
func validateKVCacheBackendMember(
	member, oldMember *workercore.KVCacheBackendMember, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	// There is no per-medium rule here any more, and its absence is deliberate: the schema now
	// enumerates the one value, so a medium this API does not render is refused before this handler
	// runs and a rule for it would be code no request can reach.

	// One tier per group is the whole list today, and the rules below read it by position for the
	// same reason the immutability ones do: the position identifies a group everywhere else, so it
	// identifies the entry here too. The schema keys the list by path and refuses a duplicate, so
	// two entries naming one directory never reach this loop.
	for i := range member.LocalDisks {
		var oldDisk *workercore.KVCacheBackendMemberLocalDisk
		if oldMember != nil && i < len(oldMember.LocalDisks) {
			oldDisk = &oldMember.LocalDisks[i]
		}
		errs = append(errs, validateKVCacheBackendLocalDisk(&member.LocalDisks[i], oldDisk,
			fldPath.Child("localDisks").Index(i))...)
	}

	errs = append(errs, validateKVCacheBackendMemberHostPaths(member, fldPath.Child("hostPaths"))...)

	// A resource.Quantity is a STRING in the schema, so no numeric bound in a marker can reach it —
	// these two are the only place either can be refused. Zero is refused rather than defaulted,
	// because the renderer omits the segment size it derives when the value is not positive, and a
	// member that mounts no segment is indistinguishable from one whose leader lost it.
	switch {
	case member.CapacityPerMember.CmpInt64(0) <= 0:
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(),
			"must be greater than 0: a member contributing nothing is a Pod with no reason to run"))
	case quantityx.OverflowsInt64(member.CapacityPerMember):
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(), quantityTooLarge))
	case len(member.LocalDisks) > 0 &&
		member.CapacityPerMember.CmpInt64(mooncake.MemberBucketSizeLimit) < 0 &&
		(oldMember == nil || member.CapacityPerMember.Cmp(oldMember.CapacityPerMember) != 0):
		// The floor applies only to a group that declares a tier, and only when this update moved
		// the value. Re-judging a figure the update left alone is what would strand an object
		// admitted before the bound existed — and not every update is the user's.
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(), fmt.Sprintf(
				"must be at least %s for a group declaring localDisks: that is one bucket, the unit the "+
					"tier is written in, and its bytes are held in this segment until the bucket is "+
					"complete — so a smaller segment leaves the tier empty under every workload, with "+
					"nothing reporting it", memberBucketSize.String())))
	}
	if member.LocalBufferSize.CmpInt64(0) < 0 {
		errs = append(errs, field.Invalid(fldPath.Child("localBufferSize"),
			member.LocalBufferSize.String(),
			"must not be negative: it is added to the member Pod's memory request"))
	} else if quantityx.OverflowsInt64(member.LocalBufferSize) {
		errs = append(errs, field.Invalid(fldPath.Child("localBufferSize"),
			member.LocalBufferSize.String(), quantityTooLarge))
	}

	// The selector is copied into the DaemonSet's pod template verbatim, and the API server refuses
	// a template whose node selector is not made of labels. That refusal arrives as a create error
	// inside a reconcile, where the only trace of it is a line in this operator's log — so the
	// group would simply never come up, with nothing on the object to say why.
	for _, key := range slices.Sorted(maps.Keys(member.NodeSelector)) {
		selectorPath := fldPath.Child("nodeSelector").Key(key)
		if msgs := validation.IsQualifiedName(key); len(msgs) > 0 {
			errs = append(errs, field.Invalid(selectorPath, key,
				"is not a label key: "+strings.Join(msgs, "; ")))
		}
		if msgs := validation.IsValidLabelValue(member.NodeSelector[key]); len(msgs) > 0 {
			errs = append(errs, field.Invalid(selectorPath, member.NodeSelector[key],
				"is not a label value: "+strings.Join(msgs, "; ")))
		}
	}

	// The member renderer uses this override verbatim, without the trim the backend-level image
	// gets, so blanks would reach the container runtime as an image reference.
	switch image := strings.TrimSpace(member.Image); {
	case member.Image == "":
	case image == "":
		errs = append(errs, field.Invalid(fldPath.Child("image"), member.Image,
			"must not be blank: an image is either named here or left out so the backend's decides"))
	default:
		errs = append(errs, checkImageReference(member.Image, fldPath.Child("image"))...)
	}

	var oldExtraArgs []string
	var oldExtraEnv []workercore.InstanceEnvVar
	if oldMember != nil {
		oldExtraArgs, oldExtraEnv = oldMember.ExtraArgs, oldMember.ExtraEnv
	}
	if !unchangedPassthrough(oldMember != nil, oldExtraArgs, member.ExtraArgs) {
		errs = append(errs, validateExtraArgs(member.ExtraArgs,
			mooncake.MemberExtraArgsRules, fldPath.Child("extraArgs"))...)
	}
	if !unchangedPassthrough(oldMember != nil, oldExtraEnv, member.ExtraEnv) {
		errs = append(errs, validateExtraEnvs(member.ExtraEnv,
			mooncake.MemberDerivedEnvs, fldPath.Child("extraEnv"))...)
	}

	return errs
}

// pathsOverlap reports whether two mount paths are the same path or one contains the other.
//
// CONTAINMENT and not just equality, because a declared /dev would be mounted over the device nodes
// a host fabric's plugin injects under /dev/infiniband, and which of the two the container sees is
// decided by the container runtime rather than by anything here. A rule that depends on an ordering
// nobody in this repository verified is not a rule, so the overlap is refused instead and the
// question never arises.
//
// Compared by path ELEMENT, so /dev/infiniband2 does not read as being under /dev/infiniband the way
// a plain string prefix would have it. Both sides are cleaned first: the schema requires an absolute
// path with no empty element, which leaves a trailing slash as the one difference Clean still has to
// remove.
func pathsOverlap(a, b string) bool {
	a, b = filepath.Clean(a), filepath.Clean(b)
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// validateKVCacheBackendMemberHostPaths keeps a group's declared mounts from landing on each other
// or on one the renderer owns.
//
// Kubernetes accepts a container carrying two mounts at one path — or at two paths where one
// contains the other — and leaves the outcome to the runtime, so no collision here is reported
// anywhere: the member starts, one of the mounts is simply not what reads it finds. That is the
// whole reason these are refused here rather than left to the Pod, and it is why EVERY comparison
// below is by overlap rather than by equality.
//
// The device tree's path is refused UNCONDITIONALLY, including on a group whose protocol is granted
// no device today: the protocol is a field an update may change while a mount path is judged only
// when it is written, so admitting it under tcp would leave a collision that arrives on the day
// somebody edits an unrelated field. The disk tier's path is judged against the group's CURRENT
// localDisks instead, because that path is itself declared right there — a group that moves its
// tier is re-judged against the new one on the same update.
func validateKVCacheBackendMemberHostPaths(
	member *workercore.KVCacheBackendMember, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for i := range member.HostPaths {
		mountPath := member.HostPaths[i].MountPath
		mountPathPath := fldPath.Index(i).Child("mountPath")

		// Against the entries BEFORE this one, and by overlap rather than by equality: two declared
		// mounts where one contains the other are ordered by their position in this list, and which
		// of the two the container ends up seeing is the runtime's to decide — the same reason the
		// renderer-owned paths below are judged by overlap. Comparing backwards reports the pair
		// once, on the later entry, instead of once from each side.
		if prior := slices.IndexFunc(member.HostPaths[:i], func(e workercore.KVCacheBackendMemberHostPath) bool {
			return pathsOverlap(mountPath, e.MountPath)
		}); prior >= 0 {
			errs = append(errs, field.Invalid(mountPathPath, mountPath, fmt.Sprintf(
				"overlaps the mount path of hostPaths[%d] (%s): a container carrying overlapping "+
					"paths leaves which mount wins to the runtime, and reports nothing either way",
				prior, member.HostPaths[prior].MountPath)))
			continue
		}

		// The device tree first, and unconditionally: the check is not about a tier at all, and a
		// group with none must meet it just the same.
		if pathsOverlap(mountPath, mooncake.RDMADevicePath) {
			errs = append(errs, field.Invalid(mountPathPath, mountPath,
				"overlaps where a host fabric's device plugin injects the granted device nodes ("+
					mooncake.RDMADevicePath+"), and is refused under every protocol because the "+
					"protocol is editable: declaring it here would collide with those nodes the "+
					"day the group becomes RDMA or EFA"))
			continue
		}

		// Then every declared tier, and not just the first: the loop is one line longer than
		// reading entry zero and stays correct the day the entry bound is lifted with the status
		// shape that justifies it.
		for _, disk := range member.LocalDisks {
			if pathsOverlap(mountPath, disk.Path) {
				errs = append(errs, field.Invalid(mountPathPath, mountPath,
					fmt.Sprintf("overlaps where this group's localDisks tier is mounted (%s): the "+
						"tier is a declared capacity with its own deregistration hook, so it is "+
						"reached through localDisks and not through a plain mount", disk.Path)))
			}
		}
	}

	return errs
}

// validateExtraEnvs enforces the environment passthrough's rules, against whichever side's derived
// names it is handed.
//
// It is separate from validateExtraArgs rather than sharing it, because the half that differs is the
// half that matters: a config key and an environment-variable name have different shapes, judged by
// different upstream rules, and a message naming the wrong one sends someone looking for a mistake
// they did not make. What they have in common is one list walk, which is smaller than the
// abstraction that would hold it.
//
// Its scoping matches validateExtraArgs for the same reason: the derived lists GROW as this API
// renders more, and re-judging a list an update did not touch would retroactively refuse an object
// admitted before the entry existed — on every update, including the reconciler's own removal of
// this object's finalizer. See unchangedPassthrough.
func validateExtraEnvs(
	extraEnv []workercore.InstanceEnvVar, derived []string, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for _, e := range extraEnv {
		// Checked before the derived list, because that list holds names and anything that is not a
		// name cannot collide with one while still reaching the container.
		//
		// The STRICT rule and not the relaxed one, deliberately. Kubernetes has two: the classic form
		// this calls, and a relaxed form accepting any printable ASCII but "=", which newer API
		// servers apply to a Pod's env. Judging by the strict one refuses a handful of names a new
		// enough cluster would have taken — and every name it accepts is accepted by an API server of
		// any age, which is the direction that cannot fail silently. The other way round, this webhook
		// would admit a name that the cluster's own Pod validation then refuses inside a reconcile,
		// where the only trace is a line in this operator's log while the group never comes up. No
		// setting this hatch exists to reach carries such a name.
		if msgs := validation.IsEnvVarName(e.Name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(fldPath.Key(e.Name), e.Name,
				"is not an environment variable name: "+strings.Join(msgs, "; ")))
			continue
		}
		if slices.Contains(derived, e.Name) {
			errs = append(errs, field.Forbidden(fldPath.Key(e.Name),
				"this variable is rendered from this spec, and two definitions of one name make the "+
					"rendered container ambiguous: Kubernetes accepts both and leaves the winner to "+
					"the container runtime, so nothing would report the collision"))
		}
	}

	return errs
}

// validateKVCacheBackendLocalDisk refuses a disk tier the node could not carry.
//
// The path becomes a hostPath mount, which is why it is checked here at all: the API server takes
// any string, and the kubelet's refusal of a bad one arrives inside a reconcile, where the only
// trace is a line in this operator's log while the group simply never comes up.
//
// It refuses "/" and NOT other sensitive host paths, and that asymmetry is deliberate rather than an
// unfinished blocklist. An administrator who writes /etc or /var is REQUESTING that directory, and
// this project does not infer privilege on an operator's behalf or refuse it on their behalf either
// — the same rule that keeps transport.protocol: Auto from promoting itself to RDMA. "/" is refused
// because it is the one value that cannot be a request: it mounts the node's entire filesystem into
// a third-party container, and no tier is served by it. A blocklist of "dangerous" paths would also
// be unclosable — every entry invites a reader to trust that what is missing from it is safe.
func validateKVCacheBackendLocalDisk(
	disk, oldDisk *workercore.KVCacheBackendMemberLocalDisk, fldPath *field.Path,
) field.ErrorList {
	if disk == nil {
		return nil
	}

	var errs field.ErrorList

	pathPath := fldPath.Child("path")
	switch path := strings.TrimSpace(disk.Path); {
	case path == "":
		errs = append(errs, field.Required(pathPath,
			"a directory on the node is required: it is mounted from the host, and this operator "+
				"picks no default because the wrong host directory fills a filesystem nothing in "+
				"Kubernetes accounts for"))
	case path != disk.Path:
		// Refused rather than trimmed: a validating webhook returns a verdict and cannot write, so
		// normalising here would admit the untrimmed value and mount it as written.
		//
		// The message says "whitespace" rather than "a space" because TrimSpace also removes tabs
		// and newlines, and a message narrower than the rule sends someone looking for a space that
		// is not there.
		errs = append(errs, field.Invalid(pathPath, disk.Path,
			"must not begin or end with whitespace: the path is mounted exactly as written here"))
	case !filepath.IsAbs(path):
		errs = append(errs, field.Invalid(pathPath, disk.Path,
			"must be an absolute path: it names a directory on the node, and a relative one is "+
				"refused by the kubelet inside a reconcile rather than here"))
	case hasParentDirComponent(path):
		// The store refuses these itself, statically, before it looks at the filesystem — so a
		// path admitted here would produce a member that never becomes ready, with the reason only
		// in a container log. Checked on the RAW components rather than after Clean, because Clean
		// resolves ".." away and would hide exactly what the store is looking for.
		errs = append(errs, field.Invalid(pathPath, disk.Path,
			`must not contain a ".." component: the store refuses a path with one as traversal, `+
				"before it checks whether the directory exists, so the member would start and "+
				"never mount its tier"))
	case filepath.Clean(path) == "/":
		errs = append(errs, field.Invalid(pathPath, disk.Path,
			"must not be the root directory: it would mount the node's whole filesystem into a "+
				"third-party container"))
	case pathOverlaps(filepath.Clean(path), mooncake.RDMADevicePath):
		// The tier and the device nodes a host fabric's plugin injects land in ONE container, so a
		// collision is resolved by the container runtime rather than reported here — one shadows
		// the other, and which one wins is not something this object records. Refused whatever
		// the transport is today, because the transport is editable: a tier that merely does not
		// collide yet would start colliding the moment someone switched the backend to a host
		// fabric. Neither host fabric makes the renderer mount anything of its own; an EFA member
		// takes its libfabric from the image.
		errs = append(errs, field.Invalid(pathPath, disk.Path, fmt.Sprintf(
			"must not overlap %s, where a host fabric's device plugin injects the granted device "+
				"nodes into the same container: the tier would shadow them or be shadowed, and "+
				"the transport can be switched to one after this path is set",
			mooncake.RDMADevicePath)))
	}

	// The capacity is a resource.Quantity, so it is a string in the schema and no marker can bound
	// it. Zero is legitimate and means "no ceiling of ours" — the store's own applies — which is
	// the same thing leaving the field out means. A positive value needs one full bucket: the store
	// stops taking offload work as soon as one more bucket would not fit under this ceiling, so a
	// tier below it never receives a key.
	switch {
	case disk.Capacity.CmpInt64(0) < 0:
		errs = append(errs, field.Invalid(fldPath.Child("capacity"), disk.Capacity.String(),
			"must not be negative: it caps what this tier stores"))
	case !disk.Capacity.IsZero() && disk.Capacity.Cmp(memberBucketSize) < 0 &&
		(oldDisk == nil || disk.Capacity.Cmp(oldDisk.Capacity) != 0):
		errs = append(errs, field.Invalid(fldPath.Child("capacity"), disk.Capacity.String(),
			fmt.Sprintf("must be at least %s, which is one bucket: the store stops taking offload "+
				"work as soon as one more bucket would not fit under this ceiling",
				memberBucketSize.String())))
	case quantityx.OverflowsInt64(disk.Capacity):
		errs = append(errs, field.Invalid(fldPath.Child("capacity"), disk.Capacity.String(),
			quantityTooLarge))
	}

	// The key ceiling's other half. The schema bounds it at zero and no lower, which is all a marker
	// can say; the bucket's worth below it is the same fact as the capacity floor above, on the
	// count the store checks in the same breath as the bytes.
	if disk.KeyLimit > 0 && disk.KeyLimit < mooncake.MemberBucketKeysLimit &&
		(oldDisk == nil || disk.KeyLimit != oldDisk.KeyLimit) {
		errs = append(errs, field.Invalid(fldPath.Child("keyLimit"), disk.KeyLimit,
			fmt.Sprintf("must be at least %d, which is one bucket's worth of keys: the store stops "+
				"taking offload work as soon as one more bucket would not fit under this ceiling",
				mooncake.MemberBucketKeysLimit)))
	}

	errs = append(errs, validateKVCacheBackendLocalDiskEviction(disk, fldPath)...)

	return errs
}

// validateKVCacheBackendLocalDiskEviction refuses an eviction block that would render settings the
// store accepts and then does not act on.
//
// Every rule here is a PAIR rule, which is why none of them is a schema marker: the enum, the
// percentage bounds and the default on enabled are all in the schema already, and what is left is
// what one field means in the presence of another.
func validateKVCacheBackendLocalDiskEviction(
	disk *workercore.KVCacheBackendMemberLocalDisk, diskPath *field.Path,
) field.ErrorList {
	eviction := disk.Eviction
	if eviction == nil {
		return nil
	}

	var errs field.ErrorList

	fldPath := diskPath.Child("eviction")

	// The schema defaults this to true, so nil is an object that never reached an API server and is
	// read the way the schema would have read it.
	disabled := eviction.Enabled != nil && !*eviction.Enabled

	if disabled && eviction.Policy != "" {
		errs = append(errs, field.Forbidden(fldPath.Child("policy"),
			"eviction is disabled here, and there is no order in which nothing leaves. Remove one of "+
				"the two rather than leaving a policy that reads as taken"))
	}

	if eviction.Watermark == nil {
		return errs
	}

	watermarkPath := fldPath.Child("watermark")

	switch {
	case disabled:
		errs = append(errs, field.Forbidden(watermarkPath,
			"eviction is disabled here, so there is nothing for these marks to start and stop. "+
				"Remove one of the two rather than leaving a band that reads as configured"))
	case disk.Capacity.IsZero():
		// The marks are a fraction of the tier's ceiling, and the store's own default for the quota
		// they are taken against is zero — which its eviction path reads as "no quota" and returns
		// from without evicting. Refused rather than rendered, because a band that never fires looks
		// exactly like one that has not been reached yet.
		errs = append(errs, field.Required(diskPath.Child("capacity"), fmt.Sprintf(
			"a capacity is required when %s is set: the marks are a percentage of it, and without one "+
				"the store evicts nothing while the band reads as configured", watermarkPath)))
	}

	if eviction.Watermark.Low >= eviction.Watermark.High {
		errs = append(errs, field.Invalid(watermarkPath.Child("low"), eviction.Watermark.Low,
			fmt.Sprintf("must be below high (%d): equal or inverted marks make every write past the "+
				"mark evict, and the member's own startup check refuses the pair — inside a container "+
				"log rather than here", eviction.Watermark.High)))
	}

	return errs
}

// pathOverlaps reports whether two cleaned absolute paths would mount over one another — equal, or
// either containing the other.
//
// Containment counts in both directions, which a plain prefix test on strings would get wrong twice:
// it would miss "the tier is the parent of the device tree", and it would falsely match a sibling
// whose name merely starts with the same letters ("/dev/infiniband-x" is not inside
// "/dev/infiniband").
func pathOverlaps(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// hasParentDirComponent reports whether any path component is "..".
//
// It walks the components rather than searching for the substring, so a directory legitimately
// named "..data" — which is what a projected volume mounts — is not mistaken for traversal.
func hasParentDirComponent(path string) bool {
	for component := range strings.SplitSeq(filepath.ToSlash(path), "/") {
		if component == ".." {
			return true
		}
	}
	return false
}

// leaderSnapshotDeclared reports whether a high-availability block asks for snapshots, reading an
// absent block as "no" so the two sides of an update compare without either being dereferenced.
func leaderSnapshotDeclared(ha *workercore.KVCacheBackendLeaderHighAvailability) bool {
	return ha != nil && ha.Snapshot != nil
}

// unchangedPassthrough reports whether this call is an UPDATE that left one passthrough list
// exactly as it already was, in which case that list's rules are not re-run.
//
// REQUIRED: this is not leniency, it is the same scoping the fallback-image check needs, and for the
// same failure. These rules grow -- `enable_oplog` was added to the leader's forbidden list by the
// high-availability work -- and every addition retroactively refuses an object that was admitted
// before it existed. Refusing it is not the problem; refusing it on EVERY update is, because not
// every update is the user's: the reconciler removes this object's finalizer through one, and a
// refusal there strands the object undeletable after teardown has already removed its workloads.
//
// A user who touches the list gets the rule. A user who touches anything else, and the controller
// touching nothing, do not. The leader's extraArgs caller adds one condition on top of this: an
// update that moves `highAvailability` moves which keys are derived, so it re-runs the rules
// regardless.
func unchangedPassthrough[T comparable](isUpdate bool, old, current []T) bool {
	return isUpdate && slices.Equal(old, current)
}

// validateExtraArgs enforces one side's escape-hatch rules. Each refusal says which KIND of problem
// it is, because the fix differs: use the field instead, spell the flag as one token, drop one of
// two entries, or drop the entry.
func validateExtraArgs(
	extraArgs []string, rules mooncake.ExtraArgsRules, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	seen := make(map[string]int, len(extraArgs))
	for i, entry := range extraArgs {
		entryPath := fldPath.Index(i)

		// One entry is one flag token, and the check runs first because the split spelling --
		// ["-rpc_timeout", "5000"] -- puts a bare value where a key belongs, and the collision
		// check would see nothing to collide with while both halves still reach the artifact.
		if !strings.HasPrefix(entry, "-") {
			errs = append(errs, field.Invalid(entryPath, entry,
				`must begin with "-": one entry is one "-flag" or "-flag=value" token, and a value `+
					`written beside its flag carries no key of its own -- a value that itself `+
					`begins with "-" is spelled "-flag=--value"`))
			continue
		}

		// The dashes are decoration the artifact's own parser treats alike, one or two, and the
		// key stops at the first "=" so a boolean flag written as one token keeps its name. This
		// is the key the tables and the duplicate check share.
		//
		// A blank key and a key carrying whitespace are refused in the same breath: neither can
		// be a setting's name, and both render an argument the artifact cannot read.
		key, _, _ := strings.Cut(strings.TrimLeft(entry, "-"), "=")
		if key == "" || strings.ContainsAny(key, " \t\n") {
			errs = append(errs, field.Invalid(entryPath, entry,
				"the key here -- what precedes the first \"=\" once the dashes are off -- is not "+
					"a setting's name, so the artifact cannot read the argument it renders"))
			continue
		}
		if _, dup := seen[key]; dup {
			errs = append(errs, field.Invalid(entryPath, entry, fmt.Sprintf(
				"carries the key %q a second time: a list does not say which of two entries the "+
					"artifact reads, and neither does anything on this object", key)))
			continue
		}
		seen[key] = i

		if reason, ok := rules.Forbidden[key]; ok {
			errs = append(errs, field.Forbidden(entryPath, reason))
			continue
		}
		if slices.Contains(rules.Derived, key) {
			errs = append(errs, field.Forbidden(entryPath,
				"this key is derived from a field of this spec, and two sources for one setting "+
					"make the rendered result ambiguous"))
		}
	}

	for _, group := range rules.Exclusive {
		var present []string
		for _, key := range group {
			if _, ok := seen[key]; ok {
				present = append(present, key)
			}
		}
		if len(present) > 1 {
			errs = append(errs, field.Forbidden(fldPath,
				fmt.Sprintf("%s are mutually exclusive; set at most one",
					strings.Join(present, " and "))))
		}
	}

	return errs
}

// validateKVCacheBackendImmutable freezes what cannot be changed under a running backend. The
// branch is frozen because switching it would abandon or adopt a whole workload; a medium is frozen
// because the segments already mounted from it cannot change kind underneath the data in them.
//
// It also refuses moving a group between positions, which is a different kind of rule: the
// per-position ones below ask whether a field changed, and that one asks whether the whole group at
// a position is one that used to be somewhere else. See validateKVCacheBackendMembersNotMoved for
// why no per-field rule can reach it.
//
// Everything else is editable on purpose: an image, a node selector, a capacity, an extraArgs or
// extraEnv entry, a tier's ceilings and its eviction settings, and the transport block all converge
// on the next pass.
func validateKVCacheBackendImmutable(oldKvcb, newKvcb *workercore.KVCacheBackend) field.ErrorList {
	var errs field.ErrorList

	specPath := field.NewPath("spec")

	if oldKvcb.Spec.Type != newKvcb.Spec.Type {
		errs = append(errs, field.Forbidden(specPath.Child("type"), "type is immutable"))
	}

	oldManaged := oldKvcb.Spec.Connection.Managed != nil
	newManaged := newKvcb.Spec.Connection.Managed != nil
	if oldManaged != newManaged {
		errs = append(errs, field.Forbidden(specPath.Child("connection"),
			"the connection branch is immutable"))
	}

	if !oldManaged || !newManaged {
		return errs
	}

	oldMembers, newMembers := oldKvcb.Spec.Connection.Managed.Members, newKvcb.Spec.Connection.Managed.Members
	membersPath := specPath.Child("connection", "managed", "members")
	errs = append(errs, validateKVCacheBackendMembersNotMoved(oldMembers, newMembers, membersPath)...)
	for i := range newMembers {
		if i >= len(oldMembers) {
			break
		}
		// Live now that the enum carries two values: a medium changed under a running group would
		// remount its segments from a different kind of memory underneath the data they hold, and
		// nothing would fail until someone did it.
		if oldMembers[i].Medium != newMembers[i].Medium {
			errs = append(errs, field.Forbidden(membersPath.Index(i).Child("medium"),
				"medium is immutable"))
		}
		errs = append(errs, validateKVCacheBackendLocalDiskImmutable(
			oldMembers[i].LocalDisks, newMembers[i].LocalDisks,
			membersPath.Index(i).Child("localDisks"))...)
	}

	return errs
}

// validateKVCacheBackendMembersNotMoved refuses an update that carries a group from one position to
// another, which is the edit that hands a running DaemonSet a different group's spec.
//
// WHY IT IS NOT ENOUGH TO FREEZE FIELDS. A group's position IS its identity: the DaemonSet's name,
// its immutable selector labels and the port its members serve all derive from the index. Reordering
// keeps every one of those and moves only the spec underneath, so the members at a position are
// rebuilt against another group's configuration and the cache they held goes with them, with nothing
// on the object saying so. The per-position rules below catch that only when the two groups differ in
// a frozen field — swapping two groups that both lack a disk tier passes them all, because every
// field that does differ is one this API deliberately leaves editable.
//
// WHAT IT RECOGNIZES, AND WHY THAT IS THE WHOLE OF WHAT CAN BE. Without a name of its own, a group is
// only recognizable by its contents, so the one thing that can be told apart from an ordinary edit is
// a group that ARRIVED UNCHANGED at a position another group LEFT. That covers both shapes this
// happens in: a swap, and the upward shift that removing a group ahead of others produces. A reorder
// combined with an edit to the same group is indistinguishable from two edits and is not caught —
// stating that here rather than leaving the next reader to discover the gap.
//
// LEFT is load-bearing, and is not a synonym for "held". A group still sitting at its own position
// has moved nowhere, so an edit that merely makes some other position resemble it is an edit and not
// a move. Without that condition, the documented way to take a group out of service — narrowing its
// nodeSelector until it selects nothing — would be REFUSED on the second of two groups that differ
// only by selector, because narrowing the second makes it equal to the first. Measured: with the
// condition removed, that update is refused.
//
// THE PRICE OF THAT CONDITION IS ONE ADMITTED SHIFT, AND IT CANNOT BE PAID ANY OTHER WAY. Removing a
// group when a LATER group is identical to the one taking its place — [A,B,C] becoming [A,C,C] —
// reaches this and is admitted, which is a shift that the plain [A,B,C] to [A,C] is refused for.
// There is no predicate that separates the two: "position 1 was edited to equal an unchanged
// position 2" and "position 1 was removed and position 2 cloned" produce the SAME two lists from the
// same starting list, so nothing in the request distinguishes them. What the admitted case leaves
// behind is at least visible — the resulting spec holds two identical groups, which the refused
// shapes do not.
//
// WHAT IT LEAVES ALONE, deliberately, because each is an operation this API supports:
//
//   - appending a group, which is why positions beyond the old list's end are never examined;
//   - removing from the END of the list, which changes no surviving position;
//   - editing a group in place, including widening a nodeSelector to gain nodes — the only way a
//     widened selector could trip this rule is by making the group identical to one another position
//     already held, which is a reorder written as an edit.
//
// The comparison is SEMANTIC rather than structural, so two specs differing only in how a quantity
// was spelled — 1Gi against 1024Mi — count as the same group rather than as an edit that would slip
// past the rule.
func validateKVCacheBackendMembersNotMoved(
	oldMembers, newMembers []workercore.KVCacheBackendMember, fldPath *field.Path,
) field.ErrorList {
	for i := range newMembers {
		if i >= len(oldMembers) {
			// A position that did not exist before is an append, and an append moves nothing.
			break
		}
		if kubemeta.DeepEqual(newMembers[i], oldMembers[i]) {
			continue
		}

		for j := range oldMembers {
			if j == i || !kubemeta.DeepEqual(newMembers[i], oldMembers[j]) {
				continue
			}
			if j < len(newMembers) && kubemeta.DeepEqual(newMembers[j], oldMembers[j]) {
				// The group this one now resembles never left position j, so nothing moved: this is
				// an edit that made two positions alike. Refusing it would forbid an edit on the
				// strength of another position that did not change.
				continue
			}
			// One error and not one per position: a swap trips at both of its ends and a shift at
			// every position after the gap, and the fix is the same single edit for all of them.
			return field.ErrorList{field.Forbidden(fldPath.Index(i), fmt.Sprintf(
				"this position now carries the group that was at position %d, which moves a group "+
					"rather than editing one. The position is a group's identity here: the DaemonSet "+
					"at this position keeps its name, its selector and its port, so its members would "+
					"be rebuilt against another group's spec and the cache they hold would go with "+
					"them. Append groups at the end and remove them from the end; to take a group out "+
					"of service in place, narrow its nodeSelector until it matches no node", j))}
		}
	}

	return nil
}

// kvCacheBackendMaxConsumerNames caps how many claimants the refusal below spells out.
const kvCacheBackendMaxConsumerNames = 20

// validateKVCacheBackendMultiTenancyWithdrawal refuses taking the tenant ledger away from a backend
// something already holds.
//
// A KVCachePool over a backend with multi-tenancy registers its reuse domains on that master's
// ledger. Pool admission only warns about a backend without it, so nothing there stops the flag being
// withdrawn under a pool that has already registered, and this rule is what does.
//
// What the withdrawal costs is not only quota correctness, where every request falls into one default
// tenant and two reuse domains read each other's blocks. It costs the EXIT: a pool's finalizer
// releases what it registered on the master, and a master with no ledger has nothing to release from.
//
// The claims are read from the OLD object, which is what the API server holds: status is a
// subresource, so an update to the spec carries whatever status was already there and cannot flip
// this rule's input in the same request that flips the flag.
//
// It refuses on the RAW list rather than on the claims that still resolve, because this handler holds
// no client. An entry naming a pool that is gone therefore refuses too, and status.usedBy is where an
// operator clears it — the same list the backend's own finalizer refuses deletion on.
//
// The claimants are NAMED in the refusal, because an operator whose edit is refused needs to know
// what to go and remove. Sampled, since usedBy has no item bound and the message is what carries the
// refusal.
func validateKVCacheBackendMultiTenancyWithdrawal(
	oldKvcb, newKvcb *workercore.KVCacheBackend,
) field.ErrorList {
	oldManaged, newManaged := oldKvcb.Spec.Connection.Managed, newKvcb.Spec.Connection.Managed
	if oldManaged == nil || newManaged == nil {
		return nil
	}
	if !oldManaged.Leader.MultiTenancy || newManaged.Leader.MultiTenancy {
		return nil
	}
	if len(oldKvcb.Status.UsedBy) == 0 {
		return nil
	}

	// Narrowed BEFORE anything is built, so the cap bounds the work and not only the sentence: a
	// list this admission path renders in full and then discards is one whose cost still scales with
	// usedBy, which has no item bound. The remainder is counted off the original slice for the same
	// reason. Same shape as the reconciler's own listBoundedNames.
	sample := oldKvcb.Status.UsedBy
	if len(sample) > kvCacheBackendMaxConsumerNames {
		sample = sample[:kvCacheBackendMaxConsumerNames]
	}
	names := make([]string, 0, len(sample))
	for _, ref := range sample {
		names = append(names, ref.Kind+"/"+ref.Name)
	}
	consumers := strings.Join(names, ", ")
	if extra := len(oldKvcb.Status.UsedBy) - len(sample); extra > 0 {
		consumers = fmt.Sprintf("%s and %d more", consumers, extra)
	}

	return field.ErrorList{field.Forbidden(
		field.NewPath("spec", "connection", "managed", "leader", "multiTenancy"),
		fmt.Sprintf("multi-tenancy cannot be turned off while %s consume(s) this backend: the master "+
			"would hold no tenant ledger, every request would fall into one default tenant where two "+
			"reuse domains read each other's blocks, and the quota each consumer registered could no "+
			"longer be released — which leaves them undeletable. Remove them first",
			consumers))}
}

// validateKVCacheBackendLocalDiskImmutable freezes whether a group has a disk tier and where it
// lives, while leaving what it may hold editable.
//
// The split follows what the change costs. Turning the tier on or off, or moving it to another
// directory, strands whatever the members already wrote at the old configuration — the data stays
// on the node with nothing addressing it. Raising or lowering the ceiling re-renders one
// environment variable, which the pod fingerprint restarts the group for by the mechanism that
// already exists, and the tier's contents survive that restart.
//
// It compares BY POSITION, and that is not an approximation of some better pairing: the position is
// what identifies a group everywhere else too. MemberObjectName derives the DaemonSet's name from
// it, and the Pod ownership checks read it back. So reordering the list, or removing a group ahead
// of others, genuinely redefines every position after it — the members at those positions are
// rebuilt against a different group's spec, and their caches go with them. Refusing the immutable
// fields there is reporting that, not mistaking it. The messages below say "at this position" so an
// operator who reordered can tell which of the two happened.
//
// The entries are compared as the lists' first elements rather than paired by path, because the
// list carries at most one entry and the position already identifies the group that owns it.
func validateKVCacheBackendLocalDiskImmutable(
	oldDisks, newDisks []workercore.KVCacheBackendMemberLocalDisk, fldPath *field.Path,
) field.ErrorList {
	switch {
	case len(oldDisks) == 0 && len(newDisks) == 0:
		return nil
	case len(oldDisks) == 0:
		return field.ErrorList{field.Forbidden(fldPath,
			"a local disk tier cannot be added to the group at this position: its members would "+
				"have to restart to mount the host directory, and the leader would begin "+
				"offloading to a tier that no existing key is on. If you reordered members or "+
				"removed an earlier group, note that the position identifies a group here — the "+
				"members at this position now belong to a different group's spec")}
	case len(newDisks) == 0:
		return field.ErrorList{field.Forbidden(fldPath,
			"a local disk tier cannot be removed from the group at this position: whatever its "+
				"members have already written stays on their nodes with nothing addressing it. "+
				"Reordering members or removing an earlier group reaches this rule too, because "+
				"the position is what identifies a group")}
	case oldDisks[0].Path != newDisks[0].Path:
		return field.ErrorList{field.Forbidden(fldPath.Index(0).Child("path"),
			"the path is immutable: what the members at this position have already written stays "+
				"at the old one. Reordering members reaches this rule as well, since the position "+
				"is what identifies a group")}
	}

	return nil
}
