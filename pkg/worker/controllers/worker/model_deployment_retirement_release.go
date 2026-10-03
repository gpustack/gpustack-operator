package worker

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
)

// The release predicates live here because they answer a different question from the state machine
// beside them. The state machine decides where the operation is; these decide whether the capacity
// it promised to give back actually came back, and they answer it by reading the server rather
// than by assuming a shape.
//
// Completion once asserted a release it had never observed. The fix is a durable record of
// what the target actually held, captured while it is still possible to observe, and predicates
// that read the server and compare against it.

// modelDeploymentRetirementReleaseAnnotation carries the one active operation's captured facts.
//
// It is ordinary object metadata on the ModelDeployment rather than a status field: the published
// retirement wire is frozen, and an annotation is something this operator already writes, owns and
// watches. It is internal evidence, not provenance and not an authentication claim. Matching fields
// prove a record belongs to an operation; they do not prove nobody edited it.
const modelDeploymentRetirementReleaseAnnotation = "modeldeployment.gpustack.ai/retirement-release"

// modelDeploymentRetirementRelease is what one operation captured about its target, together with
// the identity the record is bound to.
//
// StartedAt alone is not enough to tell two operations apart. The checked-out staging time type
// serializes RFC3339 to whole seconds, so two retries in the same second share it. The retry token
// is bound as well; it is already persisted distinctly before the directive is cleared, so a new
// operation cannot inherit an old one's evidence.
type modelDeploymentRetirementRelease struct {
	ModelDeploymentUID     types.UID `json:"modelDeploymentUID"`
	ObservedGeneration     int64     `json:"observedGeneration"`
	RoleName               string    `json:"roleName"`
	ReplicaOrdinal         int32     `json:"replicaOrdinal"`
	TargetMemberUIDs       []string  `json:"targetMemberUIDs"`
	TargetWorkloadUID      string    `json:"targetWorkloadUID"`
	StartedAt              string    `json:"startedAt"`
	LastConsumedRetryToken string    `json:"lastConsumedRetryToken"`

	Members      []modelDeploymentRetirementReleaseMember `json:"members"`
	WorkloadUIDs []string                                 `json:"workloadUIDs"`
	// The ledger the release is judged against: which node, which Devices object, and the card
	// identities captured while they were still readable.
	Nodes []modelDeploymentRetirementReleaseNode `json:"nodes"`
}

type modelDeploymentRetirementReleaseNode struct {
	NodeName    string                                 `json:"nodeName"`
	DevicesName string                                 `json:"devicesName"`
	DevicesUID  types.UID                              `json:"devicesUID"`
	Cards       []modelDeploymentRetirementReleaseCard `json:"cards"`
	// Inventory is the node's inventory and its slicing capabilities as they read at capture time.
	// A card can keep its identity and change what it can offer, and a release is only meaningful
	// against the capabilities the target was actually carved from.
	Inventory string `json:"inventory"`
}

// The applicability a captured member states for itself. Only these two values are evidence; an
// absent field is not a member that asked for nothing, it is a member nobody read.
const (
	modelDeploymentRetirementClaimNone = "none"
	modelDeploymentRetirementClaimCard = "card"
)

type modelDeploymentRetirementReleaseMember struct {
	PodUID    types.UID `json:"podUID"`
	Namespace string    `json:"namespace"`
	Name      string    `json:"name"`
	NodeName  string    `json:"nodeName"`
	// AcceleratorClaim states what the member itself asked for, read with the production
	// classifier. It is a named value rather than a flag because a missing field and an explicit
	// "none" are different facts, and only the second one is evidence that this member was read.
	AcceleratorClaim string                                 `json:"acceleratorClaim"`
	Cards            []modelDeploymentRetirementReleaseCard `json:"cards"`
	// Claim is the allocation the member was recorded as holding, kept as the producer wrote it. A
	// card's identity survives a change of mode or units, so the identities above do not by
	// themselves say the member still holds what it held.
	Claim string `json:"claim"`
}

type modelDeploymentRetirementReleaseCard struct {
	GroupID      string `json:"groupID"`
	Manufacturer string `json:"manufacturer"`
	DeviceID     string `json:"deviceID"`
	Index        uint32 `json:"index"`
}

// bindModelDeploymentRetirementRelease reports whether a record belongs to the live reservation.
//
// Every identity field is compared and nothing else. A record that matches is this operation's
// evidence; a record that does not is treated as no evidence at all, which is the safe direction,
// because a stale or foreign record holds the operation rather than clearing it.
func bindModelDeploymentRetirementRelease(
	record *modelDeploymentRetirementRelease, md *workercore.ModelDeployment,
	plan *modelDeploymentRetirementPlan,
) error {
	reservation := plan.Reservation
	switch {
	case record.ModelDeploymentUID != md.UID:
		return fmt.Errorf("it belongs to another deployment")
	case record.ObservedGeneration != reservation.ObservedGeneration:
		return fmt.Errorf("it was captured at generation %d, the reservation is at %d",
			record.ObservedGeneration, reservation.ObservedGeneration)
	case record.RoleName != reservation.RoleName || record.ReplicaOrdinal != reservation.ReplicaOrdinal:
		return fmt.Errorf("it names %s ordinal %d, the reservation names %s ordinal %d",
			record.RoleName, record.ReplicaOrdinal, reservation.RoleName, reservation.ReplicaOrdinal)
	}

	// The target set is compared sorted: the wire promises a set, not an order, and a record
	// written in a different order is the same operation.
	want := slices.Clone(reservation.TargetMemberUIDs)
	got := slices.Clone(record.TargetMemberUIDs)
	slices.Sort(want)
	slices.Sort(got)
	if !slices.Equal(want, got) {
		return fmt.Errorf("it was captured for %v, the reservation targets %v", got, want)
	}

	if record.TargetWorkloadUID != reservation.TargetWorkloadUID {
		return fmt.Errorf("it froze workload %q, the reservation freezes %q",
			record.TargetWorkloadUID, reservation.TargetWorkloadUID)
	}
	// The start time is compared as the stored representation, not as an instant rebuilt from
	// nanoseconds. Two operations a second apart share a serialized time on purpose, which is
	// exactly why the token is bound with it.
	if record.StartedAt != reservation.StartedAt.UTC().Format(time.RFC3339) {
		return fmt.Errorf("it was captured for an operation started at %s, this one started at %s",
			record.StartedAt, reservation.StartedAt.UTC().Format(time.RFC3339))
	}
	if record.LastConsumedRetryToken != reservation.LastConsumedRetryToken {
		return fmt.Errorf("it belongs to a different retry")
	}

	return nil
}

// captureModelDeploymentRetirementRelease reads what the target actually holds, while it can still
// be read.
//
// It runs in Draining after the withdrawal and the drain are verified and before the operation
// moves to Deleting, because that is the last moment the members, their allocation records and
// their Workloads are all still present. A member already deleted cannot be reconstructed from the
// pods that remain, and guessing at it is the failure this record exists to prevent.
func (r *ModelDeploymentReconciler) captureModelDeploymentRetirementRelease(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) (*modelDeploymentRetirementRelease, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("no API reader is configured, so nothing can be observed")
	}
	if plan.held.Len() == 0 {
		return nil, fmt.Errorf("the target's members are not all present, so there is nothing to capture")
	}

	// A Pod cannot be fetched by UID, so the namespace is listed and matched. The list is by
	// namespace and unfiltered on purpose: filtering out terminating or terminal objects here would
	// hide a member that still exists, and this is the last moment it can be read at all.
	pods := new(core.PodList)
	if err := r.APIReader.List(ctx, pods, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return nil, fmt.Errorf("list the target's pods: %w", err)
	}
	byUID := make(map[types.UID]*core.Pod, len(pods.Items))
	for i := range pods.Items {
		byUID[pods.Items[i].UID] = &pods.Items[i]
	}

	reservation := plan.Reservation
	record := &modelDeploymentRetirementRelease{
		ModelDeploymentUID:     md.UID,
		ObservedGeneration:     reservation.ObservedGeneration,
		RoleName:               reservation.RoleName,
		ReplicaOrdinal:         reservation.ReplicaOrdinal,
		TargetMemberUIDs:       slices.Clone(reservation.TargetMemberUIDs),
		TargetWorkloadUID:      reservation.TargetWorkloadUID,
		StartedAt:              reservation.StartedAt.UTC().Format(time.RFC3339),
		LastConsumedRetryToken: reservation.LastConsumedRetryToken,
		WorkloadUIDs:           []string{},
	}

	// The frozen target set decides what is captured, not the live held set. The held set is what
	// this pass happens to see, and a member it skipped would be written down as though it had
	// never held anything. An empty or repeated UID is refused rather than folded away, because a
	// target set that cannot be read exactly cannot be released exactly.
	memberUIDs := make([]types.UID, 0, len(reservation.TargetMemberUIDs))
	seen := make(map[types.UID]struct{}, len(reservation.TargetMemberUIDs))
	for _, name := range reservation.TargetMemberUIDs {
		uid := types.UID(name)
		if uid == "" {
			return nil, fmt.Errorf("the frozen target set names an empty member UID, so it cannot be captured")
		}
		if _, duplicate := seen[uid]; duplicate {
			return nil, fmt.Errorf("the frozen target set names member %q twice, so it cannot be captured", name)
		}
		seen[uid] = struct{}{}
		memberUIDs = append(memberUIDs, uid)
	}
	slices.Sort(memberUIDs)
	for _, memberUID := range memberUIDs {
		uid := memberUID
		pod, present := byUID[uid]
		if !present {
			// The member is already gone, so its allocation cannot be read and must not be
			// invented from whatever remains on the node.
			return nil, fmt.Errorf("target member %q is already absent, so its allocation cannot be captured", memberUID)
		}
		requests := podRequestsAccelerator(pod)
		cards, err := releaseCardsOf(pod)
		if err != nil {
			return nil, fmt.Errorf("read member %q's allocation: %w", memberUID, err)
		}
		// A member that asks for a card and carries no readable record has not been read; it is not
		// a member that asked for none. Reading an absent record as CPU is how a GPU requester
		// would be recorded as holding nothing.
		if requests && len(cards) == 0 {
			return nil, fmt.Errorf(
				"target member %q requests an accelerator but carries no readable allocation record", memberUID)
		}
		if pod.Spec.NodeName == "" && len(cards) > 0 {
			return nil, fmt.Errorf("target member %q holds accelerators but is not on a node", memberUID)
		}
		claim := modelDeploymentRetirementClaimNone
		if requests {
			claim = modelDeploymentRetirementClaimCard
		}
		record.Members = append(record.Members, modelDeploymentRetirementReleaseMember{
			PodUID:           pod.UID,
			Claim:            pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey],
			Namespace:        pod.Namespace,
			Name:             pod.Name,
			NodeName:         pod.Spec.NodeName,
			AcceleratorClaim: claim,
			Cards:            cards,
		})
	}
	slices.SortFunc(record.Members, func(a, b modelDeploymentRetirementReleaseMember) int {
		return strings.Compare(string(a.PodUID), string(b.PodUID))
	})

	workloads, err := r.releaseWorkloadUIDs(ctx, md, plan)
	if err != nil {
		return nil, err
	}
	record.WorkloadUIDs = workloads

	// The ledger is captured while the cards are still attributed. Devices is cluster-scoped and
	// named after its node, so this is one Get per node. Its UID and the card identities are kept
	// because a later ledger for the same node name is a different ledger, and comparing against it
	// would prove something about an object this operation never held.
	for _, node := range recordNodesOf(record) {
		devices := new(workercore.Devices)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: node.NodeName}, devices); err != nil {
			return nil, fmt.Errorf("read the node's ledger: %w", err)
		}
		if devices.UID == "" {
			return nil, fmt.Errorf("the node's ledger carries no identity")
		}
		for _, card := range node.Cards {
			if !cardInInventory(devices, card) {
				return nil, fmt.Errorf("accelerator %q of group %q is not in the node's current inventory",
					card.DeviceID, card.GroupID)
			}
		}
		// The target's own allocation is read STRICTLY before the record is written. A malformed
		// claim, a claim for a card that is not in this inventory, or a mode conflict between two
		// holders is discovered here, while the target still exists to be asked about, rather than
		// after the delete that this evidence authorizes.
		onNode := new(core.PodList)
		if err := r.APIReader.List(ctx, onNode,
			ctrlcli.MatchingFields{"spec.nodeName": node.NodeName}); err != nil {
			return nil, fmt.Errorf("list the node's pods: %w", err)
		}
		if _, _, err := deviceplugin.BuildDesiredStatusStrict(
			ctrllog.FromContext(ctx), devices, onNode); err != nil {
			return nil, fmt.Errorf("the target's allocation cannot be read strictly: %w", err)
		}

		// The members are read again after the node above. A claim that changed between the two
		// reads is individually valid at each end and incoherent together, and persisting the
		// earlier one would describe a target as holding something it no longer holds.
		if reason, ok := r.releaseMembersUnchanged(ctx, md, record); !ok {
			return nil, fmt.Errorf("the target changed while its release was being captured: %s", reason)
		}

		// The ledger is read again after the node input too, and it has to be the same ledger with
		// the same capabilities. The reads either side are separate, so an inventory that moved
		// between them is a changed input rather than the one this record is about.
		projection := devicesInventoryProjection(devices)
		after := new(workercore.Devices)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: node.NodeName}, after); err != nil {
			return nil, fmt.Errorf("re-read the node's ledger: %w", err)
		}
		if after.UID != devices.UID {
			return nil, fmt.Errorf("the node's ledger was replaced (was %q, now %q) while its release "+
				"was being captured", devices.UID, after.UID)
		}
		if got := devicesInventoryProjection(after); got != projection {
			return nil, fmt.Errorf("the inventory on node %q changed while its release was being captured",
				node.NodeName)
		}

		record.Nodes = append(record.Nodes, modelDeploymentRetirementReleaseNode{
			NodeName:    node.NodeName,
			DevicesName: node.NodeName,
			DevicesUID:  devices.UID,
			Cards:       node.Cards,
			Inventory:   projection,
		})
	}
	slices.SortFunc(record.Nodes, func(a, b modelDeploymentRetirementReleaseNode) int {
		return strings.Compare(a.NodeName, b.NodeName)
	})

	return record, nil
}

// podRequestsAccelerator reports whether any container of the pod asks for an accelerator, read
// with the production classifier the allocator itself uses. A CPU-only pod is not an error here; it
// is the case whose applicability has to be recorded explicitly.
func podRequestsAccelerator(pod *core.Pod) bool {
	for i := range pod.Spec.Containers {
		if requestsCard(&pod.Spec.Containers[i]) {
			return true
		}
	}

	return false
}

// validateReleaseFacts reports whether a bound record actually describes the operation it claims.
//
// Identity alone does not make a record usable. A record with no member facts, or with no node facts
// for members that held cards, says nothing about what was released, and completing on it would
// clear the operation on evidence that was never captured.
func validateReleaseFacts(record *modelDeploymentRetirementRelease) error {
	if len(record.Members) == 0 {
		return fmt.Errorf("it records no member facts, so it cannot say what the target held")
	}

	want := make(map[string]struct{}, len(record.TargetMemberUIDs))
	for _, name := range record.TargetMemberUIDs {
		if name == "" {
			return fmt.Errorf("its target set names an empty member UID")
		}
		if _, repeated := want[name]; repeated {
			return fmt.Errorf("its target set names member %q twice", name)
		}
		want[name] = struct{}{}
	}
	got := make(map[string]struct{}, len(record.Members))
	for i := range record.Members {
		member := &record.Members[i]
		if member.PodUID == "" {
			return fmt.Errorf("member %q records no identity", member.Name)
		}
		if _, duplicate := got[string(member.PodUID)]; duplicate {
			return fmt.Errorf("member %q is recorded twice", member.Name)
		}
		got[string(member.PodUID)] = struct{}{}
		// The claim is a named fact, not a flag: an absent one means nobody recorded what this
		// member asked for, which is not the same as a member that asked for nothing.
		switch member.AcceleratorClaim {
		case modelDeploymentRetirementClaimNone:
			if len(member.Cards) != 0 {
				return fmt.Errorf("member %q claims no accelerator but records %d cards",
					member.Name, len(member.Cards))
			}
		case modelDeploymentRetirementClaimCard:
			if len(member.Cards) == 0 {
				return fmt.Errorf("member %q claims an accelerator but records no card", member.Name)
			}
			if member.Claim == "" {
				return fmt.Errorf("member %q claims an accelerator but records no allocation", member.Name)
			}
		default:
			return fmt.Errorf("member %q states accelerator applicability %q, which is not a recorded answer",
				member.Name, member.AcceleratorClaim)
		}
		cardIdentities := make(map[string]struct{}, len(member.Cards))
		for _, card := range member.Cards {
			if _, repeated := cardIdentities[releaseCardKey(card)]; repeated {
				return fmt.Errorf("member %q records card %q twice", member.Name, card.DeviceID)
			}
			cardIdentities[releaseCardKey(card)] = struct{}{}
		}
	}
	if len(got) != len(want) {
		return fmt.Errorf("it holds facts for %d of the %d target members", len(got), len(want))
	}
	for name := range want {
		if _, present := got[name]; !present {
			return fmt.Errorf("it holds no facts for target member %q", name)
		}
	}

	// A member that held cards has to have a node entry, and that entry has to carry the same cards.
	// A card recorded on a member and absent from the node facts is a release nobody can read.
	byNode := make(map[string]map[string]struct{}, len(record.Nodes))
	for i := range record.Nodes {
		node := &record.Nodes[i]
		if node.NodeName == "" || node.DevicesUID == "" {
			return fmt.Errorf("a recorded node carries no name or identity")
		}
		if _, duplicate := byNode[node.NodeName]; duplicate {
			return fmt.Errorf("node %q is recorded twice", node.NodeName)
		}
		byNode[node.NodeName] = map[string]struct{}{}
		for _, card := range node.Cards {
			byNode[node.NodeName][releaseCardKey(card)] = struct{}{}
		}
	}
	for i := range record.Members {
		member := &record.Members[i]
		if len(member.Cards) == 0 {
			continue
		}
		if member.NodeName == "" {
			return fmt.Errorf("member %q holds cards but records no node", member.Name)
		}
		node, present := byNode[member.NodeName]
		if !present {
			return fmt.Errorf("member %q holds cards on node %q, which has no ledger facts",
				member.Name, member.NodeName)
		}
		for _, card := range member.Cards {
			if _, present := node[releaseCardKey(card)]; !present {
				return fmt.Errorf("card %q held by member %q is missing from node %q's facts",
					card.DeviceID, member.Name, member.NodeName)
			}
		}
	}

	// Every recorded Workload is a real identity, and the frozen one is among them. An empty entry
	// cannot be shown absent later, so it is not evidence to record.
	workloads := make(map[string]struct{}, len(record.WorkloadUIDs))
	for _, uid := range record.WorkloadUIDs {
		if uid == "" {
			return fmt.Errorf("it records a workload with no identity")
		}
		if _, repeated := workloads[uid]; repeated {
			return fmt.Errorf("it records workload %q twice", uid)
		}
		workloads[uid] = struct{}{}
	}
	if record.TargetWorkloadUID != "" {
		if _, present := workloads[record.TargetWorkloadUID]; !present {
			return fmt.Errorf("it does not record the frozen workload %q it was captured for",
				record.TargetWorkloadUID)
		}
	}

	for i := range record.Nodes {
		if record.Nodes[i].Inventory == "" {
			return fmt.Errorf("node %q records no inventory to judge its release against",
				record.Nodes[i].NodeName)
		}
	}

	return nil
}

// releaseCardKey is the identity of a card as the record states it.
func releaseCardKey(card modelDeploymentRetirementReleaseCard) string {
	return strings.Join([]string{
		card.GroupID, card.Manufacturer, card.DeviceID,
		strconv.FormatUint(uint64(card.Index), 10),
	}, "/")
}

// releaseMembersUnchanged re-reads the frozen members and reports whether they still say what the
// record says they said. It is the capture's own bounded input bookend: the reads either side of it
// are separate, and a member that moved between them is a changed input rather than a result.
func (r *ModelDeploymentReconciler) releaseMembersUnchanged(
	ctx context.Context, md *workercore.ModelDeployment,
	record *modelDeploymentRetirementRelease,
) (string, bool) {
	pods := new(core.PodList)
	if err := r.APIReader.List(ctx, pods, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return fmt.Sprintf("re-reading the target's pods: %v", err), false
	}
	byUID := make(map[types.UID]*core.Pod, len(pods.Items))
	for i := range pods.Items {
		byUID[pods.Items[i].UID] = &pods.Items[i]
	}

	for i := range record.Members {
		captured := &record.Members[i]
		pod, present := byUID[captured.PodUID]
		if !present {
			return fmt.Sprintf("member %q left while its release was being captured", captured.Name), false
		}
		cards, err := releaseCardsOf(pod)
		if err != nil {
			return fmt.Sprintf("re-read member %q's allocation: %v", captured.Name, err), false
		}
		if pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] != captured.Claim {
			return fmt.Sprintf("member %q now records a different allocation", captured.Name), false
		}
		if podRequestsAccelerator(pod) != (captured.AcceleratorClaim == modelDeploymentRetirementClaimCard) {
			return fmt.Sprintf("member %q now requests a different set of accelerators", captured.Name), false
		}
		if !sameCardSet(captured.Cards, cards) {
			return fmt.Sprintf("member %q now records different accelerators", captured.Name), false
		}
	}

	return "", true
}

// sameCardSet compares two card sets by identity, order-independently.
func sameCardSet(a, b []modelDeploymentRetirementReleaseCard) bool {
	if len(a) != len(b) {
		return false
	}
	keys := make(map[string]struct{}, len(a))
	for _, card := range a {
		keys[releaseCardKey(card)] = struct{}{}
	}
	for _, card := range b {
		if _, present := keys[releaseCardKey(card)]; !present {
			return false
		}
	}

	return true
}

// recordNodesOf collapses the members onto the nodes whose ledgers have to be compared.
func recordNodesOf(record *modelDeploymentRetirementRelease) []modelDeploymentRetirementReleaseNode {
	byNode := map[string][]modelDeploymentRetirementReleaseCard{}
	for i := range record.Members {
		member := &record.Members[i]
		if len(member.Cards) == 0 || member.NodeName == "" {
			continue
		}
		byNode[member.NodeName] = append(byNode[member.NodeName], member.Cards...)
	}
	nodes := make([]modelDeploymentRetirementReleaseNode, 0, len(byNode))
	for name, cards := range byNode {
		slices.SortFunc(cards, func(a, b modelDeploymentRetirementReleaseCard) int {
			if a.GroupID != b.GroupID {
				return strings.Compare(a.GroupID, b.GroupID)
			}

			return strings.Compare(a.DeviceID, b.DeviceID)
		})
		nodes = append(nodes, modelDeploymentRetirementReleaseNode{NodeName: name, Cards: cards})
	}
	slices.SortFunc(nodes, func(a, b modelDeploymentRetirementReleaseNode) int {
		return strings.Compare(a.NodeName, b.NodeName)
	})

	return nodes
}

// cardInInventory reports whether a captured card is still in the node's current inventory, by the
// identity the capture used.
func cardInInventory(devices *workercore.Devices, card modelDeploymentRetirementReleaseCard) bool {
	// The identity of a card is its group, its manufacturer and its position in that group. An
	// ID and an index alone are not a card: two vendors both number from zero, so a card whose
	// manufacturer changed is a different card and the captured claim no longer describes it.
	for i := range devices.Spec.Groups {
		group := &devices.Spec.Groups[i]
		if group.ID != card.GroupID || group.Manufacturer != card.Manufacturer {
			continue
		}
		for j := range group.Accelerators {
			accelerator := &group.Accelerators[j]
			if accelerator.ID == card.DeviceID && accelerator.Index == card.Index {
				return true
			}
		}
	}

	return false
}

// releaseWorkloadUIDs is the actual Workload UID set associated with the target, plus the frozen
// status UID even when the list did not contain it.
//
// Including the frozen UID unconditionally is deliberate: a Workload composed after this capture is
// still one the release predicate has to account for, and a proof that only knows about what
// existed at capture time would miss it.
func (r *ModelDeploymentReconciler) releaseWorkloadUIDs(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) ([]string, error) {
	workloads := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, workloads, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return nil, fmt.Errorf("list workloads: %w", err)
	}

	uids := make([]string, 0, len(workloads.Items)+1)
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		// AN OBJECT WITH NO UID IS NOT AN IDENTITY. The capture records the Workload set by UID
		// because that is what distinguishes a later composition from the one being observed;
		// recording an empty UID would put "" in the set, and a later object that also has no UID
		// would then match it, which is a proof by nothing. A Workload the API server has not given
		// an identity is not something this predicate can speak about.
		if workload.UID == "" {
			continue
		}
		for _, owner := range workload.OwnerReferences {
			if plan.holds(owner.UID) {
				uids = append(uids, string(workload.UID))

				break
			}
		}
	}
	if frozen := plan.Reservation.TargetWorkloadUID; frozen != "" {
		if !slices.Contains(uids, frozen) {
			uids = append(uids, frozen)
		}
	}
	slices.Sort(uids)

	return uids, nil
}

// releaseCardsOf reads the accelerators one pod actually holds from its own allocation record.
//
// A pod with no record is not read as holding nothing; the caller decides what that means, and for
// an accelerator-requesting pod the absence is a reason to hold rather than a clean bill of health.
func releaseCardsOf(pod *core.Pod) ([]modelDeploymentRetirementReleaseCard, error) {
	allocations, err := deviceplugin.AllocatedAcceleratorsOf(pod)
	if err != nil {
		return nil, err
	}
	if allocations == nil {
		return nil, nil
	}

	cards := make([]modelDeploymentRetirementReleaseCard, 0)
	for _, container := range allocations {
		for i := range container.Devices.Groups {
			group := &container.Devices.Groups[i]
			for j := range group.Accelerators {
				card := &group.Accelerators[j]
				cards = append(cards, modelDeploymentRetirementReleaseCard{
					GroupID:      group.ID,
					Manufacturer: group.Manufacturer,
					DeviceID:     card.ID,
					Index:        card.Index,
				})
			}
		}
	}
	// Sorted, so two captures of one pod compare equal.
	slices.SortFunc(cards, func(a, b modelDeploymentRetirementReleaseCard) int {
		if a.GroupID != b.GroupID {
			return strings.Compare(a.GroupID, b.GroupID)
		}

		return strings.Compare(a.DeviceID, b.DeviceID)
	})

	return cards, nil
}

// loadModelDeploymentRetirementRelease reads the durable record off the object and binds it.
//
// A missing annotation, unreadable JSON and a record belonging to another operation are all the
// same answer to the caller: there is no usable evidence, and the operation holds.
func (r *ModelDeploymentReconciler) loadModelDeploymentRetirementRelease(
	md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) (*modelDeploymentRetirementRelease, error) {
	raw, present := md.Annotations[modelDeploymentRetirementReleaseAnnotation]
	if !present {
		return nil, fmt.Errorf("the operation carries no captured release record")
	}
	record := new(modelDeploymentRetirementRelease)
	if err := json.Unmarshal([]byte(raw), record); err != nil {
		return nil, fmt.Errorf("the captured release record is not readable: %w", err)
	}
	if err := bindModelDeploymentRetirementRelease(record, md, plan); err != nil {
		return nil, fmt.Errorf("the captured release record does not belong to this operation: %w", err)
	}
	if err := validateReleaseFacts(record); err != nil {
		return nil, fmt.Errorf("the captured release record cannot say what the target held: %w", err)
	}

	return record, nil
}

// persistModelDeploymentRetirementRelease writes the record onto the object.
//
// It is a resourceVersion-aware patch and it runs BEFORE the Deleting status is written. A
// controller that dies between the two leaves a record with no reservation, which is inert, and
// never a reservation with no record, which would be one able to authorize a delete it cannot
// justify. A conflict is returned so the caller holds and re-reads rather than overwriting.
func (r *ModelDeploymentReconciler) persistModelDeploymentRetirementRelease(
	ctx context.Context, md *workercore.ModelDeployment, record *modelDeploymentRetirementRelease,
) error {
	raw, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encode the release record: %w", err)
	}
	patched := md.DeepCopy()
	if patched.Annotations == nil {
		patched.Annotations = map[string]string{}
	}
	patched.Annotations[modelDeploymentRetirementReleaseAnnotation] = string(raw)
	// A MERGE PATCH CARRIES NO RESOURCE VERSION of its own, so without the optimistic lock this
	// write is last-writer-wins and two passes can each believe they wrote the record. The option
	// makes the patch carry the version it was computed from, so a writer that raced this one is
	// told so instead of overwriting it.
	if err := r.Client.Patch(ctx, patched, ctrlcli.MergeFromWithOptions(
		md, ctrlcli.MergeFromWithOptimisticLock{})); err != nil {
		if apierrors.IsConflict(err) {
			return fmt.Errorf("the release record was written against a stale object: %w", err)
		}

		return fmt.Errorf("write the release record: %w", err)
	}
	// The caller's object carries the record forward, and the RESOURCE VERSION WITH IT, because the
	// status write that follows in the same pass is checked against it and a reservation that never
	// persists is an operation that re-runs this step forever. This is bookkeeping, not a guard: the
	// guard is the optimistic lock on the write above.
	if md.Annotations == nil {
		md.Annotations = map[string]string{}
	}
	md.Annotations[modelDeploymentRetirementReleaseAnnotation] = string(raw)
	md.ResourceVersion = patched.ResourceVersion

	return nil
}

// observeModelDeploymentRetirementReleased reads the server and reports whether the release has
// actually been observed.
//
// The caller's context is used throughout and never replaced: this runs after the delete is
// committed, and a background context here would outlive the pass that asked the question.
func (r *ModelDeploymentReconciler) observeModelDeploymentRetirementReleased(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) (string, bool) {
	if r.APIReader == nil {
		return "no API reader is configured, so the release cannot be observed", false
	}
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("the observation was cancelled: %v", err), false
	}

	record, err := r.loadModelDeploymentRetirementRelease(md, plan)
	if err != nil {
		return fmt.Sprintf("the release has not been observed: %v", err), false
	}

	// The operation is read fresh BEFORE the collection as well as after it. A record read from a
	// caller's in-memory copy proves nothing about what the server is doing, and a reservation that
	// moves during the reads below is a reservation these reads no longer describe.
	if reason, ok := r.observeReleaseOperationUnchanged(ctx, md, plan, record); !ok {
		return reason, false
	}

	if reason, ok := r.observeReleaseTargetsAbsent(ctx, record); !ok {
		return reason, false
	}
	if reason, ok := r.observeReleaseWorkloadsGone(ctx, md, record); !ok {
		return reason, false
	}
	if reason, ok := r.observeReleaseAcceleratorsReturned(ctx, record); !ok {
		return reason, false
	}
	// The same operation bookend runs again after every read above, so a reservation or a record
	// that moved while the collection was in flight holds instead of being cleared on evidence that
	// no longer describes it.
	if reason, ok := r.observeReleaseOperationUnchanged(ctx, md, plan, record); !ok {
		return reason, false
	}

	// The reservation is re-read after the reads above, so a reservation that moved while they were
	// in flight holds instead of clearing on evidence it no longer matches. There is no claim here
	// that these reads are one atomic snapshot; they are separate observations, bounded.
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("the observation was cancelled: %v", err), false
	}

	return "", true
}

// observeReleaseOperationUnchanged reads the operation from the server and reports whether it is
// still the one the record was captured for.
//
// It is called before the collection and again after it. The identity it checks is the whole
// reservation binding: the deployment, the retry token, the start time, the frozen target set and
// the frozen workload, plus the evidence annotation itself. Any of them moving means the reads
// around it describe a different operation.
func (r *ModelDeploymentReconciler) observeReleaseOperationUnchanged(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
	record *modelDeploymentRetirementRelease,
) (string, bool) {
	current := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), current); err != nil {
		return fmt.Sprintf("reading the operation: %v", err), false
	}
	if current.UID != md.UID {
		return "the deployment this release was captured for is no longer the one on the server", false
	}
	// The whole reservation binding is compared against the fresh object. Checking the retry token
	// alone leaves a generation, a role, an ordinal or a start time free to move.
	reservation := current.Status.Retirement
	switch {
	case reservation == nil:
		return "the operation's reservation is no longer on the server", false
	case reservation.ObservedGeneration != record.ObservedGeneration:
		return fmt.Sprintf("the operation is at generation %d, the record was captured at %d",
			reservation.ObservedGeneration, record.ObservedGeneration), false
	case reservation.RoleName != record.RoleName:
		return fmt.Sprintf("the operation now names role %q, the record named %q",
			reservation.RoleName, record.RoleName), false
	case reservation.ReplicaOrdinal != record.ReplicaOrdinal:
		return fmt.Sprintf("the operation now names ordinal %d, the record named %d",
			reservation.ReplicaOrdinal, record.ReplicaOrdinal), false
	case reservation.TargetWorkloadUID != record.TargetWorkloadUID:
		return fmt.Sprintf("the operation now freezes workload %q, the record froze %q",
			reservation.TargetWorkloadUID, record.TargetWorkloadUID), false
	case reservation.LastConsumedRetryToken != record.LastConsumedRetryToken:
		return fmt.Sprintf("the operation moved to retry %q while this release was being read",
			reservation.LastConsumedRetryToken), false
	}

	// The start time is compared as it is stored, because that is the form the record froze.
	if got := reservation.StartedAt.UTC().Format(time.RFC3339); got != record.StartedAt {
		return fmt.Sprintf("the operation now started at %s, the record was captured for one started at %s",
			got, record.StartedAt), false
	}

	// The target set is a set, so it is compared unordered, and a repeated UID in it is invalid
	// rather than something to fold away.
	frozen := sets.New[string]()
	for _, name := range reservation.TargetMemberUIDs {
		if frozen.Has(name) {
			return fmt.Sprintf("the operation's target set names %q twice", name), false
		}
		frozen.Insert(name)
	}
	if !frozen.Equal(sets.New(record.TargetMemberUIDs...)) {
		return fmt.Sprintf("the operation now targets %v, the record captured %v",
			reservation.TargetMemberUIDs, record.TargetMemberUIDs), false
	}

	// The phase is checked against the one this collection is authorized under, not against a list
	// of phases that happen to be live. A phase change is legitimate between passes of one
	// operation and is not legitimate inside a collection that has already begun: a reservation
	// that moved from Settling back to Deleting mid-read is authority this pass was not given.
	if reservation.State != plan.Reservation.State {
		return fmt.Sprintf("the operation is now %q, this collection is authorized under %q",
			reservation.State, plan.Reservation.State), false
	}

	// The evidence itself is read back from the server, not from the caller's copy.
	onServer := new(modelDeploymentRetirementRelease)
	raw, present := current.Annotations[modelDeploymentRetirementReleaseAnnotation]
	if !present {
		return "the operation carries no release record on the server", false
	}
	if err := json.Unmarshal([]byte(raw), onServer); err != nil {
		return fmt.Sprintf("the release record on the server is not readable: %v", err), false
	}
	if onServer.LastConsumedRetryToken != record.LastConsumedRetryToken ||
		onServer.StartedAt != record.StartedAt {
		return "the release record on the server belongs to a different operation", false
	}
	// The evidence on the server is validated on its own terms and compared in full, rather than
	// trusted because two of its fields happen to agree. A record edited on the server keeps its
	// retry token, and that is exactly the edit this has to notice.
	if err := validateReleaseFacts(onServer); err != nil {
		return fmt.Sprintf("the release record on the server cannot say what the target held: %v", err), false
	}
	if !sameReleaseFacts(onServer, record) {
		return "the release record on the server is not the one this operation loaded", false
	}

	return "", true
}

// observeReleaseTargetsAbsent is the Deleting predicate: every captured member UID must be
// genuinely absent, including an object that is deleting or terminal.
func (r *ModelDeploymentReconciler) observeReleaseTargetsAbsent(
	ctx context.Context,
	record *modelDeploymentRetirementRelease,
) (string, bool) {
	for i := range record.Members {
		member := &record.Members[i]
		pod := new(core.Pod)
		err := r.APIReader.Get(ctx,
			ctrlcli.ObjectKey{Namespace: member.Namespace, Name: member.Name}, pod)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return fmt.Sprintf("reading member %q: %v", member.Name, err), false
		}
		// A same-name replacement carrying a different UID is not the target and is not removed
		// here. A target still present in any state is not absent, and the plan's held set skips
		// deleting and terminal objects, so this is where they are caught.
		if pod.UID == member.PodUID {
			return fmt.Sprintf("member %q is still present (phase %s, deleting %t)",
				member.Name, pod.Status.Phase, pod.DeletionTimestamp != nil), false
		}
	}

	return "", true
}

// observeReleaseWorkloadsGone is the quota predicate: every captured Workload UID is gone, and no
// current Workload still claims a target member.
//
// There is no Count arithmetic here. The admitted PodSet exposes a count taken at admission time
// that does not change on reclaim, so decrementing that number would be an invented proof. A
// shared Workload that still owns survivors stays and holds the operation, because the target's
// quota was never released independently of it.
func (r *ModelDeploymentReconciler) observeReleaseWorkloadsGone(
	ctx context.Context, md *workercore.ModelDeployment,
	record *modelDeploymentRetirementRelease,
) (string, bool) {
	workloads := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, workloads, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return fmt.Sprintf("listing workloads: %v", err), false
	}

	// Ownership is decided against the FROZEN target set, not against what this pass happens to
	// hold. The held set is a mutable subset: a member it skipped, or one whose pod is already gone,
	// would otherwise be a target member no Workload is ever asked about.
	targets := make(map[types.UID]struct{}, len(record.TargetMemberUIDs))
	for _, name := range record.TargetMemberUIDs {
		targets[types.UID(name)] = struct{}{}
	}

	live := make(map[string]struct{}, len(workloads.Items))
	for i := range workloads.Items {
		workload := &workloads.Items[i]
		// A relevant Workload with no identity cannot be shown absent, so it holds rather than
		// being skipped. A same-name object with a different UID is a different object and does not
		// answer for this one.
		if workload.UID == "" {
			for _, owner := range workload.OwnerReferences {
				if _, owned := targets[owner.UID]; owned {
					return fmt.Sprintf(
						"workload %q owns the target's member %q but carries no identity, so its "+
							"release cannot be shown", workload.Name, owner.Name), false
				}
			}

			continue
		}
		live[string(workload.UID)] = struct{}{}
		for _, owner := range workload.OwnerReferences {
			if _, owned := targets[owner.UID]; owned {
				return fmt.Sprintf(
					"workload %q still owns the target's member %q, so the target's quota is not released",
					workload.Name, owner.Name), false
			}
		}
	}
	for _, uid := range record.WorkloadUIDs {
		if uid == "" {
			continue
		}
		if _, present := live[uid]; present {
			return fmt.Sprintf("captured workload %q is still present", uid), false
		}
	}

	return "", true
}

// observeModelDeploymentRetirementTargetsGone is the Deleting half of the release: every captured
// member UID must be genuinely absent from the server.
//
// It is separate from the Settling predicate on purpose. This step answers only "is the target
// gone", which is the question a delete can be waited on. The accelerator and quota release is
// answered later, against the ledger and the Workloads, and answering it here would either block
// the delete on a ledger that has not been rebuilt yet or clear on one that has.
func (r *ModelDeploymentReconciler) observeModelDeploymentRetirementTargetsGone(
	ctx context.Context, md *workercore.ModelDeployment, plan *modelDeploymentRetirementPlan,
) (string, bool) {
	if r.APIReader == nil {
		return "no API reader is configured, so the target's absence cannot be observed", false
	}
	if err := ctx.Err(); err != nil {
		return fmt.Sprintf("the observation was cancelled: %v", err), false
	}

	record, err := r.loadModelDeploymentRetirementRelease(md, plan)
	if err != nil {
		return fmt.Sprintf("the target's absence cannot be observed: %v", err), false
	}

	// A Pod cannot be fetched by UID, so the namespace is listed and matched. The list is
	// unfiltered on purpose: a deleting or terminal object is still an object, and the held set
	// deliberately does not count it.
	pods := new(core.PodList)
	if err := r.APIReader.List(ctx, pods, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return fmt.Sprintf("listing the target's pods: %v", err), false
	}
	live := make(map[types.UID]*core.Pod, len(pods.Items))
	for i := range pods.Items {
		live[pods.Items[i].UID] = &pods.Items[i]
	}

	for i := range record.Members {
		member := &record.Members[i]
		pod, present := live[member.PodUID]
		if !present {
			continue
		}
		// A same-name replacement carrying a different UID is not the target and is never removed
		// here. A target still present in any state is not absent.
		return fmt.Sprintf("member %q is still present (phase %s, deleting %t)",
			member.Name, pod.Status.Phase, pod.DeletionTimestamp != nil), false
	}

	return "", true
}

// observeReleaseAcceleratorsReturned is the accelerator half of the release.
//
// It answers one question per captured card: does the node's own ledger now agree, card for card,
// with what the operator's accounting says that card should hold? The ledger is rebuilt STRICTLY
// from the allocation records of every pod on the node -- across every namespace, including
// deleting and terminal ones, because a container in its grace period is still carved -- and an
// input the strict rebuild refuses is a refusal here rather than a card reading free.
//
// IT IS AN ACCOUNTING ASSERTION, NOT A PHYSICAL MEASUREMENT. Nothing here observes a driver, a
// context or a partition, and a matching pair of ledgers says the records agree, not that the
// hardware is idle.
func (r *ModelDeploymentReconciler) observeReleaseAcceleratorsReturned(
	ctx context.Context,
	record *modelDeploymentRetirementRelease,
) (string, bool) {
	if len(record.Nodes) == 0 {
		// A target that held no accelerator has nothing to observe. That is not a pass by default:
		// it is the case where the capture found no cards, and the capture refuses a GPU-requesting
		// member with no readable record before it ever gets here.
		return "", true
	}

	for i := range record.Nodes {
		node := &record.Nodes[i]

		// The ledger's identity is bookended. Same name and same UID: a Devices object recreated
		// under the same name is a different ledger, and one that was replaced mid-observation is
		// a changed input rather than a result.
		devices := new(workercore.Devices)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: node.DevicesName}, devices); err != nil {
			return fmt.Sprintf("reading the node's ledger: %v", err), false
		}
		if devices.UID != node.DevicesUID {
			return fmt.Sprintf(
				"the node's ledger is no longer the one that was captured (was %q, now %q)",
				node.DevicesUID, devices.UID), false
		}
		for _, card := range node.Cards {
			if !cardInInventory(devices, card) {
				return fmt.Sprintf(
					"accelerator %q of group %q has left the node's inventory", card.DeviceID, card.GroupID), false
			}
		}

		// Every pod on the node, in every namespace. A pod in another namespace can hold the card
		// this target released, and a namespace filter would miss exactly the holder that decides
		// whether the card came back.
		onNode := new(core.PodList)
		if err := r.APIReader.List(ctx, onNode, ctrlcli.MatchingFields{"spec.nodeName": node.NodeName}); err != nil {
			// A server that cannot answer the field selector is not a server this predicate can
			// reason from, and it is not a node with no holders.
			return fmt.Sprintf("listing the node's pods: %v", err), false
		}

		rebuilt, _, err := deviceplugin.BuildDesiredStatusStrict(
			ctrllog.FromContext(ctx), devices, onNode)
		if err != nil {
			return fmt.Sprintf("the node's allocation cannot be read strictly: %v", err), false
		}
		if reason, ok := releaseCardsAgree(node.Cards, rebuilt, devices.Status); !ok {
			return reason, false
		}

		// The node's inputs are bookended rather than read once. The projection above was built from
		// one listing; a pod that appears while it is being compared decides the answer, and reading
		// the input again is the only way to notice. These are two bounded reads, not an atomic
		// snapshot of the node, and they are not a claim that nothing changed before the first one.
		after := new(core.PodList)
		if err := r.APIReader.List(ctx, after, ctrlcli.MatchingFields{"spec.nodeName": node.NodeName}); err != nil {
			return fmt.Sprintf("re-reading the node's pods: %v", err), false
		}
		if !samePodProjection(onNode, after) {
			return fmt.Sprintf("the pods on node %q changed while its release was being read", node.NodeName), false
		}

		// The inventory and its capabilities are bookended for the same reason. A ledger that
		// gained or lost a card, or a group that changed what it can offer, changed while this was
		// being read; comparing the card that was captured says nothing about the others.
		again := new(workercore.Devices)
		if err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Name: node.DevicesName}, again); err != nil {
			return fmt.Sprintf("re-reading the node's ledger: %v", err), false
		}
		if again.UID != node.DevicesUID {
			return fmt.Sprintf("the node's ledger was replaced (was %q, now %q) while its release was being read",
				node.DevicesUID, again.UID), false
		}
		if before, after := devicesInventoryProjection(devices), devicesInventoryProjection(again); before != after {
			return fmt.Sprintf("the inventory on node %q changed while its release was being read",
				node.NodeName), false
		}
		// And against the projection captured with the target, so a capability that changed between
		// the capture and this observation is caught rather than only one that changed mid-read.
		if got := devicesInventoryProjection(devices); got != node.Inventory {
			return fmt.Sprintf("the inventory on node %q is not the one the target was carved from",
				node.NodeName), false
		}
	}

	return "", true
}

// devicesInventoryProjection renders everything a node's inventory says, so two reads of it can be
// compared. It is the whole inventory and not just the captured card: a group that gained a card, or
// a card that changed what it can offer, is a different set of capabilities to release into.
func devicesInventoryProjection(devices *workercore.Devices) string {
	lines := make([]string, 0, len(devices.Spec.Groups))
	for i := range devices.Spec.Groups {
		group := &devices.Spec.Groups[i]
		profiles := make([]string, 0, len(group.AcceleratorSlicedDetail.Physical.Profiles))
		for _, profile := range group.AcceleratorSlicedDetail.Physical.Profiles {
			profiles = append(profiles, strings.Join([]string{
				profile.Name,
				strconv.FormatInt(int64(profile.Count), 10),
				strconv.FormatInt(profile.MemoryMib, 10),
			}, "-"))
		}
		slices.Sort(profiles)
		groupLine := strings.Join([]string{
			group.ID, group.Manufacturer, group.Name,
			strconv.FormatUint(group.Memory, 10),
			strconv.FormatUint(uint64(group.Cores), 10),
			group.ComputeCapability, group.Family, group.DriverVersion, group.RuntimeVersion,
			strconv.FormatBool(group.AcceleratorSlicedDetail.Logical.CoresPercentageOvercommit),
			strconv.FormatInt(int64(group.AcceleratorSlicedDetail.Logical.Count), 10),
			strconv.FormatInt(int64(group.AcceleratorSlicedDetail.Physical.Count), 10),
			strings.Join(profiles, ","),
		}, "/")
		for j := range group.Accelerators {
			accelerator := &group.Accelerators[j]
			physical := make([]string, 0, len(accelerator.Status.PhysicalSliced.Profiles))
			for _, profile := range accelerator.Status.PhysicalSliced.Profiles {
				physical = append(physical, strings.Join([]string{
					profile.Name,
					strconv.FormatInt(profile.MemoryMib, 10),
					strconv.FormatInt(int64(profile.ComputeSlices), 10),
					strconv.FormatInt(int64(profile.MemorySlices), 10),
					strconv.FormatInt(int64(profile.Count), 10),
					renderPlacements(profile.Placements),
				}, "-"))
			}
			slices.Sort(physical)
			physicalIndexes := make([]string, 0, len(accelerator.PhysicalIndexes))
			for _, index := range accelerator.PhysicalIndexes {
				physicalIndexes = append(physicalIndexes, strconv.FormatUint(uint64(index), 10))
			}
			lines = append(lines, strings.Join([]string{
				groupLine, accelerator.ID,
				strconv.FormatUint(uint64(accelerator.Index), 10),
				strings.Join(physicalIndexes, ","),
				strconv.FormatBool(accelerator.Status.Unhealthy),
				strconv.FormatBool(accelerator.Status.LogicalSliced.CoresPercentageOvercommit),
				strconv.FormatInt(int64(accelerator.Status.LogicalSliced.Count), 10),
				strconv.FormatInt(int64(accelerator.Status.PhysicalSliced.Count), 10),
				strings.Join(physical, ","),
			}, "/"))
		}
	}
	slices.Sort(lines)

	return strings.Join(lines, "\n")
}

// renderPlacements renders a cached legal placement set. The ledger subtracts the intervals pods
// occupy from this set to derive what is still buildable, so a different set is a different set of
// possible releases and belongs in the projection.
func renderPlacements(placements []workercore.AcceleratorPlacement) string {
	rendered := make([]string, 0, len(placements))
	for _, placement := range placements {
		rendered = append(rendered, strconv.FormatInt(int64(placement.Start), 10)+
			":"+strconv.FormatInt(int64(placement.Length), 10))
	}
	slices.Sort(rendered)

	return strings.Join(rendered, ",")
}

// sameReleaseFacts reports whether two records state the same facts.
func sameReleaseFacts(a, b *modelDeploymentRetirementRelease) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	if err != nil {
		return false
	}

	return string(left) == string(right)
}

// samePodProjection reports whether two reads of a node's pods describe the same input, by
// identity and version. A pod that appeared, vanished or was rewritten between them is a different
// input, and the accounting built from the earlier one cannot answer for the later one.
func samePodProjection(before, after *core.PodList) bool {
	if len(before.Items) != len(after.Items) {
		return false
	}
	byUID := make(map[types.UID]string, len(before.Items))
	for i := range before.Items {
		byUID[before.Items[i].UID] = before.Items[i].ResourceVersion
	}
	for i := range after.Items {
		version, present := byUID[after.Items[i].UID]
		if !present || version != after.Items[i].ResourceVersion {
			return false
		}
	}

	return true
}

// releaseCardsAgree compares each captured card's PUBLISHED accounting with the strict rebuild.
//
// The comparison is by identity, not by position, and it compares the closed set of accounting
// fields the ledger publishes: mode, units and the slice/profile counts. A card may legitimately
// be carved for someone else by the time this runs, and that still completes: what is being asked
// is whether the records agree, not whether the card is idle. A whole-card-zero rule would refuse
// a card that was correctly handed on.
func releaseCardsAgree(
	cards []modelDeploymentRetirementReleaseCard, rebuilt workercore.DevicesStatus,
	published workercore.DevicesStatus,
) (string, bool) {
	for i := range cards {
		card := &cards[i]
		rebuiltCard := findPublishedCard(rebuilt, card)
		if rebuiltCard == nil {
			return fmt.Sprintf(
				"accelerator %q of group %q has no rebuilt accounting, so its release cannot be read",
				card.DeviceID, card.GroupID), false
		}
		publishedCard := findPublishedCard(published, card)
		if publishedCard == nil {
			return fmt.Sprintf(
				"accelerator %q of group %q has no published row", card.DeviceID, card.GroupID), false
		}
		if !releaseCardsMatch(rebuiltCard, publishedCard) {
			return fmt.Sprintf(
				"accelerator %q of group %q has not settled: published %s, the node's records say %s",
				card.DeviceID, card.GroupID, releaseCardString(publishedCard), releaseCardString(rebuiltCard)), false
		}
	}

	return "", true
}

// releaseCardString renders the published accounting of one card, for a reason a human can act on.
func releaseCardString(card *workercore.AcceleratorAllocation) string {
	return fmt.Sprintf("mode %s with %d of %d units and %d slices",
		card.Mode, card.Allocated, card.Allocated+card.Remaining, card.AllocatedSlices)
}

// releaseCardsMatch compares the published accounting of one card. Every field is compared, and
// nil and empty profile maps are equivalent only because an absent map and an empty one say the
// same thing about a card with no profiles on it.
func releaseCardsMatch(rebuilt, published *workercore.AcceleratorAllocation) bool {
	if rebuilt.ID != published.ID || rebuilt.Index != published.Index ||
		rebuilt.Mode != published.Mode ||
		rebuilt.Allocated != published.Allocated || rebuilt.Remaining != published.Remaining ||
		rebuilt.AllocatedSlices != published.AllocatedSlices ||
		!releaseProfileMapsEqual(rebuilt.AllocatedProfiles, published.AllocatedProfiles) ||
		!releaseProfileMapsEqual(rebuilt.RemainingProfiles, published.RemainingProfiles) {
		return false
	}

	return true
}

// releaseProfileMapsEqual compares a profile list by NAME and count, order-independently.
//
// The ledger emits these as a list, and the order in which two independent folds visit the pods
// that contributed them is not a property of the accounting. A duplicated name is not a reordering,
// and it is refused here rather than compared as if the two counts were two profiles.
func releaseProfileMapsEqual(a, b []workercore.AcceleratorProfileCount) bool {
	if len(a) == 0 && len(b) == 0 {
		return true
	}
	if len(a) != len(b) {
		return false
	}
	counts := make(map[string]int32, len(a))
	for i := range a {
		if _, duplicate := counts[a[i].Name]; duplicate {
			return false
		}
		counts[a[i].Name] = a[i].Count
	}
	// The second list is held to the same rule. A duplicate there is not a reordering either, and
	// comparing it as if the two counts were two profiles would read a doubled list as a match.
	seen := make(map[string]struct{}, len(b))
	for i := range b {
		if _, duplicate := seen[b[i].Name]; duplicate {
			return false
		}
		seen[b[i].Name] = struct{}{}
		count, present := counts[b[i].Name]
		if !present || count != b[i].Count {
			return false
		}
	}

	return true
}

// findPublishedCard resolves a captured identity to a row in an accounting ledger, refusing an
// ambiguous match rather than picking one.
func findPublishedCard(
	status workercore.DevicesStatus, card *modelDeploymentRetirementReleaseCard,
) *workercore.AcceleratorAllocation {
	var found *workercore.AcceleratorAllocation
	for i := range status.Groups {
		group := &status.Groups[i]
		if group.ID != card.GroupID || group.Manufacturer != card.Manufacturer {
			continue
		}
		for j := range group.Accelerators {
			accelerator := &group.Accelerators[j]
			if accelerator.ID != card.DeviceID || accelerator.Index != card.Index {
				continue
			}
			if found != nil {
				// Two rows claim one identity, so neither can be the one being compared.
				return nil
			}
			found = accelerator
		}
	}

	return found
}
