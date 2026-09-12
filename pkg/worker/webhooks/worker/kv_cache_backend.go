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
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
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
	errs = append(errs, validateKVCacheBackendTransport(
		kvcb, old, specPath.Child("transport"))...)

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

// validateKVCacheBackendTransport refuses a device resource name the API server would not take as a
// resource list key.
//
// The schema bounds this value twice — a pattern and a length — and between them they express every
// rule the API server applies EXCEPT one: a resource name's domain is limited to 253 characters as a
// whole, and a regular expression cannot say that about a repeated group whose parts vary in length.
// Labels of 63, 63, 63 and 62 characters are each inside their own limit and make a domain of 254, in
// a value of 256 against a cap of 317, so the schema admits it. It then travels into the rendered
// DaemonSet's resource list, where the API server refuses the whole object — a create error inside a
// reconcile, with nothing on this object to say why the member group never came up. That is the same
// shape validateKVCacheBackendName exists for.
//
// The bound is READ from the upstream predicate rather than restated as a number here, so it cannot
// drift from the one the API server applies. It is the call the node selector's keys are already held
// to below, which is not a coincidence: both end up as qualified names in a rendered Pod spec.
//
// A CREATE is judged whatever the protocol is, though only a host fabric renders the value. Checking
// it only on the protocols that consume it would leave a bad name sitting admitted under TCP, which
// is a worse place to find it than at the create that wrote it.
//
// AN UPDATE THAT LEAVES THE NAME WHERE IT WAS IS ADMITTED, and that exemption carries more weight
// here than anywhere else in this file. A name this rule refuses is one whose backend never came
// up — the DaemonSet was never created — so the object carrying it is precisely the object somebody
// needs to DELETE. Deletion runs through an update: the reconciler removes the finalizer with one,
// and refusing that would leave the object undeletable forever, stranded by the rule that was
// supposed to spare its owner the failure in the first place.
//
// THE EXEMPTION'S OWN BLIND SPOT, which is the reason the protocol is read here at all. Switching to
// a host fabric is what makes this name consequential, and that update touches the PROTOCOL rather
// than the name — so an exemption asking only whether the value moved would carry a grandfathered
// bad name into the first render that uses it, which is the failure this rule exists to prevent.
// The name is therefore read again when a protocol change starts rendering it.
//
// Only that direction. Switching a host fabric OFF also moves the protocol, and it is how an
// already-admitted bad name stops mattering: refusing it would close the way out of the very state
// the case above describes.
func validateKVCacheBackendTransport(
	kvcb, old *workercore.KVCacheBackend, fldPath *field.Path,
) field.ErrorList {
	name := kvcb.Spec.Transport.DeviceResourceName
	if name == "" {
		return nil
	}

	if old != nil && old.Spec.Transport.DeviceResourceName == name {
		wasRendered := mooncake.MemberProtocolIsHostFabric(mooncake.MemberProtocol(old))
		isRendered := mooncake.MemberProtocolIsHostFabric(mooncake.MemberProtocol(kvcb))
		if wasRendered || !isRendered {
			return nil
		}
	}

	msgs := validation.IsQualifiedName(name)
	if len(msgs) == 0 {
		return nil
	}

	return field.ErrorList{field.Invalid(fldPath.Child("deviceResourceName"), name,
		"is not a resource name: "+strings.Join(msgs, "; "))}
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
// oldManaged is nil on create. It is used for one thing: skipping the escape-hatch rules over a map
// no update touched. See unchangedExtraArgs.
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

	// REQUIRED: an update that switches high availability on or off re-runs these rules even over a
	// map it did not touch, and the exemption below is what makes that necessary. Turning the field
	// on is what turns `enable_ha`, `ha_backend_type`, `ha_backend_connstring` and `cluster_id` into
	// DERIVED flags; an object admitted before they were derived may carry one, and the renderer
	// appends the escape hatch AFTER the derived flags, so `enable_ha=false` left in the map would
	// win over the `-enable_ha=true` the election needs -- several unelected masters, admitted by a
	// rule that only ever looked at whether the map moved.
	var oldLeaderExtraArgs map[string]string
	haUnchanged := true
	if oldManaged != nil {
		oldLeaderExtraArgs = oldManaged.Leader.ExtraArgs
		haUnchanged = (oldManaged.Leader.HighAvailability == nil) ==
			(managed.Leader.HighAvailability == nil)
	}
	if !haUnchanged ||
		!unchangedExtraArgs(oldManaged != nil, oldLeaderExtraArgs, managed.Leader.ExtraArgs) {
		errs = append(errs, validateExtraArgs(managed.Leader.ExtraArgs,
			mooncake.LeaderExtraArgsRules, fldPath.Child("leader", "extraArgs"))...)
	}

	for i := range managed.Members {
		var oldMember *workercore.KVCacheBackendMember
		if oldManaged != nil && i < len(oldManaged.Members) {
			oldMember = &oldManaged.Members[i]
		}
		errs = append(errs, validateKVCacheBackendMember(&managed.Members[i], oldMember,
			fldPath.Child("members").Index(i))...)
	}

	errs = append(errs, validateKVCacheBackendOffload(managed, oldManaged, fldPath)...)
	errs = append(errs, validateKVCacheBackendScaleIn(managed, fldPath.Child("scaleIn"))...)

	return errs
}

// validateKVCacheBackendOffload enforces that the disk tier is declared on both sides, and that
// only one group carries it.
//
// Every rule here refuses a combination the store ACCEPTS and then quietly does not honor, which
// is the bar for putting a rule in a webhook rather than in the schema: a schema can say a value is
// wrong, only a webhook can say a pair is.
func validateKVCacheBackendOffload(
	managed, oldManaged *workercore.KVCacheBackendManaged, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	var withDisk []int
	for i := range managed.Members {
		if managed.Members[i].LocalDisk != nil {
			withDisk = append(withDisk, i)
		}
	}

	offloadPath := fldPath.Child("leader", "offload")
	offload := managed.Leader.Offload
	enabled := offload != nil && offload.Enabled

	switch {
	case len(withDisk) > 0 && !enabled:
		// The member would report its disk capacity to the leader and the leader would never send
		// it anything, so the backend reads as having a cold tier that never takes a byte.
		errs = append(errs, field.Required(offloadPath.Child("enabled"), fmt.Sprintf(
			"must be true when a member group declares localDisk (group %d does): the leader is "+
				"what decides a key goes to disk, so without it the tier is never written to while "+
				"still reporting its capacity", withDisk[0])))
	case len(withDisk) == 0 && enabled:
		// The mirror image: the leader queues offload work for clients that registered no local
		// disk segment, and its own guard drops it without anything on this object saying so.
		errs = append(errs, field.Required(fldPath.Child("members"), fmt.Sprintf(
			"a member group must declare localDisk when %s is true: no group does, so the leader "+
				"would queue offload work for members that have nowhere to put it",
			offloadPath.Child("enabled"))))
	}

	if offload != nil && offload.OnEvict && !offload.Enabled {
		errs = append(errs, field.Forbidden(offloadPath.Child("onEvict"), fmt.Sprintf(
			"requires %s: the store ands the two together, so this alone is accepted, echoed back "+
				"in the leader's own startup log, and then does nothing",
			offloadPath.Child("enabled"))))
	}

	// The mirror image, and the reason it is a refusal rather than a documented caveat: the store
	// gives this pair no protection at all. It holds an object queued for offload in memory by
	// taking a reference on the memory replica when the object enters the queue and releasing it
	// only once the client reports the disk write landed -- and that happens on the deferred branch
	// alone. The write-through branch returns before reaching it, so an object whose bucket has not
	// yet been flushed has its only replica evicted and is gone.
	//
	// Refusing the pair removes the mode. Defaulting the other one instead would leave an
	// administrator who chose this one deliberately in exactly the same place.
	//
	// Scoped to the update that INTRODUCES the pair, like the capacityPerMember floor above it and
	// for the same reason: re-judging a pair an update left alone would strand a backend admitted
	// before this rule existed, and not every update is the user's. This webhook opts into
	// ReceiveDeletionUpdate, so the reconciler removing the finalizer is one such update -- refusing
	// that would leave the object undeletable rather than merely unsafe.
	var oldOffload *workercore.KVCacheBackendLeaderOffload
	if oldManaged != nil {
		oldOffload = oldManaged.Leader.Offload
	}
	storedUnprotected := oldOffload != nil && oldOffload.Enabled && !oldOffload.OnEvict
	if enabled && !offload.OnEvict && !storedUnprotected {
		errs = append(errs, field.Required(offloadPath.Child("onEvict"), fmt.Sprintf(
			"must be true when %s is true: the store pins a queued object's memory replica until "+
				"its disk write lands only when this is set, and evicting without it destroys the "+
				"sole replica of an object whose bucket has not been flushed",
			offloadPath.Child("enabled"))))
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
		errs = append(errs, field.Forbidden(
			fldPath.Child("members").Index(withDisk[1]).Child("localDisk"),
			fmt.Sprintf("only one member group may declare localDisk, and groups %v declare one: "+
				"the leader reports every disk tier through one pair of gauges, so status.capacity "+
				"could not say which figure belonged to which group", withDisk)))
	}

	return errs
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

// validateKVCacheBackendMember holds the per-group rules a schema cannot carry: a medium the schema
// accepts but nothing renders, and two quantities whose schema type is a string.
func validateKVCacheBackendMember(
	member, oldMember *workercore.KVCacheBackendMember, fldPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	// There is no per-medium rule here any more, and its absence is deliberate: the schema now
	// enumerates the one value, so a medium this API does not render is refused before this handler
	// runs and a rule for it would be code no request can reach.

	var oldDisk *workercore.KVCacheBackendMemberLocalDisk
	if oldMember != nil {
		oldDisk = oldMember.LocalDisk
	}
	errs = append(errs, validateKVCacheBackendLocalDisk(member.LocalDisk, oldDisk, fldPath.Child("localDisk"))...)

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
	case member.LocalDisk != nil &&
		member.CapacityPerMember.CmpInt64(mooncake.MemberBucketSizeLimit) < 0 &&
		(oldMember == nil || member.CapacityPerMember.Cmp(oldMember.CapacityPerMember) != 0):
		// The floor applies only to a group that declares a tier, and only when this update moved
		// the value. Re-judging a figure the update left alone is what would strand an object
		// admitted before the bound existed — and not every update is the user's.
		errs = append(errs, field.Invalid(fldPath.Child("capacityPerMember"),
			member.CapacityPerMember.String(), fmt.Sprintf(
				"must be at least %s for a group declaring localDisk: that is one bucket, the unit the "+
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

	var oldExtraArgs, oldExtraEnvs map[string]string
	if oldMember != nil {
		oldExtraArgs, oldExtraEnvs = oldMember.ExtraArgs, oldMember.ExtraEnvs
	}
	if !unchangedExtraArgs(oldMember != nil, oldExtraArgs, member.ExtraArgs) {
		errs = append(errs, validateExtraArgs(member.ExtraArgs,
			mooncake.MemberExtraArgsRules, fldPath.Child("extraArgs"))...)
	}
	if !unchangedExtraArgs(oldMember != nil, oldExtraEnvs, member.ExtraEnvs) {
		errs = append(errs, validateExtraEnvs(member.ExtraEnvs, fldPath.Child("extraEnvs"))...)
	}

	return errs
}

// validateExtraEnvs enforces the environment passthrough's two rules.
//
// It is separate from validateExtraArgs rather than sharing it, because the half that differs is the
// half that matters: a config key and an environment-variable name have different shapes, judged by
// different upstream rules, and a message naming the wrong one sends someone looking for a mistake
// they did not make. What they have in common is one list walk, which is smaller than the
// abstraction that would hold it.
//
// Its scoping matches validateExtraArgs for the same reason: the derived list GROWS as this API
// renders more, and re-judging a map an update did not touch would retroactively refuse an object
// admitted before the entry existed — on every update, including the reconciler's own removal of
// this object's finalizer. See unchangedExtraArgs.
func validateExtraEnvs(extraEnvs map[string]string, fldPath *field.Path) field.ErrorList {
	var errs field.ErrorList

	for _, name := range slices.Sorted(maps.Keys(extraEnvs)) {
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
		if msgs := validation.IsEnvVarName(name); len(msgs) > 0 {
			errs = append(errs, field.Invalid(fldPath.Key(name), name,
				"is not an environment variable name: "+strings.Join(msgs, "; ")))
			continue
		}
		if slices.Contains(mooncake.MemberDerivedEnvs, name) {
			errs = append(errs, field.Forbidden(fldPath.Key(name),
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
		// The two mounts land in ONE container, so a collision is resolved by the kubelet rather
		// than reported here — one mount shadows the other, and which one wins is not something
		// this object records. Refused whatever the transport is today, because the transport is
		// editable: a tier that merely does not collide yet would start colliding the moment
		// someone switched the backend to a host fabric. The device tree is the only mount either
		// host fabric adds; an EFA member takes its libfabric from the image.
		errs = append(errs, field.Invalid(pathPath, disk.Path, fmt.Sprintf(
			"must not overlap %s, which the RDMA and EFA transports mount into the same container: "+
				"one mount would shadow the other, and the transport can be switched to one after "+
				"this path is set", mooncake.RDMADevicePath)))
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

// unchangedExtraArgs reports whether this call is an UPDATE that left an extraArgs map exactly as it
// already was, in which case its escape-hatch rules are not re-run.
//
// REQUIRED: this is not leniency, it is the same scoping the fallback-image check needs, and for the
// same failure. These rules grow -- `enable_oplog` was added to the leader's forbidden list by the
// high-availability work -- and every addition retroactively refuses an object that was admitted
// before it existed. Refusing it is not the problem; refusing it on EVERY update is, because not
// every update is the user's: the reconciler removes this object's finalizer through one, and a
// refusal there strands the object undeletable after teardown has already removed its workloads.
//
// A user who touches the map gets the rule. A user who touches anything else, and the controller
// touching nothing, do not. The leader's caller adds one condition on top of this: an update that
// moves `highAvailability` moves which keys are derived, so it re-runs the rules regardless.
func unchangedExtraArgs(isUpdate bool, old, current map[string]string) bool {
	return isUpdate && maps.Equal(old, current)
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
// It also refuses moving a group between positions, which is a different kind of rule: the
// per-position ones below ask whether a field changed, and that one asks whether the whole group at
// a position is one that used to be somewhere else. See validateKVCacheBackendMembersNotMoved for
// why no per-field rule can reach it.
//
// Everything else is editable on purpose: an image, a node selector, a capacity, an extraArgs or
// extraEnvs entry, a tier's ceilings and its eviction settings, and the transport block all converge
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
		// Unreachable while the enum carries one value, and kept for the day it carries two: on
		// that day a medium becomes mutable by default, under segments already mounted from it,
		// and nothing would fail until someone did it. A rule that is only correct after a later
		// change is cheaper to keep than to remember to add.
		if oldMembers[i].Medium != newMembers[i].Medium {
			errs = append(errs, field.Forbidden(membersPath.Index(i).Child("medium"),
				"medium is immutable"))
		}
		errs = append(errs, validateKVCacheBackendLocalDiskImmutable(
			oldMembers[i].LocalDisk, newMembers[i].LocalDisk,
			membersPath.Index(i).Child("localDisk"))...)
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
// A KVCachePool is refused at creation when its backend runs without multi-tenancy. That rule
// governs one admission moment and leaves the other open — the flag can be withdrawn under a pool
// already admitted — and this is the second half of it.
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
func validateKVCacheBackendLocalDiskImmutable(
	oldDisk, newDisk *workercore.KVCacheBackendMemberLocalDisk, fldPath *field.Path,
) field.ErrorList {
	switch {
	case oldDisk == nil && newDisk == nil:
		return nil
	case oldDisk == nil:
		return field.ErrorList{field.Forbidden(fldPath,
			"a local disk tier cannot be added to the group at this position: its members would "+
				"have to restart to mount the host directory, and the leader would begin "+
				"offloading to a tier that no existing key is on. If you reordered members or "+
				"removed an earlier group, note that the position identifies a group here — the "+
				"members at this position now belong to a different group's spec")}
	case newDisk == nil:
		return field.ErrorList{field.Forbidden(fldPath,
			"a local disk tier cannot be removed from the group at this position: whatever its "+
				"members have already written stays on their nodes with nothing addressing it. "+
				"Reordering members or removing an earlier group reaches this rule too, because "+
				"the position is what identifies a group")}
	case oldDisk.Path != newDisk.Path:
		return field.ErrorList{field.Forbidden(fldPath.Child("path"),
			"the path is immutable: what the members at this position have already written stays "+
				"at the old one. Reordering members reaches this rule as well, since the position "+
				"is what identifies a group")}
	}

	return nil
}
