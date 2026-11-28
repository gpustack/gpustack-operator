package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrlrecord "k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// TestRecomputePinnedKeepsAdmittedTreesBeyondPrefetchMembership pins the
// source-file retention union: a published tree an admitted generation
// reads stays pinned on its node while the generation stands, whatever the
// prefetches' placement, artifact revisions, or existence say. Ordinary
// retention removes the pin once no admitted generation remains.
func TestRecomputePinnedKeepsAdmittedTreesBeyondPrefetchMembership(t *testing.T) {
	otherDigest := "sha256:" + strings.Repeat("2", 64)

	// admittedNode is node-1's report: the disk copy is Ready and one
	// admitted generation reads the tree of testDigest.
	admittedNode := func() *workercore.NodeModelStore {
		nms := testNodeModelStoreEntry("node-1", "Ready", hourAgo(1), []string{})
		nms.Status.ModelTransfer = testNodeModelStoreModelTransfer("node-1", "7", testDigest,
			workercore.NodeModelStoreModelTransferGenerationStateServing).Status.ModelTransfer

		return nms
	}
	// retargetedPrefetch is a prefetch whose placement moved to a pool no
	// node in the fixture matches.
	retargetedPrefetch := func() *workercore.ModelPrefetch {
		return testPrefetch("warm", func(pf *workercore.ModelPrefetch) {
			pf.Spec.Placement.NodeSelector.MatchLabels = map[string]string{"pool": "h200"}
		})
	}

	cases := []struct {
		name          string
		objs          []ctrlcli.Object
		reconcileName string
		wantPinned    []string
	}{
		{
			name:          "placement leaving the admitted node keeps its pin",
			objs:          []ctrlcli.Object{retargetedPrefetch(), admittedNode()},
			reconcileName: "warm",
			// The prefetch's placement moved to another pool: the admitted
			// generation on the left node outlives the placement edit.
			wantPinned: []string{testDigest},
		},
		{
			name: "an artifact revision change keeps the old tree pinned",
			objs: func() []ctrlcli.Object {
				artifact := testResolvedArtifactSized("model", 5<<30)
				artifact.Status.Resolved.ManifestDigest = otherDigest

				return []ctrlcli.Object{retargetedPrefetch(), artifact, admittedNode()}
			}(),
			reconcileName: "warm",
			// The artifact re-resolved to another revision: the tree the old
			// admitted generation reads keeps its pin.
			wantPinned: []string{testDigest},
		},
		{
			name: "deleting the last naming prefetch keeps the tree pinned",
			objs: func() []ctrlcli.Object {
				artifact := testResolvedArtifactSized("model-two", 5<<30)
				artifact.Status.Resolved.ManifestDigest = otherDigest
				pf := testPrefetch("other", nil)
				pf.Spec.ArtifactRef.Name = "model-two"

				return []ctrlcli.Object{pf, artifact, admittedNode()}
			}(),
			reconcileName: "other",
			// The last prefetch naming the digest is gone: the surviving
			// prefetch names another digest, and the admitted tree stays.
			wantPinned: []string{testDigest},
		},
		{
			name: "no admitted generation leaves the pin to ordinary retention",
			objs: []ctrlcli.Object{
				retargetedPrefetch(),
				testNodeModelStoreEntry("node-1", "Ready", hourAgo(1), []string{testDigest}),
			},
			reconcileName: "warm",
			// The generation's report is gone: the stale pin goes the
			// ordinary way, whatever once held it.
			wantPinned: []string{},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, cli := newTestModelPrefetchEnv(t, tc.objs...)
			reconcileModelPrefetch(t, r, "team-a", tc.reconcileName)

			nms := new(workercore.NodeModelStore)
			require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, nms))
			if len(tc.wantPinned) == 0 {
				assert.Empty(t, nms.Spec.Pinned,
					"the pinned set follows the admitted generations and ordinary retention")
			} else {
				assert.Equal(t, tc.wantPinned, nms.Spec.Pinned,
					"the pinned set follows the admitted generations and ordinary retention")
			}
		})
	}

	t.Run("zero prefetches still recompute pins when generations appear and exit", func(t *testing.T) {
		nms := admittedNode()
		r, cli := newTestModelPrefetchEnv(t, nms)

		reqs := r.enqueueAllPrefetches(context.Background(), nms)
		require.Len(t, reqs, 1, "a NodeModelStore change with zero prefetches still enqueues a recompute request")
		_, err := r.Reconcile(context.Background(), reqs[0])
		require.NoError(t, err)

		got := new(workercore.NodeModelStore)
		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
		assert.Equal(t, []string{testDigest}, got.Spec.Pinned,
			"an admitted generation pins its digest even when no ModelPrefetch exists")

		// Now the generation drains and leaves the node's status report.
		got.Status.ModelTransfer = nil
		require.NoError(t, cli.Update(context.Background(), got))
		reqs = r.enqueueAllPrefetches(context.Background(), got)
		require.Len(t, reqs, 1)
		_, err = r.Reconcile(context.Background(), reqs[0])
		require.NoError(t, err)

		require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got))
		assert.Empty(t, got.Spec.Pinned,
			"the pin is retired once the generation leaves the report, even with zero prefetches")
	})
}

func numberedDigest(prefix string, i int) string {
	return fmt.Sprintf("sha256:%s%056x", prefix, i)
}

func testNodeStoreKernelArtifactForPins(ns, name, digest, seedNode string) *workercore.ModelKernelArtifact {
	return &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{
			Namespace:  ns,
			Name:       name,
			UID:        types.UID("uid-" + ns + "-" + name),
			Generation: 1,
		},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				NodeStore: &workercore.ModelKernelArtifactNodeStoreSource{
					ManifestDigest:         digest,
					DispatchManifestDigest: "sha256:" + strings.Repeat("d", 64),
					SizeBytes:              4096,
					FileCount:              3,
					SeedNode:               seedNode,
					ForgerRef: workercore.ModelKernelArtifactForgerReference{
						Name: "forger-a",
						UID:  "uid-forger-a",
					},
				},
			},
			Compatibility: workercore.ModelKernelArtifactCompatibility{
				Engine:        "vLLM",
				EngineVersion: "0.8.0",
				Manufacturer:  "nvidia",
				Architecture:  "sm_90a",
				KernelKeys:    []string{"vllm/0.8.0/nvidia/sm_90a/rmsnorm/0123456789abcdef"},
			},
		},
	}
}

func testModelDeploymentPod(ns, mdName, roleName, podName, nodeName string, phase core.PodPhase) *core.Pod {
	return &core.Pod{
		ObjectMeta: meta.ObjectMeta{
			Namespace: ns,
			Name:      podName,
			Labels: map[string]string{
				modelDeploymentLabelKeyName:      modelDeploymentLabelValueName,
				modelDeploymentLabelKeyInstance:  mdName,
				modelDeploymentLabelKeyComponent: roleName,
			},
		},
		Spec:   core.PodSpec{NodeName: nodeName},
		Status: core.PodStatus{Phase: phase},
	}
}

func TestRecomputePinnedThreeVoiceUnionAndRollback(t *testing.T) {
	transferDigest := numberedDigest("11111111", 1)
	activeKernelDigest := numberedDigest("22222222", 1)
	prevKernelDigest := numberedDigest("33333333", 1)
	prefetchDigest := numberedDigest("44444444", 1)

	cases := []struct {
		name       string
		objs       []ctrlcli.Object
		wantByNode map[string][]string
	}{
		{
			name: "pins active nodeStore kernel artifact on seedNode and live deployment pod nodes",
			objs: []ctrlcli.Object{
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}},
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-2"}},
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-3"}},
				testNodeStoreKernelArtifactForPins("team-a", "gen-2", activeKernelDigest, "node-1"),
				&workercore.ModelDeployment{
					ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{
							{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-2"}},
							{Name: "prefill"},
						},
					},
				},
				testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-0", "node-2", core.PodRunning),
				// Dead pod on node-3 and pod of another role on node-3 must not pin node-3.
				testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-old", "node-3", core.PodSucceeded),
				testModelDeploymentPod("team-a", "qwen", "prefill", "qwen-prefill-0", "node-3", core.PodRunning),
			},
			wantByNode: map[string][]string{
				"node-1": {activeKernelDigest},
				"node-2": {activeKernelDigest},
				"node-3": nil,
			},
		},
		{
			name: "deleting kernel artifact releases its pins",
			objs: func() []ctrlcli.Object {
				deleting := testNodeStoreKernelArtifactForPins("team-a", "gen-1", activeKernelDigest, "node-1")
				now := meta.Now()
				deleting.DeletionTimestamp = &now
				deleting.Finalizers = []string{workercore.ModelKernelArtifactProtectionFinalizer}

				return []ctrlcli.Object{
					&workercore.NodeModelStore{
						ObjectMeta: meta.ObjectMeta{Name: "node-1"},
						Spec:       workercore.NodeModelStoreSpec{Pinned: []string{activeKernelDigest}},
					},
					deleting,
				}
			}(),
			wantByNode: map[string][]string{
				"node-1": nil,
			},
		},
		{
			name: "pins previousKernelArtifact on its seedNode and target deployment live pod nodes during rollout",
			objs: []ctrlcli.Object{
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}},
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-2"}},
				&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-3"}},
				testNodeStoreKernelArtifactForPins("team-a", "gen-1", prevKernelDigest, "node-3"),
				testNodeStoreKernelArtifactForPins("team-a", "gen-2", activeKernelDigest, "node-1"),
				&workercore.ModelDeployment{
					ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
					Spec: workercore.ModelDeploymentSpec{
						Roles: []workercore.ModelDeploymentRole{
							{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-2"}},
						},
					},
				},
				&workercore.ModelDeploymentForger{
					ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen-forger"},
					Spec: workercore.ModelDeploymentForgerSpec{
						TargetRef: core.LocalObjectReference{Name: "qwen"},
					},
					Status: workercore.ModelDeploymentForgerStatus{
						Roles: []workercore.ModelDeploymentForgerRoleStatus{
							{
								Name:                   "decode",
								Phase:                  workercore.ModelDeploymentForgerRolePhasePromoting,
								ActiveKernelArtifact:   "gen-2",
								PreviousKernelArtifact: "gen-1",
							},
						},
					},
				},
				testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-0", "node-2", core.PodRunning),
			},
			wantByNode: map[string][]string{
				"node-1": {activeKernelDigest},
				"node-2": {activeKernelDigest, prevKernelDigest},
				"node-3": {prevKernelDigest},
			},
		},
		{
			name: "unions ModelTransfer, ModelKernelArtifact, and ModelPrefetch sorted and deduplicated",
			objs: func() []ctrlcli.Object {
				nms := &workercore.NodeModelStore{
					ObjectMeta: meta.ObjectMeta{Name: "node-1"},
					Status: workercore.NodeModelStoreStatus{
						Models: []workercore.NodeModelStoreModel{
							{Digest: prefetchDigest, State: workercore.NodeModelStoreModelStateReady, LastUsedTime: hourAgo(1)},
						},
						ModelTransfer: testNodeModelStoreModelTransfer("node-1", "1", transferDigest,
							workercore.NodeModelStoreModelTransferGenerationStateServing).Status.ModelTransfer,
					},
				}
				artifact := testResolvedArtifact("model")
				artifact.Status.Resolved.ManifestDigest = prefetchDigest
				pf := testPrefetch("warm", func(p *workercore.ModelPrefetch) {
					p.Spec.Retention.Pinned = true
				})
				// Non-nodeStore kernel artifacts must not add pins.
				hfKernel := &workercore.ModelKernelArtifact{
					ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "hf-kernel"},
					Spec: workercore.ModelKernelArtifactSpec{
						Source: workercore.ModelKernelArtifactSource{
							HuggingFace: &workercore.ModelArtifactHubSource{
								Repository: "owner/kernels",
								Revision:   strings.Repeat("a", 40),
							},
						},
					},
				}

				return []ctrlcli.Object{
					testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
					nms,
					artifact,
					testPinnedBinding(),
					pf,
					testNodeStoreKernelArtifactForPins("team-a", "gen-1", activeKernelDigest, "node-1"),
					hfKernel,
				}
			}(),
			wantByNode: map[string][]string{
				"node-1": {transferDigest, activeKernelDigest, prefetchDigest},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, cli := newTestModelPrefetchEnv(t, tc.objs...)
			_, err := r.Reconcile(context.Background(), ctrl.Request{
				NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName},
			})
			require.NoError(t, err)

			for node, want := range tc.wantByNode {
				nms := new(workercore.NodeModelStore)
				require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: node}, nms))
				if len(want) == 0 {
					assert.Empty(t, nms.Spec.Pinned, "node %s", node)
				} else {
					assert.Equal(t, want, nms.Spec.Pinned, "node %s", node)
				}
			}
		})
	}
}

func TestRecomputePinnedPerNodeCapPriorityAndIsolation(t *testing.T) {
	// Node 1 has 250 Tier-1 (ModelTransfer), 10 Tier-2 (ModelKernelArtifact), and 5 Tier-3 (ModelPrefetch)
	// digests, plus 1 digest shared between Tier 1 and Tier 2.
	// Priority order: Tier 1 (250) > Tier 2 (6 new slots, lexicographic) > Tier 3 (0 slots).
	// Node 2 has 1 Tier-2 and 1 Tier-3 digest and must be completely unaffected by Node 1's cap.
	var objs []ctrlcli.Object

	sharedDigest := numberedDigest("aaaaaaaa", 0)
	gens := make([]workercore.NodeModelStoreModelTransferGeneration, 0, 250)
	gens = append(gens, workercore.NodeModelStoreModelTransferGeneration{
		Generation: "t-0",
		Digest:     sharedDigest,
		State:      workercore.NodeModelStoreModelTransferGenerationStateServing,
	})
	for i := 1; i < 250; i++ {
		gens = append(gens, workercore.NodeModelStoreModelTransferGeneration{
			Generation: fmt.Sprintf("t-%d", i),
			Digest:     numberedDigest("ffffffff", i),
			State:      workercore.NodeModelStoreModelTransferGenerationStateServing,
		})
	}

	node1Models := make([]workercore.NodeModelStoreModel, 0, 5)
	for i := 0; i < 5; i++ {
		d := numberedDigest("00000000", i) // lexicographically smallest, yet Tier 3 so dropped!
		node1Models = append(node1Models, workercore.NodeModelStoreModel{
			Digest:       d,
			State:        workercore.NodeModelStoreModelStateReady,
			LastUsedTime: hourAgo(1),
		})
		art := testResolvedArtifact(fmt.Sprintf("model-%d", i))
		art.Status.Resolved.ManifestDigest = d
		pf := testPrefetch(fmt.Sprintf("pf-%d", i), func(p *workercore.ModelPrefetch) {
			p.Spec.ArtifactRef.Name = art.Name
			p.Spec.Retention.Pinned = true
		})
		objs = append(objs, art, pf)
	}

	node2PrefetchDigest := numberedDigest("00000000", 0)
	node2KernelDigest := numberedDigest("55555555", 1)

	nms1 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Status: workercore.NodeModelStoreStatus{
			Models:        node1Models,
			ModelTransfer: &workercore.NodeModelStoreModelTransferStatus{Generations: gens},
		},
	}
	nms2 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-2"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{
				{Digest: node2PrefetchDigest, State: workercore.NodeModelStoreModelStateReady, LastUsedTime: hourAgo(1)},
			},
		},
	}

	objs = append(objs,
		testModelNodeLabeled("node-1", map[string]string{"pool": "h100"}),
		testModelNodeLabeled("node-2", map[string]string{"pool": "h100"}),
		testPinnedBinding(),
		nms1,
		nms2,
		// Shared digest in Tier 2 must not consume an extra slot or be treated as dropped.
		testNodeStoreKernelArtifactForPins("team-a", "k-shared", sharedDigest, "node-1"),
		// Node 2 kernel artifact unaffected by Node 1's cap.
		testNodeStoreKernelArtifactForPins("team-a", "k-node2", node2KernelDigest, "node-2"),
	)

	var admittedKernels, droppedKernels []string
	for i := 0; i < 10; i++ {
		d := numberedDigest("bbbbbbbb", i)
		objs = append(objs, testNodeStoreKernelArtifactForPins("team-a", fmt.Sprintf("k-%02d", i), d, "node-1"))
		if i < 6 {
			admittedKernels = append(admittedKernels, d)
		} else {
			droppedKernels = append(droppedKernels, d)
		}
	}

	r, cli := newTestModelPrefetchEnv(t, objs...)
	recorder := ctrlrecord.NewFakeRecorder(32)
	r.Recorder = recorder

	require.NoError(t, r.recomputePinned(context.Background()))

	got1 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	require.Len(t, got1.Spec.Pinned, nodeModelStoreMaxPinned)
	assert.Contains(t, got1.Spec.Pinned, sharedDigest)
	for _, g := range gens {
		assert.Contains(t, got1.Spec.Pinned, g.Digest, "all 250 Tier-1 digests must be admitted")
	}
	for _, d := range admittedKernels {
		assert.Contains(t, got1.Spec.Pinned, d, "lexicographically first 6 Tier-2 digests must be admitted")
	}
	for _, d := range droppedKernels {
		assert.NotContains(t, got1.Spec.Pinned, d, "excess Tier-2 digests beyond 256 must be dropped")
	}
	for _, m := range node1Models {
		assert.NotContains(t, got1.Spec.Pinned, m.Digest, "Tier-3 digests must be dropped before Tier-1 and Tier-2")
	}

	got2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Equal(t, []string{node2PrefetchDigest, node2KernelDigest}, got2.Spec.Pinned,
		"node-2 pin union must be isolated from node-1 cap saturation")

	var events []string
	for len(recorder.Events) > 0 {
		events = append(events, <-recorder.Events)
	}
	assert.Len(t, events, len(droppedKernels), "one PinnedCapacityExceeded warning event per dropped kernel artifact")
	for _, ev := range events {
		assert.Contains(t, ev, workercore.ModelKernelArtifactReasonPinnedCapacityExceeded)
	}
}

func TestRecomputePinnedPerNodeUpdateErrorIsolation(t *testing.T) {
	d1 := numberedDigest("11111111", 1)
	d2 := numberedDigest("22222222", 2)

	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).
		WithObjects(
			&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}},
			&workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-2"}},
			testNodeStoreKernelArtifactForPins("team-a", "k1", d1, "node-1"),
			testNodeStoreKernelArtifactForPins("team-a", "k2", d2, "node-2"),
		).
		WithInterceptorFuncs(interceptor.Funcs{
			Update: func(ctx context.Context, c ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				if nms, ok := obj.(*workercore.NodeModelStore); ok && nms.Name == "node-1" {
					return errors.New("injected node-1 update failure")
				}
				return c.Update(ctx, obj, opts...)
			},
		}).Build()

	r := &ModelPrefetchReconciler{Client: cli, Now: func() time.Time { return testNow }}
	err := r.recomputePinned(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "node-1")

	got2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Equal(t, []string{d2}, got2.Spec.Pinned,
		"an update failure on node-1 must never abort pin reconciliation for node-2")
}

func TestModelPrefetchPinnedWatchesAndPredicates(t *testing.T) {
	d1 := numberedDigest("11111111", 1)
	k1 := testNodeStoreKernelArtifactForPins("team-a", "gen-1", d1, "node-1")
	r, _ := newTestModelPrefetchEnv(t, k1)

	reqs := r.enqueuePinned(context.Background(), k1)
	require.Equal(t, []ctrl.Request{{NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName}}}, reqs)

	// ModelKernelArtifact predicate tracks nodeStore create/delete/deletionTimestamp/resolved changes.
	k1Copy := k1.DeepCopy()
	k1Resolved := k1.DeepCopy()
	k1Resolved.Status.Resolved = &workercore.ModelKernelArtifactResolved{ManifestDigest: d1}
	hfMKA := &workercore.ModelKernelArtifact{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "hf"},
		Spec: workercore.ModelKernelArtifactSpec{
			Source: workercore.ModelKernelArtifactSource{
				HuggingFace: &workercore.ModelArtifactHubSource{Repository: "owner/repo", Revision: strings.Repeat("a", 40)},
			},
		},
	}
	assert.True(t, modelKernelArtifactPinChanged.Create(ctrlevent.CreateEvent{Object: k1}))
	assert.False(t, modelKernelArtifactPinChanged.Create(ctrlevent.CreateEvent{Object: hfMKA}))
	assert.True(t, modelKernelArtifactPinChanged.Delete(ctrlevent.DeleteEvent{Object: k1}))
	assert.True(t, modelKernelArtifactPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: k1, ObjectNew: k1Resolved}))
	assert.False(t, modelKernelArtifactPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: k1, ObjectNew: k1Copy}))
	assert.False(t, modelKernelArtifactPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: hfMKA, ObjectNew: hfMKA.DeepCopy()}))
	assert.False(t, modelKernelArtifactPinChanged.Generic(ctrlevent.GenericEvent{Object: k1}))

	// ModelDeployment kernelArtifactRef predicate fires on ref changes and ignores unrelated changes.
	mdOld := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{{Name: "decode", Replicas: 1}},
		},
	}
	mdScale := mdOld.DeepCopy()
	mdScale.Spec.Roles[0].Replicas = 2
	assert.False(t, modelDeploymentKernelRefsChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdOld, ObjectNew: mdScale}))

	mdPromoted := mdOld.DeepCopy()
	mdPromoted.Spec.Roles[0].KernelArtifactRef = &core.LocalObjectReference{Name: "gen-1"}
	assert.True(t, modelDeploymentKernelRefsChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdOld, ObjectNew: mdPromoted}))
	assert.True(t, modelDeploymentKernelRefsChanged.Create(ctrlevent.CreateEvent{Object: mdPromoted}))
	assert.True(t, modelDeploymentKernelRefsChanged.Delete(ctrlevent.DeleteEvent{Object: mdPromoted}))
	assert.False(t, modelDeploymentKernelRefsChanged.Create(ctrlevent.CreateEvent{Object: mdOld}))
	assert.False(t, modelDeploymentKernelRefsChanged.Generic(ctrlevent.GenericEvent{Object: mdPromoted}))

	// ModelDeploymentForger rollback predicate fires when previousKernelArtifact changes.
	mdfOld := &workercore.ModelDeploymentForger{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "forger"},
		Status: workercore.ModelDeploymentForgerStatus{
			Roles: []workercore.ModelDeploymentForgerRoleStatus{{Name: "decode", Phase: workercore.ModelDeploymentForgerRolePhaseIdle}},
		},
	}
	mdfNew := mdfOld.DeepCopy()
	mdfNew.Status.Roles[0].PreviousKernelArtifact = "gen-1"
	assert.True(t, modelDeploymentForgerPinChanged.Create(ctrlevent.CreateEvent{Object: mdfNew}))
	assert.True(t, modelDeploymentForgerPinChanged.Delete(ctrlevent.DeleteEvent{Object: mdfNew}))
	assert.True(t, modelDeploymentForgerPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdfOld, ObjectNew: mdfNew}))
	assert.False(t, modelDeploymentForgerPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: mdfOld, ObjectNew: mdfOld.DeepCopy()}))
	assert.False(t, modelDeploymentForgerPinChanged.Generic(ctrlevent.GenericEvent{Object: mdfNew}))

	// ModelDeployment Pod predicate fires on scheduling onto a node and terminal phase transitions.
	podPending := testModelDeploymentPod("team-a", "qwen", "decode", "p-0", "", core.PodPending)
	podRunning := testModelDeploymentPod("team-a", "qwen", "decode", "p-0", "node-1", core.PodRunning)
	podDone := testModelDeploymentPod("team-a", "qwen", "decode", "p-0", "node-1", core.PodSucceeded)
	podUnrelated := &core.Pod{ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "other"}, Spec: core.PodSpec{NodeName: "node-1"}}
	assert.True(t, modelDeploymentPodPinChanged.Create(ctrlevent.CreateEvent{Object: podRunning}))
	assert.True(t, modelDeploymentPodPinChanged.Delete(ctrlevent.DeleteEvent{Object: podRunning}))
	assert.True(t, modelDeploymentPodPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: podPending, ObjectNew: podRunning}))
	assert.True(t, modelDeploymentPodPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: podRunning, ObjectNew: podDone}))
	assert.False(t, modelDeploymentPodPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: podRunning, ObjectNew: podRunning.DeepCopy()}))
	assert.False(t, modelDeploymentPodPinChanged.Update(ctrlevent.UpdateEvent{ObjectOld: podUnrelated, ObjectNew: podUnrelated.DeepCopy()}))
	assert.False(t, modelDeploymentPodPinChanged.Generic(ctrlevent.GenericEvent{Object: podRunning}))
	assert.Equal(t, []ctrl.Request{{NamespacedName: ctrlcli.ObjectKey{Name: nodeModelStorePinnedRequestName}}},
		r.enqueuePinnedForModelDeploymentPod(context.Background(), podRunning))

	// Pod in namespace without nodeStore kernel artifacts enqueues nothing.
	podOtherNS := testModelDeploymentPod("team-b", "qwen", "decode", "p-0", "node-1", core.PodRunning)
	assert.Empty(t, r.enqueuePinnedForModelDeploymentPod(context.Background(), podOtherNS))

	// selectNodePins edge cases: zero cap, Tier 1 saturation, and Tier 3 dedup with higher tiers.
	admitted, dropped := selectNodePins(
		map[string]struct{}{"b": {}, "a": {}},
		map[string]struct{}{"c": {}},
		map[string]struct{}{"a": {}, "d": {}},
		1,
	)
	assert.Equal(t, []string{"a"}, admitted)
	assert.Equal(t, []string{"c"}, dropped)

	admitted, dropped = selectNodePins(
		map[string]struct{}{"a": {}},
		map[string]struct{}{"b": {}},
		map[string]struct{}{"a": {}, "b": {}, "c": {}},
		3,
	)
	assert.Equal(t, []string{"a", "b", "c"}, admitted)
	assert.Empty(t, dropped)

	admitted, dropped = selectNodePins(nil, map[string]struct{}{"b": {}}, nil, 0)
	assert.Nil(t, admitted)
	assert.Equal(t, []string{"b"}, dropped)
}

func TestRecomputePinnedSeedNodeLifecycleAfterResolution(t *testing.T) {
	digest := numberedDigest("77777777", 1)

	// 1. Before initial resolution (Status.Resolved == nil): seedNode stays pinned even before its Ready report lands.
	mka := testNodeStoreKernelArtifactForPins("team-a", "gen-1", digest, "node-1")
	nms1 := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-1"}}
	nms2 := &workercore.NodeModelStore{ObjectMeta: meta.ObjectMeta{Name: "node-2"}}
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{
				{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-1"}},
			},
		},
	}
	podOnNode2 := testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-0", "node-2", core.PodRunning)

	r, cli := newTestModelPrefetchEnv(t, nms1, nms2, mka, md, podOnNode2)
	require.NoError(t, r.recomputePinned(context.Background()))

	got1 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Equal(t, []string{digest}, got1.Spec.Pinned, "awaiting initial resolution must protect seedNode")

	// 2. Resolved and seedNode reports Ready: seedNode pin stays held.
	now := meta.Now()
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mka), mka))
	mka.Status.Resolved = &workercore.ModelKernelArtifactResolved{
		ManifestDigest:         digest,
		DispatchManifestDigest: mka.Spec.Source.NodeStore.DispatchManifestDigest,
		FileCount:              3,
		SizeBytes:              4096,
		ResolvedTime:           now,
		LastValidatedTime:      &now,
		DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
	}
	require.NoError(t, cli.Update(context.Background(), mka))

	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	got1.Status.Models = []workercore.NodeModelStoreModel{{
		Digest: digest,
		State:  workercore.NodeModelStoreModelStateReady,
	}}
	require.NoError(t, cli.Update(context.Background(), got1))

	require.NoError(t, r.recomputePinned(context.Background()))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Equal(t, []string{digest}, got1.Spec.Pinned, "seedNode pin must be held while seedNode reports Ready")

	// 3. Seed node drops out of Ready after resolution (no live referencing Pod on node-1): seedNode pin is released, while live Pod node-2 keeps its pin!
	got1.Status.Models = nil
	require.NoError(t, cli.Update(context.Background(), got1))

	require.NoError(t, r.recomputePinned(context.Background()))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Empty(t, got1.Spec.Pinned, "seedNode pin must be released once seedNode drops out of Ready after resolution")

	got2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Equal(t, []string{digest}, got2.Spec.Pinned, "live referencing Pod node must retain its pin")

	// 4. If a live referencing Pod also runs on seedNode, seedNode stays pinned via the live Pod rule.
	podOnNode1 := testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-1", "node-1", core.PodRunning)
	require.NoError(t, cli.Create(context.Background(), podOnNode1))

	require.NoError(t, r.recomputePinned(context.Background()))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Equal(t, []string{digest}, got1.Spec.Pinned, "live referencing Pod on seedNode still requires a pin")
}

func TestRecomputePinnedQuarantinedKernelArtifactDropsSeedNodeKeepsLivePods(t *testing.T) {
	digest := numberedDigest("88888888", 1)
	now := meta.Now()

	mka := testNodeStoreKernelArtifactForPins("team-a", "gen-3", digest, "node-1")
	mka.Status.Resolved = &workercore.ModelKernelArtifactResolved{
		ManifestDigest:         digest,
		DispatchManifestDigest: mka.Spec.Source.NodeStore.DispatchManifestDigest,
		FileCount:              3,
		SizeBytes:              4096,
		ResolvedTime:           now,
		LastValidatedTime:      &now,
		DigestSource:           workercore.ModelKernelArtifactDigestSourceForge,
	}
	unquarantined := mka.DeepCopy()
	mka.Status.Quarantined = true
	mka.Status.QuarantineReason = "UnsupportedKernelArtifact"

	assert.True(t, modelKernelArtifactPinChanged.Update(ctrlevent.UpdateEvent{
		ObjectOld: unquarantined,
		ObjectNew: mka,
	}), "quarantining a nodeStore ModelKernelArtifact must trigger pin recomputation")

	nms1 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-1"},
		Spec:       workercore.NodeModelStoreSpec{Pinned: []string{digest}},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: digest,
				State:  workercore.NodeModelStoreModelStateReady,
			}},
		},
	}
	nms2 := &workercore.NodeModelStore{
		ObjectMeta: meta.ObjectMeta{Name: "node-2"},
		Status: workercore.NodeModelStoreStatus{
			Models: []workercore.NodeModelStoreModel{{
				Digest: digest,
				State:  workercore.NodeModelStoreModelStateReady,
			}},
		},
	}
	md := &workercore.ModelDeployment{
		ObjectMeta: meta.ObjectMeta{Namespace: "team-a", Name: "qwen"},
		Spec: workercore.ModelDeploymentSpec{
			Roles: []workercore.ModelDeploymentRole{
				{Name: "decode", KernelArtifactRef: &core.LocalObjectReference{Name: "gen-3"}},
			},
		},
	}
	rkRender := buildModelDeploymentKernelRender(mka, kernelArtifactDeliveryKind(mka))
	require.NotNil(t, rkRender)
	vols, _ := rkRender.volumes(false)
	canaryPod := testModelDeploymentPod("team-a", "qwen", "decode", "qwen-decode-0", "node-2", core.PodRunning)
	canaryPod.Spec.Volumes = vols

	r, cli := newTestModelPrefetchEnv(t, nms1, nms2, mka, md, canaryPod)

	// 1. Quarantined artifact immediately drops its seedNode (node-1) pin while keeping live Pod node-2 pinned.
	require.NoError(t, r.recomputePinned(context.Background()))

	got1 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Empty(t, got1.Spec.Pinned, "quarantined ModelKernelArtifact must not stay pinned on seedNode")

	got2 := new(workercore.NodeModelStore)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Equal(t, []string{digest}, got2.Spec.Pinned, "live Pod node must stay pinned until rollback replaces Pods")

	// 2. Rollback reverts ModelDeployment.spec.roles[].kernelArtifactRef to nil while canaryPod is still running on node-2:
	// node-2 remains pinned via the live Pod CSI volume until canaryPod rolls off.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(md), md))
	md.Spec.Roles[0].KernelArtifactRef = nil
	require.NoError(t, cli.Update(context.Background(), md))

	require.NoError(t, r.recomputePinned(context.Background()))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Equal(t, []string{digest}, got2.Spec.Pinned, "node hosting live Pod still mounting quarantined CSI volume must stay pinned")

	// 3. Once canaryPod rolls off (terminates), node-2 drops the pin too while the quarantined API record persists.
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(canaryPod), canaryPod))
	canaryPod.Status.Phase = core.PodSucceeded
	require.NoError(t, cli.Status().Update(context.Background(), canaryPod))

	require.NoError(t, r.recomputePinned(context.Background()))
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-1"}, got1))
	assert.Empty(t, got1.Spec.Pinned)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: "node-2"}, got2))
	assert.Empty(t, got2.Spec.Pinned, "all pins for quarantined artifact must release once live Pods roll off")

	persistedMKA := new(workercore.ModelKernelArtifact)
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKeyFromObject(mka), persistedMKA))
	assert.True(t, persistedMKA.Status.Quarantined, "quarantined ModelKernelArtifact record must persist in API")
}
