package server

import (
	"context"
	"fmt"
	"slices"

	"golang.org/x/exp/maps"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	authz2 "gpustack.ai/gpustack/pkg/server/authz"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

const _TeamSubjectResource = "teamsubjects"

// TeamSubjectsHandler is a handler for v1.TeamSubjects objects,
// which is a subresource of v1.Team objects.
//
// TeamSubjectsHandler maps the rbac RoleBinding objects to the server v1.TeamSubjects objects.
type TeamSubjectsHandler struct {
	extensionapi.ObjectInfo
	extensionapi.GetOperation
	extensionapi.UpdateOperation

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func newTeamSubjectsHandler(opts extensionapi.SetupOptions) *TeamSubjectsHandler {
	h := &TeamSubjectsHandler{}

	// As storage.
	h.ObjectInfo = &server.TeamSubjects{}
	h.GetOperation = extensionapi.WithGet(h)
	h.UpdateOperation = extensionapi.WithUpdate(h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	return h
}

var (
	_ rest.Storage = (*TeamSubjectsHandler)(nil)
	_ rest.Getter  = (*TeamSubjectsHandler)(nil)
	_ rest.Updater = (*TeamSubjectsHandler)(nil)
	_ rest.Patcher = (*TeamSubjectsHandler)(nil)
)

func (h *TeamSubjectsHandler) New() runtime.Object {
	return &server.TeamSubjects{}
}

func (h *TeamSubjectsHandler) Destroy() {}

func (h *TeamSubjectsHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	// Validate.
	if key.Namespace != kuberess.SystemNamespaceName {
		return nil, kerrors.NewNotFound(server.Resource(_TeamSubjectResource), key.Name)
	}

	// List.
	rbList := new(rbac.RoleBindingList)
	err := h.APIReader.List(ctx, rbList,
		ctrlcli.InNamespace(key.Name),
		ctrlcli.MatchingLabelsSelector{
			Selector: systemmeta.GetResourcesLabelSelectorOfType("rolebindings"),
		})
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}
	rbList = systemmeta.FilterResourceListByNotes(rbList, "team", key.String())

	// Convert.
	tsbjs := convertTeamSubjectsFromRoleBindingList(rbList)
	if tsbjs == nil {
		return nil, kerrors.NewNotFound(server.Resource(_TeamSubjectResource), key.Name)
	}

	// Get and refill.
	team := new(server.Team)
	err = h.Client.Get(ctx, key, team, &opts)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}
	tsbjs.ObjectMeta = team.ObjectMeta

	return tsbjs, nil
}

func (h *TeamSubjectsHandler) OnUpdate(ctx context.Context, obj, objOld runtime.Object, _ ctrlcli.UpdateOptions) (runtime.Object, error) {
	tsbjs, tsbjsOld := obj.(*server.TeamSubjects), objOld.(*server.TeamSubjects)

	// Validate and map.
	subjRoleMap := make(map[server.SubjectReference]server.SubjectRole)
	{
		var errs field.ErrorList
		for i, tsbj := range tsbjs.Items {
			err := tsbj.Role.Validate()
			if err != nil {
				errs = append(errs, field.Invalid(
					field.NewPath(fmt.Sprintf("items[%d].role", i)), tsbj.Role, err.Error()))
			}
			subj := new(server.Subject)
			err = h.Client.Get(ctx, tsbj.ToNamespacedName(), subj)
			if err != nil {
				errs = append(errs, field.Invalid(
					field.NewPath(fmt.Sprintf("items[%d]", i)), tsbj.SubjectReference, err.Error()),
				)
				continue
			}
			subjRoleMap[tsbj.SubjectReference] = subj.Spec.Role
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_TeamSubjectResource), tsbjs.Name, errs)
		}
	}

	// Figure out delta.
	tsbjsSet := sets.New[server.TeamSubject](tsbjs.Items...)
	tsbjsOldSet := sets.New[server.TeamSubject](tsbjsOld.Items...)
	needRevoking := tsbjsOldSet.Difference(tsbjsSet)
	needGranting := tsbjsSet.Difference(tsbjsOldSet)

	// NB(thxCode): we revoke the old permission from the Team namespace first,
	// then TeamSubjectAuthzReconciler will take care of revoking the old permission from the Team namespace.
	tsbjsOld.Items = maps.Keys(needRevoking)
	err := authz2.RevokeTeamSubjects(ctx, h.Client, tsbjsOld)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("revoke team subject: %w", err))
	}

	// NB(thxCode): we grant the new permission to the Team namespace first,
	// then TeamSubjectAuthzReconciler will take care of granting the new permission to the Team namespace.
	tsbjs.Items = slices.DeleteFunc(maps.Keys(needGranting), func(tsbj server.TeamSubject) bool {
		// Only grant to subject who is not an admin.
		return subjRoleMap[tsbj.SubjectReference] == server.SubjectRoleAdmin
	})
	err = authz2.GrantTeamSubjects(ctx, h.Client, tsbjs)
	if err != nil {
		return nil, kerrors.NewInternalError(fmt.Errorf("grant team subject: %w", err))
	}

	// Get.
	return h.OnGet(ctx, ctrlcli.ObjectKeyFromObject(tsbjs),
		ctrlcli.GetOptions{
			Raw: &meta.GetOptions{ResourceVersion: "0"},
		})
}

func convertTeamSubjectFromRoleBinding(rb *rbac.RoleBinding) *server.TeamSubject {
	if rb == nil || rb.RoleRef.Kind != "ClusterRole" {
		return nil
	}

	if rb.DeletionTimestamp != nil {
		return nil
	}

	r := authz2.ConvertTeamRoleFromClusterRoleName(rb.RoleRef.Name)
	if r.Validate() != nil {
		return nil
	}

	var ns, n string
	for _, subj := range rb.Subjects {
		if subj.Kind != rbac.ServiceAccountKind {
			continue
		}
		ns = subj.Namespace
		if ns == "" {
			continue
		}
		n = authz2.ConvertSubjectNameFromServiceAccountName(subj.Name)
		if n == "" {
			continue
		}
	}
	if ns == "" || n == "" {
		return nil
	}

	tsbj := &server.TeamSubject{
		SubjectReference: server.SubjectReference{
			Namespace: ns,
			Name:      n,
		},
		Role: r,
	}
	return tsbj
}

func convertTeamSubjectsFromRoleBindingList(rbList *rbac.RoleBindingList) *server.TeamSubjects {
	if rbList == nil {
		return nil
	}

	tsbjs := &server.TeamSubjects{
		Items: make([]server.TeamSubject, 0, len(rbList.Items)),
	}

	for i := range rbList.Items {
		tsbj := convertTeamSubjectFromRoleBinding(&rbList.Items[i])
		if tsbj == nil {
			continue
		}
		tsbjs.Items = append(tsbjs.Items, *tsbj)
	}

	return tsbjs
}
