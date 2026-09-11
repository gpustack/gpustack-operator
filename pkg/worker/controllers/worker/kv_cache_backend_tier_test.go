package worker

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apps "k8s.io/api/apps/v1"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/validation"
	"k8s.io/client-go/tools/record"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/worker/kuberess"
	"gpustack.ai/gpustack/pkg/worker/kvcache/mooncake"
)

// surveyPod builds a member Pod carrying one survey reading.
func surveyPod(message string) *core.Pod {
	return &core.Pod{
		Status: core.PodStatus{
			InitContainerStatuses: []core.ContainerStatus{
				{
					Name: mooncake.MemberLocalDiskSurveyContainerName,
					State: core.ContainerState{
						Terminated: &core.ContainerStateTerminated{Message: message},
					},
				},
			},
		},
	}
}

// TestTierSurveyReading covers what a survey is allowed to conclude.
//
// The cases that matter are the ones where the survey FAILED, because every one of them yields an
// empty or missing entry count and "the tier is empty" is the answer a caller is looking for. Each
// is pinned separately rather than folded into one "bad input" case: they arrive through different
// failures and a reader has to see that none of them is believed.
func TestTierSurveyReading(t *testing.T) {
	for _, tc := range []struct {
		name    string
		pod     *core.Pod
		entries int
		ok      bool
	}{
		{
			name:    "a directory with content is reported",
			pod:     surveyPod("entries=7 control=21"),
			entries: 7,
			ok:      true,
		},
		{
			name:    "an empty directory is reported, because the control says the survey ran",
			pod:     surveyPod("entries=0 control=21"),
			entries: 0,
			ok:      true,
		},
		{
			name: "a zero control is the survey having failed, not an empty tier",
			// The root of a running container is never empty, so control=0 can only mean the
			// survey did not read anything. Believed, it would report every failure as a clean
			// directory - which is exactly the value that suppresses the warning.
			pod: surveyPod("entries=0 control=0"),
			ok:  false,
		},
		{
			name: "a reading with no control at all is refused",
			pod:  surveyPod("entries=0"),
			ok:   false,
		},
		{
			// The control says the survey RAN, which is a different question from whether it could
			// read the tier. `ls` on a directory the container's user cannot list exits non-zero
			// with empty output, and `wc -l` turns that into the same 0 an empty directory gives.
			// The survey reports -1 for it so that the two are not one value.
			name: "an unreadable tier is refused even beside a healthy control",
			pod:  surveyPod("entries=-1 control=21"),
			ok:   false,
		},
		{
			name: "an empty count with a good control is refused rather than read as zero",
			pod:  surveyPod("entries= control=21"),
			ok:   false,
		},
		{
			name: "an empty message is refused",
			pod:  surveyPod(""),
			ok:   false,
		},
		{
			name: "a survey that has not terminated has said nothing yet",
			pod: &core.Pod{
				Status: core.PodStatus{
					InitContainerStatuses: []core.ContainerStatus{
						{
							Name:  mooncake.MemberLocalDiskSurveyContainerName,
							State: core.ContainerState{Running: &core.ContainerStateRunning{}},
						},
					},
				},
			},
			ok: false,
		},
		{
			name: "a Pod carrying no survey at all says nothing",
			pod: &core.Pod{
				Status: core.PodStatus{
					InitContainerStatuses: []core.ContainerStatus{
						{Name: "something-else"},
					},
				},
			},
			ok: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			entries, ok := kvCacheBackendTierSurveyReading(tc.pod)
			assert.Equal(t, tc.ok, ok)
			if tc.ok {
				assert.Equal(t, tc.entries, entries)
			}
		})
	}
}

// TestTierPathsOverlap covers which directories one rm would take with it.
func TestTierPathsOverlap(t *testing.T) {
	for _, tc := range []struct {
		name    string
		a, b    string
		overlap bool
	}{
		{name: "the same directory", a: "/mnt/tier", b: "/mnt/tier", overlap: true},
		{name: "nested below", a: "/mnt/tier", b: "/mnt/tier/inner", overlap: true},
		{name: "nested above", a: "/mnt/tier/inner", b: "/mnt/tier", overlap: true},
		{name: "siblings", a: "/mnt/tier-a", b: "/mnt/tier-b", overlap: false},
		{
			// The prefix test has to respect the separator: emptying /mnt/tier does not touch
			// /mnt/tiers, and a bare string prefix would say it does.
			name: "a sibling whose name extends the other",
			a:    "/mnt/tier", b: "/mnt/tiers", overlap: false,
		},
		{name: "unrelated", a: "/mnt/tier", b: "/var/lib/other", overlap: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.overlap, kvCacheBackendPathsOverlap(tc.a, tc.b))
		})
	}
}

// TestTierSharedWith covers which other backend counts as a live claim on the same directory.
//
// Skipping is the right answer when the other backend's data has to survive, and the wrong one when
// both administrators asked for the same directory to be emptied: there, each backend sees the
// other, each skips, and the content nobody wanted stays.
func TestTierSharedWith(t *testing.T) {
	other := func(name string, deleting, clean bool) workercore.KVCacheBackend {
		o := workercore.KVCacheBackend{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec: workercore.KVCacheBackendSpec{
				Connection: workercore.KVCacheBackendConnection{
					Managed: &workercore.KVCacheBackendManaged{
						Members: []workercore.KVCacheBackendMember{{
							LocalDisk: &workercore.KVCacheBackendMemberLocalDisk{
								Path: "/mnt/tier", CleanAfterDelete: clean,
							},
						}},
					},
				},
			},
		}
		if deleting {
			o.DeletionTimestamp = &meta.Time{Time: time.Unix(1, 0)}
		}
		return o
	}

	for _, tc := range []struct {
		name       string
		others     []workercore.KVCacheBackend
		wantHolder string
	}{
		{
			name:       "a live backend on the same path is a claim",
			others:     []workercore.KVCacheBackend{other("b", false, false)},
			wantHolder: "b",
		},
		{
			// Both asked for the directory to be emptied and both are going. Treating the other as
			// a live claim deadlocks them: each skips for the other and the content survives.
			name:   "a backend deleting with cleanAfterDelete is not a claim",
			others: []workercore.KVCacheBackend{other("b", true, true)},
		},
		{
			// The default means "keep my content". The object going away does not revoke that, so
			// emptying the shared directory here would destroy data whose owner asked to keep it.
			name:       "a backend deleting WITHOUT cleanAfterDelete still holds its data",
			others:     []workercore.KVCacheBackend{other("b", true, false)},
			wantHolder: "b",
		},
		{
			name:   "no other backend at all",
			others: nil,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := &workercore.KVCacheBackend{ObjectMeta: meta.ObjectMeta{Name: "a"}}
			got := kvCacheBackendTierSharedWith(kvcb,
				&workercore.KVCacheBackendList{Items: tc.others}, "/mnt/tier",
				&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}})
			assert.Equal(t, tc.wantHolder, got)
		})
	}
}

// TestTierNodeSelected covers which nodes a member group reaches.
func TestTierNodeSelected(t *testing.T) {
	labels := map[string]string{"disk": "nvme", "zone": "a"}
	for _, tc := range []struct {
		name     string
		selector map[string]string
		selected bool
	}{
		{
			// Every DaemonSet with no selector runs everywhere, so cleanup has to cover every node
			// too. Read as "selects nothing" this would silently skip the whole cluster.
			name:     "an empty selector takes every node",
			selector: map[string]string{},
			selected: true,
		},
		{name: "a nil selector takes every node", selector: nil, selected: true},
		{name: "one matching pair", selector: map[string]string{"disk": "nvme"}, selected: true},
		{name: "every pair matching", selector: labels, selected: true},
		{name: "a value that differs", selector: map[string]string{"disk": "hdd"}, selected: false},
		{name: "a key the node lacks", selector: map[string]string{"rack": "7"}, selected: false},
		{
			name:     "one pair of two matching is not a match",
			selector: map[string]string{"disk": "nvme", "rack": "7"},
			selected: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.selected, kvCacheBackendNodeSelected(tc.selector, labels))
		})
	}
}

// TestTierCleanable covers which backends have a tier to empty at all.
func TestTierCleanable(t *testing.T) {
	tier := func(path string, clean bool) *workercore.KVCacheBackendMemberLocalDisk {
		return &workercore.KVCacheBackendMemberLocalDisk{Path: path, CleanAfterDelete: clean}
	}
	backend := func(members ...workercore.KVCacheBackendMember) *workercore.KVCacheBackend {
		return &workercore.KVCacheBackend{
			Spec: workercore.KVCacheBackendSpec{
				Connection: workercore.KVCacheBackendConnection{
					Managed: &workercore.KVCacheBackendManaged{Members: members},
				},
			},
		}
	}

	for _, tc := range []struct {
		name     string
		kvcb     *workercore.KVCacheBackend
		wantPath string
	}{
		{
			name:     "a tier that asked to be emptied",
			kvcb:     backend(workercore.KVCacheBackendMember{LocalDisk: tier("/mnt/tier", true)}),
			wantPath: "/mnt/tier",
		},
		{
			// The default, and the behavior of every release before the switch existed.
			name: "a tier that did not ask keeps its content",
			kvcb: backend(workercore.KVCacheBackendMember{LocalDisk: tier("/mnt/tier", false)}),
		},
		{
			name: "a group with no tier",
			kvcb: backend(workercore.KVCacheBackendMember{}),
		},
		{
			// Nothing here can empty a path that is not named, and treating blank as the working
			// directory would empty whatever the cleanup Pod happened to start in.
			name: "a tier asking to be emptied with no path",
			kvcb: backend(workercore.KVCacheBackendMember{LocalDisk: tier("   ", true)}),
		},
		{
			name: "an external backend renders no member at all",
			kvcb: &workercore.KVCacheBackend{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			member := kvCacheBackendCleanableTier(tc.kvcb)
			if tc.wantPath == "" {
				assert.Nil(t, member)
				return
			}
			assert.NotNil(t, member)
			assert.Equal(t, tc.wantPath, member.LocalDisk.Path)
		})
	}
}

// TestTierCleanupPodCarriesNoFinalizer pins the absence that keeps a cleanup Pod deletable.
//
// The repository locks objects whose disappearance it needs to notice. This Pod is the opposite: it
// lives for one teardown, and the object that would release it is the backend, which is deleted
// moments later. Locked, it outlives its owner as a Pod nobody can delete — measured, before this
// was pinned: the Pods sat with a deletionTimestamp and the finalizer still on them, permanently.
func TestTierCleanupPodCarriesNoFinalizer(t *testing.T) {
	pod := kvCacheBackendTierCleanupPod(
		&workercore.KVCacheBackend{},
		ctrlcli.ObjectKey{Name: "b-tier-clean-x", Namespace: "gpustack-system"},
		"mooncake:v0.3.13", "/mnt/tier",
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}},
	)

	assert.Empty(t, pod.Finalizers,
		"a cleanup Pod outlives the object that could release it, so a finalizer strands it")
	assert.Equal(t, "node-a", pod.Spec.NodeName,
		"an explicit nodeName is what reaches a cordoned node and fails fast on one that is gone")
	assert.Equal(t, core.RestartPolicyOnFailure, pod.Spec.RestartPolicy,
		"the kubelet has to retry in place: a Pod deleted and recreated to retry resets the "+
			"creation timestamp the give-up deadline is measured from, and the deadline never fires")
}

// TestTierCleanupPodIsCollectableWithoutThisController pins what removes the Pod when the teardown
// does not get to.
//
// The teardown deletes these Pods on the path it completes, but that is not the only path: a
// force-deleted backend, a stripped finalizer or a controller stopped between creating a Pod and
// collecting it would otherwise leave them in the operator's namespace forever. The ownerReference
// hands that job to the API server's garbage collector -- and must not carry blockOwnerDeletion,
// which would let the Pod hold its own owner's deletion open.
func TestTierCleanupPodIsCollectableWithoutThisController(t *testing.T) {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "store", UID: "11111111-2222-3333-4444-555555555555"},
	}
	pod := kvCacheBackendTierCleanupPod(kvcb,
		ctrlcli.ObjectKey{Name: "store-tier-clean-x", Namespace: "gpustack-system"},
		"mooncake:v0.3.13", "/mnt/tier",
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}},
	)

	if assert.Len(t, pod.OwnerReferences, 1) {
		owner := pod.OwnerReferences[0]
		assert.Equal(t, "KVCacheBackend", owner.Kind)
		assert.Equal(t, kvcb.UID, owner.UID)
		assert.Nil(t, owner.BlockOwnerDeletion,
			"a Pod that can block its owner's deletion is the failure this feature exists to prevent")
	}
	assert.Equal(t, string(kvcb.UID), pod.Labels[kvCacheBackendTierCleanupUIDLabel],
		"a reused backend name must not inherit the previous incarnation's Pods, which are the clock")
}

// TestTierCleanupPodNameFitsTheAPIServer pins the bound a refused name would wedge a teardown on.
//
// A Pod name validates as a DNS subdomain, so 253 -- measured against a live API server, which
// accepted 253 and refused 254. Both names this one is built from are DNS subdomains too, so either
// can reach 253 alone. A name over the bound is refused on Create, the teardown fails on every pass,
// and the finalizer leaves a backend nobody can delete.
func TestTierCleanupPodNameFitsTheAPIServer(t *testing.T) {
	long := strings.Repeat("a", 253)
	for _, tc := range []struct {
		name           string
		backend, node  string
		wantPrefixedBy string
	}{
		{
			name:    "ordinary names keep the backend name readable",
			backend: "store", node: "node-a",
			wantPrefixedBy: "store-tier-clean-",
		},
		{
			name:    "a node name using the whole bound does not push the Pod name over it",
			backend: "store", node: long,
			wantPrefixedBy: "store-tier-clean-",
		},
		{
			name:    "a backend name using the whole bound gives up the readable prefix",
			backend: long, node: "node-a",
			wantPrefixedBy: "kvcache-tier-clean-",
		},
		{
			// The cut a truncating implementation would make lands on the dot and leaves a segment
			// starting with "-", which the API server refuses for a different reason than length.
			name:    "a long backend name with a dot at the cut is still a valid name",
			backend: strings.Repeat("a", 225) + "." + strings.Repeat("b", 27), node: "node-a",
			wantPrefixedBy: "kvcache-tier-clean-",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := kvCacheBackendTierCleanupPodName(
				&workercore.KVCacheBackend{ObjectMeta: meta.ObjectMeta{Name: tc.backend}},
				&core.Node{ObjectMeta: meta.ObjectMeta{Name: tc.node}},
			)

			assert.LessOrEqual(t, len(got), kvCacheBackendTierCleanupPodNameLimit,
				"a name over the bound is refused on Create and the teardown never completes")
			assert.Empty(t, validation.IsDNS1123Subdomain(got),
				"the API server validates a Pod name as a DNS subdomain, length aside")
			assert.True(t, strings.HasPrefix(got, tc.wantPrefixedBy), got)
		})
	}
}

// TestTierCleanupPodNameSeparatesItsIdentities pins that the hash alone cannot confuse two Pods.
//
// Once the backend name is long enough the readable prefix is given up, and from there the hash is
// the ONLY thing telling two Pods apart. The hash writes its arguments end to end, so without a
// separator a backend/node pair and a re-split of the same characters sum the same: two different
// nodes would share one cleanup Pod, and whichever the Pod did not run on would be reported as
// finished having never been touched.
//
// Both names here are past the length where the prefix is dropped, on purpose. With short names the
// prefix distinguishes them whatever the hash does, and the assertion would hold with no separator
// at all -- passing while testing nothing.
func TestTierCleanupPodNameSeparatesItsIdentities(t *testing.T) {
	name := func(backend, node string) string {
		return kvCacheBackendTierCleanupPodName(
			&workercore.KVCacheBackend{ObjectMeta: meta.ObjectMeta{Name: backend}},
			&core.Node{ObjectMeta: meta.ObjectMeta{Name: node}},
		)
	}
	long := strings.Repeat("a", 230)

	require.False(t, strings.HasPrefix(name(long+"b", "c"), long),
		"the case is only meaningful past the length where the readable prefix is given up")
	assert.NotEqual(t, name(long+"b", "c"), name(long, "bc"),
		"a re-split of the same characters must not name the same Pod")
	assert.NotEqual(t, name("store", "node-a"), name("store", "node-b"),
		"two nodes of one backend each need their own Pod")
}

// TestTierReuseVerdictNeedsFullCoverage covers what evidence each verdict is allowed to rest on.
//
// The condition is written ONCE and never revisited, because the fact it records -- whether the
// directory was empty before anything wrote to it -- stops being observable after the first write.
// That makes a premature verdict permanent, and the two verdicts do not need the same evidence: one
// node holding content settles "not empty" on its own, while "was empty" generalises over every node
// and so cannot be decided while one of them has not answered.
func TestTierReuseVerdictNeedsFullCoverage(t *testing.T) {
	backendBorn := meta.NewTime(time.Unix(1_700_000_000, 0))
	// Within the window a Pod is the backend's first generation; past it, a replacement whose survey
	// describes a tier this backend has itself filled.
	pod := func(node, message string, after time.Duration) *core.Pod {
		p := &core.Pod{
			ObjectMeta: meta.ObjectMeta{
				Name:              "member-" + node,
				Namespace:         kuberess.SystemNamespaceName,
				CreationTimestamp: meta.NewTime(backendBorn.Add(after)),
				Labels: mooncake.MemberSelectorLabels(
					&workercore.KVCacheBackend{ObjectMeta: meta.ObjectMeta{Name: "store"}}, 0),
			},
			Spec: core.PodSpec{NodeName: node},
		}
		if message != "" {
			p.Status = surveyPod(message).Status
		}
		return p
	}
	memberPod := func(node, message string) *core.Pod { return pod(node, message, time.Minute) }
	replacementPod := func(node, message string) *core.Pod {
		return pod(node, message, kvCacheBackendTierSurveyWindow+time.Minute)
	}

	for _, tc := range []struct {
		name       string
		pods       []*core.Pod
		nodes      int32
		wantStatus meta.ConditionStatus
		wantAbsent bool
	}{
		{
			name:       "every node answered and every one was empty",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21"), memberPod("b", "entries=0 control=21")},
			wantStatus: meta.ConditionTrue,
		},
		{
			// The case this test exists for. Believing the one reading in hand would freeze
			// "empty on all 1 node(s)" while node b has not looked yet.
			name:       "one node has not answered yet, so empty is not decided",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21"), memberPod("b", "")},
			wantAbsent: true,
		},
		{
			// Asymmetric on purpose: content on one node is content, whatever b reports later.
			name:       "content on one node settles it even while another is silent",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=7 control=21"), memberPod("b", "")},
			wantStatus: meta.ConditionFalse,
		},
		{
			// A failed survey reads as control=0 and is refused, so it is a node that has not
			// answered rather than a node that reported an empty directory.
			name:       "a survey that failed is not a node reporting an empty tier",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21"), memberPod("b", "entries=0 control=0")},
			wantAbsent: true,
		},
		{
			name:       "no member Pod has reported at all",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", ""), memberPod("b", "")},
			wantAbsent: true,
		},
		{
			// The false accusation this guards. The condition was never written because b never
			// answered; a is then recreated and surveys a tier THIS backend has since filled, and
			// its entries would be recorded as a previous backend's leftovers.
			name:       "a replacement Pod reporting content is not evidence of reuse",
			nodes:      2,
			pods:       []*core.Pod{replacementPod("a", "entries=7 control=21"), memberPod("b", "")},
			wantAbsent: true,
		},
		{
			// A replaced Pod is not evidence, and the denominator is NODES, so its node has simply
			// not answered. "Empty on every node" cannot be claimed over a node whose only reading
			// came from a Pod that started after this backend had been writing.
			name:       "a node whose only reading came from a replacement has not answered",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21"), replacementPod("b", "entries=9 control=21")},
			wantAbsent: true,
		},
		{
			// What the denominator buys: one node reporting empty is not every node reporting
			// empty, while a second node's Pod has not been created at all.
			name:       "a node whose Pod does not exist yet has not answered either",
			nodes:      2,
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21")},
			wantAbsent: true,
		},
		{
			name:       "one node, and it answered",
			pods:       []*core.Pod{memberPod("a", "entries=0 control=21")},
			nodes:      1,
			wantStatus: meta.ConditionTrue,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kvcb := &workercore.KVCacheBackend{
				ObjectMeta: meta.ObjectMeta{Name: "store", CreationTimestamp: backendBorn},
				Spec: workercore.KVCacheBackendSpec{
					Connection: workercore.KVCacheBackendConnection{
						Managed: &workercore.KVCacheBackendManaged{
							Members: []workercore.KVCacheBackendMember{
								{LocalDisk: &workercore.KVCacheBackendMemberLocalDisk{Path: "/mnt/tier"}},
							},
						},
					},
				},
			}

			// The member DaemonSet is the denominator: its desired count is how many nodes carry
			// the tier, which is the figure "every node has reported" is measured against.
			objs := []ctrlcli.Object{&apps.DaemonSet{
				ObjectMeta: meta.ObjectMeta{
					Name:      mooncake.MemberObjectName(kvcb, 0),
					Namespace: kuberess.SystemNamespaceName,
				},
				Status: apps.DaemonSetStatus{DesiredNumberScheduled: tc.nodes},
			}}
			for _, pod := range tc.pods {
				objs = append(objs, pod)
			}
			r := &KVCacheBackendReconciler{
				Client: ctrlfake.NewClientBuilder().
					WithScheme(scheme.Scheme).
					WithObjects(objs...).
					Build(),
				Recorder: record.NewFakeRecorder(10),
			}

			holder := kvcb.DeepCopy()
			r.reportKVCacheBackendTierReuse(context.Background(), kvcb, holder)

			if tc.wantAbsent {
				assert.False(t, KVCacheBackendConditionTierWasEmpty.Exists(holder),
					"an undecided question has to stay undecided: the condition is written once")
				return
			}
			assert.Equal(t, string(tc.wantStatus),
				KVCacheBackendConditionTierWasEmpty.GetStatus(holder),
				KVCacheBackendConditionTierWasEmpty.GetMessage(holder))
		})
	}
}

// TestTierCleanupPodFailsWhenContentRemains pins that the Pod's verdict is a test and not a report.
//
// An earlier revision ended the script on `ls -A /tier | wc -l`. That exits zero whatever it counts
// and whatever `ls` did, so a partial removal reached PodSucceeded and the node was recorded as
// cleaned — the silent give-up this feature is written to make impossible. The assertion is on the
// shape of the last command, because that is what becomes the Pod's exit code.
func TestTierCleanupPodFailsWhenContentRemains(t *testing.T) {
	pod := kvCacheBackendTierCleanupPod(
		&workercore.KVCacheBackend{},
		ctrlcli.ObjectKey{Name: "b", Namespace: "gpustack-system"},
		"mooncake:v0.3.13", "/mnt/tier",
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}},
	)
	script := pod.Spec.Containers[0].Command[2]

	assert.Contains(t, script, `[ "$(ls -A /tier | wc -l)" -eq 0 ]`,
		"the last command has to be a test, so remaining content becomes a non-zero exit")
	assert.Contains(t, script, `ls -A /tier >/dev/null 2>&1;`,
		"the count cannot fail on a directory it cannot read -- `ls` writes to stderr and `wc -l` "+
			"prints 0 -- so readability is probed where set -e can act on its status")
	assert.NotContains(t, script, "|| true",
		"rm must not be made unconditionally successful: -f already ignores unmatched globs")
	assert.NotContains(t, script, "2>/dev/null",
		"hiding rm's stderr hides the reason a node kept its content; the readability probe "+
			"discards its own output with `>/dev/null 2>&1`, which is not this")
}

// TestTierCleanupPodCarriesPullSecrets pins the credentials the cleanup needs to start at all.
//
// Without them a private store image leaves the Pod Pending until the deadline, and the node is then
// reported as abandoned for a reason that has nothing to do with the node.
func TestTierCleanupPodCarriesPullSecrets(t *testing.T) {
	kvcb := &workercore.KVCacheBackend{
		Spec: workercore.KVCacheBackendSpec{
			ImagePullSecrets: []core.LocalObjectReference{{Name: "registry-cred"}},
		},
	}
	pod := kvCacheBackendTierCleanupPod(kvcb,
		ctrlcli.ObjectKey{Name: "b", Namespace: "gpustack-system"},
		"private/mooncake:v0.3.13", "/mnt/tier",
		&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}},
	)

	assert.Equal(t, kvcb.Spec.ImagePullSecrets, pod.Spec.ImagePullSecrets)
}

// TestMemberPodStuckReadsInitContainers pins the fault list this change made incomplete.
//
// Members had no init containers before the disk tier survey, so reading only ContainerStatuses was
// a complete account of why a member was not running. It is not any more: a Pod stuck in init never
// reaches its other containers, and their statuses stay empty, so an image that cannot run the
// survey reported a member that was not ready with no reason attached.
func TestMemberPodStuckReadsInitContainers(t *testing.T) {
	initStatus := func(state core.ContainerState, restarts int32,
		last core.ContainerState,
	) core.ContainerStatus {
		return core.ContainerStatus{
			Name:                 mooncake.MemberLocalDiskSurveyContainerName,
			State:                state,
			RestartCount:         restarts,
			LastTerminationState: last,
		}
	}

	for _, tc := range []struct {
		name       string
		pod        *core.Pod
		wantStuck  bool
		wantReason string
	}{
		{
			// The case this exists for: no shell in the image, so the survey cannot start.
			name: "a survey that cannot start is a reported fault",
			pod: &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "member-a"},
				Status: core.PodStatus{InitContainerStatuses: []core.ContainerStatus{
					initStatus(core.ContainerState{Waiting: &core.ContainerStateWaiting{
						Reason: "CreateContainerError", Message: `exec: "sh": not found`,
					}}, 0, core.ContainerState{}),
				}},
			},
			wantStuck: true, wantReason: "CreateContainerError",
		},
		{
			name: "an init container looping is reported with its exit code",
			pod: &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "member-a"},
				Status: core.PodStatus{InitContainerStatuses: []core.ContainerStatus{
					initStatus(core.ContainerState{}, 3, core.ContainerState{
						Terminated: &core.ContainerStateTerminated{ExitCode: 127},
					}),
				}},
			},
			wantStuck: true, wantReason: "MemberInitCrashLooping",
		},
		{
			// PodInitializing is progress, not a fault, and it is the ordinary state of every
			// member Pod for the moment the survey takes.
			name: "a survey still running is not a fault",
			pod: &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "member-a"},
				Status: core.PodStatus{InitContainerStatuses: []core.ContainerStatus{
					initStatus(core.ContainerState{Running: &core.ContainerStateRunning{}}, 0,
						core.ContainerState{}),
				}},
			},
		},
		{
			name: "a finished survey says nothing about the member",
			pod: &core.Pod{
				ObjectMeta: meta.ObjectMeta{Name: "member-a"},
				Status: core.PodStatus{InitContainerStatuses: []core.ContainerStatus{
					initStatus(core.ContainerState{Terminated: &core.ContainerStateTerminated{}}, 0,
						core.ContainerState{}),
				}},
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fault, stuck := memberPodStuck(tc.pod)
			assert.Equal(t, tc.wantStuck, stuck)
			if tc.wantStuck {
				assert.Equal(t, tc.wantReason, fault.reason)
				assert.Contains(t, fault.detail, mooncake.MemberLocalDiskSurveyContainerName,
					"the detail has to name which init container stopped the Pod")
			}
		})
	}
}

// TestTierEventsSurviveANilRecorder pins that a reconciler built without a recorder degrades to
// silence instead of panicking.
//
// The recorder is assigned in SetupController and nowhere else, and this suite's own reconcile
// helpers construct the reconciler with a client and nothing else. Measured before the guard: a nil
// pointer dereference on the shared-path branch, from a fixture that merely declared a tier.
func TestTierEventsSurviveANilRecorder(t *testing.T) {
	tier := func(clean bool) *workercore.KVCacheBackendMemberLocalDisk {
		return &workercore.KVCacheBackendMemberLocalDisk{Path: "/mnt/tier", CleanAfterDelete: clean}
	}
	backend := func(name string, clean bool) *workercore.KVCacheBackend {
		return &workercore.KVCacheBackend{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec: workercore.KVCacheBackendSpec{
				Image: "mooncake:v0.3.13",
				Connection: workercore.KVCacheBackendConnection{
					Managed: &workercore.KVCacheBackendManaged{
						Members: []workercore.KVCacheBackendMember{{LocalDisk: tier(clean)}},
					},
				},
			},
		}
	}

	// Two backends on one path, so the shared-path branch is the one that runs: it is the branch
	// that dereferenced the recorder first.
	kvcb := backend("store", true)
	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(
			kvcb, backend("other", false),
			&core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}},
		).Build(),
	}

	require.NotPanics(t, func() {
		_, err := r.cleanKVCacheBackendTier(context.Background(), kvcb)
		assert.NoError(t, err)
	}, "a reconciler with no recorder must not take the teardown down with it")
}

// TestTierCleanupPodOfAnotherIncarnationIsNotBelieved pins that the UID guards the Pod this pass
// reads, not only the ones it lists.
//
// The Pod's name is derived from the backend name and the node name, so it repeats across
// incarnations. Listing filters on the UID label; this Get does not, so a Pod left by a previous
// backend of the same name would be adopted: Succeeded, it reports a node cleaned that this backend
// never touched; anything else, it hands the new backend a deadline that has already run out.
func TestTierCleanupPodOfAnotherIncarnationIsNotBelieved(t *testing.T) {
	kvcb := &workercore.KVCacheBackend{
		ObjectMeta: meta.ObjectMeta{Name: "store", UID: "22222222-3333-4444-5555-666666666666"},
		Spec: workercore.KVCacheBackendSpec{
			Image: "mooncake:v0.3.13",
			Connection: workercore.KVCacheBackendConnection{
				Managed: &workercore.KVCacheBackendManaged{
					Members: []workercore.KVCacheBackendMember{{
						LocalDisk: &workercore.KVCacheBackendMemberLocalDisk{
							Path: "/mnt/tier", CleanAfterDelete: true,
						},
					}},
				},
			},
		},
	}
	node := &core.Node{ObjectMeta: meta.ObjectMeta{Name: "node-a"}}
	member := &kvcb.Spec.Connection.Managed.Members[0]

	stale := kvCacheBackendTierCleanupPod(kvcb,
		ctrlcli.ObjectKey{
			Name:      kvCacheBackendTierCleanupPodName(kvcb, node),
			Namespace: kuberess.SystemNamespaceName,
		},
		"mooncake:v0.3.13", "/mnt/tier", node)
	stale.Labels[kvCacheBackendTierCleanupUIDLabel] = "11111111-1111-1111-1111-111111111111"
	stale.Status.Phase = core.PodSucceeded

	r := &KVCacheBackendReconciler{
		Client: ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
			WithObjects(kvcb, node, stale).Build(),
	}

	finished, _, err := r.ensureKVCacheBackendTierCleanupPod(
		context.Background(), kvcb, member, "/mnt/tier", node)
	require.NoError(t, err)
	assert.False(t, finished,
		"a Succeeded Pod from another incarnation is not this backend's node being cleaned")

	got := new(core.Pod)
	err = r.Client.Get(context.Background(), ctrlcli.ObjectKey{
		Name: stale.Name, Namespace: stale.Namespace,
	}, got)
	assert.True(t, kerrors.IsNotFound(err),
		"the stale Pod has to go, or the next pass reads it again")
}
