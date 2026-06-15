package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/nodefeature"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// newInstanceTypeClusterQueue builds an "instancetypes" ClusterQueue that
// belongs to the given cohort, as the CohortReconciler's CQ index expects.
func newInstanceTypeClusterQueue(name, cohort string) *kueue.ClusterQueue {
	cq := &kueue.ClusterQueue{
		ObjectMeta: meta.ObjectMeta{Name: name},
		Spec: kueue.ClusterQueueSpec{
			CohortName: kueue.CohortReference(cohort),
		},
	}
	systemmeta.NoteResource(cq, "instancetypes", nil)
	return cq
}

func buildCohortClient(objs ...ctrlcli.Object) ctrlcli.Client {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithIndex(&core.Node{}, IndexingNodeByCohortProfile, indexNodeByCohortProfile).
		WithIndex(&kueue.ClusterQueue{}, IndexingClusterQueuesByCohortName, indexClusterQueueByCohortName).
		Build()
}

func reconcileCohort(t *testing.T, cli ctrlcli.Client, name string) (ctrl.Result, error) {
	t.Helper()
	r := &CohortReconciler{Client: cli}
	return r.Reconcile(context.Background(),
		ctrl.Request{NamespacedName: ctrlcli.ObjectKey{Name: name}})
}

func TestCohortReconciler_Reconcile_NodePresentCreatesCohort(t *testing.T) {
	node := newGeneralNode("node-1")
	cohortName := nodefeature.ExtractNodeProfiles(node)[0].Cohort

	cli := buildCohortClient(node)
	_, err := reconcileCohort(t, cli, cohortName)
	require.NoError(t, err)

	co := &kueue.Cohort{}
	require.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: cohortName}, co),
		"cohort must be created when a node references it")
}

func TestCohortReconciler_Reconcile_NoNodeButClusterQueueKeepsCohort(t *testing.T) {
	// No Node references the cohort, but a ClusterQueue still does (e.g. it is
	// draining). The cohort must be kept — deleting it would cascade-delete the
	// CQ via the ownerRef and disrupt running workloads.
	const cohortName = "gpustack--generic-ln-x64-4c-16g"
	co := &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: cohortName}}
	cq := newInstanceTypeClusterQueue("gpustack--generic-ln-x64-4c-16g", cohortName)

	cli := buildCohortClient(co, cq)
	_, err := reconcileCohort(t, cli, cohortName)
	require.NoError(t, err)

	got := &kueue.Cohort{}
	assert.NoError(t, cli.Get(context.Background(), ctrlcli.ObjectKey{Name: cohortName}, got),
		"cohort with a referencing ClusterQueue must be kept")
}

func TestCohortReconciler_Reconcile_NoNodeNoClusterQueueDeletesCohort(t *testing.T) {
	// Fully idle: neither a Node nor a ClusterQueue references the cohort.
	const cohortName = "gpustack--generic-ln-x64-4c-16g"
	co := &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: cohortName}}

	cli := buildCohortClient(co)
	_, err := reconcileCohort(t, cli, cohortName)
	require.NoError(t, err)

	got := &kueue.Cohort{}
	err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: cohortName}, got)
	assert.True(t, kerrors.IsNotFound(err),
		"fully idle cohort (no node, no CQ) must be deleted, got err=%v", err)
}
