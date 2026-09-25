package worker

import (
	"context"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
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
	"gpustack.ai/gpustack/pkg/worker/kvcache/inject"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
	"gpustack.ai/gpustack/pkg/worker/kvcache/router"
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
// MOST VALIDATION RULES ARE ANSWERED FROM THE OBJECT ALONE. Resource mode and card count depend on
// an InstanceType. A new cache binding's transport depends on its Binding, pool and backend.
//
// WHAT READS ANOTHER OBJECT READS IT FROM THE API SERVER AND NEVER FROM A CACHE, including defaulting
// and the transport check. A cache decides the outcome in both directions when it is behind — a type
// created moments ago reads as absent and refuses a deployment that names a type that exists, and a
// type recreated under the same name reads with its old acceleratable flag and writes a count from
// it. The second is the worse one, because the wrong value is then persisted.
//
// TYPE READS ARE MEMOIZED BY NAME WITHIN EACH RULE, not per role. The mutating and validating
// halves are separate calls, and a new binding also reads its pool and backend. These reads happen
// on a user-initiated write rather than in a reconcile loop.
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
// THE COST OF LEAVING IT UNSET IS NOT A SMALLER REPLICA, it is a replica that runs outside the
// quota it was supposed to be answerable to. An accelerated pool's ClusterQueue covers only that
// manufacturer's credits, so a role requesting no accelerator requests nothing the queue covers --
// and the manager is configured to ignore what a queue does not declare. Measured on a cluster: the
// Workload of such a replica reserves, reports Admitted, its Pod runs, and the queue's usage of
// those credits stays at zero. Nothing errors anywhere.
//
// THAT IS A QUIETER FAILURE THAN THE ONE IT REPLACES, which is why the rule outlived its first
// reason. While a role was one PodSet of a Workload shared with its siblings, the same shape made
// the scheduler write fewer assignments than the Workload had PodSets, the API refused the update,
// and the Workload requeued forever -- roughly a hundred scheduling cycles a second, parked and
// loud. Each replica carries its own single-PodSet Workload now, so that collision cannot happen;
// what is left is a replica holding accelerators nobody charged it for.
//
// AN EXPLICIT ZERO IS LEFT ALONE. Zero is a value the user wrote, and silently replacing it would
// make the request stop meaning what it says. Validation refuses it on an accelerated type shared
// with another role and points a CPU-only replica at a CPU-only InstanceType instead.
//
// IT RUNS ON UPDATE AS WELL AS CREATE, because an existing role can still have an unset count.
// Adding or removing a role is refused by validation. The defaulter is handed the incoming object
// and no previous one, so it defaults any unset count it finds, which on an object that was stored
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

	// Roles may each name a DIFFERENT InstanceType, so each is defaulted against the type it names
	// and the reads are memoized rather than assumed to be one. That is no longer a defense against
	// running before validation -- it is the shape: a deployment's roles are admitted as a set
	// ACROSS types, one group per replica, so several types in one object is ordinary rather than a
	// state some later rule rejects.
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

// validateModelDeploymentHostAccess applies the Instance host access gates to each role. Access
// already held by a role remains valid when an administrator closes a gate.
func validateModelDeploymentHostAccess(
	old, md *workercore.ModelDeployment, privilegedAllowed, hostPathAllowed bool,
) field.ErrorList {
	var errs field.ErrorList
	oldRoles := make(map[string]*workercore.ModelDeploymentRole)
	if old != nil {
		for i := range old.Spec.Roles {
			oldRoles[old.Spec.Roles[i].Name] = &old.Spec.Roles[i]
		}
	}
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		held := oldRoles[role.Name]
		rolePath := field.NewPath("spec", "roles").Index(i)
		if role.Privileged && !privilegedAllowed && (held == nil || !held.Privileged) {
			errs = append(errs, field.Forbidden(rolePath.Child("privileged"),
				fmt.Sprintf("privileged mode is not allowed: enable the %q setting to allow it",
					settings.InstancePrivilegedAllowed.Name())))
		}
		if hostPathAllowed {
			continue
		}
		for j := range role.AdditionalVolumes {
			want := &role.AdditionalVolumes[j]
			if want.HostPath == nil {
				continue
			}
			covered := false
			if held != nil {
				for k := range held.AdditionalVolumes {
					prev := &held.AdditionalVolumes[k]
					if prev.HostPath != nil && coversHostAccess(
						&workercore.InstanceAdditionalVolume{HostPath: prev.HostPath, SubPath: prev.SubPath, ReadOnly: prev.ReadOnly},
						&workercore.InstanceAdditionalVolume{HostPath: want.HostPath, SubPath: want.SubPath, ReadOnly: want.ReadOnly},
					) {
						covered = true
						break
					}
				}
			}
			if !covered {
				errs = append(errs, field.Forbidden(rolePath.Child("additionalVolumes").Index(j).Child("hostPath"),
					fmt.Sprintf("mounting a host path is not allowed: enable the %q setting to allow it",
						settings.InstanceHostPathVolumeAllowed.Name())))
			}
		}
	}
	return errs
}

// validateModelDeploymentRoleListenPorts refuses a role that declares ports and passes a --port, in
// any spelling the engine reads as it, naming a different one. The operator renders the first
// declared port into the engine's --port only where the role passed none, and the Service targets
// that declared port, so a role's own --port naming another one moves the engine off the port every
// request is sent to. The two values cannot both be true, and the refusal names both.
//
// A DIRECT DECODER IS EXEMPT, because there the two differ by design: its routing proxy owns the
// declared port and the role's --port places the engine behind it, which
// validateModelDeploymentRouterPorts already rules on.
//
// A ROLE THE OBJECT ALREADY HELD WITH THE SAME PORTS AND ARGUMENTS IS LEFT ALONE. The rule came after
// objects that break it were stored, and refusing every later edit to them would strand them over a
// field the edit did not touch; an edit that touches either field is judged by it.
func validateModelDeploymentRoleListenPorts(old, md *workercore.ModelDeployment) field.ErrorList {
	held := make(map[string]*workercore.ModelDeploymentRole)
	if old != nil {
		for i := range old.Spec.Roles {
			held[old.Spec.Roles[i].Name] = &old.Spec.Roles[i]
		}
	}

	var errs field.ErrorList
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if len(role.Ports) == 0 || workerctrl.ModelDeploymentRoleFrontedByProxy(md, role) {
			continue
		}
		if prev := held[role.Name]; prev != nil && kubemeta.DeepEqual(prev.Ports, role.Ports) &&
			slices.Equal(workerctrl.ModelDeploymentRoleArgs(prev), workerctrl.ModelDeploymentRoleArgs(role)) {
			continue
		}
		port, ok := workerctrl.ModelDeploymentRoleListenPort(md.Spec.Engine.Name, role)
		if !ok || port == role.Ports[0].Port {
			continue
		}

		rolePath := field.NewPath("spec", "roles").Index(i)
		argsPath := rolePath.Child("extraArgs")
		if len(role.Command) > 0 {
			argsPath = rolePath.Child("command")
		}
		errs = append(errs, field.Invalid(argsPath, fmt.Sprintf("--port=%d", port),
			fmt.Sprintf("role %q passes --port=%d in %s but declares %d in %s; the engine listens on "+
				"the one and the Service targets the other, so they must name the same port: drop "+
				"--port, or make the two equal",
				role.Name, port, argsPath, role.Ports[0].Port,
				rolePath.Child("ports").Index(0).Child("port"))))
	}

	return errs
}

// validateModelDeploymentRoleReservedListenPorts refuses a role that declares no ports and passes a
// --port, in any spelling the engine reads as it, naming a port the operator reserves on that role.
// The render places that port on the container beside the synthesized listeners, and the two
// claiming one number fail the render on every pass. A role passing none serves on the default
// port, which no listener reserves. A take-over role is exempt, because the render synthesizes no
// listener onto it.
//
// The refusal names the router when there is one, as validateModelDeploymentRolePorts does, and
// otherwise lands on the arguments, the field the object carries, rather than on ports it does not.
//
// A COLLISION THE OBJECT ALREADY HELD ON THE SAME ROLE IS LEFT ALONE, so that later edits do not
// strand an object stored before the rule. It is compared as the collision rather than as the role's
// ports and arguments, because the reserved set also moves with the router: adding one to an object
// whose role passes the event port brings a listener onto that port without touching the role.
func validateModelDeploymentRoleReservedListenPorts(old, md *workercore.ModelDeployment) field.ErrorList {
	held := make(map[string]int32)
	if old != nil {
		for i := range old.Spec.Roles {
			if port, _, ok := modelDeploymentRoleReservedListenPort(old, &old.Spec.Roles[i]); ok {
				held[old.Spec.Roles[i].Name] = port
			}
		}
	}

	var errs field.ErrorList
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		port, reserved, ok := modelDeploymentRoleReservedListenPort(md, role)
		if !ok {
			continue
		}
		if prev, stored := held[role.Name]; stored && prev == port {
			continue
		}

		argsPath := field.NewPath("spec", "roles").Index(i).Child("extraArgs")
		if md.Spec.Router != nil {
			errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
				fmt.Sprintf("role %q passes --port=%d in %s, which the operator reserves for a listener "+
					"it synthesizes onto this role under engine %q and router %q; reserved here are %v",
					role.Name, port, argsPath, md.Spec.Engine.Name, md.Spec.Router.Name, reserved)))
		} else {
			errs = append(errs, field.Invalid(argsPath, fmt.Sprintf("--port=%d", port),
				fmt.Sprintf("role %q passes --port=%d, which the operator reserves for a listener it "+
					"synthesizes onto this role under engine %q; reserved here are %v",
					role.Name, port, md.Spec.Engine.Name, reserved)))
		}
	}

	return errs
}

// modelDeploymentRoleReservedListenPort returns the port a role declaring no ports passes as its own
// --port, with the ports reserved on that role, and true when the one is among the others.
func modelDeploymentRoleReservedListenPort(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) (int32, []int32, bool) {
	if len(role.Ports) > 0 {
		return 0, nil, false
	}
	port, ok := workerctrl.ModelDeploymentRoleListenPort(md.Spec.Engine.Name, role)
	if !ok {
		return 0, nil, false
	}
	reserved := modelDeploymentRouterReservedPorts(md, role)

	return port, reserved, slices.Contains(reserved, port)
}

// validateModelDeploymentPoolTransport checks a new cache binding against each role's engine.
// An unchanged binding is held access: an existing deployment remains editable if its backend
// later changes transport, as with the host access gates above.
func (r *ModelDeploymentWebhook) validateModelDeploymentPoolTransport(
	ctx context.Context, old, md *workercore.ModelDeployment,
) (field.ErrorList, error) {
	if md.Spec.KVCache == nil || md.DeletionTimestamp != nil ||
		old != nil && kubemeta.DeepEqual(old.Spec.KVCache, md.Spec.KVCache) {
		return nil, nil
	}
	path := field.NewPath("spec", "kvCache", "poolRef", "name")
	binding := &workercore.KVCachePoolBinding{}
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{
		Namespace: md.Namespace, Name: md.Spec.KVCache.PoolRef.Name,
	}, binding); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, field.InternalError(path, fmt.Errorf("get kv cache pool binding: %w", err))
	}
	pool := &workercore.KVCachePool{}
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: binding.Spec.PoolRef.Name}, pool); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, field.InternalError(path, fmt.Errorf("get kv cache pool: %w", err))
	}
	if len(pool.Spec.Backends) == 0 {
		return nil, nil
	}
	backend := &workercore.KVCacheBackend{}
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: pool.Spec.Backends[0]}, backend); err != nil {
		if kerrors.IsNotFound(err) {
			return nil, nil
		}
		return nil, field.InternalError(path, fmt.Errorf("get kv cache backend: %w", err))
	}
	offers := mooncake.MemberProtocols(backend)
	nonempty := sets.New[string]()
	for _, offer := range offers {
		if offer != "" {
			nonempty.Insert(offer)
		}
	}
	if nonempty.Len() < 2 {
		return nil, nil
	}
	seen := make(map[string]struct{}, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if _, ok := seen[role.InstanceType]; ok {
			continue
		}
		seen[role.InstanceType] = struct{}{}
		manufacturer := ""
		if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM {
			instType, err := r.getInstanceType(ctx, role.InstanceType, i)
			if err != nil {
				return nil, err
			}
			manufacturer = instType.Status.Detail.Manufacturer
		}
		engine, err := workerctrl.ModelDeploymentInjectEngine(md.Spec.Engine.Name, manufacturer)
		if err != nil {
			return nil, field.InternalError(path, err)
		}
		if err := inject.ValidateBindingTransport(engine, offers); err != nil {
			return field.ErrorList{field.Forbidden(path, err.Error())}, nil
		}
	}
	return nil, nil
}

// validateModelDeploymentInterfaceRequests checks the device key against the resolved cache
// backend and the direct leg each role actually renders. An unresolved cache binding cannot
// justify a positive device request, because it may later resolve to another fabric.
func (r *ModelDeploymentWebhook) validateModelDeploymentInterfaceRequests(
	ctx context.Context, md *workercore.ModelDeployment,
) (field.ErrorList, error) {
	var requested []int
	for i := range md.Spec.Roles {
		ress := md.Spec.Roles[i].Resources
		if ress != nil && ress.Interface != nil && ress.Interface.Sign() > 0 {
			requested = append(requested, i)
		}
	}
	if len(requested) == 0 {
		return nil, nil
	}

	firstPath := field.NewPath("spec", "roles").Index(requested[0]).Child("resources", "interface")
	var protocols []string
	if md.Spec.KVCache != nil {
		binding := &workercore.KVCachePoolBinding{}
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{
			Namespace: md.Namespace, Name: md.Spec.KVCache.PoolRef.Name,
		}, binding); err != nil {
			if kerrors.IsNotFound(err) {
				return field.ErrorList{field.Forbidden(firstPath, "cache binding must resolve before requesting interfaces")}, nil
			}
			return nil, field.InternalError(firstPath, fmt.Errorf("get kv cache pool binding: %w", err))
		}
		pool := &workercore.KVCachePool{}
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: binding.Spec.PoolRef.Name}, pool); err != nil {
			if kerrors.IsNotFound(err) {
				return field.ErrorList{field.Forbidden(firstPath, "cache pool must resolve before requesting interfaces")}, nil
			}
			return nil, field.InternalError(firstPath, fmt.Errorf("get kv cache pool: %w", err))
		}
		if len(pool.Spec.Backends) == 0 {
			return field.ErrorList{field.Forbidden(firstPath, "cache backend must resolve before requesting interfaces")}, nil
		}
		backend := &workercore.KVCacheBackend{}
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: pool.Spec.Backends[0]}, backend); err != nil {
			if kerrors.IsNotFound(err) {
				return field.ErrorList{field.Forbidden(firstPath, "cache backend must resolve before requesting interfaces")}, nil
			}
			return nil, field.InternalError(firstPath, fmt.Errorf("get kv cache backend: %w", err))
		}
		protocols = mooncake.MemberProtocols(backend)
	}

	var errs field.ErrorList
	for _, i := range requested {
		role := &md.Spec.Roles[i]
		count, whole := role.Resources.Interface.AsInt64()
		if !whole {
			continue
		}
		manufacturer := ""
		if md.Spec.Engine.Name == workercore.ModelDeploymentEngineVLLM {
			instType, err := r.getInstanceType(ctx, role.InstanceType, i)
			if err != nil {
				return nil, err
			}
			manufacturer = instType.Status.Detail.Manufacturer
		}
		roleProtocols := protocols
		if len(role.Command) > 0 {
			roleProtocols = nil
		}
		_, err := workerctrl.ModelDeploymentInterfaceResource(roleProtocols,
			workerctrl.ModelDeploymentDirectInterfaceProtocol(md, role, manufacturer), count)
		if err != nil {
			errs = append(errs, field.Invalid(field.NewPath("spec", "roles").Index(i).Child("resources", "interface"),
				role.Resources.Interface.String(), err.Error()))
		}
	}
	return errs, nil
}

func (r *ModelDeploymentWebhook) ValidateCreate(
	ctx context.Context, obj runtime.Object,
) (ctrladmission.Warnings, error) {
	md := obj.(*workercore.ModelDeployment)

	errs := validateModelDeployment(md, nil)
	errs = append(errs, validateModelDeploymentHostAccess(nil, md,
		settings.InstancePrivilegedAllowed.ShouldValueBool(ctx),
		settings.InstanceHostPathVolumeAllowed.ShouldValueBool(ctx))...)
	errs = append(errs, validateModelDeploymentRoleListenPorts(nil, md)...)
	errs = append(errs, validateModelDeploymentRoleReservedListenPorts(nil, md)...)
	errs = append(errs, validateModelDeploymentServedModelNames(nil, md)...)

	errs = append(errs, validateModelDeploymentBarrierIsInstallable(ctx, md)...)

	typeErrs, err := r.validateRoleResourcesAgainstInstanceTypes(ctx, md)
	if err != nil {
		return nil, err
	}
	errs = append(errs, typeErrs...)
	if len(errs) == 0 {
		interfaceErrs, err := r.validateModelDeploymentInterfaceRequests(ctx, md)
		if err != nil {
			return nil, err
		}
		errs = append(errs, interfaceErrs...)
	}
	if len(errs) == 0 {
		transportErrs, err := r.validateModelDeploymentPoolTransport(ctx, nil, md)
		if err != nil {
			return nil, err
		}
		errs = append(errs, transportErrs...)
	}

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
	errs = append(errs, validateModelDeploymentHostAccess(old, md,
		settings.InstancePrivilegedAllowed.ShouldValueBool(ctx),
		settings.InstanceHostPathVolumeAllowed.ShouldValueBool(ctx))...)
	errs = append(errs, validateModelDeploymentRoleListenPorts(old, md)...)
	errs = append(errs, validateModelDeploymentRoleReservedListenPorts(old, md)...)
	errs = append(errs, validateModelDeploymentServedModelNames(old, md)...)
	errs = append(errs, validateModelDeploymentIdentity(md, old)...)
	errs = append(errs, validateModelDeploymentRouterName(md, old)...)
	errs = append(errs, validateModelDeploymentBarrierIsInstallable(ctx, md)...)

	typeErrs, err := r.validateRoleResourcesAgainstInstanceTypes(ctx, md)
	if err != nil {
		return nil, err
	}
	errs = append(errs, typeErrs...)
	if len(errs) == 0 && old != nil && (!kubemeta.DeepEqual(old.Spec.KVTransfer, md.Spec.KVTransfer) ||
		!kubemeta.DeepEqual(old.Spec.Router, md.Spec.Router)) {
		interfaceErrs, err := r.validateModelDeploymentInterfaceRequests(ctx, md)
		if err != nil {
			return nil, err
		}
		errs = append(errs, interfaceErrs...)
	}
	if len(errs) == 0 {
		transportErrs, err := r.validateModelDeploymentPoolTransport(ctx, old, md)
		if err != nil {
			return nil, err
		}
		errs = append(errs, transportErrs...)
	}

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

// modelDeploymentReplicaSizeFrozenMessage is the reason a size change carries, and it points at the
// field that does move rather than only refusing.
//
// IT NAMES replicas BECAUSE THE REFUSAL IS OTHERWISE A DEAD END. An operator changing size almost
// always wants more capacity, which replicas gives without touching anything already serving. The
// one thing size can do -- serve at a different instance shape -- genuinely needs a new deployment,
// and saying both is what separates "you asked for the wrong field" from "you cannot have this".
const modelDeploymentReplicaSizeFrozenMessage = "an instance's size is fixed when the deployment is " +
	"created: the Pods of a running instance cannot become a different number of Pods. Change " +
	"replicas to run more or fewer instances, or create a deployment that declares the size you want"

// validateModelDeploymentRouterName allows a router to be added or removed, but not changed in
// place. Changing the implementation is a delete and create with an interval between them.
func validateModelDeploymentRouterName(md, old *workercore.ModelDeployment) field.ErrorList {
	if old == nil || old.Spec.Router == nil || md.Spec.Router == nil ||
		old.Spec.Router.Name == md.Spec.Router.Name {
		return nil
	}

	return field.ErrorList{field.Invalid(
		field.NewPath("spec", "router", "name"), md.Spec.Router.Name, "field is immutable")}
}

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
// recreating. Its mirror image is the role's privileged field, which the criterion leaves editable even
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
	if md.Spec.Engine.Name != old.Spec.Engine.Name {
		errs = append(errs, field.Invalid(
			specPath.Child("engine", "name"), md.Spec.Engine.Name, modelDeploymentIdentityMessage))
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
// command is frozen because it decides whether the operator configures this role at all: a
// role that supplies one is taken over by its author, which changes cache injection and what status
// can claim. The rest of the role's container fields are how the build is fetched, shaped and
// tuned, and are editable.
//
// size IS FROZEN FOR A DIFFERENT REASON AND SO CARRIES A DIFFERENT MESSAGE. The others are identity:
// a different value describes a different deployment. This one is arithmetic the Pods of a running
// instance cannot survive -- an instance of n Pods is admitted as one group of n, and a new n makes
// every living instance the wrong shape at once, with no intermediate state in which the deployment
// is serving. What the reader needs is the field that does move, so the message names replicas.
//
// FREEZING IT ALSO KEEPS ONE NUMBER UNDER ONE WRITER. The size is the group's declared total, the
// member count the operator creates, and the rank count it publishes to every container; all three
// are derived from this one field on every pass. A count that never moves cannot be read as two
// different totals by two readers, and a count that moved would change a group's declared total
// while the group is running -- which Kueue answers by stopping every member it already admitted.
func validateModelDeploymentRoleIdentityFields(
	rolePath *field.Path, role, was *workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList
	if role.Kind != was.Kind {
		errs = append(errs, field.Invalid(
			rolePath.Child("kind"), role.Kind, modelDeploymentIdentityMessage))
	}
	if role.ReplicaSize != was.ReplicaSize {
		errs = append(errs, field.Invalid(
			rolePath.Child("size"), role.ReplicaSize, modelDeploymentReplicaSizeFrozenMessage))
	}
	if role.InstanceType != was.InstanceType {
		errs = append(errs, field.Invalid(
			rolePath.Child("instanceType"), role.InstanceType, modelDeploymentIdentityMessage))
	}
	if !kubemeta.DeepEqual(role.Resources, was.Resources) {
		errs = append(errs, field.Invalid(
			rolePath.Child("resources"), role.Resources, modelDeploymentIdentityMessage))
	}
	if !kubemeta.DeepEqual(role.Command, was.Command) {
		errs = append(errs, field.Invalid(
			rolePath.Child("command"), role.Command, modelDeploymentIdentityMessage))
	}

	return errs
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
	errs = append(errs, validateModelDeploymentEngineVersion(md)...)
	errs = append(errs, validateModelDeploymentKVCache(md)...)
	errs = append(errs, validateModelDeploymentRolesCount(md)...)
	errs = append(errs, validateModelDeploymentRoleNames(md)...)
	errs = append(errs, validateModelDeploymentRoleServiceNames(md, existingRoles)...)
	errs = append(errs, validateModelDeploymentServiceNamesAreDistinct(md)...)
	errs = append(errs, validateModelDeploymentRoleMemberNames(md)...)
	errs = append(errs, validateModelDeploymentRoleKinds(md)...)
	errs = append(errs, validateModelDeploymentRoleTopology(md)...)
	errs = append(errs, validateModelDeploymentRouter(md)...)

	return errs
}

func validateModelDeploymentRoleTopology(md *workercore.ModelDeployment) field.ErrorList {
	rolesPath := field.NewPath("spec", "roles")
	var errs field.ErrorList
	for i := range md.Spec.Roles {
		topology := md.Spec.Roles[i].Topology
		if topology == nil || topology.RequiredLevel == "" {
			continue
		}
		path := rolesPath.Index(i).Child("topology", "requiredLevel")
		if msgs := validation.IsQualifiedName(topology.RequiredLevel); len(msgs) > 0 {
			errs = append(errs, field.Invalid(path, topology.RequiredLevel,
				"is not a topology label key: "+strings.Join(msgs, "; ")))
		}
		if topology.RequiredLevel == core.LabelHostname {
			errs = append(errs, field.Forbidden(path,
				"kubernetes.io/hostname is implicit and cannot be required; "+
					"omitting requiredLevel leaves placement unconstrained, it does not keep the Pods on one Node"))
		}
	}
	return errs
}

// modelDeploymentRouterOwnedArgs is the flags a router's rendering derives, keyed by router.
//
// THE ENTRIES DIVIDE INTO TWO KINDS, and the refusal message is the same for both only because the
// user-facing consequence is. Most of them are refused because the operator already computes the
// value -- the selector, the target ports and the configuration file path all restate something this
// deployment's own objects decide, and two values for one setting cannot be told apart.
//
// --secure-serving and --metrics-endpoint-auth are refused for a DIFFERENT reason: they are not
// derived, they are a boundary. A router manages east-west traffic inside the cluster; transport
// security and caller authentication are north-south concerns owned by the gateway that admits
// traffic. Both are rendered off, against an upstream default of on, and the rendering states why.
// Leaving them out of this list would not make them configurable in a useful way -- it would make
// the boundary a per-object choice, which is the thing being refused.
var modelDeploymentRouterOwnedArgs = map[string][]string{
	workercore.ModelDeploymentRouterLLMD: {
		"--endpoint-selector",
		"--endpoint-target-ports",
		"--config-file",
		"--secure-serving",
		"--grpc-health-port",
		"--metrics-endpoint-auth",
	},
	// THE BIND ADDRESSES AND THE CONNECTOR ARE OWNED FOR THE REASON THEY ARE RENDERED AT ALL: each
	// of the three overrides an upstream default that fails silently, so a user value winning would
	// restore exactly the failure the rendering exists to prevent -- a router nothing can reach, or
	// one that routes correctly and never transfers. The rest restate what this deployment's own
	// objects decide.
	workercore.ModelDeploymentRouterVLLM: {
		"--host",
		"--port",
		"--prometheus-host",
		"--prometheus-port",
		"--kv-connector",
		"--service-discovery",
		"--service-discovery-namespace",
		"--service-discovery-port",
		"--selector",
		"--prefill-selector",
		"--decode-selector",
		"--vllm-pd-disaggregation",
	},
	// The same catalog as the row above MINUS the two flags that project does not have: its
	// transfer backend is the engine's, and its disaggregation switch is spelled without the
	// engine's name. The lists are written out rather than derived from each other, because what
	// makes an entry correct is a flag existing in one upstream program, and two programs that
	// happen to agree today are not one program.
	workercore.ModelDeploymentRouterSGLang: {
		"--host",
		"--port",
		"--prometheus-host",
		"--prometheus-port",
		"--service-discovery",
		"--service-discovery-namespace",
		"--service-discovery-port",
		"--selector",
		"--prefill-selector",
		"--decode-selector",
		"--pd-disaggregation",
	},
}

// modelDeploymentRouterEngines is which engine each router may front.
//
// IT IS ENGINE-MATCHED RATHER THAN OPEN, and the entries are not symmetric. "llm-d-router" takes
// either engine because upstream carries a handshake connector and a metrics configuration for each
// of them. The other value is one project's router for that project's own engine, and admitting it
// in front of another would be a claim this repository has not measured; widening it later is a
// widening, whereas narrowing it later would break objects that already exist.
var modelDeploymentRouterEngines = map[string][]string{
	workercore.ModelDeploymentRouterLLMD: {
		workercore.ModelDeploymentEngineVLLM,
		workercore.ModelDeploymentEngineSGLang,
	},
	workercore.ModelDeploymentRouterVLLM: {
		workercore.ModelDeploymentEngineVLLM,
	},
	workercore.ModelDeploymentRouterSGLang: {
		workercore.ModelDeploymentEngineSGLang,
	},
}

func validateModelDeploymentRouter(md *workercore.ModelDeployment) field.ErrorList {
	if md.Spec.Router == nil {
		return nil
	}

	var errs field.ErrorList
	argsPath := field.NewPath("spec", "router", "extraArgs")
	for i, arg := range md.Spec.Router.ExtraArgs {
		name := workerctrl.ModelDeploymentArgName(arg)
		if !slices.Contains(modelDeploymentRouterOwnedArgs[md.Spec.Router.Name], name) {
			continue
		}
		errs = append(errs, field.Invalid(argsPath.Index(i), arg, fmt.Sprintf(
			"%q is set by the operator for router %q and must not be supplied here, because two "+
				"values for it cannot be told apart", name, md.Spec.Router.Name)))
	}

	// THE THRESHOLD IS REFUSED RATHER THAN IGNORED on the routers that have no concept for it. A
	// field that is legal to write and renders nothing is the shape this API has rejected before,
	// and the refusal is stable because the router name is frozen after creation -- so an object
	// cannot become invalid through a later edit to a different field.
	if md.Spec.Router.DisaggregationThresholdTokens != nil &&
		md.Spec.Router.Name != workercore.ModelDeploymentRouterLLMD {
		errs = append(errs, field.Invalid(
			field.NewPath("spec", "router", "disaggregationThresholdTokens"),
			*md.Spec.Router.DisaggregationThresholdTokens,
			fmt.Sprintf("router %q has no prompt-token threshold to set; it is meaningful under %q "+
				"alone, whose scheduler decides per request whether to split one",
				md.Spec.Router.Name, workercore.ModelDeploymentRouterLLMD)))
	}

	if engines, known := modelDeploymentRouterEngines[md.Spec.Router.Name]; known &&
		!slices.Contains(engines, md.Spec.Engine.Name) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "router", "name"), md.Spec.Router.Name,
			fmt.Sprintf("router %q does not front engine %q; it accepts %s",
				md.Spec.Router.Name, md.Spec.Engine.Name, strings.Join(engines, ", "))))
	}

	// THE ENGINE METRICS TABLE IS THE PICKER'S PRECONDITION ALONE. It exists to feed that router's
	// metrics extractor; the other two score on their own observations and read none of it. The
	// gate is not a preference: the refusal names the router from this package's own constant
	// rather than from the object, so a call left unconditional tells a user who asked for one
	// router that some other router requires a metric.
	if md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD {
		if _, err := router.MetricsForEngine(md.Spec.Engine.Name); err != nil {
			errs = append(errs, field.Invalid(field.NewPath("spec", "engine"), md.Spec.Engine.Name, err.Error()))
		}
	}

	ports := make([]int32, 0, len(md.Spec.Roles))
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if port := workerctrl.ModelDeploymentRoleServingProtocol(role); port != core.ProtocolTCP {
			errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
				fmt.Sprintf("every routed role must use TCP serving ports; role %q uses %s",
					role.Name, port)))
		}
		if workerctrl.ModelDeploymentRoleServingScheme(md.Spec.Engine.Name, role) == core.URISchemeHTTPS {
			errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
				fmt.Sprintf("managed router supports plaintext engine endpoints only; role %q uses HTTPS",
					role.Name)))
		}
		ports = append(ports, workerctrl.ModelDeploymentRoleServingPort(md, role))
		errs = append(errs, validateModelDeploymentRouterPorts(md, role)...)
	}
	slices.Sort(ports)
	ports = slices.Compact(ports)
	if len(ports) != 1 {
		errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
			fmt.Sprintf("every routed role must use the same serving port; got %v", ports)))
	}

	return errs
}

// validateModelDeploymentRolePorts refuses the port choices that collide with listeners the
// operator synthesizes onto a replica. It runs for EVERY role, routed or not, because the one
// listener that does not follow a router -- SGLang's bootstrap registry, which follows the role of
// a declared pair -- renders on an unrouted prefiller attached to a shared pool all the same. The
// rule lives at admission because the collision it prevents is permanent there: the replica
// renders, and then a process binds a port something else already holds.
func validateModelDeploymentRolePorts(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	// The render places these on the same container the role's declared ports describe, and the
	// declared set is the whole collision surface of a role declaring any: its own --port is held to
	// the first of them everywhere but on a direct decoder, which reserves nothing. A role declaring
	// none serves on the port its own --port names, which
	// validateModelDeploymentRoleReservedListenPorts judges, because telling a stored collision from
	// a new one takes the old object this rule does not have.
	//
	// The refusal names the router when there is one, because a router is what drags the two vLLM
	// listeners in; without one the listener on this role is the engine's own -- SGLang's registry,
	// rendered for the role alone -- so the refusal lands on the port the user declared rather than
	// on a field their object does not carry.
	if reserved := modelDeploymentRouterReservedPorts(md, role); len(reserved) > 0 {
		for _, port := range role.Ports {
			if !slices.Contains(reserved, port.Port) {
				continue
			}
			if md.Spec.Router != nil {
				errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
					fmt.Sprintf("role %q declares port %d, which the operator reserves for a listener it "+
						"synthesizes onto this role under engine %q and router %q; reserved here are %v",
						role.Name, port.Port, md.Spec.Engine.Name, md.Spec.Router.Name, reserved)))
			} else {
				errs = append(errs, field.Invalid(rolePath.Child("ports"), port.Port,
					fmt.Sprintf("role %q declares port %d, which the operator reserves for a listener it "+
						"synthesizes onto this role under engine %q; reserved here are %v",
						role.Name, port.Port, md.Spec.Engine.Name, reserved)))
			}
		}
	}

	return errs
}

// validateModelDeploymentRouterPorts refuses the port choices that collide with the serving
// surface a managed router takes over. Both rules live at admission because the collision they
// prevent is permanent there: the replica renders, and then a process binds a port something else
// already holds.
func validateModelDeploymentRouterPorts(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) field.ErrorList {
	var errs field.ErrorList

	// A direct decode role is fronted by a routing proxy that takes the serving port, and the model
	// server has to move off it. The render moves it only when the operator placed the flag; a role
	// passing --port itself keeps the value, and the render then fails on the collision on every
	// pass. The manufacturer is not knowable here, so the rule fires wherever the proxy MIGHT be
	// rendered -- on Ascend none is, and refusing the flag there costs nothing, because the fill
	// would have set that very value.
	if md.Spec.Router.Name != workercore.ModelDeploymentRouterLLMD ||
		workerctrl.ModelDeploymentEffectiveRoleKind(role) != workercore.ModelDeploymentRoleKindDecode ||
		len(role.Command) > 0 {
		return errs
	}
	if port, ok := workerctrl.ModelDeploymentRoleListenPort(md.Spec.Engine.Name, role); ok &&
		port == workerctrl.ModelDeploymentRoleServingPort(md, role) {
		errs = append(errs, field.Invalid(field.NewPath("spec", "router"), md.Spec.Router,
			fmt.Sprintf("role %q passes --port=%d, the port its Service publishes; the routing proxy "+
				"a direct decode role is fronted by takes that port, so the model server must leave it",
				role.Name, port)))
	}

	return errs
}

// modelDeploymentRouterReservedPorts are the container ports the operator synthesizes onto THIS
// role, under this deployment's engine and router. They are the inject package's own constants
// rather than restated numbers, so the refusal and the render cannot drift apart.
//
// IT IS COMPUTED PER ROLE RATHER THAN BEING ONE LIST, because the listeners are not one set: each
// is rendered by a different condition, and a single list is wrong in both directions at once. It
// would refuse a port nothing on that role uses -- a decoder never publishes events, and no role
// under a router configured by argv does -- while releasing one that IS used, because SGLang's
// bootstrap registry is rendered on any prefiller and the old list only ever ran for vLLM.
//
// IT ANSWERS FOR UNROUTED DEPLOYMENTS TOO, because SGLang's registry follows the role rather than
// any router: a prefiller of a declared pair attached to a shared pool binds it with nothing
// routing the halves. The two vLLM listeners are the opposite case -- each renders only under a
// router, the event publisher under one alone and the bootstrap server under whichever is named --
// so they reserve nothing once no router is declared.
//
// THE PAIR IS READ ON THE SAME AXIS THE RENDER READS IT, because unlike the pool's vendor it is
// knowable here: the roles are in the spec. Each bootstrap listener renders on a prefiller of a
// declared pair alone -- the transfer leg and SGLang's split arguments each follow the pair rule
// the router already follows -- so the reservation follows it too, and a lone prefiller feeding a
// shared pool keeps every port free.
//
// The pool's accelerator vendor is not knowable at admission and is what excludes the Ascend
// backend from publishing, so the reservation is made wherever a listener MIGHT be rendered.
// Refusing a port there costs a user nothing they cannot rename.
func modelDeploymentRouterReservedPorts(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) []int32 {
	// A take-over role is exempt: the render synthesizes none of these onto it, so nothing is there
	// to collide with.
	if len(role.Command) > 0 {
		return nil
	}

	kind := workerctrl.ModelDeploymentEffectiveRoleKind(role)
	var reserved []int32

	switch md.Spec.Engine.Name {
	case workercore.ModelDeploymentEngineVLLM:
		// Both listeners below render only under a router, so an unrouted vLLM role reserves
		// nothing: the event publisher feeds one router's data layer, and the transfer leg that
		// carries the bootstrap server follows a routed pair.
		if md.Spec.Router == nil {
			break
		}
		// The event publisher and its replay socket feed the picker's data layer, so they exist
		// under that router alone -- and not on a decoder, which consumes rather than publishes.
		if md.Spec.Router.Name == workercore.ModelDeploymentRouterLLMD &&
			kind != workercore.ModelDeploymentRoleKindDecode {
			reserved = append(reserved, inject.VLLMKVEventsPort, inject.VLLMKVEventsReplayPort)
		}
		// The Mooncake bootstrap server runs on the prefiller alone, and UNDER EVERY ROUTER rather
		// than one. The leg that binds it reads a router name only to exclude an Ascend pair behind
		// a router that cannot drive it; on every other vendor the leg renders under whichever
		// router is named. This rule cannot see the vendor, so naming one router here releases the
		// port under the other two while the render still binds it -- the permanent collision the
		// paragraph above describes as being wrong in both directions at once. The renderer states
		// the same rule from its own side, that the reservation keeping a user's port off this one
		// is vendor-blind. The pair term is read here too, because it CAN be: the leg follows a
		// declared pair, and a lone prefiller under a router binds no bootstrap server.
		if kind == workercore.ModelDeploymentRoleKindPrefill &&
			workerctrl.ModelDeploymentDeclaresBothHalves(md) {
			reserved = append(reserved, inject.VLLMMooncakeBootstrapPort)
		}
	case workercore.ModelDeploymentEngineSGLang:
		// SGLang's bootstrap registry follows the ROLE and not any router, because naming the split
		// is what it means to be one half on this engine. It renders on a declared pair, so the
		// reservation reads the same rule. It also renders only where the render can synthesize a
		// connector at all, read from the predicate that gates the render: a pair with neither a
		// router nor a cache renders none, and its prefiller keeps the port free.
		if kind == workercore.ModelDeploymentRoleKindPrefill &&
			workerctrl.ModelDeploymentDeclaresBothHalves(md) &&
			workerctrl.ModelDeploymentMaySynthesizeConnector(md) {
			reserved = append(reserved, inject.SGLangBootstrapPort)
		}
	}

	return reserved
}

// validateModelDeploymentEngineVersion refuses an engine without a version when any role would
// have its image synthesized.
//
// The version is optional ONLY because a role that names its own image never reads it. A role
// naming none has its image assembled from the engine, the version and the role's own
// InstanceType, and an empty version assembles a malformed tag naming something never typed.
// Refusing that here, where the writer can act, beats what the render would do with it: a
// reconcile error loop on a Pod that is never created, reported as an Event far from the field
// that caused it.
//
// The rule is answerable from the object alone and runs on create and on update alike: dropping
// the version while a role still needs it is the same mistake as omitting it on create.
func validateModelDeploymentEngineVersion(md *workercore.ModelDeployment) field.ErrorList {
	if md.Spec.Engine.Version != "" {
		return nil
	}

	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if role.Image != "" {
			continue
		}

		return field.ErrorList{field.Required(
			field.NewPath("spec", "engine", "version"),
			fmt.Sprintf(
				"role %q names no image of its own, so its image is synthesized from the engine "+
					"and its version, and an empty version assembles a tag naming something never "+
					"typed. Give the engine a version, or name an image on the role",
				role.Name),
		)}
	}

	return nil
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
	if md.Spec.KVCache == nil {
		return nil
	}

	if md.Spec.KVCache.PoolRef.Name != "" {
		return nil
	}

	return field.ErrorList{field.Required(
		field.NewPath("spec", "kvCache", "poolRef", "name"),
		"the Binding name must not be empty: the Binding is the authorization point for reaching "+
			"the pool, and an empty reference names none",
	)}
}

// modelDeploymentMaxRoles is the number of roles one deployment may declare.
//
// IT WAS KUEUE'S NUMBER AND IS NOW THIS PROJECT'S, which is the whole of what changed. While every
// role was one PodSet of a single Workload, this was Workload.spec.podSets's own maxItems and the
// value had to be read off the Kueue that runs rather than the type library this module compiles
// against. Each replica is admitted as its own Workload carrying one PodSet now, so no Kueue bound
// constrains how many roles a deployment declares, and ten is a product bound: a prefill/decode
// deployment names two, nothing rendered here has a use for ten, and an unbounded count would let
// one object fan out into arbitrarily many Workloads, Services and queue references with no answer
// at admission time.
//
// SO THE NUMBER IS NO LONGER WORTH CHECKING AGAINST A CLUSTER, which the wording this replaces
// asked a reader to do. Raising it is a product decision, not a Kueue upgrade.
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
			"at most %d roles: every role renders its own replicas, Services and queue references, "+
				"and a deployment naming more than that is a shape this operator does not serve — a "+
				"prefill/decode deployment names two",
			modelDeploymentMaxRoles,
		),
	)}
}

// validateModelDeploymentRoleNames refuses two roles sharing a name.
//
// A replica's Kueue group is derived from the role's name and the replica's ordinal, so two roles
// sharing a name put their same-numbered replicas in ONE group — a group that declares a total of
// one and now holds two members. Kueue answers the excess by deleting the newer of them, so one
// role's replica is removed on account of the other's, repeatedly, with nothing reporting why.
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
				"a role's name is part of the Kueue group each of its replicas joins, so this does "+
					"not collide with %s — it puts the two roles' same-numbered replicas into one "+
					"group that declares a single member, and Kueue deletes whichever arrived later",
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

// validateModelDeploymentServiceNamesAreDistinct refuses a deployment two of whose Services would
// be named the same thing.
//
// THE COLLISION IS BETWEEN TWO DIFFERENT FORMULAS, which is why distinct role names are not enough
// to prevent it. A role is fronted by `<deployment>-<role>`, and a role of several members publishes
// each instance behind `<deployment>-<role>-r<ordinal>` — so a role named `x` running instances of
// several Pods derives `<deployment>-x-r0`, and a sibling role named `x-r0` derives the same name
// for itself. Both roles are legal, they do not share a name, and nothing else in this handler
// compares them.
//
// WHAT IT PREVENTS IS SILENT, WHICH IS WHY IT IS REFUSED RATHER THAN RESOLVED. The converger aligns
// the Services it renders against what the cluster holds, one list entry at a time; two entries
// naming one object make every pass rewrite that object into the other's shape. The instance's
// headless Service and the role's ClusterIP Service are not interchangeable — one publishes per-Pod
// records with no cluster IP, the other load-balances — so whichever shape loses the pass takes its
// consumers with it: either the members of an instance stop resolving each other, or the role stops
// answering. Nothing reports it; both objects exist and one of them is the wrong kind of Service.
//
// IT IS CHECKED ON EVERY REQUEST rather than only for roles that are new. `replicas` is a field a
// user is invited to change, and it decides how many instance Services a role derives, so a scale-up
// is exactly the edit that walks a legal deployment into a collision.
func validateModelDeploymentServiceNamesAreDistinct(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	// The deployment's own Service is named after the deployment, and it claims that name before any
	// role is considered.
	claimedBy := map[string]string{md.Name: "the deployment's own Service"}

	// A MANAGED ROUTER IS FRONTED BY A SERVICE TOO, under `<deployment>-router`, which is the name a
	// role called "router" derives for itself. Every router value renders its objects under that one
	// name, so the claim is made whenever a router is asked for at all rather than per value.
	if md.Spec.Router != nil {
		claimedBy[md.Name+"-router"] = "the Service fronting the managed router"
	}

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]

		derived := []string{md.Name + "-" + role.Name}
		describes := []string{fmt.Sprintf("the Service fronting role %q", role.Name)}
		// Only above one member: an instance of a single Pod has nobody to address and no headless
		// Service is rendered for it, so enumerating names here that nothing creates would refuse a
		// deployment that works.
		if role.ReplicaSize > 1 {
			for ordinal := range int(role.Replicas) {
				derived = append(derived,
					fmt.Sprintf("%s-%s-r%d", md.Name, role.Name, ordinal))
				describes = append(describes,
					fmt.Sprintf("the headless Service publishing instance %d of role %q",
						ordinal, role.Name))
			}
		}

		for j, name := range derived {
			if by, taken := claimedBy[name]; taken {
				errs = append(errs, field.Invalid(
					rolesPath.Index(i).Child("name"), role.Name, fmt.Sprintf(
						"%s would be named %q, and so would %s. One name is one object, and the "+
							"two need different shapes, so whichever is written last leaves the "+
							"other without the addresses it publishes. Rename this role",
						describes[j], name, by)))

				continue
			}
			claimedBy[name] = describes[j]
		}
	}

	return errs
}

// validateModelDeploymentRoleMemberNames refuses a role whose members would be named something
// Kubernetes cannot use as a hostname.
//
// IT IS A SEPARATE RULE FROM THE SERVICE-NAME ONE ABOVE, AND NOT A WIDENING OF IT, because the two
// have different subjects and different triggers. That rule is about <deployment>-<role>, which no
// update can move, so it is checked once when a role appears. This one is about
// <deployment>-<role>-r<ordinal>-m<member>, whose length depends on `replicas` -- a field a user is
// invited to change -- so it has to be checked on every update, including for roles that already
// exist. Merging them would mean either re-checking an immutable pair forever or letting a scale
// walk a legal deployment into an illegal one.
//
// THE FAILURE IT PREVENTS IS A LOOP RATHER THAN AN ERROR. A member's name is its hostname, which is
// a DNS-1123 label of 63 characters, while the names above are only checked to 63 for the shorter
// composite -- so a 61-character <deployment>-<role> is legal there and produces a 67-character
// member here. Without this rule the deployment is admitted, every Pod create is rejected by the API
// server, and the reconciler retries forever with the cause two objects away from the field that
// caused it. Above one member that is the ONLY thing that happens: there is nothing partial to
// observe, because the group is never composed at all.
//
// IT MEASURES THE LONGEST NAME THE SPEC CAN CURRENTLY PRODUCE rather than a worst case over the
// field's type. Budgeting for a ten-digit ordinal would take twenty-four characters away from every
// deployment to cover counts nobody runs; measuring the declared counts costs nothing and refuses
// the scale that would break it, at the moment that scale is requested.
func validateModelDeploymentRoleMemberNames(md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList

	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		// At one member per replica the render names nothing: the Pod keeps the generated name it
		// has always had, and no hostname is set. There is no budget to check.
		if role.ReplicaSize <= 1 {
			continue
		}

		// The highest ordinal and the highest member index, which together spell the longest name
		// this role can produce. Both floor at zero so a role declaring no replicas -- which the
		// schema refuses, but which a rule must not depend on -- still measures something real.
		name := fmt.Sprintf("%s-%s-r%d-m%d", md.Name, role.Name,
			max(int(role.Replicas)-1, 0), int(role.ReplicaSize)-1)
		why := validation.IsDNS1123Label(name)
		if len(why) == 0 {
			continue
		}

		// THE ERROR IS ATTACHED TO THE ROLE RATHER THAN TO ONE OF ITS FIELDS, because four inputs
		// spell this name -- the deployment's name, the role's name, `replicas` and `size` -- and
		// which of them moved is not knowable from the object being validated. Naming `size` would
		// be actively misleading on the edit that reaches here most often: a scale that takes
		// `replicas` from 9 to 10 lengthens the ordinal and trips this, while `size` is immutable
		// and therefore the one input the user cannot act on. The detail below names every input
		// that can be changed instead.
		errs = append(errs, field.Invalid(
			rolesPath.Index(i), name, fmt.Sprintf(
				"a replica of this role is addressed by naming each of its Pods, and the longest "+
					"such name would be %q (%d characters), which cannot be a hostname: %s. "+
					"Shorten the role or the deployment, or declare fewer replicas",
				name, len(name), strings.Join(why, "; "))))
	}

	return errs
}

// validateModelDeploymentRoleKinds holds the three rules about what a role is told it is.
//
// The first is about the SET: a server serves whole requests by itself, so "one plain server plus a
// prefiller" names no shape anything consumes, and accepting it would mean rendering a transfer
// configuration whose meaning is undefined.
//
// The second is about MULTIPLICITY, and it is deliberately asymmetric. Several roles of kind server
// are a set of equals, and something in front of them can pick between them; several prefillers are
// not, because nothing consuming these roles expresses a second one, so the extra role would render
// into a configuration nothing downstream can reach. The rule therefore refuses a repeated NON-SERVER
// kind rather than repetition in general, and server is exempt by that reasoning rather than by
// having been overlooked.
//
// The third is about the ENGINE: a kind is only real if the engine's rendering has a term for it.
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

	// firstOfKind remembers where a non-server kind was first declared, so the refusal can name the
	// role the second one collides with rather than only the index it sits at.
	firstOfKind := make(map[workercore.ModelDeploymentRoleKind]int, len(md.Spec.Roles))

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

		if kind != workercore.ModelDeploymentRoleKindServer {
			if first, seen := firstOfKind[kind]; seen {
				errs = append(errs, field.Invalid(kindPath, kind, fmt.Sprintf(
					"kind %q is already declared by role %q: only %q may be declared by more than one "+
						"role, because a set of servers is a set of equals, whereas nothing consuming "+
						"these roles expresses a second %q and the extra role would render into a "+
						"configuration nothing downstream can reach",
					kind, md.Spec.Roles[first].Name, workercore.ModelDeploymentRoleKindServer, kind,
				)))

				continue
			}

			firstOfKind[kind] = i
		}

		// NO ENGINE THIS API ACCEPTS IS REFUSED BY THIS RULE TODAY, and it is kept rather than
		// removed. Both engines render all three kinds, so the only shapes left for it are a kind
		// with no mapping and an engine added to the renderer without a table entry -- and the
		// second is the one worth a standing rule. The support table is the rendering side's own
		// truth; an engine that arrives without an entry reports "no term" for every kind, and this
		// is the only place that turns that into a refusal the user can still act on. Without it
		// such a deployment is admitted and then fails to render, in a reconciler the user cannot
		// reach.
		if !workerctrl.ModelDeploymentSupportsRoleKind(md.Spec.Engine.Name, kind) {
			errs = append(errs, field.Invalid(kindPath, kind, fmt.Sprintf(
				"engine %q has no rendering term for kind %q; accepting it would leave the container "+
					"looking configured and behaving as though the role were never declared",
				md.Spec.Engine.Name, kind,
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

		errs = append(errs, validateModelDeploymentRoleExtraArgs(md.Spec.Engine.Name, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleParallelWidth(md.Spec.Engine.Name, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleEnv(md.Spec.Engine.Name, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleResources(role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleAdditionalVolumes(role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleArtifactKeys(md, role, rolePath)...)
		errs = append(errs, validateModelDeploymentRoleReservedMountPaths(md, role, rolePath)...)
		// Unconditional rather than inside the router's own rules, because the listeners this
		// refuses against are not all the router's: SGLang's bootstrap registry follows the role
		// and renders on an unrouted prefiller, and the router-gated walk never runs for one.
		errs = append(errs, validateModelDeploymentRolePorts(md, role, rolePath)...)
	}

	return errs
}

// validateModelDeploymentRoleAdditionalVolumes enforces the three rules the field documentation
// states and the schema cannot.
//
// THEY ARE REFUSED HERE RATHER THAN BY THE API SERVER LATER. Without them the first sign of either
// path mistake is the rendered Pod being rejected on every reconcile pass, which surfaces as a
// create failure quoting a volumeMount the user never wrote, on an object that was admitted cleanly.
//
// AN ENTRY NAMING NO SOURCE IS WORSE THAN EITHER, because nothing rejects it at all: the renderer
// has no volume source to build and skips the entry, so the container starts without the mount that
// was asked for and no error, Event or condition says so. The three sources are separate optional
// pointers, which is a shape the schema cannot constrain to exactly one.
//
// A ".." ELEMENT IS CHECKED PER SEGMENT, not by substring: a subPath of "a..b" contains those two
// characters and is a perfectly ordinary directory name, while "a/../b" is the traversal the rule
// is about.
//
// THE UNIQUENESS RULE REACHES THESE ENTRIES AND NOT THE OPERATOR'S OWN MOUNTS, which is a limit
// rather than an oversight. The renderer appends the connector's volume mounts to the same
// container, and those come from the KV cache backend this deployment resolves at reconcile time --
// an object admission cannot see, since resolving it is the controller's work and its result can
// change without this object changing. So a path colliding with an operator-rendered mount is still
// admitted here and still diverges silently at render. Closing that needs the check where the
// mounts are known, which is the renderer, not this validator.
func validateModelDeploymentRoleAdditionalVolumes(
	role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	avsPath := rolePath.Child("additionalVolumes")
	seen := make(map[string]int, len(role.AdditionalVolumes))
	for i := range role.AdditionalVolumes {
		av, avPath := &role.AdditionalVolumes[i], avsPath.Index(i)

		if av.ConfigMap == nil && av.Secret == nil && av.HostPath == nil {
			errs = append(errs, field.Required(avPath, fmt.Sprintf(
				"one of configMap, secret or hostPath, so that %q names something to mount; an "+
					"entry naming none is skipped at render and the container starts without it",
				av.MountPath,
			)))
		}

		if first, duplicate := seen[av.MountPath]; duplicate {
			errs = append(errs, field.Duplicate(avPath.Child("mountPath"), fmt.Sprintf(
				"%q is already mounted by %s; two volumes cannot occupy one path, and which of them "+
					"the container would see is not something this API decides",
				av.MountPath, avsPath.Index(first).Child("mountPath"),
			)))
		} else {
			seen[av.MountPath] = i
		}

		if av.SubPath == "" {
			continue
		}
		if slices.Contains(strings.Split(av.SubPath, "/"), "..") {
			errs = append(errs, field.Invalid(avPath.Child("subPath"), av.SubPath,
				"must not contain a \"..\" element: the path is resolved inside the volume, and an "+
					"element that climbs out of it reaches the host filesystem of whatever backs the "+
					"volume"))
		}
	}

	return errs
}

// validateModelDeploymentServedModelNames applies validateModelDeploymentRoleServedModelName to every
// role, except a role an update leaves with the served names it already had: the rule arrived after
// objects were stored, and one it would refuse must still take an edit elsewhere -- a scale, a label
// -- rather than be stranded until its arguments are fixed in the same request.
func validateModelDeploymentServedModelNames(old, md *workercore.ModelDeployment) field.ErrorList {
	var errs field.ErrorList
	rolesPath := field.NewPath("spec", "roles")
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		if old != nil {
			if j := slices.IndexFunc(old.Spec.Roles, func(r workercore.ModelDeploymentRole) bool {
				return r.Name == role.Name
			}); j >= 0 {
				was, _ := workerctrl.ModelDeploymentServedModelNames(old.Spec.Engine.Name, old.Spec.Roles[j].ExtraArgs)
				now, _ := workerctrl.ModelDeploymentServedModelNames(md.Spec.Engine.Name, role.ExtraArgs)
				if slices.Equal(was, now) && old.Spec.Model.Name == md.Spec.Model.Name &&
					len(old.Spec.Roles[j].Command) == len(role.Command) {
					continue
				}
			}
		}
		errs = append(errs, validateModelDeploymentRoleServedModelName(md, role, rolesPath.Index(i))...)
	}

	return errs
}

// validateModelDeploymentRoleServedModelName refuses a managed role whose own --served-model-name
// is anything but spec.model.name.
//
// THE RULE CANNOT BE LOOSENED, and it holds with or without an artifact. A router, the router's
// tokenizer calls and the metrics' model label all match on spec.model.name. A role serving under
// another name was measured to fail twice and silently: requests by spec.model.name answer 404
// through the router, and requests by the role's name succeed while the router's token producer
// fails on every one and its prefix-cache scoring falls to zero -- with the deployment reporting
// Ready. Nothing but this refusal stops it.
//
// A second name is refused too: vLLM takes several, and its metrics carry only the first.
//
// A role that replaced its command line is not judged: its author owns every argument, which is
// what that tier is for.
func validateModelDeploymentRoleServedModelName(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if len(role.Command) > 0 {
		return nil
	}
	names, found := workerctrl.ModelDeploymentServedModelNames(md.Spec.Engine.Name, role.ExtraArgs)
	if !found || (len(names) == 1 && names[0] == md.Spec.Model.Name) {
		return nil
	}

	return field.ErrorList{field.Invalid(rolePath.Child("extraArgs"), names, fmt.Sprintf(
		"%s must be exactly %q, the name spec.model.name serves under: a router, its tokenizer "+
			"calls and the metrics' model label all match on that name, so any other value, or a "+
			"second one, makes them miss while the deployment reports Ready; leave the flag out "+
			"to have the operator render it",
		workerctrl.ModelDeploymentServedModelNameArg, md.Spec.Model.Name,
	))}
}

// validateModelDeploymentRoleArtifactKeys refuses, while spec.model.artifactRef is set, an argument
// or environment entry naming what the operator renders to deliver the weights: the model the
// engine loads, the pin on it, where a download lands, and how the engine reaches the Hub.
//
// A later flag of the same name would silently replace the operator's, and the deployment would
// serve weights other than the artifact's while status echoes the artifact. A role that replaced
// its command line gets none of those renders, so it is not judged.
func validateModelDeploymentRoleArtifactKeys(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if md.Spec.Model.ArtifactRef == nil || len(role.Command) > 0 {
		return nil
	}

	engine := md.Spec.Engine.Name
	var errs field.ErrorList
	for i, arg := range role.ExtraArgs {
		owned, ok := workerctrl.ModelDeploymentArtifactOwnedArg(engine, arg)
		if !ok {
			continue
		}
		errs = append(errs, field.Invalid(rolePath.Child("extraArgs").Index(i), arg, fmt.Sprintf(
			"%q is set by the operator while spec.model.artifactRef names the weights, and a second "+
				"value would silently replace the artifact's; replace the whole command line through "+
				"%s to own it instead", owned, rolePath.Child("command"),
		)))
	}
	for i := range role.Env {
		name := role.Env[i].Name
		if !workerctrl.ModelDeploymentArtifactOwnsEnv(engine, name) {
			continue
		}
		errs = append(errs, field.Invalid(rolePath.Child("env").Index(i), name, fmt.Sprintf(
			"%q is set by the operator while spec.model.artifactRef names the weights: it is how an "+
				"engine downloading them reaches the Hub with the artifact's credential; replace the "+
				"whole command line through %s to own it instead", name, rolePath.Child("command"),
		)))
	}

	return errs
}

// validateModelDeploymentRoleReservedMountPaths refuses, while spec.model.artifactRef is set, a
// role volume at, inside or around the paths the weights are mounted at: one inside would shadow
// part of the weights, one around them would shadow all of them.
func validateModelDeploymentRoleReservedMountPaths(
	md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if md.Spec.Model.ArtifactRef == nil {
		return nil
	}

	var errs field.ErrorList
	for i, av := range role.AdditionalVolumes {
		for _, reserved := range []string{workerctrl.ModelDeploymentModelMountPath, workerctrl.ModelDeploymentModelCachePath} {
			if !pathsOverlap(av.MountPath, reserved) {
				continue
			}
			errs = append(errs, field.Invalid(rolePath.Child("additionalVolumes").Index(i).Child("mountPath"),
				av.MountPath, fmt.Sprintf(
					"overlaps %q, where the operator mounts the weights spec.model.artifactRef names", reserved)))
			break
		}
	}

	return errs
}

// validateModelDeploymentRoleExtraArgs refuses an append-tier argument the operator owns.
//
// A silent merge is what this prevents, and the reason is diagnosability rather than tidiness: two
// values for one connector argument leave no way to tell which one won, and the user who wrote the
// second has no way to learn the first exists.
//
// A PARALLELISM DECLARATION THE PARSE CANNOT READ IS REFUSED HERE AS WELL, because the KV transfer
// document is rendered from these same books: beside a declaration the engine itself would reject
// at startup, no default the operator could write into the document is anything but a wrong answer
// that starts. The refusal names the flag through the parse's own message. The books are the
// role's one argument stream -- its ExtraArgs, or its Command when that replaces the line -- and
// an unknown flag inside either stays admitted exactly as before: rejecting what IT does not know
// is the engine's own work. The inert ExtraArgs beside a take-over Command are no stream at all,
// so they are parsed by nobody, not even to be refused.
func validateModelDeploymentRoleExtraArgs(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	argsPath := rolePath.Child("extraArgs")
	for i, arg := range role.ExtraArgs {
		owned, ok := workerctrl.ModelDeploymentOwnedArg(engine, arg)
		if !ok {
			continue
		}

		// A spelling the engine reads as the owned key is named beside the key, or the refusal
		// would cite a flag the user never wrote.
		subject := fmt.Sprintf("%q", owned)
		if name := workerctrl.ModelDeploymentArgName(arg); name != owned {
			subject = fmt.Sprintf("%q is %q, which", name, owned)
		}
		errs = append(errs, field.Invalid(argsPath.Index(i), arg, fmt.Sprintf(
			"%s is set by the operator for engine %q and must not be supplied here, because two "+
				"values for it cannot be told apart; replace the whole command line through "+
				"%s to own it instead",
			subject, engine, rolePath.Child("command"),
		)))
	}

	if _, err := workerctrl.ParseModelDeploymentDeclaredParallelism(
		engine, workerctrl.ModelDeploymentRoleArgs(role), role.Env); err != nil {
		errs = append(errs, field.Invalid(rolePath, role.Name, fmt.Sprintf(
			"declares parallelism the KV transfer document must follow but cannot read: %s", err,
		)))
	}

	return errs
}

// validateModelDeploymentRoleParallelWidth is the first check that ever ties a role's declared
// parallelism to its request: the per-member engine width must fit the per-member card count.
//
// THE CHECK IS DELIBERATELY SMALL. It computes only from numbers the author already wrote and
// refuses only an arrangement that cannot start; everything ambiguous stays silent -- a wiring
// flag (some of the width may live off this member and the table does not model placement), an
// unreadable or zero card count, and a declaration the parse already refused. It never computes a
// right size and never reads a pool: the author's own books, arithmetic, and a refusal only when
// the arithmetic cannot run. The books are the role's one argument stream, so the inert ExtraArgs
// beside a take-over Command enter no width -- the Command is that role's declaration.
//
// THE WIDTH FORMULAS ARE THE ENGINES' OWN. vLLM places tensor x pipeline x prefill-context
// ranks on one member per data-parallel rank, and multiplies the data-parallel width on top:
// the declared LOCAL share when one is written above zero -- with no wiring flag that share is
// exactly the ranks this member runs -- else the full width, a declared zero reading as
// undeclared because it is the engine's own sentinel for DP specified externally.
// Decode-context reuses tensor ranks and expert-parallel is a mode, so neither enters the
// product. SGLang places tensor x pipeline x data-parallel, narrowing to tensor x pipeline when
// DP attention, DWDP, MoE-DP or attention-CP moves the data-parallel width inside the tensor
// world.
func validateModelDeploymentRoleParallelWidth(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	if role.Resources == nil || role.Resources.Accelerator == nil {
		return nil
	}

	declared, err := workerctrl.ParseModelDeploymentDeclaredParallelism(
		engine, workerctrl.ModelDeploymentRoleArgs(role), role.Env)
	if err != nil || len(declared.Wiring) > 0 {
		return nil
	}

	cards, ok := role.Resources.Accelerator.AsInt64()
	if !ok || cards <= 0 {
		return nil
	}

	// multiply folds one declared degree into the running width. Every degree is at least one,
	// so the product only grows, and once it would overflow an int64 the refusal is already
	// decided -- the card count fits in one too -- so the width pins one past the cards rather
	// than wrap, and the refusal message then names no figure.
	width, pinned := int64(1), false
	multiply := func(degree int) {
		if pinned {
			return
		}
		if width > math.MaxInt64/int64(degree) {
			width, pinned = cards+1, true
			return
		}
		width *= int64(degree)
	}

	var factors []string
	multiply(declared.TensorParallel)
	multiply(declared.PipelineParallel)
	if declared.TensorParallel > 1 {
		factors = append(factors, fmt.Sprintf("tensor-parallel %d", declared.TensorParallel))
	}
	if declared.PipelineParallel > 1 {
		factors = append(factors, fmt.Sprintf("pipeline-parallel %d", declared.PipelineParallel))
	}

	switch engine {
	case workercore.ModelDeploymentEngineVLLM:
		if declared.PrefillContextParallel > 1 {
			multiply(declared.PrefillContextParallel)
			factors = append(factors, fmt.Sprintf("prefill-context-parallel %d", declared.PrefillContextParallel))
		}
		// Any wiring flag already returned above, so a declared local share is exactly the DP
		// ranks this member runs: it IS the per-member data-parallel width and multiplies.
		if declared.DataParallelLocal > 1 {
			multiply(declared.DataParallelLocal)
			factors = append(factors, fmt.Sprintf("data-parallel-local %d", declared.DataParallelLocal))
		} else if declared.DataParallelLocal == 0 {
			multiply(declared.DataParallel)
			if declared.DataParallel > 1 {
				factors = append(factors, fmt.Sprintf("data-parallel %d", declared.DataParallel))
			}
		}
	case workercore.ModelDeploymentEngineSGLang:
		narrows := slices.Contains(declared.Modes, "--enable-dp-attention") ||
			declared.DWDPSize > 1 || declared.MoEDataParallel > 1 || declared.AttentionContextParallel > 1
		if !narrows {
			multiply(declared.DataParallel)
			if declared.DataParallel > 1 {
				factors = append(factors, fmt.Sprintf("data-parallel %d", declared.DataParallel))
			}
		}
	default:
		return nil
	}

	if width <= cards {
		return nil
	}

	needs := fmt.Sprintf("%d", width)
	if pinned {
		needs = fmt.Sprintf("more than %d", cards)
	}

	return field.ErrorList{field.Invalid(
		rolePath.Child("resources", "accelerator"), cards, fmt.Sprintf(
			"%d cards cannot hold the width the role declares (%s): the engine needs %s per "+
				"member, and the width is per member, so %s does not rescue it",
			cards, strings.Join(factors, ", "), needs, rolePath.Child("size"),
		))}
}

// validateModelDeploymentRoleEnv refuses an environment entry the operator owns.
//
// Ownership here is about what a key destroys rather than what it duplicates: the config-path
// variable is the only pointer to the file the operator wrote, so re-pointing it swaps the entire
// client configuration for another file's and moves every symptom one layer away from its cause.
// Keys the operator merely defaults are not owned, so a user's value wins there with no refusal.
//
// The set refused here must equal the set the renderer drops. The renderer drops unconditionally,
// including when a role takes over the command line, so this refuses unconditionally too: a
// disagreement between the two sets is what produces a silent drop, whichever way it leans.
func validateModelDeploymentRoleEnv(
	engine string, role *workercore.ModelDeploymentRole, rolePath *field.Path,
) field.ErrorList {
	var errs field.ErrorList

	for i := range role.Env {
		name := role.Env[i].Name
		if !workerctrl.ModelDeploymentOwnsEnv(engine, name) {
			continue
		}

		errs = append(errs, field.Invalid(rolePath.Child("env").Index(i), name, fmt.Sprintf(
			"%q is set by the operator for engine %q and must not be supplied here, because it "+
				"selects the client configuration the operator rendered; replace the whole "+
				"command line through %s to own it instead",
			name, engine, rolePath.Child("command"),
		)))
	}

	return errs
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
	if ress == nil {
		return nil
	}
	var errs field.ErrorList
	if ress.Interface != nil {
		count, whole := ress.Interface.AsInt64()
		if !whole || count < 0 {
			errs = append(errs, field.Invalid(rolePath.Child("resources", "interface"),
				ress.Interface.String(), "interface count must be a non-negative whole number"))
		}
	}
	if ress.AcceleratorPartitionedProfile == "" {
		return errs
	}

	if ress.AcceleratorSlicedMemoryPercentage == 0 && ress.AcceleratorSlicedCoresPercentage == 0 {
		return errs
	}

	ressPath := rolePath.Child("resources")

	return append(errs, field.Invalid(
		ressPath.Child("acceleratorPartitionedProfile"), ress.AcceleratorPartitionedProfile,
		fmt.Sprintf(
			"a partition profile cannot be combined with %s or %s: hardware partitioning and "+
				"software slicing cannot both apply to one accelerator",
			ressPath.Child("acceleratorSlicedMemoryPercentage"),
			ressPath.Child("acceleratorSlicedCoresPercentage"),
		),
	))
}

// validateRoleResourcesAgainstInstanceTypes applies the rules that need the InstanceType a role
// names, and is the only validation path here that reads another object.
//
// THE REFUSAL LANDS AT THE API INSTEAD OF DEEPER IN THE CHAIN, which is the whole of what these
// rules buy. Without them an infeasible request is still refused -- by the scheduling chain's own
// gates, on a Workload, naming neither the deployment nor the field the user wrote. The outcome was
// never wrong; the message was, and a message an operator cannot act on costs the time to find what
// this one states.
//
// AN OBJECT BEING DELETED IS NOT JUDGED BY THESE RULES. A rule that reads the type refuses when the
// type is absent, so leaving them on would let a deleted InstanceType block the very update that
// clears this deployment's finalizer. That is the same reasoning Default states for declining
// wholesale, applied to the part of validation that acquired the same dependency.
//
// A ROLE NAMING NO TYPE, OR NO RESOURCES, IS SKIPPED rather than looked up. An empty name is a field
// error the object-only rules already report, and looking it up would fail on the request and answer
// with a message about reading the cluster instead of about the name.
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
				// A FIELD ERROR JOINS THE OTHERS INSTEAD OF ENDING THE PASS. A named type that does
				// not exist is a fact about this object's field, and returning it bare does two
				// things wrong: the response comes back as a plain denial rather than an Invalid
				// naming the path, and it SHORT-CIRCUITS -- one mistyped instanceType hides every
				// other validation error on the object, so the operator fixes one thing per apply.
				//
				// A TRANSPORT FAILURE IS CARRIED AS A FIELD ERROR TOO AND MUST NOT BE FOLDED IN.
				// getInstanceType reports an unreadable API with field.InternalError, which is a
				// *field.Error like the not-found one and means something entirely different: the
				// object may be perfectly valid and the cluster could not be asked. Folding it in
				// would tell the operator their object is invalid on the strength of a failed read,
				// so it still ends the pass and the failure policy decides what happens.
				var fieldErr *field.Error
				if !errors.As(err, &fieldErr) || fieldErr.Type == field.ErrorTypeInternal {
					return nil, err
				}
				errs = append(errs, fieldErr)

				continue
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

	errs = append(errs, validateModelDeploymentZeroAcceleratorInMultiRoleGroup(md, seen, rolesPath)...)

	return append(errs, validateModelDeploymentPairCannotShareOneAccelerator(md, seen, rolesPath)...), nil
}

// validateModelDeploymentZeroAcceleratorInMultiRoleGroup refuses a role that requests nothing an
// accelerated queue covers while sharing that instance type with another role.
//
// WHAT IT PREVENTS IS A REPLICA THAT RUNS UNCHARGED. The queue covers only the manufacturer's
// credits and the manager ignores what a queue does not declare, so such a replica reserves,
// reports Admitted and runs while the queue's usage of those credits stays at zero -- measured on a
// cluster, with nothing erroring. Its siblings on the same type are charged for every card they
// hold and compete for a pool this one spends from without being counted.
//
// THE RULE IS SCOPED TO A SHARED TYPE rather than to every zero, because a role alone on an
// acceleratable type is a deployment whose whole pool went uncharged -- visible as a deployment
// that never consumes quota -- while one hidden among charged siblings is not visible at all.
// NOTE that the reason above is not the one this rule was written for. That one was Kueue refusing
// a partial assignment for a Workload whose PodSets outnumbered its assignments, and it cannot
// happen now that every replica carries a Workload of a single PodSet.
func validateModelDeploymentZeroAcceleratorInMultiRoleGroup(
	md *workercore.ModelDeployment,
	instanceTypes map[string]*worker.InstanceType,
	rolesPath *field.Path,
) field.ErrorList {
	groupSizes := make(map[string]int, len(instanceTypes))
	for i := range md.Spec.Roles {
		groupSizes[md.Spec.Roles[i].InstanceType]++
	}

	var errs field.ErrorList
	for i := range md.Spec.Roles {
		role := &md.Spec.Roles[i]
		instType := instanceTypes[role.InstanceType]
		if instType == nil || !instType.Spec.Acceleratable || groupSizes[role.InstanceType] < 2 ||
			role.Resources == nil || role.Resources.Accelerator == nil || role.Resources.Accelerator.Sign() != 0 {
			continue
		}

		errs = append(errs, field.Invalid(
			rolesPath.Index(i).Child("resources", "accelerator"), role.Resources.Accelerator.String(),
			fmt.Sprintf(
				"must be greater than zero because the queue behind acceleratable instance type %q "+
					"accounts only in accelerator credits: this role's replicas would be admitted and "+
					"run while that queue charges them nothing, spending from the pool its sibling "+
					"roles on the same type are charged for; request at least one accelerator or use "+
					"a non-acceleratable instance type for this CPU-only role",
				role.InstanceType,
			),
		))
	}

	return errs
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
			// AN UNSET GROUP IS UNKNOWN, NOT A GROUP THAT TWO TYPES SHARE. The InstanceType webhook
			// requires the field on an acceleratable type, but only objects that passed that rule
			// carry it, and two empty strings comparing equal would refuse a pair on the strength of
			// what neither type says. Refusing a valid deployment because a field is blank is the
			// wrong way for this rule to be wrong: it blocks the shape the split exists to enable,
			// and the operator has nothing to edit, since instanceType is frozen.
			if decodeType == nil ||
				prefillType.Spec.AcceleratorGroup == "" || decodeType.Spec.AcceleratorGroup == "" ||
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
// SEVERAL instanceTypes ARE SEVERAL CLUSTER QUEUES, and the queues are what this rule turns on.
// Being several Workloads is not what separates the refused shape from the accepted one -- every
// deployment of more than one replica is several Workloads, since each replica is admitted as its
// own. Being answered by several QUEUES is. What relates Workloads across queues is the
// joint-admission check, and that check reaches a Workload only through a ClusterQueue that
// references it -- which the queue reconciler does only while the derived-from-node setting is on.
// With it off an administrator authors queues through the InstanceType API, no queue carries the
// check, and the barrier is not installed anywhere.
//
// THE SHAPE IS REFUSED RATHER THAN ADMITTED UNGUARDED. Admitting it would let a prefiller start and
// serve while its decoder waits for capacity that never arrives -- a deployment that reads as
// half-started and is in fact never going to finish, with nothing naming the reason. A refusal at the
// API names the setting, which is the one thing the operator can act on.
//
// A SINGLE-instanceType DEPLOYMENT IS NOT REFUSED HERE whatever the setting says, and that is
// narrower than it reads. Its roles are still several Workloads and Kueue still admits them one at
// a time; what it is not is several queues, so there is no second quota pool that can be empty
// while the first is not. The joint-admission barrier still covers such a deployment wherever a
// queue carries the check -- this rule is about the shape no check can be installed for, not about
// a shape that needs none.
func validateModelDeploymentBarrierIsInstallable(
	ctx context.Context, md *workercore.ModelDeployment,
) field.ErrorList {
	// A DEPLOYMENT BEING DELETED IS NOT ASKING TO BE ADMITTED, and refusing it strands the object.
	// This rule reads cluster state, so it can start refusing an object that was accepted: a
	// multi-instanceType deployment created while the setting was on is refused by this rule the
	// moment the setting goes off. Validation still runs during the deletion window, so the update
	// that clears the finalizer is refused too -- and since instanceType is frozen, the operator
	// cannot edit their way out of it either. The object then has no reachable state from which it
	// can be removed.
	//
	// IT IS THE SAME GATE THE TYPE-READING RULES TAKE, for the same reason rather than by analogy: a
	// rule whose answer depends on something outside the object must not be able to hold that object
	// hostage. What it costs is named rather than hidden -- an edit made while a deployment is being
	// deleted can produce a multi-instanceType shape the cluster cannot gate. Nothing renders it,
	// because the deployment is going away.
	if md.DeletionTimestamp != nil {
		return nil
	}

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
// one asking for a mode it does not offer, and one asking for more whole cards than its pool has.
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
		errs = append(errs, validateRoleSingleCardRequest(ress, "partitioned", ressPath)...)
	case ress.AcceleratorSlicedMemoryPercentage != 0 || ress.AcceleratorSlicedCoresPercentage != 0:
		// A pool with no logically sliceable card cannot serve the request at all: admitted, such a
		// role stays Pending forever rather than being reshaped into a whole-card one.
		if !instType.Status.Detail.IsLogicallySliceable() {
			errs = append(errs, field.Forbidden(
				ressPath.Child("acceleratorSlicedMemoryPercentage"),
				fmt.Sprintf("instance type %s does not offer logical slicing", instType.Name)))
		}
		errs = append(errs, validateRoleSingleCardRequest(ress, "sliced", ressPath)...)
	default:
		// THE WHOLE-CARD CEILING BOUNDS A WHOLE-CARD REQUEST AND NOTHING ELSE, which is why it sits
		// inside this branch. status.accelerator is the whole-card view and counts UNPARTITIONED
		// cards only, so a pool whose cards are all partitioned reports zero there while its
		// partitioned view serves requests all day. Applied to every mode it refuses a valid
		// partitioned or sliced role with a ceiling of 0 -- a sentence that misdescribes the type
		// rather than the request.
		//
		// AND THE REFUSED VALUE IS ONE THIS WEBHOOK WROTE. A role that names no count is defaulted to
		// one card by the mutating half, so the operator never typed the number the rule rejects and
		// has nothing to correct. A refusal nobody can act on is worse than no rule.
		//
		// The Instance webhook already draws the line here: validateExclusiveAcceleratorRequest is
		// reached on the whole-card path only.
		//
		// THE CEILING IS THE VIEW'S CAPACITY, NOT ITS ONCE-MAX-REQUEST. onceMaxRequest is the
		// largest single node's FREE cards, so it falls to zero whenever the pool is busy, and read
		// here it refused every deployment submitted while the cards were held. That is the queue's
		// question, not admission's: a deployment the pool can serve once a card is released is
		// admitted and waits in its queue. Capacity does not move with occupancy.
		//
		// IT IS A LOOSE BOUND, and that is a known limit rather than a guarantee. Capacity sums the
		// whole pool, while one Pod's cards come from one node, so on a pool of several nodes a
		// request larger than the largest node but within the pool's total is admitted and then
		// stays queued. The status carries no per-node capacity to bound it tighter.
		//
		// THE CEILING IS CARRIED IN THE MESSAGE rather than left for the reader to look up. "Exceeds
		// the maximum" states that the request was wrong; the number states what would be right, and
		// the difference is whether the next attempt is a guess.
		if ress.Accelerator != nil {
			if ceiling := instType.Status.Accelerator.Capacity; ress.Accelerator.Cmp(ceiling) > 0 {
				errs = append(errs, field.Invalid(
					ressPath.Child("accelerator"), ress.Accelerator.String(),
					fmt.Sprintf("instance type %s has at most %s whole accelerator(s) in its pool",
						instType.Name, ceiling.String())))
			}
		}
	}

	return errs
}

// validateRoleSingleCardRequest refuses a sliced or partitioned role that asks for more than one
// card.
//
// A SLICE IS A FRACTION OF ONE CARD AND A PARTITION IS ONE INSTANCE ON ONE CARD, so the size of the
// request is carried by the percentages or the profile name and the card count is always one. Two
// cards at fifty percent each describes nothing the scheduler can place, and the resource key the
// controller emits for it is a per-card one either way.
//
// IT IS WHAT BOUNDS THESE MODES NOW THAT THE WHOLE-CARD CEILING DOES NOT. The Instance webhook has
// drawn this line all along, with validateSingleCardRequest on exactly these two paths; the rule was
// simply missing here, and its absence was hidden because the whole-card ceiling happened to refuse
// the same over-large requests for the wrong reason -- while refusing correct one-card requests too.
func validateRoleSingleCardRequest(
	ress *workercore.ModelDeploymentRoleResources, kind string, ressPath *field.Path,
) field.ErrorList {
	one := resource.NewQuantity(1, resource.DecimalSI)
	if ress.Accelerator != nil && ress.Accelerator.Cmp(*one) == 0 {
		return nil
	}

	got := "0"
	if ress.Accelerator != nil {
		got = ress.Accelerator.String()
	}

	return field.ErrorList{field.Invalid(
		ressPath.Child("accelerator"), got,
		fmt.Sprintf("accelerator request must be exactly 1 for a %s request", kind))}
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
