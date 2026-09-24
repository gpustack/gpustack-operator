package worker

import (
	"context"
	"encoding/json"
	"slices"
	"strconv"
	"strings"

	admissionv1 "k8s.io/api/admission/v1"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrladmission "sigs.k8s.io/controller-runtime/pkg/webhook/admission"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/webhook"
	workerctrl "gpustack.ai/gpustack/pkg/worker/controllers/worker"
	"gpustack.ai/gpustack/pkg/worker/settings"
)

// WorkloadWebhook pins a Kueue Workload that this operator's InstanceType chain admits to nodes
// whose fit labels show that one card can still host it, so Kueue's topology-aware scheduling skips
// a node whose free room is spread over cards none of which fits.
//
// Per PodSet, a logical slice of U units per card requires the sliced-max-free-units label to be
// greater than U-1, and a shared request of N >= 2 cards requires the shared-free-cards label to be
// greater than N-1. The expression is ANDed into every required node-selector term of the PodSet
// template. TAS reads that template's affinity; the Pod and its owner's template never carry it,
// because Kueue copies only labels, annotations, a nodeSelector, tolerations and scheduling gates
// onto them. That matters: the kubelet re-admits a running Pod against current node labels when it
// restarts, and a fit label always falls below the threshold once the Pod holds its card.
//
// Kueue may serve other tenants, so a Workload is this operator's only when its LocalQueue points
// at a ClusterQueue carrying the operator's InstanceType mark and a same-named InstanceType names
// an accelerator group. Anything else, and any read that fails, leaves the Workload untouched: the
// webhook never denies, and without the pin the node-devices check still holds a request placed on
// a node that cannot host it.
//
// An UPDATE is acted on only while the old Workload holds no quota reservation. Kueue rebuilds a
// suspended job's Workload spec in place, which would drop the pin, and it lets PodSets change only
// under that same condition.
//
// nolint: lll
// +k8s:webhook-gen:mutating:group="kueue.x-k8s.io",version="v1beta2",resource="workloads",scope="Namespaced"
// +k8s:webhook-gen:mutating:operations=["CREATE","UPDATE"],failurePolicy="Ignore",sideEffects="None",matchPolicy="Equivalent",timeoutSeconds=10
// +k8s:webhook-gen:mutating:matchConditions=[{"name":"gpustack-local-queue","expression":"has(object.spec.queueName) && object.spec.queueName.startsWith('gpustack-fnv64-')"}]
// +k8s:webhook-gen:mutating:namePrefix="gpustack-worker"
type WorkloadWebhook struct {
	Client ctrlcli.Client
}

func (r *WorkloadWebhook) SetupWebhook(_ context.Context, opts webhook.SetupOptions) (runtime.Object, error) {
	r.Client = opts.Manager.GetClient()

	return &kueue.Workload{}, nil
}

var _ ctrladmission.Defaulter[runtime.Object] = (*WorkloadWebhook)(nil)

// fitPin is one PodSet's per-Pod demand, which Default turns into node-affinity expressions.
type fitPin struct {
	podSet      int
	slicedUnits int32
	sharedCards int32
}

func (r *WorkloadWebhook) Default(ctx context.Context, obj runtime.Object) error {
	wl := obj.(*kueue.Workload)
	logger := ctrllog.FromContext(ctx).WithValues("workload", ctrlcli.ObjectKeyFromObject(wl))

	if !strings.HasPrefix(string(wl.Spec.QueueName), nodefeature.LocalQueueNamePrefix) {
		return nil
	}
	pins := fitPinsOf(wl)
	if len(pins) == 0 {
		return nil
	}
	if !settings.WorkloadFitAffinity.ShouldValueBool(ctx) {
		return nil
	}
	if reserved, err := oldWorkloadHoldsQuota(ctx); err != nil || reserved {
		if err != nil {
			logger.Error(err, "decode old workload, leaving it unpinned")
		}
		return nil
	}

	group, err := r.acceleratorGroup(ctx, wl)
	if err != nil {
		logger.Error(err, "resolve the workload's instance type, leaving it unpinned")
		return nil
	}
	if group == "" {
		return nil
	}

	// An administrator's InstanceType may name a group that is not label grammar; a key built from it
	// would make the affinity invalid, so such a Workload is left unpinned.
	slicedKey, sharedKey := nodefeature.FitSlicedMaxFreeUnitsLabelKey(group), nodefeature.FitSharedFreeCardsLabelKey(group)
	if !nodefeature.IsFitLabelKey(slicedKey) || !nodefeature.IsFitLabelKey(sharedKey) {
		logger.Info("accelerator group is not a valid label key part, leaving the workload unpinned", "group", group)
		return nil
	}
	for _, p := range pins {
		spec := &wl.Spec.PodSets[p.podSet].Template.Spec
		if p.slicedUnits > 0 {
			requireNodeExpression(spec, slicedKey, p.slicedUnits)
		}
		if p.sharedCards >= 2 {
			requireNodeExpression(spec, sharedKey, p.sharedCards)
		}
	}
	return nil
}

// fitPinsOf returns the PodSets of a Workload that ask for a fit pin, with their per-Pod demand.
// A shared request of one card needs none: the node's summed shared capacity already counts only
// cards that can still grant a share.
func fitPinsOf(wl *kueue.Workload) []fitPin {
	var pins []fitPin
	for i := range wl.Spec.PodSets {
		units, cards := workerctrl.PodSetFitDemand(&wl.Spec.PodSets[i])
		if units > 0 || cards >= 2 {
			pins = append(pins, fitPin{podSet: i, slicedUnits: units, sharedCards: cards})
		}
	}
	return pins
}

// oldWorkloadHoldsQuota reports whether the request is an UPDATE of a Workload that already holds a
// quota reservation, whose PodSets Kueue no longer lets change. A context carrying no admission
// request is treated as a CREATE.
func oldWorkloadHoldsQuota(ctx context.Context) (bool, error) {
	req, reqErr := ctrladmission.RequestFromContext(ctx)
	if reqErr == nil && req.Operation == admissionv1.Update {
		old := new(kueue.Workload)
		if err := json.Unmarshal(req.OldObject.Raw, old); err != nil {
			return false, err
		}
		return kueueworkload.HasQuotaReservation(old), nil
	}
	return false, nil
}

// acceleratorGroup follows the queue chain this operator builds, from the Workload's LocalQueue to a
// ClusterQueue carrying the InstanceType mark and on to the same-named InstanceType, and returns
// that InstanceType's accelerator group. It returns an empty group when any link is missing or is
// not this operator's.
func (r *WorkloadWebhook) acceleratorGroup(ctx context.Context, wl *kueue.Workload) (string, error) {
	namespace := wl.Namespace
	if req, err := ctrladmission.RequestFromContext(ctx); err == nil && req.Namespace != "" {
		namespace = req.Namespace
	}
	lq := new(kueue.LocalQueue)
	err := r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: namespace, Name: string(wl.Spec.QueueName)}, lq)
	if err != nil {
		return "", ctrlcli.IgnoreNotFound(err)
	}
	cq := new(kueue.ClusterQueue)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: string(lq.Spec.ClusterQueue)}, cq)
	if err != nil {
		return "", ctrlcli.IgnoreNotFound(err)
	}
	if !workerctrl.IsInstanceTypeClusterQueue(cq) {
		return "", nil
	}
	it := new(workercore.InstanceType)
	err = r.Client.Get(ctx, ctrlcli.ObjectKey{Name: cq.Name}, it)
	if err != nil {
		return "", ctrlcli.IgnoreNotFound(err)
	}
	return it.Spec.AcceleratorGroup, nil
}

// requireNodeExpression requires the node label key to be greater than need-1, ANDed into every
// required node-selector term of spec, or as the only term when there is none.
//
// An empty term, and a required selector that already exists with no term, are left alone: each
// matches no node, and adding the expression would make it match some. An expression already present, the same key, operator and value, is not added twice, and any
// other expression is kept, so a stricter requirement someone wrote on the same key still holds.
func requireNodeExpression(spec *core.PodSpec, key string, need int32) {
	pin := core.NodeSelectorRequirement{
		Key:      key,
		Operator: core.NodeSelectorOpGt,
		Values:   []string{strconv.FormatInt(int64(need)-1, 10)},
	}

	if spec.Affinity == nil {
		spec.Affinity = &core.Affinity{}
	}
	if spec.Affinity.NodeAffinity == nil {
		spec.Affinity.NodeAffinity = &core.NodeAffinity{}
	}
	na := spec.Affinity.NodeAffinity
	// Terms are ORed, so the pin joins every one of them; a template requiring nothing gets one term.
	if na.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		na.RequiredDuringSchedulingIgnoredDuringExecution = &core.NodeSelector{
			NodeSelectorTerms: []core.NodeSelectorTerm{{MatchExpressions: []core.NodeSelectorRequirement{pin}}},
		}
		return
	}
	required := na.RequiredDuringSchedulingIgnoredDuringExecution
	for i := range required.NodeSelectorTerms {
		term := &required.NodeSelectorTerms[i]
		if len(term.MatchExpressions) == 0 && len(term.MatchFields) == 0 {
			continue
		}
		if !slices.ContainsFunc(term.MatchExpressions, func(e core.NodeSelectorRequirement) bool {
			return e.Key == pin.Key && e.Operator == pin.Operator && slices.Equal(e.Values, pin.Values)
		}) {
			term.MatchExpressions = append(term.MatchExpressions, pin)
		}
	}
}
