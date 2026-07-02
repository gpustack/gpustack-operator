package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// TestNodeQueueEntranceReconciler_CopiesNotes pins that a LocalQueue is created
// in a non-reserved Namespace carrying the descriptive notes (including the
// per-card VRAM) copied from its ClusterQueue and branded as a selector view; the
// unit spec is not part of that view.
func TestNodeQueueEntranceReconciler_CopiesNotes(t *testing.T) {
	const cqName = "gpustack-nvidia-ln-x64"
	const ns = "team-a"

	cq := &kueue.ClusterQueue{ObjectMeta: meta.ObjectMeta{Name: cqName}}
	systemmeta.NoteResource(cq, _ClusterQueueResType, map[string]string{
		"acceleratable": "true",
		"manufacturer":  "nvidia",
		"product":       "h100",
		"family":        "hopper",
		"memory":        "81920Mi",
		"unitCPU":       "8",
		"unitRAM":       "64",
		"localStorage":  "256",
	})
	namespace := &core.Namespace{ObjectMeta: meta.ObjectMeta{Name: ns}}

	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(cq, namespace).
		Build()

	r := &NodeQueueEntranceReconciler{Client: cli}
	_, err := r.Reconcile(context.Background(),
		ctrlreconcile.Request{NamespacedName: ctrlcli.ObjectKey{Name: cqName}})
	require.NoError(t, err)

	lq := new(kueue.LocalQueue)
	require.NoError(t, cli.Get(context.Background(),
		ctrlcli.ObjectKey{Namespace: ns, Name: nodefeature.FormatLocalQueueName(cqName)}, lq))

	assert.Equal(t, kueue.ClusterQueueReference(cqName), lq.Spec.ClusterQueue)

	resType, notes := systemmeta.DescribeResource(lq)
	assert.Equal(t, _NodeQueueEntranceResType, resType)
	assert.Equal(t, "true", notes["acceleratable"])
	assert.Equal(t, "nvidia", notes["manufacturer"])
	assert.Equal(t, "h100", notes["product"])
	assert.Equal(t, "hopper", notes["family"])
	assert.Equal(t, "81920Mi", notes["memory"])
	// The unit spec belongs to the queue, not the selector view it copies down.
	assert.NotContains(t, notes, "unitCPU")
	assert.NotContains(t, notes, "localStorage")
}
