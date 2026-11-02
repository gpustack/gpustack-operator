package worker

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	jsonpatch "github.com/evanphx/json-patch/v5"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metainternal "k8s.io/apimachinery/pkg/apis/meta/internalversion"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/watch"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// The policy handler is a pure proxy: every public v1 call must reach the v1alpha1
// object behind it and answer with what that object holds. These tests drive the real
// handler on its normal WithCurdProxy composition over a fake internal client, so what
// they observe is the storage contract, not a mock of it. Every subtest seeds its own
// store or watcher, so any child runs alone and none depends on a sibling's writes.

const _DevicesRDMASelectionPolicyResource = "devicesrdmaselectionpolicies"

// policySelector builds the fixture selector with both match forms, so an expectation
// that passes with a non-nil but emptied selector is a lost content check.
func policySelector() *meta.LabelSelector {
	return &meta.LabelSelector{
		MatchLabels: map[string]string{"gpustack.ai/pool": "rail-a"},
		MatchExpressions: []meta.LabelSelectorRequirement{{
			Key:      "gpustack.ai/zone",
			Operator: meta.LabelSelectorOpIn,
			Values:   []string{"z1", "z2"},
		}},
	}
}

// fullPolicySpec builds the complete expected spec: the full selector, a named include,
// and an exclusion. Expectations are declared from this fixture, never from what the
// handler answers, so a field the proxy drops cannot mirror itself into the expectation.
func fullPolicySpec() workercore.DevicesRDMASelectionPolicySpec {
	return policyFixture("", nil).Spec
}

// policyFixture builds a public policy with a complete spec, so any cast or write that
// loses a member fails the state comparison instead of passing vacuously.
func policyFixture(name string, mutate func(*worker.DevicesRDMASelectionPolicy)) *worker.DevicesRDMASelectionPolicy {
	p := &worker.DevicesRDMASelectionPolicy{
		ObjectMeta: meta.ObjectMeta{
			Name:        name,
			Labels:      map[string]string{"gpustack.ai/profile": "ib-rail-a"},
			Annotations: map[string]string{"gpustack.ai/description": "rail-a HCAs"},
		},
		Spec: workercore.DevicesRDMASelectionPolicySpec{
			NodeSelector: policySelector(),
			Include:      []string{"mlx5_0", "mlx5_1"},
			Exclude:      []string{"mlx5_9"},
		},
	}
	if mutate != nil {
		mutate(p)
	}
	return p
}

// policyAsInternal casts a fixture to the internal version, the way the handler's own
// casts do, for seeding the fake store directly.
func policyAsInternal(p *worker.DevicesRDMASelectionPolicy) *workercore.DevicesRDMASelectionPolicy {
	return (*workercore.DevicesRDMASelectionPolicy)(p)
}

func internalPolicy(name, resourceVersion string) *workercore.DevicesRDMASelectionPolicy {
	p := policyAsInternal(policyFixture(name, nil))
	p.ResourceVersion = resourceVersion
	return p
}

// policySelState compares the selector and both name lists.
type policySelState struct {
	selector       *meta.LabelSelector
	includePresent bool
	names          []string
	exclude        []string
}

func policySelStateOfSpec(spec workercore.DevicesRDMASelectionPolicySpec) policySelState {
	return policySelState{
		selector: spec.NodeSelector, includePresent: len(spec.Include) > 0,
		names: spec.Include, exclude: spec.Exclude,
	}
}

func (s policySelState) assertEqual(t *testing.T, want policySelState, label string) {
	t.Helper()
	if want.selector == nil {
		assert.Nil(t, s.selector, "%s: nodeSelector must be absent", label)
	} else {
		require.NotNil(t, s.selector, "%s: nodeSelector must be present", label)
		assert.Equal(t, want.selector, s.selector, "%s: nodeSelector content", label)
	}
	assert.Equal(t, want.includePresent, s.includePresent, "%s: include presence is the restriction switch", label)
	assert.Equal(t, policySelStringList(want.names), policySelStringList(s.names), "%s: include names (nil equals empty)", label)
	assert.Equal(t, policySelStringList(want.exclude), policySelStringList(s.exclude), "%s: exclude (nil equals empty)", label)
}

func policySelStringList(s []string) []string {
	if len(s) == 0 {
		return []string{}
	}
	return s
}

// newPolicyHandler wires the real policy handler exactly as its SetupHandler does, over
// the given client standing in for the manager's cache client and API reader.
func newPolicyHandler(t *testing.T, cli ctrlcli.WithWatch) *DevicesRDMASelectionPolicyHandler {
	t.Helper()
	tc, err := extensionapi.NewJSONPathTemplateTableConvertor()
	require.NoError(t, err)
	h := &DevicesRDMASelectionPolicyHandler{}
	h.ObjectInfo = &worker.DevicesRDMASelectionPolicy{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.DevicesRDMASelectionPolicy, *worker.DevicesRDMASelectionPolicyList,
		*workercore.DevicesRDMASelectionPolicy, *workercore.DevicesRDMASelectionPolicyList,
	](tc, h, cli, cli)
	return h
}

// newPolicyStorageWith builds a fake internal client with the given interceptor funcs
// and the handler on top, plus the request context of a cluster-scoped read.
func newPolicyStorageWith(
	t *testing.T, funcs interceptor.Funcs, objs ...ctrlcli.Object,
) (*DevicesRDMASelectionPolicyHandler, ctrlcli.WithWatch, context.Context) {
	t.Helper()
	cli := ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(funcs).
		Build()
	return newPolicyHandler(t, cli), cli, requestContext("", _DevicesRDMASelectionPolicyResource)
}

func newPolicyStorage(t *testing.T, objs ...ctrlcli.Object) (*DevicesRDMASelectionPolicyHandler, ctrlcli.WithWatch, context.Context) {
	t.Helper()
	return newPolicyStorageWith(t, interceptor.Funcs{}, objs...)
}

// seedPolicy creates the fixture through the handler and answers the created object, so
// the calling subtest owns its identity references without sharing state with siblings.
func seedPolicy(
	t *testing.T, h *DevicesRDMASelectionPolicyHandler, ctx context.Context,
	name string, mutate func(*worker.DevicesRDMASelectionPolicy),
) *worker.DevicesRDMASelectionPolicy {
	t.Helper()
	obj, err := h.Create(ctx, policyFixture(name, mutate), nil, &meta.CreateOptions{})
	require.NoError(t, err)
	created, ok := obj.(*worker.DevicesRDMASelectionPolicy)
	require.True(t, ok, "create must answer a *worker.DevicesRDMASelectionPolicy, got %T", obj)
	return created
}

// getInternalPolicy reads the internal stored object, the state the public calls proxy.
func getInternalPolicy(t *testing.T, cli ctrlcli.WithWatch, ctx context.Context, name string) *workercore.DevicesRDMASelectionPolicy {
	t.Helper()
	stored := new(workercore.DevicesRDMASelectionPolicy)
	require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Name: name}, stored))
	return stored
}

// readbackForUpdate returns a copy of the public object as read, mutated the way a
// client would before putting it back: the fake store enforces optimistic concurrency,
// so an update object must carry the revision it was read at. Expectations must be
// declared from fixtures instead of this readback.
func readbackForUpdate(
	t *testing.T, h *DevicesRDMASelectionPolicyHandler, ctx context.Context,
	name string, mutate func(*worker.DevicesRDMASelectionPolicy),
) *worker.DevicesRDMASelectionPolicy {
	t.Helper()
	obj, err := h.Get(ctx, name, nil)
	require.NoError(t, err)
	updated := obj.(*worker.DevicesRDMASelectionPolicy).DeepCopy()
	mutate(updated)
	return updated
}

// requireSameStatus requires the refused call to carry the sentinel's code, reason and
// message: the proxy may rewrap an upstream API error, but it must not swallow or
// rewrite what the caller is told.
func requireSameStatus(t *testing.T, want, got error) {
	t.Helper()
	require.Error(t, got)
	var ws, gs kerrors.APIStatus
	require.ErrorAs(t, want, &ws)
	require.ErrorAs(t, got, &gs)
	assert.Equal(t, ws.Status().Code, gs.Status().Code, "code")
	assert.Equal(t, ws.Status().Reason, gs.Status().Reason, "reason")
	assert.Equal(t, ws.Status().Message, gs.Status().Message, "message")
}

// TestDevicesRDMASelectionPolicyHandler_CRUD drives each public verb against its own
// seeded store and pins the complete internal spec, the metadata, and the identity
// references the public answers must carry.
func TestDevicesRDMASelectionPolicyHandler_CRUD(t *testing.T) {
	t.Run("create", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)

		created := seedPolicy(t, h, ctx, "rail-a", nil)
		assert.Equal(t, worker.SchemeGroupVersion.WithKind("DevicesRDMASelectionPolicy"), created.GroupVersionKind(),
			"the public answer carries the public GVK")
		assert.False(t, created.CreationTimestamp.IsZero(), "create fills the creation timestamp")
		assert.NotEmpty(t, created.UID, "create fills the UID")

		stored := getInternalPolicy(t, cli, ctx, "rail-a")
		policySelStateOfSpec(stored.Spec).assertEqual(t, policySelStateOfSpec(fullPolicySpec()), "stored after create")
		assert.Equal(t, policyFixture("rail-a", nil).Labels, stored.Labels, "stored labels")
		assert.Equal(t, policyFixture("rail-a", nil).Annotations, stored.Annotations, "stored annotations")
		assert.Equal(t, created.UID, stored.UID, "the public answer references the stored object")
		assert.Equal(t, created.ResourceVersion, stored.ResourceVersion, "the public answer references the stored revision")
	})

	t.Run("get", func(t *testing.T) {
		h, _, ctx := newPolicyStorage(t)
		created := seedPolicy(t, h, ctx, "rail-a", nil)

		obj, err := h.Get(ctx, "rail-a", nil)
		require.NoError(t, err)
		got, ok := obj.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "get must answer a *worker.DevicesRDMASelectionPolicy, got %T", obj)
		policySelStateOfSpec(got.Spec).assertEqual(t, policySelStateOfSpec(fullPolicySpec()), "got")
		assert.Equal(t, created.UID, got.UID)
		assert.Equal(t, created.ResourceVersion, got.ResourceVersion)
		assert.Equal(t, created.CreationTimestamp.Unix(), got.CreationTimestamp.Unix(),
			"the create answer and the stored readback carry the same creation second")
		assert.Equal(t, worker.SchemeGroupVersion.WithKind("DevicesRDMASelectionPolicy"), got.GroupVersionKind())

		_, err = h.Get(ctx, "absent", nil)
		assert.True(t, kerrors.IsNotFound(err), "a missing name is NotFound, got %v", err)
	})

	t.Run("list", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)
		seedPolicy(t, h, ctx, "rail-a", nil)
		seedPolicy(t, h, ctx, "rail-b", nil)

		obj, err := h.List(ctx, nil)
		require.NoError(t, err)
		list, ok := obj.(*worker.DevicesRDMASelectionPolicyList)
		require.True(t, ok, "list must answer a *worker.DevicesRDMASelectionPolicyList, got %T", obj)
		require.Len(t, list.Items, 2)
		assert.Equal(t, []string{"rail-a", "rail-b"}, []string{list.Items[0].Name, list.Items[1].Name})

		internal := new(workercore.DevicesRDMASelectionPolicyList)
		require.NoError(t, cli.List(ctx, internal))
		require.Len(t, internal.Items, 2)
		assert.Equal(t, internal.ResourceVersion, list.ResourceVersion,
			"the public list answers with the internal list revision")
		for i := range list.Items {
			policySelStateOfSpec(list.Items[i].Spec).assertEqual(t, policySelStateOfSpec(fullPolicySpec()), "listed item")
			assert.Equal(t, internal.Items[i].UID, list.Items[i].UID)
			assert.Equal(t, internal.Items[i].ResourceVersion, list.Items[i].ResourceVersion)
			assert.Equal(t, worker.SchemeGroupVersion.WithKind("DevicesRDMASelectionPolicy"), list.Items[i].GroupVersionKind())
		}

		table, err := h.ConvertToTable(ctx, list, nil)
		require.NoError(t, err)
		require.Len(t, table.Rows, 2)
		assert.Equal(t, "rail-a", table.Rows[0].Cells[0])
		assert.Equal(t, "rail-b", table.Rows[1].Cells[0])
	})

	t.Run("update", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)
		created := seedPolicy(t, h, ctx, "rail-a", nil)

		mutate := func(p *worker.DevicesRDMASelectionPolicy) {
			p.Spec.Include = []string{"mlx5_2"}
			p.Spec.Exclude = []string{"mlx5_9", "mlx5_10"}
		}
		// The input needs the revision a client read; the expectation is declared from
		// the fixture alone so a field lost on the readback cannot mirror into it.
		updated := readbackForUpdate(t, h, ctx, "rail-a", mutate)
		wantSpec := policyFixture("rail-a", mutate).Spec

		obj, _, err := h.Update(ctx, "rail-a", rest.DefaultUpdatedObjectInfo(updated), nil, nil, false, &meta.UpdateOptions{})
		require.NoError(t, err)
		got, ok := obj.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "update must answer a *worker.DevicesRDMASelectionPolicy, got %T", obj)

		stored := getInternalPolicy(t, cli, ctx, "rail-a")
		policySelStateOfSpec(stored.Spec).assertEqual(t, policySelStateOfSpec(wantSpec), "stored after update")
		assert.Equal(t, created.UID, stored.UID, "update keeps the object identity")
		assert.Equal(t, created.CreationTimestamp.Unix(), stored.CreationTimestamp.Unix(),
			"update keeps the creation second")
		assert.Equal(t, created.UID, got.UID)
		assert.NotEqual(t, created.ResourceVersion, got.ResourceVersion, "update advances the revision")
	})

	t.Run("update_missing_is_not_found", func(t *testing.T) {
		h, _, ctx := newPolicyStorage(t)

		_, _, err := h.Update(ctx, "absent",
			rest.DefaultUpdatedObjectInfo(policyFixture("absent", nil)), nil, nil, false, &meta.UpdateOptions{})
		assert.True(t, kerrors.IsNotFound(err), "an update without create-on-update is NotFound, got %v", err)
	})

	t.Run("update_with_force_allow_create_creates", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)

		obj, createdNow, err := h.Update(ctx, "created-by-update",
			rest.DefaultUpdatedObjectInfo(policyFixture("created-by-update", nil)), nil, nil, true, &meta.UpdateOptions{})
		require.NoError(t, err)
		assert.True(t, createdNow, "create-on-update reports that it created")
		_, ok := obj.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "create-on-update must answer a *worker.DevicesRDMASelectionPolicy, got %T", obj)

		stored := getInternalPolicy(t, cli, ctx, "created-by-update")
		policySelStateOfSpec(stored.Spec).assertEqual(t, policySelStateOfSpec(fullPolicySpec()), "stored create-on-update")
	})

	t.Run("delete", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)
		kept := seedPolicy(t, h, ctx, "rail-a", nil)
		removed := seedPolicy(t, h, ctx, "rail-b", nil)

		obj, _, err := h.Delete(ctx, "rail-b", nil, nil)
		require.NoError(t, err)
		st, ok := obj.(*meta.Status)
		require.True(t, ok, "delete must answer a Status, got %T", obj)
		assert.Equal(t, meta.StatusSuccess, st.Status)
		require.NotNil(t, st.Details)
		assert.Equal(t, "rail-b", st.Details.Name)
		assert.Equal(t, worker.GroupName, st.Details.Group)
		assert.Equal(t, removed.UID, st.Details.UID, "the delete status references the removed object")

		stored := getInternalPolicy(t, cli, ctx, "rail-a")
		assert.Equal(t, kept.UID, stored.UID, "only the requested object is removed")
		err = cli.Get(ctx, ctrlcli.ObjectKey{Name: "rail-b"}, new(workercore.DevicesRDMASelectionPolicy))
		assert.True(t, kerrors.IsNotFound(err), "delete removed the internal object, got %v", err)

		_, _, err = h.Delete(ctx, "absent", nil, nil)
		assert.True(t, kerrors.IsNotFound(err), "deleting a missing name is NotFound, got %v", err)
	})

	t.Run("collection_delete", func(t *testing.T) {
		h, cli, ctx := newPolicyStorage(t)
		seedPolicy(t, h, ctx, "rail-a", nil)
		seedPolicy(t, h, ctx, "rail-b", nil)

		obj, err := h.DeleteCollection(ctx, nil, nil, nil)
		require.NoError(t, err)
		list, ok := obj.(*worker.DevicesRDMASelectionPolicyList)
		require.True(t, ok, "collection delete must answer a list, got %T", obj)
		assert.Len(t, list.Items, 2, "the answer names the removed objects")

		internal := new(workercore.DevicesRDMASelectionPolicyList)
		require.NoError(t, cli.List(ctx, internal))
		assert.Empty(t, internal.Items, "collection delete emptied the internal store")
	})
}

// TestDevicesRDMASelectionPolicyHandler_IncludeSemantics checks proxied replacement
// of a named include and removal of the restriction.
func TestDevicesRDMASelectionPolicyHandler_IncludeSemantics(t *testing.T) {
	type includeSemanticsCase struct {
		name        string
		seedSpec    workercore.DevicesRDMASelectionPolicySpec
		updatedSpec workercore.DevicesRDMASelectionPolicySpec
		want        policySelState
	}
	cases := []includeSemanticsCase{
		{
			name:        "a_named_include_cleared_to_nil_removes_the_restriction",
			seedSpec:    fullPolicySpec(),
			updatedSpec: workercore.DevicesRDMASelectionPolicySpec{NodeSelector: policySelector(), Exclude: []string{"mlx5_9"}},
			want:        policySelState{selector: policySelector(), exclude: []string{"mlx5_9"}},
		},
		{
			name: "an_absent_include_gains_names",
			seedSpec: workercore.DevicesRDMASelectionPolicySpec{
				NodeSelector: policySelector(), Exclude: []string{"mlx5_9"},
			},
			updatedSpec: workercore.DevicesRDMASelectionPolicySpec{
				NodeSelector: policySelector(),
				Include:      []string{"mlx5_0"},
				Exclude:      []string{"mlx5_9"},
			},
			want: policySelState{
				selector: policySelector(), includePresent: true,
				names: []string{"mlx5_0"}, exclude: []string{"mlx5_9"},
			},
		},
		{
			name:     "a_replace_that_omits_the_selector_stores_none",
			seedSpec: fullPolicySpec(),
			updatedSpec: workercore.DevicesRDMASelectionPolicySpec{
				Include: []string{"mlx5_0", "mlx5_1"},
				Exclude: []string{"mlx5_9"},
			},
			want: policySelState{
				includePresent: true,
				names:          []string{"mlx5_0", "mlx5_1"}, exclude: []string{"mlx5_9"},
			},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, cli, ctx := newPolicyStorage(t)
			_, err := h.Create(ctx, &worker.DevicesRDMASelectionPolicy{
				ObjectMeta: meta.ObjectMeta{
					Name:        "rail-a",
					Labels:      map[string]string{"gpustack.ai/profile": "ib-rail-a"},
					Annotations: map[string]string{"gpustack.ai/description": "rail-a HCAs"},
				},
				Spec: c.seedSpec,
			}, nil, &meta.CreateOptions{})
			require.NoError(t, err)

			input := readbackForUpdate(t, h, ctx, "rail-a", func(*worker.DevicesRDMASelectionPolicy) {})
			input.Spec = c.updatedSpec
			_, _, err = h.Update(ctx, "rail-a", rest.DefaultUpdatedObjectInfo(input), nil, nil, false, &meta.UpdateOptions{})
			require.NoError(t, err)

			stored := getInternalPolicy(t, cli, ctx, "rail-a")
			policySelStateOfSpec(stored.Spec).assertEqual(t, c.want, "stored")

			obj, err := h.Get(ctx, "rail-a", nil)
			require.NoError(t, err)
			policySelStateOfSpec(obj.(*worker.DevicesRDMASelectionPolicy).Spec).assertEqual(t, c.want, "read back")
		})
	}
}

// TestDevicesRDMASelectionPolicyHandler_MergePatchThroughStorage exercises the patch
// update path end to end: a JSON merge patch is applied to the read public object with
// the apiserver's merge-patch library and fed back through the real storage Update.
// Under RFC 7386, null removes a member and a list replaces the old list.
// This uses a fake client, not an HTTP field manager or a real apiserver.
func TestDevicesRDMASelectionPolicyHandler_MergePatchThroughStorage(t *testing.T) {
	cases := []struct {
		name  string
		patch string
		want  policySelState
	}{
		{
			name:  "a_new_include_replaces_the_old_list",
			patch: `{"spec":{"include":["mlx5_2"]}}`,
			want: policySelState{
				selector: policySelector(), includePresent: true, names: []string{"mlx5_2"}, exclude: []string{"mlx5_9"},
			},
		},
		{
			name:  "exclude_null_removes_the_exclusions",
			patch: `{"spec":{"exclude":null}}`,
			want: policySelState{
				selector: policySelector(), includePresent: true, names: []string{"mlx5_0", "mlx5_1"},
			},
		},
		{
			name:  "include_null_removes_the_restriction",
			patch: `{"spec":{"include":null}}`,
			want:  policySelState{selector: policySelector(), exclude: []string{"mlx5_9"}},
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			h, cli, ctx := newPolicyStorage(t)
			created := seedPolicy(t, h, ctx, "rail-a", nil)

			// The patch applies to the public object exactly as read.
			read, err := h.Get(ctx, "rail-a", nil)
			require.NoError(t, err)
			current, merr := json.Marshal(read)
			require.NoError(t, merr)

			require.True(t, json.Valid([]byte(c.patch)), "test setup: the patch fixture must be well-formed JSON")
			merged, err := jsonpatch.MergePatch(current, []byte(c.patch))
			require.NoError(t, err)

			patched := new(worker.DevicesRDMASelectionPolicy)
			require.NoError(t, runtime.DecodeInto(scheme.Codecs.UniversalDeserializer(), merged, patched))

			updated, _, err := h.Update(ctx, "rail-a", rest.DefaultUpdatedObjectInfo(patched), nil, nil, false, &meta.UpdateOptions{})
			require.NoError(t, err)
			got, ok := updated.(*worker.DevicesRDMASelectionPolicy)
			require.True(t, ok, "update must answer a *worker.DevicesRDMASelectionPolicy, got %T", updated)

			stored := getInternalPolicy(t, cli, ctx, "rail-a")
			policySelStateOfSpec(stored.Spec).assertEqual(t, c.want, "stored")

			obj, err := h.Get(ctx, "rail-a", nil)
			require.NoError(t, err)
			back := obj.(*worker.DevicesRDMASelectionPolicy)
			policySelStateOfSpec(back.Spec).assertEqual(t, c.want, "read back")
			assert.Equal(t, created.UID, back.UID, "the patch update kept the identity")
			assert.Equal(t, got.ResourceVersion, back.ResourceVersion)
			assert.Equal(t, policyFixture("", nil).Labels, back.Labels, "the patch update left metadata alone")
		})
	}
}

// TestDevicesRDMASelectionPolicyHandler_OptionsAndErrors pins that the request options
// reach the internal client call as they were given, that a sentinel API error from
// behind the proxy answers with the same code, reason and message, and that a canceled
// request context reaches the internal call and surfaces its cause.
func TestDevicesRDMASelectionPolicyHandler_OptionsAndErrors(t *testing.T) {
	conflict := kerrors.NewConflict(worker.Resource(_DevicesRDMASelectionPolicyResource), "rail-a",
		errors.New("the object has been modified"))
	unavailable := kerrors.NewServiceUnavailable("the internal reader is down")

	t.Run("options_reach_the_internal_call", func(t *testing.T) {
		var gotCreate ctrlcli.CreateOptions
		var gotUpdate ctrlcli.UpdateOptions
		var gotGet ctrlcli.GetOptions
		var gotList ctrlcli.ListOptions
		var gotDelete ctrlcli.DeleteOptions
		funcs := interceptor.Funcs{
			Create: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				for _, o := range opts {
					o.ApplyToCreate(&gotCreate)
				}
				return cli.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				for _, o := range opts {
					o.ApplyToUpdate(&gotUpdate)
				}
				return cli.Update(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				for _, o := range opts {
					o.ApplyToGet(&gotGet)
				}
				return cli.Get(ctx, key, obj, opts...)
			},
			List: func(ctx context.Context, cli ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
				for _, o := range opts {
					o.ApplyToList(&gotList)
				}
				return cli.List(ctx, list, opts...)
			},
			Delete: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
				for _, o := range opts {
					o.ApplyToDelete(&gotDelete)
				}
				return cli.Delete(ctx, obj, opts...)
			},
		}
		h, _, ctx := newPolicyStorageWith(t, funcs)
		seedPolicy(t, h, ctx, "rail-a", nil)

		_, err := h.Create(ctx, policyFixture("with-options", nil), nil, &meta.CreateOptions{
			FieldManager: "public-v1", FieldValidation: meta.FieldValidationWarn,
		})
		require.NoError(t, err)
		assert.Equal(t, "public-v1", gotCreate.FieldManager, "create field manager")
		assert.Equal(t, meta.FieldValidationWarn, gotCreate.FieldValidation, "create field validation")

		selector, err := labels.Parse("gpustack.ai/profile=ib-rail-a")
		require.NoError(t, err)
		_, err = h.List(ctx, &metainternal.ListOptions{
			Limit: 5, Continue: "tok", LabelSelector: selector,
		})
		require.NoError(t, err)
		assert.Equal(t, int64(5), gotList.Limit, "list limit")
		assert.Equal(t, "tok", gotList.Continue, "list continue")
		assert.Equal(t, selector.String(), gotList.LabelSelector.String(), "list label selector")
		assert.Empty(t, gotList.Namespace, "the cluster-scoped list carries no namespace")

		_, err = h.Get(ctx, "rail-a", &meta.GetOptions{ResourceVersion: "7"})
		require.NoError(t, err)
		require.NotNil(t, gotGet.Raw, "get options carry the raw form")
		assert.Equal(t, "7", gotGet.Raw.ResourceVersion, "get resource version")

		input := readbackForUpdate(t, h, ctx, "rail-a", func(*worker.DevicesRDMASelectionPolicy) {})
		_, _, err = h.Update(ctx, "rail-a",
			rest.DefaultUpdatedObjectInfo(input), nil, nil, false, &meta.UpdateOptions{FieldManager: "public-v1"})
		require.NoError(t, err)
		assert.Equal(t, "public-v1", gotUpdate.FieldManager, "update field manager")

		_, _, err = h.Delete(ctx, "rail-a", nil, &meta.DeleteOptions{
			GracePeriodSeconds: ptr.To[int64](30), PropagationPolicy: ptr.To(meta.DeletePropagationForeground),
		})
		require.NoError(t, err)
		assert.Equal(t, int64(30), ptr.Deref(gotDelete.GracePeriodSeconds, 0), "delete grace period")
		assert.Equal(t, meta.DeletePropagationForeground, ptr.Deref(gotDelete.PropagationPolicy, ""), "delete propagation")
	})

	// sentinelFuncs answers every internal call with the given error, so each verb can
	// be checked against the sentinel the proxy received.
	sentinelFuncs := func(sentinel error) interceptor.Funcs {
		return interceptor.Funcs{
			Create: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				return sentinel
			},
			Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				return sentinel
			},
			List: func(ctx context.Context, cli ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) error {
				return sentinel
			},
			Watch: func(ctx context.Context, cli ctrlcli.WithWatch, list ctrlcli.ObjectList, opts ...ctrlcli.ListOption) (watch.Interface, error) {
				return nil, sentinel
			},
			Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				return sentinel
			},
			Delete: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.DeleteOption) error {
				return sentinel
			},
		}
	}

	t.Run("create_conflict", func(t *testing.T) {
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(conflict))
		_, err := h.Create(ctx, policyFixture("rail-a", nil), nil, &meta.CreateOptions{})
		requireSameStatus(t, conflict, err)
	})

	t.Run("get_not_found", func(t *testing.T) {
		notFound := kerrors.NewNotFound(worker.Resource(_DevicesRDMASelectionPolicyResource), "absent")
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(notFound))
		_, err := h.Get(ctx, "absent", nil)
		requireSameStatus(t, notFound, err)
		assert.True(t, kerrors.IsNotFound(err), "the NotFound meaning survives the proxy")
	})

	t.Run("list_unavailable", func(t *testing.T) {
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(unavailable))
		_, err := h.List(ctx, nil)
		requireSameStatus(t, unavailable, err)
		assert.True(t, kerrors.IsServiceUnavailable(err), "the unavailable meaning survives the proxy")
	})

	t.Run("watch_refused", func(t *testing.T) {
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(unavailable))
		_, err := h.Watch(ctx, nil)
		requireSameStatus(t, unavailable, err)
	})

	t.Run("update_conflict", func(t *testing.T) {
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(conflict),
			policyAsInternal(policyFixture("rail-a", nil)))
		_, _, err := h.Update(ctx, "rail-a",
			rest.DefaultUpdatedObjectInfo(policyFixture("rail-a", nil)), nil, nil, false, &meta.UpdateOptions{})
		requireSameStatus(t, conflict, err)
	})

	t.Run("delete_conflict", func(t *testing.T) {
		h, _, ctx := newPolicyStorageWith(t, sentinelFuncs(conflict),
			policyAsInternal(policyFixture("rail-a", nil)))
		_, _, err := h.Delete(ctx, "rail-a", nil, nil)
		requireSameStatus(t, conflict, err)
	})

	t.Run("a_canceled_context_answers_its_cause", func(t *testing.T) {
		// The interceptor honors the context the way every real clientset does, so a
		// canceled request reaching this call is the proxy's own propagation.
		honorCtx := interceptor.Funcs{
			Create: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return cli.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, cli ctrlcli.WithWatch, key ctrlcli.ObjectKey, obj ctrlcli.Object, opts ...ctrlcli.GetOption) error {
				if err := ctx.Err(); err != nil {
					return err
				}
				return cli.Get(ctx, key, obj, opts...)
			},
		}
		h, cli, _ := newPolicyStorageWith(t, honorCtx)
		live := requestContext("", _DevicesRDMASelectionPolicyResource)
		_, err := h.Create(live, policyFixture("rail-a", nil), nil, &meta.CreateOptions{})
		require.NoError(t, err, "test setup: the live context must pass the honoring interceptor")

		canceled, cancel := context.WithCancel(requestContext("", _DevicesRDMASelectionPolicyResource))
		cancel()

		_, err = h.Create(canceled, policyFixture("rail-b", nil), nil, &meta.CreateOptions{})
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled", "the create answers the cancellation cause")
		var gs kerrors.APIStatus
		require.ErrorAs(t, err, &gs)
		assert.Equal(t, int32(500), gs.Status().Code, "a canceled call is not mistaken for an API refusal")

		err = cli.Get(canceled, ctrlcli.ObjectKey{Name: "rail-b"}, new(workercore.DevicesRDMASelectionPolicy))
		require.ErrorIs(t, err, context.Canceled, "the canceled context reached the internal call")

		_, err = h.Get(canceled, "rail-a", nil)
		require.Error(t, err)
		assert.Contains(t, err.Error(), "context canceled", "the get answers the cancellation cause")
	})
}

// startPolicyWatch builds a handler whose upstream watch is a caller-driven fake and
// starts it, answering the proxy stream, the fake, and the cancel func, so each watch
// subtest owns its events without depending on a sibling.
func startPolicyWatch(t *testing.T) (dw watch.Interface, fw *watch.FakeWatcher, cancel func()) {
	t.Helper()
	base := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	fw = watch.NewFake()
	h := newPolicyHandler(t, &fakeFlavorWatchClient{WithWatch: base, fw: fw})
	ctx, cancel := context.WithCancel(context.Background())
	w, err := h.Watch(ctx, nil)
	require.NoError(t, err)
	return w, fw, cancel
}

// policyRecv receives one event from the proxy stream, failing on a hang or an early
// close rather than polling or sleeping.
func policyRecv(t *testing.T, dw watch.Interface) watch.Event {
	t.Helper()
	select {
	case e, ok := <-dw.ResultChan():
		require.True(t, ok, "watch channel closed unexpectedly")
		return e
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for a watch event")
		return watch.Event{}
	}
}

// TestDevicesRDMASelectionPolicyHandler_Watch drives the proxied watch over a fake
// watcher: internal events surface as public objects with their complete spec, a
// revision already announced is not delivered again unless it carries a deletion,
// foreign objects are dropped, and a canceled context closes the stream.
func TestDevicesRDMASelectionPolicyHandler_Watch(t *testing.T) {
	t.Run("internal_event_surfaces_as_public_object", func(t *testing.T) {
		dw, fw, cancel := startPolicyWatch(t)
		defer cancel()
		defer dw.Stop()

		fw.Add(internalPolicy("rail-a", "2"))

		e := policyRecv(t, dw)
		assert.Equal(t, watch.Added, e.Type)
		p, ok := e.Object.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "the event must carry a *worker.DevicesRDMASelectionPolicy, got %T", e.Object)
		assert.Equal(t, "rail-a", p.Name)
		assert.Equal(t, worker.SchemeGroupVersion.WithKind("DevicesRDMASelectionPolicy"), p.GroupVersionKind(),
			"the cast stamps the public GVK")
		policySelStateOfSpec(p.Spec).assertEqual(t, policySelStateOfSpec(fullPolicySpec()), "watch event")
		assert.Equal(t, "2", p.ResourceVersion)
	})

	t.Run("duplicate_revision_is_dropped", func(t *testing.T) {
		dw, fw, cancel := startPolicyWatch(t)
		defer cancel()
		defer dw.Stop()

		// The watch chain is unbuffered end to end, so events are handed over one at a
		// time: announce the revision, then offer its duplicate.
		fw.Add(internalPolicy("rail-a", "2"))
		e := policyRecv(t, dw)
		require.Equal(t, watch.Added, e.Type, "test setup: the announced revision must be consumed first")

		fw.Add(internalPolicy("rail-a", "2"))
		fw.Add(internalPolicy("rail-a", "3"))

		e = policyRecv(t, dw)
		p, ok := e.Object.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "the next event must be the revision 3 one, got %v", e)
		assert.Equal(t, "3", p.ResourceVersion, "the repeated revision 2 event was dropped")
	})

	t.Run("deletion_at_announced_revision_is_delivered", func(t *testing.T) {
		dw, fw, cancel := startPolicyWatch(t)
		defer cancel()
		defer dw.Stop()

		fw.Add(internalPolicy("rail-a", "2"))
		e := policyRecv(t, dw)
		require.Equal(t, watch.Added, e.Type, "test setup: the announced revision must be consumed first")

		deleted := internalPolicy("rail-a", "2")
		now := meta.Now()
		deleted.DeletionTimestamp = &now
		fw.Action(watch.Modified, deleted)

		e = policyRecv(t, dw)
		assert.Equal(t, watch.Modified, e.Type)
		p, ok := e.Object.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "the deletion event must carry a *worker.DevicesRDMASelectionPolicy, got %T", e.Object)
		assert.NotNil(t, p.DeletionTimestamp, "the deletion passes the revision dedup")
	})

	t.Run("foreign_object_is_dropped", func(t *testing.T) {
		dw, fw, cancel := startPolicyWatch(t)
		defer cancel()
		defer dw.Stop()

		fw.Add(&workercore.Devices{ObjectMeta: meta.ObjectMeta{Name: "stray", ResourceVersion: "4"}})
		fw.Add(internalPolicy("rail-a", "5"))

		e := policyRecv(t, dw)
		p, ok := e.Object.(*worker.DevicesRDMASelectionPolicy)
		require.True(t, ok, "the next event must be the policy one, got %v", e)
		assert.Equal(t, "5", p.ResourceVersion, "the foreign object was dropped")
	})

	t.Run("cancel_closes_the_stream", func(t *testing.T) {
		dw, _, cancel := startPolicyWatch(t)
		defer dw.Stop()

		cancel()

		select {
		case e, ok := <-dw.ResultChan():
			assert.False(t, ok, "the stream closed without an event, got %v", e)
		case <-time.After(2 * time.Second):
			t.Fatal("timed out waiting for the stream to close")
		}
	})
}

// TestDevicesRDMASelectionPolicyHandler_ScopeAndStatusShape pins the shape the proxy
// declares: the resource is cluster scoped, so a request context without a namespace
// resolves it, and no status subresource exists on either version for the storage to
// route.
func TestDevicesRDMASelectionPolicyHandler_ScopeAndStatusShape(t *testing.T) {
	h, cli, _ := newPolicyStorage(t)

	assert.False(t, h.NamespaceScoped(), "the policy is cluster scoped")
	_, hasStatus := h.New().(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "the public type serves no status subresource")
	_, hasStatus = any(policyAsInternal(policyFixture("", nil))).(extensionapi.ObjectWithStatusSubResource)
	assert.False(t, hasStatus, "the internal type serves no status subresource")

	// A cluster-scoped key needs no namespace: a request context carrying only the
	// request info must resolve the object.
	nsless := genericapirequest.WithRequestInfo(context.Background(), &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		APIGroup:          worker.GroupName,
		APIVersion:        worker.GroupVersion.Version,
		Resource:          _DevicesRDMASelectionPolicyResource,
	})
	created := seedPolicy(t, h, nsless, "rail-a", nil)

	obj, err := h.Get(nsless, "rail-a", nil)
	require.NoError(t, err, "a namespace-less context resolves a cluster-scoped object")
	assert.Equal(t, created.UID, obj.(*worker.DevicesRDMASelectionPolicy).UID)

	_, err = h.List(nsless, nil)
	require.NoError(t, err, "a namespace-less context lists a cluster-scoped resource")
	internal := new(workercore.DevicesRDMASelectionPolicyList)
	require.NoError(t, cli.List(nsless, internal))
	require.Len(t, internal.Items, 1)
}
