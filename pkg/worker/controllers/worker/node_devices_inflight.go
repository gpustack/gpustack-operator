package worker

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/utils/clock"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueconstants "sigs.k8s.io/kueue/pkg/constants"
	kueueadmissioncheck "sigs.k8s.io/kueue/pkg/util/admissioncheck"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// A Workload answered Ready holds no card in the ledger until the device plugin's Allocate records
// its Pods, which happens only after Kueue admits it, the scheduler binds its Pods and the kubelet
// starts them. A second Workload judged inside that window would see the first one's cards as free.
// So every judgment counts, per node, the Pods of the Workloads already answered Ready that the
// ledger does not hold yet: the inflight Pods.

// _podNodeNameField is the Pod field the API server filters a list by bound node on.
const _podNodeNameField = "spec.nodeName"

// chargedKey names the Pods of one Workload podset bound to one node, the node named by its
// hostname as a topology assignment names it.
type chargedKey struct {
	namespace string
	workload  string
	podSet    kueue.PodSetReference
	hostname  string
}

// ledgerCharges is what chargeLedgers read: the hostnames whose ledger was rebuilt, and per Workload
// podset the Pods whose allocation that rebuilt ledger holds.
type ledgerCharges struct {
	hostnames sets.Set[string]
	pods      map[chargedKey]int32
}

// chargeLedgers replaces the Status of every pool node a demand can use with one rebuilt from the
// Pods bound to that node, through the device manager's own aggregation, and counts per Workload
// podset the Pods that rebuilt Status holds.
//
// Both come from one uncached list. The published Status is rebuilt by the device manager after a
// Pod's allocation lands on the Pod, so a Pod read as charged there may not be in the Status yet;
// counting it as charged would leave it in neither the ledger nor the inflight set.
//
// A demand naming a node needs that node only; a demand naming none can use every node of the pool.
func (r *NodeDevicesAdmissionReconciler) chargeLedgers(
	ctx context.Context, pool []scopedDevices, demands []familyDemand,
) (ledgerCharges, error) {
	logger := ctrllog.FromContext(ctx)

	needed := sets.New[string]()
	poolWide := false
	for _, d := range demands {
		if d.node == "" {
			poolWide = true
		}
		needed.Insert(d.node)
	}

	charges := ledgerCharges{hostnames: sets.New[string](), pods: make(map[chargedKey]int32)}
	for i := range pool {
		sd := &pool[i]
		if !poolWide && (sd.hostname == "" || !needed.Has(sd.hostname)) {
			continue
		}
		pods := new(core.PodList)
		if err := r.APIReader.List(ctx, pods, ctrlcli.MatchingFields{_podNodeNameField: sd.devices.Name}); err != nil {
			return ledgerCharges{}, err
		}
		sd.devices.Status, _ = deviceplugin.BuildDesiredStatus(logger, &sd.devices, pods)
		if sd.hostname == "" {
			continue
		}
		charges.hostnames.Insert(sd.hostname)
		for j := range pods.Items {
			pod := &pods.Items[j]
			name := pod.Annotations[kueue.WorkloadAnnotation]
			if name == "" || !podCharged(pod) {
				continue
			}
			key := chargedKey{
				namespace: pod.Namespace,
				workload:  name,
				podSet:    kueue.PodSetReference(pod.Labels[kueueconstants.PodSetLabel]),
				hostname:  sd.hostname,
			}
			charges.pods[key]++
		}
	}
	return charges, nil
}

// podCharged reports whether the Pod's allocation record holds every container that requests an
// accelerator card, so the ledger rebuilt from it carries the Pod's whole demand. A Pod partly
// allocated is not charged: its demand is then counted whole as inflight, on top of the part the
// ledger holds, which overstates the node's use rather than understating it. A record that cannot be
// read is not charged either, because the rebuild skips it too.
func podCharged(pod *core.Pod) bool {
	allocations, err := deviceplugin.AllocatedAcceleratorsOf(pod)
	if err != nil || len(allocations) == 0 {
		return false
	}
	containers := slices.Concat(pod.Spec.InitContainers, pod.Spec.Containers)
	for i := range containers {
		if !requestsCard(&containers[i]) {
			continue
		}
		if _, ok := allocations[containers[i].Name]; !ok {
			return false
		}
	}
	return true
}

// requestsCard reports whether the container asks for a card of any accelerator family.
func requestsCard(ctr *core.Container) bool {
	for name, qty := range mergedContainerResources(ctr) {
		family := nodefeature.ResourceFamilyOf(name)
		switch family {
		case nodefeature.ResourceFamilyExclusive, nodefeature.ResourceFamilyShared,
			nodefeature.ResourceFamilySliced, nodefeature.ResourceFamilyPartitioned:
			if isCardKey(name, family) && qty.Value() > 0 {
				return true
			}
		}
	}
	return false
}

// inflightDemands returns the demands of the inflight Pods on the nodes whose ledger was rebuilt:
// for each other Workload that holds its cards, the Pods its topology assignment places on such a
// node minus the Pods of it the ledger already holds there. Each demand keeps the flavor its own
// podset was assigned, so it is fitted on the cards its own judgment used, and the flavor's claim
// is added to the node's scopes.
//
// Only a topology assignment names the node an inflight Pod will reach. A Workload without one is
// not counted; every queue this operator derives assigns a hostname.
//
// It returns the hostname of a node where an inflight demand's flavor resolves to no card
// population, empty when there is none. Such a demand cannot be placed on any card, so leaving it
// out would report its room as free.
func (r *NodeDevicesAdmissionReconciler) inflightDemands(
	ctx context.Context, self *kueue.Workload, pool []scopedDevices, charges ledgerCharges,
) ([]familyDemand, string, error) {
	if charges.hostnames.Len() == 0 {
		return nil, "", nil
	}

	list := new(kueue.WorkloadList)
	if err := r.Client.List(ctx, list); err != nil {
		return nil, "", err
	}

	var inflight []familyDemand
	for _, wl := range r.withUnobservedReady(list.Items) {
		if wl.Namespace == self.Namespace && wl.Name == self.Name {
			continue
		}
		var demands []familyDemand
		for i := range wl.Spec.PodSets {
			ps := &wl.Spec.PodSets[i]
			flavor := assignedFlavor(wl, ps.Name)
			for _, n := range assignedNodes(wl, ps.Name) {
				if !charges.hostnames.Has(n.hostname) {
					continue
				}
				key := chargedKey{namespace: wl.Namespace, workload: wl.Name, podSet: ps.Name, hostname: n.hostname}
				left := n.count - charges.pods[key]
				if left <= 0 {
					continue
				}
				for _, d := range podSetFamilyDemands(ps, left) {
					d.flavor, d.node = flavor, n.hostname
					demands = append(demands, d)
				}
			}
		}
		if len(demands) == 0 {
			continue
		}
		holds, err := r.holdsCards(ctx, wl)
		if err != nil {
			return nil, "", err
		}
		if holds {
			inflight = append(inflight, demands...)
		}
	}
	sortDemands(inflight)

	unresolved, err := r.scopeInflightFlavors(ctx, pool, inflight)
	return inflight, unresolved, err
}

// holdsCards reports whether the Workload's cards are promised to it: it holds a quota reservation,
// is neither evicted, finished nor deactivated, and is either admitted or answered Ready by a check
// this controller owns. Once admitted its Pods are on their way whichever checks it went through.
func (r *NodeDevicesAdmissionReconciler) holdsCards(ctx context.Context, wl *kueue.Workload) (bool, error) {
	if !kueueworkload.HasQuotaReservation(wl) || kueueworkload.IsEvicted(wl) ||
		kueueworkload.IsFinished(wl) || !kueueworkload.IsActive(wl) {
		return false, nil
	}
	if kueueworkload.IsAdmitted(wl) {
		return true, nil
	}
	checks, err := kueueadmissioncheck.FilterForController(ctx, r.Client, wl.Status.AdmissionChecks, _NodeDevicesControllerName)
	if err != nil {
		return false, err
	}
	for _, name := range checks {
		if acs := kueueadmissioncheck.FindAdmissionCheck(wl.Status.AdmissionChecks, name); acs != nil &&
			acs.State == kueue.CheckStateReady {
			return true, nil
		}
	}
	return false, nil
}

// scopeInflightFlavors adds each inflight demand's flavor to the scopes of the node it names, read
// the way candidateDevices reads the Workload's own flavors, so collectCards stamps the cards that
// flavor covers there. It returns the hostname of the first node where a flavor is gone or pins no
// single accelerator key.
func (r *NodeDevicesAdmissionReconciler) scopeInflightFlavors(
	ctx context.Context, pool []scopedDevices, inflight []familyDemand,
) (string, error) {
	scopes := make(map[kueue.ResourceFlavorReference]flavorScope)
	for _, d := range inflight {
		if d.flavor == "" {
			return d.node, nil
		}
		scope, seen := scopes[d.flavor]
		if !seen {
			scope = flavorScope{flavor: d.flavor}
			rf := new(kueue.ResourceFlavor)
			err := r.Client.Get(ctx, ctrlcli.ObjectKey{Name: string(d.flavor)}, rf)
			switch {
			case err == nil:
				scope.acceleratorKey = flavorAcceleratorKey(rf)
				scope.acceleratorCount = flavorAcceleratorCount(rf, scope.acceleratorKey)
			case !apierrors.IsNotFound(err):
				return "", err
			}
			scopes[d.flavor] = scope
		}
		if scope.acceleratorKey == "" {
			return d.node, nil
		}
		for i := range pool {
			if pool[i].hostname == d.node && !slices.Contains(pool[i].scopes, scope) {
				pool[i].scopes = append(pool[i].scopes, scope)
			}
		}
	}
	return "", nil
}

// unresolvedInflightMessage explains a hold behind a Workload whose cards on the node cannot be
// told apart, which clears once that Workload's Pods are allocated.
func unresolvedInflightMessage(node string) string {
	return fmt.Sprintf("a Workload admitted to the node %q before this one has a flavor that resolves to no"+
		" card population there, so the cards it will take cannot be counted; will retry once its Pods"+
		" have been allocated", node)
}

// withUnobservedReady returns the cached Workloads, each replaced by the copy readyUnobserved holds
// while the cached one does not show that Ready yet, sorted by namespace and name so the inflight
// demands come out in the same order on every judgment.
//
// An entry is dropped once the cache shows the Ready, shows the Workload no longer holding its
// reservation, carries another Workload under its name, or no longer carries it at all: from then on
// the cache answers for it.
func (r *NodeDevicesAdmissionReconciler) withUnobservedReady(cached []kueue.Workload) []*kueue.Workload {
	out := make([]*kueue.Workload, 0, len(cached))
	seen := sets.New[types.NamespacedName]()
	for i := range cached {
		wl := &cached[i]
		key := types.NamespacedName{Namespace: wl.Namespace, Name: wl.Name}
		seen.Insert(key)
		written, pending := r.readyUnobserved[key]
		if !pending {
			out = append(out, wl)
			continue
		}
		if written.UID != wl.UID || showsChecks(wl, written) || !kueueworkload.HasQuotaReservation(wl) ||
			kueueworkload.IsEvicted(wl) || kueueworkload.IsFinished(wl) {
			delete(r.readyUnobserved, key)
			out = append(out, wl)
			continue
		}
		out = append(out, written)
	}
	for key := range r.readyUnobserved {
		if !seen.Has(key) {
			delete(r.readyUnobserved, key)
		}
	}
	slices.SortFunc(out, func(a, b *kueue.Workload) int {
		return cmp.Or(cmp.Compare(a.Namespace, b.Namespace), cmp.Compare(a.Name, b.Name))
	})
	return out
}

// showsChecks reports whether wl carries every Ready check written carries.
func showsChecks(wl, written *kueue.Workload) bool {
	for i := range written.Status.AdmissionChecks {
		w := &written.Status.AdmissionChecks[i]
		if w.State != kueue.CheckStateReady {
			continue
		}
		if acs := kueueadmissioncheck.FindAdmissionCheck(wl.Status.AdmissionChecks, w.Name); acs == nil ||
			acs.State != kueue.CheckStateReady {
			return false
		}
	}
	return true
}

// recordVerdict keeps a copy of a Workload just answered Ready, as written, until the cache shows
// it, and forgets any earlier copy when the verdict is anything else.
func (r *NodeDevicesAdmissionReconciler) recordVerdict(
	wl *kueue.Workload, written []kueue.AdmissionCheckState, state kueue.CheckState,
) {
	key := types.NamespacedName{Namespace: wl.Namespace, Name: wl.Name}
	if state != kueue.CheckStateReady {
		delete(r.readyUnobserved, key)
		return
	}
	cp := wl.DeepCopy()
	for i := range written {
		kueueworkload.SetAdmissionCheckState(&cp.Status.AdmissionChecks, written[i], clock.RealClock{})
	}
	if r.readyUnobserved == nil {
		r.readyUnobserved = make(map[types.NamespacedName]*kueue.Workload)
	}
	r.readyUnobserved[key] = cp
}
