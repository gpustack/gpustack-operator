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
	"gpustack.ai/gpustack/pkg/systemname"
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

func TestCohortReconciler_Reconcile(t *testing.T) {
	// The cohort name is derived purely from the general profile (os/arch/CPU),
	// not the node name, so a probe node yields the same name every node uses.
	cohortName := nodefeature.ExtractNodeProfiles(newGeneralNode("probe"))[0].Cohort

	cases := []struct {
		name string

		withNode         bool
		nodeUnmanaged    bool // node present but gpustack.ai/managed=false
		withCohort       bool
		withClusterQueue bool

		wantExists bool
	}{
		{
			// A Node references the cohort → it must be created.
			name:       "node present creates cohort",
			withNode:   true,
			wantExists: true,
		},
		{
			// No Node references the cohort, but a ClusterQueue still does (e.g. it
			// is draining). The cohort must be kept — deleting it would
			// cascade-delete the CQ via the ownerRef and disrupt running workloads.
			name:             "no node but ClusterQueue keeps cohort",
			withCohort:       true,
			withClusterQueue: true,
			wantExists:       true,
		},
		{
			// Fully idle: neither a Node nor a ClusterQueue references the cohort.
			name:       "no node no ClusterQueue deletes cohort",
			withCohort: true,
		},
		{
			// A Node exists but is no longer managed (gpustack.ai/managed=false), so
			// indexNodeByCohortProfile excludes it. With no ClusterQueue either, the
			// cohort is idle and must be deleted. Guards the index's managed filter
			// (the path a node leaving management relies on); does NOT exercise the
			// Node-watch predicate.
			name:          "unmanaged node deletes cohort",
			withNode:      true,
			nodeUnmanaged: true,
			withCohort:    true,
		},
	}

	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var objs []ctrlcli.Object
			if c.withNode {
				nd := newGeneralNode("node-1")
				if c.nodeUnmanaged {
					nd.Labels[systemname.ManagedLabelKey] = "false"
				}
				objs = append(objs, nd)
			}
			if c.withCohort {
				objs = append(objs, &kueue.Cohort{ObjectMeta: meta.ObjectMeta{Name: cohortName}})
			}
			if c.withClusterQueue {
				objs = append(objs, newInstanceTypeClusterQueue(cohortName, cohortName))
			}
			cli := buildCohortClient(objs...)

			_, err := reconcileCohort(t, cli, cohortName)
			require.NoError(t, err)

			got := &kueue.Cohort{}
			err = cli.Get(context.Background(), ctrlcli.ObjectKey{Name: cohortName}, got)
			if c.wantExists {
				assert.NoError(t, err, "cohort must be kept/created")
				return
			}
			assert.True(t, kerrors.IsNotFound(err),
				"fully idle cohort (no node, no CQ) must be deleted, got err=%v", err)
		})
	}
}
