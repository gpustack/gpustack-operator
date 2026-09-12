package worker

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// kvCacheBackendEventTierReused is recorded when a backend's tier directory already had content in
// it at the moment the backend started.
const kvCacheBackendEventTierReused = "KVCacheTierNotEmpty"

// reportKVCacheBackendTierReuse decides, ONCE, whether this backend started on a directory that was
// not empty, and records the answer where it survives.
//
// It REPORTS AND NOTHING ELSE: it does not empty the directory, does not write a marker into it, and
// does not keep the backend from starting. Reusing a path is a legitimate thing to do — restoring a
// tier deliberately is the obvious case — so the only defensible action is to say so.
//
// WHY A CONDITION AND NOT ONLY AN EVENT. The question is "was the directory empty when this backend
// started", and after the first write nothing on the cluster can still answer it: the member's own
// data is indistinguishable from somebody else's. So it cannot be recomputed from observed state the
// way the rest of this status is, and the alternatives were both worse. Gating on "no segment
// mounted yet" was measured and does not work — the window between a readable survey and the first
// segment is a few seconds wide, the reconcile does not land inside it, and on a backend that never
// mounts a segment the gate never closes and the event repeats forever. A marker file would answer
// it durably and would mean writing into the administrator's directory, which is the one thing this
// feature exists to avoid doing unasked.
//
// So the condition is written once and then left alone, and Exists is what enforces that. It is a
// deliberate piece of non-level-based state in a status that is otherwise recomputed every pass, and
// it is here because the fact it records stops being observable.
func (r *KVCacheBackendReconciler) reportKVCacheBackendTierReuse(
	ctx context.Context, kvcb, holder *workercore.KVCacheBackend,
) {
	if kvcb.Spec.Connection.Managed == nil {
		return
	}
	// Already decided. Never revisited, so a member restarting on this backend's own data cannot
	// turn a clean start into a reported reuse.
	if KVCacheBackendConditionTierWasEmpty.Exists(holder) {
		return
	}

	logger := ctrllog.FromContext(ctx)

	var wanted, surveyed int
	reused := map[string]int{}

	for group := range kvcb.Spec.Connection.Managed.Members {
		if kvcb.Spec.Connection.Managed.Members[group].LocalDisk == nil {
			continue
		}

		// THE DENOMINATOR IS NODES, NOT PODS THAT HAPPEN TO EXIST. Counting the Pods in hand made
		// "every node has reported" true as soon as the first one did, because a node whose Pod has
		// not been created yet -- still scheduling, still pulling -- contributes nothing to either
		// side of the comparison. The DaemonSet's own desired count is the figure that already
		// accounts for the selector and for the nodes it cannot place on, so it is taken rather than
		// recomputed from a node listing that would disagree with it.
		ds := new(apps.DaemonSet)
		err := r.Client.Get(ctx, ctrlcli.ObjectKey{
			Name:      mooncake.MemberObjectName(kvcb, group),
			Namespace: kuberess.SystemNamespaceName,
		}, ds)
		if err != nil {
			// Not rendered yet, or unreadable. Either way the question cannot be closed on this
			// pass, and closing it would be permanent.
			if !kerrors.IsNotFound(err) {
				logger.Error(err, "read the member daemonset to size the disk tier survey",
					"group", group)
			}
			return
		}
		wanted += int(ds.Status.DesiredNumberScheduled)

		pods := new(core.PodList)
		err = r.Client.List(ctx, pods,
			ctrlcli.InNamespace(kuberess.SystemNamespaceName),
			ctrlcli.MatchingLabels(mooncake.MemberSelectorLabels(kvcb, group)))
		if err != nil {
			logger.Error(err, "list member pods to read the disk tier survey", "group", group)
			return
		}

		for i := range pods.Items {
			pod := &pods.Items[i]
			// A REPLACEMENT POD CANNOT ANSWER A QUESTION ABOUT THE PAST. The survey runs before its
			// own member, so the first Pod on a node reports what the tier held before this backend
			// wrote anything. A Pod recreated later -- a node reboot, an eviction, an image change --
			// re-runs the same survey against a tier THIS backend has since filled, and its entries
			// would be reported as somebody else's leftovers. It is not evidence either way, so it
			// is left out of both counts rather than counted as a node that has not answered, which
			// would block the verdict forever.
			if !kvCacheBackendTierSurveyInScope(kvcb, pod) {
				continue
			}
			entries, ok := kvCacheBackendTierSurveyReading(pod)
			if !ok {
				continue
			}
			surveyed++
			if entries > 0 {
				reused[pod.Spec.NodeName] = entries
			}
		}
	}

	// THE TWO VERDICTS DO NOT NEED THE SAME EVIDENCE, and treating them alike is what made a partial
	// survey dangerous. One node reporting content settles "the tier was not empty" whatever the
	// other nodes say, so the warning goes out on the readings in hand. "The tier was empty"
	// generalises over nodes that have not answered, so it waits until every member Pod has: written
	// early, one Pod still pulling would freeze a clean verdict over a node holding content, and the
	// write-once rule makes that permanent.
	//
	// The price is that a survey which can never succeed on one node -- an image with no shell, a
	// mount that cannot be read -- leaves the condition ABSENT rather than wrong. That is the right
	// way round: absent says the question was not answered, which is true, and it is documented so a
	// reader is not left guessing.
	if len(reused) == 0 {
		if wanted == 0 || surveyed != wanted {
			return
		}
		KVCacheBackendConditionTierWasEmpty.True(holder, "Empty",
			fmt.Sprintf("the disk tier was empty on all %d node(s) carrying it when this backend "+
				"started", wanted))
		return
	}

	nodes := make([]string, 0, len(reused))
	for node := range reused {
		nodes = append(nodes, node)
	}
	sort.Strings(nodes)

	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		parts = append(parts, fmt.Sprintf("%s: %d", node, reused[node]))
	}
	message := fmt.Sprintf("the disk tier already held entries when this backend started (%s); "+
		"nothing was removed, and a key written here can read back as content the previous backend "+
		"left", strings.Join(parts, ", "))

	KVCacheBackendConditionTierWasEmpty.False(holder, "PreexistingContent", message)
	// One Event beside the condition, on the transition rather than on every pass: the condition is
	// what a reader finds later, and the Event is what reaches anybody watching now. Guarded like
	// the cleanup's, because a reconciler built without a recorder would panic here instead.
	r.recordTierWarning(kvcb, kvCacheBackendEventTierReused, "%s", message)
}

// kvCacheBackendTierSurveyWindow is how long after a backend is created its member Pods are still
// taken to be its FIRST generation, and so still able to say what the tier held before it ran.
//
// There is no exact signal for this. Nothing records which Pod was first on a node, and no status
// field marks the moment the store could first have written, so a Pod created long after the backend
// is distinguished from one created during the initial rollout by elapsed time and nothing else.
//
// Chosen for the direction it fails in rather than for precision. Too short and a slow rollout --
// scheduling plus a cold image pull -- leaves the condition ABSENT, which says the question went
// unanswered and is true. Too long and a Pod replaced inside the window is believed, which is the
// false accusation this exists to prevent. So it is generous against a rollout and still far shorter
// than the life of a backend.
const kvCacheBackendTierSurveyWindow = 30 * time.Minute

// kvCacheBackendTierSurveyInScope reports whether a member Pod's survey still describes the tier as
// this backend FOUND it, rather than as this backend has since made it.
func kvCacheBackendTierSurveyInScope(kvcb *workercore.KVCacheBackend, pod *core.Pod) bool {
	if kvcb.CreationTimestamp.IsZero() || pod.CreationTimestamp.IsZero() {
		return false
	}
	return pod.CreationTimestamp.Sub(kvcb.CreationTimestamp.Time) <= kvCacheBackendTierSurveyWindow
}

// kvCacheBackendTierSurveyReading reads one member's survey, and refuses a reading that cannot be
// trusted.
//
// The survey reports two counts and the second one is why this can return a verdict at all. Every
// way the survey can fail — no shell in the image, an unreadable mount, a command that is not there
// — produces an EMPTY first count, which is indistinguishable from the directory being empty, and
// "empty" is the answer being looked for. The root directory of a running container is never empty,
// so a control of zero is the survey having failed and is refused rather than reported as a clean
// tier.
//
// A NEGATIVE COUNT IS THE OTHER FAILURE, and the control cannot stand in for it. The control says
// the survey ran; it says nothing about whether the tier could be read. A directory the container's
// user cannot list produces a healthy control beside a count of zero, so the survey reports -1 for
// it explicitly and that is refused here. Two failures, two signals, because one signal cannot carry
// both.
func kvCacheBackendTierSurveyReading(pod *core.Pod) (entries int, ok bool) {
	var message string
	for i := range pod.Status.InitContainerStatuses {
		status := &pod.Status.InitContainerStatuses[i]
		if status.Name != mooncake.MemberLocalDiskSurveyContainerName {
			continue
		}
		if status.State.Terminated == nil {
			return 0, false
		}
		message = status.State.Terminated.Message
		break
	}
	if message == "" {
		return 0, false
	}

	var entriesSeen, control string
	for field := range strings.SplitSeq(strings.TrimSpace(message), " ") {
		key, value, found := strings.Cut(field, "=")
		if !found {
			continue
		}
		switch key {
		case "entries":
			entriesSeen = value
		case "control":
			control = value
		}
	}

	controlCount, err := strconv.Atoi(strings.TrimSpace(control))
	if err != nil || controlCount == 0 {
		return 0, false
	}
	entries, err = strconv.Atoi(strings.TrimSpace(entriesSeen))
	if err != nil || entries < 0 {
		return 0, false
	}
	return entries, true
}
