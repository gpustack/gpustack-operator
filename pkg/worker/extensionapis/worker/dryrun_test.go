package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	storage "k8s.io/api/storage/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	apivalidation "k8s.io/apimachinery/pkg/api/validation"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation/field"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	worker "gpustack.ai/gpustack/api/worker/v1"
	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/extensionapi"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/system"
)

var (
	dryRunCreate = &meta.CreateOptions{DryRun: []string{meta.DryRunAll}}
	dryRunUpdate = &meta.UpdateOptions{DryRun: []string{meta.DryRunAll}}
)

// admittingClient returns a fake client that stands in for the api server behind a handler: admit runs
// on every create, update and patch before the dry-run option is read, as the api server runs its
// admission, and the fake then persists nothing for a dry run.
func admittingClient(admit func(ctrlcli.Object) error, objs ...ctrlcli.Object) ctrlcli.WithWatch {
	return ctrlfake.NewClientBuilder().
		WithScheme(scheme.Scheme).
		WithObjects(objs...).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.CreateOption) error {
				if err := admit(obj); err != nil {
					return err
				}
				return cli.Create(ctx, obj, opts...)
			},
			Update: func(ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, opts ...ctrlcli.UpdateOption) error {
				if err := admit(obj); err != nil {
					return err
				}
				return cli.Update(ctx, obj, opts...)
			},
			Patch: func(
				ctx context.Context, cli ctrlcli.WithWatch, obj ctrlcli.Object, patch ctrlcli.Patch, opts ...ctrlcli.PatchOption,
			) error {
				if err := admit(obj); err != nil {
					return err
				}
				return cli.Patch(ctx, obj, patch, opts...)
			},
		}).
		Build()
}

// admitAll admits every write.
func admitAll(ctrlcli.Object) error {
	return nil
}

// requestContext returns the context the aggregated api server hands a storage for a request on the
// given resource.
func requestContext(namespace, resource string) context.Context {
	ctx := genericapirequest.WithNamespace(context.Background(), namespace)
	return genericapirequest.WithRequestInfo(ctx, &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		APIGroup:          worker.GroupName,
		APIVersion:        worker.GroupVersion.Version,
		Namespace:         namespace,
		Resource:          resource,
	})
}

// requireSameRefusal requires both errors to refuse the request with the same status, message and
// field causes, and the wanted one to name the given field.
func requireSameRefusal(t *testing.T, want, got error, field string) {
	t.Helper()

	require.Error(t, want)
	require.Error(t, got, "the dry run was admitted where the real request is refused")
	var ws, gs kerrors.APIStatus
	require.ErrorAs(t, want, &ws)
	require.ErrorAs(t, got, &gs)
	require.NotNil(t, ws.Status().Details)
	require.NotEmpty(t, ws.Status().Details.Causes)
	assert.Equal(t, field, ws.Status().Details.Causes[0].Field)
	require.NotNil(t, gs.Status().Details)
	assert.Equal(t, ws.Status().Code, gs.Status().Code)
	assert.Equal(t, ws.Status().Reason, gs.Status().Reason)
	assert.Equal(t, ws.Status().Message, gs.Status().Message)
	assert.Equal(t, ws.Status().Details.Causes, gs.Status().Details.Causes)
}

// transformTo returns an UpdatedObjectInfo that derives the updated object from the existing one, the
// way the api server applies a patch.
func transformTo[T runtime.Object](mutate func(T)) rest.UpdatedObjectInfo {
	return rest.DefaultUpdatedObjectInfo(nil,
		func(_ context.Context, _, oldObj runtime.Object) (runtime.Object, error) {
			obj := oldObj.DeepCopyObject().(T)
			mutate(obj)
			return obj, nil
		})
}

// admitInstanceCPU refuses an Instance requesting more than 16 CPUs, in the shape the Instance
// webhook refuses it with.
func admitInstanceCPU(obj ctrlcli.Object) error {
	inst, ok := obj.(*workercore.Instance)
	if !ok || inst.Spec.Resources == nil || inst.Spec.Resources.CPU.Cmp(resource.MustParse("16")) <= 0 {
		return nil
	}
	return kerrors.NewInvalid(workercore.Kind("Instance"), inst.Name, field.ErrorList{
		field.Invalid(field.NewPath("spec", "resources", "cpu"), inst.Spec.Resources.CPU.String(),
			"exceeds the maximum CPU request 16"),
	})
}

func newInstance(namespace, name, cpu string) *workercore.Instance {
	return &workercore.Instance{
		ObjectMeta: meta.ObjectMeta{Namespace: namespace, Name: name},
		Spec: workercore.InstanceSpec{
			Type: "cpu",
			InstanceTemplate: workercore.InstanceTemplate{
				Resources: &workercore.InstanceResources{CPU: resource.MustParse(cpu)},
			},
		},
	}
}

// TestInstanceHandler_DryRun pins that a v1 dry run is refused with the error the v1alpha1 request
// behind it is refused with, on create, update and patch alike, and that an admitted one persists
// nothing.
func TestInstanceHandler_DryRun(t *testing.T) {
	// CastObjectFrom reads settings through the loopback kube client.
	system.LoopbackKubeClient.Configure(kubefake.NewSimpleClientset())

	const ns = "default"
	cli := admittingClient(admitInstanceCPU, newInstance(ns, "existing", "4"))
	h := &InstanceHandler{}
	h.ObjectInfo = &worker.Instance{}
	h.CurdOperations = extensionapi.WithCurdProxy[
		*worker.Instance, *worker.InstanceList, *workercore.Instance, *workercore.InstanceList,
	](nil, h, cli, cli)
	ctx := requestContext(ns, _InstanceResource)

	getExisting := func(t *testing.T) *workercore.Instance {
		t.Helper()
		inst := new(workercore.Instance)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "existing"}, inst))
		return inst
	}
	setCPU := func(cpu string) func(*worker.Instance) {
		return func(inst *worker.Instance) {
			inst.Spec.Resources.CPU = resource.MustParse(cpu)
		}
	}

	t.Run("create refused", func(t *testing.T) {
		want := cli.Create(ctx, newInstance(ns, "refused", "60"), ctrlcli.DryRunAll)

		_, got := h.Create(ctx, (*worker.Instance)(newInstance(ns, "refused", "60")), nil, dryRunCreate)
		requireSameRefusal(t, want, got, "spec.resources.cpu")
	})

	t.Run("create admitted, not persisted", func(t *testing.T) {
		obj, err := h.Create(ctx, (*worker.Instance)(newInstance(ns, "admitted", "4")), nil, dryRunCreate)
		require.NoError(t, err)
		assert.Equal(t, "admitted", obj.(*worker.Instance).Name)

		err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "admitted"}, new(workercore.Instance))
		assert.True(t, kerrors.IsNotFound(err), "the dry run persisted the instance: %v", err)
	})

	t.Run("update refused", func(t *testing.T) {
		inst := getExisting(t)
		inst.Spec.Resources.CPU = resource.MustParse("60")
		want := cli.Update(ctx, inst.DeepCopy(), ctrlcli.DryRunAll)

		_, _, got := h.Update(ctx, "existing",
			rest.DefaultUpdatedObjectInfo((*worker.Instance)(inst)), nil, nil, false, dryRunUpdate)
		requireSameRefusal(t, want, got, "spec.resources.cpu")
	})

	t.Run("update admitted, not persisted", func(t *testing.T) {
		inst := getExisting(t)
		inst.Spec.Resources.CPU = resource.MustParse("8")

		obj, _, err := h.Update(ctx, "existing",
			rest.DefaultUpdatedObjectInfo((*worker.Instance)(inst)), nil, nil, false, dryRunUpdate)
		require.NoError(t, err)
		assert.Equal(t, "8", obj.(*worker.Instance).Spec.Resources.CPU.String())

		assert.Equal(t, "4", getExisting(t).Spec.Resources.CPU.String(), "the dry run persisted the update")
	})

	t.Run("patch refused", func(t *testing.T) {
		inst := getExisting(t)
		patched := inst.DeepCopy()
		patched.Spec.Resources.CPU = resource.MustParse("60")
		want := cli.Patch(ctx, patched, ctrlcli.MergeFrom(inst), ctrlcli.DryRunAll)

		_, _, got := h.Update(ctx, "existing", transformTo(setCPU("60")), nil, nil, false, dryRunUpdate)
		requireSameRefusal(t, want, got, "spec.resources.cpu")
	})

	t.Run("patch admitted, not persisted", func(t *testing.T) {
		obj, _, err := h.Update(ctx, "existing", transformTo(setCPU("8")), nil, nil, false, dryRunUpdate)
		require.NoError(t, err)
		assert.Equal(t, "8", obj.(*worker.Instance).Spec.Resources.CPU.String())

		assert.Equal(t, "4", getExisting(t).Spec.Resources.CPU.String(), "the dry run persisted the patch")
	})
}

// TestInstanceImagePullSecretHandler_DryRun pins that a dry run is refused by the validation the real
// request is refused by, and that an admitted one persists no Secret.
func TestInstanceImagePullSecretHandler_DryRun(t *testing.T) {
	const ns = "default"
	cli := admittingClient(admitAll)
	h := &InstanceImagePullSecretHandler{Client: cli, APIReader: cli}
	h.ObjectInfo = &worker.InstanceImagePullSecret{}
	h.CurdOperations = extensionapi.WithCurd(nil, h)
	ctx := requestContext(ns, _InstanceImagePullSecretResource)

	newObj := func(name, registry string) *worker.InstanceImagePullSecret {
		return &worker.InstanceImagePullSecret{
			ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name},
			Spec: worker.InstanceImagePullSecretSpec{
				Registry: registry,
				Username: "user",
				Password: "pass",
			},
		}
	}

	t.Run("create refused", func(t *testing.T) {
		_, want := h.Create(ctx, newObj("refused", ""), nil, &meta.CreateOptions{})

		_, got := h.Create(ctx, newObj("refused", ""), nil, dryRunCreate)
		requireSameRefusal(t, want, got, "spec.registry")
	})

	t.Run("create admitted, not persisted", func(t *testing.T) {
		_, err := h.Create(ctx, newObj("admitted", "registry.example.com"), nil, dryRunCreate)
		require.NoError(t, err)

		err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "admitted"}, new(core.Secret))
		assert.True(t, kerrors.IsNotFound(err), "the dry run persisted the secret: %v", err)
	})

	_, err := h.Create(ctx, newObj("existing", "registry.example.com"), nil, &meta.CreateOptions{})
	require.NoError(t, err)
	getExisting := func(t *testing.T) *core.Secret {
		t.Helper()
		sec := new(core.Secret)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "existing"}, sec))
		return sec
	}
	setRegistry := func(registry string) func(*worker.InstanceImagePullSecret) {
		return func(obj *worker.InstanceImagePullSecret) {
			obj.Spec = newObj("", registry).Spec
		}
	}

	t.Run("update refused", func(t *testing.T) {
		_, _, want := h.Update(ctx, "existing", transformTo(setRegistry("")), nil, nil, false, &meta.UpdateOptions{})

		_, _, got := h.Update(ctx, "existing", transformTo(setRegistry("")), nil, nil, false, dryRunUpdate)
		requireSameRefusal(t, want, got, "spec.registry")
	})

	t.Run("update admitted, not persisted", func(t *testing.T) {
		before := getExisting(t)

		_, _, err := h.Update(ctx, "existing",
			transformTo(setRegistry("mirror.example.com")), nil, nil, false, dryRunUpdate)
		require.NoError(t, err)

		assert.Equal(t, before, getExisting(t), "the dry run persisted the update")
	})
}

// admitObjectMeta refuses an object whose metadata the api server refuses to store.
func admitObjectMeta(obj ctrlcli.Object) error {
	errs := apivalidation.ValidateObjectMetaAccessor(obj, true,
		apivalidation.NameIsDNSSubdomain, field.NewPath("metadata"))
	if len(errs) == 0 {
		return nil
	}
	return kerrors.NewInvalid(core.SchemeGroupVersion.WithKind("Secret").GroupKind(), obj.GetName(), errs)
}

// TestInstanceSSHPublicKeyHandler_DryRun pins that a dry run is refused by the admission of the Secret
// behind it, as the real request is, and that an admitted one persists no Secret.
func TestInstanceSSHPublicKeyHandler_DryRun(t *testing.T) {
	const ns = "default"
	cli := admittingClient(admitObjectMeta)
	h := &InstanceSSHPublicKeyHandler{Client: cli, APIReader: cli}
	h.ObjectInfo = &worker.InstanceSSHPublicKey{}
	h.CurdOperations = extensionapi.WithCurd(nil, h)
	ctx := requestContext(ns, _InstanceSSHPublicKeyResource)

	newObj := func(name string, labels map[string]string) *worker.InstanceSSHPublicKey {
		return &worker.InstanceSSHPublicKey{
			ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name, Labels: labels},
			Spec:       worker.InstanceSSHPublicKeySpec{Data: "ssh-ed25519 AAAA"},
		}
	}
	badLabels := map[string]string{"team": "-invalid-"}

	t.Run("create refused", func(t *testing.T) {
		_, want := h.Create(ctx, newObj("refused", badLabels), nil, &meta.CreateOptions{})

		_, got := h.Create(ctx, newObj("refused", badLabels), nil, dryRunCreate)
		requireSameRefusal(t, want, got, "metadata.labels")
	})

	t.Run("create admitted, not persisted", func(t *testing.T) {
		_, err := h.Create(ctx, newObj("admitted", nil), nil, dryRunCreate)
		require.NoError(t, err)

		err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "admitted"}, new(core.Secret))
		assert.True(t, kerrors.IsNotFound(err), "the dry run persisted the secret: %v", err)
	})

	_, err := h.Create(ctx, newObj("existing", nil), nil, &meta.CreateOptions{})
	require.NoError(t, err)
	getExisting := func(t *testing.T) *core.Secret {
		t.Helper()
		sec := new(core.Secret)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "existing"}, sec))
		return sec
	}

	t.Run("update refused", func(t *testing.T) {
		setBadLabels := transformTo(func(obj *worker.InstanceSSHPublicKey) { obj.Labels = badLabels })
		_, _, want := h.Update(ctx, "existing", setBadLabels, nil, nil, false, &meta.UpdateOptions{})

		_, _, got := h.Update(ctx, "existing", setBadLabels, nil, nil, false, dryRunUpdate)
		requireSameRefusal(t, want, got, "metadata.labels")
	})

	t.Run("update admitted, not persisted", func(t *testing.T) {
		before := getExisting(t)

		_, _, err := h.Update(ctx, "existing",
			transformTo(func(obj *worker.InstanceSSHPublicKey) { obj.Spec.Data = "ssh-rsa BBBB" }),
			nil, nil, false, dryRunUpdate)
		require.NoError(t, err)

		assert.Equal(t, before, getExisting(t), "the dry run persisted the update")
	})
}

// TestInstancePersistentVolumeHandler_DryRun pins that a dry run is refused by the validation the real
// request is refused by, and that an admitted one persists no PersistentVolumeClaim.
func TestInstancePersistentVolumeHandler_DryRun(t *testing.T) {
	const ns = "default"
	cli := admittingClient(admitAll, &storage.StorageClass{ObjectMeta: meta.ObjectMeta{Name: "standard"}})
	h := &InstancePersistentVolumeHandler{Client: cli, APIReader: cli}
	h.ObjectInfo = &worker.InstancePersistentVolume{}
	h.CurdOperations = extensionapi.WithCurd(nil, h)
	ctx := requestContext(ns, _InstancePersistentVolumeResource)

	newObj := func(name, typ string) *worker.InstancePersistentVolume {
		return &worker.InstancePersistentVolume{
			ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name},
			Spec:       worker.InstancePersistentVolumeSpec{Type: ptr.To(typ)},
		}
	}

	t.Run("create refused", func(t *testing.T) {
		_, want := h.Create(ctx, newObj("refused", "absent"), nil, &meta.CreateOptions{})

		_, got := h.Create(ctx, newObj("refused", "absent"), nil, dryRunCreate)
		requireSameRefusal(t, want, got, "spec.type")
	})

	t.Run("create admitted, not persisted", func(t *testing.T) {
		_, err := h.Create(ctx, newObj("admitted", "standard"), nil, dryRunCreate)
		require.NoError(t, err)

		err = cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "admitted"}, new(core.PersistentVolumeClaim))
		assert.True(t, kerrors.IsNotFound(err), "the dry run persisted the claim: %v", err)
	})

	_, err := h.Create(ctx, newObj("existing", "standard"), nil, &meta.CreateOptions{})
	require.NoError(t, err)
	getExisting := func(t *testing.T) *core.PersistentVolumeClaim {
		t.Helper()
		pvc := new(core.PersistentVolumeClaim)
		require.NoError(t, cli.Get(ctx, ctrlcli.ObjectKey{Namespace: ns, Name: "existing"}, pvc))
		return pvc
	}

	t.Run("update refused", func(t *testing.T) {
		grow := transformTo(func(obj *worker.InstancePersistentVolume) {
			obj.Spec.Capacity = resource.MustParse("40Gi")
		})
		_, _, want := h.Update(ctx, "existing", grow, nil, nil, false, &meta.UpdateOptions{})

		_, _, got := h.Update(ctx, "existing", grow, nil, nil, false, dryRunUpdate)
		requireSameRefusal(t, want, got, "spec.capacity")
	})

	t.Run("update admitted, not persisted", func(t *testing.T) {
		before := getExisting(t)

		_, _, err := h.Update(ctx, "existing",
			transformTo(func(obj *worker.InstancePersistentVolume) { obj.Spec.Description = "renamed" }),
			nil, nil, false, dryRunUpdate)
		require.NoError(t, err)

		assert.Equal(t, before, getExisting(t), "the dry run persisted the update")
	})
}

// TestInstancePersistentVolumeTypeHandler_DryRunWritesNothing pins that a dry run writes neither the
// StorageClass nor the S3 access Secret: the handler makes writes that do not carry the dry-run
// option, so a dry run must stop before them.
func TestInstancePersistentVolumeTypeHandler_DryRunWritesNothing(t *testing.T) {
	cli := admittingClient(admitAll)
	h := &InstancePersistentVolumeTypeHandler{Client: cli, APIReader: cli}
	h.ObjectInfo = &worker.InstancePersistentVolumeType{}
	h.CurdOperations = extensionapi.WithCurd(nil, h)
	ctx := requestContext("", _InstancePersistentVolumeTypeResource)

	newObj := func(name string) *worker.InstancePersistentVolumeType {
		return &worker.InstancePersistentVolumeType{
			ObjectMeta: meta.ObjectMeta{Name: name},
			Spec: worker.InstancePersistentVolumeTypeSpec{
				InstancePersistentVolumeSource: worker.InstancePersistentVolumeSource{
					S3: &worker.S3InstancePersistentVolumeSource{
						Endpoint:  "https://s3.example.com",
						Bucket:    "bucket",
						AccessKey: "access",
						SecretKey: "secret",
					},
				},
			},
		}
	}
	requireNoWrites := func(t *testing.T, stgClsBefore *storage.StorageClassList, secBefore *core.SecretList) {
		t.Helper()
		stgCls, secs := new(storage.StorageClassList), new(core.SecretList)
		require.NoError(t, cli.List(ctx, stgCls))
		require.NoError(t, cli.List(ctx, secs))
		assert.ElementsMatch(t, stgClsBefore.Items, stgCls.Items, "the dry run wrote a storage class")
		assert.ElementsMatch(t, secBefore.Items, secs.Items, "the dry run wrote a secret")
	}

	t.Run("create", func(t *testing.T) {
		_, err := h.Create(ctx, newObj("dry"), nil, dryRunCreate)
		require.NoError(t, err)

		requireNoWrites(t, new(storage.StorageClassList), new(core.SecretList))
	})

	t.Run("update", func(t *testing.T) {
		_, err := h.Create(ctx, newObj("existing"), nil, &meta.CreateOptions{})
		require.NoError(t, err)
		stgCls, secs := new(storage.StorageClassList), new(core.SecretList)
		require.NoError(t, cli.List(ctx, stgCls))
		require.NoError(t, cli.List(ctx, secs))
		require.NotEmpty(t, secs.Items, "the real create wrote no s3 access secret to compare against")

		_, _, err = h.Update(ctx, "existing",
			transformTo(func(obj *worker.InstancePersistentVolumeType) {
				obj.Spec.Description = "renamed"
				obj.Spec.S3.SecretKey = "rotated"
			}),
			nil, nil, false, dryRunUpdate)
		require.NoError(t, err)

		requireNoWrites(t, stgCls, secs)
	})
}
