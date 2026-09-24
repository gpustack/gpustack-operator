package extensionapi

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	genericapirequest "k8s.io/apiserver/pkg/endpoints/request"
	"k8s.io/apiserver/pkg/registry/rest"
	"k8s.io/utils/ptr"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	gpustack "gpustack.ai/gpustack/api/v1"
	kubefake "gpustack.ai/gpustack/pkg/kubeclients/kubernetes/fake"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
	"gpustack.ai/gpustack/pkg/setting"
	"gpustack.ai/gpustack/pkg/system"
	"gpustack.ai/gpustack/pkg/systemmeta"
)

// TestSettingHandler_DryRunWritesNothing pins that a dry-run update leaves the value of a setting as
// it is: Configure writes the delegated Secret without the dry-run option, so a dry run must stop
// before it.
func TestSettingHandler_DryRunWritesNothing(t *testing.T) {
	ns, name := setting.DelegatedSecretNamespace, setting.DelegatedSecretName
	sec := &core.Secret{
		ObjectMeta: meta.ObjectMeta{Namespace: ns, Name: name},
		Data:       map[string][]byte{"sample": []byte("old")},
	}
	systemmeta.NoteResource(sec, _SettingResource, nil)
	kubeCli := kubefake.NewSimpleClientset(sec.DeepCopy())
	system.LoopbackKubeClient.Configure(kubeCli)
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(sec.DeepCopy()).Build()

	ss := setting.Settings{}
	ss.NewEditable("sample", "A sample setting.", setting.InitializeFrom("old"), setting.Allow())
	h := NewSettingHandler(ss.Index)
	h.ObjectInfo = &gpustack.Setting{}
	h.GetOperation = WithGet(h)
	h.UpdateOperation = WithUpdate(h)
	h.Client, h.APIReader = cli, cli

	ctx := genericapirequest.WithNamespace(context.Background(), ns)
	ctx = genericapirequest.WithRequestInfo(ctx, &genericapirequest.RequestInfo{
		IsResourceRequest: true,
		APIGroup:          gpustack.GroupName,
		APIVersion:        gpustack.GroupVersion.Version,
		Namespace:         ns,
		Resource:          _SettingResource,
	})
	setNew := rest.DefaultUpdatedObjectInfo(nil,
		func(_ context.Context, _, oldObj runtime.Object) (runtime.Object, error) {
			set := oldObj.DeepCopyObject().(*gpustack.Setting)
			set.Spec.Value = ptr.To("new")
			return set, nil
		})
	getValue := func(t *testing.T) string {
		t.Helper()
		got, err := kubeCli.CoreV1().Secrets(ns).Get(ctx, name, meta.GetOptions{})
		require.NoError(t, err)
		return string(got.Data["sample"])
	}

	_, _, err := h.Update(ctx, "sample", setNew, nil, nil, false,
		&meta.UpdateOptions{DryRun: []string{meta.DryRunAll}})
	require.NoError(t, err)
	assert.Equal(t, "old", getValue(t), "the dry run configured the setting")

	// The same update without the dry-run option writes the value, so the read above can see a write.
	_, _, err = h.Update(ctx, "sample", setNew, nil, nil, false, &meta.UpdateOptions{})
	require.NoError(t, err)
	assert.Equal(t, "new", getValue(t))
}
