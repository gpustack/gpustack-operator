package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/apimachinery/pkg/util/validation/field"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/utils/ctrlclix"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
)

// ModelDeploymentWebhook validates a v1alpha1.ModelDeployment.
//
// It is validating only. Every default this API has — the connector discriminator and the replica
// count — is a CRD schema default, and every enum is a CRD schema enum, so there is no mutating
// half. For the enums a webhook could not help even if one were written: structural schema
// validation runs before the validating admission chain, so a value outside an enum is refused
// before this handler is reached.
//
// What is left here is what a schema cannot express: a bound that must carry an actionable message,
// a comparison between two entries of a list, and a collision between what a user supplies and what
// the operator owns.
//
// nolint: lll
// +k8s:webhook-gen:validating:group="worker.gpustack.ai",version="v1alpha1",resource="modeldeployments",scope="Namespaced"
// +k8s:webhook-gen:validating:operations=["CREATE","UPDATE"],failurePolicy="Fail",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
type ModelDeploymentWebhook struct {
	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (r *ModelDeploymentWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	// The client is here for ONE rule: an acceleratorKey has to be resolved against the flavors its
	// pool actually offers, and that is a question about the cluster rather than about the object.
	// Every other rule below is answered from the object itself and from the renderer's tables.
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()

	return &workercore.ModelDeployment{}, nil
}

var _ ctrladmission.Validator[runtime.Object] = (*ModelDeploymentWebhook)(nil)

func (r *ModelDeploymentWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := obj.(*workercore.ModelDeployment)

	if errs := r.validate(ctx, md, nil); len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
}

func (r *ModelDeploymentWebhook) ValidateUpdate(
	ctx context.Context, oldObj, newObj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := newObj.(*workercore.ModelDeployment)

	// There is nothing immutable to check. An Instance's template is frozen after creation, but that
	// is a rule the Instance webhook enforces on InstanceSpec rather than a property of any template
	// type, so this CR simply does not carry it: a mutable template is what makes a rollout possible.
	//
	// The role names the object ALREADY had are carried in, and the reason is that a rule this handler
	// gained after an object was stored must not be able to strand that object. The Service-name rule
	// is the one that could: a deployment's own name is immutable, so a role whose combined name is
	// too long could never be shortened, and every later edit -- including one that removes the
	// offending role -- would be refused. That is worse than the reconcile failure the rule prevents.
	if errs := r.validate(ctx, md, modelDeploymentRoleNames(oldObj)); len(errs) > 0 {
		return nil, kerrors.NewInvalid(md.GroupVersionKind().GroupKind(), md.Name, errs)
	}

	return nil, nil
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

// validate runs the rules answerable from the object, and asks the cluster only once they hold.
//
// The order is not cosmetic: the accelerator-key rule resolves ONE ClusterQueue because the
// identical-instanceType rule has already passed, and a malformed object would otherwise be told
// "this pool does not offer that key" alongside the reason it has no single pool to speak of.
func (r *ModelDeploymentWebhook) validate(
	ctx context.Context, md *workercore.ModelDeployment, existingRoles sets.Set[string],
) field.ErrorList {
	errs := validateModelDeployment(md, existingRoles)
	if len(errs) > 0 {
		return errs
	}

	return r.validateModelDeploymentAcceleratorKeys(ctx, md)
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
	errs = append(errs, validateModelDeploymentRoleInstanceTypes(md)...)
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

// validateModelDeploymentRoleInstanceTypes refuses roles spread over two pools.
//
// ONE KUEUE WORKLOAD CARRIES ONE queueName, and the queue name is derived from the instanceType. So
// two roles on two instanceTypes cannot be one pod group, and therefore cannot be admitted
// atomically — the property this whole shape exists to provide. Kueue enforces the same rule on the
// Pods, unretryably, so letting it through would trade a refusal here for a group that never
// assembles.
//
// The refusal names acceleratorKey, because "these roles want different hardware" is the reason a
// user reaches for two instanceTypes, and it is expressible INSIDE one pool: Kueue assigns a
// ResourceFlavor per PodSet.
func validateModelDeploymentRoleInstanceTypes(md *workercore.ModelDeployment) field.ErrorList {
	if len(md.Spec.Roles) < 2 {
		return nil
	}

	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	first := md.Spec.Roles[0].InstanceType
	for i := 1; i < len(md.Spec.Roles); i++ {
		if md.Spec.Roles[i].InstanceType == first {
			continue
		}

		errs = append(errs, field.Invalid(
			rolesPath.Index(i).Child("instanceType"), md.Spec.Roles[i].InstanceType,
			fmt.Sprintf(
				"every role must name the same instanceType (%s is %q): the roles form one Kueue "+
					"Workload, one Workload carries one queue name, and the queue name comes from "+
					"the instanceType — so roles on two of them cannot be admitted together at all. "+
					"To put roles on different accelerator models within one pool, leave the "+
					"instanceType alone and set %s",
				rolesPath.Index(0).Child("instanceType"), first,
				rolesPath.Index(i).Child("acceleratorKey"),
			),
		))
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

// validateModelDeploymentAcceleratorKeys resolves each role's acceleratorKey against the accelerator
// keys its pool actually offers.
//
// THE RULE EXISTS BECAUSE AN UNKNOWN KEY DOES NOT FAIL — IT IS IGNORED. Kueue's flavor assignment
// keeps only those nodeSelector keys a candidate flavor's own nodeLabels carry and drops the rest,
// so a key no flavor offers stops being a constraint: an arbitrary flavor is assigned, the Workload
// is admitted, and the Pod then sits Pending at the scheduler because the real Node label does not
// match. The mistake would surface two gates downstream with nothing naming it.
//
// AN EMPTY READ IS NEVER A REFUSAL, and there are three of them. A queue that does not exist yet, a
// queue whose resource groups the NodeQueue reconciler has not filled, and a pool whose flavors have
// all gone are all states a fresh or shrinking cluster passes through, and the key may become valid
// a minute later. Refusing on any of them would make this rule report "the pool does not offer that"
// for a pool that has not answered. A key that stays wrong is then the per-accelerator check's
// Retry, which is the transient-shortage path already.
//
// NO FLAVOR IS THE EXEMPTION; NO ACCELERATOR KEY IS NOT. A pool whose flavors are read and carry no
// accelerator key at all has answered — it is a CPU pool — and a role asking one of it is asking for
// hardware the pool does not have. That is why the count of flavors read is carried out of the read
// rather than inferred from the key set being empty: the two are different facts and only one of
// them is a reason to stay silent.
//
// Roles all share one instanceType by the time this runs, so one queue is read rather than one per
// role.
func (r *ModelDeploymentWebhook) validateModelDeploymentAcceleratorKeys(
	ctx context.Context, md *workercore.ModelDeployment,
) field.ErrorList {
	if !slices.ContainsFunc(md.Spec.Roles, func(role workercore.ModelDeploymentRole) bool {
		return role.AcceleratorKey != ""
	}) {
		return nil
	}

	cq, err := r.getClusterQueue(ctx, md.Spec.Roles[0].InstanceType)
	if err != nil {
		return field.ErrorList{field.InternalError(
			field.NewPath("spec", "roles").Index(0).Child("instanceType"), err)}
	}
	if cq == nil {
		return nil
	}

	offered, read, err := r.poolAcceleratorKeys(ctx, cq)
	if err != nil {
		return field.ErrorList{field.InternalError(
			field.NewPath("spec", "roles").Index(0).Child("instanceType"), err)}
	}
	if read == 0 {
		return nil
	}

	// A CPU pool reaches here with nothing to list, and saying so is the actionable half of the
	// message: "offers []" reads like a pool that has not answered, which is the one state this rule
	// deliberately stays silent about.
	offers := fmt.Sprintf("offers %v", sets.List(offered))
	if offered.Len() == 0 {
		offers = "carries no accelerator at all"
	}

	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		key := md.Spec.Roles[i].AcceleratorKey
		if key == "" || offered.Has(key) {
			continue
		}

		errs = append(errs, field.Invalid(
			rolesPath.Index(i).Child("acceleratorKey"), key, fmt.Sprintf(
				"the pool of instanceType %q %s: Kueue keeps only those nodeSelector keys a "+
					"candidate flavor pins and drops the rest, so a key none of them offers is not a "+
					"constraint that fails — the deployment would be admitted onto an arbitrary "+
					"accelerator model and its Pods would then sit Pending at the scheduler",
				md.Spec.Roles[0].InstanceType, offers,
			)))
	}

	return errs
}

// getClusterQueue reads the ClusterQueue backing an InstanceType, which carries the same name. A nil
// queue and a nil error mean it is not there, which this handler treats as "no answer yet" rather
// than as a refusal.
func (r *ModelDeploymentWebhook) getClusterQueue(
	ctx context.Context, instanceType string,
) (*kueue.ClusterQueue, error) {
	cq := new(kueue.ClusterQueue)
	key := ctrlcli.ObjectKey{Name: instanceType}

	err := r.Client.Get(ctx, key, cq)
	if err == nil {
		return cq, nil
	}
	if !kerrors.IsNotFound(err) {
		return nil, fmt.Errorf("get cluster queue %q: %w", instanceType, err)
	}

	// A queue created moments ago is absent from the cache before it is absent from the API server,
	// and this is an admission path: the uncached read is what stops a correct object from being
	// judged against a view that has not caught up.
	if err = r.APIReader.Get(ctx, key, cq, ctrlclix.WithoutQuorum); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("get cluster queue %q: %w", instanceType, err)
	}

	return cq, nil
}

// poolAcceleratorKeys collects the accelerator keys the queue's own ResourceFlavors pin, and returns
// how many flavors it actually read beside them.
//
// The flavors are read from the queue's resource groups rather than by re-deriving the pool's label
// selector, because THIS SET IS THE ONE KUEUE WILL CHOOSE FROM: a flavor that exists but is not
// referenced by the queue is not a candidate for a Workload in it, so offering its key here would
// accept a constraint that still ends up dropped.
//
// A flavor that has since been deleted is skipped rather than failing the read, and it does not
// count: the queue drops the reference on its next reconcile, and refusing in between would refuse
// on a state that is already being repaired.
func (r *ModelDeploymentWebhook) poolAcceleratorKeys(
	ctx context.Context, cq *kueue.ClusterQueue,
) (sets.Set[string], int, error) {
	keys, read := sets.New[string](), 0

	seen := sets.New[kueue.ResourceFlavorReference]()
	for i := range cq.Spec.ResourceGroups {
		for _, quotas := range cq.Spec.ResourceGroups[i].Flavors {
			if seen.Has(quotas.Name) {
				continue
			}
			seen.Insert(quotas.Name)

			rf := new(kueue.ResourceFlavor)
			if err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: string(quotas.Name)}, rf); err != nil {
				if kerrors.IsNotFound(err) {
					continue
				}

				return nil, 0, fmt.Errorf("get resource flavor %q: %w", quotas.Name, err)
			}
			read++
			keys.Insert(nodefeature.ExtractAcceleratableKeys(rf.Spec.NodeLabels)...)
		}
	}

	return keys, read, nil
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
// BOTH TIERS ARE CHECKED BECAUSE THE RENDERER READS BOTH. mergeModelDeploymentEnv appends
// role.Env and role.Template.Env into one list and then skips every owned name in it. Validating
// only the append tier let an owned key arrive through the overlay, pass admission, and be dropped
// at render time with no refusal and no event -- which is exactly the silent outcome this rule
// exists to prevent, reached by the one path the rule did not cover.
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
// discard the percentages. The two other things worth validating here — that the InstanceType
// actually offers the requested mode, and that the request fits its per-unit ceiling — need the
// InstanceType, so they arrive with the Binding-resolution rule that gives this handler a client.
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
