package worker

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrlinterceptor "sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	kueue "sigs.k8s.io/kueue/apis/kueue/v1beta2"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// A failed read of the server is an error to the caller, not a wait reason. A wait reason ends the
// pass without a requeue, and a read that failed changes nothing in the cluster, so no event would
// wake the slot again.
func TestReplacementReads_ServerReadFailureIsAnErrorNotAWaitReason(t *testing.T) {
	errRead := errors.New("api server unavailable")

	testCases := []struct {
		name  string
		fails func(list ctrlcli.ObjectList) bool
	}{
		{name: "listing the members fails", fails: func(list ctrlcli.ObjectList) bool {
			_, ok := list.(*core.PodList)

			return ok
		}},
		{name: "listing the workloads fails", fails: func(list ctrlcli.ObjectList) bool {
			_, ok := list.(*kueue.WorkloadList)

			return ok
		}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			reader := ctrlfake.NewClientBuilder().
				WithScheme(scheme.Scheme).
				WithInterceptorFuncs(ctrlinterceptor.Funcs{
					List: func(
						ctx context.Context, c ctrlcli.WithWatch, list ctrlcli.ObjectList,
						opts ...ctrlcli.ListOption,
					) error {
						if tc.fails(list) {
							return errRead
						}

						return c.List(ctx, list, opts...)
					},
				}).
				Build()
			r := &ModelDeploymentReconciler{Client: reader, APIReader: reader}
			md := newRenderDeployment()
			slot := modelDeploymentReplacementSlot{Role: "server", Ordinal: 0}

			vacant, _, err := r.replacementVacant(context.Background(), md, slot)
			assert.False(t, vacant)
			require.ErrorIs(t, err, errRead)
		})
	}

	t.Run("admission read of the members fails", func(t *testing.T) {
		reader := ctrlfake.NewClientBuilder().
			WithScheme(scheme.Scheme).
			WithInterceptorFuncs(ctrlinterceptor.Funcs{
				List: func(context.Context, ctrlcli.WithWatch, ctrlcli.ObjectList, ...ctrlcli.ListOption) error {
					return errRead
				},
			}).
			Build()
		r := &ModelDeploymentReconciler{Client: reader, APIReader: reader}

		admitted, _, err := r.replacementAdmitted(context.Background(), newRenderDeployment(),
			modelDeploymentReplacementSlot{Role: "server", Ordinal: 0}, nil)
		assert.False(t, admitted)
		require.ErrorIs(t, err, errRead)
	})
}
