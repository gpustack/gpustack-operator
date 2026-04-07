package server

import (
	"context"
	"fmt"

	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/server/authz"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// SubjectReconciler reconciles the v1.Subject object below the system namespace,
// and ensures its permissions are granted.
type SubjectReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*SubjectReconciler)(nil)

func (r *SubjectReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	subj := new(server.Subject)
	err := r.Client.Get(ctx, req.NamespacedName, subj)
	if err != nil {
		logger.Error(err, "fetch subject")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Revoke if deleted.
	if subj.DeletionTimestamp != nil {
		// Return if already unlocked.
		if systemmeta.Unlock(subj) {
			return ctrl.Result{}, nil
		}

		// List related role bindings.
		rbList := new(rbac.RoleBindingList)
		err = r.Client.List(ctx, rbList,
			ctrlcli.MatchingFields{IndexingByNonOrgSubject: kubemeta.GetNamespacedNameKey(subj)},
			ctrlcli.UnsafeDisableDeepCopy)
		if err != nil {
			logger.Error(err, "list related role bindings")
			return ctrl.Result{}, err
		}

		// Delete related role bindings.
		for i := range rbList.Items {
			eRb := &rbList.Items[i]
			err = r.Client.Delete(ctx, eRb)
			if err != nil && !kerrors.IsNotFound(err) {
				logger.Error(err, "delete related role binding", "rolebinding", kubemeta.GetNamespacedNameKey(eRb))
				return ctrl.Result{}, err
			}
		}

		// Unlock.
		_, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, subj)
		if err != nil {
			logger.Error(err, "unlock subject")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}

		return ctrl.Result{}, nil
	}

	// Lock if not.
	if !systemmeta.Lock(subj) {
		subj, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, subj)
		if err != nil {
			logger.Error(err, "lock subject")
			return ctrl.Result{}, err
		}
	}

	// Grant.
	err = authz.GrantSubject(ctx, r.Client, subj)
	if err != nil {
		logger.Error(err, "grant subject, requeue")
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

const (
	IndexingByNonOrgSubject = "rolebindings[scope!=organization].subject"
)

func (r *SubjectReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &rbac.RoleBinding{}, IndexingByNonOrgSubject,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			resType, notes := systemmeta.DescribeResource(obj)
			if resType != "rolebindings" {
				return nil
			}
			if notes["scope"] == "organization" {
				return nil
			}
			return []string{notes["subject"]}
		})
	if err != nil {
		return fmt.Errorf("index role binding '%s': %w", IndexingByNonOrgSubject, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("server.manage.subjects").
		For(
			// Focus on the Subject.
			&server.Subject{},
			ctrlbuilder.WithPredicates(
				// Consider on role changes of Subject.
				ctrlpredicate.Funcs{
					UpdateFunc: func(e ctrlevent.UpdateEvent) bool {
						allow := e.ObjectNew.GetDeletionTimestamp() != nil ||
							e.ObjectNew.(*server.Subject).Spec.Role != e.ObjectOld.(*server.Subject).Spec.Role
						return allow
					},
				},
			),
		).
		Complete(r)
}
