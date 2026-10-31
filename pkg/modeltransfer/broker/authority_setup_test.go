package broker

import (
	"context"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	kubefake "k8s.io/client-go/kubernetes/fake"
	ktesting "k8s.io/client-go/testing"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	workercore "gpustack.ai/gpustack/api/worker/v1alpha1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

func TestAuthorityInitializationConvergesAndNeverRecreatesPinnedState(t *testing.T) {
	ctx := context.Background()
	binding := &workercore.ModelStoreBinding{ObjectMeta: meta.ObjectMeta{Name: "grant", Namespace: "tenant", UID: "binding-uid"}, Spec: workercore.ModelStoreBindingSpec{SourceMemory: &workercore.ModelStoreBindingSourceMemory{Bytes: resource.MustParse("1Gi"), Registrations: 4, Sessions: 4}}}
	client := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(binding).Build()
	kube := kubefake.NewSimpleClientset()
	kube.PrependReactor("create", "configmaps", func(action ktesting.Action) (bool, runtime.Object, error) {
		object := action.(ktesting.CreateAction).GetObject().(*core.ConfigMap)
		object.UID = types.UID("authority-uid")
		object.ResourceVersion = "1"
		return false, nil, nil
	})
	maps := kube.CoreV1().ConfigMaps("operator")
	var wg sync.WaitGroup
	errs := make(chan error, 2)
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, _, err := EnsureReservationAuthority(ctx, client, maps, "operator", "tenant", "grant", binding.UID, Limits{})
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	live := &workercore.ModelStoreBinding{}
	require.NoError(t, client.Get(ctx, ctrlcli.ObjectKeyFromObject(binding), live))
	pin, err := ParseAuthorityPin(live.Annotations[AuthorityAnnotation])
	require.NoError(t, err)
	assert.Equal(t, "authority-uid", pin.UID)
	assert.Len(t, pin.Epoch, 64)
	cm, err := maps.Get(ctx, AuthorityName(binding.UID), meta.GetOptions{})
	require.NoError(t, err)
	require.NoError(t, ValidateAuthorityPin(cm, binding.UID, pin))
	reader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(live, cm).Build()
	entries, err := ReadPinnedAuthority(ctx, reader, "operator", live)
	require.NoError(t, err)
	assert.Empty(t, entries)
	changed := cm.DeepCopy()
	changed.UID = "replacement-uid"
	badReader := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).WithObjects(live, changed).Build()
	_, err = ReadPinnedAuthority(ctx, badReader, "operator", live)
	require.ErrorIs(t, err, ErrReservationUnavailable)
	require.NoError(t, maps.Delete(ctx, cm.Name, meta.DeleteOptions{}))
	_, _, err = EnsureReservationAuthority(ctx, client, maps, "operator", "tenant", "grant", binding.UID, Limits{})
	require.ErrorIs(t, err, ErrReservationUnavailable)
	_, err = maps.Get(ctx, cm.Name, meta.GetOptions{})
	require.Error(t, err)
}

func TestAuthorityPinRejectsMalformedValues(t *testing.T) {
	for _, value := range []string{"", `{}`, `{"UID":"x","Epoch":"bad"}`, `{"UID":"x","UID":"y","Epoch":"` + testEpoch + `"}`} {
		_, err := ParseAuthorityPin(value)
		require.ErrorIs(t, err, ErrReservationUnavailable)
	}
}
