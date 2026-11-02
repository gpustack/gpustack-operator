// SPDX-FileCopyrightText: 2026 GPUStack, Inc.
// SPDX-License-Identifier: Apache-2.0

package deviceplugin

import (
	"context"
	"fmt"
	"slices"
	"time"
	"unicode/utf8"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	ctrlsource "sigs.k8s.io/controller-runtime/pkg/source"

	apiv1 "gpustack.ai/gpustack/api/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/device"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/utils/osx"
)

// rdmaSelectionResyncPeriod is the periodic requeue every pass arms so an Unknown observation
// or a missed event still converges: the bound is the repo's informer-resync floor, the same
// value the manager refuses to configure an informer cache below.
const rdmaSelectionResyncPeriod = 5 * time.Minute

const (
	// RDMASelectionResolvedConditionType is the root Devices condition type this observer
	// owns, and the RDMA selection's only condition. It reports whether the node resolved a
	// selection, the no-policy default included, and its message names the cause of a failed
	// resolution.
	RDMASelectionResolvedConditionType = "RDMASelectionResolved"

	// RDMASelectionUnknownReason says the inputs cannot decide: a policy list or local Node
	// read failed, or the local verbs observation is missing or incomplete.
	RDMASelectionUnknownReason = "ResolutionUnknown"
	// RDMASelectionNoPolicyReason says a successful list found no policy targeting the node,
	// so the resolved default holds. With no policyRef, this is the only resolved default.
	RDMASelectionNoPolicyReason = "NoPolicy"
	// RDMASelectionPolicyResolvedReason says exactly one policy targets the node and its
	// reference is the policyRef.
	RDMASelectionPolicyResolvedReason = "PolicyResolved"
	// RDMASelectionPolicyConflictReason says more than one policy targets the node.
	RDMASelectionPolicyConflictReason = "PolicyConflict"
	// RDMASelectionPolicyInvalidReason says the one targeting policy carries rules the node's
	// complete inventory cannot apply; its reference is still the policyRef.
	RDMASelectionPolicyInvalidReason = "PolicyInvalid"
)

// rdmaSelectionMessageLimit is the message length the generated Devices CRD permits on
// status.conditions[].message. The observer tests read the same schema path from
// workercore.GetCustomResourceDefinitions, so a schema change fails them until this limit
// follows.
const rdmaSelectionMessageLimit = 32768

// rdmaSelectionBoundMessage caps a condition message at the schema limit. The resolver and
// read failures report unbounded diagnostics — one invalid policy can name tens of
// thousands of unknown devices — and a message past the limit makes a validating API
// server reject the whole status patch, leaving the preceding condition displayed. An
// oversized message keeps its leading text and closes with a mark naming the original
// length, so the loss stays visible; the count is in code points, the limit's own unit,
// and the cut lands between runes, so the published message is valid UTF-8.
func rdmaSelectionBoundMessage(message string) string {
	total := utf8.RuneCountInString(message)
	if total <= rdmaSelectionMessageLimit {
		return message
	}
	mark := fmt.Sprintf(" ... [truncated from %d characters]", total)
	keep := rdmaSelectionMessageLimit - utf8.RuneCountInString(mark)
	return string([]rune(message)[:keep]) + mark
}

// RDMASelectionObserver observes the desired RDMA selection for this node and publishes only
// status.interfaces.rdmaSelectionPolicy.policyRef, the status.interfaces.rdmaSelectionPolicy.excluded
// list, and the root RDMASelectionResolved condition. The observation fails closed: a resolution
// that cannot be applied — Invalid, Conflict, or Unknown — withdraws the previous selection
// rather than keeping it stale, and reports the cause in the condition message.
//
// Every input is read through the manager's cached client each pass, and the controller watches
// this node's Devices, every DevicesRDMASelectionPolicy, and the local Node on label changes, so
// a policy or label edit wakes a pass immediately instead of waiting out the polling interval.
// The device manager narrows each node's cache to its own Devices and Node with CacheByObject,
// so no node's informer carries another node's state, and the policy informer is the only
// cluster-wide one. Only a cache miss on the local Node — a fresh start racing the informer's
// first sync — falls back to one read through the uncached reader, and a read that still fails
// publishes Unknown and retries on the polling interval.
type RDMASelectionObserver struct {
	// NodeName is the node whose Devices object this observer publishes onto. SetupController
	// fills it from the KUBERNETES_NODE_NAME environment variable.
	NodeName string

	// Client is the manager's cached client, used for the Devices, policy list, and local
	// Node reads, and for the status patch.
	Client ctrlcli.Client

	// APIReader is the manager's uncached reader, used only as a fallback when a local Node
	// read through Client returns NotFound before the node informer's first sync.
	APIReader ctrlcli.Reader

	// RDMAInterfaceInventory is the local verbs observation source, wired by the device
	// manager before controller setup. Nil leaves the inventory unknown, which the resolver
	// reports as such.
	RDMAInterfaceInventory func() *device.RDMASnapshot

	// Reconciler is the local DevicesReconciler serving RDMA advertisements and grants. When
	// wired, the observer applies the resolved selection on it for subsequent advertisements
	// and allocations; a failed resolution applies an empty selection, so every HCA goes
	// unhealthy and new grants carry nothing until the resolution recovers.
	Reconciler *DevicesReconciler

	// events wakes this observer when the detector records a local RDMA interface snapshot
	// change between resync ticks.
	events chan ctrlevent.GenericEvent
}

// SetupController wires the clients and registers the controller: it watches this node's
// Devices object for creation and spec changes, every DevicesRDMASelectionPolicy for
// generation changes, and the local Node for creation, deletion and label changes, plus the
// local wake-up channel. The watches ride the manager's shared cache — which CacheByObject has
// narrowed to this node's own Devices and Node — and setup contacts no API server.
func (r *RDMASelectionObserver) SetupController(_ context.Context, opts controller.SetupOptions) error {
	r.NodeName = osx.Getenv("KUBERNETES_NODE_NAME")
	r.Client = opts.Manager.GetClient()
	r.APIReader = opts.Manager.GetAPIReader()
	r.events = make(chan ctrlevent.GenericEvent, 1)

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("deviceplugin.observe.rdmaselection").
		For(
			&workercore.Devices{},
			ctrlbuilder.WithPredicates(
				r.devicesEventPredicate(),
			),
		).
		Watches(
			// A policy edit re-resolves the selection for this node without waiting out the
			// polling interval; generation-only keeps the watch from firing on status echo.
			&workercore.DevicesRDMASelectionPolicy{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueDevicesForThisNode),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.GenerationChangedPredicate{},
			),
		).
		Watches(
			// The node's labels are scheduling input; its status and annotations are not, so
			// only a label change (or the object arriving or leaving) wakes a pass.
			&core.Node{},
			ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueDevicesForThisNode),
			ctrlbuilder.WithPredicates(
				r.nodeEventPredicate(),
			),
		).
		WatchesRawSource(
			ctrlsource.Channel(
				r.events,
				ctrlhandler.EnqueueRequestsFromMapFunc(r.enqueueDevicesForThisNode),
			),
		).
		Complete(r)
}

// Notify enqueues a reconcile pass for this node's Devices without blocking.
func (r *RDMASelectionObserver) Notify() {
	if r == nil || r.events == nil {
		return
	}
	select {
	case r.events <- ctrlevent.GenericEvent{}:
	default:
	}
}

func (r *RDMASelectionObserver) enqueueDevicesForThisNode(
	context.Context, ctrlcli.Object,
) []ctrlreconcile.Request {
	return []ctrlreconcile.Request{
		{
			NamespacedName: ctrlcli.ObjectKey{
				Name: r.NodeName,
			},
		},
	}
}

// devicesEventPredicate enqueues this node's Devices on creation, on spec changes, and on
// deletion, and ignores status-only updates — this writer's own publishes included. That is
// what keeps a published status from re-triggering its own reconcile, including when an
// older Devices CRD prunes the fields the observer writes: the echo of every such write is
// a status-only update the predicate drops, so the write loop stays bounded at the pass
// cadence the polling interval and genuine spec changes set.
func (r *RDMASelectionObserver) devicesEventPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.And(
		ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
			return object.GetName() == r.NodeName
		}),
		ctrlpredicate.GenerationChangedPredicate{},
	)
}

// nodeEventPredicate enqueues this node's Devices on the node's creation and deletion, and on
// label changes only: a kubelet heartbeat or a status or annotation update carries no
// scheduling input, so waking the pass for one would only churn the write loop the polling
// interval already bounds.
func (r *RDMASelectionObserver) nodeEventPredicate() ctrlpredicate.Predicate {
	return ctrlpredicate.And(
		ctrlpredicate.NewPredicateFuncs(func(object ctrlcli.Object) bool {
			return object.GetName() == r.NodeName
		}),
		ctrlpredicate.LabelChangedPredicate{},
	)
}

// Reconcile publishes one observation for the node's Devices: read the three resolution inputs,
// resolve through the shared resolver, and patch the owned fields only when they differ from
// the stored state. The published state fails closed: for Policy and Default the resolved
// policyRef and exclusions land; for Invalid, Conflict, and Unknown the exclusions are
// cleared — and the reference is withdrawn, except that an Invalid naming one offending
// policy keeps its reference so the report carries provenance. Read failures settle as
// Unknown and requeue on the polling interval; a Devices read or write that fails returns a
// retryable error instead of claiming a settled status. When a Reconciler is wired, the
// resolved selection is applied to it before status publication, so local advertisement and
// grant enforcement never fail open when a status write fails.
func (r *RDMASelectionObserver) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	nodeName := req.Name
	logger := ctrllog.FromContext(ctx).WithValues("node", nodeName)

	devs := new(workercore.Devices)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, devs)
	if apierrors.IsNotFound(err) {
		// Nothing to publish onto; the detector's create event brings the pass back.
		return ctrl.Result{}, nil
	}
	if err != nil {
		logger.Error(err, "fetch devices")
		return ctrl.Result{}, err
	}

	observation := r.observe(ctx, nodeName)
	// Fail closed on the local side before touching status: applying first is what keeps
	// advertisement and grant enforcement from failing open while the status write below
	// fails, and a resolution that agrees with the stored state changes nothing here.
	if r.Reconciler != nil {
		r.Reconciler.applyRDMASelection(observation)
	}
	condition := rdmaSelectionResolvedConditionOf(observation, devs.Generation)

	// Fail closed: an unresolved selection withholds its previous reference and exclusions
	// instead of keeping them stale. Only Policy and Default carry a resolved selection's
	// exclusions; PolicyRef is nil for Default, Conflict, and Unknown, and names the
	// offending policy for an Invalid that has one.
	policyRef := observation.PolicyRef
	excluded := rdmaSelectionExclusionsOf(observation.Excluded)
	switch observation.Status {
	case RDMAPolicyResolutionPolicy, RDMAPolicyResolutionDefault:
	default:
		excluded = nil
	}

	if rdmaSelectionAgrees(devs, policyRef, excluded, condition) {
		return ctrl.Result{RequeueAfter: rdmaSelectionResyncPeriod}, nil
	}

	if err := r.publishSelection(ctx, devs, policyRef, excluded, condition); err != nil {
		logger.Error(err, "publish RDMA selection status")
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: rdmaSelectionResyncPeriod}, nil
}

// observe reads the three resolution inputs — the policy list and the local Node through the
// cached client, the verbs inventory through the wired source — and returns the desired
// resolution. A failed read is an Unknown resolution naming its cause, never an empty policy
// list: only a successful read permits the no-policy default. A local Node the cache has not
// populated yet falls back to one read through the uncached reader, so a fresh start does not
// publish Unknown for a full polling interval waiting on the informer's first sync.
func (r *RDMASelectionObserver) observe(ctx context.Context, nodeName string) RDMAPolicyResolution {
	policies := new(workercore.DevicesRDMASelectionPolicyList)
	if err := r.Client.List(ctx, policies); err != nil {
		return rdmaSelectionUnknownResolution(fmt.Errorf("list the RDMA selection policies: %w", err))
	}
	node := new(core.Node)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, node)
	if apierrors.IsNotFound(err) && r.APIReader != nil {
		// The cache has not synced this node's informer yet; settle it uncached rather than
		// publishing Unknown for a full polling interval.
		err = r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, node)
	}
	if err != nil {
		return rdmaSelectionUnknownResolution(fmt.Errorf("read the local node: %w", err))
	}

	var inventory *device.RDMASnapshot
	if r.RDMAInterfaceInventory != nil {
		inventory = r.RDMAInterfaceInventory()
	}

	return ResolveRDMAPolicy(RDMAPolicyInputs{
		Synced:    true,
		Node:      RDMAPolicyNodeInput{Name: node.Name, UID: string(node.UID), Labels: node.Labels},
		Policies:  policies.Items,
		Inventory: inventory,
	})
}

// rdmaSelectionUnknownResolution builds the Unknown resolution a failed read publishes, with
// the cause in the message.
func rdmaSelectionUnknownResolution(err error) RDMAPolicyResolution {
	return RDMAPolicyResolution{
		Status:  RDMAPolicyResolutionUnknown,
		Message: err.Error(),
	}
}

// rdmaSelectionResolvedConditionOf maps a resolution onto the condition this observer
// owns. The resolver's diagnostic message is carried for the states that have one, bounded
// to the condition message limit the generated schema carries, so no state can publish a
// message a validating API server would reject; the settled states state their conclusion
// plainly. observedGeneration is the Devices object's generation the observation was
// resolved at.
func rdmaSelectionResolvedConditionOf(
	resolution RDMAPolicyResolution, devicesGeneration int64,
) apiv1.Condition {
	condition := apiv1.Condition{
		Type:               RDMASelectionResolvedConditionType,
		ObservedGeneration: devicesGeneration,
	}
	switch resolution.Status {
	case RDMAPolicyResolutionPolicy:
		condition.Status = meta.ConditionTrue
		condition.Reason = RDMASelectionPolicyResolvedReason
		condition.Message = fmt.Sprintf(
			"the RDMA selection resolves from policy %s", resolution.PolicyRef.Name,
		)
	case RDMAPolicyResolutionDefault:
		condition.Status = meta.ConditionTrue
		condition.Reason = RDMASelectionNoPolicyReason
		condition.Message = "no RDMA selection policy targets this node"
	case RDMAPolicyResolutionInvalid:
		condition.Status = meta.ConditionFalse
		condition.Reason = RDMASelectionPolicyInvalidReason
		condition.Message = resolution.Message
	case RDMAPolicyResolutionConflict:
		condition.Status = meta.ConditionFalse
		condition.Reason = RDMASelectionPolicyConflictReason
		condition.Message = resolution.Message
	default: // RDMAPolicyResolutionUnknown
		condition.Status = meta.ConditionUnknown
		condition.Reason = RDMASelectionUnknownReason
		condition.Message = resolution.Message
	}
	condition.Message = rdmaSelectionBoundMessage(condition.Message)
	return condition
}

// rdmaSelectionAgrees reports whether the stored status already carries exactly the
// observation: the policy reference, the exclusions, and the resolved condition content,
// compared semantically, transition instants excluded. An agreeing observation writes nothing.
func rdmaSelectionAgrees(
	devs *workercore.Devices,
	ref *workercore.DevicesRDMASelectionPolicyReference,
	excluded []workercore.DevicesRDMAExclusion,
	condition apiv1.Condition,
) bool {
	if !rdmaSelectionPolicyRefEqual(rdmaSelectionStoredPolicyRef(devs), ref) {
		return false
	}
	if !rdmaSelectionExcludedEqual(rdmaSelectionStoredExcluded(devs), excluded) {
		return false
	}
	return rdmaSelectionResolvedEqual(
		rdmaSelectionStoredResolved(devs.Status.Conditions), condition,
	)
}

// rdmaSelectionStoredPolicyRef reads the policy reference out of a Devices status.
func rdmaSelectionStoredPolicyRef(
	devs *workercore.Devices,
) *workercore.DevicesRDMASelectionPolicyReference {
	if devs.Status.Interfaces == nil || devs.Status.Interfaces.RDMASelectionPolicy == nil {
		return nil
	}
	return devs.Status.Interfaces.RDMASelectionPolicy.PolicyRef
}

// rdmaSelectionStoredResolved reads the resolved condition out of a Devices status.
func rdmaSelectionStoredResolved(conditions []apiv1.Condition) *apiv1.Condition {
	for i := range conditions {
		if conditions[i].Type == RDMASelectionResolvedConditionType {
			return &conditions[i]
		}
	}
	return nil
}

// rdmaSelectionPolicyRefEqual compares two policy references semantically, nil beside nil
// included.
func rdmaSelectionPolicyRefEqual(
	have, want *workercore.DevicesRDMASelectionPolicyReference,
) bool {
	if have == nil || want == nil {
		return have == want
	}
	return kubemeta.DeepEqual(*have, *want)
}

// rdmaSelectionResolvedEqual compares a stored resolved condition against the wanted one on
// the fields the writer owns, the transition instant excluded: the same content rendered at
// another instant, or in another zone, is the same observation.
func rdmaSelectionResolvedEqual(have *apiv1.Condition, want apiv1.Condition) bool {
	return have != nil &&
		have.Status == want.Status &&
		have.Reason == want.Reason &&
		have.Message == want.Message &&
		have.ObservedGeneration == want.ObservedGeneration
}

// publishSelection patches the owned fields only: a merge patch over a deep copy of the
// fresh read, carrying the read's resource version, so a foreign status writer that moved
// the object in between is told so instead of overwritten. Withdrawing the reference or the
// exclusions clears the stored values rather than keeping them. NotFound means the Devices
// object is gone and there is nothing left to publish onto.
func (r *RDMASelectionObserver) publishSelection(
	ctx context.Context,
	devs *workercore.Devices,
	ref *workercore.DevicesRDMASelectionPolicyReference,
	excluded []workercore.DevicesRDMAExclusion,
	condition apiv1.Condition,
) error {
	patched := devs.DeepCopy()
	setRDMASelectionPolicyRef(patched, ref)
	setRDMASelectionExclusions(patched, excluded)
	setRDMASelectionResolvedCondition(&patched.Status.Conditions, condition, meta.Now())

	err := r.Client.Status().Patch(ctx, patched, ctrlcli.MergeFromWithOptions(
		devs, ctrlcli.MergeFromWithOptimisticLock{},
	))
	if apierrors.IsNotFound(err) {
		return nil
	}
	return err
}

// setRDMASelectionPolicyRef moves the policy reference on the patched copy. Withdrawing it
// clears the stored reference rather than keeping it stale.
func setRDMASelectionPolicyRef(
	devs *workercore.Devices, ref *workercore.DevicesRDMASelectionPolicyReference,
) {
	if ref == nil {
		if devs.Status.Interfaces != nil && devs.Status.Interfaces.RDMASelectionPolicy != nil {
			devs.Status.Interfaces.RDMASelectionPolicy.PolicyRef = nil
		}
		return
	}
	if devs.Status.Interfaces == nil {
		devs.Status.Interfaces = new(workercore.DevicesInterfacesStatus)
	}
	if devs.Status.Interfaces.RDMASelectionPolicy == nil {
		devs.Status.Interfaces.RDMASelectionPolicy = new(workercore.DevicesRDMASelectionPolicyStatus)
	}
	devs.Status.Interfaces.RDMASelectionPolicy.PolicyRef = ref.DeepCopy()
}

// setRDMASelectionExclusions moves the exclusion list on the patched copy. Withdrawing it
// clears the stored list rather than keeping it stale.
func setRDMASelectionExclusions(
	devs *workercore.Devices, excluded []workercore.DevicesRDMAExclusion,
) {
	if len(excluded) == 0 {
		if devs.Status.Interfaces != nil && devs.Status.Interfaces.RDMASelectionPolicy != nil {
			devs.Status.Interfaces.RDMASelectionPolicy.Excluded = nil
		}
		return
	}
	if devs.Status.Interfaces == nil {
		devs.Status.Interfaces = new(workercore.DevicesInterfacesStatus)
	}
	if devs.Status.Interfaces.RDMASelectionPolicy == nil {
		devs.Status.Interfaces.RDMASelectionPolicy = new(workercore.DevicesRDMASelectionPolicyStatus)
	}
	devs.Status.Interfaces.RDMASelectionPolicy.Excluded = slices.Clone(excluded)
}

// setRDMASelectionResolvedCondition upserts the resolved condition in the slice, leaving
// every foreign condition untouched. The transition instant moves only when the status
// changes or the condition is new, so a reason or message move is not a transition, and a
// condition that carries no stamp yet is stamped once.
func setRDMASelectionResolvedCondition(
	conditions *[]apiv1.Condition, want apiv1.Condition, now meta.Time,
) {
	stamp := now.Rfc3339Copy()
	for i := range *conditions {
		if (*conditions)[i].Type != want.Type {
			continue
		}
		have := (*conditions)[i]
		transitioned := have.Status != want.Status
		have.Status = want.Status
		have.Reason = want.Reason
		have.Message = want.Message
		have.ObservedGeneration = want.ObservedGeneration
		if transitioned || have.LastTransitionTime.IsZero() {
			have.LastTransitionTime = stamp
		}
		(*conditions)[i] = have
		return
	}
	want.LastTransitionTime = stamp
	*conditions = append(*conditions, want)
}

// rdmaSelectionExclusionsOf converts resolved policy exclusions into the wire status slice.
func rdmaSelectionExclusionsOf(
	exclusions []RDMAPolicyExclusion,
) []workercore.DevicesRDMAExclusion {
	if len(exclusions) == 0 {
		return nil
	}
	out := make([]workercore.DevicesRDMAExclusion, len(exclusions))
	for i := range exclusions {
		out[i] = workercore.DevicesRDMAExclusion{
			Name:   exclusions[i].Name,
			Reason: string(exclusions[i].Reason),
		}
	}
	return out
}

// rdmaSelectionStoredExcluded reads the resolved selection's exclusions out of a Devices status.
func rdmaSelectionStoredExcluded(
	devs *workercore.Devices,
) []workercore.DevicesRDMAExclusion {
	if devs.Status.Interfaces == nil || devs.Status.Interfaces.RDMASelectionPolicy == nil {
		return nil
	}
	return devs.Status.Interfaces.RDMASelectionPolicy.Excluded
}

// rdmaSelectionExcludedEqual compares two exclusion lists semantically, nil beside empty included.
func rdmaSelectionExcludedEqual(
	have, want []workercore.DevicesRDMAExclusion,
) bool {
	if len(have) != len(want) {
		return false
	}
	if len(have) == 0 {
		return true
	}
	return kubemeta.DeepEqual(have, want)
}
