package worker

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	core "k8s.io/api/core/v1"
	meta "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrlcli "sigs.k8s.io/controller-runtime/pkg/client"
	ctrlfake "sigs.k8s.io/controller-runtime/pkg/client/fake"

	worker "gpustack.ai/gpustack/api/worker/v1"
	"gpustack.ai/gpustack/pkg/kubeclients/kubernetes/scheme"
)

// TestInstanceImagePullSecretHandler_CreateUpdate pins that a create and an update write a
// dockerconfigjson Secret carrying the credentials of the registry.
func TestInstanceImagePullSecretHandler_CreateUpdate(t *testing.T) {
	ctx := context.Background()
	cli := ctrlfake.NewClientBuilder().WithScheme(scheme.Scheme).Build()
	h := &InstanceImagePullSecretHandler{Client: cli, APIReader: cli}

	key := ctrlcli.ObjectKey{Namespace: "default", Name: "registry"}
	obj := &worker.InstanceImagePullSecret{
		ObjectMeta: meta.ObjectMeta{Namespace: key.Namespace, Name: key.Name},
		Spec: worker.InstanceImagePullSecretSpec{
			Registry: "registry.example.com",
			Username: "user",
			Password: "pass",
			Email:    "user@example.com",
		},
	}
	getConfig := func(t *testing.T) string {
		t.Helper()
		sec := new(core.Secret)
		require.NoError(t, cli.Get(ctx, key, sec))
		assert.Equal(t, core.SecretTypeDockerConfigJson, sec.Type)
		return sec.StringData[core.DockerConfigJsonKey]
	}

	_, err := h.OnCreate(ctx, obj.DeepCopy(), ctrlcli.CreateOptions{})
	require.NoError(t, err)
	// base64("user:pass")
	assert.JSONEq(t,
		`{"auths":{"registry.example.com":{"auth":"dXNlcjpwYXNz","email":"user@example.com"}}}`,
		getConfig(t))

	existing, err := h.OnGet(ctx, key, ctrlcli.GetOptions{})
	require.NoError(t, err)
	updated := existing.(*worker.InstanceImagePullSecret).DeepCopy()
	updated.Spec = obj.Spec
	updated.Spec.Password = "rotated"
	_, err = h.OnUpdate(ctx, updated, existing, ctrlcli.UpdateOptions{})
	require.NoError(t, err)
	// base64("user:rotated")
	assert.JSONEq(t,
		`{"auths":{"registry.example.com":{"auth":"dXNlcjpyb3RhdGVk","email":"user@example.com"}}}`,
		getConfig(t))
}
