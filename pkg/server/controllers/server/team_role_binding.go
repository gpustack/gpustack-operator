package server

import (
	"context"
	"fmt"
	"time"

	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	ctrlbuilder "sigs.k8s.io/controller-runtime/pkg/builder"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlevent "sigs.k8s.io/controller-runtime/pkg/event"
	ctrlhandler "sigs.k8s.io/controller-runtime/pkg/handler"
	ctrllog "sigs.k8s.io/controller-runtime/pkg/log"
	ctrlpredicate "sigs.k8s.io/controller-runtime/pkg/predicate"
	ctrlreconcile "sigs.k8s.io/controller-runtime/pkg/reconcile"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/controller"
	"gpustack.ai/gpustack/pkg/kubeclientset"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/server/authz"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// TeamRoleBindingReconciler reconciles the Kubernetes RoleBinding object below the Team namespace.
//
// TeamRoleBindingReconciler works like a dispatcher,
// which listens to the role bindings created under the Team namespace.
// And then, it copies the role bindings to the related Environments.
//
// TeamRoleBindingReconciler will be requeue if a new Team of the Team is created,
// so that we will not miss any assigned TeamSubject.
type TeamRoleBindingReconciler struct {
	Client ctrlcli.Client
}

var _ ctrlreconcile.Reconciler = (*TeamRoleBindingReconciler)(nil)

func (r *TeamRoleBindingReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := ctrllog.FromContext(ctx)

	// Fetch.
	rb := new(rbac.RoleBinding)
	err := r.Client.Get(ctx, req.NamespacedName, rb)
	if err != nil {
		logger.Error(err, "fetch role binding")
		return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
	}

	// Revoke if deleted.
	if rb.DeletionTimestamp != nil {
		// Return if already unlocked.
		if systemmeta.Unlock(rb) {
			return ctrl.Result{}, nil
		}

		// List related role bindings.
		rbList := new(rbac.RoleBindingList)
		err = r.Client.List(ctx, rbList,
			ctrlcli.MatchingFields{IndexingByProjectSubject: rb.Name},
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
		_, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, rb)
		if err != nil {
			logger.Error(err, "unlock role binding")
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}

		return ctrl.Result{}, nil
	}

	// Lock if not.
	if !systemmeta.Lock(rb) {
		rb, err = kubeclientset.UpdateWithCtrlClient(ctx, r.Client, rb)
		if err != nil {
			logger.Error(err, "lock role binding")
		}
	}

	// Get subject.
	subj := new(server.Subject)
	{
		var ns, n string
		for _, s := range rb.Subjects {
			if s.Kind != rbac.ServiceAccountKind {
				continue
			}
			ns = s.Namespace
			if ns == "" {
				continue
			}
			n = authz.ConvertSubjectNameFromServiceAccountName(s.Name)
			if n == "" {
				continue
			}
			break
		}
		if ns == "" || n == "" {
			return ctrl.Result{}, nil
		}

		err = r.Client.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: n}, subj)
		if err != nil {
			// Revoke if the subject is not found
			err = kubeclientset.DeleteWithCtrlClient(ctx, r.Client, rb)
			if err != nil {
				logger.Error(err, "delete role binding of not found subject")
			}
			return ctrl.Result{}, ctrlcli.IgnoreNotFound(err)
		}
	}

	// List related projects.
	teamKey := kubemeta.ParseNamespacedNameKey(systemmeta.DescribeResourceNote(rb, "team"))
	projList := new(server.ProjectList)
	err = r.Client.List(ctx, projList,
		ctrlcli.InNamespace(teamKey.Name),
		ctrlcli.UnsafeDisableDeepCopy)
	if err != nil {
		logger.Error(err, "list related projects")
		return ctrl.Result{}, err
	}

	// Grant: copy to related projects.
	for i := range projList.Items {
		proj := &projList.Items[i]
		if proj.DeletionTimestamp != nil {
			continue
		}

		eRb := &rbac.RoleBinding{
			ObjectMeta: meta.ObjectMeta{
				Namespace: proj.Name,
				Name:      rb.Name,
			},
			RoleRef:  rb.RoleRef,
			Subjects: rb.Subjects,
		}
		systemmeta.NoteResource(eRb, "rolebindings", map[string]string{
			"scope":    "project",
			"projects": kubemeta.GetNamespacedNameKey(proj),
			"subject":  kubemeta.GetNamespacedNameKey(subj),
		})

		// Create.
		_, err = kubeclientset.CreateWithCtrlClient(ctx, r.Client, eRb,
			kubeclientset.WithRecreateIfDuplicated(kubeclientset.NewRbacRoleBindingCompareFunc(eRb)))
		if err != nil {
			return ctrl.Result{RequeueAfter: time.Second}, fmt.Errorf("create role binding: %w", err)
		}
	}

	return ctrl.Result{}, nil
}

const (
	IndexingByProjectSubject = "rolebindings[scope=project].name"
)

func (r *TeamRoleBindingReconciler) SetupController(ctx context.Context, opts controller.SetupOptions) error {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err := fi.IndexField(ctx, &rbac.RoleBinding{}, IndexingByProjectSubject,
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			resType, notes := systemmeta.DescribeResource(obj)
			if resType != "rolebindings" {
				return nil
			}
			if notes["scope"] != "project" {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return fmt.Errorf("index role binding '%s': %w", IndexingByProjectSubject, err)
	}

	r.Client = opts.Manager.GetClient()

	return ctrl.NewControllerManagedBy(opts.Manager).
		Named("server.manage.team_role_binding").
		For(
			// Focus on the RoleBindings under the Project namespace.
			&rbac.RoleBinding{},
			ctrlbuilder.WithPredicates(
				// Consider on team-scoped RoleBindings.
				ctrlpredicate.NewPredicateFuncs(func(obj ctrlcli.Object) bool {
					resType, notes := systemmeta.DescribeResource(obj)
					return resType == "rolebindings" &&
						notes["scope"] == "team" &&
						kubemeta.ContainsNameInNamespacedNameKey(obj.GetNamespace(), notes["team"])
				}),
			),
		).
		Watches(
			// Enqueue the corresponding RoleBindings when creating a Project.
			&server.Project{},
			ctrlhandler.EnqueueRequestsFromMapFunc(
				r.findObjectsWhenProjectCreating,
			),
			ctrlbuilder.WithPredicates(
				ctrlpredicate.Not(ctrlpredicate.Funcs{
					CreateFunc: func(_ ctrlevent.CreateEvent) bool { return false },
				}),
			),
		).
		Complete(r)
}

func (r *TeamRoleBindingReconciler) findObjectsWhenProjectCreating(ctx context.Context, obj ctrlcli.Object) []ctrlreconcile.Request {
	logger := ctrllog.FromContext(ctx)

	teamSubjs := new(server.TeamSubjects)
	{
		team := &server.Team{
			ObjectMeta: meta.ObjectMeta{
				Namespace: kuberess.SystemNamespaceName,
				Name:      obj.GetNamespace(),
			},
		}
		err := r.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(team), team)
		if err != nil {
			logger.Error(err, "get team")
			return []ctrlreconcile.Request{}
		}
		err = r.Client.SubResource("subjects").Get(ctx, team, teamSubjs)
		if err != nil {
			logger.Error(err, "get team subjects")
			return []ctrlreconcile.Request{}
		}
	}

	reqs := make([]ctrlreconcile.Request, len(teamSubjs.Items))
	for i, item := range teamSubjs.Items {
		reqs[i] = ctrlreconcile.Request{
			NamespacedName: ctrlcli.ObjectKey{
				Namespace: obj.GetNamespace(),
				Name:      authz.GetTeamSubjectRoleBindingName(&item),
			},
		}
	}
	return reqs
}
