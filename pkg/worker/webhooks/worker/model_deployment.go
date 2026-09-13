package worker

import (
	"context"
	"fmt"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// ModelDeploymentWebhook validates a v1alpha1.ModelDeployment, and defaults the one field a schema
// cannot.
//
// Every other default this API has — the connector discriminator and the replica count — is a CRD
// schema default, and every enum is a CRD schema enum. The mutating half exists for the single
// value whose default depends on ANOTHER object: a role's accelerator count, which is one on an
// acceleratable InstanceType and absent on any other, and no schema default can tell the two apart.
// For the enums a webhook could not help even if one were written: structural schema
// validation runs before the validating admission chain, so a value outside an enum is refused
// before this handler is reached.
//
// What is left here is what a schema cannot express: a bound that must carry an actionable message,
// a comparison between two entries of a list, and a collision between what a user supplies and what
// the operator owns.
//
// MOST VALIDATION RULES ARE ANSWERED FROM THE OBJECT ALONE, AND TWO ARE NOT. Whether the named
// InstanceType offers the resource mode a role asks for, and whether the role's card count fits what
// that type hands out at once, are facts about another object. Everything else here is decided
// without leaving the request.
//
// WHAT READS ANOTHER OBJECT READS IT FROM THE API SERVER AND NEVER FROM A CACHE, the default and
// those two rules alike. A cache decides the outcome in both directions when it is behind — a type
// created moments ago reads as absent and refuses a deployment that names a type that exists, and a
// type recreated under the same name reads with its old acceleratable flag and writes a count from
// it. The second is the worse one, because the wrong value is then persisted.
//
// THE PRICE IS ONE CONSISTENT READ PER HANDLER PASS, not per role: every role of a valid deployment
// names the same type and each pass memoizes by name. An admission therefore pays two, one for the
// mutating half and one for the validating half, because they are separate calls and a value carried
// between them would be a third cache with no one watching it. This handler runs on a user-initiated
// write to one object rather than in a reconcile loop, which is what makes that the cheaper side.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeployments",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeployments",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelDeploymentWebhook struct {
	// APIReader reads through to the API server. There is deliberately no cached client beside it:
	// the one value this handler defaults is read from another object, and a cache that is behind
	// would decide it.
	APIReader ctrlcli.Reader
}

func (r *ModelDeploymentWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.ModelDeployment{}, nil
}

var (
	_ ctrladmission.Validator[runtime.Object] = (*ModelDeploymentWebhook)(nil)
	_ ctrladmission.Defaulter[runtime.Object] = (*ModelDeploymentWebhook)(nil)
	_ webhook.ReceiveDeletionUpdate           = (*ModelDeploymentWebhook)(nil)
)

// ReceiveDeletionUpdate keeps this webhook validating updates to a deployment that is being deleted.
// Its own finalizer releases the Binding it holds, so it stays for as long as that takes, and the
// role rules are what keep an edit in that window from producing a shape nothing consumes.
//
// THE MARKER IS ONE DECISION COVERING BOTH HALVES, so opting validation in opts defaulting in with
// it. What each half then does inside the deletion window is decided by whether it reads another
// object, and that line does not run between the halves -- it runs through the middle of validation.
//
// ANY RULE THAT READS AN InstanceType DECLINES FOR AN OBJECT BEING DELETED, in both halves. Such a
// rule refuses when the type is absent, so the update that clears the finalizer would be refused
// whenever the type went first -- an object its own teardown can never release. Default declines
// wholesale, since filling a count on an object that is going away changes nothing. Validation keeps
// every rule answerable from the object alone and drops only the two that need the type, because
// those are the rules that would strand the object, and the rest are what keep an edit in that
// window from producing a shape nothing consumes.
//
// WHAT THAT COSTS IS NAMED RATHER THAN HIDDEN: an edit made while a deployment is being deleted can
// move a role onto a mode its type does not offer. The deployment is going away, so nothing renders
// the result, and the alternative -- refusing it -- is the deadlock above.
func (r *ModelDeploymentWebhook) ReceiveDeletionUpdate() {}

// Default fills the accelerator count a role left unset, which is one card on an acceleratable
// InstanceType and nothing on any other.
//
// IT IS THE SAME RULE THE Instance WEBHOOK ALREADY APPLIES, and this type was the one that did not
// have it. A role that names no count is asking for the ordinary thing — a card — and until now it
// got a replica that requested no accelerator at all.
//
// THE COST OF LEAVING IT UNSET IS NOT A SMALLER REPLICA, it is a deployment that never starts. An
// accelerated pool's ClusterQueue covers only that manufacturer's credits, so a role requesting no
// accelerator requests nothing the queue covers. A Workload whose ONLY PodSet is such a role is
// admitted, with an assignment carrying no flavors.
//
// Add a second PodSet of any kind and the scheduler writes an admission carrying fewer assignments
// than the Workload has PodSets, the API refuses it, and the Workload is requeued immediately and
// forever -- measured at roughly a hundred scheduling cycles a second, with the deployment parked
// and nothing reporting why. So this surfaces on a multi-role deployment whether or not its other
// roles ask for cards: a mixed deployment is refused exactly like an all-uncovered one.
//
// AN EXPLICIT ZERO IS LEFT ALONE and still reaches that state. Zero is a value the user wrote, and
// silently replacing it would be worse than the failure: the request would stop meaning what it
// says. A CPU-only deployment belongs on a CPU-only InstanceType, whose queue covers cpu, which
// every replica requests -- so the shape that hangs is reachable only by asking for a pool this
// deployment does not need.
//
// IT RUNS ON UPDATE AS WELL AS CREATE, because roles are not frozen: a deployment edited to add a
// second role would otherwise carry an undefaulted one and reach exactly the state above. The
// defaulter cannot tell a new role from an old one -- it is handed the incoming object and no
// previous one -- so it defaults any unset count it finds, which on an object that was stored
// before this rule existed changes a request that had already been admitted. That is acceptable
// only because this API is in no released version, so there are no such objects outside a branch.
func (r *ModelDeploymentWebhook) Default(ctx context.Context, obj runtime.Object) error {
	md := obj.(*workercore.ModelDeployment)

	// AN OBJECT BEING DELETED IS NOT DEFAULTED. This handler opts out of the shared deletion guard
	// to keep validating updates in that window, and that opt-out is one decision covering
	// defaulting too. The read below refuses when the InstanceType is absent, so without this the
	// update that clears the finalizer would be refused whenever the type was deleted first, and
	// nothing could then release the object.
	if md.DeletionTimestamp != nil {
		return nil
	}

	// Roles must all name one InstanceType, but that is a VALIDATION rule and validation has not run
	// yet -- mutating admission comes first. So each role is defaulted against the type it names,
	// and the reads are memoized rather than assumed to be one.
	seen := make(map[string]*worker.InstanceType, 1)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.Resources != nil && role.Resources.Accelerator != nil {
			continue
		}

		// THE SCHEMA HAS NOT RUN YET EITHER, so a name this API would refuse still arrives here. An
		// empty one is left to the rules that report it as the field error it is: looking it up
		// would fail on the request rather than on the object, and deny the write with a message
		// about reading the cluster instead of about the name.
		if role.InstanceType == "" {
			continue
		}

		instType, ok := seen[role.InstanceType]
		if !ok {
			var err error
			if instType, err = r.getInstanceType(ctx, role.InstanceType, i); err != nil {
				return err
			}
			seen[role.InstanceType] = instType
		}
		if !instType.Spec.Acceleratable {
			continue
		}

		if role.Resources == nil {
			role.Resources = new(workercore.ModelDeploymentRoleResources)
		}
		role.Resources.Accelerator = resource.NewQuantity(1, resource.DecimalSI)
	}

	return nil
}

// getInstanceType reads one InstanceType, from the API server and never from a cache.
//
// A QUORUM READ IS THE WHOLE OF WHAT MAKES THIS DEFAULT SAFE TO ADD. A cached read decides the
// outcome whenever the informer is behind, and it does so in BOTH directions: a type created moments
// earlier reads as absent and the deployment is refused for naming a type that exists, while a type
// deleted and recreated under the same name reads with its old acceleratable flag and the count is
// written from it -- the second is the worse one, because the wrong value is then persisted. Reading
// with ResourceVersion "0" is not a fix either: that is answered from the API server's watch cache,
// which is a second cache behind for the same reasons.
//
// THE COST IS ONE READ PER ADMISSION, not one per role. Every role of a valid deployment names the
// same type, and the caller memoizes by name, so the reads collapse. This handler runs on a
// user-initiated write to one object rather than on a reconcile loop, which is what makes paying for
// consistency here the cheaper side of the trade.
//
// A MISS REFUSES THE WRITE, which on an update blocks every edit to a deployment that already exists
// for as long as the type is absent. What bounds that is the name being read from the INCOMING
// object: pointing the role at a type that does exist is admitted, and an object being deleted is
// not defaulted at all. Admitting it instead would store a deployment the renderer cannot build, and
// that failure reaches a reader only as an Event while the conditions go on saying no replica has
// been created yet — which is how a slow start reads too.
//
// THE MESSAGE NAMES THE TYPE RATHER THAN THE READ, because those send a reader to different places.
// "instance type not found" is a name to check; "could not read instance type" is a cluster to
// check, and reporting the second for the first costs an operator the time to find a typo.
func (r *ModelDeploymentWebhook) getInstanceType(
	ctx context.Context, name string, roleIdx int,
) (*worker.InstanceType, error) {
	instType := &worker.InstanceType{ObjectMeta: meta.ObjectMeta{Name: name}}
	rolePath := field.NewPath("spec.roles").Index(roleIdx).Child("instanceType")

	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(instType), instType); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, field.NotFound(rolePath, name)
		}

		return nil, field.InternalError(rolePath, fmt.Errorf("get instance type: %w", err))
	}

	return instType, nil
}

func (r *ModelDeploymentWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := obj.(*workercore.ModelDeployment)

	errs := validateModelDeployment(md, nil)

	errs = append(errs, validateModelDeploymentBarrierIsInstallable(ctx, md)...)

	typeErrs, err := r.validateRoleResourcesAgainstInstanceTypes(ctx, md)
	if err != nil {
		return nil, err
	}
	errs = append(errs, typeErrs...)

	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
}

func (r *ModelDeploymentWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := newObj.(*workercore.ModelDeployment)
	old, _ := oldObj.(*workercore.ModelDeployment)

	// WHAT THIS DEPLOYMENT IS CANNOT BE EDITED; HOW IT IS CURRENTLY RUN CAN. The fields answering the
	// first question are refused here, and the criterion that sorts them is stated on
	// validateModelDeploymentIdentity rather than left as the list it produces, so the next field
	// added has a question to be judged against.
	//
	// The role names the object ALREADY had are carried in, and the reason is that a rule this handler
	// gained after an object was stored must not be able to strand that object. The Service-name rule
	// is the one that could: a deployment's own name is immutable, so a role whose combined name is
	// too long could never be shortened, and every later edit -- including one that removes the
	// offending role -- would be refused. That is worse than the reconcile failure the rule prevents.
	errs := validateModelDeployment(md, modelDeploymentRoleNames(oldObj))
	errs = append(errs, validateModelDeploymentIdentity(md, old)...)
	errs = append(errs, validateModelDeploymentBarrierIsInstallable(ctx, md)...)

	typeErrs, err := r.validateRoleResourcesAgainstInstanceTypes(ctx, md)
	if err != nil {
		return nil, err
	}
	errs = append(errs, typeErrs...)

	if len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
}

// modelDeploymentIdentityMessage is the one reason every identity refusal carries.
//
// IT STATES THE RULE RATHER THAN A MECHANISM, and the difference is where it sends the reader. An
// earlier framing of this freeze was "shrink the surface two writers can disagree on"; a message
// carrying that sends an operator hunting for a locking problem that does not exist. What they need
// is the question the rule answers -- this value is part of what makes the object this deployment,
// so a different value describes a different deployment, and a different deployment is created.
const modelDeploymentIdentityMessage = "this is part of what makes this deployment the deployment " +
	"it is: a different value describes a different deployment, which is created rather than edited"

// validateModelDeploymentIdentity refuses an update that changes what the deployment IS, and admits
// one that changes how it is currently run.
//
// THE CRITERION IS THE RULE, NOT THE LIST BELOW. A field is frozen when it answers "which deployment
// is this" -- what is served, what serves it, whose cache it shares, and the shape of the roles that
// serve it. A field is editable when it answers "how is this deployment being run right now" -- how
// many replicas, which build, how that build is fetched and tuned. Judging a NEW field means asking
// that question, not appending to the list; a list alone grows by precedent and stops meaning
// anything.
//
// ONE FIELD IS FROZEN AGAINST THE CRITERION and is marked here so it is not read as an oversight:
// roles[].resources does not answer which deployment this is, but it changes what admission has to
// find. Changing it renegotiates the scheduling, which is not materially different from deleting and
// recreating. Its mirror image is template.privileged, which the criterion leaves editable even
// though a different argument could move it.
//
// ROLES ARE MATCHED BY NAME, NEVER BY POSITION. The field is a listType=map keyed by name, so a
// reordered list is the same set of roles and the API already treats it as one; comparing by index
// would refuse a declarative apply that changed nothing, on the one field most likely to come back
// serialized in another order.
//
// THE STORED OBJECT NEEDS NO DEFAULTING PASS BEFORE THE COMPARISON, and that is a property of this
// webhook rather than an assumption. The mutating half is registered for CREATE as well as UPDATE,
// so every object that reached storage was defaulted on the way in and carries the same accelerator
// count the incoming one gets. The gap -- an object stored before the default existed, which would
// read as nil becoming one and be refused for an edit nobody made -- is closed by this API being in
// no released version, which is the same ground Default already stands on.
//
// NOTHING OUTSIDE spec IS COMPARED. Labels, annotations and finalizers stay editable because the
// controllers that write them include this operator, and status is not a user's to send.
func validateModelDeploymentIdentity(md, old *workercore.ModelDeployment) field.ErrorList {
	if old == nil {
		return nil
	}

	specPath := field.NewPath("spec")

	var errs field.ErrorList
	if !kubemeta.DeepEqual(md.Spec.Model, old.Spec.Model) {
		errs = append(errs, field.Invalid(
			specPath.Child("model"), md.Spec.Model, modelDeploymentIdentityMessage))
	}
	if md.Spec.Engine != old.Spec.Engine {
		errs = append(errs, field.Invalid(
			specPath.Child("engine"), md.Spec.Engine, modelDeploymentIdentityMessage))
	}
	if !kubemeta.DeepEqual(md.Spec.KVCache, old.Spec.KVCache) {
		errs = append(errs, field.Invalid(
			specPath.Child("kvCache"), md.Spec.KVCache, modelDeploymentIdentityMessage))
	}

	return append(errs, validateModelDeploymentRoleIdentity(md, old)...)
}

// validateModelDeploymentRoleIdentity refuses a change to the set of roles, or to the frozen fields
// of a role the object already carried.
//
// THE SET IS PART OF THE SHAPE, so adding a role and removing one are both refused, each named where
// a reader can act on it: an added role at its own index, a removed one at the list. Iteration runs
// over the two slices rather than over a map, so the refusals come out in a stable order and a test
// can assert which field was named rather than only that something was.
func validateModelDeploymentRoleIdentity(md, old *workercore.ModelDeployment) field.ErrorList {
	rolesPath := field.NewPath("spec", "roles")

	stored := make(map[string]*workercore.ModelDeploymentRole, len(old.Spec.Roles))
	for i := range old.Spec.Roles {
		stored[old.Spec.Roles[i].Name] = &old.Spec.Roles[i]
	}

	var errs field.ErrorList
	incoming := sets.New[string]()
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		incoming.Insert(role.Name)

		was, ok := stored[role.Name]
		if !ok {
			errs = append(errs, field.Invalid(
				rolesPath.Index(i).Child("name"), role.Name, modelDeploymentIdentityMessage))

			continue
		}

		errs = append(errs, validateModelDeploymentRoleIdentityFields(rolesPath.Index(i), role, was)...)
	}

	for i := range old.Spec.Roles {
		if name := old.Spec.Roles[i].Name; !incoming.Has(name) {
			errs = append(errs, field.Invalid(rolesPath, name, modelDeploymentIdentityMessage))
		}
	}

	return errs
}

// validateModelDeploymentRoleIdentityFields compares one role's frozen fields against the stored
// role of the same name.
//
// template.command is frozen because it decides whether the operator configures this role at all: a
// role that supplies one is taken over by its author, which changes cache injection and what status
// can claim. The rest of the template is how the build is fetched, shaped and tuned, and is
// editable. template.resources has no side here because an existing rule refuses it outright, so it
// is never part of an update in either direction.
func validateModelDeploymentRoleIdentityFields(
	rolePath *field.Path, role, was *workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList
	if role.Kind != was.Kind {
		errs = append(errs, field.Invalid(
			rolePath.Child("kind"), role.Kind, modelDeploymentIdentityMessage))
	}
	if role.InstanceType != was.InstanceType {
		errs = append(errs, field.Invalid(
			rolePath.Child("instanceType"), role.InstanceType, modelDeploymentIdentityMessage))
	}
	if !kubemeta.DeepEqual(role.Resources, was.Resources) {
		errs = append(errs, field.Invalid(
			rolePath.Child("resources"), role.Resources, modelDeploymentIdentityMessage))
	}
	if cmd, wasCmd := modelDeploymentRoleCommand(role), modelDeploymentRoleCommand(was); !kubemeta.DeepEqual(cmd, wasCmd) {
		errs = append(errs, field.Invalid(
			rolePath.Child("template", "command"), cmd, modelDeploymentIdentityMessage))
	}

	return errs
}

// modelDeploymentRoleCommand reads a role's command through an absent template, so that a role
// gaining a template it did not have is not reported as a command change it did not make.
func modelDeploymentRoleCommand(role *workercore.ModelDeploymentRole) []string {
	if role.Template == nil {
		return nil
	}

	return role.Template.Command
}

// modelDeploymentRoleNames is the set of role names an object already carried, or nil on create.
func modelDeploymentRoleNames(obj runtime.Object) sets.Set[string] {
	md, ok := obj.(*workercore.ModelDeployment)
	if !ok {
		return nil
	}

	names := sets.New[string]()
	for i := range md.Spec.Roles {
		names.Insert(md.Spec.Roles[i].Name)
	}

	return names
}

func (r *ModelDeploymentWebhook) ValidateDelete(
	_ context.Context, _ runtime.Object,
) (ctrladmission.Warnings, error) {
	// Deletion is not refused here. What holds a deployment back is its own finalizer releasing the
	// Binding it holds, which needs the consumers this handler cannot see.
	return nil, nil
}

// validateModelDeployment holds every rule answerable from the object alone.
//
// The seam the single-role version left here — the rules that outlive it, separated from the one
// bound that did not — has been spent: validateModelDeploymentSingleRole is gone and the group rules
// below took its place, so lifting the bound really was deleting one call.
func validateModelDeployment(
	md *workercore.ModelDeployment, existingRoles sets.Set[string],
) field.ErrorList {
	errs := validateModelDeploymentRoles(md)
	errs = append(errs, validateModelDeploymentKVCache(md)...)
	errs = append(errs, validateModelDeploymentRolesCount(md)...)
	errs = append(errs, validateModelDeploymentRoleNames(md)...)
	errs = append(errs, validateModelDeploymentRoleServiceNames(md, existingRoles)...)
	errs = append(errs, validateModelDeploymentRoleKinds(md)...)

	return errs
}

// validateModelDeploymentKVCache refuses a Binding reference with an empty name.
//
// THIS RULE IS HERE ONLY BECAUSE THE SCHEMA CANNOT HOLD IT. Every other required string in the spec
// gets its lower bound from a minLength marker, which is earlier, cheaper and applies to every
// client; this field's type is upstream's core.LocalObjectReference, and a marker cannot be attached
// to a struct this API does not own. So the exception is forced by the type, not chosen.
//
// The bound itself is needed because `required` makes the KEY present, not the VALUE non-empty: an
// object carrying `poolRef: {name: ""}` satisfies the schema completely.
func validateModelDeploymentKVCache(md *workercore.ModelDeployment) field.ErrorList {
	if md.Spec.KVCache.PoolRef.Name != "" {
		return nil
	}

	return field.ErrorList{field.Required(
		field.NewPath("spec", "kvCache", "poolRef", "name"),
		"the Binding name must not be empty: the Binding is the authorization point for reaching "+
			"the pool, and an empty reference names none",
	)}
}

// modelDeploymentMaxRoles is the number of roles one deployment may declare, and it is KUEUE'S
// number rather than this project's: each role becomes one PodSet of the group's single Workload,
// and Workload.spec.podSets is capped at ten.
//
// THE VALUE MUST BE READ OFF THE KUEUE THAT RUNS, not off the type library this module compiles
// against. The two are deliberately different versions here, and this cap has moved between Kueue
// releases, so go-to-definition answers a question about the wrong tree: the number to check against
// is the podSets maxItems in the Workload CRD the cluster has installed.
const modelDeploymentMaxRoles = 10

// validateModelDeploymentRolesCount caps the number of roles.
//
// The bound lives here rather than as a schema maxItems for two reasons, both inherited from the
// length-1 rule this replaces: the refusal can name whose limit it is, which is the difference
// between a user filing a bug against this operator and a user reading Kueue's; and tracking an
// upstream number is a webhook edit rather than a CRD schema change every stored object must
// survive.
//
// Only the UPPER bound is here. The lower one is the schema's minItems, which runs before this
// handler is reached, so a second check for it would be a branch nothing can enter.
func validateModelDeploymentRolesCount(md *workercore.ModelDeployment) field.ErrorList {
	if len(md.Spec.Roles) <= modelDeploymentMaxRoles {
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "roles"), len(md.Spec.Roles),
		fmt.Sprintf(
			"at most %d roles: every role becomes one PodSet of the deployment's single Kueue "+
				"Workload, and Kueue caps Workload.spec.podSets at %d — an extra role produces a "+
				"Workload the API server will not store, which surfaces as a Workload that is never "+
				"created rather than as an error on this object",
			modelDeploymentMaxRoles, modelDeploymentMaxRoles,
		),
	)}
}

// validateModelDeploymentRoleNames refuses two roles sharing a name.
//
// The name becomes the Kueue PodSet name, so a duplicate does not collide — it MERGES. Two roles
// with one name are grouped into a single PodSet whose count is their sum, and the deployment then
// runs a shape nobody asked for with nothing reporting it.
//
// The name's SHAPE — Kueue's PodSetReference pattern and its 63-character bound — is the schema's,
// not restated here. Structural validation runs before this handler, so a pattern check here could
// never fire.
//
// NEITHER DOES THIS ONE, THROUGH THE API SERVER. `Roles` is marked `+listType=map +listMapKey=name`,
// and the API server enforces that key's uniqueness during validation — before any webhook. Measured
// on a live cluster: two roles named `worker` come back as `spec.roles[1]: Duplicate value`, from the
// schema, and this function's message is never seen. It is kept as the backstop for the marker being
// dropped, which would otherwise remove the guarantee with nothing failing; a case asserting the
// refusal must assert the SCHEMA's wording, because that is the layer that owns it in practice.
func validateModelDeploymentRoleNames(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	seen := make(map[string]int, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		name := md.Spec.Roles[i].Name
		first, dup := seen[name]
		if !dup {
			seen[name] = i
			continue
		}

		// THE SECOND ARGUMENT IS THE VALUE, and the API server renders it verbatim after
		// "Duplicate value: ". An explanation passed here comes back as a sentence quoted as if it
		// were the name the user typed. The explanation goes in an Invalid error beside it, where
		// the Detail is a place for prose.
		errs = append(errs,
			field.Duplicate(rolesPath.Index(i).Child("name"), name),
			field.Invalid(rolesPath.Index(i).Child("name"), name, fmt.Sprintf(
				"a role name is its Kueue PodSet name, so this does not collide with %s — it MERGES, "+
					"grouping both roles into one PodSet whose count is their sum",
				rolesPath.Index(first).Child("name"))))
	}

	return errs
}

// validateModelDeploymentRoleServiceNames refuses a role whose Service name cannot exist.
//
// Each role is fronted by a Service named `<deployment>-<role>`, and a Service name is a DNS-1035
// LABEL: at most 63 characters, where an object name runs to 253. Both halves are legal on their own
// and their concatenation is not -- a 40-character deployment with a 30-character role is 71 -- so
// this is the one rule neither field can carry alone.
//
// IT IS A NEW CLASS OF FAILURE, WHICH IS WHY IT IS REFUSED HERE. A deployment whose OWN name is too
// long already had no Service, so that case was visible immediately. This one has a working
// deployment-wide Service and a per-role Service the API server rejects on every create, which the
// reconciler retries forever with the cause two objects away from the field that caused it.
//
// THE WHOLE SHAPE IS CHECKED, NOT THE LENGTH. An earlier version checked only the length, on the
// reasoning that both halves are already patterned to a DNS label -- which is false for the
// deployment's half: an object name is a DNS SUBDOMAIN, so `team.model.serving` is a legal
// ModelDeployment name whose combined Service name carries a dot and is refused on every create,
// well inside 63 characters. IsDNS1035Label covers the length and the alphabet together.
//
// A ROLE THE OBJECT ALREADY HAD IS EXEMPT. This rule arrived after objects could exist, the
// deployment's own name is immutable, and every later edit runs through here -- so refusing a stored
// role would strand the object with no edit able to rescue it, including the edit that removes the
// role. New and renamed roles are still refused, which is where the mistake is actually made.
func validateModelDeploymentRoleServiceNames(
	md *workercore.ModelDeployment, existingRoles sets.Set[string],
) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := md.Spec.Roles[i].Name
		if existingRoles.Has(role) {
			continue
		}

		name := md.Name + "-" + role
		why := validation.IsDNS1035Label(name)
		if len(why) == 0 {
			continue
		}

		errs = append(errs, field.Invalid(
			rolesPath.Index(i).Child("name"), role, fmt.Sprintf(
				"this role is fronted by a Service named %q (%d characters), which is not a valid "+
					"Service name: %s. Shorten or rename this role, or the deployment",
				name, len(name), strings.Join(why, "; "))))
	}

	return errs
}

// validateModelDeploymentRoleKinds holds the two rules about what a role is told it is.
//
// The first is about the SET: a server serves whole requests by itself, so "one plain server plus a
// prefiller" names no shape anything consumes, and accepting it would mean rendering a transfer
// configuration whose meaning is undefined.
//
// The second is about the ENGINE: a kind is only real if the engine's rendering has a term for it.
// Refusing here is the whole point — the alternative is a container that starts, looks configured,
// and behaves as though the role were never declared, or one the engine rejects at start-up with a
// message naming none of this.
func validateModelDeploymentRoleKinds(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")

	hasServer, hasOther := false, false
	for i := range md.Spec.Roles {
		if workerctrl.ModelDeploymentEffectiveRoleKind(&md.Spec.Roles[i]) == workercore.ModelDeploymentRoleKindServer {
			hasServer = true
		} else {
			hasOther = true
		}
	}

	for i := range md.Spec.Roles {
		role, kindPath := &md.Spec.Roles[i], rolesPath.Index(i).Child("kind")
		kind := workerctrl.ModelDeploymentEffectiveRoleKind(role)

		if hasServer && hasOther {
			errs = append(errs, field.Invalid(kindPath, kind, fmt.Sprintf(
				"%q cannot be combined with another kind in one deployment: a %q role serves whole "+
					"requests by itself, so a deployment holding one beside a %q or %q role "+
					"describes no arrangement the engines' KV transfer configuration can express",
				workercore.ModelDeploymentRoleKindServer, workercore.ModelDeploymentRoleKindServer,
				workercore.ModelDeploymentRoleKindPrefill, workercore.ModelDeploymentRoleKindDecode,
			)))

			continue
		}

		if !workerctrl.ModelDeploymentSupportsRoleKind(md.Spec.Engine, kind) {
			errs = append(errs, field.Invalid(kindPath, kind, fmt.Sprintf(
				"engine %q has no rendering term for kind %q; accepting it would leave the container "+
					"looking configured and behaving as though the role were never declared",
				md.Spec.Engine, kind,
			)))
		}
	}

	return errs
}

// validateModelDeploymentRoles holds the per-role rules that every later version keeps.
func validateModelDeploymentRoles(md *workercore.ModelDeployment) field.ErrorList {
	errs := make(field.ErrorList, 0, len(md.Spec.Roles))

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role, rolePath := &md.Spec.Roles[i], rolesPath.Index(i)

		errs = append(errs, validateModelDeploymentRoleExtraArgs(md.Spec.Engine, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleEnv(md.Spec.Engine, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleTemplate(role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleResources(role, rolePath)...)
	}

	return errs
}

// validateModelDeploymentRoleExtraArgs refuses an append-tier argument the operator owns.
//
// A silent merge is what this prevents, and the reason is diagnosability rather than tidiness: two
// values for one connector argument leave no way to tell which one won, and the user who wrote the
// second has no way to learn the first exists.
func validateModelDeploymentRoleExtraArgs(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	argsPath := rolePath.Child("extraArgs")
	for i, arg := range role.ExtraArgs {
		name := workerctrl.ModelDeploymentArgName(arg)
		if !workerctrl.ModelDeploymentOwnsArg(engine, name) {
			continue
		}

		errs = append(errs, field.Invalid(argsPath.Index(i), arg, fmt.Sprintf(
			"%q is set by the operator for engine %q and must not be supplied here, because two "+
				"values for it cannot be told apart; replace the whole command line through "+
				"%s to own it instead",
			name, engine, rolePath.Child("template", "command"),
		)))
	}

	return errs
}

// validateModelDeploymentRoleEnv refuses an append-tier environment entry the operator owns.
//
// Ownership here is about what a key destroys rather than what it duplicates: the config-path
// variable is the only pointer to the file the operator wrote, so re-pointing it swaps the entire
// client configuration for another file's and moves every symptom one layer away from its cause.
// Keys the operator merely defaults are not owned, so a user's value wins there with no refusal.
//
// BOTH TIERS ARE CHECKED BECAUSE THE RENDERER READS BOTH. mergeModelDeploymentEnv appends role.Env
// and role.Template.Env into one list and then skips every owned name in it. Validating only the
// append tier let an owned key arrive through the overlay, pass admission, and be dropped at render
// time with no refusal and no event -- exactly the silent outcome this rule exists to prevent,
// reached by the one path the rule did not cover.
//
// The set refused here must equal the set the renderer drops. The renderer drops unconditionally,
// including when a role takes over the command line, so this refuses unconditionally too: a
// disagreement between the two sets is what produces a silent drop, whichever way it leans.
func validateModelDeploymentRoleEnv(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	errs := validateModelDeploymentOwnedEnv(engine, role.Env, rolePath, rolePath.Child("env"))
	if role.Template != nil {
		errs = append(errs, validateModelDeploymentOwnedEnv(
			engine, role.Template.Env, rolePath, rolePath.Child("template", "env"),
		)...)
	}

	return errs
}

// validateModelDeploymentOwnedEnv refuses every owned name in one tier of environment entries.
//
// envPath is passed rather than derived so that the refusal names the tier the user actually wrote
// in: a message pointing at roles[i].env for a value supplied under roles[i].template.env sends the
// reader to a field they never touched.
func validateModelDeploymentOwnedEnv(
	engine string, env []workercore.InstanceEnvVar, rolePath, envPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for i := range env {
		name := env[i].Name
		if !workerctrl.ModelDeploymentOwnsEnv(engine, name) {
			continue
		}

		errs = append(errs, field.Invalid(envPath.Index(i), name, fmt.Sprintf(
			"%q is set by the operator for engine %q and must not be supplied here, because it "+
				"selects the client configuration the operator rendered; replace the whole "+
				"command line through %s to own it instead",
			name, engine, rolePath.Child("template", "command"),
		)))
	}

	return errs
}

// validateModelDeploymentRoleTemplate keeps the scheduling scalars out of the overlay tier.
//
// The template may override container content and never the resource request. Inferring the request
// from container content would make the admission feasibility check read a ledger that does not
// match reality, so the refusal names the structured field that does decide it.
func validateModelDeploymentRoleTemplate(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if role.Template == nil || role.Template.Resources == nil {
		return nil
	}

	return field.ErrorList{field.Invalid(
		rolePath.Child("template", "resources"), role.Template.Resources,
		fmt.Sprintf(
			"the accelerator request belongs in %s and the rest is derived from %s; a template "+
				"that could shadow either would make the feasibility check read a ledger that "+
				"does not match reality",
			rolePath.Child("resources"), rolePath.Child("instanceType"),
		),
	)}
}

// validateModelDeploymentRoleResources refuses a request that asks for hardware partitioning and
// software slicing at once.
//
// One accelerator cannot serve both, so a request naming both has no correct reading — and the
// operator's renderer resolves the pair by precedence, which would silently grant the profile and
// discard the percentages.
//
// THE TWO RULES THAT NEED THE InstanceType ARE NOT HERE, and that is a property of what they read
// rather than a split of the subject. This one is decided from the request alone, so it holds for a
// deployment being deleted and for one whose type has not been read; the other two are in
// validateRoleResourcesAgainstInstanceType, which is reached only when the type could be read.
func validateModelDeploymentRoleResources(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	ress := role.Resources
	if ress == nil || ress.AcceleratorPartitionedProfile == "" {
		return nil
	}

	if ress.AcceleratorSlicedMemoryPercentage == 0 && ress.AcceleratorSlicedCoresPercentage == 0 {
		return nil
	}

	ressPath := rolePath.Child("resources")

	return field.ErrorList{field.Invalid(
		ressPath.Child("acceleratorPartitionedProfile"), ress.AcceleratorPartitionedProfile,
		fmt.Sprintf(
			"a partition profile cannot be combined with %s or %s: hardware partitioning and "+
				"software slicing cannot both apply to one accelerator",
			ressPath.Child("acceleratorSlicedMemoryPercentage"),
			ressPath.Child("acceleratorSlicedCoresPercentage"),
		),
	)}
}

// validateRoleResourcesAgainstInstanceTypes applies the two rules that need the InstanceType a role
// names, and is the only validation path here that reads another object.
//
// THE REFUSAL LANDS AT THE API INSTEAD OF DEEPER IN THE CHAIN, which is the whole of what these two
// rules buy. Without them an infeasible request is still refused -- by the scheduling chain's own
// gates, on a Workload, naming neither the deployment nor the field the user wrote. The outcome was
// never wrong; the message was, and a message an operator cannot act on costs the time to find what
// this one states.
//
// AN OBJECT BEING DELETED IS NOT JUDGED BY THESE TWO. A rule that reads the type refuses when the
// type is absent, so leaving them on would let a deleted InstanceType block the very update that
// clears this deployment's finalizer. That is the same reasoning Default states for declining
// wholesale, applied to the part of validation that acquired the same dependency.
//
// A ROLE NAMING NO TYPE, OR ASKING FOR NOTHING, IS SKIPPED rather than looked up. An empty name is a
// field error the object-only rules already report, and looking it up would fail on the request and
// answer with a message about reading the cluster instead of about the name.
//
// A TYPE THAT COULD NOT BE READ STOPS THE PASS RATHER THAN BECOMING A SECOND REFUSAL. getInstanceType
// already distinguishes a name that does not exist from a cluster that would not answer, and the
// defaulting half raises the same error first on every path that reaches storage.
func (r *ModelDeploymentWebhook) validateRoleResourcesAgainstInstanceTypes(
	ctx context.Context, md *workercore.ModelDeployment,
) (field.ErrorList, error) {
	if md.DeletionTimestamp != nil {
		return nil, nil
	}

	rolesPath := field.NewPath("spec", "roles")

	var errs field.ErrorList
	seen := make(map[string]*worker.InstanceType, 1)
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.InstanceType == "" || role.Resources == nil {
			continue
		}

		instType, ok := seen[role.InstanceType]
		if !ok {
			var err error
			if instType, err = r.getInstanceType(ctx, role.InstanceType, i); err != nil {
				return nil, err
			}
			seen[role.InstanceType] = instType
		}
		if !instType.Spec.Acceleratable {
			continue
		}

		// AN UNCOMPUTED DETAIL IS A TRANSIENT REFUSAL, NOT A PERMANENT ONE. An empty detail is the
		// not-yet-synced state rather than "this type offers no modes", and the two are told apart by
		// the same helper the Instance webhook asks -- a second reading of the same emptiness is what
		// would let them drift. Written without it, a valid deployment applied in the seconds after
		// its pool appears is refused for a mode the type does in fact offer, and the user's fix is
		// to wait, which a field error does not say.
		if slicingRequestNotReady(instType, roleInstanceResources(role.Resources)) {
			return nil, kerrors.NewInternalError(
				fmt.Errorf("instance type %s is not ready yet; retry", instType.Name))
		}

		errs = append(errs, validateRoleResourcesAgainstInstanceType(
			instType, role.Resources, rolesPath.Index(i).Child("resources"))...)
	}

	return append(errs, validateModelDeploymentPairCannotShareOneAccelerator(md, seen, rolesPath)...), nil
}

// validateModelDeploymentPairCannotShareOneAccelerator refuses a prefill and a decode role that both
// ask for a logical slice of an accelerator their two InstanceTypes can both select.
//
// THE PAIR EXISTS TO SEPARATE TWO WORKLOADS THAT CONTEND, so letting both hold a slice of ONE card
// undoes the split while the object still reads as disaggregated. Nothing errors: the replicas start,
// they serve, and the prefiller's bursts land on the same silicon the decoder is streaming from.
//
// THE QUALIFIER IS A DISJOINT ACCELERATOR POPULATION, AND TWO DIFFERENT NAMES DO NOT ESTABLISH ONE.
// An admin can author an InstanceType against an acceleratorGroup a derived type already covers, and
// the two are then two views of one accelerator. A rule keyed on the names being different leaves
// open precisely the case it was written to close, and one with no qualifier at all over-refuses:
// it blocks the heterogeneous shape that roles on two instanceTypes exist to enable.
//
// A PARTITIONED PAIR IS ACCEPTED EVEN ON ONE CARD. Hardware partitions are isolated from each other
// by the device, which is the property this rule is about; a logical slice is not, and that is the
// whole difference. A whole-card pair is accepted for the same reason at a coarser grain.
//
// A SINGLE-ROLE DEPLOYMENT IS UNAFFECTED. This is a statement about a pair, and a lone role sharing
// a card with itself is what a slice is for.
//
// EVERY PREFILL IS COMPARED WITH EVERY DECODE, because a deployment may declare more than one role of
// either kind: the kind rule refuses only mixing a server role with the others, not two prefillers.
// Keeping one role per kind would compare whichever pair happened to be declared last and admit a
// violating pair declared anywhere before it.
func validateModelDeploymentPairCannotShareOneAccelerator(
	md *workercore.ModelDeployment,
	types map[string]*worker.InstanceType,
	rolesPath *field.Path,
) field.ErrorList {
	var prefills, decodes []*workercore.ModelDeploymentRole
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if !roleRequestsALogicalSlice(role) {
			continue
		}
		switch role.Kind {
		case workercore.ModelDeploymentRoleKindPrefill:
			prefills = append(prefills, role)
		case workercore.ModelDeploymentRoleKindDecode:
			decodes = append(decodes, role)
		}
	}

	var errs field.ErrorList
	for _, prefill := range prefills {
		prefillType := types[prefill.InstanceType]
		if prefillType == nil {
			// A type that could not be read, or a role naming none: the rules that report those have
			// already done so, and answering this question from half the pair would be a guess.
			continue
		}
		for _, decode := range decodes {
			decodeType := types[decode.InstanceType]
			if decodeType == nil ||
				prefillType.Spec.AcceleratorGroup != decodeType.Spec.AcceleratorGroup {
				continue
			}

			errs = append(errs, field.Invalid(
				rolesPath, prefill.Name+", "+decode.Name,
				fmt.Sprintf(
					"roles %q and %q both request a logical slice, and instance types %q and %q draw from "+
						"the same accelerator group %q — so the two can land on one card, which is what the "+
						"prefill/decode split exists to prevent. Give one of them an instance type over a "+
						"different accelerator group, or request whole cards or hardware partition profiles, "+
						"which are isolated by the device",
					prefill.Name, decode.Name,
					prefill.InstanceType, decode.InstanceType, prefillType.Spec.AcceleratorGroup,
				),
			))
		}
	}

	return errs
}

// validateModelDeploymentBarrierIsInstallable refuses a deployment whose roles span several
// instanceTypes when nothing in the cluster can gate the set.
//
// SEVERAL instanceTypes ARE SEVERAL POD GROUPS AND SEVERAL WORKLOADS, and Kueue's own atomicity
// covers one group. What relates them is the joint-admission check, and that check reaches a Workload
// only through a ClusterQueue that references it -- which the queue reconciler does only while the
// derived-from-node setting is on. With it off an administrator authors queues through the
// InstanceType API, no queue carries the check, and the barrier is not installed anywhere.
//
// THE SHAPE IS REFUSED RATHER THAN ADMITTED UNGUARDED. Admitting it would let a prefiller start and
// serve while its decoder waits for capacity that never arrives -- a deployment that reads as
// half-started and is in fact never going to finish, with nothing naming the reason. A refusal at the
// API names the setting, which is the one thing the operator can act on.
//
// A SINGLE-instanceType DEPLOYMENT IS UNAFFECTED whatever the setting says: it is one group, and one
// group is admitted as a unit by Kueue without help from anything here.
func validateModelDeploymentBarrierIsInstallable(
	ctx context.Context, md *workercore.ModelDeployment,
) field.ErrorList {
	types := sets.New[string]()
	for i := range md.Spec.Roles {
		types.Insert(md.Spec.Roles[i].InstanceType)
	}
	if types.Len() < 2 {
		return nil
	}

	if settings.InstanceTypeDerivedFromNode.ShouldValueBool(ctx) {
		return nil
	}

	return field.ErrorList{field.Forbidden(
		field.NewPath("spec", "roles"),
		"roles on several instance types are several Kueue workloads, and what admits them together "+
			"is an admission check referenced from the queues this operator derives. The "+
			"instance-type-derived-from-node setting is off, so no queue carries it and the "+
			"deployment could start one role and never the other. Put every role on one instance "+
			"type, or turn that setting on",
	)}
}

// roleRequestsALogicalSlice reports whether a role asks for a fraction of a card in software.
//
// Either percentage alone is a slice request: the defaulting half copies one into the other, so a
// rule reading only the memory one would miss a compute-only request entirely.
func roleRequestsALogicalSlice(role *workercore.ModelDeploymentRole) bool {
	ress := role.Resources

	return ress != nil &&
		(ress.AcceleratorSlicedMemoryPercentage != 0 || ress.AcceleratorSlicedCoresPercentage != 0)
}

// roleInstanceResources projects a role's request onto the accelerator fields of InstanceResources.
//
// It exists so the readiness question is asked through the helper that already answers it for an
// Instance, rather than by a second reading of the same emptiness that could come to disagree.
// ModelDeploymentRoleResources mirrors those fields by name and by meaning, which is what makes this
// a rename rather than a translation -- and the reason the projection carries no CPU, RAM or local
// storage is that a role does not declare them.
func roleInstanceResources(ress *workercore.ModelDeploymentRoleResources) *workercore.InstanceResources {
	return &workercore.InstanceResources{
		Accelerator:                       ress.Accelerator,
		AcceleratorSlicedMemoryPercentage: ress.AcceleratorSlicedMemoryPercentage,
		AcceleratorSlicedCoresPercentage:  ress.AcceleratorSlicedCoresPercentage,
		AcceleratorPartitionedProfile:     ress.AcceleratorPartitionedProfile,
	}
}

// validateRoleResourcesAgainstInstanceType refuses a request the named InstanceType cannot serve:
// one asking for a mode it does not offer, and one asking for more cards than it hands out at once.
//
// THE MODE IS DECIDED BY WHAT THE REQUEST NAMES, not by what the type has. A partition profile makes
// the request a partitioned one, a non-zero percentage makes it a sliced one, and a request naming
// neither is a whole-card one that every acceleratable type offers. The pair naming both is refused
// before this by the rule that owns that contradiction, so the branches here do not overlap.
//
// BOTH REFUSALS NAME THE TYPE, because the field alone does not locate the problem: the same request
// is correct against another type, and what the operator has to change is one of the two.
func validateRoleResourcesAgainstInstanceType(
	instType *worker.InstanceType,
	ress *workercore.ModelDeploymentRoleResources,
	ressPath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	switch {
	case ress.AcceleratorPartitionedProfile != "":
		errs = append(errs, validateRolePartitionProfileOffered(instType, ress, ressPath)...)
	case ress.AcceleratorSlicedMemoryPercentage != 0 || ress.AcceleratorSlicedCoresPercentage != 0:
		// A pool with no logically sliceable card cannot serve the request at all: admitted, such a
		// role stays Pending forever rather than being reshaped into a whole-card one.
		if !instType.Status.Detail.IsLogicallySliceable() {
			errs = append(errs, field.Forbidden(
				ressPath.Child("acceleratorSlicedMemoryPercentage"),
				fmt.Sprintf("instance type %s does not offer logical slicing", instType.Name)))
		}
	}

	// THE CEILING IS CARRIED IN THE MESSAGE rather than left for the reader to look up. "Exceeds the
	// maximum" states that the request was wrong; the number states what would be right, and the
	// difference is whether the next attempt is a guess.
	if ress.Accelerator != nil {
		if ceiling := instType.Status.Accelerator.OnceMaxRequest; ress.Accelerator.Cmp(ceiling) > 0 {
			errs = append(errs, field.Invalid(
				ressPath.Child("accelerator"), ress.Accelerator.String(),
				fmt.Sprintf("instance type %s hands out at most %s accelerator(s) at once",
					instType.Name, ceiling.String())))
		}
	}

	return errs
}

// validateRolePartitionProfileOffered checks a partition request against the profiles the pool
// reports.
//
// THE MISSING CAPABILITY IS REPORTED BEFORE THE PROFILE LOOKUP. A pool whose cards are not in a
// partitioning mode offers an empty profile set, and reporting that as "this profile is not offered"
// reads as a typo -- sending the operator to correct a name that was never the problem.
func validateRolePartitionProfileOffered(
	instType *worker.InstanceType,
	ress *workercore.ModelDeploymentRoleResources,
	ressPath *field.Path,
) field.ErrorList {
	profilePath := ressPath.Child("acceleratorPartitionedProfile")

	if !instType.Status.Detail.IsPhysicallySliceable() {
		return field.ErrorList{field.Forbidden(profilePath,
			fmt.Sprintf("instance type %s does not offer hardware partitioning", instType.Name))}
	}

	profiles := instType.Status.Detail.SlicedDetail.Physical.Profiles
	offered := make([]string, 0, len(profiles))
	for _, p := range profiles {
		if p.Name == ress.AcceleratorPartitionedProfile {
			return nil
		}
		offered = append(offered, p.Name)
	}

	return field.ErrorList{field.Invalid(profilePath, ress.AcceleratorPartitionedProfile,
		fmt.Sprintf("instance type %s does not offer this partition profile; offered: %v",
			instType.Name, offered))}
}
