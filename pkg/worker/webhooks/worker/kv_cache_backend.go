package worker

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net"
	"net/url"
	"slices"
	"strconv"
	"strings"

	conregname "github.com/google/go-containerregistry/pkg/name"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/quantityx"
	"gpustack.ai/gpustack/pkg/webhook"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

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

var _ ctrladmission.Validator[runtime.Object] = (*KVCacheBackendWebhook)(nil)

func (r *KVCacheBackendWebhook) ValidateCreate(ctx context.Context, obj runtime.Object) (ctrladmission.Warnings, error) {
	kvcb := obj.(*workercore.KVCacheBackend)

	if errs := validateKVCacheBackendSpec(ctx, kvcb, true); len(errs) > 0 {
		return nil, kerrors.NewInvalid(kvcb.GroupVersionKind().GroupKind(), kvcb.Name, errs)
	}

	return nil, nil
}

func (r *KVCacheBackendWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	oldKvcb, newKvcb := oldObj.(*workercore.KVCacheBackend), newObj.(*workercore.KVCacheBackend)

	errs := validateKVCacheBackendSpec(ctx, newKvcb, oldKvcb.Spec.Image != newKvcb.Spec.Image)
	errs = append(errs, validateKVCacheBackendImmutable(oldKvcb, newKvcb)...)
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
	ctx context.Context, kvcb *workercore.KVCacheBackend, checkFallback bool,
) field.ErrorList {
	specPath := field.NewPath("spec")

	errs := validateKVCacheBackendName(kvcb)
	errs = append(errs, validateKVCacheBackendImage(ctx, kvcb, checkFallback, specPath.Child("image"))...)
	errs = append(errs, validateKVCacheBackendPullSecrets(
		kvcb.Spec.ImagePullSecrets, specPath.Child("imagePullSecrets"))...)
	errs = append(errs, validateKVCacheBackendConnection(&kvcb.Spec, specPath.Child("connection"))...)

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
// could be refusing on behalf of. The setting ships blank on purpose, so without this guard every
// external backend — the shape the documentation shows — would be refused on a default install.
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
	spec *workercore.KVCacheBackendSpec, fldPath *field.Path,
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
		return validateKVCacheBackendManaged(managed, fldPath.Child("managed"))
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
func validateKVCacheBackendManaged(
	managed *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	if replicas := managed.Leader.Replicas; replicas != nil && *replicas != 1 {
		errs = append(errs, field.Invalid(fldPath.Child("leader", "replicas"), *replicas,
			"only 1 is supported: electing a leader among several needs an HA backend store, "+
				"which the leader high-availability subject owns"))
	}

	// field.TooMany would say "must have at most 1 item" and nothing else. The shape accepts a
	// second group on purpose — a hot tier and a cold tier are expressible — so the refusal has to
	// say that what is missing is the tiering, not the shape.
	if len(managed.Members) > 1 {
		errs = append(errs, field.Forbidden(fldPath.Child("members"), fmt.Sprintf(
			"only 1 member group is reconciled in this scope, and %d were given: a second group is a "+
				"second medium tier, and placing or migrating data across tiers is what the tiering "+
				"subject owns", len(managed.Members))))
	}

	errs = append(errs, validateExtraArgs(managed.Leader.ExtraArgs,
		mooncake.LeaderExtraArgsRules, fldPath.Child("leader", "extraArgs"))...)

	for i := range managed.Members {
		errs = append(errs, validateKVCacheBackendMember(&managed.Members[i],
			fldPath.Child("members").Index(i))...)
	}

	return errs
}

const quantityTooLarge = "must not exceed 9223372036854775807 (2^63-1) bytes: the renderer reads " +
	"this as a signed 64-bit count, and a larger one does not survive the conversion"

// validateKVCacheBackendMember holds the per-group rules a schema cannot carry: a medium the schema
// accepts but nothing renders, and two quantities whose schema type is a string.
func validateKVCacheBackendMember(
	member *workercore.KVCacheBackendMember, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	if !mooncake.MemberMediumIsReconciled(member.Medium) {
		errs = append(errs, field.Forbidden(fldPath.Child("medium"), fmt.Sprintf(
			"only %q is reconciled in this scope, and %q was given: the store supports this medium, "+
				"but running it needs the leader's file or DAX flags and a mount on the member, and "+
				"rendering those per medium is what the tiering subject owns",
			mooncake.MemberMediumDRAM, member.Medium)))
	}

	// A resource.Quantity is a STRING in the schema, so no numeric bound in a marker can reach it —
	// these two are the only place either can be refused. Zero is refused rather than defaulted,
	// because the renderer omits the segment size it derives when the value is not positive, and a
	// member that mounts no segment is indistinguishable from one whose leader lost it.
	if member.CapacityPerMember.CmpInt64(0) <= 0 {
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(),
			"must be greater than 0: a member contributing nothing is a Pod with no reason to run"))
	} else if quantityx.OverflowsInt64(member.CapacityPerMember) {
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(), quantityTooLarge))
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

	errs = append(errs, validateExtraArgs(member.ExtraArgs,
		mooncake.MemberExtraArgsRules, fldPath.Child("extraArgs"))...)

	return errs
}

// validateExtraArgs enforces one side's escape-hatch rules. Each refusal says which KIND of problem
// it is, because the fix differs: use the field instead, drop one of two keys, or drop the key.
func validateExtraArgs(
	extraArgs map[string]string, rules mooncake.ExtraArgsRules, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for _, key := range slices.Sorted(maps.Keys(extraArgs)) {
		// Checked before the tables, because the tables key on the BARE NAME and anything else
		// misses every entry while still reaching the artifact as the flag they protect. Two
		// decorations do it, and both were measured: the leader renderer prepends one dash, so
		// "-rpc_port" renders "--rpc_port=" and gflags reads the flag "rpc_port" names; and it
		// joins key and value with "=", so "rpc_port=1" renders "-rpc_port=1=60000" and gflags
		// takes everything before the FIRST "=" as the flag — "rpc_port" again. A blank key and a
		// key carrying a space are refused in the same breath: neither can be a setting's name,
		// and both render an argument the artifact cannot read.
		if key == "" || strings.HasPrefix(key, "-") || strings.ContainsAny(key, "= \t\n") {
			errs = append(errs, field.Invalid(fldPath.Key(key), key,
				"a key is the bare name of a setting: no leading dash, no \"=\", no spaces. The "+
					"renderer adds what the artifact expects, and a decorated key reaches it as a "+
					"flag no rule here could recognise"))
			continue
		}
		if reason, ok := rules.Forbidden[key]; ok {
			errs = append(errs, field.Forbidden(fldPath.Key(key), reason))
			continue
		}
		if slices.Contains(rules.Derived, key) {
			errs = append(errs, field.Forbidden(fldPath.Key(key),
				"this key is derived from a field of this spec, and two sources for one setting "+
					"make the rendered result ambiguous"))
		}
	}

	for _, group := range rules.Exclusive {
		var present []string
		for _, key := range group {
			if _, ok := extraArgs[key]; ok {
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
// Everything else is editable on purpose: an image, a node selector, a capacity, an extraArgs entry
// and the transport block all converge on the next pass.
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
	for i := range newMembers {
		if i >= len(oldMembers) {
			break
		}
		if oldMembers[i].Medium != newMembers[i].Medium {
			errs = append(errs, field.Forbidden(membersPath.Index(i).Child("medium"),
				"medium is immutable"))
		}
	}

	return errs
}
