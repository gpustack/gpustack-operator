package server

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"

	core "k8s.io/api/core/v1"
	rbac "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/sets"
	"k8s.io/apimachinery/pkg/util/validation/field"
	"k8s.io/apimachinery/pkg/watch"
	authnuser "k8s.io/apiserver/pkg/authentication/user"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"

	server "gpustack.ai/gpustack/api/server/v1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubemeta"
	"gpustack.ai/gpustack/pkg/kubereviewsubject"
	"gpustack.ai/gpustack/pkg/server/authz"
	"gpustack.ai/gpustack/pkg/server/kuberess"
	"gpustack.ai/gpustack/pkg/systemmeta"
	"gpustack.ai/gpustack/pkg/utils/gox"
	"gpustack.ai/gpustack/pkg/utils/stringx"
)

const _TeamResource = "teams"

// TeamHandler handles v1.Team objects.
//
// TeamHandler maps the v1.Team object to a Kubernetes Namespace resource,
// which is named as the team's name.
type TeamHandler struct {
	extensionapi.ObjectInfo
	extensionapi.CurdOperations

	Client    ctrlcli.Client
	APIReader ctrlcli.Reader
}

func (h *TeamHandler) SetupHandler(
	ctx context.Context,
	opts extensionapi.SetupOptions,
) (gvr schema.GroupVersionResource, srs map[string]rest.Storage, err error) {
	// Configure field indexer.
	fi := opts.Manager.GetFieldIndexer()
	err = fi.IndexField(ctx, &core.Namespace{}, "metadata.name",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			return []string{obj.GetName()}
		})
	if err != nil {
		return gvr, srs, fmt.Errorf("index namespace 'metadata.name': %w", err)
	}
	err = fi.IndexField(ctx, &rbac.RoleBinding{}, "rolebindings[scope=team].subject",
		func(obj ctrlcli.Object) []string {
			if obj == nil {
				return nil
			}
			resType, notes := systemmeta.DescribeResource(obj)
			if resType != "rolebindings" {
				return nil
			}
			if notes["scope"] != "team" {
				return nil
			}
			return []string{notes["subject"]}
		})
	if err != nil {
		return gvr, srs, fmt.Errorf("index role binding 'rolebindings[scope=team].subject': %w", err)
	}

	// Declare GVR.
	gvr = server.SchemeGroupVersionResource(_TeamResource)

	// Create table convertor to pretty the kubectl's output.
	tc, err := extensionapi.NewJSONPathTableConvertor(
		extensionapi.JSONPathTableColumnDefinition{
			TableColumnDefinition: meta.TableColumnDefinition{
				Name: "Phase",
				Type: "string",
			},
			JSONPath: ".status.phase",
		})
	if err != nil {
		return gvr, srs, err
	}

	// As storage.
	h.ObjectInfo = &server.Team{}
	h.CurdOperations = extensionapi.WithCurd(tc, h)

	// Set client.
	h.Client = opts.Manager.GetClient()
	h.APIReader = opts.Manager.GetAPIReader()

	// Create subresource handlers.
	srs = map[string]rest.Storage{
		"subjects": newTeamSubjectsHandler(opts),
	}

	return gvr, srs, err
}

var (
	_ rest.Storage           = (*TeamHandler)(nil)
	_ rest.Creater           = (*TeamHandler)(nil)
	_ rest.Lister            = (*TeamHandler)(nil)
	_ rest.Watcher           = (*TeamHandler)(nil)
	_ rest.Getter            = (*TeamHandler)(nil)
	_ rest.Updater           = (*TeamHandler)(nil)
	_ rest.Patcher           = (*TeamHandler)(nil)
	_ rest.GracefulDeleter   = (*TeamHandler)(nil)
	_ rest.CollectionDeleter = (*TeamHandler)(nil)
)

func (h *TeamHandler) New() runtime.Object {
	return &server.Team{}
}

func (h *TeamHandler) Destroy() {
}

func (h *TeamHandler) OnCreate(ctx context.Context, obj runtime.Object, opts ctrlcli.CreateOptions) (runtime.Object, error) {
	// Validate.
	team := obj.(*server.Team)
	{
		var errs field.ErrorList
		if team.Namespace != kuberess.SystemNamespaceName {
			errs = append(errs, field.Invalid(
				field.NewPath("metadata.namespace"), team.Namespace, "team namespace must be "+kuberess.SystemNamespaceName))
		}
		if slices.Contains([]string{"kube-system", "kube-public", kuberess.SystemNamespaceName}, team.Name) {
			errs = append(errs, field.Invalid(
				field.NewPath("metadata.name"), team.Name, "team name is reserved"))
		}
		if stringx.StringWidth(team.Name) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("metadata.name"), stringx.StringWidth(team.Name), 30))
		}
		if stringx.StringWidth(team.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(team.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(team.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(team.Spec.Description), 50))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_TeamResource), team.Name, errs)
		}
	}

	// Create.
	if team.Name == kuberess.DefaultTeamName {
		// NB(thxCode): The default team is created by the system,
		// so we need another approach to adopt the default team.
		_, err := h.OnGet(ctx, ctrlcli.ObjectKeyFromObject(team),
			ctrlcli.GetOptions{
				Raw: &meta.GetOptions{ResourceVersion: "0"},
			})
		if err == nil {
			return nil, kerrors.NewAlreadyExists(server.Resource(_TeamResource), team.Name)
		}
		ns := convertNamespaceFromTeam(team)
		{
			// Refill UID and ResourceVersion.
			aNs := new(core.Namespace)
			err := h.Client.Get(ctx, ctrlcli.ObjectKeyFromObject(ns), aNs)
			if err != nil {
				return nil, kerrors.NewInternalError(fmt.Errorf("get default namespace: %w", err))
			}
			ns.UID = aNs.UID
			ns.ResourceVersion = aNs.ResourceVersion
		}
		err = h.Client.Update(ctx, ns)
		if err != nil {
			return nil, kerrors.NewInternalError(fmt.Errorf("create default team: %w", err))
		}
		team = convertTeamFromNamespace(ns)
	} else {
		ns := convertNamespaceFromTeam(team)
		err := h.Client.Create(ctx, ns, &opts)
		if err != nil {
			return nil, err
		}
		team = convertTeamFromNamespace(ns)
	}

	// Grant.
	err := authz.GrantTeamSubjectRole(ctx, h.Client, team, server.TeamRoleOwner)
	if err != nil {
		return nil, kerrors.NewInternalError(err)
	}

	return team, nil
}

func (h *TeamHandler) NewList() runtime.Object {
	return &server.TeamList{}
}

func (h *TeamHandler) OnList(ctx context.Context, opts ctrlcli.ListOptions) (runtime.Object, error) {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return nil, kerrors.NewForbidden(server.Resource(_TeamResource), "", errors.New("request user not found"))
	}

	// List.
	nsList := new(core.NamespaceList)
	err := h.APIReader.List(ctx, nsList,
		convertNamespaceListOptsFromTeamListOpts(opts))
	if err != nil {
		return nil, err
	}

	// Convert.
	pList := convertTeamListFromNamespaceList(nsList, opts)
	return h.filterTeamList(ctx, ui, pList), nil
}

func (h *TeamHandler) filterTeamList(ctx context.Context, ui authnuser.Info, pList *server.TeamList) *server.TeamList {
	// Fast-path: check with well-known admin user.
	if authz.IsWellKnownAdminUser(ui) {
		return pList
	}

	// Slow-path: check with a subject access review.
	revs := kubereviewsubject.Reviews{
		{
			ResourceAttributes: &kubereviewsubject.ResourceAttributes{
				Group:    server.GroupName,
				Resource: _TeamResource,
				Verb:     "list",
			},
		},
	}
	err := kubereviewsubject.CanSpecificUserDoWithCtrlClient(ctx, h.Client, revs, ui)
	if err == nil {
		return pList
	}

	// Slower-path: check with informer cache.
	{
		subjNamespace, subjName, ok := authz.ConvertSubjectNamesFromAuthnUser(ui)
		if ok {
			rbList := new(rbac.RoleBindingList)
			err = h.Client.List(ctx, rbList, ctrlcli.MatchingFields{
				"rolebindings[scope=team].subject": subjNamespace + "/" + subjName,
			})
			if err == nil {
				allowTeams := sets.New[string]()
				for _, rb := range rbList.Items {
					allowTeams.Insert(rb.Namespace)
				}
				pList.Items = slices.DeleteFunc(pList.Items, func(team server.Team) bool {
					return !allowTeams.Has(team.Name)
				})
				return pList
			}
		}
	}

	// Slowest-path: check with multiple subject access reviews.
	items := pList.Items
	pList.Items = pList.Items[:0]
	for i := range items {
		revs = kubereviewsubject.Reviews{
			{
				ResourceAttributes: &kubereviewsubject.ResourceAttributes{
					Group:     server.GroupName,
					Resource:  _TeamResource,
					Namespace: items[i].Name,
					Verb:      "list",
				},
			},
		}
		err := kubereviewsubject.CanSpecificUserDoWithCtrlClient(ctx, h.Client, revs, ui)
		if err != nil {
			continue
		}
		pList.Items = append(pList.Items, items[i])
	}
	return pList
}

func (h *TeamHandler) OnWatch(ctx context.Context, opts ctrlcli.ListOptions) (watch.Interface, error) {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return nil, kerrors.NewForbidden(server.Resource(_TeamResource), "", errors.New("request user not found"))
	}

	// Watch.
	uw, err := h.Client.(ctrlcli.WithWatch).Watch(ctx, new(core.NamespaceList),
		convertNamespaceListOptsFromTeamListOpts(opts))
	if err != nil {
		return nil, err
	}

	c := make(chan watch.Event)
	dw := watch.NewProxyWatcher(c)
	gox.Go(func() {
		defer close(c)
		defer uw.Stop()

		for {
			select {
			case <-ctx.Done():
				// Cancel by context.
				return
			case <-dw.StopChan():
				// Stop by downstream.
				return
			case e, ok := <-uw.ResultChan():
				if !ok {
					// Close by upstream.
					return
				}

				// Nothing to do.
				if e.Object == nil {
					c <- e
					continue
				}

				// Type assert.
				ns, ok := e.Object.(*core.Namespace)
				if !ok {
					c <- e
					continue
				}

				// Process bookmark.
				if e.Type == watch.Bookmark {
					resType, _ := systemmeta.DescribeResource(ns)
					if resType == _TeamResource {
						e.Object = &server.Team{ObjectMeta: ns.ObjectMeta}
						c <- e
					}
					continue
				}

				// Convert.
				team := safeConvertTeamFromNamespace(ns, opts.Namespace)
				if team == nil {
					// Skip if not belong to the requested namespace.
					continue
				}

				// Filter.
				team = h.filterTeamWatch(ctx, ui, team)
				if team == nil {
					continue
				}

				// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
				if !teamMatchFieldSelector(opts, team) {
					continue
				}

				// Dispatch.
				e.Object = team
				c <- e
			}
		}
	})

	return dw, nil
}

func (h *TeamHandler) filterTeamWatch(ctx context.Context, ui authnuser.Info, team *server.Team) *server.Team {
	// Fast-path: check with well-known admin user.
	if authz.IsWellKnownAdminUser(ui) {
		return team
	}

	// Slow-path: check with a subject access review.
	revs := kubereviewsubject.Reviews{
		{
			ResourceAttributes: &kubereviewsubject.ResourceAttributes{
				Group:     server.GroupName,
				Resource:  _TeamResource,
				Namespace: team.Name,
				Verb:      "watch",
			},
		},
	}
	err := kubereviewsubject.CanSpecificUserDoWithCtrlClient(ctx, h.Client, revs, ui)
	if err == nil {
		return team
	}
	return nil
}

func (h *TeamHandler) OnGet(ctx context.Context, key types.NamespacedName, opts ctrlcli.GetOptions) (runtime.Object, error) {
	ui, ok := genericapirequest.UserFrom(ctx)
	if !ok {
		return nil, kerrors.NewForbidden(server.Resource(_TeamResource), "", errors.New("request user not found"))
	}

	// Validate.
	if key.Namespace != kuberess.SystemNamespaceName {
		return nil, kerrors.NewNotFound(server.Resource(_TeamResource), key.Name)
	}

	// Get.
	ns := &core.Namespace{
		ObjectMeta: meta.ObjectMeta{
			Name: key.Name,
		},
	}
	err := h.APIReader.Get(ctx, ctrlcli.ObjectKeyFromObject(ns), ns, &opts)
	if err != nil {
		return nil, kerrors.NewNotFound(server.Resource(_TeamResource), key.Name)
	}

	// Convert.
	team := convertTeamFromNamespace(ns)
	if team == nil {
		return nil, kerrors.NewNotFound(server.Resource(_TeamResource), key.Name)
	}

	// Filter.
	team = h.filterTeamGet(ctx, ui, team)
	if team == nil {
		return nil, kerrors.NewNotFound(server.Resource(_TeamResource), key.Name)
	}

	return team, nil
}

func (h *TeamHandler) filterTeamGet(ctx context.Context, ui authnuser.Info, team *server.Team) *server.Team {
	// Fast-path: check with well-known admin user.
	if authz.IsWellKnownAdminUser(ui) {
		return team
	}

	// Slow-path: check with a subject access review.
	revs := kubereviewsubject.Reviews{
		{
			ResourceAttributes: &kubereviewsubject.ResourceAttributes{
				Group:     server.GroupName,
				Resource:  _TeamResource,
				Namespace: team.Name,
				Verb:      "get",
			},
		},
	}
	err := kubereviewsubject.CanSpecificUserDoWithCtrlClient(ctx, h.Client, revs, ui)
	if err == nil {
		return team
	}
	return nil
}

func (h *TeamHandler) OnUpdate(ctx context.Context, obj, _ runtime.Object, opts ctrlcli.UpdateOptions) (runtime.Object, error) {
	// Validate.
	team := obj.(*server.Team)
	{
		var errs field.ErrorList
		if stringx.StringWidth(team.Spec.DisplayName) > 30 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.displayName"), stringx.StringWidth(team.Spec.DisplayName), 30))
		}
		if stringx.StringWidth(team.Spec.Description) > 50 {
			errs = append(errs, field.TooLong(
				field.NewPath("spec.description"), stringx.StringWidth(team.Spec.Description), 50))
		}
		if len(errs) > 0 {
			return nil, kerrors.NewInvalid(server.Kind(_TeamResource), team.Name, errs)
		}
	}

	// TODO Validate RBAC

	// Update.
	{
		ns := convertNamespaceFromTeam(team)
		err := h.Client.Update(ctx, ns, &opts)
		if err != nil {
			return nil, err
		}
		team = convertTeamFromNamespace(ns)
	}

	return team, nil
}

func (h *TeamHandler) OnDelete(ctx context.Context, obj runtime.Object, opts ctrlcli.DeleteOptions) error {
	team := obj.(*server.Team)

	// Validate.
	{
		// Prevent deleting default team.
		if team.Name == kuberess.DefaultTeamName {
			return kerrors.NewBadRequest("default team is reserved")
		}
	}

	// Delete.
	ns := &core.Namespace{ObjectMeta: team.ObjectMeta}
	ns.Namespace = ""
	return h.Client.Delete(ctx, ns, &opts)
}

func convertNamespaceListOptsFromTeamListOpts(in ctrlcli.ListOptions) (out *ctrlcli.ListOptions) {
	// Ignore namespace selector.
	if in.Namespace == kuberess.SystemNamespaceName {
		in.Namespace = ""
	}
	if in.FieldSelector != nil {
		reqs := slices.DeleteFunc(in.FieldSelector.Requirements(), func(req fields.Requirement) bool {
			return req.Field == "metadata.namespace"
		})
		if len(reqs) == 0 {
			in.FieldSelector = nil
		} else {
			in.FieldSelector = kubemeta.FieldSelectorFromRequirements(reqs)
		}
	}

	// Add necessary label selector.
	if lbs := systemmeta.GetResourcesLabelSelectorOfType(_TeamResource); in.LabelSelector == nil {
		in.LabelSelector = lbs
	} else {
		reqs, _ := lbs.Requirements()
		in.LabelSelector = in.LabelSelector.DeepCopySelector().Add(reqs...)
	}

	return &in
}

func convertNamespaceFromTeam(team *server.Team) *core.Namespace {
	ns := &core.Namespace{
		ObjectMeta: team.ObjectMeta,
	}
	systemmeta.NoteResource(ns, _TeamResource, map[string]string{
		"scope":       "organization",
		"displayName": team.Spec.DisplayName,
		"description": team.Spec.Description,
	})
	ns.Namespace = ""
	return ns
}

func convertTeamFromNamespace(ns *core.Namespace) *server.Team {
	if ns == nil {
		return nil
	}

	resType, notes := systemmeta.UnnoteResource(ns)
	if resType != _TeamResource {
		return nil
	}

	team := &server.Team{
		ObjectMeta: ns.ObjectMeta,
		Spec: server.TeamSpec{
			DisplayName: notes["displayName"],
			Description: notes["description"],
		},
		Status: server.TeamStatus{
			Phase: ns.Status.Phase,
		},
	}
	team.Namespace = kuberess.SystemNamespaceName
	return team
}

func safeConvertTeamFromNamespace(ns *core.Namespace, reqNamespace string) *server.Team {
	team := convertTeamFromNamespace(ns)
	if team != nil && reqNamespace != "" && reqNamespace != team.Namespace {
		// NB(thxCode): sanitize if the team's namespace doesn't match requested namespace.
		team = nil
	}
	return team
}

func convertTeamListFromNamespaceList(nsList *core.NamespaceList, opts ctrlcli.ListOptions) *server.TeamList {
	if nsList == nil {
		return &server.TeamList{}
	}

	// Sort by resource version.
	sort.SliceStable(nsList.Items, func(i, j int) bool {
		l, r := nsList.Items[i].ResourceVersion, nsList.Items[j].ResourceVersion
		return len(l) < len(r) ||
			(len(l) == len(r) && l < r)
	})

	teamList := &server.TeamList{
		ListMeta: nsList.ListMeta,
		Items:    make([]server.Team, 0, len(nsList.Items)),
	}

	for i := range nsList.Items {
		team := safeConvertTeamFromNamespace(&nsList.Items[i], opts.Namespace)
		if team == nil {
			continue
		}

		// Ignore if not be selected by `kubectl get --field-selector=metadata.namespace=...`.
		if !teamMatchFieldSelector(opts, team) {
			continue
		}
		teamList.Items = append(teamList.Items, *team)
	}

	return teamList
}

// teamMatchFieldSelector checks if the Team matches the field selector in list options.
func teamMatchFieldSelector(opts ctrlcli.ListOptions, team *server.Team) bool {
	fs := opts.FieldSelector
	if fs == nil {
		return true
	}
	return fs.Matches(fields.Set{"metadata.namespace": team.Namespace, "metadata.name": team.Name})
}
