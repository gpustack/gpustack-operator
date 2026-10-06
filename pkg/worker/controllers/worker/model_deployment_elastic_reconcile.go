package worker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/deviceplugin"
	"gpustack.ai/gpustack/pkg/kubeapistatus"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/worker/elasticengine"
)

const (
	modelDeploymentElasticWithdrawnAnnotation = "modeldeployment.gpustack.ai/elastic-withdrawn"
	elasticObservationLabel                   = "gpustack.ai/elastic-observation"

	// ModelDeploymentConditionElasticResize reports whether an elastic scale command stands
	// terminally refused. It is declared beside the convergence that decides it, like every other
	// condition whose vocabulary sits with the code that can observe it. It is False with
	// ScaleRefused only while the durable record says the retry bound is spent, and True with
	// NoRefusal the rest of the time: a refusal still inside its bound is not terminal and must
	// not read as one.
	ModelDeploymentConditionElasticResize kubeapistatus.ConditionType = "ElasticResize"

	// modelDeploymentReasonScaleRefused is ElasticResize's False reason: the record is abandoned
	// and its message carries the engine's own last refusal.
	modelDeploymentReasonScaleRefused = "ScaleRefused"

	// modelDeploymentReasonNoScaleRefusal is ElasticResize's True reason: no record stands
	// refused, whether none exists, one is in flight, or one has just completed.
	modelDeploymentReasonNoScaleRefusal = "NoRefusal"
)

// The internal observation document preserves independent values and a last proven width.
// A previous value only selects the next observation. It never authorizes an engine action.
type modelDeploymentElasticStatus struct {
	DeploymentUID      types.UID             `json:"deploymentUID"`
	ObservedGeneration int64                 `json:"observedGeneration"`
	Desired            elasticLayer          `json:"desired"`
	Admitted           elasticLayer          `json:"admitted"`
	Allocated          elasticLayer          `json:"allocated"`
	Ray                elasticLayer          `json:"ray"`
	Effective          elasticLayer          `json:"effective"`
	State              elasticOperationState `json:"state"`
	Reason             string                `json:"reason"`
	StableWidth        int                   `json:"stableWidth"`
	MasterUID          types.UID             `json:"masterUID"`
}

type modelDeploymentElasticRayReader interface {
	Observe(context.Context, *workercore.ModelDeployment, []modelDeploymentElasticCapturedMember,
		int32, string) (modelDeploymentElasticObservation, error)
}

func modelDeploymentElasticObservationName(md *workercore.ModelDeployment) string {
	return "elastic-observation-" + string(md.UID)
}

func (r *ModelDeploymentReconciler) elasticObservationDocument(ctx context.Context, md *workercore.ModelDeployment) (*core.ConfigMap, error) {
	cm := new(core.ConfigMap)
	err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: modelDeploymentElasticObservationName(md)}, cm)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := elasticControllingOwnerIs(cm, md.Name, md.UID); err != nil {
		return nil, err
	}
	return cm, nil
}

func (r *ModelDeploymentReconciler) persistElasticObservation(
	ctx context.Context, md *workercore.ModelDeployment, status modelDeploymentElasticStatus,
) error {
	live := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), live); err != nil {
		return err
	}
	if live.UID != md.UID || live.Generation != md.Generation || live.DeletionTimestamp != nil {
		return fmt.Errorf("deployment changed before observation persistence")
	}
	cm, err := r.elasticObservationDocument(ctx, md)
	if err != nil {
		return err
	}
	if cm != nil {
		var prior modelDeploymentElasticStatus
		if err := json.Unmarshal([]byte(cm.Data["observation.json"]), &prior); err != nil {
			return err
		}
		if status.StableWidth == 0 && prior.MasterUID == status.MasterUID {
			status.StableWidth = prior.StableWidth
		}
	}
	body, err := json.Marshal(status)
	if err != nil {
		return err
	}
	if cm == nil {
		cm = &core.ConfigMap{
			ObjectMeta: meta.ObjectMeta{
				Name: modelDeploymentElasticObservationName(md), Namespace: md.Namespace,
				Labels: map[string]string{elasticObservationLabel: "true"},
				OwnerReferences: []meta.OwnerReference{{
					APIVersion: workercore.GroupVersion.String(), Kind: "ModelDeployment",
					Name: md.Name, UID: md.UID, Controller: elasticControllerRef(),
				}},
			},
			Data: map[string]string{"observation.json": string(body)},
		}
		return r.Client.Create(ctx, cm)
	}
	if cm.Data["observation.json"] == string(body) {
		return nil
	}
	cm.Data = map[string]string{"observation.json": string(body)}
	return r.Client.Update(ctx, cm)
}

func elasticOwnedBy(md *workercore.ModelDeployment, obj ctrlcli.Object) bool {
	owner := meta.GetControllerOf(obj)
	return owner != nil && owner.Kind == "ModelDeployment" &&
		owner.APIVersion == workercore.GroupVersion.String() && owner.Name == md.Name && owner.UID == md.UID && obj.GetUID() != ""
}

// errElasticDeploymentChanged reports that the live deployment no longer matches the identity or
// generation this pass captured. It is not a read failure: the spec watch requeues the deployment and
// the next pass re-reads it, so callers that would otherwise persist a reason skip it.
var errElasticDeploymentChanged = errors.New("deployment identity or generation changed")

func (r *ModelDeploymentReconciler) elasticLiveMembers(ctx context.Context, md *workercore.ModelDeployment) ([]core.Pod, error) {
	if r.APIReader == nil {
		return nil, fmt.Errorf("elastic observations require an uncached API reader")
	}
	live := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), live); err != nil {
		return nil, err
	}
	if live.UID != md.UID || live.Generation != md.Generation || live.DeletionTimestamp != nil {
		return nil, errElasticDeploymentChanged
	}
	// The list is narrowed to this deployment's identity labels so the pass does not walk the whole
	// namespace. The labels are only a server-side prefilter: elasticOwnedBy still carries the owner
	// UID, kind, API version and name, so a Pod a same-name recreated deployment left behind is
	// still excluded.
	list := new(core.PodList)
	if err := r.APIReader.List(ctx, list,
		ctrlcli.InNamespace(md.Namespace),
		ctrlcli.MatchingLabels{
			modelDeploymentLabelKeyName:     modelDeploymentLabelValueName,
			modelDeploymentLabelKeyInstance: md.Name,
		},
	); err != nil {
		return nil, err
	}
	role := ModelDeploymentElasticRole(md)
	var pods []core.Pod
	for i := range list.Items {
		pod := &list.Items[i]
		if !elasticOwnedBy(md, pod) {
			continue
		}
		if modelDeploymentPodRole(pod) == role.Name || modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
			pods = append(pods, *pod)
		}
	}
	slices.SortFunc(pods, func(a, b core.Pod) int { return strings.Compare(a.Name, b.Name) })
	return pods, nil
}

func elasticRuntimeMembers(md *workercore.ModelDeployment, pods []core.Pod) ([]modelDeploymentElasticCapturedMember, *core.Pod, error) {
	role := ModelDeploymentElasticRole(md)
	var captured []modelDeploymentElasticCapturedMember
	var master *core.Pod
	heads := 0
	for i := range pods {
		pod := &pods[i]
		memberRole := modelDeploymentElasticRoleWorker
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
			memberRole = modelDeploymentElasticRoleHead
			heads++
		} else {
			ordinal, valid := modelDeploymentPodOrdinal(pod)
			if !valid {
				return nil, nil, fmt.Errorf("GPU member has no ordinal")
			}
			if ordinal == 0 {
				if master != nil {
					return nil, nil, fmt.Errorf("multiple reserved masters")
				}
				master = pod
				memberRole = modelDeploymentElasticRoleMaster
			}
			if modelDeploymentPodRole(pod) != role.Name {
				return nil, nil, fmt.Errorf("foreign role")
			}
		}
		running := false
		for _, c := range pod.Status.ContainerStatuses {
			if c.Name == modelDeploymentMainContainerName && c.ContainerID != "" && c.State.Running != nil {
				running = true
			}
		}
		if !running {
			return nil, master, fmt.Errorf("member %s has no running engine identity", pod.Name)
		}
		captured = append(captured, modelDeploymentElasticCapturedMember{
			PodUID: pod.UID, PodName: pod.Name, Container: modelDeploymentMainContainerName, Role: memberRole,
		})
	}
	if master == nil || heads != 1 || master.Status.PodIP == "" {
		return nil, master, fmt.Errorf("reserved master or CPU head is not ready for observation")
	}
	return captured, master, nil
}

func elasticSameRuntime(before, after []core.Pod) bool {
	if len(before) != len(after) {
		return false
	}
	for i := range before {
		a, b := &before[i], &after[i]
		if a.Name != b.Name || a.UID != b.UID || a.Status.PodIP != b.Status.PodIP || a.DeletionTimestamp != nil || b.DeletionTimestamp != nil ||
			!reflect.DeepEqual(a.OwnerReferences, b.OwnerReferences) || !reflect.DeepEqual(a.Status.ContainerStatuses, b.Status.ContainerStatuses) ||
			!reflect.DeepEqual(a.Spec, b.Spec) ||
			a.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] != b.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] {
			return false
		}
	}
	return true
}

func elasticRankMapComplete(obs modelDeploymentElasticObservation, width int, master types.UID) bool {
	if !obs.WidthKnown || obs.RayWidth != width || len(obs.RankToPodUID) != width || obs.RankToPodUID[0] != master {
		return false
	}
	seen := sets.New[types.UID]()
	for rank := 0; rank < width; rank++ {
		uid := obs.RankToPodUID[rank]
		if uid == "" || seen.Has(uid) {
			return false
		}
		seen.Insert(uid)
	}
	return true
}

func (r *ModelDeploymentReconciler) elasticNativeClient(master *core.Pod) (*elasticengine.Client, error) {
	if r.elasticClient != nil {
		return r.elasticClient(master)
	}
	port := master.Annotations["prometheus.io/port"]
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return nil, fmt.Errorf("master listener port is unknown")
	}
	scheme := master.Annotations["prometheus.io/scheme"]
	if scheme != "http" {
		return nil, fmt.Errorf("unsupported native listener scheme")
	}
	return elasticengine.New("http://"+net.JoinHostPort(master.Status.PodIP, port), 150*time.Second)
}

func (r *ModelDeploymentReconciler) elasticEffective(ctx context.Context, md *workercore.ModelDeployment, pods []core.Pod,
	captured []modelDeploymentElasticCapturedMember, master *core.Pod, client *elasticengine.Client, width int,
) (elasticLayer, modelDeploymentElasticObservation, bool) {
	if r.elasticObserver == nil {
		return unknownLayer("Ray reader is unavailable"), modelDeploymentElasticObservation{}, false
	}
	first, err := r.elasticObserver.Observe(ctx, md, captured, int32(width), modelDeploymentElasticDefaultGCSAddress)
	if err != nil || !elasticRankMapComplete(first, width, master.UID) {
		return unknownLayer("native rank identities are unknown"), first, false
	}
	names, present := ModelDeploymentServedModelNames(md.Spec.Engine.Name, ModelDeploymentElasticRole(md).ExtraArgs)
	model := md.Spec.Model.Name
	if present && len(names) > 0 {
		model = names[0]
	}
	native, failure := client.ObserveNativeWorld(ctx, model, "Hello", width)
	if failure != nil {
		return unknownLayer(failure.Error()), first, false
	}
	second, err := r.elasticObserver.Observe(ctx, md, captured, int32(width), modelDeploymentElasticDefaultGCSAddress)
	after, identityErr := r.elasticLiveMembers(ctx, md)
	if err != nil || identityErr != nil || !elasticSameRuntime(pods, after) || !elasticRankMapComplete(second, width, master.UID) ||
		!reflect.DeepEqual(first.RankToPodUID, second.RankToPodUID) {
		return unknownLayer("runtime identities moved across native forwards"), second, false
	}
	if native.ObservedWidth != width || len(native.Forwards) != width {
		return unknownLayer("native forward proof is incomplete"), second, false
	}
	for rank, forward := range native.Forwards {
		if forward.Rank != rank || !forward.Served {
			return unknownLayer("native forward proof is incomplete"), second, false
		}
	}
	return knownLayer(width), second, true
}

func (r *ModelDeploymentReconciler) elasticSetWithdrawn(ctx context.Context, md *workercore.ModelDeployment, master *core.Pod, withdraw bool) error {
	fresh := new(core.Pod)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(master), fresh); err != nil {
		return err
	}
	if fresh.UID != master.UID || !elasticOwnedBy(md, fresh) {
		return fmt.Errorf("master changed before endpoint update")
	}
	if fresh.Annotations == nil {
		fresh.Annotations = map[string]string{}
	}
	if withdraw {
		fresh.Annotations[modelDeploymentElasticWithdrawnAnnotation] = "true"
		delete(fresh.Labels, modelDeploymentLabelKeyEndpointEligible)
	} else {
		delete(fresh.Annotations, modelDeploymentElasticWithdrawnAnnotation)
	}
	if !reflect.DeepEqual(fresh.Annotations, master.Annotations) || !reflect.DeepEqual(fresh.Labels, master.Labels) {
		if err := r.Client.Update(ctx, fresh); err != nil {
			return err
		}
	}
	if withdraw {
		if err := r.syncModelDeploymentService(ctx, md, true); err != nil {
			return err
		}
		return r.syncModelDeploymentRouter(ctx, md, true)
	}
	return nil
}

func (r *ModelDeploymentReconciler) elasticWithdrawal(ctx context.Context, md *workercore.ModelDeployment, master *core.Pod) (bool, string) {
	if err := r.elasticSetWithdrawn(ctx, md, master, true); err != nil {
		return false, err.Error()
	}
	if reason, held := r.observeModelDeploymentRetirementResidual(ctx, md, sets.New(master.UID)); held {
		return false, reason
	}
	drain, err := r.modelDeploymentDrainReaderOf().Drain(ctx, modelDeploymentDrainTarget{
		PodUID:        master.UID,
		DeploymentUID: md.UID, Container: modelDeploymentMainContainerName, Role: ModelDeploymentElasticRole(md).Name, Engine: md.Spec.Engine.Name,
	})
	if err != nil {
		return false, err.Error()
	}
	return drain.Complete && drain.State == modelDeploymentDrainIdle, drain.Reason
}

// elasticOperationMemberSurvives says whether any captured identity is still the live pod it was
// recorded against. It is the line between a mismatch the record can still act on and one whose
// world is gone: a node loss empties the record permanently, because replacement pods carry new
// identities no pass can ever match again.
func elasticOperationMemberSurvives(op *elasticOperation, pods []core.Pod) bool {
	byName := map[string]types.UID{}
	for i := range pods {
		byName[pods[i].Name] = pods[i].UID
	}
	for _, member := range append(slices.Clone(op.Members), op.Master, op.Head) {
		if member.UID != "" && byName[member.Name] == member.UID {
			return true
		}
	}
	return false
}

func elasticOperationMembersMatch(op *elasticOperation, pods []core.Pod) bool {
	byName := map[string]types.UID{}
	for i := range pods {
		byName[pods[i].Name] = pods[i].UID
	}
	for _, member := range append(slices.Clone(op.Members), op.Master, op.Head) {
		if member.UID == "" {
			return false
		}
		if byName[member.Name] != member.UID {
			retired := false
			if op.State == elasticStateReleased {
				for _, worker := range op.Workers {
					if worker == member && byName[member.Name] == "" {
						retired = true
					}
				}
			}
			if !retired {
				return false
			}
		}
	}
	return true
}

// recordElasticResizeCondition levels the deployment's ElasticResize condition with the durable
// record: False with the preserved refusal while a record stands abandoned, True with NoRefusal
// otherwise — including when no record exists, so a refusal never outlives the record it came
// from. The pass hands over the record as it stands after its own writes; a caller whose record
// was just deleted hands over nil.
//
// IT IS ITS OWN WRITE rather than a field set before the pass's status sync, for the same reason
// the retirement reservation is: the sync compares the derived status with the observed one and
// would find an assigned field equal to itself. The patch is optimistic and asserts only this
// condition, and the in-memory status takes the server's response so the same pass's later status
// sync rebuilds from what the server now holds. An unchanged condition writes nothing.
func (r *ModelDeploymentReconciler) recordElasticResizeCondition(
	ctx context.Context, md *workercore.ModelDeployment, op *elasticOperation,
) error {
	refused := op != nil && op.State == elasticStateAbandoned
	reason, message := modelDeploymentReasonNoScaleRefusal, "no elastic scale command stands refused"
	if refused {
		reason, message = modelDeploymentReasonScaleRefused, elasticScaleRefusalMessage(op)
	}
	base := md.DeepCopy()
	candidate := base.DeepCopy()
	if refused {
		ModelDeploymentConditionElasticResize.False(candidate, reason, message)
	} else {
		ModelDeploymentConditionElasticResize.True(candidate, reason, message)
	}
	if kubemeta.DeepEqual(base.Status, candidate.Status) {
		return nil
	}
	optimistic := ctrlcli.MergeFromWithOptions(base, ctrlcli.MergeFromWithOptimisticLock{})
	if err := r.Client.Status().Patch(ctx, candidate, optimistic); err != nil {
		return err
	}
	md.ResourceVersion = candidate.ResourceVersion
	md.Status = candidate.Status
	return nil
}

// elasticScaleRetryDue says how long the next refused-command attempt still owes, from the bound
// the record states: attempt n+1 waits min(15s*2^(n-1), 4m) after refusal n. Zero means due now.
func elasticScaleRetryDue(op *elasticOperation, now time.Time) time.Duration {
	if op.ScaleLastFailedAt == nil {
		return 0
	}
	wait := elasticScaleRetryBase
	for i := max(op.ScaleAttempts, 1); i > 1 && wait < elasticScaleRetryCap; i-- {
		wait *= 2
	}
	wait = min(wait, elasticScaleRetryCap)
	if due := op.ScaleLastFailedAt.Add(wait); now.Before(due) {
		return due.Sub(now)
	}
	return 0
}

// reconcileModelDeploymentElasticResize is the only native mutation caller.
// The durable record precedes the request. A resumed sent record only observes.
func (r *ModelDeploymentReconciler) reconcileModelDeploymentElasticResize(ctx context.Context, md *workercore.ModelDeployment) error {
	if r.APIReader == nil {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()
	role := ModelDeploymentElasticRole(md)
	status := modelDeploymentElasticStatus{
		DeploymentUID: md.UID, ObservedGeneration: md.Generation,
		Desired: knownLayer(int(role.ElasticEP.Width)), Admitted: unknownLayer("not collected"),
		Allocated: unknownLayer("not collected"), Ray: unknownLayer("not collected"), Effective: unknownLayer("not collected"),
	}
	finish := func(reason string) error { status.Reason = reason; return r.persistElasticObservation(ctx, md, status) }
	pods, err := r.elasticLiveMembers(ctx, md)
	if err != nil {
		if errors.Is(err, errElasticDeploymentChanged) {
			// The deployment moved under this pass, so the observation belongs to a world that no
			// longer exists and persisting it would be refused anyway. The spec watch requeues.
			return nil
		}
		// A read that failed is not an observation. Say so, so the hold names the failure instead of
		// leaving the deployment without a reason.
		return finish(err.Error())
	}
	status.Admitted, status.Allocated = r.observeModelDeploymentElasticAllocation(ctx, md, pods)
	captured, master, err := elasticRuntimeMembers(md, pods)
	if master != nil {
		status.MasterUID = master.UID
	}
	if err != nil {
		return finish(err.Error())
	}
	client, err := r.elasticNativeClient(master)
	if err != nil {
		return finish(err.Error())
	}
	store := newElasticOperationStore(r.Client)
	var op *elasticOperation
	op, err = store.Read(ctx, md.Namespace, md.Name, md.UID)
	if err != nil && !apierrors.IsNotFound(err) {
		return finish(err.Error())
	}
	if apierrors.IsNotFound(err) {
		op = nil
	}
	// From here the pass has read the record, so every verdict it finishes also levels the
	// deployment's condition with the record's refusal. The holds above asserted nothing about
	// the record and leave the condition exactly as the last record-reading pass left it.
	finish = func(reason string) error {
		status.Reason = reason
		if err := r.persistElasticObservation(ctx, md, status); err != nil {
			return err
		}
		return r.recordElasticResizeCondition(ctx, md, op)
	}
	if op != nil && !elasticOperationMembersMatch(op, pods) {
		// A MISMATCH IS ONLY A WEDGE WHEN THE RECORD STILL DESCRIBES THE WORLD. Node loss empties
		// the record permanently — replacement pods carry new identities, so no later pass can
		// ever match again — and the pending retirement has no work left, because the pods it
		// would retire are already gone. When no captured identity is still live and the spec has
		// moved on since the record was written, the pass recovers from the live world instead of
		// holding: reconstruction re-proves the native width over the live members through the
		// ordinary rank and forward proofs before any record moves, so no removal proof is
		// bypassed. A partial survival keeps the hold — some captured member still live means the
		// retirement may still have work to do. A same-generation drift keeps the hold too —
		// pods replaced while the spec is unchanged is an in-flight scene the record may still
		// describe, and only the spec edit proves the world it captured is not the one wanted.
		if op.Generation == md.Generation || elasticOperationMemberSurvives(op, pods) {
			status.State = op.State
			return finish("a captured member identity no longer matches; the operation holds")
		}
		return r.reconstructElasticGeneration(ctx, md, &status, finish, pods, captured, master, client, store, op)
	}
	if op != nil && op.Generation != md.Generation {
		return r.reconstructElasticGeneration(ctx, md, &status, finish, pods, captured, master, client, store, op)
	}
	var width int
	if op != nil {
		if op.SentState() {
			width = op.Width.Target
		} else {
			width = op.Width.Old
		}
	} else {
		boot, valid := ModelDeploymentElasticBootstrapFromMaster(
			string(master.UID), master.Annotations[ModelDeploymentElasticMasterBootWidthAnnotation], md,
		)
		if !valid {
			return finish("master bootstrap width is unknown")
		}
		width = int(boot)
		cm, readErr := r.elasticObservationDocument(ctx, md)
		if readErr != nil {
			return finish(readErr.Error())
		}
		if cm != nil {
			var old modelDeploymentElasticStatus
			if err := json.Unmarshal([]byte(cm.Data["observation.json"]), &old); err != nil {
				return finish("prior observation is malformed")
			}
			if old.MasterUID == master.UID && old.StableWidth >= 2 && old.StableWidth <= 64 {
				width = old.StableWidth
			}
		}
	}
	effective, ray, proven := r.elasticEffective(ctx, md, pods, captured, master, client, width)
	status.Effective = effective
	if ray.RegisteredGPUKnown {
		status.Ray = knownLayer(ray.RegisteredGPU)
	} else {
		status.Ray = unknownLayer(ray.RegisteredGPUUnknownReason)
	}
	if op == nil {
		// THE BOOKKEEPING WIDTH IS A HINT, NOT A GATE. A live world wider than the bookkeeping
		// width — an interrupted upscale whose record was lost — fails the rank-map check at the
		// bookkeeping width by construction: the rank proof is bounded by the width it is asked
		// for, so ranks beyond it are a width hold and the probe cannot even name the wider
		// world. Without a re-probe the pass would hold on that unknown forever. The admitted
		// member count is the live world's own claim, so when it names a wider world, re-prove at
		// that width: the same rank and forward proofs, run against the world that actually
		// exists, and the proven live width becomes the old width the ordinary corrective block
		// below works from. A narrower or unknown live world keeps today's behavior — a shrunken
		// cluster is a degraded scene the width hint cannot speak for.
		if !proven && status.Admitted.Known && status.Admitted.Value > width {
			live := status.Admitted.Value
			effective, ray, proven = r.elasticEffective(ctx, md, pods, captured, master, client, live)
			status.Effective = effective
			if ray.RegisteredGPUKnown {
				status.Ray = knownLayer(ray.RegisteredGPU)
			} else {
				status.Ray = unknownLayer(ray.RegisteredGPUUnknownReason)
			}
			if !proven {
				return finish(status.Effective.Reason)
			}
			width = live
		}
		if !proven {
			return finish(status.Effective.Reason)
		}
		status.StableWidth = width
		if width == int(role.ElasticEP.Width) {
			return finish("native ranks and forwards agree")
		}
		if int(role.ElasticEP.Width) > width &&
			(!status.Admitted.Known || status.Admitted.Value < int(role.ElasticEP.Width) ||
				!status.Allocated.Known || status.Allocated.Value < int(role.ElasticEP.Width) ||
				!status.Ray.Known || status.Ray.Value < int(role.ElasticEP.Width)) {
			return finish("new members are not admitted, allocated and registered")
		}
		op = &elasticOperation{
			Name: md.Name, Namespace: md.Namespace, DeploymentUID: md.UID, Generation: md.Generation,
			Width:  elasticWidth{Old: width, Target: int(role.ElasticEP.Width)},
			Master: elasticCapturedIdentity{Name: master.Name, UID: master.UID}, State: elasticStateRecorded,
		}
		var retiring []core.Pod
		for i := range pods {
			pod := &pods[i]
			identity := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
			if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
				op.Head = identity
			} else {
				op.Members = append(op.Members, identity)
			}
			if op.Width.Upward() {
				continue
			}
			for rank, uid := range ray.RankToPodUID {
				if rank >= op.Width.Target && uid == pod.UID {
					op.Workers = append(op.Workers, identity)
					retiring = append(retiring, *pod)
				}
			}
		}
		if !op.Width.Upward() {
			if len(op.Workers) != op.Width.Old-op.Width.Target {
				return finish("retirement rank identities are incomplete")
			}
			op.Release, err = r.captureModelDeploymentElasticRelease(ctx, md, retiring)
			if err != nil {
				return finish(err.Error())
			}
		}
		if _, err = store.Create(ctx, op); err != nil {
			return err
		}
	}
	status.State = op.State
	facts := elasticFacts{
		DeploymentUID: md.UID, Generation: md.Generation, Desired: status.Desired, Admitted: status.Admitted,
		AdmittedAllocation: status.Allocated, Ray: status.Ray, Effective: status.Effective, ForwardProven: proven,
		ActorMappingKnown: ray.ActorsComplete, ActorFree: map[types.UID]bool{},
	}
	// A native read that fails is not a width the engine has not reported. It is a question this pass
	// could not ask, so it is carried out to the hold below rather than folded into NativeComplete,
	// which would report the engine as still scaling and advance the operation on an answer that
	// never arrived. The withdrawal and release blocks above still run, so a read failure does not
	// reorder the scale-down contract.
	var nativeReadFailure error
	if op.SentState() && proven {
		scaling, readErr := client.IsScaling(ctx)
		if readErr != nil {
			nativeReadFailure = readErr
		}
		facts.NativeComplete = readErr == nil && !scaling
	} else if op.SentState() && op.ScaleLastError != "" {
		// A record carrying a refusal is asked the same question even though its target world is
		// not proven: a retry is owed to an idle engine and to nothing else, so a refusal is never
		// re-issued into a resize that is already running.
		scaling, readErr := client.IsScaling(ctx)
		if readErr != nil {
			nativeReadFailure = readErr
		}
		facts.ScalingKnown = readErr == nil
		facts.Scaling = scaling
	}
	for uid, fact := range ray.ActorFree {
		facts.ActorFree[uid] = fact.ActorFree
	}
	if !op.Width.Upward() {
		var reason string
		facts.WithdrawalConfirmed, reason = r.elasticWithdrawal(ctx, md, master)
		if !facts.WithdrawalConfirmed {
			return finish(reason)
		}
	}
	if op.State == elasticStateReleased {
		if !proven || !facts.NativeComplete || !ray.ActorsComplete {
			return finish("native release proof is not current")
		}
		if err := r.elasticDeleteCaptured(ctx, md, op, ray); err != nil {
			return finish(err.Error())
		}
		reason, gone := r.observeReleaseTargetsAbsent(ctx, op.Release)
		if !gone {
			return finish(reason)
		}
		reason, gone = r.observeReleaseWorkloadsGone(ctx, md, op.Release)
		if !gone {
			return finish(reason)
		}
		reason, gone = r.observeReleaseAcceleratorsReturned(ctx, op.Release)
		if !gone {
			return finish(reason)
		}
		facts.ReleaseObserved = true
	}
	if !op.SentState() && !proven {
		return finish("current native ranks and forwards do not prove the old world")
	}
	if nativeReadFailure != nil {
		return finish(nativeReadFailure.Error())
	}
	decision := decideElasticOperation(op, facts)
	switch decision.Action {
	case elasticActionRequestScale:
		// Fresh deployment and every member are checked again after the intent-producing observations.
		live, err := r.elasticLiveMembers(ctx, md)
		if err != nil || !elasticSameRuntime(pods, live) {
			return finish("identity changed before native intent")
		}
		op.State = elasticStateCommandSent
		op.CommandSent = true
		op.CommandIntent = fmt.Sprintf("scale to %d", op.Width.Target)
		op.ScaleAttempts = 1
		if err := store.Update(ctx, op); err != nil {
			return err
		}
		if err := client.Scale(ctx, op.Width.Target, 120*time.Second); err != nil {
			// The engine answered with a refusal, which is an answer: the record keeps its text
			// and the after-command passes turn it into a bounded retry or a terminal abandon.
			failedAt := r.modelDeploymentNow()
			op.ScaleLastError = err.Error()
			op.ScaleLastFailedAt = &failedAt
			if err := store.Update(ctx, op); err != nil {
				return err
			}
			status.State = op.State
			return finish(err.Error())
		}
		status.State = op.State
		return finish("native command recorded and sent; completion unobserved")
	case elasticActionRetryScale:
		// The same identity re-check the first send makes: a command re-issued to a world that
		// moved since the observations is a command about members this pass cannot vouch for.
		live, err := r.elasticLiveMembers(ctx, md)
		if err != nil || !elasticSameRuntime(pods, live) {
			return finish("identity changed before the refused scale command is re-issued")
		}
		// The bound is the kernel's decision; when the attempt is due is this clock's question.
		if wait := elasticScaleRetryDue(op, r.modelDeploymentNow()); wait > 0 {
			return finish(fmt.Sprintf(
				"the engine refused the scale command to width %d; the next attempt backs off %s; the recorded refusal: %s",
				op.Width.Target, wait.Truncate(time.Second), op.ScaleLastError))
		}
		op.ScaleAttempts++
		if err := store.Update(ctx, op); err != nil {
			return err
		}
		if err := client.Scale(ctx, op.Width.Target, 120*time.Second); err != nil {
			failedAt := r.modelDeploymentNow()
			op.ScaleLastError = err.Error()
			op.ScaleLastFailedAt = &failedAt
			if err := store.Update(ctx, op); err != nil {
				return err
			}
			status.State = op.State
			return finish(err.Error())
		}
		// The re-issued command was accepted, so the refusal it follows is no longer the last
		// word: the record clears it and the ordinary observation decides the outcome.
		op.ScaleLastError = ""
		op.ScaleLastFailedAt = nil
		if err := store.Update(ctx, op); err != nil {
			return err
		}
		status.State = op.State
		return finish("native command recorded and sent; completion unobserved")
	case elasticActionRelease:
		op.State = elasticStateReleased
		if err := store.Update(ctx, op); err != nil {
			return err
		}
		if err := r.elasticDeleteCaptured(ctx, md, op, ray); err != nil {
			return finish(err.Error())
		}
	case elasticActionComplete:
		status.StableWidth = op.Width.Target
		status.State = elasticStateCompleted
		if err := r.persistElasticObservation(ctx, md, status); err != nil {
			return err
		}
		if err := r.elasticSetWithdrawn(ctx, md, master, false); err != nil {
			return err
		}
		if err := store.Delete(ctx, op); err != nil {
			return err
		}
		// The record is gone, so no refusal can stand: the condition is leveled from that fact
		// rather than from the in-memory record this pass deleted.
		return r.recordElasticResizeCondition(ctx, md, nil)
	default:
		if op.State != decision.State {
			op.State = decision.State
			if err := store.Update(ctx, op); err != nil {
				return err
			}
		}
	}
	status.State = op.State
	return finish(decision.Reason)
}

// reconstructElasticGeneration re-observes a record bound to an older generation instead of
// returning before observation. The captured envelope, its width, its release and its intent stay
// exactly as recorded, and the old generation is never substituted into a current fact: the layers
// below are read against the CURRENT deployment. Nothing is dispatched and nothing is deleted on
// this pass, so no new target is sent here; only proof gathered now may retire the record, and only
// through the narrow exits below. Anything less keeps the whole record and all capacity.
func (r *ModelDeploymentReconciler) reconstructElasticGeneration(
	ctx context.Context, md *workercore.ModelDeployment, status *modelDeploymentElasticStatus,
	finish func(string) error, pods []core.Pod, captured []modelDeploymentElasticCapturedMember,
	master *core.Pod, client *elasticengine.Client, store *elasticOperationStore, op *elasticOperation,
) error {
	// The width to prove is the one the record asked for, read from the record's own envelope and
	// never from the current spec, so a generation edit cannot turn this pass into a fresh target.
	observed := op.Width.Old
	if op.SentState() {
		observed = op.Width.Target
	}
	effective, ray, proven := r.elasticEffective(ctx, md, pods, captured, master, client, observed)
	status.Effective = effective
	if ray.RegisteredGPUKnown {
		status.Ray = knownLayer(ray.RegisteredGPU)
	} else {
		status.Ray = unknownLayer(ray.RegisteredGPUUnknownReason)
	}
	status.State = op.State
	status.StableWidth = 0
	if !proven {
		if reason, recovered, err := r.recoverElasticUpscaleAfterGenerationChange(
			ctx, md, status, pods, captured, master, client, store, op,
		); err != nil {
			return err
		} else if recovered {
			// The recovery either retired the record or swapped it for a release-only one, and
			// both outcomes leave no refusal standing, so the condition is leveled from a
			// completed record rather than from the one this pass entered with.
			op.State = elasticStateCompleted
			return finish(reason)
		}
	}

	reason, safe := r.elasticGenerationExit(ctx, md, status, client, ray, proven, op)
	if !safe {
		return finish(reason)
	}

	// The record goes only after the proof is durable and the instance it withheld is given back, so
	// the next ordinary pass reads a verified stable width rather than a missing operation.
	status.StableWidth = observed
	status.Reason = reason
	status.State = ""
	if op.SentState() {
		status.State = elasticStateCompleted
	}
	if err := r.persistElasticObservation(ctx, md, *status); err != nil {
		return err
	}
	if err := r.elasticSetWithdrawn(ctx, md, master, false); err != nil {
		return err
	}
	if err := store.Delete(ctx, op); err != nil {
		return err
	}
	// The record this pass held was deleted, so no refusal can stand, whatever state the record
	// carried: the condition is leveled from the deletion rather than from the in-memory copy.
	return r.recordElasticResizeCondition(ctx, md, nil)
}

// recoverElasticUpscaleAfterGenerationChange handles the one stale-intent case that ordinary
// reconstruction cannot retire: an upward command was recorded, the spec was then returned to the
// proven old width, and the engine still proves that old world. The target width is deliberately not
// treated as reached. Instead, the old proof establishes that the command did not advance the native
// world, and the extra admitted members are placed in a fresh, release-only record. That record is
// marked Released because its native target is already observed; it still requires the ordinary
// withdrawal, actor-free and allocation-return proofs before deleting anything.
func (r *ModelDeploymentReconciler) recoverElasticUpscaleAfterGenerationChange(
	ctx context.Context, md *workercore.ModelDeployment, status *modelDeploymentElasticStatus,
	pods []core.Pod, captured []modelDeploymentElasticCapturedMember, master *core.Pod,
	client *elasticengine.Client, store *elasticOperationStore, op *elasticOperation,
) (string, bool, error) {
	if !op.Width.Upward() || !status.Desired.Known || status.Desired.Value != op.Width.Old {
		return "", false, nil
	}

	oldEffective, oldRay, oldProven := r.elasticEffective(
		ctx, md, pods, captured, master, client, op.Width.Old,
	)
	if !oldProven {
		return "", false, nil
	}
	status.Effective = oldEffective
	if oldRay.RegisteredGPUKnown {
		status.Ray = knownLayer(oldRay.RegisteredGPU)
	} else {
		status.Ray = unknownLayer(oldRay.RegisteredGPUUnknownReason)
	}
	if !oldRay.ActorsComplete {
		return "", false, nil
	}
	scaling, err := client.IsScaling(ctx)
	if err != nil {
		return "", false, err
	}
	if scaling {
		return "", false, nil
	}

	ranked := sets.New[types.UID]()
	for _, uid := range oldRay.RankToPodUID {
		ranked.Insert(uid)
	}
	role := ModelDeploymentElasticRole(md)
	var retiring []core.Pod
	var gpuMembers []core.Pod
	var headIdentity, masterIdentity elasticCapturedIdentity
	for i := range pods {
		pod := &pods[i]
		if modelDeploymentPodRole(pod) == modelDeploymentElasticHeadName(md) {
			headIdentity = elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
			continue
		}
		if modelDeploymentPodRole(pod) != role.Name {
			continue
		}
		gpuMembers = append(gpuMembers, *pod)
		identity := elasticCapturedIdentity{Name: pod.Name, UID: pod.UID}
		ordinal, valid := modelDeploymentPodOrdinal(pod)
		if valid && ordinal == 0 {
			masterIdentity = identity
		}
		if !ranked.Has(pod.UID) {
			retiring = append(retiring, *pod)
		}
	}
	if len(retiring) == 0 {
		// THE PROOF OUTLIVES THE RECORD, not the other way around: the observation claiming the
		// completed old width is written before the stale record is retired, so a failed write
		// or a crash in between leaves the record holding the deployment rather than a deleted
		// record behind an observation nothing backs.
		status.StableWidth = op.Width.Old
		status.State = elasticStateCompleted
		status.Reason = "the interrupted upscale never left the proven old native width; the stale record was retired"
		if err := r.persistElasticObservation(ctx, md, *status); err != nil {
			return "", false, err
		}
		if err := store.Delete(ctx, op); err != nil {
			return "", false, err
		}
		return status.Reason, true, nil
	}
	if len(gpuMembers) <= op.Width.Old || masterIdentity.UID == "" || headIdentity.UID == "" {
		return "", false, nil
	}
	release, err := r.captureModelDeploymentElasticRelease(ctx, md, retiring)
	if err != nil {
		return "", false, err
	}
	recovered := &elasticOperation{
		Name: md.Name, Namespace: md.Namespace, DeploymentUID: md.UID, Generation: md.Generation,
		Width:  elasticWidth{Old: len(gpuMembers), Target: op.Width.Old},
		Master: masterIdentity, Head: headIdentity, Release: release,
		State: elasticStateReleased, CommandSent: true,
		CommandIntent: fmt.Sprintf("recovered at already observed native width %d", op.Width.Old),
	}
	for _, member := range captured {
		identity := elasticCapturedIdentity{Name: member.PodName, UID: member.PodUID}
		if member.Role != modelDeploymentElasticRoleHead {
			recovered.Members = append(recovered.Members, identity)
		}
	}
	for _, pod := range retiring {
		recovered.Workers = append(recovered.Workers,
			elasticCapturedIdentity{Name: pod.Name, UID: pod.UID})
	}
	// The observation is written before the swap, and the swap itself is one write: a Delete
	// followed by a Create over the same name would leave the deployment with no record at all
	// between them, and a crash or a failed Create there would lose both the stale intent and
	// the retirement it owed.
	status.State = recovered.State
	status.StableWidth = op.Width.Old
	status.Reason = "the interrupted upscale was recovered at the proven old native width; excess members await the normal release proofs"
	if err := r.persistElasticObservation(ctx, md, *status); err != nil {
		return "", false, err
	}
	if err := store.RecoverInterrupted(ctx, op, recovered); err != nil {
		return "", false, err
	}
	return status.Reason, true, nil
}

// elasticGenerationExit is the whole decision of the reconstruction pass: a reason for every case
// that keeps the record, and safe=true only where the CURRENT world already proves the outcome. It
// deletes nothing, so a downward exit is reachable only when the resources are already back.
func (r *ModelDeploymentReconciler) elasticGenerationExit(
	ctx context.Context, md *workercore.ModelDeployment, status *modelDeploymentElasticStatus,
	client *elasticengine.Client, ray modelDeploymentElasticObservation,
	proven bool, op *elasticOperation,
) (string, bool) {
	if !proven {
		return "the current native ranks and forwards do not prove this record's own width; " +
			"the operation holds", false
	}
	scaling, err := client.IsScaling(ctx)
	if err != nil {
		return err.Error(), false
	}
	if scaling {
		return "the engine is still scaling, so this record's outcome is not observable yet; " +
			"the operation holds", false
	}
	if !op.SentState() {
		if !status.Desired.Known || status.Desired.Value != op.Width.Target {
			return "the desired target changed before send; the operation and admitted capacity stay held", false
		}
		return "the current native world proves this unsent record's old width and no scale is in " +
			"flight, so it never left that width", true
	}
	if !ray.ActorsComplete {
		return "the actor mapping is not complete, so the runtime identities this record captured " +
			"cannot be re-proved; the operation holds", false
	}
	if op.Width.Upward() {
		if !status.Admitted.Known || status.Admitted.Value < op.Width.Target ||
			!status.Allocated.Known || status.Allocated.Value < op.Width.Target ||
			!status.Ray.Known || status.Ray.Value < op.Width.Target {
			return "the target width is reached natively, but the current admitted, allocated and " +
				"registered members do not yet hold it; the operation holds", false
		}
		return "the current native width, every rank forward, the actor mapping and the admitted, " +
			"allocated and registered members all prove the upward target", true
	}
	// A narrowing record has no capacity to give back on this pass, so every return must already
	// have happened on its own. Nothing here creates an absence.
	if op.Release == nil || op.Release.ModelDeploymentUID != op.DeploymentUID ||
		op.Release.ObservedGeneration != op.Generation || len(op.Workers) == 0 {
		return "this narrowing record carries no complete captured release, so its return cannot be " +
			"proved; the operation holds", false
	}
	if reason, absent := r.observeReleaseTargetsAbsent(ctx, op.Release); !absent {
		return reason, false
	}
	if reason, gone := r.observeReleaseWorkloadsGone(ctx, md, op.Release); !gone {
		return reason, false
	}
	if reason, returned := r.observeReleaseAcceleratorsReturned(ctx, op.Release); !returned {
		return reason, false
	}
	return "the target width, every rank forward, the actor mapping and the already-returned " +
		"captured release all prove the narrowing outcome", true
}

// elasticDeleteCaptured removes the members one captured operation retired, under preconditions that
// name the identity it captured rather than the name it bears now.
func (r *ModelDeploymentReconciler) elasticDeleteCaptured(
	ctx context.Context, md *workercore.ModelDeployment, op *elasticOperation, ray modelDeploymentElasticObservation,
) error {
	if op.Release == nil || op.Release.ModelDeploymentUID != op.DeploymentUID || op.Release.ObservedGeneration != op.Generation || len(op.Workers) == 0 {
		return fmt.Errorf("captured allocation lineage is missing")
	}
	freshMD := new(workercore.ModelDeployment)
	if err := r.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(md), freshMD); err != nil {
		return err
	}
	if freshMD.UID != op.DeploymentUID || freshMD.Generation != op.Generation {
		return fmt.Errorf("deployment changed before retirement")
	}
	for _, worker := range op.Workers {
		pod := new(core.Pod)
		err := r.APIReader.Get(ctx, ctrlcli.ObjectKey{Namespace: md.Namespace, Name: worker.Name}, pod)
		if apierrors.IsNotFound(err) {
			continue
		}
		if err != nil {
			return err
		}
		fact, known := ray.ActorFree[worker.UID]
		if !known || !fact.ActorFree {
			return fmt.Errorf("captured worker has no current actor-free proof")
		}
		if pod.UID != worker.UID || !elasticOwnedBy(md, pod) || pod.UID == op.Master.UID {
			return fmt.Errorf("captured worker identity changed")
		}
		matched := false
		for _, member := range op.Release.Members {
			if member.PodUID == worker.UID && member.Name == worker.Name && member.Claim == pod.Annotations[deviceplugin.AllocatedAcceleratorAnnoKey] {
				matched = true
			}
		}
		if !matched {
			return fmt.Errorf("captured allocation changed before retirement")
		}
		if pod.DeletionTimestamp != nil {
			continue
		}
		if err := r.Client.Delete(ctx, pod, ctrlcli.Preconditions{UID: &worker.UID, ResourceVersion: &pod.ResourceVersion}); err != nil {
			return err
		}
	}
	workloads := new(kueue.WorkloadList)
	if err := r.APIReader.List(ctx, workloads, ctrlcli.InNamespace(md.Namespace)); err != nil {
		return err
	}
	for i := range workloads.Items {
		wl := &workloads.Items[i]
		if !slices.Contains(op.Release.WorkloadUIDs, string(wl.UID)) {
			continue
		}
		matched := false
		for _, owner := range wl.OwnerReferences {
			for _, worker := range op.Workers {
				if owner.UID == worker.UID && owner.Name == worker.Name && owner.Kind == "Pod" && owner.APIVersion == "v1" {
					matched = true
				}
			}
		}
		if !matched {
			return fmt.Errorf("captured Workload owner changed")
		}
		if wl.DeletionTimestamp == nil {
			if err := r.Client.Delete(ctx, wl, ctrlcli.Preconditions{UID: &wl.UID, ResourceVersion: &wl.ResourceVersion}); err != nil {
				return err
			}
		}
	}
	return nil
}
