package worker

import (
	"context"
	"fmt"
	"testing"

	"github.com/go-logr/logr"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime/schema"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// TestObjectWriteResult pins the contract of objectWriteResult: a conflict ends the reconcile with
// the caller's onConflict result, a deleted object ends it with nothing to do, both without an
// error and without an error log, and any other failure is returned and logged. A wrapped error is
// classified by what it wraps.
func TestObjectWriteResult(t *testing.T) {
	gr := schema.GroupResource{Resource: "objects"}
	conflict := apierrors.NewConflict(gr, "o", fmt.Errorf("the object has been modified"))
	testCases := []struct {
		name       string
		err        error
		onConflict ctrl.Result
		want       ctrl.Result
		wantErr    bool
		wantLogged int
	}{
		{
			name: "a conflict with an event to follow ends with nothing to do",
			err:  conflict,
		},
		{
			name:       "a conflict without an event to follow requeues",
			err:        conflict,
			onConflict: _requeueAfterConflict,
			want:       _requeueAfterConflict,
		},
		{
			name:       "a wrapped conflict is still a conflict",
			err:        fmt.Errorf("deactivate workload w: %w", conflict),
			onConflict: _requeueAfterConflict,
			want:       _requeueAfterConflict,
		},
		{
			name:       "a deleted object ends with nothing to do, whatever onConflict says",
			err:        apierrors.NewNotFound(gr, "o"),
			onConflict: _requeueAfterConflict,
		},
		{
			name:       "any other failure is returned and logged",
			err:        apierrors.NewInternalError(fmt.Errorf("etcd unavailable")),
			onConflict: _requeueAfterConflict,
			wantErr:    true,
			wantLogged: 1,
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var logged int
			res, err := objectWriteResult(logr.New(errorCountingSink{errors: &logged}), tc.err, "write", tc.onConflict)
			if tc.wantErr {
				assert.Error(t, err)
			} else {
				assert.NoError(t, err)
			}
			assert.Equal(t, tc.want, res)
			assert.Equal(t, tc.wantLogged, logged, "error log lines")
		})
	}
}

// TestReconcilers_FetchingADeletedObjectIsQuiet pins that a reconcile whose own object is gone ends
// without an error and without an error log. The object is deleted between the event and the fetch
// often enough, and there is nothing left to reconcile.
func TestReconcilers_FetchingADeletedObjectIsQuiet(t *testing.T) {
	testCases := []struct {
		name string
		new  func(ctrlcli.Client) ctrlreconcile.Reconciler
		key  ctrlcli.ObjectKey
	}{
		{
			name: "node feature",
			new:  func(c ctrlcli.Client) ctrlreconcile.Reconciler { return &NodeFeatureReconciler{Client: c} },
			key:  ctrlcli.ObjectKey{Name: "node"},
		},
		{
			name: "node fit label",
			new:  func(c ctrlcli.Client) ctrlreconcile.Reconciler { return &NodeFitLabelReconciler{Client: c} },
			key:  ctrlcli.ObjectKey{Name: "node"},
		},
		{
			name: "node capacity",
			new:  func(c ctrlcli.Client) ctrlreconcile.Reconciler { return &NodeCapacityReconciler{Client: c} },
			key:  ctrlcli.ObjectKey{Name: "node"},
		},
		{
			name: "node queue entrance",
			new:  func(c ctrlcli.Client) ctrlreconcile.Reconciler { return &NodeQueueEntranceReconciler{Client: c} },
			key:  ctrlcli.ObjectKey{Name: "queue"},
		},
		{
			name: "model deployment joint admission",
			new: func(c ctrlcli.Client) ctrlreconcile.Reconciler {
				return &ModelDeploymentJointAdmissionReconciler{Client: c}
			},
			key: ctrlcli.ObjectKey{Namespace: "team-a", Name: "wl"},
		},
		{
			name: "model deployment joint admission check",
			new: func(c ctrlcli.Client) ctrlreconcile.Reconciler {
				return &ModelDeploymentJointAdmissionCheckReconciler{Client: c}
			},
			key: ctrlcli.ObjectKey{Name: _JointAdmissionCheckName},
		},
	}
	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			r := tc.new(ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build())

			var logged int
			ctx := ctrllog.IntoContext(context.Background(), logr.New(errorCountingSink{errors: &logged}))
			res, err := r.Reconcile(ctx, ctrlreconcile.Request{NamespacedName: tc.key})
			assert.NoError(t, err)
			assert.Equal(t, ctrlreconcile.Result{}, res)
			assert.Equal(t, 0, logged, "error log lines")
		})
	}
}

// injectedOutcome is what the reconcile that hit an injected failure returned, with the number of
// error log lines it wrote.
type injectedOutcome struct {
	res    ctrlreconcile.Result
	err    error
	logged int
}

// reconcileUntilInjected reconciles until the injected failure has fired, at most a few times, and
// returns the outcome of the reconcile that hit it. The reconciles before it only bring the object
// up to the write, so they must succeed.
func reconcileUntilInjected(
	t *testing.T, r ctrlreconcile.Reconciler, req ctrlreconcile.Request, injected *bool,
) injectedOutcome {
	t.Helper()
	for range 5 {
		var out injectedOutcome
		ctx := ctrllog.IntoContext(context.Background(), logr.New(errorCountingSink{errors: &out.logged}))
		out.res, out.err = r.Reconcile(ctx, req)
		if *injected {
			return out
		}
		require.NoError(t, out.err, "a reconcile before the injected failure")
	}
	require.FailNow(t, "the injected failure never fired")
	return injectedOutcome{}
}
