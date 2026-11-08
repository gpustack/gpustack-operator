package worker

import (
	"context"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
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
