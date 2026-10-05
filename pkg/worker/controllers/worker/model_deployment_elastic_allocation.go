package worker

import (
	"context"
	"fmt"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"
	kueueworkload "sigs.k8s.io/kueue/pkg/workload"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/nodefeature"
)

// The elastic allocation producer answers the kernel's two capacity layers: how many seated
// elastic members Kubernetes has admitted, and how many hold a real whole-card accelerator
// right now. The layers are answered by separate reads and held by separate failures, and
// every hold is an Unknown with its reason, never a smaller count: a layer that cannot be
// read may not authorize anything, and under-reporting holds the operation, the safe
// direction. The release capture freezes what the retirable members hold, while they can
// still be read, into the record shape the retirement release predicates already consume.

// modelDeploymentElasticWorkerOf reports whether a Pod is one of the elastic role's seated
// GPU members: the role's own component, a valid ordinal, and this deployment's controlling
// owner compared in full -- UID, kind, API version and name. The CPU head carries its own
// component name and never passes; a foreign or unseated Pod is nobody's member.
func modelDeploymentElasticWorkerOf(
	pod *core.Pod, md *workercore.ModelDeployment, role *workercore.ModelDeploymentRole,
) bool {
	if modelDeploymentPodRole(pod) != role.Name {
		return false
	}
	if _, seated := modelDeploymentPodOrdinal(pod); !seated {
		return false
	}
	owner := meta.GetControllerOf(pod)

	return owner != nil && owner.UID == md.UID && owner.Kind == "ModelDeployment" &&
		owner.APIVersion == workercore.GroupVersion.String() && owner.Name == md.Name
}

// elasticWholeCardOf reads the one whole unsliced accelerator a member's main container
// records, in the shape the allocator writes it: exclusive mode, the card's full unit count,
// no slices or profiles. A POD WITH NO RECORD AT ALL IS UNALLOCATED, not unreadable: an
// absent record is the state a member not yet allocated carries, and the caller reads it as
// a member that holds nothing. A record present but saying anything other than exactly one
// whole card is refused with a reason, because trusting it would count a member that holds
// something else.
func elasticWholeCardOf(pod *core.Pod) (card modelDeploymentRetirementReleaseCard, reason string, held bool) {
	allocations, err := deviceplugin.AllocatedAcceleratorsOf(pod)
	if err != nil {
		return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q's allocation record is not readable: %v", pod.Name, err), false
	}
	if len(allocations) == 0 {
		return modelDeploymentRetirementReleaseCard{}, "", false
	}
	main, recorded := allocations[modelDeploymentMainContainerName]
	if !recorded {
		return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records its allocation on containers other than %q",
			pod.Name, modelDeploymentMainContainerName), false
	}
	cards := 0
	for i := range main.Devices.Groups {
		group := &main.Devices.Groups[i]
		for j := range group.Accelerators {
			accelerator := &group.Accelerators[j]
			cards++
			card = modelDeploymentRetirementReleaseCard{
				GroupID: group.ID, Manufacturer: group.Manufacturer,
				DeviceID: accelerator.ID, Index: accelerator.Index,
			}
			switch {
			case accelerator.Mode != workercore.DeviceAllocationModeExclusive:
				return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records a non-exclusive allocation on %q", pod.Name, accelerator.ID), false
			case accelerator.Allocated != int32(nodefeature.ResourceMaxUnits):
				return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records %d of %d units on %q, not a whole card",
					pod.Name, accelerator.Allocated, nodefeature.ResourceMaxUnits, accelerator.ID), false
			case accelerator.AllocatedSlices != 0 ||
				len(accelerator.AllocatedProfiles) != 0 || len(accelerator.RemainingProfiles) != 0:
				return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records a sliced allocation on %q", pod.Name, accelerator.ID), false
			}
		}
	}
	if cards == 0 {
		return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records an allocation with no accelerator", pod.Name), false
	}
	if cards > 1 {
		return modelDeploymentRetirementReleaseCard{}, fmt.Sprintf("member %q records %d accelerators where an elastic member holds exactly one",
			pod.Name, cards), false
	}

	return card, "", true
}

// elasticMemberWorkload finds the Workload naming this member Pod at full identity: one
// Pod-kind owner reference carrying this Pod's UID and name. A member no Workload names
// returns no workload and no reason -- the state a member not yet composed into a group
// carries. Two Workloads claiming one Pod is a claim no caller may pick from, and is a
// reason. A found Workload reports whether it is a GROUP OF ONE: each elastic member is its
// own replica, its own group and its own admission, so a Workload owning several Pods is
// another shape's admission, not this member's.
func elasticMemberWorkload(
	workloads []kueue.Workload, pod *core.Pod,
) (workload *kueue.Workload, groupOfOne bool, reason string) {
	for i := range workloads {
		candidate := &workloads[i]
		claims := 0
		for _, ref := range candidate.OwnerReferences {
			if modelDeploymentOwnerRefNamesAPod(ref) && ref.UID == pod.UID && ref.Name == pod.Name {
				claims++
			}
		}
		if claims == 0 {
			continue
		}
		if workload != nil {
			return nil, false,
				fmt.Sprintf("workloads %q and %q both claim member %q", workload.Name, candidate.Name, pod.Name)
		}
		workload = candidate
	}
	if workload == nil {
		return nil, false, ""
	}
	owners := 0
	for _, ref := range workload.OwnerReferences {
		if modelDeploymentOwnerRefNamesAPod(ref) {
			owners++
		}
	}

	return workload, owners == 1, ""
}

// elasticMemberAdmitted judges a found Workload as this member's admission: a group of one
// with an identity, a reserved quota, an admitted condition, and behind them one podset of
// one member matched by exactly one assignment at an effective count of one -- the
// assignment's nil Count reading as the spec podset's own -- under a named ClusterQueue.
// It reports not-admitted with a named reason, and never treats an absent Workload as a
// refusal -- a member not yet composed is a member that is not admitted, not an unreadable
// one.
func elasticMemberAdmitted(wl *kueue.Workload, groupOfOne bool) (bool, string) {
	switch {
	case wl == nil:
		return false, ""
	case !groupOfOne:
		return false, "it is not a group of one, so it is not this member's admission"
	case wl.UID == "":
		return false, "it carries no identity, so its admission cannot be released later"
	case !kueueworkload.HasQuotaReservation(wl) || !kueueworkload.IsAdmitted(wl):
		return false, "it holds no reserved admission"
	case wl.Status.Admission == nil || len(wl.Status.Admission.PodSetAssignments) == 0:
		return false, "it records no admission assignment"
	}
	switch {
	case wl.Status.Admission.ClusterQueue == "":
		return false, "it names no ClusterQueue, so no queue holds its quota"
	case len(wl.Spec.PodSets) != 1:
		return false, "its spec does not declare exactly one podset of one member"
	case wl.Spec.PodSets[0].Count != 1:
		return false, fmt.Sprintf("its spec declares a podset of %d members, not one",
			wl.Spec.PodSets[0].Count)
	case len(wl.Status.Admission.PodSetAssignments) != 1:
		return false, fmt.Sprintf("it records %d admission assignments for one podset",
			len(wl.Status.Admission.PodSetAssignments))
	case wl.Status.Admission.PodSetAssignments[0].Name != wl.Spec.PodSets[0].Name:
		return false, fmt.Sprintf("it admits podset %q, which the spec does not declare",
			wl.Status.Admission.PodSetAssignments[0].Name)
	case ptr.Deref(wl.Status.Admission.PodSetAssignments[0].Count, wl.Spec.PodSets[0].Count) != 1:
		return false, fmt.Sprintf("it admits %d members where the member is one",
			ptr.Deref(wl.Status.Admission.PodSetAssignments[0].Count, wl.Spec.PodSets[0].Count))
	}

	return true, ""
}

// elasticNodeLedgerAgrees verifies the claimed cards against the node's own ledger: the
// cards are in the current inventory, the node's records rebuild strictly, and the PUBLISHED
// accounting agrees with that rebuild card for card. The node's pods and the ledger are
// bookended around the reads, so an input that moved while it was being judged holds rather
// than answers. The Devices object the verification read is returned, because a capture
// freezes its identity and inventory beside the cards. It is an accounting assertion, not a
// physical measurement, the same way it is for the retirement release.
func (r *ModelDeploymentReconciler) elasticNodeLedgerAgrees(
	ctx context.Context, nodeName string,
	cards []modelDeploymentRetirementReleaseCard,
	membersRecheck func() (string, bool),
) (devices *workercore.Devices, reason string, ok bool) {
	hold := func(format string, args ...any) (*workercore.Devices, string, bool) {
		return nil, fmt.Sprintf(format, args...), false
	}
	devices = new(workercore.Devices)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, devices); err != nil {
		return hold("read the node's ledger: %v", err)
	}
	if devices.UID == "" {
		return hold("the node's ledger carries no identity")
	}
	for _, card := range cards {
		if !cardInInventory(devices, card) {
			return hold("accelerator %q of group %q is not in the node's current inventory",
				card.DeviceID, card.GroupID)
		}
	}
	onNode := new(core.PodList)
	if err := r.APIReader.List(ctx, onNode, ctrlcli.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return hold("list the node's pods: %v", err)
	}
	rebuilt, _, err := deviceplugin.BuildDesiredStatusStrict(ctrllog.FromContext(ctx), devices, onNode)
	if err != nil {
		return hold("the node's allocation cannot be read strictly: %v", err)
	}
	if reason, ok := releaseCardsAgree(cards, rebuilt, devices.Status); !ok {
		return nil, reason, false
	}
	if membersRecheck != nil {
		if reason, ok := membersRecheck(); !ok {
			return nil, reason, false
		}
	}
	after := new(core.PodList)
	if err := r.APIReader.List(ctx, after, ctrlcli.MatchingFields{"spec.nodeName": nodeName}); err != nil {
		return hold("re-reading the node's pods: %v", err)
	}
	if !samePodProjection(onNode, after) {
		return hold("the pods on node %q changed while their ledger was being read", nodeName)
	}
	again := new(workercore.Devices)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: nodeName}, again); err != nil {
		return hold("re-read the node's ledger: %v", err)
	}
	if again.UID != devices.UID {
		return hold("the node's ledger was replaced (was %q, now %q) while it was being read",
			devices.UID, again.UID)
	}
	if got := devicesInventoryProjection(again); got != devicesInventoryProjection(devices) {
		return hold("the inventory on node %q changed while it was being read", nodeName)
	}

	return devices, "", true
}

// observeModelDeploymentElasticAllocation reads the two capacity layers the elastic kernel
// decides from. THE LAYERS NEVER BORROW EACH OTHER'S ANSWER: a member whose Workload claim
// cannot be decided holds the admission layer and nothing else, a member whose node ledger
// cannot be trusted holds the allocation layer and nothing else, and only a member that
// cannot be read at all holds both. Admission is never read off a label, a scheduling
// condition or an annotation, and an allocation is never inferred from an admitted Workload.
// A member whose Pod is gone or replaced between reads is a member this pass counts no
// longer, on either layer, and that is an ordinary state a resize pass observes.
func (r *ModelDeploymentReconciler) observeModelDeploymentElasticAllocation(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) (elasticLayer, elasticLayer) {
	role := ModelDeploymentElasticRole(md)
	if role == nil {
		return unknownLayer("the deployment declares no elastic role"),
			unknownLayer("the deployment declares no elastic role")
	}
	if r.APIReader == nil {
		reason := "no API reader is configured, so nothing can be observed"
		return unknownLayer(reason), unknownLayer(reason)
	}
	if err := ctx.Err(); err != nil {
		reason := fmt.Sprintf("the observation was cancelled: %v", err)
		return unknownLayer(reason), unknownLayer(reason)
	}

	var (
		workloads        kueue.WorkloadList
		admittedUnknown  string
		allocatedUnknown string
	)
	if err := r.APIReader.List(ctx, &workloads, ctrlcli.InNamespace(md.Namespace)); err != nil {
		admittedUnknown = fmt.Sprintf("listing workloads: %v", err)
	}
	admittedCount, allocatedCount := 0, 0
	seen := map[types.UID]struct{}{}
	for i := range pods {
		pod := &pods[i]
		if !modelDeploymentElasticWorkerOf(pod, md, role) {
			continue
		}
		if _, duplicate := seen[pod.UID]; duplicate {
			return unknownLayer(fmt.Sprintf("member %q is supplied twice", pod.Name)),
				unknownLayer(fmt.Sprintf("member %q is supplied twice", pod.Name))
		}
		seen[pod.UID] = struct{}{}

		actual := new(core.Pod)
		err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, actual)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			reason := fmt.Sprintf("member %q: %v", pod.Name, err)
			if admittedUnknown == "" {
				admittedUnknown = reason
			}
			if allocatedUnknown == "" {
				allocatedUnknown = reason
			}
			continue
		}
		if actual.UID != pod.UID {
			continue
		}

		// THE ADMITTED LAYER: one reserved, admitted group-of-one Workload naming this very
		// Pod. Anything else is a member that is not admitted, and a claim that cannot be
		// decided holds the layer.
		if admittedUnknown == "" {
			wl, groupOfOne, reason := elasticMemberWorkload(workloads.Items, actual)
			if reason != "" {
				admittedUnknown = reason
			} else if ok, _ := elasticMemberAdmitted(wl, groupOfOne); ok {
				admittedCount++
			}
		}

		// THE ALLOCATION LAYER, answered without ever reading the Workload above. No record
		// at all is a member pending allocation: a known zero contribution, never a
		// borrowed answer.
		if allocatedUnknown == "" {
			card, reason, held := elasticWholeCardOf(actual)
			switch {
			case reason != "":
				allocatedUnknown = reason
			case !held:
			default:
				if _, ledgerReason, ok := r.elasticNodeLedgerAgrees(ctx, actual.Spec.NodeName,
					[]modelDeploymentRetirementReleaseCard{card}, nil); !ok {
					allocatedUnknown = ledgerReason
				} else {
					allocatedCount++
				}
			}
		}
	}

	admitted, allocated := knownLayer(admittedCount), knownLayer(allocatedCount)
	if admittedUnknown != "" {
		admitted = unknownLayer(admittedUnknown)
	}
	if allocatedUnknown != "" {
		allocated = unknownLayer(allocatedUnknown)
	}

	return admitted, allocated
}

// captureModelDeploymentElasticRelease reads what the supplied retirable members actually
// hold, while they can still be read. It captures EXACTLY the members it was supplied, and
// refuses a set it cannot stand behind: an empty one, a member without an identity or
// ordinal, the master, a member this deployment does not fully own, one whose live Pod left
// or changed under it, one holding anything other than exactly one whole accelerator, or one
// whose Workload is not a reserved, admitted group of one. The record binds to the
// deployment and generation it was captured at; the reservation fields the retirement
// operation owns are deliberately left unset, because an elastic release has none, and
// inventing one would borrow an authority nobody granted.
func (r *ModelDeploymentReconciler) captureModelDeploymentElasticRelease(
	ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
) (*modelDeploymentRetirementRelease, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("no API reader is configured, so nothing can be observed")
	}
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("the capture was cancelled: %w", err)
	}
	role := ModelDeploymentElasticRole(md)
	if role == nil {
		return nil, fmt.Errorf("the deployment declares no elastic role, so no member is retirable")
	}
	if md.UID == "" {
		return nil, fmt.Errorf("the deployment carries no identity, so the record cannot be bound to it")
	}
	if len(pods) == 0 {
		return nil, fmt.Errorf("no retirable member was supplied, so there is nothing to capture")
	}

	record := &modelDeploymentRetirementRelease{
		ModelDeploymentUID: md.UID,
		ObservedGeneration: md.Generation,
		WorkloadUIDs:       []string{},
	}
	claimKey := deviceplugin.AllocatedAcceleratorAnnoKey
	fresh := map[types.UID]*core.Pod{}
	seen := map[types.UID]struct{}{}
	for i := range pods {
		pod := &pods[i]
		if pod.UID == "" {
			return nil, fmt.Errorf("member %q carries no identity, so it cannot be captured", pod.Name)
		}
		if _, duplicate := seen[pod.UID]; duplicate {
			return nil, fmt.Errorf("member %q is supplied twice, so it cannot be captured", pod.Name)
		}
		seen[pod.UID] = struct{}{}
		if !modelDeploymentElasticWorkerOf(pod, md, role) {
			return nil, fmt.Errorf("member %q is not a fully owned seated member of role %q, so it cannot be captured",
				pod.Name, role.Name)
		}
		if ordinal, _ := modelDeploymentPodOrdinal(pod); ordinal == 0 {
			return nil, fmt.Errorf("member %q is the master, and the master is never retirable", pod.Name)
		}

		actual := new(core.Pod)
		err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: pod.Namespace, Name: pod.Name}, actual)
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("member %q is already absent, so its allocation cannot be captured", pod.Name)
		}
		if err != nil {
			return nil, fmt.Errorf("read member %q: %w", pod.Name, err)
		}
		if actual.UID != pod.UID {
			return nil, fmt.Errorf("pod %q carries uid %s, the member was supplied as %s (same-name replacement)",
				pod.Name, actual.UID, pod.UID)
		}
		if actual.Annotations[claimKey] != pod.Annotations[claimKey] {
			return nil, fmt.Errorf("member %q now records a different allocation than the supplied member did",
				pod.Name)
		}
		card, reason, held := elasticWholeCardOf(actual)
		if !held {
			if reason == "" {
				reason = "it carries no allocation record"
			}
			return nil, fmt.Errorf("member %q holds no whole accelerator to capture: %s", pod.Name, reason)
		}
		// The record's card set is read with the production reader the release comparisons
		// use, so what is frozen here is what those comparisons will read back. A member
		// holding anything outside its main container is not a whole-GPU member and is
		// refused rather than frozen half-described.
		cards, err := releaseCardsOf(actual)
		if err != nil {
			return nil, fmt.Errorf("read member %q's allocation: %w", pod.Name, err)
		}
		if len(cards) != 1 || !sameCardSet(cards, []modelDeploymentRetirementReleaseCard{card}) {
			return nil, fmt.Errorf("member %q records accelerators outside its main container", pod.Name)
		}
		if actual.Spec.NodeName == "" {
			return nil, fmt.Errorf("member %q holds accelerators but is not on a node", pod.Name)
		}

		fresh[actual.UID] = actual
		record.TargetMemberUIDs = append(record.TargetMemberUIDs, string(actual.UID))
		record.Members = append(record.Members, modelDeploymentRetirementReleaseMember{
			PodUID:           actual.UID,
			Namespace:        actual.Namespace,
			Name:             actual.Name,
			NodeName:         actual.Spec.NodeName,
			AcceleratorClaim: modelDeploymentRetirementClaimCard,
			Cards:            cards,
			Claim:            actual.Annotations[claimKey],
		})
	}
	slices.Sort(record.TargetMemberUIDs)
	slices.SortFunc(record.Members, func(a, b modelDeploymentRetirementReleaseMember) int {
		return strings.Compare(string(a.PodUID), string(b.PodUID))
	})

	workloads := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, workloads, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}
	for i := range record.Members {
		member := &record.Members[i]
		wl, groupOfOne, reason := elasticMemberWorkload(workloads.Items, fresh[member.PodUID])
		if reason != "" {
			return nil, fmt.Errorf("member %q's workload cannot be decided: %s", member.Name, reason)
		}
		if wl == nil {
			return nil, fmt.Errorf("member %q composes no workload, so its quota cannot be captured", member.Name)
		}
		if admitted, refusal := elasticMemberAdmitted(wl, groupOfOne); !admitted {
			return nil, fmt.Errorf("member %q's workload is not capturable: %s", member.Name, refusal)
		}
		record.WorkloadUIDs = append(record.WorkloadUIDs, string(wl.UID))
	}
	slices.Sort(record.WorkloadUIDs)

	// The ledgers are read STRICTLY while the members are still attributed, the members are
	// re-read against the record being built, and every node input is bookended, exactly as
	// the retirement capture runs -- the same evidence, read under the same rules.
	recheck := func() (string, bool) { return r.releaseMembersUnchanged(ctx, md, record) }
	for _, node := range recordNodesOf(record) {
		devices, reason, ok := r.elasticNodeLedgerAgrees(ctx, node.NodeName, node.Cards, recheck)
		if !ok {
			return nil, fmt.Errorf("the members' release cannot be captured: %s", reason)
		}
		record.Nodes = append(record.Nodes, modelDeploymentRetirementReleaseNode{
			NodeName: node.NodeName, DevicesName: node.NodeName, DevicesUID: devices.UID,
			Cards: node.Cards, Inventory: devicesInventoryProjection(devices),
		})
	}
	slices.SortFunc(record.Nodes, func(a, b modelDeploymentRetirementReleaseNode) int {
		return strings.Compare(a.NodeName, b.NodeName)
	})

	return record, nil
}
