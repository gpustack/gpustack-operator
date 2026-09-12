package worker

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"time"

	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/utils/stringx"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
)

// kvCacheBackendTierCleanupDeadline bounds how long a deletion waits for one node's tier to be
// emptied.
//
// IT IS MEASURED FROM THAT NODE'S OWN CLEANUP POD, not from the object's deletion timestamp and not
// from the oldest Pod of the pass. Timing from the deletion does not work: everything before this
// step is unbounded, because the teardown holds while a consumer still names the backend and again
// while the rendered workloads terminate -- measured on a cluster, that second wait alone held an
// object for twelve minutes when one node was unreachable. Timed that way the cleanup would arrive
// already expired, report every node as abandoned, and delete the Pods it had just created, having
// never once tried.
//
// Per node rather than per pass, because the Pods are not born together: a Create that failed
// transiently is retried on a later requeue, and a shared clock would then report a young node as
// abandoned "after 5m" with a fraction of that elapsed -- an elapsed time belonging to a different
// node.
//
// Counting passes instead would be worse than either: a counter kept in memory resets on every
// controller restart, and a wedged node would hold the object open forever.
//
// The Pods are therefore the clock, which holds only while they are never recreated. That is why
// ensureKVCacheBackendTierCleanupPod leaves a failing Pod alone rather than rebuilding it, and why
// the one path that creates no Pod at all -- an image that cannot be resolved -- gives up instead of
// reporting itself as pending. A clock made of Pods stops being a clock the moment something
// restarts it.
//
// It can be short because the cleanup Pod runs the image the members ran, which is already on every
// node this group covered, so no pull stands between the Pod and its work.
const kvCacheBackendTierCleanupDeadline = 5 * time.Minute

// Event reasons for the tier cleanup. They are recorded against the NODE, not against the backend:
// the backend is being deleted and takes its events with it, while the leftover data and the node
// outlive it together.
const (
	kvCacheBackendEventTierAbandoned     = "KVCacheTierNotCleaned"
	kvCacheBackendEventTierShared        = "KVCacheTierSharedPath"
	kvCacheBackendEventTierPartlyEmptied = "KVCacheTierPartlyEmptied"
)

// cleanKVCacheBackendTier empties the disk tier of a backend being deleted, on every node the
// declaring member group covered.
//
// It reports done when nothing is left to wait for, which includes having GIVEN UP: past the
// deadline the remaining nodes keep their content and the deletion proceeds, because a finalizer
// that waits for a node which will never answer leaves an object nobody can delete. Giving up is
// never silent - each node that keeps its content gets a warning Event and a logged error.
//
// It runs after the rendered workloads are gone and before the lock comes off. Both halves of that
// ordering are load-bearing: with the members still running the store would write into a directory
// being emptied, and after the lock comes off the object is gone and nothing would run this at all.
func (r *KVCacheBackendReconciler) cleanKVCacheBackendTier(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) (done bool, err error) {
	logger := ctrllog.FromContext(ctx)

	member := kvCacheBackendCleanableTier(kvcb)
	if member == nil {
		return true, nil
	}
	path := filepath.Clean(member.LocalDisk.Path)

	nodes := new(core.NodeList)
	if err = r.Client.List(ctx, nodes, ctrlcli.MatchingLabels(member.NodeSelector)); err != nil {
		return false, fmt.Errorf("list nodes carrying the disk tier: %w", err)
	}

	// A node that no longer exists is not waited for and is not reported. Its local disk left with
	// it, so there is nothing on it to clean.
	//
	// ASSUMPTION, stated because it is the kind that fails quietly: that a missing Node object means
	// a missing disk. A Node deleted while its machine keeps running would leave content behind that
	// this pass never mentions. Nothing here can reach such a node to find out.
	if len(nodes.Items) == 0 {
		return true, nil
	}

	// Read once for the whole pass rather than once per node. The reads are served from the informer
	// cache, so this is not about round-trips; it is that every one of them copies every backend in
	// the cluster, and inside the node loop that is a copy per node per backend on every teardown
	// tick. One snapshot also means every node in this pass is judged against the same set.
	others := new(workercore.KVCacheBackendList)
	if err = r.Client.List(ctx, others); err != nil {
		return false, fmt.Errorf("list kv cache backends sharing the disk tier: %w", err)
	}

	var pending int
	for i := range nodes.Items {
		node := &nodes.Items[i]

		// Another backend's tier in the same directory is the one case where doing the work is worse
		// than skipping it: emptying the directory would take that backend's LIVE data with it.
		// Nothing refuses two backends naming one path, so this is checked here rather than assumed
		// away.
		// IT IS A CHECK AND NOT A LOCK, and the difference is worth stating rather than leaving to be
		// discovered. Closing the window entirely needs an atomic claim on the path, which is a
		// design this change does not introduce.
		//
		// WHAT THE CHECK COVERS IS THE POD'S WHOLE LIFE AND NOT THE INSTANT IT RUNS IN, which an
		// earlier version of this comment got wrong: it described the window as lying between this
		// read and the Pod's `rm`, when the sharing can begin at any point while that `rm` is still
		// going. Re-running the check each pass does not on its own reach a removal that has already
		// started -- the Pod was built by an earlier pass and nothing revisits the decision -- so
		// skipping the node here would leave the deletion this branch just refused running to
		// completion, against a directory another backend now holds. The Pod is therefore ENDED
		// rather than merely not created.
		//
		// What still gets through is whatever the `rm` managed between one pass and the next, which
		// is why the second event exists: a partial removal of another backend's live data is a
		// different thing to report from a directory left alone.
		if holder := kvCacheBackendTierSharedWith(kvcb, others, path, node); holder != "" {
			stopped, serr := r.stopKVCacheBackendTierCleanupPod(ctx, kvcb, node)
			if serr != nil {
				return false, serr
			}
			r.recordWarning(node, kvCacheBackendEventTierShared,
				"%s was not emptied for deleted KVCacheBackend %q: KVCacheBackend %q declares an "+
					"overlapping path on this node and emptying it would remove that backend's data",
				path, kvcb.Name, holder)
			if stopped {
				r.recordWarning(node, kvCacheBackendEventTierPartlyEmptied,
					"%s may hold a partial result: emptying it for deleted KVCacheBackend %q was "+
						"already running when KVCacheBackend %q was found to declare an overlapping "+
						"path, and was stopped; check that backend's data on this node",
					path, kvcb.Name, holder)
			}
			logger.Info("skipped a disk tier shared with another backend",
				"node", node.Name, "path", path, "sharedWith", holder, "stoppedRemoval", stopped)
			continue
		}

		finished, pod, cerr := r.ensureKVCacheBackendTierCleanupPod(ctx, kvcb, member, path, node)
		if cerr != nil {
			return false, cerr
		}
		if finished {
			continue
		}

		// EACH NODE IS TIMED BY ITS OWN POD. The Pods are not born on one pass -- a Create that
		// failed transiently is retried on a later requeue -- so a shared clock taken from the
		// oldest of them would report a young node as abandoned "after 5m" with a fraction of that
		// elapsed, asserting a duration belonging to a different node.
		//
		// No Pod means no clock and nothing to report yet: this node is waiting for one to be built,
		// which is the state right after a stale Pod of another incarnation was cleared away.
		if pod == nil || pod.CreationTimestamp.IsZero() ||
			time.Since(pod.CreationTimestamp.Time) <= kvCacheBackendTierCleanupDeadline {
			pending++
			continue
		}

		// The message says what is KNOWN, which is that the directory was not confirmed empty --
		// not that it still holds data, which this cannot see. The difference is not pedantic: a Pod
		// still Pending at the deadline has never run, and the likeliest reason is that the
		// directory is not on this node at all, where "still holds data" is a false accusation. The
		// phase is named so the two are told apart by whoever reads the event.
		// THE MESSAGE CARRIES NO ELAPSED TIME, only the deadline it crossed. The teardown requeues
		// every couple of seconds while any other node is still pending, so this branch fires again
		// on each pass -- and client-go folds repeats into one Event with a count only while the
		// message is IDENTICAL. An exact age made every repeat a distinct Event, which is how a
		// single abandoned node turned into a hundred of them.
		r.recordWarning(node, kvCacheBackendEventTierAbandoned,
			"%s was not confirmed empty for deleted KVCacheBackend %q: its cleanup was still %s "+
				"after %s and the deletion was not held open for it; a Pod that never left pending "+
				"usually means the directory does not exist on this node",
			path, kvcb.Name, strings.ToLower(string(pod.Status.Phase)),
			kvCacheBackendTierCleanupDeadline)
		// The log carries the exact age, which the Event deliberately does not: a log line is not
		// deduplicated by its text, so precision costs nothing here.
		logger.Error(nil, "gave up emptying a disk tier",
			"node", node.Name, "path", path, "phase", pod.Status.Phase,
			"age", time.Since(pod.CreationTimestamp.Time),
			"deadline", kvCacheBackendTierCleanupDeadline)
	}

	if pending > 0 {
		return false, nil
	}

	// Past here every node is either emptied, skipped, or reported, and the Pods go with the pass.
	// A Pod in the operator's own namespace naming a backend that no longer exists is litter, and
	// nothing would ever collect it: the object that owns it is about to be released.
	//
	// The cost is that a failed Pod's logs go too, in exactly the case where they would have been
	// the account of why a node kept its content. The Event is what remains, which is why it names
	// the path, the backend and the deadline rather than only saying that something was skipped.
	return true, r.deleteKVCacheBackendTierCleanupPods(ctx, kvcb)
}

// stopKVCacheBackendTierCleanupPod ends this backend's cleanup on one node and reports whether there
// was one to end.
//
// It is how a decision taken at the top of a pass reaches a removal that a previous pass started. The
// shared-path check runs every pass, but the `rm` does not ask anything between its own start and its
// end -- so without this the check refuses to begin a deletion that is already most of the way
// through, which reads as a refusal and behaves as a completion.
//
// THE UID LABEL IS CHECKED AND NOT THE NAME. The name is derived from the backend and the node, so it
// repeats across incarnations; a Pod belonging to another one is emptying a path on another object's
// behalf, and ending it here would do to that object exactly what this branch exists to prevent.
//
// Ending it is not instantaneous -- the Pod terminates on its own budget -- so this bounds the
// removal rather than canceling it. That is the same trade the give-up path already takes, and for
// the same reason: a partially emptied directory is a cost, while a removal nobody can stop running
// against another backend's live data is a loss.
func (r *KVCacheBackendReconciler) stopKVCacheBackendTierCleanupPod(
	ctx context.Context, kvcb *workercore.KVCacheBackend, node *core.Node,
) (stopped bool, err error) {
	key := ctrlcli.ObjectKey{
		Name:      kvCacheBackendTierCleanupPodName(kvcb, node),
		Namespace: kuberess.SystemNamespaceName,
	}
	pod := new(core.Pod)
	if err = r.Client.Get(ctx, key, pod); err != nil {
		return false, ctrlcli.IgnoreNotFound(err)
	}
	if pod.Labels[kvCacheBackendTierCleanupUIDLabel] != string(kvcb.UID) {
		return false, nil
	}

	// Already going, so reporting it stopped would be an event about work this pass did not do; the
	// removal it names was ended by whichever pass issued the delete.
	if pod.DeletionTimestamp != nil {
		return false, nil
	}
	if err = r.Client.Delete(ctx, pod); err != nil {
		if kerrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("stop the disk tier cleanup pod on a shared path: %w", err)
	}

	return true, nil
}

// kvCacheBackendTierCleanupPods lists the cleanup Pods belonging to one backend.
func (r *KVCacheBackendReconciler) kvCacheBackendTierCleanupPods(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) ([]core.Pod, error) {
	pods := new(core.PodList)
	err := r.Client.List(ctx, pods,
		ctrlcli.InNamespace(kuberess.SystemNamespaceName),
		ctrlcli.MatchingLabels(map[string]string{
			kvCacheBackendTierCleanupLabel:    kvcb.Name,
			kvCacheBackendTierCleanupUIDLabel: string(kvcb.UID),
		}))
	if err != nil {
		return nil, fmt.Errorf("list the disk tier cleanup pods: %w", err)
	}
	return pods.Items, nil
}

// kvCacheBackendCleanableTier returns the disk tier this backend asked to have emptied, with the
// node selector of the group that declared it.
//
// Admission allows only one member group to declare localDisk, so there is one tier and one
// selector rather than a set of them. The loop is still written over every group, because a rule
// enforced elsewhere is not a fact this function can read.
// It returns the whole member group rather than the tier alone, because the cleanup has to run the
// image THAT GROUP runs. A group may override the backend-wide image, and the cleanup Pod's promise
// that no pull stands between it and its work only holds for the image the members already pulled.
func kvCacheBackendCleanableTier(
	kvcb *workercore.KVCacheBackend,
) *workercore.KVCacheBackendMember {
	if kvcb.Spec.Connection.Managed == nil {
		return nil
	}
	for i := range kvcb.Spec.Connection.Managed.Members {
		member := &kvcb.Spec.Connection.Managed.Members[i]
		if member.LocalDisk == nil || !member.LocalDisk.CleanAfterDelete {
			continue
		}
		if strings.TrimSpace(member.LocalDisk.Path) == "" {
			continue
		}
		return member
	}
	return nil
}

// kvCacheBackendTierSharedWith names another backend whose tier would be destroyed by emptying path
// on node, or "" when none is.
//
// Overlap rather than equality: a tier nested inside another one is removed by emptying the outer
// directory just as surely as an identical path is.
//
// It takes the backends rather than reading them, so one snapshot serves every node of a pass.
func kvCacheBackendTierSharedWith(
	kvcb *workercore.KVCacheBackend, others *workercore.KVCacheBackendList, path string, node *core.Node,
) string {
	for i := range others.Items {
		other := &others.Items[i]
		if other.Name == kvcb.Name || other.Spec.Connection.Managed == nil {
			continue
		}
		// A backend that is ITSELF being deleted AND asked for its tier to be emptied is not a live
		// claim on this path: its own cleanup is removing the same content. Without this, two
		// backends sharing a directory and deleted together each see the other, each skips, and the
		// content both administrators asked to have removed survives with only a "shared path"
		// event.
		//
		// The two halves are both required. A backend being deleted WITHOUT cleanAfterDelete is
		// keeping its content on purpose -- that is what the default means -- so emptying the
		// directory would destroy data whose owner asked for it to stay, which is worse than the
		// deadlock this avoids.
		if other.DeletionTimestamp != nil && kvCacheBackendCleanableTier(other) != nil {
			continue
		}
		for j := range other.Spec.Connection.Managed.Members {
			member := &other.Spec.Connection.Managed.Members[j]
			if member.LocalDisk == nil {
				continue
			}
			if !kvCacheBackendPathsOverlap(path, filepath.Clean(member.LocalDisk.Path)) {
				continue
			}
			// The other backend only reaches this node if its own group selects it.
			if !kvCacheBackendNodeSelected(member.NodeSelector, node.Labels) {
				continue
			}
			return other.Name
		}
	}
	return ""
}

// kvCacheBackendNodeSelected reports whether a member group's nodeSelector picks a node.
//
// It is the kubelet's own rule for spec.nodeSelector: every pair must be present on the node, and an
// empty selector picks every node. Written out rather than borrowed from labels.Set, because the
// empty case is the one that matters here and reading it from the code is cheaper than trusting a
// memory of which helper treats empty as "all" and which as "none".
func kvCacheBackendNodeSelected(selector, nodeLabels map[string]string) bool {
	for k, v := range selector {
		if nodeLabels[k] != v {
			return false
		}
	}
	return true
}

// kvCacheBackendPathsOverlap reports whether emptying one directory would remove the other.
func kvCacheBackendPathsOverlap(a, b string) bool {
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ensureKVCacheBackendTierCleanupPod drives one node's cleanup Pod and reports whether that node is
// finished with.
//
// The Pod carries an explicit nodeName and no toleration logic, which is deliberate: bypassing the
// scheduler is what lets it run on a cordoned or tainted node, and what makes a node that is truly
// gone fail immediately instead of sitting Pending until the deadline.
func (r *KVCacheBackendReconciler) ensureKVCacheBackendTierCleanupPod(
	ctx context.Context,
	kvcb *workercore.KVCacheBackend,
	member *workercore.KVCacheBackendMember,
	path string,
	node *core.Node,
) (finished bool, pod *core.Pod, err error) {
	// THE EXISTING POD IS READ BEFORE THE IMAGE IS RESOLVED, and the order is the point. The image is
	// needed only to create a Pod, so resolving it first lets a setting that changed between passes
	// speak about a node whose Pod already ran: a succeeded cleanup would be reported as abandoned,
	// and a running one would be given up on for a reason that is not its deadline.
	key := ctrlcli.ObjectKey{
		Name:      kvCacheBackendTierCleanupPodName(kvcb, node),
		Namespace: kuberess.SystemNamespaceName,
	}
	pod = new(core.Pod)
	err = r.Client.Get(ctx, key, pod)
	switch {
	case err == nil:
		// THE NAME REPEATS ACROSS INCARNATIONS and the Get does not filter, so the UID has to be
		// checked here as well as in the listing. A Pod left behind by a previous backend of this
		// name -- force-deleted, or collected too slowly -- is found by this Get: believed, a
		// Succeeded one reports a node cleaned that this backend never touched, and any other phase
		// hands this backend a deadline that ran out before it started.
		if pod.Labels[kvCacheBackendTierCleanupUIDLabel] != string(kvcb.UID) {
			if derr := r.Client.Delete(ctx, pod); derr != nil && !kerrors.IsNotFound(derr) {
				return false, nil, fmt.Errorf("delete a disk tier cleanup pod of another "+
					"incarnation: %w", derr)
			}
			return false, nil, nil
		}
		if pod.Status.Phase == core.PodSucceeded {
			return true, pod, nil
		}
		// EVERY OTHER PHASE IS STILL IN FLIGHT, AND THE POD IS NEVER DELETED TO RETRY IT. Retrying is
		// the kubelet's job here: the Pod asks for RestartPolicyOnFailure, so a transient failure --
		// a mount briefly unavailable, a node rebooting mid-run -- is restarted in place with
		// backoff. Measured on a live kubelet: a container failing every time keeps its Pod in
		// Running with a climbing restart count and never reaches Failed at all.
		//
		// Deleting a Pod so the next pass recreates it would be the one thing that breaks the
		// deadline, because the deadline is the Pod's own creation timestamp: a fresh Pod every pass
		// erases the elapsed time, expired never becomes true, and a node that can never be cleaned
		// holds the backend open forever.
		return false, pod, nil
	case !kerrors.IsNotFound(err):
		return false, nil, fmt.Errorf("get the disk tier cleanup pod: %w", err)
	}

	// The GROUP's image, not the backend's. A group may name its own, and running the backend-wide
	// one here would pull an image onto a node that never had it -- which is exactly the wait this
	// Pod is supposed to have none of.
	image := member.Image
	if image == "" {
		if image, err = resolveKVCacheBackendImage(ctx, kvcb); err != nil {
			// GIVING UP HERE, not waiting. Nothing can run without an image, and on this branch no
			// Pod exists, so there is no clock either: reported as pending, this node would wait on
			// a deadline that never starts, because the deadline is measured from a Pod that was
			// never created. The finalizer would then hold the backend open for as long as the
			// operator stays without an image, which is the object nobody can delete that this
			// feature exists to prevent.
			//
			// So it takes the same exit a node past the deadline takes, on the same Event, and the
			// message names the image as the reason rather than the node.
			r.recordWarning(node, kvCacheBackendEventTierAbandoned,
				"%s still holds data from deleted KVCacheBackend %q: no image could be resolved to "+
					"empty it, and the deletion was not held open for it (%v)",
				path, kvcb.Name, err)
			ctrllog.FromContext(ctx).Error(err, "gave up emptying a disk tier with no image to run",
				"node", node.Name, "path", path)
			return true, nil, nil
		}
	}

	pod = kvCacheBackendTierCleanupPod(kvcb, key, image, path, node)
	if err = r.Client.Create(ctx, pod); err != nil && !kerrors.IsAlreadyExists(err) {
		return false, nil, fmt.Errorf("create the disk tier cleanup pod: %w", err)
	}
	// Just created. Returned as it stands, so the caller times this node from the Pod it now has
	// rather than from whichever Pod happened to be created first.
	return false, pod, nil
}

// kvCacheBackendTierCleanupPod renders one node's cleanup Pod.
//
// It removes the CONTENT of the directory and never the directory itself. Whoever prepared the node
// created it, gave it an owner this operator did not choose, and may have mounted a filesystem
// there; removing it would undo all three.
func kvCacheBackendTierCleanupPod(
	kvcb *workercore.KVCacheBackend, key ctrlcli.ObjectKey, image, path string, node *core.Node,
) *core.Pod {
	pod := &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Name:      key.Name,
			Namespace: key.Namespace,
			Labels: map[string]string{
				kvCacheBackendTierCleanupLabel:    kvcb.Name,
				kvCacheBackendTierCleanupUIDLabel: string(kvcb.UID),
			},
			// OWNED BY THE BACKEND, so the API server collects these Pods even when this controller
			// does not get to. The teardown deletes them itself on the path it completes, but that
			// path is not the only one: a force-deleted object, a stripped finalizer, or a
			// controller that stops between creating a Pod and collecting it would otherwise leave
			// them in the operator's namespace with nothing that ever removes them.
			//
			// blockOwnerDeletion is left unset. Setting it would make these Pods able to hold the
			// backend's own deletion open, which is the failure this whole feature exists to
			// prevent, one level down -- the same reason they carry no finalizer.
			OwnerReferences: []meta.OwnerReference{
				{
					APIVersion: workercore.GroupVersion.String(),
					Kind:       "KVCacheBackend",
					Name:       kvcb.Name,
					UID:        kvcb.UID,
				},
			},
		},
		Spec: core.PodSpec{
			NodeName: node.Name,
			// OnFailure, so the kubelet retries the container in place and the Pod outlives its own
			// failures. That is what keeps the creation timestamp a usable deadline: the alternative
			// -- Never, plus this controller deleting a failed Pod so the next pass builds another --
			// resets the clock on every pass, and the give-up deadline then never fires.
			RestartPolicy: core.RestartPolicyOnFailure,
			// The same credentials the members pulled with. Without them a private store image
			// leaves this Pod Pending until the deadline, and the node is then reported as
			// abandoned for a reason that has nothing to do with the node.
			ImagePullSecrets: kvcb.Spec.ImagePullSecrets,
			Containers: []core.Container{
				{
					Name:  "clean",
					Image: image,
					// The glob does not reach dotfiles, so the store's own hidden entries are named
					// separately. The directory itself is never an argument to rm.
					//
					// THE LAST COMMAND IS THE VERDICT, and it is a test rather than a report. An
					// earlier revision ended on `ls -A /tier | wc -l`, which exits zero whatever it
					// counts, so a partial removal reached PodSucceeded and was recorded as a
					// cleaned node. That is the failure this feature is written to make impossible:
					// giving up has to be loud, and a pipeline that cannot fail turns it silent.
					//
					// THE COUNT ALONE STILL CANNOT FAIL ON A DIRECTORY IT CANNOT READ, and an
					// earlier version of this comment claimed it could. `ls` writes its error to
					// stderr and nothing to stdout, `wc -l` prints 0, and the test passes — a node
					// whose content was never touched, recorded as emptied. So the readability is
					// probed as its own command, where `set -e` can act on its status. It is the
					// same hole, and the same fix, as the member survey's entries=-1: a count cannot
					// carry a failure whose symptom is a count.
					//
					// `rm -rf` is not tolerated with `|| true` either, and it does not need to be:
					// `-f` already ignores operands that do not exist, which is what an unmatched
					// glob expands to. Measured in `sh`: the three globs against an empty directory
					// exit zero under `set -e`.
					Command: []string{
						"sh", "-c",
						`set -e; rm -rf -- /tier/* /tier/.[!.]* /tier/..?*; ` +
							`ls -A /tier >/dev/null 2>&1; ` +
							`[ "$(ls -A /tier | wc -l)" -eq 0 ]`,
					},
					VolumeMounts: []core.VolumeMount{
						{Name: "tier", MountPath: "/tier"},
					},
				},
			},
			Volumes: []core.Volume{
				{
					Name: "tier",
					VolumeSource: core.VolumeSource{
						HostPath: &core.HostPathVolumeSource{
							Path: path,
							// Directory rather than DirectoryOrCreate, because creating the path
							// would leave an empty directory the administrator never asked for --
							// the same objection that keeps this operator from preparing the tier
							// in the first place.
							//
							// THE COST IS THAT AN ABSENT PATH IS NOT A QUICK NO-OP, and an earlier
							// version of this comment said it was. The kubelet fails the type check,
							// the Pod sits Pending on FailedMount, and the node is waited on for the
							// whole deadline before being reported. Nothing on the Pod distinguishes
							// that from any other Pending: FailedMount is an Event, not a condition,
							// so the give-up Event names the phase instead of asserting the node
							// still holds data it cannot see.
							Type: ptr.To(core.HostPathDirectory),
						},
					},
				},
			},
		},
	}
	// DELIBERATELY NOT LOCKED. The repository's finalizer marks an object whose disappearance the
	// operator needs to notice, and this Pod is the opposite of that: it exists for one teardown and
	// the object that would remove its finalizer is the backend, which is deleted moments later.
	// Locked, it outlives its owner as a Pod nobody can delete - which is the failure this whole
	// feature is written to avoid, one level down.
	return pod
}

// kvCacheBackendTierCleanupLabel finds the cleanup Pods of one backend, and
// kvCacheBackendTierCleanupUIDLabel narrows that to one INCARNATION of it.
//
// A name is reusable: delete a backend and create another with the same name, and a Pod left over
// from the first would answer to the second's label. That matters because these Pods are the
// deadline's clock -- a stale Pod's creation timestamp is old, so the new backend's very first
// teardown pass would read itself as already expired and report every node as abandoned without
// having tried once. The UID cannot be reused, so matching on both makes the clock belong to the
// object that started it.
const (
	kvCacheBackendTierCleanupLabel    = "kvcache.gpustack.ai/tier-cleanup-of"
	kvCacheBackendTierCleanupUIDLabel = "kvcache.gpustack.ai/tier-cleanup-uid"
)

// kvCacheBackendTierCleanupPodNameLimit is what the API server accepts for a Pod name.
//
// Measured against a live server rather than recalled, because the number that comes to mind is the
// wrong one: a Pod name validates as a DNS SUBDOMAIN, so 253, and not the 63 of a DNS label. Probed
// at v1.36.1 with a server-side dry run, 253 was accepted and 254 refused.
const kvCacheBackendTierCleanupPodNameLimit = 253

// kvCacheBackendTierCleanupPodName names one node's cleanup Pod.
//
// NEITHER NAME IT IS BUILT FROM IS BOUNDED BELOW THE LIMIT. A node name and a KVCacheBackend name are
// both DNS subdomains, so either can use the whole 253 on its own, and a name built by concatenating
// them is refused on exactly the clusters that have long ones. The hash carries both identities, so
// the readable prefix is there for the operator reading `kubectl get pods` and can be given up
// whenever it does not fit, with no two nodes or two backends ever colliding.
//
// The prefix is dropped WHOLE rather than truncated. A KVCacheBackend name may contain dots, and a
// cut landing on one leaves a segment starting with "-", which the API server refuses just as surely
// as a name that is too long -- trading a bug that needs a long name for a bug that needs a long name
// with a dot in the wrong place.
//
// Getting this wrong is not cosmetic. A refused Create fails the teardown on every pass, and the
// finalizer then leaves a backend nobody can delete, which is the outcome this whole feature exists
// to prevent.
func kvCacheBackendTierCleanupPodName(kvcb *workercore.KVCacheBackend, node *core.Node) string {
	// The separator matters: the hash writes its arguments end to end, so without one ("a", "bc")
	// and ("ab", "c") would be the same Pod. A "/" cannot occur in either name.
	suffix := "-tier-clean-" + stringx.SumByFNV64a(kvcb.Name, "/", node.Name)
	if len(kvcb.Name)+len(suffix) <= kvCacheBackendTierCleanupPodNameLimit {
		return kvcb.Name + suffix
	}
	return "kvcache" + suffix
}

// deleteKVCacheBackendTierCleanupPods removes the Pods this backend's cleanup created.
func (r *KVCacheBackendReconciler) deleteKVCacheBackendTierCleanupPods(
	ctx context.Context, kvcb *workercore.KVCacheBackend,
) error {
	pods, err := r.kvCacheBackendTierCleanupPods(ctx, kvcb)
	if err != nil {
		return err
	}
	for i := range pods {
		// EVERY POD GOES, INCLUDING A RUNNING ONE, and that is a deliberate reversal. Leaving a
		// running `rm` alone past the deadline spares a slow but healthy node from being
		// interrupted -- but the Pod then keeps deleting from a path this backend no longer holds.
		// Nothing stops a new backend claiming the same directory the moment the finalizer is
		// released, and its freshly written data would be removed by the previous backend's `rm`.
		// The ownerReference collects the Pod eventually, and "eventually" is not a bound on how
		// fast an operator can recreate a backend.
		//
		// The two costs are not the same size. Interrupting means the directory is partially
		// emptied, which the give-up Event already reports and which is content somebody asked to
		// have removed. Not interrupting means destroying data nobody asked to have removed. So the
		// removal is ended here, and the Event says the cleanup was still running when it was.
		//
		// A plain delete, with nothing to release first, because these Pods carry no finalizer. See
		// kvCacheBackendTierCleanupPod for why they must not.
		if err = r.Client.Delete(ctx, &pods[i]); err != nil && !kerrors.IsNotFound(err) {
			return fmt.Errorf("delete the disk tier cleanup pod: %w", err)
		}
	}
	return nil
}
